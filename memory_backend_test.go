package rate_limiter

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- helpers ---

func memOf(t testing.TB, l *Limiter) *memBackend {
	t.Helper()
	b, ok := l.backend.(*memBackend)
	if !ok {
		t.Fatalf("expected memBackend, got %T", l.backend)
	}
	return b
}

// fakeClock replaces the backend's monotonic clock so tests can move time
// without sleeping.
type fakeClock struct{ ns atomic.Int64 }

func (c *fakeClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

// newFakeLimiter returns an in-memory limiter driven by a controllable clock,
// with the background sweeper disabled so tests drive sweep() themselves.
func newFakeLimiter(t testing.TB, opts ...LimiterOption) (*Limiter, *fakeClock) {
	t.Helper()
	l := NewInMemoryLimiter(append([]LimiterOption{WithSweepInterval(0)}, opts...)...)
	c := &fakeClock{}
	c.ns.Store(int64(time.Hour)) // start away from the epoch
	memOf(t, l).now = c.ns.Load
	t.Cleanup(func() { l.Close() })
	return l, c
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
	c := newTestClient(t)

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
			redis := NewLimiter(c, WithRateLimit(tc.limit))
			mem := NewInMemoryLimiter(WithRateLimit(tc.limit))
			defer mem.Close()

			key := "parity:" + t.Name()
			if err := redis.Reset(ctx, key); err != nil {
				t.Fatalf("reset: %v", err)
			}
			defer redis.Reset(ctx, key)

			for i, s := range tc.steps {
				rr, err := redis.AllowN(ctx, key, s.n)
				if err != nil {
					t.Fatalf("step %d redis: %v", i, err)
				}
				mr, err := mem.AllowN(ctx, key, s.n)
				if err != nil {
					t.Fatalf("step %d mem: %v", i, err)
				}
				if rr.Allowed != s.wantAllowed || rr.Remaining != s.wantRemain {
					t.Errorf("step %d redis: allowed=%d remaining=%d, want %d/%d",
						i, rr.Allowed, rr.Remaining, s.wantAllowed, s.wantRemain)
				}
				if mr.Allowed != rr.Allowed || mr.Remaining != rr.Remaining {
					t.Errorf("step %d divergence: mem allowed=%d remaining=%d, redis allowed=%d remaining=%d",
						i, mr.Allowed, mr.Remaining, rr.Allowed, rr.Remaining)
				}
				// RetryAfter must agree on sign: -1 when allowed, positive when not.
				if (mr.RetryAfter < 0) != (rr.RetryAfter < 0) {
					t.Errorf("step %d RetryAfter sign: mem=%v redis=%v", i, mr.RetryAfter, rr.RetryAfter)
				}
			}
		})
	}
}

