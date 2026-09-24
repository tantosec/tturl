package ranking

import (
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tantosec/tturl/stats"
)

// syntheticComparator models position bias, noise, and an optional target.
//
//	latentScore[p] = bias(p, n) + noise() + offset[item in position p]
//	rank           = ascending order of latentScore (0 = first)
//
// Offset is zero for a baseline, positive for a late target, and negative for
// an early target.
type syntheticComparator struct {
	n      int
	bias   func(pos, n int) float64
	noise  func(rng *rand.Rand) float64
	offset []float64
	rng    *rand.Rand
	calls  int

	// controls is the baseline supply declared by comparator.
	controls ControlSupply

	// Optional observations of candidate ranks and comparison count.
	observedSamples  []int
	observedRankSums []int
}

// comparator returns c behind its declared profile.
func (c *syntheticComparator) comparator() *Comparator {
	return NewComparator(Profile{
		MaxWidth: c.n,
		Controls: map[ControlKind]ControlSupply{Baseline: c.controls},
	}, c.compare)
}

func (c *syntheticComparator) compare(_ context.Context, byPosition []ItemID) ([]int, error) {
	c.calls++
	n := len(byPosition)
	scores := make([]float64, n)
	for p := range n {
		offset := 0.0 // a control is a baseline
		if byPosition[p].IsCandidate() {
			offset = c.offset[int(byPosition[p])]
		}
		scores[p] = c.bias(p, n) + c.noise(c.rng) + offset
	}
	ranks := ranksByScore(scores)
	for position, item := range byPosition {
		if item.IsCandidate() && c.observedSamples != nil {
			candidate := int(item)
			c.observedSamples[candidate]++
			c.observedRankSums[candidate] += ranks[position]
		}
	}
	return ranks, nil
}

// ranksByScore returns zero-based ascending ranks, breaking ties by position.
func ranksByScore(scores []float64) []int {
	n := len(scores)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	// Insertion sort of positions by (score, index).
	for i := 1; i < n; i++ {
		for j := i; j > 0; j-- {
			a, b := order[j-1], order[j]
			if scores[a] < scores[b] || (scores[a] == scores[b] && a < b) {
				break
			}
			order[j-1], order[j] = order[j], order[j-1]
		}
	}
	ranks := make([]int, n)
	for r, p := range order {
		ranks[p] = r
	}
	return ranks
}

func TestRanksByScore(t *testing.T) {
	scores := []float64{3, -1, 3, 2}
	if got, want := ranksByScore(scores), []int{2, 0, 3, 1}; !slices.Equal(got, want) {
		t.Errorf("ranksByScore(%v) = %v, want %v", scores, got, want)
	}
	if want := []float64{3, -1, 3, 2}; !slices.Equal(scores, want) {
		t.Errorf("ranksByScore mutated input to %v, want %v", scores, want)
	}
}

// --- bias shapes (local; ranking must not import a simulator) ----------------

func flatBias(int, int) float64 { return 0 }

func linearBias(step float64) func(int, int) float64 {
	return func(pos, _ int) float64 { return float64(pos) * step }
}

func reversedBias(step float64) func(int, int) float64 {
	return func(pos, n int) float64 { return float64(n-1-pos) * step }
}

func stepBias(jump float64) func(int, int) float64 {
	return func(pos, n int) float64 {
		if pos >= n/2 {
			return jump
		}
		return 0
	}
}

func randomBias(seed uint64, scale float64) func(int, int) float64 {
	return func(pos, n int) float64 {
		//nolint:gosec // math/rand in tests, not security-sensitive
		r := rand.New(rand.NewPCG(seed^uint64(pos+1)*0x9e3779b97f4a7c15, ^uint64(pos)))
		return r.NormFloat64() * scale
	}
}

func gaussianNoise(sd float64) func(*rand.Rand) float64 {
	return func(rng *rand.Rand) float64 { return rng.NormFloat64() * sd }
}

// oneSidedNoise adds positive exponential spikes to Gaussian noise.
func oneSidedNoise(sd, spikeRate, spikeScale float64) func(*rand.Rand) float64 {
	return func(rng *rand.Rand) float64 {
		x := rng.NormFloat64() * sd
		if rng.Float64() < spikeRate {
			x += rng.ExpFloat64() * spikeScale
		}
		return x
	}
}

type biasShape struct {
	name string
	fn   func(pos, n int) float64
}

