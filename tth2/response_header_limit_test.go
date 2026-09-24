package tth2_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

func TestResponseHeaderSectionLimitMatrix(t *testing.T) {
	t.Parallel()
	const limit = uint32(64 << 10)
	for _, section := range []string{"informational", "final", "trailer"} {
		for _, capture := range []bool{false, true} {
			for _, delta := range []int{-1, 0, 1} {
				name := fmt.Sprintf("%s/capture=%t/delta=%+d",
					section, capture, delta)
				t.Run(name, func(t *testing.T) {
					fields := sizedResponseSection(t, section, limit, delta)
					wantSize := uint32(int(limit) + delta) //nolint:gosec // Delta stays around a positive constant.
					if got, want := headerListSize(fields), wantSize; got != want {
						t.Fatalf("decoded field size = %d, want %d", got, want)
					}
					encoded, err := encodeHeaderBlock(fields)
					if err != nil {
						t.Fatalf("encode header: %v", err)
					}
					if len(encoded) >= int(headerListSize(fields)) {
						t.Fatalf("encoded block = %d bytes, decoded fields = %d; test did not exercise compression",
							len(encoded), headerListSize(fields))
					}

					addr, tlsCfg := startRawH2Server(t, func(
						tb testing.TB,
						framer *http2.Framer,
						bw *bufio.Writer,
						_ net.Conn,
					) {
						if err := doServerPreface(tb, framer, bw); err != nil {
							tb.Logf("preface: %v", err)
							return
						}
						streamID, err := readRequestStream(framer)
						if err != nil {
							tb.Logf("request: %v", err)
							return
						}
						if err := writeResponseSection(
							framer, bw, streamID, section, fields,
						); err != nil {
							tb.Logf("response: %v", err)
						}
					})
					transport := &tth2.Transport{
						TLSClientConfig: tlsCfg, MaxResponseHeaderBytes: int64(limit),
					}
					t.Cleanup(transport.CloseIdleConnections)
					client := &tth2.Client{Transport: transport}
					var opts []tth2.SendOption
					if capture {
						opts = append(opts, tth2.WithResponseHeaderCapture())
					}
					batch, err := client.SendBatch(t.Context(), []*http.Request{
						mustReq(t, "https://"+addr+"/"),
					}, opts...)
					if delta > 0 {
						if err == nil || !strings.Contains(err.Error(), "exceeds") {
							t.Fatalf("SendBatch error = %v, want section-limit error", err)
						}
						if batch != nil {
							t.Errorf("oversized section returned partial batch %+v", batch)
						}
						return
					}
					if err != nil {
						t.Fatalf("SendBatch: %v", err)
					}
					response := batch.Results[0].Response
					if response.StatusCode != http.StatusOK {
						t.Errorf("status = %d, want 200", response.StatusCode)
					}
					assertSectionRetention(t, response, section, capture)
				})
			}
		}
	}
}

func sizedResponseSection(
	t testing.TB,
	section string,
	limit uint32,
	delta int,
) []hpack.HeaderField {
	t.Helper()
	fields := []hpack.HeaderField{{Name: "x-limit"}}
	if section != "trailer" {
		status := "200"
		if section == "informational" {
			status = "103"
		}
		fields = append([]hpack.HeaderField{{Name: ":status", Value: status}}, fields...)
	}
	base := int(headerListSize(fields))
	valueBytes := int(limit) + delta - base
	if valueBytes < 1 {
		t.Fatalf("limit %d leaves invalid field value size %d", limit, valueBytes)
	}
	fields[len(fields)-1].Value = strings.Repeat("a", valueBytes)
	return fields
}

func headerListSize(fields []hpack.HeaderField) uint32 {
	var size uint32
	for _, field := range fields {
		size += uint32(len(field.Name) + len(field.Value) + 32) //nolint:gosec // test fields are bounded
	}
	return size
}

func encodeHeaderBlock(fields []hpack.HeaderField) ([]byte, error) {
	var block bytes.Buffer
	encoder := hpack.NewEncoder(&block)
	for _, field := range fields {
		if err := encoder.WriteField(field); err != nil {
			return nil, err
		}
	}
	return block.Bytes(), nil
}

func writeHeaderBlock(
	framer *http2.Framer,
	bw *bufio.Writer,
	streamID uint32,
	endStream bool,
	fields []hpack.HeaderField,
) error {
	block, err := encodeHeaderBlock(fields)
	if err != nil {
		return err
	}
	const fragmentSize = 16 << 10
	firstBytes := min(fragmentSize, len(block))
	if err := framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: block[:firstBytes],
		EndStream:     endStream,
		EndHeaders:    firstBytes == len(block),
	}); err != nil {
		return err
	}
	for offset := firstBytes; offset < len(block); {
		next := min(offset+fragmentSize, len(block))
		if err := framer.WriteContinuation(
			streamID, next == len(block), block[offset:next]); err != nil {
			return err
		}
		offset = next
	}
	return bw.Flush()
}

