package main

import (
	"bytes"
	"context"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

// TestParseCommandErrorHint checks that a parse failure is wrapped with the
// per-command help hint, so a user who mistypes a flag is pointed at the right
// usage.
func TestParseCommandErrorHint(t *testing.T) {
	layout := newCommand(commandByID(commandMeasure))
	p := layout.parser
	addSharedFlags(layout)
	_, done, err := parseCommand(
		p, commandByID(commandMeasure),
		[]string{"--definitely-not-a-flag"}, io.Discard, io.Discard)
	if done {
		t.Fatal("a bad flag is not a done (help/version) outcome")
	}
	if err == nil ||
		!strings.Contains(err.Error(), "see '"+toolName+" measure -h'") {
		t.Errorf("error = %v, want the per-command help hint", err)
	}
}

func TestParseCommandAnnouncesLifecycle(t *testing.T) {
	for _, test := range []struct {
		name      string
		lifecycle commandLifecycle
		want      string
	}{
		{name: "stable"},
		{
			name: "beta",
			lifecycle: commandLifecycle{
				status: commandBeta, message: "Expect change.",
			},
			want: "tturl: notice: probe is beta. Expect change.\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := *commandByID(commandMeasure)
			spec.name = "probe"
			spec.lifecycle = test.lifecycle
			p := newCommand(&spec).parser
			var stdout, stderr bytes.Buffer
			plan, done, err := parseCommand(
				p, &spec, []string{"https://example.test/"},
				&stdout, &stderr)
			if err != nil || done || plan == nil {
				t.Fatalf(
					"parseCommand() = plan %v, done %t, error %v; want a plan",
					plan, done, err)
			}
			if stderr.String() != test.want {
				t.Errorf("stderr = %q, want %q", &stderr, test.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want no lifecycle output", &stdout)
			}
		})
	}
}

func TestFlagErrorsRenderUnsafeInputAsASCII(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  func() error
	}{
		{
			name: "arrangement",
			err: func() error {
				_, err := parseArrangement("caf\xc3\xa9", tth2.ArrangeNone)
				return err
			},
		},
		{
			name: "direction",
			err: func() error {
				_, err := parseDirection("caf\xc3\xa9")
				return err
			},
		},
		{
			name: "padding",
			err: func() error {
				_, err := parsePaddingCadences("caf\xc3\xa9=1")
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.err()
			if err == nil {
				t.Fatal("error = nil, want invalid input rejection")
			}
			for _, b := range []byte(err.Error()) {
				if b >= utf8RuneSelf || b == '\x1b' {
					t.Fatalf("error contains unsafe byte 0x%02x: %v", b, err)
				}
			}
			if !strings.Contains(err.Error(), "$HEX[") {
				t.Errorf("error = %v, want escaped input", err)
			}
		})
	}
}

// helpText returns what `tturl <command> -h` prints. It drives the real run
// function, so the help under test is the help a user gets, registered by the
// command itself rather than by a copy of its registration that could drift.
func helpText(t *testing.T, command string) string {
	t.Helper()
	spec := commandByName(command)
	if spec == nil {
		t.Fatalf("no such command %q", command)
	}
	output, stdout, _ := captureOutput(t)
	err := spec.run(
		context.Background(), spec, commandCatalogue, []string{"-h"}, output)
	// -h is a "done" outcome, not a failure: it prints and returns nil, which is
	// what keeps `-h` from exiting non-zero.
	if err != nil {
		t.Fatalf("%s -h returned %v, want nil", command, err)
	}
	if stdout.Len() == 0 {
		t.Fatalf("%s -h printed nothing", command)
	}
	return stdout.String()
}

type helpMetrics struct {
	lines            int
	widestLine       int
	unwrappedWidth   int
	continuationRows int
	sections         []string
}

// measureHelp records help structure without binding tests to its prose. A
// top-level unindented paragraph closes the option region; examples retain
// their authored indentation and are not mistaken for option rows.
func measureHelp(help string) helpMetrics {
	lines := strings.Split(strings.TrimSuffix(help, "\n"), "\n")
	m := helpMetrics{lines: len(lines)}
	inOptions := false
	optionWidth := 0
	finishOption := func() {
		m.unwrappedWidth = max(m.unwrappedWidth, optionWidth)
		optionWidth = 0
	}
	for _, line := range lines {
		m.widestLine = max(m.widestLine, len(line))
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		isSection := (indent == 0 || indent == 2) &&
			(strings.Contains(trimmed, "options") || trimmed == "Pacing:" ||
				trimmed == "Connections:" || trimmed == "Detect decisions (before --block):" ||
				trimmed == sectionExec+":" || trimmed == "Body release and stream policy:" ||
				trimmed == "Delivery preview:") &&
			strings.HasSuffix(trimmed, ":")
		if isSection {
			finishOption()
			m.sections = append(m.sections, trimmed)
			inOptions = true
			continue
		}
		if inOptions && indent == 0 && trimmed != "" {
			finishOption()
			inOptions = false
		}
		if !inOptions {
			continue
		}
		if strings.HasPrefix(trimmed, "-") && (indent == 2 || indent == 4) {
			finishOption()
			optionWidth = len(line)
		} else if trimmed != "" && indent >= 20 {
			m.continuationRows++
			optionWidth += 1 + len(trimmed)
		} else if trimmed == "" {
			finishOption()
		}
	}
	finishOption()
	return m
}

