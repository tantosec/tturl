package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

// runDetect adaptively sends batches until it identifies an outlier,
// rules out one meeting the configured boundary, or reaches the comparison
// limit.
func runDetect(ctx context.Context, args []string, out commandOutput) (err error) {
	return runDetectCommand(
		ctx, commandByID(commandDetect), commandCatalogue, args, out)
}

type detectCommandFlags struct {
	falsePositiveRisk *curlblocks.Flag[float64]
	falseNegativeRisk *curlblocks.Flag[float64]
	direction         *curlblocks.Flag[string]
	strategy          *curlblocks.Flag[string]
	width             *curlblocks.Flag[int]
	comparisonsMax    *curlblocks.Flag[string]
	edge              *curlblocks.Flag[float64]
	warmup            *curlblocks.Flag[int]
	padding           positionalPaddingFlags
	responseLimits    *responseLimitFlags
	roles             detectRequestRoleFlags
	view              *rankViewFlags
	pacing            *pacingFlags
	shared            *sharedFlags
}

const detectDefaultTop = 4

func configureDetectCommand(layout commandLayout) detectCommandFlags {
	command := layout.command
	flags := detectCommandFlags{
		direction: command.String(
			"direction", "", "early|late|either", defaultDirection,
			"required; fast=early, slow=late; either needs baselines and a reserved strategy").
			CompletionValues(append(directionNames, directionFastAlias, directionSlowAlias)...),
		strategy: command.String(
			"strategy", "", "STRATEGY", string(detectStrategyAuto),
			"auto: peer-first without baseline blocks, baseline-reserved with them; choices below").
			CompletionValues(detectStrategyNames...),
		width: command.Int("width", "", "N", defaultWidth,
			"requests per comparison"),
		comparisonsMax: command.String(
			"comparisons-max", "", "N|unlimited", defaultComparisonsMax,
			"comparison budget; excludes priming").
			CompletionValues(unlimitedFlagValue),
		falsePositiveRisk: command.Float64(
			"false-positive-risk", "", "P", defaultFalsePositiveRisk,
			"risk of any false find; 0 < P < 1"),
		falseNegativeRisk: command.Float64(
			"false-negative-risk", "", "P", defaultFalseNegativeRisk,
			"risk of excluding a qualifying outlier; 0 < P < 1"),
		edge: command.Float64(
			"edge", "", "EDGE", ranking.OutlierDefaultMinimumEdge,
			"smallest target advantage to rule out; 0 < EDGE < 0.5"),
		warmup: command.Int("warmup", "", "N", 0,
			"make N priming attempts per connection; excludes measured budget"),
		view: addRankViewFlags(
			layout.output,
			rankView{endRows: detectDefaultTop},
			"show N rows at each extreme"),
	}
	command.Table([][]string{
		{"Strategy", "Baselines", "Directions"},
		{"auto", "optional", "early/late; either with baselines"},
		{"peer-first", "optional", "early/late"},
		{"rolling-peer-first", "optional", "early/late"},
		{"baseline-confirmed", "required", "early/late"},
		{"rolling-baseline-confirmed", "required", "early/late"},
		{"baseline-reserved", "required", "early/late/either"},
		{"rolling-baseline-reserved", "required", "early/late/either"},
	})
	command.Notes("tturl detect priming requires at least one --warmup-only block.")
	flags.padding = addPositionalPaddingFlags(command)
	flags.roles = addDetectRequestRoleFlags(layout.parser)
	flags.responseLimits = addResponseLimitFlags(layout.execution)
	flags.pacing = addPacingFlags(layout.parser, layout.execution)
	flags.shared = addSharedFlags(layout)
	return flags
}

