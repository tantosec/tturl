package tth2_test

import (
	"slices"
	"testing"

	"github.com/tantosec/tturl/tth2"
)

func TestPartialResultQueries(t *testing.T) {
	t.Parallel()
	ordinary := tth2.Result{
		Response: &tth2.Response{}, ArrivalRank: 1,
	}
	beforeHeaders := tth2.Result{
		ArrivalRank: -1, Reset: &tth2.StreamError{},
	}
	afterHeaders := tth2.Result{
		Response: &tth2.Response{}, ArrivalRank: 0,
		Reset: &tth2.StreamError{},
	}
	if !ordinary.Arrived() || beforeHeaders.Arrived() || !afterHeaders.Arrived() {
		t.Fatalf("Arrived disagrees with documented response states")
	}

	complete := tth2.BatchResult{
		Results:      []tth2.Result{ordinary, afterHeaders},
		ArrivalOrder: []int{1, 0},
	}
	if !complete.AllResponsesArrived() {
		t.Fatal("AllResponsesArrived = false, want true")
	}
	order, ok := complete.FullArrivalOrder()
	if !ok || !slices.Equal(order, []int{1, 0}) {
		t.Fatalf("FullArrivalOrder = %v, %v", order, ok)
	}

	incomplete := tth2.BatchResult{
		Results:      []tth2.Result{ordinary, beforeHeaders},
		ArrivalOrder: []int{0},
	}
	if incomplete.AllResponsesArrived() {
		t.Fatal("AllResponsesArrived = true for a pre-header reset")
	}
	if order, ok := incomplete.FullArrivalOrder(); ok || order != nil {
		t.Fatalf("incomplete FullArrivalOrder = %v, %v", order, ok)
	}

	empty := tth2.BatchResult{}
	order, ok = empty.FullArrivalOrder()
	if !ok || len(order) != 0 {
		t.Fatalf("empty FullArrivalOrder = %v, %v; want empty, true", order, ok)
	}
}

func TestFullArrivalOrderRejectsMalformedPublicState(t *testing.T) {
	t.Parallel()
	results := []tth2.Result{
		{Response: &tth2.Response{}, ArrivalRank: 0},
		{Response: &tth2.Response{}, ArrivalRank: 1},
	}
	for _, order := range [][]int{nil, {0}, {0, 0}, {0, 2}, {1, 0}} {
		batch := tth2.BatchResult{Results: results, ArrivalOrder: order}
		if got, ok := batch.FullArrivalOrder(); ok || got != nil {
			t.Errorf("FullArrivalOrder(%v) = %v, %v; want nil, false",
				order, got, ok)
		}
	}
}

func FuzzBatchResultQueries(f *testing.F) {
	f.Add([]byte{0, 1}, []byte{0, 1})
	f.Add([]byte{1, 1}, []byte{1, 0})
	f.Fuzz(func(t *testing.T, states, orderBytes []byte) {
		if len(states) > 64 || len(orderBytes) > 64 {
			t.Skip()
		}
		results := make([]tth2.Result, len(states))
		for i, state := range states {
			results[i].ArrivalRank = int(state) - 128
			if state&1 != 0 {
				results[i].Response = &tth2.Response{}
			} else {
				results[i].Reset = &tth2.StreamError{}
			}
		}
		order := make([]int, len(orderBytes))
		for i, value := range orderBytes {
			order[i] = int(value) - 128
		}
		batch := tth2.BatchResult{Results: results, ArrivalOrder: order}
		_ = batch.AllResponsesArrived()
		_, _ = batch.FullArrivalOrder()
	})
}
