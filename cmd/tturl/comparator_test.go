package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

type scriptedLeaseSend struct {
	batch  *tth2.BatchResult
	err    error
	active bool
}

type scriptedLease struct {
	id       tth2.ConnectionID
	active   bool
	sends    []scriptedLeaseSend
	widths   []int
	requests [][]*http.Request
	discard  bool
}

func (l *scriptedLease) ID() tth2.ConnectionID { return l.id }
func (l *scriptedLease) Active() bool          { return l.active }

func (l *scriptedLease) Discard() {
	l.active = false
	l.discard = true
}

func (l *scriptedLease) SendBatch(
	_ context.Context, reqs []*http.Request, _ ...tth2.SendOption,
) (*tth2.BatchResult, error) {
	l.widths = append(l.widths, len(reqs))
	l.requests = append(l.requests, append([]*http.Request(nil), reqs...))
	if len(l.sends) == 0 {
		return nil, errors.New("unexpected send")
	}
	send := l.sends[0]
	l.sends = l.sends[1:]
	l.active = send.active
	return send.batch, send.err
}

func TestLeasedDetectComparatorResolvesBaselineControls(t *testing.T) {
	candidates := testRequests(t, "candidate-a", "candidate-b")
	baselineA, err := http.NewRequestWithContext(
		t.Context(), http.MethodPost, "https://example.test/baseline-a",
		strings.NewReader("reusable"))
	if err != nil {
		t.Fatal(err)
	}
	baselines := []baselineRequestTemplate{
		{req: baselineA, supply: finiteBaselineSupply(2)},
		{req: testRequests(t, "baseline-b")[0], supply: finiteBaselineSupply(1)},
	}
	lease := &scriptedLease{id: 7, active: true, sends: []scriptedLeaseSend{
		{batch: completeBatch(7, 4), active: true},
	}}
	evidence := newDetectEvidenceCollector(2, 2)
	comparator := &leasedDetectComparator{
		origin: "example.test:443", reqs: candidates, baselines: baselines,
		width: 4, evidence: evidence,
		acquire: func(context.Context, string) (connectionLease, error) {
			return lease, nil
		},
	}
	comparator.initWorkers(1)
	t.Cleanup(comparator.Close)

	items := []ranking.ItemID{
		ranking.Control(ranking.Baseline, 0),
		1,
		ranking.Control(ranking.Baseline, 2),
		ranking.Control(ranking.Baseline, 1),
	}
	if _, err := comparator.compare(t.Context(), items); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(lease.requests[0]))
	for i, req := range lease.requests[0] {
		paths[i] = req.URL.Path
	}
	wantPaths := []string{
		"/baseline-a", "/candidate-b", "/baseline-a", "/baseline-b",
	}
	if !slices.Equal(paths, wantPaths) {
		t.Errorf("request paths = %v, want %v", paths, wantPaths)
	}
	for _, position := range []int{0, 2} {
		body, err := lease.requests[0][position].GetBody()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(body)
		if err != nil {
			t.Fatal(err)
		}
		if err := body.Close(); err != nil {
			t.Fatal(err)
		}
		if string(got) != "reusable" {
			t.Errorf("position %d body = %q", position, got)
		}
	}
	result := evidence.unavailable(errors.New("test"), failedRun(
		errors.New("test"), completionComparisonFailed))
	if result.Requests[1].Participations != 1 ||
		result.Baselines[0].Participations != 2 ||
		result.Baselines[1].Participations != 1 ||
		result.Ranking.Participations != 4 {
		t.Errorf("role evidence = %+v", result)
	}
}

