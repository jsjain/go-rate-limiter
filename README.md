# go-rate-limiter

[![Go Reference](https://pkg.go.dev/badge/github.com/jsjain/go-rate-limiter.svg)](https://pkg.go.dev/github.com/jsjain/go-rate-limiter)

GCRA ([Generic Cell Rate Algorithm](https://en.wikipedia.org/wiki/Generic_cell_rate_algorithm),
aka leaky bucket) rate limiting for Go, with two interchangeable backends:

| Backend | Constructor | Scope of the limit | Cost per check |
|---|---|---|---|
| Redis | `NewLimiter(client, ...)` | Shared by every process on the same Redis | one network round trip |
| In-memory | `NewInMemoryLimiter(...)` | This process only | a map lookup and a CAS |

Based on [rwz/redis-gcra](https://github.com/rwz/redis-gcra), with
[go-redis/redis_rate](https://github.com/go-redis/redis_rate) as inspiration. The Redis backend
uses [rueidis](https://github.com/redis/rueidis) and requires Redis 3.2 or newer for
[`replicate_commands`](https://redis.io/commands/eval#replicating-commands-instead-of-scripts).

## Installation

```shell
go get github.com/jsjain/go-rate-limiter
```

## Read this before using the in-memory backend

The in-memory backend is not a faster drop-in for the Redis one. It is a **different guarantee**,
and swapping constructors without changing anything else will silently raise your effective
limit. Four things to know:

1. **It limits per process, not per service.** Ten replicas configured for 100 req/s will admit
   1000 req/s in aggregate. There is no coordination between processes, by design. If the number
   you configure is a number you bill against or promise to a customer, use `NewLimiter`.
2. **State is lost on restart.** Every deploy, crash, or scale-up hands every key a full fresh
   burst. A rolling restart is a burst amplifier.
3. **A caller spread across instances gets a budget per instance.** Behind a load balancer, one
   client sees N times the limit; behind sticky routing it sees roughly the right one.
4. **You must call `Close()`.** It stops the background sweeper that reclaims idle keys. Skip it
   and you leak a goroutine per limiter. Skip the sweeper itself (`WithSweepInterval(0)`) and an
   unbounded keyspace — IPs, user IDs, API keys — leaks memory instead.

It is the right choice when the thing you are protecting is genuinely per process: a local
connection pool, one instance's budget for a downstream call, a single-binary deployment, or a
sidecar mapped 1:1 with the workload it fronts. It is also the right choice when you cannot
tolerate the limiter itself being an availability dependency, since it cannot fail because Redis
is down.

## Usage

### Redis backend

```go
package main

import (
	"context"
	"fmt"

	rl "github.com/jsjain/go-rate-limiter"
	"github.com/redis/rueidis"
)

func main() {
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{"127.0.0.1:6379"},
	})
	if err != nil {
		panic(err)
	}
	defer client.Close()

	limiter := rl.NewLimiter(client, rl.WithRateLimit(rl.PerSecond(10)))
	defer limiter.Close() // does not close the client; you still own it

	res, err := limiter.Allow(context.Background(), "user:42")
	if err != nil {
		panic(err)
	}
	fmt.Println("allowed", res.Allowed, "remaining", res.Remaining)
	// allowed 1 remaining 9
}
```

### In-memory backend

```go
limiter := rl.NewInMemoryLimiter(rl.WithRateLimit(rl.PerSecond(10)))
defer limiter.Close()

res, err := limiter.Allow(context.Background(), "user:42")
if err != nil {
	panic(err)
}
fmt.Println("allowed", res.Allowed, "remaining", res.Remaining)
// allowed 1 remaining 9
```

No client, no context deadline to worry about, and the call cannot return a network error.

### Per-key limits

Custom limits are compiled once, at the point you set them, so no per-call encoding happens on
the hot path. They work identically on both backends.

```go
limiter := rl.NewInMemoryLimiter(
	rl.WithRateLimit(rl.PerSecond(20)),                            // default for every key
	rl.WithCustomLimits(map[string]rl.Limit{
		"premium:1": {Rate: 50, Burst: 50, Period: time.Second},    // override
	}),
)
defer limiter.Close()

limiter.SetCustomLimit("premium:2", rl.PerMinute(500)) // also works after construction
```

An invalid limit (`Rate <= 0`, `Period <= 0`, `Burst < 0`) is **ignored rather than applied**:
`WithRateLimit` keeps the package default and `SetCustomLimit` leaves the key on the limiter's
default. This is deliberate, so a bad config cannot arm a division by zero in a later request.

### Options

| Option | Effect |
|---|---|
| `WithRateLimit(Limit)` | Default limit for keys without an override. |
| `WithCustomLimits(map[string]Limit)` | Per-key overrides, compiled at construction. |
| `WithPrefix(string)` | Redis key prefix. No effect in memory, where the keyspace is private. |
| `WithSweepInterval(d)` | How often the in-memory backend reclaims idle keys (default 1m; `0` disables). No effect on Redis, which gets expiry from `SET ... EX`. |

### API

```go
Allow(ctx, key)                    // AllowN with n = 1
AllowN(ctx, key, n)                // all n events, or none
AllowAtMostN(ctx, key, n)          // up to n events, clamped to what is available
AllowAtMost(ctx, key, limit, n)    // same, with an explicit limit
Reset(ctx, key)                    // clear a key's state
Close()                            // release resources; idempotent
```

`AllowAtMost` takes an explicit `Limit` and **does not consult per-key custom limits** — the
argument always wins. Use `AllowAtMostN` if you want the same resolution `AllowN` uses. It is
also the cheaper of the two, since `AllowAtMost` must encode its limit argument on every call.

## Benchmarks

### In-memory versus Redis

| Workload | Redis | In-memory | Speedup |
|---|---|---|---|
| `AllowN`, single key, sequential | 25,826 ns/op | **53.9 ns/op** | 479x |
| `AllowN`, 100k keys, all cores | 7,093 ns/op | **20.1 ns/op** | 353x |
| `AllowN`, single hot key, all cores | 7,089 ns/op | **502 ns/op** | 14x |
| `AllowN`, denied (over limit), all cores | — | **14.8 ns/op** | — |
| Allocations per call | 6 allocs, 288 B | **1 alloc, 64 B** | — |

Roughly: ~39k checks/s sequential and ~141k/s across cores on Redis, against ~18.6M/s
sequential and ~50M/s across cores in memory.

`AllowAtMostN` reuses its cached limit entry where the older `AllowAtMost` re-encodes an
explicit one on every call. Measured in the same run:

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `AllowAtMost` (explicit limit, re-encoded per call) | 25,571 | 413 | 9 |
| `AllowAtMostN` (cached entry) | 25,827 | 325 | **6** |

**Measured on:** Apple M4 Pro, 14 cores, 24 GB, macOS 26.6.2 (Darwin 25.6.0). Go 1.27.0,
darwin/arm64. Redis 7.2.4 running on localhost over TCP. `-benchtime 2s -count 5`, medians of
five. Local Redis is the *best* case for the network path — a real deployment crosses a
network and will be slower, which widens the in-memory gap rather than narrowing it. Your
numbers will differ; what should hold is the shape, not the digits.

Reproduce with:

```shell
go test -run '^$' -bench 'Benchmark(Redis|Mem)' -benchtime 2s -count 5
```

**The single-hot-key row is the caveat worth reading.** Every core contending on one key's CAS
costs 502 ns/op, twenty-five times worse than the same limiter spread across many keys
(20.1 ns/op). Rate limiters are normally keyed by user or IP, so the 100k-key row is the
realistic one, but if you funnel everything through one global key you will not see the
headline number. The denied path is the fastest of all: it takes no lock and performs no write.

The single in-memory allocation is the returned `*Result`.

The Redis path's own numbers are unchanged by this rework, within run-to-run noise. A ~7 µs
round trip dominates everything the Go side does, so removing encoding work from the hot path
cannot show up in `ns/op`. It does show up in garbage, which `ns/op` cannot see.

## Behaviour notes

- **Redis can report one token fewer than are available.** The Lua scripts compute in
  floating-point seconds and subtract two values of epoch magnitude, which loses ~6e-8s. For
  `PerSecond(10)` that turns 9 available tokens into 8.999999976, and Redis truncates it to 8.
  Across rates 1–60 at per-second, per-minute and per-hour periods, 51 of 180 limits are
  affected. The gap is always exactly one and always in that direction.

  **The admit/deny decision is never affected** — `Allowed` agreed on all 180 — so you are not
  rate limiting incorrectly, you are reporting one token low on a header or a metric. The
  in-memory backend computes in integer nanoseconds and is always exact, so the two backends
  can differ by one in `Remaining` for the same key.

  An exact integer-microsecond version of the scripts was written and measured; it cost about
  12% of Redis throughput, so the float version was kept deliberately. The exact one is in the
  git history if you would rather have the precision.
- **`AllowAtMost` partial fills inherit the same slack.** The clamped count comes from the same
  float quotient, so Redis can admit one fewer than the in-memory backend would. It also charges
  a fractional cost to the key's state while reporting the truncated integer, so the two
  backends' stored state can drift after a clamped call. Every other operation agrees.
- **Clocks.** The Redis backend reads time via the Redis `TIME` command and inherits that
  server's wall clock. The in-memory backend uses a monotonic clock and is immune to wall-clock
  jumps and NTP steps.
- **Periods shorter than 10ms** were silently broken: the period was encoded with two decimal
  places, so anything under 10ms reached the script as `0.00` and made it divide by zero. It is
  now encoded at full precision.

A randomised differential test runs both backends through the same sequences on every
`go test`, holding them to exactly this contract: identical decisions, and a `Remaining` that
Redis may report one low but never high and never by more than one.

## License

See [LICENSE](LICENSE).
