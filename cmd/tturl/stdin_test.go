package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/h2test"
)

func TestProcessInputInheritedCancellation(t *testing.T) {
	const helper = "TTURL_TEST_INHERITED_STDIN"
	if os.Getenv(helper) == "1" {
		// Each invocation uses the actual inherited, blocking os.Stdin. The
		// producer stays open and supplies no bytes throughout these reads.
		for range 3 {
			ctx, cancel := context.WithCancel(t.Context())
			input := &signalledInput{
				Reader:  &processInput{ctx: ctx, file: os.Stdin},
				started: make(chan struct{}),
			}
			cancelled := make(chan struct{})
			go func() {
				defer close(cancelled)
				<-input.started
				time.Sleep(50 * time.Millisecond)
				cancel()
			}()
			started := time.Now()
			data, err := newStdinSnapshot(ctx, commandOutput{stdin: input})()
			<-cancelled
			cancel()
			if len(data) != 0 || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled snapshot: %q, %v", data, err)
			}
			if elapsed := time.Since(started); elapsed > time.Second {
				t.Fatalf("cancellation took %s", elapsed)
			}
		}
		if _, err := os.Stdin.Stat(); err != nil {
			t.Fatalf("borrowed stdin was closed: %v", err)
		}
		stack := make([]byte, 1<<20)
		stack = stack[:runtime.Stack(stack, true)]
		if bytes.Contains(stack, []byte("processInput).Read")) {
			t.Fatalf("stdin reader or cancellation callback survived:\n%s", stack)
		}
		return
	}
	reader, producer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	defer func() { _ = producer.Close() }()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	//nolint:gosec // Re-execute this test with fixed helper arguments.
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestProcessInputInheritedCancellation$")
	cmd.Env = append(os.Environ(), helper+"=1")
	cmd.Stdin = reader
	output, err := cmd.CombinedOutput()
	// The writer is deliberately still open: producer EOF must not rescue a
	// blocked read. Waiting for child exit also verifies reader cleanup.
	if err != nil {
		t.Fatalf("inherited stdin did not cancel and clean up: %v\n%s", err, output)
	}
}

type signalledInput struct {
	io.Reader
	started chan struct{}
}

func (r *signalledInput) Read(p []byte) (int, error) {
	select {
	case <-r.started:
	default:
		close(r.started)
	}
	return r.Reader.Read(p)
}

func TestStdinSnapshotCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r, w := io.Pipe()
	defer func() { _ = w.Close() }()
	input := &signalledInput{Reader: r, started: make(chan struct{})}
	closed := make(chan struct{})
	load := newStdinSnapshot(ctx, commandOutput{
		stdin: input,
		cancelInput: func() error {
			defer close(closed)
			return r.Close()
		},
	})
	returned := make(chan error, 1)
	go func() { _, err := load(); returned <- err }()
	<-input.started
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("load: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked read did not return")
	}
	select {
	case <-closed:
	default:
		t.Fatal("cancellation callback was not joined")
	}
	if _, err := w.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("pipe remains open: %v", err)
	}
}

func TestStdinSnapshotReuse(t *testing.T) {
	input := strings.NewReader("a\x00\xff\r\nb")
	load := newStdinSnapshot(t.Context(), commandOutput{
		stdin: input,
	})
	first, err := load()
	if err != nil {
		t.Fatal(err)
	}
	first[0] = 'z'
	second, err := load()
	if err != nil || !bytes.Equal(second, []byte("a\x00\xff\r\nb")) {
		t.Fatalf("snapshot: %q, %v", second, err)
	}
}

