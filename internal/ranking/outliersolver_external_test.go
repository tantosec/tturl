package ranking_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/tantosec/tturl/internal/ranking"
)

// fixedHorizonOutlierSketch demonstrates the third-party family seam with a
// one-sided fixed-horizon Hoeffding test. It compares its p-value directly with
// FalseFindRisk and makes a decision only after its precommitted horizon;
// progress is never an early decision boundary.
type fixedHorizonOutlierSketch struct {
	Horizon int
}

// burstOutlierSketch deliberately over-launches comparisons to exercise the
// comparator's safety boundary. It is not an example of valid evidence
// scheduling and therefore returns only an inconclusive result.
type burstOutlierSketch struct {
	Calls int
}

// descriptiveOutlierSketch can nominate an item but deliberately makes no
// controlled-error claim.
type descriptiveOutlierSketch struct{}

var _ ranking.OutlierSolver = descriptiveOutlierSketch{}

func (descriptiveOutlierSketch) Inference() ranking.OutlierInferenceRegime {
	return ranking.OutlierInferenceUncontrolled
}

func (descriptiveOutlierSketch) Identify(
	context.Context, ranking.OutlierRequest, *ranking.Comparator,
) (ranking.OutlierResult, error) {
	return ranking.OutlierResult{
		Outcome: ranking.OutlierFound, Index: 0,
		Items: []ranking.ItemStat{{Samples: 0, MeanRank: math.NaN()}},
	}, nil
}

var _ ranking.OutlierSolver = burstOutlierSketch{}

func (burstOutlierSketch) Inference() ranking.OutlierInferenceRegime {
	return ranking.OutlierInferenceUncontrolled
}

