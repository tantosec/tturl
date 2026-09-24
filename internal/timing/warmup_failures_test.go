package timing

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
)

func distinctWarmupRequests(t *testing.T, urls ...string) []Request {
	t.Helper()
	requests := campaignTestPlan(t, urls...).Requests
	for index := range requests {
		requests[index].ID += 100
	}
	return requests
}

func TestRunSequentialReplacementWarmsWholeWorker(t *testing.T) {
	for _, incomplete := range []bool{false, true} {
		name := "complete"
		if incomplete {
			name = "incomplete"
		}
		t.Run(name, func(t *testing.T) {
			plan := campaignTestPlan(t, "http://one.test/a", "http://two.test/b", "http://one.test/c")
			plan.Warmup = 1
			plan.Priming = distinctWarmupRequests(t, "http://one.test/w", "http://two.test/w")
			cause := errors.New("replacement response truncated")
			var exchanges []int
			transport := &campaignTestTransport{exchange: func(
				_ context.Context, lease *campaignTestLease, request Request,
			) error {
				exchanges = append(exchanges, request.ID)
				if request.ID == 1 {
					lease.mu.Lock()
					lease.retired = true
					lease.mu.Unlock()
					lease.dispose(context.Canceled)
				}
				if incomplete && request.ID == 101 && lease.index == 2 {
					return cause
				}
				return nil
			}}
			var trials []Trial
			var warmups []Warmup
			result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
				if event.Trial != nil {
					trials = append(trials, *event.Trial)
				}
				if event.Warmup != nil {
					warmups = append(warmups, *event.Warmup)
				}
				return nil
			}, transport.acquire)
			if len(trials) != 1 || len(warmups) != 2 || transport.live != 0 || result.Accounting.ConnectionsReplaced != 1 {
				t.Fatalf("result=%+v err=%v trials=%+v warmups=%+v live=%d", result, err, trials, warmups, transport.live)
			}
			trial := trials[0]
			first := trial.Outcomes[0]
			if !first.Attempted || !first.Response.Complete || !first.Timing.Duration.Present || first.Failure != nil ||
				warmups[1].Phase != "replacement" ||
				warmups[1].PhaseIndex != 1 ||
				!slices.Equal(warmups[1].Order, []int{101, 102}) {
				t.Fatalf("measured evidence=%+v replacement=%+v", trial, warmups[1])
			}
			if incomplete {
				if !errors.Is(err, cause) || result.Completion != CompletionFailed || trial.Complete ||
					!slices.Equal(exchanges, []int{101, 102, 1, 101}) || warmups[1].Complete ||
					result.Accounting.RequestOperationsComplete != 1 || result.Accounting.RequestOperationsUnattempted != 2 ||
					trial.Outcomes[1].Attempted || trial.Outcomes[2].Attempted {
					t.Fatalf("terminal result=%+v err=%v trial=%+v exchanges=%v", result, err, trial, exchanges)
				}
			} else {
				if err != nil || result.Completion != CompletionComplete || !trial.Complete ||
					!slices.Equal(exchanges, []int{101, 102, 1, 101, 102, 2, 3}) ||
					warmups[1].Outcomes[0].ConnectionID == first.ConnectionID ||
					warmups[1].Outcomes[1].ConnectionID != trial.Outcomes[1].ConnectionID ||
					trial.Outcomes[2].ConnectionID != warmups[1].Outcomes[0].ConnectionID {
					t.Fatalf("replacement result=%+v err=%v trial=%+v warmups=%+v exchanges=%v",
						result, err, trial, warmups, exchanges)
				}
			}
		})
	}
}

type joinedCampaignLease struct {
	transportLease
	joined *atomic.Int64
}

func (l *joinedCampaignLease) join() {
	l.transportLease.join()
	l.joined.Add(1)
}

func TestRunInitialAcquisitionFailureJoinsAdmittedWorkers(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "failure"
		if cancelled {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			plan := campaignTestPlan(t, "http://one.test/a")
			plan.Trials, plan.Connections, plan.Warmup = 2, 2, 1
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			acquired := make(chan struct{})
			secondStarted := make(chan struct{})
			var acquisitions, joins, exchanges atomic.Int64
			transport := &campaignTestTransport{exchange: func(context.Context, *campaignTestLease, Request) error {
				exchanges.Add(1)
				return nil
			}}
			cause := errors.New("destination unavailable")
			factory := func(ctx context.Context, request Request, plan Plan) (transportLease, error) {
				if acquisitions.Add(1) == 1 {
					lease, err := transport.acquire(ctx, request, plan)
					return &joinedCampaignLease{transportLease: lease, joined: &joins}, err
				}
				close(secondStarted)
				<-acquired
				if cancelled {
					<-ctx.Done()
					return nil, context.Cause(ctx)
				}
				return nil, cause
			}
			result, err := runCampaign(ctx, &Resolved{Plan: plan}, func(event Event) error {
				if event.Connection != nil && event.Connection.State == "acquired" {
					close(acquired)
					if cancelled {
						<-secondStarted
						cancel()
					}
				}
				return nil
			}, factory)
			want := cause
			if cancelled {
				want = context.Canceled
			}
			if !errors.Is(err, want) || result.Completion != CompletionFailed || acquisitions.Load() != 2 || joins.Load() != 1 ||
				transport.live != 0 || exchanges.Load() != 0 || result.Accounting.TrialsUnattempted != 2 {
				t.Fatalf("result=%+v err=%v acquired=%d joined=%d exchanges=%d live=%d",
					result, err, acquisitions.Load(), joins.Load(), exchanges.Load(), transport.live)
			}
		})
	}
}

