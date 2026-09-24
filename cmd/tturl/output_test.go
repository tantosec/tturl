package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/timing"
)

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

type closeErrorBuffer struct {
	bytes.Buffer
	err error
}

func (w *closeErrorBuffer) Close() error { return w.err }

type failingFile struct {
	err    error
	closed bool
}

type writeCloseErrorFile struct {
	writeErr error
	closeErr error
}

func (f *writeCloseErrorFile) Write([]byte) (int, error) {
	return 0, f.writeErr
}
func (f *writeCloseErrorFile) Close() error { return f.closeErr }

type recordLimitWriter struct {
	bytes.Buffer
	records int
	limit   int
}

type markerErrorWriter struct {
	bytes.Buffer
	marker string
	failed bool
}

func assertTextReportHierarchy(
	t *testing.T,
	out string,
	title string,
	sections ...string,
) {
	t.Helper()
	equals := strings.Repeat("=", textWidth)
	if got := strings.Count(out, equals+"\n"); got != 2 {
		t.Errorf("report has %d root rules, want 2:\n%s", got, out)
	}
	if !strings.Contains(out, centre(title, textWidth)+"\n") {
		t.Errorf("report lacks root title %q:\n%s", title, out)
	}
	last := -1
	for _, section := range sections {
		line := heading(section)
		at := strings.Index(out, line)
		if at < 0 {
			t.Errorf("report lacks %q section:\n%s", section, out)
			continue
		}
		if at <= last {
			t.Errorf("section %q is out of order:\n%s", section, out)
		}
		last = at
	}
	if rule := strings.Repeat("-", textWidth); strings.Contains(out, rule) {
		t.Errorf("report contains a decorative dashed fence:\n%s", out)
	}
	requests := strings.Index(out, heading("Requests"))
	experiment := strings.Index(out, heading("Experiment"))
	if requests >= 0 && experiment > requests {
		requestSection := out[requests:experiment]
		for _, prefix := range []string{
			"\nPacing: ", "\nConnections: ", "\nStreams: ",
			"\nBody release: ", "\nTLS: ", "\nArrangement: ",
		} {
			if strings.Contains(requestSection, prefix) {
				t.Errorf("Requests contains experiment fact %q:\n%s", prefix, out)
			}
		}
	}
}

// assertTextLinesAtMost is for controlled fixtures whose inputs contain no
// intrinsically wide user data or experimental evidence. Do not use it as a
// universal assertion over arbitrary reports.
func assertTextLinesAtMost(t *testing.T, out string, width int) {
	t.Helper()
	for index, line := range strings.Split(out, "\n") {
		if len(line) > width {
			t.Errorf("line %d has %d columns, want <= %d: %q",
				index+1, len(line), width, line)
		}
	}
}

func (w *markerErrorWriter) Write(p []byte) (int, error) {
	if w.failed {
		return 0, io.ErrClosedPipe
	}
	n, err := w.Buffer.Write(p)
	if err == nil && bytes.Contains(w.Bytes(), []byte(w.marker)) {
		w.failed = true
		return n, io.ErrClosedPipe
	}
	return n, err
}

func (w *recordLimitWriter) Write(p []byte) (int, error) {
	if w.records >= w.limit {
		return 0, io.ErrClosedPipe
	}
	written := 0
	for written < len(p) {
		if w.records >= w.limit {
			return written, io.ErrClosedPipe
		}
		recordEnd := bytes.IndexByte(p[written:], '\n')
		if recordEnd < 0 {
			n, err := w.Buffer.Write(p[written:])
			return written + n, err
		}
		recordEnd += written + 1
		n, err := w.Buffer.Write(p[written:recordEnd])
		written += n
		if err != nil {
			return written, err
		}
		w.records++
	}
	return written, nil
}

func (f *failingFile) Write([]byte) (int, error) { return 0, f.err }
func (f *failingFile) Close() error {
	f.closed = true
	return nil
}

func TestReportWriterKeepsFirstFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("write failed")
	w := newReportWriter(failingWriter{err: want})

	emit(w, "first")
	emit(w, "second")

	if !errors.Is(w.Err(), want) {
		t.Errorf("Err() = %v, want %v", w.Err(), want)
	}
}

