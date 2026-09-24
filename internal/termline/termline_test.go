package termline

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestFrame(t *testing.T) {
	tests := []struct {
		name     string
		prevCols int
		s        string
		width    int
		wantOut  string
		wantCols int
	}{
		{"basic", 0, "abc", 0, "\rabc", 3},
		{"empty string pads over previous", 3, "", 0, "\r   ", 0},
		{"shrink pads the tail", 5, "ab", 0, "\rab   ", 2},
		{"grow does not pad", 2, "abcd", 0, "\rabcd", 4},
		{"truncates to width minus one", 0, "abcdef", 4, "\rabc", 3},
		{"resize caps erase padding", 10, "a", 5, "\ra   ", 1},
		{"fits within width", 0, "ab", 10, "\rab", 2},
		{"width one leaves no usable column", 0, "ab", 1, "\r", 0},
		{"width two leaves one usable column", 0, "ab", 2, "\ra", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, cols := frame(tc.prevCols, tc.s, tc.width)
			if out != tc.wantOut || cols != tc.wantCols {
				t.Errorf("frame(%d, %q, %d) = (%q, %d), want (%q, %d)",
					tc.prevCols, tc.s, tc.width, out, cols, tc.wantOut, tc.wantCols)
			}
		})
	}
}

func TestSanitise(t *testing.T) {
	var printableASCII strings.Builder
	for b := byte(' '); b <= '~'; b++ {
		printableASCII.WriteByte(b)
	}
	var unsupportedASCII strings.Builder
	for b := range byte(' ') {
		if b != '\t' && b != '\n' && b != '\r' {
			unsupportedASCII.WriteByte(b)
		}
	}
	unsupportedASCII.WriteByte('\x7f')

	tests := []struct{ in, want string }{
		{"a\nb\tc\rd", "a b c d"},
		{printableASCII.String(), printableASCII.String()},
		{unsupportedASCII.String(), strings.Repeat("?", unsupportedASCII.Len())},
		{"hé\xffllo", "h??llo"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := sanitise(tc.in); got != tc.want {
			t.Errorf("sanitise(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func FuzzSanitiseAndFrame(f *testing.F) {
	f.Add("plain", uint16(0), uint16(0))
	f.Add("line\nwith\ttabs", uint16(20), uint16(8))
	f.Add("hé\xffllo", uint16(10), uint16(1))

	f.Fuzz(func(t *testing.T, input string, prevSeed, widthSeed uint16) {
		prevCols := int(prevSeed % 257)
		width := int(widthSeed % 257)
		safe := sanitise(input)
		for i := range len(safe) {
			if safe[i] < ' ' || safe[i] > '~' {
				t.Fatalf("sanitise(%q)[%d] = %#x, want printable ASCII",
					input, i, safe[i])
			}
		}
		if got := sanitise(safe); got != safe {
			t.Fatalf("sanitise is not idempotent: sanitise(%q) = %q",
				safe, got)
		}

		out, cols := frame(prevCols, safe, width)
		if !strings.HasPrefix(out, "\r") {
			t.Fatalf("frame(%d, %q, %d) = %q, want carriage-return prefix",
				prevCols, safe, width, out)
		}
		if width > 0 && len(out)-1 > width-1 {
			t.Fatalf("frame(%d, %q, %d) occupies %d columns, want <= %d",
				prevCols, safe, width, len(out)-1, width-1)
		}
		if cols < 0 || cols > len(out)-1 {
			t.Fatalf("frame(%d, %q, %d) columns = %d for output %q",
				prevCols, safe, width, cols, out)
		}
	})
}

func TestNonTerminalDetection(t *testing.T) {
	t.Run("writer without descriptor", func(t *testing.T) {
		if IsTerminal(&bytes.Buffer{}) {
			t.Error("IsTerminal(bytes.Buffer) = true, want false")
		}
	})

	t.Run("regular file uses append mode", func(t *testing.T) {
		f, err := os.CreateTemp(t.TempDir(), "termline")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := f.Close(); err != nil {
				t.Errorf("close status file: %v", err)
			}
		})
		if IsTerminal(f) {
			t.Error("IsTerminal(regular file) = true, want false")
		}

		l := New(f)
		l.Update("status")
		l.Done()
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		if want := "status\n"; string(got) != want {
			t.Errorf("regular-file output = %q, want %q", got, want)
		}
	})
}

// TestUpdateNonTerminalAppends verifies the non-terminal fallback: each Update
// is a whole sanitised line, and Done adds nothing (there is no in-place frame
// to close).
func TestUpdateNonTerminalAppends(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf)
	l.Update("first")
	l.Update("second\twith\tescape\x1b")
	l.Done()

	want := "first\nsecond with escape?\n"
	if buf.String() != want {
		t.Errorf("non-terminal output = %q, want %q", buf.String(), want)
	}
}

// TestUpdateInPlaceAndDone exercises terminal rewriting, width re-reading,
// erase padding across a resize, and closing the active frame.
func TestUpdateInPlaceAndDone(t *testing.T) {
	var buf bytes.Buffer
	sizeErr := errors.New("size unavailable")
	sizes := []struct {
		width int
		err   error
	}{
		{width: 5},
		{width: 3},
		{err: sizeErr},
		{width: -1},
	}
	widthRead := 0
	fdw := &fdWriter{Writer: &buf, fd: 42}
	l := newWriter(fdw, terminalAccess{
		isTerminal: func(fd int) bool {
			if fd != 42 {
				t.Errorf("terminal check fd = %d, want 42", fd)
			}
			return true
		},
		getSize: func(fd int) (int, int, error) {
			if fd != 42 {
				t.Errorf("size check fd = %d, want 42", fd)
			}
			size := sizes[widthRead]
			widthRead++
			return size.width, 99, size.err
		},
	})
	l.Update("abcdef")
	l.Update("x")
	l.Update("yz")
	l.Update("q")
	l.Done()

	want := "\rabcd\rx \ryz\rq \n"
	if buf.String() != want {
		t.Errorf("in-place output = %q, want %q", buf.String(), want)
	}
	if widthRead != len(sizes) {
		t.Errorf("terminal width read %d times, want %d", widthRead, len(sizes))
	}

	l.Done() // idempotent: nothing more written
	if buf.String() != want {
		t.Errorf("second Done wrote extra: %q", buf.String())
	}
}

type fdWriter struct {
	io.Writer
	fd uintptr
}

func (w *fdWriter) Fd() uintptr { return w.fd }

func TestConcurrentWriter(t *testing.T) {
	const updates = 64
	want := make([]string, updates)
	for i := range updates {
		want[i] = fmt.Sprintf("tick-%02d", i)
	}

	t.Run("append mode preserves every update", func(t *testing.T) {
		var buf bytes.Buffer
		l := New(&buf)
		var wg sync.WaitGroup
		for _, status := range want {
			wg.Go(func() {
				l.Update(status)
			})
		}
		wg.Wait()
		l.Done()

		got := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("concurrent updates = %q, want %q", got, want)
		}
	})

	t.Run("terminal updates and Done remain atomic", func(t *testing.T) {
		recorder := &writeRecorder{}
		l := &Writer{w: recorder, terminalWidth: func() int { return 20 }}
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, status := range want {
			wg.Go(func() {
				<-start
				l.Update(status)
			})
			wg.Go(func() {
				<-start
				l.Done()
			})
		}
		close(start)
		wg.Wait()
		l.Done()
		writesAfterClose := recorder.len()
		l.Done()

		if recorder.overlap.Load() {
			t.Error("underlying writer calls overlapped")
		}
		if got := recorder.len(); got != writesAfterClose {
			t.Errorf("second final Done made %d writes, want 0",
				got-writesAfterClose)
		}
		gotUpdates, newlines, unexpected := recorder.results()
		if len(unexpected) > 0 {
			t.Errorf("unexpected terminal writes: %q", unexpected)
		}
		slices.Sort(gotUpdates)
		if !slices.Equal(gotUpdates, want) {
			t.Errorf("terminal updates = %q, want %q", gotUpdates, want)
		}
		if newlines < 1 || newlines > updates {
			t.Errorf("terminal newline writes = %d, want in [1,%d]",
				newlines, updates)
		}
	})
}

type writeRecorder struct {
	active  atomic.Int32
	overlap atomic.Bool
	mu      sync.Mutex
	writes  []string
}

func (w *writeRecorder) Write(p []byte) (int, error) {
	if w.active.Add(1) != 1 {
		w.overlap.Store(true)
	}
	defer w.active.Add(-1)
	runtime.Gosched()

	w.mu.Lock()
	w.writes = append(w.writes, string(p))
	w.mu.Unlock()
	return len(p), nil
}

func (w *writeRecorder) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.writes)
}

func (w *writeRecorder) results() (
	updates []string,
	newlines int,
	unexpected []string,
) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, write := range w.writes {
		switch {
		case write == "\n":
			newlines++
		case strings.HasPrefix(write, "\r"):
			updates = append(updates, strings.TrimPrefix(write, "\r"))
		default:
			unexpected = append(unexpected, write)
		}
	}
	return updates, newlines, unexpected
}
