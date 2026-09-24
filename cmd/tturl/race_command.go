package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/termline"
	"github.com/tantosec/tturl/tth2"
)

type raceCommandFlags struct {
	trials         *curlblocks.Flag[string]
	warmup         *curlblocks.Flag[int]
	arrange        *curlblocks.Flag[string]
	padding        positionalPaddingFlags
	roles          requestRoleFlags
	captureHeaders *curlblocks.Flag[bool]
	captureBody    *curlblocks.Flag[string]
	extractRegex   *curlblocks.Flag[[]string]
	responseLimits *responseLimitFlags
	pacing         *pacingFlags
	shared         *sharedFlags
}

func configureRaceCommand(layout commandLayout) raceCommandFlags {
	command := layout.command
	flags := raceCommandFlags{
		trials: command.String(
			"trials", "", "N|unlimited", strconv.Itoa(defaultRaceTrials),
			"request N trials; unlimited continues; rotate rounds up to whole cycles").CompletionValues(
			unlimitedFlagValue),
		warmup: command.Int("warmup", "", "N", defaultWarmup,
			warmupHelp),
		arrange: command.String("arrange", "", "POLICY", "", arrangeHelp).
			HelpDefault("none at 1 trial; random otherwise").
			CompletionValues(arrangementNames...),
		captureHeaders: layout.output.Bool("capture-headers", "", false,
			"retain headers/trailers; requires --report json"),
		captureBody: layout.output.String(
			"capture-body", "", "SIZE|unlimited", "",
			"retain response prefix; requires --report json or --extract-regex").CompletionValues(
			unlimitedFlagValue).HelpDefault("disabled"),
		extractRegex: layout.output.StringArray(
			"extract-regex", "", "REGEX", nil,
			"search retained prefix; matches or named groups; requires nonzero --capture-body; repeatable"),
	}
	layout.output.Notes("Capture does not raise --response-body-max, normally 8MiB.")
	flags.padding = addPositionalPaddingFlags(command)
	flags.roles = addRequestRoleFlags(layout.parser)
	flags.responseLimits = addResponseLimitFlags(layout.execution)
	flags.pacing = addPacingFlags(layout.parser, layout.execution)
	flags.shared = addSharedFlags(layout)
	return flags
}

// runRace streams a bounded number of batch races and reports every
// observation without inferring a finding.
func runRace(ctx context.Context, args []string, out commandOutput) (err error) {
	return runRaceCommand(
		ctx, commandByID(commandRace), commandCatalogue, args, out)
}

