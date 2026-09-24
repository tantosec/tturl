package tth2

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// DispatchTimes are diagnostic wall-clock observations of a batch's outbound
// TLS application records. They are not response timings and must not be used
// to infer timing differences; infer those from response arrival order.
type DispatchTimes struct {
	// HeadersSentAt is immediately before dispatching the record containing
	// every request's HEADERS, or zero if that record was not flushed.
	HeadersSentAt time.Time

	// BodyFinalisedAt is immediately before dispatching the record containing
	// every body-carrying request's final DATA and END_STREAM. It is zero if no
	// request carried body bytes or that record was not flushed.
	BodyFinalisedAt time.Time
}

// Trial is one batch outcome and its place in a run's design.
type Trial struct {
	BatchResult

	// Dispatch contains diagnostic wall-clock observations of outbound records.
	// It may be populated even when Err is non-nil if dispatch succeeded before
	// response collection failed.
	Dispatch DispatchTimes

	// Arrangement records the layout this trial was sent in:
	// Arrangement[position] is the index of the request flushed at that
	// position, so Arrangement[0] is the first-flushed request. The slice is
	// independent and is set for every policy and warmup. A distinct batch from
	// [WithWarmupRequests] uses its own request indices and the identity
	// permutation.
	Arrangement []int

	// Index is the trial's zero-based place in the design, independent of start
	// or completion order. Indices are dense over attempted measured trials.
	// Warmups have index -1.
	Index int

	// Cycle and Shift locate this trial within its assigned block under
	// [ArrangeRotate]. Shift is in [0, len(Arrangement)), and Index equals
	// Cycle*len(Arrangement)+Shift. Both are -1 for other policies and warmups.
	// Failed trials retain their coordinates.
	Cycle, Shift int

	// Conn identifies the connection that carried the trial. IDs are dense
	// within a run and remain stable for a connection's lifetime. A replacement
	// receives a new ID, so Conn can exceed [TrialStream.Conns].
	Conn int

	// Warmup reports that this was a priming trial rather than a counted one.
	// Priming trials run on every acquired connection and are excluded from the
	// summary.
	Warmup bool

	// Err reports that this batch attempt failed. A failed trial is excluded
	// from the summary's rank matrices even if every final response header
	// arrived and all ranks are available.
	//
	// The embedded BatchResult is zero for a trial that produced nothing. A
	// response-phase stream reset is the exception: the partial batch contains
	// completed siblings and identifies reset requests through [Result.Reset].
	//
	// A measured trial failure is an observation about the target and does not
	// end iteration. A priming failure is also yielded here; when it exhausts
	// the worker's replacement allowance, it additionally ends the run through
	// [TrialStream.Err].
	Err error
}

// errStopped unwinds a worker whose consumer has stopped iterating. It never
// reaches a caller: the consumer that would read it is the one that left.
var errStopped = errors.New("tth2: trial stream stopped")

