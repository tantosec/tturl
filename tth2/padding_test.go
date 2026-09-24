package tth2

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func mustBodyReq(tb testing.TB, contentType, body string) *http.Request {
	tb.Helper()
	r, err := http.NewRequestWithContext(tb.Context(), http.MethodPost,
		"https://127.0.0.1:8443/", strings.NewReader(body))
	if err != nil {
		tb.Fatalf("NewRequest: %v", err)
	}
	r.Header.Set("Content-Type", contentType)
	return r
}

func bodyOf(tb testing.TB, r *http.Request) string {
	tb.Helper()
	if r.GetBody == nil {
		tb.Fatal("GetBody is nil")
	}
	rc, err := r.GetBody()
	if err != nil {
		tb.Fatalf("GetBody: %v", err)
	}
	b, err := io.ReadAll(rc)
	if err != nil {
		tb.Fatalf("ReadAll: %v", err)
	}
	if err := rc.Close(); err != nil {
		tb.Fatalf("Close: %v", err)
	}
	return string(b)
}

// TestRequestPaddingQueryAndHeaders checks exact insertion, clone ownership, and
// the zero-padding identity contract at the padding boundary.
func TestRequestPaddingQueryAndHeaders(t *testing.T) {
	t.Parallel()
	orig := mustReq(t, "https://host/path?real=1")
	orig.Header.Set("X-Existing", "kept")

	zero, err := padRequest(orig, RequestPadding{})
	if err != nil {
		t.Fatalf("zero padRequest: %v", err)
	}
	if zero != orig {
		t.Error("zero padding returned a clone, want the original request")
	}

	got, err := padRequest(orig, RequestPadding{URLParams: 2, Headers: 2})
	if err != nil {
		t.Fatalf("padRequest: %v", err)
	}
	if got == orig || got.URL == orig.URL {
		t.Fatal("non-zero padding did not clone the request and URL")
	}
	if got.URL.RawQuery != "_pad1=x&_pad2=x&real=1" {
		t.Errorf("RawQuery = %q, want exact prepended parameters", got.URL.RawQuery)
	}
	for _, name := range []string{"X-Pad-1", "X-Pad-2"} {
		if values := got.Header.Values(name); !slices.Equal(values, []string{"x"}) {
			t.Errorf("%s = %v, want [x]", name, values)
		}
	}
	if got.Header.Get("X-Existing") != "kept" {
		t.Error("existing header was not preserved")
	}
	got.Header.Set("X-Existing", "clone")
	if orig.URL.RawQuery != "real=1" || orig.Header.Get("X-Existing") != "kept" ||
		orig.Header.Get("X-Pad-1") != "" {
		t.Errorf("original request mutated: URL=%q Header=%v", orig.URL.RawQuery, orig.Header)
	}
}

// TestRequestPaddingRejectsNegativeCounts makes the count domain explicit. A
// negative field cannot mean padding and must not silently become a cloned
// no-op.
func TestRequestPaddingRejectsNegativeCounts(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pad  RequestPadding
	}{
		{"URLParams", RequestPadding{URLParams: -1}},
		{"Headers", RequestPadding{Headers: -1}},
		{"BodyParams", RequestPadding{BodyParams: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			orig := mustReq(t, "https://host/?real=1")
			got, err := padRequest(orig, tc.pad)
			if err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("padRequest = (%v, %v), want nil and a %s error", got, err, tc.name)
			}
			if got != nil || orig.URL.RawQuery != "real=1" {
				t.Errorf("failed padding returned %v or mutated original query %q", got, orig.URL.RawQuery)
			}
		})
	}
}

// TestRequestPaddingBodyFormats checks each documented grammar through an
// independent parser and verifies that the resulting body remains replayable.
func TestRequestPaddingBodyFormats(t *testing.T) {
	t.Parallel()
	t.Run("JSON", func(t *testing.T) {
		t.Parallel()
		checkPaddedBody(t, "application/json", `{"real":"value"}`,
			`{"_pad1":"x","_pad2":"x","real":"value"}`)
	})
	t.Run("form", func(t *testing.T) {
		t.Parallel()
		checkPaddedBody(t, "application/x-www-form-urlencoded", "real=1&other=2",
			"_pad1=x&_pad2=x&real=1&other=2")
	})
	t.Run("multipart", func(t *testing.T) {
		t.Parallel()
		const boundary = "testboundary"
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		if err := w.SetBoundary(boundary); err != nil {
			t.Fatalf("SetBoundary: %v", err)
		}
		if err := w.WriteField("real", "value"); err != nil {
			t.Fatalf("WriteField: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close multipart writer: %v", err)
		}
		orig := mustBodyReq(t, w.FormDataContentType(), buf.String())
		padded, err := padRequest(orig, RequestPadding{BodyParams: 2})
		if err != nil {
			t.Fatalf("padRequest: %v", err)
		}
		mr := multipart.NewReader(strings.NewReader(bodyOf(t, padded)), boundary)
		var names, values []string
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("NextPart: %v", err)
			}
			value, err := io.ReadAll(part)
			if err != nil {
				t.Fatalf("ReadAll(%s): %v", part.FormName(), err)
			}
			names = append(names, part.FormName())
			values = append(values, string(value))
		}
		if !slices.Equal(names, []string{"_pad1", "_pad2", "real"}) ||
			!slices.Equal(values, []string{"x", "x", "value"}) {
			t.Errorf("multipart fields = %v/%v, want ordered padding then real field", names, values)
		}
		checkBodyMetadata(t, orig, padded, buf.String())
	})
}