// biasFamily returns the position-bias shapes used by correctness tests.
func biasFamily() []biasShape {
	return []biasShape{
		{"flat", flatBias},
		{"linear", linearBias(10)},
		{"reversed", reversedBias(10)},
		{"stepped", stepBias(25)},
		{"random", randomBias(0xBADC0FFEE, 12)},
	}
}

// newBaselineReservedOutlierSolver returns a solver with deterministic
// randomness.
func newBaselineReservedOutlierSolver(seed uint64) *BaselineReservedOutlierSolver {
	return &BaselineReservedOutlierSolver{
		MaxComparisons: 100_000,
		newRNG: func() *rand.Rand {
			//nolint:gosec // math/rand in tests, not security-sensitive
			return rand.New(rand.NewPCG(seed, 0x1234567^seed)) // pin the source for a reproducible test
		},
	}
}

// --- finding the target across the whole bias family -------------------------

func TestBaselineReservedOutlierSolverFindsTargetAcrossBiasShapes(t *testing.T) {
	const (
		m             = 16
		n             = 4
		targetIdx     = 7
		falseFindRisk = 0.01
		// A strong signal makes each fixed-seed cell a deterministic path check.
		signal  = 12.0
		noiseSD = 8.0
	)
	for _, sign := range []OutlierDirection{Late, Early} {
		for biasIndex, bs := range biasFamily() {
			offset := make([]float64, m)
			if sign == Late {
				offset[targetIdx] = signal
			} else {
				offset[targetIdx] = -signal
			}
			cmp := &syntheticComparator{
				n: n, bias: bs.fn, noise: gaussianNoise(noiseSD), offset: offset,
				rng: rand.New(rand.NewPCG(uint64(biasIndex)+1, 0xABCD)), //nolint:gosec // Reproducible test data.
			}
			f := newBaselineReservedOutlierSolver(uint64(biasIndex)*7 + 1)
			res, err := f.Identify(context.Background(),
				OutlierRequest{M: m, Direction: sign, FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01}, cmp.comparator())
			if err != nil {
				t.Fatalf("sign=%v bias=%s: %v", sign, bs.name, err)
			}
			if res.Outcome != OutlierFound || res.Index != targetIdx || res.Direction != sign {
				t.Errorf("sign=%v bias=%s: result=(%v, %d, %v), want (Found, %d, %v)",
					sign, bs.name, res.Outcome, res.Index, res.Direction, targetIdx, sign)
			}
			assertGuarantee(t, res.OutlierGuarantee, OutlierGuarantee{
				ErrorDirection: OutlierErrorFalseFind, ErrorRiskLimit: falseFindRisk,
				Direction: sign, Premise: OutlierPremiseScheduledReferenceMean,
				Regime: OutlierInferenceAnytimeValid,
			})
		}
	}
}

// --- representative null decisions across channel shapes -------------------

func TestBaselineReservedOutlierSolverReturnsNegativeAcrossNullShapes(t *testing.T) {
	const (
		m             = 8
		n             = 4
		falseFindRisk = 0.000001
		// A larger edge accelerates elimination but does not affect positive
		// evidence or its false-find threshold.
		edge = 0.1
	)
	// Exercise one reproducible path through each channel shape.
	noises := []struct {
		name string
		fn   func(*rand.Rand) float64
	}{
		{"gaussian", gaussianNoise(8)},
		{"one-sided", oneSidedNoise(8, 0.1, 25)}, // late-tailed contaminants
	}
	for signIndex, sign := range []OutlierDirection{Late, Early} {
		for noiseIndex, nz := range noises {
			for biasIndex, bs := range biasFamily() {
				seed := uint64(100*signIndex + 10*noiseIndex + biasIndex + 1)
				cmp := &syntheticComparator{
					n: n, bias: bs.fn, noise: nz.fn, offset: make([]float64, m),
					rng: rand.New(rand.NewPCG(seed, 0x5151)), //nolint:gosec // Reproducible test data.
				}
				f := newBaselineReservedOutlierSolver(seed*13 + 3)
				res, err := f.Identify(context.Background(),
					OutlierRequest{
						M: m, Direction: sign, MinimumEdge: edge,
						FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01,
					}, cmp.comparator())
				if err != nil {
					t.Fatalf("sign=%v noise=%s bias=%s: %v", sign, nz.name, bs.name, err)
				}
				if res.Outcome != OutlierNegative {
					t.Errorf("sign=%v noise=%s bias=%s: outcome=%v, want Negative",
						sign, nz.name, bs.name, res.Outcome)
				}
				assertGuarantee(t, res.OutlierGuarantee, OutlierGuarantee{
					ErrorDirection: OutlierErrorFalseNegative, ErrorRiskLimit: 0.01,
					MinimumEdge: edge, Direction: sign,
					Premise: OutlierPremiseScheduledReferenceMean,
					Regime:  OutlierInferenceAnytimeValid,
				})
			}
		}
	}
}

