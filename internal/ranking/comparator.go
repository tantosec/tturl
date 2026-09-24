package ranking

import (
	"context"
	"fmt"
	"maps"
	"math"

	"golang.org/x/time/rate"
)

// ItemID identifies an item in a comparison. Non-negative values are candidate
// indices in [0, M); negative values identify controls declared by
// [Profile.Controls]. Construct control IDs with [Control].
type ItemID int

// ControlKind classifies a control reference by the behaviour the channel
// guarantees for it. External solver families may define additional values
// representable by [Control].
type ControlKind uint

const (
	// Baseline is a control known to behave as a non-target.
	Baseline ControlKind = iota
	// Positive is a control known to behave as a target.
	Positive
)

// ControlSupply describes how many controls of one kind a channel can place in
// a comparison. The zero value supplies none.
type ControlSupply struct {
	bound     int
	unbounded bool
}

// BoundedControls declares a channel can supply at most n controls of a kind in
// one comparison; n <= 0 supplies none.
func BoundedControls(n int) ControlSupply { return ControlSupply{bound: max(0, n)} }

// UnboundedControls declares that a channel can fill any number of positions
// with controls of one kind.
func UnboundedControls() ControlSupply { return ControlSupply{unbounded: true} }

// UpTo returns the number of controls available for at most want positions. It
// returns zero when want is non-positive.
func (s ControlSupply) UpTo(want int) int {
	if want <= 0 {
		return 0
	}
	if s.unbounded {
		return want
	}
	return min(s.bound, want)
}

// controlKindBits reserves four low bits of a control ID for its kind.
const controlKindBits = 4

// Control returns the ID of the zero-based index-th control of kind. Distinct
// indices denote distinct instances, but do not imply statistical independence.
// Control panics if kind or index cannot be represented.
func Control(kind ControlKind, index int) ItemID {
	if kind >= 1<<controlKindBits {
		panic(fmt.Sprintf("ranking: Control: kind %d is not representable", kind))
	}
	if index < 0 || index > math.MaxInt>>controlKindBits {
		panic(fmt.Sprintf("ranking: Control: index %d is not representable", index))
	}
	return ItemID(-((index << controlKindBits) | int(kind)) - 1)
}

// IsControl reports whether c names a control reference rather than a
// candidate.
func (c ItemID) IsControl() bool { return c < 0 }

// IsCandidate reports whether c names a candidate — an index in [0, M).
func (c ItemID) IsCandidate() bool { return c >= 0 }

// ControlKind returns the kind of a control ID. Its result is undefined for a
// candidate ID.
func (c ItemID) ControlKind() ControlKind {
	return ControlKind(uint(-c-1) & ((1 << controlKindBits) - 1))
}

// ControlIndex returns a control ID's zero-based index within its kind. Its
// result is undefined for a candidate ID.
func (c ItemID) ControlIndex() int {
	return int(uint(-c-1) >> controlKindBits)
}

// CompareFunc compares items arranged by position. The returned ranks are
// indexed by the same positions, with zero denoting first place. Ranks must be
// a tie-free permutation of [0, len(byPosition)).
//
// CompareFunc must not mutate the supplied arrangement and must instantiate
// every control promised by the paired [Profile]. A non-nil error means that no
// comparison was obtained. When [Profile.MaxConcurrency] exceeds one, the
// function must support that many concurrent calls and return independently
// backed rank slices.
type CompareFunc func(ctx context.Context, byPosition []ItemID) (ranks []int, err error)

// Profile describes a comparison channel's capacity, controls, and pacing
// limits. A solver reads the profile once at the start of a solve.
type Profile struct {
	_ struct{} // keep literals keyed

	// MaxWidth is the largest comparison width the channel accepts. It must be at
	// least 2; solvers may use any width in [2, MaxWidth].
	MaxWidth int

	// MaxConcurrency is the maximum number of comparisons permitted in flight.
	// Zero and one both mean sequential operation. Comparator enforces this
	// ceiling; solvers may use fewer.
	MaxConcurrency int

	// MaxParticipationsPerSecond and MaxComparisonsPerSecond limit issue rates.
	// Zero means unlimited. A participation is one filled position.
	MaxParticipationsPerSecond float64
	MaxComparisonsPerSecond    float64

	// Controls gives the per-comparison supply of each control kind. A nil map or
	// missing entry supplies none. Each supplied control must be a distinct
	// instance; comparisons always contain at least one candidate.
	Controls map[ControlKind]ControlSupply
}

