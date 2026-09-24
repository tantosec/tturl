package timing

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

type campaignParticipant struct {
	request Request
	lease   transportLease
}

type campaignGroupResult struct {
	outcomes             []Outcome
	startGate, finalGate time.Time
	committed            bool
	err                  error
}

// campaignGroup owns one readiness budget and per-release execution budgets.
// Prepared senders remain on their assigned connections until joined.
func campaignGroup(parent context.Context, participants []campaignParticipant, pacer *startPacer,
	plan Plan, anchor time.Time, cost int, charged, last bool,
) campaignGroupResult {
	timeout, delay := plan.RequestTimeout, plan.ReleaseDelay
	senders := make([]transportSender, len(participants))
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(context.Canceled)
	budget := newPhaseBudget(ctx, timeout)
	defer budget.close()
	start, finish := make(chan struct{}), make(chan struct{})
	result := campaignGroupResult{outcomes: make([]Outcome, len(senders))}
	prefixes := make(chan int, len(senders))
	completed := make(chan int, len(senders))
	var state sync.Mutex
	origin := -1
	failLocked := func(index int, failure *Failure) {
		if context.Cause(ctx) == nil {
			origin = index
			cancel(failure)
		}
	}
	released := make([]bool, len(senders))
	initialAt := make([]time.Time, len(senders))
	active := make([]bool, len(senders))
	remaining := len(senders)
	executionTimers := make([]*time.Timer, len(senders))
	var workers sync.WaitGroup
	stopReadiness := context.AfterFunc(budget.ctx, func() {
		state.Lock()
		if remaining > 0 {
			index := -1
			for candidate, launched := range released {
				if !launched {
					index = candidate
					break
				}
			}
			failLocked(index, campaignFailure(PhaseReadiness, context.Cause(budget.ctx)))
		}
		state.Unlock()
	})
	defer stopReadiness()
	for index, participant := range participants {
		sender, err := participant.lease.prepare(budget.ctx, participant.request)
		if err != nil {
			if cause := context.Cause(budget.ctx); cause != nil {
				err = cause
			}
			failure := campaignFailure(PhaseReadiness, err)
			for member := range result.outcomes {
				result.outcomes[member].Failure = &Failure{
					Phase: PhaseReadiness, Code: "cancelled",
					Message: "request cancelled after sender preparation failed", Cause: failure,
				}
			}
			result.outcomes[index].Failure = failure
			result.err = failure
			cancel(failure)
			for _, member := range participants {
				member.lease.dispose(failure)
			}
			for _, member := range participants {
				member.lease.join()
			}
			return result
		}
		senders[index] = sender
	}

	for index, sender := range senders {
		workers.Go(func() {
			onInitial := func(at time.Time) error {
				state.Lock()
				defer state.Unlock()
				if err := context.Cause(ctx); err != nil {
					return err
				}
				if err := context.Cause(budget.ctx); err != nil {
					return err
				}
				if released[index] {
					return errors.New("duplicate initial release")
				}
				released[index] = true
				initialAt[index] = at
				active[index] = true
				remaining--
				if timeout > 0 {
					executionTimers[index] = time.AfterFunc(time.Until(at.Add(timeout)), func() {
						state.Lock()
						defer state.Unlock()
						if active[index] {
							failLocked(index, campaignFailure(PhaseExecution, context.DeadlineExceeded))
						}
					})
				}
				if remaining == 0 {
					stopReadiness()
					budget.close()
				}
				return nil
			}
			onPrefix := func() error {
				select {
				case prefixes <- index:
					return nil
				case <-ctx.Done():
					return context.Cause(ctx)
				}
			}
			onComplete := func(at time.Time) error {
				state.Lock()
				defer state.Unlock()
				if !released[index] || !active[index] {
					return errors.New("invalid exchange completion")
				}
				active[index] = false
				if executionTimers[index] != nil {
					executionTimers[index].Stop()
				}
				if timeout > 0 && !at.Before(initialAt[index].Add(timeout)) {
					failLocked(index, campaignFailure(PhaseExecution, context.DeadlineExceeded))
					return context.DeadlineExceeded
				}
				return context.Cause(ctx)
			}
			outcome := sender.run(ctx, releaseGates{initial: start, final: finish, anchor: anchor}, senderHooks{
				initialRelease: onInitial, prefixComplete: onPrefix, exchangeComplete: onComplete,
			})
			state.Lock()
			outcome.Attempted = released[index]
			active[index] = false
			result.outcomes[index] = outcome
			if executionTimers[index] != nil {
				executionTimers[index].Stop()
			}
			if outcome.Failure != nil {
				failLocked(index, outcome.Failure)
			}
			state.Unlock()
			completed <- index
		})
	}
	waitReady := func(final bool) error {
		phase := initialPhase
		if final {
			phase = finalPhase
		}
		for index, sender := range senders {
			if err := sender.waitReady(ctx, phase); err != nil {
				failedPhase := PhaseReadiness
				if final {
					failedPhase = PhaseExecution
				}
				failure := campaignFailure(failedPhase, err)
				state.Lock()
				failLocked(index, failure)
				state.Unlock()
				return failure
			}
		}
		return context.Cause(ctx)
	}
	validate := func(final bool, commit func()) bool {
		for _, participant := range participants {
			participant.lease.lockState()
		}
		defer func() {
			for _, participant := range slices.Backward(participants) {
				participant.lease.unlockState()
			}
		}()
		phase := initialPhase
		if final {
			phase = finalPhase
		}
		for _, sender := range senders {
			if sender.readyLocked(phase) != nil {
				return false
			}
		}
		if context.Cause(ctx) != nil {
			return false
		}
		if commit != nil {
			commit()
		}
		return true
	}
	watchReady := func() (bool, []<-chan struct{}) {
		changes := make([]<-chan struct{}, 0, len(senders))
		for _, sender := range senders {
			ready, changed := sender.readinessSnapshot(initialPhase)
			if !ready {
				return false, nil
			}
			if changed != nil {
				changes = append(changes, changed)
			}
		}
		return true, changes
	}
	coordinate := func() error {
		for {
			if err := waitReady(false); err != nil {
				return err
			}
			if !charged {
				if validate(false, func() { result.startGate = time.Now(); close(start) }) {
					break
				}
				continue
			}
			committed, err := pacer.commit(ctx, cost, budget, func(commit func()) bool {
				return validate(false, func() {
					commit()
					result.committed = true
					result.startGate = time.Now()
					close(start)
				})
			}, watchReady)
			if err != nil {
				return campaignFailure(PhaseReadiness, err)
			}
			if committed {
				break
			}
		}
		if !last {
			close(finish)
			return nil
		}
		for range senders {
			select {
			case <-prefixes:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		for {
			if err := waitReady(true); err != nil {
				return err
			}
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return context.Cause(ctx)
			}
			if validate(true, func() { result.finalGate = time.Now(); close(finish) }) {
				return nil
			}
		}
	}
	if err := coordinate(); err != nil {
		cancel(err)
	}
	for range senders {
		<-completed
	}
	workers.Wait()
	result.err = context.Cause(ctx)
	if result.err != nil {
		for index := range result.outcomes {
			outcome := &result.outcomes[index]
			if index == origin {
				outcome.Failure = campaignFailure(PhaseExecution, result.err)
				continue
			}
			if outcome.Failure == nil && outcome.Response.Complete {
				continue
			}
			phase := PhaseReadiness
			if outcome.Attempted {
				phase = PhaseExecution
			}
			outcome.Failure = &Failure{
				Phase: phase, Code: "cancelled",
				Message: "request cancelled with outstanding trial work", Cause: result.err,
			}
		}
	}
	return result
}
