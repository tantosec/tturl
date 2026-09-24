package tth2

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type resetSignallingWriter struct {
	bytes.Buffer
	once      sync.Once
	responses *responseRead
}

type recordSizeWriter struct {
	bytes.Buffer
	sizes []int
}

func (w *recordSizeWriter) Write(p []byte) (int, error) {
	w.sizes = append(w.sizes, len(p))
	return w.Buffer.Write(p)
}

func (w *resetSignallingWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	w.once.Do(func() {
		w.responses.failSend(&StreamError{
			StreamID: 1, Code: http2.ErrCodeRefusedStream,
		})
	})
	return n, err
}

func TestWriteHeaderBlockFramesAndBudget(t *testing.T) {
	t.Parallel()

	t.Run("splits continuation frames", func(t *testing.T) {
		t.Parallel()
		var wire bytes.Buffer
		bw := bufio.NewWriter(&wire)
		c := &http2conn{bw: bw, framer: http2.NewFramer(bw, nil)}
		c.peerMaxFrameSize.Store(5)
		block := []byte("0123456789")

		if err := c.writeHeaderBlock(1, block, true); err != nil {
			t.Fatalf("writeHeaderBlock: %v", err)
		}
		if err := bw.Flush(); err != nil {
			t.Fatalf("flush frames: %v", err)
		}

		reader := http2.NewFramer(nil, bytes.NewReader(wire.Bytes()))
		first, err := reader.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame HEADERS: %v", err)
		}
		headers, ok := first.(*http2.HeadersFrame)
		if !ok {
			t.Fatalf("first frame = %T, want *http2.HeadersFrame", first)
		}
		if !headers.StreamEnded() || headers.HeadersEnded() {
			t.Errorf("HEADERS end flags = %v/%v, want true/false",
				headers.StreamEnded(), headers.HeadersEnded())
		}
		if got := headers.HeaderBlockFragment(); !bytes.Equal(got, block[:5]) {
			t.Errorf("HEADERS fragment = %q, want %q", got, block[:5])
		}

		second, err := reader.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame CONTINUATION: %v", err)
		}
		continuation, ok := second.(*http2.ContinuationFrame)
		if !ok {
			t.Fatalf("second frame = %T, want *http2.ContinuationFrame", second)
		}
		if !continuation.HeadersEnded() {
			t.Error("CONTINUATION did not end the header block")
		}
		if got := continuation.HeaderBlockFragment(); !bytes.Equal(got, block[5:]) {
			t.Errorf("CONTINUATION fragment = %q, want %q", got, block[5:])
		}
	})

	t.Run("rejects aggregate overflow before flushing", func(t *testing.T) {
		t.Parallel()
		var wire bytes.Buffer
		bw := bufio.NewWriterSize(&wire, 2*tlsRecordPlaintextBudget)
		c := &http2conn{bw: bw, framer: http2.NewFramer(bw, nil)}
		c.peerMaxFrameSize.Store(maxDataFramePayload)

		if err := c.writeHeaderBlock(1, make([]byte, 8*1024), true); err != nil {
			t.Fatalf("first header block: %v", err)
		}
		if err := c.writeHeaderBlock(3, make([]byte, 8*1024), true); err == nil {
			t.Fatal("writeHeaderBlock accepted an oversized aggregate")
		}
		if wire.Len() != 0 {
			t.Fatalf("oversized aggregate flushed %d bytes", wire.Len())
		}
	})
}

func TestTailReleaseNeeds(t *testing.T) {
	t.Parallel()
	maxPayload := tlsRecordPlaintextBudget - http2FrameHeaderSize
	got, err := tailReleaseNeeds([][]byte{make([]byte, maxPayload)}, []int{0})
	if err != nil || len(got) != 1 || got[0] != int32(maxPayload) {
		t.Fatalf("tailReleaseNeeds at record limit = %v, %v", got, err)
	}
	if _, err := tailReleaseNeeds(
		[][]byte{make([]byte, 8*1024), make([]byte, 8*1024)},
		[]int{0, 0},
	); err == nil {
		t.Fatal("tailReleaseNeeds accepted an oversized aggregate")
	}
	if _, err := tailReleaseNeeds(
		[][]byte{make([]byte, tlsRecordPlaintextBudget)}, []int{0},
	); err == nil {
		t.Fatal("tailReleaseNeeds accepted an oversized stream")
	}
	if _, err := tailReleaseNeeds([][]byte{{1}}, []int{2}); err == nil {
		t.Fatal("tailReleaseNeeds accepted a pivot beyond the body")
	}
}

