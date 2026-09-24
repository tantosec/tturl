package ranking

import (
	"context"
	"math/rand/v2"
)

const rollingBaselineConfirmedOutlierMethodID OutlierMethodID = "ranking/outlier/" +
	"rolling-baseline-confirmed-bounded-betting/v1beta1"

// RollingBaselineConfirmedOutlierSolver identifies an extreme item using
// peer-first discovery, clean-control commit evidence, and a bounded rolling
// dispatch window. For a known direction, only Baseline-relative observations
// can commit a find. A nominee is selected from committed discovery evidence;
// at most one clean confirmation remains uncommitted, and a committed discovery
// separates consecutive confirmations. Either uses conservative Baseline
// reservation and needs no additional confirmation schedule.
//
// Its evidence and multiple-target considerations are those of
// [BaselineConfirmedOutlierSolver]. Every scheduled peer and clean appearance
// additionally requires the conditional-across-dispatch-indices premise of
// [RollingBaselineReservedOutlierSolver]. Completion order changes availability
// but never the nominee or evidence order.
//
// This implementation is deliberately one-shot and does not implement
// [ResumableOutlierSolver]. Progress and result Detail values are
// [OutlierProgressDiagnostics] and [OutlierDiagnostics].
type RollingBaselineConfirmedOutlierSolver struct {
	// MaxComparisons and MaxConcurrency have the meanings documented by
	// [RollingBaselineReservedOutlierSolver].
	MaxComparisons int
	MaxConcurrency int

	// newRNG supplies deterministic randomisation in tests and experiments.
	newRNG func() *rand.Rand
}

// Inference reports anytime-valid decisions at dispatch-prefix updates.
func (*RollingBaselineConfirmedOutlierSolver) Inference() OutlierInferenceRegime {
	return OutlierInferenceAnytimeValid
}

// MethodID implements [IdentifiedOutlierSolver].
func (*RollingBaselineConfirmedOutlierSolver) MethodID() OutlierMethodID {
	return rollingBaselineConfirmedOutlierMethodID
}

// Identify implements [OutlierSolver]. It requires a Baseline control.
func (f *RollingBaselineConfirmedOutlierSolver) Identify(
	ctx context.Context, req OutlierRequest, cmp *Comparator,
) (OutlierResult, error) {
	var reserve func(int, int) int
	adaptiveConfirmation := false
	if req.Direction != Either {
		reserve = func(_, _ int) int { return 0 }
		adaptiveConfirmation = true
	}
	return (&BaselineReservedOutlierSolver{
		MaxComparisons: f.MaxComparisons, MaxConcurrency: f.MaxConcurrency,
		newRNG: f.newRNG, controlReserve: reserve, cleanCommitOnly: true,
		adaptiveCleanConfirmation: adaptiveConfirmation,
		execution:                 outlierExecutionRolling, requireBaseline: true,
		operation: "RollingBaselineConfirmedOutlierSolver",
	}).Identify(ctx, req, cmp)
}
