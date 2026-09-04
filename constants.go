package rate_limiter

import (
	"errors"
	"math"
	"time"
)

const redisPrefix = "rl:"

// defaultSweepInterval is how often an in-memory limiter reclaims keys that
// have fully reset. Tunable with WithSweepInterval.
const defaultSweepInterval = time.Minute

// tombstone marks a cell the sweeper has claimed. A writer whose CAS finds it
// must fetch a live cell from the map rather than treat it as a TAT.
const tombstone = math.MinInt64

var (
	// ErrInvalidLimit is returned when a limit reaches a backend with a
	// non-positive rate or period. Construction substitutes the default limit
	// before this can normally happen.
	ErrInvalidLimit = errors.New("rate_limiter: invalid limit")

	// ErrNegativeCount is returned for n < 0, which would refund tokens rather
	// than spend them.
	ErrNegativeCount = errors.New("rate_limiter: negative event count")

	ErrUnexpectedReply = errors.New("rate_limiter: unexpected reply from rate limit script")
)
