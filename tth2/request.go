package tth2

import (
	"bytes"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/net/http2/hpack"
)

// hopByHopHeaders are the connection-specific header fields HTTP/2
// unconditionally forbids (RFC 9113 §8.2.2). A server treats a request carrying
// one as malformed, so the encoder omits them (see encodeHeaders).
//
// TE is deliberately excluded. §8.2.2 permits TE: trailers, and any other TE
// value is a caller's probe; like net/http's HTTP/2 client, tth2 forwards TE
// verbatim and lets the server enforce the trailers-only rule.
var hopByHopHeaders = map[string]bool{
	"connection":        true,
	"keep-alive":        true,
	"proxy-connection":  true,
	"transfer-encoding": true,
	"upgrade":           true,
}

// PseudoHeader is an HTTP/2 request pseudo-header as sent on the wire.
type PseudoHeader struct {
	// Name includes the leading colon.
	Name string
	// Value is the field value.
	Value string
}

// PseudoHeaders returns a fresh slice containing req's HTTP/2 pseudo-headers in
// transmission order: :method, :path, :scheme, and :authority.
//
// The scheme comes from req.URL and defaults to "https"; it is not rewritten to
// match the TLS connection. The path comes from req.URL.RequestURI and defaults
// to "/". The authority is the first non-empty value among req.Host, the Host
// header, and req.URL.Host, converted to its IDNA A-label form when possible.
//
// PseudoHeaders panics if req or req.URL is nil.
func PseudoHeaders(req *http.Request) []PseudoHeader {
	scheme := req.URL.Scheme
	if scheme == "" {
		scheme = "https"
	}
	path := req.URL.RequestURI()
	if path == "" {
		path = "/"
	}
	return []PseudoHeader{
		{":method", req.Method},
		{":path", path},
		{":scheme", scheme},
		{":authority", authorityFromReq(req)},
	}
}

// encodeHeaders HPACK-encodes req's headers into an HTTP/2 block fragment.
// Pseudo-headers (see [PseudoHeaders]) are emitted first, followed by a single
// content-length (a caller-provided value if present, otherwise the body length
// when bodyLen >= 0), then lowercased regular headers. The regular-header loop
// skips content-length and host; both are already conveyed above. enc must be
// the encoder for the current connection — its dynamic table must persist
// across streams.
func encodeHeaders(enc *hpack.Encoder, buf *bytes.Buffer, req *http.Request, bodyLen int64) ([]byte, error) {
	buf.Reset()

	for _, ph := range PseudoHeaders(req) {
		if err := enc.WriteField(hpack.HeaderField{Name: ph.Name, Value: ph.Value}); err != nil {
			return nil, fmt.Errorf("hpack pseudo %q: %w", ph.Name, err)
		}
	}

	// Content-length: honour a caller-provided value verbatim — a deliberately
	// wrong length is a legitimate probe for content-length-based desync, and a
	// mismatch is contained to a server-issued stream reset. Otherwise emit the
	// actual body length. Exactly one is sent (the loop below skips
	// content-length); two or differing values would be malformed.
	if userCL := req.Header.Get("Content-Length"); userCL != "" {
		if err := enc.WriteField(hpack.HeaderField{Name: "content-length", Value: userCL}); err != nil {
			return nil, fmt.Errorf("hpack content-length: %w", err)
		}
	} else if bodyLen >= 0 {
		if err := enc.WriteField(hpack.HeaderField{
			Name: "content-length", Value: strconv.FormatInt(bodyLen, 10),
		}); err != nil {
			return nil, fmt.Errorf("hpack content-length: %w", err)
		}
	}

	// HTTP/2 field names MUST be lowercase (RFC 9113 §8.2.1) — uppercase is
	// malformed, so lowercasing is required, not cosmetic. Values are sent
	// verbatim, with no validation: a bad value (CR/LF/NUL, etc.) earns a
	// server-issued stream reset, which is observable signal that h2's
	// length-prefixed framing contains to this one stream. Repeated names emit
	// repeated field lines — duplicates are legal in h2 and deliberately kept.
	//
	// Three field groups are skipped rather than emitted here: content-length
	// is emitted above; host is conveyed by :authority (RFC 9113 §8.3.1), so a
	// Host field would be redundant or, if it disagreed, malformed; and the
	// connection-specific ("hop-by-hop") fields HTTP/2 forbids (RFC 9113
	// §8.2.2) must not reach the wire. Skipping them here — rather than
	// deleting them from req.Header — keeps encoding a pure read, so the
	// caller's request is never mutated and may be shared across concurrent
	// sends.
	for name, vals := range req.Header {
		lower := strings.ToLower(name)
		if lower == "content-length" || lower == "host" || hopByHopHeaders[lower] {
			continue
		}
		for _, v := range vals {
			if err := enc.WriteField(hpack.HeaderField{Name: lower, Value: v}); err != nil {
				return nil, fmt.Errorf("hpack header %q: %w", lower, err)
			}
		}
	}

	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	return out, nil
}
