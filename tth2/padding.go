package tth2

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
)

// RequestPadding specifies synthetic fields to prepend to a request. In a trial
// run, padding is assigned by stream position rather than request identity.
type RequestPadding struct {
	// URLParams is the number of synthetic query parameters to prepend to the
	// request URL (_pad1=x, _pad2=x, and so on). Servers scanning left to right
	// must parse these before reaching the real parameters.
	URLParams int

	// Headers is the number of synthetic request headers to add to the HEADERS
	// frame (X-Pad-1: x, X-Pad-2: x, and so on). Their frame order is not
	// deterministic.
	Headers int

	// BodyParams is the number of synthetic fields to inject at the start of
	// the request body. Supported media types are application/json (with an
	// object body), application/x-www-form-urlencoded, and multipart/form-data.
	// Other types return an error. Body padding requires GetBody. Supported
	// probes are not fully syntax-validated. Padding is buffered in memory per
	// trial and may change ContentLength; explicit Content-Length headers
	// remain verbatim.
	BodyParams int
}

func (p RequestPadding) isZero() bool {
	return p.URLParams == 0 && p.Headers == 0 && p.BodyParams == 0
}

func (p RequestPadding) validate() error {
	for _, field := range []struct {
		name  string
		value int
	}{
		{"URLParams", p.URLParams},
		{"Headers", p.Headers},
		{"BodyParams", p.BodyParams},
	} {
		if field.value < 0 {
			return fmt.Errorf("tth2: RequestPadding.%s must be >= 0", field.name)
		}
	}
	return nil
}

// validatePadding checks the padding configuration before a send or trial run
// acquires a connection.
func validatePadding(reqs []*http.Request, pads []RequestPadding) error {
	if len(pads) > len(reqs) {
		return fmt.Errorf("tth2: Padding length (%d) exceeds request count (%d)", len(pads), len(reqs))
	}
	for i, pad := range pads {
		if err := pad.validate(); err != nil {
			return fmt.Errorf("tth2: padding %d: %w", i, err)
		}
	}
	return nil
}

// requireReusableBodies returns an error if any request has a body tth2 cannot
// re-read. A reusable body is absent or has GetBody set. One-shot bodies are
// rejected rather than silently buffered.
func requireReusableBodies(reqs []*http.Request) error {
	for i, req := range reqs {
		if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
			return fmt.Errorf("tth2: request %d has a one-shot body (GetBody is nil); create it with bytes.NewReader or "+
				"strings.NewReader, or supply GetBody", i)
		}
	}
	return nil
}

// applyPadding returns copies of reqs with padding applied by stream position.
//
// Precondition: every req must have a reusable body (Body nil or GetBody set).
// Commitment: originals are never mutated.
//
// If pads is empty the original slice is returned unchanged (same pointer).
// Requests at indices beyond len(pads), or whose padding entry is zero, are
// returned as the same *http.Request pointer without cloning.
func applyPadding(reqs []*http.Request, pads []RequestPadding) ([]*http.Request, error) {
	if len(pads) == 0 {
		return reqs, nil
	}
	out := make([]*http.Request, len(reqs))
	for i, req := range reqs {
		if i >= len(pads) || pads[i].isZero() {
			out[i] = req
			continue
		}
		r, err := padRequest(req, pads[i])
		if err != nil {
			return nil, fmt.Errorf("tth2: padding request %d: %w", i, err)
		}
		out[i] = r
	}
	return out, nil
}

// padRequest returns a clone of orig with pad applied. It does not mutate orig.
// If pad is zero, it returns orig itself. Negative counts and unsupported body
// shapes return errors. Supported probes are preserved without full syntax
// validation.
//
// Precondition: orig has a URL and any body has GetBody set.
func padRequest(orig *http.Request, pad RequestPadding) (*http.Request, error) {
	if err := pad.validate(); err != nil {
		return nil, err
	}
	if pad.isZero() {
		return orig, nil
	}
	r := orig.Clone(orig.Context())

	if pad.URLParams > 0 {
		var prefix strings.Builder
		for i := range pad.URLParams {
			if i > 0 {
				prefix.WriteByte('&')
			}
			fmt.Fprintf(&prefix, "_pad%d=x", i+1)
		}
		if r.URL.RawQuery == "" {
			r.URL.RawQuery = prefix.String()
		} else {
			r.URL.RawQuery = prefix.String() + "&" + r.URL.RawQuery
		}
	}

	if pad.Headers > 0 {
		if r.Header == nil {
			r.Header = make(http.Header)
		}
		for i := range pad.Headers {
			r.Header.Set(fmt.Sprintf("X-Pad-%d", i+1), "x")
		}
	}

	if pad.BodyParams > 0 {
		if err := padBody(r, pad.BodyParams); err != nil {
			return nil, err
		}
	}

	return r, nil
}