// n > burst is a permanent denial: waiting cannot help, so ResetAfter is zero
// on an otherwise idle key.
func TestParity_OverBurstIsPermanent(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	limit := PerMinute(3)
	key := "parity:overburst"

	redis := NewLimiter(c, WithRateLimit(limit))
	mem := NewInMemoryLimiter(WithRateLimit(limit))
	defer mem.Close()
	redis.Reset(ctx, key)
	defer redis.Reset(ctx, key)

	for name, l := range map[string]*Limiter{"redis": redis, "mem": mem} {
		res, err := l.AllowN(ctx, key, 4)
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
	c := newTestClient(t)
	limit := PerMinute(1)
	key := "parity:reset"

	redis := NewLimiter(c, WithRateLimit(limit))
	mem := NewInMemoryLimiter(WithRateLimit(limit))
	defer mem.Close()
	redis.Reset(ctx, key)
	defer redis.Reset(ctx, key)

	for name, l := range map[string]*Limiter{"redis": redis, "mem": mem} {
		l.Allow(ctx, key)
		if res, _ := l.Allow(ctx, key); res.Allowed != 0 {
			t.Fatalf("%s: expected exhausted", name)
		}
		if err := l.Reset(ctx, key); err != nil {
			t.Fatalf("%s reset: %v", name, err)
		}
		if res, _ := l.Allow(ctx, key); res.Allowed != 1 {
			t.Errorf("%s: expected allowed after reset, got %d", name, res.Allowed)
		}
	}
}

// --- T2 continued: RetryAfter is exact, not merely positive ---

func TestInMemory_RetryAfterIsExact(t *testing.T) {
	ctx := context.Background()
	l, clock := newFakeLimiter(t, WithRateLimit(PerSecond(4))) // emission interval 250ms
	key := "retryafter"

	for i := 0; i < 4; i++ {
		if res, _ := l.AllowN(ctx, key, 1); res.Allowed != 1 {
			t.Fatalf("request %d should be allowed", i)
		}
	}
	res, _ := l.AllowN(ctx, key, 1)
	if res.Allowed != 0 {
		t.Fatal("5th request should be denied")
	}
	if res.RetryAfter != 250*time.Millisecond {
		t.Errorf("RetryAfter: got %v, want 250ms", res.RetryAfter)
	}

	// One nanosecond short of RetryAfter: still denied.
	clock.advance(res.RetryAfter - 1)
	if r, _ := l.AllowN(ctx, key, 1); r.Allowed != 0 {
		t.Errorf("expected denial 1ns before RetryAfter elapses, got allowed=%d", r.Allowed)
	}
	// Exactly at RetryAfter: allowed.
	clock.advance(1)
	if r, _ := l.AllowN(ctx, key, 1); r.Allowed != 1 {
		t.Errorf("expected admission exactly at RetryAfter, got allowed=%d", r.Allowed)
	}
}

func TestInMemory_ResetAfterIsExact(t *testing.T) {
	ctx := context.Background()
	l, _ := newFakeLimiter(t, WithRateLimit(PerSecond(4)))

	res, _ := l.AllowN(ctx, "resetafter", 2)
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
	l, _ := newFakeLimiter(t,
		WithRateLimit(PerSecond(100)),
		WithCustomLimits(map[string]Limit{key: PerSecond(1)}),
	)

	if res, _ := l.Allow(ctx, key); res.Allowed != 1 {
		t.Fatal("first call should be allowed")
	}
	if res, _ := l.Allow(ctx, key); res.Allowed != 0 {
		t.Error("custom limit of 1/s should deny the second call")
	}
	// A key without an override uses the default.
	if res, _ := l.Allow(ctx, "other"); res.Allowed != 1 {
		t.Error("default limit should admit an unrelated key")
	}
}

func TestInMemory_AllowAtMostClamps(t *testing.T) {
	ctx := context.Background()
	l, _ := newFakeLimiter(t, WithRateLimit(PerSecond(10)))
	key := "atmost"

	if res, _ := l.AllowAtMostN(ctx, key, 7); res.Allowed != 7 {
		t.Fatalf("expected 7 allowed, got %d", res.Allowed)
	}
	// Only 3 remain, so a request for 5 is clamped down to 3.
	res, _ := l.AllowAtMostN(ctx, key, 5)
	if res.Allowed != 3 {
		t.Errorf("expected clamp to 3, got %d", res.Allowed)
	}
	if res.Remaining != 0 {
		t.Errorf("expected Remaining=0 after clamp, got %d", res.Remaining)
	}
	if res, _ := l.AllowAtMostN(ctx, key, 1); res.Allowed != 0 {
		t.Errorf("expected denial once exhausted, got %d", res.Allowed)
	}
}

// --- T3: concurrency ---

// A burst of B must admit exactly B, no matter how many goroutines race.
func TestInMemory_ConcurrentExactBurst(t *testing.T) {
	ctx := context.Background()
	const burst = 50
	l, _ := newFakeLimiter(t, WithRateLimit(PerMinute(burst)))
	key := "concurrent"

	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				res, err := l.Allow(ctx, key)
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
	l, _ := newFakeLimiter(t, WithRateLimit(PerSecond(2)))
	b := memOf(t, l)
	key := "denywrite"

	l.AllowN(ctx, key, 2) // exhaust
	cell, _ := b.state.Load(key)
	before := cell.Load()

	for i := 0; i < 10; i++ {
		if res, _ := l.Allow(ctx, key); res.Allowed != 0 {
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
	l, _ := newFakeLimiter(t, WithRateLimit(PerMinute(burst)))
	b := memOf(t, l)

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
				b.sweep()
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
				res, err := l.Allow(ctx, k)
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
	l, clock := newFakeLimiter(t, WithRateLimit(PerSecond(1)))
	b := memOf(t, l)

	for _, k := range []string{"a", "b", "c"} {
		if res, _ := l.Allow(ctx, k); res.Allowed != 1 {
			t.Fatalf("key %q should be allowed", k)
		}
	}
	if n := b.state.Size(); n != 3 {
		t.Fatalf("expected 3 live keys, got %d", n)
	}

	// Not yet expired: the sweeper must leave them alone.
	if got := b.sweep(); got != 0 {
		t.Errorf("swept %d keys before they expired, want 0", got)
	}
	if n := b.state.Size(); n != 3 {
		t.Errorf("expected 3 live keys after an early sweep, got %d", n)
	}

	clock.advance(2 * time.Second) // past every TAT
	if got := b.sweep(); got != 3 {
		t.Errorf("swept %d keys, want 3", got)
	}
	if n := b.state.Size(); n != 0 {
		t.Errorf("expected an empty map after sweeping, got %d", n)
	}

	// A reclaimed key still behaves correctly: it is simply idle again.
	if res, _ := l.Allow(ctx, "a"); res.Allowed != 1 {
		t.Errorf("expected a reclaimed key to be admitted, got %d", res.Allowed)
	}
}

func TestInMemory_ResetDropsKey(t *testing.T) {
	ctx := context.Background()
	l, _ := newFakeLimiter(t, WithRateLimit(PerSecond(1)))
	b := memOf(t, l)

	l.Allow(ctx, "gone")
	if _, ok := b.state.Load("gone"); !ok {
		t.Fatal("expected the key to exist")
	}
	if err := l.Reset(ctx, "gone"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, ok := b.state.Load("gone"); ok {
		t.Error("expected the key to be removed after Reset")
	}
	// Reset on an unknown key is not an error.
	if err := l.Reset(ctx, "never-seen"); err != nil {
		t.Errorf("reset of an unknown key: %v", err)
	}
}

// --- T5: lifecycle ---

func TestInMemory_CloseIsIdempotentAndKeepsWorking(t *testing.T) {
	ctx := context.Background()
	l := NewInMemoryLimiter(WithRateLimit(PerSecond(2)), WithSweepInterval(time.Millisecond))

	if err := l.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Closing stops eviction, not enforcement.
	if res, err := l.Allow(ctx, "after-close"); err != nil || res.Allowed != 1 {
		t.Errorf("expected the limiter to keep working after Close, got allowed=%d err=%v", res.Allowed, err)
	}
}

func TestRedis_CloseDoesNotCloseClient(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	l := NewLimiter(c, WithRateLimit(PerSecond(5)))
	key := "close:client"
	defer l.Reset(ctx, key)

	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The caller owns the client, so it must still be usable.
	if res, err := l.Allow(ctx, key); err != nil || res.Allowed != 1 {
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
		if e := newLimitEntry(limit); e.valid() {
			t.Errorf("limit %+v should be rejected as invalid", limit)
		}
	}

	// An invalid default keeps the package default rather than panicking.
	l := NewInMemoryLimiter(WithRateLimit(Limit{}))
	defer l.Close()
	if l.limit.limit != defaultLimits() {
		t.Errorf("invalid WithRateLimit should keep the default, got %v", l.limit.limit)
	}

	// An invalid override is not stored, so the key uses the default limit.
	l2 := NewInMemoryLimiter(WithRateLimit(PerSecond(2)))
	defer l2.Close()
	l2.SetCustomLimit("k", Limit{Rate: 0})
	if _, ok := l2.customLimits.Load("k"); ok {
		t.Error("invalid SetCustomLimit should not be stored")
	}
	for i := 0; i < 2; i++ {
		if res, err := l2.Allow(ctx, "k"); err != nil || res.Allowed != 1 {
			t.Fatalf("call %d: expected the default limit to apply, got allowed=%d err=%v", i, res.Allowed, err)
		}
	}
	if res, _ := l2.Allow(ctx, "k"); res.Allowed != 0 {
		t.Error("expected the default limit of 2/s to deny the third call")
	}
}

func TestNegativeCountRejected(t *testing.T) {
	ctx := context.Background()
	l, _ := newFakeLimiter(t, WithRateLimit(PerSecond(5)))

	if _, err := l.AllowN(ctx, "neg", -1); err != ErrNegativeCount {
		t.Errorf("AllowN(-1): got %v, want ErrNegativeCount", err)
	}
	if _, err := l.AllowAtMostN(ctx, "neg", -1); err != ErrNegativeCount {
		t.Errorf("AllowAtMostN(-1): got %v, want ErrNegativeCount", err)
	}
	if _, err := l.AllowAtMost(ctx, "neg", Limit{}, 1); err != ErrInvalidLimit {
		t.Errorf("AllowAtMost with an invalid limit: got %v, want ErrInvalidLimit", err)
	}
}

// The Lua script computes `remaining` in floating point, so a limit whose
// emission interval is not exactly representable loses a token in the report:
// PerSecond(10) makes Lua evaluate 0.9/0.1 as 8.999999999999998, which Redis
// truncates to 8. The in-memory backend uses integer nanoseconds and reports
// the exact 9.
//
// This predates the in-memory backend and affects only the reported Remaining,
// not the admit/deny decision. The test pins it so the difference is a known
// quantity rather than a surprise.
func TestKnownDivergence_RedisFloatLosesAToken(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	limit := PerSecond(10)
	key := "divergence:persecond10"

	redis := NewLimiter(c, WithRateLimit(limit))
	mem := NewInMemoryLimiter(WithRateLimit(limit))
	defer mem.Close()
	redis.Reset(ctx, key)
	defer redis.Reset(ctx, key)

	rr, err := redis.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	mr, err := mem.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("mem: %v", err)
	}

	if mr.Remaining != 9 {
		t.Errorf("in-memory Remaining: got %d, want the exact 9", mr.Remaining)
	}
	if rr.Remaining != 8 {
		t.Errorf("Redis Remaining: got %d, want 8. If this now reads 9 the float "+
			"truncation is gone, the backends agree, and this test plus the README "+
			"note should be deleted. Any other value means the drift changed and the "+
			"documented divergence is wrong.", rr.Remaining)
	}
	if rr.Allowed != mr.Allowed {
		t.Errorf("admit/deny must not diverge: redis=%d mem=%d", rr.Allowed, mr.Allowed)
	}
}

// AllowAtMost's explicit limit argument wins over a per-key custom limit. This
// is what the original code did, so preserving it keeps existing callers'
// behaviour unchanged; AllowAtMostN is the variant that resolves custom limits.
func TestAllowAtMost_ExplicitLimitBeatsCustomLimit(t *testing.T) {
	ctx := context.Background()
	key := "atmost:explicit"
	l, _ := newFakeLimiter(t, WithRateLimit(PerSecond(2)))
	l.SetCustomLimit(key, PerSecond(3))

	explicit := PerSecond(9)
	res, err := l.AllowAtMost(ctx, key, explicit, 9)
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
	l2, _ := newFakeLimiter(t, WithRateLimit(PerSecond(2)))
	l2.SetCustomLimit(key, PerSecond(3))
	res, err = l2.AllowAtMostN(ctx, key, 9)
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
	for _, l := range limiters {
		if err := l.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	// Sweeping still works when driven by hand.
	l := NewInMemoryLimiter(WithSweepInterval(0), WithRateLimit(PerSecond(1)))
	defer l.Close()
	b := memOf(t, l)
	clock := &fakeClock{}
	clock.ns.Store(int64(time.Hour))
	b.now = clock.ns.Load

	l.Allow(context.Background(), "manual")
	clock.advance(2 * time.Second)
	if got := b.sweep(); got != 1 {
		t.Errorf("manual sweep reclaimed %d keys, want 1", got)
	}
}
