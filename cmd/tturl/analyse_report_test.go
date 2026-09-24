package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestReportAnalyseOutcomeFixtures(t *testing.T) {
	noDetection := analyseOffline(
		analyseOrderedEvidence(2, 6, func(cycle int) []int {
			if cycle%2 == 0 {
				return []int{0, 1}
			}
			return []int{1, 0}
		}),
		analyseConfig{},
	)
	unavailable := analyseOffline(
		analyseOrderedEvidence(3, 6, func(int) []int { return []int{0, 1, 2} }),
		analyseConfig{},
	)
	tests := []struct {
		name   string
		result analyseRunResult
		want   []string
	}{
		{
			name: "no detection", result: noDetection,
			want: []string{
				"Finding: No request-dependent arrival-order effect detected.",
				"Exact randomisation: p=1 > 0.05.",
				"Pair comparisons: not tested without a field-wide detection.",
				"This is not evidence of equivalence.",
			},
		},
		{
			name: "unavailable", result: unavailable,
			want: []string{
				"Finding unavailable:",
				"7 complete cycles are required for 3 requests; got 6",
				"Code: insufficient_cycles.",
				"No statistical result or pair comparison was produced.",
				"Execution",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			labels := []string{"A", "B"}
			if test.result.Width == 3 {
				labels = append(labels, "C")
			}
			if err := reportAnalyse(
				&output, test.result, labels,
			); err != nil {
				t.Fatal(err)
			}
			flat := strings.Join(strings.Fields(output.String()), " ")
			for _, want := range test.want {
				if !strings.Contains(flat, want) {
					t.Errorf("report missing %q:\n%s", want, output.String())
				}
			}
			if test.name == "unavailable" {
				for _, absent := range []string{
					heading("HTTP status outcomes"), heading("Position check"),
					heading("Method"),
				} {
					if strings.Contains(output.String(), absent) {
						t.Errorf("unavailable report contains unsupported section:\n%s",
							output.String())
					}
				}
			}
		})
	}
}

func TestReportAnalyseExperimentContext(t *testing.T) {
	t.Parallel()
	result := analyseOffline(
		analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} }),
		analyseConfig{ConnectionLimit: 2},
	)
	result.Plan.DefaultCycles = 150
	result.Plan.LowerSensitivity = true
	context := analyseReportContext{
		Delivery: deliveryPlan{
			connectionLimit: 2, connectionCeiling: 4,
			connectionSelection:    connectionExplicit,
			peerStreamLimitIgnored: true,
		},
		WarmupTrialsPerConnection: 3,
		BodyRelease: bodyReleasePlan{
			releaseDelay:      5 * time.Millisecond,
			bodyBytesWithheld: 2, hasBodyBytes: true,
		},
		Insecure: true,
	}
	var output bytes.Buffer
	if err := reportAnalyseDocumentWithContext(
		&output, result, []string{"A", "B"}, true, context,
	); err != nil {
		t.Fatal(err)
	}
	report := output.String()
	for _, want := range []string{
		"Design: 6 balanced rotation cycles (12 trials, 24 measured request",
		"Arrangement: rotate; each request occupies every position once per cycle.",
		"Priming: 3 priming attempts per connection (12 initial priming request",
		"Sensitivity: 6 cycles is below the 150-cycle modelled default",
		"Power: the fixed workload is a power choice, not a guarantee",
		"Connections: up to 2 active for this run; configured maximum 4",
		"Streams: intentionally ignore the peer's advertised concurrent-stream",
		"Body release: withhold up to 2 trailing bytes per body; " +
			"delay release by 5ms.",
		"TLS: certificate verification disabled (--insecure).",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("analyse experiment lacks %q:\n%s", want, report)
		}
	}
	assertTextLinesAtMost(t, report, textWidth)
}

func TestReportAnalyseErrors(t *testing.T) {
	result := analyseOffline(
		analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} }),
		analyseConfig{},
	)
	if err := reportAnalyse(&bytes.Buffer{}, result, []string{"A"}); err == nil {
		t.Fatal("report accepted a missing label")
	}
	want := errors.New("write failed")
	if err := reportAnalyse(
		failingWriter{err: want}, result, []string{"A", "B"},
	); !errors.Is(err, want) {
		t.Fatalf("write error = %v, want %v", err, want)
	}
}

func TestAnalysePairPValueUpperBound(t *testing.T) {
	if got := analysePairPValue("adjusted p", 1e-300, true); got != "adjusted p<=1e-300" {
		t.Fatalf("bounded p-value = %q", got)
	}
	if got := analysePairPValue("adjusted p", 0.01, false); got != "adjusted p=0.01" {
		t.Fatalf("exact p-value = %q", got)
	}
}

