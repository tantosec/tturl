package ranking

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
)

const (
	// OutlierDefaultMinimumEdge is the minimum normalised-rank advantage a
	// target must have for a negative result's guarantee when
	// [OutlierRequest.MinimumEdge] is zero. Positive evidence in adaptive-mixture
	// solvers does not use this edge.
	OutlierDefaultMinimumEdge = 0.02
)

func resolveOutlierRun(
	operation string, width, channelConcurrency int,
	minimumEdge float64, maxComparisons, maxConcurrency int,
) (edge float64, comparisons, concurrency int, err error) {
	if maxComparisons < 0 {
		return 0, 0, 0, fmt.Errorf(
			"ranking: %s: MaxComparisons must be >= 0, got %d",
			operation, maxComparisons)
	}
	edge = minimumEdge
	if edge < 0 {
		return 0, 0, 0, fmt.Errorf("ranking: %s: MinimumEdge must be >= 0 "+
			"(0 selects the default), got %v", operation, edge)
	}
	if edge == 0 {
		edge = OutlierDefaultMinimumEdge
	}
	if !(edge < 0.5) {
		return 0, 0, 0, fmt.Errorf(
			"ranking: %s: MinimumEdge must be in (0, 0.5), got %v",
			operation, edge)
	}
	comparisons = maxComparisons
	if comparisons == 0 {
		comparisons = outlierDefaultComparisonLimit(width)
	}
	if comparisons > math.MaxInt/width {
		return 0, 0, 0, fmt.Errorf(
			"ranking: %s: MaxComparisons (%d) at width %d overflows participation count",
			operation, comparisons, width)
	}
	if maxConcurrency < 0 {
		return 0, 0, 0, fmt.Errorf(
			"ranking: %s: MaxConcurrency must be >= 0, got %d",
			operation, maxConcurrency)
	}
	if channelConcurrency < 0 {
		return 0, 0, 0, fmt.Errorf(
			"ranking: %s: comparator MaxConcurrency must be >= 0, got %d",
			operation, channelConcurrency)
	}
	concurrency = max(channelConcurrency, 1)
	if maxConcurrency > 0 {
		concurrency = min(concurrency, maxConcurrency)
	}
	concurrency = min(concurrency, comparisons)
	return edge, comparisons, concurrency, nil
}

// OutlierRequest asks an [OutlierSolver] to identify a target and return at
// most one result. A population may contain multiple targets. Each solver
// documents how additional targets affect its evidence premises and power.
type OutlierRequest struct {
	_ struct{} // keep literals keyed

	// M is the population size. Candidate IDs range from 0 through M-1. It must be
	// at least 1. A population of one requires enough controls to fill a
	// comparison.
	M int
	// Direction is the extreme occupied by the target. Either means it is unknown.
	Direction OutlierDirection
	// MinimumEdge is the minimum normalised-rank advantage in (0, 1/2) covered
	// by a negative result. Zero selects [OutlierDefaultMinimumEdge]. Solvers
	// document whether it also shapes positive evidence.
	MinimumEdge float64
	// FalseFindRisk is the prospective family-wise risk of returning a non-target
	// under the solver's documented positive premise. A controlled solver
	// requires it to be finite and in (0, 1). An uncontrolled solver may define
	// another contract and must return a zero OutlierGuarantee.
	FalseFindRisk float64
	// FalseNegativeRisk is the prospective risk of ruling out a qualifying target
	// under the solver's documented negative premise and MinimumEdge. A controlled
	// solver requires it to be finite and in (0, 1).
	FalseNegativeRisk float64
	// Progress receives periodic snapshots and one final snapshot before every
	// non-error return. The solver calls it synchronously, so it must return
	// promptly and must not call lifecycle methods on the active run.
	Progress func(OutlierProgress)
	// ProgressCadence sets how often Progress receives a periodic snapshot; the
	// zero value delivers only the final one.
	ProgressCadence ProgressCadence
}

// OutlierProgress describes an [OutlierSolver]'s search at one point in time.
type OutlierProgress struct {
	_ struct{} // keep literals keyed

	ProgressCore
	// Active is the number of items still in contention.
	Active int
	// Leader is the strongest current candidate, or -1 when none remains. It is
	// not a decision until returned in an [OutlierResult].
	Leader int
	// Direction is the extreme toward which Leader tends. It is meaningful only
	// when Leader is non-negative.
	Direction OutlierDirection
	// Detail is a supported implementation-specific progress value, or nil.
	// Implementations return values rather than pointers.
	Detail OutlierProgressDetail
}

