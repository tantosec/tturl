package tth2

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
)

func TestResponseBodyStateStopsAtReceiveLimit(t *testing.T) {
	t.Parallel()
	state := responseBodyState{digest: sha256.New()}
	overflow, err := state.accept([]byte("abc"), 5, UnlimitedResponseBytes)
	if err != nil || overflow {
		t.Fatalf("first DATA = overflow %t, error %v", overflow, err)
	}
	overflow, err = state.accept(
		[]byte("defghijklmnopqrstuvwxyz"), 5, UnlimitedResponseBytes)
	if err != nil || !overflow {
		t.Fatalf("crossing DATA = overflow %t, error %v", overflow, err)
	}
	want := []byte("abcde")
	if state.bytesAccepted != int64(len(want)) ||
		string(state.captured) != string(want) ||
		string(state.digest.Sum(nil)) != string(sha256Bytes(want)) {
		t.Errorf("accepted state = %d/%q/%x, want %d/%q/%x",
			state.bytesAccepted, state.captured, state.digest.Sum(nil),
			len(want), want, sha256Bytes(want))
	}
}

func TestResponseBodyStateKeepsCaptureLimitIndependent(t *testing.T) {
	t.Parallel()
	state := responseBodyState{digest: sha256.New()}
	overflow, err := state.accept([]byte("abcdef"), 6, 2)
	if err != nil || overflow {
		t.Fatalf("exact DATA = overflow %t, error %v", overflow, err)
	}
	if state.bytesAccepted != 6 || string(state.captured) != "ab" ||
		string(state.digest.Sum(nil)) != string(sha256Bytes([]byte("abcdef"))) {
		t.Errorf("accepted state = %d/%q/%x",
			state.bytesAccepted, state.captured, state.digest.Sum(nil))
	}
}

func TestResponseReadPreservesFirstCancellationCause(t *testing.T) {
	t.Parallel()
	bodyLimit := &ResponseBodyLimitError{
		RequestIndex: 0, StreamID: 1, Limit: 4, BytesAccepted: 4,
	}

	t.Run("local first", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(t.Context())
		ctx, cancel := context.WithCancelCause(parent)
		read := &responseRead{
			c:   &http2conn{collectors: make(map[uint32]*streamCollector)},
			ctx: ctx, cancel: cancel,
		}
		read.closeAll(bodyLimit)
		cancelParent()
		if _, ok := errors.AsType[*ResponseBodyLimitError](context.Cause(ctx)); !ok {
			t.Fatalf("cause = %v, want ResponseBodyLimitError",
				context.Cause(ctx))
		}
	})

	t.Run("parent first", func(t *testing.T) {
		parent, cancelParent := context.WithCancel(t.Context())
		ctx, cancel := context.WithCancelCause(parent)
		read := &responseRead{
			c:   &http2conn{collectors: make(map[uint32]*streamCollector)},
			ctx: ctx, cancel: cancel,
		}
		cancelParent()
		read.closeAll(bodyLimit)
		if !errors.Is(context.Cause(ctx), context.Canceled) {
			t.Fatalf("cause = %v, want context.Canceled", context.Cause(ctx))
		}
	})
}

func BenchmarkResponseBodyLimitCrossing(b *testing.B) {
	frame := make([]byte, maxDataFramePayload)
	b.ReportAllocs()
	for b.Loop() {
		state := responseBodyState{digest: sha256.New()}
		_, _ = state.accept(frame, 1024, 1024)
	}
}

func sha256Bytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}
