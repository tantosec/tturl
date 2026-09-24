package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/curlblocks"
)

// blockFrom parses request arguments and returns their single resolved block,
// the front-door way to build a Block with a body (its body fields are
// curlblocks-internal, set only by the parser).
func blockFrom(t *testing.T, args ...string) curlblocks.Block {
	t.Helper()
	plan, err := curlblocks.New().Parse(args)
	if err != nil {
		t.Fatalf("Parse(%q): %v", args, err)
	}
	return plan.Blocks[0]
}

func TestBuildRequest_stripsHopByHop(t *testing.T) {
	b := curlblocks.Block{
		Headers: []curlblocks.Header{
			{Name: "Connection", Value: "keep-alive"},
			{Name: "X-Keep", Value: "yes"},
		},
	}
	req, err := buildRequest(t.Context(), b, "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Connection"); got != "" {
		t.Errorf("Connection = %q, want stripped before send/display", got)
	}
	if got := req.Header.Get("X-Keep"); got != "yes" {
		t.Errorf("X-Keep = %q, want kept", got)
	}
}

// TestBuildRequest_dataAtFile exercises the real @file path end to end: the
// block carries an "@<path>" piece and buildRequest reads it via os.ReadFile
// (the fake-reader cases live in curlblocks). --data-binary keeps the file
// verbatim, so the body must equal the file's bytes.
func TestBuildRequest_dataAtFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "body.bin")
	want := "line1\nline2\n"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	b := blockFrom(t, "https://example.com/", "--data-binary", "@"+path)
	req, err := buildRequest(t.Context(), b, "https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if string(got) != want {
		t.Errorf("body = %q, want %q (file read verbatim)", got, want)
	}
}

func TestFormatRequest_showsAuthorityNotHost(t *testing.T) {
	req, err := http.NewRequestWithContext(t.Context(), "GET", "https://example.com:8443/sleep?x=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Add("X-Foo", "a")
	// A Host header in req.Header is what curlblocks does *not* produce (it sets
	// req.Host), so setting one here is the only way to exercise the suppression:
	// an h2 wire carries the authority as :authority and no Host field line.
	req.Header.Add("Host", "fabricated.example")

	out := formatRequest(req, false)

	for _, want := range []string{
		":method: GET", ":path: /sleep?x=1", ":scheme: https",
		":authority: example.com:8443", "x-foo: a",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatRequest output missing %q:\n%s", want, out)
		}
	}
	// Field names are lowercased to match the wire, so "host:" is the spelling a
	// leaked Host header would appear under.
	if strings.Contains(out, "host: ") {
		t.Errorf("formatRequest emitted a Host field line; it belongs in :authority:\n%s", out)
	}
	if strings.Contains(out, "fabricated.example") {
		t.Errorf("the Host header's value must not reach the wire view:\n%s", out)
	}
}

// TestFormatRequest_bodyElisionVsVerbose checks that a body larger than
// maxBodyDisplay is elided by default but printed in full when fullBody is set
// (the --verbose path).
func TestFormatRequest_bodyElisionVsVerbose(t *testing.T) {
	big := strings.Repeat("A", maxBodyDisplay+1)
	req, err := http.NewRequestWithContext(t.Context(), "POST", "https://example.com/", strings.NewReader(big))
	if err != nil {
		t.Fatal(err)
	}

	if out := formatRequest(req, false); strings.Contains(out, big) || !strings.Contains(out, "bytes omitted") {
		t.Errorf("default: want the body elided to a '[n bytes omitted]' placeholder, got:\n%s", out)
	}
	if out := formatRequest(req, true); !strings.Contains(out, big) || strings.Contains(out, "bytes omitted") {
		t.Errorf("verbose: want the full body, got:\n%s", out)
	}
}

func TestFormatRequestEscapesUnsafeValues(t *testing.T) {
	req, err := http.NewRequestWithContext(
		t.Context(), "POST", "https://example.com/", strings.NewReader("caf\xc3\xa9\n"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Value", "caf\xc3\xa9\x1b[31m")
	req.Header["X-\x1b[31m"] = []string{"value"}

	out := formatRequest(req, true)
	for _, b := range []byte(out) {
		if b >= utf8RuneSelf || b == '\x1b' {
			t.Fatalf("formatRequest contains unsafe byte 0x%02x:\n%s", b, out)
		}
	}
	if !strings.Contains(out, "$HEX[") {
		t.Errorf("formatRequest omitted the escaped representation:\n%s", out)
	}
}