func readRequestStream(framer *http2.Framer) (uint32, error) {
	for {
		frame, err := framer.ReadFrame()
		if err != nil {
			return 0, err
		}
		if headers, ok := frame.(*http2.MetaHeadersFrame); ok {
			return headers.StreamID, nil
		}
	}
}

func writeResponseSection(
	framer *http2.Framer,
	bw *bufio.Writer,
	streamID uint32,
	section string,
	fields []hpack.HeaderField,
) error {
	switch section {
	case "final":
		return writeHeaderBlock(framer, bw, streamID, true, fields)
	case "informational":
		if err := writeHeaderBlock(framer, bw, streamID, false, fields); err != nil {
			return err
		}
		return writeOKResponse(framer, bw, streamID)
	case "trailer":
		if err := writeHeadersNoEnd(framer, bw, streamID); err != nil {
			return err
		}
		return writeHeaderBlock(framer, bw, streamID, true, fields)
	default:
		return fmt.Errorf("unknown response section %q", section)
	}
}

func assertSectionRetention(
	t *testing.T,
	response *tth2.Response,
	section string,
	capture bool,
) {
	t.Helper()
	if !capture {
		if response.Header != nil || response.Trailer != nil {
			t.Errorf("metadata response retained Header/Trailer = %v/%v",
				response.Header, response.Trailer)
		}
		return
	}
	switch section {
	case "final":
		if response.Header.Get("X-Limit") == "" {
			t.Error("captured final header is absent")
		}
	case "informational":
		if response.Header.Get("X-Limit") != "" {
			t.Error("informational header survived final response")
		}
	case "trailer":
		if response.Trailer.Get("X-Limit") == "" {
			t.Error("captured trailer is absent")
		}
	}
}

func TestMaxResponseHeaderBytesAdvertisement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		value int64
		want  *uint32
	}{
		{name: "default", want: new(uint32(1 << 20))},
		{name: "finite", value: 1234, want: new(uint32(1234))},
		{name: "unlimited", value: tth2.UnlimitedResponseBytes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			seen := make(chan *uint32, 1)
			addr, tlsCfg := startRawH2Server(t, func(
				tb testing.TB,
				framer *http2.Framer,
				bw *bufio.Writer,
				_ net.Conn,
			) {
				if err := framer.WriteSettings(); err != nil {
					tb.Logf("write settings: %v", err)
					return
				}
				if err := bw.Flush(); err != nil {
					tb.Logf("flush settings: %v", err)
					return
				}
				var advertised *uint32
				for {
					frame, err := framer.ReadFrame()
					if err != nil {
						tb.Logf("read preface: %v", err)
						return
					}
					settings, ok := frame.(*http2.SettingsFrame)
					if !ok || settings.IsAck() {
						continue
					}
					if err := settings.ForeachSetting(func(setting http2.Setting) error {
						if setting.ID == http2.SettingMaxHeaderListSize {
							advertised = new(setting.Val)
						}
						return nil
					}); err != nil {
						tb.Logf("inspect settings: %v", err)
						return
					}
					break
				}
				seen <- advertised
				if err := framer.WriteSettingsAck(); err != nil {
					tb.Logf("write settings ack: %v", err)
					return
				}
				if err := bw.Flush(); err != nil {
					tb.Logf("flush settings ack: %v", err)
					return
				}
				streamID, err := readRequestStream(framer)
				if err != nil {
					tb.Logf("request: %v", err)
					return
				}
				if err := writeOKResponse(framer, bw, streamID); err != nil {
					tb.Logf("response: %v", err)
				}
			})
			transport := &tth2.Transport{
				TLSClientConfig: tlsCfg, MaxResponseHeaderBytes: test.value,
			}
			t.Cleanup(transport.CloseIdleConnections)
			client := &tth2.Client{Transport: transport}
			if _, err := client.SendBatch(t.Context(), []*http.Request{
				mustReq(t, "https://"+addr+"/"),
			}); err != nil {
				t.Fatalf("SendBatch: %v", err)
			}
			got := <-seen
			switch {
			case got == nil && test.want == nil:
			case got == nil || test.want == nil || *got != *test.want:
				t.Errorf("advertised limit = %v, want %v", got, test.want)
			}
		})
	}
}

