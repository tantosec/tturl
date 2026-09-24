package curlblocks

import (
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

// mustSpec parses a --vary argument or fails the test.
func mustSpec(t *testing.T, arg string) VarySpec {
	t.Helper()
	spec, err := parseVarySpec(arg)
	if err != nil {
		t.Fatalf("parseVarySpec(%q): %v", arg, err)
	}
	return spec
}

// fanURLs runs Fan and returns each variant's single URL, for the common case
// where the token lives in the URL.
func fanURLs(t *testing.T, b Block) *Fan {
	t.Helper()
	fan, err := b.Fan()
	if err != nil {
		t.Fatalf("Fan: %v", err)
	}
	if fan == nil {
		t.Fatal("Fan: got nil, want a fan")
	}
	return fan
}

// bakedUA is the parser-supplied User-Agent the typed-text tests below run
// with. "agent/1.0" contains "gen", so a token can be aimed at text only the
// parser wrote.
const bakedUA = "agent/1.0"

// fanBlock parses args with bakedUA in play and returns the single block, so a
// test sees headers a real fold produced rather than hand-set ones.
func fanBlock(t *testing.T, args ...string) Block {
	t.Helper()
	plan, err := New(WithUserAgent(bakedUA)).Parse(args)
	if err != nil {
		t.Fatalf("Parse(%q): %v", args, err)
	}
	if len(plan.Blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(plan.Blocks))
	}
	return plan.Blocks[0]
}

// headerLines renders a variant's resolved headers as "Name: value" lines, so a
// mismatch names both the header and the part of it that varied.
func headerLines(hs []Header) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Name + ": " + h.Value
	}
	return out
}

// TestFanVariesOnlyTypedText checks the substitution rule: a token reaches the
// text the command line supplied and nothing curlblocks derived from it. An
// explicit -H is the user's own name and value, so both vary; a convenience
// flag's header keeps the name curlblocks chose and varies only the value the
// user gave; and a credential varies inside its encoding, the encoding being
// recomputed rather than scanned.
func TestFanVariesOnlyTypedText(t *testing.T) {
	basic := func(creds string) string {
		return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
	}
	ua := "User-Agent: " + bakedUA
	tests := []struct {
		name string
		args []string
		want []string // the variant's resolved headers, "Name: value"
	}{
		{
			name: "explicit -H varies its name and its value",
			args: []string{"https://h/x", "-H", "User-Agent: Agent", "--vary", "Agent={foo}"},
			want: []string{"User-foo: foo"},
		},
		{
			name: "-A varies its value, never the name it implies",
			args: []string{"https://h/x", "-A", "Agent", "--vary", "Agent={foo}"},
			want: []string{"User-Agent: foo"},
		},
		{
			name: "a baked default is nobody's typed text",
			args: []string{"https://h/gen", "--vary", "gen={X}"},
			want: []string{ua},
		},
		{
			name: "-u credentials vary inside the base64",
			args: []string{"https://h/x", "-u", "myuser:PASS", "--vary", "PASS={s3cret}"},
			want: []string{basic("myuser:s3cret"), ua},
		},
		{
			name: "the base64 of credentials is never itself scanned",
			args: []string{"https://h/O", "-u", "usr:pw", "--vary", "O={1}"},
			want: []string{basic("usr:pw"), ua},
		},
		{
			name: "--oauth2-bearer varies its token, not the scheme",
			args: []string{"https://h/x", "--oauth2-bearer", "TOK", "--vary", "TOK={t1}"},
			want: []string{"Authorization: Bearer t1", ua},
		},
		{
			name: "the Bearer scheme is never scanned",
			args: []string{"https://h/Bear", "--oauth2-bearer", "abc", "--vary", "Bear={x}"},
			want: []string{"Authorization: Bearer abc", ua},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fan := fanURLs(t, fanBlock(t, tt.args...))
			if len(fan.Variants) != 1 {
				t.Fatalf("got %d variants, want 1", len(fan.Variants))
			}
			// Every parsed block also carries the baked Accept baseline, last in
			// the list; no token may touch it, so it is the same in every case.
			want := append(tt.want, "Accept: "+acceptAll)
			got := headerLines(fan.Variants[0].Block.Headers)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("headers = %q, want %q", got, want)
			}
		})
	}
}

