package tth2

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"golang.org/x/net/http2"
)

// ConnectionError reports failure to establish a TLS+HTTP/2 connection.
// Its cause remains available through errors.Is and errors.As.
type ConnectionError struct {
	Err error
}

// Error describes the connection establishment failure.
func (e *ConnectionError) Error() string { return e.Err.Error() }

// Unwrap returns the underlying establishment failure.
func (e *ConnectionError) Unwrap() error { return e.Err }

// ResponseBodyLimitError reports that a response crossed the configured DATA
// limit. RequestIndex is the request's zero-based position in the sent batch.
// BytesAccepted is the prefix hashed and counted before the batch was stopped.
// Inspect with errors.As.
type ResponseBodyLimitError struct {
	// RequestIndex is the request's zero-based position in the sent batch.
	RequestIndex int
	// StreamID is the HTTP/2 stream that crossed the limit.
	StreamID uint32
	// Limit is the configured maximum accepted DATA bytes per response.
	Limit int64
	// BytesAccepted is always equal to Limit when this error is returned.
	BytesAccepted int64
}

// Error describes the request and stream that crossed the limit.
func (e *ResponseBodyLimitError) Error() string {
	return fmt.Sprintf(
		"tth2: response body limit of %d bytes exceeded by request %d "+
			"(stream %d) after accepting %d bytes",
		e.Limit, e.RequestIndex, e.StreamID, e.BytesAccepted)
}

// BatchTimeoutError reports that a batch exceeded its configured wall-clock
// transport limit. Inspect with errors.As.
type BatchTimeoutError struct {
	// Limit is the configured per-batch timeout.
	Limit time.Duration
}

// Error describes the configured limit that elapsed.
func (e *BatchTimeoutError) Error() string {
	return fmt.Sprintf(
		"tth2: batch timeout of %d nanoseconds exceeded", e.Limit.Nanoseconds())
}

func isLocalBatchTermination(err error) bool {
	var bodyLimit *ResponseBodyLimitError
	var timeout *BatchTimeoutError
	return errors.As(err, &bodyLimit) || errors.As(err, &timeout)
}

func batchContextError(ctx context.Context) error {
	cause := context.Cause(ctx)
	if isLocalBatchTermination(cause) {
		return cause
	}
	return ctx.Err()
}

// ConnectionID identifies one physical connection within a [Transport]
// lifetime. Its zero value means that no connection was recorded. IDs expose
// identity only; they do not encode age, order, latency, or remote identity.
// Compare IDs only between batches from the same Transport.
type ConnectionID uint64

// Result holds the outcome of one HTTP/2 request in a batch. Its zero value
// has no response and did not arrive; rank invariants apply to returned
// batches. Copying a Result shares Response and Reset.
type Result struct {
	// Response is the response metadata and selected captured evidence, or nil
	// if the server reset this stream before sending its headers.
	Response *Response

	// ArrivalRank is this request's zero-based place among responses that
	// arrived. Ranks form a permutation of [0, arrivals).
	//
	// It is -1 only when the stream was reset before response headers arrived.
	ArrivalRank int

	// Reset reports that the server ended this stream with RST_STREAM, and is
	// nil otherwise. It is scoped to this one request: its siblings raced to
	// completion and their ranks stand.
	//
	// Two shapes, distinguished by whether the response headers arrived first.
	// Reset before headers: Response is nil and ArrivalRank is -1, since a
	// stream that never delivered headers never took a rank. Reset after
	// headers: the arrival was real, so Response and ArrivalRank hold, but the
	// body may be truncated.
	Reset *StreamError

	// arrivalSeq is the response HEADERS read sequence. Informational responses
	// can make these values sparse; newBatchResult converts them to dense
	// ranks.
	arrivalSeq int

	// StreamID is the HTTP/2 stream ID used for this request.
	StreamID uint32
}

// Response describes an accepted final HTTP/2 response. Header and Trailer are
// nil unless [WithResponseHeaderCapture] was selected. When present, the maps
// belong to the Response and may be mutated by the caller. Copying a Response
// shares these maps and Body.Captured. There is no live body reader to close.
type Response struct {
	// Status is the status code followed by its standard text, such as "200
	// OK".
	Status string
	// StatusCode is the numeric HTTP status code.
	StatusCode int
	// Header contains final response headers when capture is enabled.
	Header http.Header
	// Trailer contains response trailers when capture is enabled and any
	// arrive.
	Trailer http.Header
	// ContentLength is the unambiguous non-negative declared length, or -1 when
	// the Content-Length field is absent, malformed, or conflicting.
	ContentLength int64
	// Body describes the response DATA accepted by the transport.
	Body ResponseBody
}

