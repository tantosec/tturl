package tth2_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
	"github.com/tantosec/tturl/tth2"
)

// fillBody fills b with a per-stream-deterministic pattern. Each byte's value
// depends on its position AND the stream identifier, so cross-stream
// contamination, intra-stream reordering, and truncation all break the
// resulting SHA-256.
func fillBody(b []byte, streamSeed int) {
	for j := range b {
		b[j] = byte(j ^ (streamSeed*0x9E + 0x37)) //nolint:gosec // test payload byte; wrap is intentional
	}
}

// buildStreamRequests creates POST requests for an addr, one per element of
// sizes, each carrying a unique body of the corresponding length. The returned
// wantHash[i] is the expected SHA-256 the server should compute over reqs[i]'s
// body.
func buildStreamRequests(
	tb testing.TB,
	ctx context.Context,
	addr string,
	sizes []int,
) (reqs []*http.Request, wantHash []string) {
	tb.Helper()
	reqs = make([]*http.Request, len(sizes))
	wantHash = make([]string, len(sizes))
	for i, sz := range sizes {
		body := make([]byte, sz)
		fillBody(body, i+1)
		sum := sha256.Sum256(body)
		wantHash[i] = hex.EncodeToString(sum[:])
		req, err := http.NewRequestWithContext(ctx, "POST",
			"https://"+addr+"/", bytes.NewReader(body))
		if err != nil {
			tb.Fatalf("size=%d stream=%d: NewRequest: %v", sz, i, err)
		}
		reqs[i] = req
	}
	return reqs, wantHash
}

// assertResultsMatchHashes checks that each result's bodyHashHeader matches the
// expected SHA-256 for that stream. Wraps the inner-loop checks of every
// body-scheduling integrity test.
func assertResultsMatchHashes(tb testing.TB, results []tth2.Result, wantHash []string, label string) {
	tb.Helper()
	if got := len(results); got != len(wantHash) {
		tb.Fatalf("%s: len(results) = %d, want %d", label, got, len(wantHash))
	}
	for i, r := range results {
		if !r.Arrived() {
			tb.Errorf("%s stream=%d: nil response", label, i)
			continue
		}
		if r.Response.StatusCode != http.StatusOK {
			tb.Errorf("%s stream=%d: status = %d, want 200",
				label, i, r.Response.StatusCode)
			continue
		}
		if got := r.Response.Header.Get(bodyHashHeader); got != wantHash[i] {
			tb.Errorf("%s stream=%d: server sha256 = %s, want %s",
				label, i, got, wantHash[i])
		}
	}
}

// TestSmearedSendSymmetricN verifies that the body-prefix scheduler delivers
// every byte intact at representative stream counts: one, two, 32, 33,
// and 64 streams. The bodies are large enough (256 KiB) that each
// batch's body prefix spans many TLS records and exercises the record budget
// arithmetic; any reordering, truncation, duplication, or cross-stream
// contamination breaks the server's SHA-256.
func TestSmearedSendSymmetricN(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, sha256Handler)

	for _, n := range []int{1, 2, 32, 33, 64} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			c := newClient(t, tlsCfg)

			sizes := make([]int, n)
			for i := range sizes {
				sizes[i] = 256 << 10
			}
			reqs, wantHash := buildStreamRequests(t, ctx, addr, sizes)

			batch, err := c.SendBatch(
				ctx, reqs, tth2.WithResponseHeaderCapture())
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			results := batch.Results
			assertResultsMatchHashes(t, results, wantHash, fmt.Sprintf("N=%d", n))
		})
	}
}

// TestSmearedSendAsymmetricBodies covers bodies of wildly different lengths
// released in one batch. The scheduler paces the small streams while retaining
// their withheld final byte for the shared release, rather than finishing
// early and parking handlers in Body.Read. The 5 MiB body forces many
// flow-control drains because the stdlib server's default per-stream upload
// window is 1 MiB. This exercises the drain-with-timeout path under healthy
// server conditions.
func TestSmearedSendAsymmetricBodies(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, sha256Handler)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	c := newClient(t, tlsCfg)

	sizes := []int{50, 5 << 10, 500 << 10, 5 << 20}
	reqs, wantHash := buildStreamRequests(t, ctx, addr, sizes)

	batch, err := c.SendBatch(
		ctx, reqs, tth2.WithResponseHeaderCapture())
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	results := batch.Results
	assertResultsMatchHashes(t, results, wantHash, "asymmetric")
}

// TestSmearedSendPathological covers the extreme-ratio and mixed-with-empty
// shapes where the scheduling math is at its tightest. The expectation here
// is simply that the algorithm completes correctly without hangs, errors, or
// data corruption.
func TestSmearedSendPathological(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, sha256Handler)

	tests := []struct {
		name  string
		sizes []int
	}{
		{"two-tiny", []int{1, 1}},
		{"extreme-ratio", []int{1, 10 << 20}},
		{"empty-mixed-with-bodies", []int{0, 1 << 12, 0, 1 << 16, 1 << 14}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()

			c := newClient(t, tlsCfg)

			// For "empty" streams, build a POST with an empty body reader. Its body
			// length is zero, and sha256Handler returns the hash of zero bytes.
			reqs, wantHash := buildStreamRequests(t, ctx, addr, tc.sizes)

			batch, err := c.SendBatch(
				ctx, reqs, tth2.WithResponseHeaderCapture())
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			results := batch.Results
			assertResultsMatchHashes(t, results, wantHash, tc.name)
		})
	}
}
