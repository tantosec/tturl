package timing

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type campaignTestTransport struct {
	mu            sync.Mutex
	leases        []*campaignTestLease
	live, maximum int
	exchange      func(context.Context, *campaignTestLease, Request) error
	preparation   func(context.Context, Request) error
}
type campaignTestLease struct {
	owner   *campaignTestTransport
	mu      sync.Mutex
	closed  bool
	retired bool
	active  bool
	index   int
}

func (f *campaignTestTransport) acquire(_ context.Context, _ Request, _ Plan) (transportLease, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	lease := &campaignTestLease{owner: f, index: len(f.leases)}
	f.leases = append(f.leases, lease)
	f.live++
	f.maximum = max(f.maximum, f.live)
	return lease, nil
}

func (l *campaignTestLease) prepare(ctx context.Context, request Request) (transportSender, error) {
	if l.owner.preparation != nil {
		if err := l.owner.preparation(ctx, request); err != nil {
			return nil, err
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.active {
		return nil, errors.New("connection unavailable")
	}
	l.active = true
	return &campaignTestSender{lease: l, request: request}, nil
}
func (l *campaignTestLease) lockState()   { l.mu.Lock() }
func (l *campaignTestLease) unlockState() { l.mu.Unlock() }
func (l *campaignTestLease) usable() (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.usableLocked()
}

func (l *campaignTestLease) usableLocked() (bool, string) {
	return !l.closed, "test retirement"
}

func (l *campaignTestLease) retirement() (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.retired, "test protocol retirement"
}
func (l *campaignTestLease) metadata() transportMetadata { return transportMetadata{} }
func (l *campaignTestLease) dispose(error) {
	l.mu.Lock()
	wasClosed := l.closed
	l.closed = true
	l.mu.Unlock()
	if !wasClosed {
		l.owner.mu.Lock()
		l.owner.live--
		l.owner.mu.Unlock()
	}
}
func (l *campaignTestLease) join() {}

type campaignTestSender struct {
	lease   *campaignTestLease
	request Request
}

func (s *campaignTestSender) waitReady(ctx context.Context, _ releasePhase) error {
	return context.Cause(ctx)
}

func (s *campaignTestSender) readinessSnapshot(phase releasePhase) (bool, <-chan struct{}) {
	s.lease.lockState()
	defer s.lease.unlockState()
	return s.readyLocked(phase) == nil, nil
}

func (s *campaignTestSender) readyLocked(releasePhase) error {
	if s.lease.closed {
		return errors.New("connection closed")
	}
	return nil
}

func (s *campaignTestSender) run(ctx context.Context, gates releaseGates, hooks senderHooks) Outcome {
	defer func() { s.lease.mu.Lock(); s.lease.active = false; s.lease.mu.Unlock() }()
	result := Outcome{}
	if err := awaitGate(ctx, gates.initial); err != nil {
		result.Failure = campaignFailure(PhaseReadiness, err)
		return result
	}
	initial := time.Now()
	if err := hooks.initialRelease(initial); err != nil {
		result.Failure = campaignFailure(PhaseReadiness, err)
		return result
	}
	milestones := requestMilestones{initial: initial, headers: time.Now()}
	if err := hooks.prefixComplete(); err != nil {
		result.Failure = campaignFailure(PhaseExecution, err)
	} else if err := awaitGate(ctx, gates.final); err != nil {
		result.Failure = campaignFailure(PhaseExecution, err)
	} else {
		milestones.final = time.Now()
		if s.lease.owner.exchange != nil {
			result.Failure = campaignFailure(PhaseExecution, s.lease.owner.exchange(ctx, s.lease, s.request))
		}
		if result.Failure == nil {
			milestones.written, milestones.complete = time.Now(), time.Now()
			result.Failure = campaignFailure(PhaseExecution, hooks.exchangeComplete(milestones.complete))
			result.Response.Complete = result.Failure == nil
		}
	}
	result.Timing = campaignTestTiming(milestones.evidence(gates.anchor, "not observed"))
	return result
}

func campaignTestTiming(value timingEvidence) Timing {
	convert := func(offset eventOffset) Offset {
		return Offset{NS: offset.ns, Present: offset.present, Reason: offset.reason}
	}
	return Timing{
		InitialRelease: convert(value.initial), FinalRelease: convert(value.final),
		WriteComplete: convert(value.written), FirstResponseHeaders: convert(value.firstHeaders),
		FinalResponseHeaders: convert(value.headers), ResponseComplete: convert(value.complete),
		Duration: convert(value.duration),
	}
}

func campaignTestPlan(t *testing.T, urls ...string) Plan {
	t.Helper()
	plan := Plan{
		Trials: 1, Arrangement: ArrangeNone, RequestTimeout: time.Second,
		ReceiveHeaderMax: 1 << 20, ResponseBodyMax: 1 << 20,
	}
	for id, url := range urls {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		plan.Requests = append(plan.Requests, Request{ID: id + 1, HTTP: request, Protocol: HTTP11})
	}
	return plan
}

func TestRunWholeCyclesCompleteWorkersAndCompatibleSockets(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://two.test/b", "http://one.test/c")
	plan.Trials, plan.Arrangement, plan.Connections = 4, ArrangeRotate, 5
	transport := &campaignTestTransport{}
	var trials []Trial
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Trial != nil {
			trials = append(trials, *event.Trial)
		}
		return nil
	}, transport.acquire)
	if err != nil || result.Completion != CompletionComplete {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if len(trials) != 6 || result.Accounting.RequestOperationsAttempted != 18 ||
		transport.maximum > 4 || transport.live != 0 || result.Accounting.CyclesComplete != 2 ||
		result.Accounting.CyclesAttempted != 2 || result.Accounting.CyclesUnattempted != 0 {
		t.Fatalf("trials=%d counts=%+v live=%d maximum=%d", len(trials), result.Accounting, transport.live, transport.maximum)
	}
	cycles := make(map[uint64]int)
	for _, trial := range trials {
		if trial.InitialGate.Present || trial.FinalGate.Present ||
			trial.InitialGate.Reason != "no_common_initial_gate" || trial.FinalGate.Reason != "no_common_final_gate" {
			t.Fatalf("sequential trial common gates = %+v / %+v", trial.InitialGate, trial.FinalGate)
		}
		cycle := trial.Index / 3
		if worker, ok := cycles[cycle]; ok && worker != trial.WorkerID {
			t.Fatalf("cycle %d split across workers", cycle)
		}
		cycles[cycle] = trial.WorkerID
		order := []int{1, 2, 3}
		rotation := int(trial.Index % 3)
		order = append(order[rotation:], order[:rotation]...)
		if !slices.Equal(trial.Order, order) {
			t.Fatalf("trial %d order=%v", trial.Index, trial.Order)
		}
		for _, outcome := range trial.Outcomes {
			if outcome.ConnectionID == 0 || !outcome.Timing.Duration.Present || !outcome.Response.Complete {
				t.Fatalf("missing valid observation: %+v", outcome)
			}
		}
	}
}

func TestRunIncompleteTrialPreservesHeadersWithoutReplay(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b", "http://one.test/c")
	plan.Trials = 2
	calls := make(map[int]int)
	transport := &campaignTestTransport{exchange: func(_ context.Context, _ *campaignTestLease, request Request) error {
		calls[request.ID]++
		if request.ID == 2 && calls[2] == 1 {
			return errors.New("body truncated")
		}
		return nil
	}}
	var trials []Trial
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Trial != nil {
			trials = append(trials, *event.Trial)
		}
		return nil
	}, transport.acquire)
	if err != nil || len(trials) != 2 || result.Completion != CompletionComplete {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !slices.Equal([]int{calls[1], calls[2], calls[3]}, []int{2, 2, 1}) ||
		result.Accounting.RequestOperationsAttempted != 5 || result.Accounting.MeasuredOfferedOperations != 6 ||
		result.Accounting.TrialsIncomplete != 1 || result.Accounting.RequestOperationsUnattempted != 1 {
		t.Fatalf("calls=%v counts=%+v", calls, result.Accounting)
	}
	if !trials[0].Outcomes[1].Timing.Duration.Present || trials[0].Outcomes[2].Attempted ||
		trials[0].Outcomes[2].Timing.InitialRelease.Present || !trials[1].Complete {
		t.Fatalf("partial evidence=%+v", trials)
	}
}

