package tth2

import "testing"

func FuzzNewBatchResultState(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{2, 3, 7})
	f.Add([]byte{3, 3})
	f.Fuzz(func(t *testing.T, states []byte) {
		if len(states) > 64 {
			t.Skip()
		}
		results := make([]Result, len(states))
		valid := true
		sequences := make(map[int]bool)
		for i, state := range states {
			switch state % 4 {
			case 0:
				results[i] = Result{}
				valid = false
			case 1:
				results[i] = Result{
					Response: &Response{}, arrivalSeq: -1,
				}
				valid = false
			case 2:
				results[i] = Result{Reset: &StreamError{}}
			case 3:
				sequence := int(state / 4)
				results[i] = arrivedAt(sequence)
				if sequences[sequence] {
					valid = false
				}
				sequences[sequence] = true
			}
		}

		panicked := false
		var batch *BatchResult
		func() {
			defer func() { panicked = recover() != nil }()
			batch = newBatchResult(results)
		}()
		if panicked == valid {
			t.Fatalf("newBatchResult panic = %v, valid = %v", panicked, valid)
		}
		if !valid {
			return
		}
		for rank, input := range batch.ArrivalOrder {
			if batch.Results[input].ArrivalRank != rank {
				t.Fatalf("rank %d maps to result rank %d",
					rank, batch.Results[input].ArrivalRank)
			}
		}
	})
}
