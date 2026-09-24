package tth2

import (
	"net/http"
	"testing"

	"github.com/tantosec/tturl/internal/h2test"
)

// TestArrivalOrderPopulated verifies dense ranks after a real send.
func TestArrivalOrderPopulated(t *testing.T) {
	t.Parallel()
	addr, tlsCfg := h2test.Serve(t, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	tr := &Transport{TLSClientConfig: tlsCfg}
	t.Cleanup(tr.CloseIdleConnections)
	c := &Client{Transport: tr}

	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/a"),
		mustReq(t, "https://"+addr+"/b"),
		mustReq(t, "https://"+addr+"/c"),
	}
	batch, err := c.SendBatch(t.Context(), reqs)
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}

	seen := make([]bool, len(reqs))
	for i, r := range batch.Results {
		if r.ArrivalRank < 0 || r.ArrivalRank >= len(reqs) || seen[r.ArrivalRank] {
			t.Fatalf("ranks are not a permutation: result %d has rank %d, seen %v",
				i, r.ArrivalRank, seen)
		}
		seen[r.ArrivalRank] = true
	}

	if len(batch.ArrivalOrder) != len(reqs) {
		t.Fatalf("len(ArrivalOrder) = %d, want %d", len(batch.ArrivalOrder), len(reqs))
	}
	for rank, i := range batch.ArrivalOrder {
		if batch.Results[i].ArrivalRank != rank {
			t.Errorf("ArrivalOrder[%d] = %d, whose ArrivalRank is %d", rank, i, batch.Results[i].ArrivalRank)
		}
	}
	if !batch.AllResponsesArrived() {
		t.Error("AllResponsesArrived = false, want true")
	}
}
