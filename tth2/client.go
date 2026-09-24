package tth2

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/net/http/httpguts"
)

type sendConfig struct {
	releaseDelay             time.Duration
	bodyBytesWithheld        int
	peerStreamLimitIgnored   bool
	padding                  []RequestPadding
	maxResponseBodyBytes     int64
	batchTimeout             time.Duration
	responseBodyCaptureBytes int64
	responseHeaderCapture    bool
}

// UnlimitedResponseBytes disables a response byte limit explicitly.
const UnlimitedResponseBytes int64 = -1

func defaultSendConfig() sendConfig {
	return sendConfig{
		releaseDelay:      0,
		bodyBytesWithheld: 1,
	}
}

// SendOption configures one [Client.SendBatch]. [Client.StreamTrials] accepts
// every SendOption and applies it to each batch. [Client.RunTrials] accepts the
// non-capture subset through [RunTrialsOption].
type SendOption interface {
	TrialsOption
	applySend(*sendConfig)
}

// WithMaxResponseBodyBytes limits the accepted response DATA on each stream. A
// positive value accepts at most that many bytes per response. Zero, the
// default, leaves response bodies unlimited. Crossing the limit fails the
// complete batch with [ResponseBodyLimitError] and makes its connection
// ineligible for reuse. The frame that crosses the limit may already have been
// read, but bytes beyond the accepted prefix are not hashed, retained, or
// counted.
//
// Panics if n is negative.
func WithMaxResponseBodyBytes(n int64) maxResponseBodyBytesOption {
	if n < 0 {
		panic("tth2: WithMaxResponseBodyBytes requires n >= 0")
	}
	return maxResponseBodyBytesOption{n: n}
}

type maxResponseBodyBytesOption struct{ n int64 }

func (o maxResponseBodyBytesOption) applySend(c *sendConfig) {
	c.maxResponseBodyBytes = o.n
}

func (o maxResponseBodyBytesOption) applyTrials(c *trialsConfig) {
	o.applySend(&c.send)
}

func (maxResponseBodyBytesOption) runTrialsOption() {}

// WithBatchTimeout limits one batch's transport occupancy. The timer starts
// after pacing, immediately before the first transport write. A positive
// duration covers request dispatch, body release delay, and response headers,
// bodies, and trailers. Zero, the default, leaves the batch unlimited. The
// caller's context remains authoritative when it is cancelled first. A timeout
// fails the complete batch with [BatchTimeoutError] and makes its connection
// ineligible for reuse.
//
// Panics if d is negative.
func WithBatchTimeout(d time.Duration) batchTimeoutOption {
	if d < 0 {
		panic("tth2: WithBatchTimeout requires d >= 0")
	}
	return batchTimeoutOption{d: d}
}

type batchTimeoutOption struct{ d time.Duration }

func (o batchTimeoutOption) applySend(c *sendConfig) {
	c.batchTimeout = o.d
}

func (o batchTimeoutOption) applyTrials(c *trialsConfig) {
	o.applySend(&c.send)
}

func (batchTimeoutOption) runTrialsOption() {}

// WithResponseBodyCaptureBytes sets the maximum number of response body bytes
// retained for each request. The default, 0, retains no body bytes. The
// transport still drains and hashes the entire accepted body. Pass
// [UnlimitedResponseBytes] to retain every accepted byte.
//
// This limit bounds only the retained prefix of one response. It does not bound
// accepted bytes, hashing work, response duration, total memory use, or caller
// retention of returned results. [WithMaxResponseBodyBytes] independently
// bounds accepted DATA and hashing work.
//
// Panics if maxBytes is less than [UnlimitedResponseBytes].
func WithResponseBodyCaptureBytes(maxBytes int64) responseBodyCaptureBytesOption {
	if maxBytes < UnlimitedResponseBytes {
		panic("tth2: WithResponseBodyCaptureBytes requires maxBytes >= -1")
	}
	return responseBodyCaptureBytesOption{maxBytes: maxBytes}
}