// OutlierProgressDetail is a detail value supported by an OutlierSolver's
// progress snapshots. The interface is closed to package implementations so
// consumers can exhaustively inspect the documented concrete values.
type OutlierProgressDetail interface {
	progressDetail()
}

// OutlierOutcome is the disposition of an outlier solve or incremental
// advance. Failures are returned as errors. Its zero value is
// [OutlierInconclusive].
type OutlierOutcome int

const (
	// OutlierInconclusive means the search exhausted its budget without
	// resolving: no target committed and the population not fully eliminated. No
	// guarantee holds and [OutlierResult.OutlierGuarantee] is zero.
	OutlierInconclusive OutlierOutcome = iota
	// OutlierFound means a target was identified: [OutlierResult.Index] names it
	// and a nonzero OutlierGuarantee controls false finds. An uncontrolled
	// solver may return a found suggestion with a zero guarantee.
	OutlierFound
	// OutlierNegative means no target satisfying the solver's documented negative
	// premise and edge is present: the population was fully eliminated and its
	// nonzero OutlierGuarantee controls false negatives. An uncontrolled solver
	// may return a negative suggestion with a zero guarantee.
	OutlierNegative
	// OutlierPaused means an incremental run consumed the supplied tranche
	// without resolving. Its evidence remains live and may be advanced again.
	// No guarantee holds and [OutlierResult.OutlierGuarantee] is zero. Identify
	// converts this state to OutlierInconclusive because it does not return a run.
	OutlierPaused
)

// String returns the outcome's stable descriptive name. It formats an unknown
// value as "OutlierOutcome(n)".
func (o OutlierOutcome) String() string {
	switch o {
	case OutlierInconclusive:
		return "Inconclusive"
	case OutlierFound:
		return "Found"
	case OutlierNegative:
		return "Negative"
	case OutlierPaused:
		return "Paused"
	default:
		return "OutlierOutcome(" + strconv.Itoa(int(o)) + ")"
	}
}

// OutlierResult is an OutlierSolver's answer or an OutlierRun snapshot.
type OutlierResult struct {
	_ struct{} // keep literals keyed

	// Outcome is the disposition. Controlled found and negative results carry an
	// error-risk guarantee; inconclusive and paused results do not.
	Outcome OutlierOutcome
	// Index is the identified target (an index in [0, M)) when Outcome is
	// OutlierFound, else -1.
	Index int
	OutlierGuarantee
	Cost
	// Items contains descriptive statistics indexed by candidate ID. It is
	// populated for every non-error result and nil on error.
	Items []ItemStat
	// Detail is a supported implementation-specific result value, or nil.
	// Implementations return values rather than pointers.
	Detail OutlierResultDetail
}

// OutlierErrorDirection identifies the error controlled by an
// [OutlierGuarantee]. Its zero value means no guarantee.
type OutlierErrorDirection uint8

const (
	// OutlierErrorUncontrolled means no probabilistic error guarantee is present.
	OutlierErrorUncontrolled OutlierErrorDirection = iota
	// OutlierErrorFalseFind means the error-risk limit bounds the probability of
	// returning a non-target as a target under the stated premise.
	OutlierErrorFalseFind
	// OutlierErrorFalseNegative means the error-risk limit bounds the probability
	// of ruling out a target satisfying the stated premise and minimum edge.
	OutlierErrorFalseNegative
)

// String returns a stable descriptive name for the controlled error.
func (d OutlierErrorDirection) String() string {
	switch d {
	case OutlierErrorUncontrolled:
		return "uncontrolled"
	case OutlierErrorFalseFind:
		return "false find"
	case OutlierErrorFalseNegative:
		return "false negative"
	default:
		return "OutlierErrorDirection(" + strconv.Itoa(int(d)) + ")"
	}
}

// OutlierEvidencePremise names the assumption under which an outlier guarantee
// holds. Built-in values name their conditional-mean reference. External
// solvers may define stable, domain-neutral values and document their exact
// sampling model, filtration, transformations, and bounds.
type OutlierEvidencePremise string

