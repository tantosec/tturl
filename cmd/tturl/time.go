package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"time"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/timing"
)

type timeCommandFlags struct {
	protocols      *curlblocks.ProtocolFlags
	roles          requestRoleFlags
	pacing         *curlblocks.PacingFlags
	trials         *curlblocks.Flag[string]
	arrange        *curlblocks.Flag[string]
	warmup         *curlblocks.Flag[int]
	synchronise    *curlblocks.Flag[bool]
	singleRecord   *curlblocks.Flag[bool]
	lastByte       *curlblocks.Flag[bool]
	releaseDelay   *curlblocks.Flag[string]
	requestTimeout *curlblocks.Flag[string]
	runTimeout     *curlblocks.Flag[string]
	bodyMax        *curlblocks.Flag[string]
	headers        *curlblocks.Flag[bool]
	captureBody    *curlblocks.Flag[string]
	extracts       *curlblocks.Flag[[]string]
	shared         *sharedFlags
}

type timeCommandConfig struct {
	trials         int
	arrangement    string
	warmup         int
	connections    int
	connectionsSet bool
	synchronise    bool
	singleRecord   bool
	lastByte       bool
	releaseDelay   time.Duration
	requestTimeout time.Duration
	runTimeout     time.Duration
	bodyMax        int64
	batchRate      float64
	requestRate    float64
	report         reportConfig
	capture        raceCaptureConfig
	dryRun         bool
	verbose        bool
}

func configureTimeCommand(layout commandLayout) timeCommandFlags {
	p := layout.parser
	p.Description += ". Requests execute sequentially within each trial by default. " +
		"'--synchronise' uses common release gates on separate connections."
	experiment := layout.command
	timing := layout.execution
	flags := timeCommandFlags{
		protocols: p.ProtocolFlags(p.Shared.Section("Protocol")),
		roles:     addRequestRoleFlags(p),
		trials: experiment.String("trials", "", "N|unlimited", "1",
			"request N trials; rotate completes whole cycles").
			CompletionValues(unlimitedFlagValue),
		arrange: experiment.String("arrange", "", "POLICY", "",
			"none, rotate, random; default none at one trial and random otherwise").HelpDefault("").
			CompletionValues(arrangementNames...),
		warmup: experiment.Int("warmup", "", "N", 0, "complete N unmeasured trials per worker; see tturl help time"),
		synchronise: p.Global.Section("Connections").Bool("synchronise", "", false,
			"release each trial through common gates on separate connections"),
		singleRecord: layout.release.Bool("single-record", "", true,
			"main request write in one TLS record; false permits larger requests"),
		lastByte: layout.release.Bool("last-byte-sync", "", false,
			"hold finalising traffic for a separate TLS record"),
		releaseDelay: layout.release.String("release-delay", "", "DURATION", "0s",
			"delay finalising release; nonzero requires --last-byte-sync"),
		requestTimeout: timing.String("request-timeout", "", "DURATION|unlimited", "30s",
			"bound acquisition, initial readiness and execution separately; pacing excluded").
			CompletionValues(unlimitedFlagValue),
		runTimeout: timing.String("run-timeout", "", "DURATION|unlimited", unlimitedFlagValue,
			"limit live acquisition, warmup, execution and disposal").
			CompletionValues(unlimitedFlagValue),
		bodyMax: timing.String("response-body-max", "", "SIZE|unlimited", "8MiB",
			"limit accepted response body bytes per request").
			CompletionValues(unlimitedFlagValue),
		headers: layout.output.Bool("capture-headers", "", false,
			"retain complete response headers and trailers; requires JSON"),
		captureBody: layout.output.String("capture-body", "", "SIZE|unlimited", "",
			"retain response body prefixes; use JSON or extraction").HelpDefault("disabled").
			CompletionValues(unlimitedFlagValue),
		extracts: layout.output.StringArray("extract-regex", "", "REGEX", nil,
			"search retained body prefixes; repeatable"),
	}
	timing.Notes("Duration runs from initial request release to valid final response headers.",
		"SIZE is bytes, optionally suffixed B, KiB, MiB or GiB.")
	rates := p.Global.Section("Pacing").Notes(
		"N is a positive integer. UNIT: s seconds, m minutes, h hours, d days.",
		"Both limits bound trial starts; warmup shares the global pacing budget.")
	flags.pacing = p.PacingFlags(p.Global.Section("Connections"), rates, "connections",
		"global physical connection ceiling", "limit trial starts",
		"limit request operations through trial pacing").ParallelMaxHelpDefault("derived minimum")
	layout.output.Bool("silent", "s", false, silentDescription)
	flags.shared = &sharedFlags{
		report: layout.output.String("report", "", "FORMAT", "text",
			"text or json (JSON Lines)").
			CompletionValues(reportFormatNames...),
		output: layout.output.String("output", "o", "FILE", "-",
			"write report or preview; - means stdout; creates/truncates FILE").
			CompletionFiles(curlblocks.CompletionFiles).
			CompletionValues("-"),
		verbose: layout.output.Bool("verbose", "v", false, "show resolved request fields and full bodies"),
		dryRun: layout.preview.Bool("dry-run", "", false,
			"network-free preview; requires text; creates/truncates output FILE"),
	}
	return flags
}

