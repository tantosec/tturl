package stats

import (
	"fmt"
	"math"
	"testing"
)

func TestPairwiseRecordPanicsSmallN(t *testing.T) {
	for _, n := range []int{-1, 0, 1} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewPairwiseRecord(%d) did not panic", n)
				}
			}()
			NewPairwiseRecord(n)
		}()
	}
}

func TestPairwiseRecordPanicsWhenStorageSizeOverflows(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	defer func() {
		if recover() == nil {
			t.Error("NewPairwiseRecord(maxInt) did not panic")
		}
	}()
	NewPairwiseRecord(maxInt)
}

func TestPairwiseRecordCounts(t *testing.T) {
	r := NewPairwiseRecord(4)
	r.Observe(0, 1)
	r.Observe(0, 1)
	r.Observe(1, 0)
	r.Observe(2, 3)

	wantWins := [4][4]int64{
		{0, 2, 0, 0},
		{1, 0, 0, 0},
		{0, 0, 0, 1},
		{0, 0, 0, 0},
	}
	if got := r.PopulationCount(); got != len(wantWins) {
		t.Errorf("PopulationCount = %d, want %d", got, len(wantWins))
	}
	for i := range 4 {
		for j := range 4 {
			if got := r.Wins(i, j); got != wantWins[i][j] {
				t.Errorf("Wins(%d,%d) = %d, want %d", i, j, got, wantWins[i][j])
			}
			wantTotal := wantWins[i][j] + wantWins[j][i]
			if got := r.Total(i, j); got != wantTotal {
				t.Errorf("Total(%d,%d) = %d, want %d", i, j, got, wantTotal)
			}
		}
	}
}

func TestPairwiseRecordTotalPanicsOnOverflow(t *testing.T) {
	record := NewPairwiseRecord(2)
	record.wins[record.index(0, 1)] = int64(^uint64(0) >> 1)
	record.wins[record.index(1, 0)] = 1
	defer func() {
		if recover() == nil {
			t.Error("Total did not panic on count overflow")
		}
	}()
	record.Total(0, 1)
}

func TestPairwiseRecordObservePanicsOnOutOfRange(t *testing.T) {
	// A negative index can fold into a valid linear offset (winner*n + loser
	// with e.g. loser = -1 lands one row earlier in the flat slice) and would
	// silently corrupt a different pair's counter without an explicit bounds
	// check.
	cases := []struct {
		name          string
		winner, loser int
	}{
		{"winner negative", -1, 0},
		{"loser negative", 0, -1},
		{"winner = n", 3, 0},
		{"loser = n", 0, 3},
		{"both out of range", 5, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := NewPairwiseRecord(3)
			defer func() {
				if rec := recover(); rec == nil {
					t.Errorf("Observe(%d, %d) did not panic", c.winner, c.loser)
				}
			}()
			r.Observe(c.winner, c.loser)
		})
	}
}

func TestPairwiseRecordAccessorsPanicOnOutOfRange(t *testing.T) {
	r := NewPairwiseRecord(3)
	accessors := []struct {
		name string
		call func(i, j int) int64
	}{
		{"Wins", r.Wins},
		{"Total", r.Total},
	}
	// Exercise both boundaries of both indices, including pairs such as (-1, n)
	// whose invalid coordinates cancel into a valid row-major offset.
	indices := []int{-1, 0, r.PopulationCount() - 1, r.PopulationCount()}
	for _, accessor := range accessors {
		for _, i := range indices {
			for _, j := range indices {
				if i >= 0 && i < r.PopulationCount() &&
					j >= 0 && j < r.PopulationCount() {
					continue
				}
				name := fmt.Sprintf("%s/%d/%d", accessor.name, i, j)
				t.Run(name, func(t *testing.T) {
					defer func() {
						if rec := recover(); rec == nil {
							t.Errorf("%s(%d, %d) did not panic", accessor.name, i, j)
						}
					}()
					accessor.call(i, j)
				})
			}
		}
	}
}