func TestLeasedDetectComparatorRejectsUnknownItemsBeforeAcquisition(t *testing.T) {
	for _, items := range [][]ranking.ItemID{
		{0, 2},
		{0, ranking.Control(ranking.Positive, 0)},
		{0, ranking.Control(ranking.Baseline, 1)},
	} {
		acquired := false
		comparator := &leasedDetectComparator{
			reqs: testRequests(t, "candidate"),
			baselines: []baselineRequestTemplate{{
				req:    testRequests(t, "baseline")[0],
				supply: finiteBaselineSupply(1),
			}},
			width: 2, evidence: newDetectEvidenceCollector(1, 1),
			acquire: func(context.Context, string) (connectionLease, error) {
				acquired = true
				return nil, errors.New("unexpected acquisition")
			},
		}
		comparator.initWorkers(1)
		if _, err := comparator.compare(t.Context(), items); err == nil ||
			!errors.Is(err, errDetectComparatorItem) {
			t.Errorf("compare(%v) error = %v", items, err)
		}
		if acquired {
			t.Errorf("compare(%v) acquired a connection", items)
		}
	}
}

func completeBatch(id tth2.ConnectionID, width int) *tth2.BatchResult {
	batch := &tth2.BatchResult{
		Connection: id, Results: make([]tth2.Result, width),
		ArrivalOrder: make([]int, width),
	}
	for position := range width {
		batch.Results[position] = tth2.Result{
			Response:    &tth2.Response{StatusCode: http.StatusNoContent},
			ArrivalRank: position,
		}
		batch.ArrivalOrder[position] = position
	}
	return batch
}

func testRequests(t *testing.T, paths ...string) []*http.Request {
	t.Helper()
	requests := make([]*http.Request, len(paths))
	for index, path := range paths {
		request, err := http.NewRequestWithContext(
			t.Context(), http.MethodGet, "https://example.test/"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		requests[index] = request
	}
	return requests
}

func TestLeasedDetectComparatorPrimesReplacementBeforeEvidence(t *testing.T) {
	fatal := errors.New("connection lost")
	initial := &scriptedLease{id: 11, active: true, sends: []scriptedLeaseSend{
		{err: fatal, active: false},
	}}
	replacement := &scriptedLease{id: 12, active: true, sends: []scriptedLeaseSend{
		{batch: completeBatch(12, 1), active: true},
		{batch: completeBatch(12, 2), active: true},
	}}
	evidence := newDetectEvidenceCollector(2)
	comparator := &leasedDetectComparator{
		origin: "example.test:443", reqs: testRequests(t, "a", "b"),
		warmup: testRequests(t, "prime"), width: 2, trials: 1, evidence: evidence,
	}
	leases := []connectionLease{initial, replacement}
	comparator.acquire = func(context.Context, string) (connectionLease, error) {
		lease := leases[0]
		leases = leases[1:]
		return lease, nil
	}
	comparator.initWorkers(1)
	t.Cleanup(comparator.Close)

	ranks, err := comparator.compare(t.Context(), []ranking.ItemID{0, 1})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	if !slices.Equal(ranks, []int{0, 1}) {
		t.Fatalf("ranks = %v", ranks)
	}
	result := evidence.unavailable(errors.New("test"), failedRun(
		errors.New("test"), completionComparisonFailed))
	primingOperations, primingComplete, primingIncomplete := result.Execution.primingSummary()
	if result.Ranking.Comparisons != 1 ||
		result.Execution.ConnectionReplacements != 1 ||
		primingOperations != 2 || primingComplete != 1 || primingIncomplete != 1 ||
		result.Execution.ConnectionsObserved != 2 {
		t.Fatalf("evidence = %+v", result)
	}
	if got := replacement.widths; !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("replacement sends = %v, want priming then evidence", got)
	}
}

