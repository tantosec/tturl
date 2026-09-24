package ranking

import (
	"context"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func newBaselineConfirmedOutlierSolver(seed uint64) *BaselineConfirmedOutlierSolver {
	return &BaselineConfirmedOutlierSolver{
		MaxComparisons: 200,
		newRNG: func() *rand.Rand {
			//nolint:gosec // Reproducible simulation requires a deterministic RNG.
			return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		},
	}
}

func baselineConfirmationComparator(
	width, population int, targetBeatsControls bool, observe func(bool),
) *Comparator {
	return NewComparator(Profile{
		MaxWidth: width,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, func(_ context.Context, items []ItemID) ([]int, error) {
		hasControls := false
		for _, item := range items {
			hasControls = hasControls || item.IsControl()
		}
		if observe != nil {
			observe(hasControls)
		}
		scores := make([]float64, len(items))
		for position, item := range items {
			// The position term makes every total order deterministic without
			// changing the planted target's relation to peers or controls.
			scores[position] = float64(position) / float64(10*population)
			if item.IsControl() {
				scores[position] += 1
				continue
			}
			if int(item) == 0 {
				scores[position] += 4
				if hasControls && !targetBeatsControls {
					scores[position] = -4 + float64(position)/float64(10*population)
				}
			}
		}
		return ranksByScore(scores), nil
	})
}

func TestBaselineConfirmedOutlierSolverCleanConfirmationCommits(t *testing.T) {
	cmp := baselineConfirmationComparator(4, 16, true, nil)
	result, err := newBaselineConfirmedOutlierSolver(1).Identify(
		t.Context(), OutlierRequest{
			M: 16, Direction: Late, MinimumEdge: 0.01,
			FalseFindRisk: 0.1, FalseNegativeRisk: 0.000001,
		}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutlierFound || result.Index != 0 || result.Direction != Late {
		t.Fatalf("result = %v index %d sign %v, want Found index 0 Late",
			result.Outcome, result.Index, result.Direction)
	}
	assertGuarantee(t, result.OutlierGuarantee, OutlierGuarantee{
		ErrorDirection: OutlierErrorFalseFind, ErrorRiskLimit: 0.1,
		Direction: Late, Premise: OutlierPremiseBaselineMean,
		Regime: OutlierInferenceAnytimeValid,
	})
	detail, ok := result.Detail.(OutlierDiagnostics)
	if !ok {
		t.Fatalf("Detail type = %T, want OutlierDiagnostics", result.Detail)
	}
	if detail.Items[0].LogEvidence < detail.Threshold {
		t.Errorf("clean log evidence = %v, threshold = %v",
			detail.Items[0].LogEvidence, detail.Threshold)
	}
}

func TestBaselineConfirmedOutlierSolverPeerLeaderCannotCommit(t *testing.T) {
	cleanComparisons := 0
	cmp := baselineConfirmationComparator(4, 16, false, func(clean bool) {
		if clean {
			cleanComparisons++
		}
	})
	solver := newBaselineConfirmedOutlierSolver(2)
	solver.MaxComparisons = 80
	result, err := solver.Identify(t.Context(), OutlierRequest{
		M: 16, Direction: Late, MinimumEdge: 0.01,
		FalseFindRisk: 0.1, FalseNegativeRisk: 0.000001,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if cleanComparisons == 0 {
		t.Fatal("peer leader was never nominated for clean confirmation")
	}
	if result.Outcome == OutlierFound {
		t.Fatalf("peer-only leader committed as item %d", result.Index)
	}
	detail := result.Detail.(OutlierDiagnostics)
	if detail.Items[0].LogEvidence >= detail.Threshold {
		t.Errorf("clean log evidence = %v, threshold = %v",
			detail.Items[0].LogEvidence, detail.Threshold)
	}
}

func TestBaselineConfirmedOutlierSolverSequentialConfirmationAlternates(t *testing.T) {
	var layouts []bool
	cmp := baselineConfirmationComparator(4, 16, true, func(clean bool) {
		layouts = append(layouts, clean)
	})
	solver := newBaselineConfirmedOutlierSolver(3)
	solver.MaxComparisons = 30
	_, err := solver.Identify(t.Context(), OutlierRequest{
		M: 16, Direction: Late, MinimumEdge: 0.01,
		FalseFindRisk: 0.000001, FalseNegativeRisk: 0.000001,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	cleanCount := 0
	for i, clean := range layouts {
		if !clean {
			continue
		}
		cleanCount++
		if i == 0 || layouts[i-1] {
			t.Fatalf("layout %d is a clean confirmation without intervening discovery: %v",
				i, layouts)
		}
	}
	if cleanCount < 2 {
		t.Fatalf("clean confirmations = %d, want at least 2; layouts %v",
			cleanCount, layouts)
	}
}

func TestBaselineConfirmedOutlierSolverOneConfirmationPerConcurrentWave(t *testing.T) {
	const concurrency = 4
	var (
		call atomic.Int64
		mu   sync.Mutex
		seen = make([]bool, 40)
	)
	base := baselineConfirmationComparator(4, 32, true, nil)
	profile := base.Profile()
	profile.MaxConcurrency = concurrency
	cmp := NewComparator(profile, func(ctx context.Context, items []ItemID) ([]int, error) {
		ordinal := int(call.Add(1)) - 1
		clean := false
		for _, item := range items {
			clean = clean || item.IsControl()
		}
		mu.Lock()
		seen[ordinal] = clean
		mu.Unlock()
		return base.Compare(ctx, items)
	})
	solver := newBaselineConfirmedOutlierSolver(4)
	solver.MaxComparisons = len(seen)
	solver.MaxConcurrency = concurrency
	_, err := solver.Identify(t.Context(), OutlierRequest{
		M: 32, Direction: Late, MinimumEdge: 0.01,
		FalseFindRisk: 0.000001, FalseNegativeRisk: 0.000001,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	anyConfirmation := false
	for start := 0; start < len(seen); start += concurrency {
		confirmations := 0
		for _, clean := range seen[start : start+concurrency] {
			if clean {
				confirmations++
				anyConfirmation = true
			}
		}
		if confirmations > 1 {
			t.Fatalf("wave %d has %d clean confirmations, want at most 1: %v",
				start/concurrency, confirmations, seen)
		}
	}
	if !anyConfirmation {
		t.Fatal("no concurrent wave contained a clean confirmation")
	}
}

func TestBaselineConfirmedOutlierSolverBoundedConfirmationAccounting(t *testing.T) {
	base := baselineConfirmationComparator(4, 16, true, nil)
	profile := base.Profile()
	profile.Controls[Baseline] = BoundedControls(1)
	var widths []int
	cmp := NewComparator(profile,
		func(ctx context.Context, items []ItemID) ([]int, error) {
			widths = append(widths, len(items))
			return base.Compare(ctx, items)
		})
	solver := newBaselineConfirmedOutlierSolver(5)
	solver.MaxComparisons = 30
	result, err := solver.Identify(t.Context(), OutlierRequest{
		M: 16, Direction: Late, MinimumEdge: 0.01,
		FalseFindRisk: 0.000001, FalseNegativeRisk: 0.000001,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	wantParticipations := 0
	narrow := 0
	for _, width := range widths {
		wantParticipations += width
		if width == 2 {
			narrow++
		}
	}
	if narrow == 0 {
		t.Fatalf("comparison widths = %v, want a width-2 bounded confirmation", widths)
	}
	if result.Comparisons != len(widths) || result.Participations != wantParticipations {
		t.Fatalf("cost = %d comparisons/%d participations, want %d/%d from widths %v",
			result.Comparisons, result.Participations,
			len(widths), wantParticipations, widths)
	}
}

func TestBaselineConfirmedOutlierSolverEitherCommitsFromCleanDiscovery(t *testing.T) {
	cleanComparisons := 0
	cmp := baselineConfirmationComparator(4, 16, true, func(clean bool) {
		if clean {
			cleanComparisons++
		}
	})
	result, err := newBaselineConfirmedOutlierSolver(6).Identify(
		t.Context(), OutlierRequest{
			M: 16, Direction: Either, MinimumEdge: 0.01,
			FalseFindRisk: 0.1, FalseNegativeRisk: 0.000001,
		}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutlierFound {
		t.Fatalf("outcome = %v, want a clean-discovery find", result.Outcome)
	}
	assertGuarantee(t, result.OutlierGuarantee, OutlierGuarantee{
		ErrorDirection: OutlierErrorFalseFind, ErrorRiskLimit: 0.1,
		Direction: result.Direction, Premise: OutlierPremiseBaselineMean,
		Regime: OutlierInferenceAnytimeValid,
	})
	if cleanComparisons != result.Comparisons || result.Comparisons <= 1 {
		t.Fatalf("clean comparisons = %d of %d, want every comparison clean and progress past the first layout",
			cleanComparisons, result.Comparisons)
	}
}

func TestBaselineConfirmedOutlierSolverValidatesMode(t *testing.T) {
	solver := newBaselineConfirmedOutlierSolver(7)
	solver.MaxComparisons = 1
	cleanDiscovery := false
	withBaseline := baselineConfirmationComparator(4, 8, true, func(clean bool) {
		cleanDiscovery = clean
	})
	result, err := solver.Identify(t.Context(), OutlierRequest{
		M: 8, Direction: Either, MinimumEdge: 0.01,
		FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
	}, withBaseline)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutlierInconclusive {
		t.Fatalf("Either outcome = %v, want Inconclusive", result.Outcome)
	}
	if !cleanDiscovery {
		t.Fatal("Either broad-field discovery contained no Baseline control")
	}

	withoutBaseline := NewComparator(Profile{MaxWidth: 4},
		func(_ context.Context, items []ItemID) ([]int, error) {
			ranks := make([]int, len(items))
			for i := range ranks {
				ranks[i] = i
			}
			return ranks, nil
		})
	_, err = solver.Identify(t.Context(), OutlierRequest{
		M: 8, Direction: Late, MinimumEdge: 0.01,
		FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
	}, withoutBaseline)
	if err == nil || !strings.Contains(err.Error(), "clean confirmation requires a Baseline control") {
		t.Fatalf("Baseline error = %v", err)
	}
	_, err = solver.Identify(t.Context(), OutlierRequest{
		M: 8, Direction: Either, MinimumEdge: 0.01,
		FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
	}, withoutBaseline)
	if err == nil || !strings.HasPrefix(err.Error(),
		"ranking: BaselineConfirmedOutlierSolver.Identify:") {
		t.Fatalf("Either Baseline error = %v, want wrapper operation prefix", err)
	}
}

func TestSharedOutlierEngineAttributesWrapperErrors(t *testing.T) {
	cmp := baselineConfirmationComparator(4, 8, true, nil)
	for _, test := range []struct {
		name      string
		solver    OutlierSolver
		operation string
	}{
		{
			"baseline-confirmed", &BaselineConfirmedOutlierSolver{},
			"BaselineConfirmedOutlierSolver",
		},
		{
			"peer-first", &PeerFirstOutlierSolver{},
			"PeerFirstOutlierSolver",
		},
		{
			"rolling-baseline-confirmed", &RollingBaselineConfirmedOutlierSolver{},
			"RollingBaselineConfirmedOutlierSolver",
		},
		{
			"rolling-peer-first", &RollingPeerFirstOutlierSolver{},
			"RollingPeerFirstOutlierSolver",
		},
		{
			"edge-directed", &EdgeDirectedOutlierSolver{},
			"EdgeDirectedOutlierSolver",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.solver.Identify(t.Context(), OutlierRequest{
				M: 8, Direction: Late, MinimumEdge: -1,
				FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
			}, cmp)
			if err == nil || !strings.HasPrefix(err.Error(),
				"ranking: "+test.operation+".Identify:") {
				t.Fatalf("error = %v, want %s operation prefix", err, test.operation)
			}
		})
	}
}
