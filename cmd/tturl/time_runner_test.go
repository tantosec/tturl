package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/timing"
)

func TestTimeRunnerStaticValidationBeforeOutput(t *testing.T) {
	output := filepath.Join(t.TempDir(), "report")
	runner := func(context.Context, *timing.Resolved, func(timing.Event) error) (timing.Result, error) {
		t.Fatal("invalid plan reached engine")
		return timing.Result{}, nil
	}
	err := runTimeCommandForPlatform(context.Background(), commandByID(commandTime),
		[]string{"--output", output, "--block", "http://example.test"},
		commandOutput{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}, "linux", runner)
	if err == nil {
		t.Fatal("invalid plaintext record policy accepted")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("output created before validation: %v", err)
	}
}

func TestTimeRunnerPreviewProtocolAndNoEngine(t *testing.T) {
	var out bytes.Buffer
	runner := func(context.Context, *timing.Resolved, func(timing.Event) error) (timing.Result, error) {
		t.Fatal("preview reached engine")
		return timing.Result{}, nil
	}
	err := runTimeCommandForPlatform(context.Background(), commandByID(commandTime),
		[]string{
			"--dry-run", "--verbose", "--block", "https://example.test/a", "--name", "first",
			"--http1.1", "-H", "Host: override.test",
			"--block", "https://example.test/b", "--name", "second",
		},
		commandOutput{stdout: &out, stderr: &bytes.Buffer{}}, "linux", runner)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GET /a HTTP/1.1", "Host: override.test", ":path: /b",
		"Protocols: first HTTP/1.1; second HTTP/2.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("preview lacks %q: %s", want, out.String())
		}
	}
}

func TestTimeWindowsPriority(t *testing.T) {
	for _, args := range [][]string{
		{"time", "--silent=invalid"},
		{"time", "--dry-run", "--output", "missing/report", "--block", "https://example.test", "--data-binary", "@missing"},
		{"time", "-H", "-h"},
		{"time", "--name", "-h"},
	} {
		var out, diagnostics bytes.Buffer
		status, err := runCLIForPlatform(context.Background(), args,
			commandOutput{
				stdout: &out, stderr: &diagnostics,
				openOutput: func(string) (io.WriteCloser, error) {
					t.Fatal("unsupported command opened output")
					return nil, errors.New("output unavailable")
				},
			}, "windows")
		if status != 1 || err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("%v: status %d error %v", args, status, err)
		}
		if out.Len() != 0 {
			t.Fatalf("unexpected report: %s", out.String())
		}
	}
	var out bytes.Buffer
	status, err := runCLIForPlatform(context.Background(), []string{"time", "-h"},
		commandOutput{stdout: &out, stderr: &bytes.Buffer{}}, "windows")
	if status != 0 || err != nil || !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("help: %d %v %s", status, err, out.String())
	}
}

func TestTimeRunnerCompletionClassification(t *testing.T) {
	t.Setenv("SSLKEYLOGFILE", "")
	fatal := errors.New("independent acquisition failure")
	for _, tc := range []struct {
		name   string
		result timing.Result
		runErr error
		want   error
	}{
		{name: "completed incomplete trials", result: timing.Result{
			Completion: timing.CompletionComplete,
			Accounting: timing.Accounting{TrialsAttempted: 1, TrialsIncomplete: 1},
		}},
		{name: "fatal", result: timing.Result{
			Completion: timing.CompletionFailed,
			Failure:    &timing.Failure{Phase: timing.PhaseAcquisition, Cause: fatal},
		}, want: fatal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.result.StartedAt = time.Now()
			called := false
			runner := func(context.Context, *timing.Resolved, func(timing.Event) error) (timing.Result, error) {
				called = true
				return tc.result, tc.runErr
			}
			err := runTimeCommandForPlatform(context.Background(), commandByID(commandTime),
				[]string{"--block", "https://example.test"},
				commandOutput{stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}, "linux", runner)
			if !called || !errors.Is(err, tc.want) {
				t.Fatalf("called %t error %v; want %v", called, err, tc.want)
			}
		})
	}
}

func TestTimeBrokenPipeStopRequiresFulfilledRun(t *testing.T) {
	other := errors.New("output failure")
	for _, tc := range []struct {
		name    string
		trials  int
		pipe    bool
		err     error
		failure *timing.Failure
		want    bool
	}{
		{name: "unlimited consumer stop", pipe: true, err: io.ErrClosedPipe, want: true},
		{name: "bounded consumer stop", trials: 1, pipe: true, err: io.ErrClosedPipe},
		{name: "ordinary output error", pipe: true, err: other},
		{name: "file output error", err: io.ErrClosedPipe},
		{
			name: "independent acquisition failure", pipe: true, err: io.ErrClosedPipe,
			failure: &timing.Failure{Phase: timing.PhaseAcquisition},
		},
		{
			name: "engine output stop", pipe: true, err: io.ErrClosedPipe,
			failure: &timing.Failure{Phase: timing.PhaseOutput}, want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := timeBrokenPipeStop(&reportDestination{pipe: tc.pipe}, timeCommandConfig{trials: tc.trials},
				tc.err, timing.Result{Failure: tc.failure})
			if got != tc.want {
				t.Fatalf("fulfilled stop %t; want %t", got, tc.want)
			}
		})
	}
}
