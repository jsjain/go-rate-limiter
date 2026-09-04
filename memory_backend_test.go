package rate_limiter

import (
	"context"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- helpers ---

func memOf(t testing.TB, limiter *Limiter) *memBackend {
	t.Helper()
	mem, ok := limiter.backend.(*memBackend)
	if !ok {
		t.Fatalf("expected memBackend, got %T", limiter.backend)
	}
	return mem
}

// fakeClock replaces the backend's monotonic clock so tests can move time
// without sleeping.
type fakeClock struct{ ns atomic.Int64 }

func (clock *fakeClock) advance(by time.Duration) { clock.ns.Add(int64(by)) }

// newFakeLimiter returns an in-memory limiter driven by a controllable clock,
// with the background sweeper disabled so tests drive sweep() themselves.
func newFakeLimiter(t testing.TB, opts ...LimiterOption) (*Limiter, *fakeClock) {
	t.Helper()
	limiter := NewInMemoryLimiter(append([]LimiterOption{WithSweepInterval(0)}, opts...)...)
	clock := &fakeClock{}
	clock.ns.Store(int64(time.Hour)) // start away from the epoch
	memOf(t, limiter).now = clock.ns.Load
	t.Cleanup(func() { limiter.Close() })
	return limiter, clock
}

// --- T2: parity between the in-memory and Redis backends ---

// step is one call in a parity sequence.
type step struct {
	n           int
	wantAllowed int
	wantRemain  int
}

func TestParity_InMemoryMatchesRedis(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)

	// PerMinute scale: emission interval is 1s, so microsecond drift between
	// the two backends cannot flip a Remaining boundary.
	cases := []struct {
		name  string
		limit Limit
		steps []step
	}{
		{"single tokens to exhaustion", PerMinute(3), []step{
			{1, 1, 2}, {1, 1, 1}, {1, 1, 0}, {1, 0, 0}, {1, 0, 0},
		}},
		{"n equals burst on a fresh key", PerMinute(5), []step{
			{5, 5, 0}, {1, 0, 0},
		}},
		{"n greater than burst is never admitted", PerMinute(3), []step{
			{4, 0, 0}, {1, 1, 2},
		}},
		{"zero cost does not consume", PerMinute(3), []step{
			{0, 0, 3}, {0, 0, 3}, {1, 1, 2},
		}},
		{"partial then exhaust", PerMinute(10), []step{
			{4, 4, 6}, {6, 6, 0}, {1, 0, 0},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			redis := NewLimiter(client, WithRateLimit(tc.limit))
			memLimiter := NewInMemoryLimiter(WithRateLimit(tc.limit))
			defer memLimiter.Close()

			key := "parity:" + t.Name()
			if err := redis.Reset(ctx, key); err != nil {
				t.Fatalf("reset: %v", err)
			}
			defer redis.Reset(ctx, key)

			for i, s := range tc.steps {
				redisRes, err := redis.AllowN(ctx, key, s.n)
				if err != nil {
					t.Fatalf("step %d redis: %v", i, err)
				}
				memRes, err := memLimiter.AllowN(ctx, key, s.n)
				if err != nil {
					t.Fatalf("step %d mem: %v", i, err)
				}
				if redisRes.Allowed != s.wantAllowed || redisRes.Remaining != s.wantRemain {
					t.Errorf("step %d redis: allowed=%d remaining=%d, want %d/%d",
						i, redisRes.Allowed, redisRes.Remaining, s.wantAllowed, s.wantRemain)
				}
				if memRes.Allowed != redisRes.Allowed || memRes.Remaining != redisRes.Remaining {
					t.Errorf("step %d divergence: mem allowed=%d remaining=%d, redis allowed=%d remaining=%d",
						i, memRes.Allowed, memRes.Remaining, redisRes.Allowed, redisRes.Remaining)
				}
				// RetryAfter must agree on sign: -1 when allowed, positive when not.
				if (memRes.RetryAfter < 0) != (redisRes.RetryAfter < 0) {
					t.Errorf("step %d RetryAfter sign: mem=%v redis=%v", i, memRes.RetryAfter, redisRes.RetryAfter)
				}
			}
		})
	}
}

