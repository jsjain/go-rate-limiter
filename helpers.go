package rate_limiter

import (
	"math"
	"time"
)

func fmtDur(period time.Duration) string {
	switch period {
	case time.Second:
		return "s"
	case time.Minute:
		return "m"
	case time.Hour:
		return "h"
	}
	return period.String()
}

// dur converts a seconds count from the script into a Duration, passing through
// the -1 sentinel the script uses for "no retry needed".
func dur(seconds float64) time.Duration {
	if seconds == -1 {
		return -1
	}
	return time.Duration(seconds * float64(time.Second))
}

func toResult(limit Limit, reply []float64) (*Result, error) {
	if len(reply) < 4 {
		return nil, ErrUnexpectedReply
	}
	return &Result{
		Limit:      limit,
		Allowed:    int(reply[0]),
		Remaining:  int(reply[1]),
		RetryAfter: dur(reply[2]),
		ResetAfter: dur(reply[3]),
	}, nil
}

// satMul multiplies, saturating at MaxInt64 instead of wrapping.
func satMul(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > math.MaxInt64/b {
		return math.MaxInt64
	}
	return a * b
}
