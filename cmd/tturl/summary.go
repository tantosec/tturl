package main

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
)

// summarise renders a standalone Requests document. Compact mode lifts a
// shared target and headers above rows containing each group's identity,
// method, target, and distinguishing body or headers. Verbose mode renders the
// complete HTTP/2 wire view. refs follows expanded request order.
func summarise(w io.Writer, requests []requestGroup, refs runReferences, verbose bool) {
	writeBanner(w, "Requests")
	summariseRequests(w, requests, refs, verbose)
}

// summariseSection renders Requests as a named section in a command report.
func summariseSection(
	w io.Writer,
	requests []requestGroup,
	refs runReferences,
	verbose bool,
) {
	writeHeading(w, "Requests")
	summariseRequests(w, requests, refs, verbose)
}

func summariseRequests(
	w io.Writer,
	requests []requestGroup,
	refs runReferences,
	verbose bool,
) {
	summariseRequestsWithFormatter(w, requests, refs, verbose, func(_ int, request *http.Request) string {
		return formatRequest(request, true)
	})
}

// summariseRequestsWithFormatter shares request order, identities and compact
// digests while allowing a command to render its protocol's complete wire view.
func summariseRequestsWithFormatter(w io.Writer, requests []requestGroup,
	refs runReferences, verbose bool, format func(int, *http.Request) string,
) {
	if refs.compact {
		emitln(w, "References:")
		for _, ref := range refs.refs {
			writeReferenceMapping(w, ref)
		}
		emitln(w)
	}
	labels := refs.inlineLabels()

	total, warmup, baseline := 0, 0, 0
	for _, g := range requests {
		total += g.count
		if g.warmup {
			warmup += g.count
		} else if g.baseline {
			baseline += g.count
		}
	}
	totalText := fmt.Sprintf("Total: %d %s", total, plural(total))
	if baseline > 0 {
		candidates := total - warmup - baseline
		totalText = fmt.Sprintf(
			"Total: %d candidate %s, %d baseline %s, %d warmup %s",
			candidates, plural(candidates), baseline,
			countedPlural(baseline, "template"), warmup, plural(warmup))
	} else if warmup > 0 {
		measured := total - warmup
		totalText = fmt.Sprintf("Total: %d trial %s, %d warmup %s",
			measured, plural(measured), warmup, plural(warmup))
	}
	if verbose {
		idx := 0
		for _, g := range requests {
			label := groupLabel(labels, idx, g.count)
			if g.warmup {
				label += " (warmup)"
			} else if g.baseline {
				label += " (baseline; " + baselineSupplySummary(g) + ")"
			}
			if idx > 0 {
				emitln(w)
			}
			emitln(w, subordinateHeading(label))
			emit(w, format(idx, g.req))
			idx += g.count
		}
		emitf(w, "\n%s.\n", totalText)
		return
	}

	shared := commonTarget(requests)

	// Read every group's body once: the header digest needs to know which requests
	// render a body summary, and the plain groups' bodies feed the novelty diff.
	bodies := make([][]byte, len(requests))
	reqs := make([]*http.Request, len(requests))
	hasBody := make([]bool, len(requests))
	for i, g := range requests {
		bodies[i] = requestBody(g.req)
		reqs[i] = g.req
		hasBody[i] = len(bodies[i]) > 0
	}

	// The body digest describes how the plain (non-fanned) groups differ from one
	// another; a fanned group is summarised by its own vary line, not diffed,
	// since its bodies differ by construction.
	var plainBodies [][]byte
	for i, g := range requests {
		if g.fan == nil {
			plainBodies = append(plainBodies, bodies[i])
		}
	}
	digests := bodyDigests(plainBodies)

	// Headers are digested across the whole batch, fans included, so the Common
	// line holds only what every request truly carries alike; headerRows is
	// indexed by group.
	commonHeaders, headerRows := headerDigests(reqs, hasBody, defaultHeaderNames(requests))

	if shared != "" {
		emitf(w, "Target: %s\n", curlblocks.DisplayText(shared))
	}
	if commonHeaders != "" {
		emitf(w, "Common: %s\n", commonHeaders)
	}
	if shared != "" || commonHeaders != "" {
		emitln(w)
	}

	// One row per display unit: a plain group, or a whole fan collapsed. The fan's
	// variant groups are contiguous and share one *fanInfo, so a run of them folds
	// into a single "vary ..." row labelled by the fan (its --name, else its
	// token).
	rows := make([]summaryRow, 0, len(requests))
	idx, plain := 0, 0
	for i := 0; i < len(requests); {
		g := requests[i]
		if g.fan != nil {
			// The fan's groups are contiguous, so the run ends where the next group
			// belongs to a different fan (or the batch ends).
			end, runReqs := i, 0
			for ; end < len(requests) && requests[end].fan == g.fan; end++ {
				runReqs += requests[end].count
			}
			extra := fmt.Sprintf("vary %s (%d)",
				curlblocks.DisplayText(g.fan.source), runReqs)
			if g.warmup {
				extra = "warmup  " + extra
			} else if g.baseline {
				extra = "baseline  " + extra + "  " + baselineSupplySummary(g)
			}
			if hr := uniformDigest(headerRows[i:end]); hr != "" {
				extra += "  " + hr
			}
			rows = append(rows, summaryRow{
				label:  curlblocks.DisplayText(g.fan.rowName()),
				method: curlblocks.DisplayText(g.fan.method),
				target: curlblocks.DisplayText(fanTarget(g.fan, shared)),
				extra:  extra,
			})
			idx += runReqs
			i = end
			continue
		}
		label := labels[idx]
		if g.count > 1 {
			label = labels[idx] + ".." + labels[idx+g.count-1]
		}
		origin, path := wireTarget(g.req)
		target := path
		if shared == "" {
			target = origin + path
		}
		extra := requestExtras(plainBodies[plain], g.req.Header.Get("Content-Type"),
			digests[plain], headerRows[i])
		if g.warmup {
			extra = strings.TrimSpace("warmup  " + extra)
		} else if g.baseline {
			extra = strings.TrimSpace(
				"baseline  " + extra + "  " + baselineSupplySummary(g))
		}
		rows = append(rows, summaryRow{
			label:  curlblocks.DisplayText(label),
			method: curlblocks.DisplayText(g.req.Method),
			target: curlblocks.DisplayText(target),
			extra:  extra,
		})
		idx += g.count
		plain++
		i++
	}

	rowLabels := make([]string, len(rows))
	mw := 0
	for i, row := range rows {
		rowLabels[i] = row.label
		mw = max(mw, len(row.method))
	}
	lw := labelColumn(rowLabels)
	for _, row := range rows {
		line := fmt.Sprintf(
			"  %-*s  %-*s  %s", lw, row.label, mw, row.method, row.target)
		if row.extra != "" {
			line += "  " + row.extra
		}
		emitln(w, strings.TrimRight(line, " "))
	}

	emitf(w, "\n%s.\n", totalText)
}