func runDetectCommand(
	ctx context.Context,
	spec *commandSpec,
	_ []commandSpec,
	args []string,
	out commandOutput,
) (err error) {
	out, err = commandDiagnosticOutput(spec, args, out)
	if err != nil {
		return err
	}
	layout := newCommand(spec)
	curlblocks.WithStdinReader(newStdinSnapshot(ctx, out))(layout.parser)
	flags := configureDetectCommand(layout)
	plan, done, err := parseCommand(
		layout.parser, spec, args, out.stdout, out.stderr)
	if err != nil || done {
		return err
	}
	reportCfg, err := flags.shared.reporting(plan)
	if err != nil {
		return usage(err)
	}
	dryRun := flags.shared.dryRun.Get(plan.Globals)
	if err := validateDryRunReport(dryRun, reportCfg); err != nil {
		return usage(err)
	}
	warmup := flags.warmup.Get(plan.Globals)
	if warmup < 0 {
		return usage(errors.New("--warmup must be non-negative"))
	}
	roles, baselineSupplies, err := flags.roles.resolve(plan, warmup)
	if err != nil {
		return usage(err)
	}
	if warmup > 0 && !flags.roles.hasDistinctWarmup(plan) {
		return usage(errors.New(
			"--warmup requires at least one --warmup-only request block"))
	}
	comparisonsMax, err := parsePositiveIntOrUnlimited(
		flags.comparisonsMax.Get(plan.Globals))
	if err != nil {
		return usage(fmt.Errorf("--comparisons-max %w", err))
	}
	cfg, err := parseDetect(
		flags.falsePositiveRisk.Get(plan.Globals),
		flags.falseNegativeRisk.Get(plan.Globals),
		flags.direction.Get(plan.Globals),
		flags.width.Get(plan.Globals), comparisonsMax,
		flags.edge.Get(plan.Globals))
	if err != nil {
		return usage(err)
	}
	release, err := flags.shared.bodyRelease(plan)
	if err != nil {
		return usage(err)
	}
	responseLimits, err := flags.responseLimits.resolve(plan.Globals, release)
	if err != nil {
		return usage(err)
	}
	runTimeout, err := flags.shared.resolveRunTimeout(plan.Globals)
	if err != nil {
		return usage(err)
	}
	tuning, err := transportTuning(flags.pacing, plan.Globals)
	if err != nil {
		return usage(err)
	}
	assembled, err := flags.roles.assemble(ctx, plan, roles, baselineSupplies)
	if err != nil {
		return err
	}
	detectPlan, err := newDetectRequestPlan(assembled, cfg.width)
	if err != nil {
		return usage(err)
	}
	strategy, err := resolveDetectStrategy(
		flags.strategy.Get(plan.Globals), cfg.direction,
		detectPlan.baselineTemplates > 0)
	if err != nil {
		return usage(err)
	}
	materialised, err := materialiseAssembled(assembled)
	if err != nil {
		return err
	}
	detectWidth := detectPlan.widthEffective
	pads, err := flags.padding.resolve(plan.Globals, detectWidth)
	if err != nil {
		return usage(err)
	}
	release = materialised.resolveBodyRelease(release, pads)
	measuredLabels := materialised.labels[:len(materialised.reqs)]
	view, err := flags.view.rankView(plan, measuredLabels, materialised.pins)
	if err != nil {
		return usage(err)
	}
	workLimit, participationLimit, err := detectWorkLimits(
		cfg.comparisonsMax, detectWidth)
	if err != nil {
		return usage(err)
	}

	if flags.width.IsSet(plan.Globals) && detectWidth > 2 {
		message := fmt.Sprintf(
			"effective width %d compares %d requests at once; wider batches "+
				"change target concurrency and can change the observed signal. "+
				"Confirm a surprising negative with --width 2.",
			detectWidth, detectWidth)
		if err := writeDiagnostic(
			out.stderr, diagnosticNotice, message,
		); err != nil {
			return err
		}
	}
	if strategy.warning != "" {
		if err := writeDiagnostic(
			out.stderr, diagnosticWarning, strategy.warning,
		); err != nil {
			return err
		}
	}
	activeWidth := materialised.activeBatchWidth(detectWidth, warmup)
	if dryRun {
		delivery, connections, err := previewDeliveryPlan(
			tuning, detectWidth, activeWidth, workLimit, true,
			flags.shared.ignorePeerStreamLimit.Get(plan.Globals))
		if err != nil {
			return usage(err)
		}
		refs := projectRunReferences(
			materialised.labels, detectCandidateReferenceWidth())
		experiment := detectExperimentPlan{
			Candidates: len(materialised.reqs), WidthRequested: cfg.width,
			BaselineTemplates:         detectPlan.baselineTemplates,
			BaselineSupply:            detectPlan.baselineSupply,
			BaselineEffectiveCap:      detectPlan.baselineEffectiveCap,
			StrategyRequested:         strategy.requested,
			StrategyResolved:          strategy.resolved,
			SolverMethod:              strategy.method,
			WidthExplicit:             flags.width.IsSet(plan.Globals),
			WidthEffective:            detectWidth,
			FalsePositiveRisk:         cfg.falsePositiveRisk,
			FalseNegativeRisk:         cfg.falseNegativeRisk,
			Direction:                 cfg.direction,
			ComparisonsMax:            cfg.comparisonsMax,
			ParticipationLimit:        participationLimit,
			MinimumEdge:               cfg.edge,
			Delivery:                  delivery,
			Padding:                   resolvedPadding(pads, detectWidth),
			ResponseLimits:            responseLimits,
			RunTimeout:                runTimeout,
			WarmupTrialsPerConnection: warmup,
			WarmupBatchWidth:          len(materialised.warmupReqs),
			ConnectionLimit:           connections,
			BodyRelease:               release,
			Insecure:                  plan.Globals.Insecure,
		}
		experiment.resolveScheduling(strategy)
		return reportDryRun(
			out, reportCfg, "detect", materialised, refs,
			flags.shared.verbose.Get(plan.Globals),
			func(w *reportWriter) error {
				reportDetectExperiment(w, experiment)
				return nil
			})
	}
	pre, err := prepareMaterialised(materialised, plan, tuning)
	if err != nil {
		return err
	}
	destination, err := openReportDestination(out, reportCfg.output)
	if err != nil {
		return err
	}
	defer func() { err = destination.finish(err) }()
	live := startLiveRunPhase(ctx, runTimeout)
	defer live.cancel()
	tuning, err = fitPipeline(
		live.ctx, pre, tuning, detectWidth, activeWidth, workLimit, true)
	if err != nil {
		cause := live.finish(pre.client.Transport.CloseIdleConnections)
		return liveRunError(err, cause)
	}
	defer pre.client.Transport.CloseIdleConnections()

	report := newReportWriter(destination.writer)
	refs := projectRunReferences(pre.labels, detectCandidateReferenceWidth())
	measuredRefs := runReferences{
		refs: refs.refs[:len(pre.reqs)], compact: refs.compact,
	}
	baselineEnd := len(pre.reqs) + len(pre.baselineTemplates)
	baselineRefs := runReferences{
		refs: refs.refs[len(pre.reqs):baselineEnd], compact: refs.compact,
	}
	view = measuredRefs.projectRankView(view)
	parallel := tuning.connectionLimit
	inFlightComparisonLimit := detectInFlightComparisonLimit(
		parallel, cfg.comparisonsMax, detectWidth)
	initialPriming, maximumPriming, ok := detectPrimingWork(
		warmup, len(pre.warmupReqs), parallel)
	if !ok {
		return usage(errors.New("detect priming work cannot be represented"))
	}
	delivery := resolveDeliveryPlan(
		tuning, detectWidth, parallel,
		flags.shared.ignorePeerStreamLimit.Get(plan.Globals))
	if activeWidth > detectWidth {
		delivery.activeBatchWidth = activeWidth
	}
	var reporter detectReporter = textDetectReporter{
		w: report,
		context: detectReportContext{
			ComparisonsMax:         cfg.comparisonsMax,
			ConnectionLimit:        parallel,
			ConfiguredNegativeEdge: cfg.edge,
			BaselineLabels:         baselineRefs.inlineLabels(),
		},
		width: detectWidth, view: view, refs: measuredRefs,
	}
	var startReport func() error
	echo := report
	if reportCfg.format == reportJSON {
		requests, captureErr := captureDetectRequestRecords(pre, refs)
		if captureErr != nil {
			return captureErr
		}
		allReqs := pre.allRequests()
		dialAddress, originErr := tth2.BatchOrigin(allReqs)
		if originErr != nil {
			return originErr
		}
		jsonReporter := newJSONDetectReporter(report, detectWidth)
		reporter = jsonReporter
		run := DetectRunRecord{
			BuildInfo:                              layout.buildInfo,
			Argv:                                   append([]string{"detect"}, args...),
			StartedAt:                              time.Now(),
			DialAddress:                            dialAddress,
			Origin:                                 commonWireOrigin(allReqs),
			RequestCount:                           len(allReqs),
			CandidateCount:                         detectPlan.candidates,
			BaselineTemplateCount:                  detectPlan.baselineTemplates,
			RequestedStrategy:                      strategy.requested,
			ResolvedStrategy:                       strategy.resolved,
			SolverMethod:                           strategy.method,
			FalsePositiveRisk:                      cfg.falsePositiveRisk,
			FalseNegativeRisk:                      cfg.falseNegativeRisk,
			Direction:                              cfg.direction,
			WidthRequested:                         cfg.width,
			WidthEffective:                         detectWidth,
			ComparisonsMax:                         cfg.comparisonsMax,
			ConfiguredNegativeEdge:                 cfg.edge,
			Padding:                                resolvedPadding(pads, detectWidth),
			ResponseLimits:                         responseLimits,
			RunTimeout:                             runTimeout,
			WarmupTrialsPerConnection:              warmup,
			WarmupBatchWidth:                       len(pre.warmupReqs),
			ConnectionLimit:                        parallel,
			InFlightComparisonLimit:                inFlightComparisonLimit,
			PlannedInitialPrimingRequestOperations: initialPriming,
			PlannedMaximumPrimingRequestOperations: maximumPriming,
			Delivery:                               delivery,
			ReleaseDelay:                           release.releaseDelay,
			BodyBytesWithheld:                      release.bodyBytesWithheld,
			Insecure:                               plan.Globals.Insecure,
		}
		startReport = func() error { return jsonReporter.Run(run, requests) }
		echo = newReportWriter(out.stderr)
		pre.summariseRequestEcho(
			echo, refs, flags.shared.verbose.Get(plan.Globals))
	} else {
		writeBanner(report, toolName+" detect")
		pre.summarise(
			echo, refs, flags.shared.verbose.Get(plan.Globals))
		reportDetectExperiment(report, detectExperimentPlan{
			Candidates: len(pre.reqs), WidthRequested: cfg.width,
			BaselineTemplates:    detectPlan.baselineTemplates,
			BaselineSupply:       detectPlan.baselineSupply,
			BaselineEffectiveCap: detectPlan.baselineEffectiveCap,
			StrategyRequested:    strategy.requested,
			StrategyResolved:     strategy.resolved,
			SolverMethod:         strategy.method,
			WidthExplicit:        flags.width.IsSet(plan.Globals),
			WidthEffective:       detectWidth,
			FalsePositiveRisk:    cfg.falsePositiveRisk,
			FalseNegativeRisk:    cfg.falseNegativeRisk,
			Direction:            cfg.direction, ComparisonsMax: cfg.comparisonsMax,
			ParticipationLimit: participationLimit,
			MinimumEdge:        cfg.edge, Delivery: delivery,
			Padding:                   resolvedPadding(pads, detectWidth),
			ResponseLimits:            responseLimits,
			RunTimeout:                runTimeout,
			WarmupTrialsPerConnection: warmup,
			WarmupBatchWidth:          len(pre.warmupReqs),
			ConnectionLimit:           parallel,
			InFlightComparisonLimit:   inFlightComparisonLimit,
			EvidenceUpdatePolicy:      strategy.evidenceUpdatePolicy(),
			DependencePremise:         strategy.dependencePremise(),
			BodyRelease:               release, Insecure: plan.Globals.Insecure,
		})
	}
	if err := echo.Err(); err != nil {
		if reportCfg.format == reportText && destination.isClosedPipe(err) {
			return nil
		}
		return err
	}
	if reportCfg.format == reportJSON {
		if err := reportPreflightWarnings(
			echo, jsonPreflightWarnings(delivery),
		); err != nil {
			return err
		}
	}
	if startReport != nil {
		if err := startReport(); err != nil {
			if destination.isClosedPipe(err) {
				return nil
			}
			return err
		}
	}
	sharedSendOptions := []tth2.SendOption{
		tth2.WithReleaseDelay(release.delay()),
		tth2.WithBodyBytesWithheld(release.bodyBytesWithheld),
	}
	if flags.shared.ignorePeerStreamLimit.Get(plan.Globals) {
		sharedSendOptions = append(
			sharedSendOptions, tth2.WithPeerStreamLimitIgnored())
	}
	sharedSendOptions = append(
		sharedSendOptions, responseLimits.sendOptions()...)
	comparisonOptions := append(
		[]tth2.SendOption(nil), sharedSendOptions...)
	if len(pads) > 0 {
		comparisonOptions = append(comparisonOptions, tth2.WithPadding(pads...))
	}
	primingOptions := append([]tth2.SendOption(nil), sharedSendOptions...)
	return driveDetect(
		live.ctx, pre, detectPlan, strategy, cfg,
		tuning.connectionLimit, tuning.batchRate,
		tuning.requestRateMax,
		warmup, measuredRefs.inlineLabels(), reporter, out.stderr,
		destination,
		func() error {
			return live.finish(pre.client.Transport.CloseIdleConnections)
		},
		comparisonOptions, primingOptions)
}

