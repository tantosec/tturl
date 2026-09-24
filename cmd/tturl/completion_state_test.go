package main

import (
	"context"
	"errors"
	"testing"
)

func TestRunCompletionClassification(t *testing.T) {
	ordinary := errors.New("comparison failed")
	for _, test := range []struct {
		name  string
		value runCompletion
		state completionState
		code  completionCode
	}{
		{"complete", completedRun(), completionComplete, ""},
		{"stopped", stoppedRun(), completionStopped, completionInterrupted},
		{
			"ordinary", failedRun(ordinary, completionComparisonFailed),
			completionFailed, completionComparisonFailed,
		},
		{
			"cancelled", failedRun(context.Canceled, completionAcquisitionFailed),
			completionFailed, completionInterrupted,
		},
		{
			"deadline", failedRun(context.DeadlineExceeded,
				completionAcquisitionFailed), completionFailed,
			completionDeadlineExceeded,
		},
		{
			"run timeout", runCompletion{
				State: completionFailed, Code: completionRunTimeout,
				Detail: errRunTimeout.Error(),
			}, completionFailed, completionRunTimeout,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.value.State != test.state || test.value.Code != test.code {
				t.Errorf("completion = %+v, want %s/%s",
					test.value, test.state, test.code)
			}
		})
	}
}

func TestStopInterruptedRunRequiresParentCancellation(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	failedCancellation := failedRun(
		context.Canceled, completionAcquisitionFailed)
	got, stopped := stopInterruptedRun(cancelled, failedCancellation)
	if !stopped || got != stoppedRun() {
		t.Fatalf("parent cancellation = %+v, %t; want stopped", got, stopped)
	}

	internal, stopped := stopInterruptedRun(t.Context(), failedCancellation)
	if stopped || internal != failedCancellation {
		t.Fatalf("internal cancellation = %+v, %t; want original failure",
			internal, stopped)
	}

	complete, stopped := stopInterruptedRun(cancelled, completedRun())
	if stopped || complete != completedRun() {
		t.Fatalf("late cancellation = %+v, %t; want complete", complete, stopped)
	}
	wantFailure := failedRun(errors.New("write report"), completionInternalError)
	failure, stopped := stopInterruptedRun(cancelled, wantFailure)
	if stopped || failure != wantFailure {
		t.Fatalf("failure during cancellation = %+v, %t; want primary failure",
			failure, stopped)
	}
}
