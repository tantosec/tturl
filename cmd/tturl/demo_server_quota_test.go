package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

func TestDemoServerQuotaRoutes(t *testing.T) {
	t.Parallel()
	mux := newDemoServerMux(demoServerEffectiveConfig{
		settings: defaultDemoServerConfig(),
	})

	created := serveDemoRequest(mux, http.MethodPost, "/quotas")
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /quotas status = %d, want 201", created.Code)
	}
	if got := created.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("POST /quotas Content-Type = %q", got)
	}
	var creation struct {
		Result demoQuotaCreateResult `json:"result"`
		Error  *demoQuotaError       `json:"error"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &creation); err != nil {
		t.Fatalf("decode POST /quotas: %v", err)
	}
	if creation.Error != nil || creation.Result.Limit != demoQuotaLimit {
		t.Errorf("POST /quotas = %+v", creation)
	}
	raw, err := base64.RawURLEncoding.DecodeString(creation.Result.Token)
	if err != nil || len(raw) != demoQuotaTokenBytes ||
		len(creation.Result.Token) != 12 {
		t.Fatalf("quota token = %q, decoded %d bytes, error %v",
			creation.Result.Token, len(raw), err)
	}

	path := "/quotas/" + creation.Result.Token
	initial := serveDemoRequest(mux, http.MethodGet, path)
	if got, want := initial.Body.String(),
		"{\"result\":{\"limit\":1,\"accepted\":0,\"overrun\":0},"+
			"\"error\":null}\n"; got != want {
		t.Errorf("initial GET quota = %q, want %q", got, want)
	}

	used := serveDemoRequest(mux, http.MethodPost, path+"/use")
	if used.Code != http.StatusOK ||
		used.Body.String() != "{\"result\":{\"receipt\":1},\"error\":null}\n" {
		t.Errorf("first quota use = %d, %q", used.Code, used.Body.String())
	}
	exhausted := serveDemoRequest(mux, http.MethodPost, path+"/use")
	if exhausted.Code != http.StatusConflict || exhausted.Body.String() !=
		"{\"result\":null,\"error\":{"+
			"\"code\":\"quota_exhausted\","+
			"\"message\":\"quota is exhausted\"}}\n" {
		t.Errorf("second quota use = %d, %q",
			exhausted.Code, exhausted.Body.String())
	}
	status := serveDemoRequest(mux, http.MethodGet, path)
	if got, want := status.Body.String(),
		"{\"result\":{\"limit\":1,\"accepted\":1,\"overrun\":0},"+
			"\"error\":null}\n"; got != want {
		t.Errorf("used GET quota = %q, want %q", got, want)
	}

	missing := serveDemoRequest(mux, http.MethodGet, "/quotas/not-a-token")
	if missing.Code != http.StatusNotFound || missing.Body.String() !=
		"{\"result\":null,\"error\":{"+
			"\"code\":\"quota_not_found\","+
			"\"message\":\"quota not found\"}}\n" {
		t.Errorf("missing quota = %d, %q",
			missing.Code, missing.Body.String())
	}
}

func TestDemoQuotaSeparatedTransactionCanOverrun(t *testing.T) {
	t.Parallel()
	const width = 8
	quota := &demoQuota{}
	ready := make(chan struct{}, width)
	release := make(chan struct{})
	receipts := make(chan uint64, width)
	var workers sync.WaitGroup
	workers.Add(width)
	for range width {
		go func() {
			defer workers.Done()
			if !quota.available() {
				return
			}
			ready <- struct{}{}
			<-release
			receipts <- quota.accept()
		}()
	}
	for range width {
		<-ready
	}
	close(release)
	workers.Wait()
	close(receipts)

	seen := make(map[uint64]bool, width)
	for receipt := range receipts {
		seen[receipt] = true
	}
	for receipt := uint64(1); receipt <= width; receipt++ {
		if !seen[receipt] {
			t.Errorf("missing receipt %d from %v", receipt, seen)
		}
	}
	if got, want := quota.snapshot(), (demoQuotaStatusResult{
		Limit: demoQuotaLimit, Accepted: width, Overrun: width - 1,
	}); got != want {
		t.Errorf("quota status = %+v, want %+v", got, want)
	}
}

func TestDemoQuotaStoreLRU(t *testing.T) {
	t.Parallel()
	random := bytes.NewReader(bytes.Join([][]byte{
		bytes.Repeat([]byte{1}, demoQuotaTokenBytes),
		bytes.Repeat([]byte{2}, demoQuotaTokenBytes),
		bytes.Repeat([]byte{3}, demoQuotaTokenBytes),
	}, nil))
	store := newDemoQuotaStore(2, random)
	first, err := store.create()
	if err != nil {
		t.Fatalf("create first quota: %v", err)
	}
	second, err := store.create()
	if err != nil {
		t.Fatalf("create second quota: %v", err)
	}
	if _, ok := store.get(first); !ok {
		t.Fatal("get first quota failed")
	}
	third, err := store.create()
	if err != nil {
		t.Fatalf("create third quota: %v", err)
	}
	if _, ok := store.get(second); ok {
		t.Errorf("least-recently-used quota %q remains", second)
	}
	for _, token := range []string{first, third} {
		if _, ok := store.get(token); !ok {
			t.Errorf("retained quota %q is missing", token)
		}
	}
}

func TestDemoQuotaStoreTokenErrors(t *testing.T) {
	t.Parallel()
	store := newDemoQuotaStore(1,
		iotest.ErrReader(errors.New("random failed")))
	if _, err := store.create(); err == nil ||
		!strings.Contains(err.Error(), "random failed") {
		t.Errorf("create error = %v", err)
	}

	repeated := bytes.Repeat([]byte{7},
		demoQuotaTokenBytes*(demoQuotaTokenAttempts+1))
	store = newDemoQuotaStore(2, bytes.NewReader(repeated))
	if _, err := store.create(); err != nil {
		t.Fatalf("create collision seed: %v", err)
	}
	if _, err := store.create(); err == nil ||
		!strings.Contains(err.Error(), "unique quota token") {
		t.Errorf("collision exhaustion error = %v", err)
	}
}

func TestBenchmarkDemoFixedWork(t *testing.T) {
	t.Parallel()
	calls := 0
	measure := func(iterations uint64) time.Duration {
		calls++
		//nolint:gosec // calibration caps iterations below MaxInt64
		return time.Duration(iterations)
	}
	got, err := benchmarkDemoFixedWork(measure)
	if err != nil || got.iterations != 2_097_152 ||
		got.elapsed != 2_097_152*time.Nanosecond {
		t.Errorf("benchmark = %+v, %v", got, err)
	}
	if calls > 20 {
		t.Errorf("benchmark made %d measurements, want at most 20", calls)
	}

	if _, err := benchmarkDemoFixedWork(
		func(uint64) time.Duration { return 0 }); err == nil ||
		!strings.Contains(err.Error(), "non-positive") {
		t.Errorf("unmeasurable benchmark error = %v", err)
	}
}

func TestDemoFixedWorkBenchmarkIterationsFor(t *testing.T) {
	t.Parallel()
	benchmark := demoFixedWorkBenchmark{
		iterations: 3,
		elapsed:    2 * time.Nanosecond,
	}
	for _, test := range []struct {
		name     string
		target   time.Duration
		rounding demoWorkRounding
		want     uint64
	}{
		{name: "zero", target: 0, want: 0},
		{name: "floor", target: time.Nanosecond, want: 1},
		{
			name: "ceiling", target: time.Nanosecond,
			rounding: demoWorkRoundUp, want: 2,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := benchmark.iterationsFor(test.target, test.rounding)
			if err != nil || got != test.want {
				t.Errorf("iterationsFor(%s, %d) = %d, %v; want %d",
					test.target, test.rounding, got, err, test.want)
			}
		})
	}
	subIteration := demoFixedWorkBenchmark{
		iterations: 1,
		elapsed:    2 * time.Second,
	}
	for _, rounding := range []demoWorkRounding{
		demoWorkRoundDown,
		demoWorkRoundUp,
	} {
		got, err := subIteration.iterationsFor(time.Nanosecond, rounding)
		if err != nil || got != 1 {
			t.Errorf("sub-iteration calibration with mode %d = %d, %v; want 1",
				rounding, got, err)
		}
	}

	overflow := demoFixedWorkBenchmark{
		iterations: math.MaxUint64,
		elapsed:    time.Nanosecond,
	}
	if _, err := overflow.iterationsFor(
		2*time.Nanosecond, demoWorkRoundDown,
	); err == nil ||
		!strings.Contains(err.Error(), "more than") {
		t.Errorf("overflow error = %v", err)
	}
}

func TestResolveDemoServerConfigSharesBenchmark(t *testing.T) {
	t.Parallel()
	cfg := defaultDemoServerConfig()
	cfg.quotaWorkTarget = 250 * time.Microsecond
	calls := 0
	effective, err := resolveDemoServerConfig(cfg, func(iterations uint64) time.Duration {
		calls++
		//nolint:gosec // calibration iterations fit in time.Duration
		return time.Duration(iterations)
	})
	if err != nil {
		t.Fatalf("resolve demo server config: %v", err)
	}
	if effective.settings.quotaWorkTarget != 250*time.Microsecond ||
		effective.settings.timingWorkMax.maximum != time.Second ||
		effective.settings.timingWorkMax.unlimited ||
		effective.quotaWorkIterations != 250_000 ||
		effective.workMaxIterations != 1_000_000_000 || calls > 20 {
		t.Errorf("effective config = %+v after %d measurements", effective, calls)
	}
}

func TestResolveDemoServerConfigDisabledWorkSkipsBenchmark(t *testing.T) {
	t.Parallel()
	cfg := defaultDemoServerConfig()
	cfg.timingWorkMax = demoTimingWorkLimit{}
	cfg.quotaWorkTarget = 0
	calls := 0
	effective, err := resolveDemoServerConfig(cfg, func(uint64) time.Duration {
		calls++
		return time.Second
	})
	if err != nil || calls != 0 || effective.workMaxIterations != 0 ||
		effective.quotaWorkIterations != 0 {
		t.Errorf("disabled effective config = %+v, %v after %d calls",
			effective, err, calls)
	}
}

func TestResolveDemoServerConfigTimingWorkDoesNotControlQuotaWork(t *testing.T) {
	t.Parallel()
	cfg := defaultDemoServerConfig()
	cfg.timingWorkMax = demoTimingWorkLimit{}
	effective, err := resolveDemoServerConfig(
		cfg, func(iterations uint64) time.Duration {
			//nolint:gosec // calibration iterations fit in time.Duration
			return time.Duration(iterations)
		})
	if err != nil || effective.workMaxIterations != 0 ||
		effective.quotaWorkIterations != 25_000 {
		t.Errorf("disabled timing-work config = %+v, %v", effective, err)
	}
}

func TestResolveDemoServerConfigUnlimitedWorkSkipsItsCalibration(t *testing.T) {
	t.Parallel()
	cfg := defaultDemoServerConfig()
	cfg.timingWorkMax = demoTimingWorkLimit{unlimited: true}
	cfg.quotaWorkTarget = 0
	calls := 0
	effective, err := resolveDemoServerConfig(cfg, func(uint64) time.Duration {
		calls++
		return time.Second
	})
	if err != nil || calls != 0 || effective.workMaxIterations != 0 {
		t.Errorf("unlimited effective config = %+v, %v after %d calls",
			effective, err, calls)
	}
}

func TestNewDemoServerRejectsQuotaWorkMaximum(t *testing.T) {
	t.Parallel()
	cfg := defaultDemoServerConfig()
	cfg.quotaWorkTarget = time.Duration(demoQuotaWorkMaximum + 1)
	server, err := newDemoServerWithWorkMeasure(
		cfg, io.Discard, func(iterations uint64) time.Duration {
			//nolint:gosec // calibration iterations fit in time.Duration
			return time.Duration(iterations)
		})
	if server != nil || err == nil ||
		!strings.Contains(err.Error(), "for '--quota-work'") ||
		!strings.Contains(err.Error(), "16777216 iterations") {
		t.Errorf("newDemoServer overflow = %v, %v", server, err)
	}
}
