package timing

import (
	"context"
	"errors"
	"testing"
)

func TestRunLaterSequentialPreparationPreservesEarlierEvidence(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b", "http://one.test/c")
	transport := &campaignTestTransport{preparation: func(_ context.Context, request Request) error {
		if request.ID == 2 {
			return errors.New("preparation unavailable")
		}
		return nil
	}}
	var trial *Trial
	result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Trial != nil {
			trial = event.Trial
		}
		return nil
	}, transport.acquire)
	if err != nil || trial == nil || trial.Complete || !trial.Committed ||
		result.Accounting.MeasuredOfferedOperations != 3 || result.Accounting.RequestOperationsAttempted != 1 {
		t.Fatalf("result=%+v err=%v trial=%+v", result, err, trial)
	}
	first := trial.Outcomes[0]
	if first.Failure != nil || !first.Response.Complete || !first.Timing.FinalResponseHeaders.Present ||
		!first.Timing.Duration.Present {
		t.Fatalf("earlier evidence=%+v", first)
	}
	for _, outcome := range trial.Outcomes[1:] {
		if outcome.Attempted || outcome.Timing.InitialRelease.Present {
			t.Fatalf("unreleased suffix=%+v", outcome)
		}
	}
}

func TestRunNormalPrimingRetirementWithFailedReplacementAcquisition(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.Warmup = 2
	transport := &campaignTestTransport{exchange: func(_ context.Context, lease *campaignTestLease, _ Request) error {
		lease.mu.Lock()
		lease.retired = true
		lease.mu.Unlock()
		lease.dispose(context.Canceled)
		return nil
	}}
	acquisitions := 0
	result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(Event) error { return nil },
		func(ctx context.Context, request Request, policy Plan) (transportLease, error) {
			acquisitions++
			if acquisitions == 2 {
				return nil, errors.New("replacement acquisition unavailable")
			}
			return transport.acquire(ctx, request, policy)
		})
	if err == nil || acquisitions != 2 || result.Accounting.AcquisitionFailures != 1 ||
		result.Accounting.WarmupOperationsComplete != 1 || result.Accounting.TrialsAttempted != 0 || transport.live != 0 {
		t.Fatalf("result=%+v err=%v acquisitions=%d live=%d", result, err, acquisitions, transport.live)
	}
}
