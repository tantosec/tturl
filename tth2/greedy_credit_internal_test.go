package tth2

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type greedyCreditSink struct {
	updates  chan int32
	records  int
	frames   int
	tiny     int
	sent     [33]int
	endAt    [33]int
	emptyEnd bool
	maxSkew  float64
}

func (s *greedyCreditSink) Write(p []byte) (int, error) {
	s.records++
	for off := 0; off < len(p); {
		if len(p)-off < http2FrameHeaderSize {
			return 0, fmt.Errorf("partial HTTP/2 frame header")
		}
		n := int(p[off])<<16 | int(p[off+1])<<8 | int(p[off+2])
		if off+http2FrameHeaderSize+n > len(p) {
			return 0, fmt.Errorf("partial HTTP/2 frame payload")
		}
		if p[off+3] == byte(http2.FrameData) {
			s.frames++
			sid := uint32(p[off+5]&0x7f)<<24 | uint32(p[off+6])<<16 |
				uint32(p[off+7])<<8 | uint32(p[off+8])
			idx := (sid - 1) / 2
			s.sent[idx] += n
			if p[off+4]&1 != 0 {
				s.endAt[idx] = s.records
				s.emptyEnd = s.emptyEnd || n == 0
			}
			if n <= 16 {
				s.tiny++
			}
			if n > 0 {
				s.updates <- int32(n) //nolint:gosec // one frame is <= TLS record budget
			}
		}
		off += http2FrameHeaderSize + n
	}
	lo, hi := s.sent[0], s.sent[0]
	for _, n := range s.sent {
		lo = min(lo, n)
		hi = max(hi, n)
	}
	s.maxSkew = max(s.maxSkew, float64(hi-lo)/4096)
	return len(p), nil
}

// A receiver that grants connection credit separately for each DATA frame
// exposes feedback loops in schedulers that send one tiny frame per update.
func TestGreedyCreditPerFrameUpdates(t *testing.T) {
	for _, withheld := range []int{0, 1, 3} {
		t.Run(fmt.Sprintf("withheld-%d", withheld), func(t *testing.T) {
			testGreedyCreditPerFrameUpdates(t, withheld)
		})
	}
}

