package ranking

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

var evaluationPanelNames = []string{
	"smoke", "core-comparison", "robustness", "request-policy", "concurrency",
}

func evaluationScenarioCatalogue() []evaluationScenario {
	const (
		falseFindRisk     = 0.01
		falseNegativeRisk = 0.10
		minimumEdge       = 0.10
	)
	gaussian := func(
		name string, panels []string, population int, target bool,
		sign OutlierDirection, effect float64, width int, bias evaluationBiasKind,
		biasMagnitude float64,
	) evaluationScenario {
		return evaluationScenario{
			Name: name, Panels: panels, Population: population,
			RequestDirection:  sign,
			FalseFindRisk:     falseFindRisk,
			FalseNegativeRisk: falseNegativeRisk,
			MinimumEdge:       minimumEdge, MaxComparisons: 8_192,
			Width: width, MaxConcurrency: 1, UnboundedControls: true,
			Channel: evaluationChannelSpec{
				Kind: evaluationGaussian, Noise: evaluationHomoskedastic,
				NoiseScale: 8, Effect: effect,
				Bias: bias, BiasMagnitude: biasMagnitude,
			},
			Targets: func() []evaluationTargetSpec {
				if target {
					return evaluationTargets(sign)
				}
				return nil
			}(),
		}
	}
	gaussianEffectForEdge := func(edge, noiseScale float64) float64 {
		return 2 * noiseScale * math.Erfinv(2*edge)
	}

	scenarios := []evaluationScenario{
		{
			Name:       "endpoint/m1/late/null/w4/p1",
			Panels:     []string{"smoke"},
			Population: 1, RequestDirection: Late,
			FalseFindRisk: 0.10, FalseNegativeRisk: 0.10,
			MinimumEdge: minimumEdge, MaxComparisons: 10_000,
			Width: 4, MaxConcurrency: 1, UnboundedControls: true,
			Channel: evaluationChannelSpec{
				Kind: evaluationEndpoint, EndpointEdge: minimumEdge,
			},
		},
		{
			Name:       "endpoint/m1/late/boundary/w4/p1",
			Panels:     []string{"smoke"},
			Population: 1, RequestDirection: Late, Targets: evaluationTargets(Late),
			FalseFindRisk: 0.10, FalseNegativeRisk: 0.10,
			MinimumEdge: minimumEdge, MaxComparisons: 10_000,
			Width: 4, MaxConcurrency: 1, UnboundedControls: true,
			Channel: evaluationChannelSpec{
				Kind: evaluationEndpoint, EndpointEdge: minimumEdge,
			},
		},
		{
			Name:       "endpoint/m4/late/null/w4/p1",
			Population: 4, RequestDirection: Late,
			FalseFindRisk: 0.10, FalseNegativeRisk: 0.10,
			MinimumEdge: minimumEdge, MaxComparisons: 20_000,
			Width: 4, MaxConcurrency: 1, UnboundedControls: true,
			Channel: evaluationChannelSpec{
				Kind: evaluationEndpoint, EndpointEdge: minimumEdge,
			},
		},
		{
			Name:       "endpoint/m4/late/boundary/w4/p1",
			Population: 4, RequestDirection: Late, Targets: evaluationTargets(Late),
			FalseFindRisk: 0.10, FalseNegativeRisk: 0.10,
			MinimumEdge: minimumEdge, MaxComparisons: 20_000,
			Width: 4, MaxConcurrency: 1, UnboundedControls: true,
			Channel: evaluationChannelSpec{
				Kind: evaluationEndpoint, EndpointEdge: minimumEdge,
			},
		},
		{
			Name:       "endpoint/m4/either/null/w4/p1",
			Population: 4, RequestDirection: Either,
			FalseFindRisk: 0.10, FalseNegativeRisk: 0.10,
			MinimumEdge: minimumEdge, MaxComparisons: 30_000,
			Width: 4, MaxConcurrency: 1, UnboundedControls: true,
			Channel: evaluationChannelSpec{
				Kind: evaluationEndpoint, EndpointEdge: minimumEdge,
			},
		},
		{
			Name:       "endpoint/m4/either/boundary/w4/p1",
			Population: 4, RequestDirection: Either, Targets: evaluationTargets(Either),
			FalseFindRisk: 0.10, FalseNegativeRisk: 0.10,
			MinimumEdge: minimumEdge, MaxComparisons: 30_000,
			Width: 4, MaxConcurrency: 1, UnboundedControls: true,
			Channel: evaluationChannelSpec{
				Kind: evaluationEndpoint, EndpointEdge: minimumEdge,
			},
		},
		gaussian("gaussian/m16/late/strong/flat/w8/p1",
			[]string{"smoke", "core-comparison"}, 16, true, Late, 6, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m16/late/boundary/flat/w8/p1",
			[]string{"core-comparison"}, 16, true, Late, 3, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m16/late/null/flat/w8/p1",
			[]string{"core-comparison"}, 16, false, Late, 0, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/strong/flat/w8/p1",
			[]string{"smoke", "core-comparison"}, 99, true, Late, 6, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/boundary/flat/w8/p1",
			[]string{"smoke", "core-comparison"}, 99, true, Late, 3, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/subedge/flat/w8/p1",
			[]string{"smoke", "core-comparison"}, 99, true, Late, 2, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/null/flat/w8/p1",
			[]string{"smoke", "core-comparison"}, 99, false, Late, 0, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m256/late/strong/flat/w8/p1",
			[]string{"smoke", "core-comparison"}, 256, true, Late, 6, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m256/late/boundary/flat/w8/p1",
			[]string{"core-comparison"}, 256, true, Late, 3, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m256/late/null/flat/w8/p1",
			[]string{"core-comparison"}, 256, false, Late, 0, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/boundary/reversed/w8/p1",
			[]string{"smoke", "robustness"}, 99, true, Late, 3, 8,
			evaluationReversedBias, 10),
		gaussian("gaussian/m99/early/strong/flat/w8/p1",
			[]string{"robustness"}, 99, true, Early, 6, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/early/boundary/flat/w8/p1",
			[]string{"robustness"}, 99, true, Early, 3, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/early/null/flat/w8/p1",
			[]string{"robustness"}, 99, false, Early, 0, 8,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/strong/flat/w2/p1",
			[]string{"robustness"}, 99, true, Late, 6, 2,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/boundary/flat/w2/p1",
			[]string{"robustness"}, 99, true, Late, 3, 2,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/null/flat/w2/p1",
			[]string{"robustness"}, 99, false, Late, 0, 2,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/strong/flat/w4/p1",
			[]string{"robustness"}, 99, true, Late, 6, 4,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/boundary/flat/w4/p1",
			[]string{"robustness"}, 99, true, Late, 3, 4,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/null/flat/w4/p1",
			[]string{"robustness"}, 99, false, Late, 0, 4,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/strong/flat/w16/p1",
			[]string{"robustness"}, 99, true, Late, 6, 16,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/boundary/flat/w16/p1",
			[]string{"robustness"}, 99, true, Late, 3, 16,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/null/flat/w16/p1",
			[]string{"robustness"}, 99, false, Late, 0, 16,
			evaluationFlatBias, 0),
		gaussian("gaussian/m99/late/null/stepped/w8/p1",
			[]string{"robustness"}, 99, false, Late, 0, 8,
			evaluationSteppedBias, 10),
		gaussian("gaussian/m99/late/boundary/stepped/w8/p1",
			[]string{"robustness"}, 99, true, Late, 3, 8,
			evaluationSteppedBias, 10),
		gaussian("gaussian/m99/late/null/reversed/w8/p1",
			[]string{"robustness"}, 99, false, Late, 0, 8,
			evaluationReversedBias, 10),
	}

	weakM256 := gaussian("gaussian/m256/late/null/edge005/flat/w8/p1",
		[]string{"request-policy"}, 256, false, Late,
		gaussianEffectForEdge(0.05, 8), 8, evaluationFlatBias, 0)
	weakM256.MinimumEdge = 0.05
	weakM256.MaxComparisons = 32_768
	scenarios = append(scenarios, weakM256)
	weakM256Target := weakM256
	weakM256Target.Name = "gaussian/m256/late/target/edge005/flat/w8/p1"
	weakM256Target.Targets = evaluationTargets(Late)
	scenarios = append(scenarios, weakM256Target)

	either := gaussian("gaussian/m99/either/strong/flat/w8/p1",
		[]string{"robustness"}, 99, true, Either, 6, 8,
		evaluationFlatBias, 0)
	scenarios = append(scenarios, either)
	eitherBoundary := gaussian("gaussian/m99/either/boundary/flat/w8/p1",
		[]string{"robustness"}, 99, true, Either, 3, 8,
		evaluationFlatBias, 0)
	scenarios = append(scenarios, eitherBoundary)
	eitherNull := gaussian("gaussian/m99/either/null/flat/w8/p1",
		[]string{"robustness"}, 99, false, Either, 0, 8,
		evaluationFlatBias, 0)
	scenarios = append(scenarios, eitherNull)

	peerOnly := gaussian("gaussian/m99/late/strong/peer-only/w8/p1",
		[]string{"robustness"}, 99, true, Late, 6, 8,
		evaluationFlatBias, 0)
	peerOnly.UnboundedControls = false
	scenarios = append(scenarios, peerOnly)
	peerBoundary := gaussian("gaussian/m99/late/boundary/peer-only/w8/p1",
		[]string{"robustness"}, 99, true, Late, 3, 8,
		evaluationFlatBias, 0)
	peerBoundary.UnboundedControls = false
	scenarios = append(scenarios, peerBoundary)
	peerNull := gaussian("gaussian/m99/late/null/peer-only/w8/p1",
		[]string{"robustness"}, 99, false, Late, 0, 8,
		evaluationFlatBias, 0)
	peerNull.UnboundedControls = false
	scenarios = append(scenarios, peerNull)

	bounded := gaussian("gaussian/m99/late/strong/control1/w8/p1",
		[]string{"robustness"}, 99, true, Late, 6, 8,
		evaluationFlatBias, 0)
	bounded.UnboundedControls = false
	bounded.BaselineControls = 1
	scenarios = append(scenarios, bounded)
	boundedBoundary := gaussian("gaussian/m99/late/boundary/control1/w8/p1",
		[]string{"robustness"}, 99, true, Late, 3, 8,
		evaluationFlatBias, 0)
	boundedBoundary.UnboundedControls = false
	boundedBoundary.BaselineControls = 1
	scenarios = append(scenarios, boundedBoundary)
	boundedNull := gaussian("gaussian/m99/late/null/control1/w8/p1",
		[]string{"robustness"}, 99, false, Late, 0, 8,
		evaluationFlatBias, 0)
	boundedNull.UnboundedControls = false
	boundedNull.BaselineControls = 1
	scenarios = append(scenarios, boundedNull)

	positiveBoundary := gaussian(
		"gaussian/m99/late/boundary/positive1/w8/p1",
		[]string{"robustness"}, 99, true, Late, 3, 8,
		evaluationFlatBias, 0)
	positiveBoundary.UnboundedControls = false
	positiveBoundary.PositiveControls = 1
	scenarios = append(scenarios, positiveBoundary)
	positiveNull := gaussian("gaussian/m99/late/null/positive1/w8/p1",
		[]string{"robustness"}, 99, false, Late, 0, 8,
		evaluationFlatBias, 0)
	positiveNull.UnboundedControls = false
	positiveNull.PositiveControls = 1
	scenarios = append(scenarios, positiveNull)

	biasFreeBoundary := gaussian(
		"gaussian/m99/late/boundary/bias-free/w8/p1",
		[]string{"robustness"}, 99, true, Late, 3, 8,
		evaluationFlatBias, 0)
	scenarios = append(scenarios, biasFreeBoundary)
	biasFreeNull := gaussian("gaussian/m99/late/null/bias-free/w8/p1",
		[]string{"robustness"}, 99, false, Late, 0, 8,
		evaluationFlatBias, 0)
	scenarios = append(scenarios, biasFreeNull)

	hetero := gaussian("gaussian/m99/late/null/heteroskedastic/w8/p1",
		[]string{"robustness"}, 99, false, Late, 0, 8,
		evaluationFlatBias, 0)
	hetero.Channel.Noise = evaluationHeteroskedastic
	scenarios = append(scenarios, hetero)
	heteroBoundary := gaussian(
		"gaussian/m99/late/boundary/heteroskedastic/w8/p1",
		[]string{"robustness"}, 99, true, Late, 3, 8,
		evaluationFlatBias, 0)
	heteroBoundary.Channel.Noise = evaluationHeteroskedastic
	scenarios = append(scenarios, heteroBoundary)

	oneSidedTarget := gaussian(
		"gaussian/m99/late/strong/one-sided/w8/p1",
		[]string{"robustness"}, 99, true, Late, 6, 8,
		evaluationFlatBias, 0)
	oneSidedTarget.Channel.Noise = evaluationOneSided
	oneSidedTarget.Channel.SpikeRate = 0.05
	oneSidedTarget.Channel.SpikeScale = 32
	scenarios = append(scenarios, oneSidedTarget)
	oneSidedNull := gaussian("gaussian/m99/late/null/one-sided/w8/p1",
		[]string{"robustness"}, 99, false, Late, 0, 8,
		evaluationFlatBias, 0)
	oneSidedNull.Channel.Noise = evaluationOneSided
	oneSidedNull.Channel.SpikeRate = 0.05
	oneSidedNull.Channel.SpikeScale = 32
	scenarios = append(scenarios, oneSidedNull)

	for _, edge := range []float64{0.02, 0.05} {
		edgeNull := scenarios[0]
		edgeNull.Name = fmt.Sprintf("endpoint/m1/late/null/edge%.2f/w4/p1", edge)
		edgeNull.Panels = []string{"request-policy"}
		edgeNull.MinimumEdge = edge
		edgeNull.Channel.EndpointEdge = edge
		edgeNull.MaxComparisons = 50_000
		scenarios = append(scenarios, edgeNull)
	}

	for _, risk := range []float64{0.10, 0.001} {
		positive := gaussian(
			fmt.Sprintf("gaussian/m99/late/boundary/find-risk%.3f/w8/p1", risk),
			[]string{"request-policy"}, 99, true, Late, 3, 8, evaluationFlatBias, 0,
		)
		positive.FalseFindRisk = risk
		scenarios = append(scenarios, positive)

		negative := gaussian(
			fmt.Sprintf("gaussian/m99/late/null/negative-risk%.3f/w8/p1", risk),
			[]string{"request-policy"}, 99, false, Late, 0, 8, evaluationFlatBias, 0,
		)
		negative.FalseNegativeRisk = risk
		negative.MaxComparisons = 30_000
		scenarios = append(scenarios, negative)
	}

	clone := func(name string) evaluationScenario {
		for _, scenario := range scenarios {
			if scenario.Name == name {
				return scenario
			}
		}
		panic("missing evaluation scenario: " + name)
	}

	parallelTarget := gaussian("gaussian/m99/late/strong/flat/w8/p4",
		[]string{"concurrency"}, 99, true, Late, 6, 8,
		evaluationFlatBias, 0)
	parallelTarget.MaxConcurrency = 4
	scenarios = append(scenarios, parallelTarget)
	parallelNull := gaussian("gaussian/m99/late/null/flat/w8/p4",
		[]string{"concurrency"}, 99, false, Late, 0, 8,
		evaluationFlatBias, 0)
	parallelNull.MaxConcurrency = 4
	scenarios = append(scenarios, parallelNull)
	parallelEndpoint := clone("endpoint/m1/late/boundary/w4/p1")
	parallelEndpoint.Name = "endpoint/m1/late/boundary/w4/p4"
	parallelEndpoint.Panels = []string{"concurrency"}
	parallelEndpoint.MaxConcurrency = 4
	parallelEndpoint.MaxComparisons = 40_000
	scenarios = append(scenarios, parallelEndpoint)
	scenarios = append(scenarios, evaluationRegimeScenarios()...)
	scenarios = append(scenarios, evaluationPeerHeterogeneousScenarios()...)

	lateBiasedNull := gaussian(
		"gaussian/m8/late/null/stepped/w4/p1",
		nil, 8, false, Late, 0, 4,
		evaluationSteppedBias, 10,
	)
	lateBiasedNull.FalseFindRisk = 0.05
	scenarios = append(scenarios, lateBiasedNull)

	earlyHeteroskedasticNull := gaussian(
		"gaussian/m8/early/null/heteroskedastic-reversed/w4/p1",
		nil, 8, false, Early, 0, 4,
		evaluationReversedBias, 10,
	)
	earlyHeteroskedasticNull.FalseFindRisk = 0.05
	earlyHeteroskedasticNull.Channel.Noise = evaluationHeteroskedastic
	scenarios = append(scenarios, earlyHeteroskedasticNull)

	eitherContaminatedNull := gaussian(
		"gaussian/m8/either/null/one-sided-linear-control1/w4/p1",
		nil, 8, false, Either, 0, 4,
		evaluationLinearBias, 10,
	)
	eitherContaminatedNull.FalseFindRisk = 0.05
	eitherContaminatedNull.Channel.Noise = evaluationOneSided
	eitherContaminatedNull.Channel.SpikeRate = 0.10
	eitherContaminatedNull.Channel.SpikeScale = 25
	eitherContaminatedNull.UnboundedControls = false
	eitherContaminatedNull.BaselineControls = 1
	scenarios = append(scenarios, eitherContaminatedNull)

	lateBiasedTarget := gaussian(
		"gaussian/m16/late/strong/stepped/w4/p1",
		nil, 16, true, Late, 6, 4,
		evaluationSteppedBias, 10,
	)
	scenarios = append(scenarios, lateBiasedTarget)

	eitherContaminatedTarget := gaussian(
		"gaussian/m16/either/strong/one-sided-reversed-control1/w4/p1",
		nil, 16, true, Either, 6, 4,
		evaluationReversedBias, 10,
	)
	eitherContaminatedTarget.Channel.Noise = evaluationOneSided
	eitherContaminatedTarget.Channel.SpikeRate = 0.10
	eitherContaminatedTarget.Channel.SpikeScale = 25
	eitherContaminatedTarget.UnboundedControls = false
	eitherContaminatedTarget.BaselineControls = 1
	scenarios = append(scenarios, eitherContaminatedTarget)

	parallelNullQualification := clone("endpoint/m4/late/null/w4/p1")
	parallelNullQualification.Name = "endpoint/m4/late/null/w4/p4"
	parallelNullQualification.MaxConcurrency = 4
	parallelNullQualification.MaxComparisons *= 4
	scenarios = append(scenarios, parallelNullQualification)

	parallelBoundaryQualification := clone("endpoint/m4/late/boundary/w4/p1")
	parallelBoundaryQualification.Name = "endpoint/m4/late/boundary/w4/p4"
	parallelBoundaryQualification.MaxConcurrency = 4
	parallelBoundaryQualification.MaxComparisons *= 4
	scenarios = append(scenarios, parallelBoundaryQualification)

	exhaustion := clone("endpoint/m4/late/null/w4/p1")
	exhaustion.Name = "endpoint/m4/late/null/exhausted/w4/p4"
	exhaustion.FalseFindRisk = 0.000001
	exhaustion.FalseNegativeRisk = 0.000001
	exhaustion.MaxComparisons = 4
	exhaustion.MaxConcurrency = 4
	scenarios = append(scenarios, exhaustion)

	multi := func(
		name string, panels []string, request OutlierDirection, targets []evaluationTargetSpec,
		controls int, premise string,
	) evaluationScenario {
		scenario := gaussian(name, panels, 8, false, request, 3, 4,
			evaluationFlatBias, 0)
		scenario.Targets = targets
		scenario.PremiseNote = premise
		scenario.UnboundedControls = controls < 0
		scenario.BaselineControls = max(0, controls)
		scenario.MaxComparisons = 30_000
		return scenario
	}
	baselinePremise := "Baseline-relative means hold for every membership"
	sameSignQualification := multi(
		"gaussian/m8/either/multi2-late-boundary/baseline/w4/p1",
		[]string{"robustness"}, Either,
		evaluationTargets(Late, Late), -1, baselinePremise,
	)
	scenarios = append(scenarios, sameSignQualification)
	opposedQualification := multi(
		"gaussian/m8/either/multi2-opposed-boundary/baseline/w4/p1",
		[]string{"robustness"}, Either,
		evaluationTargets(Late, Early), -1, baselinePremise,
	)
	scenarios = append(scenarios, opposedQualification)
	mixed := []evaluationTargetSpec{
		{Direction: Late, Scale: 2},
		{Direction: Late, Scale: 1},
		{Direction: Late, Scale: 0.5},
	}
	scenarios = append(scenarios, multi(
		"gaussian/m8/either/multi3-mixed/baseline/w4/p1",
		[]string{"robustness"}, Either, mixed, -1, baselinePremise,
	))
	boundedPremise := "Baseline observations are isolated; peer observations may mask"
	for _, count := range []int{2, 3, 4} {
		targets := make([]evaluationTargetSpec, count)
		for i := range targets {
			targets[i] = evaluationTargetSpec{Direction: Late, Scale: 1}
		}
		scenarios = append(scenarios, multi(
			fmt.Sprintf("gaussian/m8/late/multi%d-boundary/control1/w4/p1", count),
			[]string{"robustness"}, Late, targets, 1, boundedPremise,
		))
	}
	peerPremise := "Peer-relative target means are not established; masking study only"
	for _, count := range []int{2, 4, 5} {
		targets := make([]evaluationTargetSpec, count)
		for i := range targets {
			targets[i] = evaluationTargetSpec{Direction: Late, Scale: 1}
		}
		scenarios = append(scenarios, multi(
			fmt.Sprintf("gaussian/m8/late/multi%d-boundary/peer-only/w4/p1", count),
			[]string{"robustness"}, Late, targets, 0, peerPremise,
		))
	}
	scenarios = append(scenarios,
		multi("gaussian/m8/either/null/multi-baseline-match/w4/p1",
			[]string{"robustness"}, Either, nil, -1, baselinePremise),
		multi("gaussian/m8/late/null/multi-control1-match/w4/p1",
			[]string{"robustness"}, Late, nil, 1, boundedPremise),
		multi("gaussian/m8/late/null/multi-peer-match/w4/p1",
			[]string{"robustness"}, Late, nil, 0, peerPremise),
	)

	return scenarios
}

