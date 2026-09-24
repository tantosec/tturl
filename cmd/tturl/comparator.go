package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

var errDetectComparatorWidth = errors.New(
	"detect comparator width invariant violated")

var errDetectComparatorItem = errors.New(
	"detect comparator item invariant violated")

type connectionLease interface {
	ID() tth2.ConnectionID
	Active() bool
	SendBatch(context.Context, []*http.Request, ...tth2.SendOption) (
		*tth2.BatchResult, error)
	Discard()
}

type acquireConnectionLease func(context.Context, string) (connectionLease, error)

type detectLeaseWorker struct {
	index       int
	lease       connectionLease
	primed      bool
	replacement bool
	failed      error
}

// leasedDetectComparator gives every concurrent adaptive worker exclusive
// ownership of one physical connection. A worker establishes and primes its
// connection lazily, so ready workers need not wait for slower peers.
type leasedDetectComparator struct {
	origin         string
	reqs           []*http.Request
	baselines      []baselineRequestTemplate
	warmup         []*http.Request
	width          int
	trials         int
	comparisonOpts []tth2.SendOption
	primingOpts    []tth2.SendOption
	acquire        acquireConnectionLease
	evidence       *detectEvidenceCollector

	workers chan *detectLeaseWorker
	all     []*detectLeaseWorker
	close   sync.Once
	failMu  sync.Mutex
	fatal   error
}

func newLeasedDetectComparator(
	client *tth2.Client,
	origin string,
	reqs []*http.Request,
	baselines []baselineRequestTemplate,
	warmup []*http.Request,
	width, warmupTrials, parallel int,
	evidence *detectEvidenceCollector,
	comparisonOpts, primingOpts []tth2.SendOption,
) *leasedDetectComparator {
	comparator := &leasedDetectComparator{
		origin: origin, reqs: reqs, baselines: baselines, warmup: warmup,
		width: width, trials: warmupTrials,
		comparisonOpts: append([]tth2.SendOption(nil), comparisonOpts...),
		primingOpts:    append([]tth2.SendOption(nil), primingOpts...),
		evidence:       evidence,
	}
	comparator.acquire = func(ctx context.Context, origin string) (
		connectionLease, error,
	) {
		return client.Acquire(ctx, origin)
	}
	comparator.initWorkers(parallel)
	return comparator
}

func (c *leasedDetectComparator) initWorkers(parallel int) {
	c.workers = make(chan *detectLeaseWorker, parallel)
	c.all = make([]*detectLeaseWorker, parallel)
	for index := range parallel {
		worker := &detectLeaseWorker{index: index}
		c.all[index] = worker
		c.workers <- worker
	}
}

