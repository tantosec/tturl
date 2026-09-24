package main

import (
	"fmt"

	"github.com/tantosec/tturl/internal/ranking"
)

type detectStrategy string

const (
	detectStrategyAuto                     detectStrategy = "auto"
	detectStrategyPeerFirst                detectStrategy = "peer-first"
	detectStrategyRollingPeerFirst         detectStrategy = "rolling-peer-first"
	detectStrategyBaselineConfirmed        detectStrategy = "baseline-confirmed"
	detectStrategyRollingBaselineConfirmed detectStrategy = "rolling-baseline-confirmed"
	detectStrategyBaselineReserved         detectStrategy = "baseline-reserved"
	detectStrategyRollingBaselineReserved  detectStrategy = "rolling-baseline-reserved"
)

var detectStrategyNames = []string{
	string(detectStrategyAuto),
	string(detectStrategyPeerFirst),
	string(detectStrategyRollingPeerFirst),
	string(detectStrategyBaselineConfirmed),
	string(detectStrategyRollingBaselineConfirmed),
	string(detectStrategyBaselineReserved),
	string(detectStrategyRollingBaselineReserved),
}

type detectStrategySpec struct {
	name detectStrategy
	new  func(maxComparisons, maxConcurrency int) ranking.OutlierSolver
}

var detectStrategyRegistry = []detectStrategySpec{
	{
		name: detectStrategyPeerFirst,
		new: func(maxComparisons, maxConcurrency int) ranking.OutlierSolver {
			return &ranking.PeerFirstOutlierSolver{
				MaxComparisons: maxComparisons, MaxConcurrency: maxConcurrency,
			}
		},
	},
	{
		name: detectStrategyRollingPeerFirst,
		new: func(maxComparisons, maxConcurrency int) ranking.OutlierSolver {
			return &ranking.RollingPeerFirstOutlierSolver{
				MaxComparisons: maxComparisons, MaxConcurrency: maxConcurrency,
			}
		},
	},
	{
		name: detectStrategyBaselineConfirmed,
		new: func(maxComparisons, maxConcurrency int) ranking.OutlierSolver {
			return &ranking.BaselineConfirmedOutlierSolver{
				MaxComparisons: maxComparisons, MaxConcurrency: maxConcurrency,
			}
		},
	},
	{
		name: detectStrategyRollingBaselineConfirmed,
		new: func(maxComparisons, maxConcurrency int) ranking.OutlierSolver {
			return &ranking.RollingBaselineConfirmedOutlierSolver{
				MaxComparisons: maxComparisons, MaxConcurrency: maxConcurrency,
			}
		},
	},
	{
		name: detectStrategyBaselineReserved,
		new: func(maxComparisons, maxConcurrency int) ranking.OutlierSolver {
			return &ranking.BaselineReservedOutlierSolver{
				MaxComparisons: maxComparisons, MaxConcurrency: maxConcurrency,
			}
		},
	},
	{
		name: detectStrategyRollingBaselineReserved,
		new: func(maxComparisons, maxConcurrency int) ranking.OutlierSolver {
			return &ranking.RollingBaselineReservedOutlierSolver{
				MaxComparisons: maxComparisons, MaxConcurrency: maxConcurrency,
			}
		},
	},
}

type detectStrategySelection struct {
	requested detectStrategy
	resolved  detectStrategy
	method    ranking.OutlierMethodID
	spec      detectStrategySpec
	warning   string
}

