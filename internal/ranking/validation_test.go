package ranking

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
)

// TestOutlierDirectionString verifies named and unknown direction renderings.
func TestOutlierDirectionString(t *testing.T) {
	for direction, want := range map[OutlierDirection]string{
		Late: "Late", Early: "Early", Either: "Either",
		OutlierDirection(99): "OutlierDirection(99)",
	} {
		if got := direction.String(); got != want {
			t.Errorf("OutlierDirection(%d).String() = %q, want %q",
				int(direction), got, want)
		}
	}
}

func TestOutlierOutcomeString(t *testing.T) {
	for outcome, want := range map[OutlierOutcome]string{
		OutlierInconclusive: "Inconclusive",
		OutlierFound:        "Found",
		OutlierNegative:     "Negative",
		OutlierPaused:       "Paused",
		OutlierOutcome(99):  "OutlierOutcome(99)",
	} {
		if got := outcome.String(); got != want {
			t.Errorf("OutlierOutcome(%d).String() = %q, want %q", int(outcome), got, want)
		}
	}
}

func TestOutlierInferenceRegimeString(t *testing.T) {
	for regime, want := range map[OutlierInferenceRegime]string{
		OutlierInferenceUncontrolled:   "uncontrolled",
		OutlierInferenceAnytimeValid:   "anytime-valid",
		OutlierInferenceFixedDesign:    "fixed-design",
		OutlierInferenceScheduledLooks: "scheduled-looks",
		OutlierInferenceRegime(99):     "OutlierInferenceRegime(99)",
	} {
		if got := regime.String(); got != want {
			t.Errorf("OutlierInferenceRegime(%d).String() = %q, want %q",
				int(regime), got, want)
		}
	}
}

func TestExternalEvidencePremiseString(t *testing.T) {
	const premise OutlierEvidencePremise = "independent symmetric ranks"
	if got := premise.String(); got != string(premise) {
		t.Errorf("String = %q, want %q", got, premise)
	}
	if got := OutlierPremiseUnspecified.String(); got != "unspecified" {
		t.Errorf("unspecified String = %q", got)
	}
}