func validateEvaluationScenarioCatalogue(scenarios []evaluationScenario) error {
	if len(scenarios) == 0 {
		return fmt.Errorf("scenario catalogue is empty")
	}
	seen := make(map[string]bool, len(scenarios))
	knownPanels := make(map[string]bool, len(evaluationPanelNames))
	for _, panel := range evaluationPanelNames {
		knownPanels[panel] = true
	}
	for _, scenario := range scenarios {
		if seen[scenario.Name] {
			return fmt.Errorf("duplicate scenario name %q", scenario.Name)
		}
		seen[scenario.Name] = true
		if err := scenario.validate(); err != nil {
			return err
		}
		for _, panel := range scenario.Panels {
			if !knownPanels[panel] {
				return fmt.Errorf("scenario %q has unknown panel %q", scenario.Name, panel)
			}
		}
	}
	return nil
}

func TestEvaluationWeakM256Pair(t *testing.T) {
	null := evaluationTestScenario(
		t, "gaussian/m256/late/null/edge005/flat/w8/p1")
	target := evaluationTestScenario(
		t, "gaussian/m256/late/target/edge005/flat/w8/p1")
	normalisedTarget := target
	normalisedTarget.Name = null.Name
	normalisedTarget.Targets = null.Targets
	if !reflect.DeepEqual(null, normalisedTarget) {
		t.Fatalf("weak target/null pair differs beyond truth: null=%+v target=%+v",
			null, target)
	}
	if edge := target.effectiveEdge(target.Targets[0]); math.Abs(edge-0.05) > 1e-12 {
		t.Errorf("effective edge = %.17g, want 0.05", edge)
	}
	nullTruth, targetTruth := null.truth(0), target.truth(0)
	if !null.decisionScored() || !target.decisionScored() ||
		len(nullTruth.Targets) != 0 || len(targetTruth.Targets) != 1 ||
		!targetTruth.Targets[0].Qualifies {
		t.Errorf("weak pair scoring = null:%+v target:%+v", nullTruth, targetTruth)
	}
}