func TestReportAnalyseMonteCarloNoDetectionPValue(t *testing.T) {
	var output bytes.Buffer
	reportAnalyseGlobalEvidence(&output, analyseGlobalResult{
		Outcome: analyseNoDetection, Alpha: analyseAlpha,
		Calibration: analyseCalibration{
			Method: "monte_carlo_randomisation", Draws: analyseMonteCarloDraws,
			Extremes: 999, PValue: 0.1, PValueKind: "monte_carlo",
		},
	})
	want := "Overall test across all requests:\n" +
		"  Monte Carlo randomisation: p=0.1 > 0.05.\n" +
		"  Reference distribution: 9,999 randomisations sampled.\n"
	if output.String() != want {
		t.Fatalf("global evidence = %q, want %q", output.String(), want)
	}
}

func TestReportAnalyseResponseReview(t *testing.T) {
	evidence := analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} })
	for cycle := range evidence.Cycles {
		for trial := range evidence.Cycles[cycle].Trials {
			if (cycle+trial)%2 == 0 {
				evidence.Cycles[cycle].Trials[trial].Statuses[1] = 500
			}
		}
	}
	result := analyseOffline(evidence, analyseConfig{})
	var output bytes.Buffer
	if err := reportAnalyse(&output, result, []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"B       200 x 6, 500 x 6",
		"B's rank distribution combines different HTTP",
		"status distributions differ across A and B",
		"linking arrival-order behaviour to request identity",
		"non-2xx responses contributed ranks for B",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("report missing %q:\n%s", want, output.String())
		}
	}
}

func TestReportAnalyseStableDifferentStatuses(t *testing.T) {
	evidence := analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} })
	for cycle := range evidence.Cycles {
		for trial := range evidence.Cycles[cycle].Trials {
			evidence.Cycles[cycle].Trials[trial].Statuses[1] = 404
		}
	}
	result := analyseOffline(evidence, analyseConfig{})
	var output bytes.Buffer
	if err := reportAnalyse(&output, result, []string{"control", "target"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"control  HTTP 200 in all 12 trials",
		"target   HTTP 404 in all 12 trials",
		"status distributions differ across control and",
		"target; inspect responses",
		"linking arrival-order behaviour to request identity",
		"non-2xx responses contributed ranks for target",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("report missing %q:\n%s", want, output.String())
		}
	}
}

func TestReportAnalyseWidthEightShowsEverySupportedPair(t *testing.T) {
	result := analyseOffline(
		analyseOrderedEvidence(8, 11, func(int) []int {
			return []int{0, 1, 2, 3, 4, 5, 6, 7}
		}),
		analyseConfig{RandomisationSeed: 0x5749445448},
	)
	if result.Global.Outcome != analyseDifference {
		t.Fatalf("global = %+v", result.Global)
	}
	labels := []string{"A", "B", "C", "D", "E", "F", "G", "H"}
	var output bytes.Buffer
	if err := reportAnalyse(&output, result, labels); err != nil {
		t.Fatal(err)
	}
	wantPairs := make(map[string][]string)
	for earlier := range labels {
		for later := earlier + 1; later < len(labels); later++ {
			key := labels[earlier] + "\x00" + labels[later]
			wantPairs[key] = []string{
				"88/88", fmt.Sprintf("%.2f", float64(later-earlier)), "0.02734",
			}
		}
	}
	gotPairs := make(map[string][]string)
	inPairs := false
	for line := range strings.SplitSeq(output.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 2 && fields[0] == "earlier" && fields[1] == "later" {
			inPairs = true
			continue
		}
		if !inPairs {
			continue
		}
		if len(fields) == 0 {
			break
		}
		if len(fields) != 5 {
			t.Fatalf("malformed pair row %q", line)
		}
		key := fields[0] + "\x00" + fields[1]
		if _, exists := gotPairs[key]; exists {
			t.Fatalf("duplicate pair row %q", line)
		}
		gotPairs[key] = fields[2:]
	}
	if len(gotPairs) != len(wantPairs) {
		t.Fatalf("width-eight report has %d supported pairs, want %d:\n%s",
			len(gotPairs), len(wantPairs), output.String())
	}
	for key, want := range wantPairs {
		got, ok := gotPairs[key]
		if !ok || strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("pair %q = %v, want %v", key, got, want)
		}
	}
	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	for index, line := range lines {
		if len([]rune(line)) > textWidth {
			t.Errorf("line %d has %d columns: %s", index+1, len([]rune(line)), line)
		}
	}
}