// TestHelpLayoutContract enforces the durable routine-help layout without
// pinning incidental line or option counts.
func TestHelpLayoutContract(t *testing.T) {
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		t.Run(command, func(t *testing.T) {
			help := helpText(t, command)
			checkHelpTextContract(t, command+" usage", help)
			got := measureHelp(help)
			maxLines := 146
			if command == "detect" {
				maxLines = 170
			}
			if commandByName(command).lifecycle.notice(command) != "" {
				maxLines += 2
			}
			if got.lines > maxLines || got.widestLine > textWidth {
				t.Errorf("metrics = %+v, want at most %d lines and width <=%d",
					got, maxLines, textWidth)
			}
			wantSections := []string{
				strings.ToUpper(command[:1]) + command[1:] + " options:",
				"Connections:",
				"Pacing:",
				sectionExec + ":",
				"Body release and stream policy:",
				"Delivery preview:",
				"Request options:",
				"Block options:",
				"Output options:",
				"Common options:",
			}
			if command == "detect" {
				wantSections[0] = "Detect decisions (before --block):"
			}
			if !slices.Equal(got.sections, wantSections) {
				t.Errorf("sections = %q, want %q",
					got.sections, wantSections)
			}
		})
	}
}

func TestRoutineHelpPointsToCommandGuide(t *testing.T) {
	for _, spec := range commandCatalogue {
		if !spec.requestGrammar {
			continue
		}
		help := helpText(t, spec.name)
		if !strings.Contains(help, "Guide:\n  "+toolName+" help "+spec.name) {
			t.Errorf("%s help omits its command guide:\n%s",
				spec.name, help)
		}
	}
}

// TestHelpSections checks command design, delivery and presentation groups.
func TestHelpSections(t *testing.T) {
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		u := helpText(t, command)
		sections := []string{sectionOutput + ":", sectionExec + ":"}
		for _, want := range sections {
			if !strings.Contains(u, want) {
				t.Errorf("%s help missing the %q heading:\n%s", command, want, u)
			}
		}
		flags := []string{
			"--report", "--output", "--batch-rate-max",
			"--request-rate-max", "--connections-fit-max", "--release-delay",
		}
		if command != "analyse" {
			flags = append(flags, "--connections")
		}
		if command == "measure" || command == "detect" {
			flags = append(flags, "--rank-rows", "--pin")
		}
		flags = append(flags, "--body-bytes-withheld")
		flags = append(flags, "--ignore-peer-stream-limit")
		for _, flag := range flags {
			if !strings.Contains(u, flag) {
				t.Errorf("%s help missing %s:\n%s", command, flag, u)
			}
		}
		// The command's own subject flag leads, ahead of both headings.
		own := map[string]string{
			"race": "--trials", "measure": "--trials",
			"analyse": "--cycles", "detect": "--false-positive-risk",
		}[command]
		firstSection := sectionOutput
		if i, j := strings.Index(u, own), strings.Index(u, firstSection); i < 0 || j < 0 || i > j {
			t.Errorf("%s: want %s listed before the %q heading (at %d and %d):\n%s",
				command, own, firstSection, i, j, u)
		}
	}

	u := helpText(t, "race")
	for _, flag := range []string{
		"--trials", "--warmup", "--arrange", "--padding", "--padding-at",
		"--warmup-only", "--report", "--output", "--connections",
		"--connections-fit-max", "--batch-rate-max", "--request-rate-max",
	} {
		if !strings.Contains(u, flag) {
			t.Errorf("race help should list %s:\n%s", flag, u)
		}
	}
	detect := helpText(t, "detect")
	for _, flag := range []string{
		"--comparisons-max", "--warmup", "--warmup-only",
		"--padding", "--padding-at",
	} {
		if !strings.Contains(detect, flag) {
			t.Errorf("detect help does not expose %s:\n%s", flag, detect)
		}
	}
	measure := helpText(t, "measure")
	for _, flag := range []string{
		"--cycles", "--warmup-only", "--padding", "--padding-at",
	} {
		if !strings.Contains(measure, flag) {
			t.Errorf("measure help does not expose %s:\n%s", flag, measure)
		}
	}
	analyse := helpText(t, "analyse")
	for _, flag := range []string{"--warmup-only", "--padding", "--padding-at"} {
		if !strings.Contains(analyse, flag) {
			t.Errorf("analyse help does not expose %s:\n%s", flag, analyse)
		}
	}
	for _, flag := range []string{
		"--cycles", "--warmup", "--connections", "--batch-rate-max",
		"--connections-fit-max", "--request-rate-max",
	} {
		if !strings.Contains(analyse, flag) {
			t.Errorf("analyse help does not expose %s:\n%s", flag, analyse)
		}
	}
	for _, flag := range []string{"--arrange", "--trials"} {
		if strings.Contains(analyse, flag) {
			t.Errorf("analyse help unexpectedly exposes %s:\n%s", flag, analyse)
		}
	}
}