func testGreedyCreditPerFrameUpdates(t *testing.T, withheld int) {
	t.Helper()
	updates := make(chan int32, 4096)
	sink := &greedyCreditSink{updates: updates}
	bw := bufio.NewWriterSize(sink, 2*tlsRecordPlaintextBudget)
	conn := &http2conn{bw: bw, framer: http2.NewFramer(bw, nil)}
	conn.peerMaxFrameSize.Store(maxDataFramePayload)
	conn.peerConnSendWindow.Store(65535)
	conn.peerInitStreamWindow.Store(1 << 30)
	data := make([][]byte, 33)
	ids := make([]uint32, len(data))
	for i := range data {
		data[i] = bytes.Repeat([]byte{'x'}, 4096-withheld)
		ids[i] = uint32(2*i + 1)
	}
	flow := newFlowControl(conn, ids)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := range updates {
			time.Sleep(10 * time.Microsecond)
			_ = conn.addConnectionSendCredit(n)
			flow.notify()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	responses := &responseRead{ctx: ctx, sendErr: make(chan error)}
	_, err := conn.sendFlowControlledBody(ctx, flow, data,
		withheld == 0, "body", responses)
	close(updates)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if sink.maxSkew > 0.04 {
		t.Errorf("normalised progress skew = %.3f, want at most 0.04", sink.maxSkew)
	}
	if sink.emptyEnd {
		t.Error("empty DATA frame completed a body")
	}
	for i, record := range sink.endAt {
		if withheld == 0 && record != sink.records {
			t.Errorf("stream %d ended in record %d, want shared final record %d", i, record, sink.records)
		}
		if withheld > 0 && record != 0 {
			t.Errorf("stream %d ended in prefix record %d", i, record)
		}
	}
	t.Logf("records=%d frames=%d tiny<=16=%d max progress skew=%.3f", sink.records, sink.frames, sink.tiny, sink.maxSkew)
}

func TestGreedyCreditStallRemainsCancellable(t *testing.T) {
	updates := make(chan int32, 1)
	sink := &greedyCreditSink{updates: updates}
	bw := bufio.NewWriterSize(sink, 2*tlsRecordPlaintextBudget)
	conn := &http2conn{bw: bw, framer: http2.NewFramer(bw, nil)}
	conn.peerMaxFrameSize.Store(maxDataFramePayload)
	conn.peerConnSendWindow.Store(1024)
	conn.peerInitStreamWindow.Store(1 << 30)
	flow := newFlowControl(conn, []uint32{1})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	responses := &responseRead{ctx: ctx, sendErr: make(chan error)}
	_, err := conn.sendFlowControlledBody(ctx, flow,
		[][]byte{bytes.Repeat([]byte{'x'}, 32<<10)}, true, "body", responses)
	if err == nil {
		t.Fatal("stalled credit unexpectedly completed")
	}
}

func TestGreedyScarceCreditDoesNotLeaveAvailablePayload(t *testing.T) {
	for _, width := range []int{1, 2} {
		t.Run(fmt.Sprintf("width-%d", width), func(t *testing.T) {
			sink := &greedyCreditSink{updates: make(chan int32, 4)}
			bw := bufio.NewWriterSize(sink, 2*tlsRecordPlaintextBudget)
			conn := &http2conn{bw: bw, framer: http2.NewFramer(bw, nil)}
			conn.peerMaxFrameSize.Store(maxDataFramePayload)
			conn.peerConnSendWindow.Store(4096)
			conn.peerInitStreamWindow.Store(1 << 30)
			data := make([][]byte, width)
			ids := make([]uint32, width)
			for i := range data {
				data[i] = bytes.Repeat([]byte{'x'}, 32768)
				ids[i] = uint32(2*i + 1)
			}
			flow := newFlowControl(conn, ids)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			responses := &responseRead{ctx: ctx, sendErr: make(chan error)}
			if _, err := conn.sendFlowControlledBody(ctx, flow, data, true, "body", responses); err == nil {
				t.Fatal("send unexpectedly completed without more credit")
			}
			sent := 0
			for i := range width {
				sent += sink.sent[i]
			}
			if sent != 4096 {
				t.Errorf("sent %d bytes before credit stall, want all 4096 available bytes", sent)
			}
		})
	}
}

// Observational stress: the scarce-credit policy deliberately favours useful
// frames over fractional lockstep when some bodies can only advance by a few
// bytes. This reports the resulting skew for launch decisions.
func TestGreedyScarceCreditAsymmetricSkew(t *testing.T) {
	for _, shape := range []struct {
		name  string
		sizes []int
		steps int
	}{
		{"4-vs-32768", []int{4, 32768}, 64},
		{"1MiB-plus-1023-tiny", append([]int{1 << 20}, repeatedSizes(1023, 2)...), 600},
	} {
		t.Run(shape.name, func(t *testing.T) {
			data := make([][]byte, len(shape.sizes))
			caps := make([]int32, len(data))
			sent := make([]int, len(data))
			carrying := make([]int, len(data))
			for i, size := range shape.sizes {
				data[i] = make([]byte, size)
				carrying[i] = i
			}
			maxSkew := 0.0
			for range shape.steps {
				for i := range data {
					caps[i] = int32(min(len(data[i])-sent[i]-1, maxDataFramePayload)) //nolint:gosec // test sizes fit int32
				}
				plan := planScarceCreditRecord(data, sent, carrying, caps, 1024)
				if plan == nil {
					break
				}
				for i, n := range plan {
					sent[i] += int(n)
				}
				first := float64(sent[0]) / float64(len(data[0]))
				last := float64(sent[len(sent)-1]) / float64(len(data[len(data)-1]))
				maxSkew = max(maxSkew, math.Abs(first-last))
			}
			t.Logf("normalised progress first=%.3f, last=%.3f, max skew=%.3f",
				float64(sent[0])/float64(len(data[0])),
				float64(sent[len(sent)-1])/float64(len(data[len(data)-1])), maxSkew)
		})
	}
}

func BenchmarkGreedyScarceCredit1024Tiny(b *testing.B) {
	const streams = 1024
	data := make([][]byte, streams)
	caps := make([]int32, streams)
	sent := make([]int, streams)
	carrying := make([]int, streams)
	for i := range data {
		data[i] = []byte{'x', 'y'}
		caps[i] = 1
		carrying[i] = i
	}
	b.ReportAllocs()
	for b.Loop() {
		if plan := planScarceCreditRecord(data, sent, carrying, caps, 1024); plan == nil {
			b.Fatal("no plan")
		}
	}
}
