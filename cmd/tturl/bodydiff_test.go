package main

import (
	"bytes"
	"strings"
	"testing"
)

func bytesOf(ss ...string) [][]byte {
	b := make([][]byte, len(ss))
	for i, s := range ss {
		b[i] = []byte(s)
	}
	return b
}

func changeCount(bodies [][]byte) int {
	n := 0
	for _, s := range segmentDiff(bodies) {
		if s.change {
			n++
		}
	}
	return n
}

// TestSegmentDiff checks the skeleton's change-region count is the number of
// distinct places the bodies differ - the unit the digest reports - and is a
// count of regions, not bytes: a length-changing single edit is one region.
func TestSegmentDiff(t *testing.T) {
	cases := []struct {
		name   string
		bodies [][]byte
		want   int
	}{
		{"one field", bytesOf(`{"a":1}`, `{"a":2}`), 1},
		{"two fields", bytesOf(`{"a":1,"b":2}`, `{"a":9,"b":8}`), 2},
		{"length-mismatch single edit", bytesOf(
			strings.Repeat("A", 100)+"BBBB"+strings.Repeat("C", 100),
			strings.Repeat("A", 100)+"DDD"+strings.Repeat("C", 100),
		), 1},
		{"identical bodies have no change region", bytesOf(`{"a":1}`, `{"a":1}`), 0},
		{"three fields, middle differs", bytesOf(
			`{"a":1,"b":2,"c":3}`, `{"a":1,"b":9,"c":3}`,
		), 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := changeCount(c.bodies); got != c.want {
				t.Errorf("change regions = %d, want %d", got, c.want)
			}
		})
	}
}

func FuzzSegmentDiffReconstructsInputs(f *testing.F) {
	f.Add([]byte{}, []byte{}, []byte{})
	f.Add([]byte("shared"), []byte("shared"), []byte("shared"))
	f.Add([]byte("head-A-tail"), []byte("head-B-tail"),
		[]byte("head-long-tail"))
	f.Add([]byte{0x00, 0xff, 0x01}, []byte{0x00, 0xfe, 0x01},
		[]byte{0x00, 0xfd, 0x01})

	f.Fuzz(func(t *testing.T, first, second, third []byte) {
		const maxFuzzBody = 512
		if len(first) > maxFuzzBody || len(second) > maxFuzzBody ||
			len(third) > maxFuzzBody {
			t.Skip()
		}
		bodies := [][]byte{first, second, third}
		next := make([]int, len(bodies))
		for segmentIndex, segment := range segmentDiff(bodies) {
			if len(segment.spans) != len(bodies) {
				t.Fatalf("segment %d has %d spans, want %d",
					segmentIndex, len(segment.spans), len(bodies))
			}
			for bodyIndex, span := range segment.spans {
				if span[0] != next[bodyIndex] || span[0] < 0 ||
					span[1] < span[0] || span[1] > len(bodies[bodyIndex]) {
					t.Fatalf("segment %d body %d span = %v after %d of %d bytes",
						segmentIndex, bodyIndex, span,
						next[bodyIndex], len(bodies[bodyIndex]))
				}
				next[bodyIndex] = span[1]
			}
			if !segment.change {
				anchor := segment.spans[0]
				want := bodies[0][anchor[0]:anchor[1]]
				for offset, span := range segment.spans[1:] {
					bodyIndex := offset + 1
					if !bytes.Equal(
						want, bodies[bodyIndex][span[0]:span[1]],
					) {
						t.Fatalf("segment %d anchor differs in body %d",
							segmentIndex, bodyIndex)
					}
				}
			}
		}
		for bodyIndex, end := range next {
			if end != len(bodies[bodyIndex]) {
				t.Fatalf("body %d coverage ends at %d, want %d",
					bodyIndex, end, len(bodies[bodyIndex]))
			}
		}
	})
}

// TestBodyDigestsCollision checks that a difference buried between two shared
// runs still tells two bodies apart: a third body pushes the shared prefix and
// suffix past where the first two differ, so only the interior anchors expose
// it, and the two digests must not render as the same line.
func TestBodyDigestsCollision(t *testing.T) {
	pad := strings.Repeat
	got := bodyDigests(bytesOf(
		"P1"+pad("L", 80)+"MMMM"+pad("R", 80)+"S9",
		"P1"+pad("L", 80)+"NNNN"+pad("R", 80)+"S9",
		"P2"+pad("L", 80)+"MMMM"+pad("R", 80)+"S8",
	))
	if got[0] == "" || got[1] == "" {
		t.Fatalf("digests should be non-empty: %q, %q", got[0], got[1])
	}
	if got[0] == got[1] {
		t.Errorf("bodies 0 and 1 differ but digests collide: %q", got[0])
	}
	if !strings.Contains(got[0], "MMMM") || !strings.Contains(got[1], "NNNN") {
		t.Errorf("each digest should surface its distinguishing content: %q / %q", got[0], got[1])
	}
}