func TestOutputPathErrorsRenderUnsafeInputAsASCII(t *testing.T) {
	want := errors.New("open failed")
	_, err := openReportDestination(commandOutput{
		openOutput: func(string) (io.WriteCloser, error) {
			return nil, want
		},
	}, "caf\xc3\xa9\x1b[31m.json")
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("openReportDestination error = %v, want %v", err, want)
	}
	for _, b := range []byte(err.Error()) {
		if b >= utf8RuneSelf || b == '\x1b' {
			t.Fatalf("error contains unsafe byte 0x%02x: %v", b, err)
		}
	}
	if !strings.Contains(err.Error(), "$HEX[") {
		t.Errorf("error = %v, want escaped path", err)
	}
}

func TestCommandStopsAfterSummaryWriteFailure(t *testing.T) {
	t.Parallel()
	want := errors.New("write failed")
	var stderr bytes.Buffer
	err := runRace(context.Background(), []string{
		"--block", "https://127.0.0.1:1/a",
	}, commandOutput{
		stdout: failingWriter{err: want},
		stderr: &stderr,
	})
	if !errors.Is(err, want) {
		t.Errorf("runRace error = %v, want %v", err, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}

func TestAlreadyClosedStdoutPipeIsSuccess(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(context.Context, []string, commandOutput) error
		args []string
	}{
		{name: "race text", run: runRace, args: []string{
			"--report", "text",
			"--block", "https://127.0.0.1:1/a",
		}},
		{name: "race JSON", run: runRace, args: []string{
			"--report", "json",
			"--block", "https://127.0.0.1:1/a",
		}},
		{name: "measure", run: runMeasure, args: []string{
			"--trials", "1",
			"--arrange", "none",
			"--block", "https://127.0.0.1:1/a",
			"--block", "https://127.0.0.1:1/b",
		}},
		{name: "detect", run: runDetect, args: []string{
			"--direction", "late",
			"--comparisons-max", "1",
			"--width", "2",
			"--block", "https://127.0.0.1:1/a",
			"--block", "https://127.0.0.1:1/b",
		}},
		{name: "detect JSON", run: runDetect, args: []string{
			"--report", "json",
			"--direction", "late",
			"--comparisons-max", "1",
			"--width", "2",
			"--block", "https://127.0.0.1:1/a",
			"--block", "https://127.0.0.1:1/b",
		}},
		{name: "analyse", run: runAnalyse, args: []string{
			"--cycles", "6",
			"--block", "https://127.0.0.1:1/a",
			"--block", "https://127.0.0.1:1/b",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.run(t.Context(), test.args, commandOutput{
				stdout:     failingWriter{err: io.ErrClosedPipe},
				stderr:     io.Discard,
				stdoutPipe: true,
			})
			if err != nil {
				t.Errorf("error = %v, want nil for a closed stdout pipe", err)
			}
		})
	}
}

func TestPipeDoesNotHideOtherWriteFailures(t *testing.T) {
	t.Parallel()
	want := errors.New("write failed")
	tests := []struct {
		name string
		run  func(context.Context, []string, commandOutput) error
		args []string
	}{
		{name: "race", run: runRace, args: []string{
			"--block", "https://127.0.0.1:1/a",
		}},
		{name: "measure", run: runMeasure, args: []string{
			"--block", "https://127.0.0.1:1/a",
			"--block", "https://127.0.0.1:1/b",
		}},
		{name: "detect", run: runDetect, args: []string{
			"--direction", "late",
			"--block", "https://127.0.0.1:1/a",
			"--block", "https://127.0.0.1:1/b",
		}},
		{name: "analyse", run: runAnalyse, args: []string{
			"--cycles", "6",
			"--block", "https://127.0.0.1:1/a",
			"--block", "https://127.0.0.1:1/b",
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.run(t.Context(), test.args, commandOutput{
				stdout:     failingWriter{err: want},
				stderr:     io.Discard,
				stdoutPipe: true,
			})
			if !errors.Is(err, want) {
				t.Errorf("error = %v, want %v", err, want)
			}
		})
	}
}

func TestMeasureBrokenStdoutPipeIsSuccess(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	writer := &markerErrorWriter{marker: "Rank profile"}
	err := runMeasure(t.Context(), []string{
		"--trials", "1",
		"--arrange", "none",
		"--insecure",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, commandOutput{
		stdout:     writer,
		stderr:     io.Discard,
		stdoutPipe: true,
	})
	if err != nil {
		t.Fatalf("runMeasure error = %v, want nil for a closed stdout pipe", err)
	}
}

func TestFriendlyReportsShareHierarchy(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	blocks := []string{
		"--insecure",
		"--block", "https://" + addr + "/a", "--name", "A",
		"--block", "https://" + addr + "/b", "--name", "B",
	}

	t.Run("race", func(t *testing.T) {
		out, stdout, _ := captureOutput(t)
		args := append([]string{"--trials", "1", "--verbose"}, blocks...)
		if err := runRace(t.Context(), args, out); err != nil {
			t.Fatalf("runRace: %v", err)
		}
		report := stdout.String()
		assertTextLinesAtMost(t, report, textWidth)
		assertTextReportHierarchy(t, report, "tturl race",
			"Requests", "Experiment", "Results", "Execution")
		assertFriendlyDelivery(t, report)
		for _, want := range []string{
			"Trials: 1 x 2 requests = 2 request operations.",
			"Arrangement: none.",
			"TLS: certificate verification disabled (--insecure).",
		} {
			if !strings.Contains(report, want) {
				t.Errorf("race report lacks %q:\n%s", want, report)
			}
		}
		if strings.Contains(report, "\nWarmup:") ||
			strings.Contains(report, "\nPadding:") ||
			strings.Contains(report, "\nBody release:") {
			t.Errorf("default race report includes inactive options:\n%s", report)
		}
		for _, label := range []string{"Request A", "Request B"} {
			if !strings.Contains(report, "\n"+subordinateHeading(label)+"\n:method:") {
				t.Errorf("verbose report lacks ruled %q heading:\n%s", label, report)
			}
		}
	})

	t.Run("measure rotate", func(t *testing.T) {
		out, stdout, _ := captureOutput(t)
		args := append([]string{"--trials", "2"}, blocks...)
		if err := runMeasure(t.Context(), args, out); err != nil {
			t.Fatalf("runMeasure: %v", err)
		}
		report := stdout.String()
		assertTextLinesAtMost(t, report, textWidth)
		assertTextReportHierarchy(t, report, "tturl measure",
			"Requests", "Experiment", "Measurement", "HTTP status outcomes",
			"Position check", "Execution")
		assertFriendlyDelivery(t, report)
		if !strings.Contains(report,
			"TLS: certificate verification disabled (--insecure).") {
			t.Errorf("measure report omits active TLS policy:\n%s", report)
		}
	})

	t.Run("measure none", func(t *testing.T) {
		out, stdout, _ := captureOutput(t)
		args := append([]string{"--trials", "1", "--arrange", "none"},
			blocks...)
		if err := runMeasure(t.Context(), args, out); err != nil {
			t.Fatalf("runMeasure: %v", err)
		}
		report := stdout.String()
		assertTextLinesAtMost(t, report, textWidth)
		assertTextReportHierarchy(t, report, "tturl measure",
			"Requests", "Experiment", "Measurement", "HTTP status outcomes",
			"Position check", "Execution")
		assertFriendlyDelivery(t, report)
	})

	t.Run("detect", func(t *testing.T) {
		out, stdout, _ := captureOutput(t)
		args := append([]string{
			"--direction", "late", "--comparisons-max", "1", "--width", "2",
		}, blocks...)
		if err := runDetect(t.Context(), args, out); err != nil {
			t.Fatalf("runDetect: %v", err)
		}
		report := stdout.String()
		assertTextLinesAtMost(t, report, textWidth)
		assertTextReportHierarchy(t, report, "tturl detect",
			"Requests", "Experiment", "Detection result",
			"HTTP status outcomes", "Execution")
		assertFriendlyDelivery(t, report)
		if !strings.Contains(strings.Join(strings.Fields(report), " "),
			"Search: late arrival-order outlier among 2 candidates; effective width 2;") {
			t.Errorf("detect report omits its effective width:\n%s", report)
		}
	})

	t.Run("analyse", func(t *testing.T) {
		out, stdout, _ := captureOutput(t)
		args := append([]string{"--cycles", "6"}, blocks...)
		if err := runAnalyse(t.Context(), args, out); err != nil {
			t.Fatalf("runAnalyse: %v", err)
		}
		report := stdout.String()
		assertTextLinesAtMost(t, report, textWidth)
		assertTextReportHierarchy(t, report, "tturl analyse",
			"Requests", "Experiment", "Analysis result", "HTTP status outcomes",
			"Position check", "Execution", "Method")
		assertFriendlyDelivery(t, report)
	})
}

func assertFriendlyDelivery(t *testing.T, report string) {
	t.Helper()
	if strings.Contains(report, "Body release:") {
		t.Errorf("bodyless report includes an inapplicable release plan:\n%s", report)
	}
	report = strings.Join(strings.Fields(report), " ")
	for _, want := range []string{
		"batch-rate ceiling unlimited;",
		"request-rate ceiling unlimited;",
		"minimum batch-start interval unconstrained.",
		"Connections: up to 1 active (default).",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("friendly report lacks %q:\n%s", want, report)
		}
	}
}

func TestFriendlyReportsDescribeBodyRelease(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	blocks := []string{
		"--insecure", "--data", "value=x",
		"--block", "https://" + addr + "/a", "--name", "A",
		"--block", "https://" + addr + "/b", "--name", "B",
	}
	tests := []struct {
		name string
		run  func(context.Context, []string, commandOutput) error
		args []string
	}{
		{"race", runRace, []string{"--trials", "1"}},
		{"measure", runMeasure, []string{"--trials", "2"}},
		{"detect", runDetect, []string{
			"--direction", "late", "--comparisons-max", "1", "--width", "2",
		}},
		{"analyse", runAnalyse, []string{"--cycles", "6"}},
	}
	const want = "Body release: withhold up to 1 trailing byte per body; " +
		"no release delay."
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out, stdout, _ := captureOutput(t)
			args := append(append([]string(nil), test.args...), blocks...)
			if err := test.run(t.Context(), args, out); err != nil {
				t.Fatalf("run: %v", err)
			}
			if report := strings.Join(strings.Fields(stdout.String()), " "); !strings.Contains(report, want) {
				t.Errorf("report lacks %q:\n%s", want, stdout.String())
			}
		})
	}
}

