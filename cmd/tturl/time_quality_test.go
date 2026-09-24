package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/timing"
)

func TestTimeQualityReconcilesPlannedRequestsAndObservedResponses(t *testing.T) {
	for _, test := range []struct {
		name                             string
		acquisition, unlimited, withheld bool
		attempted, unattempted           []uint64
	}{
		{"acquisition", true, false, false, []uint64{0, 0, 0}, []uint64{3, 3, 3}},
		{"sequential_suffix_and_future_trials", false, false, false, []uint64{1, 0, 0}, []uint64{2, 3, 3}},
		{"unlimited_accounted_members", false, true, false, []uint64{1, 0, 0}, []uint64{0, 1, 1}},
		{"withheld_trial_actual_counts", false, false, true, []uint64{1, 0, 0}, []uint64{2, 3, 3}},
	} {
		for _, format := range []string{"text", "json"} {
			t.Run(test.name+"_"+format, func(t *testing.T) {
				var output bytes.Buffer
				count := "3"
				if test.unlimited {
					count = "unlimited"
				}
				args := []string{
					"--trials", count, "--arrange", "none", "--report", format,
					"--block", "https://example.test/a", "--name", "first",
					"--block", "https://example.test/b", "--name", "second",
					"--block", "https://example.test/c", "--name", "third",
				}
				anchor := time.Date(2026, time.September, 18, 0, 0, 0, 0, time.UTC)
				runner := func(_ context.Context, _ *timing.Resolved, emit func(timing.Event) error) (timing.Result, error) {
					result := timing.Result{StartedAt: anchor, Completion: timing.CompletionFailed}
					for id := range 3 {
						result.Requests = append(result.Requests, timing.RequestAccounting{
							RequestID: id, Attempted: test.attempted[id], Unattempted: test.unattempted[id],
						})
					}
					if test.acquisition {
						result.Failure = &timing.Failure{
							Phase: timing.PhaseAcquisition, Code: "acquisition", Cause: errors.New("acquisition unavailable"),
						}
						result.Accounting = timing.Accounting{
							TrialsUnattempted: 3, RequestOperationsUnattempted: 9, AcquisitionFailures: 1,
						}
						return result, result.Failure
					}
					outcomes := []timing.Outcome{
						{
							RequestID: 0, Attempted: true, Response: timing.Response{Complete: true, Status: 200},
							Timing: timing.Timing{
								InitialRelease:       timing.Offset{NS: 1, Present: true},
								FinalResponseHeaders: timing.Offset{NS: 2, Present: true},
								ResponseComplete:     timing.Offset{NS: 3, Present: true},
								Duration:             timing.Offset{NS: 1, Present: true},
							},
						},
						{RequestID: 1},
						{RequestID: 2},
					}
					if !test.withheld {
						if err := emit(timing.Event{Trial: &timing.Trial{StartedAt: anchor, Outcomes: outcomes}}); err != nil {
							return result, err
						}
					}
					result.Accounting = timing.Accounting{
						TrialsAttempted: 1, TrialsIncomplete: 1,
						RequestOperationsAttempted: 1, RequestOperationsComplete: 1,
						RequestOperationsUnattempted: 8, TrialsUnattempted: 2,
					}
					if test.unlimited {
						result.Accounting.RequestOperationsUnattempted = 2
						result.Accounting.TrialsUnattempted = 0
						result.Completion = timing.CompletionStopped
						return result, nil
					}
					result.Failure = &timing.Failure{Phase: timing.PhaseRun, Code: "timeout", Cause: context.DeadlineExceeded}
					return result, result.Failure
				}
				err := runTimeCommandForPlatform(t.Context(), commandByID(commandTime), args,
					commandOutput{stdout: &output, stderr: &bytes.Buffer{}}, "linux", runner)
				if (err == nil) != test.unlimited {
					t.Fatalf("unexpected terminal error: %v", err)
				}
				if format == "text" {
					text := output.String()
					for index := range 3 {
						heading := fmt.Sprintf("Request %d: ", index)
						start := strings.Index(text, heading)
						if start < 0 {
							t.Fatalf("missing summary %s", heading)
						}
						section := text[start:]
						if end := strings.Index(section[1:], "\nRequest "); end >= 0 {
							section = section[:end+1]
						}
						want := fmt.Sprintf("%d attempted; %d unattempted", test.attempted[index], test.unattempted[index])
						if !strings.Contains(section, want) || !strings.Contains(section, "incomplete responses: 0") {
							t.Fatalf("summary lacks reconciled quality:\n%s", section)
						}
					}
				} else {
					decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
					found := false
					for {
						var record map[string]any
						if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
							break
						} else if err != nil {
							t.Fatal(err)
						}
						if record["kind"] != "result" {
							continue
						}
						found = true
						var attempted, unattempted float64
						for index, value := range record["summaries"].([]any) {
							quality := value.(map[string]any)["quality"].(map[string]any)
							attempted += quality["attempted"].(float64)
							unattempted += quality["unattempted"].(float64)
							if quality["attempted"] != float64(test.attempted[index]) ||
								quality["unattempted"] != float64(test.unattempted[index]) ||
								quality["incomplete_responses"] != float64(0) {
								t.Fatalf("quality=%v", quality)
							}
						}
						accounting := record["accounting"].(map[string]any)
						if attempted != accounting["request_operations_attempted"] ||
							unattempted != accounting["request_operations_unattempted"] {
							t.Fatalf("request totals attempted=%v unattempted=%v accounting=%v", attempted, unattempted, accounting)
						}
					}
					if !found {
						t.Fatal("missing result quality")
					}
				}
			})
		}
	}
}

func TestTimeQualitySeparatesReleasedFailuresAndResponseEvidence(t *testing.T) {
	reporter := &timeReport{}
	reporter.recordQuality(timing.Outcome{RequestID: 0})
	reporter.recordQuality(timing.Outcome{RequestID: 0, Attempted: true})
	reporter.recordQuality(timing.Outcome{RequestID: 0, Response: timing.Response{Status: 200}})
	quality := reporter.ensureQuality(0)
	if quality.Incomplete != 2 || quality.Unattempted != 2 || quality.Attempted != 1 {
		t.Fatalf("quality=%+v", quality)
	}
}