type responseBodyCaptureBytesOption struct{ maxBytes int64 }

func (o responseBodyCaptureBytesOption) applySend(c *sendConfig) {
	c.responseBodyCaptureBytes = o.maxBytes
}

func (o responseBodyCaptureBytesOption) applyTrials(c *trialsConfig) {
	o.applySend(&c.send)
}

// WithResponseHeaderCapture retains accepted final response headers and
// trailers. Without it, responses retain only status and declared content
// length. Header parsing remains bounded by [Transport.MaxResponseHeaderBytes]
// either way.
func WithResponseHeaderCapture() responseHeaderCaptureOption {
	return responseHeaderCaptureOption{}
}

type responseHeaderCaptureOption struct{}

func (responseHeaderCaptureOption) applySend(c *sendConfig) {
	c.responseHeaderCapture = true
}

func (o responseHeaderCaptureOption) applyTrials(c *trialsConfig) {
	o.applySend(&c.send)
}

// WithReleaseDelay sets the pause before the final release of withheld body
// bytes and END_STREAM. The default is zero. Values at or below zero add no
// delay, and the value has no effect when [WithBodyBytesWithheld](0) disables
// tail withholding.
//
// A delay can let handlers reach their body reads before release, but can also
// introduce wake-up jitter. The appropriate value depends on the target.
func WithReleaseDelay(d time.Duration) releaseDelayOption {
	return releaseDelayOption{d: d}
}

type releaseDelayOption struct{ d time.Duration }

func (o releaseDelayOption) applySend(c *sendConfig)     { c.releaseDelay = o.d }
func (o releaseDelayOption) applyTrials(c *trialsConfig) { o.applySend(&c.send) }
func (releaseDelayOption) runTrialsOption()              {}

// WithBodyBytesWithheld sets the size of the deliberately withheld body tail
// released with END_STREAM. The default is one. Pass zero to send complete
// bodies without choosing a fixed withheld tail or release pause.
// The sender advances intermediate bytes until all remaining positive suffixes
// fit one shared DATA+END_STREAM record with available flow-control credit.
//
// For positive n, a body shorter than n is withheld in full. The combined
// final-release frames must fit in one TLS plaintext record or the batch is
// rejected before sending. Zero may send intermediate DATA records until the
// remaining suffixes fit; it does not impose that upfront limit.
//
// Panics if n is negative.
func WithBodyBytesWithheld(n int) bodyBytesWithheldOption {
	if n < 0 {
		panic("tth2: WithBodyBytesWithheld requires n >= 0")
	}
	return bodyBytesWithheldOption{n: n}
}

type bodyBytesWithheldOption struct{ n int }

func (o bodyBytesWithheldOption) applySend(c *sendConfig)     { c.bodyBytesWithheld = o.n }
func (o bodyBytesWithheldOption) applyTrials(c *trialsConfig) { o.applySend(&c.send) }
func (bodyBytesWithheldOption) runTrialsOption()              {}

// WithPeerStreamLimitIgnored permits a batch to exceed the peer's advertised
// SETTINGS_MAX_CONCURRENT_STREAMS value. This deliberately departs from HTTP/2
// for security testing of peer limit enforcement. By default tth2 rejects an
// oversized batch before sending any request headers; it never divides one
// batch into sequential groups, which would destroy its shared release gate.
func WithPeerStreamLimitIgnored() peerStreamLimitIgnoredOption {
	return peerStreamLimitIgnoredOption{}
}

type peerStreamLimitIgnoredOption struct{}

func (peerStreamLimitIgnoredOption) applySend(c *sendConfig) {
	c.peerStreamLimitIgnored = true
}

func (o peerStreamLimitIgnoredOption) applyTrials(c *trialsConfig) {
	o.applySend(&c.send)
}

func (peerStreamLimitIgnoredOption) runTrialsOption() {}

