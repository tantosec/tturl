package main

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/tth2"
)

func renderTextRace(
	t *testing.T,
	run RunRecord,
	labels []string,
	trials []*tth2.Trial,
	summary SummaryRecord,
) string {
	t.Helper()
	if run.TrialsRequested == 0 {
		run.TrialsRequested = run.Trials
	}
	if run.TrialBatchWidth == 0 {
		run.TrialBatchWidth = len(labels)
	}
	if run.Warmup > 0 && run.WarmupBatchWidth == 0 {
		run.WarmupBatchWidth = run.TrialBatchWidth
	}
	if run.ResponseLimits == (responseLimits{}) {
		run.ResponseLimits = defaultResponseLimits()
	}
	var b bytes.Buffer
	reporter := newTextRaceReporter(&b)
	if err := reporter.Run(run); err != nil {
		t.Fatalf("Run: %v", err)
	}
	refs := projectRunReferences(labels, raceReferenceWidth())
	for _, ref := range refs.refs {
		if err := reporter.Request(RequestRecord{Reference: ref}); err != nil {
			t.Fatalf("Request: %v", err)
		}
	}
	for _, trial := range trials {
		if err := reporter.Trial(TrialRecord{Trial: trial}); err != nil {
			t.Fatalf("Trial: %v", err)
		}
	}
	if summary.Execution == (raceExecution{}) {
		priming, measured := executionBatches{}, executionBatches{}
		connections := make(map[int]bool)
		for _, trial := range trials {
			if trial.Conn >= 0 {
				connections[trial.Conn] = true
			}
			batches := &measured
			if trial.Warmup {
				batches = &priming
			}
			batches.Attempted++
			if trial.Err == nil {
				batches.RankComplete++
			} else {
				batches.Incomplete++
			}
		}
		summary.Execution = mustProjectRaceExecution(t,
			run.Trials, run.Connections, run.TrialBatchWidth,
			run.WarmupBatchWidth, connections, priming, measured)
	}
	if err := reporter.Summary(summary); err != nil {
		t.Fatalf("Summary: %v", err)
	}
	return b.String()
}

func mustProjectRaceExecution(
	t *testing.T,
	maximum, plannedConnections, measuredWidth, primingWidth int,
	connections map[int]bool,
	priming, measured executionBatches,
) raceExecution {
	t.Helper()
	execution, err := projectRaceExecution(
		maximum, plannedConnections, measuredWidth, primingWidth,
		connections, priming, measured)
	if err != nil {
		t.Fatalf("project race execution: %v", err)
	}
	return execution
}

func response(status int, bodyBytes, contentLength int64) tth2.Result {
	return tth2.Result{
		Response: &tth2.Response{
			StatusCode:    status,
			ContentLength: contentLength,
			Body:          tth2.ResponseBody{BytesReceived: bodyBytes},
		},
	}
}

