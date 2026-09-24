package curlblocks

import (
	"reflect"
	"strings"
	"testing"
)

// TestJSONPieces checks --json pieces accumulate in command-line order: a
// block's compose on top of the shared preamble baseline (the block never
// replaces it), and an '@'-prefixed piece is recorded as a file with the '@'
// stripped.
func TestJSONPieces(t *testing.T) {
	plan, err := New().Parse([]string{
		"--json", `{"env":"base"}`,
		"--block", "https://a",
		"--block", "https://b",
		"--json", `{"env":"b"}`, "--json", "@patch.json",
	})
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	// Block A carried no --json of its own, so it has just the preamble baseline.
	wantA := []jsonPiece{{Spec: `{"env":"base"}`}}
	if got := plan.Blocks[0].json; !reflect.DeepEqual(got, wantA) {
		t.Errorf("block A json = %+v, want %+v", got, wantA)
	}
	// Block B's pieces follow the inherited baseline, preamble piece first.
	wantB := []jsonPiece{
		{Spec: `{"env":"base"}`},
		{Spec: `{"env":"b"}`},
		{Spec: "patch.json", IsFile: true},
	}
	if got := plan.Blocks[1].json; !reflect.DeepEqual(got, wantB) {
		t.Errorf("block B json = %+v, want %+v", got, wantB)
	}
}

// TestJSONParseInvalid checks that a malformed inline --json fails at parse
// time, with the spec in the message, rather than being deferred to send.
func TestJSONParseInvalid(t *testing.T) {
	_, err := New().Parse([]string{"--json", `{"a":1`, "https://x"})
	if err == nil || !strings.Contains(err.Error(), "valid JSON") {
		t.Fatalf("err = %v, want a 'valid JSON' parse error", err)
	}
}

// TestJSONBody exercises materialisation of --json pieces into the request
// body: a lone piece is verbatim, and multiple pieces fold as RFC 7386 merge
// patches (deep object merge, null-delete, wholesale array/scalar replace,
// number-literal preservation), with the object guard and the @file / @- /
// missing-file errors. readFile is faked so the test touches no real
// filesystem.
func TestJSONBody(t *testing.T) {
	read := fakeFS(map[string]string{
		"base.json":  `{"a":1,"b":2}`,
		"patch.json": `{"b":3}`,
		"bad.json":   `{oops}`,
	})

	tests := []struct {
		name   string
		json   []jsonPiece
		want   string
		errSub string
	}{
		{
			name: "lone piece is verbatim (key order and spacing preserved)",
			json: []jsonPiece{{Spec: `{"b": 2, "a": 1}`}},
			want: `{"b": 2, "a": 1}`,
		},
		{
			name: "two objects deep-merge, result keys ordered",
			json: []jsonPiece{{Spec: `{"a":1,"b":2}`}, {Spec: `{"b":3,"c":4}`}},
			want: `{"a":1,"b":3,"c":4}`,
		},
		{
			name: "null in a patch deletes a key",
			json: []jsonPiece{{Spec: `{"a":1,"b":2}`}, {Spec: `{"a":null,"c":3}`}},
			want: `{"b":2,"c":3}`,
		},
		{
			name: "nulling every inherited key resets the object",
			json: []jsonPiece{{Spec: `{"a":1,"b":2}`}, {Spec: `{"a":null,"b":null,"c":3}`}},
			want: `{"c":3}`,
		},
		{
			name: "nested object merges, nested array replaces wholesale",
			json: []jsonPiece{{Spec: `{"o":{"x":1,"y":2},"arr":[1,2]}`}, {Spec: `{"o":{"y":9},"arr":[3]}`}},
			want: `{"arr":[3],"o":{"x":1,"y":9}}`,
		},
		{
			name: "large integers keep their literal through a merge",
			json: []jsonPiece{{Spec: `{"id":10000000000000001}`}, {Spec: `{"x":1}`}},
			want: `{"id":10000000000000001,"x":1}`,
		},
		{
			name: "@file base and @file patch merge",
			json: []jsonPiece{{Spec: "base.json", IsFile: true}, {Spec: "patch.json", IsFile: true}},
			want: `{"a":1,"b":3}`,
		},
		// The not-an-object message names the kind it did get, one arm per JSON
		// type a scalar base can take.
		{
			name:   "boolean base named in the error",
			json:   []jsonPiece{{Spec: `true`}, {Spec: `{"a":1}`}},
			errSub: "got a boolean",
		},
		{
			name:   "string base named in the error",
			json:   []jsonPiece{{Spec: `"s"`}, {Spec: `{"a":1}`}},
			errSub: "got a string",
		},
		{
			name:   "number base named in the error",
			json:   []jsonPiece{{Spec: `5`}, {Spec: `{"a":1}`}},
			errSub: "got a number",
		},
		{
			name:   "null base named in the error",
			json:   []jsonPiece{{Spec: `null`}, {Spec: `{"a":1}`}},
			errSub: "got null",
		},
		{
			name:   "@file that is not valid JSON errors",
			json:   []jsonPiece{{Spec: "bad.json", IsFile: true}},
			errSub: "valid JSON",
		},
		{
			name:   "missing @file errors",
			json:   []jsonPiece{{Spec: "nope.json", IsFile: true}},
			errSub: "nope.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := (Block{json: tt.json}).jsonBody(read)
			if tt.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errSub) {
					t.Fatalf("jsonBody() error = %v, want substring %q", err, tt.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("jsonBody() unexpected error: %v", err)
			}
			if string(body) != tt.want {
				t.Errorf("body = %q, want %q", body, tt.want)
			}
		})
	}
}

