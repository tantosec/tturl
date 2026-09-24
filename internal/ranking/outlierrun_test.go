package ranking

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/tantosec/tturl/stats"
)

var (
	_ ResumableOutlierSolver  = (*BaselineReservedOutlierSolver)(nil)
	_ ResumableOutlierSolver  = (*PeerFirstOutlierSolver)(nil)
	_ ResumableOutlierSolver  = (*BaselineConfirmedOutlierSolver)(nil)
	_ ResumableOutlierSolver  = (*EdgeDirectedOutlierSolver)(nil)
	_ IdentifiedOutlierSolver = (*BaselineReservedOutlierSolver)(nil)
	_ IdentifiedOutlierSolver = (*PeerFirstOutlierSolver)(nil)
	_ IdentifiedOutlierSolver = (*BaselineConfirmedOutlierSolver)(nil)
	_ IdentifiedOutlierSolver = (*EdgeDirectedOutlierSolver)(nil)
)

func resumableTestComparator(concurrency int) *Comparator {
	return NewComparator(Profile{
		MaxWidth: 4, MaxConcurrency: concurrency,
		Controls: map[ControlKind]ControlSupply{
			Baseline: UnboundedControls(),
		},
	}, func(_ context.Context, items []ItemID) ([]int, error) {
		ranks := make([]int, len(items))
		nextRank := 0
		for position, item := range items {
			if item == 0 {
				ranks[position] = len(items) - 1
			} else {
				ranks[position] = nextRank
				nextRank++
			}
		}
		return ranks, nil
	})
}

func newOutlierRunTestRNG() *rand.Rand {
	//nolint:gosec // Reproducible solver scheduling requires a fixed seed.
	return rand.New(rand.NewPCG(41, 73))
}

func resumableTestSolvers() map[string]func() ResumableOutlierSolver {
	return map[string]func() ResumableOutlierSolver{
		"baseline-reserved": func() ResumableOutlierSolver {
			return &BaselineReservedOutlierSolver{newRNG: newOutlierRunTestRNG}
		},
		"peer-first": func() ResumableOutlierSolver {
			return &PeerFirstOutlierSolver{newRNG: newOutlierRunTestRNG}
		},
		"baseline-confirmed": func() ResumableOutlierSolver {
			return &BaselineConfirmedOutlierSolver{newRNG: newOutlierRunTestRNG}
		},
		"edge-directed": func() ResumableOutlierSolver {
			return &EdgeDirectedOutlierSolver{newRNG: newOutlierRunTestRNG}
		},
	}
}

func TestOutlierRunSerialTranchesPreserveDecisions(t *testing.T) {
	request := OutlierRequest{
		M: 4, Direction: Late, FalseFindRisk: 0.1, FalseNegativeRisk: 0.1,
	}
	for name, newSolver := range resumableTestSolvers() {
		t.Run(name, func(t *testing.T) {
			one, err := newSolver().Start(request, resumableTestComparator(1))
			if err != nil {
				t.Fatal(err)
			}
			defer one.Close()
			want, err := one.Advance(t.Context(), Budget{Comparisons: 2_000})
			if err != nil {
				t.Fatal(err)
			}
			if want.Outcome != OutlierFound {
				t.Fatalf("one-tranche outcome = %v", want.Outcome)
			}
			identified, err := newSolver().Identify(
				t.Context(), request, resumableTestComparator(1))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(identified, want) {
				t.Fatalf("Identify result differs\ngot:  %+v\nwant: %+v",
					identified, want)
			}

			many, err := newSolver().Start(request, resumableTestComparator(1))
			if err != nil {
				t.Fatal(err)
			}
			defer many.Close()
			var got OutlierResult
			for got.Outcome != OutlierFound {
				got, err = many.Advance(t.Context(), Budget{Comparisons: 1})
				if err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("many-tranche result differs\ngot:  %+v\nwant: %+v", got, want)
			}
		})
	}
}

func TestOutlierRunConcurrentTranchesPreserveWavePartition(t *testing.T) {
	request := OutlierRequest{
		M: 4, Direction: Late, FalseFindRisk: 0.1, FalseNegativeRisk: 0.1,
	}
	for name, newSolver := range resumableTestSolvers() {
		t.Run(name, func(t *testing.T) {
			one, err := newSolver().Start(request, resumableTestComparator(4))
			if err != nil {
				t.Fatal(err)
			}
			defer one.Close()
			want, err := one.Advance(t.Context(), Budget{Comparisons: 2_000})
			if err != nil {
				t.Fatal(err)
			}

			many, err := newSolver().Start(request, resumableTestComparator(4))
			if err != nil {
				t.Fatal(err)
			}
			defer many.Close()
			var got OutlierResult
			for got.Outcome != OutlierFound {
				got, err = many.Advance(t.Context(), Budget{Comparisons: 4})
				if err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("partitioned result differs\ngot:  %+v\nwant: %+v", got, want)
			}
		})
	}
}

