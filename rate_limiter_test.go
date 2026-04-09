package rate_limiter

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/rueidis"
)

// newTestClient connects to local Redis and fails the test if unavailable.
func newTestClient(t *testing.T) rueidis.Client {
	t.Helper()
	c, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{"localhost:6379"},
	})
	if err != nil {
		t.Skipf("redis unavailable: %v", err)
	}
	t.Cleanup(c.Close)
	return c
}

// --- Unit tests: limitEntry precomputation (no Redis) ---

func TestNewLimitEntry_Strings(t *testing.T) {
	cases := []struct {
		name       string
		limit      Limit
		wantBurst  string
		wantRate   string
		wantPeriod string
	}{
		{"PerSecond", PerSecond(10), "10", "10", "1.00"},
		{"PerMinute", PerMinute(60), "60", "60", "60.00"},
		{"PerHour", PerHour(100), "100", "100", "3600.00"},
		{"PerDay", PerDay(200), "200", "200", "86400.00"},
		{"CustomBurst", Limit{Rate: 5, Burst: 20, Period: time.Second}, "20", "5", "1.00"},
		{"CustomPeriod", Limit{Rate: 3, Burst: 3, Period: 5 * time.Second}, "3", "3", "5.00"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newLimitEntry(tc.limit)

			if e.limit != tc.limit {
				t.Errorf("limit: got %v, want %v", e.limit, tc.limit)
			}
			if e.burstStr != tc.wantBurst {
				t.Errorf("burstStr: got %q, want %q", e.burstStr, tc.wantBurst)
			}
			if e.rateStr != tc.wantRate {
				t.Errorf("rateStr: got %q, want %q", e.rateStr, tc.wantRate)
			}
			if e.periodStr != tc.wantPeriod {
				t.Errorf("periodStr: got %q, want %q", e.periodStr, tc.wantPeriod)
			}
		})
	}
}

// Ensure precomputed strings exactly match what strconv would produce.
func TestNewLimitEntry_MatchesStrconv(t *testing.T) {
	limits := []Limit{
		PerSecond(1), PerSecond(999),
		PerMinute(30), PerHour(500),
		PerDay(10000),
		{Rate: 7, Burst: 14, Period: 10 * time.Second},
	}
	for _, l := range limits {
		e := newLimitEntry(l)
		if got, want := e.burstStr, strconv.Itoa(l.Burst); got != want {
			t.Errorf("burstStr for %v: got %q, want %q", l, got, want)
		}
		if got, want := e.rateStr, strconv.Itoa(l.Rate); got != want {
			t.Errorf("rateStr for %v: got %q, want %q", l, got, want)
		}
		if got, want := e.periodStr, strconv.FormatFloat(l.Period.Seconds(), 'f', 2, 32); got != want {
			t.Errorf("periodStr for %v: got %q, want %q", l, got, want)
		}
	}
}

// --- Unit tests: Limiter construction (no Redis) ---

func TestNewLimiter_DefaultLimitCompiled(t *testing.T) {
	var c rueidis.Client
	l := NewLimiter(c)

	def := defaultLimits()
	if l.limit.limit != def {
		t.Errorf("default limit: got %v, want %v", l.limit.limit, def)
	}
	if l.limit.burstStr != strconv.Itoa(def.Burst) {
		t.Errorf("default burstStr: got %q, want %q", l.limit.burstStr, strconv.Itoa(def.Burst))
	}
	if l.limit.periodStr != strconv.FormatFloat(def.Period.Seconds(), 'f', 2, 32) {
		t.Errorf("default periodStr: got %q, want %q", l.limit.periodStr, strconv.FormatFloat(def.Period.Seconds(), 'f', 2, 32))
	}
}

func TestWithRateLimit_Compiled(t *testing.T) {
	var c rueidis.Client
	limit := PerMinute(100)
	l := NewLimiter(c, WithRateLimit(limit))

	if l.limit.limit != limit {
		t.Errorf("limit: got %v, want %v", l.limit.limit, limit)
	}
	if l.limit.burstStr != "100" {
		t.Errorf("burstStr: got %q, want %q", l.limit.burstStr, "100")
	}
	if l.limit.periodStr != "60.00" {
		t.Errorf("periodStr: got %q, want %q", l.limit.periodStr, "60.00")
	}
}

func TestWithCustomLimits_Compiled(t *testing.T) {
	var c rueidis.Client
	custom := map[string]Limit{
		"user:1": PerSecond(5),
		"user:2": PerMinute(100),
	}
	l := NewLimiter(c, WithCustomLimits(custom))

	for key, want := range custom {
		entry, ok := l.customLimits.Load(key)
		if !ok {
			t.Errorf("key %q not found in customLimits", key)
			continue
		}
		if entry.limit != want {
			t.Errorf("key %q: limit got %v, want %v", key, entry.limit, want)
		}
		if entry.burstStr != strconv.Itoa(want.Burst) {
			t.Errorf("key %q: burstStr got %q, want %q", key, entry.burstStr, strconv.Itoa(want.Burst))
		}
		if entry.periodStr != strconv.FormatFloat(want.Period.Seconds(), 'f', 2, 32) {
			t.Errorf("key %q: periodStr got %q", key, entry.periodStr)
		}
	}
}

func TestSetCustomLimit_Compiled(t *testing.T) {
	var c rueidis.Client
	l := NewLimiter(c)

	limit := PerHour(50)
	l.SetCustomLimit("tenant:abc", limit)

	entry, ok := l.customLimits.Load("tenant:abc")
	if !ok {
		t.Fatal("custom limit not found after SetCustomLimit")
	}
	if entry.limit != limit {
		t.Errorf("limit: got %v, want %v", entry.limit, limit)
	}
	if entry.rateStr != "50" {
		t.Errorf("rateStr: got %q, want %q", entry.rateStr, "50")
	}
	if entry.periodStr != "3600.00" {
		t.Errorf("periodStr: got %q, want %q", entry.periodStr, "3600.00")
	}
}

