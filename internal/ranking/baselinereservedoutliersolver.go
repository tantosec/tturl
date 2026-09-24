package ranking

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"sync"

	"github.com/tantosec/tturl/stats"
)

// Defaults used by [BaselineReservedOutlierSolver].
const (
	// OutlierDefaultMaxComparisons is the default comparison limit for one
	// identification. Reaching it produces an inconclusive result.
	OutlierDefaultMaxComparisons = 50_000_000

	// bettingComponents is the number of geometrically spaced bets in the
	// positive evidence mixture.
	bettingComponents = 7

	// baselineConfirmationNominationGate is a work-allocation gate, not an
	// evidence boundary. It avoids spending clean comparisons on a discovery
	// leader before its betting wealth has doubled.
	baselineConfirmationNominationGate = math.Ln2
)

// BaselineReservedOutlierSolver returns one target using bounded betting
// e-processes, randomised positions, adaptive sampling, and family-wise Ville
// thresholds.
// Its guarantees require conditional mean bounds, but not independence, a
// variance bound, or a parametric rank distribution.
//
// # Evidence and stopping
//
// For a [Late] search, an observation X is the item's normalised rank in
// [0, 1]; [Early] uses 1-X. With [Baseline] controls, X is instead the fraction
// of those controls the candidate outranks.
//
// A positive component multiplies wealth by
//
//	1 + lambda*(X - 1/2),  0 <= lambda <= 2.
//
// This is an e-factor when a non-target obeys E[X | past] <= 1/2. A fixed
// mixture of bet sizes adapts to unknown effects and rank variance. An item
// commits at ln(K/alpha), where alpha is the requested FalseFindRisk and K is M
// for a known direction or 2M for [Either].
//
// An elimination process multiplies wealth by
//
//	1 + gamma*((1/2 + MinimumEdge) - X),
//
// where gamma is predictable and no greater than 1/(1/2-MinimumEdge). It is an
// e-factor when a covered target obeys E[X | past] >= 1/2+MinimumEdge. An item
// is eliminated when this evidence reaches ln(1/beta) in every searched
// direction. Eliminating all items gives a negative with FalseNegativeRisk
// beta.
// MinimumEdge affects only the negative guarantee; positive evidence adapts
// independently. Smaller edges include subtler targets but increase negative
// cost on the order of 1/MinimumEdge^2.
//
// Thompson sampling controls scheduling only and does not enter either error
// bound.
//
// # Allocation strategy
//
// This is the conservative clean-reference strategy in the outlier family.
// When Baseline controls are available, it reserves positions for them from the
// first comparison. The broad-field reserve balances the number of candidates
// observed against the information in each control-relative rank. As candidates
// are eliminated, every spare position becomes a control; a lone survivor is
// therefore tested against width-1 clean references without a separate phase or
// evidence reset.
//
// # Scope and assumptions
//
// The conditional mean bounds condition on all prior outcomes and on the
// wave's pre-outcome plan, including selected identities, opponent counts, and
// predictable bets. They average over fresh uniform position permutations and
// comparison outcomes. Every scheduled appearance must satisfy the applicable
// bound against its actual references; averaging over adaptive membership
// choices is insufficient.
//
// With controls, the bounds concern order relative to the selected baselines;
// without them, they concern rank among the selected peers and padding items.
// Uniform positions cancel arbitrary position-only bias when candidate and
// baseline labels are exchangeable under the relevant null. They do not cancel
// identity-specific shifts. Baselines must therefore be valid mean references,
// a property the solver cannot establish from one run.
//
// This solver does not use [Positive] controls because a known target does not
// establish the conditional-mean null for a non-target.
//
// [Either] requires baselines. Without a known reference, a target at one
// extreme can make a peer appear equally exceptional at the other. A
// known-direction search may use peer-relative ranks when its conditional mean
// premise holds.
//
// Concurrent comparisons are folded in atomic waves. Each candidate receives
// the arithmetic mean of its e-factors within a wave, permitting arbitrary
// dependence among sibling outcomes. Each appearance must still satisfy its
// marginal conditional mean bound given the pre-outcome plan.
//
// # Error risks
//
// [OutlierRequest.FalseFindRisk] and [OutlierRequest.FalseNegativeRisk] spend
// separate prospective error budgets and provide anytime-valid Ville
// guarantees. A find uses a family-wise union over searched items and
// directions. A negative needs no population union because a false negative
// requires the qualifying target's own elimination process to cross. Neither
// limit is a posterior probability that a particular result is correct.
//
// # Multiple targets
//
// The guarantees do not require a unique target. A positive result returns one
// item, and the family-wise threshold controls returning any non-target. For a
// negative, choose any qualifying target as a witness: returning negative
// requires that witness's elimination process to cross its boundary.
//
// Additional targets can nevertheless invalidate a peer-relative premise by
// changing one another's normalised ranks. Baseline-relative observations do
// not have that interference because candidate-candidate order is ignored. A
// caller must establish the documented conditional-mean bound for every
// appearance used by the applicable positive or negative guarantee; target
// count alone neither establishes nor invalidates it.
type BaselineReservedOutlierSolver struct {
	// MaxComparisons caps completed comparisons in one Identify call. Zero
	// selects [OutlierDefaultMaxComparisons], narrowed when necessary to keep
	// participation counts representable. Exhaustion is inconclusive. Start
	// instead takes an explicit budget on each OutlierRun.Advance.
	MaxComparisons int
	// MaxConcurrency caps comparisons the solver plans in one atomic wave. Zero
	// uses the comparator's declared ceiling, treating its zero as one. The
	// comparator independently enforces its ceiling.
	MaxConcurrency int

	// newRNG, when set, supplies the random source for a run (tests set it to
	// pin a seed for reproducibility). When nil, each Identify draws its own
	// fresh, independently-seeded source.
	newRNG func() *rand.Rand

	// controlReserve overrides the automatic control allocation in experiments.
	controlReserve func(width, available int) int

	// boundaryPositive selects the MinimumEdge-directed positive e-process used
	// by EdgeDirectedOutlierSolver. The zero value preserves this type's mixture.
	boundaryPositive bool

	// cleanCommitOnly keeps a second positive-evidence stream containing only
	// Baseline-relative observations and uses it for commits and diagnostics.
	cleanCommitOnly bool

	// adaptiveCleanConfirmation interleaves predictable clean confirmation of a
	// discovery leader. It requires cleanCommitOnly and a Baseline control.
	adaptiveCleanConfirmation bool

	// operation names the public wrapper in shared-engine errors. Empty names
	// this type itself.
	operation string

	// execution selects the private comparison scheduler used by a public
	// wrapper. Its zero value preserves atomic-wave execution.
	execution outlierExecution

	// requireBaseline rejects a run without a clean Baseline reference. Public
	// wrappers use it when peer-relative fallback is outside their contract.
	requireBaseline bool
}

