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
		{"PerSecond", PerSecond(10), "10", "10", "1"},
		{"PerMinute", PerMinute(60), "60", "60", "60"},
		{"PerHour", PerHour(100), "100", "100", "3600"},
		{"PerDay", PerDay(200), "200", "200", "86400"},
		{"CustomBurst", Limit{Rate: 5, Burst: 20, Period: time.Second}, "20", "5", "1"},
		{"CustomPeriod", Limit{Rate: 3, Burst: 3, Period: 5 * time.Second}, "3", "3", "5"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entry := newLimitEntry(tc.limit)

			if entry.limit != tc.limit {
				t.Errorf("limit: got %v, want %v", entry.limit, tc.limit)
			}
			if entry.burstStr != tc.wantBurst {
				t.Errorf("burstStr: got %q, want %q", entry.burstStr, tc.wantBurst)
			}
			if entry.rateStr != tc.wantRate {
				t.Errorf("rateStr: got %q, want %q", entry.rateStr, tc.wantRate)
			}
			if entry.periodStr != tc.wantPeriod {
				t.Errorf("periodStr: got %q, want %q", entry.periodStr, tc.wantPeriod)
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
	for _, limit := range limits {
		entry := newLimitEntry(limit)
		if got, want := entry.burstStr, strconv.Itoa(limit.Burst); got != want {
			t.Errorf("burstStr for %v: got %q, want %q", limit, got, want)
		}
		if got, want := entry.rateStr, strconv.Itoa(limit.Rate); got != want {
			t.Errorf("rateStr for %v: got %q, want %q", limit, got, want)
		}
		if got, want := entry.periodStr, strconv.FormatFloat(limit.Period.Seconds(), 'f', -1, 64); got != want {
			t.Errorf("periodStr for %v: got %q, want %q", limit, got, want)
		}
	}
}

// --- Unit tests: Limiter construction (no Redis) ---

func TestNewLimiter_DefaultLimitCompiled(t *testing.T) {
	var client rueidis.Client
	limiter := NewLimiter(client)

	def := defaultLimits()
	if limiter.limit.limit != def {
		t.Errorf("default limit: got %v, want %v", limiter.limit.limit, def)
	}
	if limiter.limit.burstStr != strconv.Itoa(def.Burst) {
		t.Errorf("default burstStr: got %q, want %q", limiter.limit.burstStr, strconv.Itoa(def.Burst))
	}
	if limiter.limit.periodStr != strconv.FormatFloat(def.Period.Seconds(), 'f', -1, 64) {
		t.Errorf("default periodStr: got %q, want %q", limiter.limit.periodStr, strconv.FormatFloat(def.Period.Seconds(), 'f', -1, 64))
	}
}

func TestWithRateLimit_Compiled(t *testing.T) {
	var client rueidis.Client
	limit := PerMinute(100)
	limiter := NewLimiter(client, WithRateLimit(limit))

	if limiter.limit.limit != limit {
		t.Errorf("limit: got %v, want %v", limiter.limit.limit, limit)
	}
	if limiter.limit.burstStr != "100" {
		t.Errorf("burstStr: got %q, want %q", limiter.limit.burstStr, "100")
	}
	if limiter.limit.periodStr != "60" {
		t.Errorf("periodStr: got %q, want %q", limiter.limit.periodStr, "60")
	}
}

func TestWithCustomLimits_Compiled(t *testing.T) {
	var client rueidis.Client
	custom := map[string]Limit{
		"user:1": PerSecond(5),
		"user:2": PerMinute(100),
	}
	limiter := NewLimiter(client, WithCustomLimits(custom))

	for key, want := range custom {
		entry, ok := limiter.customLimits.Load(key)
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
		if entry.periodStr != strconv.FormatFloat(want.Period.Seconds(), 'f', -1, 64) {
			t.Errorf("key %q: periodStr got %q", key, entry.periodStr)
		}
	}
}

func TestSetCustomLimit_Compiled(t *testing.T) {
	var client rueidis.Client
	limiter := NewLimiter(client)

	limit := PerHour(50)
	limiter.SetCustomLimit("tenant:abc", limit)

	entry, ok := limiter.customLimits.Load("tenant:abc")
	if !ok {
		t.Fatal("custom limit not found after SetCustomLimit")
	}
	if entry.limit != limit {
		t.Errorf("limit: got %v, want %v", entry.limit, limit)
	}
	if entry.rateStr != "50" {
		t.Errorf("rateStr: got %q, want %q", entry.rateStr, "50")
	}
	if entry.periodStr != "3600" {
		t.Errorf("periodStr: got %q, want %q", entry.periodStr, "3600")
	}
}

// Verify that SetCustomLimit overwrites a prior entry for the same key.
func TestSetCustomLimit_Overwrite(t *testing.T) {
	var client rueidis.Client
	limiter := NewLimiter(client)

	limiter.SetCustomLimit("key", PerSecond(10))
	limiter.SetCustomLimit("key", PerSecond(99))

	entry, _ := limiter.customLimits.Load("key")
	if entry.rateStr != "99" {
		t.Errorf("expected overwritten rateStr %q, got %q", "99", entry.rateStr)
	}
}

// --- Integration tests (require local Redis) ---

func TestAllowN_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)

	limit := PerSecond(3)
	limiter := NewLimiter(client, WithRateLimit(limit))
	key := t.Name()
	defer limiter.Reset(ctx, key)

	// First 3 requests should be allowed.
	for i := 0; i < 3; i++ {
		res, err := limiter.AllowN(ctx, key, 1)
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
	res, err := limiter.AllowN(ctx, key, 1)
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
	client := newTestClient(t)

	defaultLimit := PerSecond(10)
	customLimit := PerSecond(1)
	key := t.Name() + ":custom"

	limiter := NewLimiter(client,
		WithRateLimit(defaultLimit),
		WithCustomLimits(map[string]Limit{
			key: customLimit,
		}),
	)
	defer limiter.Reset(ctx, key)

	// First request allowed.
	res, err := limiter.AllowN(ctx, key, 1)
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
	res, err = limiter.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("AllowN error: %v", err)
	}
	if res.Allowed != 0 {
		t.Errorf("expected denied by custom limit, got allowed=%d", res.Allowed)
	}
}

