package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type analyseJSONFixture struct {
	jsonAnalyseRunRecord
	Evidence struct {
		Requests []jsonAnalyseRequestRecord
		Trials   []jsonAnalyseTrial
	}
	ResultRecord jsonAnalyseResultRecord
}

func decodeAnalyseJSONL(t *testing.T, data []byte) analyseJSONFixture {
	t.Helper()
	var fixture analyseJSONFixture
	for _, line := range jsonLines(t, data) {
		switch recordKind(t, line) {
		case "run":
			if err := json.Unmarshal(line, &fixture.jsonAnalyseRunRecord); err != nil {
				t.Fatal(err)
			}
		case "request":
			var request jsonAnalyseRequestRecord
			if err := json.Unmarshal(line, &request); err != nil {
				t.Fatal(err)
			}
			fixture.Evidence.Requests = append(fixture.Evidence.Requests, request)
		case "trial":
			var trial jsonAnalyseTrial
			if err := json.Unmarshal(line, &trial); err != nil {
				t.Fatal(err)
			}
			fixture.Evidence.Trials = append(fixture.Evidence.Trials, trial)
		case "result":
			if err := json.Unmarshal(line, &fixture.ResultRecord); err != nil {
				t.Fatal(err)
			}
		}
	}
	return fixture
}

func reportAnalyseJSON(w io.Writer, result analyseRunResult, labels []string, contexts ...analyseJSONContext) error {
	context := analyseJSONContext{
		Argv: []string{"analyse"}, DialAddress: "example.test:443",
		MeasuredBatchWidth: result.Width, MaxResponseHeaderBytes: tturlMaxResponseHeaderBytes,
		Padding: resolvedPadding(nil, result.Width), ResponseLimits: defaultResponseLimits(),
		Delivery: deliveryPlan{
			connectionCeiling:   result.ConnectionLimit,
			connectionLimit:     result.ConnectionLimit,
			connectionSelection: connectionExplicit,
		},
		RandomisationSeed: result.Global.Calibration.RandomisationSeed,
	}
	if len(contexts) > 0 {
		context = contexts[0]
	}
	if len(context.Requests) == 0 {
		for id, label := range labels {
			context.Requests = append(context.Requests, RequestRecord{
				Reference: runReference{ID: id, Label: label}, RequestSet: "measured", URL: "https://example.test/",
			})
		}
	}
	stream, err := newAnalyseJSONStream(w, analyseWorkload{
		Width: result.Width, Cycles: result.Plan.Cycles,
		Trials: result.Plan.Trials, Operations: result.Plan.Operations, DefaultCycles: result.Plan.DefaultCycles,
		LowerSensitivity: result.Plan.LowerSensitivity,
	}, result.ConnectionLimit, labels, context, result.Evidence.Capture)
	if err != nil {
		return err
	}
	for _, observation := range result.Evidence.Observations {
		if err := stream.Trial(observation); err != nil {
			return err
		}
	}
	return stream.Finish(result, context)
}