// TestBaselineReservedOutlierSolverDefaultRNG exercises the default RNG.
func TestBaselineReservedOutlierSolverDefaultRNG(t *testing.T) {
	cmp := NewComparator(Profile{
		MaxWidth: 2,
		Controls: map[ControlKind]ControlSupply{Baseline: BoundedControls(1)},
	}, func(_ context.Context, byPosition []ItemID) ([]int, error) {
		ranks := make([]int, len(byPosition))
		for position, item := range byPosition {
			if item.IsCandidate() {
				ranks[position] = 1
			}
		}
		return ranks, nil
	})
	f := &BaselineReservedOutlierSolver{} // zero value: defaultRNG plus default edge/budget
	res, err := f.Identify(
		context.Background(), OutlierRequest{M: 1, Direction: Late, FalseFindRisk: 0.01, FalseNegativeRisk: 0.01}, cmp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutlierFound || res.Index != 0 {
		t.Errorf("outcome=%v index=%d, want Found at index 0", res.Outcome, res.Index)
	}
}

// TestBaselineReservedOutlierSolverInconclusive verifies the result returned when the
// comparison budget cannot resolve the population.
func TestBaselineReservedOutlierSolverInconclusive(t *testing.T) {
	const m, n, maxCmp = 16, 4, 20
	cmp := &syntheticComparator{
		n: n, bias: flatBias, noise: gaussianNoise(8), offset: make([]float64, m), // no target
		rng: rand.New(rand.NewPCG(1, 2)), //nolint:gosec // math/rand in tests, not security-sensitive
	}
	f := newBaselineReservedOutlierSolver(3) // tiny edge: cannot resolve in 20 comparisons
	f.MaxComparisons = maxCmp
	res, err := f.Identify(
		context.Background(), OutlierRequest{
			M: m, Direction: Late, MinimumEdge: 0.01,
			FalseFindRisk: 0.01, FalseNegativeRisk: 0.01,
		}, cmp.comparator())
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	if res.Outcome != OutlierInconclusive {
		t.Fatalf("Outcome = %v, want Inconclusive", res.Outcome)
	}
	if res.ErrorRiskLimit != 0 {
		t.Errorf("inconclusive error-risk limit = %v, want 0 (no guarantee)",
			res.ErrorRiskLimit)
	}
	if res.Index != -1 {
		t.Errorf("inconclusive Index = %d, want -1", res.Index)
	}
	if res.Comparisons != maxCmp {
		t.Errorf("Comparisons = %d, want exhausted budget %d", res.Comparisons, maxCmp)
	}
	if res.Participations != maxCmp*n {
		t.Errorf("Participations = %d, want %d", res.Participations, maxCmp*n)
	}
	if len(res.Items) != m {
		t.Errorf("len(Items) = %d, want %d (populated on every returned result)", len(res.Items), m)
	}
}

func TestBaselineReservedOutlierSolverRejectsInvalidConfiguration(t *testing.T) {
	request := func(falseFindRisk, falseNegativeRisk float64) OutlierRequest {
		return OutlierRequest{
			M: 8, Direction: Late, FalseFindRisk: falseFindRisk,
			FalseNegativeRisk: falseNegativeRisk,
		}
	}
	validReq := request(0.1, 0.1)
	validCmp := NewComparator(Profile{MaxWidth: 4}, identityCompare)
	for _, tc := range []struct {
		name    string
		req     OutlierRequest
		cmp     *Comparator
		edge    float64
		wantErr string
	}{
		{"nil comparator", validReq, nil, 0, "cmp is nil"},
		{"zero comparator", validReq, &Comparator{}, 0, "MaxWidth"},
		{"M zero", OutlierRequest{
			Direction: Late, FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
		}, validCmp, 0, "M must be >= 1"},
		{"false-find risk zero", request(0, 0.1), validCmp, 0, "FalseFindRisk must be in"},
		{"false-find risk one", request(1, 0.1), validCmp, 0, "FalseFindRisk must be in"},
		{"false-find risk NaN", request(math.NaN(), 0.1), validCmp, 0, "FalseFindRisk must be in"},
		{"false-find risk infinity", request(math.Inf(1), 0.1), validCmp, 0, "FalseFindRisk must be in"},
		{"false-negative risk below zero", request(0.1, -0.1), validCmp, 0, "FalseNegativeRisk must be in"},
		{"false-negative risk zero", request(0.1, 0), validCmp, 0, "FalseNegativeRisk must be in"},
		{"false-negative risk one", request(0.1, 1), validCmp, 0, "FalseNegativeRisk must be in"},
		{"false-negative risk NaN", request(0.1, math.NaN()), validCmp, 0, "FalseNegativeRisk must be in"},
		{"false-negative risk infinity", request(0.1, math.Inf(1)), validCmp, 0, "FalseNegativeRisk must be in"},
		{
			"invalid sign",
			OutlierRequest{
				M: 8, Direction: OutlierDirection(99), FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
			},
			validCmp, 0, "invalid OutlierDirection",
		},
		{
			"Either without baseline control",
			OutlierRequest{M: 8, Direction: Either, FalseFindRisk: 0.1, FalseNegativeRisk: 0.01},
			validCmp, 0, "Either requires a Baseline control",
		},
		{"width below two", validReq, NewComparator(Profile{MaxWidth: 1}, identityCompare), 0, "MaxWidth must be >= 2"},
		{
			"insufficient controls",
			OutlierRequest{M: 3, Direction: Late, FalseFindRisk: 0.1, FalseNegativeRisk: 0.01},
			validCmp, 0,
			"Baseline controls < comparator width",
		},
		{"negative edge", validReq, validCmp, -0.1, "MinimumEdge must be >= 0"},
		{"edge one half", validReq, validCmp, 0.5, "MinimumEdge must be in"},
		{"edge NaN", validReq, validCmp, math.NaN(), "MinimumEdge must be in"},
		{"edge infinity", validReq, validCmp, math.Inf(1), "MinimumEdge must be in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newBaselineReservedOutlierSolver(1)
			tc.req.MinimumEdge = tc.edge
			res, err := f.Identify(context.Background(), tc.req, tc.cmp)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Identify error = %v, want category containing %q", err, tc.wantErr)
			}
			if want := (OutlierResult{Index: -1}); !reflect.DeepEqual(res, want) {
				t.Errorf("Identify result = %+v, want failure result %+v", res, want)
			}
		})
	}
}

