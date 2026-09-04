package rate_limiter

import (
	"context"
)

func (redis *redisBackend) allowN(ctx context.Context, key string, entry limitEntry, n int) (*Result, error) {
	if !entry.valid() {
		return nil, ErrInvalidLimit
	}
	if n < 0 {
		return nil, ErrNegativeCount
	}
	reply, err := allowN.Exec(ctx, redis.rdb, []string{redis.prefix + key}, entry.argv(n)).AsFloatSlice()
	if err != nil {
		return nil, err
	}
	return toResult(entry.limit, reply)
}

func (redis *redisBackend) allowAtMost(ctx context.Context, key string, entry limitEntry, n int) (*Result, error) {
	if !entry.valid() {
		return nil, ErrInvalidLimit
	}
	if n < 0 {
		return nil, ErrNegativeCount
	}
	reply, err := allowAtMost.Exec(ctx, redis.rdb, []string{redis.prefix + key}, entry.argv(n)).AsFloatSlice()
	if err != nil {
		return nil, err
	}
	return toResult(entry.limit, reply)
}

func (redis *redisBackend) reset(ctx context.Context, key string) error {
	cmd := redis.rdb.B().Del().Key(redis.prefix + key).Build()
	return redis.rdb.Do(ctx, cmd).Error()
}

// close does not close the rueidis.Client: the caller owns it.
func (redis *redisBackend) close() error { return nil }
