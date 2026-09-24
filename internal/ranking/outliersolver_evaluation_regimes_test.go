package ranking

import (
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"testing"
)

const (
	evaluationBurstPhaseSalt   = uint64(0x4255525354504841)
	evaluationMixturePhaseSalt = uint64(0x4d49585450484153)
)

// evaluationRegimeSpec describes latent-utility channels whose one-comparison
// marginal can be simple while observations remain dependent or nonstationary.
// Every random component is keyed by the environment seed and call number, so
// paired solvers face the same law without sharing their solver RNG.
type evaluationRegimeSpec struct {
	BaseNoiseScale  float64 `json:"base_noise_scale"`
	Effect          float64 `json:"effect,omitempty"`
	DriftMagnitude  float64 `json:"drift_magnitude,omitempty"`
	DriftPeriod     int     `json:"drift_period,omitempty"`
	BurstNoiseScale float64 `json:"burst_noise_scale,omitempty"`
	BurstLength     int     `json:"burst_length,omitempty"`
	CommonModeScale float64 `json:"common_mode_scale,omitempty"`
	Quantisation    float64 `json:"quantisation,omitempty"`
	MixtureRate     float64 `json:"mixture_rate,omitempty"`
	MixtureEffect   float64 `json:"mixture_effect,omitempty"`
	MixtureNoise    float64 `json:"mixture_noise,omitempty"`
	MixtureLength   int     `json:"mixture_length,omitempty"`
}

func (r *evaluationRegimeSpec) validate() error {
	if r == nil {
		return fmt.Errorf("spec is missing")
	}
	finite := func(values ...float64) bool {
		for _, value := range values {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return false
			}
		}
		return true
	}
	switch {
	case !finite(
		r.BaseNoiseScale, r.Effect, r.DriftMagnitude, r.BurstNoiseScale,
		r.CommonModeScale, r.Quantisation, r.MixtureRate, r.MixtureEffect,
		r.MixtureNoise,
	):
		return fmt.Errorf("all numeric parameters must be finite")
	case r.BaseNoiseScale < 0:
		return fmt.Errorf("base noise must be non-negative")
	case r.Effect < 0:
		return fmt.Errorf("effect must be non-negative")
	case r.DriftMagnitude < 0:
		return fmt.Errorf("drift magnitude must be non-negative")
	case (r.DriftMagnitude == 0) != (r.DriftPeriod == 0):
		return fmt.Errorf("drift magnitude and period must both be set")
	case r.DriftPeriod != 0 && r.DriftPeriod < 2:
		return fmt.Errorf("drift period must be at least 2")
	case r.BurstNoiseScale < 0:
		return fmt.Errorf("burst noise must be non-negative")
	case (r.BurstNoiseScale == 0) != (r.BurstLength == 0):
		return fmt.Errorf("burst noise and length must both be set")
	case r.BurstLength != 0 && r.BurstLength < 2:
		return fmt.Errorf("burst length must be at least 2")
	case r.CommonModeScale < 0:
		return fmt.Errorf("common-mode scale must be non-negative")
	case r.Quantisation < 0:
		return fmt.Errorf("quantisation must be non-negative")
	case r.MixtureRate < 0 || r.MixtureRate >= 1:
		return fmt.Errorf("mixture rate must be in [0, 1)")
	case r.MixtureRate > 0 && (r.MixtureNoise < 0 || r.MixtureEffect < 0 ||
		r.MixtureLength < 1):
		return fmt.Errorf("mixture parameters are invalid")
	case r.MixtureRate == 0 && (r.MixtureNoise != 0 || r.MixtureEffect != 0 ||
		r.MixtureLength != 0):
		return fmt.Errorf("mixture parameters require a positive rate")
	case r.BaseNoiseScale == 0 && r.BurstNoiseScale == 0 &&
		r.Quantisation == 0:
		return fmt.Errorf("at least one item-varying component is required")
	}
	return nil
}

