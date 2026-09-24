package ranking

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// concurrentComparator makes syntheticComparator safe for concurrent calls and
// records the maximum concurrency observed. It can rendezvous an initial group
// of calls to force a known concurrency level.
type concurrentComparator struct {
	syntheticComparator
	maxConcurrency int
	noiseSeed      uint64
	rendezvous     int // if > 0, block the first this-many calls until all have arrived

	mu          sync.Mutex
	occurrences map[string]uint64
	inFlight    int
	maxSeen     int
	arrived     int
	released    chan struct{}
}

func newConcurrentComparator(
	base syntheticComparator, maxConc int, noiseSeed uint64,
) *concurrentComparator {
	return &concurrentComparator{
		syntheticComparator: base,
		maxConcurrency:      maxConc,
		noiseSeed:           noiseSeed,
		occurrences:         make(map[string]uint64),
		released:            make(chan struct{}),
	}
}

func (c *concurrentComparator) comparator() *Comparator {
	return NewComparator(Profile{
		MaxWidth:       c.n,
		MaxConcurrency: c.maxConcurrency,
		Controls:       map[ControlKind]ControlSupply{Baseline: c.controls},
	}, c.compare)
}

func (c *concurrentComparator) maxInFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxSeen
}

func (c *concurrentComparator) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *concurrentComparator) compare(_ context.Context, byPosition []ItemID) ([]int, error) {
	key := evaluationLayoutKey(byPosition)
	c.mu.Lock()
	c.calls++
	c.inFlight++
	if c.inFlight > c.maxSeen {
		c.maxSeen = c.inFlight
	}
	c.arrived++
	gate := c.rendezvous > 0 && c.arrived <= c.rendezvous
	if c.rendezvous > 0 && c.arrived == c.rendezvous {
		close(c.released) // wake the whole rendezvous group; later arrivals do not gate
	}
	released := c.released
	occurrence := c.occurrences[key]
	c.occurrences[key]++
	c.mu.Unlock()

	// Key channel randomness by layout and its own occurrence count. Concurrent
	// callback arrival order can therefore change neither the draw assigned to a
	// layout nor the resulting decision path.
	//nolint:gosec // Reproducible test simulation requires a deterministic RNG.
	rng := rand.New(rand.NewPCG(
		evaluationLayoutSeed(c.noiseSeed, key), occurrence^0x434f4e4355525245))
	noise := make([]float64, c.n)
	for i := range noise {
		noise[i] = c.noise(rng)
	}

	if gate {
		<-released
	}

	scores := make([]float64, c.n)
	for p := 0; p < c.n; p++ {
		offset := 0.0
		if byPosition[p].IsCandidate() {
			offset = c.offset[int(byPosition[p])]
		}
		scores[p] = c.bias(p, c.n) + noise[p] + offset
	}
	ranks := ranksByScore(scores)

	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
	return ranks, nil
}

// blockingComparator blocks each comparison until cancellation and reports when
// the first call starts.
type blockingComparator struct {
	width          int
	maxConcurrency int
	started        chan struct{}
}

func (c *blockingComparator) comparator() *Comparator {
	return NewComparator(Profile{MaxWidth: c.width, MaxConcurrency: c.maxConcurrency}, c.compare)
}

