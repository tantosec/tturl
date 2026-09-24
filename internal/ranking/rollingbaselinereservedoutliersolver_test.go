package ranking

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	_ OutlierSolver           = (*RollingBaselineReservedOutlierSolver)(nil)
	_ IdentifiedOutlierSolver = (*RollingBaselineReservedOutlierSolver)(nil)
)

func rollingTestRNG(seed uint64) func() *rand.Rand {
	return func() *rand.Rand {
		//nolint:gosec // Reproducible tests require deterministic randomness.
		return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	}
}

func rollingRanks(candidatePosition int, late bool) ([]int, error) {
	switch candidatePosition {
	case 0:
		if late {
			return []int{1, 0}, nil
		}
		return []int{0, 1}, nil
	case 1:
		if late {
			return []int{0, 1}, nil
		}
		return []int{1, 0}, nil
	default:
		return nil, errors.New("test comparison requires one candidate")
	}
}

func TestRollingBaselineReservedOutlierSolverIsDeliberatelyOneShot(t *testing.T) {
	if _, ok := any(&RollingBaselineReservedOutlierSolver{}).(ResumableOutlierSolver); ok {
		t.Fatal("rolling solver unexpectedly implements ResumableOutlierSolver")
	}
}

func TestRollingBaselineReservedOutlierSolverRequiresBaselineBeforeDial(t *testing.T) {
	calls := 0
	cmp := NewComparator(Profile{MaxWidth: 2, MaxConcurrency: 4},
		func(context.Context, []ItemID) ([]int, error) {
			calls++
			return []int{0, 1}, nil
		})
	_, err := (&RollingBaselineReservedOutlierSolver{MaxComparisons: 8}).Identify(
		context.Background(), OutlierRequest{
			M: 4, Direction: Late,
			FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
		}, cmp)
	if err == nil || calls != 0 {
		t.Fatalf("Identify error = %v, comparator calls = %d; want error before dial",
			err, calls)
	}
}

type rollingPrefixTestChannel struct {
	mu sync.Mutex

	coverage    []int
	coveragePos []int
	winner      int
	seenInitial []bool
	calls       []int
	dispatched  map[int]int
	initial     int
	returned    int

	allStarted chan struct{}
	suffixDone chan struct{}
	release    chan struct{}
}