func TestBaselineReservedOutlierSolverRejectsNegativeResourceLimits(t *testing.T) {
	req := OutlierRequest{M: 8, Direction: Late, FalseFindRisk: 0.1, FalseNegativeRisk: 0.01}
	cmp := NewComparator(Profile{MaxWidth: 4}, identityCompare)
	for _, test := range []struct {
		name      string
		configure func(*BaselineReservedOutlierSolver)
		wantErr   string
	}{
		{"comparisons", func(f *BaselineReservedOutlierSolver) { f.MaxComparisons = -1 }, "MaxComparisons"},
		{"concurrency", func(f *BaselineReservedOutlierSolver) { f.MaxConcurrency = -1 }, "MaxConcurrency"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBaselineReservedOutlierSolver(1)
			test.configure(f)
			res, err := f.Identify(context.Background(), req, cmp)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("Identify error = %v, want category containing %q",
					err, test.wantErr)
			}
			if want := (OutlierResult{Index: -1}); !reflect.DeepEqual(res, want) {
				t.Errorf("Identify result = %+v, want failure result %+v", res, want)
			}
		})
	}
}

func TestBaselineReservedOutlierSolverRejectsInvalidChannelLimits(t *testing.T) {
	req := OutlierRequest{
		M: 8, Direction: Late, FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
	}
	for _, test := range []struct {
		name    string
		finder  *BaselineReservedOutlierSolver
		profile Profile
		wantErr string
	}{
		{
			"negative concurrency", newBaselineReservedOutlierSolver(1),
			Profile{MaxWidth: 4, MaxConcurrency: -1},
			"MaxConcurrency",
		},
		{
			"participation overflow",
			&BaselineReservedOutlierSolver{MaxComparisons: math.MaxInt},
			Profile{MaxWidth: 2},
			"overflows participation count",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmp := NewComparator(test.profile, identityCompare)
			res, err := test.finder.Identify(context.Background(), req, cmp)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("Identify error = %v, want category containing %q",
					err, test.wantErr)
			}
			if want := (OutlierResult{Index: -1}); !reflect.DeepEqual(res, want) {
				t.Errorf("Identify result = %+v, want failure result %+v", res, want)
			}
		})
	}
}

func TestBaselineReservedOutlierSolverRejectsPopulationArithmeticOverflow(t *testing.T) {
	cmp := NewComparator(Profile{
		MaxWidth: 2,
		Controls: map[ControlKind]ControlSupply{Baseline: UnboundedControls()},
	}, identityCompare)
	for _, sign := range []OutlierDirection{Late, Either} {
		result, err := (&BaselineReservedOutlierSolver{}).Identify(context.Background(), OutlierRequest{
			M: math.MaxInt, Direction: sign, FalseFindRisk: 0.1, FalseNegativeRisk: 0.01,
		}, cmp)
		if err == nil || !strings.Contains(err.Error(), "M") ||
			!strings.Contains(err.Error(), "too large") {
			t.Errorf("sign %s: error = %v, want M-too-large error", sign, err)
		}
		if want := (OutlierResult{Index: -1}); !reflect.DeepEqual(result, want) {
			t.Errorf("sign %s: result = %+v, want failure result %+v", sign, result, want)
		}
	}
}

func TestOutlierDefaultComparisonLimitFitsParticipations(t *testing.T) {
	for _, width := range []int{2, 64} {
		got := outlierDefaultComparisonLimit(width)
		if got > math.MaxInt/width {
			t.Errorf("width %d limit %d overflows participation count", width, got)
		}
		if got > OutlierDefaultMaxComparisons {
			t.Errorf("width %d limit %d exceeds default", width, got)
		}
	}
}

// TestBaselineReservedOutlierSolverPreCancelled verifies cancellation before any comparison
// is dispatched.
func TestBaselineReservedOutlierSolverPreCancelled(t *testing.T) {
	cmp := &syntheticComparator{
		n: 4, bias: flatBias, noise: gaussianNoise(8), offset: make([]float64, 8),
		rng: rand.New(rand.NewPCG(1, 2)), //nolint:gosec // math/rand in tests, not security-sensitive
	}
	f := newBaselineReservedOutlierSolver(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before the solve begins
	res, err := f.Identify(
		ctx, OutlierRequest{M: 8, Direction: Late, FalseFindRisk: 0.01, FalseNegativeRisk: 0.01}, cmp.comparator())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res.Outcome == OutlierFound {
		t.Errorf("a cancelled find reported a commit: %+v", res)
	}
	if want := (OutlierResult{Index: -1}); !reflect.DeepEqual(res, want) {
		t.Errorf("result = %+v, want failure result %+v", res, want)
	}
}