// TestJSONMutualExclusion checks that --json cannot share a request with
// another body kind: -d or -F in the same scope, or a body set in a block over
// a --json inherited from the preamble.
func TestJSONMutualExclusion(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"json with -d", []string{"--json", "{}", "-d", "a=1", "https://x"}},
		{"json with -F", []string{"--json", "{}", "-F", "f=v", "https://x"}},
		{"preamble json with block -d", []string{"--json", "{}", "--block", "https://x", "-d", "a=1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New().Parse(tc.args); err == nil || !strings.Contains(err.Error(), "only one request body kind") {
				t.Fatalf("err = %v, want a body-kind conflict error", err)
			}
		})
	}
}

// TestJSONInheritsAndMerges reproduces the preamble+block scenario end to end:
// a shared envelope in the preamble is inherited verbatim by a block that adds
// nothing, and merged under a block that supplies a patch — the shape of
// varying one field across otherwise identical requests.
func TestJSONInheritsAndMerges(t *testing.T) {
	plan, err := New().Parse([]string{
		"--json", `{"op":"lookup","id":"AAAA"}`,
		"--block", "https://x",
		"--block", "https://x", "--json", `{"id":"BBBB"}`,
	})
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	// Block A inherits the preamble document verbatim; block B merges its patch
	// over the inherited base (id overridden, op kept; merged keys come out
	// ordered).
	for i, want := range []string{`{"op":"lookup","id":"AAAA"}`, `{"id":"BBBB","op":"lookup"}`} {
		body, present, _, err := plan.Blocks[i].RequestBody()
		if err != nil || !present || string(body) != want {
			t.Errorf("block %d body = %q (present=%v, err=%v), want %q", i, body, present, err, want)
		}
	}
}

// TestDecodeJSONRejectsTrailingTokens checks that a --json argument must be one
// complete document and nothing more. A stream decoder stops at the end of the
// first value, so without the trailing check `{"a":1} {"b":2}` would silently
// send only the first object — the shape a typo produces.
func TestDecodeJSONRejectsTrailingTokens(t *testing.T) {
	for _, spec := range []string{
		`{"a":1} {"b":2}`,
		`{"a":1} extra`,
		`{"a":1}]`,
		`1 2`,
	} {
		if _, err := decodeJSON(jsonPiece{Spec: spec}, []byte(spec)); err == nil {
			t.Errorf("decodeJSON(%q) = nil error, want the trailing content rejected", spec)
		}
	}
	// One complete document, with insignificant whitespace around it, is fine.
	if _, err := decodeJSON(jsonPiece{Spec: "ok"}, []byte(" {\"a\":1}\n")); err != nil {
		t.Errorf("decodeJSON of one document: %v", err)
	}
}

// TestMergePatchNonObject covers RFC 7386's rule for a patch or target that is
// not an object: merging is defined only between two objects, so anything else
// replaces wholesale rather than being descended into. Without this a later
// --json array would be silently dropped instead of taking effect.
func TestMergePatchNonObject(t *testing.T) {
	obj := map[string]any{"a": 1.0}
	tests := []struct {
		name          string
		target, patch any
		want          any
	}{
		{"a non-object target is replaced", []any{1.0, 2.0}, obj, obj},
		{"a non-object patch replaces the target", obj, []any{1.0}, []any{1.0}},
		{"a scalar patch replaces the target", obj, "s", "s"},
		{"a null patch replaces the target", obj, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergePatch(tt.target, tt.patch); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("mergePatch(%#v, %#v) = %#v, want %#v", tt.target, tt.patch, got, tt.want)
			}
		})
	}
}

// TestJSONBodyRejectsNonObjectMerge checks what a user meets, which is stricter
// than the mergePatch rule above: RFC 7386 would have a non-object silently
// replace the whole document, so a --json array under a --json object would
// discard the array with no diagnostic. Only a body actually being merged is
// held to it — a lone --json array is a perfectly good body and is sent as
// given.
func TestJSONBodyRejectsNonObjectMerge(t *testing.T) {
	tests := []struct {
		name    string
		pieces  []jsonPiece
		errSub  string
		wantOut string
	}{
		{
			name:   "an array base under a patch",
			pieces: []jsonPiece{{Spec: `[1,2]`}, {Spec: `{"a":1}`}},
			errSub: "the base --json must be a JSON object to merge, got an array",
		},
		{
			name:   "an array patch over an object",
			pieces: []jsonPiece{{Spec: `{"a":1}`}, {Spec: `[1,2]`}},
			errSub: "must be a JSON object to merge",
		},
		{
			name:    "a lone array is a body, not a merge",
			pieces:  []jsonPiece{{Spec: `[1,2]`}},
			wantOut: `[1,2]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := (Block{json: tt.pieces}).jsonBody(noFiles)
			if tt.errSub != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errSub) {
					t.Errorf("jsonBody = %v, want an error containing %q", err, tt.errSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("jsonBody: %v", err)
			}
			if string(body) != tt.wantOut {
				t.Errorf("body = %s, want %s", body, tt.wantOut)
			}
		})
	}
}
