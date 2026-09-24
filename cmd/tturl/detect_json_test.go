package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/h2tls"
	"github.com/tantosec/tturl/internal/netdelay"
	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/stats"
	"github.com/tantosec/tturl/tth2"
)

func TestDetectDefaultDesignIsFiniteAndNarrow(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, stdout, _ := captureOutput(t)
	err := runDetect(ctx, []string{
		"--report", "json", "--direction", "late",
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}, out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runDetect error = %v, want cancellation", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	var run jsonDetectRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatal(err)
	}
	if run.Plan.Width.Requested != 2 || run.Plan.Width.Effective != 2 ||
		run.Plan.ComparisonLimit == nil || *run.Plan.ComparisonLimit != 50_000 {
		t.Errorf("default design = %+v, want width 2 and 50000 comparisons",
			run.Plan)
	}
	if len(run.Plan.Padding) != 2 ||
		run.Plan.Padding[0] != (structuredPadding{Position: 0}) ||
		run.Plan.Padding[1] != (structuredPadding{Position: 1}) {
		t.Errorf("default padding = %+v, want two zero entries",
			run.Plan.Padding)
	}
}

func TestDetectJSONCanonicalizesDirectionWithoutRewritingArgv(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, stdout, _ := captureOutput(t)
	args := []string{
		"--report", "json", "--direction=slow",
		"--block", "https://127.0.0.1:1/a",
		"--block", "https://127.0.0.1:1/b",
	}
	err := runDetect(ctx, args, out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runDetect error = %v, want cancellation", err)
	}
	var run jsonDetectRunRecord
	if err := json.Unmarshal(jsonLines(t, stdout.Bytes())[0], &run); err != nil {
		t.Fatal(err)
	}
	if run.Method.Direction != directionLate {
		t.Errorf("design direction = %q, want %q",
			run.Method.Direction, directionLate)
	}
	wantArgv := wireStrings(append([]string{"detect"}, args...))
	if !slices.Equal(run.Argv, wantArgv) {
		t.Errorf("argv = %q, want exact input %q", run.Argv, wantArgv)
	}
}

func TestJSONDetectReporterRequiresValidSolverMethod(t *testing.T) {
	var out bytes.Buffer
	reporter := newJSONDetectReporter(&out, 2)
	for _, method := range []ranking.OutlierMethodID{"", "bad\nmethod"} {
		if err := reporter.Run(DetectRunRecord{
			SolverMethod: method,
		}, nil); err == nil || !strings.Contains(err.Error(), "solver method") {
			t.Errorf("Run solver method %q error = %v", method, err)
		}
	}
	if out.Len() != 0 {
		t.Errorf("invalid method version wrote %d bytes", out.Len())
	}
}