// Verify that SetCustomLimit overwrites a prior entry for the same key.
func TestSetCustomLimit_Overwrite(t *testing.T) {
	var c rueidis.Client
	l := NewLimiter(c)

	l.SetCustomLimit("key", PerSecond(10))
	l.SetCustomLimit("key", PerSecond(99))

	entry, _ := l.customLimits.Load("key")
	if entry.rateStr != "99" {
		t.Errorf("expected overwritten rateStr %q, got %q", "99", entry.rateStr)
	}
}

// --- Integration tests (require local Redis) ---

func TestAllowN_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	limit := PerSecond(3)
	l := NewLimiter(c, WithRateLimit(limit))
	key := t.Name()
	defer l.Reset(ctx, key)

	// First 3 requests should be allowed.
	for i := 0; i < 3; i++ {
		res, err := l.AllowN(ctx, key, 1)
		if err != nil {
			t.Fatalf("AllowN error: %v", err)
		}
		if res.Allowed != 1 {
			t.Errorf("request %d: expected allowed=1, got %d", i+1, res.Allowed)
		}
		if res.Limit != limit {
			t.Errorf("request %d: wrong limit returned", i+1)
		}
	}

	// 4th request should be denied.
	res, err := l.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("AllowN error: %v", err)
	}
	if res.Allowed != 0 {
		t.Errorf("expected denied on 4th request, got allowed=%d", res.Allowed)
	}
	if res.RetryAfter <= 0 {
		t.Errorf("expected positive RetryAfter when denied, got %v", res.RetryAfter)
	}
}

func TestAllowN_CustomLimitOverridesDefault(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	defaultLimit := PerSecond(10)
	customLimit := PerSecond(1)
	key := t.Name() + ":custom"

	l := NewLimiter(c,
		WithRateLimit(defaultLimit),
		WithCustomLimits(map[string]Limit{
			key: customLimit,
		}),
	)
	defer l.Reset(ctx, key)

	// First request allowed.
	res, err := l.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("AllowN error: %v", err)
	}
	if res.Allowed != 1 {
		t.Errorf("expected allowed=1, got %d", res.Allowed)
	}
	if res.Limit != customLimit {
		t.Errorf("expected custom limit %v, got %v", customLimit, res.Limit)
	}

	// Second request denied (custom limit is 1/s).
	res, err = l.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("AllowN error: %v", err)
	}
	if res.Allowed != 0 {
		t.Errorf("expected denied by custom limit, got allowed=%d", res.Allowed)
	}
}

func TestAllowN_SetCustomLimit(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	l := NewLimiter(c, WithRateLimit(PerSecond(100)))
	key := t.Name() + ":setcustom"
	defer l.Reset(ctx, key)

	// Override to 1/s after construction.
	l.SetCustomLimit(key, PerSecond(1))

	res, err := l.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("AllowN error: %v", err)
	}
	if res.Allowed != 1 {
		t.Errorf("expected allowed=1, got %d", res.Allowed)
	}

	res, err = l.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("AllowN error: %v", err)
	}
	if res.Allowed != 0 {
		t.Errorf("expected denied by SetCustomLimit, got allowed=%d", res.Allowed)
	}
}

func TestAllow_Shortcut(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	l := NewLimiter(c, WithRateLimit(PerSecond(1)))
	key := t.Name()
	defer l.Reset(ctx, key)

	res, err := l.Allow(ctx, key)
	if err != nil {
		t.Fatalf("Allow error: %v", err)
	}
	if res.Allowed != 1 {
		t.Errorf("expected allowed=1, got %d", res.Allowed)
	}
}

func TestAllowAtMost(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	limit := PerSecond(5)
	l := NewLimiter(c)
	key := t.Name()
	defer l.Reset(ctx, key)

	// Request 3 out of 5 available tokens.
	res, err := l.AllowAtMost(ctx, key, limit, 3)
	if err != nil {
		t.Fatalf("AllowAtMost error: %v", err)
	}
	if res.Allowed != 3 {
		t.Errorf("expected allowed=3, got %d", res.Allowed)
	}
}

func TestReset(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	l := NewLimiter(c, WithRateLimit(PerSecond(1)))
	key := t.Name()

	// Exhaust the limit.
	l.Allow(ctx, key)
	res, _ := l.Allow(ctx, key)
	if res.Allowed != 0 {
		t.Skip("expected limit to be exhausted before testing Reset")
	}

	if err := l.Reset(ctx, key); err != nil {
		t.Fatalf("Reset error: %v", err)
	}

	// Should be allowed again after reset.
	res, err := l.Allow(ctx, key)
	if err != nil {
		t.Fatalf("Allow error after reset: %v", err)
	}
	if res.Allowed != 1 {
		t.Errorf("expected allowed=1 after reset, got %d", res.Allowed)
	}
	l.Reset(ctx, key)
}

// --- Benchmarks ---

func BenchmarkNewLimitEntry(b *testing.B) {
	l := PerSecond(100)
	b.ReportAllocs()
	for b.Loop() {
		_ = newLimitEntry(l)
	}
}

func BenchmarkAllowN(b *testing.B) {
	c, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{"localhost:6379"},
	})
	if err != nil {
		b.Skipf("redis unavailable: %v", err)
	}
	defer c.Close()

	ctx := context.Background()
	l := NewLimiter(c, WithRateLimit(PerSecond(1_000_000)))
	key := "bench:allowN"
	defer l.Reset(ctx, key)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		l.AllowN(ctx, key, 1)
	}
}
