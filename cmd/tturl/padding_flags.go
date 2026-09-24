package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/tth2"
)

type positionalPaddingFlags struct {
	padding   *curlblocks.Flag[string]
	paddingAt *curlblocks.Flag[[]string]
}

func addPositionalPaddingFlags(command *curlblocks.Registry) positionalPaddingFlags {
	return positionalPaddingFlags{
		padding: command.String("padding", "", "SPEC", "",
			"query/headers/body counts; e.g. headers=0:1; counts: N, START:STEP, or -N; see tturl help padding"),
		paddingAt: command.StringArray(
			"padding-at", "", "POSITION:SPEC", nil,
			"override zero-based measured position counts; e.g. 0:headers=2; repeat for other assignments"),
	}
}

func (f positionalPaddingFlags) resolve(
	globals curlblocks.Carrier,
	width int,
) ([]tth2.RequestPadding, error) {
	return resolvePadding(
		f.padding.Get(globals), f.paddingAt.Get(globals), width)
}

type paddingComponent uint8

const (
	paddingQuery paddingComponent = iota
	paddingHeaders
	paddingBody
)

type paddingCadence struct {
	start int
	step  int
	auto  bool
}

type paddingOverrideKey struct {
	position  int
	component paddingComponent
}

func resolvePadding(
	spec string,
	at []string,
	requests int,
) ([]tth2.RequestPadding, error) {
	cadences, err := parsePaddingCadences(spec)
	if err != nil {
		return nil, fmt.Errorf("--padding: %w", err)
	}
	pads := make([]tth2.RequestPadding, requests)
	for component, cadence := range cadences {
		for position := range requests {
			value, err := cadence.value(position, requests)
			if err != nil {
				return nil, fmt.Errorf("--padding: %s at position %d: %w",
					component, position, err)
			}
			setPadding(&pads[position], component, value)
		}
	}

	seen := make(map[paddingOverrideKey]bool)
	for _, raw := range at {
		position, values, err := parsePaddingOverride(raw, requests)
		if err != nil {
			return nil, fmt.Errorf(
				"--padding-at %q: %w", curlblocks.DisplayText(raw), err)
		}
		for component, value := range values {
			key := paddingOverrideKey{position: position, component: component}
			if seen[key] {
				return nil, fmt.Errorf(
					"--padding-at assigns %s at position %d more than once",
					component, position)
			}
			seen[key] = true
			setPadding(&pads[position], component, value)
		}
	}

	for _, pad := range pads {
		if pad != (tth2.RequestPadding{}) {
			return pads, nil
		}
	}
	return nil, nil
}

func parsePaddingCadences(
	spec string,
) (map[paddingComponent]paddingCadence, error) {
	out := make(map[paddingComponent]paddingCadence)
	if spec == "" {
		return out, nil
	}
	for assignment := range strings.SplitSeq(spec, ",") {
		name, raw, ok := strings.Cut(assignment, "=")
		if !ok || name == "" || raw == "" {
			return nil, fmt.Errorf(
				"component %q must be NAME=VALUE",
				curlblocks.DisplayText(assignment))
		}
		component, err := parsePaddingComponent(name)
		if err != nil {
			return nil, err
		}
		if _, exists := out[component]; exists {
			return nil, fmt.Errorf("%s is assigned more than once", component)
		}
		cadence, err := parsePaddingCadence(raw)
		if err != nil {
			return nil, fmt.Errorf(
				"%s=%q: %w", component, curlblocks.DisplayText(raw), err)
		}
		out[component] = cadence
	}
	return out, nil
}

