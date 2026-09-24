package timing

import (
	"context"
	"math"
	"reflect"
	"sync"
	"time"
)

// startPacer admits ready workers in FIFO order across workers and pools.
// Waiting reserves no start time. The upcoming cost determines eligibility
// relative to the last actual commitment, including mixed warmup widths.
type startPacer struct {
	mu          sync.Mutex
	batchRate   float64
	requestRate float64
	last        time.Time
	charges     int64
	waiters     []*startWaiter
}

func newStartPacer(batchRate, requestRate float64) *startPacer {
	return &startPacer{batchRate: batchRate, requestRate: requestRate}
}

func (p *startPacer) interval(cost int) time.Duration {
	var seconds float64
	if p.batchRate > 0 {
		seconds = 1 / p.batchRate
	}
	if p.requestRate > 0 {
		seconds = max(seconds, float64(cost)/p.requestRate)
	}
	ns := math.Ceil(seconds * float64(time.Second))
	if ns >= float64(math.MaxInt64) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(ns)
}

func (p *startPacer) eligible(cost int) time.Time {
	if p.last.IsZero() {
		return time.Time{}
	}
	return p.last.Add(p.interval(cost))
}

// startWaiter owns admission once its turn channel closes. Only its owner
// removes it, including when cancellation occurs before admission.
type startWaiter struct {
	turn chan struct{}
}

func (p *startPacer) remove(waiter *startWaiter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, queued := range p.waiters {
		if queued != waiter {
			continue
		}
		copy(p.waiters[i:], p.waiters[i+1:])
		p.waiters[len(p.waiters)-1] = nil
		p.waiters = p.waiters[:len(p.waiters)-1]
		if i == 0 && len(p.waiters) > 0 {
			close(p.waiters[0].turn)
		}
		return
	}
}

// commit queues ready work and waits only for admission and pacing. A false
// revalidation returns uncommitted so the caller resumes readiness with its
// existing budget and rejoins at the tail. Revalidate holds connection state
// locks through its call to commit and does not wait for I/O. The commit
// callback must be called exactly once if revalidation succeeds. An optional
// watch checks readiness and snapshots change notifications atomically.
// It must not wait for I/O. Notifications trigger a fresh readiness snapshot.
func (p *startPacer) commit(
	ctx context.Context, cost int, budget *phaseBudget,
	revalidate func(commit func()) bool,
	watch func() (bool, []<-chan struct{}),
) (bool, error) {
	waiter := &startWaiter{turn: make(chan struct{})}
	p.mu.Lock()
	p.waiters = append(p.waiters, waiter)
	if len(p.waiters) == 1 {
		close(waiter.turn)
	}
	p.mu.Unlock()
	defer p.remove(waiter)
	defer budget.resume()

	for {
		p.mu.Lock()
		if err := context.Cause(ctx); err != nil {
			p.mu.Unlock()
			return false, err
		}
		if err := context.Cause(budget.ctx); err != nil {
			p.mu.Unlock()
			return false, err
		}
		var changes []<-chan struct{}
		if watch != nil {
			valid, notifications := watch()
			if !valid {
				p.mu.Unlock()
				return false, nil
			}
			changes = notifications
		}
		head := p.waiters[0] == waiter
		wait := time.Until(p.eligible(cost))
		if head && wait <= 0 {
			budget.resume()
			if err := context.Cause(budget.ctx); err != nil {
				p.mu.Unlock()
				return false, err
			}
			committed := false
			valid := revalidate(func() {
				if committed {
					panic("timing: duplicate start commitment")
				}
				p.last = time.Now()
				p.charges += int64(cost)
				committed = true
			})
			p.mu.Unlock()
			if valid != committed {
				panic("timing: inconsistent start revalidation")
			}
			return committed, nil
		}
		if err := budget.pause(); err != nil {
			p.mu.Unlock()
			return false, err
		}
		p.mu.Unlock()
		var timer *time.Timer
		var timerC <-chan time.Time
		var turn <-chan struct{}
		if head {
			timer = time.NewTimer(wait)
			timerC = timer.C
		} else {
			turn = waiter.turn
		}
		waitPacingChange(ctx, budget.ctx, turn, timerC, changes)
		if timer != nil {
			timer.Stop()
		}
	}
}

// waitPacingChange joins notification watchers before returning. Each watcher
// selects a bounded group, so readiness width does not depend on reflect.Select
// limits. Campaign cancellation remains active throughout this deliberate wait.
func waitPacingChange(ctx, budget context.Context, turn <-chan struct{},
	timer <-chan time.Time, changes []<-chan struct{},
) {
	var notification <-chan struct{}
	if len(changes) > 0 {
		stop := make(chan struct{})
		ready := make(chan struct{}, 1)
		notification = ready
		var watchers sync.WaitGroup
		const groupSize = 1024
		for start := 0; start < len(changes); start += groupSize {
			group := changes[start:min(start+groupSize, len(changes))]
			watchers.Go(func() {
				cases := make([]reflect.SelectCase, 1, len(group)+1)
				cases[0] = reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(stop)}
				for _, changed := range group {
					cases = append(cases, reflect.SelectCase{
						Dir: reflect.SelectRecv, Chan: reflect.ValueOf(changed),
					})
				}
				selected, _, _ := reflect.Select(cases)
				if selected != 0 {
					select {
					case ready <- struct{}{}:
					default:
					}
				}
			})
		}
		defer watchers.Wait()
		defer close(stop)
	}
	select {
	case <-ctx.Done():
	case <-budget.Done():
	case <-turn:
	case <-timer:
	case <-notification:
	}
}
