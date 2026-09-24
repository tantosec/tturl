package tth2_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"math/rand"
	"net"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/internal/h2tls"
	"github.com/tantosec/tturl/internal/netdelay"
	"github.com/tantosec/tturl/tth2"
)

// bodyDoneHeader carries the server-side wall-clock timestamp (Unix nanos)
// captured AFTER the handler has drained the request body — the moment Phase
// 2's END_STREAM unblocks r.Body's EOF.
const bodyDoneHeader = "X-Body-Done-Nanos"

// bodyDoneHandler discards the request body and emits a wall-clock timestamp
// captured at EOF. Minimal handler work keeps the spread measurement clean:
// no
// hashing, no header echo beyond the timestamp.
var bodyDoneHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set(bodyDoneHeader, strconv.FormatInt(time.Now().UnixNano(), 10))
})

type spreadScenario struct {
	name      string
	shapeName string
	shape     uint8
	n         uint16
	dist      LaggyDist
	jitterMs  uint32
}

var spreadScenarios = []spreadScenario{
	{"NoBody/N=2/Uniform/J=0", "NoBody/N=2", shapeNoBody, 2, LaggyUniform, 0},
	{"NoBody/N=32/Spike/J=50", "NoBody/N=32", shapeNoBody, 32, LaggySpike, 50},
	{"NoBody/N=64/Uniform/J=40", "NoBody/N=64", shapeNoBody, 64, LaggyUniform, 40},
	{"Small/N=8/Normal/J=50", "Small/N=8", shapeSmallBody, 8, LaggyNormal, 50},
	{"Large/N=2/Spike/J=50", "Large/N=2", shapeLargeBody, 2, LaggySpike, 50},
	{"Large/N=32/Uniform/J=40", "Large/N=32", shapeLargeBody, 32, LaggyUniform, 40},
	{"Large/N=48/Normal/J=20", "Large/N=48", shapeLargeBody, 48, LaggyNormal, 20},
	{"Asym/N=16/Uniform/J=40", "Asym/N=16", shapeAsymmetric, 16, LaggyUniform, 40},
}

// BenchmarkBatchReleaseSpread reports per-iteration spread of body-done
// timestamps across representative combinations of shape, batch size,
// distribution, and jitter.
// Each sub-benchmark runs repeated Sends and reports p50, p95, and pmax for
// both the complete batch span and its mean adjacent completion gap.
//
// Spread after a single-record batch release depends on GOMAXPROCS, core speed,
// GC pacing, and scheduler luck — too noisy to assert as a hard ceiling
// without flaking, but a real regression (the final release splitting across
// records) shows up as a step change in the distribution. Compare runs via
// `benchstat old.txt new.txt` rather than gating on a fixed threshold.
//
// Run with -benchtime=N(x|s) to control sample count; longer runs give tighter
// p95 estimates. Run with -count=N to repeat the whole benchmark for variance
// estimates across runs. Pipe the output through `column -t -s $'\t'` for
// consistent alignment regardless of terminal tab stops.
func BenchmarkBatchReleaseSpread(b *testing.B) {
	addr, tlsCfg := h2test.Serve(b, bodyDoneHandler)
	warmTestServer(b, addr, tlsCfg)
	benchmarkSpreadScenarios(b, addr, tlsCfg, false, func(sc spreadScenario) func(
		context.Context, string, string,
	) (net.Conn, error) {
		rngSeed := derivedRngSeed(
			sc.shape, sc.n, uint8(sc.dist), sc.jitterMs)
		return newLaggyDialer(rngSeed, LaggyPolicy{
			Kind: sc.dist, MaxMs: int(sc.jitterMs),
		})
	})
}