func TestEvaluationRegimeSpecRejectsInvalidNumericParameters(t *testing.T) {
	valid := evaluationRegimeSpec{BaseNoiseScale: 1}
	tests := []struct {
		name   string
		mutate func(*evaluationRegimeSpec)
	}{
		{"negative drift period", func(r *evaluationRegimeSpec) {
			r.DriftMagnitude, r.DriftPeriod = 1, -2
		}},
		{"negative burst length", func(r *evaluationRegimeSpec) {
			r.BurstNoiseScale, r.BurstLength = 1, -2
		}},
		{"nan", func(r *evaluationRegimeSpec) { r.Effect = math.NaN() }},
		{"positive infinity", func(r *evaluationRegimeSpec) {
			r.MixtureRate = math.Inf(1)
		}},
		{"negative infinity", func(r *evaluationRegimeSpec) {
			r.Quantisation = math.Inf(-1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := valid
			test.mutate(&spec)
			if err := spec.validate(); err == nil {
				t.Fatal("validation succeeded")
			}
		})
	}
}

type evaluationRegimeScore struct {
	position int
	score    float64
	tie      uint64
}

func (c *evaluationChannel) regimeRanks(items []ItemID, call uint64) []int {
	scores := make([]evaluationRegimeScore, len(items))
	for position, item := range items {
		scores[position] = c.regimeScore(item, position, len(items), call)
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			if scores[i].tie == scores[j].tie {
				return scores[i].position < scores[j].position
			}
			return scores[i].tie < scores[j].tie
		}
		return scores[i].score < scores[j].score
	})
	ranks := make([]int, len(items))
	for rank, scored := range scores {
		ranks[scored.position] = rank
	}
	return ranks
}

func (c *evaluationChannel) regimeScore(
	item ItemID, position, width int, call uint64,
) evaluationRegimeScore {
	r := c.scenario.Channel.Regime
	noiseScale, effect := r.BaseNoiseScale, r.Effect
	if c.regimeMixture(call) {
		noiseScale, effect = r.MixtureNoise, r.MixtureEffect
	}
	itemBits := evaluationItemBits(item)
	score := noiseScale * c.regimeNormal(call, itemBits, 0x42415345)
	if r.BurstLength > 0 {
		score += r.BurstNoiseScale * c.regimeBurstNoise(call, itemBits)
	}
	if r.CommonModeScale > 0 {
		score += r.CommonModeScale * c.regimeNormal(call, 0, 0x434f4d4d4f4e)
	}
	if r.DriftPeriod > 0 {
		period := uint64(r.DriftPeriod)
		phase := evaluationMix(c.seed^0x4452494654) % period
		t := float64((call+phase)%period) / float64(period)
		x := 2*float64(position)/float64(width-1) - 1
		score += r.DriftMagnitude * math.Sin(2*math.Pi*t) * x
	}
	positive := item.IsControl() && item.ControlKind() == Positive
	target, isTarget := c.truth.target(int(item))
	if (item.IsCandidate() && isTarget) || positive {
		direction := target.Direction
		if positive {
			direction = c.scenario.RequestDirection
		}
		if direction == Early {
			score -= effect
		} else {
			score += effect
		}
	}
	if r.Quantisation > 0 {
		score = math.Round(score/r.Quantisation) * r.Quantisation
	}
	return evaluationRegimeScore{
		position: position,
		score:    score,
		tie:      c.regimeKey(call, itemBits, 0x544945),
	}
}

func (c *evaluationChannel) regimeBurstNoise(call, item uint64) float64 {
	length := uint64(c.scenario.Channel.Regime.BurstLength) //nolint:gosec // Validated.
	block := c.regimeBlock(call, length, evaluationBurstPhaseSalt)
	return c.regimeNormal(block, item, 0x4255525354)
}

func (c *evaluationChannel) regimeMixture(call uint64) bool {
	r := c.scenario.Channel.Regime
	if r.MixtureRate == 0 {
		return false
	}
	length := uint64(r.MixtureLength) //nolint:gosec // Positive by validation.
	block := c.regimeBlock(call, length, evaluationMixturePhaseSalt)
	return evaluationUniform(c.regimeKey(block, 0, 0x4d495854555245)) <
		r.MixtureRate
}

