package rate_limiter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/rueidis"
)

const redisPrefix = "rl:"

// defaultSweepInterval is how often an in-memory limiter reclaims keys whose
// rate limit has fully reset. Tunable with WithSweepInterval.
const defaultSweepInterval = time.Minute

var (
	// ErrInvalidLimit is returned when a limit reaches a backend with a
	// non-positive rate or period. Construction normally falls back to the
	// default limit before this can happen.
	ErrInvalidLimit = errors.New("rate_limiter: invalid limit")

	// ErrNegativeCount is returned when AllowN or AllowAtMost is given n < 0,
	// which would refund tokens rather than spend them.
	ErrNegativeCount = errors.New("rate_limiter: negative event count")

	// ErrUnexpectedReply is returned when the Lua script's reply does not have
	// the four elements the caller expects.
	ErrUnexpectedReply = errors.New("rate_limiter: unexpected reply from rate limit script")
)

type Limit struct {
	Rate   int
	Burst  int
	Period time.Duration
}

func (l Limit) String() string {
	return fmt.Sprintf("%d req/%s (burst %d)", l.Rate, fmtDur(l.Period), l.Burst)
}

func (l Limit) IsZero() bool {
	return l == Limit{}
}

func fmtDur(d time.Duration) string {
	switch d {
	case time.Second:
		return "s"
	case time.Minute:
		return "m"
	case time.Hour:
		return "h"
	}
	return d.String()
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

//------------------------------------------------------------------------------

// limitEntry caches everything derivable from a Limit so neither backend pays
// for it per call: the string-encoded fields and ready-made ARGV for the Redis
// path, and the integer-nanosecond GCRA constants for the in-memory path.
//
// A zero ei marks the entry invalid. Constructors fall back to the limiter's
// default limit rather than storing one, so an invalid Limit can never arm a
// division by zero in a later request.
type limitEntry struct {
	limit     Limit
	burstStr  string
	rateStr   string
	periodStr string

	// args1 is the ARGV for the overwhelmingly common n == 1 call. rueidis
	// copies args into its own command buffer, so sharing it is safe.
	args1 []string

	// eiStr and burstOffStr are the script's arguments: the emission interval
	// and burst offset in whole microseconds. Deriving them here rather than in
	// Lua keeps the script free of per-call arithmetic.
	eiStr       string
	burstOffStr string

	ei       int64 // emission interval, nanoseconds per event
	burstOff int64 // ei * Burst
}

func newLimitEntry(l Limit) limitEntry {
	e := limitEntry{limit: l}
	if l.Rate <= 0 || l.Period <= 0 || l.Burst < 0 {
		return e
	}
	ei := int64(l.Period) / int64(l.Rate)
	if ei <= 0 || int64(l.Burst) > math.MaxInt64/ei {
		// Either finer than 1ns per token, or burstOff would overflow.
		return e
	}
	e.ei = ei
	e.burstOff = ei * int64(l.Burst)
	e.burstStr = strconv.Itoa(l.Burst)
	e.rateStr = strconv.Itoa(l.Rate)
	// Kept for the String form and for callers reading the entry; the script
	// itself no longer receives the period. Full precision, because 'f', 2
	// rendered any period under 10ms as "0.00".
	e.periodStr = strconv.FormatFloat(l.Period.Seconds(), 'f', -1, 64)

	// The script works in whole microseconds, which is the resolution Redis
	// TIME reports. A sub-microsecond interval is charged a microsecond.
	eiUS := int64(l.Period/time.Microsecond) / int64(l.Rate)
	if eiUS < 1 {
		eiUS = 1
	}
	e.eiStr = strconv.FormatInt(eiUS, 10)
	e.burstOffStr = strconv.FormatInt(eiUS*int64(l.Burst), 10)
	e.args1 = []string{e.eiStr, e.burstOffStr, "1"}
	return e
}

func (e limitEntry) valid() bool { return e.ei > 0 }

// argv returns the script arguments for n events, reusing the precomputed
// slice when n is 1.
func (e limitEntry) argv(n int) []string {
	if n == 1 {
		return e.args1
	}
	return []string{e.eiStr, e.burstOffStr, strconv.Itoa(n)}
}

//------------------------------------------------------------------------------

// backend is the seam between limit resolution and enforcement. It is
// deliberately at the whole-operation level: the Redis implementation does
// load, compute and store in a single Lua round trip, and splitting that into
// separate load/store calls would reintroduce a read-modify-write race across
// processes.
type backend interface {
	allowN(ctx context.Context, key string, e limitEntry, n int) (*Result, error)
	allowAtMost(ctx context.Context, key string, e limitEntry, n int) (*Result, error)
	reset(ctx context.Context, key string) error
	close() error
}

// Limiter controls how frequently events are allowed to happen.
type Limiter struct {
	backend      backend
	limit        limitEntry
	customLimits *xsync.Map[string, limitEntry]

	// Construction-time settings, read by the constructors after options run.
	prefix string
	sweep  time.Duration
}

type LimiterOption func(*Limiter)

// WithCustomLimits sets per-key rate limits. These are compiled once at
// construction time, so per-call strconv overhead is avoided. An invalid limit
// is ignored and the key falls back to the limiter's default limit.
func WithCustomLimits(limits map[string]Limit) LimiterOption {
	return func(l *Limiter) {
		for k, v := range limits {
			if e := newLimitEntry(v); e.valid() {
				l.customLimits.Store(k, e)
			}
		}
	}
}

// WithRateLimit sets the default limit. An invalid limit is ignored and the
// package default is kept.
func WithRateLimit(limit Limit) LimiterOption {
	return func(l *Limiter) {
		if e := newLimitEntry(limit); e.valid() {
			l.limit = e
		}
	}
}

// WithPrefix sets the Redis key prefix. It has no effect on an in-memory
// limiter, whose keyspace is private to the process.
func WithPrefix(prefix string) LimiterOption {
	return func(l *Limiter) {
		l.prefix = prefix
	}
}

// WithSweepInterval sets how often an in-memory limiter reclaims keys that
// have fully reset. It has no effect on a Redis limiter, where expiry is
// handled by the SET ... EX in the Lua script. A non-positive interval
// disables sweeping, which leaks memory on an unbounded keyspace.
func WithSweepInterval(d time.Duration) LimiterOption {
	return func(l *Limiter) {
		l.sweep = d
	}
}

func defaultLimits() Limit {
	return Limit{
		Burst:  1,
		Rate:   1,
		Period: time.Second,
	}
}

func newLimiter(opts ...LimiterOption) *Limiter {
	l := &Limiter{
		limit:        newLimitEntry(defaultLimits()),
		prefix:       redisPrefix,
		sweep:        defaultSweepInterval,
		customLimits: xsync.NewMap[string, limitEntry](),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// NewLimiter returns a new Limiter backed by Redis, enforcing limits across
// every process sharing the same Redis instance.
func NewLimiter(rdb rueidis.Client, opts ...LimiterOption) *Limiter {
	l := newLimiter(opts...)
	// Built after the options loop so WithPrefix is not silently dropped.
	l.backend = &redisBackend{rdb: rdb, prefix: l.prefix}
	return l
}

// NewInMemoryLimiter returns a new Limiter that keeps all state in this
// process and never touches the network.
//
// The limit is enforced per process, not per service: N replicas each admit
// the full limit, and state is lost on restart, so a restarted process grants
// every key a fresh burst. Use NewLimiter when the limit must hold across
// instances.
//
// Call Close when done to stop the background sweeper.
func NewInMemoryLimiter(opts ...LimiterOption) *Limiter {
	l := newLimiter(opts...)
	l.backend = newMemBackend(l.sweep)
	return l
}

// SetCustomLimit adds or updates a per-key rate limit after construction. An
// invalid limit is ignored, leaving the key on the limiter's default limit.
func (l *Limiter) SetCustomLimit(key string, limit Limit) {
	if e := newLimitEntry(limit); e.valid() {
		l.customLimits.Store(key, e)
	}
}

// entryFor resolves the limit for a key: a per-key override if one exists,
// otherwise the limiter's default.
func (l Limiter) entryFor(key string) limitEntry {
	if e, ok := l.customLimits.Load(key); ok {
		return e
	}
	return l.limit
}

// Allow is a shortcut for AllowN(ctx, key, 1).
func (l Limiter) Allow(ctx context.Context, key string) (*Result, error) {
	return l.AllowN(ctx, key, 1)
}

// AllowN reports whether n events may happen at time now.
func (l Limiter) AllowN(ctx context.Context, key string, n int) (*Result, error) {
	return l.backend.allowN(ctx, key, l.entryFor(key), n)
}

// AllowAtMost reports whether at most n events may happen at time now.
// It returns number of allowed events that is less than or equal to n.
//
// The explicit limit argument always wins: per-key custom limits are not
// consulted. Use AllowAtMostN to resolve limits the way AllowN does.
func (l Limiter) AllowAtMost(ctx context.Context, key string, limit Limit, n int) (*Result, error) {
	e := newLimitEntry(limit)
	if !e.valid() {
		return nil, ErrInvalidLimit
	}
	return l.backend.allowAtMost(ctx, key, e, n)
}

// AllowAtMostN reports whether at most n events may happen at time now, using
// the same limit resolution as AllowN: a per-key custom limit if one is set,
// otherwise the limiter's default.
func (l Limiter) AllowAtMostN(ctx context.Context, key string, n int) (*Result, error) {
	return l.backend.allowAtMost(ctx, key, l.entryFor(key), n)
}

// Reset gets a key and reset all limitations and previous usages
func (l *Limiter) Reset(ctx context.Context, key string) error {
	return l.backend.reset(ctx, key)
}

// Close releases resources held by the limiter. It stops the sweeper on an
// in-memory limiter and is a no-op on a Redis limiter, which never owns the
// rueidis.Client it was given. Close is safe to call more than once, and the
// limiter keeps answering calls afterwards; it simply stops reclaiming memory.
func (l *Limiter) Close() error {
	return l.backend.close()
}

// dur converts a microsecond count from the script into a Duration, passing
// through the -1 sentinel the script uses for "no retry needed".
func dur(us float64) time.Duration {
	if us == -1 {
		return -1
	}
	return time.Duration(us) * time.Microsecond
}

type Result struct {
	// Limit is the limit that was used to obtain this result.
	Limit Limit

	// Allowed is the number of events that may happen at time now.
	Allowed int

	// Remaining is the maximum number of requests that could be
	// permitted instantaneously for this key given the current
	// state. For example, if a rate limiter allows 10 requests per
	// second and has already received 6 requests for this key this
	// second, Remaining would be 4.
	Remaining int

	// RetryAfter is the time until the next request will be permitted.
	// It should be -1 unless the rate limit has been exceeded.
	RetryAfter time.Duration

	// ResetAfter is the time until the RateLimiter returns to its
	// initial state for a given key. For example, if a rate limiter
	// manages requests per second and received one request 200ms ago,
	// Reset would return 800ms. You can also think of this as the time
	// until Limit and Remaining will be equal.
	ResetAfter time.Duration
}