// n > burst is a permanent denial: waiting cannot help, so ResetAfter is zero
// on an otherwise idle key.
func TestParity_OverBurstIsPermanent(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	limit := PerMinute(3)
	key := "parity:overburst"

	redis := NewLimiter(client, WithRateLimit(limit))
	memLimiter := NewInMemoryLimiter(WithRateLimit(limit))
	defer memLimiter.Close()
	redis.Reset(ctx, key)
	defer redis.Reset(ctx, key)

	for name, limiter := range map[string]*Limiter{"redis": redis, "mem": memLimiter} {
		res, err := limiter.AllowN(ctx, key, 4)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Allowed != 0 {
			t.Errorf("%s: expected denied, got allowed=%d", name, res.Allowed)
		}
		if res.ResetAfter != 0 {
			t.Errorf("%s: expected ResetAfter=0 on an idle key, got %v", name, res.ResetAfter)
		}
		if res.RetryAfter <= 0 {
			t.Errorf("%s: expected positive RetryAfter, got %v", name, res.RetryAfter)
		}
	}
}

// Reset must restore a fully exhausted key on both backends.
func TestParity_ResetRestores(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	limit := PerMinute(1)
	key := "parity:reset"

	redis := NewLimiter(client, WithRateLimit(limit))
	memLimiter := NewInMemoryLimiter(WithRateLimit(limit))
	defer memLimiter.Close()
	redis.Reset(ctx, key)
	defer redis.Reset(ctx, key)

	for name, limiter := range map[string]*Limiter{"redis": redis, "mem": memLimiter} {
		limiter.Allow(ctx, key)
		if res, _ := limiter.Allow(ctx, key); res.Allowed != 0 {
			t.Fatalf("%s: expected exhausted", name)
		}
		if err := limiter.Reset(ctx, key); err != nil {
			t.Fatalf("%s reset: %v", name, err)
		}
		if res, _ := limiter.Allow(ctx, key); res.Allowed != 1 {
			t.Errorf("%s: expected allowed after reset, got %d", name, res.Allowed)
		}
	}
}

// --- T2 continued: RetryAfter is exact, not merely positive ---

func TestInMemory_RetryAfterIsExact(t *testing.T) {
	ctx := context.Background()
	limiter, clock := newFakeLimiter(t, WithRateLimit(PerSecond(4))) // emission interval 250ms
	key := "retryafter"

	for i := 0; i < 4; i++ {
		if res, _ := limiter.AllowN(ctx, key, 1); res.Allowed != 1 {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	res, _ := limiter.AllowN(ctx, key, 1)
	if res.Allowed != 0 {
		t.Fatal("5th request should be denied")
	}
	if res.RetryAfter != 250*time.Millisecond {
		t.Errorf("RetryAfter: got %v, want 250ms", res.RetryAfter)
	}

	// One nanosecond short of RetryAfter: still denied.
	clock.advance(res.RetryAfter - 1)
	if r, _ := limiter.AllowN(ctx, key, 1); r.Allowed != 0 {
		t.Errorf("expected denial 1ns before RetryAfter elapses, got allowed=%d", r.Allowed)
	}
	// Exactly at RetryAfter: allowed.
	clock.advance(1)
	if r, _ := limiter.AllowN(ctx, key, 1); r.Allowed != 1 {
		t.Errorf("expected admission exactly at RetryAfter, got allowed=%d", r.Allowed)
	}
}

func TestInMemory_ResetAfterIsExact(t *testing.T) {
	ctx := context.Background()
	limiter, _ := newFakeLimiter(t, WithRateLimit(PerSecond(4)))

	res, _ := limiter.AllowN(ctx, "resetafter", 2)
	if res.Allowed != 2 {
		t.Fatalf("expected 2 allowed, got %d", res.Allowed)
	}
	// Two tokens at a 250ms emission interval means full reset in 500ms.
	if res.ResetAfter != 500*time.Millisecond {
		t.Errorf("ResetAfter: got %v, want 500ms", res.ResetAfter)
	}
}

func TestInMemory_CustomLimitResolved(t *testing.T) {
	ctx := context.Background()
	key := "custom:key"
	limiter, _ := newFakeLimiter(t,
		WithRateLimit(PerSecond(100)),
		WithCustomLimits(map[string]Limit{key: PerSecond(1)}),
	)

	if res, _ := limiter.Allow(ctx, key); res.Allowed != 1 {
		t.Fatal("first call should be allowed")
	}
	if res, _ := limiter.Allow(ctx, key); res.Allowed != 0 {
		t.Error("custom limit of 1/s should deny the second call")
	}
	// A key without an override uses the default.
	if res, _ := limiter.Allow(ctx, "other"); res.Allowed != 1 {
		t.Error("default limit should admit an unrelated key")
	}
}

func TestInMemory_AllowAtMostClamps(t *testing.T) {
	ctx := context.Background()
	limiter, _ := newFakeLimiter(t, WithRateLimit(PerSecond(10)))
	key := "atmost"

	if res, _ := limiter.AllowAtMostN(ctx, key, 7); res.Allowed != 7 {
		t.Fatalf("expected 7 allowed, got %d", res.Allowed)
	}
	// Only 3 remain, so a request for 5 is clamped down to 3.
	res, _ := limiter.AllowAtMostN(ctx, key, 5)
	if res.Allowed != 3 {
		t.Errorf("expected clamp to 3, got %d", res.Allowed)
	}
	if res.Remaining != 0 {
		t.Errorf("expected Remaining=0 after clamp, got %d", res.Remaining)
	}
	if res, _ := limiter.AllowAtMostN(ctx, key, 1); res.Allowed != 0 {
		t.Errorf("expected denial once exhausted, got %d", res.Allowed)
	}
}

// --- T3: concurrency ---

// A burst of B must admit exactly B, no matter how many goroutines race.
func TestInMemory_ConcurrentExactBurst(t *testing.T) {
	ctx := context.Background()
	const burst = 50
	limiter, _ := newFakeLimiter(t, WithRateLimit(PerMinute(burst)))
	key := "concurrent"

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				res, err := limiter.Allow(ctx, key)
				if err != nil {
					t.Error(err)
					return
				}
				allowed.Add(int64(res.Allowed))
			}
		}()
	}
	wg.Wait()

	if got := allowed.Load(); got != burst {
		t.Errorf("admitted %d events, want exactly %d", got, burst)
	}
}

