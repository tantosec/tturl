package ranking

import "testing"

func FuzzRankPermutation(f *testing.F) {
	f.Add(byte(4), []byte{0, 1, 2, 3})
	f.Add(byte(4), []byte{0, 1, 1, 3})
	f.Add(byte(3), []byte{0, 1})
	f.Add(byte(2), []byte{0, 255})

	f.Fuzz(func(t *testing.T, widthByte byte, encoded []byte) {
		if len(encoded) > 256 {
			t.Skip()
		}
		width := int(widthByte)
		ranks := make([]int, len(encoded))
		for i, rank := range encoded {
			ranks[i] = int(rank)
			if rank >= 128 {
				ranks[i] -= 256
			}
		}

		want := referenceRankPermutation(ranks, width)
		if got := isRankPermutation(ranks, width); got != want {
			t.Errorf("isRankPermutation(%v, %d) = %v, want %v", ranks, width, got, want)
		}
	})
}

func referenceRankPermutation(ranks []int, width int) bool {
	if len(ranks) != width {
		return false
	}
	counts := make([]int, width)
	for _, rank := range ranks {
		if rank < 0 || rank >= width {
			return false
		}
		counts[rank]++
	}
	for _, count := range counts {
		if count != 1 {
			return false
		}
	}
	return true
}
