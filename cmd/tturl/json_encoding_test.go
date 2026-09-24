package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/tth2"
)

func jsonLines(t *testing.T, data []byte) [][]byte {
	t.Helper()
	for i, b := range data {
		if b >= utf8RuneSelf {
			t.Fatalf("JSON contains non-ASCII byte 0x%02x at offset %d", b, i)
		}
	}
	var lines [][]byte
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		if !json.Valid(line) {
			t.Fatalf("invalid JSONL record: %q", line)
		}
		lines = append(lines, line)
	}
	return lines
}

const utf8RuneSelf = 0x80

type testBuildIdentity struct {
	version  string
	revision string
	tree     buildinfo.TreeState
}

func (i testBuildIdentity) Version() string                { return i.version }
func (i testBuildIdentity) Revision() string               { return i.revision }
func (i testBuildIdentity) TreeState() buildinfo.TreeState { return i.tree }

func TestMakeStructuredTool(t *testing.T) {
	tests := []struct {
		name string
		info testBuildIdentity
		want structuredTool
	}{
		{
			name: "release",
			info: testBuildIdentity{
				version: "v1.2.3", revision: "0123456789abcdef",
				tree: buildinfo.TreeClean,
			},
			want: structuredTool{
				Version:  "v1.2.3",
				Revision: wireStringPointer(new("0123456789abcdef")),
				Modified: new(false),
			},
		},
		{
			name: "dirty development",
			info: testBuildIdentity{
				version: "devel", revision: "fedcba9876543210",
				tree: buildinfo.TreeDirty,
			},
			want: structuredTool{
				Version:  "devel",
				Revision: wireStringPointer(new("fedcba9876543210")),
				Modified: new(true),
			},
		},
		{
			name: "unknown development",
			info: testBuildIdentity{version: "devel"},
			want: structuredTool{Version: "devel"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := makeStructuredTool(test.info)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("tool = %+v, want %+v", got, test.want)
			}
		})
	}
}

func recordKind(t *testing.T, line []byte) string {
	t.Helper()
	var head struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		t.Fatalf("decode record kind: %v", err)
	}
	return head.Kind
}

func TestWireStringJSONEncoding(t *testing.T) {
	t.Parallel()
	invalid := string([]byte{0xff, 0x00})
	collision := "$B64[/wA=]"
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"ASCII", "plain", "plain"},
		{"Unicode", "snowman \u2603", "snowman \u2603"},
		{
			"invalid UTF-8", invalid,
			"$B64[" + base64.StdEncoding.EncodeToString([]byte(invalid)) + "]",
		},
		{
			"reserved literal", collision,
			"$B64[" + base64.StdEncoding.EncodeToString([]byte(collision)) + "]",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotJSON, err := json.Marshal(struct {
				Value wireString `json:"value"`
			}{Value: wireString(tc.raw)})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			jsonLines(t, append(gotJSON, '\n'))
			var decoded struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(gotJSON, &decoded); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if decoded.Value != tc.want {
				t.Errorf("decoded value = %q, want %q", decoded.Value, tc.want)
			}
		})
	}

	// Explicitly encoded body data does not use the reserved scalar marker.
	gotJSON, err := json.Marshal(struct {
		Value encodedString `json:"value"`
	}{Value: encodedString(collision)})
	if err != nil {
		t.Fatalf("Marshal encoded data: %v", err)
	}
	var decoded struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(gotJSON, &decoded); err != nil {
		t.Fatalf("Unmarshal encoded data: %v", err)
	}
	if decoded.Value != collision {
		t.Errorf("encoded body data = %q, want literal %q",
			decoded.Value, collision)
	}
}

func TestJSONByteRepresentationsRoundTrip(t *testing.T) {
	t.Parallel()
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}

	raw, err := json.Marshal(wireString(string(allBytes)))
	if err != nil {
		t.Fatalf("Marshal wire string: %v", err)
	}
	var marker wireString
	if err := json.Unmarshal(raw, &marker); err != nil {
		t.Fatalf("Unmarshal wire string: %v", err)
	}
	got := []byte(marker)
	if !bytes.Equal(got, allBytes) {
		t.Error("wire string did not round-trip every byte")
	}

	body := encodeData(allBytes)
	if body.Encoding != "base64" {
		t.Fatalf("body encoding = %q, want base64", body.Encoding)
	}
	got, err = base64.StdEncoding.DecodeString(body.Data)
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if !bytes.Equal(got, allBytes) {
		t.Error("body did not round-trip every byte")
	}
}

func FuzzWireStringRoundTrip(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("ordinary ASCII"))
	f.Add([]byte("snowman \xe2\x98\x83"))
	f.Add([]byte{0xff, 0x00, 0xfe})
	f.Add([]byte("$B64[/wA=]"))

	f.Fuzz(func(t *testing.T, input []byte) {
		const maxFuzzWireString = 4096
		if len(input) > maxFuzzWireString {
			t.Skip()
		}
		raw, err := json.Marshal(wireString(string(input)))
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !json.Valid(raw) {
			t.Fatalf("invalid JSON: %q", raw)
		}
		for offset, b := range raw {
			if b >= utf8RuneSelf {
				t.Fatalf("JSON byte %d at offset %d is not ASCII", b, offset)
			}
		}
		var encoded wireString
		if err := json.Unmarshal(raw, &encoded); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		got := []byte(encoded)
		if !bytes.Equal(got, input) {
			t.Fatalf("round trip = %x, want %x", got, input)
		}
	})
}

