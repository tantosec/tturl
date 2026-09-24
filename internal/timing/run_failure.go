package timing

import (
	"context"
	"errors"
)

func campaignFailure(phase Phase, err error) *Failure {
	if err == nil {
		return nil
	}
	if failure, ok := errors.AsType[*Failure](err); ok {
		return failure
	}
	code := "operational"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "timeout"
	} else if errors.Is(err, context.Canceled) {
		code = "cancelled"
	}
	return &Failure{Phase: phase, Code: code, Message: err.Error(), Cause: err}
}

// cspell:ignore errorlint
func campaignUserStop(err error) bool {
	//nolint:errorlint // Wrapped cancellation may be an independent failure.
	return err == context.Canceled
}