type outlierExecution uint8

const (
	outlierExecutionAtomicWaves outlierExecution = iota
	outlierExecutionRolling
)

const baselineReservedOutlierMethodID OutlierMethodID = "ranking/outlier/baseline-reserved-bounded-betting/v1beta1"

// Inference reports anytime-valid decisions at completed atomic waves.
func (*BaselineReservedOutlierSolver) Inference() OutlierInferenceRegime {
	return OutlierInferenceAnytimeValid
}

// MethodID implements [IdentifiedOutlierSolver].
func (*BaselineReservedOutlierSolver) MethodID() OutlierMethodID {
	return baselineReservedOutlierMethodID
}

// OutlierItemRef contains one contender's evidence toward each extreme.
type OutlierItemRef struct {
	// Item is the candidate index, in [0, M).
	Item int
	// Late and Early are the item's positive log e-values toward the late and
	// early extremes. Each races the [OutlierProgressDiagnostics.Threshold].
	Late, Early float64
}

// LogEvidence returns the item's positive log e-value toward an extreme. For
// Either it returns the stronger of the Late and Early readings.
func (r OutlierItemRef) LogEvidence(s OutlierDirection) float64 {
	switch s {
	case Early:
		return r.Early
	case Either:
		return math.Max(r.Late, r.Early)
	default:
		return r.Late
	}
}

// OutlierProgressDiagnostics is the detail value in
// [OutlierProgress.Detail] produced by [BaselineReservedOutlierSolver],
// [RollingBaselineReservedOutlierSolver],
// [PeerFirstOutlierSolver],
// [BaselineConfirmedOutlierSolver], and [EdgeDirectedOutlierSolver].
type OutlierProgressDiagnostics struct {
	// Threshold is the commit threshold in log-evidence units. It is the raw
	// counterpart of the decision boundary.
	Threshold float64
	// Contenders contains every remaining item in unspecified order.
	Contenders []OutlierItemRef
}

func (OutlierProgressDiagnostics) progressDetail() {}

// Leaders returns up to n contenders leaning most strongly toward the given
// extreme, strongest first, ranked by [OutlierItemRef.LogEvidence] for that
// extreme. It leaves [OutlierProgressDiagnostics.Contenders] untouched, so a
// consumer can read one end without disturbing the list or the other. It
// returns nil for n <= 0 or no contenders.
func (p OutlierProgressDiagnostics) Leaders(s OutlierDirection, n int) []OutlierItemRef {
	if n <= 0 || len(p.Contenders) == 0 {
		return nil
	}
	top := make([]OutlierItemRef, 0, min(n, len(p.Contenders)))
	for _, r := range p.Contenders {
		key := r.LogEvidence(s)
		if len(top) == cap(top) && key <= top[len(top)-1].LogEvidence(s) {
			continue // buffer full and r cannot displace the current worst
		}
		if len(top) < cap(top) {
			top = top[:len(top)+1]
		}
		i := len(top) - 1
		for i > 0 && key > top[i-1].LogEvidence(s) {
			top[i] = top[i-1]
			i--
		}
		top[i] = r
	}
	return top
}

// OutlierDiagnostics is the detail value in [OutlierResult.Detail] produced by
// the outlier solver family.
type OutlierDiagnostics struct {
	// Threshold is the commit threshold in log-evidence units.
	Threshold float64
	// MinimumEdge is the effective normalised-rank advantage covered by a
	// negative result. It gives EliminationLogE its target boundary.
	MinimumEdge float64
	// Items carries each item's evidence, index-aligned with
	// [OutlierResult.Items] (and so with candidate indices [0, M)).
	Items []OutlierItemStat
}

func (OutlierDiagnostics) resultDetail() {}

// OutlierItemStat contains one item's calibrated evidence.
type OutlierItemStat struct {
	// LogEvidence is the item's strongest positive log e-value across the
	// searched directions. The commit threshold acts on this evidence. It freezes
	// at elimination and otherwise reports the end of the find.
	LogEvidence float64
	// EliminationLogE is the weakest evidence, across searched directions,
	// against the item sustaining the configured MinimumEdge. It crosses the
	// elimination threshold exactly when every direction has crossed.
	EliminationLogE float64
	// Eliminated reports whether the solver ruled the item out — its evidence
	// crossed the elimination threshold in every searched direction.
	Eliminated bool
}

// defaultRNG returns an independently seeded non-cryptographic source.
func defaultRNG() *rand.Rand {
	//nolint:gosec // Non-cryptographic PRNG for randomised positioning.
	return rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
}

// cmpOutcome associates a worker result with its positioned comparison.
type cmpOutcome struct {
	index      int
	comparison []ItemID
	ranks      []int
	// confirming is the only candidate observed in a clean-confirmation
	// comparison, or -1 for an ordinary discovery comparison.
	confirming int
	err        error
}

type outlierRunCommand struct {
	ctx    context.Context
	budget int
	reply  chan outlierRunReply
}

type outlierRunReply struct {
	result OutlierResult
	err    error
}

type outlierRunInit struct {
	oneShotBudget int
	err           error
}

type outlierRun struct {
	commands chan outlierRunCommand
	done     chan struct{}
	cancel   context.CancelFunc

	mu        sync.Mutex
	advancing bool
	closed    bool

	oneShotBudget int
}

func (r *outlierRun) Advance(
	ctx context.Context, budget Budget,
) (OutlierResult, error) {
	fail := OutlierResult{Index: -1}
	if ctx == nil {
		return fail, errors.New("ranking: OutlierRun.Advance: ctx is nil")
	}
	if budget.Comparisons <= 0 {
		return fail, fmt.Errorf(
			"ranking: OutlierRun.Advance: Comparisons must be > 0, got %d",
			budget.Comparisons)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return fail, ErrOutlierRunClosed
	}
	if r.advancing {
		r.mu.Unlock()
		return fail, ErrOutlierRunBusy
	}
	r.advancing = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.advancing = false
		r.mu.Unlock()
	}()

	reply := make(chan outlierRunReply, 1)
	command := outlierRunCommand{
		ctx: ctx, budget: budget.Comparisons, reply: reply,
	}
	select {
	case r.commands <- command:
	case <-ctx.Done():
		return fail, ctx.Err()
	case <-r.done:
		return fail, ErrOutlierRunClosed
	}
	response := <-reply
	if response.err != nil || response.result.Outcome != OutlierPaused {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
	}
	return response.result, response.err
}

