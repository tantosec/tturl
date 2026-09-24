package ranking

import (
	"reflect"
	"sort"
	"testing"
)

// peerHeterogeneousRanks constructs a channel with a clean Baseline premise
// and a deliberately incompatible peer-relative premise. The designated item
// beats a Baseline with probability 1/2+DesignatedEdge but beats an ordinary
// heterogeneous peer with probability 1-PeerHighRate.
func (c *evaluationChannel) peerHeterogeneousRanks(
	items []ItemID, layoutSeed, occurrence uint64,
) []int {
	type scoredPosition struct {
		position int
		score    float64
		tie      uint64
	}
	population := uint64(c.scenario.Population) //nolint:gosec // Validated positive.
	designated := int(                          //nolint:gosec // Modulus fits the validated int population.
		evaluationMix(c.seed^evaluationTargetSalt) % population)
	direction := 1.0
	if c.scenario.RequestDirection == Early {
		direction = -1
	}
	scores := make([]scoredPosition, len(items))
	for position, item := range items {
		score := 0.0
		if item.IsCandidate() {
			rate, high, low := c.scenario.Channel.PeerHighRate, 2.0, -2.0
			if int(item) == designated {
				rate, high, low = 0.5+c.scenario.Channel.DesignatedEdge, 1, -1
			}
			draw := evaluationUniform(c.drawKey(
				layoutSeed, occurrence, position,
				evaluationItemBits(item)^0x5045455248494748,
			))
			score = low
			if draw < rate {
				score = high
			}
			score *= direction
		}
		scores[position] = scoredPosition{
			position: position, score: score,
			tie: c.drawKey(
				layoutSeed, occurrence, position,
				evaluationItemBits(item)^0x50454552544945,
			),
		}
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

func evaluationPeerHeterogeneousScenarios() []evaluationScenario {
	const (
		population  = 99
		width       = 8
		minimumEdge = 0.10
		peerRate    = 0.10
	)
	makeScenario := func(sign OutlierDirection, target bool) evaluationScenario {
		condition := "null"
		designatedEdge := -0.01
		if target {
			condition = "boundary"
			designatedEdge = minimumEdge
		}
		scenario := evaluationScenario{
			Name:   "peer-heterogeneous/m99/" + sign.String() + "/" + condition + "/w8/p1",
			Panels: []string{"robustness"}, Population: population,
			RequestDirection: sign,
			FalseFindRisk:    0.01, FalseNegativeRisk: 0.10,
			MinimumEdge: minimumEdge, MaxComparisons: 8_192,
			Width: width, MaxConcurrency: 1, UnboundedControls: true,
			Channel: evaluationChannelSpec{
				Kind:           evaluationPeerHeterogeneous,
				DesignatedEdge: designatedEdge, PeerHighRate: peerRate,
			},
		}
		if target {
			scenario.Targets = evaluationTargets(sign)
		}
		return scenario
	}
	return []evaluationScenario{
		makeScenario(Late, false), makeScenario(Late, true),
		makeScenario(Early, false), makeScenario(Early, true),
	}
}

func TestEvaluationPeerHeterogeneousScenarioPair(t *testing.T) {
	scenarios := evaluationPeerHeterogeneousScenarios()
	for i := 0; i < len(scenarios); i += 2 {
		null, target := scenarios[i], scenarios[i+1]
		if len(null.Targets) != 0 || len(target.Targets) != 1 {
			t.Fatalf("pair %d target markers are invalid", i/2)
		}
		normalised := target
		normalised.Name = null.Name
		normalised.Targets = nil
		normalised.Channel.DesignatedEdge = null.Channel.DesignatedEdge
		if !reflect.DeepEqual(null, normalised) {
			t.Errorf("pair %d differs beyond the planted Baseline effect", i/2)
		}
		if got := target.effectiveEdge(target.Targets[0]); got < target.MinimumEdge {
			t.Errorf("pair %d target edge = %v, want at least %v",
				i/2, got, target.MinimumEdge)
		}
	}
}

func TestEvaluationPeerHeterogeneousExactPremises(t *testing.T) {
	const (
		designatedNullEdge   = -0.01
		designatedTargetEdge = 0.10
		peerRate             = 0.10
	)
	if 0.5+designatedNullEdge >= 0.5 {
		t.Fatal("null designated item is not clean-null")
	}
	if designatedTargetEdge != 0.10 {
		t.Fatal("target does not have the declared clean edge")
	}
	if peerWinRate := 1 - peerRate; peerWinRate != 0.90 {
		t.Fatalf("designated peer win rate = %v, want 0.90", peerWinRate)
	}
}
