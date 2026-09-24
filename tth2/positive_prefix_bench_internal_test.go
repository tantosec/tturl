package tth2

import (
	"bytes"
	"context"
	"testing"
)

// BenchmarkPositivePrefixPhase measures the full N>0 prefix sender, including
// flow accounting, framing and record flushes, for small and wide batches.
func BenchmarkPositivePrefixPhase(b *testing.B) {
	for _, tc := range []struct {
		name  string
		sizes []int
	}{
		{"two_asymmetric", []int{128, 32768}},
		{"thirty_three_512", repeatedSizes(33, 512)},
		{"thirty_three_32768", repeatedSizes(33, 32768)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			data := make([][]byte, len(tc.sizes))
			ids := make([]uint32, len(tc.sizes))
			for i, size := range tc.sizes {
				data[i] = bytes.Repeat([]byte{'x'}, size-3)
				ids[i] = uint32(2*i + 1)
			}
			b.ReportAllocs()
			b.ResetTimer()
			records := 0
			for b.Loop() {
				wire := &flushRecordingWriter{}
				conn := makeFlattenFitConn(wire)
				conn.peerConnSendWindow.Store(1 << 30)
				conn.peerInitStreamWindow.Store(1 << 30)
				flow := newFlowControl(conn, ids)
				if _, err := conn.sendFlowControlledBody(context.Background(), flow,
					data, false, "prefix", nil); err != nil {
					b.Fatal(err)
				}
				records += len(wire.records)
			}
			b.ReportMetric(float64(records)/float64(b.N), "records/op")
		})
	}
}
