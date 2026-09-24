// Package netdelay delays traffic on accepted stream connections.
//
// It is a small, portable propagation model for local tests and
// demonstrations. The model operates on stream chunks observed by net.Conn,
// not network packets, and does not emulate bandwidth, loss, congestion
// control, or packet queues.
package netdelay

import (
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	queueDepth       = 1024
	seedMix          = 0x9e3779b97f4a7c15
	maximumDuration  = time.Duration(1<<63 - 1)
	maximumVariation = maximumDuration - 1
)

// Config describes the delay applied in each direction of an accepted
// connection. Delay is the minimum one-way propagation time. Variation is the
// maximum additional one-way delay sampled independently for each observed
// stream chunk. A zero Seed selects an internal random seed; a nonzero Seed
// makes the generated sample streams repeatable.
//
// A seed reproduces the generated sample streams for the same connection and
// direction. Stream chunk boundaries and goroutine scheduling remain operating
// system effects, so it does not make end-to-end timings deterministic.
type Config struct {
	// Delay is the minimum one-way propagation time.
	Delay time.Duration
	// Variation is the maximum additional per-chunk one-way delay.
	Variation time.Duration
	// Seed selects repeatable sample streams. Zero chooses a random seed.
	Seed uint64
}

// NewListenerWrapper validates config and returns a listener-construction
// function. The wrapper preserves the original listener address. A zero Config
// returns an identity wrapper.
func NewListenerWrapper(config Config) (func(net.Listener) net.Listener, error) {
	if config.Delay < 0 {
		return nil, fmt.Errorf("netdelay: delay must be non-negative")
	}
	if config.Variation < 0 {
		return nil, fmt.Errorf("netdelay: variation must be non-negative")
	}
	if config.Variation > maximumVariation {
		return nil, fmt.Errorf("netdelay: variation is too large")
	}
	if config.Variation > maximumDuration-config.Delay {
		return nil, fmt.Errorf(
			"netdelay: delay plus variation overflows time.Duration")
	}
	if config.Delay == 0 && config.Variation == 0 {
		return func(listener net.Listener) net.Listener { return listener }, nil
	}
	if config.Variation > 0 && config.Seed == 0 {
		config.Seed = randomSeed()
	}
	return func(listener net.Listener) net.Listener {
		return &delayListener{Listener: listener, config: config}
	}, nil
}

func randomSeed() uint64 {
	for {
		if seed := rand.Uint64(); seed != 0 { //nolint:gosec // simulation only
			return seed
		}
	}
}

// delayListener bridges each accepted socket through an in-memory connection.
// Reading ahead allows a source write to complete while its bytes await their
// scheduled delivery, up to the bounded queue.
type delayListener struct {
	net.Listener
	config Config
	next   atomic.Uint64
}

func (l *delayListener) Accept() (net.Conn, error) {
	peer, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	server, relay := net.Pipe()
	connection := l.next.Add(1)
	go runRelay(peer, relay, l.config, connection)
	return &addressedConn{
		Conn: server, local: peer.LocalAddr(), remote: peer.RemoteAddr(),
	}, nil
}

// addressedConn preserves the socket addresses hidden by net.Pipe.
type addressedConn struct {
	net.Conn
	local  net.Addr
	remote net.Addr
}

func (c *addressedConn) LocalAddr() net.Addr  { return c.local }
func (c *addressedConn) RemoteAddr() net.Addr { return c.remote }

type chunk struct {
	data      []byte
	deliverAt time.Time
}

func runRelay(peer, relay net.Conn, config Config, connection uint64) {
	done := make(chan struct{})
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			close(done)
			_ = peer.Close()
			_ = relay.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		delayedCopy(
			relay, peer, config,
			config.Seed+2*connection*seedMix, done)
		stop()
	}()
	go func() {
		defer wg.Done()
		delayedCopy(
			peer, relay, config,
			config.Seed+(2*connection+1)*seedMix, done)
		stop()
	}()
	wg.Wait()
}

func delayedCopy(
	destination, source net.Conn,
	config Config,
	seed uint64,
	done <-chan struct{},
) {
	chunks := make(chan chunk, queueDepth)
	go readChunks(source, chunks, config, seed, done)
	for item := range chunks {
		if !waitUntil(item.deliverAt, done) {
			return
		}
		if _, err := destination.Write(item.data); err != nil {
			return
		}
	}
}

func readChunks(
	source net.Conn,
	chunks chan<- chunk,
	config Config,
	seed uint64,
	done <-chan struct{},
) {
	defer close(chunks)
	// This stream models repeatable variation; it is not a security boundary.
	rng := rand.New( //nolint:gosec // deterministic simulation requires a seed
		rand.NewPCG(seed, seed^seedMix))
	var lastDelivery time.Time
	buffer := make([]byte, 32*1024)
	for {
		n, err := source.Read(buffer)
		if n > 0 {
			delivery := scheduleDelivery(
				time.Now(), lastDelivery, sampleDelay(rng, config))
			lastDelivery = delivery
			data := append([]byte(nil), buffer[:n]...)
			select {
			case chunks <- chunk{data: data, deliverAt: delivery}:
			case <-done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func sampleDelay(rng *rand.Rand, config Config) time.Duration {
	variation := rng.Int64N(int64(config.Variation) + 1)
	return config.Delay + time.Duration(variation)
}

func scheduleDelivery(
	now, lastDelivery time.Time,
	delay time.Duration,
) time.Time {
	delivery := now.Add(delay)
	if delivery.Before(lastDelivery) {
		return lastDelivery
	}
	return delivery
}

func waitUntil(deadline time.Time, done <-chan struct{}) bool {
	wait := time.Until(deadline)
	if wait <= 0 {
		return true
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-done:
		return false
	}
}