func TestStdinRequestRunners(t *testing.T) {
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		t.Run(command, func(t *testing.T) {
			var output bytes.Buffer
			args := []string{
				command, "--dry-run", "--verbose",
				"--vary", "TOKEN=@-", "--data-binary", "@-", "https://example.invalid/TOKEN",
			}
			if command == "detect" {
				args = append(args, "--direction", "late")
			}
			code, err := runCLI(t.Context(), args, commandOutput{
				stdin: strings.NewReader("a\nb\n"), stdout: &output, stderr: io.Discard,
			})
			if err != nil || code != 0 {
				t.Fatalf("status %d: %v", code, err)
			}
			for _, want := range []string{":path: /a", ":path: /b", "$HEX[610a620a]"} {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("missing %q: %s", want, &output)
				}
			}
		})
	}
}

type partialFailureInput struct {
	calls int
	err   error
}

func (r *partialFailureInput) Read(p []byte) (int, error) {
	r.calls++
	return copy(p, "partial"), r.err
}

func TestStdinSnapshotFailuresAndEmpty(t *testing.T) {
	cause := errors.New("input failed")
	input := &partialFailureInput{err: cause}
	load := newStdinSnapshot(t.Context(), commandOutput{
		stdin: input,
	})
	for range 2 {
		data, err := load()
		if len(data) != 0 || !errors.Is(err, cause) {
			t.Fatalf("%q, %v", data, err)
		}
	}
	if input.calls != 1 {
		t.Fatalf("reads: %d", input.calls)
	}
	if _, err := newStdinSnapshot(t.Context(), commandOutput{})(); err == nil {
		t.Fatal("unavailable input succeeded")
	}
	data, err := newStdinSnapshot(t.Context(), commandOutput{
		stdin: strings.NewReader(""),
	})()
	if err != nil || len(data) != 0 {
		t.Fatalf("empty snapshot: %q, %v", data, err)
	}
}

func TestStdinIsLazy(t *testing.T) {
	for _, args := range [][]string{
		{"race", "--help", "--data-binary", "@-"},
		{"race", "--version"},
		{"race", "--unknown", "--data-binary", "@-"},
		{"race", "--dry-run", "https://example.invalid/"},
	} {
		input := &partialFailureInput{err: errors.New("unexpected read")}
		_, _ = runCLI(t.Context(), args, commandOutput{
			stdin: input, stdout: io.Discard, stderr: io.Discard,
		})
		if input.calls != 0 {
			t.Fatalf("%q read stdin", args)
		}
	}
}

func TestStdinRunnerCancellation(t *testing.T) {
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		t.Run(command, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			r, w := io.Pipe()
			defer func() { _ = w.Close() }()
			input := &signalledInput{Reader: r, started: make(chan struct{})}
			var diagnostics bytes.Buffer
			var requests atomic.Int64
			addr, _ := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			args := []string{
				command, "--silent", "--insecure", "--data-binary", "@-",
				"--vary", "TOKEN=@-", "--repeat", "2", "https://" + addr + "/TOKEN",
			}
			if command == "detect" {
				args = append(args, "--direction", "late")
			}
			returned := make(chan error, 1)
			go func() {
				code, err := runCLI(ctx, args, commandOutput{
					stdin: input, cancelInput: r.Close, stdout: io.Discard, stderr: &diagnostics,
				})
				if code == 0 {
					err = errors.New("cancelled command succeeded")
				}
				returned <- err
			}()
			<-input.started
			cancel()
			select {
			case err := <-returned:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("command did not stop")
			}
			if requests.Load() != 0 {
				t.Fatalf("cancelled preparation sent %d requests", requests.Load())
			}
			if diagnostics.Len() != 0 {
				t.Fatalf("silent diagnostics: %s", &diagnostics)
			}
		})
	}
}

func TestStdinNativePipes(t *testing.T) {
	for _, test := range []struct {
		name, input string
		options     []string
		want        string
	}{
		{"binary", "a\x00\xff", []string{"--data-binary", "@-"}, "$HEX[6100ff]"},
		{"source", "a\nb\n", []string{"--vary", "TOKEN=@-"}, ":path: /b"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			args := append([]string{"run", ".", "race", "--dry-run", "--verbose"}, test.options...)
			args = append(args, "https://example.invalid/TOKEN")
			//nolint:gosec // Fixed Go command and test-authored arguments.
			cmd := exec.CommandContext(ctx, "go", args...)
			cmd.Stdin = strings.NewReader(test.input)
			output, err := cmd.CombinedOutput()
			if err != nil || !bytes.Contains(output, []byte(test.want)) {
				t.Fatalf("pipe: %v, %s", err, output)
			}
		})
	}
}