func TestMetadataProjectionAcrossStreamsAndConnections(t *testing.T) {
	t.Parallel()
	const body = "complete body"
	largeHeader := strings.Repeat("h", 32<<10)
	var handlers sync.WaitGroup
	handlers.Add(4)
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		handlers.Done()
		handlers.Wait()
		w.Header().Set("X-Large", largeHeader)
		_, _ = w.Write([]byte(body))
	}))
	transport := &tth2.Transport{
		TLSClientConfig: tlsCfg, MaxResponseHeaderBytes: 64 << 10,
		MaxConnsPerHost: 2,
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &tth2.Client{Transport: transport}
	ctx := t.Context()
	type outcome struct {
		batch *tth2.BatchResult
		err   error
	}
	outcomes := make(chan outcome, 2)
	for batchIndex := range 2 {
		requests := []*http.Request{
			mustReq(t, fmt.Sprintf("https://%s/%d/a", addr, batchIndex)),
			mustReq(t, fmt.Sprintf("https://%s/%d/b", addr, batchIndex)),
		}
		go func() {
			batch, err := client.SendBatch(ctx, requests)
			outcomes <- outcome{batch: batch, err: err}
		}()
	}
	connectionIDs := make(map[tth2.ConnectionID]bool)
	for range 2 {
		got := <-outcomes
		if got.err != nil {
			t.Fatalf("SendBatch: %v", got.err)
		}
		connectionIDs[got.batch.Connection] = true
		for _, result := range got.batch.Results {
			response := result.Response
			if response.Header != nil || response.Trailer != nil ||
				response.ContentLength != int64(len(body)) ||
				response.Body.BytesReceived != int64(len(body)) {
				t.Errorf("metadata response = %+v", response)
			}
		}
	}
	if len(connectionIDs) != 2 {
		t.Errorf("batches used %d connections, want 2", len(connectionIDs))
	}
}

func TestMetadataProjectionWithStalledLargeBody(t *testing.T) {
	t.Parallel()
	headerValue := strings.Repeat("h", 256<<10)
	body := bytes.Repeat([]byte("b"), 1<<20)
	stalled := make(chan struct{})
	release := make(chan struct{})
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("X-Large", headerValue)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(stalled)
		<-release
		_, _ = w.Write(body)
	}))
	transport := &tth2.Transport{
		TLSClientConfig: tlsCfg, MaxResponseHeaderBytes: 512 << 10,
	}
	t.Cleanup(transport.CloseIdleConnections)
	client := &tth2.Client{Transport: transport}
	ctx := t.Context()
	type outcome struct {
		batch *tth2.BatchResult
		err   error
	}
	done := make(chan outcome, 1)
	request := mustReq(t, "https://"+addr+"/")
	go func() {
		batch, err := client.SendBatch(
			ctx, []*http.Request{request})
		done <- outcome{batch: batch, err: err}
	}()
	<-stalled
	select {
	case got := <-done:
		t.Fatalf("SendBatch completed before body release: %v", got.err)
	default:
	}
	close(release)
	got := <-done
	if got.err != nil {
		t.Fatalf("SendBatch: %v", got.err)
	}
	response := got.batch.Results[0].Response
	if response.Header != nil || response.Trailer != nil {
		t.Errorf("metadata response retained Header/Trailer = %v/%v",
			response.Header, response.Trailer)
	}
	if response.Body.BytesReceived != int64(len(body)) ||
		response.Body.SHA256 != sha256.Sum256(body) {
		t.Errorf("body metadata = %d/%x",
			response.Body.BytesReceived, response.Body.SHA256)
	}
}

func BenchmarkLargeHeaderStalledBodyRetention(b *testing.B) {
	const body = "body"
	headerValue := strings.Repeat("a", 256<<10)
	type gate struct {
		stalled chan struct{}
		release chan struct{}
	}
	gates := make(chan gate, 1)
	addr, tlsCfg := h2test.Serve(b, http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		current := <-gates
		w.Header().Set("X-Large", headerValue)
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(current.stalled)
		<-current.release
		_, _ = w.Write([]byte(body))
	}))
	transport := &tth2.Transport{
		TLSClientConfig: tlsCfg, MaxResponseHeaderBytes: 512 << 10,
	}
	b.Cleanup(transport.CloseIdleConnections)
	client := &tth2.Client{Transport: transport}
	request := mustReq(b, "https://"+addr+"/")

	b.ReportAllocs()
	var maxRetainedDelta uint64
	for b.Loop() {
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		current := gate{stalled: make(chan struct{}), release: make(chan struct{})}
		gates <- current
		done := make(chan error, 1)
		go func() {
			_, err := client.SendBatch(
				context.Background(), []*http.Request{request})
			done <- err
		}()
		<-current.stalled
		runtime.GC()
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		if memory.HeapAlloc > before.HeapAlloc {
			maxRetainedDelta = max(
				maxRetainedDelta, memory.HeapAlloc-before.HeapAlloc)
		}
		close(current.release)
		if err := <-done; err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(maxRetainedDelta), "max-stalled-heap-delta-bytes")
}