func TestJSONDetectReporterRequiresCompletePadding(t *testing.T) {
	var out bytes.Buffer
	reporter := newJSONDetectReporter(&out, 3)
	err := reporter.Run(DetectRunRecord{
		SolverMethod: ranking.OutlierMethodID("example/1"), WidthEffective: 3,
		Padding: []PaddingRecord{{Position: 0}, {Position: 1}},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "2 positions, want effective width 3") {
		t.Fatalf("Run error = %v, want incomplete padding rejection", err)
	}
	if out.Len() != 0 {
		t.Errorf("invalid padding wrote %d bytes", out.Len())
	}
}

func TestDetectJSONReportsRollingStrategyPlan(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out, stdout, _ := captureOutput(t)
	err := runDetect(ctx, []string{
		"--report", "json", "--direction", "either",
		"--strategy", "rolling-baseline-reserved",
		"--comparisons-max", "20", "--connections", "7",
		"--block", "https://example.test/candidate",
		"--block", "https://example.test/control", "--baseline-only",
		"--baseline-supply", "unlimited",
	}, out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runDetect error = %v, want cancellation", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	var run jsonDetectRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatal(err)
	}
	if run.Method.Strategy.Requested != "rolling-baseline-reserved" ||
		run.Method.Strategy.Resolved != "rolling-baseline-reserved" ||
		run.Method.ID != string(
			(&ranking.RollingBaselineReservedOutlierSolver{}).MethodID()) ||
		run.Acquisition.InFlightComparisonLimit != 7 {
		t.Fatalf("run = %+v", run)
	}
	schema := compileStructuredSchema(
		t, detectSchema.filename)
	for index, record := range lines {
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(record))
		if err != nil {
			t.Fatalf("record %d JSON: %v", index, err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Fatalf("record %d does not conform: %v", index, err)
		}
	}
}

func TestDetectJSONPhysicalAccountingIncludesBaselines(t *testing.T) {
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path == "/baseline" {
			w.WriteHeader(http.StatusNotFound)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	out, stdout, _ := captureOutput(t)
	err := runDetect(t.Context(), []string{
		"--report", "json", "--insecure", "--direction", "either",
		"--comparisons-max", "1", "--width", "3",
		"--block", "https://" + addr + "/candidate",
		"--block", "https://" + addr + "/baseline", "--baseline-only",
		"--baseline-supply", "2",
	}, out)
	if err != nil {
		t.Fatal(err)
	}
	lines := jsonLines(t, stdout.Bytes())
	var result jsonDetectResultRecord
	if err := json.Unmarshal(lines[len(lines)-1], &result); err != nil {
		t.Fatal(err)
	}
	if result.Execution.Comparisons.RankComplete != 1 ||
		result.Execution.RequestOperations.Retained != 3 ||
		len(result.Detection.Requests) != 2 ||
		result.Detection.Requests[0].RequestID != 0 || result.Detection.Requests[0].Physical.Participations != 1 ||
		result.Detection.Requests[1].RequestID != 1 ||
		result.Detection.Requests[1].Physical.Participations != 2 ||
		!result.Detection.Requests[1].Non2xxObserved || !*result.Detection.StatusDistributionsDiffer {
		t.Errorf("result = %+v", result)
	}
}

func TestNormalisedDetectMean(t *testing.T) {
	for _, test := range []struct {
		name  string
		mean  *float64
		width int
		want  *float64
	}{
		{name: "unsampled", width: 8},
		{name: "width two", mean: new(0.75), width: 2, want: new(0.75)},
		{name: "width eight", mean: new(3.5), width: 8, want: new(0.5)},
		{name: "above eight", mean: new(5.5), width: 12, want: new(0.5)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := normalisedDetectMean(test.mean, test.width)
			if (got == nil) != (test.want == nil) ||
				got != nil && *got != *test.want {
				t.Errorf("normalisedDetectMean(%v, %d) = %v, want %v",
					test.mean, test.width, got, test.want)
			}
		})
	}
}

func TestDetectDefaultRisksResolveIndependently(t *testing.T) {
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, _ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	base := []string{
		"--report", "json", "--insecure", "--direction", "late",
		"--comparisons-max", "1", "--connections", "1",
		"--block", "https://" + addr + "/a",
		"--block", "https://" + addr + "/b",
	}
	for _, test := range []struct {
		name     string
		flags    []string
		positive float64
		negative float64
	}{
		{name: "defaults", positive: 0.01, negative: 0.05},
		{name: "positive only", flags: []string{
			"--false-positive-risk", "0.02",
		}, positive: 0.02, negative: 0.05},
		{name: "negative only", flags: []string{
			"--false-negative-risk", "0.03",
		}, positive: 0.01, negative: 0.03},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append(append([]string{}, test.flags...), base...)
			out, stdout, _ := captureOutput(t)
			if err := runDetect(t.Context(), args, out); err != nil {
				t.Fatalf("runDetect: %v", err)
			}
			lines := jsonLines(t, stdout.Bytes())
			var run jsonDetectRunRecord
			if err := json.Unmarshal(lines[0], &run); err != nil {
				t.Fatal(err)
			}
			if run.Method.FalsePositiveRisk != test.positive ||
				run.Method.FalseNegativeRisk != test.negative {
				t.Errorf("resolved risks = %g/%g, want %g/%g",
					run.Method.FalsePositiveRisk,
					run.Method.FalseNegativeRisk,
					test.positive, test.negative)
			}
			var result jsonDetectResultRecord
			if err := json.Unmarshal(lines[len(lines)-1], &result); err != nil {
				t.Fatal(err)
			}
			wantThreshold := stats.VilleUnionThreshold(2, test.positive)
			if result.Detection.CommitThresholdLogEvidence == nil ||
				*result.Detection.CommitThresholdLogEvidence != wantThreshold {
				t.Errorf("solver evidence = %+v, want threshold %g",
					result.Detection, wantThreshold)
			}
		})
	}
}

