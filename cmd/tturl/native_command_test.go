package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	nativeBuildTimeout   = 2 * time.Minute
	nativeCommandTimeout = 15 * time.Second
)

var nativeVersion = regexp.MustCompile(
	`^` + toolName + ` (?:` +
		`devel(?: \([0-9a-f]{1,12}(?:-dirty)?\))?` +
		`|v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.` +
		`(?:0|[1-9][0-9]*)(?:-rc\.[1-9][0-9]*)?` +
		`(?: \([0-9a-f]{1,12}\))?` +
		`)\n$`,
)

type nativeCommandRecord struct {
	Kind     string                 `json:"kind"`
	ID       int                    `json:"request_id"`
	URL      string                 `json:"url"`
	Outcomes []nativeCommandOutcome `json:"outcomes"`
}

type nativeCommandOutcome struct {
	Request  int                    `json:"request_id"`
	Response *nativeCommandResponse `json:"response"`
}

type nativeCommandResponse struct {
	Status               int                 `json:"status"`
	BodyCapturedComplete bool                `json:"body_captured_complete"`
	Headers              map[string][]string `json:"headers"`
	Body                 *nativeCommandBody  `json:"body"`
}

type nativeCommandBody struct {
	Encoding string `json:"encoding"`
	Data     string `json:"data"`
}

func TestNativeCommandSurface(t *testing.T) {
	binary := buildNativeCommand(t)
	testNativeMetaCommands(t, binary)
	if runtime.GOOS == "windows" {
		testNativeDemoServerUnsupported(t, binary)
		return
	}

	server := startNativeDemoServer(t, binary)
	testNativeRace(t, binary, server.endpoint)
	server.interrupt(t)
}

func testNativeDemoServerUnsupported(t *testing.T, binary string) {
	t.Helper()
	stdout, stderr, err := runNativeCommand(t, binary, "demo-server")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("demo-server error = %v, want exit code 1", err)
	}
	var want bytes.Buffer
	if err := writeDiagnostic(
		&want, diagnosticError, errDemoServerUnsupported.Error(),
	); err != nil {
		t.Fatalf("render unsupported diagnostic: %v", err)
	}
	if stdout != "" || stderr != want.String() {
		t.Errorf("demo-server stdout = %q, stderr = %q; want %q",
			stdout, stderr, want.String())
	}
}

func buildNativeCommand(t *testing.T) string {
	t.Helper()
	name := toolName
	if nativeExecutableSuffix != "" {
		name += nativeExecutableSuffix
	}
	path := filepath.Join(t.TempDir(), name)
	ctx, cancel := context.WithTimeout(t.Context(), nativeBuildTimeout)
	defer cancel()
	cmd := exec.CommandContext( //nolint:gosec // Fixed Go tool and temp output.
		ctx, "go", "build", "-o", path, ".")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build native %s: %v\n%s", toolName, err, output)
	}
	return path
}

func testNativeMetaCommands(t *testing.T, binary string) {
	t.Helper()
	stdout, stderr, err := runNativeCommand(t, binary, "-h")
	if err != nil {
		t.Fatalf("%s -h: %v\nstderr:\n%s", toolName, err, stderr)
	}
	if stdout != topLevelUsage || stderr != "" {
		t.Errorf("%s -h stdout = %q, stderr = %q", toolName, stdout, stderr)
	}

	var versionOutput string
	for _, spelling := range []string{"--version", "version"} {
		stdout, stderr, err = runNativeCommand(t, binary, spelling)
		if err != nil {
			t.Fatalf("%s %s: %v\nstderr:\n%s", toolName, spelling, err, stderr)
		}
		if !nativeVersion.MatchString(stdout) || stderr != "" {
			t.Errorf("%s %s stdout = %q, stderr = %q",
				toolName, spelling, stdout, stderr)
		}
		if versionOutput == "" {
			versionOutput = stdout
		} else if stdout != versionOutput {
			t.Errorf("%s %s stdout = %q, want %q",
				toolName, spelling, stdout, versionOutput)
		}
	}

	unsafeCommand := "caf\u00e9\x1b[31m"
	stdout, stderr, err = runNativeCommand(t, binary, unsafeCommand)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("%s unknown command error = %v, want exit code 2", toolName, err)
	}
	if stdout != "" || !strings.Contains(stderr, "$HEX[") {
		t.Errorf("%s unknown command stdout = %q, stderr = %q", toolName, stdout, stderr)
	}
	assertNativeASCII(t, stderr)

	stdout, stderr, err = runNativeCommand(t, binary, "demo-server", "-h")
	if err != nil {
		t.Fatalf("%s demo-server -h: %v\nstderr:\n%s", toolName, err, stderr)
	}
	if !strings.HasPrefix(stdout, toolName+" demo-server - ") || stderr != "" {
		t.Errorf("%s demo-server -h stdout = %q, stderr = %q", toolName, stdout, stderr)
	}
}

