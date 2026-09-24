package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDryRunPreviewsEveryRequestCommandWithoutNetwork(t *testing.T) {
	keyLog := filepath.Join(t.TempDir(), "keys.log")
	t.Setenv("SSLKEYLOGFILE", keyLog)
	tests := []struct {
		name string
		run  func(context.Context, []string, commandOutput) error
		args []string
	}{
		{
			name: "race", run: runRace,
			args: []string{"--trials", "2", "--connections", "1"},
		},
		{
			name: "measure", run: runMeasure,
			args: []string{
				"--trials", "2", "--connections-fit", "--batch-rate-max", "10/s",
				"--connections-fit-max", "4",
			},
		},
		{
			name: "analyse", run: runAnalyse,
			args: []string{
				"--cycles", "6", "--connections-fit", "--batch-rate-max", "10/s",
				"--connections-fit-max", "4",
			},
		},
		{
			name: "detect", run: runDetect,
			args: []string{
				"--direction", "late", "--comparisons-max", "2",
				"--connections", "1",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			args := append([]string{
				"--dry-run", "--run-timeout", "1ns",
			}, test.args...)
			args = append(args,
				"--block", "https://no-network.invalid/a",
				"--block", "https://no-network.invalid/b")
			if err := test.run(t.Context(), args, commandOutput{
				stdout: &stdout, stderr: io.Discard,
			}); err != nil {
				t.Fatalf("dry run: %v", err)
			}
			output := stdout.String()
			assertTextLinesAtMost(t, output, textWidth)
			for _, want := range []string{
				"Preview: network-free dry run", "Requests", "Experiment",
				"Run timeout: 1ns",
			} {
				if !strings.Contains(output, want) {
					t.Errorf("preview lacks %q:\n%s", want, output)
				}
			}
			for _, unwanted := range []string{
				"Results", "Measurement", "Analysis result", "Detection result",
			} {
				if strings.Contains(output, unwanted) {
					t.Errorf("preview contains terminal section %q:\n%s",
						unwanted, output)
				}
			}
			if strings.Contains(output, "--connections-fit-max") &&
				!strings.Contains(output, "unresolved until execution") {
				t.Errorf("automatic fit was not marked unresolved:\n%s", output)
			}
		})
	}
	if _, err := os.Stat(keyLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry runs opened SSLKEYLOGFILE: %v", err)
	}
}

func TestRequestCommandsFitWithFinitePacingLimits(t *testing.T) {
	commands := []struct {
		name string
		run  func(context.Context, []string, commandOutput) error
		work []string
	}{
		{"race", runRace, []string{"--trials", "2"}},
		{"measure", runMeasure, []string{"--trials", "2"}},
		{"analyse", runAnalyse, []string{"--cycles", "6"}},
		{"detect", runDetect, []string{"--direction", "late", "--comparisons-max", "2"}},
	}
	limits := []struct {
		name  string
		args  []string
		valid bool
	}{
		{"batch", []string{"--batch-rate-max", "10/s"}, true},
		{"requests", []string{"--request-rate-max", "20/s"}, true},
		{"both", []string{"--batch-rate-max", "10/s", "--request-rate-max", "20/s"}, true},
		{"unlimited requests", []string{"--batch-rate-max", "10/s", "--request-rate-max", "unlimited"}, true},
		{"unlimited batches", []string{"--batch-rate-max", "unlimited", "--request-rate-max", "20/s"}, true},
		{"both unlimited", []string{"--batch-rate-max", "unlimited", "--request-rate-max", "unlimited"}, false},
		{"defaults", nil, false},
	}
	for _, command := range commands {
		for _, limit := range limits {
			t.Run(command.name+"/"+limit.name, func(t *testing.T) {
				args := []string{"--dry-run", "--connections-fit", "--connections-fit-max", "4"}
				args = append(args, command.work...)
				args = append(args, limit.args...)
				args = append(args, "--block", "https://no-network.invalid/a",
					"--block", "https://no-network.invalid/b")
				err := command.run(t.Context(), args, commandOutput{
					stdout: io.Discard, stderr: io.Discard,
				})
				if (err == nil) != limit.valid {
					t.Fatalf("finite pacing prerequisite: error = %v; valid = %v", err, limit.valid)
				}
			})
		}
	}
}

