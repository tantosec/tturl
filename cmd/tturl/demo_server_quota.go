package main

import (
	"container/list"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/bits"
	"net/http"
	"sync"
	"time"
)

const (
	demoQuotaLimit                  uint64 = 1
	demoQuotaStoreSize                     = 4096
	demoQuotaTokenBytes                    = 9
	demoQuotaTokenAttempts                 = 16
	defaultDemoQuotaWork                   = 25 * time.Microsecond
	demoQuotaWorkMaximum            uint64 = 1 << 24
	demoWorkCalibrationFloor               = 2 * time.Millisecond
	demoWorkCalibrationInitial      uint64 = 1024
	demoWorkCalibrationProbeMaximum uint64 = 1 << 24
	demoWorkCalibrationSampleCount         = 5
)

type demoQuota struct {
	mu       sync.Mutex
	accepted uint64
}

// available and accept deliberately protect two halves of one transaction.
// The gap between them is the logic race exposed by the quota-use route.
func (q *demoQuota) available() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.accepted < demoQuotaLimit
}

func (q *demoQuota) accept() uint64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.accepted++
	return q.accepted
}

func (q *demoQuota) snapshot() demoQuotaStatusResult {
	q.mu.Lock()
	defer q.mu.Unlock()
	overrun := uint64(0)
	if q.accepted > demoQuotaLimit {
		overrun = q.accepted - demoQuotaLimit
	}
	return demoQuotaStatusResult{
		Limit:    demoQuotaLimit,
		Accepted: q.accepted,
		Overrun:  overrun,
	}
}

type demoQuotaEntry struct {
	token string
	quota *demoQuota
}

type demoQuotaStore struct {
	mu     sync.Mutex
	max    int
	random io.Reader
	byID   map[string]*list.Element
	recent list.List
}

func newDemoQuotaStore(maximum int, random io.Reader) *demoQuotaStore {
	return &demoQuotaStore{
		max:    maximum,
		random: random,
		byID:   make(map[string]*list.Element, maximum),
	}
}

// create generates outside the lock, then performs collision checking,
// eviction, and insertion as one atomic store operation.
func (s *demoQuotaStore) create() (string, error) {
	var raw [demoQuotaTokenBytes]byte
	for range demoQuotaTokenAttempts {
		if _, err := io.ReadFull(s.random, raw[:]); err != nil {
			return "", fmt.Errorf("generate quota token: %w", err)
		}
		token := base64.RawURLEncoding.EncodeToString(raw[:])

		s.mu.Lock()
		if _, exists := s.byID[token]; exists {
			s.mu.Unlock()
			continue
		}
		if s.recent.Len() == s.max {
			oldest := s.recent.Back()
			entry := oldest.Value.(demoQuotaEntry)
			delete(s.byID, entry.token)
			s.recent.Remove(oldest)
		}
		element := s.recent.PushFront(demoQuotaEntry{
			token: token,
			quota: &demoQuota{},
		})
		s.byID[token] = element
		s.mu.Unlock()
		return token, nil
	}
	return "", fmt.Errorf("generate unique quota token after %d attempts",
		demoQuotaTokenAttempts)
}

func (s *demoQuotaStore) get(token string) (*demoQuota, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	element, ok := s.byID[token]
	if !ok {
		return nil, false
	}
	s.recent.MoveToFront(element)
	return element.Value.(demoQuotaEntry).quota, true
}

type demoQuotaCreateResult struct {
	Token string `json:"token"`
	Limit uint64 `json:"limit"`
}

type demoQuotaStatusResult struct {
	Limit    uint64 `json:"limit"`
	Accepted uint64 `json:"accepted"`
	Overrun  uint64 `json:"overrun"`
}

type demoQuotaUseResult struct {
	Receipt uint64 `json:"receipt"`
}

type demoQuotaError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type demoQuotaEnvelope struct {
	Result any             `json:"result"`
	Error  *demoQuotaError `json:"error"`
}

