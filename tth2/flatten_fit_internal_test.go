package tth2

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// flushRecordingWriter records the plaintext handed to each bufio flush. The
// production TLS writer makes each bounded flush one application-data record.
type flushRecordingWriter struct {
	records [][]byte
	onWrite func(int)
}

func (w *flushRecordingWriter) Write(p []byte) (int, error) {
	w.records = append(w.records, bytes.Clone(p))
	if w.onWrite != nil {
		w.onWrite(len(w.records))
	}
	return len(p), nil
}

type bodyFrame struct {
	stream uint32
	data   string
	end    bool
}

func bodyFramesInRecord(t *testing.T, record []byte) (headers []uint32, data []bodyFrame) {
	t.Helper()
	framer := http2.NewFramer(nil, bytes.NewReader(record))
	for {
		frame, err := framer.ReadFrame()
		if errors.Is(err, io.EOF) {
			return headers, data
		}
		if err != nil {
			t.Fatalf("decode record: %v", err)
		}
		switch f := frame.(type) {
		case *http2.HeadersFrame:
			headers = append(headers, f.StreamID)
		case *http2.DataFrame:
			data = append(data, bodyFrame{f.StreamID, string(f.Data()), f.StreamEnded()})
		default:
			t.Fatalf("unexpected frame %T in body record", f)
		}
	}
}

func makeFlattenFitConn(w *flushRecordingWriter) *http2conn {
	bw := bufio.NewWriterSize(w, 2*tlsRecordPlaintextBudget)
	c := &http2conn{bw: bw, framer: http2.NewFramer(bw, nil)}
	c.hpackEnc = hpack.NewEncoder(&c.hpackBuf)
	c.peerMaxFrameSize.Store(maxDataFramePayload)
	c.peerConnSendWindow.Store(65535)
	c.peerInitStreamWindow.Store(65535)
	return c
}

