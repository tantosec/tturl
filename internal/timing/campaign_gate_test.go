package timing

// cspell:ignore synctest
import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func awaitGate(ctx context.Context, gate <-chan struct{}) error {
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func groupFakeSender(prefix <-chan struct{}, events chan<- string, early bool) groupFixture {
	return groupFixture{run: func(ctx context.Context, start, finish <-chan struct{}, initial func(time.Time) error,
		completed func() error, exchange func(time.Time) error,
	) groupObservation {
		var outcome groupObservation
		if outcome.err = awaitGate(ctx, start); outcome.err != nil {
			return outcome
		}
		now := time.Now()
		if outcome.err = initial(now); outcome.err != nil {
			return outcome
		}
		outcome.initial = now
		events <- "initial"
		if prefix != nil {
			if outcome.err = awaitGate(ctx, prefix); outcome.err != nil {
				return outcome
			}
		}
		if outcome.err = completed(); outcome.err != nil {
			return outcome
		}
		events <- "prefix"
		if early {
			outcome.headers = time.Now()
			events <- "early"
		}
		if outcome.err = awaitGate(ctx, finish); outcome.err != nil {
			return outcome
		}
		outcome.final = time.Now()
		events <- "final"
		if !early {
			outcome.headers = time.Now()
		}
		outcome.complete = time.Now()
		outcome.err = exchange(outcome.complete)
		return outcome
	}}
}

func groupReceive(t *testing.T, events <-chan string) string {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(time.Second):
		t.Fatal("missing barrier event")
		return ""
	}
}

func TestCampaignFinalGateReadinessAndEarlyMembership(t *testing.T) {
	firstPrefix := make(chan struct{})
	finalCredit := make(chan struct{})
	events := make(chan string, 20)
	first := groupFakeSender(firstPrefix, events, true)
	second := groupFakeSender(nil, events, false)
	finalWaiting := make(chan struct{}, 2)
	var finalChecks atomic.Int32
	second.validateFinal = func() bool { return finalChecks.Add(1) > 1 }
	second.waitFinal = func(ctx context.Context) error { finalWaiting <- struct{}{}; return awaitGate(ctx, finalCredit) }
	delay := 10 * time.Millisecond
	done := make(chan groupEvidence, 1)
	go func() {
		done <- runFixtureGroup(context.Background(), []groupFixture{first, second},
			newStartPacer(0, 0), time.Second, delay, true)
	}()
	seenInitial, seenPrefix := 0, 0
	for seenInitial < 2 || seenPrefix < 1 {
		switch groupReceive(t, events) {
		case "initial":
			seenInitial++
		case "prefix":
			seenPrefix++
		default:
			t.Fatal("final gate opened before all prefixes")
		}
	}
	select {
	case <-finalWaiting:
		t.Fatal("final readiness started before all prefixes")
	default:
	}
	close(firstPrefix)
	for groupReceive(t, events) != "early" {
	}
	<-finalWaiting
	readyAt := time.Now()
	close(finalCredit)
	result := <-done
	if result.err != nil {
		t.Fatal(result.err)
	}
	if finalChecks.Load() != 2 {
		t.Fatal("final readiness was not checked again after delay")
	}
	if result.finalGate.Sub(readyAt) < delay {
		t.Fatal("release delay overlapped final readiness")
	}
	for _, outcome := range result.outcomes {
		if outcome.final.Before(result.finalGate) {
			t.Fatal("sender passed closed final gate")
		}
	}
	// The early event precedes final readiness and credit above. Equal clock
	// samples preserve that causal order on clocks with coarse resolution.
	if result.outcomes[0].headers.IsZero() || result.outcomes[0].final.IsZero() ||
		result.outcomes[0].headers.After(result.outcomes[0].final) {
		t.Fatal("legal early sender withdrew from final gate")
	}
}