// StreamTrials prepares a stream that repeatedly sends reqs as a batch and
// yields trials in completion order. It is unbounded unless [WithMaxTrials] is
// set and otherwise stops when the consumer stops iterating or ctx is
// cancelled.
//
// Breaking iteration is a late stop: up to one trial per connection is already
// in flight. [TrialStream.All] drains those trials without yielding them and
// releases every held connection before returning.
//
// Requires requests to a single origin with reusable bodies (see [Client]). A
// configuration error yields no trials and is available from [TrialStream.Err].
// No goroutine starts and no connection is acquired until All is called.
//
// The stream retains a copy of the request slice and shares request pointers.
// Keep requests immutable until iteration ends. The caller owns every yielded
// trial; its storage is not reused by later trials. Response body and header
// capture options are available here because that evidence is carried by the
// yielded results.
//
// A trial runs entirely on one connection, preserving one shared release gate
// and one ordered response path; a batch is never split across two. A
// connection is held across trials. [WithMaxConns] limits how many connections
// the run holds.
//
// A connection that dies during measured work costs the yielded trial in
// flight. A stale pooled connection detected before a yielded trial outcome is
// transparently retried. A configured response-body limit or batch timeout also
// yields a failed trial with no batch evidence and discards the connection. A
// replacement for a connection-fatal trial is primed exactly as the original
// and carries a new [Trial.Conn] ID.
//
// A connection that dies during priming is replaced and primed from the start.
// If that replacement cannot reach measured dispatch, the run ends rather than
// using an unprimed connection or consuming the waiting measured trial. This
// includes retirement after completed priming. Measured dispatch ends the
// event, so a later independent connection loss has its own one-replacement
// allowance. Every priming attempt is still yielded.
// A failed priming batch that leaves its connection usable consumes the attempt
// and priming continues.
//
// Inability to acquire an initial or replacement connection also ends the run.
// Terminal acquisition and priming failures are reported by [TrialStream.Err]
// and yield no measured trial because no measured batch was dispatched. Trials
// completed before a terminal failure remain available in the stream and its
// summary. Acquisition remains bounded only by ctx and the transport dialler;
// the priming replacement limit bounds attempts, not elapsed time.
func (c *Client) StreamTrials(
	ctx context.Context,
	reqs []*http.Request,
	opts ...TrialsOption,
) *TrialStream {
	if ctx == nil {
		panic("tth2: nil context")
	}
	cfg := defaultTrialsConfig()
	for _, opt := range opts {
		opt.applyTrials(&cfg)
	}
	s := &TrialStream{
		client:  c,
		ctx:     ctx,
		reqs:    slices.Clone(reqs),
		cfg:     cfg,
		n:       len(reqs),
		summary: newTrialSummary(len(reqs)),
	}
	s.initErr = s.validate()
	if s.initErr != nil {
		return s
	}

	// The unit of distribution is a whole rotation cycle (n trials covering
	// every stream position once), so ordinary work sharing does not split a
	// cycle. Connection-fatal replacement can split the yielded records of a
	// damaged cycle; under any other policy each trial is its own unit.
	s.unit = cfg.policy.unit(s.n)
	s.conns = s.client.transport().maxConns()
	if cfg.maxConns > 0 {
		s.conns = min(s.conns, cfg.maxConns)
	}
	if cfg.maxTrials > 0 {
		blocks := (cfg.maxTrials-1)/s.unit + 1
		s.conns = min(s.conns, blocks)
	}
	return s
}

// validate resolves the run's origin and rejects a configuration no trial could
// honour. Everything it checks is a property of the inputs, so it is settled
// once, before any connection is taken.
func (s *TrialStream) validate() error {
	if err := validateSendConfig(s.cfg.send); err != nil {
		return err
	}
	if s.n == 0 {
		return errors.New("tth2: StreamTrials requires at least one request")
	}
	origin, err := BatchOrigin(s.reqs)
	if err != nil {
		return err
	}
	s.origin = origin
	if err := validatePadding(s.reqs, s.cfg.padding); err != nil {
		return err
	}
	if err := requireReusableBodies(s.reqs); err != nil {
		return err
	}
	if len(s.cfg.warmupReqs) == 0 {
		return nil
	}
	warmupOrigin, err := BatchOrigin(s.cfg.warmupReqs)
	if err != nil {
		return fmt.Errorf("tth2: warmup requests: %w", err)
	}
	if warmupOrigin != origin {
		return fmt.Errorf("tth2: warmup origin %q differs from measured origin %q",
			warmupOrigin, origin)
	}
	if err := requireReusableBodies(s.cfg.warmupReqs); err != nil {
		return fmt.Errorf("tth2: warmup requests: %w", err)
	}
	return nil
}

