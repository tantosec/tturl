package curlblocks

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// varyKind is which shape a --vary SOURCE takes.
type varyKind int

const (
	sourceFile  varyKind = iota // '@name': newline-separated values from a file
	sourceList                  // '{a,b,c}': a literal comma-separated list
	sourceRange                 // LO-HI[:STEP]: a numeric or alphabetic range
)

// varySource is a parsed --vary SOURCE. Only the field for its kind is set; a
// file defers its read to values.
type varySource struct {
	kind varyKind
	file string    // sourceFile: the filename ('@' stripped), read via values
	list []string  // sourceList: the literal items
	rng  rangeSpec // sourceRange
}

// parseVarySource parses a SOURCE by its leading character: '@' a file (the
// rest is the name verbatim), '{' a list, otherwise a range. The leading
// character wins, so '@a,b' is a file named "a,b", matching the '@file' rule of
// the --data family.
func parseVarySource(s string) (varySource, error) {
	switch {
	case s == "":
		return varySource{}, fmt.Errorf("empty source (want @file, {a,b,c}, or a range like 1-100)")
	case s[0] == '@':
		// The reference is checked by the caller, which knows the token and so can
		// name the binding at fault (see parseVarySpec).
		return varySource{kind: sourceFile, file: s[1:]}, nil
	case s[0] == '{':
		list, err := parseVaryList(s)
		if err != nil {
			return varySource{}, err
		}
		return varySource{kind: sourceList, list: list}, nil
	default:
		rng, err := parseRange(s)
		if err != nil {
			return varySource{}, err
		}
		return varySource{kind: sourceRange, rng: rng}, nil
	}
}

// staticSize reports how many values the source will produce, when that is
// known without producing them: a range settles its count at parse and a list
// is its own length. ok is false for a file, whose lines can only be counted by
// reading it. Block.Fan checks a static size before materialising anything
// and a file's size once read.
func (s varySource) staticSize() (int, bool) {
	switch s.kind {
	case sourceList:
		return len(s.list), true
	case sourceRange:
		return s.rng.count, true
	default:
		return 0, false
	}
}

// fileRef reports the file a '@file' source reads, named for a message by
// flag — the binding as the user typed it, token included. ok is false for a
// source that reads nothing.
func (s varySource) fileRef(flag string) (fileRef, bool) {
	if s.kind != sourceFile {
		return fileRef{}, false
	}
	return fileRef{flag: flag, sigil: "@", name: s.file}, true
}

// values enumerates the raw (pre-encoder) values of the source, reading a file
// source's '@file' via readFile. A file's lines are its values: an empty line
// is skipped, but a line of whitespace is a value, since trimming would corrupt
// one a wordlist meant. A value can never contain a newline. flag names the
// binding for any message (see fileRef).
func (s varySource) values(flag string, readFile func(string) ([]byte, error)) ([]string, error) {
	switch s.kind {
	case sourceFile:
		ref, _ := s.fileRef(flag)
		data, err := ref.read(readFile)
		if err != nil {
			return nil, err
		}
		return splitLines(data), nil
	case sourceList:
		return s.list, nil
	case sourceRange:
		return s.rng.values(), nil
	default:
		return nil, fmt.Errorf("curlblocks: unknown vary source kind %d", s.kind)
	}
}

// parseVaryList parses a '{a,b,c}' literal list, splitting the brace-enclosed
// body on ','. A literal comma or brace is not escapable here; use @file for
// values that need them.
func parseVaryList(s string) ([]string, error) {
	if !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("list %q must be enclosed in braces, e.g. {a,b,c}", s)
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		return nil, fmt.Errorf("empty list {}")
	}
	return strings.Split(inner, ","), nil
}

