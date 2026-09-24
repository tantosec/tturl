package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/timing"
)

func TestTimeReportPopulationExcludesWarmupAndKeepsValidFailures(t *testing.T) {
	var output bytes.Buffer
	r := newTimeReporter(newReportWriter(&output), timeCommandConfig{},
		timeRequests{Measured: []timeRequest{{ID: 0, Protocol: curlblocks.HTTP2}}}, nil, buildinfo.Info{}, nil).(*timeReport)
	valid := timing.Outcome{
		RequestID: 0,
		Timing: timing.Timing{Duration: timing.Offset{
			NS:      123,
			Present: true,
		}},
		Failure: &timing.Failure{
			Phase: timing.PhaseExecution,
			Code:  "body_failure",
		},
	}
	if err := r.Observe(timing.Event{Warmup: &timing.Warmup{Outcomes: []timing.Outcome{valid}}}); err != nil {
		t.Fatal(err)
	}
	if err := r.Observe(timing.Event{Trial: &timing.Trial{Outcomes: []timing.Outcome{
		valid,
		{RequestID: 0},
	}}}); err != nil {
		t.Fatal(err)
	}
	if len(r.samples[0]) != 1 || r.samples[0][0] != 123 {
		t.Fatal("summary population lost valid final headers or included missing/warmup samples")
	}
	if err := r.Summary(timing.Result{Completion: timing.CompletionComplete}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output.Bytes(), []byte("n=1")) {
		t.Fatal("text digest omitted population")
	}
}

func TestTimeTimingPreservesSignedOffsetsAndMissingReasons(t *testing.T) {
	record := timeTiming(timing.Timing{
		InitialRelease: timing.Offset{
			NS:      7,
			Present: true,
		},
		FinalRelease: timing.Offset{Reason: "write_failed"},
		FirstResponseHeaders: timing.Offset{
			NS:      -4,
			Present: true,
		},
	})
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["initial_release_ns"] != float64(7) || decoded["first_response_headers_ns"] != float64(-4) ||
		decoded["final_release_ns"] != nil {
		t.Fatal("timing projection changed signed observations or supplied a zero placeholder")
	}
	if decoded["missing_reasons"].(map[string]any)["final_release_ns"] != "write_failed" {
		t.Fatal("missing milestone has no reason")
	}
}

func TestTimeExtractionDigestPreservesCoverageAndGroups(t *testing.T) {
	capture, err := parseRaceCapture(false, "unlimited", []string{`(?P<value>[a-z]+)(?P<optional>[0-9]+)?`}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	r := &timeReport{cfg: timeCommandConfig{capture: capture}}
	r.recordExtraction(timing.Outcome{
		RequestID: 4, Attempted: true,
		Response: timing.Response{Body: []byte("one two2"), CaptureTruncated: true},
	})
	r.recordExtraction(timing.Outcome{RequestID: 4})
	summary := r.extraction[4][0]
	if summary.Responses != 1 || summary.CompleteResponses != 0 || summary.MatchedResponses != 1 || summary.Matches != 2 {
		t.Fatal("extraction digest changed coverage or match counts")
	}
	if summary.NamedGroups[0].Participations != 2 || summary.NamedGroups[1].Participations != 1 {
		t.Fatal("named group participation changed")
	}
}

func TestTimeJSONRejectsAbsentCalendarAnchorsBeforeWriting(t *testing.T) {
	var output bytes.Buffer
	r := &timeReport{report: newReportWriter(&output), stream: newStructuredStream(&output)}
	for _, event := range []timing.Event{{Trial: &timing.Trial{}}, {Warmup: &timing.Warmup{}}} {
		if err := r.Observe(event); err == nil {
			t.Fatal("JSON accepted evidence without calendar provenance")
		}
	}
	if err := r.Summary(timing.Result{}); err == nil {
		t.Fatal("JSON accepted result without run provenance")
	}
	if output.Len() != 0 {
		t.Fatal("invalid provenance wrote a partial record")
	}
}
