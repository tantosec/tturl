package timing

import (
	"slices"
	"sync"
	"testing"
)

func TestCampaignCompleteWorkUnits(t *testing.T) {
	queue := campaignQueue{limit: 7}
	units := make(chan uint64, 7)
	var workers sync.WaitGroup
	for range 3 {
		workers.Go(func() {
			for {
				unit, ok := queue.take()
				if !ok {
					return
				}
				units <- unit
			}
		})
	}
	workers.Wait()
	close(units)
	seen := make([]uint64, 0, 7)
	for unit := range units {
		seen = append(seen, unit)
	}
	slices.Sort(seen)
	if !slices.Equal(seen, []uint64{0, 1, 2, 3, 4, 5, 6}) {
		t.Fatalf("allocated units = %v", seen)
	}
}

func TestCampaignRotationAndIndependentCompatibleAssignment(t *testing.T) {
	const width = 4
	for identity := range width {
		positions := make([]int, width)
		for rotation := range width {
			order := campaignOrder(width, rotation, false)
			for position, id := range order {
				if id == identity {
					positions[position]++
				}
			}
		}
		if !slices.Equal(positions, []int{1, 1, 1, 1}) {
			t.Fatalf("identity %d positions = %v", identity, positions)
		}
	}
	pools := []int{1, 2, 1, 2}
	slots := map[int][]int{1: {10, 11}, 2: {20, 21}}
	for range 32 {
		assignment := campaignAssignment(pools, slots)
		seen := make(map[int]bool)
		for index, slot := range assignment {
			if seen[slot] || !slices.Contains(slots[pools[index]], slot) {
				t.Fatalf("incompatible or duplicate assignment: %v", assignment)
			}
			seen[slot] = true
		}
	}
}