func (r *outlierRun) Close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		<-r.done
		return
	}
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	<-r.done
}

// Start begins an in-memory anytime-valid run bound to req, cmp, and this
// solver's configuration.
func (f *BaselineReservedOutlierSolver) Start(
	req OutlierRequest, cmp *Comparator,
) (OutlierRun, error) {
	return f.start(req, cmp, "Start")
}

func (f *BaselineReservedOutlierSolver) start(
	req OutlierRequest, cmp *Comparator, method string,
) (*outlierRun, error) {
	ctx, cancel := context.WithCancel(context.Background())
	run := &outlierRun{
		commands: make(chan outlierRunCommand), done: make(chan struct{}),
		cancel: cancel,
	}
	init := make(chan outlierRunInit, 1)
	operation := f.operation
	if operation == "" {
		operation = "BaselineReservedOutlierSolver"
	}
	operation += "." + method
	maxComparisons := f.MaxComparisons
	if method == "Start" {
		maxComparisons = 0
	}
	go f.serve(ctx, req, cmp, operation, maxComparisons,
		init, run.commands, run.done)
	started := <-init
	if started.err != nil {
		cancel()
		<-run.done
		return nil, started.err
	}
	run.oneShotBudget = started.oneShotBudget
	return run, nil
}

// Identify implements [OutlierSolver] as one bounded advance of a resumable
// run. A budget pause becomes inconclusive because Identify does not return the
// live run.
func (f *BaselineReservedOutlierSolver) Identify(
	ctx context.Context, req OutlierRequest, cmp *Comparator,
) (OutlierResult, error) {
	run, err := f.start(req, cmp, "Identify")
	if err != nil {
		return OutlierResult{Index: -1}, err
	}
	defer run.Close()
	result, err := run.Advance(ctx, Budget{Comparisons: run.oneShotBudget})
	if result.Outcome == OutlierPaused {
		result.Outcome = OutlierInconclusive
	}
	return result, err
}

