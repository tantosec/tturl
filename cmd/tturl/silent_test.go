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
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
)

func TestSilentCommandGrammar(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		args  []string
		code  int
		quiet bool
	}{
		{[]string{"race", "-s", "--silent", "--dry-run", "https://example.test", "--repeat", "2"}, 0, true},
		{[]string{"race", "https://example.test", "--silent", "--dry-run", "--repeat", "2"}, 0, true},
		{[]string{"time", "-s", "--dry-run", "https://example.test"}, 0, true},
		{[]string{"time", "--silent", "--dry-run", "https://example.test"}, 0, true},
		{[]string{"time", "--silent=false", "https://example.test"}, 2, false},
		{[]string{"time", "-s", "--silent=true"}, 2, true},
		{[]string{"time", "-vs=false", "https://example.test"}, 2, false},
		{[]string{"race", "-s"}, 2, true},
		{[]string{"measure", "--silent"}, 2, true},
		{[]string{"analyse", "-s", "--unknown"}, 2, true},
		{[]string{"detect", "--silent", "--direction", "late"}, 2, true},
		{[]string{"race", "--reset-body", "--silent"}, 2, true},
		{[]string{"race", "--block", "https://example.test", "-s"}, 2, false},
		{[]string{"race", "--silent", "--block", "https://example.test", "-s"}, 2, true},
		{[]string{"race", "--silent=false", "https://example.test"}, 2, false},
		{[]string{"race", "-s", "--silent=true"}, 2, true},
		{[]string{"-s", "race"}, 2, false},
		{[]string{"--silent", "race"}, 2, false},
		{[]string{"missing", "--silent"}, 2, false},
		{[]string{"help", "-s", "--silent", "output"}, 0, true},
		{[]string{"help", "--silent", "--help"}, 0, true},
		{[]string{"help", "output", "-s"}, 2, false},
		{[]string{"help", "-s", "output", "--silent"}, 2, true},
		{[]string{"help", "--silent", "unknown"}, 2, true},
		{[]string{"help", "-s", "output", "race"}, 2, true},
		{[]string{"completion", "--silent", "-s", "bash"}, 0, true},
		{[]string{"completion", "-s", "--help"}, 0, true},
		{[]string{"completion", "bash", "-s"}, 2, false},
		{[]string{"completion", "-s", "bash", "--silent"}, 2, true},
		{[]string{"completion", "--silent", "unknown"}, 2, true},
		{[]string{"completion", "-s"}, 2, true},
		{[]string{"version", "-s", "--silent"}, 0, true},
		{[]string{"version", "--silent", "extra"}, 2, true},
		{[]string{"version", "extra", "-s"}, 2, false},
		{[]string{"schema", "--silent"}, 2, true},
		{[]string{"schema", "race", "-s", "--unknown"}, 2, true},
		{[]string{"demo-server", "-s", "--unknown"}, 1, true},
		{[]string{"demo-server", "--silent", "-h"}, 0, true},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			t.Parallel()
			out, stdout, stderr := captureOutput(t)
			code, err := runCLI(t.Context(), test.args, out)
			wantCode := test.code
			if test.args[0] == "time" && runtime.GOOS == "windows" {
				wantCode = 1
			}
			if code != wantCode || (err == nil) != (code == 0) {
				t.Fatalf("runCLI = %d, %v; want %d", code, err, wantCode)
			}
			if test.quiet && stderr.Len() != 0 {
				t.Errorf("silent stderr = %q", stderr)
			}
			if !test.quiet && stderr.Len() == 0 {
				t.Error("expected default diagnostic output")
			}
			if code == 0 && test.args[0] != "race" && stdout.Len() == 0 {
				t.Error("explicit utility output is empty")
			}
		})
	}
}