func runNativeCommand(
	t *testing.T,
	binary string,
	args ...string,
) (stdout, stderr string, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), nativeCommandTimeout)
	defer cancel()
	var stdoutBuffer, stderrBuffer bytes.Buffer
	cmd := exec.CommandContext( //nolint:gosec // Runs the test-built command.
		ctx, binary, args...)
	cmd.Stdout = &stdoutBuffer
	cmd.Stderr = &stderrBuffer
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s %q exceeded %s", toolName, args, nativeCommandTimeout)
	}
	return stdoutBuffer.String(), stderrBuffer.String(), err
}

func assertNativeASCII(t *testing.T, value string) {
	t.Helper()
	for i, b := range []byte(value) {
		if b >= utf8RuneSelf || b == '\x1b' {
			t.Fatalf("output byte %d = 0x%02x, want safe ASCII: %q", i, b, value)
		}
	}
}

type nativeDemoServer struct {
	cmd      *exec.Cmd
	endpoint string
	done     <-chan struct{}
	waitErr  *error
	logs     <-chan string
}

func startNativeDemoServer(t *testing.T, binary string) *nativeDemoServer {
	t.Helper()
	cmd := exec.CommandContext(
		t.Context(), binary, "demo-server", "--port", "0")
	configureNativeInterrupt(cmd)
	cmd.Stdout = nil
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("create demo-server stderr pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start native demo-server: %v", err)
	}

	endpoint := make(chan string, 1)
	logs := make(chan string, 1)
	go func() {
		var captured strings.Builder
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			captured.WriteString(line)
			captured.WriteByte('\n')
			if address, ok := strings.CutPrefix(line, "Listening on "); ok {
				select {
				case endpoint <- address:
				default:
				}
			}
		}
		if err := scanner.Err(); err != nil {
			_, _ = fmt.Fprintf(&captured, "read stderr: %v\n", err)
		}
		logs <- captured.String()
	}()

	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	server := &nativeDemoServer{
		cmd: cmd, done: done, waitErr: &waitErr, logs: logs,
	}
	t.Cleanup(func() {
		select {
		case <-done:
			return
		default:
		}
		_ = cmd.Process.Kill()
		<-done
	})

	select {
	case server.endpoint = <-endpoint:
		if err := validateNativeEndpoint(server.endpoint); err != nil {
			t.Fatalf("native demo-server endpoint: %v", err)
		}
		return server
	case <-done:
		t.Fatalf("native demo-server exited during startup: %v\n%s",
			waitErr, <-logs)
	case <-time.After(nativeCommandTimeout):
		t.Fatalf("native demo-server did not announce readiness within %s",
			nativeCommandTimeout)
	}
	return nil
}

func (s *nativeDemoServer) interrupt(t *testing.T) {
	t.Helper()
	if err := interruptNativeProcess(s.cmd.Process); err != nil {
		t.Fatalf("interrupt native demo-server: %v", err)
	}
	select {
	case <-s.done:
		logs := <-s.logs
		if *s.waitErr != nil {
			t.Fatalf("native demo-server after interrupt: %v\n%s", *s.waitErr, logs)
		}
	case <-time.After(nativeCommandTimeout):
		t.Fatalf("native demo-server did not stop within %s", nativeCommandTimeout)
	}
}

