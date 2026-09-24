package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tantosec/tturl/internal/curlblocks"
)

var errRunTimeout = errors.New("run_timeout: live request execution timed out")

type runTimeout time.Duration

func (sf *sharedFlags) resolveRunTimeout(
	globals curlblocks.Globals,
) (runTimeout, error) {
	duration, unlimited, err := parseDurationOrUnlimited(
		sf.runTimeout.Get(globals))
	if err != nil || (!unlimited && duration <= 0) {
		return 0, fmt.Errorf(
			"--run-timeout must be a positive Go duration or unlimited")
	}
	if unlimited {
		return 0, nil
	}
	return runTimeout(duration), nil
}

func reportRunTimeout(w io.Writer, timeout runTimeout) {
	value := unlimitedFlagValue
	if timeout > 0 {
		value = formatDurationASCII(time.Duration(timeout))
	}
	writeWrappedASCII(w, "Run timeout: ", value+" for live request execution.")
}

// liveRunPhase owns the optional deadline spanning fitting through disposal.
// finish snapshots its cause at the live boundary before cancelling its timer.
type liveRunPhase struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func startLiveRunPhase(parent context.Context, timeout runTimeout) liveRunPhase {
	if timeout <= 0 {
		ctx, cancel := context.WithCancel(parent)
		return liveRunPhase{ctx: ctx, cancel: cancel}
	}
	ctx, cancel := context.WithTimeoutCause(
		parent, time.Duration(timeout), errRunTimeout)
	return liveRunPhase{ctx: ctx, cancel: cancel}
}

func (phase liveRunPhase) finish(dispose func()) error {
	if dispose != nil {
		dispose()
	}
	cause := context.Cause(phase.ctx)
	phase.cancel()
	return cause
}

func liveRunFailed(
	err, cause error, fallback completionCode,
) runCompletion {
	// A concrete transport, evidence, or output error was committed before
	// cancellation. Preserve it even if the deadline fires while work drains.
	if err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		return failedRun(err, fallback)
	}
	if cause != nil && !errors.Is(cause, errRunTimeout) &&
		!errors.Is(cause, context.Canceled) &&
		!errors.Is(cause, context.DeadlineExceeded) {
		return failedRun(cause, fallback)
	}
	if errors.Is(cause, errRunTimeout) {
		return runCompletion{
			State: completionFailed, Code: completionRunTimeout,
			Detail: errRunTimeout.Error(),
			cause:  errRunTimeout,
		}
	}
	if err == nil {
		err = cause
	}
	if err == nil {
		return completedRun()
	}
	return failedRun(err, fallback)
}

func liveRunError(err, cause error) error {
	if err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if cause != nil && !errors.Is(cause, errRunTimeout) &&
		!errors.Is(cause, context.Canceled) &&
		!errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	if errors.Is(cause, errRunTimeout) {
		return errRunTimeout
	}
	if err != nil {
		return err
	}
	return cause
}

func finaliseLiveCompletion(
	completion runCompletion, cause error, fallback completionCode,
) runCompletion {
	completion = normaliseCompletion(completion)
	if completion.State == completionFailed &&
		completion.Code != completionInterrupted &&
		completion.Code != completionDeadlineExceeded {
		return completion
	}
	if cause == nil {
		return completion
	}
	return liveRunFailed(nil, cause, fallback)
}