// BenchmarkBatchReleaseSpreadThroughNetdelay measures the same request shapes
// with traffic relayed asynchronously below TLS. An effectively zero-delay
// relay control separates relay overhead from the fixed-floor and
// additive-variation cases. Server-side body completion spread is the relevant
// outcome: a shared final TLS record should make path variation common to the
// batch.
func BenchmarkBatchReleaseSpreadThroughNetdelay(b *testing.B) {
	models := []struct {
		name      string
		delay     time.Duration
		variation time.Duration
	}{
		{"RelayOnly/Variation0", time.Nanosecond, 0},
		{"Floor20ms/Variation0", 20 * time.Millisecond, 0},
		{
			"Floor20ms/Variation40ms",
			20 * time.Millisecond,
			40 * time.Millisecond,
		},
	}
	for _, model := range models {
		b.Run(model.name, func(b *testing.B) {
			wrapper, err := netdelay.NewListenerWrapper(netdelay.Config{
				Delay:     model.delay,
				Variation: model.variation,
				Seed:      1,
			})
			if err != nil {
				b.Fatalf("NewListenerWrapper: %v", err)
			}
			addr, tlsCfg := h2test.Serve(
				b,
				bodyDoneHandler,
				h2tls.WithListenerWrapper(wrapper),
			)
			warmTestServer(b, addr, tlsCfg)
			benchmarkSpreadScenarios(b, addr, tlsCfg, true, nil)
		})
	}
}

// TestTailReleaseSendSurvivesNetdelay exercises the complete send path through
// asynchronous delay and per-chunk variation below TLS. It asserts the
// deterministic integration contract: every body completes and every response
// receives a rank. TestFinalReleaseUsesOneTLSRecord checks the exact
// single-record release on the encrypted wire. The
// BenchmarkBatchReleaseSpreadThroughNetdelay benchmark measures the
// scheduler-sensitive outcome without imposing a flaky latency ceiling.
func TestTailReleaseSendSurvivesNetdelay(t *testing.T) {
	t.Parallel()
	const (
		requests = 8
		trials   = 2
	)
	wrapper, err := netdelay.NewListenerWrapper(netdelay.Config{
		Delay:     20 * time.Millisecond,
		Variation: 40 * time.Millisecond,
		Seed:      1,
	})
	if err != nil {
		t.Fatalf("NewListenerWrapper: %v", err)
	}
	addr, tlsConfig := h2test.Serve(
		t, bodyDoneHandler, h2tls.WithListenerWrapper(wrapper))
	transport := &tth2.Transport{TLSClientConfig: tlsConfig}
	t.Cleanup(transport.CloseIdleConnections)
	client := &tth2.Client{Transport: transport}
	reqs := make([]*http.Request, requests)
	for i := range reqs {
		req, err := http.NewRequestWithContext(
			t.Context(), "POST", "https://"+addr+"/",
			bytes.NewReader(bytes.Repeat([]byte{byte(i)}, 64)),
		)
		if err != nil {
			t.Fatalf("NewRequest %d: %v", i, err)
		}
		reqs[i] = req
	}
	for trial := range trials {
		batch, err := client.SendBatch(
			t.Context(), reqs, tth2.WithResponseHeaderCapture())
		if err != nil {
			t.Fatalf("trial %d: SendBatch: %v", trial, err)
		}
		requireBodyDoneTimes(t, batch, requests)
		seenRanks := make([]bool, requests)
		for request, result := range batch.Results {
			if result.ArrivalRank < 0 || result.ArrivalRank >= requests {
				t.Errorf("trial %d request %d rank = %d, want [0, %d)",
					trial, request, result.ArrivalRank, requests)
				continue
			}
			if seenRanks[result.ArrivalRank] {
				t.Errorf("trial %d rank %d appears more than once",
					trial, result.ArrivalRank)
			}
			seenRanks[result.ArrivalRank] = true
		}
	}
}

