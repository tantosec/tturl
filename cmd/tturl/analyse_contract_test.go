package main

import (
	"testing"
)

func TestResolveAnalyseDefaultWorkloads(t *testing.T) {
	tests := []struct {
		width      int
		cycles     int
		operations int
	}{
		{2, 150, 600},
		{3, 100, 900},
		{4, 75, 1200},
		{5, 75, 1875},
		{6, 50, 1800},
		{7, 50, 2450},
		{8, 50, 3200},
	}
	for _, test := range tests {
		workload, err := resolveAnalyseWorkload(test.width, 0)
		if err != nil {
			t.Fatalf("width %d: %v", test.width, err)
		}
		if workload.Cycles != test.cycles ||
			workload.Trials != test.width*test.cycles ||
			workload.Operations != test.operations ||
			workload.LowerSensitivity {
			t.Errorf("width %d workload = %+v", test.width, workload)
		}
	}
}

func TestResolveAnalyseWorkloadBoundaries(t *testing.T) {
	if _, err := resolveAnalyseWorkload(1, 0); err == nil {
		t.Fatal("accepted unsupported width")
	}
	if _, err := resolveAnalyseWorkload(8, 10); err == nil {
		t.Fatal("accepted workload below the pair-resolution floor")
	}
	workload, err := resolveAnalyseWorkload(8, 11)
	if err != nil {
		t.Fatal(err)
	}
	if !workload.LowerSensitivity {
		t.Fatalf("workload = %+v, want lower-sensitivity warning", workload)
	}
	if _, err := resolveAnalyseWorkload(
		8, analyseMaximumOperations/(8*8)+1,
	); err == nil {
		t.Fatal("accepted workload above the operation cap")
	}
}

func TestValidateAnalyseWarmup(t *testing.T) {
	if err := validateAnalyseWarmup(8, 100, 32); err != nil {
		t.Fatalf("bounded warmup: %v", err)
	}
	if err := validateAnalyseWarmup(8, 200, 32); err == nil {
		t.Fatal("accepted warmup above the operation cap")
	}
}