func TestSilentSchemaDocumentsAndFiles(t *testing.T) {
	t.Parallel()
	for _, subject := range schemaSubjects(commandCatalogue) {
		for _, flag := range []string{"-s", "--silent"} {
			for _, args := range [][]string{
				{"schema", flag, subject.name}, {"schema", subject.name, flag},
			} {
				out, stdout, stderr := captureOutput(t)
				code, err := runCLI(t.Context(), args, out)
				if code != 0 || err != nil || stderr.Len() != 0 ||
					!bytes.Equal(stdout.Bytes(), subject.descriptor.document) {
					t.Fatalf("%v = %d, %v, stderr %q", args, code, err, stderr)
				}
			}
		}
	}
	for _, name := range []string{"-s", "--silent", filepath.Join(t.TempDir(), "schema.json")} {
		out, stdout, stderr := captureOutput(t)
		var file closeErrorBuffer
		opened := ""
		out.openOutput = func(path string) (io.WriteCloser, error) {
			opened = path
			return &file, nil
		}
		code, err := runCLI(t.Context(), []string{"schema", "-s", "race", "-o", name}, out)
		if code != 0 || err != nil || opened != name || stdout.Len() != 0 ||
			stderr.Len() != 0 || !bytes.Equal(file.Bytes(), raceSchema.document) {
			t.Fatalf("file %q = %d, %v; opened %q", name, code, err, opened)
		}
	}
}

func TestSilentOutputFailures(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("supplied output failed")
	for _, mode := range []string{"open", "write", "close"} {
		out, _, stderr := captureOutput(t)
		out.openOutput = func(string) (io.WriteCloser, error) {
			switch mode {
			case "open":
				return nil, sentinel
			case "write":
				return &writeCloseErrorFile{writeErr: sentinel}, nil
			default:
				return &closeErrorBuffer{err: sentinel}, nil
			}
		}
		code, err := runCLI(t.Context(), []string{"schema", "-s", "race", "-o", "report"}, out)
		if code != 1 || !errors.Is(err, sentinel) || stderr.Len() != 0 {
			t.Errorf("%s = %d, %v; stderr %q", mode, code, err, stderr)
		}
	}
	for _, args := range [][]string{
		{"schema", "-s", "race"},
		{"help", "-s", "output"},
		{"version", "-s"},
		{"completion", "-s", "bash"},
	} {
		out, _, stderr := captureOutput(t)
		out.stdout = failingWriter{sentinel}
		code, err := runCLI(t.Context(), args, out)
		if code != 1 || !errors.Is(err, sentinel) || stderr.Len() != 0 {
			t.Errorf("%v = %d, %v; stderr %q", args, code, err, stderr)
		}
	}
	for _, silent := range []bool{false, true} {
		out, _, _ := captureOutput(t)
		out.stderr = failingWriter{sentinel}
		args := []string{"detect", "--dry-run", "--direction", "late", "https://example.test", "--repeat", "2"}
		if silent {
			args = slices.Insert(slices.Clone(args), 1, "--silent")
		}
		code, err := runCLI(t.Context(), args, out)
		if silent && (code != 0 || err != nil) {
			t.Errorf("silent discard = %d, %v", code, err)
		}
		if !silent && (code != 1 || !errors.Is(err, sentinel)) {
			t.Errorf("default diagnostic failure = %d, %v", code, err)
		}
	}
}

func TestSilentLiteralRequestValues(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"-s", "--silent"} {
		for _, option := range []string{"--url", "--header", "--data", "--output"} {
			out, _, stderr := captureOutput(t)
			// The value must not silence the following usage failure.
			code, err := runCLI(t.Context(), []string{"race", option, flag, "--unknown"}, out)
			if code != 2 || err == nil || stderr.Len() == 0 {
				t.Errorf("%s %s = %d, %v; stderr %q", option, flag, code, err, stderr)
			}
		}
		out, _, stderr := captureOutput(t)
		code, err := runCLI(t.Context(), []string{"race", "--", flag}, out)
		if code != 1 || err == nil || stderr.Len() == 0 {
			t.Errorf("literal positional URL %s = %d, %v; stderr %q", flag, code, err, stderr)
		}
	}
	out, stdout, stderr := captureOutput(t)
	code, err := runCLI(t.Context(), []string{
		"race", "--dry-run", "--block", "https://example.test", "--name", "--silent",
	}, out)
	if code != 0 || err != nil || !strings.Contains(stdout.String(), "--silent") {
		t.Fatalf("block name value = %d, %v; stderr %q", code, err, stderr)
	}
}

