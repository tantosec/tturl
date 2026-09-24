package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/h2tls"
	"github.com/tantosec/tturl/internal/netdelay"
	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

func TestRunTimeoutFlagsResolveForEveryRequestCommand(t *testing.T) {
	t.Parallel()
	for _, command := range []*commandSpec{
		commandByID(commandRace), commandByID(commandMeasure),
		commandByID(commandAnalyse), commandByID(commandDetect),
	} {
		t.Run(command.name, func(t *testing.T) {
			t.Parallel()
			layout := newCommand(command)
			var flags *sharedFlags
			switch command.id {
			case commandRace:
				flags = configureRaceCommand(layout).shared
			case commandMeasure:
				flags = configureMeasureCommand(layout).shared
			case commandAnalyse:
				flags = configureAnalyseCommand(layout).shared
			case commandDetect:
				flags = configureDetectCommand(layout).shared
			}
			plan, err := layout.parser.Parse([]string{
				"--run-timeout", "2.5s",
				"--block", "https://example.test/a",
				"--block", "https://example.test/b",
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := flags.resolveRunTimeout(plan.Globals)
			if err != nil || got != runTimeout(2500*time.Millisecond) {
				t.Fatalf("timeout = %s, %v", time.Duration(got), err)
			}

			plan, err = layout.parser.Parse([]string{
				"--run-timeout", "UNLIMITED",
				"--block", "https://example.test/a",
				"--block", "https://example.test/b",
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err = flags.resolveRunTimeout(plan.Globals)
			if err != nil || got != 0 {
				t.Fatalf("unlimited timeout = %s, %v", time.Duration(got), err)
			}
		})
	}
}

func TestRunTimeoutValidationPrecedesNetwork(t *testing.T) {
	t.Parallel()
	invalid := []string{"0s", "-1s", "1", " 1s", "1s ", "2562048h"}
	commands := []struct {
		name string
		run  func(context.Context, []string, commandOutput) error
	}{
		{name: "race", run: runRace},
		{name: "measure", run: runMeasure},
		{name: "analyse", run: runAnalyse},
		{name: "detect", run: runDetect},
	}
	for _, command := range commands {
		for _, value := range invalid {
			t.Run(command.name+"/"+value, func(t *testing.T) {
				args := []string{
					"--run-timeout", value,
					"--block", "https://127.0.0.1:1/a",
					"--block", "https://127.0.0.1:1/b",
				}
				if command.name == "detect" {
					args = append([]string{"--direction", "late"}, args...)
				}
				err := command.run(t.Context(), args, commandOutput{
					stdout: io.Discard, stderr: io.Discard,
				})
				var usageErr *usageError
				if !errors.As(err, &usageErr) ||
					!strings.Contains(err.Error(), "--run-timeout") {
					t.Fatalf("error = %v, want run-timeout usage error", err)
				}
			})
		}
	}
}

func TestLiveRunFailureCausePrecedence(t *testing.T) {
	t.Parallel()
	outputErr := errors.New("write output")
	transportErr := errors.New("transport failed")
	batchTimeoutErr := &tth2.BatchTimeoutError{Limit: time.Second}
	bodyLimitErr := &tth2.ResponseBodyLimitError{
		RequestIndex: 0, StreamID: 1, Limit: 4, BytesAccepted: 4,
	}
	for _, test := range []struct {
		name       string
		err        error
		cause      error
		fallback   completionCode
		wantCode   completionCode
		wantState  completionState
		wantDetail error
	}{
		{
			name: "owned timeout", err: context.DeadlineExceeded,
			cause: errRunTimeout, fallback: completionInternalError,
			wantCode: completionRunTimeout, wantState: completionFailed,
			wantDetail: errRunTimeout,
		},
		{
			name: "caller deadline", err: context.DeadlineExceeded,
			cause: context.DeadlineExceeded, fallback: completionInternalError,
			wantCode: completionDeadlineExceeded, wantState: completionFailed,
			wantDetail: context.DeadlineExceeded,
		},
		{
			name: "caller cancellation", err: context.Canceled,
			cause: context.Canceled, fallback: completionInternalError,
			wantCode: completionInterrupted, wantState: completionFailed,
			wantDetail: context.Canceled,
		},
		{
			name: "output wins timeout", err: outputErr,
			cause: errRunTimeout, fallback: completionInternalError,
			wantCode: completionInternalError, wantState: completionFailed,
			wantDetail: outputErr,
		},
		{
			name: "output cancellation cause", err: context.Canceled,
			cause: outputErr, fallback: completionInternalError,
			wantCode: completionInternalError, wantState: completionFailed,
			wantDetail: outputErr,
		},
		{
			name: "batch timeout wins run timeout", err: batchTimeoutErr,
			cause: errRunTimeout, fallback: completionAcquisitionFailed,
			wantCode: completionAcquisitionFailed, wantState: completionFailed,
			wantDetail: batchTimeoutErr,
		},
		{
			name: "response limit wins run timeout", err: bodyLimitErr,
			cause: errRunTimeout, fallback: completionAcquisitionFailed,
			wantCode: completionAcquisitionFailed, wantState: completionFailed,
			wantDetail: bodyLimitErr,
		},
		{
			name: "transport failure wins run timeout", err: transportErr,
			cause: errRunTimeout, fallback: completionAcquisitionFailed,
			wantCode: completionAcquisitionFailed, wantState: completionFailed,
			wantDetail: transportErr,
		},
		{
			name: "finite work exhaustion completes", fallback: completionAcquisitionFailed,
			wantState: completionComplete,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			completion := liveRunFailed(
				test.err, test.cause, test.fallback)
			if completion.Code != test.wantCode ||
				completion.State != test.wantState {
				t.Fatalf("completion = %+v, want state/code %q/%q",
					completion, test.wantState, test.wantCode)
			}
			if test.wantDetail != nil && completion.Detail != test.wantDetail.Error() {
				t.Fatalf("detail = %q, want %q",
					completion.Detail, test.wantDetail.Error())
			}
		})
	}
	if failure := classifyBatchFailure(batchTimeoutErr); failure == nil ||
		failure.Code != failureBatchTimeout {
		t.Fatalf("batch timeout classification = %+v", failure)
	}
	if failure := classifyBatchFailure(bodyLimitErr); failure == nil ||
		failure.Code != failureResponseBodyLimit {
		t.Fatalf("response limit classification = %+v", failure)
	}
}

func TestReportRunTimeout(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		timeout runTimeout
		want    string
	}{
		{want: "Run timeout: unlimited for live request execution.\n"},
		{
			timeout: runTimeout(1500 * time.Millisecond),
			want:    "Run timeout: 1.5s for live request execution.\n",
		},
	} {
		var output bytes.Buffer
		reportRunTimeout(&output, test.timeout)
		if output.String() != test.want {
			t.Errorf("output = %q, want %q", output.String(), test.want)
		}
	}
}

func TestLiveRunPhaseSnapshotsOwnedCause(t *testing.T) {
	t.Parallel()
	live := startLiveRunPhase(t.Context(), runTimeout(10*time.Millisecond))
	<-live.ctx.Done()
	if cause := live.finish(nil); !errors.Is(cause, errRunTimeout) {
		t.Fatalf("cause = %v, want run timeout", cause)
	}
}

func TestRunTimeoutStopsEveryRequestCommandWithPartialReport(t *testing.T) {
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		_ http.ResponseWriter, request *http.Request,
	) {
		<-request.Context().Done()
	}))
	requests := []string{
		"--insecure", "--connections", "1", "--batch-timeout", "unlimited",
		"--run-timeout", "30ms",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}
	tests := []struct {
		name   string
		run    func(context.Context, []string, commandOutput) error
		args   []string
		schema string
	}{
		{
			name: "race", run: runRace, args: []string{"--trials", "1"},
			schema: raceSchema.filename,
		},
		{
			name: "race_unlimited", run: runRace,
			args:   []string{"--trials", "unlimited"},
			schema: raceSchema.filename,
		},
		{
			name: "measure", run: runMeasure, args: []string{"--trials", "2"},
			schema: measureSchema.filename,
		},
		{
			name: "analyse", run: runAnalyse, args: []string{"--cycles", "6"},
			schema: analyseSchema.filename,
		},
		{name: "detect", run: runDetect, args: []string{
			"--direction", "late", "--comparisons-max", "1",
		}, schema: detectSchema.filename},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			args := append([]string{"--report", "json"}, test.args...)
			args = append(args, requests...)
			err := test.run(t.Context(), args, commandOutput{
				stdout: &stdout, stderr: io.Discard,
			})
			if !errors.Is(err, errRunTimeout) {
				t.Fatalf("error = %v, want run timeout", err)
			}
			lines := jsonLines(t, stdout.Bytes())
			if len(lines) < 2 {
				t.Fatalf("report has %d records: %s", len(lines), stdout.String())
			}
			schema := compileStructuredSchema(t, test.schema)
			for index, line := range lines {
				var instance any
				if err := json.Unmarshal(line, &instance); err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(instance); err != nil {
					t.Fatalf("record %d does not conform: %v", index, err)
				}
			}
			var invalid any
			if err := json.Unmarshal(lines[0], &invalid); err != nil {
				t.Fatal(err)
			}
			invalid.(map[string]any)["acquisition"].(map[string]any)["run_timeout_ns"] = float64(0)
			if err := schema.Validate(invalid); err == nil {
				t.Fatal("schema accepted a zero run timeout")
			}
			var run struct {
				Acquisition struct {
					RunTimeoutNS *int64 `json:"run_timeout_ns"`
				} `json:"acquisition"`
			}
			if err := json.Unmarshal(lines[0], &run); err != nil {
				t.Fatal(err)
			}
			if run.Acquisition.RunTimeoutNS == nil ||
				*run.Acquisition.RunTimeoutNS != int64(30*time.Millisecond) {
				t.Fatalf("run timeout = %v", run.Acquisition.RunTimeoutNS)
			}
			var terminal struct {
				Completion structuredCompletion `json:"completion"`
				Execution  struct {
					RequestOperations structuredExecutionRequestOperations `json:"request_operations"`
				} `json:"execution"`
			}
			if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil {
				t.Fatal(err)
			}
			if terminal.Completion.Failure == nil || terminal.Completion.Failure.Code != "run_timeout" {
				t.Fatalf("completion = %+v", terminal.Completion)
			}
			if terminal.Execution.RequestOperations.RankComplete != 0 ||
				terminal.Execution.RequestOperations.Retained != 0 {
				t.Fatalf("partial batch contributed ranks: %+v",
					terminal.Execution.RequestOperations)
			}
		})
	}
}