func newRollingPrefixTestChannel(coverage []int) *rollingPrefixTestChannel {
	positions := make([]int, len(coverage))
	for position, candidate := range coverage {
		positions[candidate] = position
	}
	return &rollingPrefixTestChannel{
		coverage: coverage, coveragePos: positions,
		winner: coverage[len(coverage)-1], seenInitial: make([]bool, len(coverage)),
		dispatched: make(map[int]int),
		allStarted: make(chan struct{}), suffixDone: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (c *rollingPrefixTestChannel) compareAt(
	_ context.Context, byPosition []ItemID, dispatchIndex int,
) ([]int, error) {
	candidate, candidatePosition := -1, -1
	for position, item := range byPosition {
		if item.IsCandidate() {
			candidate, candidatePosition = int(item), position
			break
		}
	}
	c.mu.Lock()
	c.calls = append(c.calls, candidate)
	if dispatchIndex >= 0 {
		c.dispatched[dispatchIndex] = candidate
	}
	initial := !c.seenInitial[candidate]
	if initial {
		c.seenInitial[candidate] = true
		c.initial++
		if c.initial == len(c.coverage) {
			close(c.allStarted)
		}
	}
	c.mu.Unlock()

	if initial {
		<-c.allStarted
		if c.coveragePos[candidate] == 0 {
			<-c.release
		} else {
			c.mu.Lock()
			c.returned++
			if c.returned == len(c.coverage)-1 {
				close(c.suffixDone)
			}
			c.mu.Unlock()
		}
	}

	return rollingRanks(candidatePosition, candidate == c.winner)
}

func (c *rollingPrefixTestChannel) compare(
	ctx context.Context, byPosition []ItemID,
) ([]int, error) {
	return c.compareAt(ctx, byPosition, -1)
}

func (c *rollingPrefixTestChannel) snapshotCalls() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

func (c *rollingPrefixTestChannel) dispatchedCandidate(index int) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	candidate, ok := c.dispatched[index]
	return candidate, ok
}

func TestRollingBaselineReservedWaitsForHeadAndUsesCommittedPrefix(t *testing.T) {
	const seed = 91
	rng := rollingTestRNG(seed)()
	coverage := rng.Perm(4)
	channel := newRollingPrefixTestChannel(coverage)
	cmp := NewComparator(Profile{
		MaxWidth: 2, MaxConcurrency: 4,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, channel.compare)
	cmp.compareIndexed = channel.compareAt
	solver := &RollingBaselineReservedOutlierSolver{
		MaxComparisons: 8, MaxConcurrency: 4, newRNG: rollingTestRNG(seed),
	}

	type answer struct {
		result OutlierResult
		err    error
	}
	answered := make(chan answer, 1)
	go func() {
		result, err := solver.Identify(context.Background(), OutlierRequest{
			M: 4, Direction: Late,
			FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
		}, cmp)
		answered <- answer{result: result, err: err}
	}()

	select {
	case <-channel.suffixDone:
	case <-time.After(2 * time.Second):
		t.Fatal("initial out-of-order suffix did not complete")
	}
	if calls := channel.snapshotCalls(); len(calls) != 4 {
		t.Fatalf("calls before dispatch head completed = %v, want one bounded window", calls)
	}
	close(channel.release)

	var got answer
	select {
	case got = <-answered:
	case <-time.After(2 * time.Second):
		t.Fatal("Identify did not finish")
	}
	if got.err != nil {
		t.Fatalf("Identify: %v", got.err)
	}
	calls := channel.snapshotCalls()
	if len(calls) != 8 {
		t.Fatalf("calls = %v, want eight", calls)
	}
	first := slices.Clone(calls[:4])
	slices.Sort(first)
	if !slices.Equal(first, []int{0, 1, 2, 3}) {
		t.Fatalf("initial window = %v, want complete coverage", calls[:4])
	}
	// A four-slot window cannot issue dispatch 7 until four lower dispatches
	// commit, which necessarily includes the complete initial prefix 0 through 3.
	last, ok := channel.dispatchedCandidate(7)
	if !ok || last != channel.winner {
		t.Fatalf("eighth dispatch = %d, %t; want committed-prefix leader %d",
			last, ok, channel.winner)
	}
	if got.result.Comparisons != 8 || got.result.Participations != 16 {
		t.Fatalf("cost = %+v, want 8 comparisons and 16 participations", got.result.Cost)
	}
}

func rollingDeterministicComparator(target int, candidateLate bool) *Comparator {
	return NewComparator(Profile{
		MaxWidth: 2, MaxConcurrency: 16,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, func(_ context.Context, byPosition []ItemID) ([]int, error) {
		candidate, candidatePosition := -1, -1
		for position, item := range byPosition {
			if item.IsCandidate() {
				candidate, candidatePosition = int(item), position
				break
			}
		}
		late := candidateLate && candidate == target
		return rollingRanks(candidatePosition, late)
	})
}

func TestRollingBaselineReservedFindsCleanTarget(t *testing.T) {
	const target = 17
	result, err := (&RollingBaselineReservedOutlierSolver{
		MaxComparisons: 500, MaxConcurrency: 16, newRNG: rollingTestRNG(17),
	}).Identify(context.Background(), OutlierRequest{
		M: 36, Direction: Late,
		FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
	}, rollingDeterministicComparator(target, true))
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if result.Outcome != OutlierFound || result.Index != target {
		t.Fatalf("result = %+v, want target %d", result, target)
	}
}

func TestRollingBaselineReservedMultipliesDispatchFactors(t *testing.T) {
	result, err := (&RollingBaselineReservedOutlierSolver{
		MaxComparisons: 2, MaxConcurrency: 2, newRNG: rollingTestRNG(19),
	}).Identify(context.Background(), OutlierRequest{
		M: 1, Direction: Late,
		FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
	}, rollingDeterministicComparator(0, true))
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	bets, weights := outlierBettingMixture()
	logs := make([]float64, len(bets))
	for component, bet := range bets {
		logs[component] = 2 * math.Log(1+bet*0.5)
	}
	want := outlierLogMixture(logs, weights)
	got := result.Detail.(OutlierDiagnostics).Items[0].LogEvidence
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("log evidence = %.17g, want two-factor product %.17g", got, want)
	}
}

func TestRollingBaselineReservedReturnsCleanNegative(t *testing.T) {
	result, err := (&RollingBaselineReservedOutlierSolver{
		MaxComparisons: 2_000, MaxConcurrency: 16, newRNG: rollingTestRNG(23),
	}).Identify(context.Background(), OutlierRequest{
		M: 4, Direction: Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
	}, rollingDeterministicComparator(-1, false))
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if result.Outcome != OutlierNegative || result.Index != -1 {
		t.Fatalf("result = %+v, want negative", result)
	}
}

func TestRollingBaselineReservedCancellationDrainsWindow(t *testing.T) {
	started := make(chan struct{}, 4)
	cmp := NewComparator(Profile{
		MaxWidth: 2, MaxConcurrency: 4,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, func(ctx context.Context, _ []ItemID) ([]int, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (&RollingBaselineReservedOutlierSolver{
			MaxComparisons: 100, MaxConcurrency: 4, newRNG: rollingTestRNG(29),
		}).Identify(ctx, OutlierRequest{
			M: 8, Direction: Late,
			FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
		}, cmp)
		done <- err
	}()
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("rolling window did not fill")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Identify error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Identify did not drain cancelled window")
	}
}

func TestRollingBaselineReservedReportsLowestComparatorFailure(t *testing.T) {
	want := errors.New("channel failed")
	var mu sync.Mutex
	arrived := 0
	allArrived := make(chan struct{})
	cmp := NewComparator(Profile{
		MaxWidth: 2, MaxConcurrency: 4,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, func(context.Context, []ItemID) ([]int, error) {
		mu.Lock()
		arrived++
		if arrived == 4 {
			close(allArrived)
		}
		mu.Unlock()
		<-allArrived
		return nil, want
	})
	_, err := (&RollingBaselineReservedOutlierSolver{
		MaxComparisons: 8, MaxConcurrency: 4, newRNG: rollingTestRNG(31),
	}).Identify(context.Background(), OutlierRequest{
		M: 8, Direction: Late,
		FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
	}, cmp)
	if !errors.Is(err, want) ||
		!strings.Contains(err.Error(), "comparison 0 failed") {
		t.Fatalf("Identify error = %v, want comparison 0 wrapping channel failure", err)
	}
}