func TestTransportTuning(t *testing.T) {
	t.Parallel()
	p := curlblocks.New()
	pf := addPacingFlags(p, p.Global)
	plan, err := p.Parse([]string{
		"--connections", "4",
		"--batch-rate-max", "20/s",
		"--request-rate-max", "100/s",
		"https://example.com/",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tuning, err := transportTuning(pf, plan.Globals)
	if err != nil {
		t.Fatalf("transportTuning: %v", err)
	}
	if tuning.maxConns != 4 || !tuning.parallelSet {
		t.Errorf("connections = %d, explicit %t; want 4, true",
			tuning.maxConns, tuning.parallelSet)
	}
	if tuning.batchRate != 20 ||
		tuning.minBatchInterval != 50*time.Millisecond {
		t.Errorf("batch rate = %v, interval = %v; want 20/s, 50ms",
			tuning.batchRate, tuning.minBatchInterval)
	}
	if tuning.requestRateMax != 100 {
		t.Errorf("request-rate maximum = %v, want 100/s",
			tuning.requestRateMax)
	}
}

func TestTransportTuningFitMaximum(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		want    int
		wantErr string
	}{
		{
			"positive maximum",
			[]string{"--connections-fit", "--batch-rate-max", "20/s", "--connections-fit-max", "600"},
			600, "",
		},
		{
			"unlimited",
			[]string{"--connections-fit", "--batch-rate-max", "20/s", "--connections-fit-max", "unlimited"},
			-1, "",
		},
		{
			"zero",
			[]string{"--connections-fit", "--batch-rate-max", "20/s", "--connections-fit-max", "0"},
			0, "positive decimal integer or unlimited",
		},
		{
			"without batch rate",
			[]string{"--connections-fit", "--connections-fit-max", "600"},
			0, "requires a finite pacing limit",
		},
		{
			"unlimited without batch rate",
			[]string{"--connections-fit", "--connections-fit-max", "unlimited"},
			0, "requires a finite pacing limit",
		},
		{
			"with explicit parallelism",
			[]string{
				"--connections-fit", "--batch-rate-max", "20/s", "--connections", "4",
				"--connections-fit-max", "600",
			},
			0, "conflicts with --connections",
		},
		{
			"unlimited with explicit parallelism",
			[]string{
				"--connections-fit", "--batch-rate-max", "20/s", "--connections", "4",
				"--connections-fit-max", "unlimited",
			},
			0, "conflicts with --connections",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p := curlblocks.New()
			pf := addPacingFlags(p, p.Global)
			args := append(append([]string(nil), test.args...),
				"https://example.com/")
			plan, err := p.Parse(args)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			tuning, err := transportTuning(pf, plan.Globals)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("transportTuning error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("transportTuning: %v", err)
			}
			if !tuning.parallelFitSet || tuning.parallelFitMax != test.want {
				t.Errorf("tuning = %+v, want fit maximum %d",
					tuning, test.want)
			}
		})
	}
}

func TestParsePositiveIntOrUnlimited(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		value   string
		want    int
		wantErr bool
	}{
		{value: "1", want: 1},
		{value: "007", want: 7},
		{value: "unlimited", want: -1},
		{value: "UnLiMiTeD", want: -1},
		{value: "", wantErr: true},
		{value: "0", wantErr: true},
		{value: "-1", wantErr: true},
		{value: "-2", wantErr: true},
		{value: "+1", wantErr: true},
		{value: "1.0", wantErr: true},
		{value: " unlimited", wantErr: true},
		{value: "unlimited ", wantErr: true},
		{value: "unlimit", wantErr: true},
		{value: strconv.FormatUint(uint64(math.MaxInt)+1, 10), wantErr: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			got, err := parsePositiveIntOrUnlimited(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf(
						"parsePositiveIntOrUnlimited(%q) = %d, want error",
						test.value, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Errorf(
					"parsePositiveIntOrUnlimited(%q) = (%d, %v), want (%d, nil)",
					test.value, got, err, test.want)
			}
		})
	}
}

