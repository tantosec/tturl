package ranking

import (
	"context"
	"math/rand/v2"
)

const rollingPeerFirstOutlierMethodID OutlierMethodID = "ranking/outlier/" +
	"rolling-peer-first-bounded-betting/v1beta1"

// RollingPeerFirstOutlierSolver identifies an extreme item using peer-first
// allocation and a bounded rolling dispatch window. For a known direction it
// preserves broad-field candidate capacity, adding Baseline controls only as
// elimination frees positions. Either uses the conservative Baseline reserve.
//
// Its allocation, evidence, and multiple-target considerations are those of
// [PeerFirstOutlierSolver]. Its concurrency and stopping semantics, including
// the stronger conditional-across-dispatch-indices premise, are those of
// [RollingBaselineReservedOutlierSolver]. Peer-relative appearances must obey
// that premise against their actual scheduled references; Baseline
// availability does not establish it.
//
// This implementation is deliberately one-shot and does not implement
// [ResumableOutlierSolver]. Progress and result Detail values are
// [OutlierProgressDiagnostics] and [OutlierDiagnostics].
type RollingPeerFirstOutlierSolver struct {
	// MaxComparisons and MaxConcurrency have the meanings documented by
	// [RollingBaselineReservedOutlierSolver].
	MaxComparisons int
	MaxConcurrency int

	// newRNG supplies deterministic randomisation in tests and experiments.
	newRNG func() *rand.Rand
}

// Inference reports anytime-valid decisions at dispatch-prefix updates.
func (*RollingPeerFirstOutlierSolver) Inference() OutlierInferenceRegime {
	return OutlierInferenceAnytimeValid
}

// MethodID implements [IdentifiedOutlierSolver].
func (*RollingPeerFirstOutlierSolver) MethodID() OutlierMethodID {
	return rollingPeerFirstOutlierMethodID
}

// Identify implements [OutlierSolver].
func (f *RollingPeerFirstOutlierSolver) Identify(
	ctx context.Context, req OutlierRequest, cmp *Comparator,
) (OutlierResult, error) {
	var reserve func(int, int) int
	if req.Direction != Either {
		reserve = func(_, _ int) int { return 0 }
	}
	return (&BaselineReservedOutlierSolver{
		MaxComparisons: f.MaxComparisons, MaxConcurrency: f.MaxConcurrency,
		newRNG: f.newRNG, controlReserve: reserve,
		execution: outlierExecutionRolling, operation: "RollingPeerFirstOutlierSolver",
	}).Identify(ctx, req, cmp)
}
