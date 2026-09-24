package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/tantosec/tturl/internal/curlblocks"
)

// commandOutput carries the two process streams through command orchestration.
// Keeping them explicit lets callers redirect reports independently of
// diagnostics and makes write failures observable without replacing globals.
type commandOutput struct {
	stdin io.Reader
	// cancelInput may close stdin to interrupt a read; the caller grants ownership
	// on cancellation only. Ordinary completion leaves the stream open.
	cancelInput func() error
	stdout      io.Writer
	stderr      io.Writer
	stdoutPipe  bool
	openOutput  func(string) (io.WriteCloser, error)
}

// reportDestination is the selected primary report stream. A file destination
// owns its closer; stdout does not. pipe is true only for the process stdout
// when the caller identified it as a named pipe.
type reportDestination struct {
	writer io.Writer
	closer io.Closer
	path   string
	pipe   bool
}

func openReportDestination(out commandOutput, path string) (*reportDestination, error) {
	if path == "-" {
		return &reportDestination{
			writer: out.stdout,
			path:   path,
			pipe:   out.stdoutPipe,
		}, nil
	}
	open := out.openOutput
	if open == nil {
		open = func(name string) (io.WriteCloser, error) {
			//nolint:gosec // User-selected path; honour the process umask.
			return os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
		}
	}
	f, err := open(path)
	if err != nil {
		return nil, fmt.Errorf(
			"open output %q: %w", curlblocks.DisplayText(path), err)
	}
	return &reportDestination{
		writer: f,
		closer: f,
		path:   path,
	}, nil
}

// isClosedPipe reports the one stdout failure an output consumer controls.
// Other write failures remain command errors even when stdout is a named pipe.
func (d *reportDestination) isClosedPipe(err error) bool {
	return d.pipe && (errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, syscall.EPIPE))
}

// finish closes an owned file and combines a close failure with any failure
// already ending the command. stdout is not owned and is never closed.
func (d *reportDestination) finish(runErr error) error {
	if d.closer == nil {
		return runErr
	}
	if err := d.closer.Close(); err != nil {
		closeErr := fmt.Errorf(
			"close output %q: %w", curlblocks.DisplayText(d.path), err)
		return errors.Join(runErr, closeErr)
	}
	return runErr
}

// reportWriter remembers the first write failure and enough trailing layout
// state to separate report blocks. Once failed, it rejects later writes with
// the same error.
type reportWriter struct {
	w                io.Writer
	err              error
	written          bool
	trailingNewlines int
}

func newReportWriter(w io.Writer) *reportWriter { return &reportWriter{w: w} }

func (w *reportWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.w.Write(p)
	if n > 0 {
		w.written = true
		trailing := 0
		for i := n - 1; i >= 0 && p[i] == '\n'; i-- {
			trailing++
		}
		if trailing == n {
			w.trailingNewlines += trailing
		} else {
			w.trailingNewlines = trailing
		}
	}
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *reportWriter) Err() error { return w.err }

// ensureBlankLine ensures that already-written content ends at a blank-line
// boundary. It never adds leading whitespace to an empty document.
func (w *reportWriter) ensureBlankLine() {
	if !w.written || w.trailingNewlines >= 2 {
		return
	}
	_, _ = w.Write([]byte(strings.Repeat("\n", 2-w.trailingNewlines)))
}
