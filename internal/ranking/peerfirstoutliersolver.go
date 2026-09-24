package ranking

import (
	"context"
	"math/rand/v2"
)

const peerFirstOutlierMethodID OutlierMethodID = "ranking/outlier/peer-first-bounded-betting/v1beta1"

// PeerFirstOutlierSolver identifies an extreme item while preserving
// broad-field candidate capacity for known-direction requests. It compares
// candidates against peers while at least one full comparison of contenders
// remains, then fills positions freed by elimination with available Baseline
// controls and, if necessary, eliminated peers.
//
// The evidence, stopping boundaries, random positioning, and concurrency
// treatment are those documented by [BaselineReservedOutlierSolver]. Deferring
// controls is an allocation policy only. Consequently, its guarantee requires
// peer-relative observation made before controls enter to satisfy the same
// conditional-mean bounds as a control-relative observation. In particular,
// Baseline availability alone does not establish those bounds.
//
// The policy spends all broad-field positions on candidates. It is intended
// for populations where candidate discovery is the main cost and
// known-direction peer comparisons retain the required ordering premise. As
// the active set narrows below the channel width, clean controls strengthen
// confirmation and elimination. An [Either] request uses the conservative
// broad-field control allocation because opposing peer-relative explanations
// need a clean reference.
//
// Multiple targets are isolated from one another in that conservative Either
// mode. For a known direction, additional targets can mask one another during
// peer-first phase, affecting both positive and negative premises as well as
// power. Target count alone is therefore not a safety claim for this strategy.
//
// Progress and result Detail values are [OutlierProgressDiagnostics] and
// [OutlierDiagnostics].
type PeerFirstOutlierSolver struct {
	// MaxComparisons and MaxConcurrency have the meanings
	// documented by [BaselineReservedOutlierSolver].
	MaxComparisons int
	MaxConcurrency int

	// newRNG supplies deterministic randomisation in tests and experiments.
	newRNG func() *rand.Rand
}

// Inference reports anytime-valid decisions at completed atomic waves.
func (*PeerFirstOutlierSolver) Inference() OutlierInferenceRegime {
	return OutlierInferenceAnytimeValid
}

// MethodID implements [IdentifiedOutlierSolver].
func (*PeerFirstOutlierSolver) MethodID() OutlierMethodID {
	return peerFirstOutlierMethodID
}

// Start begins an in-memory anytime-valid run.
func (f *PeerFirstOutlierSolver) Start(
	req OutlierRequest, cmp *Comparator,
) (OutlierRun, error) {
	return f.delegate(req).start(req, cmp, "Start")
}

// Identify implements [OutlierSolver].
func (f *PeerFirstOutlierSolver) Identify(
	ctx context.Context, req OutlierRequest, cmp *Comparator,
) (OutlierResult, error) {
	return f.delegate(req).Identify(ctx, req, cmp)
}

func (f *PeerFirstOutlierSolver) delegate(req OutlierRequest) *BaselineReservedOutlierSolver {
	var reserve func(int, int) int
	if req.Direction != Either {
		reserve = func(_, _ int) int { return 0 }
	}
	return &BaselineReservedOutlierSolver{
		MaxComparisons: f.MaxComparisons, MaxConcurrency: f.MaxConcurrency,
		newRNG:         f.newRNG,
		controlReserve: reserve, operation: "PeerFirstOutlierSolver",
	}
}
