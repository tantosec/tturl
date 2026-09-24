package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/tth2"
)

var errAnalyseUnavailable = errors.New("analysis unavailable; see report")

type analyseCommandFlags struct {
	cycles         *curlblocks.Flag[int]
	warmup         *curlblocks.Flag[int]
	padding        positionalPaddingFlags
	roles          requestRoleFlags
	captureHeaders *curlblocks.Flag[bool]
	captureBody    *curlblocks.Flag[string]
	responseLimits *responseLimitFlags
	pacing         *pacingFlags
	shared         *sharedFlags
}

func configureAnalyseCommand(layout commandLayout) analyseCommandFlags {
	flags := analyseCommandFlags{
		cycles: layout.command.Int(
			"cycles", "", "N", 0,
			"run N balanced cycles; minimum/default counts: tturl help analyse").HelpDefault(
			"depends on width"),
		warmup: layout.command.Int(
			"warmup", "", "N", defaultWarmup,
			"make N priming attempts per connection; excludes measured budget"),
		captureHeaders: layout.output.Bool(
			"capture-headers", "", false,
			"retain headers/trailers; requires --report json"),
		captureBody: layout.output.String(
			"capture-body", "", "SIZE|unlimited", "",
			"retain response prefix; requires --report json").CompletionValues(
			unlimitedFlagValue).HelpDefault("disabled"),
	}
	layout.output.Notes("Capture does not raise --response-body-max, normally 8MiB.")
	flags.padding = addPositionalPaddingFlags(layout.command)
	flags.roles = addRequestRoleFlags(layout.parser)
	flags.responseLimits = addResponseLimitFlags(layout.execution)
	flags.pacing = addPacingFlags(layout.parser, layout.execution)
	flags.shared = addSharedFlags(layout)
	return flags
}

func runAnalyse(ctx context.Context, args []string, out commandOutput) error {
	return runAnalyseCommand(
		ctx, commandByID(commandAnalyse), commandCatalogue, args, out)
}