func (c *evaluationChannel) regimeBlock(call, length, salt uint64) uint64 {
	phase := evaluationMix(c.seed^salt) % length
	return (call + phase) / length
}

func (c *evaluationChannel) regimeNormal(index, item, draw uint64) float64 {
	u1 := max(math.SmallestNonzeroFloat64,
		evaluationUniform(c.regimeKey(index, item, draw)))
	u2 := evaluationUniform(c.regimeKey(index, item, draw^0x9e3779b97f4a7c15))
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

func (c *evaluationChannel) regimeKey(index, item, draw uint64) uint64 {
	return evaluationMix(c.seed ^ index*0xbf58476d1ce4e5b9 ^
		item*0x94d049bb133111eb ^ draw)
}

// evaluationRegimeScenarios supplies matched target/null cells. These scenarios
// test robustness, not calibration: temporal dependence and quantisation do not
// meet the reference solver's conditional Bernoulli premise, so a negative
// against a present target is deliberately recorded as unscored.
func evaluationRegimeScenarios() []evaluationScenario {
	const (
		falseFindRisk = 0.01
		minimumEdge   = 0.10
	)
	makeScenario := func(name string, target bool, spec evaluationRegimeSpec) evaluationScenario {
		scenario := evaluationScenario{
			Name: name, Panels: []string{"robustness"}, Population: 99,
			RequestDirection: Late,
			FalseFindRisk:    falseFindRisk, FalseNegativeRisk: 0.10,
			MinimumEdge: minimumEdge, MaxComparisons: 8_192,
			Width: 8, MaxConcurrency: 1, UnboundedControls: true,
			Channel: evaluationChannelSpec{Kind: evaluationRegime, Regime: &spec},
		}
		if target {
			scenario.Targets = evaluationTargets(Late)
		}
		return scenario
	}
	type cell struct {
		name string
		spec evaluationRegimeSpec
	}
	cells := []cell{
		{
			"drifting-position",
			evaluationRegimeSpec{
				BaseNoiseScale: 8, Effect: 5,
				DriftMagnitude: 18, DriftPeriod: 37,
			},
		},
		{
			"item-bursts",
			evaluationRegimeSpec{
				BaseNoiseScale: 3, Effect: 6, BurstNoiseScale: 8,
				BurstLength: 32,
			},
		},
		{
			"quantised-ties",
			evaluationRegimeSpec{
				BaseNoiseScale: 8, Effect: 6, Quantisation: 8,
			},
		},
		{
			"block-mixture",
			evaluationRegimeSpec{
				BaseNoiseScale: 4, Effect: 9, MixtureRate: 0.35,
				MixtureEffect: 1, MixtureNoise: 14, MixtureLength: 24,
			},
		},
	}
	var scenarios []evaluationScenario
	for _, cell := range cells {
		for _, target := range []bool{false, true} {
			truth := "null"
			if target {
				truth = "target"
			}
			scenarios = append(scenarios, makeScenario(
				fmt.Sprintf("regime/m99/late/%s/%s/w8/p1", truth, cell.name),
				target, cell.spec))
		}
	}
	return scenarios
}

func TestEvaluationRegimeScenariosValidateAndPair(t *testing.T) {
	scenarios := evaluationRegimeScenarios()
	if len(scenarios) != 8 {
		t.Fatalf("scenario count = %d, want 8", len(scenarios))
	}
	for i := 0; i < len(scenarios); i += 2 {
		null, target := scenarios[i], scenarios[i+1]
		if err := null.validate(); err != nil {
			t.Fatalf("validate %s: %v", null.Name, err)
		}
		if err := target.validate(); err != nil {
			t.Fatalf("validate %s: %v", target.Name, err)
		}
		if len(null.Targets) != 0 || len(target.Targets) != 1 {
			t.Fatalf("pair %d target counts = %d/%d", i/2,
				len(null.Targets), len(target.Targets))
		}
		null.Name, target.Name = "", ""
		null.Targets, target.Targets = nil, nil
		if !reflect.DeepEqual(null, target) {
			t.Fatalf("pair %d differs beyond name and target presence", i/2)
		}
		truth := scenarios[i+1].truth(17)
		if truth.Targets[0].Scored || truth.Targets[0].Qualifies {
			t.Fatalf("pair %d target truth is decision-scored: %+v", i/2, truth)
		}
	}
}

func TestEvaluationRegimeScenarioRejectsConcurrency(t *testing.T) {
	scenario := evaluationRegimeScenarios()[0]
	scenario.MaxConcurrency = 2
	if err := scenario.validate(); err == nil {
		t.Fatal("concurrent regime scenario validation succeeded")
	}
}

func TestEvaluationRegimeTargetNullShareLatentPath(t *testing.T) {
	for i := 0; i < len(evaluationRegimeScenarios()); i += 2 {
		nullScenario := evaluationRegimeScenarios()[i]
		targetScenario := evaluationRegimeScenarios()[i+1]
		const seed = uint64(41)
		null := &evaluationChannel{
			scenario: nullScenario, truth: nullScenario.truth(seed), seed: seed,
		}
		targetTruth := targetScenario.truth(seed)
		planted, _ := targetTruth.firstTarget()
		target := &evaluationChannel{
			scenario: targetScenario, truth: targetTruth, seed: seed,
		}
		zeroEffectScenario := targetScenario
		zeroEffectSpec := *targetScenario.Channel.Regime
		zeroEffectSpec.Effect = 0
		zeroEffectSpec.MixtureEffect = 0
		zeroEffectScenario.Channel.Regime = &zeroEffectSpec
		zeroEffect := &evaluationChannel{
			scenario: zeroEffectScenario, truth: targetTruth, seed: seed,
		}
		items := make([]ItemID, targetScenario.Width)
		items[0] = ItemID(planted.Index)
		next := 0
		for position := 1; position < len(items); position++ {
			if position == len(items)-1 {
				items[position] = Control(Baseline, 0)
				continue
			}
			if next == planted.Index {
				next++
			}
			items[position] = ItemID(next)
			next++
		}
		targetEffectObserved := false
		for call := range uint64(256) {
			for position, item := range items {
				nullScore := null.regimeScore(item, position, targetScenario.Width, call)
				zeroScore := zeroEffect.regimeScore(
					item, position, targetScenario.Width, call)
				if nullScore != zeroScore {
					t.Fatalf("pair %d call %d item %d latent scores differ: %+v/%+v",
						i/2, call, item, nullScore, zeroScore)
				}
				targetScore := target.regimeScore(
					item, position, targetScenario.Width, call)
				if int(item) != planted.Index && targetScore != nullScore {
					t.Fatalf("pair %d call %d non-target %d scores differ: %+v/%+v",
						i/2, call, item, nullScore, targetScore)
				}
				if int(item) == planted.Index && targetScore.score != nullScore.score {
					targetEffectObserved = true
				}
			}
		}
		if !targetEffectObserved {
			t.Fatalf("pair %d produced no observable planted effect", i/2)
		}
	}
}

func TestEvaluationRegimeChannelReproducesBySeed(t *testing.T) {
	scenario := evaluationRegimeScenarios()[3]
	truth := scenario.truth(19)
	first, _ := newEvaluationComparator(scenario, truth, 19)
	second, _ := newEvaluationComparator(scenario, truth, 19)
	items := []ItemID{0, 1, 2, 3, 4, 5, 6, 7}
	for call := range 40 {
		got, err := first.Compare(t.Context(), items)
		if err != nil {
			t.Fatal(err)
		}
		want, err := second.Compare(t.Context(), items)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("call %d ranks = %v, want %v", call, got, want)
		}
	}
}