func TestParseRace(t *testing.T) {
	cfg, err := parseRace(3, 2, "")
	if err != nil {
		t.Fatalf("parseRace(valid): %v", err)
	}
	if cfg.trials != 3 || cfg.warmup != 2 {
		t.Errorf("cfg = %+v, want {trials:3 warmup:2}", cfg)
	}
	unbounded, err := parseRace(-1, 0, "")
	if err != nil {
		t.Fatalf("parseRace(unbounded): %v", err)
	}
	if unbounded.trials != -1 {
		t.Errorf("unbounded trials = %d, want -1", unbounded.trials)
	}
	if cfg.arrangement != tth2.ArrangeRandom {
		t.Errorf("three-trial default arrangement policy = %v, want random",
			cfg.arrangement)
	}
	one, err := parseRace(1, 0, "")
	if err != nil || one.arrangement != tth2.ArrangeNone {
		t.Errorf("one-trial default = (%+v, %v), want arrange none", one, err)
	}
	explicit, err := parseRace(1, 0, "rotate")
	if err != nil || explicit.arrangement != tth2.ArrangeRotate {
		t.Errorf("explicit arrangement policy = (%+v, %v), want rotate",
			explicit, err)
	}

	for _, c := range []struct {
		name           string
		trials, warmup int
		wantSub        string
	}{
		{name: "trials 0", trials: 0, wantSub: "--trials"},
		{name: "trials below sentinel", trials: -2, wantSub: "--trials"},
		{name: "negative warmup", trials: 1, warmup: -1, wantSub: "--warmup"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseRace(c.trials, c.warmup, ""); err == nil ||
				!strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("parseRace error = %v, want one mentioning %q",
					err, c.wantSub)
			}
		})
	}
	if _, err := parseRace(1, 0, "auto"); err == nil ||
		!strings.Contains(err.Error(), "--arrange") {
		t.Errorf("parseRace arrange error = %v, want --arrange rejection", err)
	}
}

// TestParseMeasure covers the measure flag validation in one place, as
// TestParseDetect does for detect: a valid set flows into the config, and each
// knob is rejected outside its range before a connection is opened.
func TestParseMeasure(t *testing.T) {
	cfg, err := parseMeasure(30, true, 0, false, 2, "none")
	if err != nil {
		t.Fatalf("parseMeasure(valid): %v", err)
	}
	if cfg.trials != 30 || cfg.warmup != 2 ||
		cfg.arrangement != tth2.ArrangeNone {
		t.Errorf("cfg = %+v, want trials 30, warmup 2, arrange none", cfg)
	}
	defaultCfg, err := parseMeasure(20, false, 0, false, 0, "")
	if err != nil || defaultCfg.arrangement != tth2.ArrangeRotate ||
		defaultCfg.budget != (measureBudget{unit: "trials", requested: 20}) {
		t.Errorf("measure default = (%+v, %v), want rotate", defaultCfg, err)
	}

	cyclesCfg, err := parseMeasure(20, false, 5, true, 2, "rotate")
	if err != nil || cyclesCfg.cycles != 5 ||
		cyclesCfg.budget != (measureBudget{unit: "cycles", requested: 5}) {
		t.Errorf("cycle budget = (%+v, %v)", cyclesCfg, err)
	}

	bad := []struct {
		name                   string
		trials, cycles, warmup int
		trialsSet, cyclesSet   bool
		arrangement, wantSub   string
	}{
		{"trials 0", 0, 0, 0, true, false, "rotate", "--trials"},
		{"negative trials", -1, 0, 0, true, false, "rotate", "--trials"},
		{"cycles 0", 20, 0, 0, false, true, "rotate", "--cycles"},
		{"negative cycles", 20, -1, 0, false, true, "rotate", "--cycles"},
		{"conflicting budgets", 20, 2, 0, true, true, "rotate", "mutually exclusive"},
		{"cycles random", 20, 2, 0, false, true, "random", "requires --arrange rotate"},
		{"cycles none", 20, 2, 0, false, true, "none", "requires --arrange rotate"},
		{"negative warmup", 20, 0, -1, false, false, "rotate", "--warmup"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseMeasure(
				c.trials, c.trialsSet, c.cycles, c.cyclesSet,
				c.warmup, c.arrangement); err == nil ||
				!strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("parseMeasure error = %v, want one mentioning %q", err, c.wantSub)
			}
		})
	}
}

