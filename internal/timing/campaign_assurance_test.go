package timing

import (
	"bytes"
	"context"
	"crypto/tls"
	"math"
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func TestRunInvalidProtocolLimitsAreRejectedBeforeAcquisition(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Plan)
	}{
		{"header_bound", func(p *Plan) { p.Requests[0].Protocol = HTTP2; p.ReceiveHeaderMax = int64(math.MaxUint32) + 1 }},
		{"TLS_range", func(p *Plan) {
			//nolint:gosec // An inverted TLS interval must fail before traffic.
			p.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS12}
		}},
		{"TLS_minimum", func(p *Plan) { p.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13 + 1} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := campaignTestPlan(t, "https://one.test/a")
			test.mutate(&plan)
			if _, err := Resolve(plan); err == nil {
				t.Fatal("invalid protocol plan resolved")
			}
			acquisitions := 0
			_, err := runCampaign(context.Background(), &Resolved{Plan: plan}, func(Event) error { return nil },
				func(context.Context, Request, Plan) (transportLease, error) { acquisitions++; return nil, nil })
			if err == nil || acquisitions != 0 {
				t.Fatalf("err=%v acquisitions=%d", err, acquisitions)
			}
		})
	}
}

func TestRunPreparationFailureCancelsAndDisposesPreparedPeers(t *testing.T) {
	first, second := h2NewFixture(t, tls.VersionTLS13), h2NewFixture(t, tls.VersionTLS13)
	plan := campaignTestPlan(t, "https://one.test/a", "https://two.test/b")
	plan.Synchronise, plan.SingleRecord, plan.LastByteSync = true, true, true
	for index := range plan.Requests {
		plan.Requests[index].Protocol = HTTP2
		plan.Requests[index].HTTP.Method = http.MethodPost
		plan.Requests[index].Body = []byte("x")
	}
	plan.Requests[1].Body = bytes.Repeat([]byte("x"), 2*h1RecordLimit)
	preparedPeer := false
	var trial *Trial
	result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
		if event.Trial != nil {
			trial = event.Trial
		}
		return nil
	}, func(_ context.Context, request Request, policy Plan) (transportLease, error) {
		if request.ID == 1 {
			return &h2Lease{conn: first.client, plan: policy}, nil
		}
		return &campaignObservePrepareLease{transportLease: &h2Lease{conn: second.client, plan: policy}, observe: func() {
			first.client.mu.Lock()
			defer first.client.mu.Unlock()
			preparedPeer = first.client.active != nil && first.client.active.Sent == 0
		}}, nil
	})
	if err != nil || !preparedPeer || trial == nil || trial.Committed || trial.Complete ||
		result.Accounting.MeasuredOfferedOperations != 0 || result.Accounting.RequestOperationsAttempted != 0 {
		t.Fatalf("result=%+v err=%v prepared=%t trial=%+v", result, err, preparedPeer, trial)
	}
	for index, outcome := range trial.Outcomes {
		if outcome.Failure == nil || outcome.Failure.Phase != PhaseReadiness || outcome.Attempted ||
			outcome.Timing.InitialRelease.Present || outcome.Timing.InitialRelease.Reason != "unattempted" {
			t.Fatalf("outcome=%+v", outcome)
		}
		if index == 0 && outcome.Failure.Code != "cancelled" || index == 1 && outcome.Failure.Code == "cancelled" {
			t.Fatalf("initiator and sibling classifications=%+v", trial.Outcomes)
		}
	}
	for _, fixture := range []*h2Fixture{first, second} {
		for _, done := range []<-chan struct{}{fixture.client.readerDone, fixture.client.writerDone} {
			select {
			case <-done:
			default:
				t.Fatal("prepared connection pump was not joined")
			}
		}
		for len(fixture.frames) > 0 {
			if frame := <-fixture.frames; frame.Frame.StreamID != 0 {
				t.Fatal("preparation failure released request traffic")
			}
		}
	}
}

type campaignObservePrepareLease struct {
	transportLease
	observe func()
}

func (l *campaignObservePrepareLease) prepare(ctx context.Context, request Request) (transportSender, error) {
	l.observe()
	return l.transportLease.prepare(ctx, request)
}