func TestStdinSnapshotAcrossBlocksAndVariants(t *testing.T) {
	input := &signalledInput{Reader: strings.NewReader("a\nb\n"), started: make(chan struct{})}
	closed := false
	load := newStdinSnapshot(t.Context(), commandOutput{
		stdin: input, cancelInput: func() error { closed = true; return nil },
	})
	plan, err := curlblocks.New(curlblocks.WithStdinReader(load)).Parse([]string{
		"--data-binary", "@-", "--repeat", "2",
		"--block", "--vary", "TOKEN=@-", "https://example.test/TOKEN",
		"--block", "--data-binary", "@-", "https://example.test/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	groups, err := plan.Expand()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 3 {
		t.Fatalf("groups: %d", len(groups))
	}
	for i, group := range groups {
		body, _, _, err := group.Block.RequestBody()
		want := "a\nb\n"
		if i == 2 {
			want += "&a\nb\n"
		}
		if err != nil || string(body) != want || group.Repeat != 2 {
			t.Fatalf("group %d: %q, %v", i, body, err)
		}
		body[0] = 'z'
	}
	if closed {
		t.Fatal("ordinary completion closed borrowed input")
	}
}

func TestStdinLocalExecution(t *testing.T) {
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		t.Run(command, func(t *testing.T) {
			var requests atomic.Int64
			payload := []byte("a\x00\xff\r\nb")
			addr, _ := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(body, payload) {
					t.Errorf("body %q: %v", body, err)
				}
				requests.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			args := []string{
				command, "--insecure", "--data-binary", "@-", "--warmup", "1",
				"--repeat", "2", "--report", "json",
			}
			switch command {
			case "race", "measure":
				args = append(args, "--trials", "2")
			case "analyse":
				args = append(args, "--cycles", "6")
			case "detect":
				args = append(args, "--direction", "late", "--comparisons-max", "2")
			}
			args = append(args,
				"--block", "https://"+addr+"/",
				"--block", "--warmup-only", "https://"+addr+"/prime")
			var output bytes.Buffer
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			code, err := runCLI(ctx, args, commandOutput{
				stdin: bytes.NewReader(payload), stdout: &output, stderr: io.Discard,
			})
			if code != 0 || err != nil {
				t.Fatalf("status %d: %v, %s", code, err, &output)
			}
			if requests.Load() < 4 {
				t.Fatalf("requests: %d", requests.Load())
			}
			if !bytes.Contains(output.Bytes(), []byte(`"@-"`)) {
				t.Fatalf("argv: %s", &output)
			}
		})
	}
}

func TestStdinReadFailureRunners(t *testing.T) {
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		t.Run(command, func(t *testing.T) {
			cause := errors.New("failed input stream")
			input := &partialFailureInput{err: cause}
			var diagnostics bytes.Buffer
			var requests atomic.Int64
			addr, _ := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			args := []string{
				command, "--silent", "--insecure", "--data-binary", "@-",
				"--vary", "TOKEN=@-", "https://" + addr + "/TOKEN",
			}
			if command == "detect" {
				args = append(args, "--direction", "late")
			}
			code, err := runCLI(t.Context(), args, commandOutput{
				stdin: input, stdout: io.Discard, stderr: &diagnostics,
			})
			if code == 0 || !errors.Is(err, cause) {
				t.Fatalf("status %d: %v", code, err)
			}
			if input.calls != 1 || requests.Load() != 0 || diagnostics.Len() != 0 {
				t.Fatalf("reads %d, requests %d, diagnostics %s", input.calls, requests.Load(), &diagnostics)
			}
		})
	}
}