// TrialStream is a live run of trials. One consumer may call [TrialStream.All]
// once and then read [TrialStream.Err]. [TrialStream.Summary] and
// [TrialStream.Progress] are safe to call concurrently with All. Conns is
// immutable, and Err is read after All returns. Construct a stream with
// [Client.StreamTrials]; its zero value is not a prepared run. Do not copy one.
type TrialStream struct {
	client *Client
	ctx    context.Context
	reqs   []*http.Request
	cfg    trialsConfig
	n      int
	origin string
	unit   int
	conns  int

	initErr error // a configuration fault: the run yields nothing

	ranged bool          // All is single-use; guarded by the single-consumer contract
	out    chan *Trial   // completed trials, one slot per connection
	stop   chan struct{} // closed to wind the run down
	// acquireCtx stops workers waiting for a connection without cancelling
	// batches already in flight, which retain the caller's context.
	acquireCtx    context.Context
	cancelAcquire context.CancelFunc

	stopOnce sync.Once

	mu      sync.Mutex
	summary *TrialSummary
	fatal   error

	// pmu guards the progress snapshot and its meter, which the workers advance
	// as they dispatch and complete trials. It is separate from mu so a worker
	// recording a completion never waits on the consumer's tally.
	pmu      sync.Mutex
	progress TrialProgress
	meter    rateMeter

	// lastReport is when WithProgress last fired. Reports run on the consumer's
	// goroutine, so this needs no lock.
	lastReport time.Time
}

// All yields completed trials in completion order, which may differ from
// [Trial.Index].
//
// Breaking out of the range stops the run. A trial already in flight is read to
// completion and dropped. All returns after releasing every connection.
// Iterating a sequence returned by All more than once panics. Worker cleanup
// also runs if the consumer panics; in-flight trials are drained under ctx and
// any configured batch timeout before that panic continues.
func (s *TrialStream) All() iter.Seq[*Trial] {
	return func(yield func(*Trial) bool) {
		if s.ranged {
			panic("tth2: TrialStream.All called more than once")
		}
		s.ranged = true
		if s.initErr != nil {
			return
		}
		s.start()
		defer func() {
			s.stopRun()
			for range s.out { //nolint:revive // drain workers before releasing iteration
			}
			s.reportFinal()
		}()
		for t := range s.out {
			s.tally(t)
			s.reportIfDue()
			if !yield(t) {
				s.stopRun()
				return
			}
		}
	}
}

// Err returns the failure that ended the run, or nil if the run ended because
// the consumer stopped iterating. It is meaningful only once All has returned,
// except for a configuration fault, which is known before the run starts.
// Measured-trial failures are reported only on [Trial.Err]. A terminal priming
// failure is both a yielded warmup [Trial.Err] observation and, wrapped with
// its recovery context, the error that ends the stream here.
func (s *TrialStream) Err() error {
	if s.initErr != nil {
		return s.initErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fatal
}

// Summary returns an independent tally of measured trials yielded so far.
//
// Under rotation it has exact request-by-position balance only when its tallied
// trials comprise whole cycles. FailedTrials is not a cycle-boundary
// certificate: a zero count can accompany a partial cycle while the stream is
// live, after an early stop, or when [WithMaxTrials] truncates the final cycle.
//
// The snapshot is safe to keep and to obtain concurrently with All.
//
// The counts are exact whenever they are read; an inference derived from them
// may not be. Stopping a run because a peeked statistic crossed a threshold can
// inflate its error rate. Either fix the trial count in advance
// ([Client.RunTrials]) or use a method built for optional stopping. Peeking to
// display progress is fine; peeking to decide is not.
func (s *TrialStream) Summary() *TrialSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &TrialSummary{
		Trials:             s.summary.Trials,
		RankCounts:         cloneRankMatrix(s.summary.RankCounts),
		PositionRankCounts: cloneRankMatrix(s.summary.PositionRankCounts),
		FailedTrials:       s.summary.FailedTrials,
		ResetCounts:        slices.Clone(s.summary.ResetCounts),
	}
}

// Conns reports the planned maximum number of connections carrying this run. It
// is fixed before the first trial. Pool contention may keep fewer active, and
// replacement [Trial.Conn] IDs may exceed it.
//
// It bounds the unbalanced tail of a stopped run: under [ArrangeRotate] each
// connection may be mid-cycle when the stop lands, so a consumer truncating to
// a balanced prefix must discard up to Conns() partial cycles.
func (s *TrialStream) Conns() int { return s.conns }

