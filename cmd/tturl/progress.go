package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tantosec/tturl/internal/termline"
	"github.com/tantosec/tturl/tth2"
)

const progressInterval = time.Second

type progressLine interface {
	Update(string)
	Done()
}

// progressPresenter keeps fast runs silent, coalesces snapshots to one frame
// per interval, and closes a visible terminal frame before report output
// resumes.
type progressPresenter struct {
	line     progressLine
	now      func() time.Time
	started  time.Time
	lastAt   time.Time
	interval time.Duration
	latest   string
	shown    string
	visible  bool
}

func newProgressPresenter(w io.Writer) *progressPresenter {
	if w == io.Discard {
		return newProgressPresenterAt(discardProgressLine{}, time.Now, progressInterval)
	}
	return newProgressPresenterAt(
		termline.New(w), time.Now, progressInterval)
}

type discardProgressLine struct{}

func (discardProgressLine) Update(string) {}
func (discardProgressLine) Done()         {}

func newProgressPresenterAt(
	line progressLine,
	now func() time.Time,
	interval time.Duration,
) *progressPresenter {
	started := now()
	return &progressPresenter{
		line: line, now: now, started: started, lastAt: started,
		interval: interval,
	}
}

// Observe retains every snapshot but presents one only after the lazy delay
// and cadence. Done forces the newest snapshot only after a frame was shown.
func (p *progressPresenter) Observe(s string) {
	p.latest = s
	now := p.now()
	if now.Sub(p.started) < p.interval ||
		(p.visible && now.Sub(p.lastAt) < p.interval) {
		return
	}
	p.show(s, now)
}

func (p *progressPresenter) show(s string, now time.Time) {
	if s == p.shown {
		return
	}
	p.line.Update(s)
	p.shown = s
	p.lastAt = now
	p.visible = true
}

// Done presents the authoritative final snapshot when progress became visible,
// then closes any live terminal frame. A fast run remains silent.
func (p *progressPresenter) Done() {
	if p.visible {
		p.show(p.latest, p.now())
		p.line.Done()
	}
}

// Abort closes a visible terminal frame without presenting work completed
// while an output consumer was shutting the run down.
func (p *progressPresenter) Abort() {
	if p.visible {
		p.line.Done()
	}
}

func trialProgressLine(
	p tth2.TrialProgress,
	eta trialETA,
) string {
	var b strings.Builder
	b.WriteString("  ... ")
	if eta.limit > 0 {
		fmt.Fprintf(&b, "%d/%d trials complete", p.Trials, eta.limit)
	} else {
		fmt.Fprintf(&b, "%d trials complete", p.Trials)
	}
	if p.Warmup > 0 {
		fmt.Fprintf(&b, " | %d warmup", p.Warmup)
	}
	pace := eta.pace(p).display()
	fmt.Fprintf(&b, " | %s | %s trials/%s, %s req/%s",
		countedNoun(p.Requests, "request operation"), pace.work, pace.unit, pace.requests, pace.unit)
	if estimate := eta.compact(p); estimate != "" {
		fmt.Fprintf(&b, " | eta ~%s", estimate)
	}
	return b.String()
}

func raceProgressWanted(
	trials int,
	warmup int,
	reportSharesTerminal bool,
) bool {
	return (trials != 1 || warmup > 0) && !reportSharesTerminal
}