func baselineSupplySummary(group requestGroup) string {
	configured := strconv.Itoa(group.supply.count)
	if group.supply.unlimited {
		configured = unlimitedFlagValue
	}
	return fmt.Sprintf("supply %s (effective maximum %d)",
		configured, group.baselineEffectiveMax)
}

func countedPlural(count int, singular string) string {
	if count == 1 {
		return singular
	}
	return singular + "s"
}

// summaryRow is one compact request-summary row after fan folding. Keeping the
// fields together prevents the render pass from pairing unrelated slices by
// index.
type summaryRow struct {
	label, method, target, extra string
}

func writeReferenceMapping(w io.Writer, ref runReference) {
	prefix := fmt.Sprintf("  #%d  ", ref.ID)
	continuation := strings.Repeat(" ", len(prefix))
	width := textWidth - len(prefix)
	label := ref.Label
	for first := true; len(label) > 0; first = false {
		n := min(width, len(label))
		lead := continuation
		if first {
			lead = prefix
		}
		emitln(w, lead+label[:n])
		label = label[n:]
	}
	if ref.Label == "" {
		emitln(w, prefix)
	}
}

// commonTarget returns the shared wire scheme and authority, or "" when they
// differ. Unlike dialling identity, display identity includes the scheme and
// Host override so path-only rows never conceal a target difference.
func commonTarget(requests []requestGroup) string {
	if len(requests) == 0 {
		return ""
	}
	target, _ := wireTarget(requests[0].req)
	for _, g := range requests[1:] {
		if origin, _ := wireTarget(g.req); origin != target {
			return ""
		}
	}
	return target
}

// fanTarget renders a fan's template URL for the summary: the path (token in
// place) when every request shares an origin, else that path behind the origin
// the fan's requests go out under.
func fanTarget(f *fanInfo, shared string) string {
	if shared == "" {
		return f.templateURL
	}
	return f.templatePath
}