// WithPadding applies pads[i] to stream position i. Positions beyond len(pads)
// are unpadded. Under a trial arrangement the padding follows the position, not
// the request identity; [Client.SendBatch] uses input order directly.
//
// The option owns a copy of pads and may be reused concurrently. Too many
// entries or a negative count makes batch or run preparation return an error.
func WithPadding(pads ...RequestPadding) paddingOption {
	return paddingOption{pads: append([]RequestPadding(nil), pads...)}
}

type paddingOption struct{ pads []RequestPadding }

func (o paddingOption) applySend(c *sendConfig)     { c.padding = o.pads }
func (o paddingOption) applyTrials(c *trialsConfig) { c.padding = o.pads }
func (paddingOption) runTrialsOption()              {}

// Client sends batches through a [Transport]. The zero value uses
// [DefaultTransport], and clients are safe for concurrent use.
//
// Sending does not mutate requests. Each body must be absent or reusable
// through [http.Request.GetBody], because trials and stale-connection retries
// may read it more than once. Construct requests with bytes.NewReader or
// strings.NewReader to supply independent readers automatically. Do not mutate
// a request, its URL, headers or body storage while it may be in use. GetBody
// must return independent, finite readers and tolerate concurrent calls. The
// transport closes those readers but never reads or closes Request.Body.
// GetBody and its readers run synchronously and cannot be interrupted by the
// batch context; they must return promptly. Method contexts must be non-nil.
// Configure the Transport pointer before use. Copying a Client shares its
// Transport, including the default when the pointer is nil.
type Client struct {
	// Transport supplies and pools connections. nil uses [DefaultTransport].
	Transport *Transport
}

// transport returns the client's Transport, or [DefaultTransport] if unset.
func (c *Client) transport() *Transport {
	if c.Transport != nil {
		return c.Transport
	}
	return DefaultTransport
}

// SendBatch releases reqs as one batch over one connection. It requires at
// least one request, a common origin, and reusable bodies. Results remain in
// input order.
//
// The batch is sent exactly as given: position 0 is the first request. A caller
// wanting the positions varied from trial to trial wants [Client.RunTrials] or
// [Client.StreamTrials], which own the arrangement.
//
// Stale pooled connections are discarded and retried; a failed fresh connection
// is not retried. A retry can repeat a partly dispatched batch, including
// non-idempotent methods. Caller cancellation and configured local termination
// are never retried. The result and its mutable storage belong to the caller
// and are not reused by another call. The request slice is neither modified nor
// retained after return.
//
// A reset during response collection returns [StreamError] alongside the
// partial BatchResult. Completed siblings keep their ranks, and every reset
// request carries [Result.Reset]. A reset during dispatch returns no result and
// discards the connection, since siblings may be only partly sent. Every other
// error returns a nil result. In particular, [ResponseBodyLimitError] and
// [BatchTimeoutError] discard every partial response fact and the connection
// that carried the batch.
func (c *Client) SendBatch(ctx context.Context, reqs []*http.Request, opts ...SendOption) (*BatchResult, error) {
	if ctx == nil {
		panic("tth2: nil context")
	}
	origin, toSend, cfg, err := prepareBatch(reqs, opts)
	if err != nil {
		return nil, err
	}
	return c.transport().sendBatch(ctx, origin, toSend, cfg)
}

func prepareBatch(
	reqs []*http.Request,
	opts []SendOption,
) (origin string, toSend []*http.Request, cfg sendConfig, err error) {
	cfg = defaultSendConfig()
	for _, opt := range opts {
		opt.applySend(&cfg)
	}
	if err := validateSendConfig(cfg); err != nil {
		return "", nil, cfg, err
	}
	origin, err = BatchOrigin(reqs)
	if err != nil {
		return "", nil, cfg, err
	}
	if err := validatePadding(reqs, cfg.padding); err != nil {
		return "", nil, cfg, err
	}
	if err := requireReusableBodies(reqs); err != nil {
		return "", nil, cfg, err
	}

	toSend = reqs
	if len(cfg.padding) > 0 {
		toSend, err = applyPadding(reqs, cfg.padding)
		if err != nil {
			return "", nil, cfg, fmt.Errorf("tth2: padding: %w", err)
		}
	}
	return origin, toSend, cfg, nil
}