func TestPairwiseRecordObservePanicsOnSelfMatch(t *testing.T) {
	r := NewPairwiseRecord(3)
	defer func() {
		if rec := recover(); rec == nil {
			t.Error("Observe(2, 2) did not panic")
		}
	}()
	r.Observe(2, 2)
}

func TestRankComparisonWeightDegenerate(t *testing.T) {
	// opponents <= 0: no comparisons available, weight = 0.
	for _, opp := range []int{-100, -1, 0} {
		if got := RankComparisonWeight(opp); got != 0 {
			t.Errorf("RankComparisonWeight(%d) = %v, want 0", opp, got)
		}
		if got := RankEffectiveN(opp); got != 0 {
			t.Errorf("RankEffectiveN(%d) = %v, want 0", opp, got)
		}
	}
}

func TestRankComparisonWeightAtMaximumPopulation(t *testing.T) {
	opponents := int(^uint(0) >> 1)
	if weight := RankComparisonWeight(opponents); !(weight > 0) {
		t.Errorf("RankComparisonWeight(maxInt) = %v, want positive", weight)
	}
	if effectiveN := RankEffectiveN(opponents); effectiveN < 2.999999 || effectiveN > 3 {
		t.Errorf("RankEffectiveN(maxInt) = %v, want in [2.999999, 3]", effectiveN)
	}
}

func TestRankComparisonWeightExactSmallR(t *testing.T) {
	// w(r) = 3/(r+2); n_eff(r) = 3r/(r+2). Analytically derivable.
	cases := []struct {
		opp          int
		wantW, wantN float64
	}{
		{1, 1.0, 1.0},         // 3/3 = 1; 3·1/3 = 1
		{2, 0.75, 1.5},        // 3/4; 6/4
		{3, 0.6, 1.8},         // 3/5; 9/5
		{4, 0.5, 2.0},         // 3/6; 12/6
		{7, 1.0 / 3, 7.0 / 3}, // 3/9; 21/9
		{10, 0.25, 2.5},       // 3/12; 30/12
	}
	for _, c := range cases {
		if got := RankComparisonWeight(c.opp); !within(got, c.wantW, 1e-15) {
			t.Errorf("RankComparisonWeight(%d) = %.15g, want %.15g", c.opp, got, c.wantW)
		}
		if got := RankEffectiveN(c.opp); !within(got, c.wantN, 1e-14) {
			t.Errorf("RankEffectiveN(%d) = %.15g, want %.15g", c.opp, got, c.wantN)
		}
	}
}

func TestRankEffectiveNAsymptote(t *testing.T) {
	// n_eff(r) → 3 as r → ∞, monotonically from below.
	prev := RankEffectiveN(1)
	for opp := 2; opp <= 100000; opp *= 2 {
		cur := RankEffectiveN(opp)
		if cur <= prev {
			t.Errorf("opp=%d: n_eff did not increase (%.15g <= %.15g)", opp, cur, prev)
		}
		if cur >= 3 {
			t.Errorf("opp=%d: n_eff = %.15g, must remain < 3", opp, cur)
		}
		prev = cur
	}
	// At r=10000, the gap to 3 is
	// 3 - 3·10000/10002 = 6/10002 ≈ 6e-4.
	if 3-RankEffectiveN(10000) > 1e-3 {
		t.Errorf("n_eff(10000) = %.15g, expected within 1e-3 of 3", RankEffectiveN(10000))
	}
}

func TestRankComparisonWeightMonotoneDecreasing(t *testing.T) {
	// w(r) is strictly decreasing in r for r >= 1.
	prev := RankComparisonWeight(1)
	for opp := 2; opp <= 1000; opp++ {
		cur := RankComparisonWeight(opp)
		if cur >= prev {
			t.Errorf("opp=%d: weight did not decrease (%.15g >= %.15g)", opp, cur, prev)
		}
		prev = cur
	}
}