func TestRunOutputFailureDisposesAllConnections(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://two.test/b")
	plan.Trials, plan.Connections = 100, 4
	transport := &campaignTestTransport{}
	outputErr := errors.New("output unavailable")
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan},
		func(Event) error { return outputErr }, transport.acquire)
	if !errors.Is(err, outputErr) || result.Failure.Phase != PhaseOutput || transport.live != 0 || transport.maximum > 4 {
		t.Fatalf("result=%+v err=%v live=%d maximum=%d", result, err, transport.live, transport.maximum)
	}
}

func TestRunUnboundedUserStopAndBoundedInterruption(t *testing.T) {
	for _, trials := range []uint64{0, 100} {
		t.Run(map[bool]string{true: "unlimited", false: "bounded"}[trials == 0], func(t *testing.T) {
			plan := campaignTestPlan(t, "http://one.test/a")
			plan.Trials = trials
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := &campaignTestTransport{}
			result, err := runCampaign(ctx, &Resolved{Plan: plan}, func(event Event) error {
				if event.Trial != nil {
					cancel()
				}
				return nil
			}, transport.acquire)
			if transport.live != 0 {
				t.Fatalf("live=%d", transport.live)
			}
			if trials == 0 {
				if err != nil || result.Completion != CompletionStopped {
					t.Fatalf("result=%+v err=%v", result, err)
				}
			} else if err == nil || result.Completion != CompletionFailed {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

func TestRunExecutionTimeoutPreservesCompletedSibling(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b")
	plan.Synchronise = true
	plan.RequestTimeout = 20 * time.Millisecond
	transport := &campaignTestTransport{exchange: func(ctx context.Context, _ *campaignTestLease, request Request) error {
		if request.ID == 1 {
			<-ctx.Done()
			return context.Cause(ctx)
		}
		return nil
	}}
	var trial *Trial
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Trial != nil {
			trial = event.Trial
		}
		return nil
	}, transport.acquire)
	if err != nil || trial == nil || result.Completion != CompletionComplete {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	failed, completed := trial.Outcomes[0], trial.Outcomes[1]
	if failed.Failure == nil || failed.Failure.Phase != PhaseExecution || failed.Failure.Code != "timeout" ||
		completed.Failure != nil || !completed.Response.Complete || !completed.Timing.Duration.Present ||
		result.Accounting.RequestOperationsComplete != 1 || result.Accounting.RequestOperationsFailed != 1 {
		t.Fatalf("trial=%+v counts=%+v", trial, result.Accounting)
	}
}

func TestRunRealMixedProtocolPrimingAndCommonGates(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		t.Run(map[uint16]string{tls.VersionTLS12: "TLS12", tls.VersionTLS13: "TLS13"}[version], func(t *testing.T) {
			var operations atomic.Int64
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || string(body) != "x" {
					t.Errorf("request body=%q err=%v", body, err)
				}
				operations.Add(1)
				w.Header().Set("Trailer", "X-Trace")
				w.WriteHeader(http.StatusTeapot)
				_, _ = io.WriteString(w, "opaque=123")
				w.Header().Set("X-Trace", "received")
			}))
			server.EnableHTTP2 = true
			server.StartTLS()
			defer server.Close()
			plan := campaignTestPlan(t, server.URL+"/a", server.URL+"/b", server.URL+"/c", server.URL+"/d")
			plan.Trials, plan.Arrangement, plan.Warmup = 3, ArrangeRotate, 2
			plan.Synchronise, plan.SingleRecord, plan.LastByteSync = true, true, true
			plan.Capture = Capture{Headers: true, Body: true, BodyMax: 4}
			plan.TLSConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			plan.TLSConfig.MinVersion, plan.TLSConfig.MaxVersion = version, version
			for index := range plan.Requests {
				plan.Requests[index].HTTP.Method = http.MethodPost
				plan.Requests[index].Body = []byte("x")
				if index%2 == 1 {
					plan.Requests[index].Protocol = HTTP2
				}
			}
			resolved, err := Resolve(plan)
			if err != nil {
				t.Fatal(err)
			}
			var trials int
			result, err := Run(context.Background(), resolved, func(event Event) error {
				if event.Warmup != nil {
					record := event.Warmup
					if !record.Complete || !record.InitialGate.Present || !record.FinalGate.Present {
						t.Errorf("warmup gate/completion=%+v", record)
					}
					sockets := make(map[uint64]bool)
					for _, outcome := range record.Outcomes {
						if sockets[outcome.ConnectionID] || !outcome.Response.Complete || outcome.Failure != nil ||
							outcome.Response.Status != http.StatusTeapot || !outcome.Response.CaptureTruncated ||
							outcome.Timing.InitialRelease.NS < record.InitialGate.NS ||
							outcome.Timing.FinalRelease.NS < record.FinalGate.NS {
							t.Errorf("warmup outcome=%+v", outcome)
						}
						sockets[outcome.ConnectionID] = true
					}
				}
				if event.Trial == nil {
					return nil
				}
				trial := event.Trial
				trials++
				if !trial.Complete || !trial.InitialGate.Present || !trial.FinalGate.Present {
					t.Errorf("trial gate/completion=%+v", trial)
				}
				seen := make(map[uint64]bool)
				for _, outcome := range trial.Outcomes {
					if seen[outcome.ConnectionID] {
						t.Errorf("shared active socket %d", outcome.ConnectionID)
					}
					seen[outcome.ConnectionID] = true
					if !outcome.Response.Complete || outcome.Response.Status != http.StatusTeapot ||
						outcome.Response.AcceptedBodyBytes != 10 || string(outcome.Response.Body) != "opaque=123"[:4] ||
						outcome.Response.Trailers.Get("X-Trace") != "received" || !outcome.Timing.Duration.Present ||
						outcome.Timing.InitialRelease.NS < trial.InitialGate.NS || outcome.Timing.FinalRelease.NS < trial.FinalGate.NS {
						t.Errorf("response/timing=%+v", outcome)
					}
				}
				return nil
			})
			if err != nil || result.Completion != CompletionComplete || trials != 4 || operations.Load() != 24 ||
				result.Accounting.WarmupTrials != 2 || result.Accounting.WarmupOperationsAttempted != 8 ||
				result.Accounting.RequestOperationsAttempted != 16 || result.Accounting.ConnectionsAcquired != 4 {
				t.Fatalf("result=%+v err=%v trials=%d operations=%d", result, err, trials, operations.Load())
			}
		})
	}
}