func TestEvaluationMultipleTargetTruthIsDistinctAndStable(t *testing.T) {
	scenario := evaluationTestScenario(
		t, "gaussian/m8/either/multi2-opposed-boundary/baseline/w4/p1")
	first := scenario.truth(91)
	second := scenario.truth(91)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("truth changed for one seed: %+v / %+v", first, second)
	}
	if len(first.Targets) != 2 || first.Targets[0].Index >= first.Targets[1].Index {
		t.Fatalf("targets are not distinct and index ordered: %+v", first.Targets)
	}
	signs := map[OutlierDirection]bool{}
	for _, target := range first.Targets {
		signs[target.Direction] = true
	}
	if !signs[Late] || !signs[Early] {
		t.Fatalf("opposing target signs = %v", signs)
	}
}

func TestEvaluationBaselineRanksIgnoreAdditionalTargets(t *testing.T) {
	multiple := evaluationTestScenario(
		t, "gaussian/m8/either/multi2-late-boundary/baseline/w4/p1")
	single := multiple
	single.Targets = single.Targets[:1]
	const seed = uint64(101)
	multipleTruth := multiple.truth(seed)
	singleTruth := single.truth(seed)
	first := singleTruth.Targets[0].Index
	if _, ok := multipleTruth.target(first); !ok {
		t.Fatalf("first target %d changed when another was added", first)
	}
	second := -1
	for _, target := range multipleTruth.Targets {
		if target.Index != first {
			second = target.Index
		}
	}
	items := []ItemID{
		ItemID(first), Control(Baseline, 0), Control(Baseline, 1), ItemID(second),
	}
	controlRank := func(ranks []int) int {
		count := 0
		for _, position := range []int{1, 2} {
			if ranks[position] < ranks[0] {
				count++
			}
		}
		return count
	}
	for occurrence := range uint64(100) {
		layoutSeed := evaluationMix(seed ^ occurrence)
		singleChannel := evaluationChannel{scenario: single, truth: singleTruth, seed: seed}
		multipleChannel := evaluationChannel{
			scenario: multiple, truth: multipleTruth, seed: seed,
		}
		gotSingle := controlRank(singleChannel.gaussianRanks(
			items, layoutSeed, occurrence))
		gotMultiple := controlRank(multipleChannel.gaussianRanks(
			items, layoutSeed, occurrence))
		if gotSingle != gotMultiple {
			t.Fatalf("occurrence %d control subrank = %d with one target, %d with two",
				occurrence, gotSingle, gotMultiple)
		}
	}
}