func TestTextRaceReporterSingleTrialIncludesPosition(t *testing.T) {
	results := []tth2.Result{
		response(200, 31, 31),
		response(200, 24, 24),
		response(409, 18, 30),
	}
	results[0].ArrivalRank = 1
	results[1].ArrivalRank = 0
	results[2].ArrivalRank = 2
	got := renderTextRace(t,
		RunRecord{
			Trials: 1, Connections: 1, ArrangementPolicy: arrangeRandom,
		},
		[]string{"A", "B", "TOKEN=c"},
		[]*tth2.Trial{{
			BatchResult: tth2.BatchResult{
				Results: results, ArrivalOrder: []int{1, 0, 2},
			},
			Arrangement: []int{2, 0, 1}, Index: 0, Cycle: -1, Shift: -1,
		}},
		SummaryRecord{ResetCount: make([]int, 3)},
	)
	const want = `
---------------------------------- Experiment ----------------------------------

Trials: 1 x 3 requests = 3 request operations.
Arrangement: random.
Pacing: batch-rate ceiling unlimited; request-rate ceiling unlimited; minimum
        batch-start interval unconstrained.
Connections: up to 1 active (default).
Streams: honour the peer's advertised concurrent-stream limit.
Response limits: 8MiB accepted body per stream; 30s transport time per batch.
Run timeout: unlimited for live request execution.

----------------------------------- Results ------------------------------------

  rank  pos  request  response
     0    2  B        200  24 B
     1    1  A        200  31 B
     2    0  TOKEN=c  409  18 B (Content-Length: 30)
Status: 200 x 2, 409 x 1

---------------------------------- Execution -----------------------------------

Trials: 1/1 attempted; 1 rank-complete; 0 incomplete; 0 unattempted.
Connections: 1 planned; 1 observed; 0 replacements.
Request operations: 3 attempted; 3 rank-complete; 3 retained.
`
	if got != want {
		t.Errorf("text report mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestReportRaceExperimentRoundedRotation(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := reportRaceExperiment(&out, RunRecord{
		Trials: 4, TrialsRequested: 3, Warmup: 1,
		TrialBatchWidth: 2, WarmupBatchWidth: 1, Connections: 2,
		ArrangementPolicy: arrangeRotate,
		Delivery: deliveryPlan{
			connectionCeiling:   4,
			connectionLimit:     2,
			connectionSelection: connectionExplicit,
		},
		BodyBytesWithheld: 1,
		bodyRelease: bodyReleasePlan{
			bodyBytesWithheld: 1, hasBodyBytes: true,
		},
		Insecure: true,
		Padding: []PaddingRecord{
			{Position: 0, URLParams: 1, Headers: 1},
			{Position: 1, Headers: 1},
		},
	})
	if err != nil {
		t.Fatalf("reportRaceExperiment: %v", err)
	}
	flat := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{
		"Trials: 3 requested; 4 planned as 2 complete rotation cycles " +
			"(8 request operations).",
		"Priming: 1 priming attempt per connection (2 priming request operations).",
		"Arrangement: rotate.",
		"Padding: outbound positions 0..1; query parameters [1 0]; " +
			"headers [1 1].",
		"Connections: up to 2 active for this run; configured maximum 4 " +
			"(--connections).",
		"Body release: withhold up to 1 trailing byte per body; " +
			"no release delay.",
		"TLS: certificate verification disabled (--insecure).",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("race experiment lacks %q:\n%s", want, out.String())
		}
	}
}

