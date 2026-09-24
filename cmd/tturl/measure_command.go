package main

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/tth2"
)

var errMeasurementUnavailable = errors.New("measurement unavailable; see report")

// runMeasure runs the resolved trial or cycle budget and reports descriptive
// rank distributions for the fixed batch.
func runMeasure(ctx context.Context, args []string, out commandOutput) (err error) {
	return runMeasureCommand(
		ctx, commandByID(commandMeasure), commandCatalogue, args, out)
}

type measureCommandFlags struct {
	trials         *curlblocks.Flag[int]
	cycles         *curlblocks.Flag[int]
	warmup         *curlblocks.Flag[int]
	arrange        *curlblocks.Flag[string]
	padding        positionalPaddingFlags
	roles          requestRoleFlags
	view           *rankViewFlags
	responseLimits *responseLimitFlags
	pacing         *pacingFlags
	shared         *sharedFlags
}

func configureMeasureCommand(layout commandLayout) measureCommandFlags {
	command := layout.command
	flags := measureCommandFlags{
		trials: command.Int("trials", "", "N", defaultTrials,
			measureTrialsHelp),
		cycles: command.Int("cycles", "", "N", 0,
			"run N cycles; excludes --trials; needs rotate policy, which is the default").HelpDefault(""),
		warmup: command.Int("warmup", "", "N", defaultWarmup,
			warmupHelp),
		arrange: command.String("arrange", "", "POLICY", "rotate",
			arrangeHelp).CompletionValues(arrangementNames...),
		view: addRankViewFlags(
			layout.output,
			rankView{allRows: true},
			"show N rows at each extreme"),
	}
	flags.padding = addPositionalPaddingFlags(command)
	flags.roles = addRequestRoleFlags(layout.parser)
	flags.responseLimits = addResponseLimitFlags(layout.execution)
	flags.pacing = addPacingFlags(layout.parser, layout.execution)
	flags.shared = addSharedFlags(layout)
	return flags
}