func TestEvaluationMultipleTargetScenariosHaveMatchedNulls(t *testing.T) {
	baselineNull := "gaussian/m8/either/null/multi-baseline-match/w4/p1"
	boundedNull := "gaussian/m8/late/null/multi-control1-match/w4/p1"
	peerNull := "gaussian/m8/late/null/multi-peer-match/w4/p1"
	pairs := map[string]string{
		"gaussian/m8/either/multi2-late-boundary/baseline/w4/p1":    baselineNull,
		"gaussian/m8/either/multi2-opposed-boundary/baseline/w4/p1": baselineNull,
		"gaussian/m8/either/multi3-mixed/baseline/w4/p1":            baselineNull,
		"gaussian/m8/late/multi2-boundary/control1/w4/p1":           boundedNull,
		"gaussian/m8/late/multi3-boundary/control1/w4/p1":           boundedNull,
		"gaussian/m8/late/multi4-boundary/control1/w4/p1":           boundedNull,
		"gaussian/m8/late/multi2-boundary/peer-only/w4/p1":          peerNull,
		"gaussian/m8/late/multi4-boundary/peer-only/w4/p1":          peerNull,
		"gaussian/m8/late/multi5-boundary/peer-only/w4/p1":          peerNull,
	}
	for targetName, nullName := range pairs {
		target := evaluationTestScenario(t, targetName)
		null := evaluationTestScenario(t, nullName)
		target.Name = null.Name
		target.Panels = null.Panels
		target.Targets = nil
		if !reflect.DeepEqual(target, null) {
			t.Errorf("%s differs from matched null %s beyond truth", targetName, nullName)
		}
	}
}
