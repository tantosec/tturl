package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/ranking"

	"github.com/tantosec/tturl/tth2"
)

type textFixtureCause string

func (e textFixtureCause) Error() string { return string(e) }

func TestTextFailureSemanticCauses(t *testing.T) {
	tests := []struct {
		name string
		a, b error
		same bool
	}{
		{"body limits", &tth2.ResponseBodyLimitError{Limit: 1}, &tth2.ResponseBodyLimitError{Limit: 2}, true},
		{"batch timeouts", &tth2.BatchTimeoutError{Limit: time.Second}, &tth2.BatchTimeoutError{Limit: time.Minute}, true},
		{"wrapped cancellation", context.Canceled, fmt.Errorf("worker: %w", context.Canceled), true},
		{"deadline", context.DeadlineExceeded, fmt.Errorf("worker: %w", context.DeadlineExceeded), true},
		{"run timeout", errRunTimeout, fmt.Errorf("worker: %w", errRunTimeout), true},
		{"wrapped EOF", io.EOF, fmt.Errorf("worker: %w", io.EOF), true},
		{"different fallback causes", io.EOF, io.ErrUnexpectedEOF, false},
		{"different concrete causes", errors.New("same"), textFixtureCause("same"), false},
		{"wrapped connection", &tth2.ConnectionError{Err: io.EOF}, io.EOF, false},
		{
			"network family", &net.OpError{Op: "dial", Net: "tcp", Err: io.EOF},
			&net.OpError{Op: "dial", Net: "udp", Err: io.EOF}, false,
		},
		{
			"stream identity",
			&tth2.StreamError{StreamID: 1, Code: http2.ErrCodeCancel},
			&tth2.StreamError{StreamID: 3, Code: http2.ErrCodeCancel}, true,
		},
		{"stream code", &tth2.StreamError{Code: http2.ErrCodeCancel}, &tth2.StreamError{Code: http2.ErrCodeInternal}, false},
		{
			"protocol connection code",
			http2.ConnectionError(http2.ErrCodeCancel),
			http2.ConnectionError(http2.ErrCodeInternal), false,
		},
		{
			"network protocol operation",
			&net.OpError{Op: "read", Net: "tcp", Err: &tth2.StreamError{Code: http2.ErrCodeCancel}},
			&net.OpError{Op: "write", Net: "tcp", Err: &tth2.StreamError{Code: http2.ErrCodeCancel}}, false,
		},
		{"joined errors", errors.Join(io.EOF, io.ErrUnexpectedEOF), io.EOF, false},
		{
			"network addresses",
			&net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{Port: 1}, Err: io.EOF},
			&net.OpError{Op: "dial", Net: "tcp", Addr: &net.TCPAddr{Port: 2}, Err: io.EOF}, true,
		},
		{
			"network cause",
			&net.OpError{Op: "dial", Net: "tcp", Err: io.EOF},
			&net.OpError{Op: "dial", Net: "tcp", Err: io.ErrUnexpectedEOF}, false,
		},
		{
			"network operation",
			&net.OpError{Op: "dial", Net: "tcp", Err: io.EOF},
			&net.OpError{Op: "read", Net: "tcp", Err: io.EOF}, false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := detectExecutionEvidence{AcquisitionFailures: []detectAcquisitionFailure{
				{Worker: 0, Err: test.a}, {Worker: 1, Err: test.b},
			}}
			groups := detectTextFailureGroups(evidence)
			want := 2
			if test.same {
				want = 1
			}
			if len(groups) != want {
				t.Fatalf("got %d cause groups, want %d", len(groups), want)
			}
			if test.same && (groups[0].attempts != 2 || len(groups[0].identities) != 2) {
				t.Fatalf("incorrect aggregate: %+v", groups[0])
			}
		})
	}
}