// TestFanTypoGuardIgnoresDerivedText checks the other half of the rule: because
// derived text is out of scope, a token that occurs only there still reads as
// the typo it is, rather than silently fanning identical requests.
func TestFanTypoGuardIgnoresDerivedText(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"only in a baked User-Agent", []string{"https://h/x", "--vary", "gen={X}"}},
		{"only in a derived header name", []string{"https://h/x", "-A", "Foobar", "--vary", "Agent={foo}"}},
		{"only in the base64 of -u", []string{"https://h/x", "-u", "usr:pw", "--vary", "O={1}"}},
		{"only in the Bearer scheme", []string{"https://h/x", "--oauth2-bearer", "abc", "--vary", "Bear={x}"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The guard needs no I/O, so Parse enforces it: a Plan never carries a
			// block whose token lands nowhere.
			_, err := New(WithUserAgent(bakedUA)).Parse(tt.args)
			if err == nil {
				t.Fatal("Parse: got nil error, want the token rejected as appearing nowhere")
			}
			if !strings.Contains(err.Error(), "appears nowhere") {
				t.Errorf("Parse: %v, want an 'appears nowhere' error", err)
			}
		})
	}
}

// TestFanSizeCeiling covers WithMaxFanSize: a binding whose own size passes
// the ceiling is refused, as is a clusterbomb product that passes it, while
// pitchfork -- only as long as its shortest source -- is not subject to the
// product check. A parser that sets no ceiling accepts any size.
func TestFanSizeCeiling(t *testing.T) {
	// Six lines, so a file (whose size is unknown until read) can cross a
	// ceiling of five.
	files := fakeFS(map[string]string{"six.txt": "1\n2\n3\n4\n5\n6\n"})
	fan := func(t *testing.T, maxSize int, args ...string) (*Fan, error) {
		t.Helper()
		plan, err := New(WithMaxFanSize(maxSize), WithFileReader(files)).Parse(args)
		if err != nil {
			t.Fatalf("Parse(%q): %v", args, err)
		}
		return plan.Blocks[0].Fan()
	}
	// refuse runs both phases and reports which one rejected the fan, since where
	// a size is caught is part of the contract: a size known without I/O is
	// Parse's to refuse, a file's line count Fan's.
	refuse := func(t *testing.T, maxSize int, args ...string) (phase string, err error) {
		t.Helper()
		plan, err := New(WithMaxFanSize(maxSize), WithFileReader(files)).Parse(args)
		if err != nil {
			return "Parse", err
		}
		if _, err := plan.Blocks[0].Fan(); err != nil {
			return "Fan", err
		}
		return "", nil
	}

	t.Run("a single binding over the ceiling", func(t *testing.T) {
		phase, err := refuse(t, 5, "http://h/A", "--vary", "A=1-6")
		if err == nil || !strings.Contains(err.Error(), "exceeds the maximum fan size of 5") {
			t.Fatalf("%s: %v, want a fan-size error", phase, err)
		}
		if phase != "Parse" {
			t.Errorf("refused at %s, want Parse: a range's size needs no I/O", phase)
		}
	})

	// The size a range knows at parse is checked before any value is built: were
	// it checked after, this case would enumerate a billion values first.
	t.Run("an over-large range is refused, not enumerated", func(t *testing.T) {
		phase, err := refuse(t, 5, "http://h/A", "--vary", "A=1-1000000000")
		if err == nil || !strings.Contains(err.Error(), "1000000000 values") {
			t.Fatalf("%s: %v, want the range refused by its own size", phase, err)
		}
	})

	t.Run("a file crosses the ceiling once read", func(t *testing.T) {
		phase, err := refuse(t, 5, "http://h/A", "--vary", "A=@six.txt")
		if err == nil || !strings.Contains(err.Error(), "6 values exceeds") {
			t.Fatalf("%s: %v, want the file refused by its line count", phase, err)
		}
		if phase != "Fan" {
			t.Errorf("refused at %s, want Fan: a file must be read to be counted", phase)
		}
	})

	t.Run("a clusterbomb product over the ceiling names the arithmetic", func(t *testing.T) {
		phase, err := refuse(t, 10, "http://h/A/B", "--vary", "A=1-4", "--vary", "B=1-4")
		if err == nil {
			t.Fatal("got nil error, want the product refused")
		}
		for _, want := range []string{"A=4 x B=4", "maximum fan size of 10", "pitchfork"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s error %q, want it to mention %q", phase, err, want)
			}
		}
	})

	t.Run("a product within the ceiling expands", func(t *testing.T) {
		f, err := fan(t, 10, "http://h/A/B", "--vary", "A=1-3", "--vary", "B=1-3")
		if err != nil {
			t.Fatalf("Fan: %v", err)
		}
		if len(f.Variants) != 9 {
			t.Errorf("got %d variants, want 9", len(f.Variants))
		}
	})

	// Pitchfork zips to the shortest source, so the same sources that break the
	// product ceiling are fine in lockstep -- which is what the error suggests.
	t.Run("pitchfork is not subject to the product check", func(t *testing.T) {
		f, err := fan(t, 10, "http://h/A/B", "--vary", "A=1-4", "--vary", "B=1-4",
			"--vary-mode", "pitchfork")
		if err != nil {
			t.Fatalf("Fan: %v", err)
		}
		if len(f.Variants) != 4 {
			t.Errorf("got %d variants, want 4", len(f.Variants))
		}
	})

	t.Run("no ceiling accepts what a ceiling would refuse", func(t *testing.T) {
		f, err := fan(t, 0, "http://h/A/B", "--vary", "A=1-4", "--vary", "B=1-4")
		if err != nil {
			t.Fatalf("Fan: %v", err)
		}
		if len(f.Variants) != 16 {
			t.Errorf("got %d variants, want 16", len(f.Variants))
		}
	})
}