// TestBaselineReservedOutlierSolverUnboundedControls verifies a population of
// one using an
// unbounded baseline supply.
func TestBaselineReservedOutlierSolverUnboundedControls(t *testing.T) {
	const n = 6
	offset := []float64{30} // one candidate: a lone, unmissable Late target
	cmp := &syntheticComparator{
		n: n, bias: flatBias, noise: gaussianNoise(4), offset: offset,
		controls: UnboundedControls(),
		rng:      rand.New(rand.NewPCG(3, 4)), //nolint:gosec // math/rand in tests, not security-sensitive
	}
	f := newBaselineReservedOutlierSolver(9)
	res, err := f.Identify(
		context.Background(), OutlierRequest{
			M: 1, Direction: Late, FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
		}, cmp.comparator())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if res.Outcome != OutlierFound || res.Index != 0 {
		t.Errorf("outcome=%v index=%d, want the lone suspect confirmed (Found, 0)", res.Outcome, res.Index)
	}
}

// TestBaselineReservedOutlierSolverControlsPreserveCorrectness verifies
// positive and
// negative outcomes when sampling against bounded baselines.
func TestBaselineReservedOutlierSolverControlsPreserveCorrectness(t *testing.T) {
	const (
		m             = 16
		n             = 4
		targetIdx     = 7
		falseFindRisk = 0.01
		signal        = 12.0
		noiseSD       = 8.0
	)
	// (1) target present: confirm finds it, never the wrong index.
	for _, test := range []struct {
		sign OutlierDirection
		seed uint64
	}{
		{sign: Late, seed: 1},
		{sign: Early, seed: 8},
	} {
		sign, seed := test.sign, test.seed
		offset := make([]float64, m)
		if sign == Late {
			offset[targetIdx] = signal
		} else {
			offset[targetIdx] = -signal
		}
		cmp := &syntheticComparator{
			n: n, bias: reversedBias(10), noise: gaussianNoise(noiseSD), offset: offset,
			//nolint:gosec // Reproducible test data.
			rng: rand.New(rand.NewPCG(seed, 0x9)), controls: BoundedControls(n - 1),
		}
		f := newBaselineReservedOutlierSolver(seed)
		res, err := f.Identify(context.Background(),
			OutlierRequest{M: m, Direction: sign, FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01}, cmp.comparator())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutlierFound || res.Index != targetIdx || res.Direction != sign {
			t.Errorf("sign=%v: result=(%v, %d, %v), want (Found, %d, %v)",
				sign, res.Outcome, res.Index, res.Direction, targetIdx, sign)
		}
	}
	// (2) no target: confirming a fluke leader against clean controls must not
	// manufacture a find with this stringent false-find risk.
	const nullFalseFindRisk, minimumEdge = 0.000001, 0.1
	nullCmp := &syntheticComparator{
		n: n, bias: reversedBias(10), noise: gaussianNoise(noiseSD), offset: make([]float64, m),
		//nolint:gosec // Reproducible test data.
		rng: rand.New(rand.NewPCG(101, 0x9)), controls: BoundedControls(n - 1),
	}
	f := newBaselineReservedOutlierSolver(103)
	res, err := f.Identify(context.Background(),
		OutlierRequest{
			M: m, Direction: Late, MinimumEdge: minimumEdge,
			FalseFindRisk: nullFalseFindRisk, FalseNegativeRisk: 0.01,
		}, nullCmp.comparator())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutlierNegative {
		t.Errorf("with controls and no target: Outcome = %v, want Negative", res.Outcome)
	}
}