func TestSilentCompletionPositions(t *testing.T) {
	t.Parallel()
	for _, words := range [][]string{
		{"tturl", "race", ""},
		{"tturl", "race", "https://example.test", ""},
		{"tturl", "schema", ""},
		{"tturl", "schema", "race", ""},
		{"tturl", "help", "--silent", ""},
		{"tturl", "completion", "-s", ""},
		{"tturl", "demo-server", ""},
		{"tturl", "version", ""},
	} {
		assertCompletionValues(t, words, "-s", "--silent")
	}
	for _, words := range [][]string{
		{"tturl", ""},
		{"tturl", "race", "--block", "https://example.test", ""},
		{"tturl", "help", "output", ""},
		{"tturl", "completion", "bash", ""},
		{"tturl", "race", "--", ""},
	} {
		assertCompletionOmits(t, words, "-s", "--silent")
	}
}

func TestSilentExecutionAndFailureEvidence(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, stderr := captureOutput(t)
	code, err := runCLI(t.Context(), []string{
		"race", "-s", "-k", "--trials", "3", "--arrange", "rotate",
		"--block", "https://" + addr + "/a", "--block", "https://" + addr + "/b",
	}, out)
	if code != 0 || err != nil || stderr.Len() != 0 || stdout.Len() == 0 || requests.Load() != 8 {
		t.Fatalf("live silent race = %d, %v; stderr %q; requests %d", code, err, stderr, requests.Load())
	}
	for _, format := range []string{"text", "json"} {
		out, stdout, stderr := captureOutput(t)
		code, err := runCLI(t.Context(), []string{
			"detect", "--silent", "--direction", "late", "--comparisons-max", "2", "--report", format,
			"--block", "https://127.0.0.1:1/a", "--block", "https://127.0.0.1:1/b",
		}, out)
		if code != 1 || err == nil || stderr.Len() != 0 {
			t.Fatalf("failed silent detect = %d, %v; stderr %q", code, err, stderr)
		}
		if format == "text" {
			for _, fact := range []string{
				"Decision unavailable:", "Completion: failed.",
				"No rank-complete adaptive comparisons were observed.",
			} {
				if !strings.Contains(stdout.String(), fact) {
					t.Errorf("report lacks %q", fact)
				}
			}
		} else {
			lines := jsonLines(t, stdout.Bytes())
			var result jsonDetectResultRecord
			if err := json.Unmarshal(lines[len(lines)-1], &result); err != nil {
				t.Fatal(err)
			}
			if result.Completion.State != completionFailed || result.Completion.Failure == nil ||
				result.Detection.State != "unavailable" {
				t.Errorf("failure evidence = %+v", result)
			}
			var run struct {
				Argv []string `json:"argv"`
			}
			if err := json.Unmarshal(lines[0], &run); err != nil {
				t.Fatal(err)
			}
			if len(run.Argv) < 2 || run.Argv[0] != "detect" || run.Argv[1] != "--silent" {
				t.Errorf("argv = %v", run.Argv)
			}
		}
	}
}

func TestSilentPreservesDetectTerminationSemantics(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	base := []string{
		"detect", "--report", "json", "--direction", "late",
		"--comparisons-max", "40",
		"--block", "https://example.test/a", "--name", "A",
		"--block", "https://example.test/b", "--name", "B",
	}
	var completions [2]structuredCompletion
	for index, args := range [][]string{
		base,
		slices.Insert(slices.Clone(base), 1, "--silent"),
	} {
		out, stdout, stderr := captureOutput(t)
		code, err := runCLI(ctx, args, out)
		if code != 1 || !errors.Is(err, context.Canceled) {
			t.Fatalf("run %d = %d, %v; stderr %q", index, code, err, stderr)
		}
		if index == 1 && stderr.Len() != 0 {
			t.Errorf("silent diagnostics = %q", stderr)
		}
		records := jsonLines(t, stdout.Bytes())
		assertResponseLimitRecordsConform(t, detectSchema.filename, records)
		validateStructuredRecords(t, records)
		if got := recordKind(t, records[len(records)-1]); got != "result" {
			t.Fatalf("run %d terminal kind = %q, want result", index, got)
		}
		var terminal jsonDetectResultRecord
		if err := json.Unmarshal(records[len(records)-1], &terminal); err != nil {
			t.Fatal(err)
		}
		if terminal.Detection.State != "unavailable" {
			t.Fatalf("run %d detection = %+v", index, terminal.Detection)
		}
		completions[index] = terminal.Completion
	}
	if !reflect.DeepEqual(completions[0], completions[1]) {
		t.Errorf("silent completion = %+v, want %+v", completions[1], completions[0])
	}
}

func TestSilentDemoServerStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("demo-server requires a supported timing clock")
	}
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	out, _, stderr := captureOutput(t)
	code, err := runCLI(ctx, []string{
		"demo-server", "--silent", "--port", "0", "--timing-work-max", "0", "--quota-work", "0",
	}, out)
	if code != 0 || err != nil || stderr.Len() != 0 {
		t.Fatalf("bounded silent server = %d, %v; stderr %q", code, err, stderr)
	}
}

func TestSilentPreservesDeterministicReports(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, format := range []string{"text", "json"} {
		base := []string{
			"race", "--report", format, "--arrange", "none", "--trials", "2",
			"--block", "https://example.test/a", "--block", "https://example.test/b",
		}
		var reports [2]string
		for index, args := range [][]string{base, slices.Insert(slices.Clone(base), 1, "--silent")} {
			out, stdout, stderr := captureOutput(t)
			code, err := runCLI(ctx, args, out)
			if code != 1 || !errors.Is(err, context.Canceled) || (index == 1 && stderr.Len() != 0) {
				t.Fatalf("cancelled %s = %d, %v; stderr %q", format, code, err, stderr)
			}
			reports[index] = stdout.String()
			if format == "json" {
				lines := jsonLines(t, stdout.Bytes())
				var header struct {
					StartedAt string   `json:"started_at"`
					Argv      []string `json:"argv"`
				}
				if err := json.Unmarshal(lines[0], &header); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(header.Argv, args) {
					t.Fatalf("argv = %v; want %v", header.Argv, args)
				}
				// Only wall-clock start and the intentional argv addition differ.
				reports[index] = strings.Replace(reports[index], header.StartedAt, "2000-01-01T00:00:00Z", 1)
				if index == 1 {
					reports[index] = strings.Replace(reports[index], `"argv":["race","--silent",`, `"argv":["race",`, 1)
				}
			}
		}
		if reports[0] == "" || reports[0] != reports[1] {
			t.Errorf("%s report bytes changed:%s", format, firstReportDifference(reports[0], reports[1]))
		}
	}
}

func TestSilentDoesNotLeakBetweenInvocations(t *testing.T) {
	t.Parallel()
	out, _, stderr := captureOutput(t)
	for _, args := range [][]string{{"detect", "-s"}, {"detect"}, {"detect", "--silent"}, {"detect"}} {
		stderr.Reset()
		code, err := runCLI(t.Context(), args, out)
		if code != 2 || err == nil || (stderr.Len() == 0) != (len(args) == 2) {
			t.Fatalf("%v = %d, %v; stderr %q", args, code, err, stderr)
		}
	}
}

func TestSilentLiveProgressAndRequestEchoes(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(600 * time.Millisecond)
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, silent := range []bool{false, true} {
		out, stdout, stderr := captureOutput(t)
		args := []string{
			"race", "-k", "--report", "json", "--trials", "3", "--arrange", "none",
			"--connections", "1", "https://" + addr + "/probe", "--repeat", "2",
		}
		if silent {
			args = slices.Insert(slices.Clone(args), 1, "-s")
		}
		code, err := runCLI(t.Context(), args, out)
		if code != 0 || err != nil || stdout.Len() == 0 {
			t.Fatalf("live progress run = %d, %v", code, err)
		}
		if silent && stderr.Len() != 0 {
			t.Errorf("silent progress/echo stderr = %q", stderr)
		}
		if !silent && (!strings.Contains(stderr.String(), "trials complete") ||
			!strings.Contains(stderr.String(), "Target:")) {
			t.Errorf("default run did not exercise progress and echoes: %q", stderr)
		}
	}
}

func TestSilentDetectWarnings(t *testing.T) {
	t.Parallel()
	for _, silent := range []bool{false, true} {
		out, _, stderr := captureOutput(t)
		args := []string{
			"detect", "--dry-run", "--direction", "late", "--width", "3", "--strategy", "peer-first",
			"--block", "https://example.test", "--repeat", "3", "--block", "https://example.test/base", "--baseline-only",
		}
		if silent {
			args = slices.Insert(slices.Clone(args), 1, "--silent")
		}
		code, err := runCLI(t.Context(), args, out)
		if code != 0 || err != nil {
			t.Fatalf("detect warning run = %d, %v", code, err)
		}
		if silent && stderr.Len() != 0 {
			t.Errorf("silent detect stderr = %q", stderr)
		}
		if !silent && (!strings.Contains(stderr.String(), diagnosticPrefix(diagnosticNotice)) ||
			!strings.Contains(stderr.String(), diagnosticPrefix(diagnosticWarning))) {
			t.Errorf("default detect did not exercise notices and warnings: %q", stderr)
		}
	}
}