func parsePaddingCadence(raw string) (paddingCadence, error) {
	if strings.Count(raw, ":") > 1 {
		return paddingCadence{}, fmt.Errorf("want N, START:STEP, or -N")
	}
	if startRaw, stepRaw, ok := strings.Cut(raw, ":"); ok {
		start, err := strconv.Atoi(startRaw)
		if err != nil || start < 0 {
			return paddingCadence{}, fmt.Errorf("start must be a non-negative integer")
		}
		step, err := strconv.Atoi(stepRaw)
		if err != nil {
			return paddingCadence{}, fmt.Errorf("step must be an integer")
		}
		return paddingCadence{start: start, step: step}, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return paddingCadence{}, fmt.Errorf("want N, START:STEP, or -N")
	}
	if value >= 0 {
		return paddingCadence{start: value}, nil
	}
	if value == minInt() {
		return paddingCadence{}, fmt.Errorf("auto-start step is too large")
	}
	return paddingCadence{step: -value, auto: true}, nil
}

func (c paddingCadence) value(position, requests int) (int, error) {
	if c.auto {
		factor := requests - 1 - position
		if factor > 0 && c.step > maxInt()/factor {
			return 0, fmt.Errorf("value exceeds the maximum integer")
		}
		return factor * c.step, nil
	}
	if c.step == 0 || position == 0 {
		return c.start, nil
	}
	if c.step > 0 {
		if position > (maxInt()-c.start)/c.step {
			return 0, fmt.Errorf("value exceeds the maximum integer")
		}
		return c.start + position*c.step, nil
	}
	if c.step == minInt() {
		return 0, nil
	}
	decrement := -c.step
	if position > c.start/decrement {
		return 0, nil
	}
	return c.start - position*decrement, nil
}

func parsePaddingOverride(
	raw string,
	requests int,
) (int, map[paddingComponent]int, error) {
	positionRaw, spec, ok := strings.Cut(raw, ":")
	if !ok {
		return 0, nil, fmt.Errorf("want POSITION:SPEC")
	}
	position, err := strconv.Atoi(positionRaw)
	if err != nil || position < 0 {
		return 0, nil, fmt.Errorf("position must be a non-negative integer")
	}
	if position >= requests {
		return 0, nil, fmt.Errorf(
			"position %d is outside the %d trial requests", position, requests)
	}
	values := make(map[paddingComponent]int)
	if spec == "" {
		return 0, nil, fmt.Errorf("SPEC must not be empty")
	}
	for assignment := range strings.SplitSeq(spec, ",") {
		name, valueRaw, ok := strings.Cut(assignment, "=")
		if !ok || name == "" || valueRaw == "" {
			return 0, nil, fmt.Errorf(
				"component %q must be NAME=N",
				curlblocks.DisplayText(assignment))
		}
		component, err := parsePaddingComponent(name)
		if err != nil {
			return 0, nil, err
		}
		if _, exists := values[component]; exists {
			return 0, nil, fmt.Errorf("%s is assigned more than once", component)
		}
		value, err := strconv.Atoi(valueRaw)
		if err != nil || value < 0 {
			return 0, nil, fmt.Errorf("%s must be a non-negative integer", component)
		}
		values[component] = value
	}
	return position, values, nil
}

func parsePaddingComponent(raw string) (paddingComponent, error) {
	switch raw {
	case "query":
		return paddingQuery, nil
	case "headers":
		return paddingHeaders, nil
	case "body":
		return paddingBody, nil
	default:
		return 0, fmt.Errorf(
			"unknown component %q; want query, headers, or body",
			curlblocks.DisplayText(raw))
	}
}

func setPadding(
	padding *tth2.RequestPadding,
	component paddingComponent,
	value int,
) {
	switch component {
	case paddingQuery:
		padding.URLParams = value
	case paddingHeaders:
		padding.Headers = value
	case paddingBody:
		padding.BodyParams = value
	}
}

func (c paddingComponent) String() string {
	switch c {
	case paddingQuery:
		return "query"
	case paddingHeaders:
		return "headers"
	default:
		return "body"
	}
}

func maxInt() int { return int(^uint(0) >> 1) }

func minInt() int { return -maxInt() - 1 }