// tally folds a yielded trial into the running summary. Warmup trials are
// outside it, and a failed trial is counted but contributes no ranks.
func (s *TrialStream) tally(t *Trial) {
	if t.Warmup {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, fullOrder := t.FullArrivalOrder()
	if t.Err != nil || !fullOrder {
		s.summary.FailedTrials++
		// A trial that lost a stream to a reset still says which request lost
		// it, which is the one thing a failure count cannot express. Results is
		// empty for a trial that produced nothing at all.
		for i, r := range t.Results {
			if r.Reset != nil {
				s.summary.ResetCounts[i]++
			}
		}
		return
	}
	s.summary.Trials++
	for i, r := range t.Results {
		s.summary.RankCounts[i][r.ArrivalRank]++
	}
	// Results are keyed by request, so the position tally reads each position's
	// request through the arrangement.
	for position, i := range t.Arrangement {
		s.summary.PositionRankCounts[position][t.Results[i].ArrivalRank]++
	}
}

// abort records the failure that ends the run and winds it down. The first one
// wins: later failures are consequences of it.
func (s *TrialStream) abort(err error) {
	s.mu.Lock()
	if s.fatal == nil {
		s.fatal = err
	}
	s.mu.Unlock()
	s.stopRun()
}

// stopRun cancels pending acquisitions and tells workers to finish any batch
// already in flight before stopping.
func (s *TrialStream) stopRun() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.cancelAcquire()
	})
}

// stopping reports whether the run is winding down.
func (s *TrialStream) stopping() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

// start launches the run: one worker per planned connection, plus the goroutine
// that closes the trial channel once they have all released theirs.
func (s *TrialStream) start() {
	s.stop = make(chan struct{})
	s.out = make(chan *Trial, s.conns)
	s.acquireCtx, s.cancelAcquire = context.WithCancel(s.ctx)
	// The meter and the report cadence both measure from the run's start, which
	// is here: a stream that is never ranged runs nothing and reports nothing.
	// The meter is seeded under its own lock, since a display may already be
	// polling.
	now := time.Now()
	s.pmu.Lock()
	s.meter = newRateMeter(now)
	s.pmu.Unlock()
	s.lastReport = now

	tr := s.client.transport()
	p := tr.pool(s.origin)
	dial := func() (*http2conn, error) {
		return tr.dial(s.acquireCtx, s.origin)
	}

	lease := p.leasePace(paceConfig{
		requestsPerSecond: s.cfg.maxRPS,
		minBatchInterval:  s.cfg.minBatchInterval,
	})

	// Workers draw whole blocks from one counter, and connections take ids from
	// another. Both are run-wide, which is what makes a block index a
	// coordinate in the design and a connection id unique across the run.
	var nextBlock, nextConnID atomic.Int64

	var wg sync.WaitGroup
	for range s.conns {
		wg.Go(func() { s.work(p, dial, lease, &nextBlock, &nextConnID) })
	}
	go func() {
		wg.Wait()
		s.cancelAcquire()
		lease.close()
		close(s.out)
	}()
}

// emit hands a completed trial to the consumer, reporting false once the run is
// winding down and the trial has nowhere to go.
func (s *TrialStream) emit(t *Trial) bool {
	select {
	case s.out <- t:
		return true
	case <-s.stop:
		return false
	}
}

// blockAvailable reports whether the block at idx is within the run's bound. An
// unbounded run has every block.
func (s *TrialStream) blockAvailable(idx int) bool {
	return s.cfg.maxTrials <= 0 || idx <= (s.cfg.maxTrials-1)/s.unit
}

// trialAvailable reports whether the trial at index is within the run's bound.
// It is what makes a bound exact: the last block of a bound that is not a whole
// number of cycles stops part-way, before anything is dispatched.
func (s *TrialStream) trialAvailable(index int) bool {
	return s.cfg.maxTrials <= 0 || index < s.cfg.maxTrials
}

