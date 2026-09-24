package tth2

import (
	"errors"
	"fmt"
	"time"
)

var errConnectionWriteBroken = errors.New(
	"tth2: connection write side is broken",
)

// recordWriter holds exclusive access to the frame writer from the first frame
// of one batch-release record through its flush. Control writes can run between
// records, never inside one.
type recordWriter struct {
	c    *http2conn
	held bool
}

func (c *http2conn) newRecordWriter() *recordWriter {
	return &recordWriter{c: c}
}

func (w *recordWriter) begin() error {
	if w.held {
		return nil
	}
	w.c.wireMu.Lock()
	if w.c.writeBroken.Load() {
		w.c.wireMu.Unlock()
		return errConnectionWriteBroken
	}
	if w.c.readPump != nil {
		if err := w.c.readPump.readError(); err != nil {
			w.c.wireMu.Unlock()
			return err
		}
	}
	w.held = true
	return nil
}

func (w *recordWriter) flush(label string) error {
	if err := w.begin(); err != nil {
		return err
	}
	err := w.c.flushOneRecordUnlocked(label)
	w.held = false
	w.c.wireMu.Unlock()
	return err
}

func (w *recordWriter) flushAt(label string) (time.Time, error) {
	if err := w.begin(); err != nil {
		return time.Time{}, err
	}
	now := time.Now
	if w.c.now != nil {
		now = w.c.now
	}
	at := now()
	err := w.c.flushOneRecordUnlocked(label)
	w.held = false
	w.c.wireMu.Unlock()
	if err != nil {
		return time.Time{}, err
	}
	return at, nil
}

func (w *recordWriter) flushForSend(
	label string, responses *responseRead,
) error {
	if err := responses.sendFailure(); err != nil {
		return err
	}
	return w.flush(label)
}

func (w *recordWriter) flushAtForSend(
	label string, responses *responseRead,
) (time.Time, error) {
	if err := responses.sendFailure(); err != nil {
		return time.Time{}, err
	}
	return w.flushAt(label)
}

func (w *recordWriter) close() {
	if !w.held {
		return
	}
	// Any return while a record is open means the connection may contain an
	// incomplete HTTP/2 frame sequence in bw, even if the underlying writer
	// failed before bufio retained bytes.
	w.c.writeBroken.Store(true)
	w.held = false
	w.c.wireMu.Unlock()
}

func (c *http2conn) writeControl(label string, write func() error) error {
	c.wireMu.Lock()
	defer c.wireMu.Unlock()
	if c.writeBroken.Load() {
		return errConnectionWriteBroken
	}
	if c.readPump != nil {
		if err := c.readPump.readError(); err != nil {
			return err
		}
	}
	if err := write(); err != nil {
		c.writeBroken.Store(true)
		return fmt.Errorf("tth2: %s: write: %w", label, err)
	}
	if err := c.bw.Flush(); err != nil {
		c.writeBroken.Store(true)
		return fmt.Errorf("tth2: %s: flush: %w", label, err)
	}
	return nil
}
