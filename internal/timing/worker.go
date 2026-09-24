package timing

import (
	"context"
	"slices"
	"time"
)

type campaignSlot struct {
	pool         int
	index        int
	id           uint64
	lease        transportLease
	connection   Connection
	failure      *Failure
	acquisitions int
}

type campaignWorker struct {
	state       *campaignState
	id          int
	slots       []campaignSlot
	byPool      map[int][]int
	warmupPhase int
	measured    bool
}

func (w *campaignWorker) run(ctx context.Context) (err error) {
	r := w.state.resolved
	w.byPool = make(map[int][]int, len(r.Pools))
	for _, pool := range r.Pools {
		count := 1
		if r.Plan.Synchronise {
			count = pool.Width
		}
		for range count {
			index := len(w.slots)
			w.byPool[pool.ID] = append(w.byPool[pool.ID], index)
			w.slots = append(w.slots, campaignSlot{pool: pool.ID, index: index})
		}
	}
	defer func() {
		if err != nil && context.Cause(ctx) == nil {
			w.state.cancel(err)
		}
		w.dispose()
	}()
	if err := w.acquireSet(ctx); err != nil {
		return err
	}
	for {
		ready, err := w.state.initial.arrive(ctx, w, false)
		if err != nil {
			return err
		}
		if ready {
			break
		}
		if err := w.acquireSet(ctx); err != nil {
			return err
		}
	}
	for {
		if err := w.readySet(ctx, true); err != nil {
			return err
		}
		ready, err := w.state.initial.arrive(ctx, w, true)
		if err != nil {
			return err
		}
		if ready {
			break
		}
		w.warmupPhase++
	}
	for index := range w.slots {
		w.slots[index].acquisitions = 0
	}
	w.measured = true
	//nolint:gosec // Worker IDs belong to the validated worker range.
	unit := uint64(w.id)
	for context.Cause(ctx) == nil {
		count := 1
		if r.Plan.Arrangement == ArrangeRotate {
			count = r.Width
		}
		if err := w.unit(ctx, unit, count); err != nil {
			return err
		}
		next, ok := w.state.queue.take()
		if !ok {
			return nil
		}
		unit = next
	}
	return context.Cause(ctx)
}

func (w *campaignWorker) unit(ctx context.Context, unit uint64, count int) error {
	r := w.state.resolved
	entered, complete, finished := false, true, 0
	defer func() {
		if r.Plan.Arrangement != ArrangeRotate || !entered {
			return
		}
		w.state.mu.Lock()
		defer w.state.mu.Unlock()
		w.state.counts.CyclesAttempted++
		if complete && finished == count {
			w.state.counts.CyclesComplete++
		} else {
			w.state.counts.CyclesIncomplete++
		}
	}()
	for rotation := range count {
		if context.Cause(ctx) != nil {
			return context.Cause(ctx)
		}
		// Recover before measured work. Acquisition and warmup do not
		// consume its trial budget.
		if err := w.readySet(ctx, false); err != nil {
			return err
		}
		for index := range w.slots {
			w.slots[index].acquisitions = 0
		}
		entered = true
		order := campaignOrder(r.Width, rotation, r.Plan.Arrangement == ArrangeRandom)
		trial, fatal := w.trial(ctx, unit*uint64(count)+uint64(rotation), order, r.Plan.Requests, r.Requests, false)
		trial.Complete = campaignMembershipComplete(trial.Outcomes)
		if trial.Complete {
			trial.Failure = nil
		}
		complete = complete && trial.Complete
		finished++
		w.state.publish(Event{Trial: &trial})
		if fatal != nil {
			return fatal
		}
	}
	return nil
}

