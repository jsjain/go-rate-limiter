package rate_limiter

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/puzpuzpuz/xsync/v4"
)

func newMemBackend(sweep time.Duration) *memBackend {
	epoch := time.Now()
	mem := &memBackend{
		state: xsync.NewMap[string, *atomic.Int64](),
		now:   func() int64 { return int64(time.Since(epoch)) },
		stop:  make(chan struct{}),
	}
	if sweep > 0 {
		go mem.sweepLoop(sweep)
	}
	return mem
}

// liveCell returns the TAT cell for key, creating it if absent. A fresh cell
// holds 0, which is the epoch and so always in the past, which is why the first
// request needs no special case: max(0, now) is now.
//
// The common path is xsync's lock-free lookup. Only a tombstoned cell falls
// through to Compute, which takes the bucket lock and so waits for the sweeper
// to finish rather than spinning.
func (mem *memBackend) liveCell(key string) *atomic.Int64 {
	cell, _ := mem.state.LoadOrCompute(key, func() (*atomic.Int64, bool) {
		return new(atomic.Int64), false
	})
	if cell.Load() != tombstone {
		return cell
	}
	cell, _ = mem.state.Compute(key, func(current *atomic.Int64, loaded bool) (*atomic.Int64, xsync.ComputeOp) {
		if loaded && current.Load() != tombstone {
			return current, xsync.CancelOp
		}
		return new(atomic.Int64), xsync.UpdateOp
	})
	return cell
}

// allowN mirrors the allowN Lua script in integer nanoseconds.
func (mem *memBackend) allowN(_ context.Context, key string, entry limitEntry, n int) (*Result, error) {
	if !entry.valid() {
		return nil, ErrInvalidLimit
	}
	if n < 0 {
		return nil, ErrNegativeCount
	}

	cell := mem.liveCell(key)
	for {
		now := mem.now()
		loaded := cell.Load()
		if loaded == tombstone {
			cell = mem.liveCell(key)
			continue
		}
		tat := max(loaded, now)

		// More events than the burst can ever hold. Returns what the script
		// returns, without computing ei*n and overflowing.
		if int64(n) > int64(entry.limit.Burst) {
			return &Result{
				Limit:      entry.limit,
				RetryAfter: time.Duration((tat - now) + satMul(entry.ei, int64(n)-int64(entry.limit.Burst))),
				ResetAfter: time.Duration(tat - now),
			}, nil
		}

		newTAT := tat + entry.ei*int64(n)
		diff := now - (newTAT - entry.burstOff)
		if diff < 0 {
			return &Result{
				Limit:      entry.limit,
				RetryAfter: time.Duration(-diff),
				ResetAfter: time.Duration(tat - now),
			}, nil
		}

		// Nothing to persist (n == 0 on an idle key). Matches the script's
		// `if reset_after > 0` guard and keeps probe calls from creating
		// entries the sweeper would have to reclaim.
		if newTAT <= now {
			return &Result{
				Limit:      entry.limit,
				Allowed:    n,
				Remaining:  int(diff / entry.ei),
				RetryAfter: -1,
			}, nil
		}

		// On failure another writer won, or the sweeper claimed the cell. The
		// retry re-reads now as well as loaded: computing against a stale now
		// can deny a request by the width of the retry window.
		if !cell.CompareAndSwap(loaded, newTAT) {
			continue
		}
		return &Result{
			Limit:      entry.limit,
			Allowed:    n,
			Remaining:  int(diff / entry.ei),
			RetryAfter: -1,
			ResetAfter: time.Duration(newTAT - now),
		}, nil
	}
}

// allowAtMost mirrors the allowAtMost Lua script, which is a different
// algorithm from allowN: it denies at remaining < 1, reports a different
// RetryAfter, and clamps the cost down to what is available.
func (mem *memBackend) allowAtMost(_ context.Context, key string, entry limitEntry, n int) (*Result, error) {
	if !entry.valid() {
		return nil, ErrInvalidLimit
	}
	if n < 0 {
		return nil, ErrNegativeCount
	}

	cell := mem.liveCell(key)
	for {
		now := mem.now()
		loaded := cell.Load()
		if loaded == tombstone {
			cell = mem.liveCell(key)
			continue
		}
		tat := max(loaded, now)

		diff := now - (tat - entry.burstOff)
		remaining := diff / entry.ei
		if remaining < 1 {
			return &Result{
				Limit:      entry.limit,
				RetryAfter: time.Duration(entry.ei - diff),
				ResetAfter: time.Duration(tat - now),
			}, nil
		}

		cost := min(int64(n), remaining)
		newTAT := tat + entry.ei*cost
		if newTAT <= now {
			return &Result{
				Limit:      entry.limit,
				Allowed:    int(cost),
				Remaining:  int(remaining - cost),
				RetryAfter: -1,
			}, nil
		}
		if !cell.CompareAndSwap(loaded, newTAT) {
			continue
		}
		return &Result{
			Limit:      entry.limit,
			Allowed:    int(cost),
			Remaining:  int(remaining - cost),
			RetryAfter: -1,
			ResetAfter: time.Duration(newTAT - now),
		}, nil
	}
}

// reset drops the key. Tombstoning under the bucket lock before deleting means
// a concurrent writer's CAS fails and it retries against a fresh cell, rather
// than committing into a cell no longer reachable from the map.
func (mem *memBackend) reset(_ context.Context, key string) error {
	mem.state.Compute(key, func(cell *atomic.Int64, loaded bool) (*atomic.Int64, xsync.ComputeOp) {
		if !loaded {
			return cell, xsync.CancelOp
		}
		cell.Store(tombstone)
		return cell, xsync.DeleteOp
	})
	return nil
}

func (mem *memBackend) close() error {
	mem.once.Do(func() { close(mem.stop) })
	return nil
}

func (mem *memBackend) sweepLoop(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-mem.stop:
			return
		case <-ticker.C:
			mem.sweep()
		}
	}
}

// sweep reclaims keys whose TAT has passed, giving the in-memory backend the
// expiry that Redis gets from SET ... EX.
//
// Deleting a key a live writer holds would lose that writer's update and hand
// the key a full fresh burst. The CAS to tombstone runs inside DeleteMatching's
// callback, which xsync evaluates under the bucket lock, so claiming the cell
// and removing it are one step: either the sweeper wins and the writer retries
// against a new cell, or the writer wins and the CAS fails.
func (mem *memBackend) sweep() int {
	now := mem.now()
	return mem.state.DeleteMatching(func(_ string, cell *atomic.Int64) (bool, bool) {
		claimed := cell.Load()
		return claimed < now && cell.CompareAndSwap(claimed, tombstone), false
	})
}