func TestRankLogLikK2(t *testing.T) {
	// For a 2-way comparison: rank 0 must equal log(winProb),
	// rank 1 must equal log(1−winProb). This follows directly from
	// the Plackett-Luce model with k=2.
	for _, q := range []float64{0.3, 0.5, 0.75, 0.9} {
		if got, want := RankLogLik(2, 0, q), math.Log(q); !within(got, want, 1e-14) {
			t.Errorf("RankLogLik(2,0,%.2f) = %.15g, want %.15g", q, got, want)
		}
		if got, want := RankLogLik(2, 1, q), math.Log(1-q); !within(got, want, 1e-14) {
			t.Errorf("RankLogLik(2,1,%.2f) = %.15g, want %.15g", q, got, want)
		}
	}
}

func TestRankLogLikNullIsUniform(t *testing.T) {
	// Under winProb=0.5 (s=1) all populations have equal strength, so every
	// specific permutation has probability 1/k!. RankLogLik(k, r, 0.5) must
	// equal −log(k!) for all r because the non-target ordering does not affect
	// the log-likelihood under equal strengths.
	for k := 2; k <= 6; k++ {
		ref := RankLogLik(k, 0, 0.5)
		for rank := 1; rank < k; rank++ {
			got := RankLogLik(k, rank, 0.5)
			if !within(got, ref, 1e-13) {
				t.Errorf("k=%d: RankLogLik(rank=%d, q=0.5)=%.15g != RankLogLik(rank=0)=%.15g",
					k, rank, got, ref)
			}
		}
		// Also verify the value itself: −log(k!).
		logKFact := 0.0
		for j := 2; j <= k; j++ {
			logKFact += math.Log(float64(j))
		}
		if !within(ref, -logKFact, 1e-13) {
			t.Errorf("k=%d: RankLogLik(0, q=0.5)=%.15g, want -log(%d!)=%.15g", k, ref, k, -logKFact)
		}
	}
}

func TestRankLogLikKnownValues(t *testing.T) {
	// Analytically derived under Plackett-Luce with winProb=0.75, s=3.
	//
	// k=3, r=0: P = s/(s+2) × 1/2 × 1 = 3/10
	// k=3, r=1: P = 1/(s+2) × s/(s+1) × 1 = 3/20
	// k=3, r=2: P = 1/(s+2) × 1/(s+1) × 1 = 1/20
	cases := []struct {
		rank int
		want float64
	}{
		{0, math.Log(3.0 / 10)},
		{1, math.Log(3.0 / 20)},
		{2, math.Log(1.0 / 20)},
	}
	for _, c := range cases {
		got := RankLogLik(3, c.rank, 0.75)
		if !within(got, c.want, 1e-14) {
			t.Errorf("RankLogLik(3,%d,0.75) = %.15g, want %.15g", c.rank, got, c.want)
		}
	}
	// For k=4, winProb=0.75 (s=3), rank 0:
	//   P = 3/(3+3) × 1/3 × 1/2 × 1 = 1/12
	want0 := math.Log(1.0 / 12)
	got0 := RankLogLik(4, 0, 0.75)
	if !within(got0, want0, 1e-14) {
		t.Errorf("RankLogLik(4,0,0.75) = %.15g, want %.15g", got0, want0)
	}
}

func TestRankLogLikOutOfRange(t *testing.T) {
	for _, rank := range []int{-1, -100, 2, 3, 100} {
		if got := RankLogLik(2, rank, 0.75); !math.IsInf(got, -1) {
			t.Errorf("RankLogLik(2,%d,0.75) = %v, want -Inf", rank, got)
		}
	}
	// k=1 is degenerate but not out-of-range for rank=0.
	if got := RankLogLik(1, 0, 0.75); got != 0 {
		t.Errorf("RankLogLik(1,0,0.75) = %v, want 0 (degenerate single-item ranking)", got)
	}
	for _, winProb := range []float64{0, 1, -0.1, 1.1, math.NaN()} {
		if got := RankLogLik(2, 0, winProb); !math.IsNaN(got) {
			t.Errorf("RankLogLik(2,0,%v) = %v, want NaN", winProb, got)
		}
	}
}

