package stats

import "math"

// PairwiseRecord counts wins and losses for each pair in a fixed population.
// Construct one with [NewPairwiseRecord]. Methods that accept indices panic
// when an index is outside [0, PopulationCount()). Do not copy a
// PairwiseRecord: copies share mutable count storage. Its zero value is
// invalid, and its methods panic when called on one.
type PairwiseRecord struct {
	n    int
	wins []int64 // flat row-major n×n: wins[i*n+j] = times i beat j
}

// NewPairwiseRecord returns a record for populationCount populations indexed
// from zero. It panics if populationCount is less than two or too large to
// represent its pairwise storage.
func NewPairwiseRecord(populationCount int) *PairwiseRecord {
	if populationCount < 2 {
		panic("stats: NewPairwiseRecord: populationCount must be at least 2")
	}
	if populationCount > int(^uint(0)>>1)/populationCount {
		panic("stats: NewPairwiseRecord: population is too large")
	}
	return &PairwiseRecord{
		n:    populationCount,
		wins: make([]int64, populationCount*populationCount),
	}
}

// Observe records one comparison in which winner beat loser.
//
// Observe panics if winner equals loser.
func (r *PairwiseRecord) Observe(winner, loser int) {
	idx := r.index(winner, loser)
	if winner == loser {
		panic("stats: PairwiseRecord.Observe: winner and loser must differ")
	}
	r.wins[idx] = incrementCount(r.wins[idx], "PairwiseRecord.Observe")
}

// Wins returns the number of times population i beat population j.
func (r *PairwiseRecord) Wins(i, j int) int64 { return r.wins[r.index(i, j)] }

// Total returns the total number of comparisons between populations i and j.
// It panics if the combined count would overflow int64.
func (r *PairwiseRecord) Total(i, j int) int64 {
	idx := r.index(i, j)
	wins, losses := r.wins[idx], r.wins[j*r.n+i]
	if wins > int64(^uint64(0)>>1)-losses {
		panic("stats: PairwiseRecord.Total: comparison count overflow")
	}
	return wins + losses
}

// PopulationCount returns the number of populations tracked.
func (r *PairwiseRecord) PopulationCount() int {
	r.checkInitialised()
	return r.n
}

// index validates a pair of population indices and returns its row-major
// offset. Validating before flattening prevents two invalid indices from
// cancelling into an in-range offset.
func (r *PairwiseRecord) index(i, j int) int {
	r.checkInitialised()
	if i < 0 || i >= r.n || j < 0 || j >= r.n {
		panic("stats: PairwiseRecord: index out of range")
	}
	return i*r.n + j
}

func (r *PairwiseRecord) checkInitialised() {
	if r.n < 2 || len(r.wins) != r.n*r.n {
		panic("stats: PairwiseRecord: zero or invalid value")
	}
}

// RankComparisonWeight returns the per-comparison evidence weight to apply to
// pairwise pseudo-observations derived from one rank among opponents+1
// exchangeable populations. It returns 3/(opponents+2), or zero when opponents
// is non-positive.
//
// Under exchangeability, the number W of opponents beaten is discrete uniform
// on {0,...,opponents}. Comparing Var(W/opponents) with the independent
// Bernoulli(1/2) variance gives design effect (opponents+2)/3 and its
// reciprocal weight. This construction assumes exchangeability but no
// parametric law.
func RankComparisonWeight(opponents int) float64 {
	if opponents <= 0 {
		return 0
	}
	return 3.0 / (float64(opponents) + 2)
}

// RankEffectiveN returns the effective independent sample size,
// 3·opponents/(opponents+2), contributed by one rank observation of a focal
// population among opponents+1 exchangeable populations. It equals opponents
// times [RankComparisonWeight] and returns zero for non-positive opponents.
//
// See [RankComparisonWeight] for the derivation. It approaches three as the
// number of opponents increases.
func RankEffectiveN(opponents int) float64 {
	if opponents <= 0 {
		return 0
	}
	return 3.0 * float64(opponents) / (float64(opponents) + 2)
}

// RankLogLik returns the log-likelihood of one specific full permutation
// in which a target population finishes at rank (0 = first) among
// populationCount populations,
// under the Plackett-Luce model where the target has pairwise win probability
// winProb against each of the populationCount−1 equivalent non-target
// populations.
//
// Non-targets have unit strength and the target has strength
// winProb/(1-winProb). Every ordering of the non-target populations has the
// same likelihood. To obtain the marginal probability of the target rank,
// multiply exp(RankLogLik(...)) by (populationCount-1)!.
//
// It returns NaN if winProb is outside (0, 1), and -Inf for rank outside
// [0, populationCount-1]. An invalid rank takes precedence over an invalid
// winProb.
//
// A non-positive populationCount returns -Inf. RankLogLik takes
// O(populationCount) time.
func RankLogLik(populationCount, rank int, winProb float64) float64 {
	if rank < 0 || rank >= populationCount {
		return math.Inf(-1)
	}
	if !(winProb > 0 && winProb < 1) {
		return math.NaN()
	}
	s := winProb / (1 - winProb)
	tail := populationCount - rank - 1

	// log P = log(s)
	//         − log(tail!)                             [positions after target]
	//         − Σ_{j=tail}^{populationCount−1} log(s+j)
	//                                                   [target and earlier]
	logLik := logFloat(s)
	for j := 2; j <= tail; j++ {
		logLik -= logFloat(float64(j))
	}
	for j := tail; j <= populationCount-1; j++ {
		logLik -= logFloat(s + float64(j))
	}
	return logLik
}

