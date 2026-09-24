package main

import (
	"errors"
	"fmt"
)

const (
	analyseMaximumOperations   = 50_000
	analysePairPReportingFloor = 1e-300
)

var errAnalyseWorkloadTooLarge = errors.New("analyse workload too large")

type analyseWorkload struct {
	Width            int
	Cycles           int
	Trials           int
	Operations       int
	DefaultCycles    int
	LowerSensitivity bool
}

func resolveAnalyseWorkload(width, requestedCycles int) (
	analyseWorkload, error,
) {
	defaultCycles := analyseDefaultCycles(width)
	if defaultCycles == 0 {
		return analyseWorkload{}, fmt.Errorf(
			"analyse supports 2..8 requests; got %d; use a smaller hypothesis or "+
				"run race or measure for descriptive evidence", width)
	}
	cycles := requestedCycles
	if cycles == 0 {
		cycles = defaultCycles
	}
	minimum := analysePairResolutionFloor(width)
	if cycles < minimum {
		return analyseWorkload{}, fmt.Errorf(
			"--cycles must be at least %d for %d requests; got %d",
			minimum, width, cycles)
	}
	trials, ok := checkedProduct(cycles, width)
	if !ok {
		return analyseWorkload{}, errAnalyseWorkloadTooLarge
	}
	operations, ok := checkedProduct(trials, width)
	if !ok {
		return analyseWorkload{}, errAnalyseWorkloadTooLarge
	}
	if operations > analyseMaximumOperations {
		return analyseWorkload{}, fmt.Errorf(
			"%w: requires %d measured request operations; limit is %d",
			errAnalyseWorkloadTooLarge,
			operations, analyseMaximumOperations)
	}
	return analyseWorkload{
		Width: width, Cycles: cycles, Trials: trials, Operations: operations,
		DefaultCycles: defaultCycles, LowerSensitivity: cycles < defaultCycles,
	}, nil
}

func analyseDefaultCycles(width int) int {
	switch width {
	case 2:
		return 150
	case 3:
		return 100
	case 4, 5:
		return 75
	case 6, 7, 8:
		return 50
	default:
		return 0
	}
}

func validateAnalyseWarmup(width, warmup, connections int) error {
	trials, ok := checkedProduct(warmup, connections)
	if !ok {
		return errors.New("analyse warmup is too large")
	}
	operations, ok := checkedProduct(trials, width)
	if !ok || operations > analyseMaximumOperations {
		return fmt.Errorf(
			"analyse warmup requires more than %d request operations",
			analyseMaximumOperations)
	}
	return nil
}