func TestDetectFailureGroupsPopulationsAndOrder(t *testing.T) {
	evidence := detectExecutionEvidence{}
	for worker := range 128 {
		for range 3 {
			evidence.PrimingAttempts = append(evidence.PrimingAttempts,
				detectPrimingAttempt{
					Worker: worker, Connection: tth2.ConnectionID(worker + 1),
					Err: fmt.Errorf("worker %d: %w", worker, io.EOF),
				})
		}
		evidence.AcquisitionFailures = append(evidence.AcquisitionFailures,
			detectAcquisitionFailure{Worker: worker, Err: io.EOF},
			detectAcquisitionFailure{Worker: worker, Err: fmt.Errorf("wrapped: %w", io.EOF)},
			detectAcquisitionFailure{Worker: worker, Replacement: true, Err: io.EOF})
	}
	evidence.PrimingAttempts = append(evidence.PrimingAttempts,
		detectPrimingAttempt{Connection: 1, Err: io.ErrUnexpectedEOF})
	evidence.ComparisonFailures = []detectComparisonFailure{
		{Connection: 1, Participations: 2, Err: io.EOF},
		{Connection: 1, Participations: 3, Err: io.EOF},
	}
	groups := detectTextFailureGroups(evidence)
	if len(groups) != 5 {
		t.Fatalf("got %d groups", len(groups))
	}
	if groups[0].attempts != 384 || len(groups[0].identities) != 128 ||
		groups[1].attempts != 1 || len(groups[1].identities) != 1 ||
		groups[2].attempts != 256 || len(groups[2].identities) != 128 ||
		groups[3].attempts != 128 || len(groups[3].identities) != 128 ||
		groups[4].attempts != 2 || len(groups[4].identities) != 1 || groups[4].participations != 5 {
		t.Fatalf("incorrect groups: %+v", groups)
	}
	slices.Reverse(evidence.PrimingAttempts)
	slices.Reverse(evidence.AcquisitionFailures)
	slices.Reverse(evidence.ComparisonFailures)
	if !reflect.DeepEqual(groups, detectTextFailureGroups(evidence)) {
		t.Fatal("group projection depends on recorded order")
	}
	var report bytes.Buffer
	reportDetectFailureGroups(&report, detectRunResult{Execution: evidence})
	if !strings.Contains(report.String(), "Cause: worker 0: EOF") {
		t.Fatal(report.String())
	}
}

