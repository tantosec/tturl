package timing

// cspell:ignore synctest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestPacingUpcomingCostAndActualCommitment(t *testing.T) {
	p := newStartPacer(0, 1)
	if !p.eligible(10).IsZero() {
		t.Fatal("first start must be immediately eligible")
	}
	anchor := time.Now()
	p.last = anchor
	if got := p.eligible(10).Sub(anchor); got != 10*time.Second {
		t.Fatalf("ten-operation projection: %s", got)
	}
	if got := p.eligible(1).Sub(anchor); got != time.Second {
		t.Fatalf("one-operation projection: %s", got)
	}
	p.batchRate = 0.25
	if got := p.eligible(1).Sub(anchor); got != 4*time.Second {
		t.Fatalf("batch selector composition: %s", got)
	}
	p.batchRate, p.requestRate = 0, 0
	budget := newPhaseBudget(t.Context(), 0)
	defer budget.close()
	before := time.Now()
	ok, err := p.commit(t.Context(), 3, budget, func(commit func()) bool {
		commit()
		return true
	}, nil)
	if !ok || err != nil || p.charges != 3 || p.last.Before(before) {
		t.Fatalf("actual start anchor and charge: committed=%t err=%v charges=%d", ok, err, p.charges)
	}
}

func TestPacingRevalidationAndConcurrentWorkers(t *testing.T) {
	p := newStartPacer(0, 0)
	budget := newPhaseBudget(t.Context(), 0)
	defer budget.close()
	ok, err := p.commit(t.Context(), 10, budget, func(func()) bool { return false }, nil)
	if ok || err != nil || p.charges != 0 || !p.last.IsZero() {
		t.Fatalf("stale readiness charged work: committed=%t err=%v", ok, err)
	}
	var wg sync.WaitGroup
	for _, cost := range []int{10, 1} {
		wg.Go(func() {
			ok, err := p.commit(t.Context(), cost, budget, func(commit func()) bool {
				commit()
				return true
			}, nil)
			if !ok || err != nil {
				t.Errorf("ready worker: committed=%t err=%v", ok, err)
			}
		})
	}
	wg.Wait()
	if p.charges != 11 {
		t.Fatalf("shared operation charge = %d, want 11", p.charges)
	}
}

func TestPacingCancellationRemovesUncommittedOpportunity(t *testing.T) {
	p := newStartPacer(1, 0)
	p.last = time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	budget := newPhaseBudget(ctx, time.Minute)
	defer budget.close()
	cancel()
	ok, err := p.commit(ctx, 3, budget, func(func()) bool {
		t.Error("cancelled opportunity reached revalidation")
		return false
	}, nil)
	if ok || !errors.Is(err, context.Canceled) || p.charges != 0 {
		t.Fatalf("cancellation: committed=%t err=%v charges=%d", ok, err, p.charges)
	}
}

func TestReadinessBudgetPausesWithoutRenewal(t *testing.T) {
	b := newPhaseBudget(t.Context(), time.Minute)
	defer b.close()
	// Account for active preparation without relying on scheduler latency.
	b.mu.Lock()
	b.started = b.started.Add(-10 * time.Second)
	b.mu.Unlock()
	if err := b.pause(); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	remaining := b.remaining
	b.mu.Unlock()
	if remaining <= 0 || remaining > 50*time.Second {
		t.Fatalf("preparation did not consume its budget: %s", remaining)
	}
	if err := b.pause(); err != nil {
		t.Fatal(err)
	}
	b.resume()
	b.mu.Lock()
	if b.remaining != remaining || b.paused {
		t.Error("revalidation renewed or failed to resume readiness")
	}
	b.started = b.started.Add(-time.Minute)
	b.mu.Unlock()
	if err := b.pause(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("remaining budget did not expire: %v", err)
	}
}

func TestUnlimitedReadinessRemainsCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	b := newPhaseBudget(ctx, 0)
	defer b.close()
	if err := b.pause(); err != nil {
		t.Fatal(err)
	}
	b.resume()
	cancel()
	<-b.ctx.Done()
	if !errors.Is(context.Cause(b.ctx), context.Canceled) {
		t.Fatal("unlimited readiness ignored cancellation")
	}
}

