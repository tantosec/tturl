package timing

import (
	"context"
	"errors"
	"testing"
)

type completionCampaignLease struct {
	transportLease
	change func(*Outcome)
}

func (l *completionCampaignLease) prepare(ctx context.Context, request Request) (transportSender, error) {
	sender, err := l.transportLease.prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	if request.ID != 1 {
		return sender, nil
	}
	return &completionCampaignSender{transportSender: sender, change: l.change}, nil
}

type completionCampaignSender struct {
	transportSender
	change func(*Outcome)
}

func (s *completionCampaignSender) run(ctx context.Context, gates releaseGates, hooks senderHooks) Outcome {
	outcome := s.transportSender.run(ctx, gates, hooks)
	s.change(&outcome)
	return outcome
}

func TestRunWarmupRequiresCompleteExchangeWithoutReset(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "incomplete"
		if reset {
			name = "reset"
		}
		t.Run(name, func(t *testing.T) {
			plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b")
			plan.Synchronise, plan.Warmup = true, 2
			transport := &campaignTestTransport{}
			var record *Warmup
			result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
				if event.Warmup != nil {
					record = event.Warmup
				}
				return nil
			}, func(ctx context.Context, request Request, policy Plan) (transportLease, error) {
				lease, err := transport.acquire(ctx, request, policy)
				return &completionCampaignLease{transportLease: lease, change: func(outcome *Outcome) {
					if request.ID == 1 {
						outcome.Response.Complete = reset
						outcome.Response.Reset = reset
					}
				}}, err
			})
			if err == nil || record == nil || record.Complete || record.Failure == nil ||
				result.Accounting.WarmupTrialsIncomplete != 1 || result.Accounting.WarmupOperationsComplete != 1 ||
				result.Accounting.WarmupOperationsFailed != 1 || result.Accounting.TrialsAttempted != 0 ||
				!record.Outcomes[1].Response.Complete || record.Outcomes[1].Failure != nil || transport.live != 0 {
				t.Fatalf("result=%+v err=%v record=%+v live=%d", result, err, record, transport.live)
			}
		})
	}
}

func TestWarmupFailureCancelsAdmissionBeforeEvidenceQueueDrains(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.Warmup = 1
	resolved, err := Resolve(plan)
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("warmup response incomplete")
	transport := &campaignTestTransport{exchange: func(context.Context, *campaignTestLease, Request) error {
		return cause
	}}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	state := &campaignState{resolved: resolved, cancel: cancel, pacer: newStartPacer(0, 0), events: make(chan Event, 1)}
	lease, err := transport.acquire(ctx, plan.Requests[0], plan)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.dispose(context.Canceled)
	state.events <- Event{Phase: &PhaseCompletion{Phase: "initial_acquisition", WorkerID: -1}}
	worker := campaignWorker{
		state: state, slots: []campaignSlot{{pool: 0, id: 1, lease: lease}},
		byPool: map[int][]int{0: {0}},
	}
	done := make(chan error, 1)
	go func() { done <- worker.prime(ctx) }()
	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), cause) {
		t.Fatalf("cause=%v", context.Cause(ctx))
	}
	budget := newPhaseBudget(ctx, 0)
	defer budget.close()
	committed, admissionErr := state.pacer.commit(ctx, 1, budget, func(commit func()) bool { commit(); return true }, nil)
	if committed || !errors.Is(admissionErr, cause) {
		t.Fatalf("commit=%t error=%v", committed, admissionErr)
	}
	<-state.events
	record := (<-state.events).Warmup
	if err := <-done; !errors.Is(err, cause) || record == nil || record.Complete || !errors.Is(record.Failure, cause) {
		t.Fatalf("err=%v record=%+v", err, record)
	}
}