func TestOutlierRunPauseReportsCumulativeCost(t *testing.T) {
	cmp := NewComparator(Profile{
		MaxWidth: 4, MaxConcurrency: 2,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, identityCompare)
	run, err := (&BaselineReservedOutlierSolver{
		newRNG: newOutlierRunTestRNG,
	}).Start(OutlierRequest{
		M: 4, Direction: Late, FalseFindRisk: 0.001, FalseNegativeRisk: 0.001,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	first, err := run.Advance(t.Context(), Budget{Comparisons: 3})
	if err != nil {
		t.Fatal(err)
	}
	second, err := run.Advance(t.Context(), Budget{Comparisons: 5})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != OutlierPaused || first.Comparisons != 3 ||
		second.Outcome != OutlierPaused || second.Comparisons != 8 {
		t.Fatalf("pause costs = (%v, %d), (%v, %d)",
			first.Outcome, first.Comparisons, second.Outcome, second.Comparisons)
	}
}

func TestOutlierRunBindsRequestPolicy(t *testing.T) {
	cmp := NewComparator(Profile{
		MaxWidth: 4,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, identityCompare)
	solver := &BaselineReservedOutlierSolver{
		MaxConcurrency: 1,
		newRNG:         newOutlierRunTestRNG,
	}
	req := OutlierRequest{
		M: 4, Direction: Late, MinimumEdge: 0.05,
		FalseFindRisk: 0.01, FalseNegativeRisk: 0.02,
	}
	run, err := solver.Start(req, cmp)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()

	// The caller retains its request value, but changing that value cannot alter
	// the policy already bound to the live run.
	req.MinimumEdge = 0.2
	req.FalseFindRisk = 0.2
	req.FalseNegativeRisk = 0.2
	paused, err := run.Advance(t.Context(), Budget{Comparisons: 1})
	if err != nil {
		t.Fatal(err)
	}
	detail := paused.Detail.(OutlierDiagnostics)
	if paused.Outcome != OutlierPaused || detail.MinimumEdge != 0.05 ||
		detail.Threshold != stats.VilleUnionThreshold(4, 0.01) {
		t.Fatalf("bound run policy changed: result=%+v detail=%+v", paused, detail)
	}
	terminal, err := run.Advance(t.Context(), Budget{Comparisons: 100_000})
	if err != nil {
		t.Fatal(err)
	}
	wantGuarantee := OutlierGuarantee{
		ErrorDirection: OutlierErrorFalseNegative, ErrorRiskLimit: 0.02,
		MinimumEdge: 0.05, Direction: Late, Premise: OutlierPremiseBaselineMean,
		Regime: OutlierInferenceAnytimeValid,
	}
	if terminal.Outcome != OutlierNegative || terminal.Index != -1 ||
		terminal.OutlierGuarantee != wantGuarantee {
		t.Fatalf("bound negative policy = %+v, want guarantee %+v",
			terminal, wantGuarantee)
	}

	// The same stateless solver accepts a different edge and risks on a later
	// request.
	second, err := solver.Start(OutlierRequest{
		M: 4, Direction: Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.05, FalseNegativeRisk: 0.1,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	result, err := second.Advance(t.Context(), Budget{Comparisons: 1})
	if err != nil {
		t.Fatal(err)
	}
	secondDetail := result.Detail.(OutlierDiagnostics)
	if secondDetail.MinimumEdge != 0.1 ||
		secondDetail.Threshold != stats.VilleUnionThreshold(4, 0.05) {
		t.Fatalf("second request policy = %+v", secondDetail)
	}
}

func TestOutlierRunPauseDrainsConcurrentWave(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	cmp := NewComparator(Profile{
		MaxWidth: 2, MaxConcurrency: 4,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, func(ctx context.Context, items []ItemID) ([]int, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return []int{0, 1}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	run, err := (&BaselineReservedOutlierSolver{
		MaxConcurrency: 4,
		newRNG:         newOutlierRunTestRNG,
	}).Start(
		OutlierRequest{M: 1, Direction: Late, FalseFindRisk: 0.0001, FalseNegativeRisk: 0.01}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	defer run.Close()
	done := make(chan outlierRunReply, 1)
	go func() {
		result, advanceErr := run.Advance(
			context.Background(), Budget{Comparisons: 4})
		done <- outlierRunReply{result: result, err: advanceErr}
	}()
	for range 4 {
		<-entered
	}
	select {
	case result := <-done:
		t.Fatalf("Advance returned before its wave drained: %+v", result)
	default:
	}
	close(release)
	result := <-done
	if result.err != nil || result.result.Outcome != OutlierPaused ||
		result.result.Comparisons != 4 || result.result.Participations != 8 {
		t.Fatalf("paused wave = %+v", result)
	}
}

func TestOutlierRunContinuationAvoidsRestartedEvidence(t *testing.T) {
	request := OutlierRequest{
		M: 4, Direction: Late, FalseFindRisk: 0.01, FalseNegativeRisk: 0.1,
	}
	newSolver := resumableTestSolvers()["baseline-reserved"]
	complete, err := newSolver().Identify(
		t.Context(), request, resumableTestComparator(1))
	if err != nil {
		t.Fatal(err)
	}
	tranche := complete.Comparisons - 1
	if tranche < 1 {
		t.Fatalf("unexpected complete cost %d", complete.Comparisons)
	}

	for range 2 {
		restarted := newSolver()
		result, err := restarted.Start(request, resumableTestComparator(1))
		if err != nil {
			t.Fatal(err)
		}
		paused, err := result.Advance(t.Context(), Budget{Comparisons: tranche})
		result.Close()
		if err != nil {
			t.Fatal(err)
		}
		if paused.Outcome != OutlierPaused {
			t.Fatalf("restarted outcome = %v, want paused", paused.Outcome)
		}
	}

	continued, err := newSolver().Start(request, resumableTestComparator(1))
	if err != nil {
		t.Fatal(err)
	}
	defer continued.Close()
	first, err := continued.Advance(t.Context(), Budget{Comparisons: tranche})
	if err != nil {
		t.Fatal(err)
	}
	second, err := continued.Advance(t.Context(), Budget{Comparisons: tranche})
	if err != nil {
		t.Fatal(err)
	}
	if first.Outcome != OutlierPaused || second.Outcome != OutlierFound ||
		second.Comparisons != complete.Comparisons {
		t.Fatalf("continued results = %v/%d then %v/%d, complete at %d",
			first.Outcome, first.Comparisons, second.Outcome,
			second.Comparisons, complete.Comparisons)
	}
}

func TestOutlierRunRejectsInvalidAndOverlappingAdvance(t *testing.T) {
	entered := make(chan struct{})
	var enteredOnce sync.Once
	cmp := NewComparator(Profile{
		MaxWidth: 2, MaxConcurrency: 1,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, func(ctx context.Context, _ []ItemID) ([]int, error) {
		enteredOnce.Do(func() { close(entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	})
	run, err := (&BaselineReservedOutlierSolver{}).Start(OutlierRequest{
		M: 1, Direction: Late, FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Advance(t.Context(), Budget{}); err == nil {
		t.Fatal("zero budget succeeded")
	}
	advanceDone := make(chan error, 1)
	go func() {
		_, advanceErr := run.Advance(context.Background(), Budget{Comparisons: 1})
		advanceDone <- advanceErr
	}()
	<-entered
	if _, err := run.Advance(t.Context(), Budget{Comparisons: 1}); !errors.Is(err, ErrOutlierRunBusy) {
		t.Fatalf("overlapping Advance error = %v", err)
	}
	run.Close()
	if err := <-advanceDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Advance error = %v", err)
	}
	if _, err := run.Advance(t.Context(), Budget{Comparisons: 1}); !errors.Is(err, ErrOutlierRunClosed) {
		t.Fatalf("closed Advance error = %v", err)
	}
}

func TestOutlierRunEntryPointBoundaries(t *testing.T) {
	cmp := resumableTestComparator(1)
	if _, err := (&BaselineReservedOutlierSolver{}).Start(
		OutlierRequest{M: 0, Direction: Late, FalseFindRisk: 0.1, FalseNegativeRisk: 0.01}, cmp,
	); err == nil ||
		!strings.Contains(err.Error(), "BaselineReservedOutlierSolver.Start") ||
		!strings.Contains(err.Error(), "M must") {
		t.Fatalf("invalid Start error = %v", err)
	}
	run, err := (&BaselineReservedOutlierSolver{}).Start(OutlierRequest{
		M: 4, Direction: Late, FalseFindRisk: 0.001, FalseNegativeRisk: 0.01,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Advance(t.Context(), Budget{Comparisons: -1}); err == nil {
		t.Fatal("negative-budget Advance succeeded")
	}
	if _, err := run.Advance(t.Context(), Budget{Comparisons: math.MaxInt}); err == nil {
		t.Fatal("overflowing Advance succeeded")
	}
	run.Close()
}
