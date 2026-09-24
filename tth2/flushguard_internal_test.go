package tth2

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestFlushOneRecordRejectsOversized verifies the TLS plaintext budget and
// diagnostic label at its boundary.
func TestFlushOneRecordRejectsOversized(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		buffered  int
		wantErr   bool
		wantInMsg string
	}{
		{"empty", 0, false, ""},
		{"small", 1024, false, ""},
		{"at_limit", tlsRecordPlaintextBudget, false, ""},
		{"one_over", tlsRecordPlaintextBudget + 1, true, "exceeds one TLS plaintext record"},
		{"way_over", 2 * tlsRecordPlaintextBudget, true, "exceeds one TLS plaintext record"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var sink bytes.Buffer
			bw := bufio.NewWriterSize(&sink, 4*tlsRecordPlaintextBudget)
			if tc.buffered > 0 {
				if _, err := bw.Write(make([]byte, tc.buffered)); err != nil {
					t.Fatalf("seed buffer: %v", err)
				}
			}
			c := &http2conn{bw: bw}

			err := c.flushOneRecord("test")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("buffered=%d: expected error, got nil", tc.buffered)
				}
				if !strings.Contains(err.Error(), tc.wantInMsg) {
					t.Errorf("buffered=%d: error %q does not contain %q",
						tc.buffered, err.Error(), tc.wantInMsg)
				}
				if !strings.Contains(err.Error(), "test") {
					t.Errorf("buffered=%d: error %q does not include the label",
						tc.buffered, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("buffered=%d: unexpected error: %v", tc.buffered, err)
			}
			if got := sink.Len(); got != tc.buffered {
				t.Errorf("buffered=%d: sink received %d bytes after flush", tc.buffered, got)
			}
		})
	}
}

// failingWriter returns errFailingWriter from every Write call. Used to
// verify that flushOneRecord wraps and surfaces the underlying flush error.
type failingWriter struct{}

var errFailingWriter = io.ErrShortWrite

func (failingWriter) Write(_ []byte) (int, error) { return 0, errFailingWriter }

// TestFlushOneRecordSurfacesFlushError verifies that an underlying write error
// remains distinguishable from a budget failure.
func TestFlushOneRecordSurfacesFlushError(t *testing.T) {
	t.Parallel()
	bw := bufio.NewWriterSize(failingWriter{}, tlsRecordPlaintextBudget)
	if _, err := bw.Write(make([]byte, 100)); err != nil {
		t.Fatalf("seed buffer: %v", err)
	}
	c := &http2conn{bw: bw}

	err := c.flushOneRecord("test-flush-err")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "test-flush-err") {
		t.Errorf("error %q does not include the label", err.Error())
	}
	if !strings.Contains(err.Error(), "flush") {
		t.Errorf("error %q does not mention flush", err.Error())
	}
}

func TestFlushOneRecordAtDoesNotPublishFailedFlush(t *testing.T) {
	t.Parallel()
	bw := bufio.NewWriterSize(failingWriter{}, tlsRecordPlaintextBudget)
	if _, err := bw.Write([]byte("record")); err != nil {
		t.Fatalf("seed buffer: %v", err)
	}
	wantTime := time.Date(2026, 9, 8, 1, 2, 3, 4, time.UTC)
	c := &http2conn{bw: bw, now: func() time.Time { return wantTime }}

	at, err := c.flushOneRecordAt("failed record")
	if !errors.Is(err, errFailingWriter) {
		t.Fatalf("flushOneRecordAt error = %v, want %v", err, errFailingWriter)
	}
	if !at.IsZero() {
		t.Errorf("failed flush time = %v, want zero", at)
	}
}
