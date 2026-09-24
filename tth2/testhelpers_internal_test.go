package tth2

import (
	"net/http"
	"testing"
)

// mustReq builds a GET request or fails the test. Used by internal-package
// tests; the external [tth2_test] package has its own copy in server_test.go.
func mustReq(tb testing.TB, rawURL string) *http.Request {
	tb.Helper()
	r, err := http.NewRequestWithContext(tb.Context(), "GET", rawURL, nil)
	if err != nil {
		tb.Fatalf("http.NewRequest(%q): %v", rawURL, err)
	}
	return r
}
