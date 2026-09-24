// Package termline renders one self-updating status line. Terminal output uses
// carriage returns and trailing spaces; other streams receive append-only
// lines. Frames contain printable ASCII and fit the current terminal width.
package termline

import (
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Writer draws one status line that later Updates overwrite in place. The zero
// value is not usable; construct one with New. A Writer is safe for concurrent
// use.
type Writer struct {
	w             io.Writer
	terminalWidth func() int // nil when w is not a terminal

	mu sync.Mutex
	// lastLen sizes the next erase to the visible text in the current frame.
	lastLen int
	// active reports that a frame is on screen and awaits a closing newline.
	active bool
}

type terminalAccess struct {
	isTerminal func(int) bool
	getSize    func(int) (width, height int, err error)
}

// New returns a Writer targeting w. If w is a terminal (typically os.Stderr),
// Update rewrites a single line in place; otherwise Update appends whole lines.
func New(w io.Writer) *Writer {
	return newWriter(w, terminalAccess{
		isTerminal: term.IsTerminal,
		getSize:    term.GetSize,
	})
}

func newWriter(w io.Writer, terminal terminalAccess) *Writer {
	l := &Writer{w: w}
	if f, ok := w.(interface{ Fd() uintptr }); ok {
		if fd := int(f.Fd()); terminal.isTerminal(fd) {
			l.terminalWidth = func() int {
				width, _, err := terminal.getSize(fd)
				if err != nil || width <= 0 {
					return 0
				}
				return width
			}
		}
	}
	return l
}

// IsTerminal reports whether w exposes a file descriptor attached to a
// terminal.
func IsTerminal(w io.Writer) bool {
	f, ok := w.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(int(f.Fd()))
}

// Update shows s as the current status. On a terminal it replaces the previous
// frame in place; otherwise it writes s as its own line. Printable ASCII is
// preserved, line breaks and tabs become spaces, and other runes become '?' so
// a single portable physical line is always produced.
func (l *Writer) Update(s string) {
	s = sanitise(s)

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.terminalWidth == nil {
		_, _ = fmt.Fprintln(l.w, s)
		return
	}
	width := l.terminalWidth()
	out, cols := frame(l.lastLen, s, width)
	_, _ = io.WriteString(l.w, out)
	l.lastLen = cols
	l.active = true
}

// Done finishes the status line, leaving the final frame on screen and moving
// to a fresh line so later output starts below it. It is a no-op when nothing
// was shown or the stream is not a terminal, and is safe to call more than
// once.
func (l *Writer) Done() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.active {
		_, _ = io.WriteString(l.w, "\n")
		l.active = false
	}
}

// frame builds an in-place update and returns its visible width. A width of
// zero means unknown and disables truncation.
func frame(prevCols int, s string, width int) (out string, cols int) {
	text := s
	if width > 0 {
		// Leave one column so the cursor never sits past the edge.
		if usable := width - 1; len(text) > usable {
			text = text[:usable]
		}
	}
	cols = len(text)
	pad := max(0, prevCols-cols)
	if width > 0 {
		pad = min(pad, max(0, width-1-cols))
	}
	var b strings.Builder
	b.Grow(1 + len(text) + pad)
	b.WriteByte('\r')
	b.WriteString(text)
	if pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	return b.String(), cols
}

// sanitise converts s to one printable ASCII line. Tabs and line breaks become
// spaces; every other unsupported rune becomes a visible replacement.
func sanitise(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if r < ' ' || r > '~' {
			return '?'
		}
		return r
	}, s)
}