// TestBaselineReservedOutlierSolverReportsComparisons verifies reported cost
// against the
// channel's observed comparison count.
func TestBaselineReservedOutlierSolverReportsComparisons(t *testing.T) {
	const m, n = 16, 4
	offset := make([]float64, m)
	offset[3] = 6
	cmp := &syntheticComparator{
		n: n, bias: linearBias(10), noise: gaussianNoise(8), offset: offset,
		rng: rand.New(rand.NewPCG(7, 8)), //nolint:gosec // math/rand in tests, not security-sensitive
	}
	f := newBaselineReservedOutlierSolver(5)
	res, err := f.Identify(context.Background(),
		OutlierRequest{M: m, Direction: Late, FalseFindRisk: 0.01, FalseNegativeRisk: 0.01}, cmp.comparator())
	if err != nil {
		t.Fatal(err)
	}
	if res.Comparisons != cmp.calls {
		t.Errorf("reported Comparisons %d != comparator calls %d", res.Comparisons, cmp.calls)
	}
	if res.Comparisons <= 0 {
		t.Errorf("Comparisons %d should be positive", res.Comparisons)
	}
	if res.Participations != cmp.calls*n {
		t.Errorf("Participations = %d, want calls*width = %d", res.Participations, cmp.calls*n)
	}
}

// --- Direction: Either (not known in advance) -------------------------------

// TestBaselineReservedOutlierSolverEitherFindsAndReportsSign verifies both
// unknown target
// directions across the bias family.
func TestBaselineReservedOutlierSolverEitherFindsAndReportsSign(t *testing.T) {
	const (
		m             = 16
		n             = 4
		targetIdx     = 7
		falseFindRisk = 0.01
		signal        = 12.0
		noiseSD       = 8.0
	)
	for _, plant := range []OutlierDirection{Late, Early} {
		for biasIndex, bs := range biasFamily() {
			offset := make([]float64, m)
			if plant == Late {
				offset[targetIdx] = signal
			} else {
				offset[targetIdx] = -signal
			}
			cmp := &syntheticComparator{
				n: n, bias: bs.fn, noise: gaussianNoise(noiseSD), offset: offset,
				controls: UnboundedControls(),
				rng:      rand.New(rand.NewPCG(uint64(biasIndex)+1, 0xABCD)), //nolint:gosec // Reproducible test data.
			}
			f := newBaselineReservedOutlierSolver(uint64(biasIndex)*7 + 1)
			res, err := f.Identify(context.Background(),
				OutlierRequest{
					M: m, Direction: Either,
					FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01,
				}, cmp.comparator())
			if err != nil {
				t.Fatalf("plant=%v bias=%s: %v", plant, bs.name, err)
			}
			if res.Outcome != OutlierFound || res.Index != targetIdx || res.Direction != plant {
				t.Errorf("plant=%v bias=%s: result=(%v, %d, %v), want (Found, %d, %v)",
					plant, bs.name, res.Outcome, res.Index, res.Direction, targetIdx, plant)
			}
			assertGuarantee(t, res.OutlierGuarantee, OutlierGuarantee{
				ErrorDirection: OutlierErrorFalseFind, ErrorRiskLimit: falseFindRisk,
				Direction: plant, Premise: OutlierPremiseBaselineMean,
				Regime: OutlierInferenceAnytimeValid,
			})
		}
	}
}

// TestBaselineReservedOutlierSolverEitherReturnsNegativeAcrossNullShapes
// verifies negative
// results across bias shapes and asymmetric noise.
func TestBaselineReservedOutlierSolverEitherReturnsNegativeAcrossNullShapes(t *testing.T) {
	const (
		m             = 8
		n             = 4
		falseFindRisk = 0.000001
		edge          = 0.1 // raised only to speed elimination; the bound is edge-free
	)
	noises := []struct {
		name string
		fn   func(*rand.Rand) float64
	}{
		{"gaussian", gaussianNoise(8)},
		{"one-sided", oneSidedNoise(8, 0.1, 25)}, // late-tailed contaminants
	}
	for noiseIndex, nz := range noises {
		for biasIndex, bs := range biasFamily() {
			seed := uint64(10*noiseIndex + biasIndex + 1)
			cmp := &syntheticComparator{
				n: n, bias: bs.fn, noise: nz.fn, offset: make([]float64, m),
				controls: UnboundedControls(),
				rng:      rand.New(rand.NewPCG(seed, 0x7373)), //nolint:gosec // Reproducible test data.
			}
			f := newBaselineReservedOutlierSolver(seed*13 + 3)
			res, err := f.Identify(context.Background(),
				OutlierRequest{
					M: m, Direction: Either, MinimumEdge: edge,
					FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01,
				}, cmp.comparator())
			if err != nil {
				t.Fatalf("noise=%s bias=%s: %v", nz.name, bs.name, err)
			}
			if res.Outcome != OutlierNegative {
				t.Errorf("noise=%s bias=%s: outcome=%v, want Negative",
					nz.name, bs.name, res.Outcome)
			}
			assertGuarantee(t, res.OutlierGuarantee, OutlierGuarantee{
				ErrorDirection: OutlierErrorFalseNegative, ErrorRiskLimit: 0.01,
				MinimumEdge: edge, Direction: Either,
				Premise: OutlierPremiseBaselineMean,
				Regime:  OutlierInferenceAnytimeValid,
			})
		}
	}
}