func TestLeasedDetectComparatorFailsClosedWithoutRetryingEvidence(t *testing.T) {
	evidenceErr := errors.New("response headers lost")
	lease := &scriptedLease{id: 21, active: true, sends: []scriptedLeaseSend{
		{batch: completeBatch(21, 1), active: true},
		{err: evidenceErr, active: false},
	}}
	evidence := newDetectEvidenceCollector(2)
	comparator := &leasedDetectComparator{
		origin: "example.test:443", reqs: testRequests(t, "a", "b"),
		warmup: testRequests(t, "prime"), width: 2, trials: 1, evidence: evidence,
		acquire: func(context.Context, string) (connectionLease, error) {
			return lease, nil
		},
	}
	comparator.initWorkers(1)
	t.Cleanup(comparator.Close)

	if _, err := comparator.compare(
		t.Context(), []ranking.ItemID{0, 1}); !errors.Is(err, evidenceErr) {
		t.Fatalf("compare error = %v, want %v", err, evidenceErr)
	}
	result := evidence.unavailable(evidenceErr, failedRun(
		evidenceErr, completionComparisonFailed))
	if result.Ranking.Comparisons != 0 ||
		len(result.Execution.ComparisonFailures) != 1 ||
		result.Execution.ComparisonFailures[0].Participations != 2 ||
		result.Execution.ConnectionReplacements != 0 || len(lease.widths) != 2 {
		t.Fatalf("evidence = %+v; sends = %v", result.Execution, lease.widths)
	}
}

func TestLeasedDetectComparatorPrimingFailurePolicy(t *testing.T) {
	t.Run("nonfatal incomplete consumes attempt", func(t *testing.T) {
		streamErr := errors.New("stream reset")
		lease := &scriptedLease{id: 31, active: true, sends: []scriptedLeaseSend{
			{err: streamErr, active: true},
			{batch: completeBatch(31, 2), active: true},
		}}
		evidence := newDetectEvidenceCollector(2)
		comparator := &leasedDetectComparator{
			origin: "example.test:443", reqs: testRequests(t, "a", "b"),
			warmup: testRequests(t, "prime"), width: 2, trials: 1, evidence: evidence,
			acquire: func(context.Context, string) (connectionLease, error) {
				return lease, nil
			},
		}
		comparator.initWorkers(1)
		t.Cleanup(comparator.Close)
		if _, err := comparator.compare(
			t.Context(), []ranking.ItemID{0, 1}); err != nil {
			t.Fatalf("compare: %v", err)
		}
		result := evidence.unavailable(streamErr, failedRun(
			streamErr, completionComparisonFailed))
		_, _, primingIncomplete := result.Execution.primingSummary()
		if primingIncomplete != 1 ||
			result.Execution.ConnectionReplacements != 0 ||
			result.Ranking.Comparisons != 1 {
			t.Fatalf("evidence = %+v", result.Execution)
		}
	})

	t.Run("replacement priming failure is terminal", func(t *testing.T) {
		fatal := errors.New("connection lost")
		leases := []connectionLease{
			&scriptedLease{id: 41, active: true, sends: []scriptedLeaseSend{
				{err: fatal, active: false},
			}},
			&scriptedLease{id: 42, active: true, sends: []scriptedLeaseSend{
				{err: fatal, active: false},
			}},
		}
		evidence := newDetectEvidenceCollector(2)
		comparator := &leasedDetectComparator{
			origin: "example.test:443", reqs: testRequests(t, "a", "b"),
			warmup: testRequests(t, "prime"), width: 2, trials: 1, evidence: evidence,
			acquire: func(context.Context, string) (connectionLease, error) {
				lease := leases[0]
				leases = leases[1:]
				return lease, nil
			},
		}
		comparator.initWorkers(1)
		t.Cleanup(comparator.Close)
		if _, err := comparator.compare(
			t.Context(), []ranking.ItemID{0, 1}); !errors.Is(err, fatal) {
			t.Fatalf("compare error = %v, want %v", err, fatal)
		}
		result := evidence.unavailable(fatal, failedRun(
			fatal, completionComparisonFailed))
		_, _, primingIncomplete := result.Execution.primingSummary()
		if result.Execution.ConnectionReplacements != 1 ||
			primingIncomplete != 2 ||
			result.Ranking.Comparisons != 0 {
			t.Fatalf("evidence = %+v", result.Execution)
		}
	})
}

