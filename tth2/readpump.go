package tth2

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// connectionReadPump is the single frame reader for the post-preface lifetime
// of one HTTP/2 connection. It services the connection control plane even when
// no batch owns response streams.
type connectionReadPump struct {
	c *http2conn

	mu       sync.Mutex
	err      error
	response *responseRead
	flow     *flowControl
	pings    map[[8]byte]chan pingResult
	done     chan struct{}
}

type pingResult struct {
	at  time.Time
	err error
}

func (c *http2conn) startReadPump() {
	p := &connectionReadPump{
		c: c, pings: make(map[[8]byte]chan pingResult),
		done: make(chan struct{}),
	}
	c.readPump = p
	go p.run()
}

func (p *connectionReadPump) run() {
	defer close(p.done)
	defer func() { _ = p.c.tlsConn.Close() }()
	for {
		frame, err := p.c.framer.ReadFrame()
		if err != nil {
			cause := err
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				cause = fmt.Errorf("tth2: read frame: %w", err)
			}
			p.fail(cause)
			return
		}
		if err := p.dispatch(frame); err != nil {
			p.fail(err)
			return
		}
	}
}

func (p *connectionReadPump) dispatch(frame http2.Frame) error {
	switch fr := frame.(type) {
	case *http2.MetaHeadersFrame, *http2.DataFrame:
		response := p.currentResponse()
		if response == nil {
			return fmt.Errorf("tth2: response frame on a connection without an active batch")
		}
		return response.dispatchFrame(fr)
	case *http2.RSTStreamFrame:
		response := p.currentResponse()
		if response == nil || !response.dispatchReset(fr) {
			// A reset can race the retirement of its completed stream.
			return nil
		}
		response.failSend(&StreamError{StreamID: fr.StreamID, Code: fr.ErrCode})
	case *http2.PushPromiseFrame:
		return errors.New(
			"tth2: server sent PUSH_PROMISE after push was disabled",
		)
	case *http2.GoAwayFrame:
		return fmt.Errorf("tth2: server GOAWAY: lastStream=%d code=%v",
			fr.LastStreamID, fr.ErrCode)
	case *http2.WindowUpdateFrame:
		if fr.StreamID == 0 {
			increment := int32(fr.Increment) //nolint:gosec // masked
			if err := p.c.addConnectionSendCredit(increment); err != nil {
				return fmt.Errorf("tth2: connection WINDOW_UPDATE: %w", err)
			}
			if flow := p.currentFlow(); flow != nil {
				flow.notify()
			}
		} else if flow := p.currentFlow(); flow != nil {
			if err := flow.addStream(
				fr.StreamID,
				int32(fr.Increment), //nolint:gosec // The reserved bit is masked.
			); err != nil {
				return fmt.Errorf("tth2: stream WINDOW_UPDATE: %w", err)
			}
		}
	case *http2.SettingsFrame:
		if fr.IsAck() {
			return nil
		}
		if err := p.applySettings(fr); err != nil {
			return fmt.Errorf("tth2: apply peer SETTINGS: %w", err)
		}
		return p.c.writeControl("SETTINGS ACK", func() error {
			return p.c.framer.WriteSettingsAck()
		})
	case *http2.PingFrame:
		if fr.IsAck() {
			p.completePing(fr.Data)
			return nil
		}
		return p.c.writeControl("PING ACK", func() error {
			return p.c.framer.WritePing(true, fr.Data)
		})
	}
	return nil
}

func (p *connectionReadPump) currentResponse() *responseRead {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.response
}

func (p *connectionReadPump) currentFlow() *flowControl {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.flow
}

func (p *connectionReadPump) activate(response *responseRead) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return p.err
	}
	if p.response != nil {
		return errors.New("tth2: connection already has an active batch")
	}
	p.response = response
	return nil
}

func (p *connectionReadPump) setFlow(
	response *responseRead,
	streamIDs []uint32,
) (*flowControl, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	if p.response != response {
		return nil, errors.New("tth2: send flow does not own the active batch")
	}
	flow := newFlowControl(p.c, streamIDs)
	p.flow = flow
	return flow, nil
}

func (p *connectionReadPump) applySettings(fr *http2.SettingsFrame) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delta, err := p.c.applySettingsValues(fr)
	if err != nil {
		return err
	}
	if p.flow != nil {
		if err := p.flow.applyInitDelta(delta); err != nil {
			return err
		}
	}
	return nil
}

func (p *connectionReadPump) clearFlow(flow *flowControl) {
	p.mu.Lock()
	if p.flow == flow {
		p.flow = nil
	}
	p.mu.Unlock()
}

func (p *connectionReadPump) retire(response *responseRead) {
	p.mu.Lock()
	if p.response == response {
		p.response = nil
		p.flow = nil
	}
	p.mu.Unlock()
}

func (p *connectionReadPump) readError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (p *connectionReadPump) fail(cause error) {
	p.mu.Lock()
	if p.err != nil {
		p.mu.Unlock()
		return
	}
	p.err = cause
	response := p.response
	pings := p.pings
	p.pings = make(map[[8]byte]chan pingResult)
	p.mu.Unlock()

	if response != nil {
		response.closeAll(cause)
	}
	for _, waiter := range pings {
		waiter <- pingResult{err: cause}
	}
}

func (p *connectionReadPump) registerPing(
	payload [8]byte,
) (<-chan pingResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	if _, exists := p.pings[payload]; exists {
		return nil, errors.New("tth2: duplicate in-flight PING payload")
	}
	waiter := make(chan pingResult, 1)
	p.pings[payload] = waiter
	return waiter, nil
}

func (p *connectionReadPump) cancelPing(payload [8]byte) {
	p.mu.Lock()
	delete(p.pings, payload)
	p.mu.Unlock()
}

func (p *connectionReadPump) completePing(payload [8]byte) {
	p.mu.Lock()
	waiter := p.pings[payload]
	delete(p.pings, payload)
	p.mu.Unlock()
	if waiter != nil {
		waiter <- pingResult{at: time.Now()}
	}
}
