package tth2_test

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/h2tls"
)

// startRawH2Server starts a TLS+h2 listener that accepts connections in a
// loop. For each accepted connection it consumes the client preface magic
// string, then calls onConn with a fresh framer/buffer. onConn owns the
// rest of the HTTP/2 conversation (SETTINGS, ACKs, stream-level frames) —
// typically by calling doServerPreface to handle the standard handshake.
//
// Loop-accept matters because Transport.sendBatch discards a dead pooled
// connection and re-dials; tests that assert on the original failure mode
// (GOAWAY, peer hang-up, etc.) need the server ready to replay the scenario on
// a re-dial. A batch on a freshly-dialled connection is not retried, so a test
// starting from an empty pool sees the failure on its first (and only) accept.
//
// A 30-second read deadline is applied per connection so a wedged test doesn't
// run forever. The listener and any in-flight handler goroutines are torn down
// by t.Cleanup.
func startRawH2Server(
	tb testing.TB,
	onConn func(testing.TB, *http2.Framer, *bufio.Writer, net.Conn),
) (addr string, tlsCfg *tls.Config) {
	tb.Helper()
	ep, err := h2tls.Listen(h2test.Cert(tb))
	if err != nil {
		tb.Fatalf("h2tls.Listen: %v", err)
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			conn, err := ep.Listener.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))

				// Drain the client preface magic before handing off.
				pref := make([]byte, len(http2.ClientPreface))
				if _, err := io.ReadFull(conn, pref); err != nil {
					tb.Logf("rawH2Server: read client preface: %v", err)
					return
				}
				if string(pref) != http2.ClientPreface {
					tb.Errorf("rawH2Server: bad client preface: %q", pref)
					return
				}

				bw := bufio.NewWriter(conn)
				framer := http2.NewFramer(bw, conn)
				framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
				onConn(tb, framer, bw, conn)
			})
		}
	})

	tb.Cleanup(func() {
		_ = ep.Listener.Close()
		wg.Wait()
	})

	return ep.Addr, ep.ClientConfig()
}

// doServerPreface completes the server side of the HTTP/2 connection preface:
// writes initialSettings, then reads client frames until the client's SETTINGS
// frame arrives and ACKs it. Pre-stream WINDOW_UPDATE frames are silently
// consumed. The client's eventual SETTINGS-ACK to our own SETTINGS is left for
// the caller to read (or ignore).
func doServerPreface(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, initialSettings ...http2.Setting) error {
	tb.Helper()
	if err := framer.WriteSettings(initialSettings...); err != nil {
		return fmt.Errorf("write SETTINGS: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("flush SETTINGS: %w", err)
	}
	for {
		f, err := framer.ReadFrame()
		if err != nil {
			return fmt.Errorf("preface read: %w", err)
		}
		switch fr := f.(type) {
		case *http2.SettingsFrame:
			if fr.IsAck() {
				continue
			}
			pushDisabled := false
			if err := fr.ForeachSetting(func(setting http2.Setting) error {
				if setting.ID == http2.SettingEnablePush && setting.Val == 0 {
					pushDisabled = true
				}
				return nil
			}); err != nil {
				return fmt.Errorf("read client SETTINGS: %w", err)
			}
			if !pushDisabled {
				return errors.New("client SETTINGS did not disable server push")
			}
			if err := framer.WriteSettingsAck(); err != nil {
				return fmt.Errorf("write SETTINGS ACK: %w", err)
			}
			if err := bw.Flush(); err != nil {
				return fmt.Errorf("flush SETTINGS ACK: %w", err)
			}
			return nil
		case *http2.WindowUpdateFrame:
			// Client's connection-level window update; ignore.
		}
	}
}

// writeOKResponse sends a minimal "200 OK" + END_STREAM HEADERS frame on
// streamID. Useful for raw-server tests that need to complete a stream after
// asserting whatever they wanted to assert about the send phase.
func writeOKResponse(framer *http2.Framer, bw *bufio.Writer, streamID uint32) error {
	return writeStatusHeaders(framer, bw, streamID, true)
}

// writeHeadersNoEnd sends a minimal "200 OK" HEADERS frame on streamID and
// leaves the stream open, for a server that answers and then goes on to do
// something else with the stream.
func writeHeadersNoEnd(framer *http2.Framer, bw *bufio.Writer, streamID uint32) error {
	return writeStatusHeaders(framer, bw, streamID, false)
}

// respondInPathOrder returns a raw-server conversation that receives complete
// batches containing paths, then writes their response HEADERS in the given
// order. Frame order supplies an exact arrival oracle without handler
// scheduling or elapsed-time assumptions.
func respondInPathOrder(paths ...string) func(
	testing.TB,
	*http2.Framer,
	*bufio.Writer,
	net.Conn,
) {
	return func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		tb.Helper()
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("preface: %v", err)
			return
		}
		for {
			streams := make(map[string]uint32, len(paths))
			for len(streams) < len(paths) {
				frame, err := framer.ReadFrame()
				if err != nil {
					return
				}
				headers, ok := frame.(*http2.MetaHeadersFrame)
				if !ok {
					continue
				}
				path := headers.PseudoValue("path")
				if _, duplicate := streams[path]; duplicate {
					tb.Errorf("batch contains path %q more than once", path)
					return
				}
				streams[path] = headers.StreamID
			}
			for _, path := range paths {
				streamID, ok := streams[path]
				if !ok {
					tb.Errorf("batch paths = %v, missing %s", streams, path)
					return
				}
				if err := writeOKResponse(framer, bw, streamID); err != nil {
					tb.Logf("respond %s: %v", path, err)
					return
				}
			}
		}
	}
}

func writeStatusHeaders(framer *http2.Framer, bw *bufio.Writer, streamID uint32, endStream bool) error {
	var hbuf bytes.Buffer
	enc := hpack.NewEncoder(&hbuf)
	if err := enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"}); err != nil {
		return err
	}
	if err := framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamID,
		BlockFragment: hbuf.Bytes(),
		EndStream:     endStream,
		EndHeaders:    true,
	}); err != nil {
		return err
	}
	return bw.Flush()
}
