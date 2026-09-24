package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/timing"
)

func compileTimeSchema(t *testing.T, definition string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(offlineSchemaLoader{})
	target := ""
	for _, descriptor := range []*schemaDescriptor{&commonSchema, &timeSchema} {
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(descriptor.document))
		if err != nil {
			t.Fatal(err)
		}
		id := document.(map[string]any)["$id"].(string)
		if err := compiler.AddResource(id, document); err != nil {
			t.Fatal(err)
		}
		target = id
	}
	if definition != "" {
		target += "#/$defs/" + definition
	}
	schema, err := compiler.Compile(target)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestTimeSchemaValueDomains(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		definition, instance string
		valid                bool
	}{
		{"count", "9007199254740993", true},
		{"count", "18446744073709551615", true},
		{"count", "18446744073709551616", false},
		{"count", "1.5", false},
		{"count", "-1", false},
		{"offset_ns", "null", true},
		{"offset_ns", "0", true},
		{"offset_ns", "-1", true},
		{"offset_ns", "1.5", false},
		{"offset_ns", "9223372036854775808", false},
		{"protocol", `"h2"`, true},
		{"protocol", `"http/1.1"`, true},
		{"protocol", `"h3"`, false},
		{"started_at", `null`, false},
		{"started_at", `"2026-09-18T00:00:00Z"`, true},
		{"failure", `null`, true},
		{"failure", `{"phase":"execution","code":"transport","message":"failed"}`, true},
		{"failure", `{"phase":"preparation","code":"transport","message":"failed"}`, false},
		{"failure", `{"phase":"execution","code":"transport"}`, false},
		{"header_map", `{"X-Opaque":["$B64[/w==]"]}`, true},
		{"header_map", `{"X-Opaque":"value"}`, false},
	} {
		t.Run(test.definition+"/"+test.instance, func(t *testing.T) {
			instance, err := jsonschema.UnmarshalJSON(strings.NewReader(test.instance))
			if err != nil {
				t.Fatal(err)
			}
			err = compileTimeSchema(t, test.definition).Validate(instance)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%t, want %t: %v", err == nil, test.valid, err)
			}
		})
	}
}

func TestTimeSchemaMilestonesRequireAbsenceReasons(t *testing.T) {
	t.Parallel()
	schema := compileTimeSchema(t, "timing")
	names := []string{
		"initial_release_ns", "final_release_ns", "write_complete_ns",
		"first_response_headers_ns", "final_response_headers_ns", "response_complete_ns", "duration_ns",
	}
	value := map[string]any{"missing_reasons": map[string]any{}}
	for _, name := range names {
		value[name] = json.Number("0")
	}
	value["duration_ns"] = json.Number("-5")
	if err := schema.Validate(value); err != nil {
		t.Fatal(err)
	}
	value["response_complete_ns"] = nil
	if err := schema.Validate(value); err == nil {
		t.Fatal("missing endpoint omitted its reason")
	}
	value["missing_reasons"].(map[string]any)["response_complete_ns"] = "not observed"
	if err := schema.Validate(value); err != nil {
		t.Fatal(err)
	}
	value["response_complete_ns"] = json.Number("0")
	if err := schema.Validate(value); err == nil {
		t.Fatal("valid zero endpoint was labelled missing")
	}
}

func TestTimeSchemaOwnsDurationRunEnvelope(t *testing.T) {
	t.Parallel()
	schema := compileTimeSchema(t, "")
	var document map[string]any
	if err := json.Unmarshal(timeSchemaDocument, &document); err != nil {
		t.Fatal(err)
	}
	if len(document["oneOf"].([]any)) != 6 {
		t.Fatal("record family omitted an evidence kind")
	}
	var rejectOrdinalEnvelope func(any)
	rejectOrdinalEnvelope = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if ref, ok := value["$ref"].(string); ok && strings.Contains(ref, "#/$defs/run_envelope") &&
				!strings.HasPrefix(ref, "#") {
				t.Fatal("duration records inherited an ordinal command envelope")
			}
			for _, child := range value {
				rejectOrdinalEnvelope(child)
			}
		case []any:
			for _, child := range value {
				rejectOrdinalEnvelope(child)
			}
		}
	}
	rejectOrdinalEnvelope(document)
	if err := schema.Validate(map[string]any{"kind": "connection"}); err == nil {
		t.Fatal("standalone connection record replaced inline physical history")
	}
}