func benchmarkSpreadScenarios(
	b *testing.B,
	addr string,
	tlsCfg *tls.Config,
	useShapeName bool,
	dialerFor func(spreadScenario) func(
		context.Context, string, string,
	) (net.Conn, error),
) {
	b.Helper()
	for _, sc := range spreadScenarios {
		name := sc.name
		if useShapeName {
			name = sc.shapeName
		}
		b.Run(name, func(b *testing.B) {
			rngSeed := derivedRngSeed(sc.shape, sc.n, uint8(sc.dist), sc.jitterMs)

			ctx, cancel := context.WithTimeout(b.Context(), 30*time.Minute)
			defer cancel()

			tr := &tth2.Transport{TLSClientConfig: tlsCfg}
			if dialerFor != nil {
				tr.DialContext = dialerFor(sc)
			}
			defer tr.CloseIdleConnections()
			client := &tth2.Client{Transport: tr}

			bodyRng := rand.New(rand.NewSource(rngSeed)) //nolint:gosec // math/rand in tests, not security-sensitive
			bodies := bodiesForShape(sc.shape, int(sc.n), bodyRng)
			reqs := make([]*http.Request, len(bodies))
			for i, body := range bodies {
				if body == nil {
					req, err := http.NewRequestWithContext(ctx, "GET", "https://"+addr+"/", nil)
					if err != nil {
						b.Fatalf("NewRequest: %v", err)
					}
					reqs[i] = req
					continue
				}
				req, err := http.NewRequestWithContext(ctx, "POST", "https://"+addr+"/", bytes.NewReader(body))
				if err != nil {
					b.Fatalf("NewRequest: %v", err)
				}
				reqs[i] = req
			}

			var spreads []time.Duration
			for b.Loop() {
				batch, err := client.SendBatch(
					ctx, reqs, tth2.WithResponseHeaderCapture())
				if err != nil {
					b.Fatalf("SendBatch: %v", err)
				}
				if got, want := len(batch.Results), len(reqs); got != want {
					b.Fatalf("len(Results) = %d, want %d", got, want)
				}
				dones := requireBodyDoneTimes(b, batch, len(reqs))
				spreads = append(spreads, spreadNanos(dones))
			}

			if len(spreads) == 0 {
				return
			}
			slices.Sort(spreads)
			p50 := spreads[len(spreads)/2]
			p95 := spreads[len(spreads)*95/100]
			pmax := spreads[len(spreads)-1]
			reportSpreadMetric(b, "p50", p50, len(reqs))
			reportSpreadMetric(b, "p95", p95, len(reqs))
			reportSpreadMetric(b, "pmax", pmax, len(reqs))
		})
	}
}

func requireBodyDoneTimes(
	tb testing.TB,
	batch *tth2.BatchResult,
	want int,
) []int64 {
	tb.Helper()
	if got := len(batch.Results); got != want {
		tb.Fatalf("len(Results) = %d, want %d", got, want)
	}
	dones := make([]int64, len(batch.Results))
	for i, result := range batch.Results {
		if !result.Arrived() {
			tb.Fatalf("result %d: result/response = %v", i, result)
		}
		if result.Reset != nil || result.Response.StatusCode != http.StatusOK {
			tb.Fatalf("result %d: reset/status = %v/%d, want nil/200",
				i, result.Reset, result.Response.StatusCode)
		}
		raw := result.Response.Header.Get(bodyDoneHeader)
		if raw == "" {
			tb.Fatalf("result %d: %s is absent", i, bodyDoneHeader)
		}
		nanos, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			tb.Fatalf("result %d: parse %s %q: %v",
				i, bodyDoneHeader, raw, err)
		}
		dones[i] = nanos
	}
	return dones
}

// reportSpreadMetric reports both the complete first-to-last completion span
// and that span divided across its N-1 adjacent completion gaps. The latter is
// a mean gap, not a percentile of the individual gaps.
func reportSpreadMetric(
	b *testing.B,
	quantile string,
	span time.Duration,
	requests int,
) {
	b.Helper()
	b.ReportMetric(
		float64(span)/float64(time.Millisecond), quantile+"_span_ms")
	if requests > 1 {
		meanGap := float64(span) / float64(requests-1)
		b.ReportMetric(meanGap/float64(time.Microsecond), quantile+"_mean_gap_us")
	}
}

// spreadNanos returns max(ts) - min(ts) as a Duration, or 0 for fewer than two
// elements.
func spreadNanos(ts []int64) time.Duration {
	if len(ts) < 2 {
		return 0
	}
	mn, mx := ts[0], ts[0]
	for _, t := range ts[1:] {
		if t < mn {
			mn = t
		}
		if t > mx {
			mx = t
		}
	}
	return time.Duration(mx - mn)
}

// TestSpreadNanos derives the benchmark's aggregation from exact unordered
// timestamps. It keeps malformed or missing samples from looking like a good
// zero-spread measurement; the benchmark separately requires one timestamp per
// response before calling this helper.
func TestSpreadNanos(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		ts   []int64
		want time.Duration
	}{
		{"empty", nil, 0},
		{"one", []int64{12}, 0},
		{"equal", []int64{7, 7, 7}, 0},
		{"unordered", []int64{20, -5, 12, 40}, 45},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := spreadNanos(tc.ts); got != tc.want {
				t.Errorf("spreadNanos(%v) = %v, want %v", tc.ts, got, tc.want)
			}
		})
	}
}
