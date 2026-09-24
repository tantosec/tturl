package ranking_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/tantosec/tturl/internal/ranking"
)

func TestOutlierSolverFleetExternalContract(t *testing.T) {
	cmp := ranking.NewComparator(ranking.Profile{
		MaxWidth: 2,
		Controls: map[ranking.ControlKind]ranking.ControlSupply{
			ranking.Baseline: ranking.UnboundedControls(),
		},
	}, func(_ context.Context, items []ranking.ItemID) ([]int, error) {
		ranks := make([]int, len(items))
		for position, item := range items {
			if item.IsCandidate() {
				ranks[position] = 1
			}
		}
		return ranks, nil
	})
	solvers := []struct {
		name    string
		solver  ranking.OutlierSolver
		edge    float64
		premise ranking.OutlierEvidencePremise
	}{
		{
			"baseline-reserved", &ranking.BaselineReservedOutlierSolver{
				MaxComparisons: 1_000, MaxConcurrency: 1,
			}, 0,
			ranking.OutlierPremiseBaselineMean,
		},
		{
			"peer-first", &ranking.PeerFirstOutlierSolver{
				MaxComparisons: 1_000, MaxConcurrency: 1,
			}, 0,
			ranking.OutlierPremiseScheduledReferenceMean,
		},
		{
			"baseline-confirmed", &ranking.BaselineConfirmedOutlierSolver{
				MaxComparisons: 1_000, MaxConcurrency: 1,
			}, 0,
			ranking.OutlierPremiseBaselineMean,
		},
		{
			"edge-directed", &ranking.EdgeDirectedOutlierSolver{
				MaxComparisons: 1_000, MaxConcurrency: 1,
			}, 0.1,
			ranking.OutlierPremiseScheduledReferenceMean,
		},
	}
	for _, test := range solvers {
		t.Run(test.name, func(t *testing.T) {
			result, err := test.solver.Identify(t.Context(), ranking.OutlierRequest{
				M: 1, Direction: ranking.Late, MinimumEdge: 0.1,
				FalseFindRisk: 0.1, FalseNegativeRisk: 0.1,
			}, cmp)
			if err != nil {
				t.Fatal(err)
			}
			if result.Outcome != ranking.OutlierFound || result.Index != 0 ||
				result.ErrorDirection != ranking.OutlierErrorFalseFind ||
				result.MinimumEdge != test.edge || result.Direction != ranking.Late ||
				result.Premise != test.premise ||
				result.Comparisons <= 0 || result.Participations != 2*result.Comparisons {
				t.Errorf("result = %+v", result)
			}
			if _, ok := result.Detail.(ranking.OutlierDiagnostics); !ok {
				t.Errorf("Detail type = %T, want ranking.OutlierDiagnostics", result.Detail)
			}
		})
	}
}

func ExampleBaselineReservedOutlierSolver() {
	// This example uses a perfectly separated channel: it always ranks the
	// candidate after its baseline control. A real channel would compare the
	// arranged items and return their ranks.
	cmp := ranking.NewComparator(ranking.Profile{
		MaxWidth: 2,
		Controls: map[ranking.ControlKind]ranking.ControlSupply{
			ranking.Baseline: ranking.UnboundedControls(),
		},
	}, func(_ context.Context, items []ranking.ItemID) ([]int, error) {
		ranks := make([]int, len(items))
		for position, item := range items {
			if item.IsCandidate() {
				ranks[position] = 1
			}
		}
		return ranks, nil
	})

	// MinimumEdge defines which targets a negative result excludes. Reducing it
	// can make negatives quadratically more expensive. MaxComparisons provides a
	// hard work bound; exhausting it returns OutlierInconclusive.
	solver := ranking.BaselineReservedOutlierSolver{MaxComparisons: 1_000}
	result, err := solver.Identify(context.Background(), ranking.OutlierRequest{
		M:                 1,
		Direction:         ranking.Late,
		MinimumEdge:       0.1,
		FalseFindRisk:     0.05,
		FalseNegativeRisk: 0.05,
	}, cmp)
	if err != nil {
		panic(err)
	}

	switch result.Outcome {
	case ranking.OutlierFound:
		fmt.Printf("found item %d at the %s extreme under the %s premise\n",
			result.Index, result.Direction, result.Premise)
	case ranking.OutlierNegative:
		fmt.Printf("no %s target met edge %.2f under the %s premise\n",
			result.Direction, result.MinimumEdge, result.Premise)
	case ranking.OutlierInconclusive:
		fmt.Println("comparison budget exhausted")
	}
	// These counters are transport-independent cost measures. Concurrency can
	// change latency and efficiency, but it does not turn them into wall time.
	fmt.Printf("cost recorded: %t\n",
		result.Comparisons > 0 && result.Participations == 2*result.Comparisons)

	// Output:
	// found item 0 at the Late extreme under the Baseline conditional mean premise
	// cost recorded: true
}

func ExampleResumableOutlierSolver() {
	cmp := ranking.NewComparator(ranking.Profile{
		MaxWidth: 2,
		Controls: map[ranking.ControlKind]ranking.ControlSupply{
			ranking.Baseline: ranking.UnboundedControls(),
		},
	}, func(_ context.Context, items []ranking.ItemID) ([]int, error) {
		ranks := make([]int, len(items))
		for position, item := range items {
			if item == 0 {
				ranks[position] = 1
			}
		}
		return ranks, nil
	})
	solver := &ranking.BaselineReservedOutlierSolver{}
	run, err := solver.Start(ranking.OutlierRequest{
		M: 1, Direction: ranking.Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.05, FalseNegativeRisk: 0.05,
	}, cmp)
	if err != nil {
		panic(err)
	}
	defer run.Close()

	for {
		result, advanceErr := run.Advance(
			context.Background(), ranking.Budget{Comparisons: 8})
		if advanceErr != nil {
			panic(advanceErr)
		}
		if result.Outcome == ranking.OutlierPaused {
			continue
		}
		fmt.Printf("%s index=%d\n", result.Outcome, result.Index)
		break
	}

	// Output:
	// Found index=0
}
