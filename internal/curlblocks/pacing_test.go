package curlblocks

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestParseRate covers the shared rate grammar: an explicit s/m/h/d unit
// divides the positive integer count accordingly, the empty string is "no
// limit", unlimited resolves to zero, and a missing or bad unit is an error.
func TestParseRate(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{in: "unlimited", want: 0},
		{in: "", want: 0},        // unset: no limit
		{in: "50/s", want: 50},   // per second
		{in: "120/m", want: 2},   // per minute
		{in: "36/h", want: 0.01}, // per hour, explicit
		{in: "86400/d", want: 1}, // per day
		{in: "1/d", want: 1.0 / 86400},
		{in: "3600", wantErr: true}, // unit is mandatory
		{in: "0", wantErr: true},    // count must be positive
		{in: "-5/s", wantErr: true}, // "
		{in: "abc", wantErr: true},  // not a number
		// curl's grammar for the count is a plain positive integer, so these are
		// all rejected -- and rejecting the float forms is what keeps NaN, which
		// slips past any range check, out of a consumer's pacing arithmetic.
		{in: "0.5", wantErr: true},
		{in: "1e3", wantErr: true},
		{in: "NaN", wantErr: true},
		{in: "Inf", wantErr: true},
		{in: "+5", wantErr: true},
		{in: " 5", wantErr: true},
		{in: "5 ", wantErr: true},
		{in: "10/x", wantErr: true},   // bad unit
		{in: "10/", wantErr: true},    // empty unit
		{in: "10/s/s", wantErr: true}, // trailing garbage
		{in: strconv.Itoa(math.MaxInt) + "/s", want: float64(math.MaxInt)},
		{in: strconv.Itoa(math.MaxInt) + "0/s", wantErr: true},
	} {
		got, err := ParseRate(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseRate(%q) err = %v, wantErr %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("ParseRate(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestBatchRateMaxFlagValues(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  float64
	}{
		{"25/s", 25},
		{"120/m", 2},
		{"36/h", 0.01},
		{"86400/d", 1},
		{"unlimited", 0},
	} {
		for _, attached := range []bool{false, true} {
			args := []string{"--batch-rate-max", tc.value}
			if attached {
				args = []string{"--batch-rate-max=" + tc.value}
			}
			p := New()
			pf := p.PacingFlags(p.Global, p.Global, "parallel-max", "", "", "")
			plan, err := p.Parse(append(args, "https://example.test/"))
			if err != nil {
				t.Fatalf("Parse(%q): %v", args, err)
			}
			got, err := pf.BatchRateMax(plan.Globals)
			if err != nil || got != tc.want {
				t.Errorf("BatchRateMax(%q) = %v, %v; want %v", args, got, err, tc.want)
			}
		}
	}
}

// TestPacingFlags checks the opt-in registration and its accessors: the flags
// parse, default (unset) to no parallelism and no rate limit, and reject a
// --parallel-max below 1.
func TestPacingFlags(t *testing.T) {
	set := func(args ...string) (*PacingFlags, *Plan) {
		t.Helper()
		p := New()
		pf := p.PacingFlags(p.Global, p.Global, "parallel-max", "", "", "")
		plan, err := p.Parse(args)
		if err != nil {
			t.Fatalf("Parse(%q): %v", args, err)
		}
		return pf, plan
	}

	pf, plan := set(
		"--parallel-max", "4", "--batch-rate-max", "25/s",
		"--request-rate-max", "50/s", "https://h/",
	)
	if got, err := pf.ParallelMax(plan.Globals); err != nil || got != 4 {
		t.Errorf("ParallelMax = %d (err %v), want 4", got, err)
	}
	if got, err := pf.BatchRateMax(plan.Globals); err != nil || got != 25 {
		t.Errorf("BatchRateMax = %v (err %v), want 25", got, err)
	}
	if got, err := pf.RequestRateMax(plan.Globals); err != nil || got != 50 {
		t.Errorf("RequestRateMax = %v (err %v), want 50", got, err)
	}

	// Unset: default to serial (1) and no rate limit (0).
	pf, plan = set("https://h/")
	if got, err := pf.ParallelMax(plan.Globals); err != nil || got != 1 {
		t.Errorf("default ParallelMax = %d (err %v), want 1", got, err)
	}
	if got, err := pf.BatchRateMax(plan.Globals); err != nil || got != 0 {
		t.Errorf("default BatchRateMax = %v (err %v), want 0", got, err)
	}
	if got, err := pf.RequestRateMax(plan.Globals); err != nil || got != 0 {
		t.Errorf("default RequestRateMax = %v (err %v), want 0", got, err)
	}

	// A bad value is rejected by Parse, alongside every other command-line fault,
	// rather than waiting for the consumer to read it.
	for _, tt := range []struct{ name, args, want string }{
		{"--parallel-max below 1", "--parallel-max=0", "--parallel-max must be >= 1"},
		{"fractional batch rate", "--batch-rate-max=0.5", "--batch-rate-max: count must be a positive integer"},
		{"unitless batch rate", "--batch-rate-max=5", "--batch-rate-max: unit is required"},
		{"unknown request rate unit", "--request-rate-max=5/w", "--request-rate-max: unit must be s, m, h, or d"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := New()
			p.PacingFlags(p.Global, p.Global, "parallel-max", "", "", "")
			_, err := p.Parse([]string{tt.args, "https://h/"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse(%q) = %v, want an error mentioning %q", tt.args, err, tt.want)
			}
		})
	}
}

// TestPacingFlagsCustomUsage checks that a usage string given to PacingFlags
// replaces the default help text in the rendered usage, and that "" takes the
// default.
func TestPacingFlagsCustomUsage(t *testing.T) {
	parallelOnly := New()
	parallelOnly.PacingFlags(
		parallelOnly.Global, parallelOnly.Global, "parallel-max", "MY PARALLEL HELP", "", "")
	usage := flattenSpace(parallelOnly.Usage())
	if !strings.Contains(usage, "MY PARALLEL HELP") {
		t.Errorf("usage missing the custom --parallel-max text:\n%s", usage)
	}
	if !strings.Contains(usage, flattenSpace(defaultBatchRateUsage)) {
		t.Errorf("usage missing the default --batch-rate-max text:\n%s", usage)
	}

	batchOnly := New()
	batchOnly.PacingFlags(batchOnly.Global, batchOnly.Global, "parallel-max", "", "MY BATCH HELP", "")
	usage = flattenSpace(batchOnly.Usage())
	if !strings.Contains(usage, "MY BATCH HELP") {
		t.Errorf("usage missing the custom --batch-rate-max text:\n%s", usage)
	}
	if !strings.Contains(usage, flattenSpace(defaultParallelMaxUsage)) {
		t.Errorf("usage missing the default --parallel-max text (not overridden):\n%s", usage)
	}

	customDefault := New()
	customDefault.PacingFlags(customDefault.Global, customDefault.Global, "parallel-max", "", "", "").
		ParallelMaxHelpDefault("fitted")
	usage = flattenSpace(customDefault.Usage())
	if !strings.Contains(usage, "(default: fitted)") {
		t.Errorf("usage missing custom --parallel-max default:\n%s", usage)
	}
}