func TestSendDispatchTimes(t *testing.T) {
	t.Parallel()
	headerTime := time.Date(2026, 9, 8, 1, 2, 3, 4, time.UTC)
	bodyTime := headerTime.Add(time.Millisecond)
	tests := []struct {
		name       string
		bodies     []string
		explicit   []bool
		withheld   int
		wantBodyAt bool
	}{
		{name: "bodyless", bodies: []string{""}},
		{
			name: "explicit empty body", bodies: []string{""},
			explicit: []bool{true}, withheld: 1,
		},
		{
			name: "unwithheld body", bodies: []string{"abc"},
			explicit: []bool{true}, wantBodyAt: true,
		},
		{
			name: "withheld body", bodies: []string{"abc"},
			explicit: []bool{true}, withheld: 1, wantBodyAt: true,
		},
		{
			name: "mixed bodyless and body", bodies: []string{"", "abc"},
			explicit: []bool{false, true}, withheld: 1, wantBodyAt: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var wire bytes.Buffer
			bw := bufio.NewWriterSize(&wire, 2*tlsRecordPlaintextBudget)
			clock := []time.Time{headerTime, bodyTime}
			clockCalls := 0
			conn := &http2conn{
				bw: bw, framer: http2.NewFramer(bw, nil),
				nextStreamID: 1,
				now: func() time.Time {
					at := clock[clockCalls]
					clockCalls++
					return at
				},
			}
			conn.peerMaxFrameSize.Store(maxDataFramePayload)
			conn.peerConnSendWindow.Store(65535)
			conn.peerInitStreamWindow.Store(65535)
			conn.hpackEnc = hpack.NewEncoder(&conn.hpackBuf)

			reqs := make([]*http.Request, len(test.bodies))
			streamIDs := make([]uint32, len(test.bodies))
			for i, body := range test.bodies {
				streamIDs[i] = uint32(2*i + 1)
				var reader io.Reader
				if test.explicit != nil && test.explicit[i] {
					reader = strings.NewReader(body)
				}
				req, err := http.NewRequestWithContext(
					context.Background(), http.MethodPost,
					"https://example.test/", reader,
				)
				if err != nil {
					t.Fatalf("NewRequest: %v", err)
				}
				if reader != nil && body == "" {
					req.Body = io.NopCloser(strings.NewReader(""))
					req.GetBody = func() (io.ReadCloser, error) {
						return io.NopCloser(strings.NewReader("")), nil
					}
				}
				reqs[i] = req
			}

			dispatch, err := conn.sendRequests(
				context.Background(), reqs, streamIDs, 0, test.withheld,
				nil, nil,
			)
			if err != nil {
				t.Fatalf("sendRequests: %v", err)
			}
			if dispatch.HeadersSentAt != headerTime {
				t.Errorf("HeadersSentAt = %v, want %v",
					dispatch.HeadersSentAt, headerTime)
			}
			wantCalls := 1
			if test.wantBodyAt {
				wantCalls = 2
				if dispatch.BodyFinalisedAt != bodyTime {
					t.Errorf("BodyFinalisedAt = %v, want %v",
						dispatch.BodyFinalisedAt, bodyTime)
				}
			} else if !dispatch.BodyFinalisedAt.IsZero() {
				t.Errorf("BodyFinalisedAt = %v, want zero",
					dispatch.BodyFinalisedAt)
			}
			if clockCalls != wantCalls {
				t.Errorf("clock calls = %d, want %d", clockCalls, wantCalls)
			}
		})
	}
}