func TestRankLogLikSumsToOne(t *testing.T) {
	// For each rank r in [0, k−1] there are (k−1)! permutations of k items
	// with the target at rank r. Since every permutation has the same logLik
	// for a given r, the total probability is:
	//
	//   Σ_{r=0}^{k−1} (k−1)! × exp(RankLogLik(k, r, q)) = 1
	//
	// This verifies that RankLogLik correctly parameterises a probability
	// distribution over permutations.
	for _, q := range []float64{0.5, 0.75, 0.9} {
		for k := 2; k <= 6; k++ {
			factorial := 1
			for i := 2; i <= k-1; i++ {
				factorial *= i
			}
			var total float64
			for rank := range k {
				total += float64(factorial) * math.Exp(RankLogLik(k, rank, q))
			}
			if !within(total, 1, 1e-12) {
				t.Errorf("q=%.2f, k=%d: sum (k-1)!*P = %.15g, want 1", q, k, total)
			}
		}
	}
}

func TestRankLogLikMonotoneInWinProb(t *testing.T) {
	// Higher winProb must give a higher log-likelihood when the target finishes
	// first (rank 0) and a lower one when it finishes last (rank k−1).
	for k := 2; k <= 5; k++ {
		prev0 := RankLogLik(k, 0, 0.5)
		prevLast := RankLogLik(k, k-1, 0.5)
		for _, q := range []float64{0.6, 0.7, 0.8, 0.9} {
			cur0 := RankLogLik(k, 0, q)
			curLast := RankLogLik(k, k-1, q)
			if cur0 <= prev0 {
				t.Errorf("k=%d, q=%.1f: rank-0 logLik did not increase with winProb", k, q)
			}
			if curLast >= prevLast {
				t.Errorf("k=%d, q=%.1f: last-rank logLik did not decrease with winProb", k, q)
			}
			prev0 = cur0
			prevLast = curLast
		}
	}
}

func TestRankPosteriorPanics(t *testing.T) {
	for _, n := range []int{-1, 0, 1} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewRankPosterior(%d, 0.75) did not panic", n)
				}
			}()
			NewRankPosterior(n, 0.75)
		}()
	}
	for _, wp := range []float64{0, 1, -0.1, 1.1, math.NaN()} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewRankPosterior(2, %v) did not panic", wp)
				}
			}()
			NewRankPosterior(2, wp)
		}()
	}
}

func TestRankPosteriorUniformPrior(t *testing.T) {
	// No observations → posterior equals the uniform prior: 1/n for each.
	for _, n := range []int{2, 3, 5, 10} {
		p := NewRankPosterior(n, 0.75)
		probs := p.Probabilities()
		want := 1.0 / float64(n)
		for i, prob := range probs {
			if !within(prob, want, 1e-15) {
				t.Errorf("n=%d: probs[%d] = %.15g, want %.15g", n, i, prob, want)
			}
		}
		if p.PopulationCount() != n {
			t.Errorf("PopulationCount = %d, want %d", p.PopulationCount(), n)
		}
		if got := p.MaxPopulation(); got != 0 {
			t.Errorf("uniform-prior MaxPopulation = %d, want first index 0", got)
		}
	}
}

func TestRankPosteriorProbabilitiesNormalise(t *testing.T) {
	// After any sequence of updates, probabilities must sum to 1.
	p := NewRankPosterior(4, 0.75)
	p.ObservePairwise(0, 1)
	p.ObservePairwise(2, 3)
	p.ObserveRanking([]int{0, 2, 1, 3})
	p.ObserveRanking([]int{3, 0, 2, 1})
	var sum float64
	for _, prob := range p.Probabilities() {
		sum += prob
	}
	if !within(sum, 1, 1e-14) {
		t.Errorf("probabilities sum to %.15g, want 1", sum)
	}

	first := p.Probabilities()
	first[0] = -1
	second := p.Probabilities()
	if second[0] < 0 {
		t.Errorf("Probabilities reused mutable result storage: %v", second)
	}
}

