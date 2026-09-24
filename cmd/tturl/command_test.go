package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/h2tls"
)

func TestRaceDialFailureClosesReportWithoutTrial(t *testing.T) {
	t.Parallel()
	listener, err := (&net.ListenConfig{}).Listen(
		t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	out, stdout, _ := captureOutput(t)
	err = runRace(t.Context(), []string{
		"--report", "json",
		"--trials", "3",
		"--connections", "1",
		"--insecure",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, out)
	if err == nil {
		t.Fatal("runRace returned nil error for a failed dial")
	}
	lines := jsonLines(t, stdout.Bytes())
	wantKinds := []string{"run", "request", "request", "result"}
	if len(lines) != len(wantKinds) {
		t.Fatalf("reported %d records, want %d: %s",
			len(lines), len(wantKinds), stdout.String())
	}
	for i, want := range wantKinds {
		if got := recordKind(t, lines[i]); got != want {
			t.Errorf("record %d kind = %q, want %q", i, got, want)
		}
	}
	var summary struct {
		Execution  jsonRaceExecution    `json:"execution"`
		Completion structuredCompletion `json:"completion"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Execution.Trials.RankComplete != 0 ||
		summary.Execution.Trials.Incomplete != 0 ||
		summary.Completion.State != completionFailed || summary.Completion.Failure == nil ||
		summary.Completion.Failure.Code != "connection_error" {
		t.Errorf("summary = %+v, want no trials and a terminal error", summary)
	}
}

func TestRaceRotationRoundUpIsANotice(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	out, _, stderr := captureOutput(t)
	err := runRace(t.Context(), []string{
		"--insecure", "--trials", "3", "--arrange", "rotate",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, out)
	if err != nil {
		t.Fatalf("runRace: %v", err)
	}
	want := diagnosticPrefix(diagnosticNotice) +
		"--trials 3 rounded up to 4"
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("rotation adjustment is not a notice:\n%s", stderr)
	}
}

func TestRaceReplacementFailureClosesPartialReport(t *testing.T) {
	t.Parallel()
	ep, err := h2tls.Listen(h2test.Cert(t))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var requests atomic.Int64
	var server *http.Server
	server = &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if requests.Add(1) == 1 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			closed := make(chan struct{})
			go func() {
				_ = server.Close()
				close(closed)
			}()
			<-closed
		}),
	}
	if err := http2.ConfigureServer(server, &http2.Server{}); err != nil {
		_ = ep.Listener.Close()
		t.Fatalf("configure HTTP/2 server: %v", err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ep.Listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("serve: %v", err)
		}
	})

	out, stdout, _ := captureOutput(t)
	err = runRace(t.Context(), []string{
		"--report", "json",
		"--trials", "5",
		"--connections", "1",
		"--insecure",
		"https://" + ep.Addr + "/probe",
	}, out)
	if err == nil {
		t.Fatal("runRace returned nil error after replacement acquisition failed")
	}
	lines := jsonLines(t, stdout.Bytes())
	if len(lines) < 4 {
		t.Fatalf("JSONL has %d records, want a partial run: %s",
			len(lines), stdout.String())
	}
	if got := recordKind(t, lines[len(lines)-1]); got != "result" {
		t.Fatalf("last record kind = %q, want summary", got)
	}
	trialRecords := 0
	for _, line := range lines {
		if recordKind(t, line) == "trial" {
			trialRecords++
		}
	}
	var summary jsonRaceResultRecord
	if err := json.Unmarshal(lines[len(lines)-1], &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.Completion.State != completionFailed ||
		summary.Execution.Trials.RankComplete < 1 ||
		trialRecords != summary.Execution.Trials.Attempted {
		t.Errorf("trial records/summary = %d/%+v", trialRecords, summary)
	}
}

func captureOutput(t *testing.T) (commandOutput, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	return commandOutput{stdout: &stdout, stderr: &stderr}, &stdout, &stderr
}

func TestOutlierComparisonLimit(t *testing.T) {
	if got := outlierComparisonLimit(123, 8); got != 123 {
		t.Errorf("bounded limit = %d, want 123", got)
	}
	if got := outlierComparisonLimit(-1, 8); got != math.MaxInt/8 {
		t.Errorf("unbounded limit = %d, want %d", got, math.MaxInt/8)
	}
}

func TestDetectWorkLimits(t *testing.T) {
	tests := []struct {
		name           string
		configured     int
		width          int
		wantPipeline   int
		wantOperations int
		wantErr        bool
	}{
		{"width two", 50_000, 2, 50_000, 100_000, false},
		{"width eight", 50_000, 8, 50_000, 400_000, false},
		{"above eight", 3, 11, 3, 33, false},
		{"unlimited", -1, 8, 0, 0, false},
		{
			"exact boundary", math.MaxInt / 8, 8, math.MaxInt / 8,
			(math.MaxInt / 8) * 8, false,
		},
		{"overflow", math.MaxInt/8 + 1, 8, 0, 0, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pipeline, operations, err := detectWorkLimits(
				test.configured, test.width)
			if (err != nil) != test.wantErr {
				t.Fatalf("detectWorkLimits error = %v, want error %t",
					err, test.wantErr)
			}
			if pipeline != test.wantPipeline || operations != test.wantOperations {
				t.Errorf("detectWorkLimits = %d, %d, want %d, %d",
					pipeline, operations,
					test.wantPipeline, test.wantOperations)
			}
		})
	}
}

func TestDetectWiderWidthNotice(t *testing.T) {
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, test := range []struct {
		name       string
		width      string
		candidates string
		wantWidth  int
	}{
		{name: "default", candidates: "2"},
		{name: "explicit two", width: "2", candidates: "3"},
		{name: "wider", width: "3", candidates: "3", wantWidth: 3},
		{name: "above eight", width: "9", candidates: "9", wantWidth: 9},
		{name: "capped to two", width: "9", candidates: "2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{
				"--insecure", "--direction", "late", "--comparisons-max", "1",
				"--connections", "1", "--repeat", test.candidates,
				"https://" + addr + "/candidate",
			}
			if test.width != "" {
				args = append([]string{"--width", test.width}, args...)
			}
			out, _, stderr := captureOutput(t)
			if err := runDetect(t.Context(), args, out); err != nil {
				t.Fatalf("runDetect: %v", err)
			}
			got := stderr.String()
			if test.wantWidth == 0 {
				if strings.Contains(got, "effective width") {
					t.Errorf("unexpected notice:\n%s", got)
				}
				return
			}
			if count := strings.Count(got, "effective width"); count != 1 {
				t.Fatalf("notice count = %d, want 1:\n%s", count, got)
			}
			start := strings.Index(got, diagnosticPrefix(diagnosticNotice)+
				"effective width")
			flat := strings.Join(strings.Fields(got[start:]), " ")
			want := fmt.Sprintf(
				"tturl: notice: effective width %d compares %d requests at once; "+
					"wider batches change target concurrency and can change the "+
					"observed signal. Confirm a surprising negative with --width 2.",
				test.wantWidth, test.wantWidth)
			if flat != want {
				t.Errorf("notice = %q, want %q", flat, want)
			}
			lines := strings.Split(strings.TrimSuffix(got[start:], "\n"), "\n")
			if len(lines) < 2 || !strings.HasPrefix(
				lines[1], strings.Repeat(" ", len(diagnosticPrefix(diagnosticNotice))),
			) {
				t.Errorf("notice lacks hanging indentation:\n%s", got)
			}
			assertTextLinesAtMost(t, got, textWidth)
		})
	}
}

func TestDetectWiderWidthNoticeFollowsValidation(t *testing.T) {
	args := []string{
		"--direction", "late", "--width", "3", "--pin", "missing",
		"--repeat", "3", "https://127.0.0.1:1/candidate",
	}
	out, _, stderr := captureOutput(t)
	err := runDetect(t.Context(), args, out)
	if err == nil || !strings.Contains(err.Error(), "--pin") {
		t.Fatalf("runDetect error = %v, want invalid --pin", err)
	}
	if strings.Contains(stderr.String(), "effective width") {
		t.Errorf("invalid invocation wrote a notice:\n%s", stderr)
	}
}

func TestDetectWiderWidthNoticeWriteFailurePreventsHTTP(t *testing.T) {
	var handled atomic.Int64
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		handled.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	var stdout bytes.Buffer
	stderr := &markerErrorWriter{marker: "effective width 3"}
	err := runDetect(t.Context(), []string{
		"--insecure", "--direction", "late", "--width", "3",
		"--comparisons-max", "1", "--connections", "1",
		"--repeat", "3", "https://" + addr + "/candidate",
	}, commandOutput{stdout: &stdout, stderr: stderr})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("runDetect error = %v, want %v", err, io.ErrClosedPipe)
	}
	if got := handled.Load(); got != 0 {
		t.Errorf("HTTP requests = %d, want 0", got)
	}
}

func TestDetectPaddingFollowsPositionAndSkipsDistinctPriming(t *testing.T) {
	var (
		mu      sync.Mutex
		queries = map[string]string{}
	)
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		mu.Lock()
		queries[r.URL.Path] = r.URL.RawQuery
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, _ := captureOutput(t)
	if err := runDetect(t.Context(), []string{
		"--insecure", "--direction", "late", "--comparisons-max", "1",
		"--connections", "1", "--warmup", "1",
		"--padding", "query=1:-1",
		"--block", "https://" + addr + "/prime-a", "--warmup-only",
		"--block", "https://" + addr + "/prime-b", "--warmup-only",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, out); err != nil {
		t.Fatalf("runDetect: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if queries["/prime-a"] != "" || queries["/prime-b"] != "" {
		t.Errorf("priming queries = %q/%q, want no padding",
			queries["/prime-a"], queries["/prime-b"])
	}
	candidateQueries := []string{queries["/a"], queries["/b"]}
	slices.Sort(candidateQueries)
	if !slices.Equal(candidateQueries, []string{"", "_pad1=x"}) {
		t.Errorf("candidate queries = %q, want one padded outbound position",
			candidateQueries)
	}
	flat := strings.Join(strings.Fields(stdout.String()), " ")
	if !strings.Contains(flat,
		"Padding: outbound positions 0..1; query parameters [1 0].") {
		t.Errorf("detect report omits resolved padding:\n%s", stdout)
	}
}

// TestCommandValidation checks that each command rejects bad flags before
// a connection is opened: the error returns straight from the run function
// with no dial. Every case here fails in parsing/validation, ahead of prepare.
func TestCommandValidation(t *testing.T) {
	twoBlocks := []string{"--block", "https://127.0.0.1:1/a", "--block", "https://127.0.0.1:1/b"}
	cases := []struct {
		name    string
		run     func(context.Context, []string, commandOutput) error
		args    []string
		wantSub string
	}{
		// One case per command is enough: the ranges themselves are covered at
		// their own altitude by TestParseMeasure and TestParseDetect, and what is
		// distinct here is only that the rejection precedes the dial.
		{"race rejects its own flag", runRace, append([]string{"--trials", "0"}, twoBlocks...), "--trials"},
		{"race rejects negative trials", runRace, append([]string{"--trials", "-1"}, twoBlocks...), "--trials"},
		{"measure rejects its own flag", runMeasure, append([]string{"--trials", "0"}, twoBlocks...), "--trials"},
		{"measure rejects zero cycles", runMeasure, append([]string{"--cycles", "0"}, twoBlocks...), "--cycles"},
		{
			"measure rejects two budgets", runMeasure,
			append([]string{"--trials", "20", "--cycles", "2"}, twoBlocks...),
			"mutually exclusive",
		},
		{
			"measure rejects random cycles", runMeasure,
			append([]string{"--cycles", "2", "--arrange", "random"}, twoBlocks...),
			"requires --arrange rotate",
		},
		{
			"detect rejects its own flag", runDetect,
			append([]string{"--false-positive-risk", "2"}, twoBlocks...),
			"--false-positive-risk",
		},
		{
			"detect rejects negative comparisons", runDetect,
			append([]string{
				"--direction", "late", "--comparisons-max", "-1",
			}, twoBlocks...),
			"--comparisons-max",
		},
		{"analyse rejects negative warmup", runAnalyse, append([]string{"--warmup", "-1"}, twoBlocks...), "--warmup"},
		{"analyse rejects explicit zero cycles", runAnalyse, append([]string{"--cycles", "0"}, twoBlocks...), "--cycles"},
		{
			"analyse rejects unitless batch rate", runAnalyse,
			append([]string{"--batch-rate-max", "10"}, twoBlocks...),
			"unit is required",
		},
		{
			"analyse rejects connections below one", runAnalyse,
			append([]string{"--connections", "0"}, twoBlocks...),
			"--connections",
		},
		{
			"detect rejects zero comparisons", runDetect,
			append([]string{"--direction", "late", "--comparisons-max", "0"}, twoBlocks...),
			"--comparisons-max",
		},
		{
			"detect any direction requires a baseline", runDetect,
			append([]string{"--direction", "either"}, twoBlocks...),
			"requires a --baseline-only block",
		},
		{
			"detect reserved strategy requires a baseline", runDetect,
			append([]string{
				"--direction", "late", "--strategy", "baseline-reserved",
			}, twoBlocks...),
			"requires a --baseline-only block",
		},
		{
			"detect confirmed strategy rejects any direction", runDetect,
			[]string{
				"--direction", "either", "--strategy", "baseline-confirmed",
				"--block", "https://127.0.0.1:1/candidate",
				"--block", "https://127.0.0.1:1/baseline", "--baseline-only",
			},
			"requires --direction early or late",
		},
		{
			"shared withheld bytes reject negative", runRace,
			append([]string{"--body-bytes-withheld", "-1"}, twoBlocks...),
			"--body-bytes-withheld",
		},
		{
			"shared release delay requires a duration unit", runRace,
			append([]string{"--release-delay", "5"}, twoBlocks...),
			"--release-delay",
		},
		{
			"shared release delay rejects negative", runRace,
			append([]string{"--release-delay", "-1ms"}, twoBlocks...),
			"--release-delay",
		},
		{
			"unknown report format", runRace,
			append([]string{"--report", "yaml"}, twoBlocks...),
			"--report",
		},
		{
			"empty output path", runRace,
			append([]string{"--output", ""}, twoBlocks...),
			"--output",
		},
		{
			"response payload capture needs JSON", runRace,
			append(
				[]string{"--capture-body", "1KiB"},
				twoBlocks...),
			"--report json",
		},
		{
			"invalid extraction expression", runRace,
			append([]string{"--extract-regex", "("}, twoBlocks...),
			"--extract-regex",
		},
		{"warmup block requires warmup", runRace, []string{
			"--block", "https://127.0.0.1:1/a", "--warmup-only",
			"--block", "https://127.0.0.1:1/b",
		}, "--warmup-only"},
		{"measure warmup block requires warmup", runMeasure, []string{
			"--block", "https://127.0.0.1:1/a", "--warmup-only",
			"--block", "https://127.0.0.1:1/b",
		}, "--warmup-only"},
		{"analyse warmup block requires warmup", runAnalyse, []string{
			"--block", "https://127.0.0.1:1/a", "--warmup-only",
			"--block", "https://127.0.0.1:1/b",
		}, "--warmup-only"},
		{"detect warmup block requires warmup", runDetect, []string{
			"--direction", "late",
			"--block", "https://127.0.0.1:1/a", "--warmup-only",
			"--block", "https://127.0.0.1:1/b",
			"--block", "https://127.0.0.1:1/c",
		}, "--warmup-only"},
		{"detect warmup requires distinct batch", runDetect, append([]string{
			"--direction", "late", "--warmup", "1",
		}, twoBlocks...), "--warmup-only"},
		{
			"measure rejects padding outside width", runMeasure,
			append([]string{"--padding-at", "2:query=1"}, twoBlocks...),
			"--padding-at",
		},
		{
			"analyse rejects padding outside width", runAnalyse,
			append([]string{"--padding-at", "2:query=1"}, twoBlocks...),
			"--padding-at",
		},
		{
			"detect rejects padding outside effective width", runDetect,
			append([]string{
				"--direction", "late", "--padding-at", "2:query=1",
			}, twoBlocks...),
			"--padding-at",
		},
		// The pacing dials are a different mechanism: curlblocks validates them
		// inside Parse, so they surface through parseCommand's hint rather than
		// through transportTuning, whose own error branches are unreachable.
		{"invalid batch rate", runMeasure, append([]string{"--batch-rate-max", "bogus"}, twoBlocks...), "--batch-rate-max"},
		{"unitless request rate", runMeasure, append([]string{"--request-rate-max", "10"}, twoBlocks...), "unit is required"},
		{"connections < 1", runMeasure, append([]string{"--connections", "0"}, twoBlocks...), "--connections"},
		{
			"parallel fit maximum requires a batch rate", runMeasure,
			append([]string{"--connections-fit-max", "600"}, twoBlocks...),
			"requires --connections-fit",
		},
		{
			"parallel fit maximum rejects negatives", runMeasure,
			append([]string{
				"--batch-rate-max", "10/s", "--connections-fit-max", "-1",
			}, twoBlocks...),
			"--connections-fit-max",
		},
		{
			"parallel fit maximum conflicts with explicit parallelism", runMeasure,
			append([]string{
				"--connections-fit", "--batch-rate-max", "10/s", "--connections", "2",
				"--connections-fit-max", "600",
			}, twoBlocks...),
			"conflicts with --connections",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _, _ := captureOutput(t)
			err := c.run(context.Background(), c.args, out)
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("run error = %v, want one mentioning %q", err, c.wantSub)
			}
		})
	}
}

// TestValidationPrecedesOutput verifies that assembled-batch checks fail before
// summary output or dialling.
func TestValidationPrecedesOutput(t *testing.T) {
	oneBlock := []string{"--block", "https://127.0.0.1:1/a"}
	twoBlocks := []string{"--block", "https://127.0.0.1:1/a", "--block", "https://127.0.0.1:1/b"}
	cases := []struct {
		name    string
		run     func(context.Context, []string, commandOutput) error
		args    []string
		wantSub string
	}{
		{"measure needs two requests", runMeasure, oneBlock, "at least 2 requests"},
		{
			"detect needs two requests", runDetect,
			append([]string{"--direction", "late"}, oneBlock...), "at least 2 requests",
		},
		{"analyse needs two requests", runAnalyse, oneBlock, "supports 2..8 requests"},
		{"negative --rank-rows", runMeasure, append([]string{"--rank-rows", "-1"}, twoBlocks...), "--rank-rows"},
		{
			"unknown --pin", runDetect,
			append([]string{"--direction", "late", "--pin", "nope"}, twoBlocks...), "--pin",
		},
		{"every race block is warmup-only", runRace, []string{
			"--warmup", "1",
			"--block", "https://127.0.0.1:1/a", "--warmup-only",
			"--block", "https://127.0.0.1:1/b", "--warmup-only",
		}, "trial request"},
		{
			"padding position outside trial batch", runRace,
			append([]string{"--padding-at", "2:query=1"}, twoBlocks...),
			"--padding-at",
		},
		// MaxInt is odd, so rotating over two requests would need MaxInt+1 trials.
		{
			"--trials too large to round up", runMeasure,
			append([]string{"--trials", strconv.Itoa(math.MaxInt)}, twoBlocks...), "too large",
		},
		{
			"--cycles measured request workload overflows", runMeasure,
			append([]string{"--cycles", strconv.Itoa(math.MaxInt / 2)}, twoBlocks...),
			"too many planned measured request operations",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, stdout, _ := captureOutput(t)
			err := c.run(context.Background(), c.args, out)
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("run error = %v, want one mentioning %q", err, c.wantSub)
			}
			if stdout.Len() != 0 {
				t.Errorf("a rejected command printed %d bytes before failing:\n%s",
					stdout.Len(), stdout.String())
			}
		})
	}
}

func TestRaceCancelledContextStopsUnbounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, stdout, _ := captureOutput(t)
	err := runRace(ctx, []string{
		"--trials", "unlimited",
		"--block", "https://127.0.0.1:1/a",
		"--repeat", "2",
	}, out)
	if err != nil {
		t.Fatalf("runRace error = %v, want clean interruption", err)
	}
	if !strings.Contains(stdout.String(), "Trials:") {
		t.Errorf("interrupted report lacks its completed-work summary:\n%s",
			stdout.String())
	}
	if !strings.Contains(stdout.String(), "Completion: stopped.") ||
		strings.Contains(stdout.String(), "context canceled") {
		t.Errorf("clean interruption is not marked stopped:\n%s", stdout.String())
	}
}

func TestRaceCancelledContextStopsBoundedRun(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, stdout, _ := captureOutput(t)
	err := runRace(ctx, []string{
		"--trials", "1",
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}, out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runRace error = %v, want cancellation", err)
	}
	for _, want := range []string{
		"Trials: 0/1 attempted; 0 rank-complete; 0 incomplete; 1 unattempted.",
		"Completion: stopped.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("bounded interruption report omits %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout.String(), "Error: context canceled") {
		t.Errorf("clean interruption carries redundant detail:\n%s", stdout)
	}
}

func TestMeasureCancelledContextReportsSummary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, stdout, _ := captureOutput(t)
	err := runMeasure(ctx, []string{
		"--trials", "1",
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}, out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runMeasure error = %v, want cancellation", err)
	}
	if !strings.Contains(stdout.String(), "HTTP status outcomes") ||
		!strings.Contains(stdout.String(), "Measurement unavailable") {
		t.Errorf("interrupted report lacks its measurement summary:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(),
		"no complete rotation cycle was retained") {
		t.Errorf("interrupted report misstates position balance:\n%s",
			stdout.String())
	}
	if !strings.Contains(stdout.String(), "Completion: stopped.") {
		t.Errorf("interrupted report omits stopped completion:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "Error: context canceled") {
		t.Errorf("clean interruption carries redundant detail:\n%s", stdout.String())
	}
}

func TestDetectFailureCompletesTextAndJSONReports(t *testing.T) {
	base := []string{
		"--direction", "late", "--comparisons-max", "2",
		"--block", "https://127.0.0.1:1/a", "--name", "A",
		"--block", "https://127.0.0.1:1/b", "--name", "B",
	}
	t.Run("text", func(t *testing.T) {
		out, stdout, _ := captureOutput(t)
		err := runDetect(t.Context(), base, out)
		if err == nil {
			t.Fatal("runDetect succeeded after comparison failure")
		}
		for _, want := range []string{
			"Decision unavailable:",
			"No rank-complete adaptive comparisons were observed.",
			"HTTP status outcomes", "Completion: failed.",
		} {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("detect failure report omits %q:\n%s", want, stdout)
			}
		}
	})
	t.Run("json", func(t *testing.T) {
		out, stdout, _ := captureOutput(t)
		err := runDetect(t.Context(), append(
			[]string{"--report", "json"}, base...), out)
		if err == nil {
			t.Fatal("runDetect succeeded after comparison failure")
		}
		lines := jsonLines(t, stdout.Bytes())
		var result jsonDetectResultRecord
		if err := json.Unmarshal(lines[len(lines)-1], &result); err != nil {
			t.Fatal(err)
		}
		if result.Detection.State != "unavailable" ||
			result.Completion.State != completionFailed ||
			result.Completion.Failure == nil ||
			result.Completion.Failure.Message == "" {
			t.Errorf("detect terminal result = %+v", result)
		}
	})
}

func TestDetectCancellationUsesResolvedBudget(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit string
	}{
		{name: "bounded", limit: "2"},
		{name: "unbounded", limit: "unlimited"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			out, stdout, _ := captureOutput(t)
			err := runDetect(ctx, []string{
				"--direction", "late", "--comparisons-max", test.limit,
				"--block", "https://127.0.0.1:1/a",
				"--block", "https://127.0.0.1:1/b",
			}, out)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("runDetect error = %v, want cancellation", err)
			}
			if !strings.Contains(stdout.String(),
				"Completion: stopped.") {
				t.Errorf("report omits stopped completion:\n%s", stdout)
			}
			if !strings.Contains(stdout.String(),
				"the adaptive search was stopped before a decision.") {
				t.Errorf("report omits stopped decision explanation:\n%s", stdout)
			}
		})
	}
}

func TestRaceDistinctWarmupAndPaddingWiring(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		queries = map[string][]string{}
	)
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		mu.Lock()
		queries[r.URL.Path] = append(queries[r.URL.Path], r.URL.RawQuery)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, _ := captureOutput(t)
	err := runRace(t.Context(), []string{
		"--insecure",
		"--warmup", "1",
		"--padding", "query=-1",
		"--block", "https://" + addr + "/warm",
		"--name", "prime",
		"--warmup-only",
		"--block", "https://" + addr + "/a",
		"--name", "A",
		"--block", "https://" + addr + "/b",
		"--name", "B",
	}, out)
	if err != nil {
		t.Fatalf("runRace: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := queries["/warm"]; len(got) != 1 || got[0] != "" {
		t.Errorf("warmup queries = %v, want one unpadded request", got)
	}
	if got := queries["/a"]; len(got) != 1 ||
		!strings.Contains(got[0], "_pad1=x") {
		t.Errorf("first measured queries = %v, want one padded request", got)
	}
	if got := queries["/b"]; len(got) != 1 || got[0] != "" {
		t.Errorf("second measured queries = %v, want one unpadded request", got)
	}
	for _, want := range []string{
		"/warm  warmup", "2 trial requests, 1 warmup request",
		"Trials: 1 x 2 requests = 2 request operations.",
		"Priming: 1 priming attempt per connection (1 priming request operation).",
		"Arrangement: none.",
		"Padding: outbound positions 0..1; query parameters [1 0].",
		"TLS: certificate verification disabled (--insecure).",
		"Priming 1/1", "prime",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("race output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestFixedWorkDistinctPrimingAndPadding(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(context.Context, []string, commandOutput) error
		work []string
	}{
		{name: "measure", run: runMeasure, work: []string{"--trials", "2"}},
		{name: "analyse", run: runAnalyse, work: []string{"--cycles", "6"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				queries = map[string][]string{}
			)
			addr, _ := h2test.Serve(t, http.HandlerFunc(func(
				w http.ResponseWriter,
				r *http.Request,
			) {
				mu.Lock()
				queries[r.URL.Path] = append(queries[r.URL.Path], r.URL.RawQuery)
				mu.Unlock()
				w.WriteHeader(http.StatusNoContent)
			}))
			args := []string{
				"--report", "json", "--insecure", "--connections", "1",
				"--warmup", "1", "--padding", "query=-1",
				"--block", "https://" + addr + "/warm", "--name", "prime",
				"--warmup-only",
				"--block", "https://" + addr + "/a", "--name", "A",
				"--block", "https://" + addr + "/b", "--name", "B",
			}
			args = append(test.work, args...)
			out, stdout, _ := captureOutput(t)
			if err := test.run(t.Context(), args, out); err != nil {
				t.Fatalf("run %s: %v", test.name, err)
			}

			mu.Lock()
			defer mu.Unlock()
			if got := queries["/warm"]; len(got) != 1 || got[0] != "" {
				t.Errorf("priming queries = %v, want one unpadded request", got)
			}
			padded, measured := 0, 0
			for _, path := range []string{"/a", "/b"} {
				for _, query := range queries[path] {
					measured++
					if strings.Contains(query, "_pad") {
						padded++
					}
				}
			}
			wantMeasured, wantPadded := 4, 2
			if test.name == "analyse" {
				wantMeasured, wantPadded = 24, 12
			}
			if measured != wantMeasured || padded != wantPadded {
				t.Errorf("measured requests/padded = %d/%d, want %d/%d",
					measured, padded, wantMeasured, wantPadded)
			}

			if test.name == "measure" {
				lines := jsonLines(t, stdout.Bytes())
				if len(lines) != 5 {
					t.Fatalf("measure records = %d, want 5", len(lines))
				}
				var run jsonMeasureRunRecord
				if err := json.Unmarshal(lines[0], &run); err != nil {
					t.Fatal(err)
				}
				if run.RequestCount != 3 || run.Plan.BatchWidth != 2 ||
					run.Plan.Warmup.BatchWidth != 1 {
					t.Errorf("measure plan = %+v", run.Plan)
				}
				var warmup jsonMeasureRequestRecord
				if err := json.Unmarshal(lines[3], &warmup); err != nil {
					t.Fatal(err)
				}
				if warmup.RequestID != 2 || warmup.Role != requestSetWarmup {
					t.Errorf("measure priming identity = %+v", warmup)
				}
				return
			}

			document := decodeAnalyseJSONL(t, stdout.Bytes())
			if document.RequestCount != 3 ||
				document.Plan.BatchWidth != 2 ||
				document.Plan.Warmup.BatchWidth != 1 ||
				document.ResultRecord.Execution.Priming.RequestOperations.Attempted != 1 ||
				len(document.Evidence.Requests) != 3 ||
				document.Evidence.Requests[2].Role != requestSetWarmup ||
				len(document.Evidence.Trials) != 12 {
				t.Errorf("analyse priming report = %+v", document)
			}
			if _, err := replayAnalyseJSONStream(
				bytes.NewReader(stdout.Bytes())); err != nil {
				t.Errorf("replay report: %v", err)
			}
		})
	}
}