func (f *BaselineReservedOutlierSolver) serve(
	lifetime context.Context, req OutlierRequest, cmp *Comparator,
	operation string, configuredMaxComparisons int,
	init chan<- outlierRunInit,
	commands <-chan outlierRunCommand, done chan<- struct{},
) {
	defer close(done)
	fail := OutlierResult{Index: -1}
	if err := req.validate(cmp, operation); err != nil {
		init <- outlierRunInit{err: err}
		return
	}
	prof := cmp.Profile()
	n := prof.MaxWidth
	// This run's random source: its own, so nothing is shared on the struct and
	// concurrent Identify calls stay independent. Tests pin it via newRNG.
	newRNG := f.newRNG
	if newRNG == nil {
		newRNG = defaultRNG
	}
	rng := newRNG()
	// Clean Baseline controls the channel can supply per comparison. Assembly
	// fills spare (non-candidate) positions with them, up to what the supply
	// yields; a bounded supply short of the spare count is topped up with
	// eliminated peers, while an unbounded supply fills every spare position with
	// a clean baseline.
	baselineSupply := prof.Controls[Baseline]
	if f.requireBaseline && baselineSupply.UpTo(1) == 0 {
		init <- outlierRunInit{err: fmt.Errorf(
			"ranking: %s: requires a Baseline control", operation)}
		return
	}
	if f.adaptiveCleanConfirmation && baselineSupply.UpTo(1) == 0 {
		init <- outlierRunInit{err: fmt.Errorf(
			"ranking: %s: clean confirmation requires a Baseline control",
			operation)}
		return
	}
	if req.Direction == Either && baselineSupply.UpTo(1) == 0 {
		init <- outlierRunInit{err: fmt.Errorf(
			"ranking: %s: Either requires a Baseline control", operation)}
		return
	}
	// A comparison needs n distinct items. The M candidates supply some; controls
	// (and, once elimination begins, dropped baselines) fill the rest — so M may
	// sit below the comparison width, as in the M≈1 confirm of a single suspect,
	// only if the Baseline supply can make up the difference.
	if need := n - req.M; need > 0 {
		if avail := baselineSupply.UpTo(need); avail < need {
			init <- outlierRunInit{err: fmt.Errorf(
				"ranking: %s: M (%d) + %d Baseline controls < comparator width (%d)",
				operation, req.M, avail, n)}
			return
		}
	}

	edge, maxComparisons, w, err := resolveOutlierRun(
		operation, n, prof.MaxConcurrency,
		req.MinimumEdge, configuredMaxComparisons, f.MaxConcurrency)
	if err != nil {
		init <- outlierRunInit{err: err}
		return
	}

	// The negative's error budget is per request. If any qualifying target
	// exists, a false negative
	// requires a fixed witness target's elimination, so this per-item budget needs
	// no family-wise union (unlike tauCommit).
	betaElim := req.FalseNegativeRisk
	// Directions to search: one for a known direction, both for Either. Each
	// (item, direction) pair is a hypothesis, so the family is numHyp =
	// M*len(dirs), and
	// the Ville union threshold covers all of them. Validate products before any
	// allocation or threshold arithmetic.
	dirs := []OutlierDirection{req.Direction}
	if req.Direction == Either {
		dirs = []OutlierDirection{Late, Early}
	}
	if req.M > math.MaxInt/len(dirs) || req.M > math.MaxInt/bettingComponents {
		init <- outlierRunInit{err: fmt.Errorf(
			"ranking: %s: M (%d) is too large", operation, req.M)}
		return
	}
	numHyp := req.M * len(dirs)

	tauCommit := stats.VilleUnionThreshold(numHyp, req.FalseFindRisk)
	tauElim := stats.VilleThreshold(betaElim)

	// Reserve clean baselines while the field is broad. The chosen count
	// maximises the total rank information proxy across the candidate positions;
	// as the field shrinks, every additional spare position becomes a control.
	controlReservePolicy := f.controlReserve
	if controlReservePolicy == nil {
		controlReservePolicy = outlierControlReserve
	}
	controlReserve := controlReservePolicy(n, baselineSupply.UpTo(n-1))
	observationPremise := OutlierPremiseScheduledReferenceMean
	if controlReserve > 0 {
		observationPremise = OutlierPremiseBaselineMean
	}
	positivePremise := observationPremise
	if f.cleanCommitOnly {
		positivePremise = OutlierPremiseBaselineMean
	}
	positiveEdge := 0.0
	if f.boundaryPositive {
		positiveEdge = edge
	}
	foundGuarantee := func(sign OutlierDirection) OutlierGuarantee {
		return OutlierGuarantee{
			ErrorDirection: OutlierErrorFalseFind, ErrorRiskLimit: req.FalseFindRisk,
			MinimumEdge: positiveEdge, Direction: sign, Premise: positivePremise,
			Regime: OutlierInferenceAnytimeValid,
		}
	}
	negativeGuarantee := func() OutlierGuarantee {
		return OutlierGuarantee{
			ErrorDirection: OutlierErrorFalseNegative, ErrorRiskLimit: req.FalseNegativeRisk,
			MinimumEdge: edge, Direction: req.Direction, Premise: observationPremise,
			Regime: OutlierInferenceAnytimeValid,
		}
	}

	// Positive bets test conditional mean rank <= 1/2. Elimination bets reverse
	// the test at the target boundary 1/2+MinimumEdge. A fixed mixture of positive
	// bet sizes adapts to unknown effect and rank variance while each component
	// remains an e-process in its own right.
	positiveBets, positiveLogWeights := outlierBettingMixture()
	positiveLate := outlierEvidence(req.M, len(positiveBets))
	positiveEarly := outlierEvidence(req.M, len(positiveBets))
	var cleanPositiveLate, cleanPositiveEarly [][]float64
	if f.cleanCommitOnly {
		cleanPositiveLate = outlierEvidence(req.M, len(positiveBets))
		cleanPositiveEarly = outlierEvidence(req.M, len(positiveBets))
	}
	boundaryPositiveLate := make([]outlierBoundaryBet, req.M)
	boundaryPositiveEarly := make([]outlierBoundaryBet, req.M)
	negativeLate := make([]float64, req.M)
	negativeEarly := make([]float64, req.M)
	negativeVariance := make([]outlierRankVariance, req.M)
	positive := func(c int, s OutlierDirection) float64 {
		if f.boundaryPositive {
			if s == Early {
				return boundaryPositiveEarly[c].logWealth
			}
			return boundaryPositiveLate[c].logWealth
		}
		if s == Early {
			return outlierLogMixture(positiveEarly[c], positiveLogWeights)
		}
		return outlierLogMixture(positiveLate[c], positiveLogWeights)
	}
	commitPositive := positive
	if f.cleanCommitOnly {
		commitPositive = func(c int, s OutlierDirection) float64 {
			if s == Early {
				return outlierLogMixture(cleanPositiveEarly[c], positiveLogWeights)
			}
			return outlierLogMixture(cleanPositiveLate[c], positiveLogWeights)
		}
	}
	negative := func(c int, s OutlierDirection) float64 {
		if s == Early {
			return negativeEarly[c]
		}
		return negativeLate[c]
	}

	// Alpha and beta are heuristic Beta-posterior pseudo-counts used only for
	// adaptive scheduling. The error guarantees rest on the betting
	// processes above, not on this posterior interpretation.
	alpha := make([]float64, req.M)
	beta := make([]float64, req.M)
	for i := range alpha {
		alpha[i] = 1
		beta[i] = 1
	}
	// best returns the direction in dirs with the strongest positive evidence.
	bestWith := func(c int, evidence func(int, OutlierDirection) float64) (OutlierDirection, float64) {
		bs, bl := dirs[0], evidence(c, dirs[0])
		for _, s := range dirs[1:] {
			if l := evidence(c, s); l > bl {
				bl, bs = l, s
			}
		}
		return bs, bl
	}
	best := func(c int) (OutlierDirection, float64) { return bestWith(c, positive) }
	commitBest := func(c int) (OutlierDirection, float64) {
		return bestWith(c, commitPositive)
	}
	// leaderOf returns the strongest item in any searched direction among items:
	// its index, direction, and log evidence, or (-1, dirs[0], -Inf) for no
	// contenders. It is the current best guess, the item the search
	// would commit if it stopped now.
	leaderWith := func(
		items []int, evidence func(int) (OutlierDirection, float64),
	) (idx int, sign OutlierDirection, logEvidence float64) {
		idx, sign, logEvidence = -1, dirs[0], math.Inf(-1)
		for _, c := range items {
			if s, l := evidence(c); l > logEvidence {
				idx, sign, logEvidence = c, s, l
			}
		}
		return
	}
	leaderOf := func(items []int) (int, OutlierDirection, float64) {
		return leaderWith(items, best)
	}
	commitLeaderOf := func(items []int) (int, OutlierDirection, float64) {
		return leaderWith(items, commitBest)
	}
	active := make([]int, req.M) // currently-contending item indices (compacted on elimination)
	for i := range active {
		active[i] = i
	}
	eliminated := make([]int, 0, req.M) // dropped items, reused as clean padding for under-full comparisons
	elim := make([]bool, req.M)

	// Per-item diagnostics for a returned Result. These count only comparisons in
	// which the item was an active candidate (frozen at elimination, alongside its
	// evidence), so samples[c] is the observation count behind both its mean rank
	// and its log evidence.
	samples := make([]int, req.M)
	rankSum := make([]float64, req.M)
	// diagnostics builds the two per-item summaries a Result carries: the
	// class-neutral [ItemStat] slice (Result.Items) and
	// BaselineReservedOutlierSolver's own [OutlierDiagnostics] detail
	// (Result.Detail), both index-aligned with candidate indices [0, M).
	diagnostics := func() ([]ItemStat, OutlierDiagnostics) {
		items := make([]ItemStat, req.M)
		detail := OutlierDiagnostics{
			Threshold: tauCommit, MinimumEdge: edge,
			Items: make([]OutlierItemStat, req.M),
		}
		for c := range items {
			_, l := commitBest(c)
			eliminationLogE := negative(c, dirs[0])
			for _, direction := range dirs[1:] {
				eliminationLogE = min(eliminationLogE, negative(c, direction))
			}
			mean := math.NaN()
			if samples[c] > 0 {
				mean = rankSum[c] / float64(samples[c])
			}
			items[c] = ItemStat{Samples: samples[c], MeanRank: mean}
			detail.Items[c] = OutlierItemStat{
				LogEvidence: l, EliminationLogE: eliminationLogE, Eliminated: elim[c],
			}
		}
		return items, detail
	}

	comparison := make([]ItemID, 0, n) // reused assembly buffer
	sampleBuf := make([]float64, req.M)
	var coverageOrder []int
	if f.execution == outlierExecutionRolling {
		coverageOrder = rng.Perm(req.M)
	}
	coverageCursor := 0

	// A CadenceRun, started here at the solve's start, owns all progress
	// bookkeeping: it meters throughput and decides when a report is due, seeding
	// its clock now. The solver just feeds it counts through ReportIfDue/
	// ReportFinal below; with a nil deliver (no callback wired) those calls are
	// inert, so the loop and the returns need no progress guard. The caller's
	// cadence value is untouched.
	cadence := req.ProgressCadence.Start()
	comparisons := 0
	participations := 0 // running sum of each comparison's width, for Result.Participations

	// snapshot builds a report of the running find around the cost core the
	// cadence supplies (counts + smoothed rates): it fills the class body — the
	// active count and the leader (the strongest item in any searched direction)
	// plus BaselineReservedOutlierSolver's own detail,
	// every contender with its log evidence toward each extreme, in no set order.
	snapshot := func(core ProgressCore) OutlierProgress {
		contenders := make([]OutlierItemRef, len(active))
		for i, c := range active {
			contenders[i] = OutlierItemRef{
				Item: c, Late: commitPositive(c, Late),
				Early: commitPositive(c, Early),
			}
		}
		leaderIdx, leaderSign, _ := commitLeaderOf(active)
		return OutlierProgress{
			ProgressCore: core,
			Active:       len(active),
			Leader:       leaderIdx,
			Direction:    leaderSign,
			Detail:       OutlierProgressDiagnostics{Threshold: tauCommit, Contenders: contenders},
		}
	}

	// deliver bridges the class-neutral cadence to the caller's typed callback:
	// given the metered core, it renders the snapshot and hands it over. It is
	// nil when no callback is wired, which the cadence treats as "report
	// nothing" -- so every report call below is a no-op and needs no guard.
	var deliver func(ProgressCore)
	if req.Progress != nil {
		deliver = func(core ProgressCore) { req.Progress(snapshot(core)) }
	}

	// Concurrency waves contain up to w comparisons. At w == 1 the loop below
	// runs one comparison at a time, in the same order and with the same RNG draws
	// as a fully sequential driver.

	// sampleComparison chooses and positions the next comparison from the CURRENT
	// evidence, returning a fresh comparison slice, or nil when the active set is
	// empty. It is the only reader of rng and the only mutator of the sampling
	// scratch (active, eliminated, sampleBuf, comparison); the controller calls it
	// serially, so sampling stays single-threaded even while comparisons run
	// concurrently. The returned slice is a copy — it is handed to a worker
	// goroutine and must not alias the reused assembly buffer.
	sampleComparison := func() []ItemID {
		numActive := len(active)
		if numActive == 0 {
			return nil
		}

		// Leader in any searched direction before this comparison, for the pin.
		leaderIdx, _, leaderLogEvidence := leaderOf(active)

		// Use as many active candidates as fit; spare positions are filled with clean
		// controls (or eliminated baselines). As the active set narrows to a few
		// survivors — or one suspect, the M≈1 verify case — the spare positions
		// become a clean confirmation against controls automatically.
		candidatePositions := min(numActive, n-controlReserve)

		// A rolling run first schedules every candidate once. Reservations happen at
		// dispatch, not completion, so out-of-order completion cannot select which
		// candidate receives the next initial observation.
		coveredPositions := 0
		for coveredPositions < candidatePositions &&
			coverageCursor < len(coverageOrder) {
			candidate := coverageOrder[coverageCursor]
			coverageCursor++
			if elim[candidate] {
				continue
			}
			candidateAt := -1
			for i := coveredPositions; i < numActive; i++ {
				if active[i] == candidate {
					candidateAt = i
					break
				}
			}
			if candidateAt < 0 {
				continue
			}
			active[coveredPositions], active[candidateAt] =
				active[candidateAt], active[coveredPositions]
			coveredPositions++
		}

		// Thompson fills positions not owned by initial coverage. The posterior is
		// late-oriented; score it in the searched direction — the early-win rate
		// for Early, extremeness in either direction for Either. Sampling is not
		// load-bearing, so this only steers where to spend comparisons.
		for i, c := range active {
			s := stats.BetaRV(rng, alpha[c], beta[c])
			switch req.Direction {
			case Early:
				s = 1 - s
			case Either:
				s = math.Max(s, 1-s)
			}
			sampleBuf[i] = s
		}
		for pos := coveredPositions; pos < candidatePositions; pos++ {
			top := pos
			for j := pos + 1; j < numActive; j++ {
				if sampleBuf[j] > sampleBuf[top] {
					top = j
				}
			}
			active[pos], active[top] = active[top], active[pos]
			sampleBuf[pos], sampleBuf[top] = sampleBuf[top], sampleBuf[pos]
		}
		// Pin the leader into the candidate positions if Thompson missed it, so it
		// accrues evidence every comparison once it emerges. The start is pure
		// exploration because the pin uses only positive log evidence.
		if leaderLogEvidence > 0 && candidatePositions < numActive &&
			coveredPositions < candidatePositions {
			leaderPos := -1
			for i := range numActive {
				if active[i] == leaderIdx {
					leaderPos = i
					break
				}
			}
			if leaderPos >= candidatePositions {
				active[candidatePositions-1], active[leaderPos] = active[leaderPos], active[candidatePositions-1]
			}
		}

		// Assemble the comparison: candidate positions, then control positions
		// — controls first, eliminated baselines for any shortfall when controls
		// are scarce.
		// (Eliminated picks are kept distinct by a partial Fisher–Yates.)
		comparison = comparison[:0]
		for k := range candidatePositions {
			comparison = append(comparison, ItemID(active[k]))
		}
		controlPositions := n - candidatePositions
		nc := baselineSupply.UpTo(controlPositions)
		for j := range nc {
			comparison = append(comparison, Control(Baseline, j))
		}
		for k := 0; k < controlPositions-nc; k++ {
			pick := k + rng.IntN(len(eliminated)-k)
			eliminated[k], eliminated[pick] = eliminated[pick], eliminated[k]
			comparison = append(comparison, ItemID(eliminated[k]))
		}

		// Positioning: shuffle the comparison into positions uniformly at
		// random. This is what cancels the position bias; it happens every
		// comparison.
		for i := n - 1; i > 0; i-- {
			j := rng.IntN(i + 1)
			comparison[i], comparison[j] = comparison[j], comparison[i]
		}
		return append([]ItemID(nil), comparison...)
	}
	// sampleConfirmation plans one clean observation of a frozen nominee. A
	// comparison may be narrower than MaxWidth when the Baseline supply is
	// bounded; every resulting rank used as evidence is still normalised only
	// against the Baseline controls actually present.
	sampleConfirmation := func(nominee int) []ItemID {
		controls := baselineSupply.UpTo(n - 1)
		comparison = comparison[:0]
		comparison = append(comparison, ItemID(nominee))
		for control := range controls {
			comparison = append(comparison, Control(Baseline, control))
		}
		for i := len(comparison) - 1; i > 0; i-- {
			j := rng.IntN(i + 1)
			comparison[i], comparison[j] = comparison[j], comparison[i]
		}
		return append([]ItemID(nil), comparison...)
	}

	results := make(chan cmpOutcome, w)

	type waveObservation struct {
		lateX     float64
		opponents int
		clean     bool
	}
	waveObservations := make([][]waveObservation, req.M)
	touched := make([]int, 0, req.M)
	seen := make([]bool, req.M)
	discoverySinceConfirmation := false
	applyOutcomes := func(outcomes []cmpOutcome) []int {
		for _, outcome := range outcomes {
			sent, ranks := outcome.comparison, outcome.ranks
			comparisons++
			participations += len(sent)
			controlPositions := make([]int, 0, n-1)
			for p, id := range sent {
				if id.IsControl() && id.ControlKind() == Baseline {
					controlPositions = append(controlPositions, p)
				}
			}
			for p, id := range sent {
				if id.IsControl() || elim[int(id)] ||
					(outcome.confirming >= 0 && int(id) != outcome.confirming) {
					continue
				}
				candidate := int(id)
				rank := ranks[p]
				samples[candidate]++
				rankSum[candidate] += float64(rank)
				lateX := float64(rank) / float64(n-1)
				opponents := n - 1
				if len(controlPositions) > 0 {
					lateWins := 0
					for _, controlPosition := range controlPositions {
						if rank > ranks[controlPosition] {
							lateWins++
						}
					}
					opponents = len(controlPositions)
					lateX = float64(lateWins) / float64(opponents)
				}
				if !seen[candidate] {
					seen[candidate] = true
					touched = append(touched, candidate)
				}
				waveObservations[candidate] = append(
					waveObservations[candidate], waveObservation{
						lateX: lateX, opponents: opponents,
						clean: len(controlPositions) > 0,
					})

				// Scheduling state changes only after all supplied outcomes were
				// planned. A rolling update supplies exactly one outcome.
				scale := stats.RankComparisonWeight(opponents)
				alpha[candidate] += float64(opponents) * lateX * scale
				beta[candidate] += float64(opponents) * (1 - lateX) * scale
			}
		}

		for _, candidate := range touched {
			observations := waveObservations[candidate]
			if f.boundaryPositive {
				opponents := observations[0].opponents
				lateBet := boundaryPositiveLate[candidate].bet(edge, opponents)
				earlyBet := boundaryPositiveEarly[candidate].bet(edge, opponents)
				var lateFactors, earlyFactors float64
				for _, observation := range observations {
					centred := observation.lateX - 0.5
					lateFactors += outlierBetFactor(lateBet, centred)
					earlyFactors += outlierBetFactor(earlyBet, -centred)
				}
				count := float64(len(observations))
				boundaryPositiveLate[candidate].logWealth += math.Log(lateFactors / count)
				boundaryPositiveEarly[candidate].logWealth += math.Log(earlyFactors / count)
				for _, observation := range observations {
					boundaryPositiveLate[candidate].variance.observe(observation.lateX)
					boundaryPositiveEarly[candidate].variance.observe(1 - observation.lateX)
				}
			} else {
				for j, bet := range positiveBets {
					var lateFactors, earlyFactors float64
					var cleanLateFactors, cleanEarlyFactors float64
					cleanCount := 0
					for _, observation := range observations {
						centred := observation.lateX - 0.5
						lateFactors += outlierBetFactor(bet, centred)
						earlyFactors += outlierBetFactor(bet, -centred)
						if f.cleanCommitOnly && observation.clean {
							cleanLateFactors += outlierBetFactor(bet, centred)
							cleanEarlyFactors += outlierBetFactor(bet, -centred)
							cleanCount++
						}
					}
					count := float64(len(observations))
					positiveLate[candidate][j] += math.Log(lateFactors / count)
					positiveEarly[candidate][j] += math.Log(earlyFactors / count)
					if f.cleanCommitOnly && cleanCount > 0 {
						cleanCountFloat := float64(cleanCount)
						cleanPositiveLate[candidate][j] += math.Log(
							cleanLateFactors / cleanCountFloat)
						cleanPositiveEarly[candidate][j] += math.Log(
							cleanEarlyFactors / cleanCountFloat)
					}
				}
			}
			var lateFactors, earlyFactors float64
			for _, observation := range observations {
				bet := negativeVariance[candidate].bet(edge, observation.opponents)
				centred := observation.lateX - 0.5
				lateFactors += outlierBetFactor(
					bet, edge-centred)
				earlyFactors += outlierBetFactor(
					bet, edge+centred)
			}
			count := float64(len(observations))
			negativeLate[candidate] += math.Log(lateFactors / count)
			negativeEarly[candidate] += math.Log(earlyFactors / count)
			for _, observation := range observations {
				negativeVariance[candidate].observe(observation.lateX)
			}
			waveObservations[candidate] = waveObservations[candidate][:0]
			seen[candidate] = false
		}
		updated := append([]int(nil), touched...)
		touched = touched[:0]
		return updated
	}
	settleEvidence := func(updated []int) (int, OutlierDirection, bool) {
		leaderIdx, leaderSign, leaderLogEvidence := commitLeaderOf(active)
		cadence.ReportIfDue(comparisons, participations, deliver)
		if leaderIdx >= 0 && leaderLogEvidence >= tauCommit {
			return leaderIdx, leaderSign, true
		}

		eliminatedAny := false
		for _, candidate := range updated {
			if elim[candidate] {
				continue
			}
			out := true
			for _, s := range dirs {
				if negative(candidate, s) < tauElim {
					out = false
					break
				}
			}
			if out {
				elim[candidate] = true
				eliminated = append(eliminated, candidate)
				eliminatedAny = true
			}
		}
		if eliminatedAny {
			kept := active[:0]
			for _, candidate := range active {
				if !elim[candidate] {
					kept = append(kept, candidate)
				}
			}
			active = kept
		}
		return -1, dirs[0], false
	}
	init <- outlierRunInit{oneShotBudget: maxComparisons}

	// Comparisons run in waves whose memberships and bets are predictable from the
	// pre-outcome plan. If a candidate occurs repeatedly in a wave, each betting
	// component receives the arithmetic mean of its factors, not their product. A
	// convex mean is an e-factor under arbitrary within-wave dependence, provided
	// each scheduled appearance satisfies its marginal conditional-mean bound
	// given that plan.
	for {
		var command outlierRunCommand
		select {
		case <-lifetime.Done():
			return
		case command = <-commands:
		}
		if command.budget > math.MaxInt/n-comparisons {
			command.reply <- outlierRunReply{result: fail, err: fmt.Errorf(
				"ranking: OutlierRun.Advance: cumulative comparisons overflow")}
			return
		}
		trancheEnd := comparisons + command.budget
		advanceCtx, cancelAdvance := context.WithCancel(command.ctx)
		stopLifetimeCancel := context.AfterFunc(lifetime, cancelAdvance)
		launched := comparisons
		var committed *OutlierResult
		var loopErr error
		discardedComparisons := 0
		discardedParticipations := 0

		if f.execution == outlierExecutionRolling {
			buffered := make(map[int]cmpOutcome, w)
			confirmationPending := false
			launch := func() bool {
				confirming := -1
				var batch []ItemID
				nominee, _, nominationEvidence := leaderOf(active)
				if f.adaptiveCleanConfirmation && !confirmationPending &&
					discoverySinceConfirmation && nominee >= 0 &&
					nominationEvidence >= baselineConfirmationNominationGate {
					confirming = nominee
					batch = sampleConfirmation(nominee)
					confirmationPending = true
				} else {
					batch = sampleComparison()
				}
				if batch == nil {
					return false
				}
				index := launched
				launched++
				go func(dispatchIndex int, sent []ItemID, confirmed int) {
					ranks, compareErr := cmp.compareAt(
						advanceCtx, sent, dispatchIndex)
					results <- cmpOutcome{
						index: dispatchIndex, comparison: sent, ranks: ranks,
						confirming: confirmed, err: compareErr,
					}
				}(index, batch, confirming)
				return true
			}
			fillWindow := func() {
				for launched < trancheEnd && launched-comparisons < w &&
					len(active) > 0 && advanceCtx.Err() == nil {
					if !launch() {
						break
					}
				}
			}
			drain := func() []cmpOutcome {
				cancelAdvance()
				outstanding := launched - comparisons
				drained := make([]cmpOutcome, 0, outstanding)
				for index, outcome := range buffered {
					outcome.index = index
					drained = append(drained, outcome)
					delete(buffered, index)
				}
				remaining := outstanding - len(drained)
				for range remaining {
					drained = append(drained, <-results)
				}
				launched = comparisons
				return drained
			}
			countDiscarded := func(outcomes []cmpOutcome) {
				for _, outcome := range outcomes {
					if outcome.err == nil {
						discardedComparisons++
						discardedParticipations += len(outcome.comparison)
					}
				}
			}

			fillWindow()
			for comparisons < launched && len(active) > 0 &&
				advanceCtx.Err() == nil {
				outcome := <-results
				buffered[outcome.index] = outcome
				hasFailure := outcome.err != nil
			collectReady:
				for {
					select {
					case ready := <-results:
						buffered[ready.index] = ready
						hasFailure = hasFailure || ready.err != nil
					default:
						break collectReady
					}
				}
				if hasFailure {
					drained := drain()
					if command.ctx.Err() != nil {
						loopErr = command.ctx.Err()
					} else if lifetime.Err() != nil {
						loopErr = lifetime.Err()
					} else {
						for _, substantive := range []bool{true, false} {
							failureIndex := math.MaxInt
							var failure error
							for _, candidate := range drained {
								isCancellation := errors.Is(candidate.err, context.Canceled)
								if candidate.err != nil &&
									isCancellation != substantive &&
									candidate.index < failureIndex {
									failureIndex, failure = candidate.index, candidate.err
								}
							}
							if failure != nil {
								loopErr = fmt.Errorf(
									"ranking: %s: comparison %d failed: %w",
									operation, failureIndex, failure)
								break
							}
						}
					}
					break
				}

				prefix := make([]cmpOutcome, 0, len(buffered))
				for {
					next, ok := buffered[comparisons+len(prefix)]
					if !ok {
						break
					}
					prefix = append(prefix, next)
				}
				if len(prefix) == 0 {
					continue
				}
				updated := make([]int, 0, len(prefix))
				for _, next := range prefix {
					delete(buffered, next.index)
					updated = append(updated, applyOutcomes([]cmpOutcome{next})...)
					if next.confirming >= 0 {
						confirmationPending = false
						discoverySinceConfirmation = false
					} else {
						discoverySinceConfirmation = true
					}
					if cmp.comparisonCommitted != nil {
						cmp.comparisonCommitted(next.index)
					}
				}
				leaderIdx, leaderSign, found := settleEvidence(updated)
				if found || len(active) == 0 {
					drained := drain()
					countDiscarded(drained)
					if found {
						reportedComparisons := comparisons + discardedComparisons
						reportedParticipations := participations + discardedParticipations
						cadence.ReportFinal(
							reportedComparisons, reportedParticipations, deliver)
						items, detail := diagnostics()
						committed = &OutlierResult{
							Outcome: OutlierFound, Index: leaderIdx,
							OutlierGuarantee: foundGuarantee(leaderSign),
							Cost: Cost{
								Comparisons:    reportedComparisons,
								Participations: reportedParticipations,
							},
							Items: items, Detail: detail,
						}
					}
					break
				}
				fillWindow()
			}
			if launched > comparisons {
				drain()
			}
		} else {
			for launched < trancheEnd && len(active) > 0 && advanceCtx.Err() == nil {
				waveStart := launched
				waveCapacity := min(w, trancheEnd-launched)
				nominee, _, nominationEvidence := leaderOf(active)
				confirmThisWave := f.adaptiveCleanConfirmation &&
					discoverySinceConfirmation && nominee >= 0 &&
					nominationEvidence >= baselineConfirmationNominationGate
				if confirmThisWave {
					discoverySinceConfirmation = false
				}
				for waveIndex := range waveCapacity {
					confirming := -1
					var batch []ItemID
					if confirmThisWave && waveIndex == 0 {
						confirming = nominee
						batch = sampleConfirmation(nominee)
					} else {
						batch = sampleComparison()
						discoverySinceConfirmation = true
					}
					if batch == nil {
						break
					}
					launched++
					go func(index int, sent []ItemID, confirmed int) {
						ranks, err := cmp.Compare(advanceCtx, sent)
						results <- cmpOutcome{
							index: index, comparison: sent, ranks: ranks,
							confirming: confirmed, err: err,
						}
					}(waveIndex, batch, confirming)
				}
				waveSize := launched - waveStart
				wave := make([]cmpOutcome, waveSize)
				for range waveSize {
					outcome := <-results
					wave[outcome.index] = outcome
					if outcome.err != nil {
						cancelAdvance()
					}
				}
				if command.ctx.Err() != nil {
					loopErr = command.ctx.Err()
				} else if lifetime.Err() != nil {
					loopErr = lifetime.Err()
				}
				if loopErr == nil {
					// Prefer the lowest-index substantive failure. Cancellation errors
					// induced while draining sibling calls are only a fallback.
					for _, substantive := range []bool{true, false} {
						for index, outcome := range wave {
							isCancellation := errors.Is(outcome.err, context.Canceled)
							if outcome.err != nil && isCancellation != substantive {
								loopErr = fmt.Errorf(
									"ranking: %s: comparison %d failed: %w",
									operation, waveStart+index, outcome.err)
								break
							}
						}
						if loopErr != nil {
							break
						}
					}
				}
				if loopErr != nil {
					break
				}

				updated := applyOutcomes(wave)
				leaderIdx, leaderSign, found := settleEvidence(updated)
				if found {
					cadence.ReportFinal(comparisons, participations, deliver)
					items, detail := diagnostics()
					committed = &OutlierResult{
						Outcome: OutlierFound, Index: leaderIdx,
						OutlierGuarantee: foundGuarantee(leaderSign),
						Cost:             Cost{Comparisons: comparisons, Participations: participations},
						Items:            items, Detail: detail,
					}
					break
				}
			}
		}

		if loopErr == nil {
			switch {
			case command.ctx.Err() != nil:
				loopErr = command.ctx.Err()
			case lifetime.Err() != nil:
				loopErr = lifetime.Err()
			}
		}
		stopLifetimeCancel()
		cancelAdvance()
		var response outlierRunReply
		switch {
		case committed != nil:
			response.result = *committed
		case loopErr != nil:
			response = outlierRunReply{result: fail, err: loopErr}
		case len(active) == 0:
			// The whole population has been eliminated: a trustworthy negative. Had a
			// target meeting MinimumEdge been present, it would have been eliminated
			// only with probability <= betaElim.
			reportedComparisons := comparisons + discardedComparisons
			reportedParticipations := participations + discardedParticipations
			cadence.ReportFinal(reportedComparisons, reportedParticipations, deliver)
			items, detail := diagnostics()
			response.result = OutlierResult{
				Outcome:          OutlierNegative,
				Index:            -1,
				OutlierGuarantee: negativeGuarantee(),
				Cost: Cost{
					Comparisons:    reportedComparisons,
					Participations: reportedParticipations,
				},
				Items: items, Detail: detail,
			}
		default:
			// This tranche ended without a commit or full elimination. Report a paused
			// snapshot with no guarantee while retaining all evidence for another
			// Advance.
			cadence.reportBoundary(comparisons, participations, deliver)
			items, detail := diagnostics()
			response.result = OutlierResult{
				Outcome: OutlierPaused, Index: -1,
				Cost:  Cost{Comparisons: comparisons, Participations: participations},
				Items: items, Detail: detail,
			}
		}
		command.reply <- response
		if response.err != nil || response.result.Outcome != OutlierPaused {
			return
		}
	}
}