func TestFanURL(t *testing.T) {
	b := Block{
		URLs: []string{"https://h/item/FUZZ"},
		Vary: []VarySpec{mustSpec(t, "FUZZ=1-3")},
	}
	fan := fanURLs(t, b)
	if got := fan.Tokens; len(got) != 1 || got[0] != "FUZZ" {
		t.Errorf("tokens = %v, want [FUZZ]", got)
	}
	var gotURLs, gotDisplay []string
	for _, v := range fan.Variants {
		gotURLs = append(gotURLs, v.Block.URLs[0])
		gotDisplay = append(gotDisplay, v.Values[0])
	}
	wantURLs := []string{"https://h/item/1", "https://h/item/2", "https://h/item/3"}
	if !reflect.DeepEqual(gotURLs, wantURLs) {
		t.Errorf("URLs = %v, want %v", gotURLs, wantURLs)
	}
	if want := []string{"1", "2", "3"}; !reflect.DeepEqual(gotDisplay, want) {
		t.Errorf("displays = %v, want %v", gotDisplay, want)
	}
}

func TestFanNoAliasing(t *testing.T) {
	b := Block{
		URLs:    []string{"https://h/FUZZ"},
		Headers: []Header{{Name: "X-Try", Value: "FUZZ"}},
		Vary:    []VarySpec{mustSpec(t, "FUZZ=1-2")},
	}
	fan := fanURLs(t, b)
	// The original template is untouched.
	if b.URLs[0] != "https://h/FUZZ" || b.Headers[0].Value != "FUZZ" {
		t.Errorf("original block mutated: %q %q", b.URLs[0], b.Headers[0].Value)
	}
	// Variants do not share header storage.
	fan.Variants[0].Block.Headers[0].Value = "clobbered"
	if fan.Variants[1].Block.Headers[0].Value != "2" {
		t.Errorf("variants alias header storage: %q", fan.Variants[1].Block.Headers[0].Value)
	}
}

func TestFanJSONBodyAndHeader(t *testing.T) {
	b := Block{
		URLs:    []string{"https://h/login"},
		Headers: []Header{{Name: "X-Trace", Value: "req-FUZZ"}},
		json:    []jsonPiece{{Spec: `{"user":"FUZZ"}`}},
		Vary:    []VarySpec{mustSpec(t, "FUZZ={admin,root}")},
	}
	fan := fanURLs(t, b)
	if got := fan.Variants[0].Block.json[0].Spec; got != `{"user":"admin"}` {
		t.Errorf("json[0] = %q", got)
	}
	if got := fan.Variants[1].Block.json[0].Spec; got != `{"user":"root"}` {
		t.Errorf("json[1] = %q", got)
	}
	if got := fan.Variants[1].Block.Headers[0].Value; got != "req-root" {
		t.Errorf("header[1] = %q", got)
	}
}

