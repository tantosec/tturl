package ranking

import (
	"context"
	"math/rand/v2"
	"testing"
)

var _ OutlierSolver = (*EdgeDirectedOutlierSolver)(nil)

func TestEdgeDirectedOutlierSolverFindsKnownSignTarget(t *testing.T) {
	const (
		population = 32
		width      = 8
		target     = 17
	)
	offset := make([]float64, population)
	offset[target] = 12
	channel := &syntheticComparator{
		n: width, bias: reversedBias(6), noise: gaussianNoise(4),
		offset: offset, controls: UnboundedControls(),
		rng: rand.New(rand.NewPCG(3, 4)), //nolint:gosec // Reproducible simulation.
	}
	solver := &EdgeDirectedOutlierSolver{
		MaxComparisons: 100_000,
		newRNG: func() *rand.Rand {
			//nolint:gosec // Reproducible simulation requires a deterministic RNG.
			return rand.New(rand.NewPCG(5, 6))
		},
	}
	result, err := solver.Identify(context.Background(), OutlierRequest{
		M: population, Direction: Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
	}, channel.comparator())
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutlierFound || result.Index != target {
		t.Fatalf("result = %v index %d, want Found index %d",
			result.Outcome, result.Index, target)
	}
	assertGuarantee(t, result.OutlierGuarantee, OutlierGuarantee{
		ErrorDirection: OutlierErrorFalseFind, ErrorRiskLimit: 0.01,
		MinimumEdge: 0.1, Direction: Late,
		Premise: OutlierPremiseScheduledReferenceMean,
		Regime:  OutlierInferenceAnytimeValid,
	})
}

func TestEdgeDirectedOutlierSolverUsesControlsForEither(t *testing.T) {
	controlsSeen := 0
	cmp := NewComparator(Profile{
		MaxWidth: 4,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, func(_ context.Context, items []ItemID) ([]int, error) {
		ranks := make([]int, len(items))
		for position, item := range items {
			ranks[position] = position
			if item.IsControl() {
				controlsSeen++
			}
		}
		return ranks, nil
	})
	solver := &EdgeDirectedOutlierSolver{MaxComparisons: 1}
	if _, err := solver.Identify(context.Background(), OutlierRequest{
		M: 4, Direction: Either, MinimumEdge: 0.1,
		FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
	}, cmp); err != nil {
		t.Fatal(err)
	}
	if controlsSeen == 0 {
		t.Error("Either comparison contained no Baseline control")
	}
}
