package timing

import (
	"context"
	"errors"
	"time"
)

type groupRun func(context.Context, <-chan struct{}, <-chan struct{},
	func(time.Time) error, func() error, func(time.Time) error) groupObservation

// groupFixture adapts controlled wire events to the production lease contract.
// It owns no scheduling, gate, timeout, priming or pacing policy.
type groupFixture struct {
	prepare         func(context.Context) error
	discard         func(error)
	waitInitial     func(context.Context) error
	lockReadiness   func()
	unlockReadiness func()
	validateInitial func() bool
	waitFinal       func(context.Context) error
	validateFinal   func() bool
	run             groupRun
}

type groupObservation struct {
	initial, final, headers, complete time.Time
	err                               error
}

type groupEvidence struct {
	startGate, finalGate time.Time
	outcomes             []groupObservation
	committed            bool
	err                  error
}

func runFixtureGroup(ctx context.Context, senders []groupFixture, pacer *startPacer,
	timeout, delay time.Duration, last bool,
) groupEvidence {
	participants := make([]campaignParticipant, len(senders))
	observations := make([]groupObservation, len(senders))
	for index, sender := range senders {
		participants[index] = campaignParticipant{lease: &groupLease{fixture: sender, observation: &observations[index]}}
	}
	anchor := time.Now()
	actual := campaignGroup(ctx, participants, pacer,
		Plan{RequestTimeout: timeout, ReleaseDelay: delay}, anchor, len(senders), true, last)
	result := groupEvidence{
		startGate: actual.startGate, finalGate: actual.finalGate,
		committed: actual.committed, err: actual.err, outcomes: make([]groupObservation, len(senders)),
	}
	for index, outcome := range actual.outcomes {
		result.outcomes[index] = observations[index]
		if outcome.Failure != nil {
			result.outcomes[index].err = outcome.Failure
		}
	}
	return result
}

type groupLease struct {
	fixture     groupFixture
	observation *groupObservation
}

func (l *groupLease) prepare(ctx context.Context, _ Request) (transportSender, error) {
	if l.fixture.prepare != nil {
		if err := l.fixture.prepare(ctx); err != nil {
			return nil, err
		}
	}
	return &groupTransportSender{fixture: l.fixture, observation: l.observation}, nil
}

func (l *groupLease) lockState() {
	if l.fixture.lockReadiness != nil {
		l.fixture.lockReadiness()
	}
}

func (l *groupLease) unlockState() {
	if l.fixture.unlockReadiness != nil {
		l.fixture.unlockReadiness()
	}
}
func (*groupLease) usableLocked() (bool, string) { return true, "" }
func (*groupLease) usable() (bool, string)       { return true, "" }
func (*groupLease) retirement() (bool, string)   { return false, "" }
func (*groupLease) metadata() transportMetadata  { return transportMetadata{} }
func (l *groupLease) dispose(err error) {
	if l.fixture.discard != nil {
		l.fixture.discard(err)
	}
}
func (*groupLease) join() {}

type groupTransportSender struct {
	fixture     groupFixture
	observation *groupObservation
}

func (s *groupTransportSender) waitReady(ctx context.Context, phase releasePhase) error {
	wait := s.fixture.waitInitial
	if phase == finalPhase {
		wait = s.fixture.waitFinal
	}
	if wait != nil {
		return wait(ctx)
	}
	return context.Cause(ctx)
}

func (s *groupTransportSender) readinessSnapshot(phase releasePhase) (bool, <-chan struct{}) {
	return s.readyLocked(phase) == nil, nil
}

func (s *groupTransportSender) readyLocked(phase releasePhase) error {
	ready := s.fixture.validateInitial
	if phase == finalPhase {
		ready = s.fixture.validateFinal
	}
	if ready != nil && !ready() {
		return errors.New("controlled wire is not ready")
	}
	return nil
}

func (s *groupTransportSender) run(ctx context.Context, gates releaseGates, hooks senderHooks) Outcome {
	observed := s.fixture.run(ctx, gates.initial, gates.final,
		hooks.initialRelease, hooks.prefixComplete, hooks.exchangeComplete)
	*s.observation = observed
	return Outcome{
		Response: Response{Complete: !observed.complete.IsZero()},
		Failure:  campaignFailure(PhaseExecution, observed.err), Timing: Timing{
			InitialRelease:       campaignOffset(gates.anchor, observed.initial, "unattempted"),
			FinalRelease:         campaignOffset(gates.anchor, observed.final, "not_released"),
			FinalResponseHeaders: campaignOffset(gates.anchor, observed.headers, "not_received"),
			ResponseComplete:     campaignOffset(gates.anchor, observed.complete, "not_received"),
		},
	}
}

func laterEvent(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
