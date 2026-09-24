package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tantosec/tturl/internal/h2test"

	"github.com/tantosec/tturl/internal/ranking"
)

func startRaceJSONCatalogue(t *testing.T, w io.Writer, width int, evidence ...TrialRecord) *jsonRaceReporter {
	t.Helper()
	reporter := newJSONRaceReporter(w)
	run := RunRecord{
		RequestCount:    width,
		TrialBatchWidth: width,
		Trials:          3,
		TrialsRequested: 3,
		Argv: []string{
			"race",
		},
		DialAddress:       "example.test:443",
		ArrangementPolicy: "none",
		Delivery: deliveryPlan{
			connectionCeiling:   1,
			connectionLimit:     1,
			connectionSelection: connectionDefault,
		},
		ResponseLimits: defaultResponseLimits(),
		Padding: resolvedPadding(nil,
			width),
	}
	if len(evidence) > 0 {
		for _, outcome := range evidence[0].Outcomes {
			if outcome.Response != nil {
				for _, extract := range outcome.Response.Extracts {
					run.ExtractRegex = append(run.ExtractRegex, extract.Pattern)
					run.ExtractFields = append(run.ExtractFields, extract.Fields)
				}
				break
			}
		}
	}
	if len(run.ExtractRegex) > 0 {
		run.CaptureBodyBytes = -1
	}
	if err := reporter.Run(run); err != nil {
		t.Fatal(err)
	}

	for id := range width {
		if err := reporter.Request(RequestRecord{
			Reference:  runReference{ID: id, Label: fmt.Sprintf("Request %d", id)},
			RequestSet: "trial", URL: "https://example.test/",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return reporter
}

func startMeasureJSONCatalogue(t *testing.T, w io.Writer, m *measurement) *jsonMeasureReporter {
	t.Helper()
	requests := make([]RequestRecord, m.width)
	for id := range requests {
		requests[id] = RequestRecord{
			Reference:  runReference{ID: id, Label: fmt.Sprintf("Request %d", id)},
			RequestSet: "measured", URL: "https://example.test/",
		}
	}
	r := newJSONMeasureReporter(w, MeasureRunRecord{
		RequestCount: m.width, MeasuredBatchWidth: m.width,
		TrialsRequested: m.requested, TrialsPlanned: m.planned,
		Warmup: m.warmupPerConn, WarmupBatchWidth: m.warmupWidth,
		ArrangementPolicy: arrangementName(m.arrangement),
		Argv:              []string{"measure"}, DialAddress: "example.test:443",
		Delivery: deliveryPlan{
			connectionCeiling:   max(1, m.connsPlanned),
			connectionLimit:     max(1, m.connsPlanned),
			connectionSelection: connectionDefault,
		},
		ResponseLimits: defaultResponseLimits(),
	}, requests)
	if err := r.Run(max(1, m.connsPlanned)); err != nil {
		t.Fatal(err)
	}
	return r
}

func startDetectJSONCatalogue(t *testing.T, w io.Writer) *jsonDetectReporter {
	t.Helper()
	r := newJSONDetectReporter(w, 2)
	if err := r.Run(DetectRunRecord{
		RequestCount: 2, CandidateCount: 2, RequestedStrategy: detectStrategyAuto,
		ResolvedStrategy: detectStrategyPeerFirst,
		SolverMethod:     (&ranking.PeerFirstOutlierSolver{}).MethodID(),
		ComparisonsMax:   -1, Direction: ranking.Late, WidthRequested: 2, WidthEffective: 2,
		FalsePositiveRisk: 0.01, FalseNegativeRisk: 0.05, ConfiguredNegativeEdge: 0.1,
		Delivery: deliveryPlan{connectionSelection: connectionDefault, connectionCeiling: 1, connectionLimit: 1},
		Padding:  resolvedPadding(nil, 2), ConnectionLimit: 1, InFlightComparisonLimit: 1,
	}, []detectCapturedRequest{
		{RequestRecord: RequestRecord{Reference: runReference{ID: 0}}, Role: "candidate"},
		{RequestRecord: RequestRecord{Reference: runReference{ID: 1}}, Role: "candidate"},
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func lastJSONRecord(t *testing.T, data []byte) []byte {
	t.Helper()
	lines := jsonLines(t, data)
	return lines[len(lines)-1]
}

func TestStructuredStreamSequences(t *testing.T) {
	record := struct {
		Value string `json:"value"`
	}{"<evidence>"}
	for _, count := range []int{0, 1, 3} {
		for _, evidence := range []int{0, 1, 3} {
			t.Run(fmt.Sprintf("requests=%d/evidence=%d", count, evidence), func(t *testing.T) {
				var out bytes.Buffer
				s := newStructuredStream(&out)
				if err := s.Start(record, count); err != nil {
					t.Fatal(err)
				}
				for range count {
					if err := s.Request(record); err != nil {
						t.Fatal(err)
					}
				}
				for range evidence {
					if err := s.Evidence(record); err != nil {
						t.Fatal(err)
					}
				}
				if err := s.Finish(record); err != nil {
					t.Fatal(err)
				}
				lines := jsonLines(t, out.Bytes())
				if len(lines) != count+evidence+2 {
					t.Fatal("record count differs from calls")
				}
				for _, line := range lines {
					if string(line) != `{"value":"<evidence>"}` {
						t.Fatalf("record encoding = %s", line)
					}
				}
			})
		}
	}
	tests := []struct {
		name    string
		prepare func(*structuredStream)
		call    func(*structuredStream) error
	}{
		{"negative count", func(*structuredStream) {}, func(s *structuredStream) error { return s.Start(record, -1) }},
		{"request before start", func(*structuredStream) {}, func(s *structuredStream) error { return s.Request(record) }},
		{"evidence before start", func(*structuredStream) {}, func(s *structuredStream) error { return s.Evidence(record) }},
		{"finish before start", func(*structuredStream) {}, func(s *structuredStream) error { return s.Finish(record) }},
		{
			"duplicate start",
			func(s *structuredStream) { mustStreamCall(t, s.Start(record, 0)) },
			func(s *structuredStream) error { return s.Start(record, 0) },
		},
		{
			"request overflow",
			func(s *structuredStream) { mustStreamCall(t, s.Start(record, 0)) },
			func(s *structuredStream) error { return s.Request(record) },
		},
		{
			"request underflow",
			func(s *structuredStream) { mustStreamCall(t, s.Start(record, 1)) },
			func(s *structuredStream) error { return s.Finish(record) },
		},
		{
			"early evidence",
			func(s *structuredStream) { mustStreamCall(t, s.Start(record, 1)) },
			func(s *structuredStream) error { return s.Evidence(record) },
		},
		{
			"duplicate finish",
			func(s *structuredStream) {
				mustStreamCall(t, s.Start(record, 0))
				mustStreamCall(t, s.Finish(record))
			},
			func(s *structuredStream) error { return s.Finish(record) },
		},
		{
			"request after finish",
			func(s *structuredStream) {
				mustStreamCall(t, s.Start(record, 0))
				mustStreamCall(t, s.Finish(record))
			},
			func(s *structuredStream) error { return s.Request(record) },
		},
		{
			"evidence after finish",
			func(s *structuredStream) {
				mustStreamCall(t, s.Start(record, 0))
				mustStreamCall(t, s.Finish(record))
			},
			func(s *structuredStream) error { return s.Evidence(record) },
		},
		{
			"start after finish",
			func(s *structuredStream) {
				mustStreamCall(t, s.Start(record, 0))
				mustStreamCall(t, s.Finish(record))
			},
			func(s *structuredStream) error { return s.Start(record, 0) },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			s := newStructuredStream(&out)
			test.prepare(s)
			before := out.String()
			state := s.sequence
			if err := test.call(s); err == nil {
				t.Fatal("invalid sequence succeeded")
			}
			if out.String() != before || s.sequence != state {
				t.Fatal("rejected call changed output or sequencing")
			}
		})
	}
}

func mustStreamCall(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type partialRecordWriter struct {
	bytes.Buffer
	remaining int
	calls     int
	err       error
}

func (w *partialRecordWriter) Write(data []byte) (int, error) {
	w.calls++
	if len(data) > w.remaining {
		n := w.remaining
		_, _ = w.Buffer.Write(data[:n])
		w.remaining = 0
		return n, w.err
	}
	w.remaining -= len(data)
	return w.Buffer.Write(data)
}

func TestStructuredStreamPoisoning(t *testing.T) {
	record := struct {
		Value int `json:"value"`
	}{1}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	recordBytes := len(encoded) + 1
	for stage := range 4 {
		for _, partial := range []int{0, 5} {
			t.Run(fmt.Sprintf("stage=%d/partial=%d", stage, partial), func(t *testing.T) {
				sentinel := errors.New("destination failed")
				w := &partialRecordWriter{remaining: stage*recordBytes + partial, err: sentinel}
				s := newStructuredStream(w)
				calls := []func() error{
					func() error { return s.Start(record, 1) },
					func() error { return s.Request(record) },
					func() error { return s.Evidence(record) },
					func() error { return s.Finish(record) },
				}
				for i := range stage {
					mustStreamCall(t, calls[i]())
				}
				state := s.sequence
				if err := calls[stage](); !errors.Is(err, sentinel) {
					t.Fatalf("write error = %v", err)
				}
				if s.sequence != state {
					t.Fatal("failed write advanced state")
				}
				written := w.String()
				attempts := w.calls
				for _, call := range calls {
					if err := call(); !errors.Is(err, sentinel) {
						t.Fatal("poison did not preserve original error")
					}
				}
				if w.String() != written || w.calls != attempts {
					t.Fatal("poisoned stream attempted another write")
				}
				prefix := w.Bytes()
				if end := bytes.LastIndexByte(prefix, '\n'); end >= 0 {
					prefix = prefix[:end+1]
				} else {
					prefix = nil
				}
				if bytes.Count(prefix, []byte("\n")) != stage {
					t.Fatal("complete prefix cardinality differs from successful calls")
				}
				for line := range bytes.SplitSeq(bytes.TrimSuffix(prefix, []byte("\n")), []byte("\n")) {
					if len(line) > 0 && !json.Valid(line) {
						t.Fatal("invalid complete prefix record")
					}
				}
			})
		}
	}
	t.Run("encoding error", func(t *testing.T) {
		var out bytes.Buffer
		s := newStructuredStream(&out)
		err := s.Start(func() {}, 0)
		if err == nil {
			t.Fatal("unsupported record encoded")
		}
		if s.sequence.started {
			t.Fatal("encoding failure advanced state")
		}
		if !errors.Is(s.Finish(record), err) || out.Len() != 0 {
			t.Fatal("encoding error did not poison stream")
		}
	})
}

func TestStructuredValidatorSequences(t *testing.T) {
	number := func(n int) *int { return &n }
	run := func(n int) structuredHeader { return structuredHeader{Kind: "run", RequestCount: number(n)} }
	request := func(n int) structuredHeader { return structuredHeader{Kind: "request", RequestID: number(n)} }
	trial := structuredHeader{Kind: "trial"}
	result := structuredHeader{Kind: "result"}
	sequences := [][]structuredHeader{
		{run(0), result},
		{run(1), request(0), result},
		{run(2), request(0), request(1), trial, trial, result},
	}
	for _, headers := range sequences {
		v := structuredValidator{terminalKind: "result", evidenceKind: func(kind string) bool { return kind == "trial" }}
		for _, h := range headers {
			mustStreamCall(t, v.Accept(h))
		}
		mustStreamCall(t, v.Finish())
	}
	tests := []struct {
		name   string
		prefix []structuredHeader
		record structuredHeader
	}{
		{"negative request count", nil, run(-1)},
		{"missing request count", nil, structuredHeader{Kind: "run"}},
		{"request before run", nil, request(0)},
		{"trial before run", nil, trial},
		{"result before run", nil, result},
		{"duplicate run", []structuredHeader{run(0)}, run(0)},
		{"request overflow", []structuredHeader{run(0)}, request(0)},
		{"early trial", []structuredHeader{run(1)}, trial},
		{"request underflow", []structuredHeader{run(1)}, result},
		{"missing ID", []structuredHeader{run(1)}, structuredHeader{Kind: "request"}},
		{"non-dense ID", []structuredHeader{run(1)}, request(2)},
		{"repeated ID", []structuredHeader{run(2), request(0)}, request(0)},
		{"reordered ID", []structuredHeader{run(2)}, request(1)},
		{"duplicate result", []structuredHeader{run(0), result}, result},
		{"trial after result", []structuredHeader{run(0), result}, trial},
		{"request after result", []structuredHeader{run(0), result}, request(0)},
		{"run after result", []structuredHeader{run(0), result}, run(0)},
		{"unknown record", []structuredHeader{run(0)}, structuredHeader{Kind: "other"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			v := structuredValidator{terminalKind: "result", evidenceKind: func(kind string) bool { return kind == "trial" }}
			for _, h := range test.prefix {
				mustStreamCall(t, v.Accept(h))
			}
			before := v.sequence
			if err := v.Accept(test.record); err == nil {
				t.Fatal("invalid decoded sequence succeeded")
			}
			if v.sequence != before {
				t.Fatal("rejected header changed state")
			}
		})
	}
	t.Run("missing result", func(t *testing.T) {
		v := structuredValidator{terminalKind: "result"}
		mustStreamCall(t, v.Accept(run(0)))
		if err := v.Finish(); err == nil {
			t.Fatal("unfinished stream accepted")
		}
	})
	t.Run("command check", func(t *testing.T) {
		sentinel := errors.New("command reference failed")
		v := structuredValidator{terminalKind: "result", check: func(structuredHeader) error { return sentinel }}
		if err := v.Accept(run(0)); !errors.Is(err, sentinel) {
			t.Fatal("command error was not preserved")
		}
		if v.sequence.started {
			t.Fatal("failed command check advanced state")
		}
	})
	t.Run("retained terminal", func(t *testing.T) {
		v := structuredValidator{terminalKind: "summary"}
		mustStreamCall(t, v.Accept(run(0)))
		mustStreamCall(t, v.Accept(structuredHeader{Kind: "summary"}))
		mustStreamCall(t, v.Finish())
	})
}

func TestReadStructuredJSONRecords(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		records       int
		terminalError error
	}{
		{"complete", "{\"kind\":\"run\"}\n{\"kind\":\"result\"}\n", 2, io.EOF},
		{"truncated first", "{\"kind\":", 0, io.ErrUnexpectedEOF},
		{"truncated final", "{\"kind\":\"run\"}\n{\"kind\":", 1, io.ErrUnexpectedEOF},
		{"unterminated object", "{\"kind\":\"run\"}\n{\"kind\":\"result\"}", 1, io.ErrUnexpectedEOF},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := bufio.NewReader(strings.NewReader(test.input))
			for range test.records {
				raw, err := readStructuredJSONRecord(reader)
				if err != nil || !json.Valid(raw) {
					t.Fatalf("complete record = %s, error %v", raw, err)
				}
			}
			raw, err := readStructuredJSONRecord(reader)
			if !errors.Is(err, test.terminalError) || raw != nil {
				t.Fatalf("terminal read = %s, %v", raw, err)
			}
		})
	}
}

func TestStructuredStreamConcurrentEvidence(t *testing.T) {
	var output bytes.Buffer
	s := newStructuredStream(&output)
	mustStreamCall(t, s.Start(struct {
		Kind string `json:"kind"`
	}{"run"}, 0))
	errors := make(chan error, 32)
	var group sync.WaitGroup
	for id := range 32 {
		group.Go(func() {
			errors <- s.Evidence(struct {
				Value int `json:"value"`
			}{id})
		})
	}
	group.Wait()
	close(errors)
	for err := range errors {
		mustStreamCall(t, err)
	}
	mustStreamCall(t, s.Finish(struct {
		Kind string `json:"kind"`
	}{"result"}))
	lines := jsonLines(t, output.Bytes())
	if len(lines) != 34 {
		t.Fatal("concurrent evidence lost records")
	}
	seen := make([]bool, 32)
	for _, line := range lines[1:33] {
		var record struct {
			Value int `json:"value"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if record.Value < 0 || record.Value >= 32 || seen[record.Value] {
			t.Fatal("concurrent records were duplicated or corrupted")
		}
		seen[record.Value] = true
	}
}

func TestStructuredReportsConformToSchemas(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		_, _ = w.Write([]byte("12ms 7"))
	}))
	requests := []string{
		"--insecure", "--connections", "1",
		"--block", "https://" + addr + "/a", "--name", "A",
		"--block", "https://" + addr + "/b", "--name", "B",
	}
	tests := []struct {
		name      string
		run       func([]string, commandOutput) error
		args      []string
		schema    string
		wantKinds []string
	}{
		{
			name: "race",
			run: func(args []string, out commandOutput) error {
				return runRace(t.Context(), args, out)
			},
			args: []string{
				"--trials", "1", "--capture-body", "unlimited",
				"--extract-regex", `([0-9]+)`,
				"--extract-regex", `(?P<number>[0-9]+)(?P<unit>ms)?`,
			},
			schema:    raceSchema.filename,
			wantKinds: []string{"run", "request", "request", "trial", "result"},
		},
		{
			name: "measure",
			run: func(args []string, out commandOutput) error {
				return runMeasure(t.Context(), args, out)
			},
			args:      []string{"--trials", "2"},
			schema:    measureSchema.filename,
			wantKinds: []string{"run", "request", "request", "result"},
		},
		{
			name: "analyse",
			run: func(args []string, out commandOutput) error {
				return runAnalyse(t.Context(), args, out)
			},
			args:   []string{"--cycles", "6"},
			schema: analyseSchema.filename,
			wantKinds: []string{
				"run", "request", "request",
				"trial", "trial", "trial", "trial", "trial", "trial",
				"trial", "trial", "trial", "trial", "trial", "trial",
				"result",
			},
		},
		{
			name: "detect",
			run: func(args []string, out commandOutput) error {
				return runDetect(t.Context(), args, out)
			},
			args: []string{
				"--direction", "late", "--comparisons-max", "1",
			},
			schema:    detectSchema.filename,
			wantKinds: []string{"run", "request", "request", "result"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schema := compileStructuredSchema(t, test.schema)
			out, stdout, _ := captureOutput(t)
			args := append([]string{"--report", "json"}, test.args...)
			args = append(args, requests...)
			if err := test.run(args, out); err != nil &&
				!errors.Is(err, errAnalyseUnavailable) {
				t.Fatalf("run: %v", err)
			}
			records := jsonLines(t, stdout.Bytes())
			kinds := make([]string, len(records))
			for i, record := range records {
				instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(record))
				if err != nil {
					t.Fatalf("record %d JSON: %v", i, err)
				}
				if err := schema.Validate(instance); err != nil {
					t.Fatalf("record %d does not conform: %v\n%s", i, err, record)
				}
				kinds[i] = recordKind(t, record)
			}
			if !slices.Equal(kinds, test.wantKinds) {
				t.Fatalf("record sequence = %q, want %q", kinds, test.wantKinds)
			}

			validateStructuredRecords(t, records)
		})
	}
}

func validateStructuredRecords(t *testing.T, lines [][]byte) {
	t.Helper()
	records := make([]conformanceRecord, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal(line, &records[i]); err != nil {
			t.Fatal(err)
		}
	}
	validateConformanceStream(t, records)
}

// conformanceRecord decodes only fields needed for whole-stream relationships.
// Record shapes are validated separately against their selected schema.
type conformanceRecord struct {
	Kind         string                 `json:"kind"`
	Schema       string                 `json:"schema"`
	Command      string                 `json:"command"`
	RequestCount int                    `json:"request_count"`
	RequestID    int                    `json:"request_id"`
	Role         string                 `json:"role"`
	Label        string                 `json:"label"`
	Body         *structuredRequestBody `json:"body"`
	Acquisition  struct {
		Delivery structuredDelivery `json:"delivery"`
		Capture  structuredCapture  `json:"capture"`
	} `json:"acquisition"`
	Plan struct {
		BatchWidth int                 `json:"batch_width"`
		Extractors []jsonRaceExtractor `json:"extractors"`
		Width      struct {
			Effective int `json:"effective"`
		} `json:"width"`
	} `json:"plan"`
	TrialIndex *int `json:"trial_index"`
	Outcomes   []struct {
		RequestID int               `json:"request_id"`
		Position  int               `json:"position"`
		Rank      *int              `json:"rank"`
		Response  *jsonRaceResponse `json:"response"`
	} `json:"outcomes"`
	Assignments []struct {
		RequestID int `json:"request_id"`
		Position  int `json:"position"`
	} `json:"assignments"`
	Execution struct {
		RequestOperations struct {
			Retained int `json:"retained"`
		} `json:"request_operations"`
		Trials      conformanceWork `json:"trials"`
		Comparisons conformanceWork `json:"comparisons"`
		Priming     struct {
			Batches conformanceWork `json:"batches"`
		} `json:"priming"`
	} `json:"execution"`
	Measurement struct {
		Requests []struct {
			RequestID  int `json:"request_id"`
			Samples    int `json:"samples"`
			RankCounts []struct {
				Rank  int `json:"rank"`
				Count int `json:"count"`
			} `json:"rank_counts"`
		} `json:"requests"`
		Pairwise struct {
			Before []int   `json:"before_request_ids"`
			After  []int   `json:"after_request_ids"`
			Counts [][]int `json:"counts"`
		} `json:"pairwise_precedence"`
		Assignments struct {
			RequestIDs []int   `json:"request_ids"`
			Positions  []int   `json:"positions"`
			Counts     [][]int `json:"counts"`
		} `json:"assignments"`
	} `json:"measurement"`
	Analysis struct {
		Requests []struct {
			RequestID int `json:"request_id"`
		} `json:"requests"`
		Pairs []struct {
			Left  int `json:"left_request_id"`
			Right int `json:"right_request_id"`
		} `json:"pair_tests"`
	} `json:"analysis"`
	Detection struct {
		Finding struct {
			RequestID *int `json:"request_id"`
		} `json:"finding"`
		Requests []struct {
			RequestID int `json:"request_id"`
			Physical  struct {
				Participations int      `json:"participations"`
				Mean           *float64 `json:"mean_rank"`
				Normalised     *float64 `json:"normalised_mean_rank"`
			} `json:"physical"`
			Inference *struct {
				Samples int `json:"samples"`
			} `json:"inference"`
		} `json:"requests"`
	} `json:"detection"`
}

type conformanceWork struct {
	Attempted    int  `json:"attempted"`
	RankComplete int  `json:"rank_complete"`
	Incomplete   int  `json:"incomplete"`
	Retained     *int `json:"retained"`
	Excluded     *int `json:"excluded"`
	Planned      *int `json:"planned"`
	Maximum      *int `json:"maximum"`
	Unattempted  *int `json:"unattempted"`
}

func validateConformanceStream(t *testing.T, records []conformanceRecord) {
	t.Helper()
	if len(records) == 0 {
		t.Fatal("structured stream is empty")
	}
	catalogue := structuredCatalogue{command: records[0].Command}
	var requestRole string
	var request structuredRequest
	labels := make(map[string]bool)
	validator := structuredValidator{
		check: func(header structuredHeader) error {
			if header.Kind == "request" {
				if err := validateStructuredRequest(request, labels); err != nil {
					return err
				}
			}
			return catalogue.Accept(header, requestRole)
		},
		terminalKind: "result",
		evidenceKind: func(kind string) bool { return kind == "trial" || kind == "warmup" },
	}
	for _, record := range records {
		requestRole = record.Role
		request = structuredRequest{Label: record.Label, Body: record.Body}
		header := structuredHeader{
			Kind: record.Kind, RequestCount: &record.RequestCount,
			RequestID: &record.RequestID,
		}
		if err := validator.Accept(header); err != nil {
			t.Fatal(err)
		}
	}
	if err := validator.Finish(); err != nil {
		t.Fatal(err)
	}
	run := records[0]
	roles := catalogue.roles
	width := run.Plan.BatchWidth
	if run.Command == "detect" {
		width = run.Plan.Width.Effective
	}
	validateConformanceDelivery(t, run.Acquisition.Delivery, width)
	checkID := func(id int, allowed ...string) {
		t.Helper()
		if err := catalogue.CheckID(id, allowed...); err != nil {
			t.Fatal(err)
		}
	}
	indices := make(map[int]bool)
	for _, record := range records[run.RequestCount+1 : len(records)-1] {
		if index := record.TrialIndex; index != nil {
			if indices[*index] {
				t.Fatal("trial indices must be unique")
			}
			indices[*index] = true
		}
		positions := make(map[int]bool)
		requests := make(map[int]bool)
		ranks := make(map[int]bool)
		if record.Kind != "trial" && record.Kind != "warmup" {
			t.Fatalf("invalid evidence kind %q", record.Kind)
		}
		for _, a := range record.Assignments {
			checkID(a.RequestID, "measured")
			if a.Position < 0 || a.Position >= width {
				t.Fatal("assignment outside batch")
			}
		}
		for i, o := range record.Outcomes {
			if o.Response != nil {
				if run.Command == "race" {
					if err := validateStructuredExtracts(*o.Response, run.Acquisition.Capture, run.Plan.Extractors); err != nil {
						t.Fatal(err)
					}
				} else if _, err := validateStructuredResponse(o.Response.structuredResponse, run.Acquisition.Capture); err != nil {
					t.Fatal(err)
				}
			}
			outOfOrder := i > 0 && o.RequestID <= record.Outcomes[i-1].RequestID
			if positions[o.Position] || requests[o.RequestID] || outOfOrder {
				t.Fatal("outcomes need unique positions and increasing request IDs")
			}
			positions[o.Position], requests[o.RequestID] = true, true
			if o.Rank != nil {
				if ranks[*o.Rank] {
					t.Fatal("observed ranks must be unique")
				}
				ranks[*o.Rank] = true
			}
			role := "measured"
			if record.Kind == "warmup" && slices.Contains(roles, "warmup") {
				role = "warmup"
			}
			checkID(o.RequestID, role)
			if o.Position < 0 || (record.Kind == "trial" && o.Position >= width) {
				t.Fatal("outcome position outside batch")
			}
			if o.Rank != nil && (*o.Rank < 0 || (record.Kind == "trial" && *o.Rank >= width)) {
				t.Fatal("rank outside batch")
			}
		}
	}
	result := records[len(records)-1]
	work := []conformanceWork{
		result.Execution.Trials,
		result.Execution.Comparisons,
		result.Execution.Priming.Batches,
	}
	for _, w := range work {
		if w.Attempted != w.RankComplete+w.Incomplete {
			t.Fatal("attempted must equal rank-complete plus incomplete")
		}
		if w.Retained != nil && w.Excluded != nil && w.RankComplete != *w.Retained+*w.Excluded {
			t.Fatal("rank-complete must equal retained plus excluded")
		}
		bound := w.Planned
		if bound == nil {
			bound = w.Maximum
		}
		if bound != nil && w.Unattempted != nil && *bound != w.Attempted+*w.Unattempted {
			t.Fatal("bounded work must equal attempted plus unattempted")
		}
	}
	for i, r := range result.Measurement.Requests {
		if i > 0 && r.RequestID <= result.Measurement.Requests[i-1].RequestID {
			t.Fatal("request aggregates must increase by ID")
		}
		checkID(r.RequestID, "measured")
		count := 0
		previous := -1
		for _, h := range r.RankCounts {
			if h.Rank <= previous || h.Rank >= width || h.Count <= 0 {
				t.Fatal("histogram needs increasing ranks and positive counts")
			}
			previous = h.Rank
			count += h.Count
		}
		if count != r.Samples {
			t.Fatal("histogram must sum to samples")
		}
	}
	checkMatrix := func(rows, columns []int, counts [][]int, requests bool) {
		t.Helper()
		if len(counts) != len(rows) {
			t.Fatal("matrix row dimension differs from axis")
		}
		for i, id := range rows {
			checkID(id, "measured")
			if i > 0 && id <= rows[i-1] {
				t.Fatal("row axis must increase")
			}
			if len(counts[i]) != len(columns) {
				t.Fatal("matrix column dimension differs from axis")
			}
		}
		for i, id := range columns {
			if requests {
				checkID(id, "measured")
			} else if id < 0 || id >= width {
				t.Fatal("position axis outside batch")
			}
			if i > 0 && id <= columns[i-1] {
				t.Fatal("column axis must increase")
			}
		}
	}
	if run.Command == "measure" {
		p := result.Measurement.Pairwise
		checkMatrix(p.Before, p.After, p.Counts, true)
		a := result.Measurement.Assignments
		checkMatrix(a.RequestIDs, a.Positions, a.Counts, false)
		if len(result.Measurement.Requests) != width || len(a.RequestIDs) != width ||
			len(p.Before) != width || len(p.After) != width || len(a.Positions) != width {
			t.Fatal("measurement matrices must cover the measured batch")
		}
		for i, request := range result.Measurement.Requests {
			if a.RequestIDs[i] != request.RequestID || p.Before[i] != request.RequestID || p.After[i] != request.RequestID {
				t.Fatal("measurement axes must identify the request aggregates")
			}
			total := 0
			for j, count := range a.Counts[i] {
				if count < 0 || p.Counts[i][j] < 0 {
					t.Fatal("matrix counts must be non-negative")
				}
				total += count
				if i == j {
					if p.Counts[i][j] != 0 {
						t.Fatal("self-precedence must be zero")
					}
				} else if p.Counts[i][j]+p.Counts[j][i] != request.Samples {
					t.Fatal("opposite precedence counts must sum to retained samples")
				}
			}
			if total != request.Samples {
				t.Fatal("assignment row must sum to retained samples")
			}
			if result.Execution.Trials.Retained == nil || request.Samples != *result.Execution.Trials.Retained {
				t.Fatal("each measured request must sample every retained trial")
			}
		}
	}
	for _, r := range result.Analysis.Requests {
		checkID(r.RequestID, "measured")
	}
	for _, p := range result.Analysis.Pairs {
		checkID(p.Left, "measured")
		checkID(p.Right, "measured")
	}
	physical := 0
	for i, r := range result.Detection.Requests {
		checkID(r.RequestID, "candidate", "baseline")
		if i > 0 && r.RequestID <= result.Detection.Requests[i-1].RequestID {
			t.Fatal("request aggregates must increase by ID")
		}
		physical += r.Physical.Participations
		unobserved := r.Physical.Mean == nil
		if unobserved != (r.Physical.Normalised == nil) || unobserved != (r.Physical.Participations == 0) {
			t.Fatal("physical means must be observed exactly when participations are positive")
		}
		if r.Physical.Mean != nil {
			expected := 0.0
			if width > 1 {
				expected = *r.Physical.Mean / float64(width-1)
			}
			if math.Abs(expected-*r.Physical.Normalised) > 1e-12 {
				t.Fatal("normalised physical mean differs from its denominator")
			}
		}
		if r.Inference != nil {
			checkID(r.RequestID, "candidate")
			if r.Inference.Samples > r.Physical.Participations {
				t.Fatal("inferential samples exceed physical participation")
			}
		}
	}
	if run.Command == "detect" && physical != result.Execution.RequestOperations.Retained {
		t.Fatal("physical participations must sum to retained request operations")
	}
	if id := result.Detection.Finding.RequestID; id != nil {
		checkID(*id, "candidate")
	}
}

func validateConformanceDelivery(t *testing.T, d structuredDelivery, width int) {
	t.Helper()
	if err := validateStructuredDelivery(d, width); err != nil {
		t.Fatal(err)
	}
}
