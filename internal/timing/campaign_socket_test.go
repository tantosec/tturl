package timing

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"regexp"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

func groupH2Sender(c *h2Conn, p *h2Prepared, last bool) groupFixture {
	sender := groupFixture{
		discard:       func(err error) { c.close(err); <-c.readerDone; <-c.writerDone },
		lockReadiness: c.mu.Lock, unlockReadiness: c.mu.Unlock,
		waitInitial:     func(ctx context.Context) error { return c.h2WaitReady(ctx, p, p.Prefix[0]) },
		validateInitial: func() bool { return c.readyLocked(p, p.Prefix[0]) == nil },
	}
	if last {
		sender.waitFinal = func(ctx context.Context) error { return c.h2WaitReady(ctx, p, p.Final[0]) }
		sender.validateFinal = func() bool { return c.readyLocked(p, p.Final[0]) == nil }
	}
	sender.run = func(ctx context.Context, start, finish <-chan struct{}, initial func(time.Time) error,
		prefix func() error, exchange func(time.Time) error,
	) groupObservation {
		stop := context.AfterFunc(ctx, func() { c.close(context.Cause(ctx)) })
		defer stop()
		send := func() error {
			if err := awaitGate(ctx, start); err != nil {
				return err
			}
			if err := c.h2TransferWithRelease(ctx, p, p.Prefix, !last, initial); err != nil {
				return err
			}
			if err := prefix(); err != nil {
				return err
			}
			if last {
				if err := awaitGate(ctx, finish); err != nil {
					return err
				}
				if err := c.h2Transfer(ctx, p, p.Final, true); err != nil {
					return err
				}
			}
			return nil
		}
		if err := send(); err != nil {
			c.close(err)
		}
		observation := c.h2WaitResponse(ctx, p)
		result := groupObservation{
			initial: observation.Times.Initial, final: observation.Times.Final,
			headers: observation.Times.Headers, complete: observation.Times.Complete, err: observation.Err,
		}
		if cause := context.Cause(ctx); cause != nil {
			result.err = cause
		}
		if result.err == nil {
			result.err = c.h2Release(p)
		}
		if result.err == nil {
			result.err = exchange(laterEvent(observation.Times.Written, observation.Times.Complete))
		}
		return result
	}
	return sender
}

func TestCampaignRealMixedProtocolCommonGates(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		name := "TLS12"
		if version == tls.VersionTLS13 {
			name = "TLS13"
		}
		t.Run(name+" bodyless", func(t *testing.T) { groupMixedGates(t, version, false) })
		t.Run(name+" early", func(t *testing.T) { groupMixedGates(t, version, true) })
	}
}

