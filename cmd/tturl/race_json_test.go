package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

func TestJSONResponseBinaryRoundTrip(t *testing.T) {
	t.Parallel()
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}
	encoded := encodeData(allBytes)
	invalidHeader := string([]byte{0xff, 0x00})
	rec := TrialRecord{
		Trial: &tth2.Trial{
			BatchResult: tth2.BatchResult{
				Results: []tth2.Result{{}},
			},
			Arrangement: []int{0},
		},
		Outcomes: []OutcomeRecord{{
			Request: 0, Position: 0,
			Response: &ResponseRecord{
				Status:    http.StatusOK,
				BodyBytes: int64(len(allBytes)),
				SHA256:    digestBase64(allBytes),
				Headers: HeaderRecord{
					"x-binary": {invalidHeader},
				},
				Trailers: HeaderRecord{},
				Body:     &encoded,
				Extracts: []ExtractRecord{{
					Pattern:   `(?s).*`,
					Selection: extractWholeMatch,
					Matches: []ExtractMatch{{
						Offset: [2]int{0, len(allBytes)},
						Value:  &encoded,
					}},
				}},
			},
		}},
	}
	var out bytes.Buffer
	if err := startRaceJSONCatalogue(t, &out, 1, rec).Trial(rec); err != nil {
		t.Fatalf("Trial: %v", err)
	}
	var got jsonRaceTrialRecord
	if err := json.Unmarshal(lastJSONRecord(t, out.Bytes()), &got); err != nil {
		t.Fatalf("decode trial: %v", err)
	}
	response := got.Outcomes[0].Response
	if response == nil || response.Body == nil {
		t.Fatalf("response = %+v", response)
	}
	body, err := base64.StdEncoding.DecodeString(
		string(response.Body.Data))
	if err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	if response.Body.Encoding != "base64" ||
		!bytes.Equal(body, allBytes) {
		t.Errorf("response body did not round-trip")
	}
	match := response.Extracts[0].Matches[0]
	matchBytes, err := base64.StdEncoding.DecodeString(string(*match.Data))
	if err != nil {
		t.Fatalf("decode extract: %v", err)
	}
	if match.Encoding == nil || *match.Encoding != "base64" ||
		match.Offset != [2]int{0, len(allBytes)} ||
		!bytes.Equal(matchBytes, allBytes) {
		t.Errorf("extract did not round-trip: %+v", match)
	}
	headerValues := (*response.Headers)["x-binary"]
	if len(headerValues) != 1 {
		t.Fatalf("binary header = %q", headerValues)
	}
	headerBytes := []byte(headerValues[0])
	if !bytes.Equal(headerBytes, []byte(invalidHeader)) {
		t.Errorf("header bytes = %x, want %x",
			headerBytes, []byte(invalidHeader))
	}
}

func TestJSONRaceReporterPreservesCompletionOrder(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	reporter := startRaceJSONCatalogue(t, &out, 1)
	for _, index := range []int{2, 0, 1} {
		if err := reporter.Trial(TrialRecord{
			Trial: &tth2.Trial{
				Arrangement: []int{0},
				Index:       index,
				Cycle:       index / 2,
				Shift:       index % 2,
				Conn:        index,
			},
			Outcomes: []OutcomeRecord{{
				Request: 0, Position: 0,
			}},
		}); err != nil {
			t.Fatalf("Trial %d: %v", index, err)
		}
	}
	lines := jsonLines(t, out.Bytes())[2:]
	if len(lines) != 3 {
		t.Fatalf("got %d trial records, want three", len(lines))
	}
	for emitted, wantIndex := range []int{2, 0, 1} {
		var got jsonRaceTrialRecord
		if err := json.Unmarshal(lines[emitted], &got); err != nil {
			t.Fatalf("decode trial %d: %v", emitted, err)
		}
		if got.TrialIndex != wantIndex ||
			got.ConnectionID != wantIndex ||
			got.Cycle == nil || *got.Cycle != wantIndex/2 ||
			got.Shift == nil || *got.Shift != wantIndex%2 ||
			len(got.Outcomes) != 1 ||
			got.Outcomes[0].RequestID != 0 ||
			got.Outcomes[0].Position != 0 {
			t.Errorf("record %d = %+v, want coordinates for index %d",
				emitted, got, wantIndex)
		}
	}
}

