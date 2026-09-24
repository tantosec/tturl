package curlblocks

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// NewRequest builds a reusable [http.Request] for url using the block's method,
// headers, cookies, and materialised body. It reads referenced files and sets
// GetBody for repeated sends.
//
// A Host header sets request authority. Explicit Cookie and Authorization
// headers override cookie flags and URL userinfo; suppression also prevents
// URL Basic authentication. Header names are canonicalised by [http.Header].
// Hop-by-hop headers are retained for the sending transport to handle.
func (b Block) NewRequest(ctx context.Context, url string) (*http.Request, error) {
	return b.newRequest(ctx, url, "")
}

// NewRequestWithMultipartBoundary builds a reusable request like
// [Block.NewRequest] and uses boundary when materialising a multipart form. An
// empty boundary keeps the ordinary generated-boundary behaviour. Other body
// kinds ignore boundary.
func (b Block) NewRequestWithMultipartBoundary(
	ctx context.Context,
	url, boundary string,
) (*http.Request, error) {
	return b.newRequest(ctx, url, boundary)
}

func (b Block) newRequest(
	ctx context.Context,
	url, multipartBoundary string,
) (*http.Request, error) {
	data, present, defaultHeaders, err := b.requestBody(multipartBoundary)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if present {
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, b.HTTPMethod(), url, body)
	if err != nil {
		return nil, err
	}
	for _, h := range b.Headers {
		if strings.EqualFold(h.Name, "Host") {
			req.Host = h.Value
			continue
		}
		req.Header.Add(h.Name, h.Value)
	}
	for _, h := range b.defaultHeaders(Header{Name: "Cookie", Value: b.CookieHeader()}) {
		req.Header.Add(h.Name, h.Value)
	}
	for _, h := range defaultHeaders {
		req.Header.Set(h.Name, h.Value)
	}
	if u := req.URL.User; u != nil &&
		req.Header.Get("Authorization") == "" && !b.SuppressesHeader("Authorization") {
		pass, _ := u.Password()
		req.SetBasicAuth(u.Username(), pass)
	}
	return req, nil
}

// HTTPMethod returns an explicit -X value verbatim. Otherwise it returns POST
// for a block with a body declaration and GET for a bodyless block. It does not
// read referenced files.
func (b Block) HTTPMethod() string {
	switch {
	case b.Method != "":
		return b.Method
	case len(b.data) > 0, len(b.form) > 0, len(b.json) > 0:
		return "POST"
	default:
		return "GET"
	}
}

// RequestBody materialises the declared data, form, or JSON body. A bodyless
// block returns nil, false. It uses expansion snapshots when present; other
// regular files are read on every call. It chooses
// a fresh multipart boundary. File-dependent JSON validation also occurs here.
//
// defaultHeaders contains the implied Content-Type and, for JSON, Accept
// fields, excluding names explicitly set or suppressed by the block. The
// returned slices are newly allocated.
func (b Block) RequestBody() (body []byte, present bool, defaultHeaders []Header, err error) {
	return b.requestBody("")
}

func (b Block) requestBody(
	multipartBoundary string,
) (body []byte, present bool, defaultHeaders []Header, err error) {
	if err := b.validateFileRefs(); err != nil {
		return nil, false, nil, err
	}
	readFile := b.reader()
	switch {
	case len(b.form) > 0:
		data, ct, err := b.formBody(
			readFile, multipartBoundary, b.FormEscape)
		if err != nil {
			return nil, false, nil, err
		}
		return data, true, b.defaultHeaders(Header{Name: "Content-Type", Value: ct}), nil
	case len(b.json) > 0:
		data, err := b.jsonBody(readFile)
		if err != nil {
			return nil, false, nil, err
		}
		return data, true, b.defaultHeaders(
			Header{Name: "Content-Type", Value: "application/json"},
			Header{Name: "Accept", Value: "application/json"},
		), nil
	default:
		data, present, err := b.dataBody(readFile)
		if err != nil {
			return nil, false, nil, err
		}
		if !present {
			return nil, false, nil, nil
		}
		return data, true, b.defaultHeaders(Header{Name: "Content-Type", Value: "application/x-www-form-urlencoded"}), nil
	}
}

// DefaultHeaderNames returns the distinct headers supplied by parser defaults
// or the body kind rather than by the user. Explicitly set or suppressed names
// are excluded. The order is unspecified, and the returned slice is owned by
// the caller.
func (b Block) DefaultHeaderNames() []string {
	var names nameSet
	for _, h := range b.Headers {
		if h.origin == headerBaked {
			names.add(h.Name)
		}
	}
	// The body's implied names, filtered exactly as defaultHeaders filters the
	// headers themselves, so this reports what a request would actually carry.
	var implied []string
	switch {
	case len(b.form) > 0, len(b.data) > 0:
		implied = []string{"Content-Type"}
	case len(b.json) > 0:
		implied = []string{"Content-Type", "Accept"}
	}
	for _, name := range implied {
		if !b.setsHeader(name) && !b.SuppressesHeader(name) {
			names.add(name)
		}
	}
	return names.names
}

// defaultHeaders selects, from the headers curlblocks would add by default,
// those the command line has neither set (-H 'Name: value') nor suppressed
// (-H 'Name:'), so a default never overrides the user. A candidate with an
// empty value is skipped, so a body kind that computes no type — or a block
// with no cookies — contributes nothing.
//
// A baked default does not block a candidate: it is curlblocks' own text, not
// the user's, so --json's "Accept: application/json" applies over the baked
// "Accept: */*" and replaces it, as in curl.
func (b Block) defaultHeaders(candidates ...Header) []Header {
	var out []Header
	for _, h := range candidates {
		if h.Value == "" || b.setsHeader(h.Name) || b.SuppressesHeader(h.Name) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// setsHeader reports whether the command line set a header of the given name
// (matched case-insensitively, as HTTP field names are): an explicit -H or a
// convenience flag such as -A/-u that names one. A default the parser baked in
// is not the user setting it and so does not count.
func (b Block) setsHeader(name string) bool {
	for _, h := range b.Headers {
		if h.origin != headerBaked && strings.EqualFold(h.Name, name) {
			return true
		}
	}
	return false
}

// reader is the function the block uses to load an @file/<file. It is the
// reader configured on the Parser (see WithFileReader), defaulting to
// os.ReadFile for a block built outside the parser (a literal in a test).
func (b Block) reader() func(name string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if data, ok := b.bodyFiles[name]; ok {
			return bytes.Clone(data), nil
		}
		if name == "-" {
			if b.ReadStdin == nil {
				return nil, fmt.Errorf("reading from stdin is not supported")
			}
			data, err := b.ReadStdin()
			return append([]byte(nil), data...), err
		}
		read := b.readFile
		if read == nil {
			read = os.ReadFile
		}
		return read(name)
	}
}
