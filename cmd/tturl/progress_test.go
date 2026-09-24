package main

import (
	"math"
	"testing"
	"time"

	"github.com/tantosec/tturl/tth2"
)

type progressLineRecorder struct {
	updates []string
	done    int
}

func (r *progressLineRecorder) Update(s string) {
	r.updates = append(r.updates, s)
}

func (r *progressLineRecorder) Done() { r.done++ }

func TestProgressPresenterIsLazyAndFinal(t *testing.T) {
	base := time.Unix(100, 0)
	now := base
	line := &progressLineRecorder{}
	p := newProgressPresenterAt(line, func() time.Time {
		return now
	}, time.Second)

	p.Observe("fast final")
	p.Done()
	if len(line.updates) != 0 || line.done != 0 {
		t.Fatalf("fast run wrote updates %q and Done %d times",
			line.updates, line.done)
	}

	line = &progressLineRecorder{}
	now = base
	p = newProgressPresenterAt(line, func() time.Time {
		return now
	}, time.Second)
	p.Observe("zero")
	now = now.Add(time.Second)
	p.Observe("one")
	now = now.Add(100 * time.Millisecond)
	p.Observe("final")
	p.Done()

	want := []string{"one", "final"}
	if len(line.updates) != len(want) {
		t.Fatalf("updates = %q, want %q", line.updates, want)
	}
	for i := range want {
		if line.updates[i] != want[i] {
			t.Errorf("updates[%d] = %q, want %q",
				i, line.updates[i], want[i])
		}
	}
	if line.done != 1 {
		t.Errorf("Done called %d times, want 1", line.done)
	}
}

func TestProgressPresenterCoalescesAndDeduplicates(t *testing.T) {
	base := time.Unix(100, 0)
	now := base
	line := &progressLineRecorder{}
	p := newProgressPresenterAt(line, func() time.Time {
		return now
	}, time.Second)

	now = now.Add(time.Second)
	p.Observe("one")
	now = now.Add(time.Second)
	p.Observe("one")
	p.Done()

	if len(line.updates) != 1 || line.updates[0] != "one" {
		t.Errorf("updates = %q, want [one]", line.updates)
	}
	if line.done != 1 {
		t.Errorf("Done called %d times, want 1", line.done)
	}
}

func TestProgressPresenterAbortDoesNotAdvance(t *testing.T) {
	base := time.Unix(100, 0)
	now := base
	line := &progressLineRecorder{}
	p := newProgressPresenterAt(line, func() time.Time {
		return now
	}, time.Second)

	now = now.Add(time.Second)
	p.Observe("shown")
	now = now.Add(time.Second)
	p.latest = "drained"
	p.Abort()

	if len(line.updates) != 1 || line.updates[0] != "shown" {
		t.Errorf("updates = %q, want [shown]", line.updates)
	}
	if line.done != 1 {
		t.Errorf("Done called %d times, want 1", line.done)
	}
}

func TestTrialProgressLine(t *testing.T) {
	p := tth2.TrialProgress{
		Trials: 18, Warmup: 2, Requests: 40,
		RecentTrialsPerSecond: 6, RecentRequestsPerSecond: 12,
	}
	if got, want := trialProgressLine(p, trialETA{limit: 30}),
		"  ... 18/30 trials complete | 2 warmup | 40 request operations | 6.0 trials/s, 12.0 req/s | eta ~2s"; got != want {
		t.Errorf("bounded progress = %q, want %q", got, want)
	}

	p.Warmup = 0
	p.RecentTrialsPerSecond = math.NaN()
	p.RecentRequestsPerSecond = math.NaN()
	if got, want := trialProgressLine(p, trialETA{limit: -1}),
		"  ... 18 trials complete | 40 request operations | -- trials/s, -- req/s"; got != want {
		t.Errorf("unbounded progress = %q, want %q", got, want)
	}

	p = tth2.TrialProgress{
		Trials: 1, Requests: 1,
		RecentTrialsPerSecond: 439, RecentRequestsPerSecond: 439,
	}
	eta := trialETA{
		limit: 10, requestsPerTrial: 1,
		maxRequestsPerSecond: 2.0 / 3600,
	}
	if got, want := trialProgressLine(p, eta),
		"  ... 1/10 trials complete | 1 request operation | 2.0 trials/h, 2.0 req/h | eta ~4h30m0s"; got != want {
		t.Errorf("paced progress = %q, want %q", got, want)
	}
}

func TestRaceProgressWanted(t *testing.T) {
	tests := []struct {
		name           string
		trials, warmup int
		shared         bool
		want           bool
	}{
		{"single", 1, 0, false, false},
		{"single with warmup", 1, 1, false, true},
		{"bounded elsewhere", 3, 0, false, true},
		{"unbounded elsewhere", -1, 0, false, true},
		{"bounded shared terminal", 3, 0, true, false},
		{"unbounded shared terminal", -1, 0, true, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := raceProgressWanted(
				test.trials, test.warmup, test.shared,
			); got != test.want {
				t.Errorf("raceProgressWanted(%d, %d, %t) = %t, want %t",
					test.trials, test.warmup, test.shared, got, test.want)
			}
		})
	}
}