func TestRankPosteriorPairwiseExact(t *testing.T) {
	cases := []struct {
		name         string
		n            int
		winProb      float64
		observations int
		want         []float64
	}{
		{"two populations one observation", 2, 0.75, 1, []float64{0.75, 0.25}},
		{"two populations two observations", 2, 0.75, 2, []float64{0.9, 0.1}},
		{"three populations", 3, 0.75, 1, []float64{0.5, 1.0 / 6, 1.0 / 3}},
		{
			"four populations two observations", 4, 0.8, 2,
			[]float64{0.64 / 1.18, 0.04 / 1.18, 0.25 / 1.18, 0.25 / 1.18},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewRankPosterior(c.n, c.winProb)
			for range c.observations {
				p.ObservePairwise(0, 1)
			}
			probs := p.Probabilities()
			for i, want := range c.want {
				if !within(probs[i], want, 1e-14) {
					t.Errorf("probs[%d] = %.15g, want %.15g", i, probs[i], want)
				}
			}
			if p.MaxPopulation() != 0 {
				t.Errorf("MaxPopulation = %d, want 0", p.MaxPopulation())
			}
		})
	}
}

func TestRankPosteriorPairwisePanicsOnSelfMatch(t *testing.T) {
	// A population cannot beat itself; ObservePairwise must reject winner==loser
	// rather than silently applying only the winner update (Go's switch is
	// non-fallthrough, so the loser arm would otherwise be skipped).
	p := NewRankPosterior(3, 0.75)
	defer func() {
		if r := recover(); r == nil {
			t.Error("ObservePairwise(2, 2) did not panic")
		}
	}()
	p.ObservePairwise(2, 2)
}

func TestRankPosteriorPairwisePanicsOnOutOfRange(t *testing.T) {
	// Indices outside [0, n) must panic. Without an explicit bounds check the
	// non-existent index would simply not match any iteration of the update
	// loop, silently crediting the missing participant's update to the
	// non-participant pool — a hard-to-spot logic bug.
	cases := []struct {
		name          string
		winner, loser int
	}{
		{"winner negative", -1, 0},
		{"loser negative", 0, -1},
		{"winner = n", 3, 0},
		{"loser = n", 0, 3},
		{"both out of range", 5, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewRankPosterior(3, 0.75)
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("ObservePairwise(%d, %d) did not panic", c.winner, c.loser)
				}
			}()
			p.ObservePairwise(c.winner, c.loser)
		})
	}
}

func TestRankPosteriorRankingPanicsOnOutOfRange(t *testing.T) {
	// Out-of-range indices in obs must panic with a typed message rather than
	// falling through to a raw "index out of range" from the runtime.
	cases := []struct {
		name string
		obs  []int
	}{
		{"first negative", []int{-1, 0}},
		{"middle negative", []int{0, -1, 2}},
		{"first = n", []int{3, 0}},
		{"last = n", []int{0, 1, 3}},
		{"both out of range", []int{5, 7}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewRankPosterior(3, 0.75)
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("ObserveRanking(%v) did not panic", c.obs)
				}
			}()
			p.ObserveRanking(c.obs)
		})
	}
}

func TestRankPosteriorRankingPanicsOnDuplicate(t *testing.T) {
	for _, observation := range [][]int{{0, 0}, {0, 1, 0}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("ObserveRanking(%v) did not panic", observation)
				}
			}()
			NewRankPosterior(3, 0.75).ObserveRanking(observation)
		}()
	}
}

func TestRankPosteriorRankingEmpty(t *testing.T) {
	// Empty observation is a no-op — posterior stays at the uniform prior.
	p := NewRankPosterior(3, 0.75)
	p.ObserveRanking([]int{})
	probs := p.Probabilities()
	for i, prob := range probs {
		if !within(prob, 1.0/3, 1e-15) {
			t.Errorf("probs[%d] = %.15g, want 1/3", i, prob)
		}
	}
}