// work runs one worker: it holds a connection, draws blocks of trials, and
// emits each outcome. It returns once the run is bounded out, stopped, or
// aborted, having released its connection.
func (s *TrialStream) work(
	p *originPool,
	dial func() (*http2conn, error),
	lease paceLease,
	nextBlock, nextConnID *atomic.Int64,
) {
	draw := func() int { return int(nextBlock.Add(1) - 1) }

	// Draw before priming: a worker that finds the run's blocks already claimed
	// has no trials to prime a connection for.
	blockIdx := draw()
	if !s.blockAvailable(blockIdx) {
		return
	}

	wk := trialWorker{
		connOwner: connOwner{pool: p, dial: dial},
		ids:       nextConnID,
	}
	defer wk.release()

	arr := newArranger(s.cfg.policy, s.n, newTrialRNG())
	// Priming with the measured batch draws from its own arranger. Taking the
	// counted sequence's next base would split a measured cycle across bases.
	warmArr := newArranger(s.cfg.policy, s.n, newTrialRNG())

	// send builds and sends one batch on the held connection, waiting on the
	// rate lease first. It reports stale for a pooled connection that died
	// before proving itself live, which is recoverable by acquiring again.
	send := func(
		reqs []*http.Request,
		arrangement []int,
		padding []RequestPadding,
	) (batch *BatchResult, dispatch DispatchTimes, stale bool, err error) {
		toSend, err := buildTrialBatch(reqs, arrangement, padding)
		if err != nil {
			return nil, DispatchTimes{}, false,
				fmt.Errorf("%w: %w", errPadding, err)
		}
		beforeFlush := lease.beforeFirstFlush(s.ctx, len(toSend))
		return wk.send(
			s.ctx, toSend, s.cfg.send,
			func() error {
				if beforeFlush != nil {
					if err := beforeFlush(); err != nil {
						return err
					}
				}
				s.dispatched(len(toSend))
				return nil
			},
		)
	}

	// noCoord marks a place a trial does not have, so cycle and shift resolve
	// to it under a policy with no balanced blocks.
	coords := func(block, shift int) (cycle, sh int) {
		if s.cfg.policy.balancesExactly() {
			return block, shift
		}
		return noCoord, noCoord
	}

	// newTrial assembles one trial's outcome. Results arrive keyed by position,
	// so they are re-keyed to request order here — the order the caller passed
	// them in, and the only order a consumer can interpret.
	newTrial := func(
		batch *BatchResult,
		dispatch DispatchTimes,
		arrangement []int,
		err error,
		index, cycle, shift int,
	) *Trial {
		t := &Trial{
			// The arranger rewrites its arrangement on the next trial, so this
			// copy is what makes a yielded trial safe to keep.
			Arrangement: slices.Clone(arrangement),
			Index:       index,
			Cycle:       cycle,
			Shift:       shift,
			Conn:        wk.connID,
			Warmup:      index == noCoord,
			Dispatch:    dispatch,
			Err:         err,
		}
		if batch != nil {
			t.BatchResult = scatterBatch(batch, arrangement)
		}
		return t
	}

	// warm primes a freshly acquired connection: the connection, its HPACK
	// table, and any server-side caches. Priming trials are yielded flagged and
	// their failures are not counted. A connection-fatal result asks sendTrial
	// to restart priming on a replacement; survivable failures merely consume
	// their configured attempt.
	warm := func() error {
		reqs := s.reqs
		padding := s.cfg.padding
		var arrangement []int
		if len(s.cfg.warmupReqs) > 0 {
			reqs = s.cfg.warmupReqs
			padding = nil
			arrangement = identity(len(reqs))
		} else {
			warmArr.startCycle()
		}
		for k := range s.cfg.warmup {
			if len(s.cfg.warmupReqs) == 0 {
				arrangement = warmArr.next(k % s.unit)
			}
			batch, dispatch, _, err := send(reqs, arrangement, padding)
			s.completed(true)
			if !s.emit(newTrial(
				batch, dispatch, arrangement, err, noCoord, noCoord, noCoord,
			)) {
				return errStopped
			}
			switch {
			case err == nil:
			case errors.Is(err, errPadding), s.ctx.Err() != nil:
				return err
			case !wk.held:
				return fmt.Errorf("tth2: connection died while priming: %w", err)
			}
		}
		return nil
	}

	// sendTrial runs one batch on a live, primed connection, taking and priming
	// one whenever the worker holds none — a connection that replaces one the
	// target killed is as cold as the first was, so it is primed on the same
	// terms. A pooled connection that dies before proving itself live is
	// recovered by taking a fresh one and sending again, at the cost of the
	// target having seen part of that batch. Priming may replace one dead
	// connection before the pending measured trial becomes terminally blocked.
	sendTrial := func(arrangement []int) (
		batch *BatchResult,
		dispatch DispatchTimes,
		terminal bool,
		err error,
	) {
		replacements := 0
		replace := func(err error, reason string) error {
			replacements++
			if replacements <= 1 {
				return nil
			}
			return fmt.Errorf("tth2: replacement connection %s: %w", reason, err)
		}
		for {
			fresh, err := wk.acquire(s.acquireCtx)
			if err != nil {
				return nil, DispatchTimes{}, true, err
			}
			if s.stopping() {
				return nil, DispatchTimes{}, false, errStopped
			}
			if fresh {
				if err := warm(); err != nil {
					switch {
					case errors.Is(err, errStopped):
						return nil, DispatchTimes{}, true, err
					case s.ctx.Err() != nil, wk.held:
						// Cancellation and configuration faults are terminal;
						// neither is repaired by changing connections.
						return nil, DispatchTimes{}, true, err
					}
					if terminalErr := replace(err, "could not be primed"); terminalErr != nil {
						return nil, DispatchTimes{}, true, terminalErr
					}
					continue
				}
			}
			batch, dispatch, stale, err := send(
				s.reqs, arrangement, s.cfg.padding,
			)
			switch {
			case err == nil:
				return batch, dispatch, false, err
			case stale:
				continue
			case dispatch.HeadersSentAt.IsZero() && !wk.held && s.ctx.Err() == nil:
				// HeadersSentAt is recorded only after the shared header record
				// flush succeeds. Until then the design position has not been
				// released, even though progress conservatively charges an
				// authorised but failed flush attempt.
				if terminalErr := replace(
					err, "retired before measured dispatch",
				); terminalErr != nil {
					return nil, DispatchTimes{}, true, terminalErr
				}
				continue
			default:
				return batch, dispatch, false, err
			}
		}
	}

	// One iteration per balanced block: under rotation a block is a cycle of
	// unit trials sharing one base permutation, and under any other policy it
	// is a single trial.
	for ; s.blockAvailable(blockIdx); blockIdx = draw() {
		arr.startCycle()
		for shift := range s.unit {
			index := blockIdx*s.unit + shift
			if !s.trialAvailable(index) || s.stopping() {
				return
			}
			if err := s.ctx.Err(); err != nil {
				s.abort(err)
				return
			}
			arrangement := arr.next(shift)
			batch, dispatch, terminal, err := sendTrial(arrangement)
			if terminal {
				if s.stopping() {
					return
				}
				if ctxErr := s.ctx.Err(); ctxErr != nil {
					s.abort(ctxErr)
				} else {
					s.abort(err)
				}
				return
			}
			if errors.Is(err, errStopped) {
				return
			}
			if errors.Is(err, errPeerStreamLimit) {
				s.abort(err)
				return
			}
			s.completed(false)
			switch {
			case errors.Is(err, errPadding):
				// A padding fault is a configuration bug every later trial
				// repeats.
				s.abort(err)
				return
			case err != nil && s.ctx.Err() != nil:
				s.abort(s.ctx.Err())
				return
			}
			cycle, sh := coords(blockIdx, shift)
			if !s.emit(newTrial(
				batch, dispatch, arrangement, err, index, cycle, sh,
			)) {
				return
			}
		}
	}
}

// buildTrialBatch lays reqs out by position and applies per-position padding.
// Padded batches contain fresh clones private to the caller; unpadded batches
// return a slice only the caller holds, and sending never mutates a request.
//
// The send path only reads each request (encodeHeaders skips rather than
// deletes, and GetBody hands out fresh readers over an immutable buffer), and a
// reusable body is a validated precondition, so the workers share the reqs
// slice with no cloning and no races.
func buildTrialBatch(
	reqs []*http.Request,
	arrangement []int,
	padding []RequestPadding,
) ([]*http.Request, error) {
	positioned := arrange(reqs, arrangement)
	if len(padding) == 0 {
		return positioned, nil
	}
	return applyPadding(positioned, padding)
}

// noCoord is the coordinate a trial reports for a place it does not have: a
// warmup trial sits outside the design, and no policy but [ArrangeRotate] has
// cycles.
const noCoord = -1
