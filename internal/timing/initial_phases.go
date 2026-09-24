package timing

import (
	"context"
	"slices"
	"sync"
)

// initialPhases coordinates only the admitted workers' initial lifecycle.
// Transport readers remain live: waiting owns no lease or scheduler lock.
type initialPhases struct {
	mu              sync.Mutex
	workers         int
	acquired        map[int][]transportLease
	warmed          map[int][]transportLease
	changed         chan struct{}
	acquisitionOpen bool
	warmupOpen      bool
	opening         bool
}

func newInitialPhases(workers int) *initialPhases {
	return &initialPhases{
		workers: workers, acquired: make(map[int][]transportLease),
		warmed: make(map[int][]transportLease), changed: make(chan struct{}),
	}
}

func (p *initialPhases) arrive(ctx context.Context, w *campaignWorker, warm bool) (bool, error) {
	p.mu.Lock()
	arrivals := p.acquired
	if warm {
		arrivals = p.warmed
	}
	snapshot := make([]transportLease, len(w.slots))
	for index, slot := range w.slots {
		snapshot[index] = slot.lease
	}
	arrivals[w.id] = snapshot
	for {
		if err := context.Cause(ctx); err != nil {
			p.mu.Unlock()
			return false, err
		}
		open := p.acquisitionOpen
		if warm {
			open = p.warmupOpen
		}
		if open && !p.opening {
			p.mu.Unlock()
			return true, nil
		}
		if _, present := arrivals[w.id]; !present {
			p.mu.Unlock()
			return false, nil
		}
		if len(arrivals) == p.workers && !p.opening {
			// Lock in worker/slot order to open the phase against
			// one consistent health snapshot.
			leases := make([]transportLease, 0)
			for id := range p.workers {
				for _, lease := range arrivals[id] {
					lease.lockState()
					leases = append(leases, lease)
				}
			}
			invalid := false
			for id, workerLeases := range arrivals {
				for _, lease := range workerLeases {
					if usable, _ := lease.usableLocked(); !usable {
						delete(arrivals, id)
						invalid = true
						break
					}
				}
			}
			if !invalid {
				p.opening = true
				if warm {
					p.warmupOpen = true
				} else {
					p.acquisitionOpen = true
				}
			}
			for _, lease := range slices.Backward(leases) {
				lease.unlockState()
			}
			if !invalid {
				phase := "initial_acquisition"
				if warm {
					phase = "initial_warmup"
				}
				// Publish before releasing workers. Consumers
				// see that boundary; transport locks are
				// already released.
				p.mu.Unlock()
				w.state.publish(Event{Phase: &PhaseCompletion{Phase: phase, WorkerID: -1}})
				p.mu.Lock()
				p.opening = false
			}
			close(p.changed)
			p.changed = make(chan struct{})
			continue
		}
		changed := p.changed
		p.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return false, context.Cause(ctx)
		}
		p.mu.Lock()
	}
}
