package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/h2test"
)

func TestRunCLIInterruptedDetectExitsOneAfterTerminalReport(t *testing.T) {
	for _, test := range []struct {
		name   string
		limit  string
		format string
	}{
		{name: "bounded text", limit: "2"},
		{name: "unbounded text", limit: "unlimited"},
		{name: "bounded JSON", limit: "2", format: "json"},
		{name: "unbounded JSON", limit: "unlimited", format: "json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			args := []string{
				"detect", "--direction", "late",
				"--comparisons-max", test.limit,
				"--block", "https://127.0.0.1:1/a", "--name", "A",
				"--block", "https://127.0.0.1:1/b", "--name", "B",
			}
			if test.format != "" {
				args = append(args[:1], append(
					[]string{"--report", test.format}, args[1:]...)...)
			}
			var stdout, stderr bytes.Buffer
			code, err := runCLI(ctx, args, commandOutput{
				stdout: &stdout, stderr: &stderr,
			})
			if code != 1 || !errors.Is(err, context.Canceled) {
				t.Fatalf("runCLI = %d, %v; want 1, cancellation", code, err)
			}
			if strings.Contains(stderr.String(),
				diagnosticPrefix(diagnosticError)) ||
				strings.Contains(stderr.String(), "context canceled") {
				t.Errorf("stderr adds a second failure:\n%s", &stderr)
			}
			if test.format == "json" {
				lines := jsonLines(t, stdout.Bytes())
				schema := compileStructuredSchema(
					t, detectSchema.filename)
				for index, line := range lines {
					var record any
					if err := json.Unmarshal(line, &record); err != nil {
						t.Fatal(err)
					}
					if err := schema.Validate(record); err != nil {
						t.Fatalf("record %d does not conform: %v", index, err)
					}
				}
				var result jsonDetectResultRecord
				if err := json.Unmarshal(lines[len(lines)-1], &result); err != nil {
					t.Fatal(err)
				}
				if result.Detection.State != "unavailable" ||
					result.Completion.State != completionStopped ||
					result.Completion.Failure == nil ||
					result.Completion.Failure.Code != "interrupted" {
					t.Errorf("terminal result = %+v", result)
				}
			} else if !strings.Contains(stdout.String(),
				"Completion: stopped.") ||
				strings.Contains(stdout.String(), "Completion: failed.") {
				t.Errorf("terminal text report:\n%s", &stdout)
			}
		})
	}
}

func TestRunCLIInterruptedFixedWorkExitsOneAfterTerminalReport(t *testing.T) {
	blocks := []string{
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}
	for _, test := range []struct {
		command string
		budget  []string
		schema  string
	}{
		{
			command: "race", budget: []string{"--trials", "1"},
			schema: raceSchema.filename,
		},
		{
			command: "measure", budget: []string{"--trials", "1"},
			schema: measureSchema.filename,
		},
		{
			command: "analyse", budget: []string{"--cycles", "6"},
			schema: analyseSchema.filename,
		},
	} {
		t.Run(test.command, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			args := []string{test.command, "--report", "json"}
			args = append(args, test.budget...)
			args = append(args, blocks...)
			var stdout, stderr bytes.Buffer
			code, err := runCLI(ctx, args, commandOutput{
				stdout: &stdout, stderr: &stderr,
			})
			if code != 1 || !errors.Is(err, context.Canceled) {
				t.Fatalf("runCLI = %d, %v; want 1, cancellation", code, err)
			}
			if strings.Contains(stderr.String(),
				diagnosticPrefix(diagnosticError)) ||
				strings.Contains(stderr.String(), "context canceled") {
				t.Errorf("stderr adds a second failure:\n%s", &stderr)
			}
			lines := jsonLines(t, stdout.Bytes())
			schema := compileStructuredSchema(t, test.schema)
			for index, line := range lines {
				var record any
				if err := json.Unmarshal(line, &record); err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(record); err != nil {
					t.Fatalf("record %d does not conform: %v", index, err)
				}
			}
			var result struct {
				Completion structuredCompletion `json:"completion"`
			}
			if err := json.Unmarshal(lines[len(lines)-1], &result); err != nil {
				t.Fatal(err)
			}
			if result.Completion.State != completionStopped || result.Completion.Failure == nil ||
				result.Completion.Failure.Code != "interrupted" || result.Completion.Failure.Message == "" {
				t.Errorf("terminal completion = %+v", result.Completion)
			}
		})
	}
}