// requestExtras renders a request's body kind, size, distinguishing content,
// and varying headers. It returns "" when none applies.
func requestExtras(body []byte, contentType, digest, headerRow string) string {
	var parts []string
	if len(body) > 0 {
		s := bodyKind(contentType) + " " + byteCount(len(body))
		if digest != "" {
			s += " " + digest
		}
		parts = append(parts, s)
	}
	if headerRow != "" {
		parts = append(parts, headerRow)
	}
	return strings.Join(parts, "  ")
}

// bodyTracked names headers already represented by body kind and size. They
// remain ordinary visible headers on bodyless requests.
var bodyTracked = map[string]bool{
	"content-type": true, "content-length": true,
}

// headerDigests separates headers shared by the whole batch from per-request
// differences. Returned rows align with reqs. Body-derived headers are omitted
// where the body summary already represents them. Parser defaults are hidden
// only while uniform. Values remain unredacted but are length-bounded, and
// names sort alphabetically.
func headerDigests(reqs []*http.Request, hasBody []bool, defaults map[string]bool) (common string, perRow []string) {
	perRow = make([]string, len(reqs))

	// The union of header names across the batch. net/http stores field names
	// canonicalised, so the same header keys identically everywhere and one
	// casing represents it.
	var names []string
	seen := map[string]bool{}
	for _, req := range reqs {
		for name := range req.Header {
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var commonParts []string
	rowParts := make([][]string, len(reqs))
	for _, name := range names {
		tracked := bodyTracked[strings.ToLower(name)]
		present := make([]bool, len(reqs))
		vals := make([]string, len(reqs))
		for i, req := range reqs {
			if tracked && hasBody[i] {
				continue // the body summary already shows this request's kind and size
			}
			if v := req.Header.Values(name); len(v) > 0 {
				present[i], vals[i] = true, strings.Join(v, ", ")
			}
		}
		if headersUniform(present, vals) {
			if defaults[strings.ToLower(name)] {
				continue // keep uniform defaults off the Common line
			}
			commonParts = append(commonParts, renderHeader(name, vals[0]))
			continue
		}
		for i := range reqs {
			if present[i] {
				rowParts[i] = append(rowParts[i], renderHeader(name, vals[i]))
			}
		}
	}

	common = strings.Join(commonParts, "  ")
	for i := range reqs {
		perRow[i] = strings.Join(rowParts[i], "  ")
	}
	return common, perRow
}

// headersUniform reports whether every request carries the header (present)
// with the same value - the test for lifting it to the shared Common line. A
// header missing from any request, or differing in value, varies instead.
func headersUniform(present []bool, vals []string) bool {
	for i := range present {
		if !present[i] || vals[i] != vals[0] {
			return false
		}
	}
	return true
}

// uniformDigest returns the header digest shared by every row of a collapsed
// run, or "" where they differ. A fan folds to one row, so it can only speak
// for its variants' headers where all of them agree; a fan that varies a header
// says so through its vary source instead.
func uniformDigest(rows []string) string {
	for _, r := range rows[1:] {
		if r != rows[0] {
			return ""
		}
	}
	return rows[0]
}

// renderHeader renders one header for the compact summary as "Name: value", the
// value sanitised to a single inline run (see [sanitiseInline]) and capped to
// maxDigest with a ".." elision so one long header (a JWT, a fat cookie) can't
// blow out the row - the complete value is a -v away, as for a long body. A
// value with a byte unsafe to inline falls back to "Name: [n bytes]" so the row
// stays ASCII on any console.
func renderHeader(name, value string) string {
	name = curlblocks.DisplayText(name)
	if s, ok := sanitiseInline([]byte(value)); ok {
		return name + ": " + capMiddle(s, maxDigest)
	}
	return fmt.Sprintf("%s: [%s]", name, byteCount(len(value)))
}

// defaultHeaderNames collects, lowercased, every header name curlblocks
// supplied for some request in the batch rather than the user asking for it.
// The union is the right unit: the summary decides per name whether to lift it
// to the Common line, and a name any request got as housekeeping is
// housekeeping wherever it is uniform.
func defaultHeaderNames(requests []requestGroup) map[string]bool {
	names := map[string]bool{}
	for _, g := range requests {
		for _, name := range g.defaults {
			names[strings.ToLower(name)] = true
		}
	}
	return names
}