// TestSendGatePrecedesFirstFlush verifies the pacing seam for every send
// shape. A rejecting gate must observe fully buffered HEADERS while the wire
// remains empty, and it must prevent every later body phase.
func TestSendGatePrecedesFirstFlush(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("gate closed")
	tests := []struct {
		name     string
		body     string
		withheld int
	}{
		{name: "no body", withheld: 1},
		{name: "body unwithheld", body: "abc", withheld: 0},
		{name: "body with tail release", body: "abc", withheld: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var wire bytes.Buffer
			bw := bufio.NewWriterSize(&wire, 2*tlsRecordPlaintextBudget)
			conn := &http2conn{
				bw:           bw,
				framer:       http2.NewFramer(bw, nil),
				nextStreamID: 1,
			}
			conn.peerMaxFrameSize.Store(maxDataFramePayload)
			conn.hpackEnc = hpack.NewEncoder(&conn.hpackBuf)

			method := http.MethodGet
			var body io.Reader
			if test.body != "" {
				method = http.MethodPost
				body = strings.NewReader(test.body)
			}
			request, err := http.NewRequestWithContext(
				context.Background(), method, "https://example.test/", body)
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			calls := 0
			_, err = conn.sendRequests(
				context.Background(), []*http.Request{request}, []uint32{1}, 0,
				test.withheld, func() error {
					calls++
					if bw.Buffered() != 0 {
						t.Errorf("gate observed %d buffered bytes", bw.Buffered())
					}
					if wire.Len() != 0 {
						t.Errorf("gate observed %d bytes already on wire", wire.Len())
					}
					return wantErr
				}, nil)
			if !errors.Is(err, wantErr) {
				t.Fatalf("sendRequests error = %v, want %v", err, wantErr)
			}
			if calls != 1 {
				t.Errorf("gate calls = %d, want 1", calls)
			}
			if wire.Len() != 0 {
				t.Errorf("rejected send wrote %d bytes", wire.Len())
			}
		})
	}
}

func TestAllEmptyBodiesSkipDelayedRelease(t *testing.T) {
	t.Parallel()
	var wire bytes.Buffer
	bw := bufio.NewWriterSize(&wire, 2*tlsRecordPlaintextBudget)
	conn := &http2conn{
		bw:           bw,
		framer:       http2.NewFramer(bw, nil),
		nextStreamID: 1,
	}
	conn.peerMaxFrameSize.Store(maxDataFramePayload)
	conn.hpackEnc = hpack.NewEncoder(&conn.hpackBuf)

	request, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, "https://example.test/",
		strings.NewReader(""))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Body = io.NopCloser(strings.NewReader(""))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("")), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := conn.sendRequests(
		ctx, []*http.Request{request}, []uint32{1}, time.Hour, 1, func() error {
			cancel()
			return nil
		}, nil,
	); err != nil {
		t.Fatalf("sendRequests with an empty body: %v", err)
	}
	if wire.Len() == 0 {
		t.Fatal("sendRequests wrote no HEADERS")
	}
}

func TestBodySendStopsAtRecordBoundaryAfterReset(t *testing.T) {
	t.Parallel()
	responses := &responseRead{sendErr: make(chan error, 1)}
	wire := &resetSignallingWriter{responses: responses}
	bw := bufio.NewWriterSize(wire, 2*tlsRecordPlaintextBudget)
	conn := &http2conn{bw: bw, framer: http2.NewFramer(bw, nil)}
	conn.peerMaxFrameSize.Store(maxDataFramePayload)
	conn.peerConnSendWindow.Store(1 << 20)
	conn.peerInitStreamWindow.Store(1 << 20)
	flow := newFlowControl(conn, []uint32{1})
	body := bytes.Repeat([]byte("x"), 4*tlsRecordPlaintextBudget)

	_, err := conn.sendFlowControlledBody(
		context.Background(), flow, [][]byte{body}, true, "body", responses,
	)
	var reset *StreamError
	if !errors.As(err, &reset) || reset.StreamID != 1 {
		t.Fatalf("sendFlowControlledBody error = %v, want stream 1 reset", err)
	}

	reader := http2.NewFramer(nil, bytes.NewReader(wire.Bytes()))
	dataBytes := 0
	for {
		frame, readErr := reader.ReadFrame()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			t.Fatalf("ReadFrame: %v", readErr)
		}
		if data, ok := frame.(*http2.DataFrame); ok {
			dataBytes += len(data.Data())
			if data.StreamEnded() {
				t.Error("reset body send emitted END_STREAM")
			}
		}
	}
	if dataBytes == 0 || dataBytes >= len(body) {
		t.Errorf("body bytes sent = %d, want a non-zero prefix below %d",
			dataBytes, len(body))
	}
}

