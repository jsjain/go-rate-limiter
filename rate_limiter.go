package rate_limiter

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/rueidis"
)

func (limit Limit) String() string {
	return fmt.Sprintf("%d req/%s (burst %d)", limit.Rate, fmtDur(limit.Period), limit.Burst)
}

func (limit Limit) IsZero() bool {
	return limit == Limit{}
}

func PerSecond(rate int) Limit {
	return Limit{
		Rate:   rate,
		Period: time.Second,
		Burst:  rate,
	}
}

func PerMinute(rate int) Limit {
	return Limit{
		Rate:   rate,
		Period: time.Minute,
		Burst:  rate,
	}
}

func PerHour(rate int) Limit {
	return Limit{
		Rate:   rate,
		Period: time.Hour,
		Burst:  rate,
	}
}

func PerDay(rate int) Limit {
	return Limit{
		Rate:   rate,
		Period: 24 * time.Hour,
		Burst:  rate,
	}
}

func defaultLimits() Limit {
	return Limit{
		Burst:  1,
		Rate:   1,
		Period: time.Second,
	}
}

//------------------------------------------------------------------------------

func newLimitEntry(limit Limit) limitEntry {
	entry := limitEntry{limit: limit}
	if limit.Rate <= 0 || limit.Period <= 0 || limit.Burst < 0 {
		return entry
	}
	emissionInterval := int64(limit.Period) / int64(limit.Rate)
	// Reject an interval finer than a nanosecond, or a burst offset that would
	// overflow.
	if emissionInterval <= 0 || int64(limit.Burst) > math.MaxInt64/emissionInterval {
		return entry
	}
	entry.ei = emissionInterval
	entry.burstOff = emissionInterval * int64(limit.Burst)
	entry.burstStr = strconv.Itoa(limit.Burst)
	entry.rateStr = strconv.Itoa(limit.Rate)
	// Full precision: 'f', 2 rendered any period under 10ms as "0.00".
	entry.periodStr = strconv.FormatFloat(limit.Period.Seconds(), 'f', -1, 64)

	// The script works in whole microseconds, the resolution Redis TIME
	// reports. A sub-microsecond interval is charged one microsecond.
	intervalUS := int64(limit.Period/time.Microsecond) / int64(limit.Rate)
	if intervalUS < 1 {
		intervalUS = 1
	}
	entry.eiStr = strconv.FormatInt(intervalUS, 10)
	entry.burstOffStr = strconv.FormatInt(intervalUS*int64(limit.Burst), 10)
	entry.args1 = []string{entry.eiStr, entry.burstOffStr, "1"}
	return entry
}

func (entry limitEntry) valid() bool { return entry.ei > 0 }

func (entry limitEntry) argv(n int) []string {
	if n == 1 {
		return entry.args1
	}
	return []string{entry.eiStr, entry.burstOffStr, strconv.Itoa(n)}
}

//------------------------------------------------------------------------------

// WithCustomLimits sets per-key rate limits, compiled once at construction. An
// invalid limit is ignored and the key falls back to the limiter's default.
func WithCustomLimits(limits map[string]Limit) LimiterOption {
	return func(limiter *Limiter) {
		for key, limit := range limits {
			if entry := newLimitEntry(limit); entry.valid() {
				limiter.customLimits.Store(key, entry)
			}
		}
	}
}

// WithRateLimit sets the default limit. An invalid limit is ignored and the
// package default is kept.
func WithRateLimit(limit Limit) LimiterOption {
	return func(limiter *Limiter) {
		if entry := newLimitEntry(limit); entry.valid() {
			limiter.limit = entry
		}
	}
}

// WithPrefix sets the Redis key prefix. It has no effect in memory, where the
// keyspace is private to the process.
func WithPrefix(prefix string) LimiterOption {
	return func(limiter *Limiter) {
		limiter.prefix = prefix
	}
}