func checkPaddedBody(tb testing.TB, contentType, original, want string) {
	tb.Helper()
	orig := mustBodyReq(tb, contentType, original)
	padded, err := padRequest(orig, RequestPadding{BodyParams: 2})
	if err != nil {
		tb.Fatalf("padRequest: %v", err)
	}
	if got := bodyOf(tb, padded); got != want {
		tb.Errorf("padded body = %q, want %q", got, want)
	}
	checkBodyMetadata(tb, orig, padded, original)
}

func checkBodyMetadata(tb testing.TB, orig, padded *http.Request, original string) {
	tb.Helper()
	if got := bodyOf(tb, orig); got != original {
		tb.Errorf("original body = %q, want %q", got, original)
	}
	first, second := bodyOf(tb, padded), bodyOf(tb, padded)
	if first != second {
		tb.Errorf("GetBody is not replayable: %q then %q", first, second)
	}
	if padded.ContentLength != int64(len(first)) {
		tb.Errorf("ContentLength = %d, want %d", padded.ContentLength, len(first))
	}
}

// TestRequestPaddingBodyErrors covers every distinct failure boundary in the body
// padding dispatcher and readers.
func TestRequestPaddingBodyErrors(t *testing.T) {
	t.Parallel()
	wantGetBodyErr := errors.New("get body failed")
	tests := []struct {
		name string
		req  func(*testing.T) *http.Request
		want string
	}{
		{"missing content type", func(t *testing.T) *http.Request {
			return mustBodyReq(t, "", "data")
		}, "Content-Type"},
		{"unsupported content type", func(t *testing.T) *http.Request {
			return mustBodyReq(t, "application/xml", "<root/>")
		}, "application/xml"},
		{"empty JSON", func(t *testing.T) *http.Request {
			return mustBodyReq(t, "application/json", " \r\n\t")
		}, "empty"},
		{"JSON array", func(t *testing.T) *http.Request {
			return mustBodyReq(t, "application/json", "[1]")
		}, "array"},
		{"JSON scalar", func(t *testing.T) *http.Request {
			return mustBodyReq(t, "application/json", "null")
		}, "not a JSON object"},
		{"multipart missing boundary", func(t *testing.T) *http.Request {
			return mustBodyReq(t, "multipart/form-data", "body")
		}, "missing boundary"},
		{"multipart boundary absent from body", func(t *testing.T) *http.Request {
			return mustBodyReq(t, "multipart/form-data; boundary=b", "body")
		}, "not found"},
		{"GetBody returns nil", func(t *testing.T) *http.Request {
			r := mustBodyReq(t, "application/json", "{}")
			r.GetBody = func() (io.ReadCloser, error) { return nil, nil }
			return r
		}, "returned nil"},
		{"GetBody returns error", func(t *testing.T) *http.Request {
			r := mustBodyReq(t, "application/json", "{}")
			r.GetBody = func() (io.ReadCloser, error) { return nil, wantGetBodyErr }
			return r
		}, wantGetBodyErr.Error()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := padRequest(tc.req(t), RequestPadding{BodyParams: 1})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("padRequest = (%v, %v), want nil and error containing %q", got, err, tc.want)
			}
			if got != nil {
				t.Errorf("padRequest returned request %v on failure", got)
			}
		})
	}
}

// TestApplyPaddingContract owns the internal position mapping: empty padding
// preserves the input slice, short and zero entries preserve request pointers,
// and an error identifies its request index.
func TestApplyPaddingContract(t *testing.T) {
	t.Parallel()
	reqs := []*http.Request{
		mustReq(t, "https://host/0"),
		mustReq(t, "https://host/1"),
		mustReq(t, "https://host/2"),
	}
	unchanged, err := applyPadding(reqs, nil)
	if err != nil {
		t.Fatalf("empty applyPadding: %v", err)
	}
	if &unchanged[0] != &reqs[0] {
		t.Error("empty padding did not preserve the input slice")
	}
	if err := validatePadding(reqs[:1], []RequestPadding{{}, {}}); err == nil {
		t.Error("validatePadding accepted more entries than requests")
	}
	if err := validatePadding(reqs, []RequestPadding{{Headers: -1}}); err == nil {
		t.Error("validatePadding accepted a negative count")
	}

	got, err := applyPadding(reqs, []RequestPadding{{URLParams: 1}, {}})
	if err != nil {
		t.Fatalf("applyPadding: %v", err)
	}
	if got[0] == reqs[0] || got[0].URL.RawQuery != "_pad1=x" {
		t.Errorf("padded request = %p/%q, want a padded clone", got[0], got[0].URL.RawQuery)
	}
	if got[1] != reqs[1] || got[2] != reqs[2] {
		t.Errorf("unpadded request pointers changed: got %p/%p, want %p/%p",
			got[1], got[2], reqs[1], reqs[2])
	}

	bad := mustBodyReq(t, "application/xml", "<root/>")
	_, err = applyPadding([]*http.Request{reqs[0], bad},
		[]RequestPadding{{}, {BodyParams: 1}})
	if err == nil || !strings.Contains(err.Error(), "request 1") {
		t.Errorf("applyPadding error = %v, want request index context", err)
	}
}

// TestBodyReadersRejectNilGetBodyResult keeps both internal body-reading paths
// aligned on the malformed factory boundary.
func TestBodyReadersRejectNilGetBodyResult(t *testing.T) {
	t.Parallel()
	r := mustBodyReq(t, "text/plain", "body")
	r.GetBody = func() (io.ReadCloser, error) { return nil, nil }
	if _, err := readBodyBytes(r); err == nil {
		t.Error("readBodyBytes accepted a nil GetBody result")
	}
	if _, err := readAllBodies([]*http.Request{r}); err == nil {
		t.Error("readAllBodies accepted a nil GetBody result")
	}
}