func (s burstOutlierSketch) Identify(
	ctx context.Context, _ ranking.OutlierRequest, cmp *ranking.Comparator,
) (ranking.OutlierResult, error) {
	errs := make(chan error, s.Calls)
	var wg sync.WaitGroup
	for range s.Calls {
		wg.Go(func() {
			_, err := cmp.Compare(ctx, []ranking.ItemID{0, 1})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return ranking.OutlierResult{}, err
		}
	}
	return ranking.OutlierResult{
		Outcome: ranking.OutlierInconclusive,
		Index:   -1,
		Cost: ranking.Cost{
			Comparisons: s.Calls, Participations: 2 * s.Calls,
		},
	}, nil
}

var _ ranking.OutlierSolver = fixedHorizonOutlierSketch{}

func (fixedHorizonOutlierSketch) Inference() ranking.OutlierInferenceRegime {
	return ranking.OutlierInferenceFixedDesign
}

func (s fixedHorizonOutlierSketch) Identify(
	ctx context.Context, req ranking.OutlierRequest, cmp *ranking.Comparator,
) (ranking.OutlierResult, error) {
	if req.M != 1 || req.Direction != ranking.Late || s.Horizon < 1 ||
		!(req.FalseFindRisk > 0 && req.FalseFindRisk < 1) ||
		!(req.FalseNegativeRisk > 0 && req.FalseNegativeRisk < 1) {
		return ranking.OutlierResult{}, errors.New(
			"fixed sketch requires M=1, Late, valid risks, and a positive horizon")
	}
	const width = 2
	late := 0
	rankSum := 0
	for range s.Horizon {
		ranks, err := cmp.Compare(ctx, []ranking.ItemID{
			0, ranking.Control(ranking.Baseline, 0),
		})
		if err != nil {
			return ranking.OutlierResult{}, err
		}
		rankSum += ranks[0]
		if ranks[0] == 1 {
			late++
		}
	}
	mean := float64(late) / float64(s.Horizon)
	pValue := 1.0
	if mean > 0.5 {
		edge := mean - 0.5
		pValue = math.Exp(-2 * float64(s.Horizon) * edge * edge)
	}
	result := ranking.OutlierResult{
		Outcome: ranking.OutlierInconclusive,
		Index:   -1,
		Cost: ranking.Cost{
			Comparisons: s.Horizon, Participations: width * s.Horizon,
		},
		Items: []ranking.ItemStat{{
			Samples: s.Horizon, MeanRank: float64(rankSum) / float64(s.Horizon),
		}},
	}
	if pValue <= req.FalseFindRisk {
		result.Outcome = ranking.OutlierFound
		result.Index = 0
		result.OutlierGuarantee = ranking.OutlierGuarantee{
			ErrorDirection: ranking.OutlierErrorFalseFind,
			ErrorRiskLimit: req.FalseFindRisk, Direction: ranking.Late,
			Premise: ranking.OutlierPremiseBaselineMean,
			Regime:  ranking.OutlierInferenceFixedDesign,
		}
	}
	if req.Progress != nil {
		req.Progress(ranking.OutlierProgress{
			ProgressCore: ranking.ProgressCore{
				Cost: ranking.Cost{
					Comparisons:    result.Comparisons,
					Participations: result.Participations,
				},
			},
			Active: 1, Leader: 0, Direction: ranking.Late,
		})
	}
	return result, nil
}

func TestExternalFixedHorizonOutlierSolver(t *testing.T) {
	const horizon = 7
	cmp := ranking.NewComparator(ranking.Profile{
		MaxWidth: 2,
		Controls: map[ranking.ControlKind]ranking.ControlSupply{
			ranking.Baseline: ranking.UnboundedControls(),
		},
	}, func(_ context.Context, _ []ranking.ItemID) ([]int, error) {
		return []int{1, 0}, nil
	})
	progressCalls := 0
	result, err := (fixedHorizonOutlierSketch{Horizon: horizon}).Identify(
		t.Context(), ranking.OutlierRequest{
			M: 1, Direction: ranking.Late,
			FalseFindRisk: 0.05, FalseNegativeRisk: 0.05,
			Progress: func(ranking.OutlierProgress) { progressCalls++ },
		}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != ranking.OutlierFound || result.Index != 0 ||
		result.Regime != ranking.OutlierInferenceFixedDesign ||
		result.ErrorDirection != ranking.OutlierErrorFalseFind ||
		result.Premise != ranking.OutlierPremiseBaselineMean ||
		result.ErrorRiskLimit != 0.05 ||
		result.Detail != nil || result.Comparisons != horizon ||
		result.Participations != 2*horizon || progressCalls != 1 {
		t.Fatalf("fixed-horizon result = %+v, progress calls = %d",
			result, progressCalls)
	}
	if got := (fixedHorizonOutlierSketch{}).Inference(); got != ranking.OutlierInferenceFixedDesign {
		t.Fatalf("Inference = %v, want fixed-design", got)
	}
}

func TestExternalDescriptiveSolverHasNoGuarantee(t *testing.T) {
	solver := descriptiveOutlierSketch{}
	result, err := solver.Identify(t.Context(), ranking.OutlierRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if solver.Inference() != ranking.OutlierInferenceUncontrolled ||
		result.Outcome != ranking.OutlierFound ||
		result.OutlierGuarantee != (ranking.OutlierGuarantee{}) {
		t.Fatalf("descriptive result = %+v, inference = %v",
			result, solver.Inference())
	}
}

func TestComparatorCapsExternalOutlierSolver(t *testing.T) {
	const ceiling, calls = 2, 8
	started := make(chan struct{}, calls)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	active, peak, delegated := 0, 0, 0
	cmp := ranking.NewComparator(ranking.Profile{
		MaxWidth: 2, MaxConcurrency: ceiling,
	}, func(ctx context.Context, _ []ranking.ItemID) ([]int, error) {
		mu.Lock()
		active++
		delegated++
		peak = max(peak, active)
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})

	result := make(chan error)
	go func() {
		_, err := (burstOutlierSketch{Calls: calls}).Identify(
			ctx, ranking.OutlierRequest{}, cmp)
		result <- err
	}()
	for range ceiling {
		<-started
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("external solver error = %v, want context.Canceled", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if delegated != ceiling {
		t.Errorf("CompareFunc called %d times, want %d", delegated, ceiling)
	}
	if peak != ceiling {
		t.Errorf("peak CompareFunc calls = %d, want %d", peak, ceiling)
	}
	if active != 0 {
		t.Errorf("active CompareFunc calls = %d after completion, want 0", active)
	}
}