// WithSweepInterval sets how often an in-memory limiter reclaims keys that have
// fully reset. A non-positive interval disables sweeping, which leaks memory on
// an unbounded keyspace. No effect on Redis, which expires keys via SET ... EX.
func WithSweepInterval(interval time.Duration) LimiterOption {
	return func(limiter *Limiter) {
		limiter.sweep = interval
	}
}

func newLimiter(opts ...LimiterOption) *Limiter {
	limiter := &Limiter{
		limit:        newLimitEntry(defaultLimits()),
		prefix:       redisPrefix,
		sweep:        defaultSweepInterval,
		customLimits: xsync.NewMap[string, limitEntry](),
	}
	for _, opt := range opts {
		opt(limiter)
	}
	return limiter
}

// NewLimiter returns a Limiter backed by Redis, enforcing limits across every
// process sharing the same Redis instance.
func NewLimiter(rdb rueidis.Client, opts ...LimiterOption) *Limiter {
	limiter := newLimiter(opts...)
	// Built after the options loop so WithPrefix is not silently dropped.
	limiter.backend = &redisBackend{rdb: rdb, prefix: limiter.prefix}
	return limiter
}

// NewInMemoryLimiter returns a Limiter that keeps all state in this process and
// never touches the network.
//
// The limit is enforced per process, not per service: N replicas each admit the
// full limit, and state is lost on restart, so a restarted process grants every
// key a fresh burst. Use NewLimiter when the limit must hold across instances.
//
// Call Close when done to stop the background sweeper.
func NewInMemoryLimiter(opts ...LimiterOption) *Limiter {
	limiter := newLimiter(opts...)
	limiter.backend = newMemBackend(limiter.sweep)
	return limiter
}

// SetCustomLimit adds or updates a per-key rate limit after construction. An
// invalid limit is ignored, leaving the key on the limiter's default.
func (limiter *Limiter) SetCustomLimit(key string, limit Limit) {
	if entry := newLimitEntry(limit); entry.valid() {
		limiter.customLimits.Store(key, entry)
	}
}

func (limiter Limiter) entryFor(key string) limitEntry {
	if entry, ok := limiter.customLimits.Load(key); ok {
		return entry
	}
	return limiter.limit
}

// Allow is a shortcut for AllowN(ctx, key, 1).
func (limiter Limiter) Allow(ctx context.Context, key string) (*Result, error) {
	return limiter.AllowN(ctx, key, 1)
}

// AllowN reports whether n events may happen at time now.
func (limiter Limiter) AllowN(ctx context.Context, key string, n int) (*Result, error) {
	return limiter.backend.allowN(ctx, key, limiter.entryFor(key), n)
}

// AllowAtMost reports whether at most n events may happen at time now.
// It returns number of allowed events that is less than or equal to n.
//
// The explicit limit argument always wins: per-key custom limits are not
// consulted. Use AllowAtMostN to resolve limits the way AllowN does.
func (limiter Limiter) AllowAtMost(ctx context.Context, key string, limit Limit, n int) (*Result, error) {
	entry := newLimitEntry(limit)
	if !entry.valid() {
		return nil, ErrInvalidLimit
	}
	return limiter.backend.allowAtMost(ctx, key, entry, n)
}

// AllowAtMostN reports whether at most n events may happen at time now, using
// the same limit resolution as AllowN.
func (limiter Limiter) AllowAtMostN(ctx context.Context, key string, n int) (*Result, error) {
	return limiter.backend.allowAtMost(ctx, key, limiter.entryFor(key), n)
}

// Reset gets a key and reset all limitations and previous usages
func (limiter *Limiter) Reset(ctx context.Context, key string) error {
	return limiter.backend.reset(ctx, key)
}

// Close releases resources held by the limiter. It stops the sweeper in memory
// and is a no-op on Redis, which never owns the rueidis.Client it was given.
// Close is safe to call more than once, and the limiter keeps answering calls
// afterwards; it simply stops reclaiming memory.
func (limiter *Limiter) Close() error {
	return limiter.backend.close()
}