func detectPrimingWork(warmup, width, parallel int) (int, int, bool) {
	batches, ok := checkedProduct(warmup, parallel)
	if !ok {
		return 0, 0, false
	}
	initial, ok := checkedProduct(batches, width)
	if !ok {
		return 0, 0, false
	}
	maximum, ok := checkedProduct(initial, 2)
	return initial, maximum, ok
}

func detectWorkLimits(configured, width int) (
	pipelineLimit, participationLimit int,
	err error,
) {
	if configured < 0 {
		return 0, 0, nil
	}
	participations, ok := checkedProduct(configured, width)
	if !ok {
		return 0, 0, fmt.Errorf(
			"--comparisons-max %d produces too many measured request "+
				"participations at effective width %d",
			configured, width)
	}
	return configured, participations, nil
}

// driveDetect runs the resolved outlier solver over the prepared request
// comparator and presents the result. The configured risks independently
// govern found and negative decisions. The caller validates the batch before
// producing output.
func driveDetect(
	ctx context.Context,
	pre *prepared,
	plan detectRequestPlan,
	strategy detectStrategySelection,
	cfg detectConfig,
	parallel int,
	maxBatchesPerSecond float64,
	maxRequestsPerSecond float64,
	warmupTrials int,
	labels []string,
	reporter detectReporter,
	stderr io.Writer,
	destination *reportDestination,
	liveFinish func() error,
	comparisonOptions []tth2.SendOption,
	primingOptions []tth2.SendOption,
) error {
	m := plan.candidates
	width := plan.widthEffective

	evidence := newDetectEvidenceCollector(m, len(pre.baselineTemplates))
	origin, err := tth2.BatchOrigin(pre.allRequests())
	if err != nil {
		return err
	}
	sc := newLeasedDetectComparator(
		pre.client, origin, pre.reqs, pre.baselineTemplates, pre.warmupReqs,
		width, warmupTrials, parallel, evidence,
		comparisonOptions, primingOptions)
	defer sc.Close()
	cmp := ranking.NewComparator(
		plan.comparatorProfile(parallel),
		sc.compare,
	)
	f, err := strategy.newSolver(
		outlierComparisonLimit(cfg.comparisonsMax, width), parallel)
	if err != nil {
		return err
	}

	progress := newProgressPresenter(stderr)
	req := ranking.OutlierRequest{
		M: m, Direction: cfg.direction, MinimumEdge: cfg.edge,
		FalseFindRisk:     cfg.falsePositiveRisk,
		FalseNegativeRisk: cfg.falseNegativeRisk,
		ProgressCadence:   ranking.EveryInterval(progressInterval),
		Progress: func(r ranking.OutlierProgress) {
			progress.Observe(detectProgressLine(
				r, cfg.comparisonsMax, maxBatchesPerSecond,
				maxRequestsPerSecond, cfg.direction, labels))
		},
	}

	res, err := f.Identify(ctx, req, cmp)
	sc.Close()
	liveCause := liveFinish()
	progress.Done()
	if err != nil {
		completion := liveRunFailed(
			err, liveCause, detectComparisonFailureCode(err))
		terminalErr := err
		if completion.Code == completionRunTimeout {
			terminalErr = reportedCommandFailure(errRunTimeout)
		}
		if stopped, ok := stopInterruptedRun(ctx, completion); ok {
			completion = stopped
			terminalErr = reportedCommandFailure(err)
		}
		reportErr := reporter.Result(evidence.unavailable(err, completion))
		if destination.isClosedPipe(reportErr) {
			return nil
		}
		if reportErr != nil {
			return reportErr
		}
		return terminalErr
	}
	result, err := evidence.result(res)
	if err != nil {
		result := evidence.unavailable(
			err, failedRun(err, completionInternalError))
		reportErr := reporter.Result(result)
		if destination.isClosedPipe(reportErr) {
			return nil
		}
		if reportErr != nil {
			return reportErr
		}
		return err
	}
	result.Completion = finaliseLiveCompletion(
		result.Completion, liveCause, completionComparisonFailed)
	reportErr := reporter.Result(result)
	if destination.isClosedPipe(reportErr) {
		return nil
	}
	if reportErr != nil {
		return reportErr
	}
	if result.Completion.Code == completionRunTimeout {
		return reportedCommandFailure(errRunTimeout)
	}
	return nil
}

func detectComparisonFailureCode(err error) completionCode {
	if errors.Is(err, errDetectComparatorWidth) ||
		errors.Is(err, errDetectComparatorItem) {
		return completionInternalError
	}
	return completionComparisonFailed
}

// outlierComparisonLimit maps detect's unbounded sentinel to the largest
// comparison count whose fixed-width participation total remains representable.
func outlierComparisonLimit(configured, width int) int {
	if configured > 0 {
		return configured
	}
	return math.MaxInt / width
}

func detectInFlightComparisonLimit(connections, comparisons, width int) int {
	return min(connections, outlierComparisonLimit(comparisons, width))
}

func (p *detectExperimentPlan) resolveScheduling(strategy detectStrategySelection) {
	p.EvidenceUpdatePolicy = strategy.evidenceUpdatePolicy()
	p.DependencePremise = strategy.dependencePremise()
	if !p.Delivery.pathFitUnresolved {
		p.InFlightComparisonLimit = detectInFlightComparisonLimit(
			p.ConnectionLimit, p.ComparisonsMax, p.WidthEffective)
	}
}