// outlierDefaultComparisonLimit keeps the default's fixed-width participation
// total representable on the current architecture.
func outlierDefaultComparisonLimit(width int) int {
	return min(OutlierDefaultMaxComparisons, math.MaxInt/width)
}

// outlierEvidence allocates per-item log wealth for a fixed betting mixture.
func outlierEvidence(items, components int) [][]float64 {
	flat := make([]float64, items*components)
	out := make([][]float64, items)
	for i := range out {
		out[i] = flat[i*components : (i+1)*components]
	}
	return out
}

// outlierBettingMixture spans subtle through strong positive effects without
// using MinimumEdge. Each fixed component is an e-process, so
// their weighted mixture is one too.
func outlierBettingMixture() ([]float64, []float64) {
	weights := [...]float64{0.05, 0.10, 0.20, 0.25, 0.20, 0.15, 0.05}
	bets := []float64{1.0 / 32, 1.0 / 16, 1.0 / 8, 1.0 / 4, 1.0 / 2, 1, math.Nextafter(2.0, 0)}
	logWeights := make([]float64, bettingComponents)
	for i := range logWeights {
		logWeights[i] = math.Log(weights[i])
	}
	return bets, logWeights
}

// outlierLogMixture returns log(sum_j weight_j*exp(logValues_j)).
func outlierLogMixture(logValues, logWeights []float64) float64 {
	peak := math.Inf(-1)
	for i, value := range logValues {
		peak = max(peak, value+logWeights[i])
	}
	if math.IsInf(peak, -1) {
		return peak
	}
	var scaled float64
	for i, value := range logValues {
		scaled += math.Exp(value + logWeights[i] - peak)
	}
	return peak + math.Log(scaled)
}