func (flags timeCommandFlags) resolve(plan *curlblocks.Plan) (timeCommandConfig, error) {
	g := plan.Globals
	var cfg timeCommandConfig
	count, err := parsePositiveIntOrUnlimited(flags.trials.Get(g))
	if err != nil {
		return cfg, fmt.Errorf("--trials %w", err)
	}
	if count < 0 {
		count = 0
	}
	cfg.trials = count
	cfg.arrangement = flags.arrange.Get(g)
	if cfg.arrangement == "" {
		cfg.arrangement = arrangeRandom
		if count == 1 {
			cfg.arrangement = arrangeNone
		}
	}
	if cfg.arrangement != arrangeNone && cfg.arrangement != arrangeRandom && cfg.arrangement != arrangeRotate {
		return cfg, fmt.Errorf("--arrange must be none, rotate, or random")
	}
	cfg.warmup = flags.warmup.Get(g)
	if cfg.warmup < 0 {
		return cfg, fmt.Errorf("--warmup must be >= 0")
	}
	if err := flags.roles.validate(plan, cfg.warmup); err != nil {
		return cfg, err
	}
	cfg.connections, err = flags.pacing.ParallelMax(g)
	if err != nil {
		return cfg, err
	}
	cfg.connectionsSet = flags.pacing.ParallelMaxIsSet(g)
	cfg.batchRate, err = flags.pacing.BatchRateMax(g)
	if err != nil {
		return cfg, err
	}
	cfg.requestRate, err = flags.pacing.RequestRateMax(g)
	if err != nil {
		return cfg, err
	}
	cfg.synchronise = flags.synchronise.Get(g)
	cfg.singleRecord = flags.singleRecord.Get(g)
	cfg.lastByte = flags.lastByte.Get(g)
	cfg.releaseDelay, err = time.ParseDuration(flags.releaseDelay.Get(g))
	if err != nil || cfg.releaseDelay < 0 {
		return cfg, fmt.Errorf("--release-delay must be a non-negative Go duration")
	}
	if cfg.releaseDelay > 0 && !cfg.lastByte {
		return cfg, fmt.Errorf("nonzero --release-delay requires --last-byte-sync")
	}
	cfg.requestTimeout, err = parseTimeTimeout("request-timeout", flags.requestTimeout.Get(g))
	if err != nil {
		return cfg, err
	}
	if cfg.requestTimeout > 0 && cfg.requestTimeout <= cfg.releaseDelay {
		return cfg, fmt.Errorf("--request-timeout must exceed --release-delay")
	}
	cfg.runTimeout, err = parseTimeTimeout("run-timeout", flags.runTimeout.Get(g))
	if err != nil {
		return cfg, err
	}
	cfg.bodyMax, err = parsePositiveByteSizeOrUnlimited(flags.bodyMax.Get(g))
	if err != nil {
		return cfg, fmt.Errorf("--response-body-max %w", err)
	}
	cfg.report, err = flags.shared.reporting(plan)
	if err != nil {
		return cfg, err
	}
	cfg.dryRun = flags.shared.dryRun.Get(g)
	cfg.verbose = flags.shared.verbose.Get(g)
	if err := validateDryRunReport(cfg.dryRun, cfg.report); err != nil {
		return cfg, err
	}
	cfg.capture, err = parseRaceCapture(flags.headers.Get(g), flags.captureBody.Get(g),
		flags.extracts.Get(g), cfg.report.format)
	return cfg, err
}