func TestRunPrimingRecoveryIsBoundedBeforeMeasuredWork(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.Warmup = 2
	transport := &campaignTestTransport{exchange: func(_ context.Context, lease *campaignTestLease, _ Request) error {
		lease.mu.Lock()
		lease.retired = true
		lease.mu.Unlock()
		lease.dispose(context.Canceled)
		return nil
	}}
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan},
		func(Event) error { return nil }, transport.acquire)
	if err == nil || result.Failure.Code != "recovery" || result.Accounting.ConnectionsAcquired != 2 ||
		result.Accounting.WarmupTrials != 2 || result.Accounting.TrialsAttempted != 0 ||
		result.Accounting.TrialsUnattempted != 1 || transport.maximum != 1 || transport.live != 0 {
		t.Fatalf("result=%+v err=%v max=%d live=%d", result, err, transport.maximum, transport.live)
	}
}

func TestRunIncompleteWarmupRetainsFailureAndUnattemptedMembership(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a", "http://one.test/b")
	plan.Warmup, plan.Arrangement = 2, ArrangeRotate
	cause := errors.New("stream reset")
	transport := &campaignTestTransport{exchange: func(context.Context, *campaignTestLease, Request) error {
		return cause
	}}
	var record *Warmup
	result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Warmup != nil {
			record = event.Warmup
		}
		return nil
	}, transport.acquire)
	if !errors.Is(err, cause) || record == nil || record.Complete || result.Completion != CompletionFailed ||
		result.Accounting.WarmupTrialsIncomplete != 1 || result.Accounting.WarmupOperationsAttempted != 1 ||
		result.Accounting.WarmupOperationsFailed != 1 || result.Accounting.WarmupOperationsUnattempted != 1 ||
		result.Accounting.TrialsAttempted != 0 || transport.live != 0 {
		t.Fatalf("result=%+v err=%v record=%+v live=%d", result, err, record, transport.live)
	}
}

