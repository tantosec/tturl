package timing

import (
	"math/rand/v2"
	"sync"
)

// campaignQueue assigns complete work units. A rotation unit contains an entire
// cycle; the worker completes it before asking for another unit. A zero limit
// allows units until cancellation by the campaign owner.
type campaignQueue struct {
	mu    sync.Mutex
	next  uint64
	limit uint64
}

func (q *campaignQueue) take() (uint64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.limit > 0 && q.next >= q.limit {
		return 0, false
	}
	unit := q.next
	q.next++
	return unit, true
}

// campaignOrder keeps preparation positions independent of socket assignment.
// Every result owns its index slice, including the authored arrangement.
func campaignOrder(width, rotation int, random bool) []int {
	if random {
		//nolint:gosec // Randomised positioning does not require secret draws.
		return rand.Perm(width)
	}
	order := make([]int, width)
	for position := range width {
		order[position] = (position + rotation) % width
	}
	return order
}

// campaignAssignment draws a fresh compatible pairing for each pool. Pool
// numbers identify compatibility, while slot indices belong to one worker.
func campaignAssignment(pools []int, slots map[int][]int) []int {
	assignment := make([]int, len(pools))
	permutations := make(map[int][]int, len(slots))
	next := make(map[int]int, len(slots))
	for pool, compatible := range slots {
		//nolint:gosec // Socket pairing does not require secret draws.
		permutations[pool] = rand.Perm(len(compatible))
	}
	for index, pool := range pools {
		position := next[pool]
		assignment[index] = slots[pool][permutations[pool][position]]
		next[pool]++
	}
	return assignment
}