// --- Result.Items (per-item diagnostics) -------------------------------------

func TestBaselineReservedOutlierSolverResultItems(t *testing.T) {
	const (
		m             = 16
		n             = 4
		targetIdx     = 7
		falseFindRisk = 0.01
		signal        = 6.0
		noiseSD       = 8.0
	)
	offset := make([]float64, m)
	offset[targetIdx] = signal // a Late target
	cmp := &syntheticComparator{
		n: n, bias: linearBias(10), noise: gaussianNoise(noiseSD), offset: offset,
		controls:         UnboundedControls(),
		observedSamples:  make([]int, m),
		observedRankSums: make([]int, m),
		rng:              rand.New(rand.NewPCG(3, 4)), //nolint:gosec // math/rand in tests, not security-sensitive
	}
	f := newBaselineReservedOutlierSolver(11)
	res, err := f.Identify(context.Background(),
		OutlierRequest{
			M: m, Direction: Late,
			FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01,
		}, cmp.comparator())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutlierFound || res.Index != targetIdx {
		t.Fatalf("expected to find target %d, got found=%v index=%d", targetIdx, res.Outcome == OutlierFound, res.Index)
	}
	if len(res.Items) != m {
		t.Fatalf("len(Items) = %d, want %d", len(res.Items), m)
	}

	for i, it := range res.Items {
		if it.Samples != cmp.observedSamples[i] {
			t.Errorf("item %d: Samples = %d, want observed count %d",
				i, it.Samples, cmp.observedSamples[i])
		}
		if cmp.observedSamples[i] == 0 {
			if !math.IsNaN(it.MeanRank) {
				t.Errorf("item %d: MeanRank = %v, want NaN without samples", i, it.MeanRank)
			}
			continue
		}
		wantMean := float64(cmp.observedRankSums[i]) / float64(cmp.observedSamples[i])
		if it.MeanRank != wantMean {
			t.Errorf("item %d: MeanRank = %v, want observed mean %v", i, it.MeanRank, wantMean)
		}
	}

	// The committed item's calibrated evidence (impl detail) must have reached
	// the threshold.
	detail, ok := res.Detail.(OutlierDiagnostics)
	if !ok {
		t.Fatalf("Result.Detail %T, want OutlierDiagnostics", res.Detail)
	}
	if detail.MinimumEdge != OutlierDefaultMinimumEdge {
		t.Errorf("Detail.MinimumEdge = %v, want %v",
			detail.MinimumEdge, OutlierDefaultMinimumEdge)
	}
	if tau := stats.VilleUnionThreshold(m, falseFindRisk); detail.Items[targetIdx].LogEvidence < tau {
		t.Errorf("committed item LogEvidence %.3f < commit threshold %.3f", detail.Items[targetIdx].LogEvidence, tau)
	}
	if res.ErrorRiskLimit != falseFindRisk {
		t.Errorf("ErrorRiskLimit = %.17g, want requested risk %.17g",
			res.ErrorRiskLimit, falseFindRisk)
	}
	if detail.Items[targetIdx].Eliminated {
		t.Error("committed item marked Eliminated")
	}
}