func TestDetectJSONIntegration(t *testing.T) {
	t.Parallel()
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, stderr := captureOutput(t)
	err := runDetect(t.Context(), []string{
		"--report", "json",
		"--insecure",
		"--direction", "late",
		"--false-positive-risk", "0.03",
		"--false-negative-risk", "0.07",
		"--padding", "query=1:-1",
		"--comparisons-max", "1",
		"--connections", "3",
		"--block", "https://" + addr + "/a",
		"--name", "first",
		"--block", "https://" + addr + "/b",
		"--name", "second",
	}, out)
	if err != nil {
		t.Fatalf("runDetect: %v", err)
	}

	lines := jsonLines(t, stdout.Bytes())
	if len(lines) != 4 {
		t.Fatalf("records = %d, want run + 2 requests + result:\n%s",
			len(lines), stdout.String())
	}
	for i, want := range []string{"run", "request", "request", "result"} {
		if got := recordKind(t, lines[i]); got != want {
			t.Errorf("record %d kind = %q, want %q", i, got, want)
		}
	}

	var run jsonDetectRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Schema != detectSchema.logical || run.Command != "detect" ||
		run.Method.ID != string((&ranking.PeerFirstOutlierSolver{}).MethodID()) ||
		run.RequestCount != 2 ||
		run.Method.FalsePositiveRisk != 0.03 ||
		run.Method.FalseNegativeRisk != 0.07 ||
		run.Method.Direction != "late" || run.Plan.Width.Requested != defaultWidth ||
		run.Plan.Width.Effective != 2 || run.Plan.ComparisonLimit == nil ||
		*run.Plan.ComparisonLimit != 1 ||
		run.Method.Edge != ranking.OutlierDefaultMinimumEdge ||
		len(run.Plan.Padding) != 2 ||
		run.Plan.Padding[0] != (structuredPadding{Position: 0, QueryFields: 1}) ||
		run.Plan.Padding[1] != (structuredPadding{Position: 1}) ||
		run.Acquisition.Delivery.ConnectionCeiling != 3 ||
		run.Acquisition.Delivery.ConnectionLimit != 1 ||
		run.Acquisition.Delivery.Selection.Mode != "fixed" {
		t.Errorf("run = %+v", run)
	}
	for i, want := range []string{"first", "second"} {
		var request jsonDetectRequestRecord
		if err := json.Unmarshal(lines[i+1], &request); err != nil {
			t.Fatalf("decode request %d: %v", i, err)
		}
		if request.RequestID != i || request.Label != want ||
			request.Role != "candidate" {
			t.Errorf("request %d = %+v", i, request)
		}
	}
	var result jsonDetectResultRecord
	if err := json.Unmarshal(lines[3], &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Detection.Finding.Status != "inconclusive" ||
		result.Execution.Comparisons.RankComplete != 1 ||
		result.Execution.RequestOperations.Retained != 2 ||
		len(result.Detection.Requests) != 2 {
		t.Errorf("result = %+v", result)
	}
	for _, item := range result.Detection.Requests {
		if item.Physical.MeanRank == nil ||
			item.Physical.NormalisedMeanRank == nil ||
			*item.Physical.MeanRank != *item.Physical.NormalisedMeanRank {
			t.Errorf("width-two item ranks = %+v", item)
		}
	}
	for _, item := range result.Detection.Requests {
		if len(item.StatusCounts) != 1 || item.StatusCounts[0] !=
			(structuredStatusCount{Status: http.StatusNoContent, Count: 1}) {
			t.Errorf("status counts = %+v", item.StatusCounts)
		}
	}
	if result.Execution.Connections.Observed != 1 ||
		*result.Detection.StatusDistributionsDiffer {
		t.Errorf("response evidence = %+v", result)
	}
	if !strings.Contains(stderr.String(), "Target:") ||
		!strings.Contains(stderr.String(), "Total: 2 requests.") {
		t.Errorf("JSON report omitted request facts on stderr:\n%s", stderr)
	}
	if strings.Contains(stderr.String(), diagnosticPrefix(diagnosticWarning)) {
		t.Errorf("ordinary detect plan emitted a warning:\n%s", stderr)
	}
}

// TestDetectPipelineFitUsesComparisonWidth verifies that delivery planning uses
// the number of requests in each adaptive comparison, rather than the larger
// candidate field. The request ceiling makes those two widths imply different
// intervals and therefore different path fits.
func TestDetectPipelineFitUsesComparisonWidth(t *testing.T) {
	t.Parallel()
	const oneWay = 60 * time.Millisecond
	wrapper, err := netdelay.NewListenerWrapper(netdelay.Config{
		Delay: oneWay,
		Seed:  1,
	})
	if err != nil {
		t.Fatalf("NewListenerWrapper: %v", err)
	}
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.WriteHeader(http.StatusNoContent)
	}), h2tls.WithListenerWrapper(wrapper))

	args := []string{
		"--report", "json",
		"--insecure",
		"--direction", "late",
		"--comparisons-max", "1",
		"--width", "2",
		"--connections-fit", "--batch-rate-max", "10/s",
		"--request-rate-max", "20/s",
	}
	for i := range 10 {
		args = append(args,
			"--block", "https://"+addr+"/"+strconv.Itoa(i),
			"--name", "request-"+strconv.Itoa(i))
	}
	out, stdout, _ := captureOutput(t)
	if err := runDetect(t.Context(), args, out); err != nil {
		t.Fatalf("runDetect: %v", err)
	}

	lines := jsonLines(t, stdout.Bytes())
	var run jsonDetectRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Plan.Width.Effective != 2 {
		t.Fatalf("effective width = %d, want 2", run.Plan.Width.Effective)
	}
	if want := wireStrings(append([]string{"detect"}, args...)); !slices.Equal(run.Argv, want) {
		t.Errorf("argv = %q, want exact input %q", run.Argv, want)
	}
	delivery := run.Acquisition.Delivery
	if delivery.RateLimits == nil ||
		delivery.RateLimits.EffectiveBatchIntervalNS !=
			int64(100*time.Millisecond) {
		t.Fatalf("effective interval = %v, want 100ms",
			delivery.RateLimits.EffectiveBatchIntervalNS)
	}
	if delivery.Selection.Mode != "path_fit" {
		t.Errorf("connection selection = %q, want %q",
			delivery.Selection.Mode, connectionPathPing)
	}
	if len(delivery.PathFit.RTTSamplesNS) != pathPingSamples ||
		delivery.PathFit == nil {
		t.Fatalf("PING samples/basis = %v/%v, want %d samples and a basis",
			delivery.PathFit.RTTSamplesNS, delivery.PathFit.RTTBasisNS,
			pathPingSamples)
	}
	basis := int64(0)
	for i, sample := range delivery.PathFit.RTTSamplesNS {
		if sample < int64(2*oneWay) {
			t.Errorf("PING sample %d = %s, want at least %s",
				i, time.Duration(sample), 2*oneWay)
		}
		basis = max(basis, sample)
	}
	if delivery.PathFit.RTTBasisNS != basis {
		t.Errorf("PING basis = %s, want maximum sample %s",
			time.Duration(delivery.PathFit.RTTBasisNS), time.Duration(basis))
	}
	interval := time.Duration(delivery.RateLimits.EffectiveBatchIntervalNS)
	wantFit, err := fittedConnectionCount(time.Duration(basis), interval)
	if err != nil {
		t.Fatalf("fittedConnectionCount: %v", err)
	}
	if wantFit < 2 {
		t.Fatalf("fit = %d from RTT %s and interval %s, want at least 2",
			wantFit, time.Duration(basis), interval)
	}
	if delivery.PathFit.FittedConnections != wantFit ||
		delivery.ConnectionCeiling != wantFit ||
		delivery.ConnectionLimit != 1 {
		t.Errorf("delivery = %+v, want fit/ceiling %d and finite-work limit 1",
			delivery, wantFit)
	}
}

