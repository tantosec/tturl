package main

import (
	"bytes"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/ranking"
)

func assembleDetectPlan(
	t *testing.T,
	args ...string,
) (*assembled, detectRequestPlan, error) {
	t.Helper()
	layout := newCommand(commandByID(commandDetect))
	flags := configureDetectCommand(layout)
	plan, err := layout.parser.Parse(args)
	if err != nil {
		return nil, detectRequestPlan{}, err
	}
	roles, supplies, err := flags.roles.resolve(plan, 0)
	if err != nil {
		return nil, detectRequestPlan{}, err
	}
	assembled, err := flags.roles.assemble(t.Context(), plan, roles, supplies)
	if err != nil {
		return nil, detectRequestPlan{}, err
	}
	detectPlan, err := newDetectRequestPlan(assembled, 8)
	return assembled, detectPlan, err
}

func TestDetectBaselineSupplyValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			"supply outside baseline block",
			[]string{"--block", "https://h/a", "--baseline-supply", "2"},
			"--baseline-supply requires --baseline-only",
		},
		{
			"zero supply",
			[]string{
				"--block", "https://h/a", "--baseline-only",
				"--baseline-supply", "0",
			},
			"--baseline-supply",
		},
		{
			"negative supply",
			[]string{
				"--block", "https://h/a", "--baseline-only",
				"--baseline-supply=-1",
			},
			"--baseline-supply",
		},
		{
			"malformed supply",
			[]string{
				"--block", "https://h/a", "--baseline-only",
				"--baseline-supply", "many",
			},
			"--baseline-supply",
		},
		{
			"warmup and baseline roles",
			[]string{
				"--block", "https://h/a", "--baseline-only", "--warmup-only",
			},
			"mutually exclusive",
		},
		{
			"explicit repeat one",
			[]string{
				"--block", "https://h/a", "--baseline-only", "--repeat", "1",
			},
			"--repeat cannot be used with --baseline-only",
		},
		{
			"inherited repeat",
			[]string{
				"https://h/default", "--repeat", "2",
				"--block", "https://h/a", "--baseline-only",
				"--block", "https://h/b",
			},
			"--repeat cannot be used with --baseline-only",
		},
		{
			"unlimited mixed with finite block",
			[]string{
				"--block", "https://h/a", "--baseline-only",
				"--baseline-supply", "unlimited",
				"--block", "https://h/b", "--baseline-only",
			},
			"unlimited baseline block cannot be combined",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := assembleDetectPlan(t, test.args...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func TestDetectBaselineExpansionAndSupply(t *testing.T) {
	assembled, plan, err := assembleDetectPlan(t,
		"--block", "https://h/candidate", "--name", "candidate",
		"--block", "https://h/a", "https://h/b", "--baseline-only",
		"--baseline-supply", "2", "--name", "plain",
		"--block", "https://h/V", "--vary", "V={c,d}",
		"--baseline-only", "--baseline-supply", "3", "--name", "fan",
	)
	if err != nil {
		t.Fatal(err)
	}
	if plan.candidates != 1 || plan.baselineTemplates != 4 ||
		plan.baselineSupply.count != 10 || plan.widthEffective != 8 ||
		plan.baselineEffectiveCap != 7 {
		t.Fatalf("detect plan = %+v", plan)
	}
	if len(assembled.groups) != 5 {
		t.Fatalf("groups = %d, want 5", len(assembled.groups))
	}
	for i, group := range assembled.groups[1:] {
		wantSupply := 2
		if i >= 2 {
			wantSupply = 3
		}
		if !group.baseline || group.count != 1 ||
			group.supply.count != wantSupply {
			t.Errorf("baseline group %d = %+v, want supply %d", i, group, wantSupply)
		}
	}
	wantLabels := []string{"candidate", "plain~1", "plain~2", "fan:V=c", "fan:V=d"}
	if !slices.Equal(assembled.labels, wantLabels) {
		t.Errorf("labels = %v, want %v", assembled.labels, wantLabels)
	}
}

func TestDetectSummaryKeepsBaselineSupplyCompact(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "finite layered templates",
			args: []string{
				"--block", "https://h/candidate", "--name", "candidate",
				"--block", "https://h/a", "https://h/b", "--baseline-only",
				"--baseline-supply", "2", "--name", "plain",
				"--block", "https://h/V", "--vary", "V={c,d}",
				"--baseline-only", "--baseline-supply", "3", "--name", "fan",
			},
			want: []string{
				"baseline", "supply 2 (effective maximum 2)",
				"supply 3 (effective maximum 3)",
				"Total: 1 candidate request, 4 baseline templates, 0 warmup requests.",
			},
		},
		{
			name: "unlimited templates",
			args: []string{
				"--block", "https://h/candidate", "--name", "candidate",
				"--block", "https://h/a", "https://h/b", "--baseline-only",
				"--baseline-supply", "unlimited", "--name", "reference",
			},
			want: []string{
				"baseline", "supply unlimited (effective maximum 7)",
				"Total: 1 candidate request, 2 baseline templates, 0 warmup requests.",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assembled, _, err := assembleDetectPlan(t, test.args...)
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			summarise(&output, assembled.groups,
				projectRunReferences(assembled.labels, 1000), false)
			for _, want := range test.want {
				if !strings.Contains(output.String(), want) {
					t.Errorf("summary lacks %q:\n%s", want, &output)
				}
			}
			for line := range strings.SplitSeq(output.String(), "\n") {
				if len(line) > textWidth {
					t.Errorf("summary line has %d columns: %q", len(line), line)
				}
			}
		})
	}
}