const (
	// OutlierPremiseUnspecified means no evidence premise is present.
	OutlierPremiseUnspecified OutlierEvidencePremise = ""
	// OutlierPremiseBaselineMean requires the relevant conditional-mean bound
	// against selected Baseline controls.
	OutlierPremiseBaselineMean OutlierEvidencePremise = "Baseline conditional mean"
	// OutlierPremiseScheduledReferenceMean requires the relevant conditional-mean
	// bound against every reference set actually scheduled, which may contain
	// candidate peers, Baseline controls, or both.
	OutlierPremiseScheduledReferenceMean OutlierEvidencePremise = "scheduled-reference conditional mean"
)

// String returns a stable descriptive name for the evidence premise.
func (p OutlierEvidencePremise) String() string {
	if p == OutlierPremiseUnspecified {
		return "unspecified"
	}
	return string(p)
}

// OutlierInferenceRegime states when a solver's controlled decisions are
// valid. It changes which stopping, retry, and resumption behaviour is safe.
type OutlierInferenceRegime uint8

const (
	// OutlierInferenceUncontrolled means the solver makes no probabilistic error
	// guarantee. Its conclusive result must carry a zero OutlierGuarantee.
	OutlierInferenceUncontrolled OutlierInferenceRegime = iota
	// OutlierInferenceAnytimeValid permits a decision after any complete atomic
	// evidence update documented by the solver.
	OutlierInferenceAnytimeValid
	// OutlierInferenceFixedDesign permits a decision only after the solver's
	// precommitted horizon.
	OutlierInferenceFixedDesign
	// OutlierInferenceScheduledLooks permits decisions only at the solver's
	// preregistered looks.
	OutlierInferenceScheduledLooks
)

// String returns a stable machine-oriented name for the inference regime.
func (r OutlierInferenceRegime) String() string {
	switch r {
	case OutlierInferenceUncontrolled:
		return "uncontrolled"
	case OutlierInferenceAnytimeValid:
		return "anytime-valid"
	case OutlierInferenceFixedDesign:
		return "fixed-design"
	case OutlierInferenceScheduledLooks:
		return "scheduled-looks"
	default:
		return "OutlierInferenceRegime(" + strconv.Itoa(int(r)) + ")"
	}
}

// OutlierGuarantee describes a result's controlled-error interpretation. Its
// zero value means no guarantee and accompanies an inconclusive result or a
// conclusive suggestion from an uncontrolled solver.
type OutlierGuarantee struct {
	_ struct{} // keep literals keyed

	ErrorDirection OutlierErrorDirection
	// ErrorRiskLimit is the prospective repeated-run error bound in (0, 1).
	// It is not a posterior probability that this result is wrong.
	ErrorRiskLimit float64
	// MinimumEdge is the normalised-rank advantage covered by the guarantee. It
	// is zero when a found result adapts to effects without a minimum edge.
	MinimumEdge float64
	// Direction is the searched direction. For a found Either result it is the
	// discovered direction; for a negative it remains Either.
	Direction OutlierDirection
	Premise   OutlierEvidencePremise
	// Regime identifies the stopping schedule under which this guarantee is
	// valid. It is never uncontrolled for a nonzero guarantee.
	Regime OutlierInferenceRegime
}

// Cost is the physical comparison work performed by a ranking operation.
// Comparisons counts completed comparator calls. Participations counts filled
// positions across those comparisons. Its zero value means no work.
type Cost struct {
	_ struct{} // keep literals keyed

	// Comparisons is the number of completed comparator calls.
	Comparisons int
	// Participations is the number of occupied item positions across completed
	// comparisons.
	Participations int
}

// OutlierResultDetail is a detail value supported by an OutlierSolver result.
// The interface is closed to package implementations so consumers can
// exhaustively inspect the documented concrete values.
type OutlierResultDetail interface {
	resultDetail()
}

// ItemStat contains descriptive statistics for one candidate.
type ItemStat struct {
	_ struct{} // keep literals keyed

	// Samples is the number of comparisons in which the item participated while
	// in contention.
	Samples int
	// MeanRank is the mean finishing rank (0 = first) over those Samples, or NaN
	// when Samples == 0. It is descriptive only: when the set of opponents varies
	// between comparisons, it is not a calibrated signal.
	MeanRank float64
}