func (c *leasedDetectComparator) compare(
	ctx context.Context, byPosition []ranking.ItemID,
) ([]int, error) {
	if len(byPosition) != c.width {
		return nil, fmt.Errorf(
			"%w: got %d positions, want %d",
			errDetectComparatorWidth, len(byPosition), c.width)
	}
	ordered := make([]*http.Request, len(byPosition))
	refs := make([]detectEvidenceRef, len(byPosition))
	for position, id := range byPosition {
		req, ref, err := c.resolveItem(id)
		if err != nil {
			return nil, fmt.Errorf("%w: position %d: %w",
				errDetectComparatorItem, position, err)
		}
		ordered[position] = req
		refs[position] = ref
	}
	var worker *detectLeaseWorker
	select {
	case worker = <-c.workers:
		defer func() { c.workers <- worker }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if worker.failed != nil {
		return nil, worker.failed
	}
	if err := c.fatalError(); err != nil {
		worker.failed = err
		return nil, err
	}
	if err := c.ensurePrimed(ctx, worker); err != nil {
		worker.failed = err
		return nil, err
	}
	batch, err := worker.lease.SendBatch(ctx, ordered, c.comparisonOpts...)
	if err == nil {
		if _, ok := batch.FullArrivalOrder(); !ok {
			err = fmt.Errorf("comparison returned an incomplete arrival order")
		}
	}
	if err != nil {
		if classifyBatchFailure(err) != nil {
			worker.lease.Discard()
			c.setFatal(err)
		}
		c.evidence.comparisonFailed(
			worker.index, worker.lease.ID(), refs, err)
		worker.failed = err
		return nil, err
	}
	c.evidence.observeResolved(refs, batch)
	ranks := make([]int, len(batch.Results))
	for position, result := range batch.Results {
		ranks[position] = result.ArrivalRank
	}
	return ranks, nil
}

func (c *leasedDetectComparator) resolveItem(
	id ranking.ItemID,
) (*http.Request, detectEvidenceRef, error) {
	if id.IsCandidate() {
		candidate := int(id)
		if candidate < 0 || candidate >= len(c.reqs) {
			return nil, detectEvidenceRef{}, fmt.Errorf(
				"candidate %d is out of range", candidate)
		}
		return c.reqs[candidate], detectEvidenceRef{
			role: requestGroupCandidate, index: candidate,
		}, nil
	}
	if id.ControlKind() != ranking.Baseline {
		return nil, detectEvidenceRef{}, fmt.Errorf(
			"control kind %d is unsupported", id.ControlKind())
	}
	template, err := baselineTemplateIndex(c.baselines, id.ControlIndex())
	if err != nil {
		return nil, detectEvidenceRef{}, err
	}
	return c.baselines[template].req, detectEvidenceRef{
		role: requestGroupBaseline, index: template,
	}, nil
}

func (c *leasedDetectComparator) ensurePrimed(
	ctx context.Context, worker *detectLeaseWorker,
) error {
	if worker.primed {
		return nil
	}
	if err := c.acquireWorker(ctx, worker); err != nil {
		return err
	}
	if c.trials == 0 {
		worker.primed = true
		return nil
	}
	for attempt := 1; attempt <= c.trials; attempt++ {
		batch, err := worker.lease.SendBatch(ctx, c.warmup, c.primingOpts...)
		if err == nil {
			if _, ok := batch.FullArrivalOrder(); !ok {
				err = fmt.Errorf("priming returned an incomplete arrival order")
			}
		}
		complete := err == nil
		c.evidence.primingAttempt(detectPrimingAttempt{
			Worker: worker.index, Attempt: attempt,
			Connection: worker.lease.ID(), Replacement: worker.replacement,
			RequestOperations: len(c.warmup), Complete: complete, Err: err,
		})
		if classifyBatchFailure(err) != nil {
			worker.lease.Discard()
			c.setFatal(err)
			return err
		}
		if fatal := c.fatalError(); fatal != nil {
			return fatal
		}
		if err == nil || worker.lease.Active() {
			continue
		}
		if worker.replacement {
			return fmt.Errorf("worker %d replacement priming: %w", worker.index, err)
		}
		worker.replacement = true
		if err := c.acquireWorker(ctx, worker); err != nil {
			return err
		}
		attempt = 0
	}
	worker.primed = true
	return nil
}

func (c *leasedDetectComparator) acquireWorker(
	ctx context.Context, worker *detectLeaseWorker,
) error {
	lease, err := c.acquire(ctx, c.origin)
	if err != nil {
		c.evidence.acquisitionFailed(
			worker.index, worker.replacement, err)
		return fmt.Errorf("worker %d acquire connection: %w", worker.index, err)
	}
	worker.lease = lease
	c.evidence.connectionAcquired(lease.ID(), worker.replacement)
	return nil
}

func (c *leasedDetectComparator) setFatal(err error) {
	c.failMu.Lock()
	defer c.failMu.Unlock()
	if c.fatal == nil {
		c.fatal = err
	}
}

func (c *leasedDetectComparator) fatalError() error {
	c.failMu.Lock()
	defer c.failMu.Unlock()
	return c.fatal
}

func (c *leasedDetectComparator) Close() {
	c.close.Do(func() {
		for _, worker := range c.all {
			if worker.lease != nil {
				worker.lease.Discard()
			}
		}
	})
}