func TestDetectBaselineDefaultAndUnlimitedSupply(t *testing.T) {
	for _, test := range []struct {
		name      string
		value     string
		wantCount int
		unlimited bool
	}{
		{name: "default", wantCount: 1},
		{name: "finite", value: "4", wantCount: 4},
		{name: "unlimited", value: "unlimited", unlimited: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := []string{
				"--block", "https://h/candidate",
				"--block", "https://h/baseline", "--baseline-only",
			}
			if test.value != "" {
				args = append(args, "--baseline-supply", test.value)
			}
			assembled, plan, err := assembleDetectPlan(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			supply := assembled.groups[1].supply
			if supply.count != test.wantCount || supply.unlimited != test.unlimited ||
				plan.baselineSupply != supply {
				t.Errorf("group supply = %+v; plan supply = %+v",
					supply, plan.baselineSupply)
			}
		})
	}
}

func TestDetectRequestPlanWidth(t *testing.T) {
	tests := []struct {
		name       string
		candidates int
		supply     baselineSupply
		width      int
		wantWidth  int
		wantCap    int
	}{
		{"no baseline caps to candidates", 3, baselineSupply{}, 8, 3, 0},
		{"finite supply fills width", 3, finiteBaselineSupply(2), 8, 5, 2},
		{"finite excess is capped", 1, finiteBaselineSupply(20), 8, 8, 7},
		{"unlimited fills width", 1, unlimitedBaselineSupply(), 8, 8, 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			groups := []requestGroup{{count: test.candidates}}
			if test.supply.count > 0 || test.supply.unlimited {
				groups = append(groups, requestGroup{
					count: 1, baseline: true, supply: test.supply,
				})
			}
			got, err := newDetectRequestPlan(
				&assembled{groups: groups}, test.width)
			if err != nil {
				t.Fatal(err)
			}
			if got.widthEffective != test.wantWidth ||
				got.baselineEffectiveCap != test.wantCap {
				t.Errorf("plan = %+v, want width %d and cap %d",
					got, test.wantWidth, test.wantCap)
			}
		})
	}
}