// ResponseBody describes all accepted body bytes while retaining only the
// configured prefix in Captured. SHA256 covers BytesReceived bytes, including
// accepted bytes beyond Captured. On a reset it identifies only the accepted
// prefix, not bytes the peer never sent. A batch that crosses
// [WithMaxResponseBodyBytes] returns no ResponseBody.
type ResponseBody struct {
	// Captured is the retained body prefix. The response owns this slice.
	Captured []byte
	// BytesReceived is the total DATA accepted before completion or reset.
	BytesReceived int64
	// SHA256 is the digest of all BytesReceived bytes.
	SHA256 [sha256.Size]byte
}

// CapturedComplete reports whether the captured body prefix contains every
// accepted body byte. It does not report whether the response ended normally.
func (b ResponseBody) CapturedComplete() bool {
	return int64(len(b.Captured)) == b.BytesReceived
}

// Arrived reports whether final response headers arrived. A stream reset after
// final headers still arrived; a reset before them did not.
func (r Result) Arrived() bool { return r.Response != nil }

// BatchResult is the outcome of one batch send. Its zero value is an
// empty, complete batch. Returned slices and responses belong to the caller;
// copying the struct shares them. Reads may be concurrent while no goroutine
// mutates that storage.
type BatchResult struct {
	// Connection identifies the physical connection that carried this batch.
	// Repeated batches on an exclusive lease have the same value. A replacement
	// connection has a different value.
	Connection ConnectionID

	// Results holds one Result per request in input order.
	Results []Result

	// ArrivalOrder records the sequence in which final response headers
	// arrived: ArrivalOrder[rank] is the input index of the request at that
	// rank. It is the inverse of the per-request Results[i].ArrivalRank.
	//
	// It covers what arrived: normally every request, and one entry short per
	// stream the server reset before its headers (see [Result.Reset]), whose
	// request appears nowhere in it.
	ArrivalOrder []int
}

// AllResponsesArrived reports whether every request received final response
// headers. It says nothing about whether every response body completed.
func (b BatchResult) AllResponsesArrived() bool {
	for _, result := range b.Results {
		if !result.Arrived() {
			return false
		}
	}
	return true
}

// FullArrivalOrder returns ArrivalOrder when it is a complete, exact inverse of
// Results' ranks. The returned slice aliases ArrivalOrder. The boolean
// distinguishes an empty complete batch from an incomplete or malformed one.
func (b BatchResult) FullArrivalOrder() ([]int, bool) {
	if !b.AllResponsesArrived() || len(b.ArrivalOrder) != len(b.Results) {
		return nil, false
	}
	for rank, input := range b.ArrivalOrder {
		if input < 0 || input >= len(b.Results) ||
			b.Results[input].ArrivalRank != rank {
			return nil, false
		}
	}
	return b.ArrivalOrder, true
}

// newBatchResult assigns dense ranks and builds arrival order. results are in
// position order and become owned by the returned value.
func newBatchResult(results []Result) *BatchResult {
	order := make([]int, 0, len(results))
	for i := range results {
		r := &results[i]
		if r.Arrived() {
			if r.arrivalSeq < 0 {
				panic("tth2: negative internal arrival sequence")
			}
			order = append(order, i)
			continue
		}
		if r.Reset == nil {
			panic("tth2: result has neither response nor reset")
		}
		r.ArrivalRank = -1
	}
	sort.Slice(order, func(a, b int) bool {
		return results[order[a]].arrivalSeq < results[order[b]].arrivalSeq
	})
	for rank := 1; rank < len(order); rank++ {
		if results[order[rank-1]].arrivalSeq == results[order[rank]].arrivalSeq {
			panic("tth2: duplicate internal arrival sequence")
		}
	}
	for rank, i := range order {
		results[i].ArrivalRank = rank
	}
	return &BatchResult{
		Results:      results,
		ArrivalOrder: order,
	}
}

// StreamError reports that the server reset one stream with RST_STREAM. It is
// stream-scoped. During response collection, a partial batch accompanies the
// error and the connection remains reusable. During dispatch, partly sent
// siblings make the connection unusable and no batch is returned. A GOAWAY is
// connection-fatal and surfaces as an ordinary error. Inspect Result.Reset for
// every reset when more than one stream was reset; the returned error
// identifies one of them without a deterministic selection rule. Inspect the
// returned error with errors.As.
type StreamError struct {
	// StreamID identifies the reset HTTP/2 stream.
	StreamID uint32
	// Code is the peer's RST_STREAM error code.
	Code http2.ErrCode
}

// Error reports which stream the server reset and with what HTTP/2 error code.
func (e *StreamError) Error() string {
	return fmt.Sprintf("tth2: stream %d reset with code %v", e.StreamID, e.Code)
}
