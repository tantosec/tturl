package ranking

import (
	"context"
	"math/rand/v2"
	"testing"
)

func newPeerFirstOutlierSolver(seed uint64) *PeerFirstOutlierSolver {
	return &PeerFirstOutlierSolver{
		MaxComparisons: 100_000,
		newRNG: func() *rand.Rand {
			//nolint:gosec // Reproducible simulation requires a deterministic RNG.
			return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		},
	}
}

func TestPeerFirstOutlierSolverDefersAvailableControls(t *testing.T) {
	const width = 4
	var observed []ItemID
	cmp := NewComparator(Profile{
		MaxWidth: width,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, func(_ context.Context, items []ItemID) ([]int, error) {
		observed = append([]ItemID(nil), items...)
		ranks := make([]int, len(items))
		for position := range ranks {
			ranks[position] = position
		}
		return ranks, nil
	})
	solver := newPeerFirstOutlierSolver(1)
	solver.MaxComparisons = 1
	result, err := solver.Identify(context.Background(), OutlierRequest{
		M: 8, Direction: Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutlierInconclusive {
		t.Fatalf("Outcome = %v, want Inconclusive", result.Outcome)
	}
	if len(observed) != width {
		t.Fatalf("comparison width = %d, want %d", len(observed), width)
	}
	for position, item := range observed {
		if item.IsControl() {
			t.Errorf("position %d contains control %v in broad field", position, item)
		}
	}
}

func TestPeerFirstOutlierSolverUsesControlsForNarrowField(t *testing.T) {
	const width = 4
	var candidates, controls int
	cmp := NewComparator(Profile{
		MaxWidth: width,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, func(_ context.Context, items []ItemID) ([]int, error) {
		ranks := make([]int, len(items))
		nextControlRank := 0
		for position, item := range items {
			if item.IsCandidate() {
				candidates++
				ranks[position] = width - 1
			} else {
				controls++
				ranks[position] = nextControlRank
				nextControlRank++
			}
		}
		return ranks, nil
	})
	solver := newPeerFirstOutlierSolver(2)
	solver.MaxComparisons = 1
	_, err := solver.Identify(context.Background(), OutlierRequest{
		M: 1, Direction: Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if candidates != 1 || controls != width-1 {
		t.Errorf("comparison contains %d candidates and %d controls, want 1 and %d",
			candidates, controls, width-1)
	}
}

func TestPeerFirstOutlierSolverKnownSignWithAndWithoutControls(t *testing.T) {
	const (
		population = 16
		width      = 4
		target     = 7
	)
	for _, sign := range []OutlierDirection{Late, Early} {
		for _, controls := range []ControlSupply{{}, UnboundedControls()} {
			name := sign.String() + "/baseline-controls"
			if controls.UpTo(1) == 0 {
				name = sign.String() + "/peer-only"
			}
			t.Run(name, func(t *testing.T) {
				offset := make([]float64, population)
				offset[target] = 12
				if sign == Early {
					offset[target] = -offset[target]
				}
				channel := &syntheticComparator{
					n: width, bias: reversedBias(6), noise: gaussianNoise(4),
					offset: offset, controls: controls,
					rng: rand.New(rand.NewPCG(3, 4)), //nolint:gosec // Reproducible test simulation.
				}
				result, err := newPeerFirstOutlierSolver(5).Identify(
					context.Background(), OutlierRequest{
						M: population, Direction: sign, MinimumEdge: 0.1,
						FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
					}, channel.comparator())
				if err != nil {
					t.Fatal(err)
				}
				if result.Outcome != OutlierFound || result.Index != target {
					t.Errorf("result = %v index %d, want Found index %d",
						result.Outcome, result.Index, target)
				}
				assertGuarantee(t, result.OutlierGuarantee, OutlierGuarantee{
					ErrorDirection: OutlierErrorFalseFind, ErrorRiskLimit: 0.01,
					Direction: sign, Premise: OutlierPremiseScheduledReferenceMean,
					Regime: OutlierInferenceAnytimeValid,
				})
			})
		}
	}
}

func TestPeerFirstOutlierSolverUsesControlsForEither(t *testing.T) {
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
	solver := newPeerFirstOutlierSolver(1)
	solver.MaxComparisons = 1
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