// The deny path must not write: a denied call cannot push the TAT further out.
func TestInMemory_DenyDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	limiter, _ := newFakeLimiter(t, WithRateLimit(PerSecond(2)))
	mem := memOf(t, limiter)
	key := "denywrite"

	limiter.AllowN(ctx, key, 2) // exhaust
	cell, _ := mem.state.Load(key)
	before := cell.Load()

	for i := 0; i < 10; i++ {
		if res, _ := limiter.Allow(ctx, key); res.Allowed != 0 {
			t.Fatal("expected denial")
		}
	}
	if after := cell.Load(); after != before {
		t.Errorf("denied calls moved the TAT: before=%d after=%d", before, after)
	}
}

// Regression test for the sweeper losing updates. A key whose TAT is in the
// past is eligible for reclamation at exactly the moment a writer is claiming
// its first token. If the sweeper deletes the cell out from under that writer,
// the write lands in an orphan, the next caller sees a fresh key, and the key
// admits more than its burst.
//
// A data-race detector cannot catch this: the failure is a lost update, not a
// racy access. Only the admission count exposes it.
func TestInMemory_SweeperDoesNotLoseUpdates(t *testing.T) {
	ctx := context.Background()
	const (
		keys        = 2000
		racersPer   = 4
		burst       = 1
		sweepRounds = 400
	)
	// Burst of 1 maximises sensitivity: a single lost update shows up as 2.
	limiter, _ := newFakeLimiter(t, WithRateLimit(PerMinute(burst)))
	mem := memOf(t, limiter)

	done := make(chan struct{})
	var sweeps sync.WaitGroup
	sweeps.Add(1)
	go func() {
		defer sweeps.Done()
		for {
			select {
			case <-done:
				return
			default:
				mem.sweep()
			}
		}
	}()

	counts := make([]atomic.Int64, keys)
	var wg sync.WaitGroup
	for i := 0; i < keys; i++ {
		key := "sweeprace:" + string(rune('a'+i%26)) + "-" + itoa(i)
		for r := 0; r < racersPer; r++ {
			wg.Add(1)
			go func(idx int, k string) {
				defer wg.Done()
				res, err := limiter.Allow(ctx, k)
				if err != nil {
					t.Error(err)
					return
				}
				counts[idx].Add(int64(res.Allowed))
			}(i, key)
		}
	}
	wg.Wait()
	close(done)
	sweeps.Wait()

	over := 0
	for i := range counts {
		if counts[i].Load() > burst {
			over++
		}
	}
	if over > 0 {
		t.Errorf("%d of %d keys admitted more than their burst of %d; the sweeper lost updates",
			over, keys, burst)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}

// --- T4: eviction ---

func TestInMemory_SweepReclaimsExpiredKeys(t *testing.T) {
	ctx := context.Background()
	limiter, clock := newFakeLimiter(t, WithRateLimit(PerSecond(1)))
	mem := memOf(t, limiter)

	for _, k := range []string{"a", "b", "c"} {
		if res, _ := limiter.Allow(ctx, k); res.Allowed != 1 {
			t.Fatalf("key %q should be allowed", k)
		}
	}
	if n := mem.state.Size(); n != 3 {
		t.Fatalf("expected 3 live keys, got %d", n)
	}

	// Not yet expired: the sweeper must leave them alone.
	if got := mem.sweep(); got != 0 {
		t.Errorf("swept %d keys before they expired, want 0", got)
	}
	if n := mem.state.Size(); n != 3 {
		t.Errorf("expected 3 live keys after an early sweep, got %d", n)
	}

	clock.advance(2 * time.Second) // past every TAT
	if got := mem.sweep(); got != 3 {
		t.Errorf("swept %d keys, want 3", got)
	}
	if n := mem.state.Size(); n != 0 {
		t.Errorf("expected an empty map after sweeping, got %d", n)
	}

	// A reclaimed key still behaves correctly: it is simply idle again.
	if res, _ := limiter.Allow(ctx, "a"); res.Allowed != 1 {
		t.Errorf("expected a reclaimed key to be admitted, got %d", res.Allowed)
	}
}

func TestInMemory_ResetDropsKey(t *testing.T) {
	ctx := context.Background()
	limiter, _ := newFakeLimiter(t, WithRateLimit(PerSecond(1)))
	mem := memOf(t, limiter)

	limiter.Allow(ctx, "gone")
	if _, ok := mem.state.Load("gone"); !ok {
		t.Fatal("expected the key to exist")
	}
	if err := limiter.Reset(ctx, "gone"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, ok := mem.state.Load("gone"); ok {
		t.Error("expected the key to be removed after Reset")
	}
	// Reset on an unknown key is not an error.
	if err := limiter.Reset(ctx, "never-seen"); err != nil {
		t.Errorf("reset of an unknown key: %v", err)
	}
}

// --- T5: lifecycle ---

func TestInMemory_CloseIsIdempotentAndKeepsWorking(t *testing.T) {
	ctx := context.Background()
	limiter := NewInMemoryLimiter(WithRateLimit(PerSecond(2)), WithSweepInterval(time.Millisecond))

	if err := limiter.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := limiter.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Closing stops eviction, not enforcement.
	if res, err := limiter.Allow(ctx, "after-close"); err != nil || res.Allowed != 1 {
		t.Errorf("expected the limiter to keep working after Close, got allowed=%d err=%v", res.Allowed, err)
	}
}

func TestRedis_CloseDoesNotCloseClient(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)
	limiter := NewLimiter(client, WithRateLimit(PerSecond(5)))
	key := "close:client"
	defer limiter.Reset(ctx, key)

	if err := limiter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The caller owns the client, so it must still be usable.
	if res, err := limiter.Allow(ctx, key); err != nil || res.Allowed != 1 {
		t.Errorf("client unusable after Close: allowed=%d err=%v", res.Allowed, err)
	}
}

// --- T6: invalid input ---

func TestInvalidLimitsFallBackToDefault(t *testing.T) {
	ctx := context.Background()
	bad := []Limit{
		{},
		{Rate: 0, Burst: 5, Period: time.Second},
		{Rate: 5, Burst: 5, Period: 0},
		{Rate: 5, Burst: -1, Period: time.Second},
		{Rate: 1, Burst: 1 << 62, Period: 24 * time.Hour}, // burstOffset would overflow
	}

	for _, limit := range bad {
		if entry := newLimitEntry(limit); entry.valid() {
			t.Errorf("limit %+v should be rejected as invalid", limit)
		}
	}

	// An invalid default keeps the package default rather than panicking.
	limiter := NewInMemoryLimiter(WithRateLimit(Limit{}))
	defer limiter.Close()
	if limiter.limit.limit != defaultLimits() {
		t.Errorf("invalid WithRateLimit should keep the default, got %v", limiter.limit.limit)
	}

	// An invalid override is not stored, so the key uses the default limit.
	fallbackLimiter := NewInMemoryLimiter(WithRateLimit(PerSecond(2)))
	defer fallbackLimiter.Close()
	fallbackLimiter.SetCustomLimit("k", Limit{Rate: 0})
	if _, ok := fallbackLimiter.customLimits.Load("k"); ok {
		t.Error("invalid SetCustomLimit should not be stored")
	}
	for i := 0; i < 2; i++ {
		if res, err := fallbackLimiter.Allow(ctx, "k"); err != nil || res.Allowed != 1 {
			t.Fatalf("call %d: expected the default limit to apply, got allowed=%d err=%v", i, res.Allowed, err)
		}
	}
	if res, _ := fallbackLimiter.Allow(ctx, "k"); res.Allowed != 0 {
		t.Error("expected the default limit of 2/s to deny the third call")
	}
}

func TestNegativeCountRejected(t *testing.T) {
	ctx := context.Background()
	limiter, _ := newFakeLimiter(t, WithRateLimit(PerSecond(5)))

	if _, err := limiter.AllowN(ctx, "neg", -1); err != ErrNegativeCount {
		t.Errorf("AllowN(-1): got %v, want ErrNegativeCount", err)
	}
	if _, err := limiter.AllowAtMostN(ctx, "neg", -1); err != ErrNegativeCount {
		t.Errorf("AllowAtMostN(-1): got %v, want ErrNegativeCount", err)
	}
	if _, err := limiter.AllowAtMost(ctx, "neg", Limit{}, 1); err != ErrInvalidLimit {
		t.Errorf("AllowAtMost with an invalid limit: got %v, want ErrInvalidLimit", err)
	}
}

// PerSecond(10) is the case that used to expose the Lua script's float error:
// it computed the remaining count as 8.999999976, which Redis truncated to 8
// where the exact answer is 9. Both backends now work in exact integers.
func TestParity_PerSecondRemainingIsExact(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)

	for _, limit := range []Limit{PerSecond(10), PerSecond(3), PerSecond(7), PerSecond(13), PerMinute(11), PerHour(17)} {
		key := "exact:" + limit.String()
		redis := NewLimiter(client, WithRateLimit(limit))
		memLimiter := NewInMemoryLimiter(WithRateLimit(limit))
		redis.Reset(ctx, key)

		redisRes, err := redis.AllowN(ctx, key, 1)
		if err != nil {
			t.Fatalf("%v redis: %v", limit, err)
		}
		memRes, err := memLimiter.AllowN(ctx, key, 1)
		if err != nil {
			t.Fatalf("%v mem: %v", limit, err)
		}
		want := limit.Burst - 1
		if redisRes.Remaining != want {
			t.Errorf("%v redis Remaining: got %d, want the exact %d", limit, redisRes.Remaining, want)
		}
		if memRes.Remaining != want {
			t.Errorf("%v mem Remaining: got %d, want the exact %d", limit, memRes.Remaining, want)
		}
		redis.Reset(ctx, key)
		memLimiter.Close()
	}
}

