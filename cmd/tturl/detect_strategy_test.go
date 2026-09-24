package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/ranking"
)

func TestDetectAutomaticStrategyMatrix(t *testing.T) {
	for _, test := range []struct {
		name        string
		sign        ranking.OutlierDirection
		hasBaseline bool
		want        detectStrategy
		wantError   string
	}{
		{"slow without baseline", ranking.Late, false, detectStrategyPeerFirst, ""},
		{"fast without baseline", ranking.Early, false, detectStrategyPeerFirst, ""},
		{"slow with baseline", ranking.Late, true, detectStrategyBaselineReserved, ""},
		{"fast with baseline", ranking.Early, true, detectStrategyBaselineReserved, ""},
		{"any with baseline", ranking.Either, true, detectStrategyBaselineReserved, ""},
		{"any without baseline", ranking.Either, false, "", "requires a --baseline-only block"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveDetectStrategy(
				string(detectStrategyAuto), test.sign, test.hasBaseline)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want one containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.requested != detectStrategyAuto || got.resolved != test.want ||
				!got.method.Valid() || got.warning != "" {
				t.Errorf("selection = %+v, want resolved %q", got, test.want)
			}
		})
	}
}

func TestDetectExplicitStrategyConstraints(t *testing.T) {
	for _, test := range []struct {
		strategy    detectStrategy
		sign        ranking.OutlierDirection
		hasBaseline bool
		wantError   string
		wantWarning bool
	}{
		{detectStrategyPeerFirst, ranking.Late, false, "", false},
		{detectStrategyPeerFirst, ranking.Early, true, "", true},
		{detectStrategyPeerFirst, ranking.Either, true, "requires --direction early or late", false},
		{detectStrategyRollingPeerFirst, ranking.Late, false, "", false},
		{detectStrategyRollingPeerFirst, ranking.Early, true, "", true},
		{detectStrategyRollingPeerFirst, ranking.Either, true, "requires --direction early or late", false},
		{detectStrategyBaselineConfirmed, ranking.Late, true, "", false},
		{detectStrategyBaselineConfirmed, ranking.Late, false, "requires a --baseline-only block", false},
		{detectStrategyBaselineConfirmed, ranking.Either, true, "requires --direction early or late", false},
		{detectStrategyRollingBaselineConfirmed, ranking.Late, true, "", false},
		{detectStrategyRollingBaselineConfirmed, ranking.Late, false, "requires a --baseline-only block", false},
		{detectStrategyRollingBaselineConfirmed, ranking.Either, true, "requires --direction early or late", false},
		{detectStrategyBaselineReserved, ranking.Either, true, "", false},
		{detectStrategyBaselineReserved, ranking.Early, false, "requires a --baseline-only block", false},
		{detectStrategyRollingBaselineReserved, ranking.Either, true, "", false},
		{detectStrategyRollingBaselineReserved, ranking.Late, false, "requires a --baseline-only block", false},
	} {
		name := string(test.strategy) + "/" + test.sign.String()
		t.Run(name, func(t *testing.T) {
			got, err := resolveDetectStrategy(
				string(test.strategy), test.sign, test.hasBaseline)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want one containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.resolved != test.strategy || (got.warning != "") != test.wantWarning {
				t.Errorf("selection = %+v", got)
			}
		})
	}
}

func TestDetectStrategyCreatesNamedSolver(t *testing.T) {
	wantTypes := map[detectStrategy]reflect.Type{
		detectStrategyPeerFirst:                reflect.TypeFor[*ranking.PeerFirstOutlierSolver](),
		detectStrategyRollingPeerFirst:         reflect.TypeFor[*ranking.RollingPeerFirstOutlierSolver](),
		detectStrategyBaselineConfirmed:        reflect.TypeFor[*ranking.BaselineConfirmedOutlierSolver](),
		detectStrategyRollingBaselineConfirmed: reflect.TypeFor[*ranking.RollingBaselineConfirmedOutlierSolver](),
		detectStrategyBaselineReserved:         reflect.TypeFor[*ranking.BaselineReservedOutlierSolver](),
		detectStrategyRollingBaselineReserved:  reflect.TypeFor[*ranking.RollingBaselineReservedOutlierSolver](),
	}
	for strategy, wantType := range wantTypes {
		hasBaseline := strategy != detectStrategyPeerFirst &&
			strategy != detectStrategyRollingPeerFirst
		selection, err := resolveDetectStrategy(
			string(strategy), ranking.Late, hasBaseline)
		if err != nil {
			t.Fatal(err)
		}
		solver, err := selection.newSolver(123, 4)
		if err != nil {
			t.Fatal(err)
		}
		if reflect.TypeOf(solver) != wantType || solver.MethodID() != selection.method {
			t.Errorf("strategy %q solver = %T method %q, want %v method %q",
				strategy, solver, solver.MethodID(), wantType, selection.method)
		}
	}
}

func TestDetectStrategyEvidenceUpdatePolicy(t *testing.T) {
	for _, strategy := range detectStrategyRegistry {
		selection, err := resolveDetectStrategy(
			string(strategy.name), ranking.Late,
			strategy.name != detectStrategyPeerFirst)
		if err != nil {
			t.Fatal(err)
		}
		want := "complete-atomic-wave"
		if selection.rolling() {
			want = "maximal-contiguous-dispatch-prefix"
		}
		if got := selection.evidenceUpdatePolicy(); got != want {
			t.Errorf("strategy %q update policy = %q, want %q",
				strategy.name, got, want)
		}
		wantPremise := "arbitrary-within-atomic-wave"
		if selection.rolling() {
			wantPremise = "conditional-across-dispatch-indices"
		}
		if got := selection.dependencePremise(); got != wantPremise {
			t.Errorf("strategy %q dependence premise = %q, want %q",
				strategy.name, got, wantPremise)
		}
	}
}

type unidentifiedDetectSolver struct{}

func (unidentifiedDetectSolver) Inference() ranking.OutlierInferenceRegime {
	return ranking.OutlierInferenceUncontrolled
}

func (unidentifiedDetectSolver) Identify(
	context.Context,
	ranking.OutlierRequest,
	*ranking.Comparator,
) (ranking.OutlierResult, error) {
	return ranking.OutlierResult{}, nil
}

func TestDetectStrategyRejectsUnidentifiedSolver(t *testing.T) {
	selection := detectStrategySelection{
		resolved: detectStrategyPeerFirst,
		spec: detectStrategySpec{
			name: detectStrategyPeerFirst,
			new: func(int, int) ranking.OutlierSolver {
				return unidentifiedDetectSolver{}
			},
		},
	}
	if _, err := selection.newSolver(1, 1); err == nil ||
		!strings.Contains(err.Error(), "unidentified") {
		t.Errorf("newSolver error = %v", err)
	}
}
