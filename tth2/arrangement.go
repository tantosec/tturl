package tth2

import (
	"math/rand/v2"
	"net/http"
)

// ArrangementPolicy selects how trials assign requests to stream positions.
type ArrangementPolicy int

const (
	// ArrangeRotate is the default. Each cycle draws a base permutation and
	// sends one cyclic shift per position, placing every request in every
	// position once. Exact balance requires retaining every shift in a cycle. A
	// failed or omitted trial can therefore unbalance its siblings.
	ArrangeRotate ArrangementPolicy = iota

	// ArrangeNone sends requests in input order for every trial, confounding
	// request identity with stream position.
	ArrangeNone

	// ArrangeRandom draws an independent uniform permutation for each trial.
	// Positions balance only in expectation.
	ArrangeRandom
)

// balancesExactly reports whether the policy achieves exact position balance
// over a block of trials, which is what makes a trial count that is a whole
// number of blocks meaningful. Only cyclic rotation does; the random policy
// balances in expectation, and the none policy balances nothing.
func (p ArrangementPolicy) balancesExactly() bool { return p == ArrangeRotate }

// unit is the number of trials the policy needs to complete one balanced block,
// and so the granularity at which trials are distributed across connections: a
// whole cycle under rotation, a single trial otherwise.
func (p ArrangementPolicy) unit(n int) int {
	if p.balancesExactly() {
		return n
	}
	return 1
}

// WithArrangementPolicy sets the trial arrangement policy. The default is
// [ArrangeRotate]. It panics if p is not defined.
func WithArrangementPolicy(p ArrangementPolicy) arrangementOption {
	if p < ArrangeRotate || p > ArrangeRandom {
		panic("tth2: WithArrangementPolicy requires a defined policy")
	}
	return arrangementOption{p: p}
}

type arrangementOption struct{ p ArrangementPolicy }

func (o arrangementOption) applyTrials(c *trialsConfig) { c.policy = o.p }
func (arrangementOption) runTrialsOption()              {}

// arranger produces one arrangement per trial under a policy. An arrangement is
// indexed by position: arrangement[position] is the index of the request to
// flush there, so arrangement[0] names the first-flushed request.
//
// One arranger serves one worker, which owns whole blocks, so it holds its own
// random source and needs no synchronisation.
type arranger struct {
	policy ArrangementPolicy
	n      int
	rng    *rand.Rand
	base   []int // the current cycle's base permutation, under ArrangeRotate
	buf    []int // the arrangement handed out, rewritten per trial
}

// newArranger returns an arranger for n requests under policy, drawing from
// rng.
func newArranger(policy ArrangementPolicy, n int, rng *rand.Rand) *arranger {
	a := &arranger{policy: policy, n: n, rng: rng, buf: make([]int, n)}
	if policy == ArrangeRotate {
		a.base = identity(n)
	}
	return a
}

// startCycle draws the base permutation for the cycle about to run. Under any
// other policy it does nothing. Callers invoke it once per cycle, before the
// cycle's first trial.
func (a *arranger) startCycle() {
	if a.policy == ArrangeRotate {
		a.rng.Shuffle(a.n, func(i, j int) { a.base[i], a.base[j] = a.base[j], a.base[i] })
	}
}

// next returns the arrangement for the trial at the given shift within the
// current cycle. The returned slice is owned by the arranger and rewritten on
// the next call; a caller keeping it past that copies it.
func (a *arranger) next(shift int) []int {
	switch a.policy {
	case ArrangeNone:
		for p := range a.buf {
			a.buf[p] = p
		}
	case ArrangeRandom:
		for p := range a.buf {
			a.buf[p] = p
		}
		a.rng.Shuffle(a.n, func(i, j int) { a.buf[i], a.buf[j] = a.buf[j], a.buf[i] })
	default: // ArrangeRotate
		for p := range a.buf {
			a.buf[p] = a.base[(p+shift)%a.n]
		}
	}
	return a.buf
}

// scatterBatch re-keys a batch from the order it was sent in to the order the
// requests were given in. Ranks are per response and unaffected; the arrival
// order is a sequence of positions, so it is translated with them.
func scatterBatch(b *BatchResult, arrangement []int) BatchResult {
	out := BatchResult{
		Connection:   b.Connection,
		Results:      scatter(b.Results, arrangement),
		ArrivalOrder: make([]int, len(b.ArrivalOrder)),
	}
	for rank, position := range b.ArrivalOrder {
		out.ArrivalOrder[rank] = arrangement[position]
	}
	return out
}

// identity returns the arrangement that sends requests in the order given.
func identity(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// arrange returns reqs laid out by position: out[position] is the request to
// flush at that position. The slice is fresh; the requests in it are shared,
// which is safe because sending only reads them.
func arrange(reqs []*http.Request, arrangement []int) []*http.Request {
	out := make([]*http.Request, len(arrangement))
	for position, i := range arrangement {
		out[position] = reqs[i]
	}
	return out
}

// scatter inverts an arrangement, mapping results back to request order:
// results[position] came from the request at that position, so it belongs at
// arrangement[position].
func scatter(results []Result, arrangement []int) []Result {
	out := make([]Result, len(arrangement))
	for position, i := range arrangement {
		out[i] = results[position]
	}
	return out
}

// newTrialRNG returns an independently seeded source for one worker.
func newTrialRNG() *rand.Rand {
	//nolint:gosec // non-cryptographic PRNG: position balancing, not a secret
	return rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
}