// pacingResult records the commitment itself, before its owner can rejoin.
type pacingResult struct {
	name      string
	at        time.Time
	committed bool
	err       error
}

func enqueuePacingWorker(t *testing.T, p *startPacer, ctx context.Context,
	name string, cost int, valid bool, results chan<- pacingResult,
) *phaseBudget {
	t.Helper()
	budget := newPhaseBudget(ctx, time.Second)
	t.Cleanup(budget.close)
	go func() {
		var at time.Time
		committed, err := p.commit(ctx, cost, budget, func(commit func()) bool {
			if valid {
				commit()
				at = time.Now()
			}
			return valid
		}, nil)
		results <- pacingResult{name: name, at: at, committed: committed, err: err}
	}()
	synctest.Wait()
	return budget
}

func TestPacingFIFOAndTailReentry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartPacer(0, 1)
		anchor := time.Now()
		p.last = anchor
		results := make(chan pacingResult, 4)
		budgets := []*phaseBudget{
			enqueuePacingWorker(t, p, t.Context(), "first", 3, true, results),
			enqueuePacingWorker(t, p, t.Context(), "second", 1, true, results),
			enqueuePacingWorker(t, p, t.Context(), "third", 2, true, results),
		}
		for _, b := range budgets {
			b.mu.Lock()
			paused, remaining := b.paused, b.remaining
			b.mu.Unlock()
			if !paused || remaining != time.Second {
				t.Fatalf("queued budget: paused=%t remaining=%s", paused, remaining)
			}
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		first := <-results
		if first.name != "first" || !first.committed || first.err != nil || first.at.Sub(anchor) != 3*time.Second {
			t.Fatalf("first commitment: %+v", first)
		}
		enqueuePacingWorker(t, p, t.Context(), "first-again", 1, true, results)
		for _, want := range []struct {
			name  string
			delay time.Duration
			at    time.Duration
		}{
			{"second", time.Second, 4 * time.Second},
			{"third", 2 * time.Second, 6 * time.Second},
			{"first-again", time.Second, 7 * time.Second},
		} {
			time.Sleep(want.delay)
			synctest.Wait()
			got := <-results
			if got.name != want.name || !got.committed || got.err != nil || got.at.Sub(anchor) != want.at {
				t.Fatalf("commitment: %+v, want %s at %s", got, want.name, want.at)
			}
		}
		if p.charges != 7 || len(p.waiters) != 0 {
			t.Fatalf("completed queue: charges=%d waiters=%d", p.charges, len(p.waiters))
		}
	})
}

func TestPacingHeadRemovalAndActualAnchor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartPacer(0, 1)
		anchor := time.Now()
		p.last = anchor
		results := make(chan pacingResult, 4)
		ctx, cancel := context.WithCancel(t.Context())
		enqueuePacingWorker(t, p, ctx, "cancelled", 10, true, results)
		enqueuePacingWorker(t, p, t.Context(), "invalid", 2, false, results)
		enqueuePacingWorker(t, p, t.Context(), "ready", 1, true, results)
		cancel()
		synctest.Wait()
		if got := <-results; got.name != "cancelled" || got.committed || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("cancelled head: %+v", got)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		for range 2 {
			got := <-results
			switch got.name {
			case "invalid":
				if got.committed || got.err != nil {
					t.Fatalf("invalid head: %+v", got)
				}
			case "ready":
				if !got.committed || got.err != nil || got.at.Sub(anchor) != 2*time.Second {
					t.Fatalf("ready successor: %+v", got)
				}
			default:
				t.Fatalf("unexpected worker: %+v", got)
			}
		}
		if p.charges != 1 {
			t.Fatalf("actual charges = %d, want 1", p.charges)
		}
		// Preparation must finish before this worker joins.
		time.Sleep(10 * time.Second)
		enqueuePacingWorker(t, p, t.Context(), "prepared", 1, true, results)
		if got := <-results; !got.committed || got.at.Sub(anchor) != 12*time.Second {
			t.Fatalf("prepared worker: %+v", got)
		}
		enqueuePacingWorker(t, p, t.Context(), "following", 1, true, results)
		if len(results) != 0 {
			t.Fatal("successor committed before its actual-anchor interval")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if got := <-results; !got.committed || got.at.Sub(anchor) != 13*time.Second {
			t.Fatalf("paced successor: %+v", got)
		}
	})
}

func TestPacingQueuedShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartPacer(1, 0)
		p.last = time.Now().Add(time.Hour)
		ctx, cancel := context.WithCancel(t.Context())
		results := make(chan pacingResult, 3)
		for _, name := range []string{"first", "second", "third"} {
			enqueuePacingWorker(t, p, ctx, name, 1, true, results)
		}
		cancel()
		synctest.Wait()
		for range 3 {
			if got := <-results; got.committed || !errors.Is(got.err, context.Canceled) {
				t.Fatalf("shutdown: %+v", got)
			}
		}
		if p.charges != 0 || len(p.waiters) != 0 {
			t.Fatalf("shutdown queue: charges=%d waiters=%d", p.charges, len(p.waiters))
		}
	})
}

func TestPacingCampaignDeadlineRemovesHead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartPacer(0, 1)
		anchor := time.Now()
		p.last = anchor
		results := make(chan pacingResult, 2)
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		enqueuePacingWorker(t, p, ctx, "expired", 10, true, results)
		enqueuePacingWorker(t, p, t.Context(), "ready", 1, true, results)
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if got := <-results; got.name != "expired" || got.committed || !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("expired head: %+v", got)
		}
		time.Sleep(500 * time.Millisecond)
		synctest.Wait()
		if got := <-results; got.name != "ready" || !got.committed || got.err != nil || got.at.Sub(anchor) != time.Second {
			t.Fatalf("deadline successor: %+v", got)
		}
		if p.charges != 1 || len(p.waiters) != 0 {
			t.Fatalf("deadline queue: charges=%d waiters=%d", p.charges, len(p.waiters))
		}
	})
}

func TestPacingReadinessChangeRemovesExpensiveHead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newStartPacer(0, 1)
		anchor := time.Now()
		p.last = anchor
		results := make(chan pacingResult, 2)
		budget := newPhaseBudget(t.Context(), time.Second)
		defer budget.close()
		var state sync.Mutex
		valid := true
		changed := make(chan struct{})
		watch := func() (bool, []<-chan struct{}) {
			state.Lock()
			defer state.Unlock()
			return valid, []<-chan struct{}{changed}
		}
		go func() {
			committed, err := p.commit(t.Context(), 10, budget, func(func()) bool {
				t.Error("invalidated worker reached commitment")
				return false
			}, watch)
			results <- pacingResult{name: "invalidated", committed: committed, err: err}
		}()
		synctest.Wait()
		enqueuePacingWorker(t, p, t.Context(), "ready", 1, true, results)
		time.Sleep(500 * time.Millisecond)
		state.Lock()
		// Valid changes refresh the readiness snapshot.
		close(changed)
		changed = make(chan struct{})
		state.Unlock()
		synctest.Wait()
		if len(results) != 0 {
			t.Fatal("state refresh removed valid queued work")
		}
		time.Sleep(500 * time.Millisecond)
		state.Lock()
		valid = false
		close(changed)
		state.Unlock()
		synctest.Wait()
		for range 2 {
			got := <-results
			switch got.name {
			case "invalidated":
				if got.committed || got.err != nil {
					t.Fatalf("invalidated head: %+v", got)
				}
			case "ready":
				if !got.committed || got.err != nil || got.at.Sub(anchor) != time.Second {
					t.Fatalf("ready successor: %+v", got)
				}
			default:
				t.Fatalf("unexpected result: %+v", got)
			}
		}
		if p.charges != 1 || len(p.waiters) != 0 {
			t.Fatalf("readiness queue: charges=%d waiters=%d", p.charges, len(p.waiters))
		}
	})
}

func TestPacingReadinessNotificationsAcrossGroups(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		name := "notification"
		if cancelWait {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				pending, last := make(chan struct{}), make(chan struct{})
				changes := make([]<-chan struct{}, 65537)
				for index := range changes {
					changes[index] = pending
				}
				changes[len(changes)-1] = last
				done := make(chan struct{})
				go func() {
					waitPacingChange(ctx, t.Context(), nil, nil, changes)
					close(done)
				}()
				synctest.Wait()
				if cancelWait {
					cancel()
				} else {
					close(last)
				}
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("notification wait failed to return after joining watchers")
				}
			})
		})
	}
}
