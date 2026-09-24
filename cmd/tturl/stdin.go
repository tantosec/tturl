package main

import (
	"context"
	"errors"
	"io"
	"sync"
)

// newStdinSnapshot owns one lazy invocation read, including its failure. A
// cancellation callback unblocks an explicitly cancellable input. The read
// remains in the calling goroutine and the callback is joined before return.
func newStdinSnapshot(ctx context.Context, out commandOutput) func() ([]byte, error) {
	var once sync.Once
	var data []byte
	var readErr error
	return func() ([]byte, error) {
		once.Do(func() {
			if err := ctx.Err(); err != nil {
				readErr = err
				return
			}
			if out.stdin == nil {
				readErr = errors.New("standard input is unavailable")
				return
			}
			var stop func() bool
			var joined chan struct{}
			if out.cancelInput != nil {
				joined = make(chan struct{})
				stop = context.AfterFunc(ctx, func() {
					defer close(joined)
					_ = out.cancelInput()
				})
			}
			data, readErr = io.ReadAll(out.stdin)
			if stop != nil && !stop() {
				<-joined
			}
			if err := ctx.Err(); err != nil {
				readErr = err
			}
			if readErr != nil {
				data = nil
			}
		})
		return append([]byte(nil), data...), readErr
	}
}