func TestDetectDistinctConnectionPrimingIsSeparateFromEvidence(t *testing.T) {
	t.Parallel()
	var (
		mu       sync.Mutex
		primed   = map[string]bool{}
		measured int
	)
	addr, _ := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/prime" {
			primed[r.RemoteAddr] = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		measured++
		if !primed[r.RemoteAddr] {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	out, stdout, _ := captureOutput(t)
	err := runDetect(t.Context(), []string{
		"--report", "json", "--insecure", "--direction", "late",
		"--comparisons-max", "1", "--connections", "2", "--width", "2",
		"--warmup", "2",
		"--block", "https://" + addr + "/prime", "--name", "prime",
		"--warmup-only",
		"--block", "https://" + addr + "/a", "--name", "A",
		"--block", "https://" + addr + "/b", "--name", "B",
		"--block", "https://" + addr + "/c", "--name", "C",
	}, out)
	if err != nil {
		t.Fatalf("runDetect: %v", err)
	}
	lines := jsonLines(t, stdout.Bytes())
	if len(lines) != 6 {
		t.Fatalf("records = %d, want run + 4 requests + result:\n%s",
			len(lines), stdout.String())
	}
	var run jsonDetectRunRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatal(err)
	}
	if run.RequestCount != 4 || run.Plan.Width.Effective != 2 ||
		run.Plan.Warmup.TrialsPerConnection != 2 ||
		run.Plan.Warmup.BatchWidth != 1 ||
		run.Acquisition.Delivery.ConnectionLimit != 1 ||
		run.Plan.Priming.InitialRequestOperations != 2 ||
		run.Plan.Priming.MaximumRequestOperations != 4 {
		t.Fatalf("run = %+v", run)
	}
	for index, wantSet := range []string{"candidate", "candidate", "candidate", "warmup"} {
		var request jsonDetectRequestRecord
		if err := json.Unmarshal(lines[index+1], &request); err != nil {
			t.Fatal(err)
		}
		if request.Role != wantSet {
			t.Errorf("request %d set = %q, want %q", index, request.Role, wantSet)
		}
	}
	var result jsonDetectResultRecord
	if err := json.Unmarshal(lines[len(lines)-1], &result); err != nil {
		t.Fatal(err)
	}
	if len(result.PrimingAttempts) != 2 ||
		result.Execution.Priming.RequestOperations.Attempted != 2 ||
		result.Execution.Priming.Batches.RankComplete != 2 ||
		len(result.ComparisonFailures) != 0 ||
		result.Execution.Comparisons.RankComplete != 1 {
		t.Fatalf("result = %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if measured != 2 || len(primed) != 1 {
		t.Fatalf("measured = %d; primed connections = %d", measured, len(primed))
	}
}

func TestDetectJSONCatalogueSupplyAndRoles(t *testing.T) {
	for _, supply := range []string{"2", "unlimited"} {
		t.Run(supply, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			out, stdout, _ := captureOutput(t)
			err := runDetect(ctx, []string{
				"--report", "json", "--direction", "either", "--width", "3", "--warmup", "1",
				"--block", "https://example.test/candidate", "--name", "Candidate",
				"--block", "https://example.test/baseline", "--baseline-only", "--baseline-supply", supply,
				"--block", "https://example.test/prime", "--warmup-only",
			}, out)
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			records := jsonLines(t, stdout.Bytes())
			validateStructuredRecords(t, records)
			var run jsonDetectRunRecord
			if err := json.Unmarshal(records[0], &run); err != nil {
				t.Fatal(err)
			}
			if run.Method.Strategy.Requested != "auto" || run.Method.Strategy.Resolved != "baseline-reserved" ||
				run.Method.ID != string((&ranking.BaselineReservedOutlierSolver{}).MethodID()) {
				t.Fatalf("method = %+v", run.Method)
			}
			for id, role := range []string{"candidate", "baseline", "warmup"} {
				var request jsonDetectRequestRecord
				if err := json.Unmarshal(records[id+1], &request); err != nil {
					t.Fatal(err)
				}
				if request.RequestID != id || request.Role != role {
					t.Fatalf("request %d = %+v", id, request)
				}
				if role == "baseline" {
					if supply == "2" && (request.SupplyLimit == nil || *request.SupplyLimit != 2) {
						t.Fatalf("finite supply = %+v", request)
					}
					if supply == "unlimited" && request.SupplyLimit != nil {
						t.Fatalf("unlimited supply = %+v", request)
					}
				}
			}
		})
	}
}

