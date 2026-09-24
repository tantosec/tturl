package tth2_test

import (
	"bufio"
	"net"
	"net/http"
	"testing"

	"golang.org/x/net/http2"

	"github.com/tantosec/tturl/tth2"
)

// refuseOnePathServer answers every request except those for refusePath, whose
// stream it resets before answering anything. It serves batches indefinitely,
// however many streams each carries.
func refuseOnePathServer(refusePath string, batchSize int) func(testing.TB, *http2.Framer, *bufio.Writer, net.Conn) {
	return func(tb testing.TB, framer *http2.Framer, bw *bufio.Writer, _ net.Conn) {
		tb.Helper()
		if err := doServerPreface(tb, framer, bw); err != nil {
			tb.Logf("refuseOnePathServer: preface: %v", err)
			return
		}
		for {
			answer := make([]uint32, 0, batchSize)
			for seen := 0; seen < batchSize; {
				f, err := framer.ReadFrame()
				if err != nil {
					return // the client hung up
				}
				hf, ok := f.(*http2.MetaHeadersFrame)
				if !ok {
					continue
				}
				seen++
				if hf.PseudoValue("path") == refusePath {
					if err := framer.WriteRSTStream(hf.StreamID, http2.ErrCodeRefusedStream); err != nil {
						tb.Logf("refuseOnePathServer: WriteRSTStream: %v", err)
						return
					}
					if err := bw.Flush(); err != nil {
						tb.Logf("refuseOnePathServer: flush RST: %v", err)
						return
					}
					continue
				}
				answer = append(answer, hf.StreamID)
			}
			for _, sid := range answer {
				if err := writeOKResponse(framer, bw, sid); err != nil {
					tb.Logf("refuseOnePathServer: WriteHeaders: %v", err)
					return
				}
			}
		}
	}
}

// TestRunTrialsResetCountsAttribution refuses one request in every trial. No
// rank row is tallied, every trial fails, and reset counts follow request
// identity as rotation moves the request between positions.
func TestRunTrialsResetCountsAttribution(t *testing.T) {
	t.Parallel()
	const (
		refusePath = "/refused"
		batchSize  = 2
		trials     = 4 // two whole rotation cycles
		refused    = 1 // the index of the refused request in reqs
	)
	addr, tlsCfg := startRawH2Server(t, refuseOnePathServer(refusePath, batchSize))

	c := newClientMaxConns(t, tlsCfg, 1)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/served"),
		mustReq(t, "https://"+addr+refusePath),
	}
	summary, err := c.RunTrials(t.Context(), reqs, trials)
	if err != nil {
		t.Fatalf("RunTrials: %v; a refused request is an observation, not a failed run", err)
	}

	if summary.Trials != 0 {
		t.Errorf("Trials = %d, want 0: every trial lost a stream", summary.Trials)
	}
	if summary.FailedTrials != trials {
		t.Errorf("FailedTrials = %d, want %d", summary.FailedTrials, trials)
	}
	for i, got := range summary.ResetCounts {
		want := 0
		if i == refused {
			want = trials
		}
		if got != want {
			t.Errorf("ResetCounts[%d] = %d, want %d", i, got, want)
		}
	}
	for i, row := range summary.RankCounts {
		for rank, got := range row {
			if got != 0 {
				t.Errorf("RankCounts[%d][%d] = %d, want 0: a partial trial contributes no ranks",
					i, rank, got)
			}
		}
	}
	for position, row := range summary.PositionRankCounts {
		for rank, got := range row {
			if got != 0 {
				t.Errorf("PositionRankCounts[%d][%d] = %d, want 0", position, rank, got)
			}
		}
	}
}

// TestStreamTrialsResetTrialCarriesItsPartialBatch checks what a consumer sees
// per trial, which the summary's counts cannot show: the trial reports the
// error, carries the batch that raced beside it, and names the refused request
// -- keyed by request, whatever position the arrangement put it in.
func TestStreamTrialsResetTrialCarriesItsPartialBatch(t *testing.T) {
	t.Parallel()
	const (
		refusePath = "/refused"
		batchSize  = 2
		trials     = 2
		refused    = 1
	)
	addr, tlsCfg := startRawH2Server(t, refuseOnePathServer(refusePath, batchSize))

	c := newClientMaxConns(t, tlsCfg, 1)
	reqs := []*http.Request{
		mustReq(t, "https://"+addr+"/served"),
		mustReq(t, "https://"+addr+refusePath),
	}
	s := c.StreamTrials(t.Context(), reqs, tth2.WithMaxTrials(trials))

	seen := 0
	for tr := range s.All() {
		seen++
		if tr.Err == nil {
			t.Errorf("trial %d: Err is nil, want the server's reset", tr.Index)
			continue
		}
		if len(tr.Results) != batchSize {
			t.Fatalf("trial %d: len(Results) = %d, want %d", tr.Index, len(tr.Results), batchSize)
		}
		if r := tr.Results[refused]; r.Reset == nil {
			t.Errorf("trial %d: Results[%d].Reset is nil, want the reset for the refused request",
				tr.Index, refused)
		} else if r.ArrivalRank != -1 {
			t.Errorf("trial %d: Results[%d].ArrivalRank = %d, want -1", tr.Index, refused, r.ArrivalRank)
		}
		// The served request was the only arrival, so it took the only rank.
		if r := tr.Results[1-refused]; r.Reset != nil {
			t.Errorf("trial %d: Results[%d].Reset = %v, want nil", tr.Index, 1-refused, r.Reset)
		} else if r.ArrivalRank != 0 {
			t.Errorf("trial %d: Results[%d].ArrivalRank = %d, want 0", tr.Index, 1-refused, r.ArrivalRank)
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if seen != trials {
		t.Errorf("yielded %d trials, want %d", seen, trials)
	}
}
