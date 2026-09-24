package ranking

import (
	"context"
	"math/rand/v2"
)

const rollingBaselineReservedOutlierMethodID OutlierMethodID = "ranking/outlier/" +
	"rolling-baseline-reserved-bounded-betting/v1beta1"

// RollingBaselineReservedOutlierSolver identifies an extreme item using clean
// Baseline references and a bounded rolling dispatch window. It is intended for
// sparse outlier searches where a channel needs substantial comparison
// concurrency but atomic-wave planning would provide too few adaptive updates.
//
// Comparisons are numbered by dispatch order. The solver keeps at most
// MaxConcurrency comparisons uncommitted, commits their results in dispatch
// order, and dispatches replacements from the latest committed evidence.
// Physical completion order never selects candidates or evidence. Initial
// dispatches cover every candidate once before avoidable repeats; subsequent
// allocation uses the same Thompson sampling and positive-evidence leader pin
// as [BaselineReservedOutlierSolver].
//
// Unlike the atomic-wave solver, this method multiplies each dispatch-ordered
// comparison's e-factor. Its guarantees therefore require the documented
// Baseline conditional-mean bound to continue to hold after conditioning on
// every lower dispatch-indexed outcome, including outcomes of comparisons that
// overlapped physically and were unavailable when the comparison was planned.
// It does not permit arbitrary dependence within the rolling window. A caller
// must establish this stronger premise for the selected channel and delivery
// profile; observing a timing signal does not establish it.
//
// This first rolling implementation is one-shot. It intentionally does not
// implement [ResumableOutlierSolver]. Progress and result Detail values are
// [OutlierProgressDiagnostics] and [OutlierDiagnostics].
type RollingBaselineReservedOutlierSolver struct {
	// MaxComparisons caps comparisons dispatched by Identify. Zero selects
	// [OutlierDefaultMaxComparisons]. Exhaustion is inconclusive.
	MaxComparisons int
	// MaxConcurrency caps the number of dispatched but uncommitted comparisons.
	// Zero uses the comparator's declared ceiling, treating its zero as one.
	MaxConcurrency int

	// newRNG supplies deterministic randomisation in tests and experiments.
	newRNG func() *rand.Rand
}

// Inference reports anytime-valid decisions at dispatch-prefix updates.
func (*RollingBaselineReservedOutlierSolver) Inference() OutlierInferenceRegime {
	return OutlierInferenceAnytimeValid
}

// MethodID implements [IdentifiedOutlierSolver].
func (*RollingBaselineReservedOutlierSolver) MethodID() OutlierMethodID {
	return rollingBaselineReservedOutlierMethodID
}

// Identify implements [OutlierSolver]. It requires at least one Baseline
// control and does not expose resumable execution.
func (f *RollingBaselineReservedOutlierSolver) Identify(
	ctx context.Context, req OutlierRequest, cmp *Comparator,
) (OutlierResult, error) {
	return (&BaselineReservedOutlierSolver{
		MaxComparisons: f.MaxComparisons, MaxConcurrency: f.MaxConcurrency,
		newRNG: f.newRNG, execution: outlierExecutionRolling,
		requireBaseline: true, operation: "RollingBaselineReservedOutlierSolver",
	}).Identify(ctx, req, cmp)
}