// RankPosterior maintains a log-posterior over which of n populations is the
// distinguished one, under the Plackett-Luce model where that population
// has pairwise win probability winProb against each of the n−1 equivalent
// other populations. The prior is uniform over all n populations.
//
// Full rankings and pairwise observations may be mixed, but each update must
// represent new evidence. Do not also submit comparisons derived from an
// already observed ranking. A winProb below one-half models a population that
// tends to lose. Construct a posterior with [NewRankPosterior]. Do not copy a
// RankPosterior: copies share mutable posterior storage. Its zero value is
// invalid, and its methods panic when called on one.
type RankPosterior struct {
	logWeights []float64 // unnormalised log-posterior; uniform prior = all zeros
	winProb    float64
	logWin     float64 // cached log(winProb)
	logLoss    float64 // cached log(1 - winProb)
}

// NewRankPosterior returns a posterior for populationCount populations and the
// distinguished population's pairwise win probability. It panics if
// populationCount is less than two or winProb is outside (0, 1).
func NewRankPosterior(populationCount int, winProb float64) *RankPosterior {
	if populationCount < 2 {
		panic("stats: NewRankPosterior: populationCount must be at least 2")
	}
	if !(winProb > 0 && winProb < 1) {
		panic("stats: NewRankPosterior: winProb must be in (0, 1)")
	}
	return &RankPosterior{
		logWeights: make([]float64, populationCount),
		winProb:    winProb,
		logWin:     logFloat(winProb),
		logLoss:    math.Log1p(-winProb),
	}
}

// ObserveRanking incorporates a ranking of distinct population indices. An
// empty observation is a no-op. It panics for an out-of-range or duplicate
// index. The slice is neither modified nor retained. A partial ranking
// compares only its listed populations, rather than treating omitted ones as
// ranked last.
//
// ObserveRanking takes O(k) time regardless of the total population size.
func (p *RankPosterior) ObserveRanking(obs []int) {
	p.checkInitialised()
	k := len(obs)
	if k == 0 {
		return
	}
	n := len(p.logWeights)
	seen := make(map[int]struct{}, k)
	for _, idx := range obs {
		if idx < 0 || idx >= n {
			panic("stats: RankPosterior.ObserveRanking: index out of range")
		}
		if _, duplicate := seen[idx]; duplicate {
			panic("stats: RankPosterior.ObserveRanking: duplicate index")
		}
		seen[idx] = struct{}{}
	}
	s := p.winProb / (1 - p.winProb)
	// log(k!): added to every participant update so non-participants
	// implicitly receive −log(k!) without an O(n) sweep.
	logKFact, _ := math.Lgamma(float64(k + 1))
	logLik := RankLogLik(k, 0, p.winProb) + logKFact
	p.logWeights[obs[0]] += logLik
	for rank := 1; rank < k; rank++ {
		logLik += logFloat(float64(k-rank)) - logFloat(s+float64(k-rank-1))
		p.logWeights[obs[rank]] += logLik //nolint:gosec // obs indices validated against len(logWeights) above
	}
}

// ObservePairwise incorporates a 2-way comparison in which winner beat loser.
//
// ObservePairwise panics if winner equals loser or either index is outside
// [0, PopulationCount()). It takes constant time.
func (p *RankPosterior) ObservePairwise(winner, loser int) {
	p.checkInitialised()
	n := len(p.logWeights)
	if winner < 0 || winner >= n || loser < 0 || loser >= n {
		panic("stats: RankPosterior.ObservePairwise: index out of range")
	}
	if winner == loser {
		panic("stats: RankPosterior.ObservePairwise: winner and loser must differ")
	}
	p.logWeights[winner] += p.logWin + math.Ln2
	p.logWeights[loser] += p.logLoss + math.Ln2
}

// Probabilities returns a newly allocated slice containing the normalised
// posterior probability of each population in index order.
func (p *RankPosterior) Probabilities() []float64 {
	p.checkInitialised()
	n := len(p.logWeights)
	probs := make([]float64, n)

	// Numerically stable softmax: subtract the maximum log-weight before
	// exponentiating to prevent overflow.
	maxLW := p.logWeights[0]
	for _, lw := range p.logWeights[1:] {
		if lw > maxLW {
			maxLW = lw
		}
	}

	var sum float64
	for i, lw := range p.logWeights {
		probs[i] = math.Exp(lw - maxLW)
		sum += probs[i]
	}
	for i := range probs {
		probs[i] /= sum
	}
	return probs
}

// MaxPopulation returns the index of the population with the highest posterior
// probability (equivalently, the highest unnormalised log-weight). Ties are
// broken by returning the smallest index.
func (p *RankPosterior) MaxPopulation() int {
	p.checkInitialised()
	best := 0
	for i := 1; i < len(p.logWeights); i++ {
		if p.logWeights[i] > p.logWeights[best] {
			best = i
		}
	}
	return best
}

// PopulationCount returns the number of populations tracked.
func (p *RankPosterior) PopulationCount() int {
	p.checkInitialised()
	return len(p.logWeights)
}

func (p *RankPosterior) checkInitialised() {
	if len(p.logWeights) < 2 {
		panic("stats: RankPosterior: zero or invalid value")
	}
}