func (w *campaignWorker) trial(ctx context.Context, index uint64, order []int,
	requests []Request, requestInfo []RequestInfo, warmup bool,
) (Trial, error) {
	r := w.state.resolved
	anchor := time.Now()
	trial := Trial{
		Index: index, WorkerID: w.id, StartedAt: anchor.UTC(), Complete: true,
		Order: make([]int, len(order)), Outcomes: make([]Outcome, len(order)),
		InitialGate: campaignOffset(anchor, time.Time{}, "no_common_initial_gate"),
		FinalGate:   campaignOffset(anchor, time.Time{}, "no_common_final_gate"),
	}
	width := len(requests)
	pools := make([]int, width)
	for index, info := range requestInfo {
		pools[index] = info.PoolID
	}
	assignment := make([]int, width)
	if r.Plan.Synchronise {
		assignment = campaignAssignment(pools, w.byPool)
	} else {
		for requestIndex, pool := range pools {
			assignment[requestIndex] = w.byPool[pool][0]
		}
	}
	for position, requestIndex := range order {
		request := requests[requestIndex]
		trial.Order[position] = request.ID
		trial.Outcomes[position] = w.unattempted(request, pools[requestIndex], position,
			w.slots[assignment[requestIndex]].id, anchor)
	}
	if r.Plan.Synchronise {
		participants := make([]campaignParticipant, len(order))
		for position, requestIndex := range order {
			participants[position] = campaignParticipant{
				request: requests[requestIndex],
				lease:   w.slots[assignment[requestIndex]].lease,
			}
		}
		group := campaignGroup(ctx, participants, w.state.pacer, r.Plan, anchor, width, true, r.Plan.LastByteSync)
		w.applyGroup(&trial, group, 0, anchor)
		return trial, nil
	}
	for position, requestIndex := range order {
		slotIndex := assignment[requestIndex]
		if position > 0 {
			var err error
			if warmup {
				if usable, _ := w.slots[slotIndex].lease.usable(); !usable {
					return trial, nil
				}
			} else {
				err = w.readySet(ctx, false)
			}
			if err != nil {
				trial.Complete = false
				trial.Failure = campaignFailure(PhaseAcquisition, err)
				return trial, err
			}
		}
		trial.Outcomes[position].ConnectionID = w.slots[slotIndex].id
		participant := campaignParticipant{request: requests[requestIndex], lease: w.slots[slotIndex].lease}
		group := campaignGroup(ctx, []campaignParticipant{participant}, w.state.pacer,
			r.Plan, anchor, width, position == 0, r.Plan.LastByteSync)
		w.applyGroup(&trial, group, position, anchor)
		if !warmup && trial.Outcomes[position].Attempted {
			// Renew recovery after forward measured progress. Until
			// then, replacement warmup gets one fresh identity.
			for index := range w.slots {
				w.slots[index].acquisitions = 0
			}
		}
		if group.err != nil {
			break
		}
	}
	return trial, nil
}

func (w *campaignWorker) applyGroup(trial *Trial, group campaignGroupResult, first int, anchor time.Time) {
	if group.committed {
		trial.Committed = true
		trial.OfferedOperations = uint64(len(trial.Outcomes))
	}
	if w.state.resolved.Plan.Synchronise {
		trial.InitialGate = campaignOffset(anchor, group.startGate, "gate not opened")
		trial.FinalGate = campaignOffset(anchor, group.finalGate, "gate not opened")
	}
	for position, outcome := range group.outcomes {
		previous := trial.Outcomes[first+position]
		outcome.RequestID = previous.RequestID
		outcome.PoolID, outcome.Position = previous.PoolID, previous.Position
		outcome.WorkerID, outcome.ConnectionID = w.id, previous.ConnectionID
		outcome.Protocol = previous.Protocol
		if !outcome.Attempted && !outcome.Timing.InitialRelease.Present {
			outcome.Timing = campaignMissingTiming(anchor, "unattempted")
		}
		trial.Outcomes[first+position] = outcome
		if outcome.Failure != nil {
			for index := range w.slots {
				if w.slots[index].id == outcome.ConnectionID {
					w.slots[index].failure = outcome.Failure
				}
			}
		}
		if outcome.Failure != nil || !outcome.Response.Complete {
			trial.Complete = false
		}
	}
	if group.err != nil {
		trial.Complete = false
		trial.Failure = campaignFailure(PhaseExecution, group.err)
	}
}

func (w *campaignWorker) unattempted(request Request, pool, position int,
	connection uint64, anchor time.Time,
) Outcome {
	return Outcome{
		RequestID: request.ID, Position: position, PoolID: pool,
		WorkerID: w.id, ConnectionID: connection, Protocol: request.Protocol,
		Timing: campaignMissingTiming(anchor, "unattempted"),
	}
}

func campaignOffset(anchor, event time.Time, reason string) Offset {
	value := offsetEvent(anchor, event, reason)
	return Offset{NS: value.ns, Present: value.present, Reason: value.reason}
}

func campaignMissingTiming(anchor time.Time, reason string) Timing {
	missing := campaignOffset(anchor, time.Time{}, reason)
	return Timing{
		InitialRelease: missing, FinalRelease: missing, WriteComplete: missing,
		FirstResponseHeaders: missing, FinalResponseHeaders: missing,
		ResponseComplete: missing, Duration: missing,
	}
}

// acquireSet restores every slot before any worker warmup is released.
func (w *campaignWorker) acquireSet(ctx context.Context) error {
	for index := range w.slots {
		if err := w.ensure(ctx, index); err != nil {
			return err
		}
	}
	return nil
}

