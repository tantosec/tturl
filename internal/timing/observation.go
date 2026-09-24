package timing

import "time"

// eventOffset retains absence separately from a valid zero or negative offset.
type eventOffset struct {
	ns      int64
	present bool
	reason  string
}

// requestMilestones holds monotonic observations until engine projection.
type requestMilestones struct {
	initial, final, written, firstHeaders, headers, complete time.Time
}

type timingEvidence struct {
	initial, final, written, firstHeaders, headers, complete eventOffset
	duration                                                 eventOffset
}

func offsetEvent(anchor, event time.Time, reason string) eventOffset {
	if anchor.IsZero() {
		return eventOffset{reason: "trial anchor unavailable"}
	}
	if event.IsZero() {
		return eventOffset{reason: reason}
	}
	return eventOffset{ns: event.Sub(anchor).Nanoseconds(), present: true}
}

// evidence computes elapsed values from Go readings before reporting. Calendar
// timestamps are provenance and never participate in the elapsed calculation.
func (m requestMilestones) evidence(anchor time.Time, missing string) timingEvidence {
	if missing == "" {
		missing = "not observed"
	}
	if anchor.IsZero() {
		missing = "trial anchor unavailable"
	}
	timing := timingEvidence{
		initial:      offsetEvent(anchor, m.initial, missing),
		final:        offsetEvent(anchor, m.final, missing),
		written:      offsetEvent(anchor, m.written, missing),
		firstHeaders: offsetEvent(anchor, m.firstHeaders, missing),
		headers:      offsetEvent(anchor, m.headers, missing),
		complete:     offsetEvent(anchor, m.complete, missing),
		duration:     eventOffset{reason: missing},
	}
	if !anchor.IsZero() && !m.initial.IsZero() && !m.headers.IsZero() {
		timing.duration = eventOffset{ns: m.headers.Sub(m.initial).Nanoseconds(), present: true}
	}
	return timing
}
