# In-memory rate limiting + architecture rework

Repo: `github.com/jsjain/go-rate-limiter` (Go 1.26, deps: `rueidis`, `xsync/v4`)
Files today: `rate_limiter.go` (245), `lua.go` (116), `rate_limiter_test.go` (379)

Revision 2. Revised after review; review findings are folded in and marked `[R<n>]`.

## Goals

- G1. A mode where a rate-limit check never touches the network: pure in-process GCRA.
- G2. Keep the Redis path's atomicity and cross-process correctness exactly as they are now.
  One deliberate exception: F7 changes Redis behaviour for `Period < 10ms`, where the current
  path is already broken. See F7 and OPEN-3.
- G3. Cut per-call allocations and copies on both hot paths.
- G4. Prove G3 with numbers instead of claiming it.

Non-goal: a hybrid local-lease / local-cache mode in front of Redis. See "Deferred" — an
explicit cut, not an oversight.

## Current architecture, as read

`Limiter` holds `rueidis.Client`, a default `limitEntry`, an `xsync.Map[string, limitEntry]`
of per-key overrides, and a key prefix. `AllowN` resolves the entry, builds a 4-element
`[]string` ARGV, and runs `allowN` (a `rueidis.NewLuaScript` GCRA script) via EVALSHA.
The Lua script does GET, compute, SET+EX in one round trip. Atomicity is the whole point of it
being a script.

### Defects found while reading

- F1. `func (l Limiter) Allow` / `AllowN` use **value receivers** (`rate_limiter.go:153,158`)
  while every other method uses a pointer receiver.
- F2. `AllowAtMost` calls `newLimitEntry(limit)` on **every call** (`rate_limiter.go:191`) —
  three `strconv` allocations per call, precisely the cost `limitEntry` exists to remove.
- F3. `AllowAtMost` ignores `customLimits`, so the two entry points resolve limits by different
  rules. Undocumented and surprising.
- F4. `result[0]`..`result[3]` are indexed with no length check (`rate_limiter.go:176-178`).
  A script or protocol change becomes a panic in the caller's request path.
- F5. `[]string{...}` ARGV slice is allocated on every call.
- F6. README documents `haxmap` for `WithCustomLimits`; the code takes `map[string]Limit`. The
  example does not compile.
- F7. **[R4] `periodStr` loses precision.** `strconv.FormatFloat(seconds, 'f', 2, 32)`
  (`rate_limiter.go:89`) renders any `Period < 10ms` as `"0.00"` or `"0.01"`. Redis then computes
  `emission_interval = 0` and `remaining = diff/0` is inf/nan. The Redis path is already wrong
  for sub-10ms periods, and in-memory parity there is impossible. Fix `periodStr` to full
  precision (`'f', -1, 64`). This is a **Redis-path behaviour change**, so P1 is not a pure
  refactor — see P1's note.
- F8. **[R3] No validation of `Limit` or `n`.** In-memory `emissionInterval = Period / Rate` is
  integer division: `Rate == 0` panics the process. `SetCustomLimit(key, Limit{})` at runtime is
  enough to trigger it. Today Redis returns a script error instead. Negative `n` moves TAT
  backwards in both Lua and Go, refunding tokens.

## Design

### D1. Backend seam at the operation, not at load/store

The tempting refactor is a two-method store (`loadTAT` / `storeTAT`) with the GCRA math shared
across backends. That is wrong here: splitting load and store turns one EVALSHA into a GET plus
a SET with no atomicity, reintroducing a read-modify-write race across processes. A
`compareAndSet` store variant would need two round trips. The seam is the whole operation.

```go
type backend interface {
    allowN(ctx context.Context, key string, e limitEntry, n int) (*Result, error)
    allowAtMost(ctx context.Context, key string, e limitEntry, n int) (*Result, error)
    reset(ctx context.Context, key string) error
    close() error
}
```

`limitEntry` is passed **by value** [R8]: `customLimits.Load` returns a copy, and taking a
pointer to that local to pass through an interface method forces it to the heap — one
allocation per call, the exact cost D5 exists to remove.