func groupH1Setup(t *testing.T, version uint16) (groupFixture, <-chan error) {
	t.Helper()
	h1client, h1peer, _ := h1TLS(t, version)
	request := h1Request(t, nil, false)
	prepared, err := prepareH1(request, nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	h1done := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(h1peer)
		// Read exactly the prepared prefix before allowing the held final byte.
		prefix := make([]byte, len(prepared.records[0]))
		if _, err := io.ReadFull(reader, prefix); err != nil {
			h1done <- err
			return
		}
		final := make([]byte, 1)
		if _, err := io.ReadFull(reader, final); err != nil {
			h1done <- err
			return
		}
		_, err := io.WriteString(h1peer, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
		h1done <- err
	}()
	h1sender := groupFixture{
		discard: func(error) { _ = h1client.Close() },
		run: func(ctx context.Context, start, finish <-chan struct{}, initial func(time.Time) error,
			prefix func() error, exchange func(time.Time) error,
		) groupObservation {
			obs := runH1WithHooks(ctx, h1client, prepared, start, finish, 1<<20, 1<<20, initial, prefix)
			result := groupObservation{
				initial: obs.initial, final: obs.final,
				headers: obs.finalHeaders, complete: obs.complete, err: obs.err,
			}
			if cause := context.Cause(ctx); cause != nil {
				result.err = cause
			}
			if result.err == nil {
				result.err = exchange(laterEvent(obs.written, obs.complete))
			}
			return result
		},
	}
	return h1sender, h1done
}

func groupMixedGates(t *testing.T, version uint16, early bool) {
	t.Helper()
	h1sender, h1done := groupH1Setup(t, version)
	x := h2NewFixture(t, version)
	var body []byte
	if early {
		body = []byte{1}
	}
	p, err := x.client.h2Prepare(h2Fields(), body, true, true)
	if err != nil {
		t.Fatal(err)
	}
	h2done := make(chan struct{})
	go func() {
		defer close(h2done)
		for {
			select {
			case frame := <-x.frames:
				if frame.Frame.StreamID == p.Stream && len(frame.Fields) > 0 {
					x.respond(t, p.Stream, []hpack.HeaderField{
						{Name: ":status", Value: "200"}, {Name: "content-length", Value: "0"},
					}, true)
					if !early {
						return
					}
				}
				if early && frame.Frame.StreamID == p.Stream && frame.Frame.Type == http2.FrameData {
					if !frame.Frame.Flags.Has(http2.FlagDataEndStream) || len(frame.Body) != 1 {
						t.Error("held DATA did not finalise request")
					}
					return
				}
			case <-x.ctx.Done():
				return
			}
		}
	}()
	h2sender := groupH2Sender(x.client, p, true)
	if early {
		h2sender.waitFinal = func(ctx context.Context) error {
			for {
				x.client.mu.Lock()
				done, changed := p.Done, x.client.changed
				x.client.mu.Unlock()
				if done {
					return x.client.h2WaitReady(ctx, p, p.Final[0])
				}
				select {
				case <-changed:
				case <-ctx.Done():
					return context.Cause(ctx)
				}
			}
		}
	}
	result := runFixtureGroup(t.Context(), []groupFixture{h1sender, h2sender},
		newStartPacer(0, 0), time.Second, 0, true)
	if result.err != nil {
		t.Fatal(result.err)
	}
	if err := <-h1done; err != nil {
		t.Fatal(err)
	}
	<-h2done
	// Final readiness waits for response completion; clock readings may tie.
	if early && (result.outcomes[1].complete.IsZero() || result.outcomes[1].final.IsZero() ||
		result.outcomes[1].final.Before(result.outcomes[1].complete)) {
		t.Fatal("early response withdrew sender from legal finalisation")
	}
	for _, outcome := range result.outcomes {
		if result.startGate.IsZero() || result.finalGate.IsZero() ||
			outcome.initial.IsZero() || outcome.final.IsZero() ||
			outcome.initial.Before(result.startGate) || outcome.final.Before(result.finalGate) {
			t.Fatal("sender crossed a closed common gate")
		}
		if outcome.headers.IsZero() || outcome.complete.IsZero() {
			t.Fatal("mixed protocol response evidence missing")
		}
	}
}

func TestCampaignRealCommittedControlWriterReadinessTimeout(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	x.tap.mu.Lock()
	x.tap.block = make(chan struct{})
	x.tap.began = make(chan struct{}, 1)
	began := x.tap.began
	x.tap.mu.Unlock()
	pacer := newStartPacer(0, 0)
	sender := groupH2Sender(x.client, p, false)
	normal := sender.run
	committed := make(chan struct{})
	allowRequest := make(chan struct{})
	sender.run = func(ctx context.Context, start, finish <-chan struct{}, initial func(time.Time) error,
		prefix func() error, exchange func(time.Time) error,
	) groupObservation {
		if err := awaitGate(ctx, start); err != nil {
			return groupObservation{err: err}
		}
		close(committed)
		if err := awaitGate(ctx, allowRequest); err != nil {
			return groupObservation{err: err}
		}
		return normal(ctx, start, finish, initial, prefix, exchange)
	}
	done := make(chan groupEvidence, 1)
	go func() { done <- runFixtureGroup(x.ctx, []groupFixture{sender}, pacer, 100*time.Millisecond, 0, false) }()
	select {
	case <-committed:
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
	// The peer authorises the ACK only after the initial gate has committed.
	x.send(t, func(f *http2.Framer) error { return f.WritePing(false, [8]byte{7}) })
	select {
	case <-began:
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
	select {
	case <-x.client.done:
		t.Fatal("readiness expired before controlled wire blockage")
	default:
	}
	close(allowRequest)
	select {
	case result := <-done:
		if !errors.Is(result.err, context.DeadlineExceeded) {
			t.Fatalf("readiness timeout: %v", result.err)
		}
		if !result.committed || pacer.charges != 1 {
			t.Fatal("blocked control writer lost committed charge")
		}
		if !result.outcomes[0].initial.IsZero() {
			t.Fatal("wire waiter fabricated initial release")
		}
	case <-time.After(time.Second):
		t.Fatal("committed sender stranded behind control writer")
	}
	select {
	case <-x.client.writerDone:
	case <-time.After(time.Second):
		t.Fatal("blocked control writer stranded")
	}
}

func TestCampaignReaderMilestonesPrecedeDelayedConsumer(t *testing.T) {
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), nil, true, false)
	if err != nil {
		t.Fatal(err)
	}
	sender := groupH2Sender(x.client, p, false)
	consumerReady := make(chan struct{})
	allowConsumer := make(chan struct{})
	consumed := make(chan struct{})
	var hashStarted time.Time
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		for {
			select {
			case frame := <-x.frames:
				if frame.Frame.StreamID == p.Stream && len(frame.Fields) > 0 {
					x.respond(t, p.Stream, []hpack.HeaderField{
						{Name: ":status", Value: "200"}, {Name: "content-length", Value: "10"},
					}, false)
					x.send(t, func(f *http2.Framer) error { return f.WriteData(p.Stream, true, []byte("opaque=123")) })
					return
				}
			case <-x.ctx.Done():
				return
			}
		}
	}()
	done := make(chan groupEvidence, 1)
	go func() {
		result := runFixtureGroup(x.ctx, []groupFixture{sender}, newStartPacer(0, 0), time.Second, 0, false)
		close(consumerReady)
		if err := awaitGate(x.ctx, allowConsumer); err != nil {
			result.err = err
			done <- result
			return
		}
		x.client.mu.Lock()
		body := append([]byte(nil), p.Obs.Body...)
		x.client.mu.Unlock()
		hashStarted = time.Now()
		digest := sha256.Sum256(body)
		if digest != sha256.Sum256([]byte("opaque=123")) {
			t.Error("accepted body digest changed")
		}
		if value := regexp.MustCompile(`opaque=(\d+)`).FindSubmatch(body); len(value) != 2 || string(value[1]) != "123" {
			t.Error("downstream extraction lost retained evidence")
		}
		close(consumed)
		done <- result
	}()
	select {
	case <-consumerReady:
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
	x.client.mu.Lock()
	headers, end := p.Obs.Times.Headers, p.Obs.Times.Complete
	x.client.mu.Unlock()
	if headers.IsZero() || end.IsZero() {
		t.Fatal("reader observations waited for consumer scheduling")
	}
	close(allowConsumer)
	<-consumed
	result := <-done
	<-peerDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	// The consumer gate establishes order even when clock readings coincide.
	if hashStarted.IsZero() || hashStarted.Before(headers) || hashStarted.Before(end) {
		t.Fatal("hashing or extraction selected receive endpoints")
	}
	if result.outcomes[0].headers != headers || result.outcomes[0].complete != end {
		t.Fatal("consumer reconstructed reader times")
	}
}

func waitH2State(t *testing.T, x *h2Fixture, check func() bool) {
	t.Helper()
	for {
		x.client.mu.Lock()
		ready, changed := check(), x.client.changed
		x.client.mu.Unlock()
		if ready {
			return
		}
		select {
		case <-changed:
		case <-x.ctx.Done():
			t.Fatal(x.ctx.Err())
		}
	}
}

func TestCampaignFinalCreditChangeDuringDelay(t *testing.T) {
	h1sender, h1done := groupH1Setup(t, tls.VersionTLS13)
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), []byte{1}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	h2sender := groupH2Sender(x.client, p, true)
	firstReady := make(chan struct{})
	invalidated := make(chan struct{})
	var readyOnce, invalidOnce sync.Once
	h2sender.waitFinal = func(ctx context.Context) error {
		err := x.client.h2WaitReady(ctx, p, p.Final[0])
		if err == nil {
			readyOnce.Do(func() { close(firstReady) })
		}
		return err
	}
	h2sender.validateFinal = func() bool {
		err := x.client.readyLocked(p, p.Final[0])
		if errors.Is(err, errH2NotReady) {
			invalidOnce.Do(func() { close(invalidated) })
		}
		return err == nil
	}
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		for {
			select {
			case frame := <-x.frames:
				if frame.Frame.StreamID == p.Stream && frame.Frame.Type == http2.FrameData {
					x.respond(t, p.Stream, []hpack.HeaderField{{Name: ":status", Value: "200"}}, true)
					return
				}
			case <-x.ctx.Done():
				return
			}
		}
	}()
	done := make(chan groupEvidence, 1)
	delay := 40 * time.Millisecond
	go func() {
		done <- runFixtureGroup(x.ctx, []groupFixture{h1sender, h2sender}, newStartPacer(0, 0), time.Second, delay, true)
	}()
	<-firstReady
	x.send(t, func(f *http2.Framer) error {
		return f.WriteSettings(http2.Setting{ID: http2.SettingInitialWindowSize, Val: 0})
	})
	waitH2State(t, x, func() bool { return x.client.streamWindow == 0 })
	select {
	case <-invalidated:
	case <-x.ctx.Done():
		t.Fatal(x.ctx.Err())
	}
	x.client.mu.Lock()
	final := p.Obs.Times.Final
	x.client.mu.Unlock()
	if !final.IsZero() {
		t.Fatal("invalidated final credit passed the common gate")
	}
	select {
	case <-h1done:
		t.Fatal("other pool released while H2 final credit was missing")
	default:
	}
	granted := time.Now()
	x.send(t, func(f *http2.Framer) error { return f.WriteWindowUpdate(p.Stream, 1) })
	result := <-done
	<-peerDone
	if result.err != nil {
		t.Fatal(result.err)
	}
	if err := <-h1done; err != nil {
		t.Fatal(err)
	}
	if result.finalGate.Sub(granted) < delay {
		t.Fatal("release delay overlapped checked again final credit wait")
	}
	if result.outcomes[1].final.Sub(result.outcomes[1].initial) < 2*delay {
		t.Fatal("final readiness failure bypassed the common delay")
	}
}

