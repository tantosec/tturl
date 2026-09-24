package ranking

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// identityCompare ranks each position by its index.
func identityCompare(_ context.Context, byPosition []ItemID) ([]int, error) {
	r := make([]int, len(byPosition))
	for i := range r {
		r[i] = i
	}
	return r, nil
}

// TestItemIDEncoding verifies that candidate and control IDs are disjoint and
// that control IDs round-trip their kind and index.
func TestItemIDEncoding(t *testing.T) {
	for _, i := range []int{0, 1, 5, 100} {
		c := ItemID(i)
		if !c.IsCandidate() || c.IsControl() {
			t.Errorf("candidate %d: IsCandidate=%v IsControl=%v", i, c.IsCandidate(), c.IsControl())
		}
		if int(c) != i {
			t.Errorf("candidate %d round-trips to %d", i, int(c))
		}
	}

	seen := map[ItemID]string{}
	for _, kind := range []ControlKind{Baseline, Positive} {
		for _, idx := range []int{0, 1, 2, 7, 100} {
			c := Control(kind, idx)
			if c.IsCandidate() || !c.IsControl() {
				t.Errorf("control (%d,%d): IsCandidate=%v IsControl=%v", kind, idx, c.IsCandidate(), c.IsControl())
			}
			if c.ControlKind() != kind || c.ControlIndex() != idx {
				t.Errorf("control (%d,%d) round-trips to (%d,%d)", kind, idx, c.ControlKind(), c.ControlIndex())
			}
			if prev, dup := seen[c]; dup {
				t.Errorf("control (%d,%d) collides with %s at ItemID %d", kind, idx, prev, int(c))
			}
			seen[c] = "ok"
		}
	}
}

func TestControlRejectsUnrepresentableInputs(t *testing.T) {
	for _, test := range []struct {
		name  string
		kind  ControlKind
		index int
	}{
		{"negative index", Baseline, -1},
		{"large kind", ControlKind(1 << controlKindBits), 0},
		{"large index", Baseline, math.MaxInt>>controlKindBits + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			deferred := false
			func() {
				defer func() { deferred = recover() != nil }()
				Control(test.kind, test.index)
			}()
			if !deferred {
				t.Error("Control did not panic")
			}
		})
	}
}

func TestControlSupplyUpTo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		supply ControlSupply
		want   []int
	}{
		{"zero", ControlSupply{}, []int{0, 0, 0, 0}},
		{"negative bound", BoundedControls(-1), []int{0, 0, 0, 0}},
		{"bounded", BoundedControls(2), []int{0, 0, 1, 2}},
		{"unbounded", UnboundedControls(), []int{0, 0, 1, 3}},
	} {
		wants := []int{-1, 0, 1, 3}
		for i, want := range wants {
			if got := tc.supply.UpTo(want); got != tc.want[i] {
				t.Errorf("%s: UpTo(%d) = %d, want %d", tc.name, want, got, tc.want[i])
			}
		}
	}
}

// TestComparatorProfileAndDelegate verifies profile retention, delegation, and
// channel error propagation.
func TestComparatorProfileAndDelegate(t *testing.T) {
	profile := Profile{
		MaxWidth:                   3,
		MaxConcurrency:             4,
		MaxParticipationsPerSecond: 20,
		MaxComparisonsPerSecond:    5,
		Controls:                   map[ControlKind]ControlSupply{Baseline: BoundedControls(2)},
	}
	c := NewComparator(profile, identityCompare)
	if got := c.Profile(); !reflect.DeepEqual(got, profile) {
		t.Errorf("Profile() = %+v, want %+v", got, profile)
	}
	ranks, err := c.Compare(context.Background(), []ItemID{0, 1, 2})
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(ranks) != 3 || ranks[0] != 0 || ranks[1] != 1 || ranks[2] != 2 {
		t.Errorf("ranks = %v, want [0 1 2]", ranks)
	}

	sentinel := errors.New("comparison unavailable")
	failing := NewComparator(Profile{MaxWidth: 2}, func(context.Context, []ItemID) ([]int, error) {
		return nil, sentinel
	})
	if _, err := failing.Compare(context.Background(), []ItemID{0, 1}); !errors.Is(err, sentinel) {
		t.Errorf("Compare error = %v, want sentinel error", err)
	}
}