func TestResolveMeasureBudget(t *testing.T) {
	cfg := measureConfig{
		cycles: 5, budget: measureBudget{unit: "cycles", requested: 5},
		arrangement: tth2.ArrangeRotate,
	}
	requested, planned, err := resolveMeasureBudget(cfg, 8)
	if err != nil || requested != 40 || planned != 40 {
		t.Errorf("cycle budget = %d requested, %d planned, %v; want 40, 40, nil",
			requested, planned, err)
	}

	for _, test := range []struct {
		name   string
		cycles int
		width  int
	}{
		{"trial count", math.MaxInt, 2},
		{"request workload", math.MaxInt / 2, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg.cycles = test.cycles
			cfg.budget.requested = test.cycles
			if _, _, err := resolveMeasureBudget(cfg, test.width); err == nil ||
				!strings.Contains(err.Error(), "too") {
				t.Errorf("resolve error = %v, want overflow rejection", err)
			}
		})
	}
}

// TestResolveTrials covers the rotation round-up: with rotation the count is
// raised to the next multiple of the request count (every request must pass
// through every stream position) and the adjustment is announced;
// the other arrangement policies use the count as given.
func TestResolveTrials(t *testing.T) {
	t.Run("rounds up and says so", func(t *testing.T) {
		got, note, err := resolveTrials(7, 3, tth2.ArrangeRotate)
		if err != nil {
			t.Fatalf("resolveTrials: %v", err)
		}
		if got != 9 { // next multiple of 3 at or above 7
			t.Errorf("trials = %d, want 9", got)
		}
		for _, want := range []string{"7", "9", "3 requests"} {
			if !strings.Contains(note, want) {
				t.Errorf("note = %q, want it to mention %q", note, want)
			}
		}
	})

	t.Run("an exact multiple needs no note", func(t *testing.T) {
		got, note, err := resolveTrials(9, 3, tth2.ArrangeRotate)
		if err != nil || got != 9 {
			t.Fatalf("resolveTrials = (%d, %v), want (9, nil)", got, err)
		}
		if note != "" {
			t.Errorf("note = %q, want none when nothing was adjusted", note)
		}
	})

	t.Run("none uses the count as given", func(t *testing.T) {
		got, note, err := resolveTrials(7, 3, tth2.ArrangeNone)
		if err != nil || got != 7 || note != "" {
			t.Errorf("resolveTrials(none) = (%d, %q, %v), want (7, \"\", nil)",
				got, note, err)
		}
	})

	t.Run("random uses the count as given", func(t *testing.T) {
		got, note, err := resolveTrials(7, 3, tth2.ArrangeRandom)
		if err != nil || got != 7 || note != "" {
			t.Errorf("resolveTrials(random) = (%d, %q, %v), want (7, \"\", nil)",
				got, note, err)
		}
	})

	t.Run("a count that cannot be rounded up is rejected", func(t *testing.T) {
		if _, _, err := resolveTrials(
			math.MaxInt, 3, tth2.ArrangeRotate); err == nil ||
			!strings.Contains(err.Error(), "too large") {
			t.Errorf("error = %v, want a too-large rejection rather than a wrapped count", err)
		}
	})
}

func TestRoundUpToMultiple(t *testing.T) {
	cases := []struct{ v, m, want int }{
		{10, 4, 12}, {12, 4, 12}, {1, 4, 4}, {0, 4, 0}, {7, 7, 7}, {8, 7, 14},
	}
	for _, c := range cases {
		got, ok := roundUpToMultiple(c.v, c.m)
		if !ok {
			t.Errorf("roundUpToMultiple(%d, %d) reported overflow, want %d", c.v, c.m, c.want)
			continue
		}
		if got != c.want {
			t.Errorf("roundUpToMultiple(%d, %d) = %d, want %d", c.v, c.m, got, c.want)
		}
	}

	// The next multiple of 4 above MaxInt-1 is not representable. Reporting that
	// beats returning a wrapped negative, which would be printed as a trial count.
	if _, ok := roundUpToMultiple(math.MaxInt-1, 4); ok {
		t.Errorf("roundUpToMultiple(MaxInt-1, 4) = ok, want an overflow report")
	}
	// MaxInt is itself a multiple of 1, so it needs no rounding and cannot
	// overflow.
	if got, ok := roundUpToMultiple(math.MaxInt, 1); !ok || got != math.MaxInt {
		t.Errorf("roundUpToMultiple(MaxInt, 1) = (%d, %v), want (MaxInt, true)", got, ok)
	}
}

