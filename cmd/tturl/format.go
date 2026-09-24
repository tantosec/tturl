package main

import (
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

// emit, emitf, and emitln keep report builders linear. Command orchestration
// supplies a reportWriter and checks it at output boundaries.
func emit(w io.Writer, args ...any) { _, _ = fmt.Fprint(w, args...) }

func emitf(w io.Writer, format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }

func emitln(w io.Writer, args ...any) { _, _ = fmt.Fprintln(w, args...) }

// formatDurationASCII preserves Go's exact duration representation while
// spelling microseconds with ASCII characters for terminal output.
func formatDurationASCII(duration time.Duration) string {
	return strings.ReplaceAll(duration.String(), "\u00b5s", "us")
}

// textWidth is the target maximum width of authored terminal text and the
// width of report-root banners and section headings. Intrinsically wide user
// data and complete experimental evidence may exceed it.
const textWidth = 80

// writeWrappedASCII writes prefix and text within textWidth, using a hanging
// indent and preferring punctuation or word boundaries. All callers provide
// the CLI's printable-ASCII display projection.
func writeWrappedASCII(w io.Writer, prefix, text string) {
	continuation := strings.Repeat(" ", len(prefix))
	for first := true; first || len(text) > 0; first = false {
		lead := continuation
		if first {
			lead = prefix
		}
		room := max(1, textWidth-len(lead))
		n := min(room, len(text))
		if n < len(text) {
			if split := strings.LastIndex(text[:n+1], ", "); split >= 0 {
				n = split + 1
			} else if split := strings.LastIndexByte(text[:n+1], ' '); split > 0 {
				n = split
			}
		}
		emitln(w, lead+text[:n])
		text = strings.TrimPrefix(text[n:], " ")
	}
}

// heading constructs one section-heading line without contextual whitespace.
func heading(label string) string {
	padded := " " + label + " "
	if len(padded) >= textWidth {
		return padded
	}
	dashes := textWidth - len(padded)
	left := dashes / 2
	return strings.Repeat("-", left) + padded + strings.Repeat("-", dashes-left)
}

// subordinateHeading constructs a left-anchored ruler for a named item within
// a section. It deliberately carries no contextual whitespace: request
// summaries place the wire representation immediately below it so the only
// blank line within that representation remains the header/body boundary.
func subordinateHeading(label string) string {
	prefix := "--- " + label + " "
	const minimumRule = 3
	if len(prefix)+minimumRule >= textWidth {
		return prefix + strings.Repeat("-", minimumRule)
	}
	return prefix + strings.Repeat("-", textWidth-len(prefix))
}

// writeHeading separates a section from preceding content and leaves one blank
// line before its body. Report streams avoid adding a second blank line when
// the preceding block already supplied one. Plain writers are used only for
// report fragments that are appended after existing content.
func writeHeading(w io.Writer, label string) {
	if report, ok := w.(*reportWriter); ok {
		report.ensureBlankLine()
		emitln(report, heading(label))
		report.ensureBlankLine()
		return
	}
	emitf(w, "\n%s\n\n", heading(label))
}

// centre pads s with leading spaces so it sits roughly centred within width.
func centre(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return strings.Repeat(" ", (width-len(s))/2) + s
}

// banner constructs a centred document title fenced above and below by
// full-width '=' rules. It ends with a newline but no blank line.
func banner(title string) string {
	rule := strings.Repeat("=", textWidth)
	return fmt.Sprintf("%s\n%s\n%s\n", rule, centre(title, textWidth), rule)
}

// writeBanner opens a document and leaves one blank line before its content.
func writeBanner(w io.Writer, title string) {
	emit(w, banner(title))
	if report, ok := w.(*reportWriter); ok {
		report.ensureBlankLine()
		return
	}
	emitln(w)
}

// labelColumn returns the label column width for a text table. It never
// drops below a floor, so a table of only short labels still gets a label
// column wide enough to read.
func labelColumn(labels []string) int {
	w := 6 // floor: keeps a short-label table's column readable
	for _, l := range labels {
		if len(l) > w {
			w = len(l)
		}
	}
	return w
}

// pct formats a fraction in [0,1] as a one-decimal percentage.
func pct(fraction float64) string {
	return fmt.Sprintf("%.1f%%", 100*fraction)
}

// compactRate formats a rate in its selected time unit: "--" until a rate is
// known, enough fractional digits that a positive rate never reads as zero,
// and k/M suffixes above a thousand. ASCII only, for a portable console.
func compactRate(v float64) string {
	switch {
	case invalidRate(v):
		return "--"
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 1:
		return fmt.Sprintf("%.1f", v)
	default:
		decimals := int(-math.Floor(math.Log10(v))) + 1
		if decimals > 6 {
			return "<0.000001"
		}
		return fmt.Sprintf("%.*f", decimals, v)
	}
}

// paceRates is a sustainable work and request pace in per-second units. Work
// is a trial for race and measure and a comparison for detect.
type paceRates struct {
	work, requests float64
}

// sustainablePace retains the recent work-per-request relationship while
// constraining both rates to the configured batch and request capacities. With
// no binding ceiling, the recent rates pass through unchanged.
func sustainablePace(
	work, requests, maxWorkPerSecond, maxRequestsPerSecond float64,
) paceRates {
	if maxWorkPerSecond > 0 && !invalidRate(work) && work > maxWorkPerSecond {
		scale := maxWorkPerSecond / work
		work = maxWorkPerSecond
		if !invalidRate(requests) {
			requests *= scale
		}
	}
	if maxRequestsPerSecond > 0 && !invalidRate(requests) &&
		requests > maxRequestsPerSecond {
		scale := maxRequestsPerSecond / requests
		requests = maxRequestsPerSecond
		if !invalidRate(work) {
			work *= scale
		}
	}
	return paceRates{work: work, requests: requests}
}

type paceDisplay struct {
	work, requests, unit string
}

// display selects one shared unit for both rates so their request cost remains
// visible. Units grow only as far as days; very small positive values retain
// fractional digits rather than becoming a false zero.
func (p paceRates) display() paceDisplay {
	basis := p.requests
	if invalidRate(basis) {
		basis = p.work
	}
	unit, scale := "s", 1.0
	switch {
	case invalidRate(basis), basis >= 1:
	case basis*60 >= 1:
		unit, scale = "m", 60
	case basis*3600 >= 1:
		unit, scale = "h", 3600
	default:
		unit, scale = "d", 86400
	}
	return paceDisplay{
		work:     compactRate(p.work * scale),
		requests: compactRate(p.requests * scale),
		unit:     unit,
	}
}

// trialETA describes the capacities governing a bounded run. Batch rate applies
// directly; measured width turns a request-rate ceiling into a trial-rate
// ceiling; progress supplies the rate the client and target are sustaining.
type trialETA struct {
	limit                int
	requestsPerTrial     int
	maxBatchesPerSecond  float64
	maxRequestsPerSecond float64
}

func (e trialETA) pace(p tth2.TrialProgress) paceRates {
	return sustainablePace(
		p.RecentTrialsPerSecond, p.RecentRequestsPerSecond,
		e.maxBatchesPerSecond, e.maxRequestsPerSecond,
	)
}

// compact projects the remaining trials through the same sustainable pace the
// progress line presents. Before any throughput is measurable, a configured
// request cap and the fixed measured batch width still supply a known trial
// capacity.
func (e trialETA) compact(p tth2.TrialProgress) string {
	remaining := e.limit - p.Trials
	if e.limit <= 0 || remaining <= 0 {
		return ""
	}
	capacity := e.pace(p).work
	if invalidRate(capacity) && e.maxRequestsPerSecond > 0 &&
		e.requestsPerTrial > 0 {
		capacity = e.maxRequestsPerSecond / float64(e.requestsPerTrial)
	}
	if e.maxBatchesPerSecond > 0 &&
		(invalidRate(capacity) || capacity > e.maxBatchesPerSecond) {
		capacity = e.maxBatchesPerSecond
	}
	if invalidRate(capacity) {
		return ""
	}
	return compactETADuration(float64(remaining) / capacity)
}

func invalidRate(rate float64) bool {
	return math.IsNaN(rate) || math.IsInf(rate, 0) || rate <= 0
}

// compactETADuration rounds an estimate upward without presenting false
// precision or promising zero time while work remains.
func compactETADuration(seconds float64) string {
	maxSeconds := float64(math.MaxInt64) / float64(time.Second)
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 ||
		seconds > maxSeconds {
		return ""
	}
	step := time.Second
	switch {
	case seconds >= 60:
		step = 10 * time.Second
	case seconds >= 10:
		step = 5 * time.Second
	}
	units := math.Ceil(seconds / step.Seconds())
	maxUnits := float64(math.MaxInt64 / int64(step))
	if units > maxUnits {
		return ""
	}
	estimate := time.Duration(units) * step
	return estimate.String()
}

// detectProgressLine composes a one-line progress update: rank-complete request
// participations and the sustainable comparison and request pace, the strongest
// observed log evidence against its decision threshold, and the front-runners
// toward the end(s) it cares about. The caller renders it via a
// [termline.Writer].
func detectProgressLine(
	r ranking.OutlierProgress,
	comparisonsMax int,
	maxBatchesPerSecond float64,
	maxRequestsPerSecond float64,
	direction ranking.OutlierDirection,
	labels []string,
) string {
	var b strings.Builder
	b.WriteString("  ... ")
	if comparisonsMax > 0 {
		fmt.Fprintf(&b, "%d/%d comparisons",
			r.Comparisons, comparisonsMax)
	} else {
		fmt.Fprintf(&b, "%d comparisons", r.Comparisons)
	}
	pace := sustainablePace(
		r.ComparisonsPerSecond, r.ParticipationsPerSecond,
		maxBatchesPerSecond, maxRequestsPerSecond,
	).display()
	fmt.Fprintf(&b, " | %s | %s cmp/%s, %s req/%s",
		countedNoun(r.Participations, "request participation"), pace.work, pace.unit, pace.requests, pace.unit)
	if d, ok := r.Detail.(ranking.OutlierProgressDiagnostics); ok {
		if direction != ranking.Early {
			b.WriteString(extremeList("late", d, ranking.Late, 3, labels))
		}
		if direction != ranking.Late {
			b.WriteString(extremeList("early", d, ranking.Early, 3, labels))
		}
	}
	best := math.Inf(-1)
	threshold := math.Inf(1)
	if d, ok := r.Detail.(ranking.OutlierProgressDiagnostics); ok {
		threshold = d.Threshold
		for _, contender := range d.Contenders {
			if direction != ranking.Early {
				best = max(best, contender.LogEvidence(ranking.Late))
			}
			if direction != ranking.Late {
				best = max(best, contender.LogEvidence(ranking.Early))
			}
		}
	}
	if !math.IsInf(best, -1) && !math.IsInf(threshold, 1) {
		fmt.Fprintf(&b, " | log evidence %.1f/%.1f", best, threshold)
	}
	return b.String()
}

// extremeList renders up to n front-runners toward one extreme, as
// "  | late: C 3.1, B 1.2", each with its log evidence for that extreme
// ([ranking.OutlierProgressDiagnostics.Leaders]).
func extremeList(
	label string, board ranking.OutlierProgressDiagnostics,
	dir ranking.OutlierDirection, n int, labels []string,
) string {
	leaders := board.Leaders(dir, n)
	if len(leaders) == 0 {
		return ""
	}
	parts := make([]string, len(leaders))
	for i, r := range leaders {
		parts[i] = fmt.Sprintf("%s %.1f", labels[r.Item], r.LogEvidence(dir))
	}
	return fmt.Sprintf(" | %s: %s", label, strings.Join(parts, ", "))
}

// directionWord renders a ranking.OutlierDirection in the tool's vocabulary.
func directionWord(direction ranking.OutlierDirection) string {
	switch direction {
	case ranking.Early:
		return "early"
	case ranking.Either:
		return "either"
	default:
		return "late"
	}
}

// groupLabel names a request group occupying labels[idx:idx+count]. A single
// request is "Request <name>"; a repeated one spans "Requests <first> through
// <last> (<count> requests)". The group's labels are contiguous (see
// curlblocks.ResolveNames), so first-through-last is exact.
func groupLabel(labels []string, idx, count int) string {
	if count == 1 {
		return fmt.Sprintf("Request %s", labels[idx])
	}
	first, last := labels[idx], labels[idx+count-1]
	return fmt.Sprintf("Requests %s through %s (%d %s)", first, last, count, plural(count))
}

// plural returns "request" for n == 1 and "requests" otherwise.
func plural(n int) string {
	if n == 1 {
		return "request"
	}
	return "requests"
}

func countedNoun(n int, singular string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", singular)
	}
	if strings.HasSuffix(singular, "batch") {
		return fmt.Sprintf("%d %ses", n, singular)
	}
	return fmt.Sprintf("%d %ss", n, singular)
}