func TestComparatorOwnsProfileControls(t *testing.T) {
	controls := map[ControlKind]ControlSupply{Baseline: BoundedControls(1)}
	comparator := NewComparator(Profile{MaxWidth: 2, Controls: controls}, identityCompare)

	controls[Baseline] = BoundedControls(2)
	if got := comparator.Profile().Controls[Baseline].UpTo(2); got != 1 {
		t.Fatalf("source mutation changed comparator profile: got %d controls", got)
	}

	returned := comparator.Profile()
	returned.Controls[Baseline] = BoundedControls(2)
	if got := comparator.Profile().Controls[Baseline].UpTo(2); got != 1 {
		t.Fatalf("returned profile mutation changed comparator: got %d controls", got)
	}
}

func TestComparatorProfileControlsAreIndependentConcurrently(t *testing.T) {
	controls := map[ControlKind]ControlSupply{Baseline: BoundedControls(1)}
	comparator := NewComparator(Profile{MaxWidth: 2, Controls: controls}, identityCompare)

	var group sync.WaitGroup
	for worker := range 16 {
		group.Go(func() {
			for i := range 100 {
				profile := comparator.Profile()
				profile.Controls[Baseline] = BoundedControls(worker + i + 2)
				if got := comparator.Profile().Controls[Baseline].UpTo(2); got != 1 {
					t.Errorf("live profile changed to %d controls", got)
					return
				}
			}
		})
	}
	group.Wait()
}

func TestComparatorCompareValidatesRanks(t *testing.T) {
	permutation := func(n int) []int {
		ranks := make([]int, n)
		for i := range ranks {
			ranks[i] = n - 1 - i
		}
		return ranks
	}
	duplicate := func(n int) []int {
		ranks := permutation(n)
		ranks[n-1] = ranks[n-2]
		return ranks
	}
	for _, tc := range []struct {
		name  string
		items []ItemID
		ranks []int
		valid bool
	}{
		{"small valid", make([]ItemID, 4), permutation(4), true},
		{"large valid", make([]ItemID, 65), permutation(65), true},
		{"short", make([]ItemID, 4), permutation(3), false},
		{"negative", make([]ItemID, 4), []int{0, 1, 2, -1}, false},
		{"out of range", make([]ItemID, 4), []int{0, 1, 2, 4}, false},
		{"small duplicate", make([]ItemID, 4), duplicate(4), false},
		{"large duplicate", make([]ItemID, 65), duplicate(65), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.items {
				tc.items[i] = ItemID(i)
			}
			cmp := NewComparator(Profile{MaxWidth: len(tc.items)},
				func(context.Context, []ItemID) ([]int, error) {
					return slices.Clone(tc.ranks), nil
				})
			got, err := cmp.Compare(context.Background(), tc.items)
			if tc.valid {
				if err != nil {
					t.Fatalf("Compare: %v", err)
				}
				if !slices.Equal(got, tc.ranks) {
					t.Errorf("ranks = %v, want %v", got, tc.ranks)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "not a permutation") {
				t.Errorf("Compare error = %v, want invalid-permutation error", err)
			}
			if got != nil {
				t.Errorf("ranks = %v, want nil on invalid output", got)
			}
		})
	}
}

