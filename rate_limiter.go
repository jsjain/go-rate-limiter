package rate_limiter

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/alphadose/haxmap"
	"github.com/redis/rueidis"
)

const redisPrefix = "rl:"

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

// Limiter controls how frequently events are allowed to happen.
type Limiter struct {
	rdb          rueidis.Client
	limit        Limit
	customLimits *haxmap.Map[string, Limit]
	prefix       string
}

type LimiterOption func(*Limiter)

func WithCustomLimits(limits *haxmap.Map[string, Limit]) LimiterOption {
	return func(l *Limiter) {
		l.customLimits = limits
	}
}

func WithRateLimit(limit Limit) LimiterOption {
	return func(l *Limiter) {
		l.limit = limit
	}
}

func WithPrefix(prefix string) LimiterOption {
	return func(l *Limiter) {
		l.prefix = prefix
	}
}

func defaultLimits() Limit {
	return Limit{
		Burst:  1,
		Rate:   1,
		Period: time.Second,
	}
}

// NewLimiter returns a new Limiter.
func NewLimiter(rdb rueidis.Client, opts ...LimiterOption) *Limiter {
	limiter := &Limiter{
		rdb:    rdb,
		limit:  defaultLimits(),
		prefix: redisPrefix,
	}
	for _, opt := range opts {
		opt(limiter)
	}

	if limiter.customLimits == nil {
		limiter.customLimits = haxmap.New[string, Limit]()
	}

	return limiter
}

// Allow is a shortcut for AllowN(ctx, key, limit, 1).
func (l Limiter) Allow(ctx context.Context, key string) (*Result, error) {
	return l.AllowN(ctx, key, 1)
}

// AllowN reports whether n events may happen at time now.
func (l Limiter) AllowN(
	ctx context.Context,
	key string,
	n int,
) (*Result, error) {
	limit := l.limit
	if cl, ok := l.customLimits.Get(key); ok {
		limit = cl
	}
	values := []string{strconv.Itoa(limit.Burst),
		strconv.Itoa(limit.Rate),
		strconv.FormatFloat(limit.Period.Seconds(), 'f', 2, 32),
		strconv.Itoa(n)}
	result, err := allowN.Exec(ctx, l.rdb, []string{redisPrefix + key}, values).AsFloatSlice()
	if err != nil {
		return nil, err
	}

	retryAfter := result[2]
	resetAfter := result[3]
	res := &Result{
		Limit:      limit,
		Allowed:    int(result[0]),
		Remaining:  int(result[1]),
		RetryAfter: dur(retryAfter),
		ResetAfter: dur(resetAfter),
	}
	return res, nil
}

// AllowAtMost reports whether at most n events may happen at time now.
// It returns number of allowed events that is less than or equal to n.
func (l Limiter) AllowAtMost(
	ctx context.Context,
	key string,
	limit Limit,
	n int,
) (*Result, error) {
	values := []string{strconv.Itoa(limit.Burst),
		strconv.Itoa(limit.Rate),
		strconv.FormatFloat(limit.Period.Seconds(), 'f', 2, 32),
		strconv.Itoa(n)}
	result, err := allowAtMost.Exec(ctx, l.rdb, []string{redisPrefix + key}, values).AsFloatSlice()
	if err != nil {
		return nil, err
	}

	retryAfter := result[2]
	resetAfter := result[3]

	res := &Result{
		Limit:      limit,
		Allowed:    int(result[0]),
		Remaining:  int(result[1]),
		RetryAfter: dur(retryAfter),
		ResetAfter: dur(resetAfter),
	}
	return res, nil
}

// Reset gets a key and reset all limitations and previous usages
func (l *Limiter) Reset(ctx context.Context, key string) error {
	cmd := l.rdb.B().Del().Key(redisPrefix + key).Build()
	return l.rdb.Do(ctx, cmd).Error()
}

func dur(f float64) time.Duration {
	if f == -1 {
		return -1
	}
	return time.Duration(f * float64(time.Second))
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
