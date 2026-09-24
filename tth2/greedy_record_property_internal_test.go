package tth2

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

type greedyAuditSink struct{ records [][]byte }

func (w *greedyAuditSink) Write(p []byte) (int, error) {
	w.records = append(w.records, bytes.Clone(p))
	return len(p), nil
}

// TestGreedyRecordMembership checks the public zero-withholding contract on
// deterministic varied body shapes. Every byte must arrive in order, and the
// final DATA frame for each body must contain payload and share one record.
func TestGreedyRecordMembership(t *testing.T) {
	shapes := []struct {
		name  string
		sizes []int
	}{
		{"one-byte", []int{1}},
		{"small-unequal", []int{4, 6}},
		{"exact-record", []int{8183, 8183}},
		{"one-over-record", []int{8184, 8183}},
		{"single-exact-record", []int{tlsRecordPlaintextBudget - http2FrameHeaderSize}},
		{"single-over-record", []int{tlsRecordPlaintextBudget - http2FrameHeaderSize + 1}},
		{"past-32-streams", repeatedSizes(33, 512)},
		{"tiny-and-wide", append([]int{1}, repeatedSizes(32, 512)...)},
		{"64-streams", repeatedSizes(64, 1024)},
		{"large-asymmetric", []int{1, 50, 8192, 65536, 500000}},
	}
	rng := rand.New(rand.NewSource(20260923)) //nolint:gosec // deterministic test shapes
	for trial := range 40 {
		width := 1 + rng.Intn(64)
		sizes := make([]int, width)
		for i := range sizes {
			switch rng.Intn(4) {
			case 0:
				sizes[i] = 1
			case 1:
				sizes[i] = 1 + rng.Intn(64)
			case 2:
				sizes[i] = 256 + rng.Intn(8192)
			default:
				sizes[i] = 8192 + rng.Intn(32768)
			}
		}
		shapes = append(shapes, struct {
			name  string
			sizes []int
		}{fmt.Sprintf("random-%02d", trial), sizes})
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			checkGreedyRecordMembership(t, shape.sizes)
		})
	}
}

func repeatedSizes(n, size int) []int {
	sizes := make([]int, n)
	for i := range sizes {
		sizes[i] = size
	}
	return sizes
}

func checkGreedyRecordMembership(t *testing.T, sizes []int) {
	t.Helper()
	wire := &greedyAuditSink{}
	bw := bufio.NewWriterSize(wire, 2*tlsRecordPlaintextBudget)
	conn := &http2conn{bw: bw, framer: http2.NewFramer(bw, nil)}
	conn.hpackEnc = hpack.NewEncoder(&conn.hpackBuf)
	conn.peerMaxFrameSize.Store(maxDataFramePayload)
	conn.peerConnSendWindow.Store(1 << 30)
	conn.peerInitStreamWindow.Store(1 << 30)

	reqs := make([]*http.Request, len(sizes))
	ids := make([]uint32, len(sizes))
	want := make([][]byte, len(sizes))
	for i, size := range sizes {
		body := make([]byte, size)
		for j := range body {
			body[j] = byte(i*37 + j*13) //nolint:gosec // synthetic pattern deliberately wraps
		}
		want[i] = body
		ids[i] = uint32(2*i + 1)
		var err error
		reqs[i], err = http.NewRequestWithContext(context.Background(),
			http.MethodPost, "https://example.test/", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.sendRequests(context.Background(), reqs, ids, 0, 0, nil, nil); err != nil {
		t.Fatalf("sendRequests: %v", err)
	}
	if len(wire.records) < 2 {
		t.Fatalf("records = %d, want headers and at least one body record", len(wire.records))
	}
	got := make([][]byte, len(sizes))
	ended := make([]bool, len(sizes))
	for recordIndex, record := range wire.records {
		if len(record) > tlsRecordPlaintextBudget {
			t.Errorf("record %d has %d plaintext bytes", recordIndex, len(record))
		}
		if recordIndex > 0 && recordIndex < len(wire.records)-1 {
			// Before this nonterminal record, a shared positive completion
			// must not yet fit. Credit is ample and static in this test.
			remainingRecordBytes := 0
			for i, size := range sizes {
				left := size - len(got[i])
				if left <= 0 {
					t.Errorf("stream %d has no positive suffix before final record", ids[i])
				}
				remainingRecordBytes += http2FrameHeaderSize + left
			}
			if remainingRecordBytes <= tlsRecordPlaintextBudget {
				t.Errorf("record %d skipped feasible shared completion (%d bytes)",
					recordIndex, remainingRecordBytes)
			}
		}
		framer := http2.NewFramer(nil, bytes.NewReader(record))
		for {
			frame, err := framer.ReadFrame()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("record %d: ReadFrame: %v", recordIndex, err)
			}
			switch f := frame.(type) {
			case *http2.HeadersFrame, *http2.ContinuationFrame:
				if recordIndex != 0 {
					t.Errorf("HEADERS in body record %d", recordIndex)
				}
			case *http2.DataFrame:
				if recordIndex == 0 || f.StreamID%2 != 1 ||
					int((f.StreamID-1)/2) >= len(sizes) {
					t.Fatalf("record %d: unexpected DATA stream %d", recordIndex, f.StreamID)
				}
				i := int((f.StreamID - 1) / 2)
				if ended[i] {
					t.Errorf("stream %d has DATA after END_STREAM", f.StreamID)
				}
				got[i] = append(got[i], f.Data()...)
				if f.StreamEnded() {
					if recordIndex != len(wire.records)-1 || len(f.Data()) == 0 {
						t.Errorf("stream %d terminal DATA in record %d has %d bytes",
							f.StreamID, recordIndex, len(f.Data()))
					}
					ended[i] = true
				}
			default:
				t.Fatalf("record %d: unexpected frame %T", recordIndex, f)
			}
		}
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) || !ended[i] {
			t.Errorf("stream %d: bytes=%d/%d match=%v ended=%v",
				ids[i], len(got[i]), len(want[i]), bytes.Equal(got[i], want[i]), ended[i])
		}
	}
}
