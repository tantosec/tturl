package timing

import (
	"context"
	"sync"
	"time"
)

// phaseBudget bounds active readiness time. Deliberate pacing pauses it;
// revalidation and post-commit writer waiting consume the remaining budget.
// A zero limit removes the finite bound, but retains parent cancellation.
type phaseBudget struct {
	mu         sync.Mutex
	ctx        context.Context
	cancel     context.CancelCauseFunc
	remaining  time.Duration
	started    time.Time
	timer      *time.Timer
	limited    bool
	paused     bool
	generation uint64
}

func newPhaseBudget(parent context.Context, limit time.Duration) *phaseBudget {
	ctx, cancel := context.WithCancelCause(parent)
	b := &phaseBudget{ctx: ctx, cancel: cancel, remaining: limit, limited: limit > 0}
	b.mu.Lock()
	b.resumeLocked()
	b.mu.Unlock()
	return b
}

func (b *phaseBudget) resumeLocked() {
	b.paused = false
	b.started = time.Now()
	if !b.limited || b.ctx.Err() != nil {
		return
	}
	if b.remaining <= 0 {
		b.cancel(context.DeadlineExceeded)
		return
	}
	b.generation++
	generation := b.generation
	b.timer = time.AfterFunc(b.remaining, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if generation == b.generation && !b.paused {
			b.cancel(context.DeadlineExceeded)
		}
	})
}

func (b *phaseBudget) pause() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := context.Cause(b.ctx); err != nil {
		return err
	}
	if b.paused {
		return nil
	}
	if b.limited {
		b.remaining -= time.Since(b.started)
		if b.remaining <= 0 {
			b.cancel(context.DeadlineExceeded)
			return context.DeadlineExceeded
		}
	}
	b.paused = true
	b.generation++
	if b.timer != nil {
		b.timer.Stop()
	}
	return nil
}

func (b *phaseBudget) resume() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.paused {
		b.resumeLocked()
	}
}

func (b *phaseBudget) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.generation++
	if b.timer != nil {
		b.timer.Stop()
	}
	b.cancel(context.Canceled)
}
