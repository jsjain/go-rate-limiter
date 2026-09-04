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

Apple M4 Pro (14 cores), Go 1.27, Redis 8 on localhost over TCP, `-benchtime 2s -count 3`,
medians. Local Redis is the *best* case for the network path; a real deployment is slower.

### In-memory versus Redis

| Workload | Redis | In-memory | Speedup |
|---|---|---|---|
| `AllowN`, single key, sequential | 25,337 ns/op | **56.8 ns/op** | 446x |
| `AllowN`, 100k keys, all cores | 7,053 ns/op | **19.9 ns/op** | 355x |
| `AllowN`, single hot key, all cores | 7,010 ns/op | **503 ns/op** | 14x |
| `AllowN`, denied (over limit), all cores | — | **15.2 ns/op** | — |
| Allocations per call | 6 allocs, 288 B | **1 alloc, 64 B** | — |

Roughly: ~39k checks/s sequential and ~142k/s across cores on Redis, against ~17.6M/s
sequential and ~50M/s across cores in memory.

**The single-hot-key row is the honest caveat.** Every core contending on one key's CAS costs
503 ns/op, an order of magnitude worse than the same limiter spread over many keys (19.9 ns/op)
and worse than the single-threaded number (56.8 ns/op). Rate limiters are usually keyed by
user or IP, so the 100k-key row is the realistic one, but if you limit everything under one
global key you will not see the headline speedup. The denied path is the fastest of all, since
it takes no lock and performs no write.

The single allocation is the returned `*Result`.

### Redis path, before and after this rework

| Benchmark | Before | After |
|---|---|---|
| `AllowN`, single key, sequential | 27,437 ns/op, 6 allocs | 25,337 ns/op, 6 allocs |
| `AllowN`, single hot key, all cores | 7,096 ns/op, 6 allocs | 7,010 ns/op, 6 allocs |
| `AllowN`, 100k keys, all cores | 7,100 ns/op, 6 allocs | 7,053 ns/op, 6 allocs |

**Unchanged within run-to-run noise, and that is the expected result.** A round trip of ~7 µs
dominates everything the Go side does, so the encoding work removed from the hot path is
invisible in `ns/op`. It is not invisible in garbage: reusing the argument slice for the common
`n == 1` call drops one 64-byte allocation per check, which `ns/op` cannot show.

Where the Redis path did measurably improve is `AllowAtMost`, which re-encoded its limit on
every single call. Measured in the same run:

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `AllowAtMost` (explicit limit, re-encoded per call) | 25,552 | 413 | 9 |
| `AllowAtMostN` (cached entry) | 25,725 | 321 | **6** |

Reproduce with:

```shell
go test -run '^$' -bench 'Benchmark(Redis|Mem)' -benchtime 2s -count 3
```

## Behaviour notes

- **Clocks.** The Redis backend reads time via the Redis `TIME` command, so it inherits that
  server's wall clock. The in-memory backend uses a monotonic clock and is immune to wall-clock
  jumps and NTP steps.
- **The Redis backend can under-report `Remaining` by one.** The Lua script computes the
  remaining token count in floating point, so `PerSecond(10)` makes it evaluate `0.9/0.1` as
  `8.999999999999998`, which Redis truncates to `8` where the exact answer is `9`. The
  in-memory backend works in integer nanoseconds and reports `9`. This predates the in-memory
  backend, matches upstream `redis_rate`, and affects only the reported number, never the
  admit/deny decision. Limits whose period divides evenly by the rate (`PerMinute(10)`,
  `PerSecond(100)`) are unaffected.
- **`AllowAtMost` partial fills differ slightly between backends.** In Lua the remaining-token
  count is a float, so a clamped request charges a fractional cost to the key's state while
  Redis truncates the count it reports back. The in-memory backend charges exactly what it
  reports. The two backends' internal state can therefore drift apart after a clamped
  `AllowAtMost` call; every other operation agrees exactly.
- **Periods shorter than 10ms** used to be silently broken on the Redis path: the period was
  encoded with two decimal places, so anything under 10ms reached the script as `0.00` and made
  it divide by zero. It is now encoded at full precision.

## License

See [LICENSE](LICENSE).
