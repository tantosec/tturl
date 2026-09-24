package timing

// cspell:ignore synctest
import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	virtual "testing/synctest"
)

func TestRunRepresentativeWarmupCostsAndRotation(t *testing.T) {
	for _, synchronise := range []bool{false, true} {
		t.Run(fmt.Sprint(synchronise), func(t *testing.T) {
			plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b", "http://one.test/c")
			plan.Warmup, plan.Arrangement, plan.Synchronise = 2, ArrangeRotate, synchronise
			transport := &campaignTestTransport{}
			var warmups []Warmup
			var trials []Trial
			result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
				if event.Warmup != nil {
					warmups = append(warmups, *event.Warmup)
				}
				if event.Trial != nil {
					trials = append(trials, *event.Trial)
				}
				return nil
			}, transport.acquire)
			if err != nil ||
				result.Accounting.WarmupTrialsComplete != 2 ||
				result.Accounting.WarmupOperationsComplete != 6 ||
				len(warmups) != 2 {
				t.Fatalf("result=%+v err=%v records=%+v", result, err, warmups)
			}
			for index, expected := range [][]int{{1, 2, 3}, {2, 3, 1}} {
				record := warmups[index]
				if !slices.Equal(record.Order, expected) ||
					record.Phase != "initial" ||
					record.PhaseIndex != 0 ||
					record.TrialIndex != index {
					t.Fatalf("record=%+v expected=%v", record, expected)
				}
				if synchronise {
					connections := make(map[uint64]bool)
					for _, outcome := range record.Outcomes {
						connections[outcome.ConnectionID] = true
					}
					if len(connections) != 3 || !record.InitialGate.Present {
						t.Fatalf("record=%+v", record)
					}
				}
			}
			if !slices.Equal(trials[0].Order, []int{1, 2, 3}) ||
				result.Accounting.TrialsAttempted != 3 ||
				result.Accounting.CyclesComplete != 1 {
				t.Fatalf("measured=%+v result=%+v", trials, result)
			}
		})
	}
}

func TestRunWarmupEveryAdmittedWorker(t *testing.T) {
	urls := make([]string, 128)
	for index := range urls {
		urls[index] = fmt.Sprintf("http://one.test/%d", index)
	}
	plan := campaignTestPlan(t, urls...)
	plan.Synchronise, plan.Connections, plan.Trials, plan.Warmup = true, 512, 4, 10
	transport := &campaignTestTransport{}
	perWorker := make(map[int]int)
	result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Warmup != nil {
			perWorker[event.Warmup.WorkerID]++
		}
		return nil
	}, transport.acquire)
	if err != nil ||
		result.Accounting.WarmupTrialsComplete != 40 ||
		result.Accounting.WarmupOperationsComplete != 5120 ||
		len(perWorker) != 4 {
		t.Fatalf("result=%+v err=%v workers=%v", result, err, perWorker)
	}
	for worker, count := range perWorker {
		if count != 10 {
			t.Fatalf("worker=%d count=%d", worker, count)
		}
	}
}