func TestDetectRequestPlanComparatorProfile(t *testing.T) {
	for _, test := range []struct {
		name   string
		plan   detectRequestPlan
		want   int
		absent bool
	}{
		{
			name:   "absent",
			plan:   detectRequestPlan{widthEffective: 3},
			absent: true,
		},
		{
			name: "bounded",
			plan: detectRequestPlan{
				widthEffective: 4, baselineTemplates: 2,
				baselineSupply: finiteBaselineSupply(7),
			},
			want: 7,
		},
		{
			name: "unbounded",
			plan: detectRequestPlan{
				widthEffective: 4, baselineTemplates: 2,
				baselineSupply: unlimitedBaselineSupply(),
			},
			want: 20,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := test.plan.comparatorProfile(3)
			if profile.MaxWidth != test.plan.widthEffective ||
				profile.MaxConcurrency != 3 {
				t.Errorf("profile = %+v", profile)
			}
			supply, ok := profile.Controls[ranking.Baseline]
			if ok == test.absent {
				t.Fatalf("baseline presence = %t, want %t", ok, !test.absent)
			}
			if ok && supply.UpTo(20) != test.want {
				t.Errorf("baseline supply up to 20 = %d, want %d",
					supply.UpTo(20), test.want)
			}
		})
	}
}

func TestDetectRequestPlanPopulationAndOverflow(t *testing.T) {
	if _, err := newDetectRequestPlan(&assembled{}, 2); err == nil ||
		!strings.Contains(err.Error(), "candidate") {
		t.Errorf("no-candidate error = %v", err)
	}
	if _, err := newDetectRequestPlan(&assembled{
		groups: []requestGroup{{count: 1}},
	}, 2); err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Errorf("one-candidate error = %v", err)
	}
	if _, err := newDetectRequestPlan(&assembled{groups: []requestGroup{
		{count: 1},
		{count: 1, baseline: true, supply: finiteBaselineSupply(math.MaxInt)},
		{count: 1, baseline: true, supply: finiteBaselineSupply(1)},
	}}, 2); err == nil || !strings.Contains(err.Error(), "cannot be represented") {
		t.Errorf("aggregate-overflow error = %v", err)
	}
}

func TestBaselineTemplateMapping(t *testing.T) {
	finite := &prepared{materialised: &materialised{
		baselineTemplates: []baselineRequestTemplate{
			{label: "A", supply: finiteBaselineSupply(2)},
			{label: "B", supply: finiteBaselineSupply(1)},
			{label: "C", supply: finiteBaselineSupply(3)},
		},
	}}
	wantFinite := []int{0, 1, 2, 0, 2, 2}
	for control, want := range wantFinite {
		got, err := finite.baselineTemplateIndex(control)
		if err != nil || got != want {
			t.Errorf("finite control %d = (%d, %v), want %d",
				control, got, err, want)
		}
	}
	if _, err := finite.baselineTemplateIndex(len(wantFinite)); err == nil {
		t.Error("finite out-of-range control was accepted")
	}

	unlimited := &prepared{materialised: &materialised{
		baselineTemplates: []baselineRequestTemplate{
			{label: "A", supply: unlimitedBaselineSupply()},
			{label: "B", supply: unlimitedBaselineSupply()},
			{label: "C", supply: unlimitedBaselineSupply()},
		},
	}}
	for control := range 10 {
		got, err := unlimited.baselineTemplateIndex(control)
		if err != nil || got != control%3 {
			t.Errorf("unlimited control %d = (%d, %v), want %d",
				control, got, err, control%3)
		}
	}
}

func TestDetectVeryLargeBaselineSupplyStaysCompact(t *testing.T) {
	assembled, plan, err := assembleDetectPlan(t,
		"--block", "https://h/candidate",
		"--block", "https://h/baseline", "--baseline-only",
		"--baseline-supply", strconv.Itoa(math.MaxInt),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(assembled.groups) != 2 || plan.baselineTemplates != 1 ||
		plan.baselineSupply.count != math.MaxInt {
		t.Fatalf("large supply expanded: groups=%d plan=%+v",
			len(assembled.groups), plan)
	}
}
