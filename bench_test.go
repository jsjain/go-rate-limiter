package rate_limiter

import (
	"context"
	"strconv"
	"testing"

	"github.com/redis/rueidis"
)

// BenchmarkNewLimitEntry measures the per-call encoding that custom limits
// avoid by being compiled once at construction.
func BenchmarkNewLimitEntry(b *testing.B) {
	limit := PerSecond(100)
	b.ReportAllocs()
	for b.Loop() {
		_ = newLimitEntry(limit)
	}
}

func benchClient(b *testing.B) rueidis.Client {
	b.Helper()
	client, err := rueidis.NewClient(rueidis.ClientOption{
		InitAddress: []string{"localhost:6379"},
	})
	if err != nil {
		b.Skipf("redis unavailable: %v", err)
	}
	b.Cleanup(client.Close)
	return client
}

// 100k distinct keys, precomputed so key construction is not part of the measurement.
var benchKeys = func() []string {
	ks := make([]string, 100_000)
	for i := range ks {
		ks[i] = "bench:k:" + strconv.Itoa(i)
	}
	return ks
}()

// B1: single hot key, sequential.
func BenchmarkRedis_AllowN_HotKey(b *testing.B) {
	ctx := context.Background()
	limiter := NewLimiter(benchClient(b), WithRateLimit(PerSecond(1_000_000_000)))
	key := "bench:hot"
	defer limiter.Reset(ctx, key)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := limiter.AllowN(ctx, key, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// B1p: single hot key, all cores.
func BenchmarkRedis_AllowN_HotKey_Parallel(b *testing.B) {
	ctx := context.Background()
	limiter := NewLimiter(benchClient(b), WithRateLimit(PerSecond(1_000_000_000)))
	key := "bench:hotpar"
	defer limiter.Reset(ctx, key)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := limiter.AllowN(ctx, key, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// B2: 100k distinct keys, all cores.
func BenchmarkRedis_AllowN_ManyKeys_Parallel(b *testing.B) {
	ctx := context.Background()
	limiter := NewLimiter(benchClient(b), WithRateLimit(PerSecond(1_000_000_000)))

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := limiter.AllowN(ctx, benchKeys[i%len(benchKeys)], 1); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

// AllowAtMost isolates the per-call newLimitEntry cost (defect F2).
func BenchmarkRedis_AllowAtMost_HotKey(b *testing.B) {
	ctx := context.Background()
	limiter := NewLimiter(benchClient(b))
	limit := PerSecond(1_000_000_000)
	key := "bench:atmost"
	defer limiter.Reset(ctx, key)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := limiter.AllowAtMost(ctx, key, limit, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// AllowAtMostN resolves limits like AllowN and reuses the cached entry, so it
// avoids the per-call strconv that the legacy AllowAtMost signature keeps.
func BenchmarkRedis_AllowAtMostN_HotKey(b *testing.B) {
	ctx := context.Background()
	limiter := NewLimiter(benchClient(b), WithRateLimit(PerSecond(1_000_000_000)))
	key := "bench:atmostn"
	defer limiter.Reset(ctx, key)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := limiter.AllowAtMostN(ctx, key, 1); err != nil {
			b.Fatal(err)
		}
	}
}

// --- in-memory backend ---

func BenchmarkMem_AllowN_HotKey(b *testing.B) {
	ctx := context.Background()
	limiter := NewInMemoryLimiter(WithRateLimit(PerSecond(1_000_000_000)))
	defer limiter.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := limiter.AllowN(ctx, "bench:hot", 1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMem_AllowN_HotKey_Parallel(b *testing.B) {
	ctx := context.Background()
	limiter := NewInMemoryLimiter(WithRateLimit(PerSecond(1_000_000_000)))
	defer limiter.Close()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := limiter.AllowN(ctx, "bench:hotpar", 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkMem_AllowN_ManyKeys_Parallel(b *testing.B) {
	ctx := context.Background()
	limiter := NewInMemoryLimiter(WithRateLimit(PerSecond(1_000_000_000)))
	defer limiter.Close()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := limiter.AllowN(ctx, benchKeys[i%len(benchKeys)], 1); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

func BenchmarkMem_AllowAtMostN_HotKey(b *testing.B) {
	ctx := context.Background()
	limiter := NewInMemoryLimiter(WithRateLimit(PerSecond(1_000_000_000)))
	defer limiter.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := limiter.AllowAtMostN(ctx, "bench:atmostn", 1); err != nil {
			b.Fatal(err)
		}
	}
}

// Denied calls take the read-only path: no CAS, no store.
func BenchmarkMem_AllowN_Denied_Parallel(b *testing.B) {
	ctx := context.Background()
	limiter := NewInMemoryLimiter(WithRateLimit(PerSecond(1)))
	defer limiter.Close()
	limiter.AllowN(ctx, "bench:denied", 1) // exhaust

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := limiter.AllowN(ctx, "bench:denied", 1); err != nil {
				b.Fatal(err)
			}
		}
	})
}
