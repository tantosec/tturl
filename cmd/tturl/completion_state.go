package main

import (
	"context"
	"errors"
)

type completionState string

const (
	completionComplete completionState = "complete"
	completionStopped  completionState = "stopped"
	completionFailed   completionState = "failed"
)

type completionCode string

const (
	completionAcquisitionFailed completionCode = "acquisition_failed"
	completionComparisonFailed  completionCode = "comparison_failed"
	completionDeadlineExceeded  completionCode = "deadline_exceeded"
	completionInterrupted       completionCode = "interrupted"
	completionInternalError     completionCode = "internal_error"
	completionRunTimeout        completionCode = "run_timeout"
)

// runCompletion describes how acquisition ended independently of whether the
// command-specific result is available.
type runCompletion struct {
	State  completionState
	Code   completionCode
	Detail string
	cause  error
}

func completedRun() runCompletion {
	return runCompletion{State: completionComplete}
}

func stoppedRun() runCompletion {
	return runCompletion{State: completionStopped, Code: completionInterrupted, cause: context.Canceled}
}

// stopInterruptedRun maps only cancellation of the command's parent context
// to a clean stop. A derived context may also be cancelled to propagate a
// report or evidence failure; that cancellation must not hide its cause.
func stopInterruptedRun(
	ctx context.Context, completion runCompletion,
) (runCompletion, bool) {
	completion = normaliseCompletion(completion)
	if !errors.Is(ctx.Err(), context.Canceled) ||
		completion.State != completionFailed ||
		completion.Code != completionInterrupted {
		return completion, false
	}
	return stoppedRun(), true
}

func failedRun(err error, fallback completionCode) runCompletion {
	code := fallback
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		code = completionDeadlineExceeded
	case errors.Is(err, context.Canceled):
		code = completionInterrupted
	}
	return runCompletion{State: completionFailed, Code: code, Detail: err.Error(), cause: err}
}

func normaliseCompletion(completion runCompletion) runCompletion {
	if completion.State == "" {
		return completedRun()
	}
	return completion
}