// Drive both backends through the same randomised sequence of calls and require
// them to agree on every field that is not a wall-clock duration. Limits are
// kept at an emission interval of 100ms or more so the microseconds that elapse
// between the paired calls cannot refill a token and cause a false mismatch.
func TestParity_RandomisedSequences(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)

	limits := []Limit{
		PerSecond(10), PerSecond(3), PerSecond(7), PerSecond(1),
		PerMinute(13), PerMinute(60), PerHour(11), PerDay(5),
		{Rate: 4, Burst: 9, Period: time.Second},
		{Rate: 9, Burst: 4, Period: 2 * time.Second},
	}

	rnd := rand.New(rand.NewSource(seedFromEnv()))
	for _, limit := range limits {
		limit := limit
		t.Run(limit.String(), func(t *testing.T) {
			key := "rand:" + limit.String()
			redis := NewLimiter(client, WithRateLimit(limit))
			memLimiter := NewInMemoryLimiter(WithRateLimit(limit))
			defer memLimiter.Close()
			if err := redis.Reset(ctx, key); err != nil {
				t.Fatalf("reset: %v", err)
			}
			defer redis.Reset(ctx, key)

			for i := 0; i < 40; i++ {
				n := rnd.Intn(limit.Burst + 2) // sometimes over the burst
				atMost := rnd.Intn(2) == 0

				var redisRes, memRes *Result
				var err error
				if atMost {
					if redisRes, err = redis.AllowAtMostN(ctx, key, n); err != nil {
						t.Fatalf("step %d redis: %v", i, err)
					}
					if memRes, err = memLimiter.AllowAtMostN(ctx, key, n); err != nil {
						t.Fatalf("step %d mem: %v", i, err)
					}
				} else {
					if redisRes, err = redis.AllowN(ctx, key, n); err != nil {
						t.Fatalf("step %d redis: %v", i, err)
					}
					if memRes, err = memLimiter.AllowN(ctx, key, n); err != nil {
						t.Fatalf("step %d mem: %v", i, err)
					}
				}

				op := "AllowN"
				if atMost {
					op = "AllowAtMostN"
				}
				if redisRes.Allowed != memRes.Allowed {
					t.Fatalf("step %d %s(n=%d): Allowed redis=%d mem=%d", i, op, n, redisRes.Allowed, memRes.Allowed)
				}
				if redisRes.Remaining != memRes.Remaining {
					t.Fatalf("step %d %s(n=%d): Remaining redis=%d mem=%d", i, op, n, redisRes.Remaining, memRes.Remaining)
				}
				if (redisRes.RetryAfter < 0) != (memRes.RetryAfter < 0) {
					t.Fatalf("step %d %s(n=%d): RetryAfter sign redis=%v mem=%v", i, op, n, redisRes.RetryAfter, memRes.RetryAfter)
				}
			}
		})
	}
}