// outlierBetFactor evaluates a bounded-betting factor without rounding a
// mathematically positive endpoint factor to zero. Bets clipped below their
// reciprocal payoff boundary rely on that strict inequality.
func outlierBetFactor(bet, payoff float64) float64 {
	return math.FMA(bet, payoff, 1)
}

// outlierControlReserve chooses the clean-control count that maximises total
// effective rank information across all remaining candidate positions.
func outlierControlReserve(width, available int) int {
	available = min(available, width-1)
	best, bestInformation := 0, 0.0
	for controls := 1; controls <= available; controls++ {
		information := float64(width-controls) * stats.RankEffectiveN(controls)
		if information >= bestInformation {
			best, bestInformation = controls, information
		}
	}
	return best
}

// outlierRankVariance tracks one candidate's normalised-rank variance. Its
// state is read before a wave and updated only after the wave, making every bet
// predictable even when sibling comparisons are dependent.
type outlierRankVariance struct {
	observations int
	mean, m2     float64
}

func (v outlierRankVariance) bet(edge float64, opponents int) float64 {
	// Eight uniform-control-subrank pseudo-observations stabilise startup. As
	// evidence accumulates, the observed variance replaces that working model;
	// the estimate affects efficiency only because clipping makes every bet valid.
	const priorObservations = 8.0
	priorVariance := float64(opponents+2) / (12 * float64(opponents))
	variance := (priorObservations*priorVariance + v.m2) /
		(priorObservations + float64(v.observations))
	return min(edge/variance, math.Nextafter(1/(0.5-edge), 0))
}

func (v *outlierRankVariance) observe(x float64) {
	v.observations++
	delta := x - v.mean
	v.mean += delta / float64(v.observations)
	v.m2 += delta * (x - v.mean)
}