// TestBodyDigestsCountMarker checks that when changes do not all fit, the
// digest stops and reports the number dropped with correct grammar.
func TestBodyDigestsCountMarker(t *testing.T) {
	t.Run("plural", func(t *testing.T) {
		v := strings.Repeat
		got := bodyDigests(bytesOf(
			`{"a":"`+v("A", 10)+`","b":"`+v("A", 10)+`","c":"`+v("A", 10)+`","d":"`+v("A", 10)+`","e":"`+v("A", 10)+`"}`,
			`{"a":"`+v("Z", 10)+`","b":"`+v("Z", 10)+`","c":"`+v("Z", 10)+`","d":"`+v("Z", 10)+`","e":"`+v("Z", 10)+`"}`,
		))
		if !strings.Contains(got[0], "more differences") {
			t.Errorf("want a plural dropped-change count: %q", got[0])
		}
	})

	t.Run("singular", func(t *testing.T) {
		pad := strings.Repeat
		got := bodyDigests(bytesOf(
			pad("H", 40)+pad("M", 200)+pad("T", 40)+"1",
			pad("h", 40)+pad("M", 200)+pad("t", 40)+"2",
		))
		if strings.Contains(got[0], "more differences") ||
			!strings.Contains(got[0], "1 more difference") {
			t.Errorf("want singular '1 more difference': %q", got[0])
		}
	})
}

// TestBodyDigestsReconcile checks the reconciliation pass: two bodies that
// differ only in a budget-dropped region (a third body drags the frame so
// several earlier regions become "changes" where the pair happens to agree)
// must not render the same line - they are extended to show the region that
// tells them apart. Identical bodies, by contrast, keep the same line.
func TestBodyDigestsReconcile(t *testing.T) {
	f := func(c string) string { return strings.Repeat(c, 12) }

	t.Run("budget-collision is resolved", func(t *testing.T) {
		a := `{"f1":"` + f("A") + `","f2":"` + f("A") + `","f3":"` + f("A") + `","k":"XXX"}`
		b := `{"f1":"` + f("A") + `","f2":"` + f("A") + `","f3":"` + f("A") + `","k":"YYY"}`
		c := `{"f1":"` + f("Z") + `","f2":"` + f("Z") + `","f3":"` + f("Z") + `","k":"ZZZ"}`
		got := bodyDigests(bytesOf(a, b, c))
		if got[0] == "" || got[1] == "" {
			t.Fatalf("digests should be non-empty: %q, %q", got[0], got[1])
		}
		if got[0] == got[1] {
			t.Errorf("bodies 0,1 differ only in a dropped region but still collide: %q", got[0])
		}
		if !strings.Contains(got[0], "XXX") || !strings.Contains(got[1], "YYY") {
			t.Errorf("reconciliation should surface the distinguishing region: %q / %q", got[0], got[1])
		}
	})

	t.Run("identical bodies keep one line", func(t *testing.T) {
		got := bodyDigests(bytesOf(`{"n":1}`, `{"n":1}`, `{"n":2}`))
		if got[0] != got[1] {
			t.Errorf("identical bodies should render the same digest: %q vs %q", got[0], got[1])
		}
		if got[0] == got[2] {
			t.Errorf("different body should render a different digest: %q", got[2])
		}
	})
}

// TestBodyDigests checks the batch diff-and-condense entry point: results align
// with the input by index, a bodyless or binary slot yields "", and similar
// bodies each get a digest that elides the shared envelope and keeps what
// differs.
func TestBodyDigests(t *testing.T) {
	b := func(s string) []byte { return []byte(s) }
	pad := strings.Repeat("A", 40)

	t.Run("similar bodies: shared envelope elided, difference kept", func(t *testing.T) {
		got := bodyDigests([][]byte{
			b(`{"blob":"` + pad + `","sleep_us":10}`),
			b(`{"blob":"` + pad + `","sleep_us":20}`),
		})
		if len(got) != 2 {
			t.Fatalf("want 2 digests, got %d", len(got))
		}
		if !strings.HasPrefix(got[0], "...") || !strings.Contains(got[0], `:10}`) {
			t.Errorf(`digest[0] = %q, want the shared blob elided and ":10}" kept`, got[0])
		}
		if !strings.HasPrefix(got[1], "...") || !strings.Contains(got[1], `:20}`) {
			t.Errorf(`digest[1] = %q, want the shared blob elided and ":20}" kept`, got[1])
		}
	})

	// With no change region there is no novelty to point at, so each body shows
	// the same bounded head-and-tail view rather than nothing at all.
	t.Run("all-identical bodies fall back to an excerpt", func(t *testing.T) {
		body := `{"op":"probe","sleep_us":50}`
		got := bodyDigests([][]byte{b(body), b(body), b(body)})
		if len(got) != 3 {
			t.Fatalf("want 3 digests, got %d", len(got))
		}
		for i, d := range got {
			if d != body {
				t.Errorf("digest[%d] = %q, want the whole body as an excerpt (%q)", i, d, body)
			}
		}
	})

	t.Run("an over-long identical body is excerpted, not shown whole", func(t *testing.T) {
		long := "{" + strings.Repeat("x", maxDigest*2) + "}"
		got := bodyDigests([][]byte{b(long), b(long)})
		if len(got[0]) > maxDigest {
			t.Errorf("excerpt is %d chars, want it capped to %d: %q", len(got[0]), maxDigest, got[0])
		}
		if !strings.Contains(got[0], "..") {
			t.Errorf("excerpt = %q, want a '..' elision", got[0])
		}
	})

	t.Run("bodyless and binary slots yield empty, aligned by index", func(t *testing.T) {
		got := bodyDigests([][]byte{b("plain"), nil, {'{', 0x00, '}'}})
		if len(got) != 3 {
			t.Fatalf("want 3 digests aligned with input, got %d", len(got))
		}
		if got[0] == "" {
			t.Error("digest[0] for a printable body should not be empty")
		}
		if got[1] != "" || got[2] != "" {
			t.Errorf("bodyless/binary slots must be empty, got %q and %q", got[1], got[2])
		}
	})
}