`Limiter` keeps only limit resolution (default entry + `customLimits` lookup) and delegates. Two
implementations: `redisBackend{rdb, prefix}` (today's Lua path, moved verbatim) and `memBackend`.

[R10] `redisBackend.close()` must **not** close the caller-owned `rueidis.Client`. Returns nil.

### D2. In-memory backend

State per key is a single TAT (theoretical arrival time), stored as `int64` nanoseconds relative
to a construction-time epoch and read through a monotonic clock — immune to the wall-clock jumps
that Redis `TIME` is exposed to.

```go
type memBackend struct {
    state *xsync.Map[string, *atomic.Int64] // key -> TAT nanos since epoch
    now   func() int64                      // [R16] injectable clock
    stop  chan struct{}
    once  sync.Once
}
```

`xsync.Map` is already a direct dependency: no new dependency, no LRU library. Concurrency is a
CAS loop on the per-key `atomic.Int64` — lock-free reads, no global mutex, no sharded-lock table.
One small allocation the first time a key is seen, none after.

**[R16] The clock is injected.** `now` defaults to `time.Since(epoch).Nanoseconds()`. Without
this seam every refill, `RetryAfter`, and eviction test has to `time.Sleep`, which is flaky and
slow. This is the single change that makes the algorithm deterministically testable.

**`allowN`**, mirroring `lua.go:9-57` in integer nanoseconds:

```
ei          = Period / Rate          // precomputed in limitEntry
burstOffset = ei * Burst             // precomputed in limitEntry
increment   = ei * n
now         = b.now()
loaded      = p.Load()               // 0 on a fresh key
tat         = max(loaded, now)
newTAT      = tat + increment
diff        = now - (newTAT - burstOffset)
if diff < 0 -> deny; remaining = 0; retryAfter = -diff; resetAfter = tat - now  // no write
else        -> CAS(loaded, newTAT); on failure re-read BOTH loaded and now, retry
               allow; remaining = diff / ei; retryAfter = -1; resetAfter = newTAT - now
```

**[R2] `allowAtMost`** is a *different* algorithm (`lua.go:59-116`), not a variant, and must be
restated explicitly before implementation: deny condition is `remaining < 1` (not `< 0`),
`retryAfter` is `ei - diff` (not `-diff`), and `cost` is clamped down to `remaining`:

```
now         = b.now()
tat         = max(p.Load(), now)
diff        = now - (tat - burstOffset)
remaining   = diff / ei                          // integer division
if remaining < 1 -> deny; retryAfter = ei - diff; resetAfter = tat - now
cost        = min(n, remaining); remaining -= cost
newTAT      = tat + ei*cost
CAS; allow cost
```

**[R2] Known divergence, accepted and documented.** In Lua `remaining` is a float. With
`remaining = 2.7` and `cost = 3`, Lua charges `2.7 * ei` to the TAT but returns `Allowed = 2`,
because Redis truncates a Lua number when converting it to a RESP integer. The integer version
charges exactly 2. After any partial-refill clamp the two backends' TAT state diverges. The
in-memory version is the correct one; T2 must not assert parity across a clamp, and the README
must say so.

**[R5][R6] Edge cases, settled:**
- Fresh key: the initial value 0 is the epoch, always in the past, so `tat = max(0, now) = now`
  falls out of the same expression. No first-request branch. This is the whole reason the
  epoch-relative representation is chosen.
- `n > burst`: short-circuit to deny before computing `increment`. Also removes the `ei*n`
  overflow path.
- `n == 0` or `newTAT <= now`: skip the store entirely, matching Lua's `if reset_after > 0`
  guard and keeping probe calls from creating entries the sweeper then has to reap.
- Overflow: `burstOffset = ei * Burst` exceeds `MaxInt64` at roughly `PerDay(1)` with
  `Burst >= 106_713`. Checked once in validation (F8), not per call.
- Truncation: `PerSecond(3)` gives `ei = 333333333ns`, so the effective rate is
  3.000000003/s and a full refill is 1ns short. Lua's float has comparable error. `remaining =
  diff / ei` truncates, which is exactly what Redis does to the Lua float on the reply. Not a
  defect.
- **[R7]** On CAS failure, re-read `now` as well as `loaded`. A retry computing against a stale
  `now` can be denied by the width of the retry window. No live-lock risk: at least one CAS
  succeeds per round, and the deny path never writes, so denied requests never retry.

**[R3] Validation (F8).** `newLimitEntry` validates `Rate > 0`, `Period > 0`, `Burst >= 0`,
`Burst <= MaxInt64/ei`, and rejects `n < 0` at the call sites. Constructors and `SetCustomLimit`
must not be able to arm a panic for a later request. Open sub-decision: return an error from
`SetCustomLimit` (breaking) or fall back to the default limit and document it. Recommendation:
fall back, document, since `SetCustomLimit` currently returns nothing.

### D3. Eviction — the real in-memory design decision

Redis gets expiry free (`SET ... EX math.ceil(reset_after)`, `lua.go:53`). A Go map does not. An
unbounded keyspace (IPs, user IDs, API keys) makes in-memory mode a memory leak, and this is the
one part of the feature that changes the public API surface.

Two layers:

- **Lazy.** `tat = max(loaded, now)` already makes a stale entry behave identically to a missing
  one, so correctness costs nothing. Only memory is at risk.
- **Sweep.** A ticker goroutine (default 1 min, `WithSweepInterval`) reaps keys whose TAT is in
  the past. A goroutine requires a lifecycle, hence `func (l *Limiter) Close() error` — a no-op
  on the Redis backend.

#### [R1] BLOCKER in revision 1: the sweeper loses updates

Naive `Range` + `Delete` over-admits. Sequence: the sweeper reads `p.Load() < now`; request R
loads the same `*atomic.Int64` p; the sweeper deletes the key; R's CAS on p succeeds. R is
admitted, but its TAT write now lives in an orphaned atomic no one can see. The next request
creates a fresh atomic at 0, so `tat = now` and the key has a **full burst again**. Every writer
in that window is forgotten — up to one extra burst per key per sweep. `-race` will not catch
this: it is a logical race, not a data race.

**Fix: tombstone under the bucket lock, then delete.** `xsync.Map.Compute`'s callback runs while
holding the bucket mutex (verified: `map.go:665`, `rootb.mu.Lock()` precedes the `valueFn` call
at `map.go:687`), so the tombstone and the delete are one step from the map's point of view.

```go
const tombstone = math.MinInt64

// sweeper
state.Compute(key, func(p *atomic.Int64, _ bool) (*atomic.Int64, xsync.ComputeOp) {
    v := p.Load()
    if v < now && p.CompareAndSwap(v, tombstone) {
        return p, xsync.DeleteOp
    }
    return p, xsync.CancelOp
})
```

Writers already CAS against `loaded`. Exactly one of the two CASes wins: if the sweeper wins, the
writer's CAS fails, it re-reads, sees `tombstone`, and goes back to the map to `LoadOrCompute` a
fresh atomic. If the writer wins, the sweeper's CAS fails and the key survives. No write is lost
either way. Writers must therefore treat `loaded == tombstone` as "re-enter the map", not as a
TAT value.

Use `DeleteMatching` (present in v4.4.0, `map.go:1134`) rather than `Range` + `Delete` to drive
the scan.

**Documented alternative if the tombstone proves fiddly in review:** drop the `*atomic.Int64`,
store `int64` directly, run the entire GCRA computation inside `state.Compute`, and sweep with
`DeleteMatching`. Both take the bucket lock, so the race cannot arise. Cost: xsync allocates a
new entry on every update (`map.go:433,455,476`), so every admitted call allocates, and the read
path takes a lock. That trades G3 away. Recommendation: tombstone.

#### [R13] D3a — ticker versus amortized sweep

**Decision: ticker + `Close()`.** An amortized sweep every N calls needs no goroutine and no
public `Close()`, but `xsync.Map` has no resumable cursor, so the scan cannot be bounded per call
and one unlucky caller pays an O(keys) stall inside their request. Predictable tail latency is
the reason to choose in-memory mode at all. `Close()` is the only public-API growth in this plan
— flag it if you disagree.

Implementation notes: guard `close(stop)` with `sync.Once` so `Close()` is idempotent; after
`Close()`, `Allow` keeps working and simply stops evicting (simplest defined behaviour, and it
means a late call cannot panic).

### D4. Constructors

```go
func NewLimiter(rdb rueidis.Client, opts ...LimiterOption) *Limiter // unchanged
func NewInMemoryLimiter(opts ...LimiterOption) *Limiter             // new
```

A separate constructor, not a nil-client branch inside `AllowN` — keeps the check off the hot
path and makes the choice explicit at the call site.

**[R9] Construction order.** Today `NewLimiter` builds the struct and *then* applies options. If
the backend were built before the options loop, `WithPrefix` would be silently dropped. Either
keep `prefix` on `Limiter` and prefix the key before delegating, or construct the backend after
the loop. `WithPrefix` is a documented no-op for the in-memory backend.

### D5. Hot-path cost removal

- **[R11] Pointer receivers on `Allow`/`AllowN` (F1) are a BREAKING change**, and revision 1 was
  wrong to call P1 API-compatible. It breaks any caller holding a `Limiter` *value*: map values,
  struct fields typed `Limiter`, and any caller-defined interface satisfied by `Limiter` rather
  than `*Limiter`. Callers going through `NewLimiter` (i.e. `*Limiter`) are unaffected. The
  stated motive is also not measurable — the struct is ~6 words and the copy inlines. Do it for
  consistency, labelled as breaking; it is **removed from the hot-path cost list**.
- **[R12] `AllowAtMost` semantics, disambiguated.** For the existing four-arg signature the
  explicit `limit` argument **always wins**; `customLimits` is not consulted, so no existing
  caller changes behaviour. F3 is resolved by documenting this and by adding
  `AllowAtMostN(ctx, key, n)`, which resolves limits exactly like `AllowN` and uses the cached
  entry. F2's per-call `strconv` is accepted for the legacy signature.
- **[R14] Do not add the `xsync.Map[Limit, limitEntry]` memo** proposed in revision 1. It is an
  unbounded cache keyed by caller-supplied values — the same leak class D3 refuses for
  `prefix + key`. Deleted from the plan.
- Length-check the Lua result before indexing; return a typed error otherwise (F4).
- **[R15]** Precompute the `n == 1` ARGV slice inside `limitEntry` (F5). `rueidis` copies args
  into its own command buffer, so sharing a read-only slice is safe. This saves one ~64-byte
  allocation in front of a network round trip — **keep only if B3 shows it**.
- `l.prefix + key` still allocates per Redis call. Left alone deliberately: caching it needs an
  unbounded map, which is the leak D3 exists to avoid.

## Testing

- T1. Existing unit tests for `limitEntry` / construction stay green, except for the
  `periodStr` expectations, which F7 changes (`"1.00"` becomes `"1"`). Only the `wantPeriod`
  column of the `cases` table and the `periodStr` assertions are affected; `burstStr` and
  `rateStr` expectations are untouched. Affected assertions: `rate_limiter_test.go:56-58, 79-81,
  98-100, 113-116, 139-141, 162-164`, plus the `wantPeriod` values at `:35-40`.
- T2. **[R17] Parity test, expanded.** One sequence checking decisions and `Remaining` is not
  enough to trust a second implementation of the algorithm. Cover: `n > burst` (permanent deny,
  `ResetAfter == 0`); `n == burst` on a fresh key (`diff == 0`, allowed, `Remaining == 0`);
  `n == 0`; deny, wait exactly `RetryAfter`, then expect allowed (proves `RetryAfter` is correct,
  not merely positive); `ResetAfter` within tolerance; `Reset` then allowed; custom limits
  resolved through `NewInMemoryLimiter`. Use `PerMinute`-scale limits so microsecond drift
  between backends cannot flip a `Remaining` boundary. Skipped when Redis is unavailable.
  Explicitly **excluded**: the `allowAtMost` clamp case, per the documented divergence in D2.
- T3. **[R18] Concurrency, with the sweeper active.** The revision-1 test (100 goroutines, burst
  B, exactly B allowed) passes with an idle sweeper and would never have caught R1. Add a variant
  with `WithSweepInterval(time.Microsecond)` and keys going idle between rounds, asserting total
  admissions never exceed B per refill window over several rounds. Also assert that after B
  admissions a further denied call leaves the TAT unchanged (the deny path must not write). Run
  under `-race`, but note `-race` cannot detect R1's failure mode.
- T4. Eviction: keys present after use, gone after a forced sweep past their TAT (driven by the
  injected clock, not `Sleep`).
- T5. `Close()` is idempotent, stops the sweeper (no goroutine leak), does not close the
  `rueidis.Client`, and leaves `Allow` working afterwards.
- T6. **[R3]** `Limit{}` / `Rate: 0` through `NewLimiter`, `WithCustomLimits`, and
  `SetCustomLimit` does not panic. Negative `n` is rejected.

## Benchmarks (the evidence for "faster")

There is no concurrent benchmark today, so the throughput claim has nothing behind it. Add, with
`b.RunParallel`:

- B1. `AllowN` single hot key, in-memory vs Redis.
- B2. `AllowN` across 100k distinct keys (allocation and map-contention case).
- B3. Allocations per op on both, before and after D5 — this is what decides R15.

Completion is reported against these numbers, not against a description.

## Phases

- P1. Backend seam (D1) + defects F2, F4, F5, F7, F8. **Not a pure refactor**: F7 changes Redis
  behaviour for sub-10ms periods and updates T1 expectations. F1's receiver change is breaking
  and is called out in the changelog.
- P2. In-memory backend (D2, D4) + T2, T3, T6. Blocked on R1, R2, R3, R11, R12, R16 being
  settled in this document — they now are.
- P3. Eviction, tombstone sweeper, and `Close()` (D3) + T4, T5.
- P4. Benchmarks (B1–B3), README rewrite: F6, the per-process semantics note ([R19] N replicas
  give N times the limit), and the `allowAtMost` divergence.

## Open decisions for the user

These are the only forks left in the document. Implementation of P2 onward should not start
until they are answered; P1 can proceed regardless except for OPEN-3.

- **OPEN-1 (from D3a).** `Close()` is the plan's only public-API growth. Accept it, or take the
  amortized-sweep alternative that needs no goroutine and no `Close()` but stalls one caller for
  O(keys) inside their request? Recommendation: accept `Close()`.
- **OPEN-2 (from F8 / D2 validation).** An invalid `Limit` (`Rate: 0`) currently arms a panic in
  the in-memory path. `SetCustomLimit` returns nothing today. Silently fall back to the default
  limit and document it, or change the signature to return an error (breaking)?
  Recommendation: fall back and document.
- **OPEN-3 (from F7 / R11).** Two breaking or behaviour-changing items, independent of each
  other: (a) F7's `periodStr` precision fix changes Redis results for `Period < 10ms` — the
  alternative is to reject `Period < 10ms` in validation, preserving current behaviour at the
  cost of refusing legitimate configs; (b) F1's pointer-receiver change breaks callers holding a
  `Limiter` value and buys nothing measurable. Recommendation: take (a), skip (b).

## Deferred, with the trigger to build it

- Local-lease / write-behind hybrid (each instance leases a token batch from Redis, checks
  locally, reconciles). The real throughput win for distributed deploys, but it trades exactness
  for speed and needs a reconciliation protocol. Build it when a measured Redis round trip is the
  bottleneck and approximate global limits are acceptable.
- Per-key sharded locks, LRU with a hard memory cap, metrics hooks, a middleware package.