func TestTextRaceReporterExtractions(t *testing.T) {
	results := []tth2.Result{
		response(200, 31, 31),
		response(403, 24, 24),
	}
	results[0].ArrivalRank = 1
	results[1].ArrivalRank = 0
	match := func(value string) ExtractMatch {
		offset := [2]int{0, len(value)}
		data := EncodedData{Encoding: "utf-8", Data: value}
		return ExtractMatch{
			Offset: offset,
			Captures: []ExtractCapture{{
				Name: "extract", Participated: true,
				Offset: &offset, Value: &data,
			}},
		}
	}
	var b bytes.Buffer
	reporter := newTextRaceReporter(&b)
	run := RunRecord{
		Trials: 1, TrialsRequested: 1, TrialBatchWidth: 2, Connections: 1,
		CaptureBodyBytes: tth2.UnlimitedResponseBytes,
		ExtractRegex:     []string{`"result":"(?P<extract>[^"]+)"`},
		ExtractFields:    [][]string{{"extract"}},
		ResponseLimits:   defaultResponseLimits(),
	}
	if err := reporter.Run(run); err != nil {
		t.Fatal(err)
	}
	for _, ref := range fullRunReferences([]string{"A", "B"}).refs {
		if err := reporter.Request(RequestRecord{Reference: ref}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reporter.Trial(TrialRecord{
		Trial: &tth2.Trial{
			BatchResult: tth2.BatchResult{
				Results: results, ArrivalOrder: []int{1, 0},
			},
			Arrangement: []int{0, 1},
		},
		Outcomes: []OutcomeRecord{
			{
				Response: &ResponseRecord{Extracts: []ExtractRecord{{
					Complete: true,
					Matches: []ExtractMatch{
						match("quota accepted"),
						match("quota accepted"),
					},
				}}},
			},
			{
				Response: &ResponseRecord{Extracts: []ExtractRecord{{
					Complete: true,
					Matches:  []ExtractMatch{match("quota exhausted")},
				}}},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	const want = "\n---------------------------------- Experiment ----------------------------------\n\n" +
		"Trials: 1 x 2 requests = 2 request operations.\n" +
		"Arrangement: none.\n" +
		"Pacing: batch-rate ceiling unlimited; request-rate ceiling unlimited; minimum\n" +
		"        batch-start interval unconstrained.\n" +
		"Connections: up to 1 active (default).\n" +
		"Streams: honour the peer's advertised concurrent-stream limit.\n" +
		"Response limits: 8MiB accepted body per stream; 30s transport time per batch.\n" +
		"Run timeout: unlimited for live request execution.\n\n" +
		"----------------------------------- Results ------------------------------------\n\n" +
		"Extraction scope: complete response bodies.\n" +
		"Extract: `\"result\":\"(?P<extract>[^\"]+)\"`\n\n" +
		"  rank  pos  request  response\n" +
		"     0    1  B        403  24 B\n" +
		"        extract: \"quota exhausted\"\n" +
		"     1    0  A        200  31 B\n" +
		"        extract: \"quota accepted\", \"quota accepted\"\n" +
		"Status: 200 x 1, 403 x 1\n" +
		"Extract: \"quota accepted\" in 1 response, " +
		"\"quota exhausted\" in 1 response\n"
	if got := b.String(); got != want {
		t.Errorf("text extraction mismatch\n--- got ---\n%s--- want ---\n%s",
			got, want)
	}
}

func TestTextRaceReporterNamedExtractionFields(t *testing.T) {
	t.Parallel()
	pattern := `(?P<number>[0-9]+)(?P<unit>ms)?`
	cfg, err := parseRaceCapture(
		false, "1KiB", []string{pattern}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	result := response(200, 8, 8)
	result.ArrivalRank = 0
	var out bytes.Buffer
	reporter := newTextRaceReporter(&out)
	if err := reporter.Run(RunRecord{
		Trials: 1, TrialsRequested: 1, TrialBatchWidth: 1, Connections: 1,
		CaptureBodyBytes: 1024, ExtractRegex: []string{pattern},
		ExtractFields: cfg.extractFields(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Request(RequestRecord{
		Reference: runReference{ID: 0, Label: "A", Inline: "A"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Trial(TrialRecord{
		Trial: &tth2.Trial{
			BatchResult: tth2.BatchResult{
				Results: []tth2.Result{result}, ArrivalOrder: []int{0},
			},
			Arrangement: []int{0},
		},
		Outcomes: []OutcomeRecord{{Response: &ResponseRecord{
			Extracts: captureExtracts([]byte("12ms 7"), cfg.extracts, true),
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	for _, want := range []string{
		`        extract: number="12", unit="ms"`,
		`        extract: number="7"`,
		`Extract number: "12" in 1 response,`,
		`"7" in 1 response; responses may contain multiple values`,
		`Extract unit: "ms" in 1 response`,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("text report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, `unit=""`) {
		t.Errorf("nonparticipating optional field was rendered:\n%s", report)
	}
}

func TestTextRaceReporterSingleNamedExtractionField(t *testing.T) {
	t.Parallel()
	pattern := `id=(?P<number>[0-9]+)`
	cfg, err := parseRaceCapture(
		false, "1KiB", []string{pattern}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	reporter := newTextRaceReporter(&bytes.Buffer{})
	reporter.run = RunRecord{
		ExtractRegex: []string{pattern}, ExtractFields: cfg.extractFields(),
	}
	var out bytes.Buffer
	if err := reporter.writeRowExtract(
		&out, 0, captureExtracts(
			[]byte("id=12 id=7"), cfg.extracts, true)[0]); err != nil {
		t.Fatal(err)
	}
	const want = `        extract number: "12", "7"
`
	if got := out.String(); got != want {
		t.Errorf("single named extraction = %q, want %q", got, want)
	}
}

func TestExtractionSummaryCollapsesLargeDistributions(t *testing.T) {
	counts := make(map[string]int)
	for i := range 79 {
		counts[strconv.Itoa(i)] = 1
	}
	got := extractionSummary("Extract", counts, 80, 80, true, false, 0)
	const want = "79 distinct values; matched 80/80 responses;" +
		" responses may contain multiple values"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestNamedExtractionSummaryDescribesFieldParticipation(t *testing.T) {
	t.Parallel()
	got := extractionSummary(
		"Extract unit", map[string]int{}, 0, 2, false, true, 0)
	const want = "no values in 2 responses"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
}

func TestTextRaceReporterDistinguishesPrefixOnlyExtraction(t *testing.T) {
	t.Parallel()
	results := []tth2.Result{
		response(200, 3, 3),
		response(200, 10, 10),
		{},
	}
	results[0].ArrivalRank = 0
	results[1].ArrivalRank = 1
	results[2].ArrivalRank = -1
	run := RunRecord{
		Trials: 1, TrialsRequested: 1, TrialBatchWidth: 3,
		Connections: 1, CaptureBodyBytes: 4,
		ExtractRegex:  []string{`(?P<route>GET /now)`},
		ExtractFields: [][]string{{"route"}},
	}
	trial := &tth2.Trial{
		BatchResult: tth2.BatchResult{
			Results: results, ArrivalOrder: []int{0, 1},
		},
		Arrangement: []int{0, 1, 2},
	}
	var out bytes.Buffer
	reporter := newTextRaceReporter(&out)
	if err := reporter.Run(run); err != nil {
		t.Fatal(err)
	}
	for _, ref := range fullRunReferences(
		[]string{"complete", "prefix", "missing"}).refs {
		if err := reporter.Request(RequestRecord{Reference: ref}); err != nil {
			t.Fatal(err)
		}
	}
	if err := reporter.Trial(TrialRecord{
		Trial: trial,
		Outcomes: []OutcomeRecord{
			{Response: &ResponseRecord{
				BodyCapturedBytes: 3,
				Extracts: []ExtractRecord{{
					Selection: extractNamedGroups, Fields: []string{"route"},
					Complete: true, Matches: []ExtractMatch{},
				}},
			}},
			{Response: &ResponseRecord{
				BodyCapturedBytes: 4,
				Extracts: []ExtractRecord{{
					Selection: extractNamedGroups, Fields: []string{"route"},
					Complete: false, Matches: []ExtractMatch{},
				}},
			}},
			{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Summary(SummaryRecord{
		Execution: mustProjectRaceExecution(t, 1, 1, 3, 0,
			map[int]bool{0: true}, executionBatches{}, executionBatches{
				Attempted: 1, RankComplete: 1,
			}),
		ResetCount: make([]int, 3),
	}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"Extraction scope: up to 4 B from the start of each response body.",
		"        extraction: searched 4 B prefix only",
		"Extract route: no observed values in 2 responses; " +
			"1 prefix-only search",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("text report lacks %q:\n%s", want, got)
		}
	}
	if count := strings.Count(got, "prefix only"); count != 1 {
		t.Errorf("prefix-only row markers = %d, want 1:\n%s", count, got)
	}
	if strings.Contains(got, "0/3 responses") {
		t.Errorf("missing response counted as an extraction search:\n%s", got)
	}
}

func TestTextRaceReporterMultipleExtractsAndNoMatch(t *testing.T) {
	result := response(200, 3, 3)
	result.ArrivalRank = 0
	var b bytes.Buffer
	reporter := newTextRaceReporter(&b)
	if err := reporter.Run(RunRecord{
		Trials: 1, TrialsRequested: 1, TrialBatchWidth: 1, Connections: 1,
		CaptureBodyBytes: tth2.UnlimitedResponseBytes,
		ExtractRegex:     []string{"missing", "found=(.*)"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Request(RequestRecord{
		Reference: runReference{ID: 0, Label: "A", Inline: "A"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Trial(TrialRecord{
		Trial: &tth2.Trial{
			BatchResult: tth2.BatchResult{
				Results: []tth2.Result{result}, ArrivalOrder: []int{0},
			},
			Arrangement: []int{0},
		},
		Outcomes: []OutcomeRecord{{
			Response: &ResponseRecord{Extracts: []ExtractRecord{
				{Complete: true, Matches: []ExtractMatch{}},
				{Complete: true, Matches: []ExtractMatch{{
					Value: &EncodedData{
						Encoding: "utf-8", Data: "yes",
					},
				}}},
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"Extract 0: `missing`",
		"Extract 1: `found=(.*)`",
		"        extract 1: \"yes\"",
		"Extract 0: no matches in 1 response",
		"Extract 1: \"yes\" in 1 response",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "        extract 0:") {
		t.Errorf("no-match pattern produced an outcome line:\n%s", out)
	}
}

func TestWriteWrappedASCIIPreservesLongExtract(t *testing.T) {
	var b bytes.Buffer
	writeWrappedASCII(&b, "        extract: ",
		strconv.Quote(strings.Repeat("a", 100)))
	out := b.String()
	var joined strings.Builder
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		prefix := strings.Repeat(" ", len("        extract: "))
		if i == 0 {
			prefix = "        extract: "
		}
		joined.WriteString(strings.TrimPrefix(line, prefix))
	}
	if want := strconv.Quote(strings.Repeat("a", 100)); joined.String() != want {
		t.Errorf("wrapped extract = %q, want %q", joined.String(), want)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if len(line) > textWidth {
			t.Errorf("line is %d bytes, want <= %d:\n%s",
				len(line), textWidth, out)
		}
	}
}

func TestTextRaceReporterRepeatedTrial(t *testing.T) {
	results := []tth2.Result{
		response(403, 5, -1),
		response(200, 7, -1),
	}
	results[0].ArrivalRank = 1
	results[1].ArrivalRank = 0
	got := renderTextRace(t,
		RunRecord{Trials: 2, Connections: 1},
		[]string{"A", "B"},
		[]*tth2.Trial{{
			BatchResult: tth2.BatchResult{
				Results: results, ArrivalOrder: []int{1, 0},
			},
			Arrangement: []int{1, 0}, Index: 0, Cycle: -1, Shift: -1,
		}},
		SummaryRecord{ResetCount: make([]int, 2)},
	)
	const want = `
---------------------------------- Experiment ----------------------------------

Trials: 2 x 2 requests = 4 request operations.
Arrangement: none.
Pacing: batch-rate ceiling unlimited; request-rate ceiling unlimited; minimum
        batch-start interval unconstrained.
Connections: up to 1 active (default).
Streams: honour the peer's advertised concurrent-stream limit.
Response limits: 8MiB accepted body per stream; 30s transport time per batch.
Run timeout: unlimited for live request execution.

----------------------------------- Results ------------------------------------

Trial 1/2  conn 0

  rank  pos  request  response
     0    0  B        200  7 B
     1    1  A        403  5 B
Status: 200 x 1, 403 x 1

---------------------------------- Execution -----------------------------------

Trials: 1/2 attempted; 1 rank-complete; 0 incomplete; 1 unattempted.
Connections: 1 planned; 1 observed; 0 replacements.
Request operations: 2 attempted; 2 rank-complete; 2 retained.
`
	if got != want {
		t.Errorf("text report mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestTextRaceReporterHeadingPace(t *testing.T) {
	result := response(204, 0, -1)
	result.ArrivalRank = 0
	var b bytes.Buffer
	reporter := newTextRaceReporter(&b)
	if err := reporter.Run(RunRecord{
		Trials: 30, TrialsRequested: 30, Warmup: 1,
		TrialBatchWidth: 1, WarmupBatchWidth: 1, Connections: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Request(RequestRecord{
		Reference: runReference{ID: 0, Label: "A", Inline: "A"},
	}); err != nil {
		t.Fatal(err)
	}
	warmProgress := tth2.TrialProgress{
		Warmup: 1, Requests: 1,
		RecentTrialsPerSecond: 4, RecentRequestsPerSecond: 4,
	}
	if err := reporter.Trial(TrialRecord{
		Trial: &tth2.Trial{
			BatchResult: tth2.BatchResult{
				Results: []tth2.Result{result}, ArrivalOrder: []int{0},
			},
			Arrangement: []int{0}, Index: -1, Cycle: -1, Shift: -1,
			Warmup: true,
		},
		Progress: &warmProgress,
	}); err != nil {
		t.Fatal(err)
	}
	trialProgress := tth2.TrialProgress{
		Trials: 18, Warmup: 1, Requests: 19,
		RecentTrialsPerSecond: 6, RecentRequestsPerSecond: 12,
	}
	if err := reporter.Trial(TrialRecord{
		Trial: &tth2.Trial{
			BatchResult: tth2.BatchResult{
				Results: []tth2.Result{result}, ArrivalOrder: []int{0},
			},
			Arrangement: []int{0}, Index: 17, Cycle: 5, Shift: 2,
		},
		Progress: &trialProgress,
	}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if want := "Priming 1/1  conn 0  4.0 trials/s  4.0 req/s  eta ~8s\n"; !strings.Contains(out, want) {
		t.Errorf("warmup heading lacks pace %q:\n%s", want, out)
	}
	if want := "Trial 18/30  conn 0  cycle 5  shift 2" +
		"  6.0 trials/s  12.0 req/s  eta ~2s\n"; !strings.Contains(out, want) {
		t.Errorf("trial heading lacks pace and ETA %q:\n%s", want, out)
	}
}

func TestWriteTrialThroughputUsesSustainablePace(t *testing.T) {
	progress := tth2.TrialProgress{
		Trials: 1, Requests: 1,
		RecentTrialsPerSecond: 439, RecentRequestsPerSecond: 439,
	}
	eta := trialETA{
		limit: 10, requestsPerTrial: 1,
		maxRequestsPerSecond: 2.0 / 3600,
	}
	var b bytes.Buffer
	writeTrialPace(&b, &progress, eta)
	if got, want := b.String(),
		"  2.0 trials/h  2.0 req/h  eta ~4h30m0s"; got != want {
		t.Errorf("sustainable pace = %q, want %q", got, want)
	}
}

func TestTextRaceReporterUnboundedHeading(t *testing.T) {
	results := []tth2.Result{
		response(204, 0, -1),
		response(204, 0, -1),
	}
	results[0].ArrivalRank = 0
	results[1].ArrivalRank = 1
	got := renderTextRace(t,
		RunRecord{Trials: -1, Connections: 1},
		[]string{"A", "B"},
		[]*tth2.Trial{{
			BatchResult: tth2.BatchResult{
				Results: results, ArrivalOrder: []int{0, 1},
			},
			Arrangement: []int{0, 1}, Index: 41, Cycle: -1, Shift: -1,
		}},
		SummaryRecord{
			Execution: mustProjectRaceExecution(t, -1, 1, 2, 0,
				map[int]bool{0: true}, executionBatches{}, executionBatches{
					Attempted: 42, RankComplete: 42,
				}),
			ResetCount: make([]int, 2),
		},
	)
	if !bytes.Contains([]byte(got), []byte("\nTrial 42  conn 0\n")) {
		t.Errorf("unbounded report has a bounded heading:\n%s", got)
	}
	if bytes.Contains([]byte(got), []byte("Trial 42/")) {
		t.Errorf("unbounded report includes a denominator:\n%s", got)
	}
	if !strings.Contains(got, "Trials: until stopped; 2 requests per trial.") {
		t.Errorf("unbounded report omits its design:\n%s", got)
	}
}

func TestTextRaceReporterWarmupAndReset(t *testing.T) {
	warmResults := []tth2.Result{
		response(204, 0, -1),
		response(204, 0, -1),
	}
	warmResults[0].ArrivalRank = 0
	warmResults[1].ArrivalRank = 1
	resetResults := []tth2.Result{
		response(200, 3, -1),
		{
			ArrivalRank: -1,
			Reset: &tth2.StreamError{
				StreamID: 3,
				Code:     http2.ErrCodeCancel,
			},
		},
	}
	resetResults[0].ArrivalRank = 0
	got := renderTextRace(t,
		RunRecord{Trials: 1, Warmup: 1, Connections: 1},
		[]string{"A", "B"},
		[]*tth2.Trial{
			{
				BatchResult: tth2.BatchResult{
					Results: warmResults, ArrivalOrder: []int{0, 1},
				},
				Arrangement: []int{0, 1}, Index: -1, Cycle: -1,
				Shift: -1, Conn: 0, Warmup: true,
			},
			{
				BatchResult: tth2.BatchResult{
					Results: resetResults, ArrivalOrder: []int{0},
				},
				Arrangement: []int{0, 1}, Index: 0, Cycle: -1,
				Shift: -1, Conn: 0, Err: errors.New("stream reset"),
			},
		},
		SummaryRecord{ResetCount: []int{0, 1}},
	)
	const want = `
---------------------------------- Experiment ----------------------------------

Trials: 1 x 2 requests = 2 request operations.
Priming: 1 priming attempt per connection (2 priming request operations).
Arrangement: none.
Pacing: batch-rate ceiling unlimited; request-rate ceiling unlimited; minimum
        batch-start interval unconstrained.
Connections: up to 1 active (default).
Streams: honour the peer's advertised concurrent-stream limit.
Response limits: 8MiB accepted body per stream; 30s transport time per batch.
Run timeout: unlimited for live request execution.

----------------------------------- Results ------------------------------------

Priming 1/1  conn 0

  rank  pos  request  response
     0    0  A        204  0 B
     1    1  B        204  0 B
Status: 204 x 2

Trial 1/1  conn 0  failed

  rank  pos  request  response
     0    0  A        200  3 B
     -    1  B        reset CANCEL
Status: 200 x 1
  Error: stream reset

---------------------------------- Execution -----------------------------------

Trials: 1/1 attempted; 0 rank-complete; 1 incomplete; 0 unattempted.
Priming: 1 attempted; 1 rank-complete; 0 incomplete.
Connections: 1 planned; 1 observed; 0 replacements.
Request operations: 2 attempted; 0 rank-complete; 0 retained.
Resets: B x 1
`
	if got != want {
		t.Errorf("text report mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestTextRaceReporterDistinctWarmupReferences(t *testing.T) {
	var b bytes.Buffer
	reporter := newTextRaceReporter(&b)
	if err := reporter.Run(RunRecord{
		Trials: 1, TrialsRequested: 1, Warmup: 1,
		TrialBatchWidth: 2, WarmupBatchWidth: 1, Connections: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, ref := range fullRunReferences(
		[]string{"trial-A", "trial-B", "warmup"},
	).refs {
		if err := reporter.Request(RequestRecord{Reference: ref}); err != nil {
			t.Fatal(err)
		}
	}
	result := response(204, 0, -1)
	result.ArrivalRank = 0
	if err := reporter.Trial(TrialRecord{
		Trial: &tth2.Trial{
			BatchResult: tth2.BatchResult{
				Results: []tth2.Result{result}, ArrivalOrder: []int{0},
			},
			Arrangement: []int{0}, Index: -1, Cycle: -1, Shift: -1,
			Warmup: true,
		},
		RequestOffset: 2,
	}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "warmup") {
		t.Errorf("distinct warmup label is absent:\n%s", out)
	}
	if strings.Contains(out, "trial-A") || strings.Contains(out, "trial-B") {
		t.Errorf("warmup trial included trial requests:\n%s", out)
	}
}

func TestTextRaceReporterEmptyFailure(t *testing.T) {
	got := renderTextRace(t,
		RunRecord{Trials: 2, Connections: 1},
		[]string{"A", "B"},
		[]*tth2.Trial{{
			Arrangement: []int{0, 1}, Index: 1, Cycle: -1, Shift: -1,
			Err: errors.New("connection lost"),
		}},
		SummaryRecord{
			ResetCount: make([]int, 2),
			Completion: failedRun(
				errors.New("run stopped"), completionAcquisitionFailed),
		},
	)
	const want = `
---------------------------------- Experiment ----------------------------------

Trials: 2 x 2 requests = 4 request operations.
Arrangement: none.
Pacing: batch-rate ceiling unlimited; request-rate ceiling unlimited; minimum
        batch-start interval unconstrained.
Connections: up to 1 active (default).
Streams: honour the peer's advertised concurrent-stream limit.
Response limits: 8MiB accepted body per stream; 30s transport time per batch.
Run timeout: unlimited for live request execution.

----------------------------------- Results ------------------------------------

Trial 2/2  conn 0  failed

  rank  pos  request  response
     -    0  A        no response
     -    1  B        no response
Status: no responses
  Error: connection lost

---------------------------------- Execution -----------------------------------

Trials: 1/2 attempted; 0 rank-complete; 1 incomplete; 1 unattempted.
Connections: 1 planned; 1 observed; 0 replacements.
Request operations: 2 attempted; 0 rank-complete; 0 retained.
Completion: failed.
Error: run stopped
`
	if got != want {
		t.Errorf("text report mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestTextRaceReporterSummaryNamesTrialUnit(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	reporter := newTextRaceReporter(&out)
	if err := reporter.Summary(SummaryRecord{
		Execution: mustProjectRaceExecution(t, 4, 0, 0, 0, nil,
			executionBatches{Attempted: 2, RankComplete: 2},
			executionBatches{Attempted: 4, RankComplete: 3, Incomplete: 1}),
	}); err != nil {
		t.Fatal(err)
	}
	const want = `
---------------------------------- Execution -----------------------------------

Trials: 4/4 attempted; 3 rank-complete; 1 incomplete; 0 unattempted.
Priming: 2 attempted; 2 rank-complete; 0 incomplete.
Connections: 0 planned; 0 observed; 0 replacements.
Request operations: 0 attempted; 0 rank-complete; 0 retained.
`
	if got := out.String(); got != want {
		t.Errorf("text summary mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestTextRaceReporterReturnsWriteFailure(t *testing.T) {
	want := errors.New("write failed")
	reporter := newTextRaceReporter(failingWriter{err: want})
	if err := reporter.Run(RunRecord{
		Trials: 1, TrialsRequested: 1, TrialBatchWidth: 1, Connections: 1,
	}); !errors.Is(err, want) {
		t.Fatalf("Run error = %v, want %v", err, want)
	}
}

func writeRaceForTest(t *testing.T, w *bytes.Buffer) {
	t.Helper()
	reporter := newTextRaceReporter(w)
	if err := reporter.Run(RunRecord{
		Trials: 1, TrialsRequested: 1, TrialBatchWidth: 2, Connections: 1,
	}); err != nil {
		t.Fatal(err)
	}
	refs := fullRunReferences([]string{"A", "B"})
	for _, ref := range refs.refs {
		if err := reporter.Request(RequestRecord{Reference: ref}); err != nil {
			t.Fatal(err)
		}
	}
	results := []tth2.Result{
		response(200, 0, -1),
		response(200, 0, -1),
	}
	results[0].ArrivalRank = 0
	results[1].ArrivalRank = 1
	if err := reporter.Trial(TrialRecord{Trial: &tth2.Trial{
		BatchResult: tth2.BatchResult{
			Results: results, ArrivalOrder: []int{0, 1},
		},
		Arrangement: []int{0, 1}, Index: 0, Cycle: -1, Shift: -1,
	}}); err != nil {
		t.Fatal(err)
	}
}
