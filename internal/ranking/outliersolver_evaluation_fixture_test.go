package ranking

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	evaluationFixtureVersion  = "tturl/ranking/outlier-evaluation/v3"
	evaluationRunSchema       = "tturl/ranking/evaluation-run/v2"
	evaluationLegacyRunSchema = "tturl/ranking/evaluation-run/v1"
	evaluationMetaSchema      = "tturl/ranking/evaluation-meta/v3"
	evidenceCompletionSchema  = "tturl/ranking/evidence-complete/v1"

	evaluationEnvironmentSalt = uint64(0x454e5649524f4e4d)
	evaluationSolverSalt      = uint64(0x414c474f52495448)
	evaluationTargetSalt      = uint64(0x5441524745544944)
)

type evaluationSolverConfig struct {
	Seed           uint64
	MaxComparisons int
	MaxConcurrency int
}

type evaluationSolver struct {
	Name      string
	Inference OutlierInferenceRegime
	Params    map[string]string
	// Supports returns an empty string when the solver supports a scenario,
	// or a stable reason when the capability is outside its contract.
	Supports func(evaluationScenario) string
	New      func(evaluationSolverConfig) OutlierSolver
}

func evaluationSolverCatalogue() []evaluationSolver {
	return []evaluationSolver{
		{
			Name: "baseline-reserved", Inference: OutlierInferenceAnytimeValid,
			Params: map[string]string{
				"evidence":  "bounded-betting",
				"scheduler": "thompson-leader-pin",
			},
			New: func(cfg evaluationSolverConfig) OutlierSolver {
				return &BaselineReservedOutlierSolver{
					MaxComparisons: cfg.MaxComparisons,
					MaxConcurrency: cfg.MaxConcurrency,
					newRNG: func() *rand.Rand {
						//nolint:gosec // Reproducible simulation requires a deterministic RNG.
						return rand.New(rand.NewPCG(
							cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
					},
				}
			},
		},
		{
			Name:      "rolling-baseline-reserved",
			Inference: OutlierInferenceAnytimeValid,
			Params: map[string]string{
				"allocation": "baseline-reserved-thompson-leader-pin",
				"coverage":   "initial-permutation",
				"evidence":   "dispatch-ordered-bounded-betting",
				"execution":  "maximal-contiguous-dispatch-prefix",
				"premise":    "dispatch-conditional-baseline-mean",
			},
			Supports: func(s evaluationScenario) string {
				if s.profile().Controls[Baseline].UpTo(1) == 0 {
					return "rolling evidence requires a Baseline reference"
				}
				return ""
			},
			New: func(cfg evaluationSolverConfig) OutlierSolver {
				return &RollingBaselineReservedOutlierSolver{
					MaxComparisons: cfg.MaxComparisons,
					MaxConcurrency: cfg.MaxConcurrency,
					newRNG: func() *rand.Rand {
						//nolint:gosec // Reproducible simulation requires a deterministic RNG.
						return rand.New(rand.NewPCG(
							cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
					},
				}
			},
		},
		{
			Name: "peer-first", Inference: OutlierInferenceAnytimeValid,
			Params: map[string]string{
				"allocation": "peer-first-controls-on-contraction",
				"evidence":   "bounded-betting",
				"scheduler":  "thompson-leader-pin",
			},
			New: func(cfg evaluationSolverConfig) OutlierSolver {
				return &PeerFirstOutlierSolver{
					MaxComparisons: cfg.MaxComparisons,
					MaxConcurrency: cfg.MaxConcurrency,
					newRNG: func() *rand.Rand {
						//nolint:gosec // Reproducible simulation requires a deterministic RNG.
						return rand.New(rand.NewPCG(
							cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
					},
				}
			},
		},
		{
			Name: "rolling-peer-first", Inference: OutlierInferenceAnytimeValid,
			Params: map[string]string{
				"allocation": "peer-first-controls-on-contraction",
				"coverage":   "initial-permutation",
				"evidence":   "dispatch-ordered-bounded-betting",
				"execution":  "maximal-contiguous-dispatch-prefix",
				"premise":    "dispatch-conditional-scheduled-reference-mean",
			},
			New: func(cfg evaluationSolverConfig) OutlierSolver {
				return &RollingPeerFirstOutlierSolver{
					MaxComparisons: cfg.MaxComparisons,
					MaxConcurrency: cfg.MaxConcurrency,
					newRNG: func() *rand.Rand {
						//nolint:gosec // Reproducible simulation requires a deterministic RNG.
						return rand.New(rand.NewPCG(
							cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
					},
				}
			},
		},
		{
			Name: "baseline-confirmed", Inference: OutlierInferenceAnytimeValid,
			Params: map[string]string{
				"allocation":   "known-sign-peer-first-thompson",
				"confirmation": "clean-one-per-eligible-wave",
				"either":       "conservative-clean-reference",
				"evidence":     "clean-commit-bounded-betting",
				"nomination":   "log-evidence-at-least-log2",
			},
			Supports: func(s evaluationScenario) string {
				if s.profile().Controls[Baseline].UpTo(1) == 0 {
					return "clean confirmation requires a Baseline reference"
				}
				return ""
			},
			New: func(cfg evaluationSolverConfig) OutlierSolver {
				return &BaselineConfirmedOutlierSolver{
					MaxComparisons: cfg.MaxComparisons,
					MaxConcurrency: cfg.MaxConcurrency,
					newRNG: func() *rand.Rand {
						//nolint:gosec // Reproducible simulation requires a deterministic RNG.
						return rand.New(rand.NewPCG(
							cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
					},
				}
			},
		},
		{
			Name:      "rolling-baseline-confirmed",
			Inference: OutlierInferenceAnytimeValid,
			Params: map[string]string{
				"allocation":   "known-sign-peer-first-thompson",
				"confirmation": "one-pending-clean-after-committed-discovery",
				"coverage":     "initial-permutation",
				"evidence":     "dispatch-ordered-clean-commit-bounded-betting",
				"execution":    "maximal-contiguous-dispatch-prefix",
				"nomination":   "committed-log-evidence-at-least-log2",
				"premise":      "dispatch-conditional-scheduled-reference-mean",
			},
			Supports: func(s evaluationScenario) string {
				if s.profile().Controls[Baseline].UpTo(1) == 0 {
					return "rolling clean confirmation requires a Baseline reference"
				}
				return ""
			},
			New: func(cfg evaluationSolverConfig) OutlierSolver {
				return &RollingBaselineConfirmedOutlierSolver{
					MaxComparisons: cfg.MaxComparisons,
					MaxConcurrency: cfg.MaxConcurrency,
					newRNG: func() *rand.Rand {
						//nolint:gosec // Reproducible simulation requires a deterministic RNG.
						return rand.New(rand.NewPCG(
							cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
					},
				}
			},
		},
		{
			Name: "edge-directed", Inference: OutlierInferenceAnytimeValid,
			Params: map[string]string{
				"allocation": "peer-first-controls-on-contraction",
				"evidence":   "minimum-edge-variance-adaptive",
				"scheduler":  "thompson-leader-pin",
			},
			New: func(cfg evaluationSolverConfig) OutlierSolver {
				return &EdgeDirectedOutlierSolver{
					MaxComparisons: cfg.MaxComparisons,
					MaxConcurrency: cfg.MaxConcurrency,
					newRNG: func() *rand.Rand {
						//nolint:gosec // Reproducible simulation requires a deterministic RNG.
						return rand.New(rand.NewPCG(
							cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
					},
				}
			},
		},
	}
}

type evaluationChannelKind string

const (
	evaluationGaussian          evaluationChannelKind = "gaussian-utility"
	evaluationEndpoint          evaluationChannelKind = "endpoint-rank"
	evaluationRegime            evaluationChannelKind = "regime-utility"
	evaluationPeerHeterogeneous evaluationChannelKind = "peer-heterogeneous-utility"
)

type evaluationNoiseKind string

const (
	evaluationHomoskedastic   evaluationNoiseKind = "homoskedastic"
	evaluationHeteroskedastic evaluationNoiseKind = "heteroskedastic"
	evaluationOneSided        evaluationNoiseKind = "one-sided-contamination"
)

type evaluationBiasKind string

const (
	evaluationFlatBias     evaluationBiasKind = "flat"
	evaluationLinearBias   evaluationBiasKind = "linear"
	evaluationReversedBias evaluationBiasKind = "reversed-linear"
	evaluationSteppedBias  evaluationBiasKind = "stepped"
)

type evaluationChannelSpec struct {
	Kind           evaluationChannelKind `json:"kind"`
	Noise          evaluationNoiseKind   `json:"noise,omitempty"`
	NoiseScale     float64               `json:"noise_scale,omitempty"`
	Effect         float64               `json:"effect,omitempty"`
	Bias           evaluationBiasKind    `json:"bias,omitempty"`
	BiasMagnitude  float64               `json:"bias_magnitude,omitempty"`
	EndpointEdge   float64               `json:"endpoint_edge,omitempty"`
	SpikeRate      float64               `json:"spike_rate,omitempty"`
	SpikeScale     float64               `json:"spike_scale,omitempty"`
	DesignatedEdge float64               `json:"designated_baseline_edge,omitempty"`
	PeerHighRate   float64               `json:"peer_high_rate,omitempty"`
	Regime         *evaluationRegimeSpec `json:"regime,omitempty"`
}

// evaluationTargetSpec describes one planted directional extreme. Scale multiplies
// the channel's nominal effect and defaults to one.
type evaluationTargetSpec struct {
	Direction OutlierDirection `json:"-"`
	Scale     float64          `json:"scale,omitempty"`
}

func evaluationTargets(signs ...OutlierDirection) []evaluationTargetSpec {
	targets := make([]evaluationTargetSpec, len(signs))
	for i, sign := range signs {
		targets[i] = evaluationTargetSpec{Direction: sign, Scale: 1}
	}
	return targets
}

type evaluationScenario struct {
	Name              string                 `json:"name"`
	Panels            []string               `json:"panels"`
	Population        int                    `json:"population"`
	RequestDirection  OutlierDirection       `json:"-"`
	Targets           []evaluationTargetSpec `json:"-"`
	PremiseNote       string                 `json:"premise_note,omitempty"`
	FalseFindRisk     float64                `json:"false_find_risk"`
	FalseNegativeRisk float64                `json:"false_negative_risk"`
	MinimumEdge       float64                `json:"minimum_edge"`
	MaxComparisons    int                    `json:"max_comparisons"`
	Width             int                    `json:"width"`
	MaxConcurrency    int                    `json:"max_concurrency"`
	BaselineControls  int                    `json:"baseline_controls"`
	UnboundedControls bool                   `json:"unbounded_controls"`
	PositiveControls  int                    `json:"positive_controls"`
	UnboundedPositive bool                   `json:"unbounded_positive_controls"`
	Channel           evaluationChannelSpec  `json:"channel"`
}

func (s evaluationScenario) inPanel(name string) bool {
	return slices.Contains(s.Panels, name)
}

func (s evaluationScenario) profile() Profile {
	profile := Profile{
		MaxWidth: s.Width, MaxConcurrency: s.MaxConcurrency,
	}
	controls := make(map[ControlKind]ControlSupply)
	if s.UnboundedControls {
		controls[Baseline] = UnboundedControls()
	} else if s.BaselineControls > 0 {
		controls[Baseline] = BoundedControls(s.BaselineControls)
	}
	if s.UnboundedPositive {
		controls[Positive] = UnboundedControls()
	} else if s.PositiveControls > 0 {
		controls[Positive] = BoundedControls(s.PositiveControls)
	}
	if len(controls) > 0 {
		profile.Controls = controls
	}
	return profile
}

type evaluationTargetTruth struct {
	Index     int
	Direction OutlierDirection
	Scale     float64
	Scored    bool
	Qualifies bool
}

type evaluationTruth struct {
	Targets []evaluationTargetTruth
}

func (t evaluationTruth) target(index int) (evaluationTargetTruth, bool) {
	for _, target := range t.Targets {
		if target.Index == index {
			return target, true
		}
	}
	return evaluationTargetTruth{}, false
}

func (t evaluationTruth) firstTarget() (evaluationTargetTruth, bool) {
	if len(t.Targets) == 0 {
		return evaluationTargetTruth{}, false
	}
	return t.Targets[0], true
}

func (s evaluationScenario) truth(environmentSeed uint64) evaluationTruth {
	truth := evaluationTruth{Targets: make([]evaluationTargetTruth, 0, len(s.Targets))}
	used := make(map[int]bool, len(s.Targets))
	for ordinal, spec := range s.Targets {
		// Population is validated as positive, so the modulus fits in an int.
		population := uint64(s.Population) //nolint:gosec // Checked by validate.
		key := environmentSeed ^ evaluationTargetSalt ^ uint64(ordinal)
		index := int(evaluationMix(key) % population) //nolint:gosec // Modulus fits.
		for used[index] {
			index = (index + 1) % s.Population
		}
		used[index] = true
		sign := spec.Direction
		if s.RequestDirection == Either && sign == Either {
			if evaluationMix(key^1)&1 == 0 {
				sign = Late
			} else {
				sign = Early
			}
		}
		scored := s.decisionScored()
		truth.Targets = append(truth.Targets, evaluationTargetTruth{
			Index: index, Direction: sign, Scale: spec.Scale, Scored: scored,
			Qualifies: scored && s.effectiveEdge(spec) >= s.MinimumEdge,
		})
	}
	slices.SortFunc(truth.Targets, func(a, b evaluationTargetTruth) int {
		return a.Index - b.Index
	})
	return truth
}

func (s evaluationScenario) decisionScored() bool {
	return s.Channel.Kind == evaluationEndpoint ||
		s.Channel.Kind == evaluationPeerHeterogeneous ||
		(s.Channel.Kind == evaluationGaussian &&
			s.Channel.Noise == evaluationHomoskedastic &&
			s.Channel.Bias == evaluationFlatBias)
}

func (s evaluationScenario) effectiveEdge(target evaluationTargetSpec) float64 {
	scale := target.Scale
	if scale == 0 {
		scale = 1
	}
	if s.Channel.Kind == evaluationEndpoint {
		return s.Channel.EndpointEdge * scale
	}
	if s.Channel.Kind == evaluationPeerHeterogeneous {
		return s.Channel.DesignatedEdge * scale
	}
	effect := s.Channel.Effect * scale
	if target.Direction == Early {
		effect = -effect
	}
	var probability float64
	for targetPosition := range s.Width {
		for baselinePosition := range s.Width {
			if targetPosition == baselinePosition {
				continue
			}
			difference := effect + s.positionBias(targetPosition) -
				s.positionBias(baselinePosition)
			if target.Direction == Early {
				difference = -difference
			}
			z := difference / (math.Sqrt2 * s.Channel.NoiseScale)
			probability += 0.5 * (1 + math.Erf(z/math.Sqrt2))
		}
	}
	pairs := float64(s.Width * (s.Width - 1))
	return probability/pairs - 0.5
}

func (s evaluationScenario) positionBias(position int) float64 {
	if s.Width <= 1 {
		return 0
	}
	x := 2*float64(position)/float64(s.Width-1) - 1
	switch s.Channel.Bias {
	case evaluationLinearBias:
		return s.Channel.BiasMagnitude * x
	case evaluationReversedBias:
		return -s.Channel.BiasMagnitude * x
	case evaluationSteppedBias:
		if position < s.Width/2 {
			return -s.Channel.BiasMagnitude
		}
		return s.Channel.BiasMagnitude
	default:
		return 0
	}
}

func (s evaluationScenario) validate() error {
	switch {
	case s.Name == "":
		return fmt.Errorf("scenario name is empty")
	case s.Population < 1:
		return fmt.Errorf("scenario %q population must be >= 1", s.Name)
	case s.Width < 2:
		return fmt.Errorf("scenario %q width must be >= 2", s.Name)
	case s.MaxConcurrency < 0:
		return fmt.Errorf("scenario %q concurrency must be >= 0", s.Name)
	case s.MaxComparisons < 1:
		return fmt.Errorf("scenario %q comparison limit must be >= 1", s.Name)
	case s.MinimumEdge <= 0 || s.MinimumEdge >= 0.5:
		return fmt.Errorf("scenario %q minimum edge must be in (0, 0.5)", s.Name)
	case s.FalseFindRisk <= 0 || s.FalseFindRisk >= 1:
		return fmt.Errorf("scenario %q false-find risk must be in (0, 1)", s.Name)
	case s.FalseNegativeRisk <= 0 || s.FalseNegativeRisk >= 1:
		return fmt.Errorf("scenario %q false-negative risk must be in (0, 1)", s.Name)
	case s.RequestDirection != Late && s.RequestDirection != Early && s.RequestDirection != Either:
		return fmt.Errorf("scenario %q has invalid request sign", s.Name)
	case len(s.Targets) > s.Population:
		return fmt.Errorf("scenario %q has more targets than candidates", s.Name)
	case s.RequestDirection == Either && !s.UnboundedControls && s.BaselineControls < 1:
		return fmt.Errorf("scenario %q Either needs a Baseline control", s.Name)
	case s.UnboundedControls && s.BaselineControls > 0:
		return fmt.Errorf("scenario %q declares bounded and unbounded controls", s.Name)
	case s.BaselineControls < 0 || s.PositiveControls < 0:
		return fmt.Errorf("scenario %q has negative control supply", s.Name)
	case s.UnboundedPositive && s.PositiveControls > 0:
		return fmt.Errorf(
			"scenario %q declares bounded and unbounded positive controls", s.Name)
	case s.Population < s.Width && !s.UnboundedControls && !s.UnboundedPositive &&
		s.BaselineControls+s.PositiveControls < s.Width-s.Population:
		return fmt.Errorf("scenario %q cannot fill its comparison", s.Name)
	case s.Channel.Kind == evaluationEndpoint &&
		(s.Channel.EndpointEdge < 0 || s.Channel.EndpointEdge >= 0.5):
		return fmt.Errorf("scenario %q endpoint edge must be in [0, 0.5)", s.Name)
	case s.Channel.Kind == evaluationEndpoint && len(s.Targets) > 1:
		return fmt.Errorf("scenario %q endpoint channel supports one target", s.Name)
	case s.Channel.Kind == evaluationGaussian && s.Channel.NoiseScale <= 0:
		return fmt.Errorf("scenario %q Gaussian noise must be positive", s.Name)
	case s.Channel.Kind == evaluationRegime && s.MaxConcurrency > 1:
		return fmt.Errorf("scenario %q regime channel must be sequential", s.Name)
	case s.Channel.Kind == evaluationRegime:
		if err := s.Channel.Regime.validate(); err != nil {
			return fmt.Errorf("scenario %q regime channel: %w", s.Name, err)
		}
	case s.Channel.Kind == evaluationPeerHeterogeneous &&
		(!(0.5+s.Channel.DesignatedEdge > 0 && 0.5+s.Channel.DesignatedEdge < 1) ||
			!(s.Channel.PeerHighRate > 0 && s.Channel.PeerHighRate < 1)):
		return fmt.Errorf("scenario %q has invalid heterogeneous-peer rates", s.Name)
	case s.Channel.Kind == evaluationGaussian && s.Channel.Effect < 0:
		return fmt.Errorf("scenario %q Gaussian effect must be non-negative", s.Name)
	case s.Channel.Kind == evaluationGaussian &&
		s.Channel.Noise != evaluationHomoskedastic &&
		s.Channel.Noise != evaluationHeteroskedastic &&
		s.Channel.Noise != evaluationOneSided:
		return fmt.Errorf("scenario %q has unknown Gaussian noise %q",
			s.Name, s.Channel.Noise)
	case s.Channel.Kind == evaluationGaussian && s.Channel.Noise == evaluationOneSided &&
		(!(s.Channel.SpikeRate > 0 && s.Channel.SpikeRate < 1) ||
			s.Channel.SpikeScale <= 0):
		return fmt.Errorf("scenario %q has invalid one-sided contamination", s.Name)
	case s.Channel.Kind == evaluationGaussian &&
		s.Channel.Bias != evaluationFlatBias &&
		s.Channel.Bias != evaluationLinearBias &&
		s.Channel.Bias != evaluationReversedBias &&
		s.Channel.Bias != evaluationSteppedBias:
		return fmt.Errorf("scenario %q has unknown position bias %q",
			s.Name, s.Channel.Bias)
	case s.Channel.Kind != evaluationEndpoint && s.Channel.Kind != evaluationGaussian &&
		s.Channel.Kind != evaluationRegime && s.Channel.Kind != evaluationPeerHeterogeneous:
		return fmt.Errorf("scenario %q has unknown channel kind %q", s.Name, s.Channel.Kind)
	}
	for i, target := range s.Targets {
		if target.Direction != Late && target.Direction != Early &&
			(s.RequestDirection != Either || target.Direction != Either) {
			return fmt.Errorf("scenario %q target %d has invalid sign", s.Name, i)
		}
		if s.RequestDirection != Either && target.Direction != s.RequestDirection {
			return fmt.Errorf("scenario %q target %d differs from request sign", s.Name, i)
		}
		if target.Scale < 0 {
			return fmt.Errorf("scenario %q target %d has negative scale", s.Name, i)
		}
		if (s.Channel.Kind == evaluationEndpoint ||
			s.Channel.Kind == evaluationPeerHeterogeneous) &&
			s.effectiveEdge(target) >= 0.5 {
			return fmt.Errorf("scenario %q target %d edge is not below 0.5", s.Name, i)
		}
	}
	return nil
}

func (s evaluationScenario) hasQualifyingTarget() bool {
	if !s.decisionScored() {
		return false
	}
	for _, target := range s.Targets {
		if s.effectiveEdge(target) >= s.MinimumEdge {
			return true
		}
	}
	return false
}

type evaluationChannelStats struct {
	Comparisons    int `json:"comparisons"`
	Participations int `json:"participations"`
	MaxInFlight    int `json:"max_in_flight"`
}

type evaluationChannel struct {
	scenario evaluationScenario
	truth    evaluationTruth
	seed     uint64

	mu          sync.Mutex
	occurrences map[string]uint64
	stats       evaluationChannelStats
	inFlight    int
	nextCall    uint64

	rollingMu         sync.Mutex
	rollingRegistered int
	rollingCommitted  int
	rollingChanged    chan struct{}
}

func newEvaluationComparator(
	scenario evaluationScenario, truth evaluationTruth, seed uint64,
) (*Comparator, *evaluationChannel) {
	channel := &evaluationChannel{
		scenario: scenario, truth: truth, seed: seed,
		occurrences:      make(map[string]uint64),
		rollingCommitted: -1,
		rollingChanged:   make(chan struct{}),
	}
	comparator := NewComparator(scenario.profile(), channel.compare)
	comparator.compareIndexed = channel.compareIndexed
	comparator.comparisonCommitted = channel.comparisonCommitted
	return comparator, channel
}

func (c *evaluationChannel) compare(
	ctx context.Context, byPosition []ItemID,
) ([]int, error) {
	return c.compareAt(ctx, byPosition, -1)
}

func (c *evaluationChannel) compareIndexed(
	ctx context.Context, byPosition []ItemID, dispatchIndex int,
) ([]int, error) {
	return c.compareAt(ctx, byPosition, dispatchIndex)
}

func (c *evaluationChannel) comparisonCommitted(dispatchIndex int) {
	c.rollingMu.Lock()
	if dispatchIndex > c.rollingCommitted {
		c.rollingCommitted = dispatchIndex
		c.signalRollingChange()
	}
	c.rollingMu.Unlock()
}

func (c *evaluationChannel) signalRollingChange() {
	close(c.rollingChanged)
	c.rollingChanged = make(chan struct{})
}

func (c *evaluationChannel) compareAt(
	ctx context.Context, byPosition []ItemID, dispatchIndex int,
) ([]int, error) {
	key := evaluationLayoutKey(byPosition)
	layoutSeed := evaluationLayoutSeed(c.seed, key)
	c.mu.Lock()
	if c.stats.Comparisons >= c.scenario.MaxComparisons {
		c.mu.Unlock()
		return nil, errEvaluationCapExceeded
	}
	var occurrence, call uint64
	if dispatchIndex >= 0 {
		// Rolling comparisons are logically ordered before their goroutines run.
		// Bind simulated randomness to that order, not scheduler-dependent entry
		// into this function, so retained qualification evidence is replayable.
		occurrence = uint64(dispatchIndex)
		call = uint64(dispatchIndex)
	} else {
		occurrence = c.occurrences[key]
		c.occurrences[key]++
		call = c.nextCall
		c.nextCall++
	}
	c.stats.Comparisons++
	c.stats.Participations += len(byPosition)
	c.inFlight++
	c.stats.MaxInFlight = max(c.stats.MaxInFlight, c.inFlight)
	c.mu.Unlock()
	if dispatchIndex >= 0 {
		if err := c.awaitRollingTurn(ctx, dispatchIndex); err != nil {
			c.mu.Lock()
			c.stats.Comparisons--
			c.stats.Participations -= len(byPosition)
			c.inFlight--
			c.mu.Unlock()
			return nil, err
		}
	}
	var ranks []int
	switch c.scenario.Channel.Kind {
	case evaluationEndpoint:
		ranks = c.endpointRanks(byPosition, layoutSeed, occurrence)
	case evaluationRegime:
		ranks = c.regimeRanks(byPosition, call)
	case evaluationPeerHeterogeneous:
		ranks = c.peerHeterogeneousRanks(byPosition, layoutSeed, occurrence)
	default:
		ranks = c.gaussianRanks(byPosition, layoutSeed, occurrence)
	}

	c.mu.Lock()
	c.inFlight--
	c.mu.Unlock()
	return ranks, nil
}

func (c *evaluationChannel) awaitRollingTurn(
	ctx context.Context, dispatchIndex int,
) error {
	c.rollingMu.Lock()
	c.rollingRegistered++
	c.signalRollingChange()
	window := max(c.scenario.MaxConcurrency, 1)
	wantRegistered := min(dispatchIndex+window, c.scenario.MaxComparisons)
	for c.rollingCommitted < dispatchIndex-1 ||
		c.rollingRegistered < wantRegistered {
		changed := c.rollingChanged
		c.rollingMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		c.rollingMu.Lock()
	}
	c.rollingMu.Unlock()
	return nil
}

func (c *evaluationChannel) snapshot() evaluationChannelStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stats
}

func (c *evaluationChannel) endpointRanks(
	items []ItemID, layoutSeed, occurrence uint64,
) []int {
	targetPosition := -1
	var target evaluationTargetTruth
	targetIsPlanted := false
	for position, item := range items {
		positiveControl := item.IsControl() && item.ControlKind() == Positive
		candidateTarget, isTarget := c.truth.target(int(item))
		if (item.IsCandidate() && isTarget) || positiveControl {
			targetPosition = position
			target = candidateTarget
			targetIsPlanted = item.IsCandidate() && isTarget
			if positiveControl {
				target.Direction = c.scenario.RequestDirection
			}
			break
		}
	}
	if targetPosition < 0 && c.scenario.Population == 1 {
		for position, item := range items {
			if item.IsCandidate() {
				targetPosition = position
				target.Direction = c.scenario.RequestDirection
				break
			}
		}
	}
	if targetPosition < 0 {
		type keyedPosition struct {
			position int
			key      uint64
		}
		positions := make([]keyedPosition, len(items))
		for position, item := range items {
			positions[position] = keyedPosition{
				position: position,
				key: c.drawKey(
					layoutSeed, occurrence, position, evaluationItemBits(item)^0x52414e4b),
			}
		}
		sort.Slice(positions, func(i, j int) bool {
			if positions[i].key == positions[j].key {
				return positions[i].position < positions[j].position
			}
			return positions[i].key < positions[j].key
		})
		ranks := make([]int, len(items))
		for rank, position := range positions {
			ranks[position.position] = rank
		}
		return ranks
	}
	probability := 0.5
	if targetIsPlanted {
		scale := target.Scale
		if scale == 0 {
			scale = 1
		}
		probability += c.scenario.Channel.EndpointEdge * scale
	}
	u := evaluationUniform(c.drawKey(layoutSeed, occurrence, 0, 0))
	extreme := u < probability
	candidateRank := len(items) - 1
	if target.Direction == Early {
		candidateRank = 0
	}
	if !extreme {
		candidateRank = len(items) - 1 - candidateRank
	}
	ranks := make([]int, len(items))
	type keyedPosition struct {
		position int
		key      uint64
	}
	remaining := make([]keyedPosition, 0, len(items)-1)
	for position, item := range items {
		if position != targetPosition {
			remaining = append(remaining, keyedPosition{
				position: position,
				key: c.drawKey(
					layoutSeed, occurrence, position, evaluationItemBits(item)^0x52454d41494e),
			})
		}
	}
	sort.Slice(remaining, func(i, j int) bool {
		if remaining[i].key == remaining[j].key {
			return remaining[i].position < remaining[j].position
		}
		return remaining[i].key < remaining[j].key
	})
	ranks[targetPosition] = candidateRank
	next := 0
	for _, position := range remaining {
		if next == candidateRank {
			next++
		}
		ranks[position.position] = next
		next++
	}
	return ranks
}

func (c *evaluationChannel) gaussianRanks(
	items []ItemID, layoutSeed, occurrence uint64,
) []int {
	type scoredPosition struct {
		position int
		score    float64
	}
	scores := make([]scoredPosition, len(items))
	for position, item := range items {
		scale := c.scenario.Channel.NoiseScale
		if c.scenario.Channel.Noise == evaluationHeteroskedastic {
			identity := evaluationItemBits(item)
			scale *= 0.5 + 1.5*evaluationUniform(evaluationMix(identity^0x48455445524f))
		}
		noise := scale * c.normal(layoutSeed, occurrence, position, item)
		if c.scenario.Channel.Noise == evaluationOneSided &&
			evaluationUniform(c.drawKey(
				layoutSeed, occurrence, position,
				evaluationItemBits(item)^0x5350494b45524154,
			)) < c.scenario.Channel.SpikeRate {
			u := max(math.SmallestNonzeroFloat64, evaluationUniform(c.drawKey(
				layoutSeed, occurrence, position,
				evaluationItemBits(item)^0x5350494b45534341,
			)))
			noise += -math.Log(u) * c.scenario.Channel.SpikeScale
		}
		score := c.scenario.positionBias(position) + noise
		positiveControl := item.IsControl() && item.ControlKind() == Positive
		target, isTarget := c.truth.target(int(item))
		if (item.IsCandidate() && isTarget) || positiveControl {
			effect := c.scenario.Channel.Effect
			direction := target.Direction
			if positiveControl {
				direction = c.scenario.RequestDirection
			}
			if isTarget && target.Scale != 0 {
				effect *= target.Scale
			}
			if direction == Early {
				score -= effect
			} else {
				score += effect
			}
		}
		scores[position] = scoredPosition{position: position, score: score}
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			return scores[i].position < scores[j].position
		}
		return scores[i].score < scores[j].score
	})
	ranks := make([]int, len(items))
	for rank, scored := range scores {
		ranks[scored.position] = rank
	}
	return ranks
}

func (c *evaluationChannel) normal(
	layoutSeed, occurrence uint64, position int, item ItemID,
) float64 {
	u1 := max(math.SmallestNonzeroFloat64, evaluationUniform(c.drawKey(
		layoutSeed, occurrence, position, evaluationItemBits(item)<<1)))
	u2 := evaluationUniform(c.drawKey(
		layoutSeed, occurrence, position, evaluationItemBits(item)<<1|1))
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

func (c *evaluationChannel) drawKey(
	layoutSeed, occurrence uint64, position int, draw uint64,
) uint64 {
	// Position is an index produced by ranging over a slice.
	positionBits := uint64(position) //nolint:gosec // Nonnegative slice index.
	return evaluationMix(layoutSeed ^ positionBits*0x9e3779b97f4a7c15 ^
		occurrence*0xbf58476d1ce4e5b9 ^ draw)
}

// evaluationLayoutSeed folds a layout once per comparison. Individual channel
// draws then mix in occurrence, position, and purpose without repeatedly
// hashing the same serialised layout.
func evaluationLayoutSeed(seed uint64, layout string) uint64 {
	h := seed
	for i := range len(layout) {
		h = evaluationMix(h ^ uint64(layout[i]))
	}
	return evaluationMix(h)
}

func evaluationLayoutKey(items []ItemID) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(strconv.FormatInt(int64(item), 10))
		b.WriteByte(',')
	}
	return b.String()
}

func evaluationMix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

func evaluationUniform(key uint64) float64 {
	return float64(evaluationMix(key)>>11) * (1.0 / (1 << 53))
}

func evaluationItemBits(item ItemID) uint64 {
	// The conversion preserves every ItemID bit, including control IDs.
	return uint64(int64(item)) //nolint:gosec // Intentional two's-complement key.
}

func evaluationSeeds(seedStart uint64, run int) (uint64, uint64) {
	seed := evaluationRunSeed(seedStart, run)
	environment := evaluationMix(seed ^ evaluationEnvironmentSalt)
	solverSeed := evaluationMix(seed ^ evaluationSolverSalt)
	return environment, solverSeed
}

func evaluationRunSeed(seedStart uint64, run int) uint64 {
	// Run is produced by a nonnegative bounded iteration.
	return seedStart + uint64(run) //nolint:gosec // Nonnegative run index.
}

func evaluationSeedEnd(seedStart uint64, runs int) (uint64, error) {
	if runs < 1 {
		return 0, fmt.Errorf("evaluation runs must be >= 1")
	}
	count := uint64(runs)
	if count > ^uint64(0)-seedStart {
		return 0, fmt.Errorf("evaluation seed interval overflows uint64")
	}
	return seedStart + count, nil
}
