package tth2

import (
	"bytes"
	"net/http"
	"slices"
	"testing"

	"golang.org/x/net/http2/hpack"
)

// encodeFields runs encodeHeaders for req and decodes the resulting HPACK block
// back into the field lines it put on the wire, in emission order.
func encodeFields(tb testing.TB, req *http.Request, bodyLen int64) []hpack.HeaderField {
	tb.Helper()
	var buf bytes.Buffer
	enc := hpack.NewEncoder(&buf)
	block, err := encodeHeaders(enc, &buf, req, bodyLen)
	if err != nil {
		tb.Fatalf("encodeHeaders: %v", err)
	}
	fields, err := hpack.NewDecoder(4096, nil).DecodeFull(block)
	if err != nil {
		tb.Fatalf("hpack decode: %v", err)
	}
	return fields
}

// fieldValues returns every value emitted for the named field, in order.
func fieldValues(fields []hpack.HeaderField, name string) []string {
	var out []string
	for _, f := range fields {
		if f.Name == name {
			out = append(out, f.Value)
		}
	}
	return out
}

// TestEncodeHeaders_namesLowercasedDuplicatesKept verifies lowercase wire names
// and separate repeated field lines.
func TestEncodeHeaders_namesLowercasedDuplicatesKept(t *testing.T) {
	req := mustReq(t, "https://example.com/")
	req.Header.Add("X-Foo", "a")
	req.Header.Add("X-Foo", "b")

	fields := encodeFields(t, req, -1)

	if got, want := fieldValues(fields, "x-foo"), []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("x-foo = %v, want %v (name lowercased on the wire, duplicates kept)", got, want)
	}
	if got := fieldValues(fields, "X-Foo"); len(got) != 0 {
		t.Errorf("canonical-case X-Foo leaked onto the wire: %v", got)
	}
}

// TestEncodeHeaders_contentLength verifies explicit, inferred, and absent
// content-length fields without duplicates.
func TestEncodeHeaders_contentLength(t *testing.T) {
	t.Run("caller value honoured over body length", func(t *testing.T) {
		req := mustReq(t, "https://example.com/")
		req.Header.Set("Content-Length", "999")
		if got := fieldValues(encodeFields(t, req, 5), "content-length"); !slices.Equal(got, []string{"999"}) {
			t.Errorf("content-length = %v, want [999] (caller value verbatim, exactly one)", got)
		}
	})
	t.Run("body length when caller provides none", func(t *testing.T) {
		req := mustReq(t, "https://example.com/")
		if got := fieldValues(encodeFields(t, req, 5), "content-length"); !slices.Equal(got, []string{"5"}) {
			t.Errorf("content-length = %v, want [5]", got)
		}
	})
	t.Run("absent with no body and no caller value", func(t *testing.T) {
		req := mustReq(t, "https://example.com/")
		if got := fieldValues(encodeFields(t, req, -1), "content-length"); len(got) != 0 {
			t.Errorf("content-length = %v, want none", got)
		}
	})
}

// TestEncodeHeaders_authority verifies authority precedence and IDNA
// conversion.
func TestEncodeHeaders_authority(t *testing.T) {
	t.Run("ascii host and port from URL", func(t *testing.T) {
		req := mustReq(t, "https://example.com:8443/")
		if got := fieldValues(encodeFields(t, req, -1), ":authority"); !slices.Equal(got, []string{"example.com:8443"}) {
			t.Errorf(":authority = %v, want [example.com:8443]", got)
		}
	})
	t.Run("idn authority punycoded", func(t *testing.T) {
		req := mustReq(t, "https://example.com/")
		req.Host = "münchen.de" // http.NewRequest already sets req.Host from the URL
		if got := fieldValues(encodeFields(t, req, -1), ":authority"); !slices.Equal(got, []string{"xn--mnchen-3ya.de"}) {
			t.Errorf(":authority = %v, want [xn--mnchen-3ya.de]", got)
		}
	})
	t.Run("idn from URL host when req.Host empty", func(t *testing.T) {
		req := mustReq(t, "https://example.com/")
		req.Host = ""
		req.URL.Host = "münchen.de"
		if got := fieldValues(encodeFields(t, req, -1), ":authority"); !slices.Equal(got, []string{"xn--mnchen-3ya.de"}) {
			t.Errorf(":authority = %v, want [xn--mnchen-3ya.de]", got)
		}
	})
	t.Run("crafted ascii authority preserved byte-exact", func(t *testing.T) {
		req := mustReq(t, "https://example.com/")
		req.Host = "Weird.Authority_x" // ASCII → PunycodeHostPort is a no-op, sent as-is
		if got := fieldValues(encodeFields(t, req, -1), ":authority"); !slices.Equal(got, []string{"Weird.Authority_x"}) {
			t.Errorf(":authority = %v, want [Weird.Authority_x]", got)
		}
	})
}

// TestEncodeHeaders_hopByHopStripped verifies forbidden-field omission and TE
// preservation.
func TestEncodeHeaders_hopByHopStripped(t *testing.T) {
	req := mustReq(t, "https://example.com/")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("TE", "gzip") // a non-"trailers" TE: forwarded, not stripped
	fields := encodeFields(t, req, -1)
	if got := fieldValues(fields, "connection"); len(got) != 0 {
		t.Errorf("connection header leaked onto the wire: %v", got)
	}
	if got := fieldValues(fields, "te"); !slices.Equal(got, []string{"gzip"}) {
		t.Errorf("te = %v, want [gzip] (TE is forwarded verbatim, not stripped)", got)
	}
}