// TestBaselineReservedOutlierSolverResultItemsOnNegative verifies that a
// negative retains
// the final evidence for every eliminated item.
func TestBaselineReservedOutlierSolverResultItemsOnNegative(t *testing.T) {
	const (
		m             = 8
		n             = 4
		falseFindRisk = 0.05
		edge          = 0.1
	)
	cmp := &syntheticComparator{
		n: n, bias: flatBias, noise: gaussianNoise(8), offset: make([]float64, m),
		rng: rand.New(rand.NewPCG(5, 6)), //nolint:gosec // math/rand in tests, not security-sensitive
	}
	f := newBaselineReservedOutlierSolver(7)
	res, err := f.Identify(context.Background(),
		OutlierRequest{
			M: m, Direction: Late, MinimumEdge: edge,
			FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01,
		}, cmp.comparator())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutlierNegative {
		t.Fatalf("Outcome = %v, want Negative", res.Outcome)
	}
	if len(res.Items) != m {
		t.Fatalf("len(Items) = %d, want %d", len(res.Items), m)
	}
	detail, ok := res.Detail.(OutlierDiagnostics)
	if !ok {
		t.Fatalf("Result.Detail %T, want OutlierDiagnostics", res.Detail)
	}
	tauElim := stats.VilleThreshold(0.01)
	for i, it := range detail.Items {
		if !it.Eliminated {
			t.Errorf("item %d not marked Eliminated on a full-elimination negative", i)
		}
		if it.EliminationLogE < tauElim {
			t.Errorf("item %d: elimination log-e %.3f below threshold %.3f",
				i, it.EliminationLogE, tauElim)
		}
	}
}

func TestBaselineReservedOutlierSolverReturnsFiniteEvidence(t *testing.T) {
	for _, direction := range []OutlierDirection{Late, Early} {
		cmp := NewComparator(Profile{
			MaxWidth: 2,
			Controls: map[ControlKind]ControlSupply{
				Baseline: UnboundedControls(),
			},
		}, func(_ context.Context, positioned []ItemID) ([]int, error) {
			ranks := make([]int, len(positioned))
			for position, item := range positioned {
				if item.IsCandidate() == (direction == Late) {
					ranks[position] = 1
				}
			}
			return ranks, nil
		})
		solver := newBaselineReservedOutlierSolver(19)
		solver.MaxComparisons = 40
		result, err := solver.Identify(context.Background(), OutlierRequest{
			M: 1, Direction: direction, MinimumEdge: 0.2,
			FalseFindRisk:     math.SmallestNonzeroFloat64,
			FalseNegativeRisk: math.SmallestNonzeroFloat64,
		}, cmp)
		if err != nil {
			t.Fatalf("%s: Identify: %v", direction, err)
		}
		detail := result.Detail.(OutlierDiagnostics)
		if len(detail.Items) != 1 {
			t.Fatalf("%s: diagnostics = %+v", direction, detail)
		}
		item := detail.Items[0]
		if math.IsNaN(item.LogEvidence) || math.IsInf(item.LogEvidence, 0) ||
			math.IsNaN(item.EliminationLogE) || math.IsInf(item.EliminationLogE, 0) {
			t.Errorf("%s: non-finite evidence: %+v", direction, item)
		}
	}
}

// TestBaselineReservedOutlierSolverFalseNegativeRiskSetsBudget verifies
// explicit negative error budgets.
func TestBaselineReservedOutlierSolverFalseNegativeRiskSetsBudget(t *testing.T) {
	const m, n, edge = 8, 4, 0.1
	run := func(risk float64) OutlierResult {
		cmp := &syntheticComparator{
			n: n, bias: flatBias, noise: gaussianNoise(8), offset: make([]float64, m),
			rng: rand.New(rand.NewPCG(5, 6)), //nolint:gosec // math/rand in tests, not security-sensitive
		}
		f := newBaselineReservedOutlierSolver(7)
		res, err := f.Identify(
			context.Background(),
			OutlierRequest{
				M: m, Direction: Late, MinimumEdge: edge,
				FalseFindRisk: 0.05, FalseNegativeRisk: risk,
			},
			cmp.comparator(),
		)
		if err != nil {
			t.Fatalf("FalseNegativeRisk %v: %v", risk, err)
		}
		if res.Outcome != OutlierNegative {
			t.Fatalf("FalseNegativeRisk %v: want a full-elimination negative, got %+v", risk, res)
		}
		return res
	}
	loose := run(0.5)
	strict := run(0.01)
	if loose.ErrorRiskLimit != 0.5 || strict.ErrorRiskLimit != 0.01 {
		t.Errorf("error-risk limits = %v and %v, want 0.5 and 0.01",
			loose.ErrorRiskLimit, strict.ErrorRiskLimit)
	}
	if strict.Comparisons <= loose.Comparisons {
		t.Errorf("strict false-negative risk cost %d <= loose cost %d",
			strict.Comparisons, loose.Comparisons)
	}
	looseDetail := loose.Detail.(OutlierDiagnostics)
	strictDetail := strict.Detail.(OutlierDiagnostics)
	if strictDetail.Threshold != looseDetail.Threshold {
		t.Errorf("positive thresholds differ: strict %v, loose %v",
			strictDetail.Threshold, looseDetail.Threshold)
	}
}