func TestRunCLIDetectFiniteInconclusiveExitsZero(t *testing.T) {
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	var stdout, stderr bytes.Buffer
	code, err := runCLI(t.Context(), []string{
		"detect", "--insecure", "--direction", "late",
		"--comparisons-max", "1", "--connections", "1",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, commandOutput{stdout: &stdout, stderr: &stderr})
	if code != 0 || err != nil {
		t.Fatalf("runCLI = %d, %v; want success", code, err)
	}
	if !strings.Contains(stdout.String(), "Finding: Inconclusive;") {
		t.Errorf("report omitted inconclusive result:\n%s", &stdout)
	}
}

func TestRunCLIRootMetaOutput(t *testing.T) {
	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"-V"},
		{"--version"},
	} {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(t.Context(), args, commandOutput{
			stdout: &stdout,
			stderr: &stderr,
		})
		if err != nil || code != 0 {
			t.Errorf("runCLI(%q) = code %d, error %v; want success",
				args, code, err)
		}
		if stdout.Len() == 0 {
			t.Errorf("runCLI(%q) wrote no stdout", args)
		}
		rootHelp := len(args) == 0 || args[0] == "-h" || args[0] == "--help"
		if rootHelp && stdout.String() != topLevelUsage {
			t.Errorf("runCLI(%q) stdout differs from root help:\n%s",
				args, &stdout)
		}
		if stderr.Len() != 0 {
			t.Errorf("runCLI(%q) stderr = %q, want empty", args, &stderr)
		}
	}
}

func TestRunCLIVersionSpellingsMatch(t *testing.T) {
	var want string
	for _, args := range [][]string{{"-V"}, {"--version"}, {"version"}} {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(t.Context(), args, commandOutput{
			stdout: &stdout,
			stderr: &stderr,
		})
		if code != 0 || err != nil {
			t.Errorf("runCLI(%q) = code %d, error %v; want success",
				args, code, err)
		}
		if stderr.Len() != 0 {
			t.Errorf("runCLI(%q) stderr = %q, want empty", args, &stderr)
		}
		if want == "" {
			want = stdout.String()
		} else if got := stdout.String(); got != want {
			t.Errorf("runCLI(%q) stdout = %q, want %q", args, got, want)
		}
	}
}

func TestRunCLIHiddenVersionRejectsArguments(t *testing.T) {
	for _, argument := range []string{"extra", "-h"} {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(t.Context(), []string{"version", argument},
			commandOutput{stdout: &stdout, stderr: &stderr})
		if code != 2 || err == nil || err.Error() != "version accepts no arguments" {
			t.Errorf("runCLI(version %s) = code %d, error %v; want usage error",
				argument, code, err)
		}
		if stdout.Len() != 0 {
			t.Errorf("runCLI(version %s) stdout = %q, want empty",
				argument, &stdout)
		}
		want := diagnosticPrefix(diagnosticError) +
			"version accepts no arguments\n"
		if got := stderr.String(); got != want {
			t.Errorf("runCLI(version %s) stderr = %q, want %q",
				argument, got, want)
		}
	}
}

func TestRunCLIBareInvocationIsUsageError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runCLI(t.Context(), nil, commandOutput{
		stdout: &stdout, stderr: &stderr,
	})
	if err == nil || code != 2 {
		t.Fatalf("runCLI(nil) = code %d, error %v; want code 2", code, err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", &stdout)
	}
	wantPrefix := diagnosticPrefix(diagnosticError) + "command required\n\n"
	if got := stderr.String(); got != wantPrefix+topLevelUsage {
		t.Errorf("stderr = %q, want diagnostic and root usage", got)
	}
}

func TestRunCLICommandHelpUsesStdout(t *testing.T) {
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		t.Run(command, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, err := runCLI(
				t.Context(), []string{command, "-h"}, commandOutput{
					stdout: &stdout,
					stderr: &stderr,
				})
			if err != nil || code != 0 {
				t.Fatalf("runCLI(%s -h) = code %d, error %v; want success",
					command, code, err)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", &stderr)
			}
			if stdout.Len() == 0 {
				t.Error("command help wrote no stdout")
			}
		})
	}
}