// Comparator binds a [CompareFunc] to a [Profile] and enforces the profile's
// concurrency and pacing limits. The solver supplies the position of every
// item.
type Comparator struct {
	compare CompareFunc
	// compareIndexed is an evaluation seam for channels whose randomness must
	// be bound to the rolling solver's logical dispatch order. Production
	// comparators leave it nil and use compare.
	compareIndexed func(context.Context, []ItemID, int) ([]int, error)
	// comparisonCommitted pairs with compareIndexed in deterministic rolling
	// evaluations. Production comparators leave it nil.
	comparisonCommitted func(int)
	profile             Profile
	profileErr          error
	partLim             *rate.Limiter // participations/sec, nil if unlimited
	cmpLim              *rate.Limiter // comparisons/sec, nil if unlimited
	admit               chan struct{} // one token per admitted comparison
}

// NewComparator returns a comparator that invokes compare according to profile.
// It takes an immutable copy of the profile and its Controls map. Compare and
// solvers report a nil compare function or invalid profile before dispatch.
func NewComparator(profile Profile, compare CompareFunc) *Comparator {
	profile.Controls = cloneControls(profile.Controls)
	c := &Comparator{
		compare: compare, profile: profile,
		profileErr: validateComparatorProfile(profile, compare),
	}
	if c.profileErr != nil {
		return c
	}
	c.admit = make(chan struct{}, max(profile.MaxConcurrency, 1))
	burst := max(profile.MaxWidth, 1)
	if profile.MaxParticipationsPerSecond > 0 {
		c.partLim = rate.NewLimiter(rate.Limit(profile.MaxParticipationsPerSecond), burst)
	}
	if profile.MaxComparisonsPerSecond > 0 {
		c.cmpLim = rate.NewLimiter(rate.Limit(profile.MaxComparisonsPerSecond), 1)
	}
	return c
}

func validateComparatorProfile(profile Profile, compare CompareFunc) error {
	if compare == nil {
		return fmt.Errorf("ranking: comparator: CompareFunc is nil")
	}
	if profile.MaxWidth < 2 {
		return fmt.Errorf(
			"ranking: comparator: MaxWidth must be >= 2, got %d",
			profile.MaxWidth,
		)
	}
	if profile.MaxConcurrency < 0 {
		return fmt.Errorf(
			"ranking: comparator: MaxConcurrency must be >= 0, got %d",
			profile.MaxConcurrency,
		)
	}
	for _, rateLimit := range []struct {
		name  string
		value float64
	}{
		{"MaxParticipationsPerSecond", profile.MaxParticipationsPerSecond},
		{"MaxComparisonsPerSecond", profile.MaxComparisonsPerSecond},
	} {
		if rateLimit.value < 0 || math.IsNaN(rateLimit.value) ||
			math.IsInf(rateLimit.value, 0) {
			return fmt.Errorf(
				"ranking: comparator: %s must be finite and >= 0, got %v",
				rateLimit.name,
				rateLimit.value,
			)
		}
	}
	return nil
}

// Profile returns an independent copy of the channel's declared driving
// parameters.
func (c *Comparator) Profile() Profile {
	profile := c.profile
	profile.Controls = cloneControls(profile.Controls)
	return profile
}

func cloneControls(controls map[ControlKind]ControlSupply) map[ControlKind]ControlSupply {
	if controls == nil {
		return nil
	}
	clone := make(map[ControlKind]ControlSupply, len(controls))
	maps.Copy(clone, controls)
	return clone
}