func TestCancelledTerminalFrameDoesNotCompleteStream(t *testing.T) {
	t.Parallel()

	cause := errors.New("cancel terminal delivery")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	coll := &streamCollector{ch: make(chan frameEvent)}
	conn := &http2conn{collectors: map[uint32]*streamCollector{1: coll}}
	read := &responseRead{c: conn, ctx: ctx}
	frame := &http2.MetaHeadersFrame{
		HeadersFrame: &http2.HeadersFrame{FrameHeader: http2.FrameHeader{
			StreamID: 1,
			Flags:    http2.FlagHeadersEndStream,
		}},
		Fields: []hpack.HeaderField{{Name: ":status", Value: "200"}},
	}

	if err := read.dispatchFrame(frame); !errors.Is(err, cause) {
		t.Fatalf("dispatchFrame error = %v, want %v", err, cause)
	}
	conn.mu.Lock()
	_, registered := conn.collectors[1]
	conn.mu.Unlock()
	if !registered {
		t.Fatal("cancelled terminal frame retired its collector as complete")
	}

	result, err := conn.collectStream(read, 0, 1, coll, sendConfig{})
	if !errors.Is(err, cause) {
		t.Fatalf("collectStream error = %v, want %v", err, cause)
	}
	if result.Response != nil {
		t.Fatal("cancelled terminal frame manufactured a response")
	}
}

func TestFlowControlledBodyFragmentsStayWithinRecordBudget(t *testing.T) {
	t.Parallel()
	wire := &recordSizeWriter{}
	bw := bufio.NewWriterSize(wire, 2*tlsRecordPlaintextBudget)
	conn := &http2conn{bw: bw, framer: http2.NewFramer(bw, nil)}
	conn.peerMaxFrameSize.Store(maxDataFramePayload)
	conn.peerConnSendWindow.Store(1 << 20)
	flow := &flowControl{
		c:         conn,
		stream:    []int32{8 << 10},
		streamIDs: []uint32{1},
		changed:   make(chan struct{}, 1),
	}
	data := [][]byte{bytes.Repeat([]byte("x"), tlsRecordPlaintextBudget-10)}
	sent := make([]int, 1)
	record := conn.newRecordWriter()
	defer record.close()

	if err := conn.sendBytesUpTo(
		context.Background(), flow, data, sent, 0, 8<<10, false, nil,
		record,
	); err != nil {
		t.Fatalf("first body fragment: %v", err)
	}
	const remainingCredit = int32(tlsRecordPlaintextBudget - 10 - (8 << 10))
	if err := flow.addStream(1, remainingCredit); err != nil {
		t.Fatalf("addStream: %v", err)
	}
	if err := conn.sendBytesUpTo(
		context.Background(), flow, data, sent, 0, len(data[0]), true, nil,
		record,
	); err != nil {
		t.Fatalf("second body fragment: %v", err)
	}
	if err := record.flush("fragmented body final record"); err != nil {
		t.Fatalf("final flush: %v", err)
	}

	if len(wire.sizes) != 2 {
		t.Fatalf("record writes = %v, want two records", wire.sizes)
	}
	for i, size := range wire.sizes {
		if size > tlsRecordPlaintextBudget {
			t.Errorf("record %d size = %d, want <= %d",
				i, size, tlsRecordPlaintextBudget)
		}
	}
	reader := http2.NewFramer(nil, bytes.NewReader(wire.Bytes()))
	var got []byte
	frames := 0
	for {
		frame, err := reader.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		body, ok := frame.(*http2.DataFrame)
		if !ok {
			t.Fatalf("frame %d = %T, want *http2.DataFrame", frames, frame)
		}
		frames++
		got = append(got, body.Data()...)
		if body.StreamEnded() != (frames == 2) {
			t.Errorf("frame %d END_STREAM = %v", frames, body.StreamEnded())
		}
	}
	if frames != 2 {
		t.Errorf("DATA frames = %d, want 2", frames)
	}
	if !bytes.Equal(got, data[0]) {
		t.Errorf("reassembled body length = %d, want intact %d-byte body",
			len(got), len(data[0]))
	}
}

func TestRequestBodyLengthPreservesAbsentAndEmpty(t *testing.T) {
	t.Parallel()
	bodyless, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, "https://example.test/", nil)
	if err != nil {
		t.Fatalf("bodyless NewRequest: %v", err)
	}
	empty, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, "https://example.test/", nil)
	if err != nil {
		t.Fatalf("empty NewRequest: %v", err)
	}
	empty.Body = io.NopCloser(strings.NewReader(""))
	if got := requestBodyLength(bodyless, nil); got != -1 {
		t.Errorf("bodyless length = %d, want -1", got)
	}
	if got := requestBodyLength(empty, nil); got != 0 {
		t.Errorf("explicit empty body length = %d, want 0", got)
	}
}