func TestRankPosteriorRankingExact(t *testing.T) {
	// n=3, winProb=0.75 (s=3), observe ranking [0, 1, 2].
	//
	// From TestRankLogLikKnownValues:
	//   P(obs | H1(0)) = 3/10   (t at rank 0)
	//   P(obs | H1(1)) = 3/20   (t at rank 1)
	//   P(obs | H1(2)) = 1/20   (t at rank 2)
	//
	// Normalised posterior (uniform prior cancels):
	//   P(0) = (3/10) / (1/2) = 0.6
	//   P(1) = (3/20) / (1/2) = 0.3
	//   P(2) = (1/20) / (1/2) = 0.1
	for _, ranking := range [][]int{{0, 1, 2}, {2, 1, 0}} {
		p := NewRankPosterior(3, 0.75)
		p.ObserveRanking(ranking)
		probs := p.Probabilities()
		want := []float64{0.1, 0.3, 0.1}
		want[ranking[0]] = 0.6
		for i, probability := range probs {
			if !within(probability, want[i], 1e-14) {
				t.Errorf("ranking %v: probs[%d] = %.15g, want %.15g",
					ranking, i, probability, want[i])
			}
		}
		if p.MaxPopulation() != ranking[0] {
			t.Errorf("ranking %v: MaxPopulation = %d, want %d",
				ranking, p.MaxPopulation(), ranking[0])
		}
	}
}

func TestRankPosteriorRankingMatchesPairwiseSubset(t *testing.T) {
	// When obs covers k < n populations, ObserveRanking must apply the full
	// Plackett-Luce update — including the implicit −log(k!) for non-
	// participants — so that observing [winner, loser] as a 2-way ranking
	// yields the same posterior as ObservePairwise(winner, loser).
	for _, n := range []int{2, 3, 4, 5} {
		for _, wp := range []float64{0.6, 0.75, 0.9} {
			pPair := NewRankPosterior(n, wp)
			pPair.ObservePairwise(0, 1)

			pRank := NewRankPosterior(n, wp)
			pRank.ObserveRanking([]int{0, 1})

			prP := pPair.Probabilities()
			prR := pRank.Probabilities()
			for i := range prP {
				if !within(prP[i], prR[i], 1e-14) {
					t.Errorf("n=%d, wp=%.2f: probs[%d] ObservePairwise=%.15g, ObserveRanking=%.15g",
						n, wp, i, prP[i], prR[i])
				}
			}
		}
	}
}

func TestRankPosteriorConvergesWithRepeatedEvidence(t *testing.T) {
	// Repeatedly observing population 0 finish first in a 4-way ranking must
	// drive P(0) toward 1. After enough observations the probability should
	// exceed 0.99.
	p := NewRankPosterior(4, 0.75)
	for range 50 {
		p.ObserveRanking([]int{0, 1, 2, 3})
	}
	probs := p.Probabilities()
	if probs[0] < 0.99 {
		t.Errorf("probs[0] = %.6f after 50 consistent rankings, want > 0.99", probs[0])
	}
}

func TestRankPosteriorMixedUpdates(t *testing.T) {
	// Bayesian evidence multiplies, so mixed updates commute in log space.
	forward := NewRankPosterior(5, 0.7)
	forward.ObserveRanking([]int{2, 0, 4, 1, 3})
	forward.ObservePairwise(2, 3)
	forward.ObserveRanking([]int{2, 1, 0, 4, 3})
	forward.ObservePairwise(2, 1)

	reverse := NewRankPosterior(5, 0.7)
	reverse.ObservePairwise(2, 1)
	reverse.ObserveRanking([]int{2, 1, 0, 4, 3})
	reverse.ObservePairwise(2, 3)
	reverse.ObserveRanking([]int{2, 0, 4, 1, 3})

	got, want := forward.Probabilities(), reverse.Probabilities()
	for i := range got {
		if !within(got[i], want[i], 1e-14) {
			t.Errorf("probability %d: forward = %.15g, reverse = %.15g", i, got[i], want[i])
		}
	}
}