func TestLeasedDetectComparatorDisabledPrimingSendsOnlyEvidence(t *testing.T) {
	lease := &scriptedLease{id: 51, active: true, sends: []scriptedLeaseSend{
		{batch: completeBatch(51, 2), active: true},
	}}
	evidence := newDetectEvidenceCollector(2)
	comparator := &leasedDetectComparator{
		origin: "example.test:443", reqs: testRequests(t, "a", "b"),
		width: 2, evidence: evidence,
		acquire: func(context.Context, string) (connectionLease, error) {
			return lease, nil
		},
	}
	comparator.initWorkers(1)
	t.Cleanup(comparator.Close)
	if _, err := comparator.compare(
		t.Context(), []ranking.ItemID{0, 1}); err != nil {
		t.Fatalf("compare: %v", err)
	}
	result := evidence.unavailable(errors.New("test"), failedRun(
		errors.New("test"), completionComparisonFailed))
	primingOperations, _, _ := result.Execution.primingSummary()
	if len(result.Execution.PrimingAttempts) != 0 ||
		primingOperations != 0 ||
		!slices.Equal(lease.widths, []int{2}) {
		t.Fatalf("evidence = %+v; sends = %v", result.Execution, lease.widths)
	}
}

func TestLeasedDetectComparatorRejectsWrongWidthBeforeAcquisition(t *testing.T) {
	acquired := false
	comparator := &leasedDetectComparator{
		origin: "example.test:443", reqs: testRequests(t, "a", "b"),
		width: 2, evidence: newDetectEvidenceCollector(2),
		acquire: func(context.Context, string) (connectionLease, error) {
			acquired = true
			return nil, errors.New("unexpected acquisition")
		},
	}
	comparator.initWorkers(1)
	if _, err := comparator.compare(
		t.Context(), []ranking.ItemID{0},
	); !errors.Is(err, errDetectComparatorWidth) {
		t.Fatalf("compare error = %v, want width invariant", err)
	}
	if acquired {
		t.Fatal("wrong-width comparison acquired a connection")
	}
	if got := detectComparisonFailureCode(errDetectComparatorWidth); got !=
		completionInternalError {
		t.Errorf("completion code = %q, want %q", got, completionInternalError)
	}
}

func TestLeasedDetectComparatorStopsOnLocalLimits(t *testing.T) {
	for _, test := range []struct {
		name    string
		priming bool
		err     error
	}{
		{
			name: "comparison body limit",
			err: &tth2.ResponseBodyLimitError{
				RequestIndex: 0, StreamID: 1, Limit: 4, BytesAccepted: 4,
			},
		},
		{
			name: "priming timeout", priming: true,
			err: &tth2.BatchTimeoutError{Limit: time.Millisecond},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			lease := &scriptedLease{
				id: 61, active: true,
				sends: []scriptedLeaseSend{{err: test.err, active: true}},
			}
			acquisitions := 0
			evidence := newDetectEvidenceCollector(2)
			comparator := &leasedDetectComparator{
				origin: "example.test:443", reqs: testRequests(t, "a", "b"),
				width: 2, evidence: evidence,
				acquire: func(context.Context, string) (connectionLease, error) {
					acquisitions++
					return lease, nil
				},
			}
			if test.priming {
				comparator.warmup = testRequests(t, "prime")
				comparator.trials = 1
			}
			comparator.initWorkers(1)
			if _, err := comparator.compare(
				t.Context(), []ranking.ItemID{0, 1},
			); !errors.Is(err, test.err) {
				t.Fatalf("compare error = %v, want %v", err, test.err)
			}
			if acquisitions != 1 || !lease.discard || len(lease.widths) != 1 {
				t.Errorf("acquisitions/discard/sends = %d/%t/%v",
					acquisitions, lease.discard, lease.widths)
			}
			if _, err := comparator.compare(
				t.Context(), []ranking.ItemID{0, 1},
			); !errors.Is(err, test.err) {
				t.Fatalf("second compare error = %v, want retained failure", err)
			}
			if acquisitions != 1 || len(lease.widths) != 1 {
				t.Errorf("local limit retried: acquisitions/sends = %d/%v",
					acquisitions, lease.widths)
			}
		})
	}
}