func TestRunCLIUsageFailuresUseStderr(t *testing.T) {
	t.Run("unknown command", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(t.Context(), []string{"absent"}, commandOutput{
			stdout: &stdout,
			stderr: &stderr,
		})
		if err == nil || code != 2 {
			t.Errorf("runCLI(absent) = code %d, error %v; want code 2",
				code, err)
		}
		if stdout.Len() != 0 ||
			!strings.Contains(stderr.String(), diagnosticPrefix(diagnosticError)) ||
			!strings.Contains(stderr.String(), "unknown command \"absent\"") ||
			!strings.Contains(stderr.String(), topLevelUsage) {
			t.Errorf("stdout = %q, stderr = %q; want error and usage on stderr",
				&stdout, &stderr)
		}
	})

	t.Run("command parse error", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(
			context.Background(),
			[]string{"measure", "--not-a-flag"},
			commandOutput{stdout: &stdout, stderr: &stderr},
		)
		if err == nil || code != 2 {
			t.Errorf("runCLI(parse error) = code %d, error %v; want code 2",
				code, err)
		}
		if stdout.Len() != 0 ||
			!strings.Contains(stderr.String(), diagnosticPrefix(diagnosticError)) ||
			!strings.Contains(stderr.String(), toolName+" measure -h") {
			t.Errorf("stdout = %q, stderr = %q; want hinted error on stderr",
				&stdout, &stderr)
		}
	})

	t.Run("command validation error", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(t.Context(), []string{
			"measure", "--trials", "0",
			"--block", "https://example.invalid/a",
			"--block", "https://example.invalid/b",
		}, commandOutput{stdout: &stdout, stderr: &stderr})
		if err == nil || code != 2 {
			t.Errorf("runCLI(validation error) = code %d, error %v; want code 2",
				code, err)
		}
		if stdout.Len() != 0 ||
			!strings.Contains(stderr.String(), "--trials must be a positive decimal integer") {
			t.Errorf("stdout = %q, stderr = %q; want validation error on stderr",
				&stdout, &stderr)
		}
	})
}

func TestRunCLIUnknownCommandEscapesInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	_, err := runCLI(t.Context(), []string{"caf\xc3\xa9\x1b[31m"}, commandOutput{
		stdout: &stdout,
		stderr: &stderr,
	})
	if err == nil {
		t.Fatal("runCLI returned nil error for an unknown command")
	}
	for _, b := range stderr.Bytes() {
		if b >= utf8RuneSelf || b == '\x1b' {
			t.Fatalf("stderr contains unsafe byte 0x%02x: %s", b, stderr.String())
		}
	}
	if !strings.Contains(stderr.String(), "$HEX[") {
		t.Errorf("stderr = %q, want escaped command", stderr.String())
	}
}

func TestRunCLIPreservesMetaWriteFailures(t *testing.T) {
	want := errors.New("write failed")
	tests := []struct {
		name string
		args []string
		out  commandOutput
		code int
	}{
		{
			name: "root help",
			args: []string{"-h"},
			out: commandOutput{
				stdout: failingWriter{err: want},
				stderr: io.Discard,
			},
			code: 1,
		},
		{
			name: "root version",
			args: []string{"-V"},
			out: commandOutput{
				stdout: failingWriter{err: want},
				stderr: io.Discard,
			},
			code: 1,
		},
		{
			name: "hidden version",
			args: []string{"version"},
			out: commandOutput{
				stdout: failingWriter{err: want},
				stderr: io.Discard,
			},
			code: 1,
		},
		{
			name: "bare invocation diagnostic",
			out: commandOutput{
				stdout: io.Discard,
				stderr: failingWriter{err: want},
			},
			code: 1,
		},
		{
			name: "command help",
			args: []string{"race", "-h"},
			out: commandOutput{
				stdout: failingWriter{err: want},
				stderr: io.Discard,
			},
			code: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, err := runCLI(t.Context(), test.args, test.out)
			if code != test.code || !errors.Is(err, want) {
				t.Errorf("runCLI() = code %d, error %v; want code %d and %v",
					code, err, test.code, want)
			}
		})
	}
}
