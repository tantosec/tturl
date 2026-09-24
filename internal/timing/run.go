package timing

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

type campaignFactory func(context.Context, Request, Plan) (transportLease, error)

type campaignState struct {
	resolved        *Resolved
	cancel          context.CancelCauseFunc
	interactionDone func()
	factory         campaignFactory
	pacer           *startPacer
	queue           campaignQueue
	initial         *initialPhases
	events          chan Event
	ids             atomic.Uint64
	mu              sync.Mutex
	counts          Accounting
	requests        []RequestAccounting
	requestIndexes  map[int]int
}

// Run validates its own snapshot, owns all target interaction and delivers
// observations serially. The callback must return promptly on output failure.
// After an error, Run cancels and joins workers before returning accounting.
func Run(ctx context.Context, resolved *Resolved, emit func(Event) error) (Result, error) {
	return runCampaign(ctx, resolved, emit, acquireTransport)
}

func runCampaign(parent context.Context, resolved *Resolved, emit func(Event) error,
	factory campaignFactory,
) (Result, error) {
	result := Result{StartedAt: time.Now().UTC(), Completion: CompletionFailed}
	if resolved == nil || emit == nil || factory == nil {
		err := errors.New("timing run requires a resolved plan, callback and transport")
		result.Failure = campaignFailure(PhaseRun, err)
		return result, result.Failure
	}
	validated, err := Resolve(resolved.Plan)
	if err != nil {
		result.Failure = campaignFailure(PhaseRun, err)
		return result, result.Failure
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(context.Canceled)
	var runTimer *time.Timer
	var runClock sync.Mutex
	remainingInteractions := validated.Workers
	var runDeadline time.Time
	if validated.Plan.RunTimeout > 0 {
		runDeadline = time.Now().Add(validated.Plan.RunTimeout)
		runTimer = time.AfterFunc(time.Until(runDeadline), func() {
			runClock.Lock()
			defer runClock.Unlock()
			if remainingInteractions > 0 {
				cancel(campaignFailure(PhaseRun, context.DeadlineExceeded))
			}
		})
		defer runTimer.Stop()
	}
	interactionDone := func() {
		runClock.Lock()
		defer runClock.Unlock()
		remainingInteractions--
		if remainingInteractions == 0 && runTimer != nil {
			runTimer.Stop()
			if !time.Now().Before(runDeadline) && context.Cause(ctx) == nil {
				cancel(campaignFailure(PhaseRun, context.DeadlineExceeded))
			}
		}
	}
	//nolint:gosec // Resolve validates a positive worker count.
	nextUnit := uint64(validated.Workers)
	state := &campaignState{
		resolved: validated, factory: factory, cancel: cancel, interactionDone: interactionDone,
		pacer:  newStartPacer(validated.Plan.BatchRate, validated.Plan.RequestRate),
		queue:  campaignQueue{next: nextUnit, limit: validated.WorkUnits},
		events: make(chan Event, validated.Workers),
	}
	state.initial = newInitialPhases(validated.Workers)
	state.requests = make([]RequestAccounting, len(validated.Requests))
	state.requestIndexes = make(map[int]int, len(validated.Requests))
	for index, request := range validated.Requests {
		state.requests[index].RequestID = request.ID
		state.requestIndexes[request.ID] = index
	}
	var workers sync.WaitGroup
	for workerID := range validated.Workers {
		workers.Go(func() {
			worker := campaignWorker{state: state, id: workerID}
			_ = worker.run(ctx)
		})
	}
	go func() {
		workers.Wait()
		close(state.events)
	}()
	var outputFailure *Failure
	var deliveredTrials uint64
	for event := range state.events {
		if outputFailure != nil {
			continue
		}
		if err := emit(event); err != nil {
			outputFailure = &Failure{Phase: PhaseOutput, Code: "output", Message: err.Error(), Cause: err}
			cancel(outputFailure)
		} else if event.Trial != nil {
			deliveredTrials++
		}
	}
	state.mu.Lock()
	result.Accounting = state.counts
	result.Requests = state.requests
	state.mu.Unlock()
	if validated.PlannedRequestOperations > result.Accounting.RequestOperationsAttempted {
		result.Accounting.RequestOperationsUnattempted = validated.PlannedRequestOperations -
			result.Accounting.RequestOperationsAttempted
	}
	if validated.Plan.Trials > 0 {
		for index := range result.Requests {
			count := &result.Requests[index]
			count.Unattempted = validated.PlannedTrials - min(validated.PlannedTrials, count.Attempted)
		}
		result.Accounting.TrialsUnattempted = validated.PlannedTrials - result.Accounting.TrialsAttempted
		if validated.Plan.Arrangement == ArrangeRotate {
			result.Accounting.CyclesUnattempted = validated.WorkUnits - result.Accounting.CyclesAttempted
		}
	}
	cause := context.Cause(ctx)
	if outputFailure != nil && (cause == nil || campaignUserStop(cause)) {
		cause = outputFailure
	}
	if (cause == nil || campaignUserStop(cause)) && outputFailure == nil &&
		validated.Plan.Trials > 0 && result.Accounting.TrialsAttempted == validated.PlannedTrials &&
		deliveredTrials == validated.PlannedTrials {
		result.Completion = CompletionComplete
		return result, nil
	}
	if validated.Plan.Trials == 0 && campaignUserStop(cause) && outputFailure == nil {
		result.Completion = CompletionStopped
		return result, nil
	}
	if cause == nil {
		cause = errors.New("timing run ended before planned work was attempted")
	}
	result.Failure = campaignFailure(PhaseRun, cause)
	return result, result.Failure
}

// publish accounts and delivers produced evidence through bounded backpressure.
// Run drains this channel after cancellation and after callback failure.
func (s *campaignState) publish(event Event) {
	s.mu.Lock()
	if trial := event.Trial; trial != nil {
		s.counts.TrialsAttempted++
		if trial.Complete {
			s.counts.TrialsComplete++
		} else {
			s.counts.TrialsIncomplete++
		}
		if trial.Committed {
			s.counts.MeasuredCommitments++
			s.counts.MeasuredOfferedOperations += trial.OfferedOperations
		}
		for _, outcome := range trial.Outcomes {
			if index, ok := s.requestIndexes[outcome.RequestID]; ok {
				if outcome.Attempted {
					s.requests[index].Attempted++
				} else {
					s.requests[index].Unattempted++
				}
			}
			if outcome.Attempted {
				s.counts.RequestOperationsAttempted++
				if outcome.Failure == nil && outcome.Response.Complete {
					s.counts.RequestOperationsComplete++
				} else {
					s.counts.RequestOperationsFailed++
				}
			} else {
				s.counts.RequestOperationsUnattempted++
			}
		}
	}
	if warmup := event.Warmup; warmup != nil {
		s.counts.WarmupTrials++
		if warmup.Phase == "initial" {
			s.counts.InitialWarmupTrials++
		} else {
			s.counts.ReplacementWarmupTrials++
		}
		if warmup.Complete {
			s.counts.WarmupTrialsComplete++
		} else {
			s.counts.WarmupTrialsIncomplete++
		}
		if warmup.Committed {
			s.counts.WarmupCommitments++
			s.counts.WarmupOfferedOperations += warmup.OfferedOperations
		}
		for _, outcome := range warmup.Outcomes {
			if outcome.Attempted {
				s.counts.WarmupOperationsAttempted++
				if outcome.Failure == nil && outcome.Response.Complete && !outcome.Response.Reset {
					s.counts.WarmupOperationsComplete++
				} else {
					s.counts.WarmupOperationsFailed++
				}
			} else {
				s.counts.WarmupOperationsUnattempted++
			}
		}
	}
	if connection := event.Connection; connection != nil {
		switch connection.State {
		case "acquired":
			s.counts.ConnectionsAcquired++
			if connection.ReplacementOf != 0 {
				s.counts.ConnectionsReplaced++
			}
		case "retired":
			s.counts.ConnectionsRetired++
		case "failed":
			s.counts.AcquisitionFailures++
		}
	}
	s.mu.Unlock()
	s.events <- event
}