func TestCampaignReadinessAfterCommitmentAndExecutionTransfer(t *testing.T) {
	events := make(chan string, 20)
	blocked := make(chan struct{})
	first := groupFakeSender(nil, events, false)
	second := groupFakeSender(nil, events, false)
	normal := second.run
	second.run = func(ctx context.Context, start, finish <-chan struct{}, initial func(time.Time) error,
		prefix func() error, exchange func(time.Time) error,
	) groupObservation {
		if err := awaitGate(ctx, start); err != nil {
			return groupObservation{err: err}
		}
		events <- "wire blocked"
		if err := awaitGate(ctx, blocked); err != nil {
			return groupObservation{err: err}
		}
		return normal(ctx, start, finish, initial, prefix, exchange)
	}
	pacer := newStartPacer(0, 0)
	result := runFixtureGroup(context.Background(), []groupFixture{first, second}, pacer, 20*time.Millisecond, 0, true)
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("readiness failure: %v", result.err)
	}
	if !result.committed || pacer.charges != 2 {
		t.Fatal("committed blocked wire lost charge")
	}
	if result.outcomes[0].initial.IsZero() || !result.outcomes[1].initial.IsZero() {
		t.Fatal("release transfer fabricated a write")
	}
	if result.outcomes[0].final.IsZero() == false {
		t.Fatal("successful subset passed final gate")
	}
}

func TestCampaignExecutionBudgetStartsAtEachInitialRelease(t *testing.T) {
	events := make(chan string, 20)
	sender := groupFakeSender(nil, events, false)
	ready := make(chan struct{})
	waiting := make(chan struct{})
	sender.waitInitial = func(ctx context.Context) error { close(waiting); return awaitGate(ctx, ready) }
	normal := sender.run
	sender.run = func(ctx context.Context, start, finish <-chan struct{}, initial func(time.Time) error,
		prefix func() error, exchange func(time.Time) error,
	) groupObservation {
		outcome := normal(ctx, start, finish, initial, prefix, func(time.Time) error { return nil })
		if outcome.err != nil {
			return outcome
		}
		outcome.complete = time.Time{}
		events <- "headers"
		<-ctx.Done()
		outcome.err = context.Cause(ctx)
		return outcome
	}
	timeout := 100 * time.Millisecond
	done := make(chan groupEvidence, 1)
	go func() {
		done <- runFixtureGroup(context.Background(), []groupFixture{sender}, newStartPacer(0, 0), timeout, 0, false)
	}()
	<-waiting
	time.Sleep(60 * time.Millisecond)
	close(ready)
	for groupReceive(t, events) != "headers" {
	}
	result := <-done
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatal(result.err)
	}
	if elapsed := time.Since(result.outcomes[0].initial); elapsed < timeout {
		t.Fatal("readiness time shortened execution budget")
	}
	if result.outcomes[0].headers.IsZero() {
		t.Fatal("body failure discarded valid headers")
	}
}

