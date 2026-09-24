package tth2

import (
	"math/rand/v2"
	"net/http"
	"slices"
	"testing"
)

// pinnedRNG returns a deterministic source for arrangement tests.
func pinnedRNG() *rand.Rand {
	//nolint:gosec // non-cryptographic PRNG, pinned so an assertion cannot flake
	return rand.New(rand.NewPCG(1, 2))
}

// arrangementsFor collects the arrangements one arranger produces over
// complete cycles, copying each because the arranger reuses its buffer.
func arrangementsFor(policy ArrangementPolicy, n, cycles int) [][]int {
	a := newArranger(policy, n, pinnedRNG())
	unit := policy.unit(n)
	var out [][]int
	for range cycles {
		a.startCycle()
		for shift := range unit {
			out = append(out, slices.Clone(a.next(shift)))
		}
	}
	return out
}

// TestArrangeRotateBalancesExactly verifies one visit per request and position
// in every complete cycle.
func TestArrangeRotateBalancesExactly(t *testing.T) {
	t.Parallel()
	const n, cycles = 4, 6
	got := arrangementsFor(ArrangeRotate, n, cycles)
	if len(got) != n*cycles {
		t.Fatalf("produced %d arrangements, want %d", len(got), n*cycles)
	}
	for cycle := range cycles {
		// visits[request][position] over the cycle's n trials.
		visits := make([][]int, n)
		for i := range visits {
			visits[i] = make([]int, n)
		}
		for shift := range n {
			arrangement := got[cycle*n+shift]
			if !isPermutation(arrangement) {
				t.Fatalf("cycle %d shift %d: %v is not a permutation", cycle, shift, arrangement)
			}
			for position, i := range arrangement {
				visits[i][position]++
			}
		}
		for i, row := range visits {
			for position, count := range row {
				if count != 1 {
					t.Errorf("cycle %d: request %d visited position %d %d times, want exactly 1 (visits %v)",
						cycle, i, position, count, visits)
				}
			}
		}
	}
}

// TestArrangeRotateVariesAdjacency verifies that cycles redraw their base
// permutation. The deterministic seed makes the coverage stable.
func TestArrangeRotateVariesAdjacency(t *testing.T) {
	t.Parallel()
	const n, cycles = 4, 6
	got := arrangementsFor(ArrangeRotate, n, cycles)
	first := got[0] // shift 0 of a cycle is its base
	for cycle := 1; cycle < cycles; cycle++ {
		if !slices.Equal(got[cycle*n], first) {
			return
		}
	}
	t.Errorf("every cycle used the same base %v: the base is not being redrawn", first)
}

// TestArrangeNoneIsIdentity verifies that position equals request identity.
func TestArrangeNoneIsIdentity(t *testing.T) {
	t.Parallel()
	const n, trials = 4, 5
	want := identity(n)
	for i, arrangement := range arrangementsFor(ArrangeNone, n, trials) {
		if !slices.Equal(arrangement, want) {
			t.Fatalf("trial %d: got %v, want the identity %v", i, arrangement, want)
		}
	}
}

// TestArrangeRandomCoversPositions checks permutation validity and positional
// coverage without requiring exact counts.
func TestArrangeRandomCoversPositions(t *testing.T) {
	t.Parallel()
	const n, trials = 4, 400
	visits := make([][]int, n)
	for i := range visits {
		visits[i] = make([]int, n)
	}
	for i, arrangement := range arrangementsFor(ArrangeRandom, n, trials) {
		if !isPermutation(arrangement) {
			t.Fatalf("trial %d: %v is not a permutation", i, arrangement)
		}
		for position, req := range arrangement {
			visits[req][position]++
		}
	}
	for i, row := range visits {
		for position, count := range row {
			if count == 0 {
				t.Errorf("request %d never reached position %d in %d trials", i, position, trials)
			}
		}
	}
}

// TestArrangeAndScatterRoundTrip verifies that scattering restores request
// order after arrangement.
func TestArrangeAndScatterRoundTrip(t *testing.T) {
	t.Parallel()
	arrangement := []int{2, 0, 3, 1} // position 0 sends request 2, and so on
	reqs := []*http.Request{
		mustReq(t, "https://example.test/0"),
		mustReq(t, "https://example.test/1"),
		mustReq(t, "https://example.test/2"),
		mustReq(t, "https://example.test/3"),
	}

	positioned := arrange(reqs, arrangement)
	for position, i := range arrangement {
		if positioned[position] != reqs[i] {
			t.Errorf("position %d holds %v, want request %d (%v)",
				position, positioned[position].URL, i, reqs[i].URL)
		}
	}

	// One result per position, tagged with the position it came from, so the
	// scatter is checkable by identity rather than by value.
	results := make([]Result, len(arrangement))
	for position := range results {
		results[position] = Result{ArrivalRank: position}
	}
	byRequest := scatter(results, arrangement)
	for position, i := range arrangement {
		if byRequest[i] != results[position] {
			t.Errorf("request %d got the result from position %d, want position %d",
				i, byRequest[i].ArrivalRank, position)
		}
	}
}

// TestPolicyUnit verifies each policy's work-distribution granularity.
func TestPolicyUnit(t *testing.T) {
	t.Parallel()
	const n = 5
	for _, tc := range []struct {
		policy   ArrangementPolicy
		wantUnit int
		balances bool
	}{
		{ArrangeRotate, n, true},
		{ArrangeNone, 1, false},
		{ArrangeRandom, 1, false},
	} {
		if got := tc.policy.unit(n); got != tc.wantUnit {
			t.Errorf("policy %d: unit(%d) = %d, want %d", tc.policy, n, got, tc.wantUnit)
		}
		if got := tc.policy.balancesExactly(); got != tc.balances {
			t.Errorf("policy %d: balancesExactly = %v, want %v", tc.policy, got, tc.balances)
		}
	}
}

// TestArrangeRotateIsTheZeroValue verifies the balanced default.
func TestArrangeRotateIsTheZeroValue(t *testing.T) {
	t.Parallel()
	var p ArrangementPolicy
	if p != ArrangeRotate {
		t.Errorf("zero ArrangementPolicy = %d, want ArrangeRotate (%d)", p, ArrangeRotate)
	}
	if got := defaultTrialsConfig().policy; got != ArrangeRotate {
		t.Errorf("default trial config policy = %d, want ArrangeRotate", got)
	}
}

// TestWithArrangementPolicyRejectsUnknown verifies the option's enum domain.
func TestWithArrangementPolicyRejectsUnknown(t *testing.T) {
	t.Parallel()
	for _, policy := range []ArrangementPolicy{-1, ArrangeRandom + 1} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("WithArrangementPolicy(%d): no panic", policy)
				}
			}()
			_ = WithArrangementPolicy(policy)
		}()
	}
}

// isPermutation reports whether s holds each of 0..len(s)-1 exactly once.
func isPermutation(s []int) bool {
	seen := make([]bool, len(s))
	for _, v := range s {
		if v < 0 || v >= len(s) || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