// TestOutlierProgressLeaders verifies sign-specific ordering, count limits, and
// non-mutation.
func TestOutlierProgressLeaders(t *testing.T) {
	p := OutlierProgressDiagnostics{Contenders: []OutlierItemRef{
		{Item: 2, Late: 2, Early: -1},
		{Item: 0, Late: 5, Early: -5},
		{Item: 1, Late: -4, Early: 4},
		{Item: 3, Late: 1, Early: 1},
	}}
	orig := slices.Clone(p.Contenders)

	// LogEvidence reads the per-sign value; Either takes the stronger lean.
	if got := (OutlierItemRef{Late: 5, Early: -5}).LogEvidence(Either); got != 5 {
		t.Errorf("LogEvidence(Either) = %v, want the stronger lean 5", got)
	}
	if got := (OutlierItemRef{Late: -4, Early: 4}).LogEvidence(Either); got != 4 {
		t.Errorf("LogEvidence(Either) = %v, want 4", got)
	}

	// The two orderings are genuinely different, not one reversed: item 0 leads
	// the late end, item 1 the early end.
	late := itemsOf(p.Leaders(Late, 3))
	if wantLate := []int{0, 2, 3}; !slices.Equal(late, wantLate) { // Late desc: 5, 2, 1
		t.Errorf("Leaders(Late, 3) items = %v, want %v", late, wantLate)
	}
	early := itemsOf(p.Leaders(Early, 3))
	if wantEarly := []int{1, 3, 2}; !slices.Equal(early, wantEarly) { // Early desc: 4, 1, -1
		t.Errorf("Leaders(Early, 3) items = %v, want %v", early, wantEarly)
	}
	// Each leader reports the log e-value for the requested extreme.
	if got := p.Leaders(Late, 1)[0].LogEvidence(Late); got != 5 {
		t.Errorf("late leader LogEvidence(Late) = %v, want 5", got)
	}

	// n is a ceiling; nil for a non-positive n or no contenders.
	if got := p.Leaders(Late, 100); len(got) != len(p.Contenders) {
		t.Errorf("Leaders(Late, 100) returned %d, want all %d", len(got), len(p.Contenders))
	}
	if p.Leaders(Late, 0) != nil || (OutlierProgressDiagnostics{}).Leaders(Late, 3) != nil {
		t.Error("Leaders should be nil for n <= 0 or no contenders")
	}
	if !slices.Equal(p.Contenders, orig) {
		t.Error("Leaders mutated the underlying Contenders")
	}
}

// itemsOf projects references to candidate indices.
func itemsOf(refs []OutlierItemRef) []int {
	out := make([]int, len(refs))
	for i, r := range refs {
		out[i] = r.Item
	}
	return out
}

// --- progress emission -------------------------------------------------------

