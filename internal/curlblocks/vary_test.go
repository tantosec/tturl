package curlblocks

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// substs / displays project a []VaryValue onto its two string fields.
func substs(vals []VaryValue) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = v.Substituted
	}
	return out
}

func displays(vals []VaryValue) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = v.Display
	}
	return out
}

func TestParseVarySpecValid(t *testing.T) {
	tests := []struct {
		arg       string
		wantToken string
		wantEnc   []encoder
	}{
		{"FUZZ=1-3", "FUZZ", nil},
		{"PAD=0x00-0xff", "PAD", nil},
		{"USER:b64=@users.txt", "USER", []encoder{encB64}},
		{"X:hex:b64=1-2", "X", []encoder{encHex, encB64}},
		{"X:HEX=1-2", "X", []encoder{encHexUpper}},
		{"X:b64url={a}", "X", []encoder{encB64URL}},
		{"q:url={a,b}", "q", []encoder{encURL}},
		// The first '=' splits, so a source may contain '='.
		{"F=@weird=name.txt", "F", nil},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			spec, err := parseVarySpec(tt.arg)
			if err != nil {
				t.Fatalf("parseVarySpec(%q) error: %v", tt.arg, err)
			}
			if spec.Token != tt.wantToken {
				t.Errorf("token = %q, want %q", spec.Token, tt.wantToken)
			}
			if !reflect.DeepEqual(spec.encoders, tt.wantEnc) {
				t.Errorf("encoders = %v, want %v", spec.encoders, tt.wantEnc)
			}
		})
	}
}