func (c *blockingComparator) compare(ctx context.Context, _ []ItemID) ([]int, error) {
	select {
	case c.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

// errorComparator returns the configured error from every comparison.
type errorComparator struct {
	width          int
	maxConcurrency int
	err            error
}

func (c *errorComparator) comparator() *Comparator {
	return NewComparator(Profile{MaxWidth: c.width, MaxConcurrency: c.maxConcurrency}, c.compare)
}
func (c *errorComparator) compare(context.Context, []ItemID) ([]int, error) { return nil, c.err }

// TestBaselineReservedOutlierSolverConcurrencyWindowDiscipline verifies that
// the solver reaches but never exceeds the effective concurrency limit.
func TestBaselineReservedOutlierSolverConcurrencyWindowDiscipline(t *testing.T) {
	const m, n, targetIdx = 16, 4, 7
	for _, tc := range []struct {
		name    string
		ceiling int
		policy  int // f.MaxConcurrency; 0 selects the ceiling
		wantW   int
	}{
		{"ceiling 4, no policy", 4, 0, 4},
		{"ceiling 8, clamp to 3", 8, 3, 3},
		{"ceiling 8, force sequential", 8, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offset := make([]float64, m)
			offset[targetIdx] = 6.0
			base := syntheticComparator{
				n: n, bias: linearBias(10), noise: gaussianNoise(8), offset: offset,
			}
			cmp := newConcurrentComparator(base, tc.ceiling, 1)
			cmp.rendezvous = tc.wantW // force wantW comparisons to be simultaneously in flight
			f := newBaselineReservedOutlierSolver(3)
			f.MaxConcurrency = tc.policy
			if _, err := f.Identify(
				context.Background(), OutlierRequest{
					M: m, Direction: Late,
					FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
				}, cmp.comparator(),
			); err != nil {
				t.Fatalf("Identify: %v", err)
			}
			if got := cmp.maxInFlight(); got != tc.wantW {
				t.Errorf("max simultaneous Compare = %d, want %d", got, tc.wantW)
			}
		})
	}
}

// TestBaselineReservedOutlierSolverConcurrencyReturnsNegativeAcrossBiasShapes
// verifies that
// concurrent, out-of-order observations do not create a false commit.
func TestBaselineReservedOutlierSolverConcurrencyReturnsNegativeAcrossBiasShapes(t *testing.T) {
	const (
		m             = 8
		n             = 4
		falseFindRisk = 0.000001
		edge          = 0.1 // raised only to speed elimination; the bound is edge-free
	)
	for _, concurrency := range []struct {
		width    int
		seedBase uint64
	}{
		{width: 2, seedBase: 21},
		{width: 4, seedBase: 41},
	} {
		w := concurrency.width
		seed := concurrency.seedBase
		for _, bs := range biasFamily() {
			base := syntheticComparator{
				n: n, bias: bs.fn, noise: gaussianNoise(8), offset: make([]float64, m),
			}
			cmp := newConcurrentComparator(base, w, seed)
			f := newBaselineReservedOutlierSolver(seed*13 + 3)
			res, err := f.Identify(context.Background(),
				OutlierRequest{
					M: m, Direction: Late, MinimumEdge: edge,
					FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01,
				}, cmp.comparator())
			if err != nil {
				t.Fatalf("w=%d bias=%s: %v", w, bs.name, err)
			}
			if res.Outcome != OutlierNegative ||
				res.ErrorRiskLimit != 0.01 {
				t.Errorf("w=%d bias=%s: result=(%v, %v), want (Negative, %v)",
					w, bs.name, res.Outcome, res.ErrorRiskLimit,
					0.01)
			}
			seed++
		}
	}
}

// TestBaselineReservedOutlierSolverConcurrencyFindsTarget verifies target
// recovery across
// the bias family under concurrent comparisons.
func TestBaselineReservedOutlierSolverConcurrencyFindsTarget(t *testing.T) {
	const (
		m, n, targetIdx = 16, 4, 7
		falseFindRisk   = 0.01
		signal, noiseSD = 12.0, 8.0
		w               = 4
	)
	for biasIndex, bs := range biasFamily() {
		offset := make([]float64, m)
		offset[targetIdx] = signal
		base := syntheticComparator{
			n: n, bias: bs.fn, noise: gaussianNoise(noiseSD), offset: offset,
		}
		cmp := newConcurrentComparator(base, w, uint64(biasIndex)+1)
		f := newBaselineReservedOutlierSolver(uint64(biasIndex)*7 + 1)
		res, err := f.Identify(context.Background(),
			OutlierRequest{M: m, Direction: Late, FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01}, cmp.comparator())
		if err != nil {
			t.Fatalf("bias=%s: %v", bs.name, err)
		}
		if res.Outcome != OutlierFound || res.Index != targetIdx {
			t.Errorf("bias=%s: result=(%v, %d), want (Found, %d)",
				bs.name, res.Outcome, res.Index, targetIdx)
		}
		if res.ErrorRiskLimit != falseFindRisk {
			t.Errorf("bias=%s: error risk %v, want %v",
				bs.name, res.ErrorRiskLimit, falseFindRisk)
		}
	}
}

// TestBaselineReservedOutlierSolverConcurrencyAccountingExact verifies that
// every call in a
// decisive atomic wave contributes to reported cost.
func TestBaselineReservedOutlierSolverConcurrencyAccountingExact(t *testing.T) {
	const (
		m, n, targetIdx = 16, 4, 7
		falseFindRisk   = 0.01
		signal          = 12.0 // strong, so the run commits and actually exercises overshoot
		w               = 4
	)
	for s := range 5 {
		offset := make([]float64, m)
		offset[targetIdx] = signal
		base := syntheticComparator{
			n: n, bias: linearBias(10), noise: gaussianNoise(8), offset: offset,
		}
		cmp := newConcurrentComparator(base, w, uint64(s)+7)
		f := newBaselineReservedOutlierSolver(uint64(s)*5 + 1)
		res, err := f.Identify(context.Background(),
			OutlierRequest{M: m, Direction: Late, FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01}, cmp.comparator())
		if err != nil {
			t.Fatalf("seed=%d: %v", s, err)
		}
		if res.Outcome != OutlierFound || res.Index != targetIdx {
			t.Fatalf("seed=%d: result = (%v, %d), want (Found, %d)",
				s, res.Outcome, res.Index, targetIdx)
		}
		if calls := cmp.callCount(); calls != res.Comparisons {
			t.Errorf("seed=%d: calls = %d, Comparisons = %d, want exact accounting",
				s, calls, res.Comparisons)
		}
	}
}

func TestBaselineReservedOutlierSolverConcurrentMultipleCrossingsChooseLowestIndex(t *testing.T) {
	const (
		width       = 4
		concurrency = 4
	)
	base := syntheticComparator{
		n: width, bias: flatBias, noise: gaussianNoise(0),
		offset:   []float64{100, 100},
		controls: UnboundedControls(),
	}
	cmp := newConcurrentComparator(base, concurrency, 71)
	solver := newBaselineReservedOutlierSolver(73)
	solver.MaxConcurrency = concurrency
	result, err := solver.Identify(context.Background(), OutlierRequest{
		M: 2, Direction: Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.10, FalseNegativeRisk: 0.01,
	}, cmp.comparator())
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != OutlierFound || result.Index != 0 {
		t.Fatalf("result = (%v, %d), want (Found, 0)",
			result.Outcome, result.Index)
	}
	detail := result.Detail.(OutlierDiagnostics)
	for item := range 2 {
		if detail.Items[item].LogEvidence < detail.Threshold {
			t.Errorf("item %d evidence %v < threshold %v",
				item, detail.Items[item].LogEvidence, detail.Threshold)
		}
	}
	if result.Comparisons%concurrency != 0 ||
		result.Participations != result.Comparisons*width ||
		cmp.callCount() != result.Comparisons {
		t.Errorf("cost = %d comparisons/%d participations/%d calls",
			result.Comparisons, result.Participations, cmp.callCount())
	}
}

// TestBaselineReservedOutlierSolverConcurrencyWaveAveragesFactors verifies
// that repeated
// within-wave observations contribute an averaged e-factor.
func TestBaselineReservedOutlierSolverConcurrencyWaveAveragesFactors(t *testing.T) {
	const width = 4
	base := syntheticComparator{
		n: width, bias: flatBias, noise: gaussianNoise(0), offset: []float64{100},
		controls: UnboundedControls(),
	}
	cmp := newConcurrentComparator(base, width, 1)
	f := newBaselineReservedOutlierSolver(3)
	f.MaxComparisons = width
	res, err := f.Identify(context.Background(), OutlierRequest{
		M: 1, Direction: Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.000001, FalseNegativeRisk: 0.000001,
	}, cmp.comparator())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if res.Outcome != OutlierInconclusive {
		t.Fatalf("Outcome = %v, want Inconclusive", res.Outcome)
	}
	if res.Comparisons != width || res.Participations != width*width {
		t.Errorf("cost = (%d comparisons, %d participations), want (%d, %d)",
			res.Comparisons, res.Participations, width, width*width)
	}
	if res.Items[0].Samples != width {
		t.Errorf("Samples = %d, want %d", res.Items[0].Samples, width)
	}
	detail := res.Detail.(OutlierDiagnostics)
	bets, logWeights := outlierBettingMixture()
	logFactors := make([]float64, len(bets))
	for i, bet := range bets {
		logFactors[i] = math.Log1p(bet * 0.5)
	}
	want := outlierLogMixture(logFactors, logWeights)
	if got := detail.Items[0].LogEvidence; math.Abs(got-want) > 1e-14 {
		t.Errorf("LogEvidence = %.17g, want one-wave value %.17g", got, want)
	}
}

func TestOutlierBoundaryPositiveConcurrencyWaveAveragesFactors(t *testing.T) {
	const width = 4
	base := syntheticComparator{
		n: width, bias: flatBias, noise: gaussianNoise(0), offset: []float64{100},
		controls: UnboundedControls(),
	}
	cmp := newConcurrentComparator(base, width, 1)
	f := newBaselineReservedOutlierSolver(3)
	f.MaxComparisons = width
	f.boundaryPositive = true
	res, err := f.Identify(context.Background(), OutlierRequest{
		M: 1, Direction: Late, MinimumEdge: 0.1, FalseFindRisk: 0.000001,
		FalseNegativeRisk: 0.000001,
	}, cmp.comparator())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutlierInconclusive {
		t.Fatalf("Outcome = %v, want Inconclusive", res.Outcome)
	}
	if res.Comparisons != width || res.Items[0].Samples != width {
		t.Fatalf("comparisons/samples = %d/%d, want %d/%d",
			res.Comparisons, res.Items[0].Samples, width, width)
	}
	got := res.Detail.(OutlierDiagnostics).Items[0].LogEvidence
	want := math.Log1p((outlierBoundaryBet{}).bet(0.1, width-1) * 0.5)
	if math.Abs(got-want) > 1e-14 {
		t.Errorf("LogEvidence = %.17g, want one-wave value %.17g", got, want)
	}
}

func TestBaselineReservedOutlierSolverConcurrencyRunsPartialFinalWave(t *testing.T) {
	const width, budget = 4, 10
	base := syntheticComparator{
		n: width, bias: flatBias, noise: gaussianNoise(0), offset: []float64{0},
		controls: UnboundedControls(),
	}
	cmp := newConcurrentComparator(base, width, 4)
	f := newBaselineReservedOutlierSolver(6)
	f.MaxComparisons = budget
	res, err := f.Identify(context.Background(), OutlierRequest{
		M: 1, Direction: Late, MinimumEdge: 0.1,
		FalseFindRisk: 0.000001, FalseNegativeRisk: 0.000001,
	}, cmp.comparator())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if res.Comparisons != budget || cmp.callCount() != budget {
		t.Errorf("comparisons = %d, calls = %d, want %d each",
			res.Comparisons, cmp.callCount(), budget)
	}
	if res.Participations != budget*width {
		t.Errorf("Participations = %d, want %d", res.Participations, budget*width)
	}
}

// TestBaselineReservedOutlierSolverConcurrencyCancellation verifies that
// cancellation
// drains in-flight comparisons and returns the context error.
func TestBaselineReservedOutlierSolverConcurrencyCancellation(t *testing.T) {
	cmp := &blockingComparator{width: 4, maxConcurrency: 4, started: make(chan struct{}, 1)}
	f := newBaselineReservedOutlierSolver(1)
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		res OutlierResult
		err error
	}
	done := make(chan outcome, 1)
	var reports atomic.Int64
	go func() {
		req := OutlierRequest{
			M: 16, Direction: Late, FalseFindRisk: 0.01,
			FalseNegativeRisk: 0.01,
			ProgressCadence:   EveryComparisons(1),
			Progress:          func(OutlierProgress) { reports.Add(1) },
		}
		res, err := f.Identify(ctx, req, cmp.comparator())
		done <- outcome{res, err}
	}()
	<-cmp.started // at least one comparison is genuinely in flight
	cancel()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case o := <-done:
		if !errors.Is(o.err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", o.err)
		}
		if want := (OutlierResult{Index: -1}); !reflect.DeepEqual(o.res, want) {
			t.Errorf("result = %+v, want failure result %+v", o.res, want)
		}
		if reports.Load() != 0 {
			t.Errorf("a cancelled (error) return delivered %d progress reports, want 0 "+
				"(no final snapshot on an error return)", reports.Load())
		}
	case <-timer.C:
		t.Fatal("Identify did not return within 5s of cancellation (goroutine leak or missed drain)")
	}
}

// TestBaselineReservedOutlierSolverConcurrencyComparatorError verifies that a
// channel error
// takes precedence over cancellation induced while draining its siblings.
func TestBaselineReservedOutlierSolverConcurrencyComparatorError(t *testing.T) {
	sentinel := errors.New("channel down")
	cmp := &errorComparator{width: 4, maxConcurrency: 4, err: sentinel}
	f := newBaselineReservedOutlierSolver(1)
	res, err := f.Identify(
		context.Background(),
		OutlierRequest{M: 16, Direction: Late, FalseFindRisk: 0.01, FalseNegativeRisk: 0.01},
		cmp.comparator(),
	)
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want it to wrap the comparator error %v", err, sentinel)
	}
	if want := (OutlierResult{Index: -1}); !reflect.DeepEqual(res, want) {
		t.Errorf("result = %+v, want failure result %+v", res, want)
	}
}
