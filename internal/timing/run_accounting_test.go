package timing

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func assertRequestAccountingTotals(t *testing.T, result Result) {
	t.Helper()
	var attempted, unattempted uint64
	for _, count := range result.Requests {
		attempted += count.Attempted
		unattempted += count.Unattempted
	}
	if attempted != result.Accounting.RequestOperationsAttempted ||
		unattempted != result.Accounting.RequestOperationsUnattempted {
		t.Fatalf("request totals %d/%d disagree with global %+v", attempted, unattempted, result.Accounting)
	}
}

func TestRunRequestAccountingInitialAcquisitionFailure(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b")
	plan.Trials = 3
	plan.Requests[0].ID, plan.Requests[1].ID = 41, 7
	failure := errors.New("acquisition failed")
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan}, func(Event) error { return nil },
		func(context.Context, Request, Plan) (transportLease, error) { return nil, failure })
	want := []RequestAccounting{{RequestID: 41, Unattempted: 3}, {RequestID: 7, Unattempted: 3}}
	if !errors.Is(err, failure) || !slices.Equal(result.Requests, want) {
		t.Fatalf("result %+v error %v; want %v", result, err, want)
	}
	assertRequestAccountingTotals(t, result)
}

func TestRunRequestAccountingExcludesPrimingAndUsesPlannedRotation(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b")
	plan.Requests[0].ID, plan.Requests[1].ID = 9, 2
	plan.Trials, plan.Warmup, plan.Arrangement = 3, 1, ArrangeRotate
	transport := &campaignTestTransport{}
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan}, func(Event) error { return nil },
		transport.acquire)
	want := []RequestAccounting{{RequestID: 9, Attempted: 4}, {RequestID: 2, Attempted: 4}}
	if err != nil || !slices.Equal(result.Requests, want) || result.Accounting.WarmupOperationsAttempted == 0 {
		t.Fatalf("result %+v error %v; want %v", result, err, want)
	}
	assertRequestAccountingTotals(t, result)
}

func TestCampaignBoundedPublicationRetainsProducedEvidence(t *testing.T) {
	state := &campaignState{
		events: make(chan Event, 1), requests: []RequestAccounting{{RequestID: 9}, {RequestID: 2}},
		requestIndexes: map[int]int{9: 0, 2: 1},
	}
	first := Event{Trial: &Trial{Outcomes: []Outcome{{RequestID: 9, Attempted: true}, {RequestID: 2}}}}
	second := Event{Trial: &Trial{Outcomes: []Outcome{{RequestID: 9}, {RequestID: 2, Attempted: true}}}}
	state.publish(first)
	done := make(chan struct{})
	go func() { state.publish(second); close(done) }()
	delivered := <-state.events
	if delivered.Trial != first.Trial {
		t.Fatalf("first delivery=%+v", delivered)
	}
	<-done
	delivered = <-state.events
	if delivered.Trial != second.Trial {
		t.Fatalf("second delivery=%+v", delivered)
	}
	want := []RequestAccounting{{RequestID: 9, Attempted: 1, Unattempted: 1}, {RequestID: 2, Attempted: 1, Unattempted: 1}}
	if !slices.Equal(state.requests, want) {
		t.Fatalf("requests=%v want=%v", state.requests, want)
	}
	state.publish(Event{Warmup: &Warmup{Outcomes: second.Trial.Outcomes}})
	if event := <-state.events; event.Warmup == nil {
		t.Fatalf("warmup delivery=%+v", event)
	}
	if !slices.Equal(state.requests, want) {
		t.Fatalf("measured accounting=%v want=%v", state.requests, want)
	}
	assertRequestAccountingTotals(t, Result{Requests: state.requests, Accounting: state.counts})
}

func TestRunRequestAccountingReconcilesCancelledDelivery(t *testing.T) {
	for _, trials := range []uint64{0, 100} {
		plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b")
		plan.Trials = trials
		ctx, cancel := context.WithCancel(context.Background())
		transport := &campaignTestTransport{}
		result, err := runCampaign(ctx, &Resolved{Plan: plan}, func(event Event) error {
			if event.Trial != nil {
				cancel()
			}
			return nil
		}, transport.acquire)
		cancel()
		if len(result.Requests) != 2 || result.Requests[0].RequestID != 1 || result.Requests[1].RequestID != 2 {
			t.Fatalf("unstable accounting identities %v", result.Requests)
		}
		if trials == 0 && (err != nil || result.Completion != CompletionStopped) {
			t.Fatalf("result %+v error %v", result, err)
		}
		if trials > 0 && err == nil {
			t.Fatal("unfinished bounded work succeeded")
		}
		assertRequestAccountingTotals(t, result)
		if trials > 0 {
			for _, count := range result.Requests {
				if count.Attempted+count.Unattempted != trials {
					t.Fatalf("finite work lost: %+v", count)
				}
			}
		}
	}
}
