package tth2_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/tantosec/tturl/internal/h2test"
)

// TestSendWindowTrackingAcrossManyTrials checks flow-control accounting across
// repeated sends on one connection. Each batch exceeds the initial connection
// window and therefore requires several credit-drain cycles.
func TestSendWindowTrackingAcrossManyTrials(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	})
	addr, tlsCfg := h2test.Serve(t, handler)

	client := newClient(t, tlsCfg)

	body := make([]byte, 128*1024)
	for i := range body {
		body[i] = byte(i * 7)
	}
	reqs := make([]*http.Request, 2)
	for i := range reqs {
		req, err := http.NewRequestWithContext(t.Context(), "POST", "https://"+addr+"/", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		reqs[i] = req
	}

	const trials = 60
	summary, err := client.RunTrials(t.Context(), reqs, trials)
	if err != nil {
		t.Fatalf("RunTrials: %v", err)
	}
	if got := summary.Trials; got != trials {
		t.Errorf("Trials: got %d, want %d", got, trials)
	}
	if summary.FailedTrials != 0 {
		t.Errorf("FailedTrials: got %d, want 0", summary.FailedTrials)
	}
}