func TestDetectJSONJoinsPhysicalAndInferentialEvidence(t *testing.T) {
	var output bytes.Buffer
	reporter := startDetectJSONCatalogue(t, &output)
	ranked := ranking.OutlierResult{
		Outcome: ranking.OutlierFound, Index: 1,
		Cost: ranking.Cost{Comparisons: 2, Participations: 4},
		OutlierGuarantee: ranking.OutlierGuarantee{
			ErrorDirection: ranking.OutlierErrorFalseFind, ErrorRiskLimit: 0.01,
			Direction: ranking.Late,
		},
		Items: []ranking.ItemStat{{Samples: 1, MeanRank: 0}, {Samples: 1, MeanRank: 1}},
		Detail: ranking.OutlierDiagnostics{
			Threshold: 4.5, MinimumEdge: 0.1,
			Items: []ranking.OutlierItemStat{
				{LogEvidence: -2, EliminationLogE: 3, Eliminated: true},
				{LogEvidence: 5, EliminationLogE: -4},
			},
		},
	}
	result := detectRunResult{
		Ranking: ranked, Completion: completedRun(),
		Requests: []detectRequestEvidence{
			{Request: 0, Participations: 2, RankSum: 1, StatusCounts: []detectStatusCount{{Status: 200, Count: 2}}},
			{Request: 1, Participations: 2, RankSum: 1, StatusCounts: []detectStatusCount{{Status: 404, Count: 2}}},
		},
	}
	result.Responses = reviewDetectResponses(result.Requests, result.Baselines)
	if err := reporter.Result(result); err != nil {
		t.Fatal(err)
	}
	record := lastJSONRecord(t, output.Bytes())
	assertResponseLimitRecordsConform(t, detectSchema.filename, [][]byte{record})
	var got jsonDetectResultRecord
	if err := json.Unmarshal(record, &got); err != nil {
		t.Fatal(err)
	}
	finding := got.Detection.Finding
	if finding.Status != "found" || finding.RequestID == nil || *finding.RequestID != 1 ||
		finding.Direction == nil || *finding.Direction != "late" || finding.Guarantee == nil ||
		finding.Guarantee.ErrorDirection != "false_positive" || finding.Guarantee.ErrorRiskLimit != 0.01 {
		t.Fatalf("finding = %+v", finding)
	}
	physical := 0
	for id, request := range got.Detection.Requests {
		if request.RequestID != id || request.Inference == nil || request.Physical.Participations != 2 ||
			request.Inference.Samples != 1 || request.Physical.MeanRank == nil || *request.Physical.MeanRank != 0.5 {
			t.Fatalf("request evidence = %+v", request)
		}
		physical += request.Physical.Participations
	}
	if physical != got.Execution.RequestOperations.Retained || !*got.Detection.StatusDistributionsDiffer ||
		!got.Detection.Requests[1].Non2xxObserved || !got.Detection.Requests[0].Inference.Eliminated ||
		got.Detection.Requests[1].Inference.PositiveLogEvidence != 5 ||
		*got.Detection.CommitThresholdLogEvidence != 4.5 {
		t.Fatalf("joined evidence = %+v", got)
	}
}