func TestDryRunReportsResolvedAndUnresolvedDelivery(t *testing.T) {
	t.Run("explicit", func(t *testing.T) {
		var output bytes.Buffer
		err := runMeasure(t.Context(), []string{
			"--dry-run", "--connections", "8", "--trials", "2",
			"--arrange", "none",
			"--block", "https://example.invalid/a",
			"--block", "https://example.invalid/b",
		}, commandOutput{stdout: &output, stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		flat := strings.Join(strings.Fields(output.String()), " ")
		for _, want := range []string{
			"up to 2 active for this run; configured maximum 8",
			"Active streams: up to 4 (2 connections x 2 request streams/connection).",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("resolved preview lacks %q:\n%s", want, &output)
			}
		}
	})

	t.Run("automatic", func(t *testing.T) {
		var output bytes.Buffer
		err := runMeasure(t.Context(), []string{
			"--dry-run", "--connections-fit", "--batch-rate-max", "10/s", "--connections-fit-max", "8",
			"--warmup", "1", "--trials", "4",
			"--block", "https://example.invalid/a",
			"--block", "https://example.invalid/b",
		}, commandOutput{stdout: &output, stderr: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		flat := strings.Join(strings.Fields(output.String()), " ")
		for _, want := range []string{
			"Connections: unresolved until execution",
			"configured fit maximum 8",
			"finite work can use at most 2",
			"total unresolved until execution",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("unresolved preview lacks %q:\n%s", want, &output)
			}
		}
	})
}

func TestDryRunSupportsVerboseFileOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preview.txt")
	body := strings.Repeat("x", maxBodyDisplay+1)
	err := runRace(t.Context(), []string{
		"--dry-run", "--verbose", "--output", path,
		"--block", "https://example.invalid/a", "--data", body,
		"--block", "https://example.invalid/b",
	}, commandOutput{stdout: io.Discard, stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // Test-owned temporary path.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(body)) ||
		bytes.Contains(data, []byte("bytes omitted")) {
		t.Fatalf("verbose preview did not retain the request body:\n%s", data)
	}
}

func TestDryRunRejectsJSONBeforeOpeningOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist.jsonl")
	err := runRace(t.Context(), []string{
		"--dry-run", "--report", "json", "--output", path,
		"--block", "https://example.invalid/a",
		"--block", "https://example.invalid/b",
	}, commandOutput{stdout: io.Discard, stderr: io.Discard})
	var usageErr *usageError
	if !errors.As(err, &usageErr) || !strings.Contains(err.Error(), "--dry-run") {
		t.Fatalf("error = %v, want dry-run usage error", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid dry run opened output: %v", err)
	}
}

func TestDryRunMaterialisationFailureDoesNotOpenOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist.txt")
	err := runRace(t.Context(), []string{
		"--dry-run", "--output", path,
		"--block", "https://first.invalid/a",
		"--block", "https://second.invalid/b",
	}, commandOutput{stdout: io.Discard, stderr: io.Discard})
	if err == nil {
		t.Fatal("dry run accepted requests from different origins")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed materialisation opened output: %v", err)
	}
}

func TestDryRunExpandsRequestsAndReportsLocalInputFailure(t *testing.T) {
	var output bytes.Buffer
	err := runRace(t.Context(), []string{
		"--dry-run", "--block", "https://example.invalid/V",
		"--vary", "V={one,two}",
	}, commandOutput{stdout: &output, stderr: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Total: 2 requests.") ||
		!strings.Contains(output.String(), "2 requests = 2 request operations") {
		t.Fatalf("expanded preview is incomplete:\n%s", &output)
	}

	path := filepath.Join(t.TempDir(), "must-not-exist.txt")
	missing := filepath.Join(t.TempDir(), "missing-values.txt")
	err = runRace(t.Context(), []string{
		"--dry-run", "--output", path,
		"--block", "https://example.invalid/V", "--vary", "V=@" + missing,
	}, commandOutput{stdout: io.Discard, stderr: io.Discard})
	if err == nil {
		t.Fatal("dry run accepted a missing vary input")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed local input opened output: %v", statErr)
	}
}

func TestDetectPreviewSchedulingFacts(t *testing.T) {
	strategies := []detectStrategy{
		detectStrategyPeerFirst, detectStrategyBaselineConfirmed,
		detectStrategyBaselineReserved, detectStrategyRollingPeerFirst,
		detectStrategyRollingBaselineConfirmed, detectStrategyRollingBaselineReserved,
	}
	policies := []struct {
		name        string
		fit         bool
		connections int
		comparisons int
		width       int
		want        int
	}{
		{"fixed", false, 8, 100, 2, 8},
		{"budget", false, 8, 3, 2, 3},
		{"participations", false, 8, -1, math.MaxInt / 2, 2},
		{"fitted", true, 0, 100, 2, 0},
	}
	for _, strategy := range strategies {
		for _, policy := range policies {
			t.Run(string(strategy)+"/"+policy.name, func(t *testing.T) {
				selection := detectStrategySelection{resolved: strategy}
				plan := detectExperimentPlan{
					ConnectionLimit: policy.connections, ComparisonsMax: policy.comparisons,
					WidthEffective: policy.width,
					Delivery:       deliveryPlan{pathFitUnresolved: policy.fit},
				}
				plan.resolveScheduling(selection)
				if plan.InFlightComparisonLimit != policy.want ||
					plan.Delivery.pathFitUnresolved != policy.fit {
					t.Fatalf("scheduling = %+v; want limit %d, unresolved %v",
						plan, policy.want, policy.fit)
				}
				wantUpdate, wantDependence := "complete-atomic-wave", "arbitrary-within-atomic-wave"
				if strings.HasPrefix(string(strategy), "rolling-") {
					wantUpdate = "maximal-contiguous-dispatch-prefix"
					wantDependence = "conditional-across-dispatch-indices"
				}
				if plan.EvidenceUpdatePolicy != wantUpdate || plan.DependencePremise != wantDependence {
					t.Fatalf("strategy facts = %+v", plan)
				}
			})
		}
	}
}