func TestCampaignRealResetBeforeMixedFinalGate(t *testing.T) {
	h1sender, h1done := groupH1Setup(t, tls.VersionTLS13)
	x := h2NewFixture(t, tls.VersionTLS13)
	p, err := x.client.h2Prepare(h2Fields(), []byte{1}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	h2sender := groupH2Sender(x.client, p, true)
	h2sender.waitFinal = func(ctx context.Context) error {
		for {
			x.client.mu.Lock()
			reset, changed := p.Reset, x.client.changed
			x.client.mu.Unlock()
			if reset {
				return x.client.h2WaitReady(ctx, p, p.Final[0])
			}
			select {
			case <-changed:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
	}
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		for {
			select {
			case frame := <-x.frames:
				if frame.Frame.StreamID == p.Stream && len(frame.Fields) > 0 {
					x.respond(t, p.Stream, []hpack.HeaderField{
						{Name: ":status", Value: "200"},
						{Name: "content-length", Value: "0"},
					}, true)
					waitH2State(t, x, func() bool { return p.Done })
					x.send(t, func(f *http2.Framer) error { return f.WriteRSTStream(p.Stream, http2.ErrCodeCancel) })
					return
				}
			case <-x.ctx.Done():
				return
			}
		}
	}()
	result := runFixtureGroup(x.ctx, []groupFixture{h1sender, h2sender}, newStartPacer(0, 0), time.Second, 0, true)
	<-peerDone
	<-h1done
	if result.err == nil || !result.finalGate.IsZero() {
		t.Fatal("reset released a successful subset")
	}
	for _, outcome := range result.outcomes {
		if !outcome.final.IsZero() {
			t.Fatal("reset fabricated a final release")
		}
	}
	if result.outcomes[1].headers.IsZero() || result.outcomes[1].complete.IsZero() {
		t.Fatal("reset discarded valid early response evidence")
	}
}
