package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/tth2"
)

// buildRequest constructs one request for url from the block, via
// curlblocks.Block.NewRequest (method, headers, cookies, body, and curl's auth
// precedence), then strips hop-by-hop headers so the printed request matches
// the wire: tth2 skips them at encode time without mutating the request, so we
// strip our own copy to keep the pretty-print faithful.
func buildRequest(ctx context.Context, b curlblocks.Block, url string) (*http.Request, error) {
	return buildRequestWithMultipartBoundary(ctx, b, url, "")
}

func buildRequestWithMultipartBoundary(
	ctx context.Context,
	b curlblocks.Block,
	url, boundary string,
) (*http.Request, error) {
	req, err := b.NewRequestWithMultipartBoundary(ctx, url, boundary)
	if err != nil {
		return nil, err
	}
	stripHopByHopHeaders(req.Header)
	return req, nil
}

// stripHopByHopHeaders keeps the displayed request aligned with HTTP/2.
func stripHopByHopHeaders(h http.Header) {
	for name := range h {
		switch strings.ToLower(name) {
		case "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade":
			delete(h, name)
		}
	}
}

// wireTarget returns the origin ("scheme://authority") and path a request will
// actually go out under, read from [tth2.PseudoHeaders] so the summary and the
// wire cannot disagree. Sourcing the authority there is what makes a Host
// override visible: curlblocks maps -H 'Host: v' onto req.Host, which sets
// :authority, and req.URL alone still shows the URL's own host.
func wireTarget(req *http.Request) (origin, path string) {
	var scheme, authority string
	for _, ph := range tth2.PseudoHeaders(req) {
		switch ph.Name {
		case ":scheme":
			scheme = ph.Value
		case ":authority":
			authority = ph.Value
		case ":path":
			path = ph.Value
		}
	}
	return scheme + "://" + authority, path
}

// maxBodyDisplay is the largest body, in bytes, that summarise renders inline.
// Anything larger is elided to keep the request pretty-print readable.
const maxBodyDisplay = 256

// formatRequest renders a single request as the HTTP/2 message tth2 will send:
// the pseudo-headers (:method/:path/:scheme/:authority) exactly as they go on
// the wire, then the remaining headers lowercased and sorted, then the body
// (elided when larger than maxBodyDisplay, unless fullBody is set under
// --verbose). The returned string ends with a newline.
func formatRequest(req *http.Request, fullBody bool) string {
	var b strings.Builder

	// Pseudo-headers as tth2 will encode them — the truthful HTTP/2 view. The
	// authority is :authority, not a Host header (which has no place on an h2
	// wire); sourcing them from tth2 keeps the display and the wire in lockstep.
	for _, ph := range tth2.PseudoHeaders(req) {
		fmt.Fprintf(&b, "%s: %s\n", ph.Name,
			curlblocks.DisplayText(ph.Value))
	}

	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// host is shown as :authority above and is not re-emitted on the wire.
		if strings.EqualFold(name, "Host") {
			continue
		}
		for _, v := range req.Header[name] {
			// Lowercased to match the wire (HTTP/2 lowercases all field names);
			// req.Header keeps canonical keys so stdlib .Get lookups (e.g. tth2's
			// Content-Type check) still resolve.
			fmt.Fprintf(&b, "%s: %s\n",
				curlblocks.DisplayText(strings.ToLower(name)),
				curlblocks.DisplayText(v))
		}
	}

	if body, ok := bodyForDisplay(req, fullBody); ok {
		fmt.Fprintf(&b, "\n%s\n", curlblocks.DisplayText(body))
	}
	return b.String()
}

// bodyForDisplay returns the request body rendered for printing. Bodies larger
// than maxBodyDisplay are replaced with a "[n bytes omitted]" placeholder
// unless fullBody is set (under --verbose), in which case the whole body is
// rendered. The bool is false when the request has no body.
func bodyForDisplay(req *http.Request, fullBody bool) (string, bool) {
	data := requestBody(req)
	if len(data) == 0 {
		return "", false
	}
	if !fullBody && len(data) > maxBodyDisplay {
		return fmt.Sprintf("[%d bytes omitted]", len(data)), true
	}
	return string(data), true
}

// requestBody reads the request body via GetBody so req.Body stays unconsumed,
// returning nil for a bodyless request or on any read error.
func requestBody(req *http.Request) []byte {
	if req.GetBody == nil {
		return nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil
	}
	return data
}