func TestRaceReportsIneffectiveExplicitBodyRelease(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	base := []string{
		"--trials", "1", "--insecure",
		"--block", "https://" + addr + "/a", "--name", "A",
		"--block", "https://" + addr + "/b", "--name", "B",
	}

	t.Run("bodyless explicit controls", func(t *testing.T) {
		out, stdout, _ := captureOutput(t)
		args := append([]string{"--release-delay", "5ms"}, base...)
		if err := runRace(t.Context(), args, out); err != nil {
			t.Fatalf("runRace: %v", err)
		}
		if !strings.Contains(stdout.String(),
			"Body release: not applicable; requests contain no body bytes.") {
			t.Errorf("report omits inapplicable explicit controls:\n%s", stdout.String())
		}
	})

	t.Run("zero withholding elides delay", func(t *testing.T) {
		out, stdout, _ := captureOutput(t)
		args := append([]string{
			"--release-delay", "5ms", "--body-bytes-withheld", "0",
			"--data", "value=x",
		}, base...)
		if err := runRace(t.Context(), args, out); err != nil {
			t.Fatalf("runRace: %v", err)
		}
		report := stdout.String()
		if !strings.Contains(report,
			"Body release: no fixed tail or delay; non-empty bodies share a final record.") ||
			strings.Contains(report, "delay release by 5ms") {
			t.Errorf("zero-withholding report presents an effective delay:\n%s", report)
		}
	})

	t.Run("JSON retains configured controls", func(t *testing.T) {
		out, stdout, _ := captureOutput(t)
		args := append([]string{
			"--report", "json", "--release-delay", "500us",
			"--body-bytes-withheld", "3",
		}, base...)
		if err := runRace(t.Context(), args, out); err != nil {
			t.Fatalf("runRace: %v", err)
		}
		lines := jsonLines(t, stdout.Bytes())
		var run jsonRaceRunRecord
		if err := json.Unmarshal(lines[0], &run); err != nil {
			t.Fatal(err)
		}
		if run.Acquisition.ReleaseDelayNS != int64(500*time.Microsecond) ||
			run.Acquisition.BodyBytesWithheld != 3 {
			t.Errorf("acquisition release controls = %dns/%d bytes, want 500us/3 bytes",
				run.Acquisition.ReleaseDelayNS,
				run.Acquisition.BodyBytesWithheld)
		}
	})
}