func TestRunInitialPhasesFollowAllAdmittedWorkers(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.Connections, plan.Trials, plan.Warmup = 2, 2, 1
	var acquisitions, warmExchanges atomic.Int32
	var calls [2]atomic.Int32
	releaseSlowWarmup := make(chan struct{})
	transport := &campaignTestTransport{exchange: func(ctx context.Context, lease *campaignTestLease, _ Request) error {
		if acquisitions.Load() != 2 {
			return fmt.Errorf("release with %d acquisitions", acquisitions.Load())
		}
		call := calls[lease.index].Add(1)
		if call == 1 {
			if lease.index == 1 {
				select {
				case <-releaseSlowWarmup:
				case <-ctx.Done():
					return context.Cause(ctx)
				}
			}
			warmExchanges.Add(1)
		} else if warmExchanges.Load() != 2 {
			return fmt.Errorf("measurement release with %d warmup exchanges", warmExchanges.Load())
		}
		return nil
	}}
	phases := make(map[string]int)
	result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Phase != nil {
			phases[event.Phase.Phase]++
			if event.Phase.Phase == "worker_warmup" && phases["worker_warmup"] == 1 {
				close(releaseSlowWarmup)
			}
		}
		return nil
	}, func(ctx context.Context, request Request, policy Plan) (transportLease, error) {
		lease, err := transport.acquire(ctx, request, policy)
		acquisitions.Add(1)
		return lease, err
	})
	if err != nil || result.Completion != CompletionComplete || result.Accounting.TrialsComplete != 2 ||
		phases["initial_acquisition"] != 1 ||
		phases["initial_warmup"] != 1 ||
		phases["worker_warmup"] != 2 ||
		transport.live != 0 {
		t.Fatalf("result=%+v err=%v phases=%v live=%d", result, err, phases, transport.live)
	}
}

func TestRunDelayedAcquisitionHoldsAllInitialWarmup(t *testing.T) {
	virtual.Test(t, func(t *testing.T) {
		plan := campaignTestPlan(t, "http://one.test/a")
		plan.Connections, plan.Trials, plan.Warmup = 2, 2, 1
		var acquisitions, exchanges atomic.Int32
		release := make(chan struct{})
		started := make(chan struct{})
		transport := &campaignTestTransport{exchange: func(context.Context, *campaignTestLease, Request) error {
			exchanges.Add(1)
			return nil
		}}
		done := make(chan Result, 1)
		go func() {
			result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(Event) error { return nil },
				func(ctx context.Context, request Request, policy Plan) (transportLease, error) {
					if acquisitions.Add(1) == 2 {
						close(started)
						select {
						case <-release:
						case <-ctx.Done():
							return nil, context.Cause(ctx)
						}
					}
					return transport.acquire(ctx, request, policy)
				})
			if err != nil {
				t.Errorf("run: %v", err)
			}
			done <- result
		}()
		<-started
		virtual.Wait()
		if count := exchanges.Load(); count != 0 {
			t.Fatalf("warmup released %d exchanges during acquisition", count)
		}
		close(release)
		result := <-done
		if result.Completion != CompletionComplete ||
			result.Accounting.WarmupOperationsComplete != 2 ||
			result.Accounting.TrialsComplete != 2 ||
			transport.live != 0 {
			t.Fatalf("result=%+v live=%d", result, transport.live)
		}
	})
}

func TestRunReadyWorkerRetirementRestartsItsWarmupPhase(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.Connections, plan.Trials, plan.Warmup = 2, 2, 1
	release := make(chan struct{})
	transport := &campaignTestTransport{exchange: func(ctx context.Context, lease *campaignTestLease, _ Request) error {
		if lease.index == 1 {
			select {
			case <-release:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		return nil
	}}
	var records []Warmup
	result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Warmup != nil {
			record := *event.Warmup
			records = append(records, record)
			if len(records) == 1 {
				transport.mu.Lock()
				lease := transport.leases[0]
				transport.mu.Unlock()
				lease.mu.Lock()
				lease.retired = true
				lease.mu.Unlock()
				lease.dispose(context.Canceled)
				close(release)
			}
		}
		return nil
	}, transport.acquire)
	if err != nil ||
		result.Completion != CompletionComplete ||
		len(records) != 3 ||
		result.Accounting.ReplacementWarmupTrials != 1 ||
		result.Accounting.ConnectionsReplaced != 1 {
		t.Fatalf("result=%+v err=%v records=%+v", result, err, records)
	}
	var replacement Warmup
	for _, record := range records {
		if record.Phase == "replacement" {
			replacement = record
		}
	}
	if replacement.PhaseIndex != 1 || replacement.TrialIndex != 0 || replacement.Phase != "replacement" {
		t.Fatalf("record=%+v", replacement)
	}
}
