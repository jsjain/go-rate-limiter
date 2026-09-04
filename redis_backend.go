package rate_limiter

import (
	"context"

	"github.com/redis/rueidis"
)

// redisBackend enforces limits with the GCRA Lua scripts, so the load, the
// computation and the store happen in one atomic round trip.
type redisBackend struct {
	rdb    rueidis.Client
	prefix string
}

func (b *redisBackend) allowN(ctx context.Context, key string, e limitEntry, n int) (*Result, error) {
	if !e.valid() {
		return nil, ErrInvalidLimit
	}
	if n < 0 {
		return nil, ErrNegativeCount
	}
	res, err := allowN.Exec(ctx, b.rdb, []string{b.prefix + key}, e.argv(n)).AsFloatSlice()
	if err != nil {
		return nil, err
	}
	return toResult(e.limit, res)
}

func (b *redisBackend) allowAtMost(ctx context.Context, key string, e limitEntry, n int) (*Result, error) {
	if !e.valid() {
		return nil, ErrInvalidLimit
	}
	if n < 0 {
		return nil, ErrNegativeCount
	}
	res, err := allowAtMost.Exec(ctx, b.rdb, []string{b.prefix + key}, e.argv(n)).AsFloatSlice()
	if err != nil {
		return nil, err
	}
	return toResult(e.limit, res)
}

func (b *redisBackend) reset(ctx context.Context, key string) error {
	cmd := b.rdb.B().Del().Key(b.prefix + key).Build()
	return b.rdb.Do(ctx, cmd).Error()
}

// close does not close the rueidis.Client: the caller owns it.
func (b *redisBackend) close() error { return nil }

func toResult(limit Limit, r []float64) (*Result, error) {
	if len(r) < 4 {
		return nil, ErrUnexpectedReply
	}
	return &Result{
		Limit:      limit,
		Allowed:    int(r[0]),
		Remaining:  int(r[1]),
		RetryAfter: dur(r[2]),
		ResetAfter: dur(r[3]),
	}, nil
}