func TestRaceJSONMetadataStream(t *testing.T) {
	t.Parallel()
	responseBody := []byte{0xff, 0x00}
	userAgents := make(chan string, 1)
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		select {
		case userAgents <- r.Header.Get("User-Agent"):
		default:
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(responseBody)))
		w.Header().Set("X-Ignored-At-Metadata", "secret")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(responseBody)
	}))
	requestBody := string([]byte{0xfe, 0x01})
	out, stdout, stderr := captureOutput(t)
	err := runRace(t.Context(), []string{
		"--report", "json",
		"--insecure",
		"--ignore-peer-stream-limit",
		"--padding", "query=2",
		"--data-binary", requestBody,
		"--block", "https://" + addr + "/probe?real=1",
	}, out)
	if err != nil {
		t.Fatalf("runRace: %v", err)
	}
	select {
	case got := <-userAgents:
		want := toolName + "/" + buildinfo.Current().Version()
		if got != want {
			t.Errorf("captured User-Agent = %q, want %q", got, want)
		}
	default:
		t.Error("server captured no request User-Agent")
	}
	lines := jsonLines(t, stdout.Bytes())
	kinds := make([]string, len(lines))
	for i, line := range lines {
		kinds[i] = recordKind(t, line)
	}
	if got, want := strings.Join(kinds, ","), "run,request,trial,result"; got != want {
		t.Fatalf("record kinds = %s, want %s", got, want)
	}
	if !strings.Contains(stderr.String(), "Target:") ||
		!strings.Contains(stderr.String(), "Total: 1 request.") {
		t.Errorf("stderr lacks friendly request echo:\n%s", stderr.String())
	}
	if strings.Contains(stdout.String(), "Target:") {
		t.Errorf("JSON destination contains text request echo:\n%s", stdout.String())
	}
	if strings.Contains(stderr.String(), diagnosticPrefix(diagnosticWarning)) {
		t.Errorf("ordinary race plan emitted a warning:\n%s", stderr)
	}
	var run jsonRaceRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Schema != raceSchema.logical || run.Command != "race" ||
		run.RequestCount != 1 || run.Plan.Work.RequestedTrials == nil ||
		*run.Plan.Work.RequestedTrials != 1 || run.Plan.Work.PlannedTrials == nil ||
		*run.Plan.Work.PlannedTrials != 1 || run.Plan.BatchWidth != 1 ||
		run.Plan.Warmup.BatchWidth != 0 || run.Plan.Warmup.TrialsPerConnection != 0 ||
		run.Plan.Arrangement != "none" || run.Acquisition.ReleaseDelayNS != 0 ||
		run.Acquisition.BodyBytesWithheld != 1 || run.Acquisition.TLSVerification != "disabled" ||
		run.Acquisition.Capture.Headers || run.Acquisition.Capture.BodyBytes == nil ||
		*run.Acquisition.Capture.BodyBytes != 0 || len(run.Plan.Extractors) != 0 {
		t.Errorf("run record fields = %+v", run)
	}
	delivery := run.Acquisition.Delivery
	if delivery.ConnectionLimit != 1 || delivery.Selection.Mode != "default" ||
		!delivery.PeerStreamLimitIgnored {
		t.Errorf("delivery = %+v", delivery)
	}
	if len(run.Argv) < 1 || run.Argv[0] != "race" {
		t.Errorf("argv = %q, want command first", run.Argv)
	}
	wantArgBody := requestBody
	if !slices.Contains(run.Argv, wireString(wantArgBody)) {
		t.Errorf("argv = %q, want original binary argument %q",
			run.Argv, wantArgBody)
	}
	if _, err := time.Parse(time.RFC3339Nano, string(run.StartedAt)); err != nil {
		t.Errorf("started_at = %q: %v", run.StartedAt, err)
	}
	if run.Target.DialAddress == "" || run.Target.Origin == nil ||
		string(*run.Target.Origin) != "https://"+addr {
		t.Errorf("dial/origin = %q/%v", run.Target.DialAddress, run.Target.Origin)
	}
	if len(run.Plan.Padding) != 1 || run.Plan.Padding[0].Position != 0 ||
		run.Plan.Padding[0].QueryFields != 2 ||
		run.Plan.Padding[0].HeaderFields != 0 ||
		run.Plan.Padding[0].BodyFields != 0 {
		t.Errorf("padding = %+v", run.Plan.Padding)
	}

	var request jsonRaceRequestRecord
	if err := json.Unmarshal(lines[1], &request); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if request.RequestID != 0 || request.Role != "measured" ||
		request.Label != "A" {
		t.Errorf("request identity = %+v", request)
	}
	if strings.Contains(string(request.URL), "_pad") ||
		string(request.URL) != "https://"+addr+"/probe?real=1" {
		t.Errorf("canonical request URL = %q", request.URL)
	}
	if request.PseudoHeaders[":path"] != "/probe?real=1" ||
		request.PseudoHeaders[":authority"] != addr {
		t.Errorf("pseudo headers = %v", request.PseudoHeaders)
	}
	if got := request.Headers["content-length"]; len(got) != 1 ||
		got[0] != strconv.Itoa(len(requestBody)) {
		t.Errorf("content-length = %q", got)
	}
	if request.Body == nil || request.Body.Bytes != len(requestBody) ||
		request.Body.Encoding != "base64" ||
		string(request.Body.Data) !=
			base64.StdEncoding.EncodeToString([]byte(requestBody)) ||
		request.Body.SHA256 != digestBase64([]byte(requestBody)) {
		t.Errorf("request body = %+v", request.Body)
	}

	var trial jsonRaceTrialRecord
	if err := json.Unmarshal(lines[2], &trial); err != nil {
		t.Fatalf("decode trial: %v", err)
	}
	if trial.TrialIndex != 0 || trial.ConnectionID != 0 ||
		trial.Failure != nil || len(trial.Outcomes) != 1 {
		t.Errorf("trial fields = %+v", trial)
	}
	if trial.Dispatch.HeadersSentAt == nil ||
		trial.Dispatch.BodyFinalisedAt == nil {
		t.Fatalf("dispatch timestamps = %+v", trial.Dispatch)
	}
	for _, value := range []*wireString{
		trial.Dispatch.HeadersSentAt, trial.Dispatch.BodyFinalisedAt,
	} {
		if _, err := time.Parse(time.RFC3339Nano, string(*value)); err != nil {
			t.Errorf("dispatch timestamp = %q: %v", *value, err)
		}
	}
	outcome := trial.Outcomes[0]
	if outcome.RequestID != 0 || outcome.Position != 0 ||
		outcome.Rank == nil || *outcome.Rank != 0 ||
		outcome.StreamID == nil || outcome.Response == nil ||
		outcome.Reset != nil {
		t.Errorf("outcome = %+v", outcome)
	}
	if outcome.Response.Status != http.StatusCreated ||
		outcome.Response.ContentLength == nil ||
		*outcome.Response.ContentLength != int64(len(responseBody)) ||
		outcome.Response.BodyBytes != int64(len(responseBody)) ||
		outcome.Response.SHA256 != digestBase64(responseBody) ||
		len(outcome.Response.Extracts) != 0 ||
		outcome.Response.Headers != nil ||
		outcome.Response.Trailers != nil ||
		outcome.Response.Body != nil {
		t.Errorf("response metadata = %+v", outcome.Response)
	}

	var summary jsonRaceResultRecord
	if err := json.Unmarshal(lines[3], &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Execution.Trials.RankComplete != 1 ||
		summary.Execution.Trials.Incomplete != 0 ||
		summary.Execution.Priming.Batches.Attempted != 0 ||
		summary.Completion.State != completionComplete ||
		len(summary.Aggregates.ResetCounts) != 0 {
		t.Errorf("summary = %+v", summary)
	}
}