// AllowAtMost's explicit limit argument wins over a per-key custom limit. This
// is what the original code did, so preserving it keeps existing callers'
// behaviour unchanged; AllowAtMostN is the variant that resolves custom limits.
func TestAllowAtMost_ExplicitLimitBeatsCustomLimit(t *testing.T) {
	ctx := context.Background()
	key := "atmost:explicit"
	limiter, _ := newFakeLimiter(t, WithRateLimit(PerSecond(2)))
	limiter.SetCustomLimit(key, PerSecond(3))

	explicit := PerSecond(9)
	res, err := limiter.AllowAtMost(ctx, key, explicit, 9)
	if err != nil {
		t.Fatalf("AllowAtMost: %v", err)
	}
	if res.Limit != explicit {
		t.Errorf("AllowAtMost must use the explicit limit, got %v want %v", res.Limit, explicit)
	}
	if res.Allowed != 9 {
		t.Errorf("expected the explicit burst of 9 to be admitted, got %d", res.Allowed)
	}

	// AllowAtMostN on the same key resolves the custom limit of 3/s instead.
	fallbackLimiter, _ := newFakeLimiter(t, WithRateLimit(PerSecond(2)))
	fallbackLimiter.SetCustomLimit(key, PerSecond(3))
	res, err = fallbackLimiter.AllowAtMostN(ctx, key, 9)
	if err != nil {
		t.Fatalf("AllowAtMostN: %v", err)
	}
	if res.Limit != PerSecond(3) {
		t.Errorf("AllowAtMostN must resolve the custom limit, got %v", res.Limit)
	}
	if res.Allowed != 3 {
		t.Errorf("expected a clamp to the custom burst of 3, got %d", res.Allowed)
	}
}