// Compare validates the arrangement, waits for concurrency admission and then
// the applicable pacing limits, invokes the channel, and validates that its
// ranks form a tie-free permutation. Waiting for admission is cancellable and
// does not consume pacing capacity. An arrangement must contain a candidate
// and between two and Profile.MaxWidth distinct items. Controls must be
// declared by the profile and fall within their supply. Invalid arrangements
// never reach CompareFunc.
func (c *Comparator) Compare(ctx context.Context, byPosition []ItemID) ([]int, error) {
	return c.compareAt(ctx, byPosition, -1)
}

// compareAt is Compare with an optional logical dispatch index. Only rolling
// execution supplies an index; ordinary channels ignore it. The evaluation
// fixture uses it to make concurrent simulated outcomes exactly replayable.
func (c *Comparator) compareAt(
	ctx context.Context, byPosition []ItemID, dispatchIndex int,
) ([]int, error) {
	if err := c.validateArrangement(byPosition); err != nil {
		return nil, err
	}
	select {
	case c.admit <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-c.admit
		return nil, err
	}
	defer func() { <-c.admit }()
	if c.partLim != nil {
		if err := c.partLim.WaitN(ctx, len(byPosition)); err != nil {
			return nil, err
		}
	}
	if c.cmpLim != nil {
		if err := c.cmpLim.WaitN(ctx, 1); err != nil {
			return nil, err
		}
	}
	var ranks []int
	var err error
	if dispatchIndex >= 0 && c.compareIndexed != nil {
		ranks, err = c.compareIndexed(ctx, byPosition, dispatchIndex)
	} else {
		ranks, err = c.compare(ctx, byPosition)
	}
	if err != nil {
		return nil, err
	}
	// Enforce the CompareFunc rank postcondition here, at the channel seam it
	// belongs to, so every solver is handed a validated total order: ranks must
	// be a dense, tie-free permutation of [0, width).
	if !isRankPermutation(ranks, len(byPosition)) {
		return nil, fmt.Errorf("ranking: comparator: CompareFunc returned ranks that are not a "+
			"permutation of [0, %d): %v", len(byPosition), ranks)
	}
	return ranks, nil
}

func (c *Comparator) validateArrangement(items []ItemID) error {
	if c.profileErr != nil {
		return c.profileErr
	}
	width := len(items)
	if width < 2 || width > c.profile.MaxWidth {
		return fmt.Errorf(
			"ranking: comparator: comparison width %d is outside [2, %d]",
			width,
			c.profile.MaxWidth,
		)
	}
	seen := make(map[ItemID]struct{}, width)
	hasCandidate := false
	for position, item := range items {
		if _, ok := seen[item]; ok {
			return fmt.Errorf(
				"ranking: comparator: duplicate item %d at position %d",
				item,
				position,
			)
		}
		seen[item] = struct{}{}
		if item.IsCandidate() {
			hasCandidate = true
			continue
		}
		kind, index := item.ControlKind(), item.ControlIndex()
		supply, declared := c.profile.Controls[kind]
		if !declared || supply.UpTo(index+1) <= index {
			return fmt.Errorf(
				"ranking: comparator: control kind %d index %d at position %d is not supplied",
				kind,
				index,
				position,
			)
		}
	}
	if !hasCandidate {
		return fmt.Errorf("ranking: comparator: comparison contains no candidate")
	}
	return nil
}

// isRankPermutation reports whether ranks is a permutation of [0, n).
func isRankPermutation(ranks []int, n int) bool {
	if len(ranks) != n {
		return false
	}
	if n <= 64 {
		var seen uint64
		for _, r := range ranks {
			if r < 0 || r >= n {
				return false
			}
			if seen&(uint64(1)<<r) != 0 {
				return false
			}
			seen |= uint64(1) << r
		}
		return true
	}
	seen := make([]bool, n)
	for _, r := range ranks {
		if r < 0 || r >= n || seen[r] {
			return false
		}
		seen[r] = true
	}
	return true
}
