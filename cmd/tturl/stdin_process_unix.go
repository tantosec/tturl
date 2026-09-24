//go:build linux || darwin

package main

import (
	"errors"
	"io"
	"runtime"

	"golang.org/x/sys/unix"
)

func (r *processInput) Read(p []byte) (int, error) {
	// No other reader may consume this descriptor's readiness. Polling before
	// each read avoids changing the inherited open file's shared status flags.
	// A bounded wait observes cancellation without a callback or extra file.
	defer runtime.KeepAlive(r.file)
	fd := r.file.Fd()
	if fd > 1<<31-1 {
		return 0, errors.New("standard input descriptor is out of range")
	}
	poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		if len(p) == 0 {
			return 0, nil
		}
		ready, err := unix.Poll(poll, 20)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if ready == 0 {
			continue
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		n, err := unix.Read(int(fd), p)
		if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
			continue
		}
		if n < 0 {
			n = 0
		}
		if n == 0 && err == nil {
			err = io.EOF
		}
		return n, err
	}
}
