package main

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

type usageError struct {
	err error
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

func usage(err error) error {
	if err == nil {
		return nil
	}
	return &usageError{err: err}
}

// parseCommand parses args and handles the meta outcomes: on -h it prints the
// command's help and on -V its version, reporting done=true either way; a parse
// error is wrapped with a per-command help hint. A successfully parsed command
// announces its lifecycle before returning. The caller returns early when done
// is true or err is non-nil.
func parseCommand(
	p *curlblocks.Parser,
	spec *commandSpec,
	args []string,
	stdout, stderr io.Writer,
) (plan *curlblocks.Plan, done bool, err error) {
	plan, err = p.Parse(args)
	switch {
	case errors.Is(err, curlblocks.ErrHelp):
		_, err = fmt.Fprint(stdout, renderCommandUsage(spec, p))
		if err != nil {
			return nil, false, err
		}
		return nil, true, nil
	case errors.Is(err, curlblocks.ErrVersion):
		_, err = fmt.Fprintln(stdout, p.Version())
		if err != nil {
			return nil, false, err
		}
		return nil, true, nil
	case err != nil:
		var invalid *pflag.InvalidValueError
		if spec.id == commandMeasure && errors.As(err, &invalid) &&
			invalid.GetFlag().Name == "trials" {
			err = fmt.Errorf("--trials must be a positive decimal integer, got %q",
				curlblocks.DisplayText(invalid.GetValue()))
		}
		return nil, false, usage(fmt.Errorf(
			"%w (see '%s -h')", err, p.Name))
	}
	if err := announceCommand(stderr, spec); err != nil {
		return nil, false, err
	}
	return plan, false, nil
}

// sharedFlags are the command-wide globals every request command registers on
// top of curlblocks' request grammar. Request labelling (--name) is
// curlblocks' own: the labels arrive resolved on each expanded group (see
// assemble).
type sharedFlags struct {
	releaseDelay          *curlblocks.Flag[string]
	bodyBytesWithheld     *curlblocks.Flag[int]
	ignorePeerStreamLimit *curlblocks.Flag[bool]
	runTimeout            *curlblocks.Flag[string]
	dryRun                *curlblocks.Flag[bool]
	verbose               *curlblocks.Flag[bool]
	report                *curlblocks.Flag[string]
	output                *curlblocks.Flag[string]
}

type reportFormat string

const (
	reportText reportFormat = "text"
	reportJSON reportFormat = "json"
)

var reportFormatNames = []string{string(reportText), string(reportJSON)}

type reportConfig struct {
	format reportFormat
	output string
}

func (sf *sharedFlags) bodyRelease(
	plan *curlblocks.Plan,
) (bodyReleasePlan, error) {
	delay, err := time.ParseDuration(sf.releaseDelay.Get(plan.Globals))
	if err != nil || delay < 0 {
		return bodyReleasePlan{}, fmt.Errorf(
			"--release-delay must be a non-negative Go duration")
	}
	withheld := sf.bodyBytesWithheld.Get(plan.Globals)
	if withheld < 0 {
		return bodyReleasePlan{}, fmt.Errorf(
			"--body-bytes-withheld must be >= 0, got %d", withheld)
	}
	return bodyReleasePlan{
		releaseDelay:      delay,
		bodyBytesWithheld: withheld,
		explicitlyConfigured: sf.releaseDelay.IsSet(plan.Globals) ||
			sf.bodyBytesWithheld.IsSet(plan.Globals),
	}, nil
}

func (sf *sharedFlags) sendOptions(
	plan *curlblocks.Plan,
	release bodyReleasePlan,
) []tth2.TrialsOption {
	opts := []tth2.TrialsOption{
		tth2.WithReleaseDelay(release.delay()),
		tth2.WithBodyBytesWithheld(release.bodyBytesWithheld),
	}
	if sf.ignorePeerStreamLimit.Get(plan.Globals) {
		opts = append(opts, tth2.WithPeerStreamLimitIgnored())
	}
	return opts
}

// reporting resolves the primary report format and destination.
func (sf *sharedFlags) reporting(plan *curlblocks.Plan) (reportConfig, error) {
	cfg := reportConfig{
		format: reportFormat(sf.report.Get(plan.Globals)),
		output: sf.output.Get(plan.Globals),
	}
	switch cfg.format {
	case reportText:
	case reportJSON:
	default:
		return reportConfig{}, fmt.Errorf(
			"--report must be text or json, got %q",
			curlblocks.DisplayText(string(cfg.format)))
	}
	if cfg.output == "" {
		return reportConfig{}, fmt.Errorf("--output must not be empty")
	}
	return cfg, nil
}

// addSharedFlags registers the command-wide globals shared by every request
// command, each into the help section it belongs to: --release-delay shapes
// how the batch is sent; the report flags and -v shape where and how it is
// presented.
func addSharedFlags(layout commandLayout) *sharedFlags {
	exec, view := layout.execution, layout.output
	view.Bool("silent", "s", false, silentDescription)
	return &sharedFlags{
		releaseDelay: layout.release.String("release-delay", "", "DURATION", "0s",
			"delay withheld-tail release; applies only when body bytes are withheld"),
		bodyBytesWithheld: layout.release.Int("body-bytes-withheld", "", "N", 1,
			"N >= 0; 0 has no fixed tail; non-empty bodies share a final record"),
		ignorePeerStreamLimit: layout.release.Bool(
			"ignore-peer-stream-limit", "", false,
			"exceed the advertised peer stream limit"),
		runTimeout: exec.String(
			"run-timeout", "", "DURATION|unlimited", unlimitedFlagValue,
			"limit live fitting and request execution").CompletionValues(
			unlimitedFlagValue),
		dryRun: layout.preview.Bool(
			"dry-run", "", false,
			"network-free preview; requires --report text; creates/truncates an output FILE"),
		verbose: view.Bool("verbose", "v", false,
			"show resolved request fields and full bodies"),
		report: view.String("report", "", "FORMAT", string(reportText),
			"text or json (JSON Lines)").CompletionValues(reportFormatNames...),
		output: view.String("output", "o", "FILE", "-",
			"write report or preview; - means stdout; creates/truncates FILE").CompletionFiles(
			curlblocks.CompletionFiles).CompletionValues("-"),
	}
}

// rankViewFlags are the output-shaping globals shared by measure and detect:
// rows at each end of the elided ranking (--rank-rows) and request labels to
// always show (--pin, repeatable).
type rankViewFlags struct {
	top *curlblocks.Flag[string]
	pin *curlblocks.Flag[[]string]
}

const rankTopAll = "all"

// addRankViewFlags registers --rank-rows and --pin in the output section.
// Commands share the value grammar while declaring their own truthful default.
func addRankViewFlags(
	r *curlblocks.Registry,
	defaultView rankView,
	topHelp string,
) *rankViewFlags {
	defaultTop := rankTopAll
	if !defaultView.allRows {
		defaultTop = strconv.Itoa(defaultView.endRows)
	}
	return &rankViewFlags{
		top: r.String("rank-rows", "", "N|all", defaultTop, topHelp).
			CompletionValues(rankTopAll),
		pin: r.StringArray("pin", "", "NAME", nil,
			"also show request or variation fan; repeatable"),
	}
}

// rankView resolves --rank-rows and --pin into ranking-table layout. Pins name
// a request label or a complete fan through its name or token. It renders pin
// values before matching and reporting them, keeping diagnostics ASCII.
func (rv *rankViewFlags) rankView(
	plan *curlblocks.Plan,
	labels []string,
	pinGroups map[string][]string,
) (rankView, error) {
	rawTop := rv.top.Get(plan.Globals)
	view := rankView{allRows: rawTop == rankTopAll}
	if !view.allRows {
		top, err := strconv.Atoi(rawTop)
		if err != nil || top < 0 {
			return rankView{}, fmt.Errorf(
				"--rank-rows must be a non-negative integer or all, got %q",
				curlblocks.DisplayText(rawTop))
		}
		view.endRows = top
	}
	pins := rv.pin.Get(plan.Globals)
	if len(pins) == 0 {
		return view, nil
	}
	known := make(map[string]bool, len(labels))
	for _, n := range labels {
		known[n] = true
	}
	pinned := map[string]bool{}
	var unknown []string
	for _, raw := range pins {
		p := curlblocks.DisplayText(raw)
		switch {
		case pinGroups[p] != nil:
			for _, label := range pinGroups[p] {
				pinned[label] = true
			}
		case known[p]:
			pinned[p] = true
		default:
			unknown = append(unknown, p)
		}
	}
	if len(unknown) > 0 {
		return rankView{}, fmt.Errorf("--pin: no request or fan named %s (labels appear in the Requests summary)",
			strings.Join(unknown, ", "))
	}
	view.pinned = pinned
	return view, nil
}

// addPacingFlags registers the shared delivery controls with tturl's own help
// text. Batch cadence is the primary offered-load control; the request cap is
// the optional safety ceiling for batches whose widths differ.
type pacingFlags struct {
	shared         *curlblocks.PacingFlags
	fit            *curlblocks.Flag[bool]
	parallelFitMax *curlblocks.Flag[string]
}

func addPacingFlags(p *curlblocks.Parser, r *curlblocks.Registry) *pacingFlags {
	r = r.Section("Connections")
	rates := p.Global.Section("Pacing").Notes(
		"N is a positive integer. UNIT: s seconds, m minutes, h hours, d days.",
		"Both limits can be used; the more restrictive rate wins.",
		"Pacing includes priming; requests within a batch stay together.",
	)
	shared := p.PacingFlags(r, rates, "connections",
		"use up to N connections; cannot combine with --connections-fit",
		"limit batch starts",
		"limit requests by waiting between batches",
	)
	fit := r.Bool("connections-fit", "", false,
		"choose connections automatically; requires a finite pacing limit").
		HelpDefault("off")
	fitMax := r.String(
		"connections-fit-max", "", "N|unlimited", "",
		"limit automatically chosen connections; "+
			"requires --connections-fit; unlimited = no cap",
	).HelpDefault("").CompletionValues(unlimitedFlagValue)
	return &pacingFlags{shared: shared, fit: fit, parallelFitMax: fitMax}
}

type transportTuningConfig struct {
	maxConns         int
	parallelSet      bool
	fitSelected      bool
	parallelFitMax   int
	parallelFitSet   bool
	batchRate        float64
	requestRateMax   float64
	minBatchInterval time.Duration
	connectionFit    connectionFit
	connectionLimit  int
	pathRTTSamples   []time.Duration
	pathRTT          time.Duration
	pathFittedConns  int
	pathFitCapped    bool
}

func transportTuning(
	pf *pacingFlags,
	g curlblocks.Globals,
) (transportTuningConfig, error) {
	var tuning transportTuningConfig
	maxConns, err := pf.shared.ParallelMax(g)
	if err != nil {
		return tuning, err
	}
	batchRate, err := pf.shared.BatchRateMax(g)
	if err != nil {
		return tuning, err
	}
	requestRate, err := pf.shared.RequestRateMax(g)
	if err != nil {
		return tuning, err
	}
	parallelFitSet := pf.parallelFitMax.IsSet(g)
	parallelFitMax := 0
	if parallelFitSet {
		parallelFitMax, err = parsePositiveIntOrUnlimited(
			pf.parallelFitMax.Get(g))
		if err != nil {
			return tuning, fmt.Errorf("--connections-fit-max %w", err)
		}
	}
	parallelSet := pf.shared.ParallelMaxIsSet(g)
	fitSelected := pf.fit.Get(g)
	if fitSelected && parallelSet {
		return tuning, fmt.Errorf("--connections-fit conflicts with --connections")
	}
	if parallelFitSet && !fitSelected {
		return tuning, fmt.Errorf("--connections-fit-max requires --connections-fit")
	}
	if fitSelected && batchRate <= 0 && requestRate <= 0 {
		return tuning, fmt.Errorf("--connections-fit requires a finite pacing limit")
	}
	tuning = transportTuningConfig{
		maxConns:       maxConns,
		parallelSet:    parallelSet,
		fitSelected:    fitSelected,
		parallelFitMax: parallelFitMax,
		parallelFitSet: parallelFitSet,
		batchRate:      batchRate,
		requestRateMax: requestRate,
	}
	if batchRate > 0 {
		tuning.minBatchInterval = time.Duration(math.Ceil(
			float64(time.Second) / batchRate))
	}
	return tuning, nil
}

// Flag defaults for the commands.
const (
	defaultRaceTrials        = 1
	defaultTrials            = 20
	defaultWarmup            = 0
	defaultFalsePositiveRisk = 0.01
	defaultFalseNegativeRisk = 0.05
	defaultDirection         = ""
	defaultWidth             = 2
	defaultComparisonsMax    = "50000"
)

const (
	arrangeNone        = "none"
	arrangeRotate      = "rotate"
	arrangeRandom      = "random"
	directionLate      = "late"
	directionEarly     = "early"
	directionEither    = "either"
	directionSlowAlias = "slow"
	directionFastAlias = "fast"
)

var (
	arrangementNames = []string{arrangeNone, arrangeRotate, arrangeRandom}
	directionNames   = []string{directionEarly, directionLate, directionEither}
)

// Help descriptions shared by race and measure. Their accepted ranges and
// defaults remain command-specific.
const (
	measureTrialsHelp = "request N trials; excludes --cycles; rotate rounds up to whole cycles"
	warmupHelp        = "make N priming attempts per connection; excludes measured budget"
	arrangeHelp       = "none, rotate, random"
)

// raceConfig is the validated race configuration.
type raceConfig struct {
	trials      int
	warmup      int
	arrangement tth2.ArrangementPolicy
}

func parseRace(trials, warmup int, arrange string) (raceConfig, error) {
	if trials != -1 && trials < 1 {
		return raceConfig{}, fmt.Errorf(
			"--trials must be positive or unlimited, got %d", trials)
	}
	if warmup < 0 {
		return raceConfig{}, fmt.Errorf("--warmup must be >= 0, got %d", warmup)
	}
	defaultPolicy := tth2.ArrangeRandom
	if trials == 1 {
		defaultPolicy = tth2.ArrangeNone
	}
	arrangement, err := parseArrangement(arrange, defaultPolicy)
	if err != nil {
		return raceConfig{}, err
	}
	return raceConfig{
		trials: trials, warmup: warmup, arrangement: arrangement,
	}, nil
}

// requireComparable rejects a batch too small to race: with fewer than two
// requests there is no arrival order to compare. subject names the command, so
// the error reads in the vocabulary the user invoked.
func requireComparable(n int, subject string) error {
	if n >= 2 {
		return nil
	}
	return fmt.Errorf("%s needs at least 2 requests to compare (got %d); add requests with --repeat or more "+
		"--block URLs", subject, n)
}

// measureConfig is the validated measure configuration, the counterpart of
// [detectConfig]: parsed from the raw flags before a connection is opened, so a
// bad value fails fast rather than after the work.
type measureConfig struct {
	trials      int // requested --trials; zero when cycles supplies the budget
	cycles      int // requested --cycles; zero for a trial budget
	budget      measureBudget
	warmup      int // priming trials to discard before the counted ones
	arrangement tth2.ArrangementPolicy
}

type measureBudget struct {
	unit      string
	requested int
}

// parseMeasure validates the measure flags.
func parseMeasure(
	trials int,
	trialsSet bool,
	cycles int,
	cyclesSet bool,
	warmup int,
	arrange string,
) (measureConfig, error) {
	if trialsSet && cyclesSet {
		return measureConfig{}, errors.New(
			"--cycles and --trials are mutually exclusive")
	}
	if trialsSet && trials < 1 {
		return measureConfig{}, fmt.Errorf("--trials must be a positive decimal integer, got %d", trials)
	}
	if cyclesSet && cycles < 1 {
		return measureConfig{}, fmt.Errorf("--cycles must be >= 1, got %d", cycles)
	}
	if warmup < 0 {
		return measureConfig{}, fmt.Errorf("--warmup must be >= 0, got %d", warmup)
	}
	arrangement, err := parseArrangement(arrange, tth2.ArrangeRotate)
	if err != nil {
		return measureConfig{}, err
	}
	if cyclesSet && arrangement != tth2.ArrangeRotate {
		detail := "the random policy has no complete position-balance cycles"
		if arrangement == tth2.ArrangeNone {
			detail = "the none policy does not rotate positions"
		}
		return measureConfig{}, fmt.Errorf(
			"--cycles requires --arrange rotate; %s (use --trials)", detail)
	}
	budget := measureBudget{unit: "trials", requested: trials}
	if cyclesSet {
		budget = measureBudget{unit: "cycles", requested: cycles}
		trials = 0
	}
	return measureConfig{
		trials: trials, cycles: cycles, budget: budget,
		warmup: warmup, arrangement: arrangement,
	}, nil
}

func resolveMeasureBudget(
	cfg measureConfig,
	width int,
) (requestedTrials, plannedTrials int, err error) {
	if cfg.budget.unit == "cycles" {
		requestedTrials, ok := checkedProduct(cfg.cycles, width)
		if !ok {
			return 0, 0, fmt.Errorf(
				"--cycles %d is too large for a batch of %d requests",
				cfg.cycles, width)
		}
		if _, ok := checkedProduct(requestedTrials, width); !ok {
			return 0, 0, fmt.Errorf(
				"--cycles %d produces too many planned measured request operations for a batch of %d requests",
				cfg.cycles, width)
		}
		return requestedTrials, requestedTrials, nil
	}
	plannedTrials, _, err = resolveTrials(
		cfg.trials, width, cfg.arrangement)
	if err != nil {
		return 0, 0, err
	}
	if _, ok := checkedProduct(plannedTrials, width); !ok {
		return 0, 0, errors.New(
			"planned measured request workload is too large")
	}
	return cfg.trials, plannedTrials, nil
}

// resolveTrials turns the requested --trials into the count actually run, plus
// the note explaining any adjustment ("" when none is needed).
//
// Rotation requires a multiple of the request count; the other policies accept
// the requested count unchanged.
func resolveTrials(
	requested, n int,
	arrangement tth2.ArrangementPolicy,
) (trials int, note string, err error) {
	if arrangement != tth2.ArrangeRotate {
		return requested, "", nil
	}
	trials, ok := roundUpToMultiple(requested, n)
	if !ok {
		return 0, "", fmt.Errorf("--trials %d is too large to round up to a multiple of the %d requests; "+
			"lower it or pass --arrange none or --arrange random", requested, n)
	}
	if trials != requested {
		note = fmt.Sprintf("--trials %d rounded up to %d so each of the %d requests rotates through "+
			"every position equally.", requested, trials, n)
	}
	return trials, note, nil
}

func parseArrangement(
	raw string,
	defaultPolicy tth2.ArrangementPolicy,
) (tth2.ArrangementPolicy, error) {
	switch raw {
	case "":
		return defaultPolicy, nil
	case arrangeNone:
		return tth2.ArrangeNone, nil
	case arrangeRotate:
		return tth2.ArrangeRotate, nil
	case arrangeRandom:
		return tth2.ArrangeRandom, nil
	default:
		return 0, fmt.Errorf(
			"--arrange must be none, rotate, or random, got %q",
			curlblocks.DisplayText(raw))
	}
}

func arrangementName(policy tth2.ArrangementPolicy) string {
	switch policy {
	case tth2.ArrangeNone:
		return "none"
	case tth2.ArrangeRandom:
		return "random"
	default:
		return "rotate"
	}
}

// roundUpToMultiple returns the smallest multiple of m that is >= v. m must be
// positive. ok is false when that multiple exceeds the largest int, so a caller
// reports the input as out of range rather than passing on a wrapped negative.
func roundUpToMultiple(v, m int) (rounded int, ok bool) {
	r := v % m
	if r == 0 {
		return v, true
	}
	up := v + (m - r)
	if up < v { // wrapped past the maximum int
		return 0, false
	}
	return up, true
}

// detectConfig is the validated detect configuration, parsed from the raw flags
// before any connection is opened.
type detectConfig struct {
	// falsePositiveRisk is the family-wise false-find risk, in (0, 1).
	falsePositiveRisk float64
	// falseNegativeRisk is the qualifying-target exclusion risk, in (0, 1).
	falseNegativeRisk float64
	// direction identifies which outlier to look for.
	direction ranking.OutlierDirection
	// width is the requested batch width, capped to the field size later.
	width int
	// comparisonsMax is -1 when comparisons are unbounded.
	comparisonsMax int
	// edge is ranking's negative MinimumEdge, in (0, 0.5).
	edge float64
}

// parseDetect validates the detect flags. It runs before connecting, so a bad
// value fails fast rather than after a handshake. A strict interval check also
// rejects NaN because every ordered comparison with NaN is false.
func parseDetect(
	falsePositiveRisk, falseNegativeRisk float64,
	direction string,
	width, comparisonsMax int,
	edge float64,
) (detectConfig, error) {
	if !(falsePositiveRisk > 0 && falsePositiveRisk < 1) {
		return detectConfig{}, fmt.Errorf(
			"--false-positive-risk must be in (0, 1), got %v",
			falsePositiveRisk)
	}
	if !(falseNegativeRisk > 0 && falseNegativeRisk < 1) {
		return detectConfig{}, fmt.Errorf(
			"--false-negative-risk must be in (0, 1), got %v",
			falseNegativeRisk)
	}
	parsedDirection, err := parseDirection(direction)
	if err != nil {
		return detectConfig{}, err
	}
	if width < 2 {
		return detectConfig{}, fmt.Errorf("--width must be >= 2, got %d", width)
	}
	if comparisonsMax != -1 && comparisonsMax < 1 {
		return detectConfig{}, fmt.Errorf(
			"--comparisons-max must be positive or unlimited, got %d",
			comparisonsMax)
	}
	if math.IsNaN(edge) || edge <= 0 || edge >= 0.5 {
		return detectConfig{}, fmt.Errorf("--edge must be in (0, 0.5), got %v", edge)
	}
	return detectConfig{
		falsePositiveRisk: falsePositiveRisk,
		falseNegativeRisk: falseNegativeRisk,
		direction:         parsedDirection, width: width,
		comparisonsMax: comparisonsMax, edge: edge,
	}, nil
}

// parseDirection maps the required command value and quiet compatibility
// aliases to ranking's direction vocabulary.
func parseDirection(s string) (ranking.OutlierDirection, error) {
	switch s {
	case directionLate, directionSlowAlias:
		return ranking.Late, nil
	case directionEarly, directionFastAlias:
		return ranking.Early, nil
	case directionEither:
		return ranking.Either, nil
	default:
		return 0, fmt.Errorf(
			"--direction must be early, late, or either, got %q",
			curlblocks.DisplayText(s))
	}
}
