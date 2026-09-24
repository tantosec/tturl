package ranking

import (
	"context"
	"math/rand/v2"
)

const baselineConfirmedOutlierMethodID OutlierMethodID = "ranking/outlier/baseline-confirmed-bounded-betting/v1beta1"

// BaselineConfirmedOutlierSolver identifies an extreme item with adaptive
// discovery and clean-control commit evidence. For a known-direction request,
// discovery uses every position for candidates and Thompson sampling allocates
// comparisons. Peer-relative evidence may nominate and eliminate candidates,
// but only Baseline-relative evidence can commit a find.
//
// A discovery leader becomes eligible for confirmation after its allocation
// evidence crosses a fixed, private work gate. At most one clean confirmation
// is planned in a concurrency wave, and only after intervening discovery. The
// nominee and every layout are frozen before the wave starts. With sequential
// comparison, eligible discovery and confirmation therefore alternate; with
// concurrency, the remaining calls in a confirmation wave continue discovery.
// The gate changes work allocation, not an evidence boundary.
//
// A find's guarantee requires every non-target's transformed normalised rank
// against the selected Baseline controls to have conditional mean at most 1/2.
// A negative has a stronger premise: a qualifying target's transformed rank
// must have conditional mean at least 1/2+MinimumEdge in every scheduled
// peer-relative or Baseline-relative observation, including clean discovery
// and confirmation layouts. The bounds condition on the complete history and
// the wave's preplanned memberships, positions, and bets. Baseline availability
// alone does not establish the peer-relative bound.
//
// An [Either] request uses [BaselineReservedOutlierSolver]'s conservative
// broad-field Baseline reserve. Every discovery observation is therefore
// already Baseline-relative, so the solver does not schedule additional
// interleaved confirmations. Both directions retain separate clean commit
// evidence and the usual family-wise threshold.
//
// With a known direction, multiple targets do not contaminate the clean
// Baseline-relative evidence used for a positive commit. They can still mask
// the peer-relative evidence used for nomination and elimination, so the
// negative premise is not automatically multiple-target-safe. Either mode is
// Baseline-relative throughout and isolates both outcomes.
//
// Concurrent observations are combined exactly as for
// [BaselineReservedOutlierSolver]. Progress and result Detail values are
// [OutlierProgressDiagnostics] and [OutlierDiagnostics].
// Their positive LogEvidence values contain clean evidence only, while
// EliminationLogE combines peer and clean observations.
type BaselineConfirmedOutlierSolver struct {
	// MaxComparisons and MaxConcurrency have the meanings
	// documented by [BaselineReservedOutlierSolver].
	MaxComparisons int
	MaxConcurrency int

	// newRNG supplies deterministic randomisation in tests and experiments.
	newRNG func() *rand.Rand
}

// Inference reports anytime-valid decisions at completed atomic waves.
func (*BaselineConfirmedOutlierSolver) Inference() OutlierInferenceRegime {
	return OutlierInferenceAnytimeValid
}

// MethodID implements [IdentifiedOutlierSolver].
func (*BaselineConfirmedOutlierSolver) MethodID() OutlierMethodID {
	return baselineConfirmedOutlierMethodID
}

// Start begins an in-memory anytime-valid run.
func (f *BaselineConfirmedOutlierSolver) Start(
	req OutlierRequest, cmp *Comparator,
) (OutlierRun, error) {
	return f.delegate(req).start(req, cmp, "Start")
}

// Identify implements [OutlierSolver]. It requires at least one Baseline
// control.
func (f *BaselineConfirmedOutlierSolver) Identify(
	ctx context.Context, req OutlierRequest, cmp *Comparator,
) (OutlierResult, error) {
	return f.delegate(req).Identify(ctx, req, cmp)
}

func (f *BaselineConfirmedOutlierSolver) delegate(
	req OutlierRequest,
) *BaselineReservedOutlierSolver {
	var reserve func(int, int) int
	adaptiveConfirmation := false
	if req.Direction != Either {
		reserve = func(_, _ int) int { return 0 }
		adaptiveConfirmation = true
	}
	return &BaselineReservedOutlierSolver{
		MaxComparisons: f.MaxComparisons, MaxConcurrency: f.MaxConcurrency,
		newRNG:         f.newRNG,
		controlReserve: reserve, cleanCommitOnly: true,
		adaptiveCleanConfirmation: adaptiveConfirmation,
		operation:                 "BaselineConfirmedOutlierSolver",
	}
}