func TestSilentUtilityOutputBytes(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"help"},
		{"help", "output"},
		{"version"},
		{"completion", "bash"},
		{"completion", "zsh"},
		{"completion", "fish"},
	} {
		out, expected, _ := captureOutput(t)
		code, err := runCLI(t.Context(), args, out)
		if code != 0 || err != nil {
			t.Fatalf("default utility = %d, %v", code, err)
		}
		for _, flag := range []string{"-s", "--silent"} {
			out, actual, stderr := captureOutput(t)
			code, err := runCLI(t.Context(), slices.Insert(slices.Clone(args), 1, flag, flag), out)
			if code != 0 || err != nil || stderr.Len() != 0 || !bytes.Equal(expected.Bytes(), actual.Bytes()) {
				t.Errorf("%v %s = %d, %v; output bytes changed", args, flag, code, err)
			}
		}
	}
}

func TestSilentRequestCommandPreviews(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		base := []string{command, "--dry-run", "--repeat", "2", "https://example.test"}
		if command == "detect" {
			base = slices.Insert(slices.Clone(base), 1, "--direction", "late")
		}
		out, expected, _ := captureOutput(t)
		code, err := runCLI(t.Context(), base, out)
		if code != 0 || err != nil {
			t.Fatalf("default %s preview = %d, %v", command, code, err)
		}
		for _, flag := range []string{"-s", "--silent"} {
			for _, position := range []int{1, len(base)} {
				out, stdout, stderr := captureOutput(t)
				code, err := runCLI(t.Context(), slices.Insert(slices.Clone(base), position, flag, flag), out)
				if code != 0 || err != nil || stderr.Len() != 0 || !bytes.Equal(expected.Bytes(), stdout.Bytes()) {
					t.Fatalf("%s %s at %d = %d, %v; preview bytes changed", command, flag, position, code, err)
				}
			}
		}
		out, stdout, stderr := captureOutput(t)
		file := &failingFile{}
		var contents bytes.Buffer
		out.openOutput = func(string) (io.WriteCloser, error) {
			return silentTestFile{Writer: &contents, Closer: file}, nil
		}
		code, err = runCLI(t.Context(), slices.Insert(slices.Clone(base), 1, "-s", "--output", "preview.txt"), out)
		if code != 0 || err != nil || stdout.Len() != 0 || stderr.Len() != 0 ||
			!file.closed || !bytes.Equal(expected.Bytes(), contents.Bytes()) {
			t.Fatalf("%s file preview = %d, %v; closed %t", command, code, err, file.closed)
		}
	}
}

type silentTestFile struct {
	io.Writer
	io.Closer
}

func TestSilentRetainsClosedStdoutPolicy(t *testing.T) {
	t.Parallel()
	for _, pipe := range []bool{false, true} {
		out, _, stderr := captureOutput(t)
		out.stdout = failingWriter{io.ErrClosedPipe}
		out.stdoutPipe = pipe
		code, err := runCLI(t.Context(), []string{"schema", "-s", "race"}, out)
		if pipe && (code != 0 || err != nil) {
			t.Errorf("consumer-closed stdout = %d, %v", code, err)
		}
		if !pipe && (code != 1 || !errors.Is(err, io.ErrClosedPipe)) {
			t.Errorf("ordinary closed writer = %d, %v", code, err)
		}
		if stderr.Len() != 0 {
			t.Errorf("closed stdout diagnostic = %q", stderr)
		}
	}
}