func TestFanEncoderSubstitutesEncoded(t *testing.T) {
	// The wire carries the encoded value; the display stays plaintext.
	b := Block{
		URLs: []string{"https://h/x"},
		json: []jsonPiece{{Spec: `{"u":"FUZZ"}`}},
		Vary: []VarySpec{mustSpec(t, "FUZZ:b64={admin}")},
	}
	fan := fanURLs(t, b)
	if got := fan.Variants[0].Block.json[0].Spec; got != `{"u":"YWRtaW4="}` {
		t.Errorf("json = %q, want encoded", got)
	}
	if got := fan.Variants[0].Values[0]; got != "admin" {
		t.Errorf("display = %q, want admin", got)
	}
}

func TestFanNotFanned(t *testing.T) {
	fan, err := Block{URLs: []string{"https://h/x"}}.Fan()
	if err != nil {
		t.Fatalf("Fan: %v", err)
	}
	if fan != nil {
		t.Errorf("want nil fan for a block with no --vary, got %+v", fan)
	}
}

// TestParseRejectsVaryFaults covers the other half of TestFanErrors: none of
// these faults needs I/O to spot, so Parse refuses the command line and the
// consumer never holds a Plan whose blocks would fail to fan. The block prefix
// names which block was at fault.
func TestParseRejectsVaryFaults(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "token absent",
			args: []string{"https://h/static", "--vary", "FUZZ=1-3"},
			want: "appears nowhere",
		},
		{
			name: "multiple URLs",
			args: []string{"https://h/FUZZ", "https://h/other/FUZZ", "--vary", "FUZZ=1-2"},
			want: "single URL",
		},
		{
			name: "prefix-overlapping tokens",
			args: []string{"https://h/USER/USERID", "--vary", "USER={a}", "--vary", "USERID={b}"},
			want: "overlap",
		},
		{
			name: "named in a block",
			args: []string{"--block", "https://h/static", "--vary", "FUZZ=1-3"},
			want: "block 1: --vary FUZZ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().Parse(tt.args)
			if err == nil {
				t.Fatal("Parse: want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse error %q does not contain %q", err, tt.want)
			}
		})
	}
}

