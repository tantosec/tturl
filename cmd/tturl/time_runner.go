package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/h2tls"
	"github.com/tantosec/tturl/internal/timing"
)

type timeEngineRunner func(context.Context, *timing.Resolved, func(timing.Event) error) (timing.Result, error)

func runTimeCommand(ctx context.Context, spec *commandSpec, _ []commandSpec, args []string, out commandOutput) error {
	return runTimeCommandForPlatform(ctx, spec, args, out, runtime.GOOS, timing.Run)
}

func runTimeCommandForPlatform(ctx context.Context, spec *commandSpec, args []string, out commandOutput,
	platform string, run timeEngineRunner,
) (err error) {
	if err := timeStartupAllowed(platform, args); err != nil {
		return err
	}
	layout := newCommand(spec)
	flags := configureTimeCommand(layout)
	if platform == "windows" {
		_, err := fmt.Fprint(out.stdout, renderCommandUsage(spec, layout.parser))
		return err
	}
	out, err = commandDiagnosticOutput(spec, args, out)
	if err != nil {
		return err
	}
	curlblocks.WithStdinReader(newStdinSnapshot(ctx, out))(layout.parser)
	parsed, done, err := parseCommand(layout.parser, spec, args, out.stdout, out.stderr)
	if err != nil || done {
		return err
	}
	cfg, err := flags.resolve(parsed)
	if err != nil {
		return usage(err)
	}
	requests, err := materialiseTimeRequests(ctx, parsed, flags.protocols, flags.roles)
	if err != nil {
		return usage(err)
	}
	plan := cfg.enginePlan(requests, parsed.Globals.Insecure)
	resolved, err := timing.Resolve(plan)
	if err != nil {
		return usage(err)
	}
	destination, err := openReportDestination(out, cfg.report.output)
	if err != nil {
		return err
	}
	defer func() { err = destination.finish(err) }()
	report := newReportWriter(destination.writer)
	if cfg.dryRun {
		return reportTimePreview(report, cfg, requests, resolved)
	}
	// Key logging is a live-only side effect after complete static validation.
	keyLog, err := h2tls.KeyLogWriter()
	if err != nil {
		return err
	}
	if closer, ok := keyLog.(io.Closer); ok {
		defer func() { err = errors.Join(err, closer.Close()) }()
	}
	if keyLog != nil {
		plan.TLSConfig.KeyLogWriter = keyLog
		resolved, err = timing.Resolve(plan)
		if err != nil {
			return usage(err)
		}
	}
	reporter := newTimeReporter(report, cfg, requests, resolved, layout.buildInfo, append([]string{spec.name}, args...))
	if err := reporter.Run(); err != nil {
		if timeBrokenPipeStop(destination, cfg, err, timing.Result{}) {
			return nil
		}
		return err
	}
	if cfg.report.format == reportJSON {
		echo := newReportWriter(out.stderr)
		echoTimeRequests(echo, requests, cfg.verbose)
		if err := echo.Err(); err != nil {
			return err
		}
	}
	result, runErr := run(ctx, resolved, reporter.Observe)
	summaryErr := reporter.Summary(result)
	if timeBrokenPipeStop(destination, cfg, errors.Join(runErr, summaryErr), result) {
		return nil
	}
	if summaryErr != nil {
		return errors.Join(runErr, summaryErr)
	}
	if runErr != nil {
		return runErr
	}
	if result.Completion == timing.CompletionFailed {
		if result.Failure != nil {
			return result.Failure
		}
		return fmt.Errorf("time run did not complete")
	}
	return nil
}

func timeBrokenPipeStop(destination *reportDestination, cfg timeCommandConfig, err error, result timing.Result) bool {
	if cfg.trials != 0 || !destination.isClosedPipe(err) {
		return false
	}
	return result.Failure == nil || result.Failure.Phase == timing.PhaseOutput
}

func echoTimeRequests(report *reportWriter, requests timeRequests, verbose bool) runReferences {
	items := append(append([]timeRequest(nil), requests.Measured...), requests.Priming...)
	groups := make([]requestGroup, len(items))
	labels := make([]string, len(items))
	for index, item := range items {
		groups[index] = requestGroup{
			req: timePreviewRequest(item), count: 1,
			warmup: item.Warmup, defaults: item.Defaults,
		}
		labels[index] = item.Label
	}
	refs := projectRunReferences(labels, raceReferenceWidth())
	summariseRequestsWithFormatter(report, groups, refs, verbose, func(index int, _ *http.Request) string {
		return formatTimeRequest(items[index], true)
	})
	return refs
}
