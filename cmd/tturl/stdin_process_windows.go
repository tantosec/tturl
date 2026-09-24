package main

import (
	"context"
	"runtime"
	"time"

	"golang.org/x/sys/windows"
)

var cancelSynchronousInput = windows.NewLazySystemDLL("kernel32.dll").NewProc("CancelSynchronousIo")

func (r *processInput) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if err := cancelSynchronousInput.Find(); err != nil {
		return 0, err
	}
	// Cancellation targets the issuing thread, not the file handle. Keep this
	// thread exclusively owned until both the read and its callback finish.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	thread, err := windows.OpenThread(windows.THREAD_TERMINATE, false, windows.GetCurrentThreadId())
	if err != nil {
		return 0, err
	}
	defer func() { _ = windows.CloseHandle(thread) }()
	defer runtime.KeepAlive(r.file)
	done, joined := make(chan struct{}), make(chan struct{})
	stop := context.AfterFunc(r.ctx, func() {
		defer close(joined)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			default:
			}
			// Cancellation may arrive between the context check and read syscall.
			// Retry until completion so an early ERROR_NOT_FOUND cannot lose it.
			_, _, _ = cancelSynchronousInput.Call(uintptr(thread))
			select {
			case <-done:
				return
			case <-ticker.C:
			}
		}
	})
	var n int
	err = r.ctx.Err()
	if err == nil {
		// Preserve os.File's console encoding and end-of-file handling. Go's
		// process stdin uses synchronous I/O on this issuing thread.
		n, err = r.file.Read(p)
	}
	close(done)
	if !stop() {
		<-joined
	}
	if cause := r.ctx.Err(); cause != nil {
		return 0, cause
	}
	return n, err
}