func padBody(r *http.Request, n int) error {
	ct := r.Header.Get("Content-Type")
	if ct == "" {
		return fmt.Errorf("Content-Type header is required for body padding")
	}
	mediaType, params, _ := mime.ParseMediaType(ct)
	switch mediaType {
	case "application/json":
		return padJSONBody(r, n)
	case "application/x-www-form-urlencoded":
		return padFormBody(r, n)
	case "multipart/form-data":
		return padMultipartBody(r, n, params["boundary"])
	default:
		return fmt.Errorf("body padding not supported for Content-Type %q (supported: application/json, "+
			"application/x-www-form-urlencoded, multipart/form-data)", ct)
	}
}

func readBodyBytes(r *http.Request) ([]byte, error) {
	if r.GetBody == nil {
		return nil, fmt.Errorf("GetBody is required for body padding")
	}
	rc, err := r.GetBody()
	if err != nil {
		return nil, fmt.Errorf("GetBody: %w", err)
	}
	if rc == nil {
		return nil, fmt.Errorf("GetBody: returned nil body")
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

func setBody(r *http.Request, b []byte) {
	r.ContentLength = int64(len(b))
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(b)), nil
	}
}

func padJSONBody(r *http.Request, n int) error {
	b, err := readBodyBytes(r)
	if err != nil {
		return err
	}

	// Validate that the top-level JSON construct is an object.
	start := bytes.IndexFunc(b, func(c rune) bool {
		return c != ' ' && c != '\t' && c != '\r' && c != '\n'
	})
	if start < 0 {
		return fmt.Errorf("JSON body padding: body is empty")
	}
	if b[start] != '{' {
		switch b[start] {
		case '[':
			return fmt.Errorf("JSON body padding: body is a JSON array, not an object")
		default:
			return fmt.Errorf("JSON body padding: body is not a JSON object (first non-whitespace byte is %q)", b[start])
		}
	}

	// Build pad prefix: {"_pad1":"x","_pad2":"x",...
	var prefix strings.Builder
	prefix.WriteByte('{')
	for i := range n {
		if i > 0 {
			prefix.WriteByte(',')
		}
		fmt.Fprintf(&prefix, `"_pad%d":"x"`, i+1)
	}

	rest := b[start+1:] // everything after the opening '{'
	trimmed := bytes.TrimSpace(rest)
	var padded []byte
	if len(trimmed) > 0 && trimmed[0] == '}' {
		// Empty object: close without comma.
		padded = append([]byte(prefix.String()), '}')
	} else {
		// Non-empty object: insert a comma then the original fields.
		prefix.WriteByte(',')
		padded = append([]byte(prefix.String()), rest...)
	}
	setBody(r, padded)
	return nil
}

func padFormBody(r *http.Request, n int) error {
	b, err := readBodyBytes(r)
	if err != nil {
		return err
	}
	var prefix strings.Builder
	for i := range n {
		if i > 0 {
			prefix.WriteByte('&')
		}
		fmt.Fprintf(&prefix, "_pad%d=x", i+1)
	}
	if len(b) > 0 {
		prefix.WriteByte('&')
	}
	setBody(r, append([]byte(prefix.String()), b...))
	return nil
}

func padMultipartBody(r *http.Request, n int, boundary string) error {
	if boundary == "" {
		return fmt.Errorf("multipart/form-data body padding: missing boundary in Content-Type")
	}
	b, err := readBodyBytes(r)
	if err != nil {
		return err
	}

	// Locate the first part boundary "--{boundary}\r\n" and insert synthetic
	// parts before it so the server must parse them before any real fields.
	marker := []byte("--" + boundary + "\r\n")
	idx := bytes.Index(b, marker)
	if idx < 0 {
		return fmt.Errorf("multipart/form-data body padding: first part boundary %q not found", "--"+boundary+"\r\n")
	}

	var parts strings.Builder
	for i := range n {
		fmt.Fprintf(&parts, "--%s\r\nContent-Disposition: form-data; name=\"_pad%d\"\r\n\r\nx\r\n", boundary, i+1)
	}
	partsBytes := []byte(parts.String())
	padded := make([]byte, 0, idx+len(partsBytes)+len(b)-idx)
	padded = append(padded, b[:idx]...)
	padded = append(padded, partsBytes...)
	padded = append(padded, b[idx:]...)
	setBody(r, padded)
	return nil
}