func TestLeasedDetectComparatorPrimesEveryActiveConnection(t *testing.T) {
	var (
		mu     sync.Mutex
		primed = map[string]bool{}
	)
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/prime" {
			primed[r.RemoteAddr] = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !primed[r.RemoteAddr] {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	transport := &tth2.Transport{TLSClientConfig: tlsCfg, MaxConnsPerHost: 2}
	t.Cleanup(transport.CloseIdleConnections)
	client := &tth2.Client{Transport: transport}
	makeRequests := func(paths ...string) []*http.Request {
		requests := make([]*http.Request, len(paths))
		for index, path := range paths {
			request, err := http.NewRequestWithContext(
				t.Context(), http.MethodGet, "https://"+addr+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			requests[index] = request
		}
		return requests
	}
	evidence := newDetectEvidenceCollector(2)
	comparator := newLeasedDetectComparator(
		client, addr, makeRequests("/a", "/b"), nil, makeRequests("/prime"),
		2, 1, 2, evidence, nil, nil)
	t.Cleanup(comparator.Close)

	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := comparator.compare(
				t.Context(), []ranking.ItemID{0, 1})
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("compare: %v", err)
		}
	}
	result := evidence.unavailable(errors.New("test"), failedRun(
		errors.New("test"), completionComparisonFailed))
	_, primingComplete, _ := result.Execution.primingSummary()
	if result.Ranking.Comparisons != 2 ||
		primingComplete != 2 ||
		result.Execution.ConnectionsObserved != 2 {
		t.Fatalf("evidence = %+v", result.Execution)
	}
	for _, request := range result.Requests {
		if len(request.StatusCounts) != 1 ||
			request.StatusCounts[0].Status != http.StatusNoContent {
			t.Fatalf("request evidence = %+v", result.Requests)
		}
	}
}

func TestLeasedDetectComparatorSendsBaselineOperations(t *testing.T) {
	counts := map[string]int{}
	var mu sync.Mutex
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		mu.Lock()
		counts[r.URL.Path]++
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	transport := &tth2.Transport{TLSClientConfig: tlsCfg}
	t.Cleanup(transport.CloseIdleConnections)
	client := &tth2.Client{Transport: transport}
	makeRequest := func(path string) *http.Request {
		req, err := http.NewRequestWithContext(
			t.Context(), http.MethodGet, "https://"+addr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	evidence := newDetectEvidenceCollector(1, 1)
	comparator := newLeasedDetectComparator(
		client, addr, []*http.Request{makeRequest("/candidate")},
		[]baselineRequestTemplate{{
			req: makeRequest("/baseline"), supply: unlimitedBaselineSupply(),
		}}, nil, 2, 0, 1, evidence, nil, nil)
	t.Cleanup(comparator.Close)

	for control := range 2 {
		if _, err := comparator.compare(t.Context(), []ranking.ItemID{
			0, ranking.Control(ranking.Baseline, control),
		}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := evidence.result(ranking.OutlierResult{
		Index: -1,
		Cost:  ranking.Cost{Comparisons: 2, Participations: 4},
		Items: []ranking.ItemStat{{Samples: 2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if counts["/candidate"] != 2 || counts["/baseline"] != 2 ||
		result.Requests[0].Participations != 2 ||
		result.Baselines[0].Participations != 2 {
		t.Errorf("request counts = %v; evidence = %+v", counts, result)
	}
}