func runRaceCommand(
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
	flags := configureRaceCommand(layout)
	p := layout.parser
	plan, done, err := parseCommand(p, spec, args, out.stdout, out.stderr)
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
	captureCfg, err := parseRaceCapture(
		flags.captureHeaders.Get(plan.Globals), flags.captureBody.Get(plan.Globals),
		flags.extractRegex.Get(plan.Globals),
		reportCfg.format,
	)
	if err != nil {
		return usage(err)
	}
	trials, err := parsePositiveIntOrUnlimited(flags.trials.Get(plan.Globals))
	if err != nil {
		return usage(fmt.Errorf("--trials %w", err))
	}
	cfg, err := parseRace(
		trials, flags.warmup.Get(plan.Globals),
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
	if assembled.measuredCount == 0 {
		return usage(fmt.Errorf("at least one trial request is required"))
	}

	resolvedTrials := cfg.trials
	var note string
	if cfg.trials > 0 {
		resolvedTrials, note, err = resolveTrials(
			cfg.trials, assembled.measuredCount, cfg.arrangement)
		if err != nil {
			return usage(err)
		}
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
	if dryRun {
		delivery, connections, err := previewDeliveryPlan(
			tuning, len(materialised.reqs), materialised.activeBatchWidth(len(materialised.reqs), cfg.warmup),
			trialConnectionWorkLimit(
				resolvedTrials, len(materialised.reqs), cfg.arrangement),
			resolvedTrials != 1,
			flags.shared.ignorePeerStreamLimit.Get(plan.Globals))
		if err != nil {
			return usage(err)
		}
		refs := projectRunReferences(materialised.labels, raceReferenceWidth())
		record := RunRecord{
			Trials: resolvedTrials, TrialsRequested: cfg.trials,
			Warmup: cfg.warmup, TrialBatchWidth: len(materialised.reqs),
			WarmupBatchWidth: materialised.warmupBatchWidth(cfg.warmup),
			Connections:      connections, Delivery: delivery,
			ArrangementPolicy: arrangementName(cfg.arrangement),
			bodyRelease:       release, ResponseLimits: responseLimits,
			RunTimeout: runTimeout, Insecure: plan.Globals.Insecure,
			Padding: resolvedPadding(pads, assembled.measuredCount),
		}
		return reportDryRun(
			out, reportCfg, "race", materialised, refs,
			flags.shared.verbose.Get(plan.Globals),
			func(w *reportWriter) error { return reportRaceExperiment(w, record) })
	}
	pre, err := prepareMaterialised(materialised, plan, tuning)
	if err != nil {
		return err
	}
	live := startLiveRunPhase(ctx, runTimeout)
	defer live.cancel()
	tuning, err = fitPipeline(
		live.ctx, pre, tuning, len(pre.reqs), pre.activeBatchWidth(len(pre.reqs), cfg.warmup),
		trialConnectionWorkLimit(
			resolvedTrials, len(pre.reqs), cfg.arrangement),
		resolvedTrials != 1)
	if err != nil {
		cause := live.finish(pre.client.Transport.CloseIdleConnections)
		return liveRunError(err, cause)
	}
	defer pre.client.Transport.CloseIdleConnections()
	if note != "" {
		if err := writeDiagnostic(out.stderr, diagnosticNotice, note); err != nil {
			return err
		}
	}
	opts := []tth2.TrialsOption{
		tth2.WithWarmup(cfg.warmup),
		tth2.WithArrangementPolicy(cfg.arrangement),
	}
	opts = append(opts, flags.shared.sendOptions(plan, release)...)
	opts = append(opts, responseLimits.trialOptions()...)
	opts = append(opts, captureCfg.trialOptions()...)
	reportSharesTerminal := reportCfg.output == "-" &&
		termline.IsTerminal(out.stdout) &&
		termline.IsTerminal(out.stderr)
	var progress *progressPresenter
	if raceProgressWanted(
		resolvedTrials, cfg.warmup, reportSharesTerminal,
	) {
		progress = newProgressPresenter(out.stderr)
		eta := trialETA{
			limit: resolvedTrials, requestsPerTrial: len(pre.reqs),
			maxBatchesPerSecond:  tuning.batchRate,
			maxRequestsPerSecond: tuning.requestRateMax,
		}
		opts = append(opts, tth2.WithProgress(
			progressInterval,
			func(p tth2.TrialProgress) {
				progress.Observe(trialProgressLine(p, eta))
			},
		))
	}
	if len(pads) > 0 {
		opts = append(opts, tth2.WithPadding(pads...))
	}
	if len(pre.warmupReqs) > 0 {
		opts = append(opts, tth2.WithWarmupRequests(pre.warmupReqs...))
	}
	if resolvedTrials >= 1 {
		opts = append(opts, tth2.WithMaxTrials(resolvedTrials))
	}
	driveCtx, cancelDrive := context.WithCancel(live.ctx)
	defer cancelDrive()
	stream := pre.client.StreamTrials(driveCtx, pre.reqs, opts...)

	refs := projectRunReferences(pre.labels, raceReferenceWidth())
	requestRecords, err := captureRequestRecords(pre, refs, requestSetTrial)
	if err != nil {
		return err
	}
	allReqs := pre.allRequests()
	dialAddress, err := tth2.BatchOrigin(allReqs)
	if err != nil {
		return err
	}
	runRecord := RunRecord{
		Trials:           resolvedTrials,
		TrialsRequested:  cfg.trials,
		Warmup:           cfg.warmup,
		TrialBatchWidth:  len(pre.reqs),
		WarmupBatchWidth: raceWarmupBatchWidth(pre, cfg.warmup),
		Connections:      stream.Conns(),
		Delivery: resolveDeliveryPlan(
			tuning, len(pre.reqs), stream.Conns(),
			flags.shared.ignorePeerStreamLimit.Get(plan.Globals)),
		BuildInfo:              layout.buildInfo,
		Argv:                   append([]string{"race"}, args...),
		StartedAt:              time.Now(),
		DialAddress:            dialAddress,
		Origin:                 commonWireOrigin(allReqs),
		RequestCount:           len(allReqs),
		ArrangementPolicy:      arrangementName(cfg.arrangement),
		ReleaseDelay:           release.releaseDelay,
		BodyBytesWithheld:      release.bodyBytesWithheld,
		bodyRelease:            release,
		ResponseLimits:         responseLimits,
		RunTimeout:             runTimeout,
		Insecure:               plan.Globals.Insecure,
		CaptureHeaders:         captureCfg.headers,
		CaptureBodyBytes:       captureCfg.bodyBytes,
		MaxResponseHeaderBytes: pre.client.Transport.MaxResponseHeaderBytes,
		ExtractRegex: append(
			[]string(nil), flags.extractRegex.Get(plan.Globals)...),
		ExtractFields: captureCfg.extractFields(),
		Padding:       resolvedPadding(pads, assembled.measuredCount),
	}

	destination, err := openReportDestination(out, reportCfg.output)
	if err != nil {
		return err
	}
	defer func() { err = destination.finish(err) }()

	report := newReportWriter(destination.writer)
	echo := report
	if reportCfg.format == reportJSON {
		echo = newReportWriter(out.stderr)
		pre.summariseRequestEcho(echo, refs, flags.shared.verbose.Get(plan.Globals))
	} else {
		writeBanner(report, toolName+" race")
		pre.summarise(echo, refs, flags.shared.verbose.Get(plan.Globals))
	}
	if err := echo.Err(); err != nil {
		if reportCfg.format == reportText && destination.isClosedPipe(err) {
			return nil
		}
		return err
	}
	if reportCfg.format == reportJSON {
		if err := reportPreflightWarnings(
			echo, jsonPreflightWarnings(runRecord.Delivery),
		); err != nil {
			return err
		}
	}

	var reporter RaceReporter = newTextRaceReporter(report)
	if reportCfg.format == reportJSON {
		reporter = newJSONRaceReporter(report)
	}
	if err := reporter.Run(runRecord); err != nil {
		if destination.isClosedPipe(err) {
			return nil
		}
		return err
	}
	for _, request := range requestRecords {
		if err := reporter.Request(request); err != nil {
			if destination.isClosedPipe(err) {
				return nil
			}
			return err
		}
	}

	warmupComplete, warmupIncomplete := 0, 0
	complete, incomplete := 0, 0
	connectionsObserved := make(map[int]bool)
	reportedResets := make([]int, len(requestRecords))
	var reportErr error
	reportWriteErr := false
	for trial := range stream.All() {
		if reportErr != nil {
			continue
		}
		progressSnapshot := stream.Progress()
		offset := 0
		if trial.Warmup && len(pre.warmupReqs) > 0 {
			offset = pre.measuredCount
		}
		record, err := captureTrialRecord(trial, offset, captureCfg)
		if err != nil {
			reportErr = err
			cancelDrive()
			continue
		}
		record.Progress = &progressSnapshot
		if err := reporter.Trial(record); err != nil {
			reportErr = err
			reportWriteErr = true
			cancelDrive()
			continue
		}
		for _, outcome := range record.Outcomes {
			if outcome.Reset != nil && outcome.Request >= 0 &&
				outcome.Request < len(reportedResets) {
				reportedResets[outcome.Request]++
			}
		}
		if trial.Warmup {
			if trial.Err == nil {
				warmupComplete++
			} else {
				warmupIncomplete++
			}
		} else if trial.Err == nil {
			complete++
		} else {
			incomplete++
		}
		if trial.Conn >= 0 {
			connectionsObserved[trial.Conn] = true
		}
	}
	streamErr := stream.Err()
	liveCause := live.finish(pre.client.Transport.CloseIdleConnections)
	if progress != nil {
		if reportWriteErr && destination.isClosedPipe(reportErr) {
			progress.Abort()
		} else {
			progress.Done()
		}
	}
	if reportErr != nil && reportWriteErr {
		if reportWriteErr && destination.isClosedPipe(reportErr) {
			return nil
		}
		return reportErr
	}
	completion := completedRun()
	terminalErr := streamErr
	if reportErr != nil {
		completion = failedRun(reportErr, completionInternalError)
		terminalErr = reportErr
	} else if streamErr != nil || liveCause != nil {
		completion = liveRunFailed(
			streamErr, liveCause, completionAcquisitionFailed)
		if completion.Code == completionRunTimeout {
			terminalErr = reportedCommandFailure(errRunTimeout)
		}
		if stopped, ok := stopInterruptedRun(ctx, completion); ok {
			completion = stopped
			if resolvedTrials < 0 {
				terminalErr = nil
			} else {
				terminalErr = reportedCommandFailure(streamErr)
			}
		}
	}
	execution, executionErr := projectRaceExecution(
		resolvedTrials, stream.Conns(), len(pre.reqs), runRecord.WarmupBatchWidth,
		connectionsObserved,
		executionBatches{
			Attempted:    warmupComplete + warmupIncomplete,
			RankComplete: warmupComplete, Incomplete: warmupIncomplete,
		},
		executionBatches{
			Attempted:    complete + incomplete,
			RankComplete: complete, Incomplete: incomplete,
		})
	if executionErr != nil {
		return executionErr
	}
	if err := reporter.Summary(SummaryRecord{
		Execution: execution, ResetCount: reportedResets, Completion: completion,
		MeasuredWidth: len(pre.reqs), PrimingWidth: runRecord.WarmupBatchWidth,
	}); err != nil {
		if destination.isClosedPipe(err) {
			return nil
		}
		return err
	}
	if terminalErr != nil {
		return terminalErr
	}
	return report.Err()
}

func raceWarmupBatchWidth(pre *prepared, warmup int) int {
	return pre.warmupBatchWidth(warmup)
}
