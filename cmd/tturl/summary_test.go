package main

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/curlblocks"
)

// summariseArgs assembles args and renders the compact (non-verbose) summary.
func summariseArgs(t *testing.T, args ...string) string {
	t.Helper()
	a := assembleArgs(t, args...)
	var b bytes.Buffer
	summarise(&b, a.groups, projectRunReferences(a.labels, 1000), false)
	return b.String()
}

// rowMatches reports whether out has a compact request row whose first three
// whitespace-separated fields are label, method, and path in order.
func rowMatches(out, label, method, path string) bool {
	for ln := range strings.SplitSeq(out, "\n") {
		if f := strings.Fields(ln); len(f) >= 3 && f[0] == label && f[1] == method && f[2] == path {
			return true
		}
	}
	return false
}

// commonLine returns the summary's "Common:" line, or "" when there is none.
func commonLine(out string) string {
	for ln := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(ln, "Common: ") {
			return ln
		}
	}
	return ""
}

// TestSummarise covers the request-summary assembly: the compact one-line
// default (label, method, path, and the body/header digest), the shared Target
// line, the plain total, and that a request rendered from an assembled plan
// reflects block-level subtraction.
func TestSummarise(t *testing.T) {
	mustReq := func(url string, b curlblocks.Block) *http.Request {
		t.Helper()
		r, err := buildRequest(t.Context(), b, url)
		if err != nil {
			t.Fatalf("buildRequest: %v", err)
		}
		return r
	}

	t.Run("single request: compact line, shared target, total", func(t *testing.T) {
		var b bytes.Buffer
		groups := []requestGroup{{
			req: mustReq("https://h/a", curlblocks.Block{}), count: 1,
		}}
		summarise(&b, groups, projectRunReferences([]string{"A"}, 1000), false)
		out := b.String()
		// One compact row (label, method, path) under a shared Target line, and the
		// pluralised total.
		if !strings.Contains(out, "Target: https://h") {
			t.Errorf("want a shared Target line:\n%s", out)
		}
		if !rowMatches(out, "A", "GET", "/a") {
			t.Errorf("want a compact 'A GET /a' row:\n%s", out)
		}
		if !strings.Contains(out, "Total: 1 request") {
			t.Errorf("want the singular total:\n%s", out)
		}
	})

	t.Run("repeated group spans first..last on one line", func(t *testing.T) {
		names := []string{"p~1", "p~2", "p~3"}
		var b bytes.Buffer
		groups := []requestGroup{{
			req: mustReq("https://h/a", curlblocks.Block{}), count: 3,
		}}
		summarise(&b, groups, projectRunReferences(names, 1000), false)
		out := b.String()
		if !rowMatches(out, "p~1..p~3", "GET", "/a") {
			t.Errorf("want a 'p~1..p~3' spanning row:\n%s", out)
		}
		if !strings.Contains(out, "Total: 3 requests") {
			t.Errorf("want pluralised total:\n%s", out)
		}
	})

	t.Run("compact projection prints complete wrapped mapping", func(t *testing.T) {
		long := strings.Repeat("abcdefghijklmnopqrstuvwxyz", 3)
		refs := projectRunReferences([]string{long}, 12)
		var b bytes.Buffer
		summarise(&b,
			[]requestGroup{{req: mustReq("https://h/a", curlblocks.Block{}), count: 1}},
			refs, false)
		out := b.String()
		if !strings.Contains(out, "References:\n  #0  ") {
			t.Errorf("summary lacks reference mapping:\n%s", out)
		}
		if strings.Count(out, long) != 0 {
			t.Errorf("long label should wrap rather than appear on one line:\n%s", out)
		}
		joined := strings.ReplaceAll(out, "\n", "")
		if !strings.Contains(strings.ReplaceAll(joined, " ", ""), long) {
			t.Errorf("wrapped mapping lost part of complete label:\n%s", out)
		}
		for line := range strings.SplitSeq(out, "\n") {
			if len(line) > textWidth {
				t.Errorf("mapping line has %d columns, want <= %d: %q",
					len(line), textWidth, line)
			}
		}
	})

	t.Run("body digest inline; shared headers ride the Common line in full", func(t *testing.T) {
		out := summariseArgs(t,
			"--oauth2-bearer", "tok", "-H", "X-Probe: 1",
			"--json", `{"op":"probe","sleep_us":50}`, "https://h/p")
		// The short JSON body is shown inline (the per-block differentiator), sized
		// and kinded. A lone request shares every header with itself, so the auth
		// default and the custom header ride the Common line unredacted rather than
		// being tokenised or counted.
		for _, want := range []string{
			"POST", "json", `{"op":"probe","sleep_us":50}`,
			"Common: ", "Authorization: Bearer tok", "X-Probe: 1",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("compact summary missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("renders block-level subtraction", func(t *testing.T) {
		a := assembleArgs(t,
			"-H", "X-Shared: 1", "--json", `{"op":"x","env":"base"}`,
			"--block", "https://h/a",
			"--block", "https://h/b", "--delete-header", "X-Shared", "--reset-body", "--json", `{"env":"b"}`)
		var b bytes.Buffer
		summarise(&b, a.groups, projectRunReferences(a.labels, 1000), true) // verbose: show full bodies
		out := b.String()

		// Block A keeps the shared header and its merged envelope; block B drops the
		// header and resets the body, so B's document is fresh (no inherited "op").
		bStart := strings.Index(out, subordinateHeading("Request B"))
		if bStart < 0 {
			t.Fatalf("no Request B section:\n%s", out)
		}
		aSection, bSection := out[:bStart], out[bStart:]
		if !strings.Contains(aSection, "x-shared: 1") {
			t.Errorf("block A lost the shared header:\n%s", aSection)
		}
		if strings.Contains(bSection, "x-shared") {
			t.Errorf("block B did not drop the deleted header:\n%s", bSection)
		}
		if !strings.Contains(bSection, `{"env":"b"}`) {
			t.Errorf("block B body is not its fresh document:\n%s", bSection)
		}
		if strings.Contains(bSection, `"op"`) {
			t.Errorf("block B body inherited the envelope despite --reset-body:\n%s", bSection)
		}
	})
}

func TestSummariseVerboseRequestRulersPreserveHeaderBodyBoundary(t *testing.T) {
	a := assembleArgs(t,
		"--block", "https://h/a", "--name", "A", "--data", "alpha",
		"--block", "https://h/b", "--name", "B", "--data", "bravo")
	var b bytes.Buffer
	summarise(&b, a.groups, projectRunReferences(a.labels, 1000), true)
	out := b.String()

	for _, test := range []struct {
		label, body string
	}{
		{"Request A", "alpha"},
		{"Request B", "bravo"},
	} {
		start := subordinateHeading(test.label) + "\n:method: POST\n"
		if !strings.Contains(out, start) {
			t.Errorf("%s ruler is not immediately followed by request headers:\n%s",
				test.label, out)
		}
		if !strings.Contains(out, "\nuser-agent: tturl/devel\n\n"+test.body+"\n") {
			t.Errorf("%s request lost its sole header/body separator:\n%s",
				test.label, out)
		}
	}
}

// TestSummariseReflectsTheWire checks the compact summary against the requests
// that actually go out. Each case is one where two materially different
// requests could otherwise fold into indistinguishable rows, which would defeat
// the point of an echo a reader is meant to trust without -v.
func TestSummariseReflectsTheWire(t *testing.T) {
	t.Run("a Host override is visible and splits the shared target", func(t *testing.T) {
		// -H 'Host: v' sets the authority (curlblocks maps it to req.Host), so the
		// two blocks go to different origins even though their URLs match.
		out := summariseArgs(t,
			"--block", "https://h/sleep", "-H", "Host: evil.example",
			"--block", "https://h/sleep")
		if strings.Contains(out, "Target: ") {
			t.Errorf("the two requests differ in authority, so there is no shared Target:\n%s", out)
		}
		if !rowMatches(out, "A", "GET", "https://evil.example/sleep") {
			t.Errorf("want A's row to carry the overridden authority:\n%s", out)
		}
		if !rowMatches(out, "B", "GET", "https://h/sleep") {
			t.Errorf("want B's row to carry the URL's own authority:\n%s", out)
		}
	})

	t.Run("Content-Length shows on a bodyless request", func(t *testing.T) {
		// Nothing else carries it there: the body summary that normally accounts
		// for it is only rendered for a request that has a body. A Content-Length
		// contradicting the real body is a deliberate probe.
		out := summariseArgs(t,
			"--block", "https://h/a", "-H", "Content-Length: 100",
			"--block", "https://h/a")
		if !strings.Contains(out, "Content-Length: 100") {
			t.Errorf("want the explicit Content-Length on the bodyless request:\n%s", out)
		}
	})

	t.Run("a body's Content-Type stays with the body summary, not the header digest", func(t *testing.T) {
		// The mirror of the case above, proving summarise derives "has a body"
		// from the real body: the JSON block's Content-Type is the body summary's
		// "json" kind, while the bodyless block's explicit one is a header.
		out := summariseArgs(t,
			"--block", "https://h/a", "--json", `{"k":1}`,
			"--block", "https://h/b", "-H", "Content-Type: application/xml")
		if !strings.Contains(out, "json") {
			t.Errorf("want the JSON body's kind from the body summary:\n%s", out)
		}
		if !strings.Contains(out, "Content-Type: application/xml") {
			t.Errorf("want the bodyless block's explicit Content-Type shown:\n%s", out)
		}
		if strings.Contains(out, "Content-Type: application/json") {
			t.Errorf("the JSON body's own Content-Type is the body summary's job:\n%s", out)
		}
	})

	t.Run("a fan row carries its full origin when the batch spans two", func(t *testing.T) {
		// With no shared Target to lift out, every row must name its own origin -
		// the fan's included, taken from its requests rather than its template.
		out := summariseArgs(t,
			"--block", "https://one/a",
			"--block", "https://two/b?x=V", "--vary", "V=1-2")
		if strings.Contains(out, "Target: ") {
			t.Errorf("the batch spans two origins, so there is no shared Target:\n%s", out)
		}
		if !rowMatches(out, "A", "GET", "https://one/a") {
			t.Errorf("want the plain row's full URL:\n%s", out)
		}
		if !rowMatches(out, "V", "GET", "https://two/b?x=V") {
			t.Errorf("want the fan row's full template URL, token in place:\n%s", out)
		}
	})

	t.Run("Common excludes a header a fan sends differently", func(t *testing.T) {
		// Common claims to hold what every request carries alike, so a fan's
		// headers have to count toward it: three of these four requests send
		// X-Probe: fanned.
		out := summariseArgs(t,
			"--block", "https://h/a", "-H", "X-Probe: plain",
			"--block", "https://h/b?x=V", "--vary", "V=1-3", "-H", "X-Probe: fanned")
		common := commonLine(out)
		if strings.Contains(common, "X-Probe") {
			t.Errorf("X-Probe varies across the batch, so it cannot ride Common: %q\n%s", common, out)
		}
		if !strings.Contains(out, "X-Probe: plain") || !strings.Contains(out, "X-Probe: fanned") {
			t.Errorf("want each row to carry its own X-Probe value:\n%s", out)
		}
	})

	t.Run("a fan varying a header claims no single value for it", func(t *testing.T) {
		// The token substitutes into the header value, so each variant sends its
		// own. One folded row cannot speak for all of them, so it shows the vary
		// source alone rather than picking a variant's value to display.
		out := summariseArgs(t, "--block", "https://h/a", "-H", "X-Tok: V", "--vary", "V=1-2")
		if !rowMatches(out, "V", "GET", "/a") {
			t.Errorf("want the folded fan row:\n%s", out)
		}
		if strings.Contains(out, "X-Tok: 1") || strings.Contains(out, "X-Tok: 2") {
			t.Errorf("no single X-Tok value speaks for the whole fan:\n%s", out)
		}
		if !strings.Contains(out, "vary 1..2 (2)") {
			t.Errorf("want the vary source, which is what says the header varies:\n%s", out)
		}
	})

	t.Run("an explicit header is not dropped as a fan's default", func(t *testing.T) {
		// User-Agent is a curlblocks default, and the fan block takes the default
		// one. The defaults rule only suppresses a default that is uniform across
		// the batch, so this block's explicit value must still appear.
		out := summariseArgs(t,
			"--block", "https://h/a", "-H", "User-Agent: CUSTOM",
			"--block", "https://h/b?x=V", "--vary", "V=1-2")
		if !strings.Contains(out, "User-Agent: CUSTOM") {
			t.Errorf("an explicitly-set User-Agent must not vanish:\n%s", out)
		}
	})
}

func TestSummariseEscapesUnsafeMethod(t *testing.T) {
	var b bytes.Buffer
	summarise(&b, []requestGroup{{
		req: &http.Request{
			Method: "G\x1b[31mT",
			URL:    &url.URL{Scheme: "https", Host: "h", Path: "/a"},
		},
		count: 1,
	}}, projectRunReferences([]string{"A"}, 1000), false)

	out := b.String()
	if strings.ContainsRune(out, '\x1b') {
		t.Fatalf("summary contains an escape byte: %q", out)
	}
	if !strings.Contains(out, "$HEX[") {
		t.Errorf("summary = %q, want escaped method", out)
	}
}

// TestSummariseFan checks a --vary fan collapses to a single row: labelled by
// the fan (its --name), keeping the token in the template path, describing the
// sweep and its size, not one line per variant and no per-variant TOKEN=value
// labels in the summary (those live in the ranking and the arrival order).
func TestSummariseFan(t *testing.T) {
	out := summariseArgs(t, "--block", "https://h/item/FUZZ", "--name", "probe", "--vary", "FUZZ=0-9")

	if !rowMatches(out, "probe", "GET", "/item/FUZZ") {
		t.Errorf("want a single fan row 'probe GET /item/FUZZ':\n%s", out)
	}
	if !strings.Contains(out, "vary 0..9 (10)") {
		t.Errorf("want the sweep description 'vary 0..9 (10)':\n%s", out)
	}
	if strings.Contains(out, "FUZZ=5") {
		t.Errorf("variants should collapse, but a per-variant label leaked:\n%s", out)
	}
	if !strings.Contains(out, "Total: 10 requests") {
		t.Errorf("want the total across all variants:\n%s", out)
	}
}

// TestSummariseMultiTokenFan covers a fan crossing more than one --vary token:
// the row is labelled by the tokens joined with "," (there being no single
// token to name it), and the source describes the combine: "x" for the
// clusterbomb cross product, " + " for pitchfork's lockstep pairing.
func TestSummariseMultiTokenFan(t *testing.T) {
	t.Run("clusterbomb crosses the sources", func(t *testing.T) {
		out := summariseArgs(t, "https://h/a?x=A&y=B",
			"--vary", "A={1,2}", "--vary", "B={x,y}", "--vary-mode", "clusterbomb")
		if !rowMatches(out, "A,B", "GET", "/a?x=A&y=B") {
			t.Errorf("want one row labelled by both tokens, template intact:\n%s", out)
		}
		// 2 x 2 values, so four requests fold into the single row.
		if !strings.Contains(out, "vary A={1,2} x B={x,y} (4)") {
			t.Errorf("want the cross-product source and count:\n%s", out)
		}
		if !strings.Contains(out, "Total: 4 requests") {
			t.Errorf("want the total across the cross product:\n%s", out)
		}
	})

	t.Run("pitchfork pairs the sources in lockstep", func(t *testing.T) {
		out := summariseArgs(t, "https://h/a?x=A&y=B",
			"--vary", "A={1,2}", "--vary", "B={x,y}", "--vary-mode", "pitchfork")
		if !strings.Contains(out, "vary A={1,2} + B={x,y} (2)") {
			t.Errorf("want the paired source and a count of 2, not 4:\n%s", out)
		}
	})
}

func TestHeaderDigests(t *testing.T) {
	// req builds a bare request carrying the given "Name: value" headers.
	req := func(headers ...string) *http.Request {
		r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://h/", nil)
		if err != nil {
			t.Fatalf("NewRequestWithContext: %v", err)
		}
		for _, h := range headers {
			name, value, _ := strings.Cut(h, ": ")
			r.Header.Add(name, value)
		}
		return r
	}
	// uaDefault marks User-Agent as a name curlblocks supplied rather than the
	// user, as it does when the Parser bakes one (see
	// curlblocks.Block.DefaultHeaderNames).
	uaDefault := map[string]bool{"user-agent": true}
	// bodyless marks n requests as carrying no body — the case for bare GETs,
	// where no body summary accounts for Content-Type or Content-Length and they
	// are therefore ordinary headers.
	bodyless := func(n int) []bool { return make([]bool, n) }

	t.Run("shared header lifts to Common; varying headers show per row", func(t *testing.T) {
		common, rows := headerDigests([]*http.Request{
			req("Baseline: Foo", "Fizz: Buzz1"),
			req("Baseline: Foo", "Fizz: Buzz2", "Exclusive: 1"),
		}, bodyless(2), nil)
		if common != "Baseline: Foo" {
			t.Errorf("common = %q, want only the identically-shared Baseline", common)
		}
		// A header differing in value shows on both rows; one present on a single
		// request shows only there. Names sort alphabetically.
		if rows[0] != "Fizz: Buzz1" {
			t.Errorf("row[0] = %q, want just the differing Fizz value", rows[0])
		}
		if rows[1] != "Exclusive: 1  Fizz: Buzz2" {
			t.Errorf("row[1] = %q, want the request-unique and differing headers", rows[1])
		}
	})

	t.Run("uniform auto-added default stays off Common; a user header rides it", func(t *testing.T) {
		common, rows := headerDigests([]*http.Request{
			req("User-Agent: "+toolName+"/v0", "X-Env: prod"),
			req("User-Agent: "+toolName+"/v0", "X-Env: prod"),
		}, bodyless(2), uaDefault)
		if strings.Contains(common, "User-Agent") {
			t.Errorf("common = %q, a uniform User-Agent is housekeeping", common)
		}
		if common != "X-Env: prod" {
			t.Errorf("common = %q, want the shared user header alone", common)
		}
		if rows[0] != "" || rows[1] != "" {
			t.Errorf("rows = %q, nothing varies between the requests", rows)
		}
	})

	t.Run("a varying auto-added default still shows, since it distinguishes", func(t *testing.T) {
		_, rows := headerDigests([]*http.Request{
			req("User-Agent: curl/8"),
			req("User-Agent: " + toolName + "/v0"),
		}, bodyless(2), uaDefault)
		if rows[0] != "User-Agent: curl/8" ||
			rows[1] != "User-Agent: "+toolName+"/v0" {
			t.Errorf("rows = %q, want each request's differing User-Agent", rows)
		}
	})

	// The body summary shows a body's kind and size, so Content-Type and
	// Content-Length are suppressed for a request that has one - and only for
	// such a request. On a bodyless request nothing else carries them, so they
	// are ordinary headers: an explicit Content-Length on a GET is a deliberate
	// probe and hiding it would misreport the batch.
	t.Run("body-tracked headers are suppressed only where a body summary shows them", func(t *testing.T) {
		common, rows := headerDigests([]*http.Request{
			req("Content-Type: application/json", "X-A: 1"),
			req("Content-Type: text/plain", "X-A: 1"),
		}, []bool{true, true}, nil)
		if strings.Contains(common+rows[0]+rows[1], "Content-Type") {
			t.Errorf("with bodies, the body summary owns Content-Type: %q / %v", common, rows)
		}
		if common != "X-A: 1" {
			t.Errorf("common = %q, want the shared X-A alone", common)
		}
	})

	t.Run("a bodyless request shows Content-Length, which nothing else carries", func(t *testing.T) {
		_, rows := headerDigests([]*http.Request{
			req("Content-Length: 100"),
			req(),
		}, bodyless(2), nil)
		if rows[0] != "Content-Length: 100" {
			t.Errorf("row[0] = %q, want the explicit Content-Length on a bodyless request", rows[0])
		}
		if rows[1] != "" {
			t.Errorf("row[1] = %q, want empty for the request without it", rows[1])
		}
	})

	t.Run("a mixed batch suppresses only the request that has the body", func(t *testing.T) {
		_, rows := headerDigests([]*http.Request{
			req("Content-Type: application/json"), // bodyless: shown
			req("Content-Type: application/json"), // has a body: the summary owns it
		}, []bool{false, true}, nil)
		if rows[0] != "Content-Type: application/json" {
			t.Errorf("row[0] = %q, want the bodyless request's Content-Type", rows[0])
		}
		if rows[1] != "" {
			t.Errorf("row[1] = %q, want it suppressed where a body summary shows it", rows[1])
		}
	})

	t.Run("a long value is capped so it cannot blow out the row", func(t *testing.T) {
		long := strings.Repeat("A", 500)
		common, _ := headerDigests([]*http.Request{req("X-Long: " + long)}, bodyless(1), nil)
		if !strings.HasPrefix(common, "X-Long: ") {
			t.Fatalf("common = %q, want the X-Long header", common)
		}
		if strings.Contains(common, long) {
			t.Errorf("common carries the full 500-char value; want it capped:\n%s", common)
		}
		if !strings.Contains(common, "..") {
			t.Errorf("common = %q, want a '..' elision marking the cap", common)
		}
	})

	t.Run("a value unsafe to inline falls back to a byte count, not a drop", func(t *testing.T) {
		_, rows := headerDigests([]*http.Request{
			req("X-Blob: \x00\x01\x02"),
			req(),
		}, bodyless(2), nil)
		if !strings.Contains(rows[0], "X-Blob: [") {
			t.Errorf("row[0] = %q, want a byte-count placeholder for the non-ASCII value", rows[0])
		}
		if rows[1] != "" {
			t.Errorf("row[1] = %q, want empty for the request without the header", rows[1])
		}
	})

	t.Run("an unsafe header name is rendered safely", func(t *testing.T) {
		first, err := http.NewRequestWithContext(
			t.Context(), http.MethodGet, "https://h/", nil)
		if err != nil {
			t.Fatal(err)
		}
		first.Header["X-\x1b[31m"] = []string{"value"}
		second, err := http.NewRequestWithContext(
			t.Context(), http.MethodGet, "https://h/", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, rows := headerDigests([]*http.Request{first, second}, bodyless(2), nil)
		for _, b := range []byte(rows[0]) {
			if b >= utf8RuneSelf || b == '\x1b' {
				t.Fatalf("header row contains unsafe byte 0x%02x: %q", b, rows[0])
			}
		}
		if !strings.Contains(rows[0], "$HEX[") {
			t.Errorf("row = %q, want escaped header name", rows[0])
		}
	})
}