func resolveDetectStrategy(
	raw string,
	direction ranking.OutlierDirection,
	hasBaseline bool,
) (detectStrategySelection, error) {
	requested := detectStrategy(raw)
	if requested != detectStrategyAuto && detectStrategyByName(requested) == nil {
		return detectStrategySelection{}, fmt.Errorf(
			"--strategy must be auto, peer-first, rolling-peer-first, "+
				"baseline-confirmed, rolling-baseline-confirmed, "+
				"baseline-reserved, or rolling-baseline-reserved; got %q", raw)
	}
	if direction == ranking.Either && !hasBaseline {
		return detectStrategySelection{}, fmt.Errorf(
			"--direction either requires a --baseline-only block")
	}

	resolved := requested
	if requested == detectStrategyAuto {
		if hasBaseline {
			resolved = detectStrategyBaselineReserved
		} else {
			resolved = detectStrategyPeerFirst
		}
	}
	switch resolved {
	case detectStrategyPeerFirst, detectStrategyRollingPeerFirst:
		if direction == ranking.Either {
			return detectStrategySelection{}, fmt.Errorf(
				"--strategy %s requires --direction early or late", resolved)
		}
	case detectStrategyBaselineConfirmed, detectStrategyRollingBaselineConfirmed:
		if !hasBaseline {
			return detectStrategySelection{}, fmt.Errorf(
				"--strategy %s requires a --baseline-only block", resolved)
		}
		if direction == ranking.Either {
			return detectStrategySelection{}, fmt.Errorf(
				"--strategy %s requires --direction early or late", resolved)
		}
	case detectStrategyBaselineReserved:
		if !hasBaseline {
			return detectStrategySelection{}, fmt.Errorf(
				"--strategy baseline-reserved requires a --baseline-only block")
		}
	case detectStrategyRollingBaselineReserved:
		if !hasBaseline {
			return detectStrategySelection{}, fmt.Errorf(
				"--strategy rolling-baseline-reserved requires a " +
					"--baseline-only block")
		}
	}

	spec := detectStrategyByName(resolved)
	selection := detectStrategySelection{
		requested: requested, resolved: resolved, spec: *spec,
	}
	solver, err := selection.newSolver(1, 1)
	if err != nil {
		return detectStrategySelection{}, err
	}
	selection.method = solver.MethodID()
	if (requested == detectStrategyPeerFirst ||
		requested == detectStrategyRollingPeerFirst) && hasBaseline {
		selection.warning = "explicit " + string(requested) + " strategy defers supplied " +
			"baselines during broad-field discovery; multiple outliers can mask " +
			"one another and affect both found and negative premises"
	}
	return selection, nil
}

func (s detectStrategySelection) evidenceUpdatePolicy() string {
	if s.rolling() {
		return "maximal-contiguous-dispatch-prefix"
	}
	return "complete-atomic-wave"
}

func (s detectStrategySelection) dependencePremise() string {
	if s.rolling() {
		return "conditional-across-dispatch-indices"
	}
	return "arbitrary-within-atomic-wave"
}

func (s detectStrategySelection) rolling() bool {
	switch s.resolved {
	case detectStrategyRollingPeerFirst,
		detectStrategyRollingBaselineConfirmed,
		detectStrategyRollingBaselineReserved:
		return true
	default:
		return false
	}
}

func detectStrategyByName(name detectStrategy) *detectStrategySpec {
	for i := range detectStrategyRegistry {
		if detectStrategyRegistry[i].name == name {
			return &detectStrategyRegistry[i]
		}
	}
	return nil
}

func (s detectStrategySelection) newSolver(
	maxComparisons int,
	maxConcurrency int,
) (ranking.IdentifiedOutlierSolver, error) {
	solver := s.spec.new(maxComparisons, maxConcurrency)
	identified, ok := solver.(ranking.IdentifiedOutlierSolver)
	if !ok {
		return nil, fmt.Errorf(
			"detect strategy %q returned an unidentified outlier solver",
			s.resolved)
	}
	if id := identified.MethodID(); !id.Valid() {
		return nil, fmt.Errorf(
			"detect strategy %q returned invalid solver method ID %q",
			s.resolved, id)
	} else if s.method != "" && id != s.method {
		return nil, fmt.Errorf(
			"detect strategy %q changed solver method ID from %q to %q",
			s.resolved, s.method, id)
	}
	return identified, nil
}
