package timing

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestBodyCollectorPrefixDoesNotLimitDigest(t *testing.T) {
	body := bytes.Repeat([]byte("opaque response bytes"), 10000)
	for _, capture := range []bool{false, true} {
		collector := newBodyCollector(capture, 7)
		for start := 0; start < len(body); start += 113 {
			end := min(start+113, len(body))
			collector.accept(body[start:end])
		}
		if collector.accepted != int64(len(body)) || collector.sum() != sha256.Sum256(body) {
			t.Fatal("capture policy changed accepted-byte evidence")
		}
		captured := collector.capturedBody()
		if !capture {
			if captured != nil || len(collector.body) != 0 {
				t.Fatal("disabled capture retained response bytes")
			}
			continue
		}
		if !bytes.Equal(captured, body[:7]) || len(collector.body) != 7 {
			t.Fatal("bounded capture lost its prefix")
		}
		captured[0] ^= 1
		if !bytes.Equal(collector.capturedBody(), body[:7]) {
			t.Fatal("result capture aliases collector storage")
		}
	}
}

func TestBodyCollectorEmptyAndUnlimited(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		collector := newBodyCollector(true, limit)
		if collector.sum() != sha256.Sum256(nil) || collector.capturedBody() == nil {
			t.Fatal("enabled empty capture lost empty-body evidence")
		}
		collector.accept([]byte("a"))
		collector.accept([]byte("bc"))
		want := []byte{}
		if limit < 0 {
			want = []byte("abc")
		}
		if !bytes.Equal(collector.capturedBody(), want) || collector.sum() != sha256.Sum256([]byte("abc")) {
			t.Fatal("capture limit changed digest or capture")
		}
	}
}