// TestEncodeHeaders_doesNotMutateRequest verifies that omitted fields remain in
// the caller's header map.
func TestEncodeHeaders_doesNotMutateRequest(t *testing.T) {
	req := mustReq(t, "https://example.com/")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("X-Keep", "yes")

	fields := encodeFields(t, req, -1)
	if got := fieldValues(fields, "connection"); len(got) != 0 {
		t.Errorf("connection header leaked onto the wire: %v", got)
	}

	if got := req.Header.Get("Connection"); got != "keep-alive" {
		t.Errorf("Connection = %q after encode, want it left intact (encode must not mutate)", got)
	}
	if got := req.Header.Get("X-Keep"); got != "yes" {
		t.Errorf("X-Keep = %q after encode, want unchanged", got)
	}
}

// TestNormaliseAddr_punycodesIDN verifies IDNA conversion and ASCII
// passthrough.
func TestNormaliseAddr_punycodesIDN(t *testing.T) {
	if got, err := normaliseAddr("münchen.de"); err != nil || got != "xn--mnchen-3ya.de:443" {
		t.Errorf("normaliseAddr(münchen.de) = %q, %v; want xn--mnchen-3ya.de:443", got, err)
	}
	// ASCII host with explicit port is unchanged.
	if got, _ := normaliseAddr("127.0.0.1:1"); got != "127.0.0.1:1" {
		t.Errorf("normaliseAddr(127.0.0.1:1) = %q, want unchanged", got)
	}
}

func TestNormaliseAddrHandlesEmptyPortAndHost(t *testing.T) {
	t.Parallel()
	if got, err := normaliseAddr("example.com:"); err != nil || got != "example.com:443" {
		t.Errorf("normaliseAddr(example.com:) = %q, %v; want example.com:443", got, err)
	}
	if _, err := normaliseAddr(":443"); err == nil {
		t.Fatal("normaliseAddr(:443) accepted an empty host")
	}
}

// TestEncodeHeaders_hostHeaderFoldedIntoAuthorityAndSkipped verifies the Host
// fallback without a duplicate regular field.
func TestEncodeHeaders_hostHeaderFoldedIntoAuthorityAndSkipped(t *testing.T) {
	req := mustReq(t, "https://url.example/")
	req.Host = ""                            // simulate a manually built request
	req.Header.Set("Host", "header.example") // h1-style Host header, no req.Host

	fields := encodeFields(t, req, -1)

	if got := fieldValues(fields, ":authority"); !slices.Equal(got, []string{"header.example"}) {
		t.Errorf(":authority = %v, want [header.example] (Host header folded in)", got)
	}
	if got := fieldValues(fields, "host"); len(got) != 0 {
		t.Errorf("host = %v, want none (folded into :authority, not re-emitted)", got)
	}
}

// TestEncodeHeaders_reqHostWinsOverHostHeader verifies authority precedence.
func TestEncodeHeaders_reqHostWinsOverHostHeader(t *testing.T) {
	req := mustReq(t, "https://canonical.example/")
	req.Header.Set("Host", "header.example") // req.Host is already set by mustReq

	fields := encodeFields(t, req, -1)

	if got := fieldValues(fields, ":authority"); !slices.Equal(got, []string{"canonical.example"}) {
		t.Errorf(":authority = %v, want [canonical.example] (req.Host wins)", got)
	}
	if got := fieldValues(fields, "host"); len(got) != 0 {
		t.Errorf("host = %v, want none", got)
	}
}

// TestPseudoHeaders verifies values and transmission order for all four fields.
func TestPseudoHeaders(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  func(*testing.T) *http.Request
		want []PseudoHeader
	}{
		{
			name: "ordinary HTTPS request",
			req: func(t *testing.T) *http.Request {
				return mustReq(t, "https://example.com:8443/a/b?q=1")
			},
			want: []PseudoHeader{
				{":method", "GET"},
				{":path", "/a/b?q=1"},
				{":scheme", "https"},
				{":authority", "example.com:8443"},
			},
		},
		{
			name: "missing scheme and path take defaults",
			req: func(t *testing.T) *http.Request {
				r := mustReq(t, "https://example.com/")
				r.URL.Scheme = ""
				r.URL.Path = ""
				return r
			},
			want: []PseudoHeader{
				{":method", "GET"},
				{":path", "/"},
				{":scheme", "https"},
				{":authority", "example.com"},
			},
		},
		{
			name: "request scheme and raw target survive",
			req: func(t *testing.T) *http.Request {
				r := mustReq(t, "http://example.com/a/b")
				r.URL.RawPath = "/a%2Fb"
				r.URL.RawQuery = "q=raw space"
				return r
			},
			want: []PseudoHeader{
				{":method", "GET"},
				{":path", "/a%2Fb?q=raw space"},
				{":scheme", "http"},
				{":authority", "example.com"},
			},
		},
		{
			name: "path bytes requiring escaping are escaped",
			req: func(t *testing.T) *http.Request {
				r := mustReq(t, "https://example.com/")
				r.URL.Path = "/a b"
				return r
			},
			want: []PseudoHeader{
				{":method", "GET"},
				{":path", "/a%20b"},
				{":scheme", "https"},
				{":authority", "example.com"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PseudoHeaders(tc.req(t)); !slices.Equal(got, tc.want) {
				t.Errorf("PseudoHeaders = %+v, want %+v", got, tc.want)
			}
		})
	}
}