func TestSilentSelectionRespectsAttachedValuesAndShortOptions(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"race", "--header=--silent", "--unknown"},
		{"race", "-H--silent", "--unknown"},
		{"race", "-H=--silent", "--unknown"},
		{"race", "--data=-s", "--unknown"},
		{"schema", "--output=--silent", "unknown"},
		{"schema", "--output", "-s", "unknown"},
		{"demo-server", "--bind-host", "--silent", "--unknown"},
	} {
		out, _, stderr := captureOutput(t)
		code, err := runCLI(t.Context(), args, out)
		if code == 0 || err == nil || stderr.Len() == 0 {
			t.Errorf("literal value in %v = %d, %v; stderr %q", args, code, err, stderr)
		}
	}
	out, stdout, stderr := captureOutput(t)
	code, err := runCLI(t.Context(), []string{
		"race", "-ks", "--dry-run", "--repeat", "2", "https://example.test",
	}, out)
	if code != 0 || err != nil || stderr.Len() != 0 || stdout.Len() == 0 {
		t.Fatalf("clustered switches = %d, %v; stderr %q", code, err, stderr)
	}
	code, err = runCLI(t.Context(), []string{"__complete", "--silent", "bash", "2"}, out)
	if code != 2 || err == nil {
		t.Fatalf("completion query options = %d, %v", code, err)
	}
}

func TestSilentLiteralValuesSurviveMaterialisation(t *testing.T) {
	t.Parallel()
	base := []string{
		"detect", "--dry-run", "--verbose", "--direction", "late",
		"--data-raw", "--silent", "--header", "X-Example: -s",
		"--user-agent", "-s", "--name", "--silent", "--repeat", "2", "https://example.test",
	}
	out, expected, stderr := captureOutput(t)
	code, err := runCLI(t.Context(), base, out)
	if code != 0 || err != nil || stderr.Len() == 0 {
		t.Fatalf("literal request values = %d, %v; stderr %q", code, err, stderr)
	}
	for _, flag := range []string{"-s", "--silent"} {
		out, actual, stderr := captureOutput(t)
		code, err := runCLI(t.Context(), slices.Insert(slices.Clone(base), len(base), flag), out)
		if code != 0 || err != nil || stderr.Len() != 0 || !bytes.Equal(expected.Bytes(), actual.Bytes()) {
			t.Errorf("silent literal request values = %d, %v; preview changed", code, err)
		}
		out, _, stderr = captureOutput(t)
		var file bytes.Buffer
		owned := &failingFile{}
		out.openOutput = func(path string) (io.WriteCloser, error) {
			if path != flag {
				t.Errorf("output path = %q; want %q", path, flag)
			}
			return silentTestFile{Writer: &file, Closer: owned}, nil
		}
		args := slices.Insert(slices.Clone(base), 1, "-s", "-o", flag)
		code, err = runCLI(t.Context(), args, out)
		if code != 0 || err != nil || stderr.Len() != 0 || !owned.closed || !bytes.Equal(expected.Bytes(), file.Bytes()) {
			t.Errorf("literal filename = %d, %v; closed %t", code, err, owned.closed)
		}
	}
}

func TestSilentSchemaCreatesAndTruncatesFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "schema.json")
	for _, existing := range []bool{false, true} {
		if existing {
			if err := os.WriteFile(path, bytes.Repeat([]byte("old contents"), 10000), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		out, stdout, stderr := captureOutput(t)
		code, err := runCLI(t.Context(), []string{"schema", "race", "--silent", "--output", path}, out)
		if code != 0 || err != nil || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("schema file = %d, %v; stderr %q", code, err, stderr)
		}
		//nolint:gosec // The fixture constructs this path under t.TempDir.
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, raceSchema.document) {
			t.Fatalf("schema file bytes differ: %v", err)
		}
	}
}

func TestSilentPreservesAnalysisCautions(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, stderr := captureOutput(t)
	code, err := runCLI(t.Context(), []string{
		"analyse", "--silent", "-k", "--cycles", "6", "--report", "json",
		"--repeat", "2", "https://" + addr + "/probe",
	}, out)
	if code != 0 || err != nil || stderr.Len() != 0 {
		t.Fatalf("silent analyse = %d, %v; stderr %q", code, err, stderr)
	}
	document := decodeAnalyseJSONL(t, stdout.Bytes())
	if !document.Plan.BelowModelledDefault || document.Plan.ModelledDefaultCycles != 150 ||
		document.Plan.Work.PlannedCycles != 6 || len(document.Evidence.Trials) != 12 {
		t.Errorf("analysis caution/evidence lost: %+v", document.Plan)
	}
}