func TestParseVarySpecErrors(t *testing.T) {
	tests := []struct {
		arg  string
		want string // substring of the error
	}{
		{"FUZZ", "NAME=SOURCE"},
		{"=1-3", "empty token name"},
		{"my token=1-3", "must not contain whitespace"},
		{"my\ntoken=1-3", "must not contain whitespace"},
		{"my\u00a0token=1-3", "must not contain whitespace"},
		{"X:rot13=1-3", `unknown encoder "rot13"`},
		{"X=", "empty source"},
		{"X=@", "empty filename"},
		{"X={}", "empty list"},
		{"X={a,b", "enclosed in braces"},
		{"X=5-1", "ascending"},
		{"X=1-0xff", "mixes hex and decimal"},
		{"X=1-5:0", "step"},
		{"X=nope", "not a range"},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			_, err := parseVarySpec(tt.arg)
			if err == nil {
				t.Fatalf("parseVarySpec(%q): want error", tt.arg)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestRangeValues(t *testing.T) {
	tests := []struct {
		src  string
		want []string
	}{
		{"1-5", []string{"1", "2", "3", "4", "5"}},
		{"0-10:2", []string{"0", "2", "4", "6", "8", "10"}},
		{"08-11", []string{"08", "09", "10", "11"}}, // leading zero -> pad to 2
		{"1-10", []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}},
		{"0x0-0xf", []string{"0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "a", "b", "c", "d", "e", "f"}},
		{"0xe-0x11", []string{"e", "f", "10", "11"}},    // natural width, no pad
		{"0x0e-0x11", []string{"0e", "0f", "10", "11"}}, // leading zero -> pad to 2
		{"0x0A-0x0F", []string{"0A", "0B", "0C", "0D", "0E", "0F"}},
		{"a-e", []string{"a", "b", "c", "d", "e"}},
		{"a-i:2", []string{"a", "c", "e", "g", "i"}},
		{"X-Z", []string{"X", "Y", "Z"}},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			src, err := parseVarySource(tt.src)
			if err != nil {
				t.Fatalf("parseVarySource(%q) error: %v", tt.src, err)
			}
			got, err := src.values("--vary T", noFiles)
			if err != nil {
				t.Fatalf("values() error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("values = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRangeSizeKnownAtParse checks that a range settles its value count while
// parsing, so staticSize reports the size without enumerating: a numeric range
// far too large to materialise still parses and answers for its size, and an
// alphabetic range reports the same invariant. How large a range is acceptable
// is the consumer's ceiling, not this layer's (see WithMaxFanSize and
// TestFanSizeCeiling).
func TestRangeSizeKnownAtParse(t *testing.T) {
	for _, tt := range []struct {
		spec string
		want int
	}{
		{"1-1000000000", 1_000_000_000},
		{"1-1000000000:2", 500_000_000}, // (hi-lo)/step + 1
		{"a-z", 26},
		{"a-z:5", 6}, // a, f, k, p, u, z
		{"{a,b,c}", 3},
	} {
		src, err := parseVarySource(tt.spec)
		if err != nil {
			t.Fatalf("parseVarySource(%q) error: %v", tt.spec, err)
		}
		got, ok := src.staticSize()
		if !ok {
			t.Errorf("staticSize(%q) reported no size; a range and a list both know theirs", tt.spec)
			continue
		}
		if got != tt.want {
			t.Errorf("staticSize(%q) = %d, want %d", tt.spec, got, tt.want)
		}
	}

	// A file's lines can only be counted by reading it.
	src, err := parseVarySource("@words.txt")
	if err != nil {
		t.Fatalf("parseVarySource: %v", err)
	}
	if _, ok := src.staticSize(); ok {
		t.Error("staticSize reported a size for a file, which must be read to be counted")
	}
}

// TestAlphaRangeFanSize checks that an alphabetic range's known cardinality
// participates in the fan ceiling at Parse, just as a numeric range's does.
func TestAlphaRangeFanSize(t *testing.T) {
	if _, err := New(WithMaxFanSize(26)).Parse(
		[]string{"https://h/T", "--vary", "T=a-z"}); err != nil {
		t.Fatalf("26-value range at the ceiling: %v", err)
	}
	_, err := New(WithMaxFanSize(25)).Parse(
		[]string{"https://h/T", "--vary", "T=a-z"})
	if err == nil || !strings.Contains(err.Error(),
		"26 values exceeds the maximum fan size of 25") {
		t.Errorf("26-value range over the ceiling = %v, want size refusal", err)
	}
}

// TestAlphaRangeStepAtMaxInt checks that an enormous valid step yields only
// the low endpoint. Enumeration must not overflow while deciding that the next
// letter lies beyond the range.
func TestAlphaRangeStepAtMaxInt(t *testing.T) {
	spec := "a-z:" + strconv.Itoa(math.MaxInt)
	src, err := parseVarySource(spec)
	if err != nil {
		t.Fatalf("parseVarySource(%q): %v", spec, err)
	}
	vals, err := src.values("--vary T", noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a"}; !reflect.DeepEqual(vals, want) {
		t.Errorf("values = %v, want %v", vals, want)
	}
}

// TestFanProduct covers the product arithmetic behind the fan ceiling: it is
// exact at and either side of the limit, and it cannot overflow, however large
// the counts -- the case a plain multiplication would wrap into a small total
// that then looks acceptable.
func TestFanProduct(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	// Multiplying huge by two exceeds int on every architecture.
	huge := maxInt/2 + 1
	for _, tt := range []struct {
		counts    []int
		max       int
		wantTotal int
		wantOK    bool
	}{
		{counts: []int{3, 3}, max: 10, wantTotal: 9, wantOK: true},
		{counts: []int{5, 2}, max: 10, wantTotal: 10, wantOK: true}, // exactly at the limit
		{counts: []int{5, 3}, max: 10, wantOK: false},               // one over
		{counts: []int{7}, max: 10, wantTotal: 7, wantOK: true},
		{counts: []int{1, 1, 1, 1}, max: 1, wantTotal: 1, wantOK: true},
		{counts: []int{huge, 2}, max: maxInt, wantOK: false},
		{counts: []int{2, 0, 2}, max: 10, wantOK: false}, // a sourceless binding
	} {
		total, ok := fanProduct(tt.counts, tt.max)
		if ok != tt.wantOK {
			t.Errorf("fanProduct(%v, %d) ok = %v, want %v", tt.counts, tt.max, ok, tt.wantOK)
			continue
		}
		if ok && total != tt.wantTotal {
			t.Errorf("fanProduct(%v, %d) = %d, want %d", tt.counts, tt.max, total, tt.wantTotal)
		}
	}
}

// TestRangeStepPastMaxInt64 checks a range whose last value sits at MaxInt64:
// stepping one stride further would wrap, so enumeration must stop on the count
// rather than on a comparison against the endpoint. 0x7ffffffffffffff0 to
// 0x7fffffffffffffff by 3 is (0xff-0xf0)/3 + 1 = 6 values.
func TestRangeStepPastMaxInt64(t *testing.T) {
	src, err := parseVarySource("0x7ffffffffffffff0-0x7fffffffffffffff:3")
	if err != nil {
		t.Fatal(err)
	}
	vals, err := src.values("--vary T", noFiles)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"7ffffffffffffff0", "7ffffffffffffff3", "7ffffffffffffff6",
		"7ffffffffffffff9", "7ffffffffffffffc", "7fffffffffffffff",
	}
	if !reflect.DeepEqual(vals, want) {
		t.Errorf("values = %v, want %v", vals, want)
	}
}

// TestRangeHexBytesEndpoints covers the full-byte sweep, the size
// TestRangeValues cannot assert as a literal slice: that 0x00-0xff enumerates
// every one of the 256 values and pads both endpoints to two digits.
func TestRangeHexBytesEndpoints(t *testing.T) {
	src, err := parseVarySource("0x00-0xff")
	if err != nil {
		t.Fatal(err)
	}
	vals, err := src.values("--vary T", noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 256 {
		t.Fatalf("len = %d, want 256", len(vals))
	}
	if vals[0] != "00" || vals[255] != "ff" {
		t.Errorf("endpoints = %q..%q, want 00..ff", vals[0], vals[255])
	}
}

func TestHexCaseConflict(t *testing.T) {
	_, err := parseVarySource("0x0a-0x0F")
	if err == nil || !strings.Contains(err.Error(), "mixes upper- and lower-case") {
		t.Fatalf("want case-conflict error, got %v", err)
	}
}

func TestAlphaCaseStraddle(t *testing.T) {
	_, err := parseVarySource("A-z")
	if err == nil || !strings.Contains(err.Error(), "within a-z or within A-Z") {
		t.Fatalf("want case-straddle error, got %v", err)
	}
}

func TestListValues(t *testing.T) {
	src, err := parseVarySource("{admin,root,guest}")
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.values("--vary T", noFiles)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"admin", "root", "guest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("values = %v, want %v", got, want)
	}
}

func TestFileValuesSkipsBlankAndStripsCR(t *testing.T) {
	readFile := fakeFS(map[string]string{"users.txt": "admin\r\nroot\n\nguest\n"})
	spec, err := parseVarySpec("U=@users.txt")
	if err != nil {
		t.Fatal(err)
	}
	vals, err := spec.values(readFile)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"admin", "root", "guest"}
	if !reflect.DeepEqual(displays(vals), want) {
		t.Errorf("displays = %v, want %v", displays(vals), want)
	}
}

func TestFileValuesReadError(t *testing.T) {
	spec, err := parseVarySpec("U=@missing.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.values(noFiles); err == nil || !strings.Contains(err.Error(), "missing.txt") {
		t.Fatalf("Values error = %v, want one naming missing.txt", err)
	}
}

func TestEncodersSubstVsDisplay(t *testing.T) {
	tests := []struct {
		arg         string
		src         string
		wantSubst   []string
		wantDisplay []string
	}{
		{
			arg:         "X:hex={AB,z}",
			wantSubst:   []string{"4142", "7a"},
			wantDisplay: []string{"AB", "z"},
		},
		{
			// HEX is upper-case hex; the display keeps the plaintext.
			arg:         "X:HEX={AB,z}",
			wantSubst:   []string{"4142", "7A"},
			wantDisplay: []string{"AB", "z"},
		},
		{
			arg:         "X:b64={admin}",
			wantSubst:   []string{"YWRtaW4="},
			wantDisplay: []string{"admin"},
		},
		{
			// b64url uses the URL-safe alphabet ('+' -> '-') and drops the
			// padding standard base64 would add ("admin" -> "YWRtaW4=").
			arg:         "X:b64url={>>>,admin}",
			wantSubst:   []string{"Pj4-", "YWRtaW4"},
			wantDisplay: []string{">>>", "admin"},
		},
		{
			// url is RFC 3986 percent-encoding, for a value landing in a URL, so a
			// space is %20. A form body's '+' belongs to --data-urlencode alone (see
			// formEncode).
			arg:         "X:url={a b}",
			wantSubst:   []string{"a%20b"},
			wantDisplay: []string{"a b"},
		},
		{
			// hex then b64: base64 of the hex text "6869".
			arg:         "X:hex:b64={hi}",
			wantSubst:   []string{"Njg2OQ=="},
			wantDisplay: []string{"hi"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			spec, err := parseVarySpec(tt.arg)
			if err != nil {
				t.Fatal(err)
			}
			vals, err := spec.values(noFiles)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(substs(vals), tt.wantSubst) {
				t.Errorf("subst = %v, want %v", substs(vals), tt.wantSubst)
			}
			if !reflect.DeepEqual(displays(vals), tt.wantDisplay) {
				t.Errorf("display = %v, want %v", displays(vals), tt.wantDisplay)
			}
		})
	}
}

func TestDisplayValueHexFallback(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"printable", "admin", "admin"},
		{"interior space", "two words", "two words"},
		{"empty", "", ""},
		{"leading space", " x", "$HEX[2078]"},
		{"trailing space", "x ", "$HEX[7820]"},
		{"control byte", "a\x00b", "$HEX[610062]"},
		{"non-ASCII", "caf\xc3\xa9", "$HEX[636166c3a9]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayValue(tt.raw); got != tt.want {
				t.Errorf("displayValue(%+q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestVaryFileLines covers what a file source counts as a value. An empty line
// is separator noise and is skipped, so a file of only empty lines yields
// nothing. This is an error because the block would otherwise vanish silently
// from the plan. A line of whitespace is *not* noise: it is a value a
// wordlist may legitimately hold, and trimming it would corrupt what the user
// supplied (a label renders it $HEX[...] so it stays visible).
func TestVaryFileLines(t *testing.T) {
	files := fakeFS(map[string]string{
		"empty.txt":    "",
		"newlines.txt": "\n\n\n",
		"crlf.txt":     "\r\n\r\n",
		"spaces.txt":   "\n \n\t\n",
		"mixed.txt":    "a\n\nb\r\n\nc\n",
	})
	t.Run("no values is an error", func(t *testing.T) {
		for _, name := range []string{"empty.txt", "newlines.txt", "crlf.txt"} {
			_, err := mustSpec(t, "T=@"+name).values(files)
			if err == nil || !strings.Contains(err.Error(), "source produced no values") {
				t.Errorf("values(%s) = %v, want a no-values error", name, err)
			}
		}
	})
	t.Run("a whitespace line is a value", func(t *testing.T) {
		vals, err := mustSpec(t, "T=@spaces.txt").values(files)
		if err != nil {
			t.Fatalf("values: %v", err)
		}
		want := []string{" ", "\t"}
		if len(vals) != len(want) {
			t.Fatalf("got %d values, want %d: %+v", len(vals), len(want), vals)
		}
		for i, w := range want {
			if vals[i].Substituted != w {
				t.Errorf("value %d = %q, want %q", i, vals[i].Substituted, w)
			}
			if vals[i].Display != displayValue(w) {
				t.Errorf("value %d display = %q, want it rendered", i, vals[i].Display)
			}
		}
	})
	t.Run("empty lines between values are skipped", func(t *testing.T) {
		vals, err := mustSpec(t, "T=@mixed.txt").values(files)
		if err != nil {
			t.Fatalf("values: %v", err)
		}
		var got []string
		for _, v := range vals {
			got = append(got, v.Substituted)
		}
		if want := []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
			t.Errorf("values = %q, want %q", got, want)
		}
	})
}

// TestRangeEndpointErrors covers the two ways a range can be rejected: an
// alphabetic range that runs backwards, and an endpoint that is neither a
// number nor a single letter. Both are typos a silent empty or reversed sweep
// would hide.
func TestRangeEndpointErrors(t *testing.T) {
	tests := []struct{ spec, errSub string }{
		{"z-a", "must be ascending"},
		{"e-a", "must be ascending"},
		{"Z-A", "must be ascending"},
		{"10-1", "must be ascending"},
		{"a-1", "is not a valid number"},
		{"1-z", "is not a valid number"},
		{"a-bc", "is not a valid number"},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			_, err := parseVarySource(tt.spec)
			if err == nil || !strings.Contains(err.Error(), tt.errSub) {
				t.Errorf("parseVarySource(%q) = %v, want an error containing %q", tt.spec, err, tt.errSub)
			}
		})
	}
}