// WithSweepInterval(0) must start no goroutine. Every deterministic test in
// this file depends on it: if a sweeper were running against the real clock
// while a test drove a fake one, those tests would fail as intermittent flakes
// rather than as an obvious regression.
func TestWithSweepInterval_ZeroStartsNoGoroutine(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	limiters := make([]*Limiter, 20)
	for i := range limiters {
		limiters[i] = NewInMemoryLimiter(WithSweepInterval(0))
	}
	if got := runtime.NumGoroutine(); got > before {
		t.Errorf("WithSweepInterval(0) started %d goroutine(s); want none", got-before)
	}
	for _, created := range limiters {
		if err := created.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	// Sweeping still works when driven by hand.
	limiter := NewInMemoryLimiter(WithSweepInterval(0), WithRateLimit(PerSecond(1)))
	defer limiter.Close()
	mem := memOf(t, limiter)
	clock := &fakeClock{}
	clock.ns.Store(int64(time.Hour))
	mem.now = clock.ns.Load

	limiter.Allow(context.Background(), "manual")
	clock.advance(2 * time.Second)
	if got := mem.sweep(); got != 1 {
		t.Errorf("manual sweep reclaimed %d keys, want 1", got)
	}
}

// seedFromEnv lets the differential test be replayed with a specific seed
// (PARITY_SEED=... go test -run RandomisedSequences) while defaulting to a
// fresh one on every run, so repeated runs explore different sequences.
func seedFromEnv() int64 {
	if s := os.Getenv("PARITY_SEED"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v
		}
	}
	return time.Now().UnixNano()
}
