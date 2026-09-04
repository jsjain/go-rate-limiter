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

Apple M4 Pro (14 cores), Go 1.27, Redis 7.2.4 on localhost over TCP,
`-benchtime 2s -count 5`, medians. Local Redis is the *best* case for the network path; a real
deployment is slower.

### In-memory versus Redis

| Workload | Redis | In-memory | Speedup |
|---|---|---|---|
| `AllowN`, single key, sequential | 27,139 ns/op | **58.9 ns/op** | 461x |
| `AllowN`, 100k keys, all cores | 7,812 ns/op | **20.4 ns/op** | 383x |
| `AllowN`, single hot key, all cores | 7,651 ns/op | **411 ns/op** | 19x |
| `AllowN`, denied (over limit), all cores | — | **15.1 ns/op** | — |
| Allocations per call | 4 allocs, 285 B | **1 alloc, 64 B** | — |

Roughly: ~37k checks/s sequential and ~128k/s across cores on Redis, against ~17M/s sequential
and ~49M/s across cores in memory.

**The single-hot-key row is the caveat worth reading.** Every core contending on one key's CAS
costs 411 ns/op, twenty times worse than the same limiter spread across many keys (20.4 ns/op).
Rate limiters are normally keyed by user or IP, so the 100k-key row is the realistic one, but if
you funnel everything through one global key you will not see the headline number. The denied
path is the fastest of all: it takes no lock and performs no write.

The single allocation is the returned `*Result`.

### Redis path, before and after this rework

Measured by alternating between the two builds on the same machine, five rounds each, so
thermal drift cannot favour either side.

| Benchmark | Before | After |
|---|---|---|
| `AllowN`, single key, sequential | 26,629 ns/op, 6 allocs | 27,499 ns/op, **4 allocs** |
| `AllowN`, single hot key, all cores | 7,192 ns/op, 6 allocs | 8,081 ns/op, **4 allocs** |

**The Redis path got about 3% slower sequentially and 12% slower across cores, and that is a
deliberate trade.** The Lua scripts were rewritten to work in exact integer microseconds
instead of floating-point seconds, which is what removes the off-by-one in `Remaining`
described under Behaviour notes. Exact integer arithmetic costs a little more than sloppy float
arithmetic. In exchange the reply is now four integers rather than two integers and two
formatted floats, which removes two allocations per call.

If you would rather have the 12% than the exactness, the previous scripts are in the history;
nothing else in the package depends on which one you use.

`AllowAtMost` also improved, because it no longer re-encodes its limit on every call. Measured
in the same run:

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `AllowAtMost` (explicit limit, re-encoded per call) | 27,025 | 416 | 8 |
| `AllowAtMostN` (cached entry) | 26,936 | 321 | **4** |

Reproduce with:

```shell
go test -run '^$' -bench 'Benchmark(Redis|Mem)' -benchtime 2s -count 5
```

## Behaviour notes

- **Both backends compute GCRA in exact integers**, Redis in microseconds and in-memory in
  nanoseconds, and they agree on `Allowed` and `Remaining` for every limit. A randomised
  differential test drives both through the same sequences on every `go test` run.
- **Redis quantises the emission interval to whole microseconds**, which is the resolution
  `TIME` reports. An interval finer than a microsecond is charged one microsecond. The
  in-memory backend works in nanoseconds, so the two can differ by under a microsecond in
  `RetryAfter` and `ResetAfter`. Token counts are unaffected.
- **Clocks.** The Redis backend reads time via the Redis `TIME` command and inherits that
  server's wall clock. The in-memory backend uses a monotonic clock and is immune to wall-clock
  jumps and NTP steps.
- **Previously**, the Redis backend under-reported `Remaining` by one for limits such as
  `PerSecond(10)`: the script worked in floating-point seconds and subtracted two numbers of
  epoch magnitude, losing ~6e-8s to cancellation, so 9 available tokens were computed as
  8.999999976 and truncated to 8. `AllowAtMost` had a related flaw, charging a fractional cost
  to a key while reporting the truncated integer. Both are fixed. If you are upgrading and have
  assertions pinned to the old numbers, expect them to change by one.
- **Periods shorter than 10ms** were silently broken: the period was encoded with two decimal
  places, so anything under 10ms reached the script as `0.00` and made it divide by zero.

## License

See [LICENSE](LICENSE).