func TestFlattenFitRecordMembership(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		withheld int
		want     [][]bodyFrame
	}{
		{
			name: "zero", withheld: 0,
			want: [][]bodyFrame{{{1, "A123", true}, {3, "B12345", true}}},
		},
		{
			name: "one", withheld: 1,
			want: [][]bodyFrame{
				{{1, "A12", false}, {3, "B1234", false}},
				{{1, "3", true}, {3, "5", true}},
			},
		},
		{
			name: "three", withheld: 3,
			want: [][]bodyFrame{
				{{1, "A", false}, {3, "B12", false}},
				{{1, "123", true}, {3, "345", true}},
			},
		},
		{
			name: "covers whole bodies", withheld: 6,
			want: [][]bodyFrame{{{1, "A123", true}, {3, "B12345", true}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			wire := &flushRecordingWriter{}
			conn := makeFlattenFitConn(wire)
			reqs := make([]*http.Request, 2)
			for i, body := range []string{"A123", "B12345"} {
				var err error
				reqs[i], err = http.NewRequestWithContext(context.Background(),
					http.MethodPost, "https://example.test/", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := conn.sendRequests(context.Background(), reqs,
				[]uint32{1, 3}, 0, test.withheld, nil, nil); err != nil {
				t.Fatalf("sendRequests: %v", err)
			}
			if got, want := len(wire.records), 1+len(test.want); got != want {
				t.Fatalf("records = %d, want %d", got, want)
			}
			headers, data := bodyFramesInRecord(t, wire.records[0])
			if len(headers) != 2 || headers[0] != 1 || headers[1] != 3 || len(data) != 0 {
				t.Fatalf("first record headers=%v DATA=%v; want only HEADERS for streams 1,3", headers, data)
			}
			for i, want := range test.want {
				headers, got := bodyFramesInRecord(t, wire.records[i+1])
				if len(headers) != 0 || !equalBodyFrames(got, want) {
					t.Errorf("body record %d headers=%v DATA=%v; want DATA=%v", i, headers, got, want)
				}
			}
		})
	}
}

func equalBodyFrames(a, b []bodyFrame) bool {
	return slices.Equal(a, b)
}

func TestFlattenFitRecordBoundaryAndCredit(t *testing.T) {
	t.Parallel()
	const exactBody = (tlsRecordPlaintextBudget - 2*http2FrameHeaderSize) / 2
	for _, test := range []struct {
		name        string
		lengths     []int
		credit      int32
		wantFitting bool
	}{
		{"exact capacity", []int{exactBody, exactBody}, 65535, true},
		{"one byte over", []int{exactBody + 1, exactBody}, 65535, false},
		{"insufficient connection credit", []int{4, 6}, 9, false},
		{"insufficient stream credit", []int{4, 6}, 65535, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			wire := &flushRecordingWriter{}
			conn := makeFlattenFitConn(wire)
			conn.peerConnSendWindow.Store(test.credit)
			data := [][]byte{bytes.Repeat([]byte("a"), test.lengths[0]), bytes.Repeat([]byte("b"), test.lengths[1])}
			flow := newFlowControl(conn, []uint32{1, 3})
			if test.name == "insufficient stream credit" {
				flow.stream[1] = 5
			}
			beforeConn, beforeStream := conn.peerConnSendWindow.Load(), append([]int32(nil), flow.stream...)
			_, fitting, err := conn.sendFittingBodyRecord(flow, data, true, "body", nil)
			if err != nil || fitting != test.wantFitting {
				t.Fatalf("sendFittingBodyRecord fitting=%v err=%v, want fitting=%v", fitting, err, test.wantFitting)
			}
			if !fitting {
				if len(wire.records) != 0 || conn.bw.Buffered() != 0 ||
					conn.peerConnSendWindow.Load() != beforeConn ||
					flow.stream[0] != beforeStream[0] || flow.stream[1] != beforeStream[1] {
					t.Fatalf("failed fit changed wire, buffer, or flow credit")
				}
				return
			}
			if len(wire.records) != 1 || len(wire.records[0]) != tlsRecordPlaintextBudget {
				t.Fatalf("exact fit records=%d bytes=%d, want one %d-byte record",
					len(wire.records), len(wire.records[0]), tlsRecordPlaintextBudget)
			}
			_, frames := bodyFramesInRecord(t, wire.records[0])
			if len(frames) != 2 || !frames[0].end || !frames[1].end ||
				len(frames[0].data) != exactBody || len(frames[1].data) != exactBody {
				t.Fatalf("exact fit frames=%v", frames)
			}
		})
	}
}

func TestFlattenFitRetainsBodyStreamCap(t *testing.T) {
	t.Parallel()
	wire := &flushRecordingWriter{}
	conn := makeFlattenFitConn(wire)
	data := make([][]byte, maxBodyCarryingStreams+1)
	ids := make([]uint32, len(data))
	for i := range data {
		data[i] = []byte{'x'}
		ids[i] = uint32(2*i + 1)
	}
	flow := newFlowControl(conn, ids)
	_, _, err := conn.sendFittingBodyRecord(flow, data, true, "body", nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds the cap") {
		t.Fatalf("sendFittingBodyRecord error = %v, want stream cap", err)
	}
	if len(wire.records) != 0 || conn.peerConnSendWindow.Load() != 65535 {
		t.Fatalf("rejected fitting record changed wire or connection credit")
	}
}

func TestFullyWithheldBodiesRetainStreamCap(t *testing.T) {
	t.Parallel()
	wire := &flushRecordingWriter{}
	conn := makeFlattenFitConn(wire)
	reqs := make([]*http.Request, maxBodyCarryingStreams+1)
	ids := make([]uint32, len(reqs))
	for i := range reqs {
		var err error
		reqs[i], err = http.NewRequestWithContext(context.Background(),
			http.MethodPost, "https://example.test/", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = uint32(2*i + 1)
	}
	_, err := conn.sendWithBody(context.Background(), reqs, ids, 0, 1, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds the cap") {
		t.Fatalf("fully withheld Send error = %v, want stream cap", err)
	}
	if len(wire.records) != 0 {
		t.Fatal("rejected batch wrote headers")
	}
}

func TestFlattenFitOversizeFallsBackWithSharedCompletion(t *testing.T) {
	t.Parallel()
	const exactBody = (tlsRecordPlaintextBudget - 2*http2FrameHeaderSize) / 2
	wire := &flushRecordingWriter{}
	conn := makeFlattenFitConn(wire)
	reqs := make([]*http.Request, 2)
	for i, length := range []int{exactBody + 1, exactBody} {
		var err error
		reqs[i], err = http.NewRequestWithContext(context.Background(),
			http.MethodPost, "https://example.test/",
			bytes.NewReader(bytes.Repeat([]byte{byte('a' + i)}, length)))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.sendRequests(context.Background(), reqs,
		[]uint32{1, 3}, 0, 0, nil, nil); err != nil {
		t.Fatalf("sendRequests: %v", err)
	}
	if len(wire.records) <= 2 {
		t.Fatalf("records = %d, want headers, streamed bulk, and shared final record", len(wire.records))
	}
	var sent [2]int
	for i, record := range wire.records[1:] {
		if len(record) > tlsRecordPlaintextBudget {
			t.Errorf("body record %d has %d plaintext bytes", i, len(record))
		}
		headers, frames := bodyFramesInRecord(t, record)
		if len(headers) != 0 {
			t.Fatalf("body record %d contains HEADERS %v", i, headers)
		}
		for _, frame := range frames {
			idx := int((frame.stream - 1) / 2)
			if idx < 0 || idx > 1 {
				t.Fatalf("unexpected DATA stream %d", frame.stream)
			}
			sent[idx] += len(frame.data)
			if frame.end && i != len(wire.records)-2 {
				t.Errorf("stream %d ended before final record", frame.stream)
			}
		}
	}
	if sent != [2]int{exactBody + 1, exactBody} {
		t.Errorf("sent body lengths = %v", sent)
	}
	_, final := bodyFramesInRecord(t, wire.records[len(wire.records)-1])
	if len(final) < 2 {
		t.Fatalf("final record has %d DATA frames, want at least two", len(final))
	}
	ends := final[len(final)-2:]
	if !ends[0].end || !ends[1].end || ends[0].stream != 1 ||
		ends[1].stream != 3 || ends[0].data == "" || ends[1].data == "" {
		t.Errorf("last two DATA frames = %+v, want nonempty END_STREAM for both streams", ends)
	}
}

func TestOversizedPrefixesWithholdExactlyThreeBytes(t *testing.T) {
	t.Parallel()
	const exactBody = (tlsRecordPlaintextBudget - 2*http2FrameHeaderSize) / 2
	wire := &flushRecordingWriter{}
	conn := makeFlattenFitConn(wire)
	reqs := make([]*http.Request, 2)
	for i, size := range []int{exactBody + 4, exactBody + 3} {
		var err error
		reqs[i], err = http.NewRequestWithContext(context.Background(),
			http.MethodPost, "https://example.test/",
			bytes.NewReader(bytes.Repeat([]byte{byte('a' + i)}, size)))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.sendRequests(context.Background(), reqs,
		[]uint32{1, 3}, 0, 3, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(wire.records) != 4 {
		t.Fatalf("records = %d, want headers, two prefix records, tail", len(wire.records))
	}
	var prefixes [2]int
	for _, record := range wire.records[1:3] {
		_, frames := bodyFramesInRecord(t, record)
		for _, frame := range frames {
			if frame.end {
				t.Errorf("prefix stream %d ended early", frame.stream)
			}
			prefixes[(frame.stream-1)/2] += len(frame.data)
		}
	}
	if prefixes != [2]int{exactBody + 1, exactBody} {
		t.Errorf("prefix lengths = %v, want [%d %d]", prefixes, exactBody+1, exactBody)
	}
	_, tails := bodyFramesInRecord(t, wire.records[3])
	if !equalBodyFrames(tails, []bodyFrame{
		{1, "aaa", true}, {3, "bbb", true},
	}) {
		t.Errorf("tail frames = %v, want exactly three bytes each", tails)
	}
}

func TestGreedyPositivePrefixesAndExactSharedTails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		sizes    []int
		withheld int
	}{
		{"small-one", []int{4, 6}, 1},
		{"small-three", []int{4, 6}, 3},
		{"asymmetric-one", []int{128, 32768}, 1},
		{"asymmetric-three", []int{128, 32768}, 3},
		{"wide-one", repeatedSizes(33, 512), 1},
		{"wide-three", repeatedSizes(33, 512), 3},
		{"shorter-than-tail", []int{2, 30000}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wire := &flushRecordingWriter{}
			conn := makeFlattenFitConn(wire)
			reqs := make([]*http.Request, len(tc.sizes))
			want := make([]string, len(tc.sizes))
			for i, size := range tc.sizes {
				want[i] = string(bytes.Repeat([]byte{byte('a' + i%26)}, size))
				var err error
				reqs[i], err = http.NewRequestWithContext(context.Background(),
					http.MethodPost, "https://example.test/", strings.NewReader(want[i]))
				if err != nil {
					t.Fatal(err)
				}
			}
			ids := make([]uint32, len(reqs))
			for i := range ids {
				ids[i] = uint32(2*i + 1)
			}
			if _, err := conn.sendRequests(context.Background(), reqs, ids,
				0, tc.withheld, nil, nil); err != nil {
				t.Fatal(err)
			}
			if len(wire.records) < 2 {
				t.Fatalf("records = %d, want headers and tail", len(wire.records))
			}
			got := make([]string, len(reqs))
			ended := make([]bool, len(reqs))
			for ri, record := range wire.records[1:] {
				if len(record) > tlsRecordPlaintextBudget {
					t.Errorf("body record %d has %d plaintext bytes", ri, len(record))
				}
				_, frames := bodyFramesInRecord(t, record)
				for _, frame := range frames {
					i := int((frame.stream - 1) / 2)
					if frame.stream%2 != 1 || i >= len(reqs) || ended[i] {
						t.Fatalf("unexpected DATA stream %d in body record %d", frame.stream, ri)
					}
					got[i] += frame.data
					if frame.end {
						if ri != len(wire.records)-2 || len(frame.data) != min(tc.withheld, tc.sizes[i]) {
							t.Errorf("stream %d END in body record %d with %d tail bytes",
								frame.stream, ri, len(frame.data))
						}
						ended[i] = true
					}
				}
			}
			for i := range want {
				if got[i] != want[i] || !ended[i] {
					t.Errorf("stream %d: got %d/%d bytes, ended=%v", ids[i], len(got[i]), len(want[i]), ended[i])
				}
			}
		})
	}
}

// A short positive-withhold prefix has no implicit last-byte gate. It may
// finish while a longer prefix is still limited by flow credit; only the
// explicit withheld body tail is held for the final END_STREAM record.
func TestGreedyPositivePrefixMayFinishBeforeOtherPrefixes(t *testing.T) {
	t.Parallel()
	wire := &flushRecordingWriter{}
	conn := makeFlattenFitConn(wire)
	conn.peerInitStreamWindow.Store(1024)
	flow := newFlowControl(conn, []uint32{1, 3})
	wire.onWrite = func(record int) {
		if record == 1 {
			if err := flow.addStream(3, 32768); err != nil {
				t.Errorf("replenish long prefix credit: %v", err)
			}
		}
	}
	data := [][]byte{[]byte("abc"), bytes.Repeat([]byte{'x'}, 32768)}
	if _, err := conn.sendFlowControlledBody(context.Background(), flow,
		data, false, "prefix", nil); err != nil {
		t.Fatal(err)
	}
	if len(wire.records) < 2 {
		t.Fatalf("prefix records = %d, want at least two", len(wire.records))
	}
	_, first := bodyFramesInRecord(t, wire.records[0])
	foundShort := false
	for _, frame := range first {
		if frame.stream == 1 {
			foundShort = true
			if frame.data != "abc" || frame.end {
				t.Errorf("first short-prefix frame = %+v, want all three bytes without END", frame)
			}
		}
	}
	if !foundShort {
		t.Error("short prefix did not finish in first record")
	}
	for _, record := range wire.records[1:] {
		_, frames := bodyFramesInRecord(t, record)
		for _, frame := range frames {
			if frame.stream == 1 || frame.end {
				t.Errorf("later prefix frame = %+v, want only unfinished long prefix", frame)
			}
		}
	}
}

func TestFlattenFitMixedEmptyBodiesEndOnHeaders(t *testing.T) {
	t.Parallel()
	wire := &flushRecordingWriter{}
	conn := makeFlattenFitConn(wire)
	makeReq := func(method string, body io.Reader) *http.Request {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(),
			method, "https://example.test/", body)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	reqs := []*http.Request{
		makeReq(http.MethodGet, nil),
		makeReq(http.MethodPost, strings.NewReader("")),
		makeReq(http.MethodPost, strings.NewReader("body")),
	}
	if _, err := conn.sendRequests(context.Background(), reqs,
		[]uint32{1, 3, 5}, 0, 0, nil, nil); err != nil {
		t.Fatalf("sendRequests: %v", err)
	}
	if len(wire.records) != 2 {
		t.Fatalf("records = %d, want HEADERS then fitting DATA", len(wire.records))
	}
	framer := http2.NewFramer(nil, bytes.NewReader(wire.records[0]))
	var headerEnds []bool
	for {
		frame, err := framer.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read HEADERS record: %v", err)
		}
		headers, ok := frame.(*http2.HeadersFrame)
		if !ok {
			t.Fatalf("header record contains %T", frame)
		}
		headerEnds = append(headerEnds, headers.StreamEnded())
	}
	if len(headerEnds) != 3 || !headerEnds[0] || !headerEnds[1] || headerEnds[2] {
		t.Errorf("HEADERS END_STREAM flags = %v, want [true true false]", headerEnds)
	}
	_, frames := bodyFramesInRecord(t, wire.records[1])
	if !equalBodyFrames(frames, []bodyFrame{{5, "body", true}}) {
		t.Errorf("body record DATA = %v, want only stream 5 with END_STREAM", frames)
	}
}

// TestThirtyThreeStreamRecordBoundary checks the transition from a fitting
// release to the greedy sender at the exact TLS plaintext boundary. Both
// sides must end all streams in the same final record with nonempty DATA.
func TestThirtyThreeStreamRecordBoundary(t *testing.T) {
	t.Parallel()
	const streams = 33
	const exactFirst = tlsRecordPlaintextBudget -
		streams*http2FrameHeaderSize - (streams - 1)
	for _, tc := range []struct {
		name      string
		firstSize int
		fitting   bool
	}{
		{"exact capacity", exactFirst, true},
		{"one byte over", exactFirst + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wire := &flushRecordingWriter{}
			conn := makeFlattenFitConn(wire)
			reqs := make([]*http.Request, streams)
			ids := make([]uint32, streams)
			for i := range reqs {
				ids[i] = uint32(2*i + 1)
				body := []byte{'x'}
				if i == 0 {
					body = bytes.Repeat([]byte{'x'}, tc.firstSize)
				}
				var err error
				reqs[i], err = http.NewRequestWithContext(context.Background(),
					http.MethodPost, "https://example.test/", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := conn.sendRequests(context.Background(), reqs, ids, 0, 0, nil, nil); err != nil {
				t.Fatal(err)
			}
			if len(wire.records) < 2 {
				t.Fatalf("record count = %d, want headers and DATA", len(wire.records))
			}
			if tc.fitting && len(wire.records) != 2 {
				t.Fatalf("exact fit records = %d, want two", len(wire.records))
			}
			if !tc.fitting && len(wire.records) <= 2 {
				t.Fatalf("over-capacity records = %d, want intermediate payload", len(wire.records))
			}
			var totals [streams]int
			var finalEnds [streams]bool
			for recordIndex, record := range wire.records[1:] {
				if len(record) > tlsRecordPlaintextBudget {
					t.Errorf("record %d size = %d", recordIndex, len(record))
				}
				_, frames := bodyFramesInRecord(t, record)
				for _, frame := range frames {
					index := int((frame.stream - 1) / 2)
					if index < 0 || index >= streams {
						t.Fatalf("unexpected stream %d", frame.stream)
					}
					totals[index] += len(frame.data)
					if frame.end {
						if recordIndex != len(wire.records)-2 || finalEnds[index] {
							t.Errorf("stream %d ended outside shared final gate", frame.stream)
						}
						if frame.data == "" {
							t.Errorf("stream %d used empty END_STREAM DATA", frame.stream)
						}
						finalEnds[index] = true
					}
				}
			}
			for i := range totals {
				want := 1
				if i == 0 {
					want = tc.firstSize
				}
				if totals[i] != want || !finalEnds[i] {
					t.Errorf("stream %d sent %d bytes, ended=%v; want %d and true",
						ids[i], totals[i], finalEnds[i], want)
				}
			}
		})
	}
}

// TestFlattenedBodyMatrix checks complete payload reconstruction and the final
// record gate across stream-count boundaries, record limits, and unequal bodies.
func TestFlattenedBodyMatrix(t *testing.T) {
	t.Parallel()
	fill := func(n, size int) []int {
		lengths := make([]int, n)
		for i := range lengths {
			lengths[i] = size
		}
		return lengths
	}
	cases := [][]int{
		{1},
		{16375},
		{16376},
		{20000},
		{4, 6},
		{8183, 8183},
		{8184, 8183},
		{1, 20000},
		fill(32, 500), fill(33, 500),
	}
	for _, width := range []int{1, 2, 32, 33} {
		for variant := range 3 {
			lengths := make([]int, width)
			state := uint32(0x4f5c92a3) + uint32(width*7+variant) //nolint:gosec // test widths and variants are bounded
			for i := range lengths {
				state = state*1664525 + 1013904223
				lengths[i] = 1 + int(state%700)
			}
			if width > 2 && variant == 2 {
				lengths[0] = 12000
			}
			cases = append(cases, lengths)
		}
	}
	for caseIndex, lengths := range cases {
		minLen := lengths[0]
		for _, size := range lengths[1:] {
			minLen = min(minLen, size)
		}
		for _, withheld := range []int{0, 1, 3, minLen} {
			tailBytes := 0
			if withheld > 0 {
				for _, size := range lengths {
					tailBytes += http2FrameHeaderSize + min(withheld, size)
				}
			}
			if tailBytes > tlsRecordPlaintextBudget {
				continue // this tail is intentionally rejected before sending
			}
			t.Run(fmt.Sprintf("case=%d/N=%d", caseIndex, withheld), func(t *testing.T) {
				wire := &flushRecordingWriter{}
				conn := makeFlattenFitConn(wire)
				reqs := make([]*http.Request, len(lengths))
				ids := make([]uint32, len(lengths))
				want := make([][]byte, len(lengths))
				for i, size := range lengths {
					ids[i] = uint32(2*i + 1)
					want[i] = make([]byte, size)
					for j := range want[i] {
						want[i][j] = byte(i ^ j) //nolint:gosec // synthetic payload deliberately wraps
					}
					var err error
					reqs[i], err = http.NewRequestWithContext(context.Background(),
						http.MethodPost, "https://example.test/", bytes.NewReader(want[i]))
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, err := conn.sendRequests(context.Background(), reqs, ids,
					0, withheld, nil, nil); err != nil {
					t.Fatal(err)
				}
				if len(wire.records) < 2 {
					t.Fatalf("record count = %d", len(wire.records))
				}
				got := make([][]byte, len(lengths))
				ended := make([]bool, len(lengths))
				for recordIndex, record := range wire.records[1:] {
					if len(record) > tlsRecordPlaintextBudget {
						t.Errorf("record %d size = %d", recordIndex, len(record))
					}
					_, frames := bodyFramesInRecord(t, record)
					for _, frame := range frames {
						index := int((frame.stream - 1) / 2)
						if index < 0 || index >= len(lengths) {
							t.Fatalf("unexpected stream %d", frame.stream)
						}
						got[index] = append(got[index], frame.data...)
						if !frame.end {
							continue
						}
						if ended[index] || recordIndex != len(wire.records)-2 {
							t.Errorf("stream %d ended outside final gate", frame.stream)
						}
						ended[index] = true
						if withheld > 0 && len(frame.data) != min(withheld, lengths[index]) {
							t.Errorf("stream %d final tail = %d, want %d",
								frame.stream, len(frame.data), min(withheld, lengths[index]))
						}
						if withheld == 0 && len(frame.data) == 0 {
							t.Errorf("stream %d used empty END_STREAM DATA with N=0", frame.stream)
						}
					}
				}
				for i := range lengths {
					if !bytes.Equal(got[i], want[i]) || !ended[i] {
						t.Errorf("stream %d received %d/%d bytes; ended=%v",
							ids[i], len(got[i]), len(want[i]), ended[i])
					}
				}
			})
		}
	}
}

func TestGreedyThirtyThreeStreamsUseTwoBodyRecords(t *testing.T) {
	t.Parallel()
	wire := &flushRecordingWriter{}
	conn := makeFlattenFitConn(wire)
	const streams = 33
	reqs := make([]*http.Request, streams)
	ids := make([]uint32, streams)
	for i := range reqs {
		ids[i] = uint32(2*i + 1)
		var err error
		reqs[i], err = http.NewRequestWithContext(context.Background(),
			http.MethodPost, "https://example.test/", bytes.NewReader(bytes.Repeat([]byte{'x'}, 512)))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.sendRequests(context.Background(), reqs, ids, 0, 0, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(wire.records) != 3 {
		t.Fatalf("records = %d, want headers and two body records", len(wire.records))
	}
	if len(wire.records[1]) != tlsRecordPlaintextBudget {
		t.Errorf("first body record size = %d, want %d", len(wire.records[1]), tlsRecordPlaintextBudget)
	}
	if len(wire.records[2]) != 1106 {
		t.Errorf("final body record size = %d, want 1106", len(wire.records[2]))
	}
	_, first := bodyFramesInRecord(t, wire.records[1])
	_, final := bodyFramesInRecord(t, wire.records[2])
	if len(first) != streams || len(final) != streams {
		t.Fatalf("body frames = %d then %d, want %d each", len(first), len(final), streams)
	}
	minPrefix, maxPrefix := 512, 0
	for i := range streams {
		minPrefix = min(minPrefix, len(first[i].data))
		maxPrefix = max(maxPrefix, len(first[i].data))
		if first[i].stream != ids[i] || first[i].end ||
			final[i].stream != ids[i] || !final[i].end || len(final[i].data) == 0 ||
			len(first[i].data)+len(final[i].data) != 512 {
			t.Errorf("stream %d frames: first length=%d end=%v, final length=%d end=%v",
				ids[i], len(first[i].data), first[i].end, len(final[i].data), final[i].end)
		}
	}
	if maxPrefix-minPrefix > 1 {
		t.Errorf("first-record prefix lengths range from %d to %d, want spread <= 1",
			minPrefix, maxPrefix)
	}
}

func TestGreedyAsymmetricFirstRecordProgress(t *testing.T) {
	t.Parallel()
	wire := &flushRecordingWriter{}
	conn := makeFlattenFitConn(wire)
	flow := newFlowControl(conn, []uint32{1, 3})
	data := [][]byte{bytes.Repeat([]byte{'a'}, 128), bytes.Repeat([]byte{'b'}, 32768)}
	if _, err := conn.sendFlowControlledBody(context.Background(), flow, data, true, "body", nil); err != nil {
		t.Fatal(err)
	}
	if len(wire.records) < 2 {
		t.Fatalf("body records = %d, want intermediate and final", len(wire.records))
	}
	_, first := bodyFramesInRecord(t, wire.records[0])
	if len(first) != 2 || first[0].end || first[1].end {
		t.Fatalf("first record frames = %v, want two intermediate DATA frames", first)
	}
	smallProgress := float64(len(first[0].data)) / 127
	largeProgress := float64(len(first[1].data)) / 32767
	if delta := smallProgress - largeProgress; delta < -0.05 || delta > 0.05 {
		t.Errorf("first-record normalised progress = %.3f vs %.3f", smallProgress, largeProgress)
	}
}

func TestGreedyCreditRecoveryFavoursLaggingStream(t *testing.T) {
	t.Parallel()
	wire := &flushRecordingWriter{}
	conn := makeFlattenFitConn(wire)
	flow := newFlowControl(conn, []uint32{1, 3})
	flow.stream[0] = 32
	wire.onWrite = func(recordCount int) {
		if recordCount == 1 {
			if err := flow.addStream(1, 20000); err != nil {
				t.Errorf("restore stream credit: %v", err)
			}
		}
	}
	data := [][]byte{bytes.Repeat([]byte{'a'}, 20000), bytes.Repeat([]byte{'b'}, 20000)}
	if _, err := conn.sendFlowControlledBody(context.Background(), flow, data, true, "body", nil); err != nil {
		t.Fatal(err)
	}
	if len(wire.records) < 3 {
		t.Fatalf("body records = %d, want two intermediate and final", len(wire.records))
	}
	_, first := bodyFramesInRecord(t, wire.records[0])
	_, second := bodyFramesInRecord(t, wire.records[1])
	if len(first) != 2 || len(second) != 2 || first[0].end || first[1].end ||
		second[0].end || second[1].end {
		t.Fatalf("first two records have unexpected frames: %v; %v", first, second)
	}
	if len(first[0].data) != 32 || len(second[0].data) <= len(second[1].data) {
		t.Errorf("credit recovery records: first=%d/%d, second=%d/%d; lagging stream did not catch up",
			len(first[0].data), len(first[1].data), len(second[0].data), len(second[1].data))
	}
}

func TestGreedySparseEligibleRecordPacking(t *testing.T) {
	t.Parallel()
	wire := &flushRecordingWriter{}
	conn := makeFlattenFitConn(wire)
	conn.peerConnSendWindow.Store(1 << 30)
	conn.peerInitStreamWindow.Store(1 << 30)
	const streams = maxBodyCarryingStreams
	ids := make([]uint32, streams)
	data := make([][]byte, streams)
	for i := range data {
		ids[i] = uint32(2*i + 1)
		data[i] = []byte{'x', 'y'}
	}
	data[0] = bytes.Repeat([]byte{'z'}, 1<<20)
	if _, err := conn.sendFlowControlledBody(context.Background(),
		newFlowControl(conn, ids), data, true, "body", nil); err != nil {
		t.Fatal(err)
	}
	if len(wire.records) < 2 {
		t.Fatalf("body records = %d, want multiple", len(wire.records))
	}
	if len(wire.records[0]) < 16320 {
		t.Fatalf("first record size = %d across %d records, want >= 16320",
			len(wire.records[0]), len(wire.records))
	}
	_, first := bodyFramesInRecord(t, wire.records[0])
	if len(first) != 1 || first[0].stream != 1 || first[0].end {
		t.Errorf("first record frames = %v, want only large stream DATA", first)
	}
	_, final := bodyFramesInRecord(t, wire.records[len(wire.records)-1])
	if len(final) != streams {
		t.Fatalf("final record frames = %d, want %d", len(final), streams)
	}
	for i, frame := range final {
		if frame.stream != ids[i] || !frame.end || len(frame.data) == 0 {
			t.Errorf("final frame %d = stream %d, bytes %d, END %v",
				i, frame.stream, len(frame.data), frame.end)
		}
	}
}

func BenchmarkGreedyPlan1024(b *testing.B) {
	const streams = maxBodyCarryingStreams
	data := make([][]byte, streams)
	sent := make([]int, streams)
	caps := make([]int32, streams)
	carrying := make([]int, streams)
	for i := range data {
		data[i] = []byte{'x', 'y'}
		caps[i] = 1
		carrying[i] = i
	}
	data[0] = make([]byte, 1<<20)
	caps[0] = tlsRecordPlaintextBudget - http2FrameHeaderSize
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if plan := planGreedyIntermediateRecord(data, sent, carrying, caps, 1<<30); plan == nil {
			b.Fatal("planner made no progress")
		}
	}
}

func BenchmarkGreedyPlanTypical(b *testing.B) {
	for _, tc := range []struct {
		name  string
		sizes []int
	}{
		{"two_asymmetric", []int{128, 32768}},
		{"thirty_three", repeatedSizes(33, 512)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			data := make([][]byte, len(tc.sizes))
			sent := make([]int, len(tc.sizes))
			caps := make([]int32, len(tc.sizes))
			carrying := make([]int, len(tc.sizes))
			for i, size := range tc.sizes {
				data[i] = make([]byte, size)
				capBytes := min(size-1, tlsRecordPlaintextBudget-http2FrameHeaderSize)
				caps[i] = int32(capBytes) //nolint:gosec // sizes are bounded fixtures
				carrying[i] = i
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if plan := planGreedyIntermediateRecord(data, sent, carrying, caps, 1<<30); plan == nil {
					b.Fatal("planner made no progress")
				}
			}
		})
	}
}