func TestComparatorCompareRejectsInvalidArrangementBeforeDispatch(t *testing.T) {
	profile := Profile{
		MaxWidth: 3,
		Controls: map[ControlKind]ControlSupply{
			Baseline: BoundedControls(1),
		},
	}
	for _, test := range []struct {
		name    string
		profile Profile
		items   []ItemID
		wantErr string
	}{
		{"nil CompareFunc", profile, []ItemID{0, 1}, "CompareFunc is nil"},
		{"invalid profile width", Profile{MaxWidth: 1}, []ItemID{0, 1}, "MaxWidth"},
		{
			"negative concurrency",
			Profile{MaxWidth: 2, MaxConcurrency: -1},
			[]ItemID{0, 1},
			"MaxConcurrency",
		},
		{
			"negative participation rate",
			Profile{MaxWidth: 2, MaxParticipationsPerSecond: -1},
			[]ItemID{0, 1},
			"MaxParticipationsPerSecond",
		},
		{
			"NaN comparison rate",
			Profile{MaxWidth: 2, MaxComparisonsPerSecond: math.NaN()},
			[]ItemID{0, 1},
			"MaxComparisonsPerSecond",
		},
		{
			"infinite comparison rate",
			Profile{MaxWidth: 2, MaxComparisonsPerSecond: math.Inf(1)},
			[]ItemID{0, 1},
			"MaxComparisonsPerSecond",
		},
		{"empty", profile, nil, "width"},
		{"singleton", profile, []ItemID{0}, "width"},
		{"too wide", profile, []ItemID{0, 1, 2, 3}, "width"},
		{"duplicate candidate", profile, []ItemID{0, 0}, "duplicate item"},
		{
			"duplicate control", profile,
			[]ItemID{0, Control(Baseline, 0), Control(Baseline, 0)},
			"duplicate item",
		},
		{
			"undeclared control", profile,
			[]ItemID{0, Control(Positive, 0)},
			"not supplied",
		},
		{
			"control beyond supply", profile,
			[]ItemID{0, Control(Baseline, 1)},
			"not supplied",
		},
		{
			"controls only",
			Profile{
				MaxWidth: 3,
				Controls: map[ControlKind]ControlSupply{
					Baseline: BoundedControls(2),
				},
			},
			[]ItemID{Control(Baseline, 0), Control(Baseline, 1)},
			"no candidate",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			var compare CompareFunc
			if test.name != "nil CompareFunc" {
				compare = func(context.Context, []ItemID) ([]int, error) {
					called = true
					return nil, nil
				}
			}
			cmp := NewComparator(test.profile, compare)
			got, err := cmp.Compare(t.Context(), test.items)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Compare error = %v, want category containing %q", err, test.wantErr)
			}
			if got != nil || called {
				t.Fatalf("ranks/called = %v/%t, want nil/false", got, called)
			}
		})
	}
}

func TestComparatorCompareAcceptsMaximumUnboundedControl(t *testing.T) {
	cmp := NewComparator(Profile{
		MaxWidth: 2,
		Controls: map[ControlKind]ControlSupply{
			Baseline: UnboundedControls(),
		},
	}, identityCompare)
	items := []ItemID{0, Control(Baseline, math.MaxInt>>controlKindBits)}
	ranks, err := cmp.Compare(t.Context(), items)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ranks, []int{0, 1}) {
		t.Fatalf("ranks = %v, want [0 1]", ranks)
	}
}

// TestComparatorPacesParticipations verifies participation-rate limiting. At
// width two, the initial two-token burst is free and the remainder establishes
// the minimum expected duration.
func TestComparatorPacesParticipations(t *testing.T) {
	const rps, comparisons = 50.0, 6 // floor (12-2)/50 = 200ms
	c := NewComparator(Profile{
		MaxWidth:                   2,
		MaxParticipationsPerSecond: rps,
		MaxComparisonsPerSecond:    1e9,
	}, identityCompare)
	ctx := context.Background()
	start := time.Now()
	for range comparisons {
		if _, err := c.Compare(ctx, []ItemID{0, 1}); err != nil {
			t.Fatalf("Compare: %v", err)
		}
	}
	if elapsed, lower := time.Since(start), 150*time.Millisecond; elapsed < lower {
		t.Errorf("elapsed %v, want >= %v (rate %g participations/s)", elapsed, lower, rps)
	}
}