// OutlierSolver returns at most one target from a comparator's ordinal feedback
// or a negative. OutlierGuarantee distinguishes controlled decisions from
// suggestions. A solver does not enumerate every target. Implementations
// document their statistical premises and need not be safe for concurrent use.
type OutlierSolver interface {
	// Inference reports the solver's regime without starting a solve. Consumers
	// must check it before applying adaptive stopping, retries, or resumption.
	Inference() OutlierInferenceRegime
	// Identify runs one solve to a decision or an inconclusive result. Progress
	// callbacks are descriptive and never create additional decision boundaries.
	Identify(ctx context.Context, req OutlierRequest, cmp *Comparator) (OutlierResult, error)
}

// OutlierMethodID identifies one version of an outlier solver's decision
// method. An ID is nonempty printable ASCII. It is opaque to consumers: risk,
// edge, resource limits, width, and concurrency are separate configuration and
// do not create distinct method IDs.
//
// The owner must change an ID when the same recorded inputs can produce
// different evidence construction or allocation, stopping, commit, or
// elimination behaviour, error guarantees, or replayed decisions. Refactors
// that preserve behaviour and presentation changes retain the ID. A new ID does
// not replace calibration or compatibility review.
type OutlierMethodID string

// Valid reports whether id satisfies the nonempty printable-ASCII contract.
func (id OutlierMethodID) Valid() bool {
	if id == "" {
		return false
	}
	for _, b := range []byte(id) {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	return true
}

// IdentifiedOutlierSolver is the optional capability for solvers that expose
// an immutable, versioned decision-method identity. Consumers that persist or
// audit decisions can require this interface without enlarging
// [OutlierSolver].
type IdentifiedOutlierSolver interface {
	OutlierSolver
	MethodID() OutlierMethodID
}

// Budget bounds one incremental ranking advance. Its zero value is invalid;
// callers must explicitly choose a positive amount of work.
type Budget struct {
	_ struct{} // keep literals keyed

	// Comparisons is the maximum additional physical comparisons to complete.
	Comparisons int
}

// ErrOutlierRunClosed reports an advance after a run was closed or resolved.
var ErrOutlierRunClosed = errors.New("ranking: outlier run is closed")

// ErrOutlierRunBusy reports overlapping advances on one run. A run permits one
// active caller so its evidence and randomised schedule have a single order.
var ErrOutlierRunBusy = errors.New("ranking: outlier run is already advancing")

// OutlierRun is one in-memory anytime-valid outlier process. Advance returns
// cumulative evidence and cost. Only a paused result keeps the run live; every
// other outcome or error closes it. Close is idempotent and cancels an active
// advance. Runs are bound to their initial request, comparator, and solver
// configuration.
type OutlierRun interface {
	Advance(context.Context, Budget) (OutlierResult, error)
	Close()
}

// ResumableOutlierSolver is the optional capability for outlier solvers whose
// evidence remains valid across arbitrary positive comparison tranches.
// Implementations must report [OutlierInferenceAnytimeValid].
type ResumableOutlierSolver interface {
	OutlierSolver
	// Start validates and binds a run without performing comparisons. The caller
	// must close every run that has not already returned a terminal result.
	Start(OutlierRequest, *Comparator) (OutlierRun, error)
}

// validate checks the request and comparator invariants shared by outlier
// solvers.
func (req OutlierRequest) validate(cmp *Comparator, operation string) error {
	switch {
	case cmp == nil:
		return fmt.Errorf("ranking: %s: cmp is nil", operation)
	case req.M < 1:
		return fmt.Errorf("ranking: %s: M must be >= 1, got %d", operation, req.M)
	case !(req.FalseFindRisk > 0 && req.FalseFindRisk < 1):
		return fmt.Errorf("ranking: %s: FalseFindRisk must be in (0, 1), got %v",
			operation, req.FalseFindRisk)
	case !(req.FalseNegativeRisk > 0 && req.FalseNegativeRisk < 1):
		return fmt.Errorf(
			"ranking: %s: FalseNegativeRisk must be in (0, 1), got %v",
			operation, req.FalseNegativeRisk)
	case req.Direction != Late && req.Direction != Early && req.Direction != Either:
		return fmt.Errorf("ranking: %s: invalid OutlierDirection %d",
			operation, int(req.Direction))
	case cmp.profileErr != nil:
		return cmp.profileErr
	case cmp.Profile().MaxWidth < 2:
		return fmt.Errorf(
			"ranking: %s: comparator MaxWidth must be >= 2, got %d",
			operation, cmp.Profile().MaxWidth,
		)
	}
	return nil
}