func TestRunEarlyCompletedResponseSurvivesResetBeforeFinalGate(t *testing.T) {
	fixture := h2NewFixture(t, tls.VersionTLS13)
	fixture.send(t, func(f *http2.Framer) error {
		return f.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0})
	})
	waitH2State(t, fixture, func() bool { return fixture.client.initialWindow == 0 })
	plan := campaignTestPlan(t, "https://one.test/a")
	plan.Requests[0].Protocol = HTTP2
	plan.Requests[0].HTTP.Method = http.MethodPost
	plan.Requests[0].Body = []byte("x")
	plan.Synchronise, plan.SingleRecord, plan.LastByteSync = true, true, true
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		for {
			select {
			case frame := <-fixture.frames:
				if frame.Frame.StreamID == 0 || len(frame.Fields) == 0 {
					continue
				}
				fixture.respond(t, frame.Frame.StreamID, []hpack.HeaderField{
					{Name: ":status", Value: "200"}, {Name: "content-length", Value: "0"},
				}, true)
				waitH2State(t, fixture, func() bool { return fixture.client.active != nil && fixture.client.active.responseValid })
				fixture.send(t, func(f *http2.Framer) error { return f.WriteRSTStream(frame.Frame.StreamID, http2.ErrCodeCancel) })
				return
			case <-fixture.ctx.Done():
				return
			}
		}
	}()
	var trial *Trial
	result, err := runCampaign(fixture.ctx, &Resolved{Plan: plan}, func(event Event) error {
		if event.Trial != nil {
			trial = event.Trial
		}
		return nil
	}, func(_ context.Context, _ Request, policy Plan) (transportLease, error) {
		return &h2Lease{conn: fixture.client, plan: policy}, nil
	})
	<-peerDone
	if err != nil || trial == nil || trial.Complete || !trial.Committed || trial.FinalGate.Present {
		t.Fatalf("result=%+v err=%v trial=%+v", result, err, trial)
	}
	outcome := trial.Outcomes[0]
	if outcome.Failure == nil || !outcome.Response.Complete || !outcome.Response.Reset || !outcome.EarlyResponse ||
		!outcome.Timing.Duration.Present || !outcome.Timing.ResponseComplete.Present || outcome.Timing.FinalRelease.Present ||
		result.Accounting.RequestOperationsComplete != 0 || result.Accounting.RequestOperationsFailed != 1 {
		t.Fatalf("outcome=%+v counts=%+v", outcome, result.Accounting)
	}
}

func TestRunJoinedSynchronisedTargetsExcludeFinalOutputBackpressure(t *testing.T) {
	fixtures := []*h2Fixture{
		h2NewFixture(t, tls.VersionTLS13), h2NewFixture(t, tls.VersionTLS13), h2NewFixture(t, tls.VersionTLS13),
	}
	plan := campaignTestPlan(t, "https://one.test/a", "https://two.test/b", "https://three.test/c")
	plan.Synchronise, plan.SingleRecord, plan.LastByteSync = true, true, true
	plan.RunTimeout = 200 * time.Millisecond
	guard, cancelGuard := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelGuard()
	for index := range plan.Requests {
		plan.Requests[index].Protocol = HTTP2
		plan.Requests[index].HTTP.Method = http.MethodPost
		plan.Requests[index].Body = []byte("x")
	}
	for _, fixture := range fixtures {
		go func() {
			for {
				select {
				case frame := <-fixture.frames:
					if frame.Frame.Type == http2.FrameData && frame.Frame.Flags.Has(http2.FlagDataEndStream) {
						fixture.respond(t, frame.Frame.StreamID, []hpack.HeaderField{
							{Name: ":status", Value: "200"}, {Name: "content-length", Value: "0"},
						}, true)
						return
					}
				case <-fixture.ctx.Done():
					return
				}
			}
		}()
	}
	trialArrived, allowConsumer := make(chan struct{}), make(chan struct{})
	releaseConsumer := sync.OnceFunc(func() { close(allowConsumer) })
	defer releaseConsumer()
	type completion struct {
		result Result
		err    error
	}
	finished := make(chan completion, 1)
	go func() {
		result, err := runCampaign(t.Context(), &Resolved{Plan: plan}, func(event Event) error {
			if event.Trial != nil {
				close(trialArrived)
				select {
				case <-allowConsumer:
				case <-guard.Done():
					return context.Cause(guard)
				}
			}
			return nil
		}, func(_ context.Context, request Request, policy Plan) (transportLease, error) {
			return &h2Lease{conn: fixtures[request.ID-1].client, plan: policy}, nil
		})
		finished <- completion{result, err}
	}()
	select {
	case <-trialArrived:
	case <-guard.Done():
		t.Fatal("final trial did not arrive")
	}
	for _, fixture := range fixtures {
		for _, joined := range []<-chan struct{}{fixture.client.readerDone, fixture.client.writerDone} {
			select {
			case <-joined:
			case <-guard.Done():
				t.Fatal("target cleanup waited for report publication")
			}
		}
	}
	// The consumer remains blocked beyond the entire finite interaction budget.
	// All real connection pumps have already joined before this deliberate wait.
	<-time.After(2 * plan.RunTimeout)
	releaseConsumer()
	select {
	case outcome := <-finished:
		if outcome.err != nil || outcome.result.Completion != CompletionComplete ||
			outcome.result.Accounting.RequestOperationsComplete != 3 {
			t.Fatalf("result=%+v err=%v", outcome.result, outcome.err)
		}
	case <-guard.Done():
		t.Fatal("final output drain did not finish")
	}
}