func TestTimeReporterRecordsConformToSchema(t *testing.T) {
	cfg, err := parseTimeFlagsForTest(t, "--report", "json", "--capture-headers",
		"--capture-body", "unlimited", "--extract-regex", `(?P<number>[0-9]+)ms`)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://fixture.test/opaque", nil)
	if err != nil {
		t.Fatal(err)
	}
	requests := timeRequests{
		Measured: []timeRequest{{ID: 0, Request: httpRequest, Protocol: curlblocks.HTTP2, Label: "measured"}},
		Priming: []timeRequest{{
			ID: 1, Request: httpRequest.Clone(t.Context()), Protocol: curlblocks.HTTP2,
			Label: "priming", Warmup: true,
		}},
	}
	resolved, err := timing.Resolve(timing.Plan{
		Requests: []timing.Request{{ID: 0, HTTP: httpRequest, Protocol: timing.HTTP2}},
		Priming:  []timing.Request{{ID: 1, HTTP: httpRequest.Clone(t.Context()), Protocol: timing.HTTP2}},
		Trials:   1, Arrangement: timing.ArrangeNone, Warmup: 1,
		ReceiveHeaderMax: 4096, ResponseBodyMax: -1,
		Capture: timing.Capture{Headers: true, Body: true, BodyMax: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	reporter := newTimeReporter(newReportWriter(&output), cfg, requests, resolved, buildinfo.Info{}, []string{"time"})
	if err := reporter.Run(); err != nil {
		t.Fatal(err)
	}
	anchor := time.Now()
	if err := reporter.Observe(timing.Event{Connection: &timing.Connection{
		ID: 1, Protocol: timing.HTTP2,
		NegotiatedProtocol: "h2", State: "acquired",
	}}); err != nil {
		t.Fatal(err)
	}
	body := []byte{'7', 'm', 's', ' ', 0xff}
	offset := func(ns int64) timing.Offset { return timing.Offset{NS: ns, Present: true} }
	outcome := timing.Outcome{
		RequestID: 1, ConnectionID: 1, Protocol: timing.HTTP2, Attempted: true,
		Timing: timing.Timing{
			InitialRelease: offset(4), FinalRelease: offset(5), WriteComplete: offset(6),
			FirstResponseHeaders: offset(1), FinalResponseHeaders: offset(2), ResponseComplete: offset(7), Duration: offset(-2),
		},
		Response: timing.Response{
			Status: 200, ReceivedBodyBytes: 5, AcceptedBodyBytes: 5, Digest: digestBase64(body),
			Complete: true, Headers: http.Header{"X-Opaque": {string([]byte{0xff})}}, Body: body,
		},
	}
	if err := reporter.Observe(timing.Event{Phase: &timing.PhaseCompletion{
		Phase: "initial_acquisition", WorkerID: -1,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Observe(timing.Event{Warmup: &timing.Warmup{
		Phase: "initial", PhaseIndex: 0, TrialIndex: 0, StartedAt: anchor,
		Order: []int{1}, Outcomes: []timing.Outcome{outcome}, Complete: true,
		InitialGate: offset(0), FinalGate: offset(0),
	}}); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []timing.PhaseCompletion{
		{Phase: "worker_warmup", WorkerID: 0},
		{Phase: "initial_warmup", WorkerID: -1},
	} {
		if err := reporter.Observe(timing.Event{Phase: &phase}); err != nil {
			t.Fatal(err)
		}
	}
	outcome.RequestID = 0
	if err := reporter.Observe(timing.Event{Trial: &timing.Trial{
		StartedAt: anchor,
		Order:     []int{0}, Outcomes: []timing.Outcome{outcome}, Complete: true,
		InitialGate: offset(0), FinalGate: offset(0),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := reporter.Summary(timing.Result{StartedAt: anchor, Completion: timing.CompletionComplete}); err != nil {
		t.Fatal(err)
	}
	schema := compileTimeSchema(t, "")
	seen := make(map[string]int)
	for record := range bytes.SplitSeq(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(record))
		if err != nil {
			t.Fatal(err)
		}
		kind := instance.(map[string]any)["kind"].(string)
		if err := schema.Validate(instance); err != nil {
			t.Fatalf("%s record: %v", kind, err)
		}
		seen[kind]++
		if kind == "run" {
			experiment := instance.(map[string]any)["experiment"].(map[string]any)
			tls := experiment["tls"]
			delete(experiment, "tls")
			if err := schema.Validate(instance); err == nil {
				t.Fatal("run omitted resolved transport policy provenance")
			}
			experiment["tls"] = tls
			experiment["arrangement"] = "unknown"
			if err := schema.Validate(instance); err == nil {
				t.Fatal("run accepted an unspecified arrangement")
			}
		}
		if kind == "trial" {
			outcomes := instance.(map[string]any)["outcomes"].([]any)
			response := outcomes[0].(map[string]any)["response"].(map[string]any)
			extracts := response["extracts"]
			delete(response, "extracts")
			if err := schema.Validate(instance); err == nil {
				t.Fatal("captured body omitted its derived evidence catalogue")
			}
			response["extracts"] = extracts
			delete(instance.(map[string]any), "started_at")
			if err := schema.Validate(instance); err == nil {
				t.Fatal("trial omitted its coordinate-system provenance")
			}
		}
	}
	for _, kind := range []string{"run", "request", "phase", "warmup", "trial", "result"} {
		if seen[kind] == 0 {
			t.Fatalf("reporter omitted %s record", kind)
		}
	}
}

func TestTimeMixedProtocolCommandConformsToSchema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("live request-duration command is unavailable on Windows")
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte{'7', 'm', 's', ' ', 0xff})
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	var stdout, stderr bytes.Buffer
	err := runTimeCommand(t.Context(), commandByID(commandTime), commandCatalogue, []string{
		"--insecure", "--report", "json", "--capture-headers", "--capture-body", "unlimited",
		"--extract-regex", `(?P<number>[0-9]+)ms`, "--warmup", "1", "--trials", "1",
		"--synchronise", "--last-byte-sync",
		"--block", server.URL + "/a", "--http1.1", "--block", server.URL + "/b",
	}, commandOutput{stdout: &stdout, stderr: &stderr})
	if err != nil {
		t.Fatalf("mixed-protocol command: %v\n%s", err, stderr.String())
	}
	schema := compileTimeSchema(t, "")
	seen := make(map[string]int)
	for record := range bytes.SplitSeq(bytes.TrimSpace(stdout.Bytes()), []byte{'\n'}) {
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(record))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Fatalf("live record: %v\n%s", err, record)
		}
		seen[instance.(map[string]any)["kind"].(string)]++
	}
	for kind, want := range map[string]int{"run": 1, "request": 2, "warmup": 1, "trial": 1, "result": 1} {
		if seen[kind] != want {
			t.Fatalf("%s records=%d, want %d", kind, seen[kind], want)
		}
	}
}

func TestTimeCompletionDispositions(t *testing.T) {
	schema := compileTimeSchema(t, "completion")
	for _, result := range []timing.Result{
		{Completion: timing.CompletionComplete},
		{Completion: timing.CompletionStopped},
		{Completion: timing.CompletionFailed, Failure: &timing.Failure{
			Phase: timing.PhaseExecution, Code: "cancelled", Message: "Request cancelled",
		}},
	} {
		encoded, err := json.Marshal(timeCompletion(result))
		if err != nil {
			t.Fatal(err)
		}
		value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err != nil {
			t.Fatal(err)
		}
	}
	for _, input := range []string{
		`{"state":"complete","failure":{"phase":"run","code":"transport","message":"Failed"}}`,
		`{"state":"failed","failure":null}`,
		`{"state":"stopped","failure":null}`,
		`{"state":"stopped","failure":{"phase":"run","code":"timeout","message":"Stopped"}}`,
		`{"state":"failed","failure":{"phase":"run","code":"transport","message":""}}`,
		`{"state":"failed","failure":{"phase":"run","code":"","message":"Failed"}}`,
	} {
		value, err := jsonschema.UnmarshalJSON(strings.NewReader(input))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(value); err == nil {
			t.Fatalf("Contradictory disposition accepted: %s", input)
		}
	}
}

func TestTimeResponseCaptureEvidence(t *testing.T) {
	schema := compileTimeSchema(t, "response")
	for _, body := range [][]byte{[]byte("plain text"), {0, 0xff, '\n'}} {
		for _, enabled := range []bool{false, true} {
			r := &timeReport{}
			if enabled {
				r.cfg.capture.bodyBytes = -1
				r.cfg.capture.headers = true
			}
			response := r.outcomes([]timing.Outcome{{
				Timing: timing.Timing{FirstResponseHeaders: timing.Offset{Present: true}},
				Response: timing.Response{
					Status: 200, ReceivedBodyBytes: int64(len(body)), AcceptedBodyBytes: int64(len(body)),
					Digest: digestBase64(body), Body: body,
				},
			}})[0].(map[string]any)["response"].(map[string]any)
			encoded, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			value, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(value); err != nil {
				t.Fatal(err)
			}
			if enabled {
				capture := response["body"].(map[string]any)
				decoded, err := base64.StdEncoding.DecodeString(capture["data"].(string))
				if err != nil || !bytes.Equal(decoded, body) || capture["encoding"] != "base64" {
					t.Fatal("Captured bytes changed")
				}
				if response["body_captured_bytes"] != int64(len(body)) || response["body_captured_complete"] != true {
					t.Fatal("Capture coverage lost accepted bytes")
				}
			} else if response["body"] != nil || response["headers"] != nil || response["trailers"] != nil {
				t.Fatal("Disabled capture supplied data")
			}
		}
	}
	partialReporter := &timeReport{cfg: timeCommandConfig{capture: raceCaptureConfig{bodyBytes: 2}}}
	partial := partialReporter.outcomes([]timing.Outcome{{Response: timing.Response{
		Status: 200, ReceivedBodyBytes: 7, AcceptedBodyBytes: 5,
		Digest: digestBase64([]byte("whole")), Body: []byte("wh"), CaptureTruncated: true,
	}}})[0].(map[string]any)["response"].(map[string]any)
	if partial["body_bytes"] != int64(5) || partial["received_body_bytes"] != int64(7) ||
		partial["body_captured_bytes"] != int64(2) || partial["body_captured_complete"] != false {
		t.Fatal("Partial capture lost received, accepted or retained byte distinctions")
	}
	r := &timeReport{}
	unknown := r.outcomes([]timing.Outcome{{}})[0].(map[string]any)["response"].(map[string]any)
	if unknown["status"] != nil || unknown["sha256"] != nil || unknown["content_length"] != nil {
		t.Fatal("Unavailable response facts were supplied")
	}
}

func TestTimeUnlimitedPolicyProjection(t *testing.T) {
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://fixture.test/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := timing.Resolve(timing.Plan{
		Requests:    []timing.Request{{HTTP: request, Protocol: timing.HTTP2}},
		Arrangement: timing.ArrangeRandom, ReceiveHeaderMax: 4096, ResponseBodyMax: -1,
		Capture: timing.Capture{Body: true, BodyMax: -1},
	})
	if err != nil {
		t.Fatal(err)
	}
	value := timeExperiment(resolved)
	for _, name := range []string{
		"requested_trials", "planned_trials", "planned_request_operations", "request_timeout_ns",
		"run_timeout_ns", "batch_rate_max_per_second", "request_rate_max_per_second",
		"response_body_max_bytes", "capture_body_max_bytes",
	} {
		if value[name] != nil {
			t.Fatalf("Unlimited %s supplied a finite bound", name)
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if err := compileTimeSchema(t, "experiment").Validate(instance); err != nil {
		t.Fatal(err)
	}
	resolved.Plan.Capture = timing.Capture{}
	if timeExperiment(resolved)["capture_body_max_bytes"] != int64(0) {
		t.Fatal("Disabled capture is not zero")
	}
}

func TestTimeFailureDiagnosticFallback(t *testing.T) {
	for _, failure := range []*timing.Failure{
		{Phase: timing.PhaseRun, Code: "operational"},
		{Phase: timing.PhaseRun, Cause: errors.New("")},
	} {
		value := timeFailure(failure).(map[string]any)
		if value["message"] == wireString("") || value["code"] == wireString("") {
			t.Fatal("Failure diagnostic omitted a condition or message")
		}
	}
}

func TestTimeResultRejectsContradictoryCompletionBeforeOutput(t *testing.T) {
	for _, result := range []timing.Result{
		{Completion: timing.CompletionComplete, Failure: &timing.Failure{}},
		{Completion: timing.CompletionStopped, Failure: &timing.Failure{}},
		{Completion: timing.CompletionFailed},
	} {
		var output bytes.Buffer
		r := &timeReport{stream: newStructuredStream(&output)}
		result.StartedAt = time.Now()
		if err := r.Summary(result); err == nil || output.Len() != 0 {
			t.Fatal("Contradictory completion produced output")
		}
	}
}

func TestTimeHTTP1StatusEvidenceConformsToSchema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("live request-duration command is unavailable on Windows")
	}
	schema := compileTimeSchema(t, "")
	for _, test := range []struct {
		wire   string
		status int
		valid  bool
	}{
		{"+20 Invalid", 20, false},
		{"-20 Invalid", -20, false},
		{"600 Invalid", 600, false},
		{"200 OK", 200, true},
		{"000 Invalid", 0, false},
		{"bad Invalid", 0, false},
	} {
		t.Run(test.wire, func(t *testing.T) {
			listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			served := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					served <- err
					return
				}
				defer func() { _ = conn.Close() }()
				if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					served <- err
					return
				}
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						served <- err
						return
					}
					if line == "\r\n" {
						break
					}
				}
				_, err = io.WriteString(conn, "HTTP/1.1 "+test.wire+"\r\nX-Evidence: retained\r\nContent-Length: 0\r\n\r\n")
				served <- err
			}()
			var stdout, stderr bytes.Buffer
			err = runTimeCommand(t.Context(), commandByID(commandTime), commandCatalogue, []string{
				"--report", "json", "--trials", "1", "--arrange", "none",
				"--single-record=false", "--capture-headers", "--run-timeout", "2s",
				"--http1.1", "http://" + listener.Addr().String() + "/",
			}, commandOutput{stdout: &stdout, stderr: &stderr})
			if err != nil {
				t.Fatalf("command: %v\n%s", err, stderr.String())
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
			seen := make(map[string]int)
			for line := range bytes.SplitSeq(bytes.TrimSpace(stdout.Bytes()), []byte{'\n'}) {
				value, err := jsonschema.UnmarshalJSON(bytes.NewReader(line))
				if err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(value); err != nil {
					t.Fatalf("record: %v\n%s", err, line)
				}
				record := value.(map[string]any)
				kind := record["kind"].(string)
				seen[kind]++
				if kind == "trial" {
					outcome := record["outcomes"].([]any)[0].(map[string]any)
					response := outcome["response"].(map[string]any)
					complete, digest := response["complete"], response["sha256"]
					response["complete"], response["sha256"] = true, digestBase64(nil)
					if (schema.Validate(value) == nil) != test.valid {
						t.Fatalf("validated response accepted invalid status: %v", response["status"])
					}
					response["complete"], response["sha256"] = complete, digest
					var status any
					if test.status != 0 {
						status = json.Number(strconv.Itoa(test.status))
					}
					if fmt.Sprint(response["status"]) != fmt.Sprint(status) {
						t.Fatalf("status=%v, want %v", response["status"], status)
					}
					headers := response["headers"].(map[string]any)
					if headers["X-Evidence"].([]any)[0] != "retained" {
						t.Fatal("response header evidence lost")
					}
					timing := outcome["timing"].(map[string]any)
					if (timing["duration_ns"] != nil) != test.valid ||
						(timing["final_response_headers_ns"] != nil) != test.valid ||
						(outcome["failure"] == nil) != test.valid {
						t.Fatalf("valid=%t outcome=%v", test.valid, outcome)
					}
				}
				if kind == "result" {
					quality := record["summaries"].([]any)[0].(map[string]any)["quality"].(map[string]any)
					statuses := quality["statuses"].(map[string]any)
					want := 0
					if test.status != 0 {
						want = 1
						if fmt.Sprint(statuses[strconv.Itoa(test.status)]) != "1" {
							t.Fatalf("status counts=%v", statuses)
						}
					}
					if len(statuses) != want {
						t.Fatalf("status counts=%v", statuses)
					}
				}
			}
			for kind, want := range map[string]int{"run": 1, "request": 1, "trial": 1, "result": 1} {
				if seen[kind] != want {
					t.Fatalf("%s records=%d, want %d", kind, seen[kind], want)
				}
			}
		})
	}
}