// TestParseDetect covers the detect flag validation in one place: a fully
// valid set flows into the config, and each knob is rejected outside its
// range before a connection is opened.
func TestDetectUsageShowsConservativeDefaults(t *testing.T) {
	layout := newCommand(commandByID(commandDetect))
	configureDetectCommand(layout)
	usage := layout.parser.Usage()
	for _, want := range []string{
		"--width N                    requests per comparison (default: 2)",
		"comparison budget; excludes priming\n                               (default: 50000)",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("detect usage omits %q:\n%s", want, usage)
		}
	}
}

func TestParseDetect(t *testing.T) {
	cfg, err := parseDetect(0.03, 0.07, "late", 8, -1, 0.1)
	if err != nil {
		t.Fatalf("valid config: unexpected error %v", err)
	}
	if cfg.falsePositiveRisk != 0.03 || cfg.falseNegativeRisk != 0.07 ||
		cfg.direction != ranking.Late || cfg.width != 8 || cfg.edge != 0.1 {
		t.Errorf("parsed config = %+v, want the supplied values", cfg)
	}

	bad := []struct {
		name                  string
		falsePositiveRisk     float64
		falseNegativeRisk     float64
		direction             string
		edge                  float64
		width, comparisonsMax int
		wantSub               string
	}{
		{"false-positive risk 0", 0, 0.05, "late", 0.02, 8, -1, "--false-positive-risk"},
		{"false-positive risk 1", 1, 0.05, "late", 0.02, 8, -1, "--false-positive-risk"},
		{"false-positive risk NaN", math.NaN(), 0.05, "late", 0.02, 8, -1, "--false-positive-risk"},
		{"false-positive risk infinity", math.Inf(1), 0.05, "late", 0.02, 8, -1, "--false-positive-risk"},
		{"false-negative risk 0", 0.05, 0, "late", 0.02, 8, -1, "--false-negative-risk"},
		{"false-negative risk 1", 0.05, 1, "late", 0.02, 8, -1, "--false-negative-risk"},
		{"false-negative risk NaN", 0.05, math.NaN(), "late", 0.02, 8, -1, "--false-negative-risk"},
		{"false-negative risk infinity", 0.05, math.Inf(1), "late", 0.02, 8, -1, "--false-negative-risk"},
		{"direction", 0.05, 0.05, "up", 0.02, 8, -1, "--direction"},
		{"width < 2", 0.05, 0.05, "late", 0.02, 1, -1, "--width"},
		{"comparisons zero", 0.05, 0.05, "late", 0.02, 8, 0, "--comparisons-max"},
		{"comparisons below sentinel", 0.05, 0.05, "late", 0.02, 8, -2, "--comparisons-max"},
		{"edge 0", 0.05, 0.05, "late", 0, 8, -1, "--edge"},
		{"edge 0.5", 0.05, 0.05, "late", 0.5, 8, -1, "--edge"},
		{"edge NaN", 0.05, 0.05, "late", math.NaN(), 8, -1, "--edge"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parseDetect(
				c.falsePositiveRisk, c.falseNegativeRisk,
				c.direction, c.width, c.comparisonsMax,
				c.edge,
			); err == nil ||
				!strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("parseDetect error = %v, want one mentioning %q", err, c.wantSub)
			}
		})
	}
}

func TestDirectionVocabulary(t *testing.T) {
	for word, want := range map[string]ranking.OutlierDirection{
		"early": ranking.Early, "late": ranking.Late, "either": ranking.Either,
		"fast": ranking.Early, "slow": ranking.Late,
	} {
		if got, err := parseDirection(word); err != nil || got != want {
			t.Errorf("parseDirection(%q) = (%v, %v), want %v", word, got, err, want)
		}
	}
	if _, err := parseDirection("sideways"); err == nil {
		t.Error("parseDirection(sideways) should error")
	}
	if got := directionWord(ranking.Early); got != "early" {
		t.Errorf("directionWord(Early) = %q, want early", got)
	}
	if got := directionWord(ranking.Late); got != "late" {
		t.Errorf("directionWord(Late) = %q, want late", got)
	}
	if got := directionWord(ranking.Either); got != "either" {
		t.Errorf("directionWord(Either) = %q, want either", got)
	}
}

// pinView parses args under measure's flag set with --pin registered, assembles
// the plan, and resolves the ranking view, matching the path used to turn a
// --pin argument into a set of pinned labels.
func pinView(t *testing.T, args ...string) (rankView, error) {
	t.Helper()
	layout := newCommand(commandByID(commandMeasure))
	p := layout.parser
	rv := addRankViewFlags(
		layout.output, rankView{allRows: true},
		"show N earliest and N latest ranking rows, or all")
	addSharedFlags(layout)
	plan, err := p.Parse(args)
	if err != nil {
		t.Fatalf("parse(%q): %v", args, err)
	}
	a, err := assemble(t.Context(), plan)
	if err != nil {
		t.Fatalf("assemble(%q): %v", args, err)
	}
	return rv.rankView(plan, a.labels, a.pins)
}