func testNativeRace(t *testing.T, binary, endpoint string) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "native reports")
	if err := os.Mkdir(directory, 0o750); err != nil {
		t.Fatalf("create report directory: %v", err)
	}
	crlfPath := filepath.Join(directory, "durations crlf.txt")
	lfPath := filepath.Join(directory, "durations lf.txt")
	if err := os.WriteFile(crlfPath, []byte("1us\r\n"), 0o600); err != nil {
		t.Fatalf("write CRLF source: %v", err)
	}
	if err := os.WriteFile(lfPath, []byte("1us\n"), 0o600); err != nil {
		t.Fatalf("write LF source: %v", err)
	}

	crlfReport := filepath.Join(directory, "race crlf.jsonl")
	_, stderr, err := runNativeCommand(t, binary,
		"race", "-k", "--report", "json", "--capture-headers",
		"--capture-body", "unlimited", "--output", crlfReport,
		"--block", endpoint+"/", "--name", "index",
		"--block", endpoint+"/sleep?duration=D", "--name", "sleep",
		"--vary", "D=@"+crlfPath,
	)
	if err != nil {
		t.Fatalf("native race with CRLF source: %v\nstderr:\n%s", err, stderr)
	}
	records := readNativeCommandRecords(t, crlfReport)
	wantSleepURL := endpoint + "/sleep?duration=1us"
	validateNativeRaceRecords(t, records, endpoint+"/", wantSleepURL)

	lfReport := filepath.Join(directory, "race lf.jsonl")
	_, stderr, err = runNativeCommand(t, binary,
		"race", "-k", "--report", "json", "--output", lfReport,
		"--vary", "D=@"+lfPath, endpoint+"/sleep?duration=D",
	)
	if err != nil {
		t.Fatalf("native race with LF source: %v\nstderr:\n%s", err, stderr)
	}
	if got := nativeRequestURLs(readNativeCommandRecords(t, lfReport)); len(got) != 1 || got[0] != wantSleepURL {
		t.Fatalf("LF request URLs = %q, want [%q]", got, wantSleepURL)
	}
	if got := nativeRequestURLs(records); !containsString(got, wantSleepURL) {
		t.Fatalf("CRLF request URLs = %q, want one equal to LF URL %q", got, wantSleepURL)
	}
}

func readNativeCommandRecords(t *testing.T, path string) []nativeCommandRecord {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // Path is inside t.TempDir.
	if err != nil {
		t.Fatalf("read native report %s: %v", path, err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	records := make([]nativeCommandRecord, 0, len(lines))
	for _, line := range lines {
		var record nativeCommandRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("decode native report record %q: %v", line, err)
		}
		records = append(records, record)
	}
	return records
}

func validateNativeRaceRecords(
	t *testing.T,
	records []nativeCommandRecord,
	indexURL, sleepURL string,
) {
	t.Helper()
	requests := make(map[int]string)
	var trial *nativeCommandRecord
	for i := range records {
		switch records[i].Kind {
		case "request":
			requests[records[i].ID] = records[i].URL
		case "trial":
			if trial != nil {
				t.Fatal("native race emitted more than one trial")
			}
			trial = &records[i]
		}
	}
	if len(requests) != 2 || trial == nil || len(trial.Outcomes) != 2 {
		t.Fatalf("native race requests = %v, trial = %+v", requests, trial)
	}
	for _, outcome := range trial.Outcomes {
		requestURL := requests[outcome.Request]
		if outcome.Response == nil {
			t.Fatalf("native response for %q is absent", requestURL)
		}
		switch requestURL {
		case indexURL:
			if outcome.Response.Status != http.StatusOK {
				t.Errorf("route index status = %d, want %d",
					outcome.Response.Status, http.StatusOK)
			}
			if outcome.Response.Body == nil ||
				outcome.Response.Body.Encoding != "base64" ||
				outcome.Response.Body.Data != base64.StdEncoding.EncodeToString(
					[]byte(demoServerRouteIndex)) ||
				!outcome.Response.BodyCapturedComplete {
				t.Errorf("route index response = %+v, body = %+v",
					outcome.Response, outcome.Response.Body)
			}
		case sleepURL:
			validateNativeSleepResponse(t, outcome.Response)
		default:
			t.Errorf("unexpected native request URL %q", requestURL)
		}
	}
}

func validateNativeSleepResponse(t *testing.T, response *nativeCommandResponse) {
	t.Helper()
	if response.Status != http.StatusNoContent {
		t.Errorf("sleep status = %d, want %d", response.Status, http.StatusNoContent)
	}
	for _, name := range []string{
		"x-server-time-handler-start",
		"x-server-time-sleep-start",
		"x-server-time-sleep-end",
	} {
		values := response.Headers[name]
		if len(values) != 1 {
			t.Errorf("sleep header %s = %q, want one value", name, values)
			continue
		}
		if _, err := strconv.ParseInt(values[0], 10, 64); err != nil {
			t.Errorf("sleep header %s = %q: %v", name, values[0], err)
		}
	}
	if response.Body == nil || response.Body.Encoding != "base64" ||
		response.Body.Data != "" ||
		!response.BodyCapturedComplete {
		t.Errorf("sleep body capture = %+v", response)
	}
}

func nativeRequestURLs(records []nativeCommandRecord) []string {
	var urls []string
	for _, record := range records {
		if record.Kind == "request" {
			urls = append(urls, record.URL)
		}
	}
	return urls
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func validateNativeEndpoint(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Scheme != "https" || parsed.Hostname() != "127.0.0.1" ||
		parsed.Port() == "" || parsed.Path != "" {
		return fmt.Errorf("unexpected loopback endpoint %q", value)
	}
	return nil
}
