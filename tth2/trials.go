package tth2

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sync/atomic"
	"time"
)

type trialsConfig struct {
	send       sendConfig
	warmup     int
	warmupReqs []*http.Request
	policy     ArrangementPolicy
	padding    []RequestPadding
	// maxConns and maxRPS are per-call limits that layer under the transport's
	// own: each narrows what MaxConnsPerHost and MaxRequestsPerSecondPerHost
	// already allow, and neither can widen it. 0 adds no per-call limit,
	// leaving the transport's in force alone.
	maxConns         int
	maxRPS           float64
	minBatchInterval time.Duration
	// maxTrials bounds a run to that many counted trials; 0 leaves it
	// unbounded.
	maxTrials int
	// progressEvery is the floor between progress reports and progressReport
	// the callback they go to; a nil callback disables reporting entirely.
	progressEvery  time.Duration
	progressReport func(TrialProgress)
}

func defaultTrialsConfig() trialsConfig {
	return trialsConfig{send: defaultSendConfig()}
}

// TrialsOption configures one [Client.StreamTrials] run. Every [SendOption] is
// also a TrialsOption.
type TrialsOption interface {
	applyTrials(*trialsConfig)
}

// RunTrialsOption configures one summary-only [Client.RunTrials] run. It is the
// subset of [TrialsOption] that cannot request individual response evidence.
// Response capture options are therefore accepted by [Client.StreamTrials] but
// not by Client.RunTrials.
type RunTrialsOption interface {
	TrialsOption
	runTrialsOption()
}

// WithMaxTrials bounds [Client.StreamTrials] to n measured trials, excluding
// warmups. The bound is enforced before dispatch. It may leave a rotation cycle
// partial. WithMaxTrials panics if n is less than one. [Client.RunTrials]
// supplies its bound as an argument and does not accept this option.
func WithMaxTrials(n int) maxTrialsOption {
	if n < 1 {
		panic("tth2: WithMaxTrials requires n >= 1")
	}
	return maxTrialsOption{n: n}
}

type maxTrialsOption struct{ n int }

func (o maxTrialsOption) applyTrials(c *trialsConfig) { c.maxTrials = o.n }

// WithWarmup runs n priming trials on each initial or replacement connection
// before measured trials use it. Warmups are yielded with [Trial.Warmup] set
// and excluded from summaries. Each worker starts measured trials after its own
// connection is primed; workers need not finish warming together. Values below
// one disable warmups.
func WithWarmup(n int) warmupOption { return warmupOption{n: n} }

type warmupOption struct{ n int }

func (o warmupOption) applyTrials(c *trialsConfig) { c.warmup = o.n }
func (warmupOption) runTrialsOption()              {}

// WithWarmupRequests supplies a distinct batch for the priming trials selected
// by [WithWarmup]. The requests run in their given order on every acquired
// connection, including replacements, and [WithPadding] does not apply to them.
// Without this option, priming uses the measured batch and its arrangement.
//
// The batch must have reusable bodies, target the measured batch's origin, and
// contain at least one request. The returned option owns a copy of the slice;
// the requests themselves are shared under the same immutability contract as
// the measured requests.
func WithWarmupRequests(reqs ...*http.Request) warmupRequestsOption {
	if len(reqs) == 0 {
		panic("tth2: WithWarmupRequests requires at least 1 request")
	}
	return warmupRequestsOption{reqs: slices.Clone(reqs)}
}

type warmupRequestsOption struct{ reqs []*http.Request }

func (o warmupRequestsOption) applyTrials(c *trialsConfig) {
	c.warmupReqs = slices.Clone(o.reqs)
}

func (warmupRequestsOption) runTrialsOption() {}

// WithMaxConns caps the pooled connections carrying a run and therefore its
// concurrently active trials. It can narrow but not exceed
// [Transport.MaxConnsPerHost]. Values below one add no per-run cap. A run holds
// its connections until it ends.
func WithMaxConns(n int) maxConnsOption { return maxConnsOption{n: n} }

type maxConnsOption struct{ n int }

func (o maxConnsOption) applyTrials(c *trialsConfig) { c.maxConns = o.n }
func (maxConnsOption) runTrialsOption()              {}

// WithMaxRequestsPerSecond caps a run's average request rate across its
// workers. Values at or below zero add no per-run limit. The transport-wide
// limit also applies, so the lower rate wins. Pacing gates whole trial starts
// and does not space requests within a batch. Panics if r is non-finite.
func WithMaxRequestsPerSecond(r float64) maxRPSOption {
	if math.IsNaN(r) || math.IsInf(r, 0) {
		panic("tth2: WithMaxRequestsPerSecond requires a finite rate")
	}
	return maxRPSOption{r: r}
}

type maxRPSOption struct{ r float64 }

func (o maxRPSOption) applyTrials(c *trialsConfig) { c.maxRPS = o.r }
func (maxRPSOption) runTrialsOption()              {}

// WithMinBatchInterval sets a run-wide minimum interval between batch starts.
// It applies to warmups and measured trials across all workers. Values at or
// below zero add no per-run constraint. [Transport.MinBatchIntervalPerHost]
// also applies, so the longer interval wins. The first batch is immediately
// eligible, and idle time does not accumulate credit.
func WithMinBatchInterval(d time.Duration) minBatchIntervalOption {
	return minBatchIntervalOption{d: d}
}

type minBatchIntervalOption struct{ d time.Duration }

func (o minBatchIntervalOption) applyTrials(c *trialsConfig) {
	c.minBatchInterval = o.d
}

func (minBatchIntervalOption) runTrialsOption() {}