// splitLines splits a file's bytes into values: one per line, each stripped of
// a trailing CR (so a CRLF file reads the same as an LF one), with empty lines
// dropped.
func splitLines(data []byte) []string {
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// rangeSpec enumerates a range LO-HI[:STEP], either numeric (decimal, or hex
// when the endpoints carry a '0x' prefix) or alphabetic (single ASCII letters).
type rangeSpec struct {
	alpha bool // an alphabetic range; the numeric fields are unused
	step  int  // stride, >= 1
	count int  // how many values the range enumerates, >= 1

	// numeric. A range is held as its first value and a count rather than as
	// endpoints, so enumerating it cannot run past the last value however large
	// the stride (see values).
	lo    int64
	hex   bool // render in base 16 (the '0x' prefix is not re-emitted)
	upper bool // hex letters in upper case
	width int  // zero-pad to this width; 0 for natural width

	// alphabetic
	loCh, hiCh byte
}

// parseRange parses a "LO-HI" range with an optional ":STEP", choosing the
// alphabetic form when both endpoints are single ASCII letters and the numeric
// form otherwise.
func parseRange(s string) (rangeSpec, error) {
	body, stepStr, hasStep := strings.Cut(s, ":")
	step := 1
	if hasStep {
		n, err := strconv.Atoi(stepStr)
		if err != nil || n < 1 {
			return rangeSpec{}, fmt.Errorf("range step %q must be a positive integer", stepStr)
		}
		step = n
	}
	loStr, hiStr, ok := strings.Cut(body, "-")
	if !ok || loStr == "" || hiStr == "" {
		return rangeSpec{}, fmt.Errorf("%q is not a range (want LO-HI, e.g. 1-100, 0x00-0xff, or a-z); "+
			"use {a,b,c} for a literal list or @file to load one", s)
	}
	if isAlphaEndpoint(loStr) && isAlphaEndpoint(hiStr) {
		return parseAlphaRange(loStr[0], hiStr[0], step)
	}
	return parseNumericRange(loStr, hiStr, step)
}

// parseNumericRange parses a decimal or hex "LO-HI". Both endpoints must share
// a base (both '0x' or neither), the range must be ascending, and per curl's
// glob rule — a leading zero on either endpoint zero-pads every value to the
// wider endpoint's width. The count is settled here, before any value is
// materialised, so staticSize can report a range's size for free.
func parseNumericRange(loStr, hiStr string, step int) (rangeSpec, error) {
	loHex, loDigits := splitHex(loStr)
	hiHex, hiDigits := splitHex(hiStr)
	if loHex != hiHex {
		return rangeSpec{}, fmt.Errorf("range %s-%s mixes hex and decimal endpoints", loStr, hiStr)
	}
	base := 10
	if loHex {
		base = 16
	}
	// An endpoint is unsigned: the '-' that splits LO-HI leaves no sign on the low
	// endpoint, and one written on the high endpoint ("5--1", "1-+3") is a typo
	// rather than a bound worth honouring.
	for _, e := range []string{loDigits, hiDigits} {
		if strings.HasPrefix(e, "+") || strings.HasPrefix(e, "-") {
			return rangeSpec{}, fmt.Errorf("range endpoint %q must be an unsigned number", e)
		}
	}
	lo, err := strconv.ParseInt(loDigits, base, 64)
	if err != nil {
		return rangeSpec{}, fmt.Errorf("range endpoint %q is not a valid number", loStr)
	}
	hi, err := strconv.ParseInt(hiDigits, base, 64)
	if err != nil {
		return rangeSpec{}, fmt.Errorf("range endpoint %q is not a valid number", hiStr)
	}
	if lo > hi {
		return rangeSpec{}, fmt.Errorf("range %s-%s must be ascending (LO <= HI)", loStr, hiStr)
	}
	width := 0
	if hasLeadingZero(loDigits) || hasLeadingZero(hiDigits) {
		width = max(len(loDigits), len(hiDigits))
	}
	upper := false
	if loHex {
		if upper, err = hexCase(loDigits, hiDigits); err != nil {
			return rangeSpec{}, err
		}
	}
	// Both endpoints are non-negative (an unsigned spelling is required above) and
	// hi >= lo, so the span cannot overflow.
	count := (hi-lo)/int64(step) + 1
	// How large a range a caller will tolerate is the consumer's policy, declared
	// with WithMaxFanSize and enforced by Block.Fan. All that is rejected here is
	// a count this platform's int cannot hold.
	if count > math.MaxInt {
		return rangeSpec{}, fmt.Errorf("range %s-%s enumerates %d values, more than this platform can count",
			loStr, hiStr, count)
	}
	return rangeSpec{
		step:  step,
		lo:    lo,
		count: int(count),
		hex:   loHex,
		upper: upper,
		width: width,
	}, nil
}

// parseAlphaRange parses a single-letter "a-z" range. Both endpoints must be
// the same case and ascending.
func parseAlphaRange(lo, hi byte, step int) (rangeSpec, error) {
	if !sameAlphaCase(lo, hi) {
		return rangeSpec{}, fmt.Errorf("alpha range %c-%c must lie within a-z or within A-Z", lo, hi)
	}
	if lo > hi {
		return rangeSpec{}, fmt.Errorf("alpha range %c-%c must be ascending", lo, hi)
	}
	count := (int(hi)-int(lo))/step + 1
	return rangeSpec{
		alpha: true, step: step, count: count, loCh: lo, hiCh: hi,
	}, nil
}

// values enumerates the range's values in order. A numeric range steps by its
// index off the first value, so the count fixed at parse decides where it
// stops.
func (r rangeSpec) values() []string {
	if r.alpha {
		return r.alphaValues()
	}
	out := make([]string, 0, r.count)
	for i := range r.count {
		out = append(out, r.format(r.lo+int64(i)*int64(r.step)))
	}
	return out
}

// alphaValues enumerates a letter range from the count fixed at parse. Since
// every emitted offset is at most hiCh-loCh, its index multiplication and
// addition remain within the ASCII-letter span even when step is MaxInt.
func (r rangeSpec) alphaValues() []string {
	out := make([]string, r.count)
	for i := range r.count {
		c := int(r.loCh) + i*r.step
		out[i] = string(rune(c)) //nolint:gosec // c is a parsed ASCII letter
	}
	return out
}

// format renders one numeric value: base 16 (upper-cased on demand) or base 10,
// zero-padded to width when set. A hex value is emitted bare, without the '0x'
// prefix, so it drops straight into a hex string; a literal prefix belongs in
// the surrounding template.
func (r rangeSpec) format(i int64) string {
	base := 10
	if r.hex {
		base = 16
	}
	s := strconv.FormatInt(i, base)
	if r.hex && r.upper {
		s = strings.ToUpper(s)
	}
	if r.width > len(s) {
		s = strings.Repeat("0", r.width-len(s)) + s
	}
	return s
}

// splitHex reports whether s carries a "0x"/"0X" prefix and returns the digits
// after it; without a prefix the whole string is the (decimal) digits.
func splitHex(s string) (isHex bool, digits string) {
	if len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') {
		return true, s[2:]
	}
	return false, s
}

