package tth2_test

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"testing"
)

// mustReq builds a GET request or fails the test.
func mustReq(tb testing.TB, rawURL string) *http.Request {
	tb.Helper()
	r, err := http.NewRequestWithContext(tb.Context(), "GET", rawURL, nil)
	if err != nil {
		tb.Fatalf("http.NewRequest(%q): %v", rawURL, err)
	}
	return r
}

var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
})

// bodyHashHeader carries the server-side SHA-256 hex of the request body. Used
// by fidelity tests instead of returning the hash as the response body. Header
// reads are an O(1) map lookup with no body parse, the HEADERS frame arrives
// before any DATA, and a hash-only response emits no DATA frame at all.
const bodyHashHeader = "X-Body-Sha256"

// sha256Handler computes a streaming SHA-256 over the request body and emits
// the hex digest as the bodyHashHeader response header. Streaming (io.Copy into
// the hash) keeps the handler from buffering the body — useful for fidelity
// tests at sizes large enough to stress HTTP/2 flow control.
var sha256Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	h := sha256.New()
	if _, err := io.Copy(h, r.Body); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set(bodyHashHeader, hex.EncodeToString(h.Sum(nil)))
})
