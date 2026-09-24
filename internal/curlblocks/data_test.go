package curlblocks

import (
	"reflect"
	"strings"
	"testing"
)

// TestDataPieces checks that the --data family parses into ordered pieces: the
// variants share one accumulator so command-line order is preserved across them
// (the shared sink), and a block's data appends to the inherited preamble
// baseline rather than replacing it.
func TestDataPieces(t *testing.T) {
	plan, err := New().Parse([]string{
		"-d", "base=1",
		"--block", "https://a",
		"--block", "https://b",
		"-d", "a", "--data-binary", "b", "--data-raw", "c", "--data-urlencode", "d", "--data-ascii", "e",
	})
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	// Block A carried no data of its own, so it has just the preamble baseline.
	if got, want := plan.Blocks[0].data, []dataPiece{{Kind: dataASCII, Spec: "base=1"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("block A data = %+v, want %+v", got, want)
	}
	// Block B's data appends to the inherited baseline; the preamble piece comes
	// first, then the block's pieces in command-line order across the variants.
	wantB := []dataPiece{
		{Kind: dataASCII, Spec: "base=1"},
		{Kind: dataASCII, Spec: "a"},
		{Kind: dataBinary, Spec: "b"},
		{Kind: dataRaw, Spec: "c"},
		{Kind: dataURLEncode, Spec: "d"},
		{Kind: dataASCII, Spec: "e"},
	}
	if got := plan.Blocks[1].data; !reflect.DeepEqual(got, wantB) {
		t.Errorf("block B data = %+v, want %+v", got, wantB)
	}
}

// TestDataInheritsAndMerges verifies that sibling blocks independently append
// their data to the shared preamble baseline.
func TestDataInheritsAndMerges(t *testing.T) {
	plan, err := New().Parse([]string{
		"--data", "a=b",
		"--block", "https://x", "--data", "c=d",
		"--block", "https://x", "--data", "c=e",
	})
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	for i, want := range []string{"a=b&c=d", "a=b&c=e"} {
		body, present, _, err := plan.Blocks[i].RequestBody()
		if err != nil || !present || string(body) != want {
			t.Errorf("block %d body = %q (present=%v, err=%v), want %q", i, body, present, err, want)
		}
	}
}

// TestBlockBody exercises materialisation of the --data family into
// request-body bytes: per-kind handling of inline data and @file, '&'-merging,
// and the --data-urlencode forms, plus the unsupported-stdin and missing-file
// errors. readFile is faked so the test touches no real filesystem.
func TestBlockBody(t *testing.T) {
	read := fakeFS(map[string]string{
		"crlf.txt":   "a\r\nb\x00\r\nc",
		"spaces.txt": "x y",
	})

	tests := []struct {
		name    string
		data    []dataPiece
		want    string
		present bool
		errSub  string
	}{
		{name: "no data", data: nil, want: "", present: false},
		{name: "inline ascii", data: []dataPiece{{Kind: dataASCII, Spec: "name=daniel"}}, want: "name=daniel", present: true},
		{
			name: "pieces merge with &",
			data: []dataPiece{
				{Kind: dataASCII, Spec: "a=1"},
				{Kind: dataASCII, Spec: "b=2"},
			},
			want:    "a=1&b=2",
			present: true,
		},
		// curl strips carriage returns, newlines and null bytes from a -d file.
		{
			name:    "ascii @file strips CR/LF/NUL",
			data:    []dataPiece{{Kind: dataASCII, Spec: "@crlf.txt"}},
			want:    "abc",
			present: true,
		},
		{
			name:    "binary @file is verbatim",
			data:    []dataPiece{{Kind: dataBinary, Spec: "@crlf.txt"}},
			want:    "a\r\nb\x00\r\nc",
			present: true,
		},
		{
			name:    "raw keeps a literal @",
			data:    []dataPiece{{Kind: dataRaw, Spec: "@crlf.txt"}},
			want:    "@crlf.txt",
			present: true,
		},
		{
			name:    "urlencode bare content",
			data:    []dataPiece{{Kind: dataURLEncode, Spec: "a b&c"}},
			want:    "a+b%26c",
			present: true,
		},
		{
			name:    "urlencode =content drops the =",
			data:    []dataPiece{{Kind: dataURLEncode, Spec: "=a b"}},
			want:    "a+b",
			present: true,
		},
		{
			name:    "urlencode name=content keeps name literal",
			data:    []dataPiece{{Kind: dataURLEncode, Spec: "msg=a b"}},
			want:    "msg=a+b",
			present: true,
		},
		{
			name:    "urlencode @file encodes file content",
			data:    []dataPiece{{Kind: dataURLEncode, Spec: "@spaces.txt"}},
			want:    "x+y",
			present: true,
		},
		{
			name:    "urlencode name@file",
			data:    []dataPiece{{Kind: dataURLEncode, Spec: "f@spaces.txt"}},
			want:    "f=x+y",
			present: true,
		},
		// A '=' anywhere wins over a '@': curl posts "a@b=c" inline (name "a@b",
		// content "c"), it does not read a file "b=c".
		{
			name:    "urlencode = wins even after an @",
			data:    []dataPiece{{Kind: dataURLEncode, Spec: "a@b=c"}},
			want:    "a@b=c",
			present: true,
		},
		{
			name:    "urlencode @ in inline content is encoded",
			data:    []dataPiece{{Kind: dataURLEncode, Spec: "x=y@z"}},
			want:    "x=y%40z",
			present: true,
		},
		{
			name: "mixed variants merge in order",
			data: []dataPiece{
				{Kind: dataASCII, Spec: "a=1"},
				{Kind: dataBinary, Spec: "@crlf.txt"},
				{Kind: dataRaw, Spec: "@x"},
			},
			want:    "a=1&a\r\nb\x00\r\nc&@x",
			present: true,
		},
		{name: "missing file errors", data: []dataPiece{{Kind: dataBinary, Spec: "@nope.txt"}}, errSub: "nope.txt"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, present, err := (Block{data: tt.data}).dataBody(read)
			if tt.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errSub) {
					t.Fatalf("Body() error = %v, want substring %q", err, tt.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("Body() unexpected error: %v", err)
			}
			if present != tt.present {
				t.Errorf("present = %v, want %v", present, tt.present)
			}
			if string(body) != tt.want {
				t.Errorf("body = %q, want %q", body, tt.want)
			}
		})
	}
}