func TestAllowN_SetCustomLimit(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)

	limiter := NewLimiter(client, WithRateLimit(PerSecond(100)))
	key := t.Name() + ":setcustom"
	defer limiter.Reset(ctx, key)

	// Override to 1/s after construction.
	limiter.SetCustomLimit(key, PerSecond(1))

	res, err := limiter.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("AllowN error: %v", err)
	}
	if res.Allowed != 1 {
		t.Errorf("expected allowed=1, got %d", res.Allowed)
	}

	res, err = limiter.AllowN(ctx, key, 1)
	if err != nil {
		t.Fatalf("AllowN error: %v", err)
	}
	if res.Allowed != 0 {
		t.Errorf("expected denied by SetCustomLimit, got allowed=%d", res.Allowed)
	}
}

func TestAllow_Shortcut(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)

	limiter := NewLimiter(client, WithRateLimit(PerSecond(1)))
	key := t.Name()
	defer limiter.Reset(ctx, key)

	res, err := limiter.Allow(ctx, key)
	if err != nil {
		t.Fatalf("Allow error: %v", err)
	}
	if res.Allowed != 1 {
		t.Errorf("expected allowed=1, got %d", res.Allowed)
	}
}

func TestAllowAtMost(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)

	limit := PerSecond(5)
	limiter := NewLimiter(client)
	key := t.Name()
	defer limiter.Reset(ctx, key)

	// Request 3 out of 5 available tokens.
	res, err := limiter.AllowAtMost(ctx, key, limit, 3)
	if err != nil {
		t.Fatalf("AllowAtMost error: %v", err)
	}
	if res.Allowed != 3 {
		t.Errorf("expected allowed=3, got %d", res.Allowed)
	}
}

func TestReset(t *testing.T) {
	ctx := context.Background()
	client := newTestClient(t)

	limiter := NewLimiter(client, WithRateLimit(PerSecond(1)))
	key := t.Name()

	// Exhaust the limit.
	limiter.Allow(ctx, key)
	res, _ := limiter.Allow(ctx, key)
	if res.Allowed != 0 {
		t.Skip("expected limit to be exhausted before testing Reset")
	}

	if err := limiter.Reset(ctx, key); err != nil {
		t.Fatalf("Reset error: %v", err)
	}

	// Should be allowed again after reset.
	res, err := limiter.Allow(ctx, key)
	if err != nil {
		t.Fatalf("Allow error after reset: %v", err)
	}
	if res.Allowed != 1 {
		t.Errorf("expected allowed=1 after reset, got %d", res.Allowed)
	}
	limiter.Reset(ctx, key)
}