func TestJSONDispatchTimesAreExact(t *testing.T) {
	t.Parallel()
	headersAt := time.Date(2026, 9, 8, 1, 2, 3, 456789, time.FixedZone("test", 3600))
	bodyAt := headersAt.Add(2 * time.Millisecond)
	dispatch := tth2.DispatchTimes{
		HeadersSentAt: headersAt, BodyFinalisedAt: bodyAt,
	}
	wantHeaders := wireString(headersAt.UTC().Format(time.RFC3339Nano))
	wantBody := wireString(bodyAt.UTC().Format(time.RFC3339Nano))

	projected := makeStructuredDispatch(dispatch)
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	jsonLines(t, append(encoded, '\n'))
	var restored structuredDispatch
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.HeadersSentAt == nil || *restored.HeadersSentAt != wantHeaders ||
		restored.BodyFinalisedAt == nil || *restored.BodyFinalisedAt != wantBody {
		t.Errorf("dispatch = %+v", restored)
	}
}

func TestWireHeaderMapJSONEncoding(t *testing.T) {
	t.Parallel()
	invalid := string([]byte{0xfe})
	raw, err := json.Marshal(wireHeaderMap{
		"x-two": {"a", invalid},
		"x-one": {"\u2603"},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	jsonLines(t, append(raw, '\n'))
	var decoded map[string][]string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	wantInvalid := "$B64[" +
		base64.StdEncoding.EncodeToString([]byte(invalid)) + "]"
	if got := decoded["x-two"]; len(got) != 2 ||
		got[0] != "a" || got[1] != wantInvalid {
		t.Errorf("x-two = %q, want [a %s]", got, wantInvalid)
	}
	if got := decoded["x-one"]; len(got) != 1 || got[0] != "\u2603" {
		t.Errorf("x-one = %q, want [snowman]", got)
	}
}

func TestCaptureRequestUsesWireFields(t *testing.T) {
	t.Parallel()
	body := []byte("body")
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		"https://dial.example/probe?q=1",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Host = "wire.example"
	req.Header.Add("X-Repeat", "a")
	req.Header.Add("X-Repeat", "b")
	rec, err := captureRequestRecord(req, runReference{
		ID: 4, Label: "probe", Inline: "probe",
	})
	if err != nil {
		t.Fatalf("captureRequestRecord: %v", err)
	}
	if rec.URL != "https://wire.example/probe?q=1" {
		t.Errorf("URL = %q, want effective wire URL", rec.URL)
	}
	if rec.PseudoHeaders[":authority"] != "wire.example" ||
		rec.PseudoHeaders[":method"] != http.MethodPost {
		t.Errorf("pseudo headers = %v", rec.PseudoHeaders)
	}
	if got := rec.Headers["x-repeat"]; len(got) != 2 ||
		got[0] != "a" || got[1] != "b" {
		t.Errorf("repeated header = %q, want [a b]", got)
	}
	if got := rec.Headers["content-length"]; len(got) != 1 ||
		got[0] != strconv.Itoa(len(body)) {
		t.Errorf("synthesised content-length = %q", got)
	}
	if rec.Body == nil || rec.Body.Bytes != len(body) ||
		rec.Body.Encoding != "utf-8" || rec.Body.Data != string(body) ||
		rec.Body.SHA256 != digestBase64(body) {
		t.Errorf("body = %+v", rec.Body)
	}
}

func TestDigestBase64UsesRawSHA256(t *testing.T) {
	t.Parallel()
	data := []byte("evidence")
	sum := sha256.Sum256(data)
	want := base64.StdEncoding.EncodeToString(sum[:])
	if got := digestBase64(data); got != want {
		t.Errorf("digestBase64 = %q, want %q", got, want)
	}
}

func TestWireMapsPreserveBytesAcrossEncodingCycles(t *testing.T) {
	allBytes := make([]byte, 256)
	for i := range allBytes {
		allBytes[i] = byte(i)
	}
	literal := "$B64[/wA=]"
	for _, original := range []any{
		wireStringMap{string(allBytes): literal, literal: string(allBytes)},
		wireHeaderMap{string(allBytes): {literal, string(allBytes)}, literal: {"ASCII", "snowman \u2603"}},
	} {
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		jsonLines(t, append(encoded, '\n'))
		var restored any
		switch original.(type) {
		case wireStringMap:
			restored = new(wireStringMap)
		case wireHeaderMap:
			restored = new(wireHeaderMap)
		}
		if err := json.Unmarshal(encoded, restored); err != nil {
			t.Fatal(err)
		}
		encodedAgain, err := json.Marshal(restored)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(encoded, encodedAgain) {
			t.Fatalf("encodedAgain map = %s, want %s", encodedAgain, encoded)
		}
	}
}
