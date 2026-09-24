package ranking

import (
	"context"
	"math"
	"math/rand/v2"
)

const edgeDirectedOutlierMethodID OutlierMethodID = "ranking/outlier/edge-directed-bounded-betting/v1beta1"

// EdgeDirectedOutlierSolver identifies an extreme item with
// MinimumEdge-directed positive betting. For a known-direction request, it
// fills broad-field comparisons with candidates, then uses available Baseline
// references in positions freed by elimination and fills any shortfall with
// eliminated peers. This increases population coverage before concentrating
// clean-reference evidence on the survivors.
//
// Membership and bets are predictable, and positions are uniformly randomised,
// so the error guarantee follows the same conditional-mean e-process
// argument as [BaselineReservedOutlierSolver]. Before Baseline controls enter,
// callers must ensure the peer-relative normalised rank of every non-target has
// conditional mean at most 1/2 in the requested direction. MinimumEdge tunes
// both positive confirmation and the qualifying-target boundary for a negative
// result. A trustworthy negative additionally requires a qualifying target's
// transformed normalised rank to have conditional mean at least
// 1/2+MinimumEdge in every scheduled peer-relative or Baseline-relative
// observation.
//
// An [Either] request retains the edge-directed bet but uses the conservative
// broad-field control allocation: a peer-relative two-sided search can make an
// ordinary peer look exceptional at the opposite extreme of the true target.
//
// Multiple targets are isolated from one another in Either mode. With a known
// direction, same-direction targets can mask one another in peer-relative
// comparisons, while opposing targets can make an ordinary peer look extreme.
// Both outcome premises must therefore be justified against every scheduled
// membership.
//
// Progress and result Detail values are [OutlierProgressDiagnostics] and
// [OutlierDiagnostics].
type EdgeDirectedOutlierSolver struct {
	// MaxComparisons and MaxConcurrency have the meanings documented by
	// [BaselineReservedOutlierSolver]. [OutlierRequest.MinimumEdge] also targets
	// positive evidence for this solver.
	MaxComparisons int
	MaxConcurrency int

	newRNG func() *rand.Rand
}

// Inference reports anytime-valid decisions at completed atomic waves.
func (*EdgeDirectedOutlierSolver) Inference() OutlierInferenceRegime {
	return OutlierInferenceAnytimeValid
}

// MethodID implements [IdentifiedOutlierSolver].
func (*EdgeDirectedOutlierSolver) MethodID() OutlierMethodID {
	return edgeDirectedOutlierMethodID
}

// Start begins an in-memory anytime-valid run.
func (f *EdgeDirectedOutlierSolver) Start(
	req OutlierRequest, cmp *Comparator,
) (OutlierRun, error) {
	return f.delegate(req).start(req, cmp, "Start")
}

// Identify implements [OutlierSolver].
func (f *EdgeDirectedOutlierSolver) Identify(
	ctx context.Context, req OutlierRequest, cmp *Comparator,
) (OutlierResult, error) {
	return f.delegate(req).Identify(ctx, req, cmp)
}

func (f *EdgeDirectedOutlierSolver) delegate(req OutlierRequest) *BaselineReservedOutlierSolver {
	var reserve func(int, int) int
	if req.Direction != Either {
		reserve = func(int, int) int { return 0 }
	}
	return &BaselineReservedOutlierSolver{
		MaxComparisons: f.MaxComparisons, MaxConcurrency: f.MaxConcurrency,
		newRNG:         f.newRNG,
		controlReserve: reserve, boundaryPositive: true,
		operation: "EdgeDirectedOutlierSolver",
	}
}

type outlierBoundaryBet struct {
	variance  outlierRankVariance
	logWealth float64
}

func (b outlierBoundaryBet) bet(edge float64, opponents int) float64 {
	// The variance tracker supplies a stabilised estimate under the current
	// reference count. Clipping alone is load-bearing for validity.
	return min(b.variance.bet(edge, opponents), math.Nextafter(2, 0))
}
