package main

import (
	"fmt"
	"math"

	"github.com/tantosec/tturl/internal/ranking"
)

type detectRequestPlan struct {
	candidates           int
	baselineTemplates    int
	baselineSupply       baselineSupply
	baselineEffectiveCap int
	widthRequested       int
	widthEffective       int
}

func (p detectRequestPlan) comparatorProfile(concurrency int) ranking.Profile {
	profile := ranking.Profile{
		MaxWidth: p.widthEffective, MaxConcurrency: concurrency,
	}
	if p.baselineTemplates == 0 {
		return profile
	}
	profile.Controls = make(map[ranking.ControlKind]ranking.ControlSupply, 1)
	if p.baselineSupply.unlimited {
		profile.Controls[ranking.Baseline] = ranking.UnboundedControls()
	} else {
		profile.Controls[ranking.Baseline] = ranking.BoundedControls(
			p.baselineSupply.count)
	}
	return profile
}

func newDetectRequestPlan(
	assembled *assembled,
	requestedWidth int,
) (detectRequestPlan, error) {
	plan := detectRequestPlan{widthRequested: requestedWidth}
	for _, group := range assembled.groups {
		switch {
		case group.warmup:
			continue
		case group.baseline:
			plan.baselineTemplates++
			if group.supply.unlimited {
				plan.baselineSupply = unlimitedBaselineSupply()
				continue
			}
			if plan.baselineSupply.count > math.MaxInt-group.supply.count {
				return detectRequestPlan{}, fmt.Errorf(
					"aggregate --baseline-supply cannot be represented")
			}
			plan.baselineSupply.count += group.supply.count
		default:
			if plan.candidates > math.MaxInt-group.count {
				return detectRequestPlan{}, fmt.Errorf(
					"candidate count cannot be represented")
			}
			plan.candidates += group.count
		}
	}

	if plan.candidates < 1 {
		return detectRequestPlan{}, fmt.Errorf(
			"detect needs at least one candidate request")
	}
	hasBaseline := plan.baselineTemplates > 0
	if !hasBaseline && plan.candidates < 2 {
		return detectRequestPlan{}, requireComparable(plan.candidates, "detect")
	}

	switch {
	case !hasBaseline:
		plan.widthEffective = min(requestedWidth, plan.candidates)
	case plan.baselineSupply.unlimited:
		plan.widthEffective = requestedWidth
	default:
		plan.widthEffective = requestedWidth
		if plan.candidates < requestedWidth &&
			plan.baselineSupply.count < requestedWidth-plan.candidates {
			plan.widthEffective = plan.candidates + plan.baselineSupply.count
		}
	}
	if hasBaseline {
		plan.baselineEffectiveCap = plan.widthEffective - 1
		if !plan.baselineSupply.unlimited {
			plan.baselineEffectiveCap = min(
				plan.baselineEffectiveCap, plan.baselineSupply.count)
		}
		for i := range assembled.groups {
			group := &assembled.groups[i]
			if !group.baseline {
				continue
			}
			group.baselineEffectiveMax = plan.baselineEffectiveCap
			if !group.supply.unlimited {
				group.baselineEffectiveMax = min(
					group.baselineEffectiveMax, group.supply.count)
			}
		}
	}
	return plan, nil
}

func (p *prepared) baselineTemplateIndex(control int) (int, error) {
	return baselineTemplateIndex(p.baselineTemplates, control)
}

func baselineTemplateIndex(
	templates []baselineRequestTemplate,
	control int,
) (int, error) {
	if control < 0 || len(templates) == 0 {
		return 0, fmt.Errorf("baseline control %d is out of range", control)
	}
	if templates[0].supply.unlimited {
		return control % len(templates), nil
	}

	total := 0
	maximum := 0
	for _, template := range templates {
		total += template.supply.count
		maximum = max(maximum, template.supply.count)
	}
	if control >= total {
		return 0, fmt.Errorf("baseline control %d is out of range", control)
	}

	lo, hi := 0, maximum
	for lo < hi {
		layer := lo + (hi-lo)/2
		if baselineCapacityThrough(templates, layer) > control {
			hi = layer
		} else {
			lo = layer + 1
		}
	}
	layer := lo
	before := baselineCapacityThrough(templates, layer-1)
	offset := control - before
	for i, template := range templates {
		if template.supply.count > layer {
			if offset == 0 {
				return i, nil
			}
			offset--
		}
	}
	panic("baseline control mapping is inconsistent")
}

func baselineCapacityThrough(
	templates []baselineRequestTemplate,
	layer int,
) int {
	if layer < 0 {
		return 0
	}
	capacity := 0
	for _, template := range templates {
		capacity += min(template.supply.count, layer+1)
	}
	return capacity
}