func TestRaceJSONFullResponseAndExtraction(t *testing.T) {
	t.Parallel()
	body := []byte("pre[value]post")
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("X-Observed", "yes")
		w.Header().Set("Trailer", "X-Final")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
		w.Header().Set("X-Final", "done")
	}))
	out, stdout, _ := captureOutput(t)
	pattern := `\[(?P<extract>.*?)\]`
	err := runRace(t.Context(), []string{
		"--report", "json",
		"--capture-headers", "--capture-body", "unlimited",
		"--extract-regex", pattern,
		"--insecure",
		"https://" + addr + "/probe",
	}, out)
	if err != nil {
		t.Fatalf("runRace: %v", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	if len(lines) != 4 {
		t.Fatalf("got %d records, want four", len(lines))
	}
	var run jsonRaceRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if !run.Acquisition.Capture.Headers || run.Acquisition.Capture.BodyBytes != nil ||
		len(run.Plan.Extractors) != 1 || string(run.Plan.Extractors[0].Pattern) != pattern ||
		run.Plan.Extractors[0].Selection != string(extractNamedGroups) ||
		!slices.Equal(run.Plan.Extractors[0].Fields, []wireString{"extract"}) {
		t.Errorf("run capture fields = %+v", run)
	}
	var trial jsonRaceTrialRecord
	if err := json.Unmarshal(lines[2], &trial); err != nil {
		t.Fatalf("decode trial: %v", err)
	}
	if len(trial.Outcomes) != 1 ||
		trial.Outcomes[0].Response == nil {
		t.Fatalf("outcomes = %+v", trial.Outcomes)
	}
	response := trial.Outcomes[0].Response
	if response.Headers == nil ||
		(*response.Headers)["x-observed"][0] != "yes" ||
		response.Trailers == nil ||
		(*response.Trailers)["x-final"][0] != "done" {
		t.Errorf("headers = %v, trailers = %v",
			response.Headers, response.Trailers)
	}
	if response.Body == nil ||
		response.Body.Encoding != "base64" ||
		string(response.Body.Data) != base64.StdEncoding.EncodeToString(body) {
		t.Errorf("body = %+v", response.Body)
	}
	if len(response.Extracts) != 1 || response.Extracts[0].ExtractorID != run.Plan.Extractors[0].ExtractorID ||
		len(response.Extracts[0].Matches) != 1 {
		t.Fatalf("extractor join = %+v", response.Extracts)
	}
	match := response.Extracts[0].Matches[0]
	if match.Offset != [2]int{3, 10} || len(match.Captures) != 1 ||
		match.Captures[0].Encoding == nil ||
		*match.Captures[0].Encoding != "utf-8" ||
		match.Captures[0].Data == nil ||
		string(*match.Captures[0].Data) != "value" {
		t.Errorf("match = %+v", match)
	}
}

func TestJSONDistinctWarmupRequestIDs(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, _ := captureOutput(t)
	err := runRace(t.Context(), []string{
		"--report", "json",
		"--insecure",
		"--warmup", "1",
		"--block", "https://" + addr + "/warm",
		"--name", "prime",
		"--warmup-only",
		"--block", "https://" + addr + "/measured",
		"--name", "probe",
	}, out)
	if err != nil {
		t.Fatalf("runRace: %v", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	if len(lines) != 6 {
		t.Fatalf("got %d records, want run + 2 requests + 2 trials + summary",
			len(lines))
	}
	var run jsonRaceRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Plan.BatchWidth != 1 || run.Plan.Warmup.BatchWidth != 1 {
		t.Errorf("batch widths = %+v", run.Plan)
	}
	var requests []jsonRaceRequestRecord
	var evidence []struct {
		Kind string `json:"kind"`
		jsonRaceEvidence
	}
	for _, line := range lines {
		switch recordKind(t, line) {
		case "request":
			var rec jsonRaceRequestRecord
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatal(err)
			}
			requests = append(requests, rec)
		case "trial", "warmup":
			var rec struct {
				Kind string `json:"kind"`
				jsonRaceEvidence
			}
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatal(err)
			}
			evidence = append(evidence, rec)
		}
	}
	if len(requests) != 2 ||
		requests[0].RequestID != 0 ||
		requests[0].Role != "measured" ||
		requests[0].Label != "probe" ||
		requests[1].RequestID != 1 ||
		requests[1].Role != "warmup" ||
		requests[1].Label != "prime" {
		t.Errorf("catalogue = %+v", requests)
	}
	if len(evidence) != 2 ||
		evidence[0].Kind != "warmup" ||
		len(evidence[0].Outcomes) != 1 ||
		evidence[0].Outcomes[0].RequestID != 1 ||
		evidence[1].Kind != "trial" ||
		len(evidence[1].Outcomes) != 1 ||
		evidence[1].Outcomes[0].RequestID != 0 {
		t.Errorf("evidence joins = %+v", evidence)
	}
}

func TestJSONResetShapesAndStringErrors(t *testing.T) {
	t.Parallel()
	body := []byte("partial")
	results := []tth2.Result{
		{
			ArrivalRank: -1,
			StreamID:    1,
			Reset: &tth2.StreamError{
				StreamID: 1,
				Code:     http2.ErrCodeCancel,
			},
		},
		{
			Response: &tth2.Response{
				StatusCode:    http.StatusOK,
				ContentLength: -1,
				Body: tth2.ResponseBody{
					Captured: body, BytesReceived: int64(len(body)),
					SHA256: sha256.Sum256(body),
				},
			},
			ArrivalRank: 0,
			StreamID:    3,
			Reset: &tth2.StreamError{
				StreamID: 3,
				Code:     http2.ErrCodeCancel,
			},
		},
	}
	trialErr := errors.New("trial failed")
	trial := &tth2.Trial{
		BatchResult: tth2.BatchResult{
			Results:      results,
			ArrivalOrder: []int{1},
		},
		Arrangement: []int{0, 1},
		Index:       2,
		Cycle:       1,
		Shift:       0,
		Err:         trialErr,
	}
	rec, err := captureTrialRecord(trial, 0, raceCaptureConfig{})
	if err != nil {
		t.Fatalf("captureTrialRecord: %v", err)
	}
	var out bytes.Buffer
	reporter := startRaceJSONCatalogue(t, &out, 2)
	if err := reporter.Trial(rec); err != nil {
		t.Fatalf("Trial: %v", err)
	}
	summaryErr := errors.New("run failed")
	if err := reporter.Summary(SummaryRecord{
		Execution: mustProjectRaceExecution(t, 1, 0, 2, 0,
			map[int]bool{0: true}, executionBatches{}, executionBatches{
				Attempted: 1, Incomplete: 1,
			}),
		ResetCount:    []int{1, 1},
		MeasuredWidth: 2,
		Completion:    failedRun(summaryErr, completionAcquisitionFailed),
	}); err != nil {
		t.Fatalf("Summary: %v", err)
	}
	lines := jsonLines(t, out.Bytes())[3:]
	var gotTrial jsonRaceTrialRecord
	if err := json.Unmarshal(lines[0], &gotTrial); err != nil {
		t.Fatalf("decode trial: %v", err)
	}
	if gotTrial.Failure == nil ||
		gotTrial.Failure.Code != "transport_error" ||
		string(gotTrial.Failure.Message) != trialErr.Error() {
		t.Errorf("trial failure = %+v", gotTrial.Failure)
	}
	before, after := gotTrial.Outcomes[0], gotTrial.Outcomes[1]
	if before.Rank != nil || before.Response != nil ||
		before.Reset == nil || before.Reset.Code != "CANCEL" ||
		before.Reset.CodeNumber != uint32(http2.ErrCodeCancel) {
		t.Errorf("before-header reset = %+v", before)
	}
	if after.Rank == nil || *after.Rank != 0 || after.Response == nil ||
		after.Response.ContentLength != nil ||
		after.Response.SHA256 != digestBase64(body) ||
		after.Reset == nil {
		t.Errorf("after-header reset = %+v", after)
	}
	var gotSummary jsonRaceResultRecord
	if err := json.Unmarshal(lines[1], &gotSummary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if gotSummary.Completion.Failure == nil || string(gotSummary.Completion.Failure.Message) != summaryErr.Error() {
		t.Errorf("completion failure = %+v", gotSummary.Completion.Failure)
	}
	if !slices.Equal(gotSummary.Aggregates.ResetCounts, []structuredResetCount{
		{
			RequestID: 0,
			Count:     1,
		},
		{
			RequestID: 1,
			Count:     1,
		},
	}) {
		t.Errorf("reset aggregate = %+v", gotSummary.Aggregates)
	}
}