// TestDataInlineIsVerbatim covers the inline half of the --data family: only a
// piece loaded from a file is processed, so text given on the command line goes
// out exactly as typed for every variant, CR and LF included. The file cases in
// TestBlockBody checks the other half, where -d strips them and --data-binary
// keeps them.
func TestDataInlineIsVerbatim(t *testing.T) {
	const raw = "a=1\r\nb=2\x00c"
	for _, kind := range []struct {
		name string
		kind dataKind
	}{
		{"-d/--data", dataASCII},
		{"--data-binary", dataBinary},
		{"--data-raw", dataRaw},
	} {
		t.Run(kind.name, func(t *testing.T) {
			b := Block{data: []dataPiece{{Kind: kind.kind, Spec: raw}}}
			body, present, _, err := b.RequestBody()
			if err != nil || !present {
				t.Fatalf("RequestBody: present=%v err=%v", present, err)
			}
			if string(body) != raw {
				t.Errorf("body = %q, want the spec verbatim %q", body, raw)
			}
		})
	}
}

// TestURLEncodeUnreservedSet checks --data-urlencode's encoding boundary:
// RFC 3986's unreserved set passes through, a space becomes '+' (curl's
// form-encoding, not %20), and every other byte is percent-encoded with
// upper-case hex digits.
//
// Each case is given in the "=content" form, so the whole argument is content
// and the encoder is under test. A bare argument containing '=' would
// be split into curl's name=content shape first, which TestBlockBody covers.
func TestURLEncodeUnreservedSet(t *testing.T) {
	const unreserved = "ABCXYZabcxyz0189-._~"
	tests := []struct{ in, want string }{
		{unreserved, unreserved},
		{" ", "+"},
		{"&=+?/#%", "%26%3D%2B%3F%2F%23%25"}, // reserved: every one escaped
		{"\x00\x7f\xff", "%00%7F%FF"},        // control and high bytes
	}
	for _, tt := range tests {
		b := Block{data: []dataPiece{{Kind: dataURLEncode, Spec: "=" + tt.in}}}
		body, _, _, err := b.RequestBody()
		if err != nil {
			t.Fatalf("RequestBody(%q): %v", tt.in, err)
		}
		if string(body) != tt.want {
			t.Errorf("--data-urlencode =%q = %q, want %q", tt.in, body, tt.want)
		}
	}
}