// TrialSummary tallies completed, full-rank trials. Warmups and failed trials
// are excluded from its rank matrices. The zero value is an empty summary.
// Returned matrices and slices belong to the summary and may be mutated;
// copying the struct aliases that storage. Concurrent reads are safe while no
// goroutine mutates it. Storage is O(n*n) for n requests, independently of the
// number of trials.
type TrialSummary struct {
	// Trials is the number of trials tallied here.
	Trials int

	// RankCounts[request][rank] counts the trials in which reqs[request]
	// finished at that rank. Each row sums to Trials.
	//
	// Keyed by request identity. Under rotation, exact request-by-position
	// balance holds when the tallied trials comprise complete cycles.
	RankCounts [][]int

	// PositionRankCounts[position][rank] counts the same trials keyed by stream
	// position — the position-th stream flushed. Under rotation each position
	// is visited by every request equally often over complete cycles. Each row
	// sums to Trials. Under [ArrangeNone], it equals RankCounts.
	PositionRankCounts [][]int

	// FailedTrials is the count of trials (post-warmup) that returned an error,
	// stream-level or connection-fatal. Failed trials contribute no ranks.
	// Context cancellation ends the run and is not counted here.
	FailedTrials int

	// ResetCounts[request] counts the trials in which the server reset that
	// request's stream. These trials are included in FailedTrials and excluded
	// from both rank matrices.
	ResetCounts []int
}

// errPadding marks a padding fault: applying the configured padding to a batch
// failed. Every later trial would repeat it, so it ends the run rather than
// counting as one lost trial.
var errPadding = errors.New("tth2: padding")

// newTrialSummary returns an empty summary with the per-request and
// per-position rank matrices, and the per-request reset counts, sized for n
// requests.
func newTrialSummary(n int) *TrialSummary {
	return &TrialSummary{
		RankCounts:         newRankMatrix(n),
		PositionRankCounts: newRankMatrix(n),
		ResetCounts:        make([]int, n),
	}
}

// cloneRankMatrix copies a rank-count matrix, so a snapshot taken from a live
// tally shares nothing with it.
func cloneRankMatrix(m [][]int) [][]int {
	out := make([][]int, len(m))
	for i, row := range m {
		out[i] = slices.Clone(row)
	}
	return out
}

// newRankMatrix allocates an n-by-n rank-count matrix: one row per request or
// position, one column per rank.
func newRankMatrix(n int) [][]int {
	m := make([][]int, n)
	for i := range m {
		m[i] = make([]int, n)
	}
	return m
}

// RunTrials sends reqs for the requested number of measured trials and
// returns their summary. trials must be positive. Warmups are additional.
//
// By default requests rotate through stream positions so each request occupies
// position 0 (the first-flushed position) equally often in every fully tallied
// cycle. See [WithArrangementPolicy] for the alternatives. Under the default
// rotation, trials must be a multiple of len(reqs). Failed trials can still
// leave the resulting tally unbalanced.
//
// Per-trial errors, including configured local response termination, increment
// FailedTrials and do not abort the run. Stream resets are also attributed in
// ResetCounts. Context cancellation and run-level failures return an error with
// the summary accumulated so far. RunTrials does not retain individual trials
// and therefore does not accept response capture options; use
// [Client.StreamTrials] when individual response evidence is needed.
func (c *Client) RunTrials(
	ctx context.Context,
	reqs []*http.Request,
	trials int,
	opts ...RunTrialsOption,
) (*TrialSummary, error) {
	if ctx == nil {
		panic("tth2: nil context")
	}
	n := len(reqs)
	if n == 0 {
		return &TrialSummary{}, errors.New("tth2: RunTrials requires at least 1 request")
	}
	if trials <= 0 {
		return newTrialSummary(n), errors.New("tth2: trials must be positive")
	}
	cfg := defaultTrialsConfig()
	for _, opt := range opts {
		opt.applyTrials(&cfg)
	}
	if cfg.policy.balancesExactly() && trials%n != 0 {
		return newTrialSummary(n), fmt.Errorf("tth2: trials (%d) must be a multiple of len(reqs) (%d) under "+
			"ArrangeRotate, which balances positions over whole cycles; pass "+
			"WithArrangementPolicy(ArrangeRandom) or (ArrangeNone) to bypass this requirement",
			trials, n)
	}

	bounded := make([]TrialsOption, 0, len(opts)+1)
	for _, opt := range opts {
		bounded = append(bounded, opt)
	}
	bounded = append(bounded, WithMaxTrials(trials))
	s := c.StreamTrials(ctx, reqs, bounded...)
	for range s.All() { //nolint:revive // drained for its tally; the trials are not kept
	}
	return s.Summary(), s.Err()
}

// trialWorker holds one pooled connection across a series of batch sends
// so per-connection warmth and position-bias characteristics stay stable across
// a worker's trials. The caller owns the connection's life through acquire and
// send: acquire reports a connection the caller must prime, and send reports a
// stale pooled connection whose death is recoverable. A trialWorker is used by
// a single goroutine.
type trialWorker struct {
	connOwner
	// ids issues connection ids, dense from 0 across the run, so a replacement
	// is distinguishable from the connection it replaced.
	ids *atomic.Int64
	// connID identifies the held connection; it is what a trial reports.
	connID int
}

// acquire takes a connection unless the worker already holds one, reporting
// whether it took a fresh one. A fresh connection must be primed because a
// connection replacing one that died is as cold as the run's first was. The
// error is a context cancellation or a dial failure.
func (w *trialWorker) acquire(ctx context.Context) (fresh bool, err error) {
	fresh, err = w.connOwner.acquire(ctx)
	if err != nil {
		return false, err
	}
	if fresh {
		w.connID = int(w.ids.Add(1) - 1)
	}
	return fresh, nil
}