func TestRunZeroWarmupPublishesInitialBoundaries(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.Trials, plan.Connections = 2, 2
	var exchanges atomic.Int64
	transport := &campaignTestTransport{exchange: func(context.Context, *campaignTestLease, Request) error {
		exchanges.Add(1)
		return nil
	}}
	var phases []string
	acquired, workerPhases := 0, 0
	result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Connection != nil && event.Connection.State == "acquired" {
			acquired++
		}
		if phase := event.Phase; phase != nil {
			phases = append(phases, phase.Phase)
			switch phase.Phase {
			case "initial_acquisition":
				if acquired != 2 {
					t.Errorf("initial acquisition admitted %d connections", acquired)
				}
			case "worker_warmup":
				workerPhases++
			case "initial_warmup":
				if workerPhases != 2 {
					t.Errorf("initial warmup admitted %d workers", workerPhases)
				}
			}
		}
		if event.Trial != nil && (len(phases) != 4 || phases[3] != "initial_warmup") {
			t.Errorf("measured trial before phase boundary: %v", phases)
		}
		return nil
	}, transport.acquire)
	if err != nil || result.Completion != CompletionComplete || exchanges.Load() != 2 || transport.live != 0 ||
		!slices.Equal(phases, []string{"initial_acquisition", "worker_warmup", "worker_warmup", "initial_warmup"}) ||
		result.Accounting.WarmupOperationsAttempted != 0 || result.Accounting.RequestOperationsComplete != 2 {
		t.Fatalf("result=%+v err=%v phases=%v exchanges=%d live=%d", result, err, phases, exchanges.Load(), transport.live)
	}
}

func TestRunDistinctWarmupRotatesItsOwnMembership(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://two.test/b")
	plan.Arrangement, plan.Warmup = ArrangeRotate, 4
	plan.Priming = distinctWarmupRequests(t, "http://one.test/w1", "http://two.test/w2", "http://one.test/w3")
	transport := &campaignTestTransport{}
	var warmups []Warmup
	result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Warmup != nil {
			warmups = append(warmups, *event.Warmup)
		}
		return nil
	}, transport.acquire)
	if err != nil ||
		result.Completion != CompletionComplete ||
		len(warmups) != 4 ||
		result.Accounting.WarmupOperationsComplete != 12 ||
		result.Accounting.RequestOperationsComplete != 4 || transport.live != 0 {
		t.Fatalf("result=%+v err=%v warmups=%+v live=%d", result, err, warmups, transport.live)
	}
	for index, warmup := range warmups {
		order := []int{101, 102, 103}
		rotation := index % len(order)
		order = append(order[rotation:], order[:rotation]...)
		if !warmup.Complete || warmup.TrialIndex != index || !slices.Equal(warmup.Order, order) {
			t.Fatalf("warmup %d: %+v want order=%v", index, warmup, order)
		}
	}
}

func TestResolveSynchronisedWarmupMatchesEachPool(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b", "http://two.test/c")
	plan.Synchronise, plan.Warmup = true, 1
	plan.Priming = distinctWarmupRequests(t, "http://one.test/w1", "http://two.test/w2", "http://two.test/w3")
	if _, err := Resolve(plan); err == nil {
		t.Fatal("accepted unequal warmup membership within measured pools")
	}
	plan.Priming = distinctWarmupRequests(t, "http://one.test/w1", "http://two.test/w2", "http://one.test/w3")
	resolved, err := Resolve(plan)
	if err != nil {
		t.Fatal(err)
	}
	transport := &campaignTestTransport{}
	var warmup *Warmup
	result, err := runCampaign(t.Context(), resolved, func(event Event) error {
		if event.Warmup != nil {
			warmup = event.Warmup
		}
		return nil
	}, transport.acquire)
	if err != nil ||
		result.Completion != CompletionComplete ||
		warmup == nil ||
		!warmup.Complete ||
		!warmup.InitialGate.Present ||
		len(warmup.Outcomes) != 3 || transport.maximum != 3 || transport.live != 0 {
		t.Fatalf("result=%+v err=%v warmup=%+v max=%d live=%d", result, err, warmup, transport.maximum, transport.live)
	}
	seen := make(map[uint64]bool)
	for _, outcome := range warmup.Outcomes {
		if seen[outcome.ConnectionID] ||
			!outcome.Response.Complete ||
			outcome.Timing.InitialRelease.NS < warmup.InitialGate.NS {
			t.Fatalf("synchronised membership: %+v", warmup)
		}
		seen[outcome.ConnectionID] = true
	}
}

func TestRunSequentialCompleteRetirementMakesForwardProgress(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b", "http://one.test/c")
	var requests []int
	transport := &campaignTestTransport{exchange: func(
		_ context.Context, lease *campaignTestLease, request Request,
	) error {
		requests = append(requests, request.ID)
		lease.mu.Lock()
		lease.retired = true
		lease.mu.Unlock()
		lease.dispose(context.Canceled)
		return nil
	}}
	var record *Trial
	result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Trial != nil {
			record = event.Trial
		}
		return nil
	}, transport.acquire)
	if err != nil || record == nil || !record.Complete || result.Completion != CompletionComplete ||
		!slices.Equal(requests, []int{1, 2, 3}) || result.Accounting.RequestOperationsComplete != 3 ||
		result.Accounting.ConnectionsAcquired != 3 || result.Accounting.ConnectionsReplaced != 2 || transport.live != 0 {
		t.Fatalf("result=%+v err=%v record=%+v requests=%v live=%d", result, err, record, requests, transport.live)
	}
}
