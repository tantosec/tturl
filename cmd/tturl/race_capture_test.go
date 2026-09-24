package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tantosec/tturl/tth2"
)

type trackedReadCloser struct {
	reader   io.Reader
	closeErr error
	closed   bool
}

func (r *trackedReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *trackedReadCloser) Close() error {
	r.closed = true
	return r.closeErr
}

type dataErrorReader struct {
	data []byte
	err  error
}

func (r *dataErrorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestParseRaceCapture(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		headers  bool
		body     string
		patterns []string
		format   reportFormat
		wantErr  string
	}{
		{
			name:   "metadata text",
			format: reportText,
		},
		{
			name:    "headers need JSON",
			headers: true,
			format:  reportText,
			wantErr: "requires --report json",
		},
		{
			name:    "invalid body size",
			body:    "everything",
			format:  reportJSON,
			wantErr: "--capture-body",
		},
		{
			name:     "invalid expression",
			body:     "1KiB",
			patterns: []string{"("},
			format:   reportJSON,
			wantErr:  "--extract-regex",
		},
		{
			name:     "duplicate named group",
			body:     "unlimited",
			patterns: []string{"(?P<number>a)(?P<number>b)"},
			format:   reportJSON,
			wantErr:  `more than one group named "number"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseRaceCapture(
				tc.headers, tc.body, tc.patterns, tc.format)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("parseRaceCapture: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseRaceCaptureErrorIsASCII(t *testing.T) {
	t.Parallel()
	_, err := parseRaceCapture(
		false, "1B", []string{"\u2603("}, reportJSON)
	if err == nil {
		t.Fatal("parseRaceCapture accepted an invalid expression")
	}
	for _, b := range []byte(err.Error()) {
		if b >= utf8RuneSelf {
			t.Fatalf("error contains non-ASCII byte 0x%02x: %v", b, err)
		}
	}
}

func TestParseRaceCaptureSelectsNamedFields(t *testing.T) {
	t.Parallel()
	cfg, err := parseRaceCapture(false, "1KiB", []string{
		`version=([0-9]+)`,
		`(?P<number>[0-9]+)(?<unit>ms|s)`,
	}, reportText)
	if err != nil {
		t.Fatal(err)
	}
	if fields := cfg.extractFields(); !slices.Equal(fields[0], []string{}) ||
		!slices.Equal(fields[1], []string{"number", "unit"}) {
		t.Errorf("fields = %q", fields)
	}
}

func TestParseCaptureBodyBytes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value   string
		want    int64
		wantErr bool
	}{
		{value: "", want: 0},
		{value: "0B", want: 0},
		{value: "17", want: 17},
		{value: "17B", want: 17},
		{value: "2KiB", want: 2 << 10},
		{value: "3mib", want: 3 << 20},
		{value: "unlimited", want: tth2.UnlimitedResponseBytes},
		{value: "UNLIMITED", want: tth2.UnlimitedResponseBytes},
		{value: "-1", wantErr: true},
		{value: "1KB", wantErr: true},
		{value: "9223372036854775807GiB", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			got, err := parseCaptureBodyBytes(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %d, want an error", got)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
		})
	}
}

func TestCaptureResponseLevelsAndExtracts(t *testing.T) {
	t.Parallel()
	body := []byte("pre[value]post")
	patterns, err := parseRaceCapture(
		true, "unlimited",
		[]string{`\[(?P<extract>.*?)\]`, `missing`},
		reportJSON,
	)
	if err != nil {
		t.Fatalf("parseRaceCapture: %v", err)
	}
	response := &tth2.Response{
		StatusCode:    http.StatusCreated,
		ContentLength: int64(len(body)),
		Header: http.Header{
			"X-Repeat": {"a", "b"},
		},
		Trailer: http.Header{
			"X-Final": {"done"},
		},
		Body: tth2.ResponseBody{
			Captured: body, BytesReceived: int64(len(body)),
			SHA256: sha256.Sum256(body),
		},
	}
	result := &tth2.Result{
		Response: response, ArrivalRank: 0,
	}
	rec, err := captureTrialRecord(&tth2.Trial{
		BatchResult: tth2.BatchResult{
			Results: []tth2.Result{*result}, ArrivalOrder: []int{0},
		},
		Arrangement: []int{0},
	}, 0, patterns)
	if err != nil {
		t.Fatalf("captureTrialRecord: %v", err)
	}
	got := rec.Outcomes[0].Response
	if got == nil {
		t.Fatal("response is nil")
	}
	if got.Headers["x-repeat"][1] != "b" ||
		got.Trailers["x-final"][0] != "done" {
		t.Errorf("headers = %v, trailers = %v",
			got.Headers, got.Trailers)
	}
	if got.Body == nil || got.Body.Encoding != "base64" ||
		got.Body.Data != base64.StdEncoding.EncodeToString(body) {
		t.Errorf("body = %+v", got.Body)
	}
	if len(got.Extracts) != 2 ||
		!got.BodyCapturedComplete ||
		!got.Extracts[0].Complete ||
		!got.Extracts[1].Complete ||
		len(got.Extracts[0].Matches) != 1 ||
		got.Extracts[0].Selection != extractNamedGroups ||
		!slices.Equal(got.Extracts[0].Fields, []string{"extract"}) ||
		got.Extracts[0].Matches[0].Offset != [2]int{3, 10} ||
		len(got.Extracts[0].Matches[0].Captures) != 1 ||
		got.Extracts[0].Matches[0].Captures[0].Value == nil ||
		got.Extracts[0].Matches[0].Captures[0].Value.Data != "value" ||
		len(got.Extracts[1].Matches) != 0 {
		t.Errorf("extracts = %+v", got.Extracts)
	}

	var text bytes.Buffer
	textReporter := newTextRaceReporter(&text)
	if err := textReporter.Run(RunRecord{
		Trials: 1, TrialsRequested: 1, TrialBatchWidth: 1, Connections: 1,
		CaptureBodyBytes: tth2.UnlimitedResponseBytes,
		ExtractRegex: []string{
			patterns.extracts[0].pattern,
			patterns.extracts[1].pattern,
		},
		ExtractFields: patterns.extractFields(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := textReporter.Request(RequestRecord{
		Reference: runReference{ID: 0, Label: "A", Inline: "A"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := textReporter.Trial(rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), `extract 0: "value"`) {
		t.Errorf("text report lacks captured match:\n%s", text.String())
	}

	var jsonOut bytes.Buffer
	if err := startRaceJSONCatalogue(t, &jsonOut, 1, rec).Trial(rec); err != nil {
		t.Fatal(err)
	}
	var jsonTrial jsonRaceTrialRecord
	if err := json.Unmarshal(lastJSONRecord(t, jsonOut.Bytes()), &jsonTrial); err != nil {
		t.Fatalf("decode JSON trial: %v", err)
	}
	jsonMatch := jsonTrial.Outcomes[0].Response.Extracts[0].Matches[0]
	if jsonMatch.Offset != [2]int{3, 10} || len(jsonMatch.Captures) != 1 ||
		jsonMatch.Captures[0].Data == nil ||
		string(*jsonMatch.Captures[0].Data) != "value" ||
		jsonMatch.Captures[0].Offset == nil ||
		*jsonMatch.Captures[0].Offset != [2]int{4, 9} {
		t.Errorf("JSON match = %+v", jsonMatch)
	}
}

func TestCaptureTrialRecordMarksPrefixOnlyExtraction(t *testing.T) {
	t.Parallel()
	patterns, err := parseRaceCapture(
		false, "4", []string{`(?P<route>GET /now)`}, reportJSON)
	if err != nil {
		t.Fatalf("parseRaceCapture: %v", err)
	}
	body := []byte("GET ")
	response := &tth2.Response{
		StatusCode: http.StatusOK,
		Body: tth2.ResponseBody{
			Captured: body, BytesReceived: 10,
			SHA256: sha256.Sum256([]byte("GET /now\n")),
		},
	}
	result := tth2.Result{Response: response, ArrivalRank: 0}
	rec, err := captureTrialRecord(&tth2.Trial{
		BatchResult: tth2.BatchResult{
			Results: []tth2.Result{result}, ArrivalOrder: []int{0},
		},
		Arrangement: []int{0},
	}, 0, patterns)
	if err != nil {
		t.Fatalf("captureTrialRecord: %v", err)
	}
	got := rec.Outcomes[0].Response
	if got == nil {
		t.Fatal("response is nil")
	}
	if got.BodyCapturedComplete || got.BodyCapturedBytes != len(body) ||
		len(got.Extracts) != 1 || got.Extracts[0].Complete ||
		len(got.Extracts[0].Matches) != 0 {
		t.Errorf("prefix-only response = %+v", got)
	}
	var output bytes.Buffer
	reporter := startRaceJSONCatalogue(t, &output, 1, rec)
	if err := reporter.Trial(rec); err != nil {
		t.Fatal(err)
	}
	var trial jsonRaceTrialRecord
	if err := json.Unmarshal(lastJSONRecord(t, output.Bytes()), &trial); err != nil {
		t.Fatal(err)
	}
	wire := trial.Outcomes[0]
	if wire.Response == nil || wire.Response.BodyCapturedComplete ||
		len(wire.Response.Extracts) != 1 || wire.Response.Extracts[0].Complete {
		t.Errorf("JSON prefix-only response = %+v", wire.Response)
	}
}

func TestReadReusableBodyFailuresAndCleanup(t *testing.T) {
	t.Parallel()
	readErr := errors.New("read failed")
	closeErr := errors.New("close failed")

	t.Run("non-reusable", func(t *testing.T) {
		req := &http.Request{
			Body: io.NopCloser(strings.NewReader("body")),
		}
		t.Cleanup(func() { _ = req.Body.Close() })
		_, _, err := readReusableBody(req)
		if err == nil || err.Error() != "body is not reusable" {
			t.Fatalf("error = %v, want body is not reusable", err)
		}
	})

	t.Run("GetBody", func(t *testing.T) {
		req := &http.Request{
			Body: io.NopCloser(strings.NewReader("body")),
			GetBody: func() (io.ReadCloser, error) {
				return nil, readErr
			},
		}
		t.Cleanup(func() { _ = req.Body.Close() })
		_, _, err := readReusableBody(req)
		if !errors.Is(err, readErr) || !strings.Contains(err.Error(), "GetBody") {
			t.Fatalf("error = %v, want wrapped GetBody error", err)
		}
	})

	t.Run("read error takes precedence and closes", func(t *testing.T) {
		rc := &trackedReadCloser{
			reader:   &dataErrorReader{data: []byte("partial"), err: readErr},
			closeErr: closeErr,
		}
		req := &http.Request{
			Body:    io.NopCloser(strings.NewReader("body")),
			GetBody: func() (io.ReadCloser, error) { return rc, nil },
		}
		t.Cleanup(func() { _ = req.Body.Close() })
		_, _, err := readReusableBody(req)
		if !errors.Is(err, readErr) || errors.Is(err, closeErr) {
			t.Fatalf("error = %v, want only wrapped read error", err)
		}
		if !rc.closed {
			t.Error("reusable body was not closed after read failure")
		}
	})

	t.Run("close error", func(t *testing.T) {
		rc := &trackedReadCloser{
			reader: strings.NewReader("body"), closeErr: closeErr,
		}
		req := &http.Request{
			Body:    io.NopCloser(strings.NewReader("body")),
			GetBody: func() (io.ReadCloser, error) { return rc, nil },
		}
		t.Cleanup(func() { _ = req.Body.Close() })
		_, _, err := readReusableBody(req)
		if !errors.Is(err, closeErr) || !strings.Contains(err.Error(), "close body") {
			t.Fatalf("error = %v, want wrapped close error", err)
		}
		if !rc.closed {
			t.Error("reusable body was not closed")
		}
	})
}

func TestCaptureRequestRecordsWrapsBodyError(t *testing.T) {
	t.Parallel()
	getBodyErr := errors.New("body unavailable")
	req := &http.Request{
		Body: io.NopCloser(strings.NewReader("body")),
		GetBody: func() (io.ReadCloser, error) {
			return nil, getBodyErr
		},
	}
	t.Cleanup(func() { _ = req.Body.Close() })
	_, err := captureRequestRecords(&prepared{
		materialised: &materialised{
			assembled: &assembled{measuredCount: 1},
			reqs:      []*http.Request{req},
		},
	}, runReferences{refs: []runReference{{ID: 0}}}, requestSetTrial)
	if !errors.Is(err, getBodyErr) ||
		!strings.Contains(err.Error(), "capture request 0: GetBody") {
		t.Fatalf("error = %v, want request context around GetBody error", err)
	}
}

func TestCaptureResponseMetadataAndHeaders(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		cfg         raceCaptureConfig
		wantHeaders bool
		wantBody    bool
	}{
		{name: "metadata"},
		{
			name: "headers", cfg: raceCaptureConfig{headers: true},
			wantHeaders: true,
		},
		{
			name: "body", cfg: raceCaptureConfig{bodyBytes: 1},
			wantBody: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := &tth2.Response{
				StatusCode:    http.StatusNoContent,
				ContentLength: 0,
				Header:        http.Header{"X-Test": {"yes"}},
				Body:          tth2.ResponseBody{SHA256: sha256.Sum256(nil)},
			}
			rec, err := captureTrialRecord(&tth2.Trial{
				BatchResult: tth2.BatchResult{
					Results: []tth2.Result{{
						Response: response, ArrivalRank: 0,
					}},
					ArrivalOrder: []int{0},
				},
				Arrangement: []int{0},
			}, 0, tc.cfg)
			if err != nil {
				t.Fatalf("captureTrialRecord: %v", err)
			}
			got := rec.Outcomes[0].Response
			if (got.Headers != nil) != tc.wantHeaders ||
				(got.Trailers != nil) != tc.wantHeaders {
				t.Errorf("headers = %v, trailers = %v",
					got.Headers, got.Trailers)
			}
			if (got.Body != nil) != tc.wantBody {
				t.Errorf("body = %+v", got.Body)
			}
			if tc.wantBody &&
				(got.Body.Encoding != "base64" || got.Body.Data != "") {
				t.Errorf("empty body = %+v", got.Body)
			}
			if got.BodyBytes != 0 || got.SHA256 != digestBase64(nil) {
				t.Errorf("metadata = %+v", got)
			}
		})
	}
}

func TestCaptureResponseContentLengthShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		header   []string
		declared int64
		body     string
		want     *int64
	}{
		{"missing", nil, -1, "body", nil},
		{"malformed", []string{"nope"}, -1, "body", nil},
		{"conflicting", []string{"4", "5"}, -1, "body", nil},
		{"truthful", []string{"4"}, 4, "body", new(int64(4))},
		{"mismatched", []string{"9"}, 9, "body", new(int64(9))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			header := make(http.Header)
			for _, value := range tc.header {
				header.Add("Content-Length", value)
			}
			result := &tth2.Result{
				Response: &tth2.Response{
					StatusCode:    http.StatusOK,
					ContentLength: tc.declared,
					Header:        header,
					Body: tth2.ResponseBody{
						Captured:      []byte(tc.body),
						BytesReceived: int64(len(tc.body)),
						SHA256:        sha256.Sum256([]byte(tc.body)),
					},
				},
			}
			rec, err := captureTrialRecord(&tth2.Trial{
				BatchResult: tth2.BatchResult{
					Results: []tth2.Result{*result},
				},
				Arrangement: []int{0},
			}, 0, raceCaptureConfig{headers: true})
			if err != nil {
				t.Fatalf("captureTrialRecord: %v", err)
			}
			response := rec.Outcomes[0].Response
			switch {
			case tc.want == nil && response.ContentLength != nil:
				t.Errorf("content_length = %v, want nil",
					*response.ContentLength)
			case tc.want != nil && (response.ContentLength == nil ||
				*response.ContentLength != *tc.want):
				t.Errorf("content_length = %v, want %d",
					response.ContentLength, *tc.want)
			}
			if got := response.Headers["content-length"]; !slices.Equal(got, tc.header) {
				t.Errorf("raw Content-Length = %q, want %q",
					got, tc.header)
			}
			if response.BodyBytes != int64(len(tc.body)) {
				t.Errorf("body_bytes = %d, want %d",
					response.BodyBytes, len(tc.body))
			}
		})
	}
}

func TestCaptureExtractGroupParticipation(t *testing.T) {
	t.Parallel()
	cfg, err := parseRaceCapture(
		false, "1KiB",
		[]string{`skip|x=(?P<extract>[^;]*)`},
		reportJSON,
	)
	if err != nil {
		t.Fatalf("parseRaceCapture: %v", err)
	}
	got := captureExtracts([]byte("skip x=yes;x="), cfg.extracts, true)
	if len(got[0].Matches) != 3 {
		t.Fatalf("matches = %+v, want three regex occurrences",
			got[0].Matches)
	}
	first := got[0].Matches[0].Captures[0]
	second := got[0].Matches[1].Captures[0]
	third := got[0].Matches[2].Captures[0]
	if first.Participated || first.Offset != nil || first.Value != nil ||
		!second.Participated || second.Offset == nil ||
		*second.Offset != [2]int{7, 10} || second.Value == nil ||
		second.Value.Data != "yes" || !third.Participated ||
		third.Offset == nil || *third.Offset != [2]int{13, 13} ||
		third.Value == nil || third.Value.Data != "" {
		t.Errorf("matches = %+v", got[0].Matches)
	}
}

func TestCaptureExtractSelectionModes(t *testing.T) {
	t.Parallel()
	cfg, err := parseRaceCapture(false, "1KiB", []string{
		`v=([0-9]+)`,
		`(?P<number>[0-9]+)(?P<unit>ms)?`,
	}, reportJSON)
	if err != nil {
		t.Fatal(err)
	}
	got := captureExtracts([]byte("v=12ms 7"), cfg.extracts, true)
	whole := got[0]
	if whole.Selection != extractWholeMatch || len(whole.Fields) != 0 ||
		len(whole.Matches) != 1 || whole.Matches[0].Value == nil ||
		whole.Matches[0].Value.Data != "v=12" ||
		len(whole.Matches[0].Captures) != 0 {
		t.Errorf("whole-match extraction = %+v", whole)
	}
	named := got[1]
	if named.Selection != extractNamedGroups ||
		!slices.Equal(named.Fields, []string{"number", "unit"}) ||
		len(named.Matches) != 2 {
		t.Fatalf("named-group extraction = %+v", named)
	}
	first, second := named.Matches[0], named.Matches[1]
	if first.Offset != [2]int{2, 6} || first.Value != nil ||
		len(first.Captures) != 2 || first.Captures[0].Value == nil ||
		first.Captures[0].Value.Data != "12" ||
		first.Captures[1].Value == nil || first.Captures[1].Value.Data != "ms" {
		t.Errorf("first named match = %+v", first)
	}
	if second.Offset != [2]int{7, 8} || second.Value != nil ||
		len(second.Captures) != 2 || second.Captures[0].Value == nil ||
		second.Captures[0].Value.Data != "7" ||
		second.Captures[1].Participated || second.Captures[1].Offset != nil ||
		second.Captures[1].Value != nil {
		t.Errorf("second named match = %+v", second)
	}
}