func TestDetectExecutionSummaryFixtures(t *testing.T) {
	successful := detectExecutionEvidence{Recorded: true, ConnectionsObserved: 128}
	for worker := range 128 {
		for batch := range 3 {
			successful.PrimingAttempts = append(successful.PrimingAttempts,
				detectPrimingAttempt{
					Worker: worker, Connection: tth2.ConnectionID(worker + 1),
					Attempt: batch + 1, RequestOperations: 1, Complete: true,
				})
		}
	}
	mixed := detectExecutionEvidence{Recorded: true, ConnectionsObserved: 6, ConnectionReplacements: 2}
	for connection := range 6 {
		for batch := range 3 {
			attempt := detectPrimingAttempt{
				Connection: tth2.ConnectionID(connection + 1),
				Worker:     connection % 4, Attempt: batch + 1, RequestOperations: 1,
				Complete: true, Replacement: connection >= 4,
			}
			if batch == 2 && connection < 2 {
				attempt.Complete = false
				attempt.Err = io.EOF
				if connection == 1 {
					attempt.Err = io.ErrUnexpectedEOF
				}
			}
			mixed.PrimingAttempts = append(mixed.PrimingAttempts, attempt)
		}
	}
	acquisition := detectExecutionEvidence{Recorded: true}
	interrupted := detectExecutionEvidence{Recorded: true, ConnectionsObserved: 128}
	for worker := range 128 {
		acquisition.AcquisitionFailures = append(acquisition.AcquisitionFailures,
			detectAcquisitionFailure{Worker: worker, Err: &net.OpError{Op: "dial", Net: "tcp", Err: io.EOF}})
		interrupted.PrimingAttempts = append(interrupted.PrimingAttempts,
			detectPrimingAttempt{
				Worker: worker, Connection: tth2.ConnectionID(worker + 1),
				Attempt: 1, RequestOperations: 1, Err: context.Canceled,
			})
		interrupted.ComparisonFailures = append(interrupted.ComparisonFailures,
			detectComparisonFailure{
				Worker: worker, Connection: tth2.ConnectionID(worker + 1),
				Participations: 2, Items: []detectEvidenceRef{
					{role: requestGroupCandidate, index: 0}, {role: requestGroupCandidate, index: 1},
				}, Err: context.Canceled,
			})
	}
	primingStop := interrupted
	primingStop.ComparisonFailures = nil
	comparisonStop := interrupted
	comparisonStop.PrimingAttempts = nil
	failed := interrupted
	failed.AcquisitionFailures = []detectAcquisitionFailure{
		{Worker: 0, Err: io.ErrUnexpectedEOF},
		{Worker: 1, Replacement: true, Err: context.DeadlineExceeded},
	}
	mixedStop := detectExecutionEvidence{
		Recorded: true, ConnectionsObserved: 3,
		PrimingAttempts: []detectPrimingAttempt{
			{Connection: 1, RequestOperations: 1, Err: io.EOF},
			{Connection: 2, RequestOperations: 1, Err: context.Canceled},
			{
				Connection: 3, RequestOperations: 1,
				Err: &tth2.StreamError{StreamID: 5, Code: http2.ErrCodeInternal},
			},
		},
	}
	tests := []struct {
		name       string
		evidence   detectExecutionEvidence
		completion runCompletion
		planned    int
	}{
		{"successful", successful, stoppedRun(), 128},
		{"mixed", mixed, stoppedRun(), 4},
		{"mixed-stopped", mixedStop, stoppedRun(), 3},
		{
			"replacement-only",
			detectExecutionEvidence{Recorded: true, ConnectionsObserved: 2, ConnectionReplacements: 1},
			stoppedRun(), 1,
		},
		{"acquisition-only", acquisition, failedRun(io.EOF, completionAcquisitionFailed), 128},
		{"interrupted", interrupted, stoppedRun(), 128},
		{"interrupted-priming", primingStop, stoppedRun(), 128},
		{"interrupted-comparison", comparisonStop, stoppedRun(), 128},
		{"failed-cancellation-fallout", failed, failedRun(io.ErrUnexpectedEOF, completionAcquisitionFailed), 128},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := detectRunResult{
				Execution: test.evidence, Completion: test.completion,
				Unavailable: true, Ranking: ranking.OutlierResult{Items: make([]ranking.ItemStat, 2)},
				Requests: []detectRequestEvidence{{Request: 0}, {Request: 1}},
			}
			// Successful priming can accompany an operationally complete campaign.
			if test.name == "successful" {
				result.Completion = completedRun()
				result.Unavailable = false
				result.Ranking.Outcome = ranking.OutlierInconclusive
				result.Ranking.Detail = ranking.OutlierDiagnostics{Items: make([]ranking.OutlierItemStat, 2)}
			}
			execution, err := result.executionAccounting(-1, test.planned)
			if err != nil {
				t.Fatal(err)
			}
			encode := func() []byte {
				var out bytes.Buffer
				reporter := newJSONDetectReporter(&out, 2)
				// Exercise the terminal projection; stream conformance has separate coverage.
				reporter.stream.sequence = structuredSequence{started: true, count: 2, next: 2}
				reporter.candidates = []int{0, 1}
				reporter.plannedConnections = test.planned
				if err := reporter.Result(result); err != nil {
					t.Fatal(err)
				}
				return out.Bytes()
			}
			before := encode()
			var out bytes.Buffer
			reportDetectExecution(&out, result, detectReportContext{ComparisonsMax: -1, ConnectionLimit: test.planned})
			if !bytes.Equal(before, encode()) {
				t.Fatal("text rendering changed JSON evidence")
			}
			operations := execution.RequestOperations
			accounting := map[string]string{
				"Measured request operations: ": fmt.Sprintf(
					"%d attempted; %d rank-complete; %d retained.",
					operations.Attempted, operations.RankComplete, operations.Retained),
				"Connections: ": fmt.Sprintf("%d planned; %d observed; %d replacements.",
					execution.Connections.Planned, execution.Connections.Observed,
					execution.Connections.Replacements),
			}
			if execution.Priming.Batches.Attempted > 0 {
				accounting["Priming batches: "] = fmt.Sprintf(
					"%d attempted; %d rank-complete; %d incomplete.",
					execution.Priming.Batches.Attempted, execution.Priming.Batches.RankComplete,
					execution.Priming.Batches.Incomplete)
				accounting["Priming request operations: "] = fmt.Sprintf(
					"%d attempted; %d rank-complete; excluded from inference.",
					execution.Priming.RequestOperations.Attempted,
					execution.Priming.RequestOperations.RankComplete)
			}
			for prefix, detail := range accounting {
				var want bytes.Buffer
				writeWrappedASCII(&want, prefix, detail)
				if !strings.Contains(out.String(), want.String()) {
					t.Fatalf("report disagrees with accounting owner for %s: %s", prefix, out.String())
				}
			}
			assertGoldenReportPortable(t, out.String())
			path := filepath.Join("testdata", "reports", "detect", "execution-"+test.name+".txt")
			if *updateReportGoldens {
				if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			//nolint:gosec // The fixture table supplies fixed report names.
			golden, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(golden, out.Bytes()) {
				t.Fatalf("report differs: %s", out.String())
			}
		})
	}
}

func TestDetectFailureRepresentativeDisplay(t *testing.T) {
	message := "recorded\n" + strings.Repeat("complete diagnostic ", 20) + "\x1b"
	result := detectRunResult{Execution: detectExecutionEvidence{
		PrimingAttempts: []detectPrimingAttempt{{Connection: 0, Err: errors.New(message)}},
	}}
	groups := detectTextFailureGroups(result.Execution)
	if len(groups) != 1 || groups[0].attempts != 1 || len(groups[0].identities) != 0 {
		t.Fatalf("unrecorded connection affected population: %+v", groups)
	}
	var out, want bytes.Buffer
	reportDetectFailureGroups(&out, result)
	writeWrappedASCII(&want, "    Cause: ", curlblocks.DisplayText(message))
	if !strings.Contains(out.String(), want.String()) {
		t.Fatal("representative diagnostic was not preserved through display rendering")
	}
	assertGoldenReportPortable(t, out.String())
}
