package timing

import (
	"testing"
	"time"
)

func TestCommonAnchorPreservesSignedOffsetsAndOwnReleaseDuration(t *testing.T) {
	anchor := time.Now()
	first := requestMilestones{
		initial: anchor.Add(10 * time.Nanosecond),
		final:   anchor.Add(20 * time.Nanosecond),
		headers: anchor.Add(15 * time.Nanosecond),
	}
	second := requestMilestones{
		initial: anchor.Add(12 * time.Nanosecond),
		headers: anchor.Add(25 * time.Nanosecond),
	}
	a, b := first.evidence(anchor, "not observed"), second.evidence(anchor, "not observed")
	if a.initial.ns != 10 || b.initial.ns != 12 || a.duration.ns != 5 || b.duration.ns != 13 {
		t.Fatalf("shared offsets or own release duration changed: first=%+v second=%+v", a, b)
	}
	if got := a.headers.ns - a.final.ns; got != -5 {
		t.Fatalf("early-response evidence lost signed ordering: %d", got)
	}
	gate := offsetEvent(anchor, anchor.Add(-3*time.Nanosecond), "not opened")
	if !gate.present || gate.ns != -3 {
		t.Fatalf("gate preceding anchor: %+v", gate)
	}
}

func TestMissingEventsAndFailedBodyPreserveHeaderDuration(t *testing.T) {
	anchor := time.Now()
	unreleased := (requestMilestones{}).evidence(anchor, "unattempted")
	if unreleased.initial.present || unreleased.duration.present || unreleased.initial.reason != "unattempted" {
		t.Fatalf("unattempted request invented evidence: %+v", unreleased)
	}
	partial := requestMilestones{initial: anchor, headers: anchor.Add(7 * time.Nanosecond)}
	evidence := partial.evidence(anchor, "body failed")
	if !evidence.initial.present || evidence.initial.ns != 0 ||
		!evidence.duration.present || evidence.duration.ns != 7 || evidence.complete.present {
		t.Fatalf("body failure lost valid headers or invented completion: %+v", evidence)
	}
	noHeaders := (requestMilestones{initial: anchor}).evidence(anchor, "malformed headers")
	if noHeaders.duration.present || noHeaders.headers.present || noHeaders.duration.reason != "malformed headers" {
		t.Fatalf("malformed headers supplied duration: %+v", noHeaders)
	}
	noAnchor := offsetEvent(time.Time{}, anchor, "not observed")
	if noAnchor.present || noAnchor.reason == "" {
		t.Fatalf("missing anchor fabricated offset: %+v", noAnchor)
	}
	if got := partial.evidence(time.Time{}, ""); got.duration.present || got.duration.reason == "" {
		t.Fatalf("missing trial anchor fabricated duration: %+v", got)
	}
}