type campaignAfterRunLease struct {
	transportLease
	after func()
}

func (l *campaignAfterRunLease) prepare(ctx context.Context, request Request) (transportSender, error) {
	sender, err := l.transportLease.prepare(ctx, request)
	if err != nil {
		return nil, err
	}
	return &campaignAfterRunSender{transportSender: sender, after: l.after}, nil
}

type campaignAfterRunSender struct {
	transportSender
	after func()
}

func (s *campaignAfterRunSender) run(ctx context.Context, gates releaseGates, hooks senderHooks) Outcome {
	outcome := s.transportSender.run(ctx, gates, hooks)
	s.after()
	return outcome
}

func TestRunLateCallerCancellationPreservesCompletedMembership(t *testing.T) {
	for _, test := range []struct {
		name   string
		width  int
		warmup bool
	}{
		{"trial_full", 1, false},
		{"trial_suffix", 2, false},
		{"warmup_full", 1, true},
		{"warmup_suffix", 2, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			width := test.width
			plan := campaignTestPlan(t, "http://one.test/a")
			if width == 2 {
				second := plan.Requests[0]
				second.ID = 2
				plan.Requests = append(plan.Requests, second)
			}
			if test.warmup {
				plan.Warmup = 1
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			transport := &campaignTestTransport{}
			acquired := make(chan struct{})
			var trial *Trial
			_, _ = runCampaign(ctx, &Resolved{Plan: plan}, func(event Event) error {
				if event.Connection != nil && event.Connection.State == "acquired" {
					close(acquired)
				}
				if event.Trial != nil {
					trial = event.Trial
				}
				if test.warmup && event.Warmup != nil {
					trial = &Trial{
						Complete: event.Warmup.Complete, Failure: event.Warmup.Failure,
						Outcomes: event.Warmup.Outcomes,
					}
				}
				return nil
			}, func(ctx context.Context, request Request, policy Plan) (transportLease, error) {
				lease, err := transport.acquire(ctx, request, policy)
				return &campaignAfterRunLease{transportLease: lease, after: func() { <-acquired; cancel() }}, err
			})
			if trial == nil || !trial.Outcomes[0].Attempted || !trial.Outcomes[0].Response.Complete ||
				trial.Outcomes[0].Failure != nil || trial.Complete != (width == 1) {
				t.Fatalf("completed membership=%+v", trial)
			}
			if width == 1 && trial.Failure != nil || width == 2 && trial.Outcomes[1].Attempted {
				t.Fatalf("completed versus unreleased membership=%+v", trial)
			}
		})
	}
}
