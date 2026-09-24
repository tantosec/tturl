package main

import "math"

type analyseRandomiser struct {
	state uint64
}

type analyseUint64Source interface {
	next() uint64
}

func (r *analyseRandomiser) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *analyseRandomiser) shuffle(values []int) {
	for index := len(values) - 1; index > 0; index-- {
		//nolint:gosec // The sampled value is bounded by the slice index.
		swap := int(analyseUniformBelow(r, uint64(index+1)))
		values[index], values[swap] = values[swap], values[index]
	}
}

func analyseUniformBelow(source analyseUint64Source, bound uint64) uint64 {
	threshold := -bound % bound
	for {
		value := source.next()
		if value >= threshold {
			return value % bound
		}
	}
}

type analyseRankWorkspace struct {
	rankSums []int
}

func newAnalyseRankWorkspace(width int) *analyseRankWorkspace {
	return &analyseRankWorkspace{rankSums: make([]int, width)}
}

func (workspace *analyseRankWorkspace) statistic(
	evidence analyseRankEvidence, assignment []int,
) (float64, analyseComponents) {
	clear(workspace.rankSums)
	for cycle := range evidence.cycles {
		for shift := range evidence.width {
			for position := range evidence.width {
				request := int(assignment[cycle*evidence.width+
					(position+shift)%evidence.width])
				rank := int(evidence.ranks[(cycle*evidence.width+shift)*evidence.width+position])
				workspace.rankSums[request] += rank
			}
		}
	}
	trials := evidence.cycles * evidence.width
	centre := float64(trials*(evidence.width-1)) / 2
	var sumSquares, maximum float64
	for _, sum := range workspace.rankSums {
		difference := float64(sum) - centre
		sumSquares += difference * difference
		maximum = math.Max(maximum, math.Abs(difference)/float64(trials))
	}
	l2 := math.Sqrt(12 * sumSquares /
		float64(trials*trials*evidence.width*(evidence.width*evidence.width-1)))
	linfinity := maximum / (float64(evidence.width-1) / 2)
	components := analyseComponents{L2: l2, LInfinity: linfinity}
	return math.Max(l2, linfinity/math.Sqrt2), components
}

func analyseGlobalDecision(
	evidence analyseRankEvidence, seed uint64,
) analyseGlobalResult {
	workspace := newAnalyseRankWorkspace(evidence.width)
	observed, components := workspace.statistic(evidence, evidence.base)
	space, exact := analyseBoundedOrbitSize(
		evidence.width, evidence.cycles, analyseMonteCarloDraws,
	)
	if exact {
		return analyseExactGlobal(evidence, observed, components, space)
	}

	return analyseSampledGlobal(evidence, observed, components, seed)
}

func analyseSampledGlobal(
	evidence analyseRankEvidence, observed float64,
	components analyseComponents, seed uint64,
) analyseGlobalResult {
	workspace := newAnalyseRankWorkspace(evidence.width)
	rng := analyseRandomiser{state: seed}
	assignment := make([]int, len(evidence.base))
	extremes := 0
	for range analyseMonteCarloDraws {
		analyseRandomAssignment(evidence, &rng, assignment)
		statistic, _ := workspace.statistic(evidence, assignment)
		if statistic >= observed-1e-12 {
			extremes++
		}
	}
	p := analyseMonteCarloP(extremes, analyseMonteCarloDraws)
	outcome := analyseNoDetection
	if p <= analyseAlpha {
		outcome = analyseDifference
	}
	return analyseGlobalResult{
		Outcome: outcome, Alpha: analyseAlpha, Statistic: observed,
		Components: components,
		Calibration: analyseCalibration{
			Method: "monte_carlo_randomisation", Draws: analyseMonteCarloDraws,
			Extremes: extremes, SeedUsed: true, RandomisationSeed: seed,
			PValue: p, PValueKind: "monte_carlo",
			CompletionReason: "fixed_draw_budget_complete",
		},
	}
}

func analyseMonteCarloP(extremes, draws int) float64 {
	return float64(extremes+1) / float64(draws+1)
}

func analyseExactGlobal(
	evidence analyseRankEvidence, observed float64,
	components analyseComponents, space int,
) analyseGlobalResult {
	workspace := newAnalyseRankWorkspace(evidence.width)
	permutations := analysePermutations(evidence.width)
	assignment := make([]int, len(evidence.base))
	extreme := 0
	for code := range space {
		value := code
		for cycle := range evidence.cycles {
			permutation := permutations[value%len(permutations)]
			value /= len(permutations)
			copy(assignment[cycle*evidence.width:], permutation)
		}
		statistic, _ := workspace.statistic(evidence, assignment)
		if statistic >= observed-1e-12 {
			extreme++
		}
	}
	p := float64(extreme) / float64(space)
	outcome := analyseNoDetection
	if p <= analyseAlpha {
		outcome = analyseDifference
	}
	return analyseGlobalResult{
		Outcome: outcome, Alpha: analyseAlpha, Statistic: observed,
		Components: components,
		Calibration: analyseCalibration{
			Method: "exact_randomisation", Draws: space, Extremes: extreme,
			PValue: p, PValueKind: "exact",
			CompletionReason: "exact_orbit_complete",
		},
	}
}

func analyseRandomAssignment(
	evidence analyseRankEvidence, rng *analyseRandomiser, assignment []int,
) {
	permutation := make([]int, evidence.width)
	for cycle := range evidence.cycles {
		for index := range permutation {
			permutation[index] = index
		}
		rng.shuffle(permutation)
		for index, request := range permutation {
			assignment[cycle*evidence.width+index] = request
		}
	}
}

func analyseBoundedOrbitSize(width, cycles, limit int) (int, bool) {
	perCycle := 1
	for factor := 2; factor <= width; factor++ {
		if perCycle > limit/factor {
			return 0, false
		}
		perCycle *= factor
	}
	space := 1
	for range cycles {
		if space > limit/perCycle {
			return 0, false
		}
		space *= perCycle
	}
	return space, true
}

func analysePermutations(width int) [][]int {
	base := make([]int, width)
	for index := range base {
		base[index] = index
	}
	var result [][]int
	var visit func(int)
	visit = func(index int) {
		if index == width {
			result = append(result, append([]int(nil), base...))
			return
		}
		for swap := index; swap < width; swap++ {
			base[index], base[swap] = base[swap], base[index]
			visit(index + 1)
			base[index], base[swap] = base[swap], base[index]
		}
	}
	visit(0)
	return result
}