func TestDetectJSONRepresentsSolverEvidence(t *testing.T) {
	cmp := ranking.NewComparator(ranking.Profile{
		MaxWidth: 2,
		Controls: map[ranking.ControlKind]ranking.ControlSupply{
			ranking.Baseline: ranking.UnboundedControls(),
		},
	}, func(_ context.Context, positioned []ranking.ItemID) ([]int, error) {
		ranks := make([]int, len(positioned))
		for position, item := range positioned {
			if item.IsCandidate() {
				ranks[position] = 1
			}
		}
		return ranks, nil
	})
	ranked, err := (&ranking.BaselineReservedOutlierSolver{
		MaxComparisons: 40,
	}).Identify(t.Context(), ranking.OutlierRequest{
		M: 1, Direction: ranking.Late, MinimumEdge: 0.2,
		FalseFindRisk:     math.SmallestNonzeroFloat64,
		FalseNegativeRisk: math.SmallestNonzeroFloat64,
	}, cmp)
	if err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	reporter := newJSONDetectReporter(&output, 2)
	if err := reporter.Run(DetectRunRecord{
		Argv: []string{"detect"}, DialAddress: "example.test:443",
		RequestCount: 2, CandidateCount: 1, BaselineTemplateCount: 1,
		RequestedStrategy: detectStrategyBaselineReserved,
		ResolvedStrategy:  detectStrategyBaselineReserved,
		SolverMethod:      (&ranking.BaselineReservedOutlierSolver{}).MethodID(),
		ComparisonsMax:    40, Direction: ranking.Late,
		WidthRequested: 2, WidthEffective: 2,
		FalsePositiveRisk:      math.SmallestNonzeroFloat64,
		FalseNegativeRisk:      math.SmallestNonzeroFloat64,
		ConfiguredNegativeEdge: 0.2,
		Delivery: deliveryPlan{
			connectionSelection: connectionDefault,
			connectionCeiling:   1, connectionLimit: 1,
		},
		Padding: resolvedPadding(nil, 2), ConnectionLimit: 1,
		InFlightComparisonLimit: 1,
	}, []detectCapturedRequest{
		{
			RequestRecord: RequestRecord{
				Reference: runReference{ID: 0, Label: "Candidate"},
				URL:       "https://example.test/candidate",
			},
			Role: "candidate",
		},
		{
			RequestRecord: RequestRecord{
				Reference: runReference{ID: 1, Label: "Baseline"},
				URL:       "https://example.test/baseline",
			},
			Role: "baseline", BaselineSupply: baselineSupply{unlimited: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	result := detectRunResult{
		Ranking: ranked, Completion: completedRun(),
		Requests: []detectRequestEvidence{{
			Request: 0, Participations: 40, RankSum: 40,
			StatusCounts: []detectStatusCount{{Status: 200, Count: 40}},
		}},
		Baselines: []detectRequestEvidence{{
			Request: 0, Participations: 40,
			StatusCounts: []detectStatusCount{{Status: 200, Count: 40}},
		}},
	}
	result.Responses = reviewDetectResponses(result.Requests, result.Baselines)
	if err := reporter.Result(result); err != nil {
		t.Fatal(err)
	}

	records := jsonLines(t, output.Bytes())
	assertResponseLimitRecordsConform(t, detectSchema.filename, records)
	validateStructuredRecords(t, records)
	if len(records) != 4 || recordKind(t, records[len(records)-1]) != "result" {
		t.Fatalf("record sequence has no unique terminal result: %q", records)
	}
}

func TestDetectJSONRejectsNonFiniteInferenceWithContext(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ranking.OutlierDiagnostics)
		want   string
	}{
		{
			name: "threshold",
			change: func(detail *ranking.OutlierDiagnostics) {
				detail.Threshold = math.NaN()
			},
			want: "detect result commit threshold is non-finite (NaN)",
		},
		{
			name: "positive evidence",
			change: func(detail *ranking.OutlierDiagnostics) {
				detail.Items[1].LogEvidence = math.Inf(1)
			},
			want: "detect result candidate 1 positive log evidence is non-finite (+Inf)",
		},
		{
			name: "elimination evidence",
			change: func(detail *ranking.OutlierDiagnostics) {
				detail.Items[0].EliminationLogE = math.Inf(-1)
			},
			want: "detect result candidate 0 elimination log evidence is non-finite (-Inf)",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			reporter := startDetectJSONCatalogue(t, &output)
			detail := ranking.OutlierDiagnostics{
				Threshold: 4.5,
				Items:     make([]ranking.OutlierItemStat, 2),
			}
			test.change(&detail)
			err := reporter.Result(detectRunResult{
				Ranking: ranking.OutlierResult{
					Index: -1, Items: make([]ranking.ItemStat, 2),
					Detail: detail,
				},
				Requests:   []detectRequestEvidence{{Request: 0}, {Request: 1}},
				Completion: completedRun(),
			})
			if err == nil || err.Error() != test.want {
				t.Fatalf("Result error = %v, want %q", err, test.want)
			}
			records := jsonLines(t, output.Bytes())
			if len(records) != 3 || recordKind(t, records[len(records)-1]) != "request" {
				t.Fatalf("rejected result changed the complete prefix: %q", records)
			}
		})
	}
}

func TestDetectJSONUnavailablePreservesPartialEvidence(t *testing.T) {
	var output bytes.Buffer
	reporter := startDetectJSONCatalogue(t, &output)
	cause := &tth2.ResponseBodyLimitError{Limit: 4, BytesAccepted: 4}
	result := detectRunResult{
		Unavailable: true, Completion: failedRun(cause, completionComparisonFailed), Reason: cause,
		Ranking: ranking.OutlierResult{
			Index: -1, Cost: ranking.Cost{Comparisons: 1, Participations: 2},
			Items: []ranking.ItemStat{{Samples: 1, MeanRank: 0}, {Samples: 1, MeanRank: 1}},
		},
		Requests: []detectRequestEvidence{
			{Request: 0, Participations: 1, StatusCounts: []detectStatusCount{{Status: 200, Count: 1}}},
			{Request: 1, Participations: 1, RankSum: 1, StatusCounts: []detectStatusCount{{Status: 404, Count: 1}}},
		},
		Execution: detectExecutionEvidence{
			Recorded: true, ConnectionsObserved: 1,
			ComparisonFailures: []detectComparisonFailure{{
				Worker: 0, Connection: 1, Participations: 2, Err: cause,
				Items: []detectEvidenceRef{{role: requestGroupCandidate, index: 1}, {role: requestGroupCandidate, index: 0}},
			}},
		},
	}
	result.Responses = reviewDetectResponses(result.Requests, result.Baselines)
	if err := reporter.Result(result); err != nil {
		t.Fatal(err)
	}
	record := lastJSONRecord(t, output.Bytes())
	assertResponseLimitRecordsConform(t, detectSchema.filename, [][]byte{record})
	var got jsonDetectResultRecord
	if err := json.Unmarshal(record, &got); err != nil {
		t.Fatal(err)
	}
	if got.Detection.State != "unavailable" || len(got.Detection.Requests) != 2 ||
		got.Execution.RequestOperations.Retained != 2 || got.Execution.RequestOperations.Attempted != 4 ||
		got.Completion.Failure == nil || got.Completion.Failure.Code != "response_body_limit" {
		t.Fatalf("partial evidence = %+v", got)
	}
	failure := got.ComparisonFailures[0]
	if failure.Failure == nil || failure.Failure.Code != "response_body_limit" ||
		failure.RequestParticipations != 2 || failure.ConnectionID != 1 ||
		!slices.Equal(failure.Assignments, []jsonDetectAssignment{{RequestID: 1, Position: 0}, {RequestID: 0, Position: 1}}) {
		t.Fatalf("failure assignment = %+v", failure)
	}
}