func parseTimeTimeout(name, value string) (time.Duration, error) {
	duration, unlimited, err := parseDurationOrUnlimited(value)
	if err != nil || !unlimited && duration <= 0 {
		return 0, fmt.Errorf("--%s must be a positive Go duration or unlimited", name)
	}
	if unlimited {
		return 0, nil
	}
	return duration, nil
}

func timeStartupAllowed(platform string, args []string) error {
	if platform != "windows" {
		return nil
	}
	layout := newCommand(&commandSpec{id: commandTime, name: "time", requestGrammar: true})
	configureTimeCommand(layout)
	denied := errors.New("request sources unavailable during platform check")
	curlblocks.WithFileReader(func(string) ([]byte, error) { return nil, denied })(layout.parser)
	curlblocks.WithStdinReader(func() ([]byte, error) { return nil, denied })(layout.parser)
	_, err := layout.parser.Parse(args)
	if errors.Is(err, curlblocks.ErrHelp) {
		return nil
	}
	return fmt.Errorf("tturl time is unsupported on Windows; use Linux or macOS")
}

func (cfg timeCommandConfig) enginePlan(requests timeRequests, insecure bool) timing.Plan {
	convert := func(items []timeRequest) []timing.Request {
		var result []timing.Request
		for _, item := range items {
			protocol := timing.HTTP2
			if item.Protocol == curlblocks.HTTP11 {
				protocol = timing.HTTP11
			}
			result = append(result, timing.Request{ID: item.ID, HTTP: item.Request, Body: item.Body, Protocol: protocol})
		}
		return result
	}
	connections := 0
	if cfg.connectionsSet {
		connections = cfg.connections
	}
	bodyMax := cfg.bodyMax
	if bodyMax == 0 {
		bodyMax = -1
	}
	var trials uint64
	if cfg.trials > 0 {
		trials = uint64(cfg.trials)
	}
	return timing.Plan{
		Requests: convert(requests.Measured), Priming: convert(requests.Priming),
		Trials: trials, Arrangement: timing.Arrangement(cfg.arrangement),
		Warmup: cfg.warmup, Connections: connections, Synchronise: cfg.synchronise,
		SingleRecord: cfg.singleRecord, LastByteSync: cfg.lastByte, ReleaseDelay: cfg.releaseDelay,
		RequestTimeout: cfg.requestTimeout, RunTimeout: cfg.runTimeout,
		BatchRate: cfg.batchRate, RequestRate: cfg.requestRate,
		ReceiveHeaderMax: tturlMaxResponseHeaderBytes, ResponseBodyMax: bodyMax,
		Capture: timing.Capture{
			Headers: cfg.capture.headers, Body: cfg.capture.bodyBytes != 0, BodyMax: cfg.capture.bodyBytes,
		},
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			//nolint:gosec // The explicit -k request option disables verification.
			InsecureSkipVerify: insecure,
		},
	}
}
