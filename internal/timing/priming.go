package timing

import "context"

// prime executes exactly N representative whole trials in one worker phase.
// Complete exchanges may retire connections; incomplete exchanges are terminal.
func (w *campaignWorker) prime(ctx context.Context) error {
	r := w.state.resolved
	requests, requestInfo := r.Plan.Priming, r.Priming
	if requests == nil {
		requests = r.Plan.Requests
	}
	phase := "initial"
	if w.warmupPhase > 0 {
		phase = "replacement"
	}
	for index := range r.Plan.Warmup {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		rotation := 0
		if r.Plan.Arrangement == ArrangeRotate {
			rotation = index % len(requests)
		}
		order := campaignOrder(len(requests), rotation, r.Plan.Arrangement == ArrangeRandom)
		trial, fatal := w.trial(ctx, uint64(index), order, requests, requestInfo, true)
		record := Warmup{
			WorkerID: w.id, Phase: phase, PhaseIndex: w.warmupPhase,
			TrialIndex: index, StartedAt: trial.StartedAt, InitialGate: trial.InitialGate,
			FinalGate: trial.FinalGate, Order: trial.Order, Outcomes: trial.Outcomes,
			Committed: trial.Committed, OfferedOperations: trial.OfferedOperations,
			Complete: warmupMembershipComplete(trial.Outcomes), Failure: trial.Failure,
		}
		if record.Complete {
			record.Failure = nil
		}
		// Complete exchanges can retire before later participants.
		// Preserve the interrupted trial and restart the worker
		// phase through bounded recovery.
		retirement := false
		if !record.Complete && fatal == nil && !w.usable() {
			retirement = true
			for _, outcome := range record.Outcomes {
				if outcome.Failure != nil || outcome.Response.Reset || (outcome.Attempted && !outcome.Response.Complete) {
					retirement = false
				}
			}
		}
		if !record.Complete && !retirement {
			if record.Failure == nil {
				for _, outcome := range record.Outcomes {
					if outcome.Failure != nil {
						record.Failure = outcome.Failure
						break
					}
				}
			}
			if record.Failure == nil {
				record.Failure = &Failure{Phase: PhaseExecution, Code: "warmup", Message: "warmup exchange incomplete"}
			}
		}
		if !record.Complete && !retirement {
			w.state.cancel(record.Failure)
		}
		w.state.publish(Event{Warmup: &record})
		if !record.Complete && !retirement {
			return record.Failure
		}
		if !w.usable() {
			return nil
		}
	}
	return nil
}

func warmupMembershipComplete(outcomes []Outcome) bool {
	if !campaignMembershipComplete(outcomes) {
		return false
	}
	for _, outcome := range outcomes {
		if outcome.Response.Reset {
			return false
		}
	}
	return true
}
