package rate_limiter

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
)

// tombstone marks a cell the sweeper has claimed. A writer whose CAS finds it
// must go back to the map for a live cell instead of treating it as a TAT.
const tombstone = math.MinInt64

// memBackend runs GCRA entirely in this process. State per key is a single
// theoretical arrival time held as nanoseconds since a construction-time
// epoch, read through a monotonic clock so it cannot be skewed by wall-clock
// jumps the way the Redis TIME command can.
type memBackend struct {
	state *xsync.Map[string, *atomic.Int64]
	now   func() int64 // injectable so tests need no sleeps
	stop  chan struct{}
	once  sync.Once
}

func newMemBackend(sweep time.Duration) *memBackend {
	epoch := time.Now()
	b := &memBackend{
		state: xsync.NewMap[string, *atomic.Int64](),
		now:   func() int64 { return int64(time.Since(epoch)) },
		stop:  make(chan struct{}),
	}
	if sweep > 0 {
		go b.sweepLoop(sweep)
	}
	return b
}

// cell returns the live TAT cell for key, creating it if absent. A fresh cell
// holds 0, which is the epoch and therefore always in the past, so the first
// request needs no special case: max(0, now) is now.
//
// The common path is xsync's lock-free lookup. Only a tombstoned cell falls
// through to Compute, which takes the bucket lock and so serialises against
// the sweeper rather than spinning while it finishes its delete.
func (b *memBackend) cell(key string) *atomic.Int64 {
	p, _ := b.state.LoadOrCompute(key, func() (*atomic.Int64, bool) {
		return new(atomic.Int64), false
	})
	if p.Load() != tombstone {
		return p
	}
	p, _ = b.state.Compute(key, func(cur *atomic.Int64, loaded bool) (*atomic.Int64, xsync.ComputeOp) {
		if loaded && cur.Load() != tombstone {
			return cur, xsync.CancelOp
		}
		return new(atomic.Int64), xsync.UpdateOp
	})
	return p
}

// allowN mirrors the allowN Lua script in integer nanoseconds.
func (b *memBackend) allowN(_ context.Context, key string, e limitEntry, n int) (*Result, error) {
	if !e.valid() {
		return nil, ErrInvalidLimit
	}
	if n < 0 {
		return nil, ErrNegativeCount
	}

	p := b.cell(key)
	for {
		now := b.now()
		loaded := p.Load()
		if loaded == tombstone {
			p = b.cell(key)
			continue
		}
		tat := max(loaded, now)

		// More events than the burst can ever hold: deny with the same values
		// the script produces, without computing ei*n and overflowing.
		if int64(n) > int64(e.limit.Burst) {
			return &Result{
				Limit:      e.limit,
				RetryAfter: time.Duration((tat - now) + satMul(e.ei, int64(n)-int64(e.limit.Burst))),
				ResetAfter: time.Duration(tat - now),
			}, nil
		}

		newTAT := tat + e.ei*int64(n)
		diff := now - (newTAT - e.burstOff)
		if diff < 0 {
			return &Result{
				Limit:      e.limit,
				RetryAfter: time.Duration(-diff),
				ResetAfter: time.Duration(tat - now),
			}, nil
		}

		// Nothing to persist (n == 0 on an idle key). Matches the script's
		// `if reset_after > 0` guard, and keeps probe calls from creating
		// entries the sweeper would then have to reclaim.
		if newTAT <= now {
			return &Result{
				Limit:      e.limit,
				Allowed:    n,
				Remaining:  int(diff / e.ei),
				RetryAfter: -1,
			}, nil
		}

		// On failure another writer won, or the sweeper tombstoned the cell.
		// Retry re-reads now as well as loaded: computing against a stale now
		// can deny a request by the width of the retry window.
		if !p.CompareAndSwap(loaded, newTAT) {
			continue
		}
		return &Result{
			Limit:      e.limit,
			Allowed:    n,
			Remaining:  int(diff / e.ei),
			RetryAfter: -1,
			ResetAfter: time.Duration(newTAT - now),
		}, nil
	}
}

// allowAtMost mirrors the allowAtMost Lua script, which is a different
// algorithm from allowN: it denies at remaining < 1, reports a different
// RetryAfter, and clamps the cost down to what is available.
func (b *memBackend) allowAtMost(_ context.Context, key string, e limitEntry, n int) (*Result, error) {
	if !e.valid() {
		return nil, ErrInvalidLimit
	}
	if n < 0 {
		return nil, ErrNegativeCount
	}

	p := b.cell(key)
	for {
		now := b.now()
		loaded := p.Load()
		if loaded == tombstone {
			p = b.cell(key)
			continue
		}
		tat := max(loaded, now)

		diff := now - (tat - e.burstOff)
		remaining := diff / e.ei
		if remaining < 1 {
			return &Result{
				Limit:      e.limit,
				RetryAfter: time.Duration(e.ei - diff),
				ResetAfter: time.Duration(tat - now),
			}, nil
		}

		cost := min(int64(n), remaining)
		newTAT := tat + e.ei*cost
		if newTAT <= now {
			return &Result{
				Limit:      e.limit,
				Allowed:    int(cost),
				Remaining:  int(remaining - cost),
				RetryAfter: -1,
			}, nil
		}
		if !p.CompareAndSwap(loaded, newTAT) {
			continue
		}
		return &Result{
			Limit:      e.limit,
			Allowed:    int(cost),
			Remaining:  int(remaining - cost),
			RetryAfter: -1,
			ResetAfter: time.Duration(newTAT - now),
		}, nil
	}
}

// reset drops the key. Tombstoning under the bucket lock before deleting means
// a concurrent writer's CAS fails and it retries against a fresh cell, rather
// than committing its write into a cell no longer reachable from the map.
func (b *memBackend) reset(_ context.Context, key string) error {
	b.state.Compute(key, func(p *atomic.Int64, loaded bool) (*atomic.Int64, xsync.ComputeOp) {
		if !loaded {
			return p, xsync.CancelOp
		}
		p.Store(tombstone)
		return p, xsync.DeleteOp
	})
	return nil
}

func (b *memBackend) close() error {
	b.once.Do(func() { close(b.stop) })
	return nil
}

func (b *memBackend) sweepLoop(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			b.sweep()
		}
	}
}

// sweep reclaims keys whose TAT has passed, which is what gives the in-memory
// backend the expiry that Redis gets free from SET ... EX.
//
// Deleting a key a live writer is holding would lose that writer's update and
// hand the key a full fresh burst. The CAS to tombstone runs inside
// DeleteMatching's callback, which xsync evaluates while holding the bucket
// lock, so claiming the cell and removing it are one step: either the sweeper
// wins and the writer retries against a new cell, or the writer wins and the
// CAS fails, leaving the key in place.
func (b *memBackend) sweep() int {
	now := b.now()
	return b.state.DeleteMatching(func(_ string, p *atomic.Int64) (bool, bool) {
		v := p.Load()
		return v < now && p.CompareAndSwap(v, tombstone), false
	})
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