func runAnalyseCommand(
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
	flags := configureAnalyseCommand(layout)
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
	captureCfg, err := parseRaceCapture(
		flags.captureHeaders.Get(plan.Globals),
		flags.captureBody.Get(plan.Globals),
		nil, reportCfg.format)
	if err != nil {
		return usage(err)
	}
	if flags.warmup.Get(plan.Globals) < 0 {
		return usage(fmt.Errorf("--warmup must be >= 0, got %d",
			flags.warmup.Get(plan.Globals)))
	}
	if err := flags.roles.validate(
		plan, flags.warmup.Get(plan.Globals)); err != nil {
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
	requestedCycles := flags.cycles.Get(plan.Globals)
	minimumCycles := analysePairResolutionFloor(assembled.measuredCount)
	if flags.cycles.IsSet(plan.Globals) && requestedCycles == 0 && minimumCycles > 0 {
		return usage(fmt.Errorf(
			"--cycles must be at least %d for %d requests; got 0",
			minimumCycles, assembled.measuredCount))
	}
	workload, err := resolveAnalyseWorkload(
		assembled.measuredCount, requestedCycles)
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
			tuning, len(materialised.reqs),
			materialised.activeBatchWidth(len(materialised.reqs), flags.warmup.Get(plan.Globals)),
			workload.Cycles, true,
			flags.shared.ignorePeerStreamLimit.Get(plan.Globals))
		if err != nil {
			return usage(err)
		}
		warmupWidth := materialised.warmupBatchWidth(
			flags.warmup.Get(plan.Globals))
		if connections > 0 {
			if err := validateAnalyseWarmup(
				warmupWidth, flags.warmup.Get(plan.Globals), connections); err != nil {
				return usage(err)
			}
		}
		refs := projectRunReferences(materialised.labels, 12)
		result := analyseRunResult{
			Width: len(materialised.reqs), ConnectionLimit: connections,
			Plan: analysePlanResult{
				Cycles: workload.Cycles, Trials: workload.Trials,
				Operations: workload.Operations, DefaultCycles: workload.DefaultCycles,
				LowerSensitivity: workload.LowerSensitivity,
			},
		}
		context := analyseReportContext{
			Delivery:                  delivery,
			WarmupTrialsPerConnection: flags.warmup.Get(plan.Globals),
			WarmupBatchWidth:          warmupWidth,
			Padding:                   resolvedPadding(pads, len(materialised.reqs)),
			BodyRelease:               release,
			ResponseLimits:            responseLimits,
			RunTimeout:                runTimeout,
			Insecure:                  plan.Globals.Insecure,
		}
		return reportDryRun(
			out, reportCfg, "analyse", materialised, refs,
			flags.shared.verbose.Get(plan.Globals),
			func(w *reportWriter) error {
				return reportAnalyseExperiment(w, result, context)
			})
	}
	pre, err := prepareMaterialised(materialised, plan, tuning)
	if err != nil {
		return err
	}
	live := startLiveRunPhase(ctx, runTimeout)
	defer live.cancel()
	tuning, err = fitPipeline(
		live.ctx, pre, tuning, len(pre.reqs),
		pre.activeBatchWidth(len(pre.reqs), flags.warmup.Get(plan.Globals)),
		workload.Cycles, true)
	if err != nil {
		cause := live.finish(pre.client.Transport.CloseIdleConnections)
		return liveRunError(err, cause)
	}
	effectiveConns := tuning.connectionLimit
	if err := validateAnalyseWarmup(
		pre.warmupBatchWidth(flags.warmup.Get(plan.Globals)),
		flags.warmup.Get(plan.Globals), effectiveConns,
	); err != nil {
		return usage(err)
	}
	seed, err := newAnalyseSeed(rand.Reader)
	if err != nil {
		return err
	}
	defer pre.client.Transport.CloseIdleConnections()
	refs := projectRunReferences(pre.labels, 12)
	measuredLabels := pre.labels[:len(pre.reqs)]
	measuredRefs := runReferences{
		refs: refs.refs[:len(pre.reqs)], compact: refs.compact,
	}
	delivery := resolveDeliveryPlan(
		tuning, len(pre.reqs), effectiveConns,
		flags.shared.ignorePeerStreamLimit.Get(plan.Globals))
	reportContext := analyseReportContext{
		Delivery:                  delivery,
		WarmupTrialsPerConnection: flags.warmup.Get(plan.Globals),
		WarmupBatchWidth: pre.warmupBatchWidth(
			flags.warmup.Get(plan.Globals)),
		Padding:        resolvedPadding(pads, len(pre.reqs)),
		BodyRelease:    release,
		ResponseLimits: responseLimits,
		RunTimeout:     runTimeout,
		Insecure:       plan.Globals.Insecure,
	}
	jsonContext := analyseJSONContext{}
	if reportCfg.format == reportJSON {
		requestRecords, captureErr := captureRequestRecords(
			pre, refs, requestSetMeasured)
		if captureErr != nil {
			return captureErr
		}
		allReqs := pre.allRequests()
		dialAddress, originErr := tth2.BatchOrigin(allReqs)
		if originErr != nil {
			return originErr
		}
		jsonContext = analyseJSONContext{
			BuildInfo:                 layout.buildInfo,
			Argv:                      append([]string{"analyse"}, args...),
			StartedAt:                 time.Now(),
			DialAddress:               dialAddress,
			Origin:                    commonWireOrigin(allReqs),
			Requests:                  requestRecords,
			Delivery:                  delivery,
			ReleaseDelay:              release.releaseDelay,
			BodyBytesWithheld:         release.bodyBytesWithheld,
			Insecure:                  plan.Globals.Insecure,
			MaxResponseHeaderBytes:    pre.client.Transport.MaxResponseHeaderBytes,
			WarmupTrialsPerConnection: flags.warmup.Get(plan.Globals),
			MeasuredBatchWidth:        len(pre.reqs),
			WarmupBatchWidth: pre.warmupBatchWidth(
				flags.warmup.Get(plan.Globals)),
			Padding:           resolvedPadding(pads, len(pre.reqs)),
			ResponseLimits:    responseLimits,
			RunTimeout:        runTimeout,
			RandomisationSeed: seed,
		}
	}
	if err := reportPreflightWarnings(
		newReportWriter(out.stderr), analysePreflightWarnings(
			workload, effectiveConns, delivery, reportCfg.format == reportJSON,
		),
	); err != nil {
		return err
	}

	destination, err := openReportDestination(out, reportCfg.output)
	if err != nil {
		return err
	}
	defer func() { err = destination.finish(err) }()

	report := newReportWriter(destination.writer)
	echo := report
	diagnostics := newReportWriter(out.stderr)
	if reportCfg.format == reportJSON {
		echo = diagnostics
		pre.summariseRequestEcho(
			echo, refs, flags.shared.verbose.Get(plan.Globals))
	} else {
		writeBanner(report, toolName+" analyse")
		pre.summarise(echo, refs, flags.shared.verbose.Get(plan.Globals))
	}
	if err := echo.Err(); err != nil {
		if reportCfg.format == reportText && destination.isClosedPipe(err) {
			return nil
		}
		return err
	}
	var jsonStream *analyseJSONStream
	driveCtx, cancelDrive := context.WithCancelCause(live.ctx)
	defer cancelDrive(nil)
	var observationSink func(analyseTrialObservation) error
	if reportCfg.format == reportJSON {
		jsonStream, err = newAnalyseJSONStream(
			report, workload, effectiveConns, measuredLabels,
			jsonContext, captureCfg)
		if err != nil {
			if destination.isClosedPipe(err) {
				return nil
			}
			return err
		}
		observationSink = func(observation analyseTrialObservation) error {
			err := jsonStream.Trial(observation)
			if err != nil {
				cancelDrive(err)
			}
			return err
		}
	}
	progress := newProgressPresenter(out.stderr)
	eta := trialETA{
		limit: workload.Trials, requestsPerTrial: len(pre.reqs),
		maxBatchesPerSecond:  tuning.batchRate,
		maxRequestsPerSecond: tuning.requestRateMax,
	}
	sendOptions := []tth2.TrialsOption{
		tth2.WithWarmup(flags.warmup.Get(plan.Globals)),
		tth2.WithProgress(progressInterval, func(p tth2.TrialProgress) {
			progress.Observe(trialProgressLine(p, eta))
		}),
	}
	if len(pads) > 0 {
		sendOptions = append(sendOptions, tth2.WithPadding(pads...))
	}
	if len(pre.warmupReqs) > 0 {
		sendOptions = append(
			sendOptions, tth2.WithWarmupRequests(pre.warmupReqs...))
	}
	sendOptions = append(
		sendOptions, flags.shared.sendOptions(plan, release)...)
	sendOptions = append(sendOptions, responseLimits.trialOptions()...)
	analyseCfg := analyseConfig{
		RandomisationSeed: seed, Capture: captureCfg,
		WarmupWidth: pre.warmupBatchWidth(
			flags.warmup.Get(plan.Globals)),
		observationSink:    observationSink,
		retainObservations: false,
	}
	evidence, conns := collectAnalyseEvidenceConfigured(
		driveCtx, pre.client, pre.reqs, workload, effectiveConns,
		analyseCfg.WarmupWidth, analyseCfg.Capture,
		analyseCfg.observationSink, analyseCfg.retainObservations,
		sendOptions...)
	liveCause := live.finish(pre.client.Transport.CloseIdleConnections)
	progress.Done()
	analyseCfg.ConnectionLimit = conns
	result := finishAnalyseLive(evidence, conns, analyseCfg)
	result.Completion = finaliseLiveCompletion(
		result.Completion, liveCause, completionAcquisitionFailed)
	completion, interrupted := stopInterruptedRun(ctx, result.Completion)
	result.Completion = completion

	if reportCfg.format == reportJSON {
		err = jsonStream.Finish(result, jsonContext)
	} else {
		emitln(report)
		err = reportAnalyseDocumentWithContext(
			report, result, measuredRefs.inlineLabels(), false, reportContext)
	}
	if err != nil {
		if destination.isClosedPipe(err) {
			return nil
		}
		return err
	}
	if result.Global.Outcome == analyseUnavailable {
		closeErr := destination.finish(nil)
		destination.closer = nil
		if closeErr != nil {
			return closeErr
		}
		if interrupted {
			return reportedCommandFailure(errors.Join(
				errAnalyseUnavailable, context.Canceled))
		}
		if result.Completion.Code == completionRunTimeout {
			return reportedCommandFailure(errors.Join(
				errAnalyseUnavailable, errRunTimeout))
		}
		return errAnalyseUnavailable
	}
	if result.Completion.Code == completionRunTimeout {
		return reportedCommandFailure(errRunTimeout)
	}
	return report.Err()
}

func newAnalyseSeed(reader io.Reader) (uint64, error) {
	var encoded [8]byte
	if _, err := io.ReadFull(reader, encoded[:]); err != nil {
		return 0, fmt.Errorf("generate analysis randomisation seed: %w", err)
	}
	return binary.LittleEndian.Uint64(encoded[:]), nil
}