func writeDemoQuotaJSON(
	w http.ResponseWriter,
	status int,
	result any,
	responseError *demoQuotaError,
) {
	payload, err := json.Marshal(demoQuotaEnvelope{
		Result: result,
		Error:  responseError,
	})
	if err != nil {
		http.Error(w, "could not encode JSON response",
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(payload, '\n'))
}

func (s *demoServerRoutes) createQuota(w http.ResponseWriter, _ *http.Request) {
	token, err := s.quotas.create()
	if err != nil {
		writeDemoQuotaJSON(w, http.StatusInternalServerError, nil,
			&demoQuotaError{
				Code:    "quota_creation_failed",
				Message: "could not create quota",
			})
		return
	}
	writeDemoQuotaJSON(w, http.StatusCreated, demoQuotaCreateResult{
		Token: token,
		Limit: demoQuotaLimit,
	}, nil)
}

func (s *demoServerRoutes) quotaStatus(w http.ResponseWriter, r *http.Request) {
	quota, ok := s.quotas.get(r.PathValue("token"))
	if !ok {
		writeDemoQuotaJSON(w, http.StatusNotFound, nil, &demoQuotaError{
			Code:    "quota_not_found",
			Message: "quota not found",
		})
		return
	}
	writeDemoQuotaJSON(w, http.StatusOK, quota.snapshot(), nil)
}

func (s *demoServerRoutes) useQuota(w http.ResponseWriter, r *http.Request) {
	quota, ok := s.quotas.get(r.PathValue("token"))
	if !ok {
		writeDemoQuotaJSON(w, http.StatusNotFound, nil, &demoQuotaError{
			Code:    "quota_not_found",
			Message: "quota not found",
		})
		return
	}
	if !quota.available() {
		writeDemoQuotaJSON(w, http.StatusConflict, nil, &demoQuotaError{
			Code:    "quota_exhausted",
			Message: "quota is exhausted",
		})
		return
	}

	demoFixedWork(s.quotaWorkIterations) //nolint:staticcheck // deliberate processing window
	receipt := quota.accept()
	writeDemoQuotaJSON(w, http.StatusOK,
		demoQuotaUseResult{Receipt: receipt}, nil)
}

// demoFixedWorkBenchmark records one representative sample from which all
// startup iteration counts are derived.
type demoFixedWorkBenchmark struct {
	iterations uint64
	elapsed    time.Duration
}

type demoWorkRounding uint8

const (
	demoWorkRoundDown demoWorkRounding = iota
	demoWorkRoundUp
)

func benchmarkDemoFixedWork(
	measure func(uint64) time.Duration,
) (demoFixedWorkBenchmark, error) {
	iterations := demoWorkCalibrationInitial
	for iterations < demoWorkCalibrationProbeMaximum &&
		measure(iterations) < demoWorkCalibrationFloor {
		iterations *= 2
	}

	samples := make([]time.Duration, demoWorkCalibrationSampleCount)
	for i := range samples {
		samples[i] = measure(iterations)
	}
	for i := 1; i < len(samples); i++ {
		for j := i; j > 0 && samples[j] < samples[j-1]; j-- {
			samples[j], samples[j-1] = samples[j-1], samples[j]
		}
	}
	elapsed := samples[len(samples)/2]
	if elapsed <= 0 {
		return demoFixedWorkBenchmark{}, fmt.Errorf(
			"fixed-work benchmark measured a non-positive duration")
	}
	return demoFixedWorkBenchmark{
		iterations: iterations,
		elapsed:    elapsed,
	}, nil
}

func (b demoFixedWorkBenchmark) iterationsFor(
	target time.Duration,
	rounding demoWorkRounding,
) (uint64, error) {
	if target < 0 {
		return 0, fmt.Errorf("target duration must be non-negative")
	}
	if target == 0 {
		return 0, nil
	}
	if b.iterations == 0 || b.elapsed <= 0 {
		return 0, fmt.Errorf("fixed-work benchmark is invalid")
	}
	if rounding != demoWorkRoundDown && rounding != demoWorkRoundUp {
		return 0, fmt.Errorf("fixed-work rounding mode is invalid")
	}
	targetNanos := uint64(target.Nanoseconds())     //nolint:gosec // positive above
	elapsedNanos := uint64(b.elapsed.Nanoseconds()) //nolint:gosec // positive above
	high, low := bits.Mul64(b.iterations, targetNanos)
	if high >= elapsedNanos {
		return 0, fmt.Errorf("target requires more than %d iterations",
			uint64(math.MaxUint64))
	}
	calibrated, remainder := bits.Div64(high, low, elapsedNanos)
	if rounding == demoWorkRoundUp && remainder != 0 {
		if calibrated == math.MaxUint64 {
			return 0, fmt.Errorf("target requires more than %d iterations",
				uint64(math.MaxUint64))
		}
		calibrated++
	}
	if calibrated == 0 {
		return 1, nil
	}
	return calibrated, nil
}

func resolveDemoServerConfig(
	cfg demoServerConfig,
	measure func(uint64) time.Duration,
) (demoServerEffectiveConfig, error) {
	effective := demoServerEffectiveConfig{settings: cfg}
	if err := cfg.timingWorkMax.validate(); err != nil {
		return demoServerEffectiveConfig{}, err
	}
	if cfg.quotaWorkTarget < 0 {
		return demoServerEffectiveConfig{}, fmt.Errorf(
			"'--quota-work' must be non-negative")
	}
	if !cfg.timingWorkMax.limited() && cfg.quotaWorkTarget == 0 {
		return effective, nil
	}
	benchmark, err := benchmarkDemoFixedWork(measure)
	if err != nil {
		return demoServerEffectiveConfig{}, fmt.Errorf(
			"calibrate fixed work: %w", err)
	}
	if cfg.timingWorkMax.limited() {
		// A resource ceiling rounds toward less work.
		effective.workMaxIterations, err = benchmark.iterationsFor(
			cfg.timingWorkMax.maximum, demoWorkRoundDown)
		if err != nil {
			return demoServerEffectiveConfig{}, fmt.Errorf(
				"calibrate /work limit for '--timing-work-max': %w", err)
		}
	}
	if cfg.quotaWorkTarget > 0 {
		// Inserted work rounds up so every positive target does some work.
		effective.quotaWorkIterations, err = benchmark.iterationsFor(
			cfg.quotaWorkTarget, demoWorkRoundUp)
		if err != nil {
			return demoServerEffectiveConfig{}, fmt.Errorf(
				"calibrate quota work for '--quota-work': %w", err)
		}
		if effective.quotaWorkIterations > demoQuotaWorkMaximum {
			return demoServerEffectiveConfig{}, fmt.Errorf(
				"calibrate quota work for '--quota-work': requested duration requires more than the maximum of %d iterations",
				demoQuotaWorkMaximum)
		}
	}
	return effective, nil
}

func measureDemoFixedWork(iterations uint64) time.Duration {
	start := time.Now()
	demoFixedWork(iterations) //nolint:staticcheck // calibration observes elapsed work
	return time.Since(start)
}