func TestRunPreparationTimeoutHasNoOfferedCharge(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.RequestTimeout = 20 * time.Millisecond
	transport := &campaignTestTransport{preparation: func(ctx context.Context, _ Request) error {
		<-ctx.Done()
		return context.Cause(ctx)
	}}
	var trial *Trial
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Trial != nil {
			trial = event.Trial
		}
		return nil
	}, transport.acquire)
	if err != nil || trial == nil || trial.Failure.Phase != PhaseReadiness || trial.Failure.Code != "timeout" ||
		trial.Committed || trial.Outcomes[0].Attempted || trial.Outcomes[0].Timing.InitialRelease.Present ||
		result.Accounting.MeasuredOfferedOperations != 0 || result.Accounting.TrialsIncomplete != 1 {
		t.Fatalf("result=%+v err=%v trial=%+v", result, err, trial)
	}
}

func TestRunDeadlineFailsUnboundedAcquisition(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.Trials, plan.RunTimeout = 0, 20*time.Millisecond
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan}, func(Event) error { return nil },
		func(ctx context.Context, _ Request, _ Plan) (transportLease, error) {
			<-ctx.Done()
			return nil, context.Cause(ctx)
		})
	if !errors.Is(err, context.DeadlineExceeded) || result.Completion != CompletionFailed ||
		result.Failure.Phase != PhaseRun || result.Accounting.TrialsAttempted != 0 ||
		result.Accounting.AcquisitionFailures != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRunFinalReportConsumerIsOutsideInteractionBudget(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.RunTimeout = 20 * time.Millisecond
	transport := &campaignTestTransport{}
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Connection != nil && event.Connection.State == "disposed" {
			time.Sleep(2 * plan.RunTimeout)
		}
		return nil
	}, transport.acquire)
	if err != nil || result.Completion != CompletionComplete || transport.live != 0 {
		t.Fatalf("result=%+v err=%v live=%d", result, err, transport.live)
	}
}