func TestDetectFinalReportPipeClosureIsSuccess(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	writer := &markerErrorWriter{marker: "Detection result"}
	err := runDetect(t.Context(), []string{
		"--insecure",
		"--direction", "late",
		"--comparisons-max", "1",
		"--width", "2",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, commandOutput{
		stdout:     writer,
		stderr:     io.Discard,
		stdoutPipe: true,
	})
	if err != nil {
		t.Fatalf("runDetect error = %v, want nil for a closed stdout pipe", err)
	}
}

func TestRaceJSONStderrFailureIsNotAStdoutPipeClosure(t *testing.T) {
	t.Parallel()
	want := errors.New("stderr failed")
	err := runRace(context.Background(), []string{
		"--report", "json",
		"--block", "https://127.0.0.1:1/a",
	}, commandOutput{
		stdout:     io.Discard,
		stderr:     failingWriter{err: want},
		stdoutPipe: true,
	})
	if !errors.Is(err, want) {
		t.Errorf("runRace error = %v, want stderr failure %v", err, want)
	}
}

func TestRaceOutputFileMatchesStdout(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	args := []string{
		"--insecure",
		"--block", "https://" + addr + "/probe",
	}

	stdoutRun, want, _ := captureOutput(t)
	if err := runRace(t.Context(), args, stdoutRun); err != nil {
		t.Fatalf("stdout run: %v", err)
	}

	path := filepath.Join(t.TempDir(), "race.txt")
	if err := os.WriteFile(path, []byte("old contents"), 0o600); err != nil {
		t.Fatalf("seed output: %v", err)
	}
	fileRun, stdout, _ := captureOutput(t)
	fileArgs := append([]string{"--output", path}, args...)
	if err := runRace(t.Context(), fileArgs, fileRun); err != nil {
		t.Fatalf("file run: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty with --output", stdout.String())
	}
	//nolint:gosec // The path is inside t.TempDir.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Errorf("file report differs from stdout report\n--- file ---\n%s--- stdout ---\n%s",
			got, want.Bytes())
	}
}