func TestEvaluationRegimeCommonModeIsRankInvariant(t *testing.T) {
	scenario := evaluationRegimeScenarios()[3]
	without := scenario
	copySpec := *scenario.Channel.Regime
	copySpec.CommonModeScale = 1_000
	scenario.Channel.Regime = &copySpec
	withoutSpec := copySpec
	withoutSpec.CommonModeScale = 0
	without.Channel.Regime = &withoutSpec
	truth := scenario.truth(23)
	withComparator, _ := newEvaluationComparator(scenario, truth, 23)
	withoutComparator, _ := newEvaluationComparator(without, truth, 23)
	items := []ItemID{0, 1, 2, 3, 4, 5, 6, 7}
	for call := range 40 {
		withRanks, err := withComparator.Compare(t.Context(), items)
		if err != nil {
			t.Fatal(err)
		}
		withoutRanks, err := withoutComparator.Compare(t.Context(), items)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(withRanks, withoutRanks) {
			t.Fatalf("call %d common-mode ranks = %v, want %v",
				call, withRanks, withoutRanks)
		}
	}
}

func TestEvaluationRegimeBurstAndMixtureBlocks(t *testing.T) {
	burstScenario := evaluationRegimeScenarios()[3]
	burst := &evaluationChannel{scenario: burstScenario, seed: 29}
	item := evaluationItemBits(7)
	burstLength := uint64(burstScenario.Channel.Regime.BurstLength) //nolint:gosec // Validated.
	burstPhase := evaluationMix(burst.seed^evaluationBurstPhaseSalt) % burstLength
	burstRemainder := burstLength - burstPhase
	first := burst.regimeBurstNoise(0, item)
	if last := burst.regimeBurstNoise(burstRemainder-1, item); first != last {
		t.Fatalf("burst draw changed within block: %v != %v", first, last)
	}
	if next := burst.regimeBurstNoise(burstRemainder, item); first == next {
		t.Fatalf("adjacent burst blocks have identical draw %v", first)
	}

	mixtureScenario := evaluationRegimeScenarios()[7]
	mixture := &evaluationChannel{scenario: mixtureScenario, seed: 31}
	mixtureLength := uint64(mixtureScenario.Channel.Regime.MixtureLength) //nolint:gosec // Validated.
	seen := map[bool]bool{}
	mixturePhase := evaluationMix(mixture.seed^evaluationMixturePhaseSalt) % mixtureLength
	firstBoundary := mixtureLength - mixturePhase
	for block := range uint64(128) {
		start := firstBoundary + block*mixtureLength
		atStart := mixture.regimeMixture(start)
		atEnd := mixture.regimeMixture(start + mixtureLength - 1)
		if atStart != atEnd {
			t.Fatalf("mixture regime changed inside block %d", block)
		}
		seen[atStart] = true
	}
	if len(seen) != 2 {
		t.Fatalf("128 blocks visited %d mixture regimes, want 2", len(seen))
	}
}

func TestEvaluationRegimeQuantisationUsesNeutralTieBreak(t *testing.T) {
	scenario := evaluationRegimeScenarios()[5]
	truth := scenario.truth(37)
	channel := &evaluationChannel{scenario: scenario, truth: truth, seed: 37}
	items := []ItemID{0, 1, 2, 3, 4, 5, 6, 7}
	tied := false
	for call := range uint64(64) {
		seen := make(map[float64]bool)
		for position, item := range items {
			score := channel.regimeScore(item, position, len(items), call)
			otherPosition := (position + 1) % len(items)
			moved := channel.regimeScore(item, otherPosition, len(items), call)
			if score.tie != moved.tie {
				t.Fatalf("call %d item %d tie key depends on position", call, item)
			}
			if seen[score.score] {
				tied = true
			}
			seen[score.score] = true
		}
		ranks := channel.regimeRanks(items, call)
		ordered := slices.Clone(ranks)
		slices.Sort(ordered)
		for rank := range ordered {
			if ordered[rank] != rank {
				t.Fatalf("call %d ranks are not a total order: %v", call, ranks)
			}
		}
	}
	if !tied {
		t.Fatal("quantised scenario produced no latent ties")
	}
}