// TestComparatorPacesComparisons verifies that the tighter configured rate
// limit governs issue time.
func TestComparatorPacesComparisons(t *testing.T) {
	const rps, comparisons = 50.0, 6 // floor (6-1)/50 = 100ms
	c := NewComparator(Profile{
		MaxWidth:                   2,
		MaxComparisonsPerSecond:    rps,
		MaxParticipationsPerSecond: 1e9, // far looser: must not bind
	}, identityCompare)
	ctx := context.Background()
	start := time.Now()
	for range comparisons {
		if _, err := c.Compare(ctx, []ItemID{0, 1}); err != nil {
			t.Fatalf("Compare: %v", err)
		}
	}
	if elapsed, lower := time.Since(start), 70*time.Millisecond; elapsed < lower {
		t.Errorf("elapsed %v, want >= %v (rate %g comparisons/s)", elapsed, lower, rps)
	}
}

// TestComparatorPacingCancelled verifies that cancellation interrupts a pacing
// wait before delegation.
func TestComparatorPacingCancelled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile Profile
	}{
		{"participations", Profile{MaxWidth: 2, MaxParticipationsPerSecond: 0.001}},
		{"comparisons", Profile{MaxWidth: 2, MaxComparisonsPerSecond: 0.001}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The first call spends the burst. At this rate, the second would wait
			// for at least 1000 seconds without cancellation.
			delegations := 0
			c := NewComparator(tc.profile,
				func(ctx context.Context, byPosition []ItemID) ([]int, error) {
					delegations++
					return identityCompare(ctx, byPosition)
				})
			ctx, cancel := context.WithCancel(context.Background())
			if _, err := c.Compare(ctx, []ItemID{0, 1}); err != nil {
				t.Fatalf("first Compare (free burst): %v", err)
			}
			cancel()
			if _, err := c.Compare(ctx, []ItemID{0, 1}); !errors.Is(err, context.Canceled) {
				t.Errorf("second Compare error = %v, want context.Canceled", err)
			}
			if delegations != 1 {
				t.Errorf("CompareFunc called %d times, want only the initial unpaced call", delegations)
			}
		})
	}
}

func TestComparatorEnforcesConcurrencyCeiling(t *testing.T) {
	const calls = 8
	for _, configured := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("configured-%d", configured), func(t *testing.T) {
			ceiling := max(configured, 1)
			started := make(chan struct{}, calls)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mu sync.Mutex
			active, peak, delegated := 0, 0, 0
			cmp := NewComparator(Profile{
				MaxWidth: 2, MaxConcurrency: configured,
			}, func(ctx context.Context, _ []ItemID) ([]int, error) {
				mu.Lock()
				active++
				delegated++
				peak = max(peak, active)
				mu.Unlock()
				defer func() {
					mu.Lock()
					active--
					mu.Unlock()
				}()
				started <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			})

			errs := make(chan error, calls)
			for range calls {
				go func() {
					_, err := cmp.Compare(ctx, []ItemID{0, 1})
					errs <- err
				}()
			}
			for range ceiling {
				<-started
			}
			cancel()
			for range calls {
				if err := <-errs; !errors.Is(err, context.Canceled) {
					t.Errorf("Compare error = %v, want context.Canceled", err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if delegated != ceiling {
				t.Errorf("CompareFunc called %d times, want %d", delegated, ceiling)
			}
			if peak != ceiling {
				t.Errorf("peak CompareFunc calls = %d, want %d", peak, ceiling)
			}
			if active != 0 {
				t.Errorf("active CompareFunc calls = %d after completion, want 0", active)
			}
		})
	}
}

func TestComparatorConcurrencyAdmissionCancelled(t *testing.T) {
	delegations := 0
	cmp := NewComparator(Profile{MaxWidth: 2},
		func(ctx context.Context, items []ItemID) ([]int, error) {
			delegations++
			return identityCompare(ctx, items)
		})

	// Occupy the sole admission token without dispatching. The call must return
	// from the admission wait rather than consuming pacing or reaching the
	// channel.
	cmp.admit <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	entered := make(chan struct{})
	result := make(chan error)
	go func() {
		close(entered)
		_, err := cmp.Compare(ctx, []ItemID{0, 1})
		result <- err
	}()
	<-entered
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Compare error = %v, want context.Canceled", err)
	}
	if delegations != 0 {
		t.Fatalf("CompareFunc called %d times, want zero", delegations)
	}
	<-cmp.admit
}