func TestReportAnalyseJSONDifference(t *testing.T) {
	result := analyseOffline(analyseOrderedEvidence(2, 6, func(int) []int {
		return []int{
			0,
			1,
		}
	}), analyseConfig{
		RandomisationSeed: 7,
	})
	var output bytes.Buffer
	if err := reportAnalyseJSON(&output, result, []string{"A", "B"}); err != nil {
		t.Fatal(err)
	}
	schema := compileCurrentStructuredSchema(t, analyseSchema.filename)
	lines := jsonLines(t, output.Bytes())
	for i, line := range lines {
		var instance any
		if err := json.Unmarshal(line, &instance); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
	}
	validateStructuredRecords(t, lines)
	fixture := decodeAnalyseJSONL(t, output.Bytes())
	analysis := fixture.ResultRecord.Analysis
	if fixture.Schema != analyseSchema.logical || fixture.Method.ID != analyseMethodVersion ||
		fixture.Plan.Work.PlannedCycles != 6 || fixture.Plan.Work.PlannedTrials != 12 ||
		analysis.State != "available" || analysis.Finding.Status != string(analyseDifference) ||
		analysis.GlobalTest.Calibration.PValue != 0.03125 {
		t.Fatalf("fixture = %+v", fixture)
	}
	for _, request := range analysis.Requests {
		if request.RequestID >= len(fixture.Evidence.Requests) || request.Samples != 12 {
			t.Errorf("request = %+v", request)
		}
	}
	for _, pair := range *analysis.PairTests {
		if pair.LeftRequestID != 0 || pair.RightRequestID != 1 || pair.LeftBeforeRight != 12 ||
			pair.Significant != (pair.AdjustedP <= analysis.GlobalTest.Alpha) {
			t.Errorf("pair = %+v", pair)
		}
	}
	replayed, err := replayAnalyseJSONStream(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(makeJSONAnalyseResultRecord(result, analyseJSONContext{}),
		makeJSONAnalyseResultRecord(replayed, analyseJSONContext{})) {
		t.Fatal("replay changed analysis")
	}
}

func TestAnalyseJSONTrialCanBeDecodedBeforeResult(t *testing.T) {
	observation := analyseTrialObservation{
		Index: 0, Cycle: 0, Shift: 0, Connection: 0,
		Arrangement: []int{1, 0}, Error: "transport interrupted", Outcomes: []OutcomeRecord{{Request: 1, Position: 0}},
	}
	trial := makeJSONAnalyseTrial(observation)
	data, err := json.Marshal(trial)
	if err != nil {
		t.Fatal(err)
	}
	var decoded jsonAnalyseTrial
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	converted, retained, err := nativeAnalyseObservation(decoded, 2)
	if err != nil {
		t.Fatal(err)
	}
	if converted.Arrangement[0] != 1 || converted.Arrangement[1] != 0 || retained != nil ||
		decoded.Failure.Code != "transport_error" {
		t.Fatalf("trial = %+v", decoded)
	}
}

func TestAnalyseJSONUnavailableReasons(t *testing.T) {
	result := analyseOffline(analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} }), analyseConfig{})
	codes := []analyseValidityCode{
		analyseInvalidWidth, analyseTooFewCycles, analyseIncompleteCycle, analyseInvalidShift,
		analyseInvalidArrangement, analyseInvalidRanks, analyseInvalidStatus, analyseMixedConnections,
		analyseEvidenceTooLarge, analyseInvalidWorkload, analyseInterrupted,
	}
	schema := compileCurrentStructuredSchema(t, analyseSchema.filename)
	for _, code := range codes {
		t.Run(string(code), func(t *testing.T) {
			result.Validity = analyseValidity{Code: code, Detail: "Evidence cannot support this analysis."}
			projected := makeJSONAnalyseResultRecord(result, analyseJSONContext{})
			if projected.Analysis.State != "unavailable" || projected.Analysis.Reason.Code != code ||
				projected.Analysis.Reason.Message == "" {
				t.Fatalf("analysis = %+v", projected.Analysis)
			}
			data, err := json.Marshal(projected)
			if err != nil {
				t.Fatal(err)
			}
			var instance any
			if err := json.Unmarshal(data, &instance); err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(instance); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReportAnalyseJSONMonteCarloSeed(t *testing.T) {
	result := analyseOffline(analyseOrderedEvidence(3, 7, func(int) []int {
		return []int{
			0,
			1,
			2,
		}
	}), analyseConfig{
		RandomisationSeed: 7,
	})
	var output bytes.Buffer
	if err := reportAnalyseJSON(&output, result, []string{"A", "B", "C"}); err != nil {
		t.Fatal(err)
	}
	fixture := decodeAnalyseJSONL(t, output.Bytes())
	if fixture.Method.RandomisationSeed != 7 ||
		fixture.ResultRecord.Analysis.GlobalTest.Calibration.Method != "monte_carlo_randomisation" {
		t.Fatalf("method/calibration = %+v/%+v", fixture.Method, fixture.ResultRecord.Analysis.GlobalTest.Calibration)
	}
	if _, err := replayAnalyseJSONStream(bytes.NewReader(output.Bytes())); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyseJSONStreamReplayRejectsCorruption(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(schemaDirectory, "examples", "analyse-difference.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	unavailableConclusion := func(code string) func(map[string]any) {
		return func(v map[string]any) {
			v["analysis"] = map[string]any{"state": "unavailable", "reason": map[string]any{
				"code": code, "message": "Evidence unavailable.",
			}}
			execution := v["execution"].(map[string]any)
			trials := execution["trials"].(map[string]any)
			trials["retained"], trials["excluded"] = 0, trials["rank_complete"]
			execution["request_operations"].(map[string]any)["retained"] = 0
			execution["cycles"].(map[string]any)["retained"] = 0
		}
	}
	lines := jsonLines(t, data)
	tests := []struct {
		name   string
		line   int
		mutate func(map[string]any)
	}{
		{"schema", 0, func(v map[string]any) { v["schema"] = "tturl.unknown/v0" }},
		{"method", 0, func(v map[string]any) { v["method"].(map[string]any)["id"] = "unknown" }},
		{"catalogue role", 1, func(v map[string]any) { v["role"] = "warmup" }},
		{"catalogue ID", 1, func(v map[string]any) { v["request_id"] = 2 }},
		{"duplicate label", 2, func(v map[string]any) { v["label"] = "Request 0" }},
		{"default ceiling", 0, func(v map[string]any) {
			v["acquisition"].(map[string]any)["delivery"].(map[string]any)["connection_ceiling"] = 8
		}},
		{"request digest", 1, func(v map[string]any) {
			v["body"] = map[string]any{"bytes": 1, "encoding": "utf-8", "data": "x", "sha256": digestBase64([]byte("y"))}
		}},
		{"delivery interval", 0, func(v map[string]any) {
			v["acquisition"].(map[string]any)["delivery"].(map[string]any)["rate_limits"] = map[string]any{
				"batch_max_per_second": 2, "request_max_per_second": nil, "effective_batch_interval_ns": 1,
			}
		}},
		{"unsupported conclusion", len(lines) - 1, unavailableConclusion("unsupported_width")},
		{"unrecorded interruption", len(lines) - 1, unavailableConclusion("interrupted")},
		{"coordinates", 3, func(v map[string]any) { v["trial_index"] = 1 }},
		{"assignment", 3, func(v map[string]any) { v["assignments"].([]any)[0].(map[string]any)["request_id"] = 9 }},
		{"finding", len(lines) - 1, func(v map[string]any) {
			v["analysis"].(map[string]any)["finding"].(map[string]any)["status"] = "no_difference_detected"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			for i, line := range lines {
				if i == test.line {
					var value map[string]any
					if err := json.Unmarshal(line, &value); err != nil {
						t.Fatal(err)
					}
					test.mutate(value)
					line, err = json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
				}
				output.Write(line)
				output.WriteByte('\n')
			}
			if _, err := replayAnalyseJSONStream(bytes.NewReader(output.Bytes())); err == nil {
				t.Fatal("corrupt evidence accepted")
			}
		})
	}
	for _, prefix := range [][]byte{data[:len(data)-1], bytes.Join(lines[:len(lines)-1], []byte("\n"))} {
		if _, err := replayAnalyseJSONStream(bytes.NewReader(prefix)); err == nil {
			t.Fatal("incomplete evidence accepted")
		}
	}
}

func TestReportAnalyseJSONWriteError(t *testing.T) {
	result := analyseOffline(analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} }), analyseConfig{})
	want := errors.New("write failed")
	if err := reportAnalyseJSON(failingWriter{err: want}, result, []string{"A", "B"}); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
}

func TestAnalyseJSONReplayAtMaximumSupportedWorkload(t *testing.T) {
	const width = 8
	result := finishAnalyseLive(analyseOrderedEvidence(width, analyseMaximumOperations/(width*width),
		func(int) []int { return []int{0, 1, 2, 3, 4, 5, 6, 7} }), 1, analyseConfig{RandomisationSeed: 7, ConnectionLimit: 1})
	labels := make([]string, width)
	for i := range labels {
		labels[i] = fmt.Sprintf("Request %d", i)
	}
	var output bytes.Buffer
	if err := reportAnalyseJSON(&output, result, labels); err != nil {
		t.Fatal(err)
	}
	if _, err := replayAnalyseJSONStream(bytes.NewReader(output.Bytes())); err != nil {
		t.Fatal(err)
	}
}

func FuzzReplayAnalyseJSONStream(f *testing.F) {
	data, err := os.ReadFile(filepath.Join(schemaDirectory, "examples", "analyse-difference.jsonl"))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(data)
	f.Add(data[:len(data)/2])
	f.Add([]byte("{}\n"))
	f.Fuzz(func(t *testing.T, data []byte) { _, _ = replayAnalyseJSONStream(bytes.NewReader(data)) })
}

func TestAnalyseJSONCurrentFixtureReplay(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "replay", "analyse-current.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replayAnalyseJSONStream(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
}

func TestAnalyseJSONExampleReplay(t *testing.T) {
	for _, name := range []string{"difference", "no-difference", "unavailable", "interrupted", "acquisition-failed"} {
		t.Run(name, func(t *testing.T) {
			//nolint:gosec // Names come from the fixed fixture catalogue.
			data, err := os.ReadFile(filepath.Join(schemaDirectory, "examples", "analyse-"+name+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := replayAnalyseJSONStream(bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReportAnalyseJSONRequestResponseFacts(t *testing.T) {
	evidence := analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} })
	for cycle := range evidence.Cycles {
		for trial := range evidence.Cycles[cycle].Trials {
			if (cycle+trial)%2 == 0 {
				evidence.Cycles[cycle].Trials[trial].Statuses[1] = 500
			}
		}
	}
	result := analyseOffline(evidence, analyseConfig{})
	record := makeJSONAnalyseResultRecord(result, analyseJSONContext{})
	requests := record.Analysis.Requests
	if requests[0].RequestID != 0 || requests[0].StatusVaried || requests[0].Non2xxObserved ||
		requests[1].RequestID != 1 || !requests[1].StatusVaried || !requests[1].Non2xxObserved ||
		record.Analysis.StatusDistributionsDiffer == nil || !*record.Analysis.StatusDistributionsDiffer {
		t.Fatalf("response facts = %+v", record.Analysis)
	}
	if !reflect.DeepEqual(requests[1].StatusCounts,
		[]structuredStatusCount{
			{
				Status: 200,
				Count:  6,
			},
			{
				Status: 500,
				Count:  6,
			},
		}) {
		t.Fatalf("status counts = %+v", requests[1].StatusCounts)
	}
}