func validateSendConfig(cfg sendConfig) error {
	if cfg.maxResponseBodyBytes < 0 {
		return errors.New("tth2: maximum response body bytes must not be negative")
	}
	if cfg.batchTimeout < 0 {
		return errors.New("tth2: batch timeout must not be negative")
	}
	return nil
}

// BatchOrigin returns the normalised "host:port" shared by reqs. It punycodes
// host names and supplies port 443 when absent. An empty batch, nil request or
// URL, unusable host, or differing URL host returns an error. Requests may
// override their wire authority independently through Request.Host or headers;
// the connection destination always comes from URL.Host. ASCII hostname case
// is preserved, and the scheme does not change the TLS connection destination.
func BatchOrigin(reqs []*http.Request) (string, error) {
	if len(reqs) < 1 {
		return "", fmt.Errorf("tth2: at least one request is required")
	}
	if reqs[0] == nil || reqs[0].URL == nil {
		return "", fmt.Errorf("tth2: request 0 has no valid host: request or URL is nil")
	}
	origin, err := normaliseAddr(reqs[0].URL.Host)
	if err != nil {
		return "", fmt.Errorf("tth2: request 0 has no valid host: %w", err)
	}
	for i, r := range reqs[1:] {
		if r == nil || r.URL == nil {
			return "", fmt.Errorf("tth2: request %d has no valid host: request or URL is nil", i+1)
		}
		a, err := normaliseAddr(r.URL.Host)
		if err != nil {
			return "", fmt.Errorf("tth2: request %d has no valid host: %w", i+1, err)
		}
		if a != origin {
			return "", fmt.Errorf("tth2: request %d addr %q differs from batch origin %q", i+1, a, origin)
		}
	}
	return origin, nil
}

// hasPort reports whether host already carries a ":port" suffix. It accepts the
// shapes that arrive from a URL's Host field: bare host, host:port, "[::1]",
// and "[::1]:port".
func hasPort(host string) bool {
	_, _, err := net.SplitHostPort(host)
	return err == nil
}

// normaliseAddr accepts "host" or "host:port" (including bracketed IPv6) and
// returns "host:port" with a default :443 if no port was given. tth2 always
// uses TLS, so 443 is the appropriate default.
//
// The host is punycoded to its IDNA A-label form so the dial (DNS) and the TLS
// SNI — both derived from this address — use the ASCII form resolvers and
// servers expect; a raw IDN host would otherwise never resolve. ASCII hosts
// (including unusual but diallable ones) are returned unchanged.
func normaliseAddr(addr string) (string, error) {
	if addr == "" {
		return "", fmt.Errorf("addr is required")
	}
	if ascii, err := httpguts.PunycodeHostPort(addr); err == nil {
		addr = ascii
	}
	if host, port, err := net.SplitHostPort(addr); err == nil {
		if host == "" {
			return "", fmt.Errorf("host is required")
		}
		if port == "" {
			return net.JoinHostPort(host, "443"), nil
		}
		return addr, nil
	}
	if !hasPort(addr) {
		addr += ":443"
	}
	return addr, nil
}

// authorityFromReq selects req.Host, then the Host header, then req.URL.Host.
// It punycodes the selected authority when possible and otherwise preserves it.
func authorityFromReq(req *http.Request) string {
	authority := req.Host
	if authority == "" {
		authority = req.Header.Get("Host")
	}
	if authority == "" {
		authority = req.URL.Host
	}
	if ascii, err := httpguts.PunycodeHostPort(authority); err == nil {
		authority = ascii
	}
	return authority
}
