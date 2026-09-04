package rate_limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
	"github.com/redis/rueidis"
)

type Limit struct {
	Rate   int
	Burst  int
	Period time.Duration
}

// limitEntry caches everything derivable from a Limit so neither backend pays
// for it per call: the script's arguments for the Redis path, and the
// integer-nanosecond GCRA constants for the in-memory path.
//
// A zero ei marks the entry invalid. Constructors substitute the default limit
// rather than store one, so a bad Limit cannot arm a division by zero in a
// later request.
type limitEntry struct {
	limit     Limit
	burstStr  string
	rateStr   string
	periodStr string

	// args1 is the ARGV for the overwhelmingly common n == 1 call. rueidis
	// copies args into its own command buffer, so sharing it is safe.
	args1 []string

	// The script's arguments, in whole microseconds. Derived here rather than
	// in Lua so the script does no arithmetic per call.
	eiStr       string
	burstOffStr string

	ei       int64 // emission interval, nanoseconds per event
	burstOff int64 // ei * Burst
}

// backend is the seam between limit resolution and enforcement. It sits at the
// whole-operation level because the Redis implementation does load, compute and
// store in a single Lua round trip; splitting that into load and store would
// reintroduce a read-modify-write race across processes.
type backend interface {
	allowN(ctx context.Context, key string, entry limitEntry, n int) (*Result, error)
	allowAtMost(ctx context.Context, key string, entry limitEntry, n int) (*Result, error)
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

// redisBackend enforces limits with the GCRA Lua scripts, so the load, the
// computation and the store happen in one atomic round trip.
type redisBackend struct {
	rdb    rueidis.Client
	prefix string
}

// memBackend runs GCRA in this process. State per key is a theoretical arrival
// time in nanoseconds since a construction-time epoch, read through a monotonic
// clock so it cannot be skewed by the wall-clock jumps that Redis TIME is
// exposed to.
type memBackend struct {
	state *xsync.Map[string, *atomic.Int64]
	now   func() int64 // injectable so tests need no sleeps
	stop  chan struct{}
	once  sync.Once
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
