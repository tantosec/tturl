package tth2_test

import (
	"context"
	"crypto/tls"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// LaggyDist selects the delay distribution applied before each I/O call.
//
//   - LaggyUniform: uniform [0, MaxMs]ms on every call.
//   - LaggyNormal: Normal(MaxMs/2, MaxMs/4) clipped to [0, MaxMs].
//   - LaggySpike: 95% zero, 5% exactly MaxMs for sparse stalls.
type LaggyDist uint8

const (
	LaggyUniform LaggyDist = iota
	LaggyNormal
	LaggySpike
)

// LaggyPolicy describes how a LaggyConn samples per-call delays.
type LaggyPolicy struct {
	Kind  LaggyDist
	MaxMs int // delay cap (uniform); mean*2 (normal); spike size (spike)
}

func (p LaggyPolicy) sample(rng *rand.Rand) time.Duration {
	if p.MaxMs <= 0 {
		return 0
	}
	switch p.Kind {
	case LaggyUniform:
		return time.Duration(rng.Intn(p.MaxMs+1)) * time.Millisecond
	case LaggyNormal:
		mean := float64(p.MaxMs) / 2
		stddev := float64(p.MaxMs) / 4
		v := rng.NormFloat64()*stddev + mean
		if v < 0 {
			v = 0
		}
		if v > float64(p.MaxMs) {
			v = float64(p.MaxMs)
		}
		return time.Duration(v) * time.Millisecond
	case LaggySpike:
		if rng.Intn(20) == 0 {
			return time.Duration(p.MaxMs) * time.Millisecond
		}
		return 0
	}
	return 0
}

// LaggyConn wraps a net.Conn and pauses before each Write and Read. A Write
// pause blocks tth2's sending goroutine and separates successive TLS writes. A
// Read pause occurs before the caller knows whether bytes are available. These
// synchronous stalls stress scheduling and write boundaries; they do not model
// asynchronous network propagation.
type LaggyConn struct {
	net.Conn
	rng    *rand.Rand
	rngMu  *sync.Mutex
	policy LaggyPolicy
}

func (l *LaggyConn) Write(p []byte) (int, error) {
	l.rngMu.Lock()
	d := l.policy.sample(l.rng)
	l.rngMu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	return l.Conn.Write(p)
}

func (l *LaggyConn) Read(p []byte) (int, error) {
	l.rngMu.Lock()
	d := l.policy.sample(l.rng)
	l.rngMu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	return l.Conn.Read(p)
}

// newLaggyDialer returns a function suitable for [tth2.Transport.DialContext].
// Each connection gets a random stream derived from seed and its dial order, so
// concurrent I/O on one connection cannot consume samples meant for another.
// Scheduling may still change which benchmark worker receives each connection.
func newLaggyDialer(seed int64, policy LaggyPolicy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	const seedMix int64 = 0x61c8864680b583eb
	var next atomic.Int64
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		connectionSeed := seed + next.Add(1)*seedMix
		rng := rand.New( //nolint:gosec // deterministic test workload
			rand.NewSource(connectionSeed))
		return &LaggyConn{
			Conn: c, rng: rng, rngMu: new(sync.Mutex), policy: policy,
		}, nil
	}
}

// Send-shape selectors for laggy tests. See bodiesForShape.
const (
	shapeNoBody     uint8 = 0
	shapeSmallBody  uint8 = 1
	shapeLargeBody  uint8 = 2
	shapeAsymmetric uint8 = 3
	shapeMixed      uint8 = 4
	numShapes       uint8 = 5
)

// derivedRngSeed maps a fuzz/benchmark input tuple to a deterministic rng seed.
// Pack the four fields into an int64 so the relationship is trivial to
// reproduce by hand from a failing fuzz input.
func derivedRngSeed(shape uint8, n uint16, distKind uint8, jitterMs uint32) int64 {
	return int64(jitterMs)<<32 | int64(n)<<16 | int64(distKind)<<8 | int64(shape)
}

// bodiesForShape returns the per-stream request bodies for a given shape. A nil
// entry means "no body" (GET); a non-nil entry is a POST with that body. The
// rng drives body content so the same seed yields the same body bytes — a
// precondition for fuzz reproducibility.
func bodiesForShape(shape uint8, n int, rng *rand.Rand) [][]byte {
	bodies := make([][]byte, n)
	switch shape {
	case shapeNoBody:
		// All nil → all GETs.
	case shapeSmallBody:
		for i := range bodies {
			bodies[i] = randomBody(rng, 256+i*16)
		}
	case shapeLargeBody:
		// 16 KB to 16 KB + (n-1) KB. At N=64 the largest body is ~80 KB — spans
		// several 16 KB DATA frames and forces flow-control drains.
		for i := range bodies {
			bodies[i] = randomBody(rng, 16384+i*1024)
		}
	case shapeAsymmetric:
		// Sharply different sizes per stream exercise progress-aware body
		// scheduling.
		sizes := []int{16, 128, 4096, 16384, 32768, 65535, 256, 8192}
		for i := range bodies {
			bodies[i] = randomBody(rng, sizes[i%len(sizes)])
		}
	case shapeMixed:
		// Alternate GET / POST. Exercises sendWithBody's empty-body finalisation via
		// END_STREAM on the shared HEADERS record.
		for i := range bodies {
			if i%2 == 1 {
				bodies[i] = randomBody(rng, 1024)
			}
		}
	}
	return bodies
}

func randomBody(rng *rand.Rand, size int) []byte {
	b := make([]byte, size)
	_, _ = rng.Read(b)
	return b
}

// warmTestServer dispatches a handful of Sends through the shared test server
// before the first measured iteration, so cold-start (GC, scheduler workers,
// page faults) doesn't taint either fuzz coverage or benchmark measurements.
func warmTestServer(tb testing.TB, addr string, tlsCfg *tls.Config) {
	const sends = 5
	ctx, cancel := context.WithTimeout(tb.Context(), 10*time.Second)
	defer cancel()
	client := newClient(tb, tlsCfg)
	reqs := make([]*http.Request, 32)
	for i := range reqs {
		req, err := http.NewRequestWithContext(ctx, "GET", "https://"+addr+"/", nil)
		if err != nil {
			tb.Fatalf("warmup NewRequest: %v", err)
		}
		reqs[i] = req
	}
	for range sends {
		if _, err := client.SendBatch(ctx, reqs); err != nil {
			tb.Fatalf("warmup Send: %v", err)
		}
	}
}