func TestCampaignTwoWorkersInvalidateAfterPacingWait(t *testing.T) {
	pacer := newStartPacer(20, 0)
	firstBudget := newPhaseBudget(context.Background(), time.Second)
	defer firstBudget.close()
	committed, err := pacer.commit(context.Background(), 1, firstBudget, func(commit func()) bool {
		commit()
		return true
	}, nil)
	if !committed || err != nil {
		t.Fatal(err)
	}
	invalidBudget := newPhaseBudget(context.Background(), time.Second)
	validBudget := newPhaseBudget(context.Background(), time.Second)
	defer invalidBudget.close()
	defer validBudget.close()
	var staleCalls atomic.Int32
	var staleReady atomic.Bool
	staleReady.Store(true)
	staleDone := make(chan bool, 1)
	validDone := make(chan bool, 1)
	before := time.Now()
	go func() {
		ok, err := pacer.commit(context.Background(), 3, invalidBudget, func(commit func()) bool {
			staleCalls.Add(1)
			if staleReady.Load() {
				commit()
				return true
			}
			return false
		}, nil)
		if err != nil {
			t.Error(err)
		}
		staleDone <- ok
	}()
	go func() {
		ok, err := pacer.commit(context.Background(), 2, validBudget, func(commit func()) bool { commit(); return true }, nil)
		if err != nil {
			t.Error(err)
		}
		validDone <- ok
	}()
	for {
		invalidBudget.mu.Lock()
		paused := invalidBudget.paused
		invalidBudget.mu.Unlock()
		if paused {
			break
		}
		if time.Since(before) > time.Second {
			t.Fatal("worker never entered deliberate pacing wait")
		}
		runtime.Gosched()
	}
	staleReady.Store(false)
	if <-staleDone {
		t.Fatal("stale worker banked permission")
	}
	if !<-validDone {
		t.Fatal("ready worker failed commitment")
	}
	if time.Since(before) < pacer.interval(1) {
		t.Fatal("revalidation never followed a real pacing wait")
	}
	if staleCalls.Load() != 1 || pacer.charges != 3 {
		t.Fatal("stale readiness duplicated committed cost")
	}
}

func TestCampaignCancellationBeforeEitherGate(t *testing.T) {
	for _, final := range []bool{false, true} {
		name := "initial"
		if final {
			name = "final"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			events := make(chan string, 20)
			waiting := make(chan struct{})
			blocked := make(chan struct{})
			first := groupFakeSender(nil, events, false)
			wait := func(ctx context.Context) error { close(waiting); return awaitGate(ctx, blocked) }
			if final {
				first.waitFinal = wait
			} else {
				first.waitInitial = wait
			}
			pacer := newStartPacer(0, 0)
			done := make(chan groupEvidence, 1)
			go func() {
				done <- runFixtureGroup(ctx, []groupFixture{first, groupFakeSender(nil, events, false)}, pacer, 0, 0, true)
			}()
			<-waiting
			cancel()
			select {
			case result := <-done:
				if !errors.Is(result.err, context.Canceled) {
					t.Fatal(result.err)
				}
				if !result.finalGate.IsZero() {
					t.Fatal("cancelled gate opened")
				}
				expected := int64(0)
				if final {
					expected = 2
				}
				if pacer.charges != expected {
					t.Fatal("cancelled trial charge disagrees with commitment")
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled gate waiter stranded")
			}
		})
	}
}

func TestCampaignCompletedSenderCannotExhaustLaterExecutionBudget(t *testing.T) {
	events := make(chan string, 20)
	first := groupFakeSender(nil, events, false)
	second := groupFakeSender(nil, events, false)
	normal := second.run
	timeout := 100 * time.Millisecond
	second.run = func(ctx context.Context, start, finish <-chan struct{}, initial func(time.Time) error,
		prefix func() error, exchange func(time.Time) error,
	) groupObservation {
		if err := awaitGate(ctx, start); err != nil {
			return groupObservation{err: err}
		}
		timer := time.NewTimer(60 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return groupObservation{err: context.Cause(ctx)}
		}
		outcome := normal(ctx, start, finish, initial, prefix, func(time.Time) error { return nil })
		if outcome.err != nil {
			return outcome
		}
		outcome.complete = time.Time{}
		<-ctx.Done()
		outcome.err = context.Cause(ctx)
		return outcome
	}
	result := runFixtureGroup(t.Context(), []groupFixture{first, second}, newStartPacer(0, 0), timeout, 0, false)
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatal(result.err)
	}
	if result.outcomes[0].err != nil || result.outcomes[0].complete.IsZero() {
		t.Fatal("completed request timed out")
	}
	if result.outcomes[1].initial.IsZero() || time.Since(result.outcomes[1].initial) < timeout {
		t.Fatal("later sender inherited another execution clock")
	}
}