// TestFanErrors checks that Fan applies the same rules to a block built by
// hand, which never passed through Parse (see TestParseRejectsVaryFaults).
func TestFanErrors(t *testing.T) {
	tests := []struct {
		name string
		b    Block
		want string
	}{
		{
			name: "token absent",
			b: Block{
				URLs: []string{"https://h/static"},
				Vary: []VarySpec{mustSpec(t, "FUZZ=1-3")},
			},
			want: "appears nowhere",
		},
		{
			name: "multiple URLs",
			b: Block{
				URLs: []string{"https://h/FUZZ", "https://h/other/FUZZ"},
				Vary: []VarySpec{mustSpec(t, "FUZZ=1-2")},
			},
			want: "single URL",
		},
		{
			name: "prefix-overlapping tokens",
			b: Block{
				URLs: []string{"https://h/USER/USERID"},
				Vary: []VarySpec{mustSpec(t, "USER={a}"), mustSpec(t, "USERID={b}")},
			},
			want: "overlap",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.b.Fan()
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestParseVaryFlag(t *testing.T) {
	// Bare-preamble fan: --vary reaches the implicit block and expands it.
	plan, err := New().Parse([]string{"https://h/item/FUZZ", "--vary", "FUZZ=1-3"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fan := fanURLs(t, plan.Blocks[0])
	if len(fan.Tokens) != 1 || fan.Tokens[0] != "FUZZ" || fan.Source != "1..3" || len(fan.Variants) != 3 {
		t.Errorf("fan = {tokens:%v source:%q n:%d}, want {[FUZZ] 1..3 3}",
			fan.Tokens, fan.Source, len(fan.Variants))
	}
}

func TestParseVaryBlockOverridesPreamble(t *testing.T) {
	// A block's --vary for the same token overrides the inherited one.
	plan, err := New().Parse([]string{
		"--vary", "FUZZ=1-3",
		"--block", "https://h/FUZZ", "--vary", "FUZZ={x,y}",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fan := fanURLs(t, plan.Blocks[0])
	if got := []string{fan.Variants[0].Values[0], fan.Variants[1].Values[0]}; len(fan.Variants) != 2 ||
		got[0] != "x" || got[1] != "y" {
		t.Errorf("variants = %+v, want the block's {x,y}", fan.Variants)
	}
}

// TestParseVaryLastWinsWithinScope checks the fold within a single scope: two
// --vary for the same token in one block leave only the later binding, the same
// last-wins rule a block applies over an inherited binding.
func TestParseVaryLastWinsWithinScope(t *testing.T) {
	plan, err := New().Parse([]string{"https://h/FUZZ", "--vary", "FUZZ=1-3", "--vary", "FUZZ={a,b}"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := plan.Blocks[0].Vary; len(got) != 1 {
		t.Fatalf("Vary = %+v, want one binding after the fold", got)
	}
	if got := fanURLs(t, plan.Blocks[0]); !reflect.DeepEqual(
		[]string{got.Variants[0].Block.URLs[0], got.Variants[1].Block.URLs[0]},
		[]string{"https://h/a", "https://h/b"},
	) || len(got.Variants) != 2 {
		t.Errorf("variants = %+v, want the later {a,b}", got.Variants)
	}
}

// TestFanBatteringRam checks that one token appearing several times in a
// request is filled alike by a single value, not fanned per occurrence.
func TestFanBatteringRam(t *testing.T) {
	b := Block{
		URLs:    []string{"https://h/FUZZ/again/FUZZ"},
		Headers: []Header{{Name: "X-Echo", Value: "FUZZ"}},
		Vary:    []VarySpec{mustSpec(t, "FUZZ={a,b}")},
	}
	fan, err := b.Fan()
	if err != nil {
		t.Fatalf("Fan: %v", err)
	}
	if len(fan.Variants) != 2 {
		t.Fatalf("variants = %d, want 2 (one per value, not per occurrence)", len(fan.Variants))
	}
	for i, want := range []string{"a", "b"} {
		v := fan.Variants[i]
		if got := v.Block.URLs[0]; got != "https://h/"+want+"/again/"+want {
			t.Errorf("variant %d URL = %q, want both occurrences filled with %q", i, got, want)
		}
		if got := v.Block.Headers[0].Value; got != want {
			t.Errorf("variant %d header = %q, want %q", i, got, want)
		}
	}
}

func TestParseVaryModeAndErrors(t *testing.T) {
	// --vary-mode reaches the block through Parse.
	plan, err := New().Parse([]string{
		"https://h/A/B", "--vary", "A={1,2}", "--vary", "B={x,y}", "--vary-mode", "pitchfork",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if plan.Blocks[0].VaryMode != VaryPitchfork {
		t.Errorf("VaryMode = %v, want VaryPitchfork", plan.Blocks[0].VaryMode)
	}

	// An unknown --vary-mode is reported by Parse.
	if _, err := New().Parse([]string{"https://h/x", "--vary-mode", "bogus"}); err == nil ||
		!strings.Contains(err.Error(), "clusterbomb or pitchfork") {
		t.Errorf("bad --vary-mode error = %v, want clusterbomb/pitchfork guidance", err)
	}

	// A malformed --vary spec surfaces from Parse, not only from Fan.
	if _, err := New().Parse([]string{"https://h/FUZZ", "--vary", "FUZZ"}); err == nil ||
		!strings.Contains(err.Error(), "NAME=SOURCE") {
		t.Errorf("bad --vary error = %v, want NAME=SOURCE guidance", err)
	}
}

func TestFanSubstitutesEveryField(t *testing.T) {
	// substitute rewrites every substitutable field, not just the URL. Fan does
	// not enforce body mutual-exclusion (toBlock does), so the block sets several
	// body kinds purely to exercise each substitution path.
	b := Block{
		URLs:    []string{"https://h/FUZZ"},
		Headers: []Header{{Name: "X-FUZZ", Value: "h-FUZZ"}},
		Cookies: []Cookie{{Name: "c-FUZZ", Value: "v-FUZZ"}},
		Method:  "FUZZ",
		data:    []dataPiece{{Kind: dataRaw, Spec: "d=FUZZ"}},
		json:    []jsonPiece{{Spec: `{"j":"FUZZ"}`}},
		form: []formPart{{
			Name: "n-FUZZ", Kind: formLiteral, Value: "fv-FUZZ",
			FileName: "fn-FUZZ", Type: "type/FUZZ",
		}},
		Vary: []VarySpec{mustSpec(t, "FUZZ={z}")},
	}
	fan := fanURLs(t, b)
	v := fan.Variants[0].Block
	checks := []struct {
		field, got, want string
	}{
		{"url", v.URLs[0], "https://h/z"},
		{"header name", v.Headers[0].Name, "X-z"},
		{"header value", v.Headers[0].Value, "h-z"},
		{"cookie name", v.Cookies[0].Name, "c-z"},
		{"cookie value", v.Cookies[0].Value, "v-z"},
		{"method", v.Method, "z"},
		{"data spec", v.data[0].Spec, "d=z"},
		{"json spec", v.json[0].Spec, `{"j":"z"}`},
		{"form name", v.form[0].Name, "n-z"},
		{"form value", v.form[0].Value, "fv-z"},
		{"form filename", v.form[0].FileName, "fn-z"},
		{"form type", v.form[0].Type, "type/z"},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
}

// TestBlockMentionsEveryField is the parity guard for mapStrings: a token
// unique to each substitutable field must be seen by mentions, so mentions
// enumerates the same complete field set substitute rewrites (checked by
// TestFanSubstitutesEveryField). The two share mapStrings, so a field added to
// one can never be missed by the other; this catches a field dropped from the
// shared walker itself. A token in no field is not mentioned.
func TestBlockMentionsEveryField(t *testing.T) {
	b := Block{
		URLs:              []string{"https://h/URLTOK"},
		Headers:           []Header{{Name: "HNAMETOK", Value: "HVALTOK"}},
		SuppressedHeaders: []string{"SUPPRESSTOK"},
		Cookies:           []Cookie{{Name: "CNAMETOK", Value: "CVALTOK"}},
		Method:            "METHODTOK",
		data:              []dataPiece{{Kind: dataRaw, Spec: "DATATOK"}},
		json:              []jsonPiece{{Spec: `{"j":"JSONTOK"}`}},
		form: []formPart{{
			Name: "FNAMETOK", Kind: formLiteral, Value: "FVALTOK",
			FileName: "FFILETOK", Type: "FTYPETOK",
		}},
	}
	for _, tok := range []string{
		"URLTOK", "HNAMETOK", "HVALTOK", "CNAMETOK", "CVALTOK", "METHODTOK",
		"DATATOK", "JSONTOK", "FNAMETOK", "FVALTOK", "FFILETOK", "FTYPETOK",
		"SUPPRESSTOK",
	} {
		if !b.mentions(tok) {
			t.Errorf("mentions(%q) = false; a substitutable field is missing from mapStrings", tok)
		}
	}
	if b.mentions("ABSENT") {
		t.Error("mentions reported a token that appears in no field")
	}
}

func TestFanFileReadError(t *testing.T) {
	b := Block{
		URLs:     []string{"https://h/FUZZ"},
		Vary:     []VarySpec{mustSpec(t, "FUZZ=@missing.txt")},
		readFile: noFiles, // no real filesystem: the read must fail from here
	}
	_, err := b.Fan()
	if err == nil || !strings.Contains(err.Error(), "missing.txt") {
		t.Fatalf("Fan error = %v, want one naming missing.txt", err)
	}
}

// TestFanSourceElidesLongList checks the description a fan reports for a
// literal list longer than varySourceDescMax: the first varySourceDescMax
// characters of the joined items, then "...", inside the braces.
func TestFanSourceElidesLongList(t *testing.T) {
	items := []string{
		"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
	}
	joined := strings.Join(items, ",") // 51 chars, over the 40-char ceiling
	b := Block{
		URLs: []string{"https://h/FUZZ"},
		Vary: []VarySpec{mustSpec(t, "FUZZ={"+joined+"}")},
	}
	fan, err := b.Fan()
	if err != nil {
		t.Fatalf("Fan: %v", err)
	}
	want := "{" + joined[:varySourceDescMax] + "...}"
	if fan.Source != want {
		t.Errorf("source = %q, want %q", fan.Source, want)
	}
	if len(fan.Variants) != len(items) {
		t.Errorf("variants = %d, want %d (eliding the description drops no value)",
			len(fan.Variants), len(items))
	}
}

func TestFanClusterBomb(t *testing.T) {
	// Two tokens with the default mode take the Cartesian product, in odometer
	// order (the last token varying fastest), each substituted where it appears.
	b := Block{
		URLs: []string{"https://h/u/A/p/B"},
		Vary: []VarySpec{mustSpec(t, "A=1-2"), mustSpec(t, "B={x,y}")},
	}
	fan := fanURLs(t, b)
	if want := []string{"A", "B"}; !reflect.DeepEqual(fan.Tokens, want) {
		t.Errorf("tokens = %v, want %v", fan.Tokens, want)
	}
	var urls []string
	var vals [][]string
	for _, v := range fan.Variants {
		urls = append(urls, v.Block.URLs[0])
		vals = append(vals, v.Values)
	}
	wantURLs := []string{
		"https://h/u/1/p/x", "https://h/u/1/p/y",
		"https://h/u/2/p/x", "https://h/u/2/p/y",
	}
	if !reflect.DeepEqual(urls, wantURLs) {
		t.Errorf("urls = %v, want %v", urls, wantURLs)
	}
	if want := [][]string{{"1", "x"}, {"1", "y"}, {"2", "x"}, {"2", "y"}}; !reflect.DeepEqual(vals, want) {
		t.Errorf("values = %v, want %v", vals, want)
	}
	if fan.Ragged {
		t.Error("cluster bomb is never ragged")
	}
	if want := "A=1..2 x B={x,y}"; fan.Source != want {
		t.Errorf("source = %q, want %q", fan.Source, want)
	}
}

func TestFanPitchfork(t *testing.T) {
	// Pitchfork advances the tokens in lockstep, truncating to the shortest and
	// flagging only unequal source lengths.
	b := Block{
		URLs:     []string{"https://h/u/USER/p/PASS"},
		Vary:     []VarySpec{mustSpec(t, "USER={a,b,c}"), mustSpec(t, "PASS={x,y}")},
		VaryMode: VaryPitchfork,
	}
	fan := fanURLs(t, b)
	var urls []string
	for _, v := range fan.Variants {
		urls = append(urls, v.Block.URLs[0])
	}
	if want := []string{"https://h/u/a/p/x", "https://h/u/b/p/y"}; !reflect.DeepEqual(urls, want) {
		t.Errorf("urls = %v, want %v", urls, want)
	}
	if !fan.Ragged {
		t.Error("unequal pitchfork sources should be flagged ragged")
	}
	if want := "USER={a,b,c} + PASS={x,y}"; fan.Source != want {
		t.Errorf("source = %q, want the + operator", fan.Source)
	}

	equal := Block{
		URLs:     []string{"https://h/USER/PASS"},
		Vary:     []VarySpec{mustSpec(t, "USER={a,b}"), mustSpec(t, "PASS={x,y}")},
		VaryMode: VaryPitchfork,
	}
	equalFan := fanURLs(t, equal)
	if equalFan.Ragged {
		t.Error("equal-length pitchfork should not be ragged")
	}
	if len(equalFan.Variants) != 2 {
		t.Errorf("equal-length variants = %d, want 2",
			len(equalFan.Variants))
	}
}

func TestFanSimultaneousSubstitution(t *testing.T) {
	// Tokens substitute in a single pass: a value that happens to contain
	// another token's name is not rescanned as that token.
	b := Block{
		URLs: []string{"https://h/AA/BB"},
		Vary: []VarySpec{mustSpec(t, "AA={BB}"), mustSpec(t, "BB={z}")},
	}
	fan := fanURLs(t, b)
	if len(fan.Variants) != 1 {
		t.Fatalf("variants = %d, want 1", len(fan.Variants))
	}
	if got := fan.Variants[0].Block.URLs[0]; got != "https://h/BB/z" {
		t.Errorf("url = %q, want https://h/BB/z (AA->BB kept literal, not rescanned to z)", got)
	}
}

// TestFanFillsSchemePerVariant checks the ordering between scheme filling and
// --vary substitution. A fanned block's URL is a template, so a token may
// supply the scheme or the whole URL; filling it at parse would prepend to text
// that does not begin the final URL. The template therefore reads back as
// written and each variant is filled once substitution has made it a URL.
func TestFanFillsSchemePerVariant(t *testing.T) {
	variantURLs := func(t *testing.T, args ...string) (urls []string, stored, display string) {
		t.Helper()
		plan, err := New(WithDefaultScheme("https")).Parse(args)
		if err != nil {
			t.Fatalf("Parse(%q): %v", args, err)
		}
		fan, err := plan.Blocks[0].Fan()
		if err != nil {
			t.Fatalf("Fan: %v", err)
		}
		urls = make([]string, len(fan.Variants))
		for i, v := range fan.Variants {
			urls[i] = v.Block.URLs[0]
		}
		return urls, plan.Blocks[0].URLs[0], fan.Template
	}

	// A token spanning the whole URL: each value carries its own scheme, so no
	// default is filled and the value survives intact.
	got, tmpl, _ := variantURLs(t, "TARGET", "--vary", "TARGET={http://a.example/x,https://b.example/y}")
	if want := []string{"http://a.example/x", "https://b.example/y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("whole-URL token: variants = %v, want %v", got, want)
	}
	if tmpl != "TARGET" {
		t.Errorf("whole-URL token: template = %q, want it as written", tmpl)
	}

	// A schemeless fanned URL is still filled — once per variant.
	got, tmpl, display := variantURLs(t, "h/item/FUZZ", "--vary", "FUZZ={1,2}")
	if want := []string{"https://h/item/1", "https://h/item/2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("schemeless template: variants = %v, want %v", got, want)
	}
	if tmpl != "h/item/FUZZ" {
		t.Errorf("schemeless template = %q, want it as written", tmpl)
	}
	// Fan.Template is the display form: tokens in place, scheme filled.
	if display != "https://h/item/FUZZ" {
		t.Errorf("Fan.Template = %q, want the filled template", display)
	}

	// A token supplying only the scheme: filling would have prepended to it.
	got, _, _ = variantURLs(t, "SCHEME://h/p", "--vary", "SCHEME={http,https}")
	if want := []string{"http://h/p", "https://h/p"}; !reflect.DeepEqual(got, want) {
		t.Errorf("scheme token: variants = %v, want %v", got, want)
	}

	// An unfanned block is unaffected: its URL is a URL, so the parser fills it.
	plan, err := New(WithDefaultScheme("https")).Parse([]string{"h/plain"})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Blocks[0].URLs[0]; got != "https://h/plain" {
		t.Errorf("unfanned URL = %q, want it filled at parse", got)
	}
}

// TestFanRejectsUnusableValues covers the three ways a binding can name values
// that cannot produce requests, each refused before anything is built: a range
// endpoint carrying a sign (which the ascending check would otherwise report as
// the wrong fault), two bindings on one token (which the parser folds away but
// a hand-built block can hold), and a value that leaves no URL to request.
func TestFanRejectsUnusableValues(t *testing.T) {
	specs := []struct {
		spec string
		want string
	}{
		{"F=5--1", "unsigned"},
		{"F=1-+3", "unsigned"},
		// A signed hex endpoint fails its base check first, which is the earlier and
		// more specific diagnosis of the same typo.
		{"F=0x00--1", "mixes hex and decimal"},
	}
	for _, tt := range specs {
		_, err := New().Parse([]string{"https://h/?q=F", "--vary", tt.spec})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("--vary %s error = %v, want one mentioning %q", tt.spec, err, tt.want)
		}
	}
	// An ascending unsigned range is still fine, so the check refuses only signs.
	if _, err := New().Parse([]string{"https://h/?q=F", "--vary", "F=1-3"}); err != nil {
		t.Errorf("--vary F=1-3: %v", err)
	}

	dup := Block{URLs: []string{"https://h/?q=F"}, Vary: []VarySpec{{Token: "F"}, {Token: "F"}}}
	if _, err := dup.Fan(); err == nil || !strings.Contains(err.Error(), "bound twice") {
		t.Errorf("two bindings on one token = %v, want a duplicate-token error", err)
	}

	plan, err := New().Parse([]string{"TARGET", "--vary", "TARGET={https://a/,}"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := plan.Blocks[0].Fan(); err == nil || !strings.Contains(err.Error(), "requests nothing") {
		t.Errorf("an empty whole-URL value = %v, want a no-URL error", err)
	}
}