func detectView(t *testing.T, args ...string) (rankView, error) {
	t.Helper()
	layout := newCommand(commandByID(commandDetect))
	flags := configureDetectCommand(layout)
	plan, err := layout.parser.Parse(args)
	if err != nil {
		t.Fatalf("parse(%q): %v", args, err)
	}
	a, err := assemble(t.Context(), plan)
	if err != nil {
		t.Fatalf("assemble(%q): %v", args, err)
	}
	return flags.view.rankView(plan, a.labels, a.pins)
}

func TestRankViewCommandDefaultsAndValues(t *testing.T) {
	defaultView, err := pinView(t,
		"--block", "https://h/a", "--block", "https://h/b")
	if err != nil {
		t.Fatal(err)
	}
	if !defaultView.allRows {
		t.Fatal("default result view elides rows")
	}

	limitedView, err := pinView(t, "--rank-rows", "3",
		"--block", "https://h/a", "--block", "https://h/b")
	if err != nil {
		t.Fatal(err)
	}
	if limitedView.allRows || limitedView.endRows != 3 {
		t.Fatalf("explicit --rank-rows view = %+v, want a three-row limit",
			limitedView)
	}

	explicitAll, err := pinView(t, "--rank-rows", "all",
		"--block", "https://h/a", "--block", "https://h/b")
	if err != nil {
		t.Fatal(err)
	}
	if !explicitAll.allRows {
		t.Fatalf("explicit --rank-rows all view = %+v, want all rows", explicitAll)
	}

	detectDefault, err := detectView(t,
		"--block", "https://h/a", "--block", "https://h/b")
	if err != nil {
		t.Fatal(err)
	}
	if detectDefault.allRows || detectDefault.endRows != detectDefaultTop {
		t.Fatalf("default detect view = %+v, want %d rows per end",
			detectDefault, detectDefaultTop)
	}

	for _, value := range []string{"-1", "many"} {
		if _, err := pinView(t, "--rank-rows", value,
			"--block", "https://h/a", "--block", "https://h/b"); err == nil {
			t.Errorf("--rank-rows %s: got nil error", value)
		}
	}
}

// TestRankViewPinFan checks --pin naming a whole fan, by its token and by its
// --name, and that an unknown pin fails fast.
func TestRankViewPinFan(t *testing.T) {
	fan := func(t *testing.T, pinArg string) (rankView, error) {
		return pinView(t, "--pin", pinArg,
			"--block", "https://h/FUZZ", "--name", "sweep", "--vary", "FUZZ=0-2")
	}
	for _, key := range []string{"FUZZ", "sweep"} { // by token, then by --name
		view, err := fan(t, key)
		if err != nil {
			t.Fatalf("--pin %s: %v", key, err)
		}
		for _, label := range []string{"sweep:FUZZ=0", "sweep:FUZZ=1", "sweep:FUZZ=2"} {
			if !view.pinned[label] {
				t.Errorf("--pin %s did not pin %s", key, label)
			}
		}
	}
	if _, err := fan(t, "nope"); err == nil || !strings.Contains(err.Error(), "no request or fan named nope") {
		t.Errorf("--pin nope error = %v, want a not-found error", err)
	}
}

// TestRankViewPinRendersArgument checks that a pin matches whichever way the
// user spells a name a label had to render: as they typed it, or as the output
// shows it back. A name is pinnable either way because rendering an already
// rendered name is a no-op. An argument that matches nothing is reported
// rendered too, so a control byte in it cannot reach the terminal.
func TestRankViewPinRendersArgument(t *testing.T) {
	const name = "caf\xc3\xa9"
	rendered := curlblocks.DisplayText(name)
	if rendered == name {
		t.Fatalf("DisplayText(%q) = %q, want rendered test input", name, rendered)
	}

	named := func(t *testing.T, pinArg string) (rankView, error) {
		return pinView(t, "--pin", pinArg,
			"--block", "https://h/a", "--name", name, "--block", "https://h/b")
	}

	for _, spelling := range []string{name, rendered} {
		view, err := named(t, spelling)
		if err != nil {
			t.Fatalf("--pin %q: %v", spelling, err)
		}
		if !view.pinned[rendered] {
			t.Errorf("--pin %q did not pin %q; pinned = %v", spelling, rendered, view.pinned)
		}
	}

	_, err := named(t, "a\x1b[31mX")
	if err == nil {
		t.Fatal("--pin with a control byte: got nil error, want a not-found error")
	}
	if strings.ContainsRune(err.Error(), 0x1b) {
		t.Errorf("error %q carries the raw control byte", err)
	}
	if !strings.Contains(err.Error(), curlblocks.DisplayText("a\x1b[31mX")) {
		t.Errorf("error %q does not name the pin in its rendered form", err)
	}
}