func TestRunFinalSuccessfulRetirementIsRecordedWithoutReplacement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, "opaque")
	}))
	defer server.Close()
	plan := campaignTestPlan(t, server.URL)
	resolved, err := Resolve(plan)
	if err != nil {
		t.Fatal(err)
	}
	var retired *Connection
	var trial *Trial
	result, err := Run(context.Background(), resolved, func(event Event) error {
		if event.Connection != nil && event.Connection.State == "retired" {
			retired = event.Connection
		}
		if event.Trial != nil {
			trial = event.Trial
		}
		return nil
	})
	if err != nil || retired == nil || retired.Failure != nil || trial == nil || !trial.Complete ||
		!trial.Outcomes[0].Timing.Duration.Present || !trial.Outcomes[0].Response.Complete ||
		result.Accounting.ConnectionsRetired != 1 || result.Accounting.ConnectionsReplaced != 0 {
		t.Fatalf("result=%+v err=%v retired=%+v trial=%+v", result, err, retired, trial)
	}
}

func TestRunOutputErrorPreservesIndependentFatalCause(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.Trials = 0
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(context.Canceled)
	fatal := &Failure{Phase: PhaseAcquisition, Code: "acquisition", Message: "target unavailable"}
	outputErr := errors.New("output unavailable")
	result, err := runCampaign(ctx, &Resolved{Plan: plan}, func(Event) error {
		cancel(fatal)
		return outputErr
	}, func(context.Context, Request, Plan) (transportLease, error) { return nil, fatal })
	if result.Failure != fatal || !errors.Is(err, fatal) || result.Completion != CompletionFailed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestRunBoundedStopAfterAllPublishedTrialsCompletes(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := &campaignTestTransport{}
	result, err := runCampaign(ctx, &Resolved{Plan: plan}, func(event Event) error {
		if event.Trial != nil {
			cancel()
		}
		return nil
	}, transport.acquire)
	if err != nil || result.Completion != CompletionComplete || result.Accounting.TrialsAttempted != 1 ||
		transport.live != 0 {
		t.Fatalf("result=%+v err=%v live=%d", result, err, transport.live)
	}
}

func TestRunIndependentFailureWrappingCancellationDoesNotFulfilStop(t *testing.T) {
	plan := campaignTestPlan(t, "http://one.test/a")
	plan.Trials = 0
	fatal := &Failure{
		Phase: PhaseAcquisition, Code: "acquisition",
		Message: "target acquisition cancelled", Cause: context.Canceled,
	}
	result, err := runCampaign(context.Background(), &Resolved{Plan: plan}, func(Event) error { return nil },
		func(context.Context, Request, Plan) (transportLease, error) { return nil, fatal })
	if !errors.Is(err, fatal) || result.Completion != CompletionFailed || result.Failure != fatal {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