func runMeasureCommand(
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
	flags := configureMeasureCommand(layout)
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
	cfg, err := parseMeasure(
		flags.trials.Get(plan.Globals), flags.trials.IsSet(plan.Globals),
		flags.cycles.Get(plan.Globals), flags.cycles.IsSet(plan.Globals),
		flags.warmup.Get(plan.Globals),
		flags.arrange.Get(plan.Globals),
	)
	if err != nil {
		return usage(err)
	}
	if err := flags.roles.validate(plan, cfg.warmup); err != nil {
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
	assembled, err := flags.roles.assemble(ctx, plan)
	if err != nil {
		return err
	}
	pads, err := flags.padding.resolve(plan.Globals, assembled.measuredCount)
	if err != nil {
		return usage(err)
	}
	materialised, err := materialiseAssembled(assembled)
	if err != nil {
		return err
	}
	release = materialised.resolveBodyRelease(release, pads)
	// Everything below depends on the assembled batch, so it could not be checked
	// at flag time; it still runs before the summary is printed.
	if err := requireComparable(len(materialised.reqs), "measure"); err != nil {
		return usage(err)
	}
	measuredLabels := materialised.labels[:len(materialised.reqs)]
	view, err := flags.view.rankView(plan, measuredLabels, materialised.pins)
	if err != nil {
		return usage(err)
	}
	requestedTrials, count, err := resolveMeasureBudget(cfg, len(materialised.reqs))
	if err != nil {
		return usage(err)
	}
	cfg.trials = requestedTrials
	if dryRun {
		delivery, connections, err := previewDeliveryPlan(
			tuning, len(materialised.reqs), materialised.activeBatchWidth(len(materialised.reqs), cfg.warmup),
			trialConnectionWorkLimit(count, len(materialised.reqs), cfg.arrangement),
			true, flags.shared.ignorePeerStreamLimit.Get(plan.Globals))
		if err != nil {
			return usage(err)
		}
		refs := projectRunReferences(materialised.labels, rankProfileLabelWidth)
		experimentPlan := measurePlan{
			requested: requestedTrials, planned: count, budget: cfg.budget,
			warmup: cfg.warmup, width: len(materialised.reqs),
			warmupWidth: materialised.warmupBatchWidth(cfg.warmup),
			padding:     resolvedPadding(pads, len(materialised.reqs)),
			arrangement: cfg.arrangement, delivery: delivery,
			bodyRelease: release, responseLimits: responseLimits,
			runTimeout: runTimeout, insecure: plan.Globals.Insecure,
		}
		return reportDryRun(
			out, reportCfg, "measure", materialised, refs,
			flags.shared.verbose.Get(plan.Globals),
			func(w *reportWriter) error {
				return reportMeasureExperiment(w, experimentPlan, connections)
			})
	}
	pre, err := prepareMaterialised(materialised, plan, tuning)
	if err != nil {
		return err
	}
	live := startLiveRunPhase(ctx, runTimeout)
	defer live.cancel()
	tuning, err = fitPipeline(
		live.ctx, pre, tuning, len(pre.reqs), pre.activeBatchWidth(len(pre.reqs), cfg.warmup),
		trialConnectionWorkLimit(count, len(pre.reqs), cfg.arrangement), true)
	if err != nil {
		cause := live.finish(pre.client.Transport.CloseIdleConnections)
		return liveRunError(err, cause)
	}
	defer pre.client.Transport.CloseIdleConnections()

	refs := projectRunReferences(pre.labels, rankProfileLabelWidth)
	measuredRefs := runReferences{refs: refs.refs[:len(pre.reqs)], compact: refs.compact}
	experimentPlan := measurePlan{
		requested: requestedTrials, planned: count, budget: cfg.budget,
		warmup: cfg.warmup, width: len(pre.reqs),
		warmupWidth: pre.warmupBatchWidth(cfg.warmup), padding: resolvedPadding(
			pads, len(pre.reqs)),
		arrangement: cfg.arrangement,
		delivery: resolveDeliveryPlan(
			tuning, len(pre.reqs), tuning.connectionLimit,
			flags.shared.ignorePeerStreamLimit.Get(plan.Globals)),
		bodyRelease:    release,
		responseLimits: responseLimits,
		runTimeout:     runTimeout,
		insecure:       plan.Globals.Insecure,
	}
	var (
		jsonRun      MeasureRunRecord
		jsonRequests []RequestRecord
	)
	if reportCfg.format == reportJSON {
		jsonRequests, err = captureRequestRecords(pre, refs, requestSetMeasured)
		if err != nil {
			return err
		}
		allReqs := pre.allRequests()
		dialAddress, err := tth2.BatchOrigin(allReqs)
		if err != nil {
			return err
		}
		jsonRun = MeasureRunRecord{
			TrialsRequested:    requestedTrials,
			TrialsPlanned:      count,
			Budget:             cfg.budget,
			Warmup:             cfg.warmup,
			BuildInfo:          layout.buildInfo,
			Argv:               append([]string{"measure"}, args...),
			StartedAt:          time.Now(),
			DialAddress:        dialAddress,
			Origin:             commonWireOrigin(allReqs),
			RequestCount:       len(allReqs),
			MeasuredBatchWidth: len(pre.reqs),
			WarmupBatchWidth:   pre.warmupBatchWidth(cfg.warmup),
			Delivery:           experimentPlan.delivery,
			ArrangementPolicy:  arrangementName(cfg.arrangement),
			ReleaseDelay:       release.releaseDelay,
			BodyBytesWithheld:  release.bodyBytesWithheld,
			ResponseLimits:     responseLimits,
			RunTimeout:         runTimeout,
			Insecure:           plan.Globals.Insecure,
			Padding:            resolvedPadding(pads, len(pre.reqs)),
		}
	}

	destination, err := openReportDestination(out, reportCfg.output)
	if err != nil {
		return err
	}
	defer func() { err = destination.finish(err) }()

	report := newReportWriter(destination.writer)
	view = measuredRefs.projectRankView(view)
	echo := report
	var reporter measurementReporter = newTextMeasurementReporter(
		report, view, measuredRefs, experimentPlan)
	if reportCfg.format == reportJSON {
		reporter = newJSONMeasureReporter(report, jsonRun, jsonRequests)
		echo = newReportWriter(out.stderr)
		pre.summariseRequestEcho(
			echo, refs, flags.shared.verbose.Get(plan.Globals))
	} else {
		writeBanner(report, toolName+" measure")
		pre.summarise(
			echo, refs, flags.shared.verbose.Get(plan.Globals))
	}
	if err := echo.Err(); err != nil {
		if reportCfg.format == reportText && destination.isClosedPipe(err) {
			return nil
		}
		return err
	}
	if reportCfg.format == reportJSON {
		if err := reportPreflightWarnings(
			echo, measurePreflightWarnings(experimentPlan),
		); err != nil {
			return err
		}
	}
	sendOptions := flags.shared.sendOptions(plan, release)
	sendOptions = append(sendOptions, responseLimits.trialOptions()...)
	if len(pads) > 0 {
		sendOptions = append(sendOptions, tth2.WithPadding(pads...))
	}
	if len(pre.warmupReqs) > 0 {
		sendOptions = append(
			sendOptions, tth2.WithWarmupRequests(pre.warmupReqs...))
	}
	return driveTrials(
		live.ctx, pre, cfg, count, tuning.batchRate, tuning.requestRateMax,
		reporter, out.stderr, destination,
		func() error {
			return live.finish(pre.client.Transport.CloseIdleConnections)
		},
		sendOptions...)
}

// driveTrials runs the resolved number of batch trials and presents
// their descriptive ordering and execution evidence. The caller validates that
// pre contains at least two requests before producing output.
func driveTrials(
	ctx context.Context,
	pre *prepared,
	cfg measureConfig,
	trials int,
	maxBatchesPerSecond float64,
	rps float64,
	reporter measurementReporter,
	stderr io.Writer,
	destination *reportDestination,
	liveFinish func() error,
	opts ...tth2.TrialsOption,
) error {
	opts = append(opts,
		tth2.WithArrangementPolicy(cfg.arrangement),
		tth2.WithWarmup(cfg.warmup),
		tth2.WithMaxTrials(trials),
	)
	progress := newProgressPresenter(stderr)
	eta := trialETA{
		limit: trials, requestsPerTrial: len(pre.reqs),
		maxBatchesPerSecond:  maxBatchesPerSecond,
		maxRequestsPerSecond: rps,
	}
	opts = append(opts, tth2.WithProgress(
		progressInterval,
		func(p tth2.TrialProgress) {
			progress.Observe(trialProgressLine(p, eta))
		},
	))
	driveCtx, cancelDrive := context.WithCancel(ctx)
	defer cancelDrive()
	stream := pre.client.StreamTrials(driveCtx, pre.reqs, opts...)
	measured := newMeasurement(
		cfg.trials, trials, len(pre.reqs), stream.Conns(), cfg.arrangement)
	measured.warmupPerConn = cfg.warmup
	measured.warmupWidth = pre.warmupBatchWidth(cfg.warmup)
	if err := reporter.Run(stream.Conns()); err != nil {
		progress.Done()
		if destination.isClosedPipe(err) {
			return nil
		}
		return err
	}
	var evidenceErr error
	for trial := range stream.All() {
		if evidenceErr != nil {
			continue
		}
		if err := measured.observe(trial); err != nil {
			evidenceErr = err
			cancelDrive()
		}
	}
	summary, streamErr := stream.Summary(), stream.Err()
	liveCause := liveFinish()
	progress.Done()
	terminalErr := evidenceErr
	completion := completedRun()
	if terminalErr != nil {
		completion = failedRun(terminalErr, completionInternalError)
	} else if streamErr != nil || liveCause != nil {
		terminalErr = streamErr
		completion = liveRunFailed(
			streamErr, liveCause, completionAcquisitionFailed)
		if completion.Code == completionRunTimeout {
			terminalErr = reportedCommandFailure(errRunTimeout)
		}
		if stopped, ok := stopInterruptedRun(ctx, completion); ok {
			completion = stopped
			terminalErr = reportedCommandFailure(streamErr)
		}
	}
	measured.finish(summary, terminalErr)
	if evidenceErr == nil {
		if err := measured.reconcile(); err != nil {
			terminalErr = err
			measured.runErr = err
			completion = failedRun(err, completionInternalError)
		}
	}
	measured.completion = completion
	if err := reporter.Summary(measured); err != nil {
		if destination.isClosedPipe(err) {
			return nil
		}
		return err
	}
	if terminalErr != nil {
		return terminalErr
	}
	if measured.retained == 0 {
		return errMeasurementUnavailable
	}
	return nil
}