// hasLeadingZero reports whether s uses curl's written zero-padding. A bare
// "0" is the value zero,
// not padding.
func hasLeadingZero(s string) bool {
	return len(s) > 1 && s[0] == '0'
}

// hexCase decides the output case of a hex range from its endpoints' letters:
// upper if any is upper-case, lower otherwise. Endpoints that disagree are an
// error rather than a silent choice.
func hexCase(loDigits, hiDigits string) (upper bool, err error) {
	hasUpper := strings.ContainsAny(loDigits, "ABCDEF") || strings.ContainsAny(hiDigits, "ABCDEF")
	hasLower := strings.ContainsAny(loDigits, "abcdef") || strings.ContainsAny(hiDigits, "abcdef")
	if hasUpper && hasLower {
		return false, fmt.Errorf("hex range 0x%s-0x%s mixes upper- and lower-case digits; pick one", loDigits, hiDigits)
	}
	return hasUpper, nil
}

// isAlphaEndpoint reports whether s is a single ASCII letter, as required for
// an alphabetic range endpoint.
func isAlphaEndpoint(s string) bool {
	return len(s) == 1 && ((s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z'))
}

// sameAlphaCase reports whether two ASCII letters are both lower- or both
// upper-case, so a range does not straddle the non-letter bytes between 'Z' and
// 'a'.
func sameAlphaCase(a, b byte) bool {
	return (a >= 'a') == (b >= 'a')
}

// varySourceDescMax bounds an inline list description before it is elided.
const varySourceDescMax = 40

// describe renders a short human description of the source for a summary
// heading: a file source as '@name', a range as 'first..last' (from the
// rendered values), and a literal list as '{a,b,c}', the last elided when long.
// Every part is rendered through displayValue first, so the description is
// printable ASCII and the elision below cuts bytes that are also characters —
// a cut can neither split a multi-byte rune nor leave a control byte behind.
func (s varySource) describe(vals []VaryValue) string {
	switch s.kind {
	case sourceFile:
		return "@" + displayValue(s.file)
	case sourceRange:
		return vals[0].Display + ".." + vals[len(vals)-1].Display
	case sourceList:
		shown := make([]string, len(s.list))
		for i, item := range s.list {
			shown[i] = displayValue(item)
		}
		inner := strings.Join(shown, ",")
		if len(inner) > varySourceDescMax {
			inner = inner[:varySourceDescMax] + "..."
		}
		return "{" + inner + "}"
	default:
		return ""
	}
}