func TestRunTimeoutDuringFittingReturnsOrdinaryErrorWithoutReport(t *testing.T) {
	t.Parallel()
	wrapper, err := netdelay.NewListenerWrapper(netdelay.Config{
		Delay: 50 * time.Millisecond,
		Seed:  1,
	})
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		http.ResponseWriter, *http.Request,
	) {
		t.Error("pipeline fitting reached the HTTP handler")
	}), h2tls.WithListenerWrapper(wrapper))
	var stdout bytes.Buffer
	err = runRace(t.Context(), []string{
		"--report", "json", "--insecure", "--connections-fit", "--batch-rate-max", "10/s",
		"--run-timeout", "20ms", "--trials", "2",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, commandOutput{stdout: &stdout, stderr: io.Discard})
	if !errors.Is(err, errRunTimeout) {
		t.Fatalf("error = %v, want run timeout", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("fitting timeout wrote a report: %s", &stdout)
	}
}

func TestRunTimeoutDuringPrimingDoesNotAttemptMeasuredWork(t *testing.T) {
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		_ http.ResponseWriter, request *http.Request,
	) {
		<-request.Context().Done()
	}))
	var stdout bytes.Buffer
	err := runRace(t.Context(), []string{
		"--report", "json", "--insecure", "--connections", "1",
		"--batch-timeout", "unlimited", "--run-timeout", "30ms",
		"--warmup", "1", "--trials", "1",
		"--block", "https://" + addr + "/prime", "--warmup-only",
		"--block", "https://" + addr + "/measure",
	}, commandOutput{stdout: &stdout, stderr: io.Discard})
	if !errors.Is(err, errRunTimeout) {
		t.Fatalf("error = %v, want run timeout", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	var terminal struct {
		Execution jsonRaceExecution `json:"execution"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal.Execution.Priming.Batches.Attempted != 1 ||
		terminal.Execution.Priming.Batches.Incomplete != 1 ||
		terminal.Execution.Trials.Attempted != 0 ||
		terminal.Execution.RequestOperations.Attempted != 0 {
		t.Fatalf("execution = %+v", terminal.Execution)
	}
}

func TestRunTimeoutPreservesCompletedMeasureEvidence(t *testing.T) {
	var requests atomic.Int64
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		writer http.ResponseWriter, request *http.Request,
	) {
		if requests.Add(1) <= 2 {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		<-request.Context().Done()
	}))
	var stdout bytes.Buffer
	err := runMeasure(t.Context(), []string{
		"--report", "json", "--insecure", "--connections", "1",
		"--batch-timeout", "unlimited", "--run-timeout", "50ms",
		"--trials", "2", "--warmup", "0", "--arrange", "none",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}, commandOutput{stdout: &stdout, stderr: io.Discard})
	if !errors.Is(err, errRunTimeout) {
		t.Fatalf("error = %v, want run timeout", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	var terminal struct {
		Execution  jsonMeasureExecution `json:"execution"`
		Completion structuredCompletion `json:"completion"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal.Completion.Failure == nil ||
		terminal.Completion.Failure.Code != "run_timeout" ||
		terminal.Execution.Trials.RankComplete != 1 ||
		terminal.Execution.RequestOperations.RankComplete != 2 ||
		terminal.Execution.RequestOperations.Retained != 2 {
		t.Fatalf("terminal = %+v", terminal)
	}
	if got := requests.Load(); got < 3 || got > 4 {
		t.Fatalf("requests started = %d, want only two submitted batches", got)
	}
}

func TestRunTimeoutCancelsReplacementAcquisition(t *testing.T) {
	t.Parallel()
	fatal := errors.New("connection lost")
	initial := &scriptedLease{id: 61, active: true, sends: []scriptedLeaseSend{{
		err: fatal, active: false,
	}}}
	evidence := newDetectEvidenceCollector(2)
	comparator := &leasedDetectComparator{
		origin: "example.test:443", reqs: testRequests(t, "a", "b"),
		warmup: testRequests(t, "prime"), width: 2, trials: 1,
		evidence: evidence,
	}
	acquisitions := 0
	comparator.acquire = func(
		ctx context.Context, _ string,
	) (connectionLease, error) {
		acquisitions++
		if acquisitions == 1 {
			return initial, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	comparator.initWorkers(1)
	defer comparator.Close()
	live := startLiveRunPhase(t.Context(), runTimeout(20*time.Millisecond))
	_, compareErr := comparator.compare(live.ctx, []ranking.ItemID{0, 1})
	comparator.Close()
	cause := live.finish(nil)
	completion := liveRunFailed(
		compareErr, cause, completionComparisonFailed)
	result := evidence.unavailable(compareErr, completion)
	execution, err := result.executionAccounting(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if completion.Code != completionRunTimeout || acquisitions != 2 ||
		execution.Comparisons.Attempted != 0 ||
		execution.RequestOperations.Attempted != 0 ||
		len(result.Execution.AcquisitionFailures) != 1 {
		t.Fatalf("completion = %+v, acquisitions = %d, execution = %+v",
			completion, acquisitions, execution)
	}
}

func TestFinishedLivePhaseDoesNotTimeOfflineAnalysis(t *testing.T) {
	t.Parallel()
	live := startLiveRunPhase(t.Context(), runTimeout(20*time.Millisecond))
	cause := live.finish(nil)
	time.Sleep(30 * time.Millisecond)
	result := analyseOffline(
		analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} }),
		analyseConfig{ConnectionLimit: 1},
	)
	result.Completion = finaliseLiveCompletion(
		result.Completion, cause, completionAcquisitionFailed)
	if !result.Validity.Available || result.Completion != completedRun() {
		t.Fatalf("analysis = available %t, completion %+v",
			result.Validity.Available, result.Completion)
	}
}

func TestDriveDetectPreservesDecisionCommittedBeforeDisposalTimeout(t *testing.T) {
	t.Parallel()
	requests := testRequests(t, "a", "b")
	pre := &prepared{
		materialised: &materialised{reqs: requests},
		client:       &tth2.Client{},
	}
	solver := committedDetectSolver{result: ranking.OutlierResult{
		Outcome: ranking.OutlierFound, Index: 1,
		Items: make([]ranking.ItemStat, len(requests)),
	}}
	strategy := detectStrategySelection{
		resolved: detectStrategyPeerFirst, method: solver.MethodID(),
		spec: detectStrategySpec{new: func(int, int) ranking.OutlierSolver {
			return solver
		}},
	}
	disposed := false
	reporter := &captureDetectReporter{afterDispose: func() bool {
		return disposed
	}}
	err := driveDetect(
		t.Context(), pre,
		detectRequestPlan{candidates: 2, widthEffective: 2},
		strategy,
		detectConfig{direction: ranking.Late, comparisonsMax: 1},
		1, 0, 0, 0, []string{"A", "B"}, reporter, io.Discard,
		&reportDestination{},
		func() error {
			disposed = true
			return errRunTimeout
		},
		nil, nil)
	if !errors.Is(err, errRunTimeout) {
		t.Fatalf("driveDetect error = %v, want run timeout", err)
	}
	result := reporter.result
	if !reporter.called || !disposed ||
		result.Ranking.Outcome != ranking.OutlierFound ||
		result.Ranking.Index != 1 || result.Unavailable ||
		result.Completion.Code != completionRunTimeout {
		t.Fatalf("reported result = %+v; called %t, disposed %t",
			result, reporter.called, disposed)
	}
}

type committedDetectSolver struct {
	result ranking.OutlierResult
}

func (committedDetectSolver) Inference() ranking.OutlierInferenceRegime {
	return ranking.OutlierInferenceUncontrolled
}

func (committedDetectSolver) MethodID() ranking.OutlierMethodID {
	return "test-committed-decision/1"
}

func (s committedDetectSolver) Identify(
	context.Context, ranking.OutlierRequest, *ranking.Comparator,
) (ranking.OutlierResult, error) {
	return s.result, nil
}

type captureDetectReporter struct {
	called       bool
	result       detectRunResult
	afterDispose func() bool
}

func (r *captureDetectReporter) Result(result detectRunResult) error {
	if r.afterDispose != nil && !r.afterDispose() {
		return errors.New("detect result reported before live disposal")
	}
	r.called = true
	r.result = result
	return nil
}