func TestRateControlComposition(t *testing.T) {
	for _, tc := range []struct {
		batch, requests string
		interval        time.Duration
	}{
		{"", "", 0},
		{"unlimited", "unlimited", 0},
		{"10/s", "", 100 * time.Millisecond},
		{"", "25/s", 200 * time.Millisecond},
		{"10/s", "unlimited", 100 * time.Millisecond},
		{"unlimited", "25/s", 200 * time.Millisecond},
		{"10/s", "25/s", 200 * time.Millisecond},
	} {
		t.Run(tc.batch+"_"+tc.requests, func(t *testing.T) {
			p := curlblocks.New()
			pf := addPacingFlags(p, p.Global)
			args := []string{"--connections", "1", "https://example.com/"}
			if tc.batch != "" {
				args = append(args, "--batch-rate-max", tc.batch)
			}
			if tc.requests != "" {
				args = append(args, "--request-rate-max", tc.requests)
			}
			plan, err := p.Parse(args)
			if err != nil {
				t.Fatal(err)
			}
			tuning, err := transportTuning(pf, plan.Globals)
			if err != nil {
				t.Fatal(err)
			}
			if got := effectiveBatchInterval(tuning, 5); got != tc.interval {
				t.Errorf("interval = %v, want %v", got, tc.interval)
			}
		})
	}
}

func TestConnectionSelectionPlans(t *testing.T) {
	for _, tc := range []struct {
		name       string
		args       []string
		selection  connectionFit
		unresolved bool
		invalid    bool
	}{
		{"default", nil, connectionDefault, false, false},
		{"batch pacing", []string{"--batch-rate-max", "10/s"}, connectionDefault, false, false},
		{"request pacing", []string{"--request-rate-max", "12/s"}, connectionDefault, false, false},
		{"batch fitting", []string{"--connections-fit", "--batch-rate-max", "10/s"}, connectionPathPing, true, false},
		{"request fitting", []string{"--connections-fit", "--request-rate-max", "12/s"}, connectionPathPing, true, false},
		{
			"combined fitting",
			[]string{"--connections-fit", "--batch-rate-max", "10/s", "--request-rate-max", "12/s"},
			connectionPathPing, true, false,
		},
		{"fixed pacing", []string{"--connections", "3", "--batch-rate-max", "10/s"}, connectionExplicit, false, false},
		{
			"fixed fit conflict",
			[]string{"--connections", "1", "--connections-fit", "--batch-rate-max", "10/s"},
			"", false, true,
		},
		{"cap prerequisite", []string{"--connections-fit-max", "8", "--batch-rate-max", "10/s"}, "", false, true},
		{
			"unlimited cap prerequisite",
			[]string{"--connections-fit-max", "unlimited", "--batch-rate-max", "10/s"},
			"", false, true,
		},
		{
			"finite rate prerequisite",
			[]string{"--connections-fit", "--batch-rate-max", "unlimited", "--request-rate-max", "unlimited"},
			"", false, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := curlblocks.New()
			pf := addPacingFlags(p, p.Global)
			parsed, err := p.Parse(append(tc.args, "https://example.com/"))
			if err != nil {
				t.Fatal(err)
			}
			tuning, err := transportTuning(pf, parsed.Globals)
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid selection accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			plan, _, err := previewDeliveryPlan(tuning, 2, 2, 10, true, false)
			if err != nil {
				t.Fatal(err)
			}
			if plan.connectionSelection != tc.selection || plan.pathFitUnresolved != tc.unresolved {
				t.Errorf("plan = %+v", plan)
			}
			if tc.selection == connectionDefault && plan.connectionLimit != 1 {
				t.Errorf("default capacity = %d", plan.connectionLimit)
			}
		})
	}
}

func TestSingleTrialFittingPlan(t *testing.T) {
	tuning := transportTuningConfig{
		maxConns: 1, fitSelected: true,
		batchRate: 10, minBatchInterval: 100 * time.Millisecond,
		parallelFitSet: true, parallelFitMax: 8,
	}
	plan, capacity, err := previewDeliveryPlan(tuning, 2, 2, 1, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.pathFitInactive || plan.pathFitUnresolved || capacity != 1 || plan.parallelFitMax != 8 {
		t.Errorf("single trial plan = %+v, capacity %d", plan, capacity)
	}
	plan, _, err = previewDeliveryPlan(tuning, 2, 2, 1, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.pathFitUnresolved || plan.pathFitInactive {
		t.Errorf("repeated trial plan = %+v", plan)
	}
}