func TestBaselineReservedOutlierSolverProgressReport(t *testing.T) {
	const (
		m             = 16
		n             = 4
		falseFindRisk = 0.01
	)
	offset := make([]float64, m)
	offset[2] = 6.0 // one Late target for this test
	cmp := &syntheticComparator{
		n: n, bias: linearBias(10), noise: gaussianNoise(8), offset: offset,
		controls: UnboundedControls(),
		rng:      rand.New(rand.NewPCG(1, 2)), //nolint:gosec // math/rand in tests, not security-sensitive
	}
	f := newBaselineReservedOutlierSolver(21)
	var reports []OutlierProgress
	req := OutlierRequest{
		M: m, Direction: Either, FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01,
		ProgressCadence: EveryComparisons(1),
		Progress:        func(r OutlierProgress) { reports = append(reports, r) },
	}
	res, err := f.Identify(context.Background(), req, cmp.comparator())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutlierFound || res.Index != 2 || res.Direction != Late {
		t.Fatalf("result = (%v, %d, %v), want (Found, 2, Late)", res.Outcome, res.Index, res.Direction)
	}
	if want := res.Comparisons + 1; len(reports) != want {
		t.Fatalf("got %d reports, want %d periodic reports plus final", len(reports), want)
	}
	wantTau := stats.VilleUnionThreshold(2*m, req.FalseFindRisk) // 2M hypotheses under Either
	for reportIndex, r := range reports {
		wantComparisons := reportIndex + 1
		if reportIndex == len(reports)-1 {
			wantComparisons = res.Comparisons
		}
		if r.Comparisons != wantComparisons {
			t.Errorf("report %d: Comparisons = %d, want %d", reportIndex, r.Comparisons, wantComparisons)
		}
		// Universal cost core: this solver fills every comparison to width n, so
		// participations track comparisons exactly.
		if r.Participations != r.Comparisons*n {
			t.Errorf("Participations %d, want Comparisons*n = %d", r.Participations, r.Comparisons*n)
		}
		// Class body: a leader within the population.
		if r.Active < 0 || r.Active > m {
			t.Errorf("Active %d out of range [0, %d]", r.Active, m)
		}
		if r.Leader < 0 || r.Leader >= m {
			t.Errorf("Leader %d out of range [0, %d)", r.Leader, m)
		}
		// Impl detail: the BaselineReservedOutlierSolver progress detail — every
		// contender,
		// each with its log e-value toward both extremes.
		d, ok := r.Detail.(OutlierProgressDiagnostics)
		if !ok {
			t.Fatalf("Detail %T, want OutlierProgressDiagnostics", r.Detail)
		}
		if d.Threshold != wantTau {
			t.Errorf("Threshold %.6f, want %.6f (2M union)", d.Threshold, wantTau)
		}
		if len(d.Contenders) != r.Active {
			t.Errorf("Contenders length %d, want Active %d", len(d.Contenders), r.Active)
		}
		seen := make([]bool, m)
		for _, it := range d.Contenders {
			if it.Item < 0 || it.Item >= m {
				t.Errorf("contender Item %d out of range [0, %d)", it.Item, m)
				continue
			}
			if seen[it.Item] {
				t.Errorf("contender Item %d appears more than once", it.Item)
			}
			seen[it.Item] = true
		}
	}
	final := reports[len(reports)-1]
	if final.Comparisons != res.Comparisons || final.Participations != res.Participations ||
		final.Leader != res.Index || final.Direction != res.Direction {
		t.Errorf("final progress = %+v, want terminal result state %+v", final, res)
	}
}

// TestBaselineReservedOutlierSolverProgressFinalAlways verifies that a zero
// cadence still
// produces the final snapshot on success.
func TestBaselineReservedOutlierSolverProgressFinalAlways(t *testing.T) {
	const (
		m             = 8
		n             = 4
		falseFindRisk = 0.05
	)
	offset := make([]float64, m)
	offset[3] = 6.0 // a Late target the solver will commit
	cmp := &syntheticComparator{
		n: n, bias: linearBias(10), noise: gaussianNoise(8), offset: offset,
		rng: rand.New(rand.NewPCG(3, 4)), //nolint:gosec // math/rand in tests, not security-sensitive
	}
	f := newBaselineReservedOutlierSolver(7)
	var reports []OutlierProgress
	// ProgressCadence deliberately left zero: no periodic snapshots.
	req := OutlierRequest{
		M: m, Direction: Late, FalseFindRisk: falseFindRisk, FalseNegativeRisk: 0.01,
		Progress: func(r OutlierProgress) { reports = append(reports, r) },
	}

	res, err := f.Identify(context.Background(), req, cmp.comparator())
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("no cadence: got %d reports, want exactly 1 (the final)", len(reports))
	}
	if res.Outcome != OutlierFound || res.Index != 3 || res.Direction != Late {
		t.Fatalf("result = (%v, %d, %v), want (Found, 3, Late)", res.Outcome, res.Index, res.Direction)
	}
	if reports[0].Comparisons != res.Comparisons {
		t.Errorf("final report Comparisons = %d, want %d (the returned result)", reports[0].Comparisons, res.Comparisons)
	}
	if reports[0].Participations != res.Participations {
		t.Errorf("final report Participations = %d, want %d (the returned result)",
			reports[0].Participations, res.Participations)
	}
	if reports[0].Leader != res.Index || reports[0].Direction != res.Direction {
		t.Errorf("final report state = (%d, %v), want result state (%d, %v)",
			reports[0].Leader, reports[0].Direction, res.Index, res.Direction)
	}
}
