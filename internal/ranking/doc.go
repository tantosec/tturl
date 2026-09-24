// Package ranking infers order from noisy ordinal comparisons. It provides a
// shared comparison substrate on which distinct solver families can answer
// different questions about a population. A solver arranges items by position
// through a [Comparator] and observes only their finishing ranks.
//
// Solvers own item positioning. This lets a solver account for position bias
// without requiring the comparison channel to understand its evidence model.
// Outlier identification is the package's current solver family, and
// [OutlierSolver] is that family's single-result answer shape. It is not a
// universal solver interface: a future top-n, partial-order, or other family
// should reuse comparison primitives while defining the request, result, and
// guarantee vocabulary its question requires.
//
// Solver family, concrete algorithm, inferential regime, and execution policy
// are separate choices. The current outlier implementations use anytime-valid
// evidence processes at their documented atomic updates: complete waves or
// complete dispatch prefixes. That is an implementation guarantee, not a
// requirement that every future ranking solver use the same inferential method.
// A solver must state the assumptions and stopping rules under which its
// conclusions are valid.
//
// An outlier solve returns one qualifying item, not every qualifying item.
// Multiple extremes are allowed when each scheduled observation retains the
// implementation's documented conditional-mean premise. Baseline-relative
// evidence isolates candidates from one another; peer-relative evidence may
// be masked or distorted by same-direction or opposing extremes. Removing a
// result and restarting changes the reference population and is not a
// statistically free enumeration procedure: it spends a new error budget. A
// future set-valued solver should own simultaneous coverage and evidence reuse.
//
// # Choosing an outlier solver
//
// Start with the evidence premise:
//
//   - If the peer-relative conditional-mean bound is not defensible, choose
//     [BaselineReservedOutlierSolver] with trustworthy Baseline controls. It
//     reserves controls from the first comparison and uses their clean
//     reference for both outcomes. This gives up some broad-field candidate
//     capacity for the most direct premise and comparatively inexpensive
//     negative decisions.
//   - Choose [RollingBaselineReservedOutlierSolver] for a sparse,
//     Baseline-controlled search when transport requires enough concurrency
//     that atomic waves leave too few adaptive updates. It keeps a bounded
//     dispatch window and consumes maximal dispatch prefixes, but requires the
//     Baseline conditional-mean bound to hold after conditioning on overlapping
//     lower-index outcomes. It does not tolerate arbitrary dependence within
//     the window.
//   - With a known direction and no Baselines, Baseline Reserved and
//     [PeerFirstOutlierSolver] use the same peer-first allocation. When
//     Baselines are available, Peer First instead spends every broad-field
//     position on candidates and defers controls until the field contracts.
//     This can reduce discovery work, but both outcomes depend on every
//     scheduled peer set preserving the documented bound. Choose
//     [RollingPeerFirstOutlierSolver] when this policy also needs rolling
//     transport occupancy; it adds the conditional-across-dispatch-indices
//     premise and remains exposed only as an explicit method.
//   - Choose [BaselineConfirmedOutlierSolver] when clean positive attribution
//     matters enough to pay for confirmation. It requires Baselines. For a
//     known direction it discovers peer-first, confirms a find against
//     Baselines, and retains the peer-relative premise for its negative.
//     [Either] mode is Baseline-relative throughout. Choose
//     [RollingBaselineConfirmedOutlierSolver] when the same discovery and clean
//     commit split needs rolling occupancy. It permits only one pending clean
//     confirmation and requires committed discovery between confirmations.
//   - An [Either] request requires Baselines and makes Peer First use the
//     baseline-reserved allocation.
//   - Choose [EdgeDirectedOutlierSolver] when MinimumEdge is a meaningful
//     positive design target and peer comparability is defensible. It uses the
//     peer-first allocation and aims its positive bet at that edge; the
//     adaptive mixture in the other strategies is less committed to one effect
//     size. MinimumEdge on a found result describes the evidence design, not a
//     lower bound on the target's effect.
//
// All implementations return at most one target. Their type documentation
// states how additional targets affect each outcome's premise. Their
// implementation diagnostics are [OutlierProgressDiagnostics] and
// [OutlierDiagnostics].
//
// # Extension boundary
//
// [OutlierSolver] is implemented by both package and third-party outlier
// methods. Its Detail fields deliberately accept only package diagnostics;
// external implementations leave them nil and expose any method-specific
// instrumentation through their own API. External solvers may define their
// own stable, domain-neutral [OutlierEvidencePremise] values.
//
// A future top-n family should define, for example, its own request containing
// M and N and a result containing a set of indices with a simultaneous
// guarantee. It can reuse [Comparator], [Profile], [ItemID], [Cost], and
// [ProgressCore], but should not implement [OutlierSolver], return a sentinel
// single index, or accept outlier-only direction and negative-edge settings.
//
// # Execution ownership
//
// A [Comparator] enforces its profile's concurrency and pacing limits for
// every caller. A built-in outlier solver's MaxConcurrency may select a lower
// ceiling because concurrency changes how it preplans and combines evidence.
// Comparator backpressure is a channel safety boundary, not a substitute for
// statistically valid scheduling: a solver must bound its own launches and
// define the atomic unit at which outcomes may affect later work. Atomic-wave
// solvers combine every concurrently planned wave before adapting. The rolling
// solvers bound their uncommitted dispatch window and adapt only after folding
// a maximal dispatch prefix.
//
// # Inferential regimes
//
// [OutlierSolver.Inference] lets a consumer check stopping semantics before
// execution. A controlled result repeats that regime in
// [OutlierGuarantee.Regime]. The current fleet reports
// [OutlierInferenceAnytimeValid]: it may decide after any documented atomic
// evidence update, and progress callbacks remain descriptive rather than extra
// looks.
//
// A fixed-design solver reports [OutlierInferenceFixedDesign], runs its
// precommitted horizon inside Identify, and returns no decision at an early
// cancellation or progress callback. A group-sequential solver reports
// [OutlierInferenceScheduledLooks] and decides only at its preregistered
// looks; an execution pause between them is not a decision boundary. An
// uncontrolled ranker reports [OutlierInferenceUncontrolled], may ignore
// requested error risks, and leaves OutlierGuarantee zero even when it returns
// a found or negative suggestion. Consumers that require adaptive retries,
// controlled error, or arbitrary resumption must reject incompatible regimes
// before starting rather than reinterpret their results afterwards.
//
// # Resumable execution
//
// The atomic-wave built-in solvers also implement [ResumableOutlierSolver].
// Start binds one in-memory [OutlierRun] to the complete request, comparator,
// and solver configuration. Each positive [Budget] passed to Advance
// authorises at most that many additional physical comparisons. Costs and item
// statistics are cumulative. [OutlierPaused] is a healthy, nonterminal
// snapshot; callers may advance that same run or close it. Found, negative,
// and errors are terminal. Identify is the one-shot convenience and maps its
// budget pause to [OutlierInconclusive]. Rolling implementations are currently
// one-shot because a pause with outstanding dispatches needs a separate public
// lifecycle contract.
//
// An OutlierRun permits one active Advance. Close is idempotent, cancels active
// comparison work, and must be called for every nonterminal run. A caller
// retaining runs for retries should key them by its own explicit decision
// identity, discard them when the ordered field or semantics change, and
// charge downstream consumers only the cost delta between cumulative
// snapshots.
//
// A tranche boundary completes every comparison it launched; it never selects
// favourable siblings from a partial wave. With concurrency greater than one,
// tranche sizes can change the partition into atomic waves and thus the valid
// randomised allocation path. Exact one-tranche versus many-tranche replay is
// guaranteed when MaxConcurrency is one. Concurrent replay requires the same
// wave partition; different partitions remain anytime-valid but need not
// produce identical descriptive state or stopping cost.
package ranking
