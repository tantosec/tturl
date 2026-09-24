package tth2

import (
	"slices"
	"strconv"
	"testing"
)

// arrivedAt builds a result standing for a response that came back at seq in
// the read order. The response itself is what marks it as arrived, so it is the
// one field beyond the sequence that has to be set.
func arrivedAt(seq int) Result {
	return Result{Response: &Response{}, arrivalSeq: seq}
}

// TestNewBatchResultRanksDensely verifies that sparse read sequences become
// dense ranks while requests that never arrived remain at -1.
func TestNewBatchResultRanksDensely(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// seqs gives each request's place in the read order, or -1 for a request
		// whose stream the server reset before answering.
		seqs      []int
		wantRanks []int
		wantOrder []int
	}{
		{
			name:      "contiguous, in position order",
			seqs:      []int{0, 1, 2},
			wantRanks: []int{0, 1, 2},
			wantOrder: []int{0, 1, 2},
		},
		{
			name:      "contiguous, arrived reversed",
			seqs:      []int{2, 1, 0},
			wantRanks: []int{2, 1, 0},
			wantOrder: []int{2, 1, 0},
		},
		{
			name:      "a gap from an informational response",
			seqs:      []int{0, 3, 4}, // stream 1 sent a 1xx, consuming 1 and 2
			wantRanks: []int{0, 1, 2},
			wantOrder: []int{0, 1, 2},
		},
		{
			name:      "gap and out of order",
			seqs:      []int{7, 0, 4},
			wantRanks: []int{2, 0, 1},
			wantOrder: []int{1, 2, 0},
		},
		{
			name:      "single request",
			seqs:      []int{0},
			wantRanks: []int{0},
			wantOrder: []int{0},
		},
		{
			// The survivors rank among themselves: two arrivals take ranks 0 and 1,
			// and the one that never arrived takes neither.
			name:      "one request never arrived",
			seqs:      []int{2, -1, 0},
			wantRanks: []int{1, -1, 0},
			wantOrder: []int{2, 0},
		},
		{
			name:      "nothing arrived",
			seqs:      []int{-1, -1},
			wantRanks: []int{-1, -1},
			wantOrder: []int{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			results := make([]Result, len(tc.seqs))
			for i, seq := range tc.seqs {
				if seq < 0 {
					results[i] = Result{Reset: &StreamError{}}
					continue
				}
				results[i] = arrivedAt(seq)
			}
			batch := newBatchResult(results)

			got := make([]int, len(results))
			for i, r := range batch.Results {
				got[i] = r.ArrivalRank
			}
			if !slices.Equal(got, tc.wantRanks) {
				t.Errorf("ranks = %v, want %v", got, tc.wantRanks)
			}
			if !slices.Equal(batch.ArrivalOrder, tc.wantOrder) {
				t.Errorf("ArrivalOrder = %v, want %v", batch.ArrivalOrder, tc.wantOrder)
			}
			// The two views must invert each other, which is the property that lets a
			// consumer read either and trust both.
			for rank, i := range batch.ArrivalOrder {
				if batch.Results[i].ArrivalRank != rank {
					t.Errorf("ArrivalOrder[%d] = %d, whose ArrivalRank is %d",
						rank, i, batch.Results[i].ArrivalRank)
				}
			}
		})
	}
}

func TestNewBatchResultRejectsMalformedInternalState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		results []Result
	}{
		{"no response or reset", []Result{{}}},
		{"negative arrival sequence", []Result{{
			Response: &Response{}, arrivalSeq: -1,
		}}},
		{"duplicate arrival sequence", []Result{
			arrivedAt(2), arrivedAt(2),
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("newBatchResult did not reject malformed state")
				}
			}()
			newBatchResult(test.results)
		})
	}
}

func BenchmarkNewBatchResult(b *testing.B) {
	for _, size := range []int{2, 32, 1024} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			response := &Response{}
			for b.Loop() {
				results := make([]Result, size)
				for i := range results {
					results[i] = Result{
						Response: response, arrivalSeq: size - i - 1,
					}
				}
				_ = newBatchResult(results)
			}
		})
	}
}
