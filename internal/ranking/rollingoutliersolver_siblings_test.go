package ranking

import (
	"context"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"
)

var (
	_ OutlierSolver           = (*RollingPeerFirstOutlierSolver)(nil)
	_ IdentifiedOutlierSolver = (*RollingPeerFirstOutlierSolver)(nil)
	_ OutlierSolver           = (*RollingBaselineConfirmedOutlierSolver)(nil)
	_ IdentifiedOutlierSolver = (*RollingBaselineConfirmedOutlierSolver)(nil)
)

func rollingSiblingRNG(seed uint64) func() *rand.Rand {
	return func() *rand.Rand {
		//nolint:gosec // Reproducible tests require deterministic randomness.
		return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	}
}

func TestRollingSiblingSolversAreDeliberatelyOneShot(t *testing.T) {
	for name, solver := range map[string]OutlierSolver{
		"peer-first":         &RollingPeerFirstOutlierSolver{},
		"baseline-confirmed": &RollingBaselineConfirmedOutlierSolver{},
	} {
		if _, ok := solver.(ResumableOutlierSolver); ok {
			t.Errorf("%s unexpectedly implements ResumableOutlierSolver", name)
		}
	}
}

func TestRollingPeerFirstDefersAvailableControls(t *testing.T) {
	const width = 4
	var observed []ItemID
	cmp := NewComparator(Profile{
		MaxWidth: width, MaxConcurrency: 4,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, func(_ context.Context, items []ItemID) ([]int, error) {
		observed = append([]ItemID(nil), items...)
		ranks := make([]int, len(items))
		for i := range ranks {
			ranks[i] = i
		}
		return ranks, nil
	})
	result, err := (&RollingPeerFirstOutlierSolver{
		MaxComparisons: 1, MaxConcurrency: 4, newRNG: rollingSiblingRNG(1),
	}).Identify(t.Context(), OutlierRequest{
		M: 8, Direction: Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutlierInconclusive {
		t.Fatalf("Outcome = %v, want Inconclusive", result.Outcome)
	}
	for position, item := range observed {
		if item.IsControl() {
			t.Errorf("position %d contains control %v in broad field", position, item)
		}
	}
}

func TestRollingPeerFirstKnownDirectionWithoutControls(t *testing.T) {
	const (
		population = 16
		target     = 7
	)
	offset := make([]float64, population)
	offset[target] = 12
	channel := &syntheticComparator{
		n: 4, bias: reversedBias(6), noise: gaussianNoise(4), offset: offset,
		rng: rand.New(rand.NewPCG(3, 4)), //nolint:gosec // Reproducible test simulation.
	}
	result, err := (&RollingPeerFirstOutlierSolver{
		MaxComparisons: 100_000, MaxConcurrency: 8,
		newRNG: rollingSiblingRNG(5),
	}).Identify(t.Context(), OutlierRequest{
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
		Direction: Late, Premise: OutlierPremiseScheduledReferenceMean,
		Regime: OutlierInferenceAnytimeValid,
	})
}

func TestRollingBaselineConfirmedUsesCleanCommitEvidence(t *testing.T) {
	for _, test := range []struct {
		name                string
		targetBeatsControls bool
		wantFound           bool
	}{
		{"clean target", true, true},
		{"peer-only leader", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			cleanComparisons := 0
			cmp := baselineConfirmationComparator(4, 16,
				test.targetBeatsControls, func(clean bool) {
					if clean {
						cleanComparisons++
					}
				})
			result, err := (&RollingBaselineConfirmedOutlierSolver{
				MaxComparisons: 300, MaxConcurrency: 4,
				newRNG: rollingSiblingRNG(7),
			}).Identify(t.Context(), OutlierRequest{
				M: 16, Direction: Late, MinimumEdge: 0.01,
				FalseFindRisk: 0.1, FalseNegativeRisk: 0.000001,
			}, cmp)
			if err != nil {
				t.Fatal(err)
			}
			if cleanComparisons == 0 {
				t.Fatal("peer leader was never confirmed against Baselines")
			}
			if got := result.Outcome == OutlierFound; got != test.wantFound {
				t.Fatalf("result = %+v, found = %t, want %t",
					result, got, test.wantFound)
			}
			if test.wantFound {
				assertGuarantee(t, result.OutlierGuarantee, OutlierGuarantee{
					ErrorDirection: OutlierErrorFalseFind, ErrorRiskLimit: 0.1,
					Direction: Late, Premise: OutlierPremiseBaselineMean,
					Regime: OutlierInferenceAnytimeValid,
				})
			}
		})
	}
}

func TestRollingBaselineConfirmedKeepsOneCleanConfirmationPending(t *testing.T) {
	var active, maximum, total atomic.Int64
	base := baselineConfirmationComparator(4, 32, true, nil)
	profile := base.Profile()
	profile.MaxConcurrency = 8
	cmp := NewComparator(profile, func(ctx context.Context, items []ItemID) ([]int, error) {
		candidates := 0
		for _, item := range items {
			if item.IsCandidate() {
				candidates++
			}
		}
		confirmation := candidates == 1
		if confirmation {
			now := active.Add(1)
			for old := maximum.Load(); now > old && !maximum.CompareAndSwap(old, now); {
				old = maximum.Load()
			}
			total.Add(1)
			defer active.Add(-1)
			time.Sleep(100 * time.Microsecond)
		}
		return base.Compare(ctx, items)
	})
	_, err := (&RollingBaselineConfirmedOutlierSolver{
		MaxComparisons: 100, MaxConcurrency: 8, newRNG: rollingSiblingRNG(11),
	}).Identify(t.Context(), OutlierRequest{
		M: 32, Direction: Late, MinimumEdge: 0.01,
		FalseFindRisk: 0.000001, FalseNegativeRisk: 0.000001,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if total.Load() < 2 {
		t.Fatalf("clean confirmations = %d, want at least two", total.Load())
	}
	if maximum.Load() != 1 {
		t.Fatalf("concurrent clean confirmations = %d, want one", maximum.Load())
	}
}