func TestRaceJSONOutputFileContainsOnlyJSONL(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "race.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, stdout, stderr := captureOutput(t)
	err := runRace(ctx, []string{
		"--report", "json",
		"--trials", "unlimited",
		"--output", path,
		"--block", "https://127.0.0.1:1/probe",
	}, out)
	if err != nil {
		t.Fatalf("runRace: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
	//nolint:gosec // The path is inside t.TempDir.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	lines := jsonLines(t, data)
	schema := compileStructuredSchema(t, raceSchema.filename)
	for index, line := range lines {
		var record any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode record %d: %v", index, err)
		}
		if err := schema.Validate(record); err != nil {
			t.Fatalf("record %d does not conform: %v", index, err)
		}
	}
	if len(lines) != 3 ||
		recordKind(t, lines[0]) != "run" ||
		recordKind(t, lines[1]) != "request" ||
		recordKind(t, lines[2]) != "result" {
		t.Errorf("JSONL prefix has %d records: %s", len(lines), data)
	}
	var run struct {
		Design struct {
			TrialsRequested *int `json:"trials_requested"`
			TrialsLimit     *int `json:"trials_limit"`
		} `json:"design"`
	}
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Design.TrialsRequested != nil || run.Design.TrialsLimit != nil {
		t.Errorf("unbounded trial counts = %+v, want null", run.Design)
	}
	if !strings.Contains(stderr.String(), "Target:") {
		t.Errorf("stderr lacks friendly request echo:\n%s", stderr.String())
	}
}

func TestRaceJSONBrokenPipeLeavesCompleteRecords(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	writer := &recordLimitWriter{limit: 2}
	err := runRace(t.Context(), []string{
		"--report", "json",
		"--insecure",
		"https://" + addr + "/probe",
	}, commandOutput{
		stdout:     writer,
		stderr:     io.Discard,
		stdoutPipe: true,
	})
	if err != nil {
		t.Fatalf("runRace: %v", err)
	}
	lines := jsonLines(t, writer.Bytes())
	if len(lines) != 2 ||
		recordKind(t, lines[0]) != "run" ||
		recordKind(t, lines[1]) != "request" {
		t.Errorf("JSONL prefix = %s", writer.Bytes())
	}
}

func TestAnalyseJSONClosedStdoutLeavesCompletePrefix(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	writer := &recordLimitWriter{limit: 3}
	err := runAnalyse(t.Context(), []string{
		"--report", "json", "--insecure", "--cycles", "6",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, commandOutput{
		stdout: writer, stderr: io.Discard, stdoutPipe: true,
	})
	if err != nil {
		t.Fatalf("runAnalyse: %v", err)
	}
	lines := jsonLines(t, writer.Bytes())
	if len(lines) != 3 ||
		recordKind(t, lines[0]) != "run" ||
		recordKind(t, lines[1]) != "request" ||
		recordKind(t, lines[2]) != "request" {
		t.Errorf("analyse JSONL prefix = %s", writer.Bytes())
	}
}

func TestRaceJSONAcquisitionFailureIsAValidStream(t *testing.T) {
	t.Parallel()
	var handled atomic.Int32
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		handled.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, _ := captureOutput(t)
	err := runRace(t.Context(), []string{
		"--report", "json",
		"https://" + addr + "/probe",
	}, out)
	if err == nil {
		t.Fatal("runRace returned nil error for a failed TLS handshake")
	}
	lines := jsonLines(t, stdout.Bytes())
	if len(lines) != 3 {
		t.Fatalf("JSONL has %d records: %s", len(lines), stdout.Bytes())
	}
	if got := []string{
		recordKind(t, lines[0]),
		recordKind(t, lines[1]),
		recordKind(t, lines[2]),
	}; !slices.Equal(got, []string{
		"run", "request", "result",
	}) {
		t.Errorf("record kinds = %q", got)
	}
	var summary jsonRaceResultRecord
	if err := json.Unmarshal(lines[2], &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Completion.State != completionFailed ||
		summary.Execution.Trials.Incomplete != 0 ||
		summary.Execution.Trials.RankComplete != 0 {
		t.Errorf("summary = %+v", summary)
	}
	if got := handled.Load(); got != 0 {
		t.Errorf("handler received %d requests despite TLS verification failure", got)
	}
}

func TestRaceOutputOpenFailurePrecedesNetwork(t *testing.T) {
	t.Parallel()
	want := errors.New("open failed")
	opened := 0
	err := runRace(context.Background(), []string{
		"--output", "capture.txt",
		"--block", "https://127.0.0.1:1/probe",
	}, commandOutput{
		stdout: io.Discard,
		stderr: io.Discard,
		openOutput: func(string) (io.WriteCloser, error) {
			opened++
			return nil, want
		},
	})
	if !errors.Is(err, want) {
		t.Errorf("runRace error = %v, want %v", err, want)
	}
	if opened != 1 {
		t.Errorf("output opened %d times, want once", opened)
	}
}

func TestEveryModeWritesTheSelectedOutput(t *testing.T) {
	t.Parallel()
	want := errors.New("write failed")
	args := []string{
		"-o", "capture.txt",
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}
	for i := range commandCatalogue {
		spec := &commandCatalogue[i]
		if !spec.requestGrammar {
			continue
		}
		t.Run(spec.name, func(t *testing.T) {
			var stdout bytes.Buffer
			file := &failingFile{err: want}
			commandArgs := args
			if spec.id == commandDetect {
				commandArgs = append([]string{"--direction", "late"}, args...)
			}
			out := commandOutput{
				stdout: &stdout,
				stderr: io.Discard,
				openOutput: func(string) (io.WriteCloser, error) {
					return file, nil
				},
			}
			var err error
			if spec.id == commandTime {
				// Output handling is independent of the host platform. Select a
				// supported platform and require failure before engine execution.
				err = runTimeCommandForPlatform(t.Context(), spec, commandArgs, out, "linux",
					func(context.Context, *timing.Resolved, func(timing.Event) error) (timing.Result, error) {
						t.Fatal("failed report reached engine")
						return timing.Result{}, nil
					})
			} else {
				err = spec.run(t.Context(), spec, commandCatalogue, commandArgs, out)
			}
			if !errors.Is(err, want) {
				t.Errorf("run error = %v, want selected-output failure %v",
					err, want)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if !file.closed {
				t.Error("selected output was not closed")
			}
		})
	}
}

func TestTextCommandsWriteCompleteSelectedOutput(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	blocks := []string{
		"--insecure",
		"--block", "https://" + addr + "/a", "--name", "A",
		"--block", "https://" + addr + "/b", "--name", "B",
	}
	tests := []struct {
		name    string
		run     func(context.Context, []string, commandOutput) error
		args    []string
		section string
	}{
		{"race", runRace, []string{"--trials", "1"}, "Results"},
		{"measure", runMeasure, []string{"--cycles", "1"}, "Measurement"},
		{"analyse", runAnalyse, []string{"--cycles", "6"}, "Analysis result"},
		{"detect", runDetect, []string{
			"--direction", "late", "--comparisons-max", "1", "--width", "2",
		}, "Detection result"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), test.name+".txt")
			out, stdout, _ := captureOutput(t)
			args := append([]string{"--output", path}, test.args...)
			args = append(args, blocks...)
			if err := test.run(t.Context(), args, out); err != nil {
				t.Fatalf("run: %v", err)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			//nolint:gosec // The path is inside t.TempDir.
			report, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read report: %v", err)
			}
			for _, want := range []string{
				centre(toolName+" "+test.name, textWidth),
				heading("Requests"), heading("Experiment"),
				heading(test.section), heading("Execution"),
			} {
				if !bytes.Contains(report, []byte(want)) {
					t.Errorf("selected report omits %q:\n%s", want, report)
				}
			}
		})
	}
}

func TestRaceOutputCloseFailureIsReported(t *testing.T) {
	t.Parallel()
	want := errors.New("close failed")
	file := &closeErrorBuffer{err: want}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runRace(ctx, []string{
		"--trials", "unlimited",
		"--output", "capture.txt",
		"--block", "https://127.0.0.1:1/probe",
	}, commandOutput{
		stdout: io.Discard,
		stderr: io.Discard,
		openOutput: func(string) (io.WriteCloser, error) {
			return file, nil
		},
	})
	if !errors.Is(err, want) {
		t.Errorf("runRace error = %v, want %v", err, want)
	}
	if !strings.Contains(file.String(), "Trials:") {
		t.Errorf("report was not written before close: %q", file.String())
	}
}

func TestRaceOutputPreservesWriteAndCloseFailures(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write failed")
	closeErr := errors.New("close failed")
	file := &writeCloseErrorFile{
		writeErr: writeErr,
		closeErr: closeErr,
	}
	err := runRace(context.Background(), []string{
		"--output", "capture.txt",
		"--block", "https://127.0.0.1:1/probe",
	}, commandOutput{
		stdout: io.Discard,
		stderr: io.Discard,
		openOutput: func(string) (io.WriteCloser, error) {
			return file, nil
		},
	})
	if !errors.Is(err, writeErr) || !errors.Is(err, closeErr) {
		t.Errorf("runRace error = %v, want joined write and close errors",
			err)
	}
}