func TestCampaignPacingWaitLongerThanReadinessBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pacer := newStartPacer(10, 0)
		first := newPhaseBudget(t.Context(), 0)
		defer first.close()
		committed, err := pacer.commit(t.Context(), 1, first, func(commit func()) bool { commit(); return true }, nil)
		if err != nil || !committed {
			t.Fatalf("initial commitment: committed=%t err=%v", committed, err)
		}
		budget := newPhaseBudget(t.Context(), 20*time.Millisecond)
		defer budget.close()
		time.Sleep(5 * time.Millisecond)
		began := time.Now()
		committed, err = pacer.commit(t.Context(), 1, budget, func(commit func()) bool { commit(); return true }, nil)
		if err != nil || !committed {
			t.Fatalf("deliberate pacing exhausted readiness: %v", err)
		}
		if elapsed := time.Since(began); elapsed != 95*time.Millisecond {
			t.Fatalf("pacing pause = %s, want 95ms", elapsed)
		}
		budget.mu.Lock()
		remaining := budget.remaining
		budget.mu.Unlock()
		if remaining != 15*time.Millisecond {
			t.Fatalf("remaining readiness = %s, want 15ms", remaining)
		}
		time.Sleep(14 * time.Millisecond)
		synctest.Wait()
		if err := context.Cause(budget.ctx); err != nil {
			t.Fatalf("readiness expired before remaining allowance: %v", err)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if err := context.Cause(budget.ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("readiness did not expire at remaining allowance: %v", err)
		}
		if pacer.charges != 2 {
			t.Fatalf("pacing charge = %d, want 2", pacer.charges)
		}
	})
}

func TestCampaignCompletedExchangeSurvivesDelayedConsumer(t *testing.T) {
	events := make(chan string, 20)
	sender := groupFakeSender(nil, events, false)
	normal := sender.run
	consumerReady := make(chan struct{})
	allowConsumer := make(chan struct{})
	sender.run = func(ctx context.Context, start, finish <-chan struct{}, initial func(time.Time) error,
		prefix func() error, exchange func(time.Time) error,
	) groupObservation {
		outcome := normal(ctx, start, finish, initial, prefix, exchange)
		close(consumerReady)
		if outcome.err == nil {
			outcome.err = awaitGate(ctx, allowConsumer)
		}
		return outcome
	}
	timeout := 20 * time.Millisecond
	done := make(chan groupEvidence, 1)
	go func() {
		done <- runFixtureGroup(t.Context(), []groupFixture{sender}, newStartPacer(0, 0), timeout, 0, false)
	}()
	<-consumerReady
	timer := time.NewTimer(2 * timeout)
	<-timer.C
	close(allowConsumer)
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("completed exchange expired during consumer wait: %v", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("completed consumer stranded")
	}
}

func TestCampaignCompletionAtDeadlineFailsBeforeRuntimeExpiry(t *testing.T) {
	events := make(chan string, 20)
	sender := groupFakeSender(nil, events, false)
	normal := sender.run
	timeout := time.Hour
	sender.run = func(ctx context.Context, start, finish <-chan struct{}, initial func(time.Time) error,
		prefix func() error, exchange func(time.Time) error,
	) groupObservation {
		observed := normal(ctx, start, finish, initial, prefix, func(time.Time) error { return nil })
		if observed.err == nil {
			// Controlled transport events exercise the exact deadline boundary while
			// the runtime timer remains far from expiry. Event time owns classification.
			observed.complete = observed.initial.Add(timeout)
			observed.err = exchange(observed.complete)
		}
		return observed
	}
	result := runFixtureGroup(t.Context(), []groupFixture{sender}, newStartPacer(0, 0), timeout, 0, false)
	if !errors.Is(result.err, context.DeadlineExceeded) ||
		!errors.Is(result.outcomes[0].err, context.DeadlineExceeded) || result.outcomes[0].headers.IsZero() {
		t.Fatalf("late completion=%+v", result)
	}
}