// joinRequestLabels renders selected request identities as an English list.
func joinRequestLabels(labels []string, requests []int) string {
	selected := make([]string, len(requests))
	for i, request := range requests {
		selected[i] = labels[request]
	}
	if len(selected) < 3 {
		return strings.Join(selected, " and ")
	}
	return strings.Join(selected[:len(selected)-1], ", ") +
		", and " + selected[len(selected)-1]
}

// maxDigest bounds the rendered body digest; a longer one is shortened to its
// head and tail around a ".." elision.
const maxDigest = 64

// sanitiseInline collapses interior runs of whitespace in a body slice to
// single spaces and trims the ends, so a formatted JSON or form body reads on
// one line. ok is false if the slice holds a non-printable or non-ASCII byte;
// binary is not safe to show on a console line.
func sanitiseInline(data []byte) (string, bool) {
	var b strings.Builder
	pendingSpace := false
	for _, c := range data {
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			pendingSpace = true
		case c < 0x20 || c > 0x7e:
			return "", false
		default:
			if pendingSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			pendingSpace = false
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// capMiddle shortens s to at most maxLen characters, keeping its head and tail
// around a ".." elision. s is sanitised ASCII (see sanitiseInline), so a byte
// index is a character index.
func capMiddle(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	keep := maxLen - 2 // the ".." costs two
	head := (keep + 1) / 2
	return s[:head] + ".." + s[len(s)-(keep-head):]
}

// bodyKind names a request body from its Content-Type media type, in the
// compact vocabulary the summary uses: json, form, multipart, or a plain "body"
// for anything else (including a missing type).
func bodyKind(contentType string) string {
	mediaType, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "application/json":
		return "json"
	case "application/x-www-form-urlencoded":
		return "form"
	case "multipart/form-data":
		return "multipart"
	default:
		return "body"
	}
}

// byteCount formats a byte size for the compact summary: plain bytes below a
// kibibyte, then kB / MB with one decimal. ASCII only, for a portable console.
func byteCount(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fkB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