func (w *campaignWorker) usable() bool {
	for index := range w.slots {
		if w.slots[index].lease == nil {
			return false
		}
	}
	for index := range w.slots {
		w.slots[index].lease.lockState()
	}
	defer func() {
		for _, v := range slices.Backward(w.slots) {
			v.lease.unlockState()
		}
	}()
	for index := range w.slots {
		if usable, _ := w.slots[index].lease.usableLocked(); !usable {
			return false
		}
	}
	return true
}

// readySet restarts whole-worker warmup after any lost physical identity.
// A slot gets at most two identities before initial measurement, and one
// replacement before renewed measured progress. Warmup never resets it.
func (w *campaignWorker) readySet(ctx context.Context, initial bool) error {
	if !initial && w.usable() {
		return nil
	}
	if !initial {
		w.warmupPhase++
	}
	for {
		if err := w.acquireSet(ctx); err != nil {
			return err
		}
		if err := w.prime(ctx); err != nil {
			return err
		}
		if w.usable() {
			w.state.publish(Event{Phase: &PhaseCompletion{
				Phase: "worker_warmup", WorkerID: w.id, PhaseIndex: w.warmupPhase,
			}})
			return nil
		}
		w.warmupPhase++
	}
}

func (w *campaignWorker) ensure(ctx context.Context, index int) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	slot := &w.slots[index]
	if slot.lease != nil {
		if usable, _ := slot.lease.usable(); usable {
			return nil
		}
	}
	previous := slot.id
	if slot.lease != nil {
		_, reason := slot.lease.usable()
		w.closeSlot(slot, "disposed", reason)
	}
	limit := 2
	if w.measured {
		limit = 1
	}
	if slot.acquisitions >= limit {
		return &Failure{Phase: PhaseReadiness, Code: "recovery", Message: "connection recovery allowance exhausted"}
	}
	slot.acquisitions++

	request := w.representative(slot.pool)
	slot.id = w.state.ids.Add(1)
	slot.connection = Connection{
		ID: slot.id, WorkerID: w.id, PoolID: slot.pool,
		Slot: slot.index, ReplacementOf: previous, Protocol: request.Protocol,
	}
	lease, err := w.state.factory(ctx, request, w.state.resolved.Plan)
	if err != nil {
		slot.connection.State = "failed"
		slot.connection.Failure = campaignFailure(PhaseAcquisition, err)
		w.state.cancel(slot.connection.Failure)
		w.state.publish(Event{Connection: new(slot.connection)})
		return slot.connection.Failure
	}
	slot.lease, slot.failure = lease, nil
	metadata := lease.metadata()
	slot.connection.NegotiatedProtocol = metadata.negotiatedProtocol
	slot.connection.LocalAddress, slot.connection.RemoteAddress = metadata.localAddress, metadata.remoteAddress
	slot.connection.State = "acquired"
	w.state.publish(Event{Connection: new(slot.connection)})
	return nil
}

func (w *campaignWorker) representative(poolID int) Request {
	for index, info := range w.state.resolved.Requests {
		if info.PoolID == poolID {
			return w.state.resolved.Plan.Requests[index]
		}
	}
	panic("timing: validated pool has no measured request")
}

func (w *campaignWorker) closeSlot(slot *campaignSlot, state, reason string) {
	if connection := w.disposeSlot(slot, state, reason); connection != nil {
		w.state.publish(Event{Connection: connection})
	}
}

func (w *campaignWorker) disposeSlot(slot *campaignSlot, state, reason string) *Connection {
	if slot.lease == nil {
		return nil
	}
	retired, retirementReason := slot.lease.retirement()
	usable, unavailableReason := slot.lease.usable()
	if retired {
		state, reason = "retired", retirementReason
	} else if !usable {
		state, reason = "disposed", unavailableReason
	}
	slot.lease.dispose(context.Canceled)
	slot.lease.join()
	slot.lease = nil
	connection := slot.connection
	connection.State, connection.Reason = state, reason
	if !retired && !usable {
		connection.Failure = slot.failure
		if connection.Failure == nil {
			connection.Failure = &Failure{Phase: PhaseExecution, Code: "connection", Message: reason}
		}
	}
	return &connection
}

func (w *campaignWorker) dispose() {
	connections := make([]Connection, 0, len(w.slots))
	for index := range w.slots {
		if connection := w.disposeSlot(&w.slots[index], "disposed", "run finished"); connection != nil {
			connections = append(connections, *connection)
		}
	}
	w.state.interactionDone()
	for _, connection := range connections {
		w.state.publish(Event{Connection: new(connection)})
	}
}

func campaignMembershipComplete(outcomes []Outcome) bool {
	for _, outcome := range outcomes {
		if !outcome.Attempted || !outcome.Response.Complete || outcome.Failure != nil {
			return false
		}
	}
	return true
}
