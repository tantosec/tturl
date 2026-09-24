package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/h2tls"
	"github.com/tantosec/tturl/tth2"
)

// maxFanSize caps how many requests one --vary fan may expand to. The ceiling
// is here to refuse a mistyped source or combination before the requests are
// built, not to be reached.
const maxFanSize = 100_000

const multipartBoundaryBytes = 30

// tturlMaxResponseHeaderBytes is the decoded response header or trailer section
// limit applied by every request command and reported in structured metadata.
const tturlMaxResponseHeaderBytes int64 = 1 << 20

// requestGroup is one prepared request together with the number of identical
// copies the plan calls for (a block's --repeat). Display collapses the copies
// into a single entry; expand re-materialises them for sending. A group that is
// one variant of a --vary fan carries the shared fanInfo describing that fan,
// so the summary collapses the whole fan into a single row.
type requestGroup struct {
	req                  *http.Request
	count                int
	fan                  *fanInfo // non-nil for a fan variant; shared across the fan's variants
	warmup               bool
	baseline             bool
	supply               baselineSupply
	baselineEffectiveMax int
	// defaults are the header names curlblocks supplied for this request rather
	// than the user asking for them (see curlblocks.Block.DefaultHeaderNames),
	// which the summary treats as housekeeping while they stay uniform.
	defaults []string
}

// assemble expands the plan and builds its requests without network access. It
// preserves curlblocks' group, label, pin, and shared-fan relationships.
func assemble(
	ctx context.Context,
	plan *curlblocks.Plan,
) (*assembled, error) {
	return assembleWithRoles(ctx, plan, nil)
}

// assembleWithRoles orders measured requests before distinct warmup requests,
// preserving canonical input order within each set. A nil isWarmup makes every
// block measured.
func assembleWithRoles(
	ctx context.Context,
	plan *curlblocks.Plan,
	isWarmup func(blockIndex int) bool,
) (*assembled, error) {
	return assembleWithRolesAndBoundarySource(ctx, plan, isWarmup, rand.Reader)
}

func assembleWithRolesAndBoundarySource(
	ctx context.Context,
	plan *curlblocks.Plan,
	isWarmup func(blockIndex int) bool,
	boundarySource io.Reader,
) (*assembled, error) {
	return assembleWithRequestRolesAndBoundarySource(
		ctx, plan, func(blockIndex int) requestGroupRole {
			if isWarmup != nil && isWarmup(blockIndex) {
				return requestGroupWarmup
			}
			return requestGroupCandidate
		}, nil, boundarySource)
}

type requestGroupRole uint8

const (
	requestGroupCandidate requestGroupRole = iota
	requestGroupBaseline
	requestGroupWarmup
)

func assembleWithRequestRoles(
	ctx context.Context,
	plan *curlblocks.Plan,
	role func(blockIndex int) requestGroupRole,
	supplies []baselineSupply,
) (*assembled, error) {
	return assembleWithRequestRolesAndBoundarySource(
		ctx, plan, role, supplies, rand.Reader)
}

func assembleWithRequestRolesAndBoundarySource(
	ctx context.Context,
	plan *curlblocks.Plan,
	role func(blockIndex int) requestGroupRole,
	supplies []baselineSupply,
	boundarySource io.Reader,
) (*assembled, error) {
	expanded, err := plan.Expand()
	if err != nil {
		return nil, err
	}
	boundary, err := newMultipartBoundary(boundarySource)
	if err != nil {
		return nil, err
	}
	var candidateGroups, baselineGroups, warmupGroups []requestGroup
	var candidateLabels, baselineLabels, warmupLabels []curlblocks.Label
	var notes []string
	// One fanInfo per fan, keyed by the *Fan its groups share, so the summary row
	// and the ragged-sources note are each produced once for the whole fan.
	fis := map[*curlblocks.Fan]*fanInfo{}
	for _, g := range expanded {
		req, err := buildRequestWithMultipartBoundary(
			ctx, g.Block, g.URL, boundary)
		if err != nil {
			return nil, plan.BlockError(g.BlockIndex, err)
		}
		var fi *fanInfo
		if g.Fan != nil {
			fi = fis[g.Fan]
			if fi == nil {
				// The pre-substitution block, so the label reads in tokens (as
				// Fan.Template does) rather than in the first variant's values. The
				// origin comes from this first variant's built request, which every
				// variant of the fan shares.
				origin, _ := wireTarget(req)
				fi = newFanInfo(plan.Blocks[g.BlockIndex], g.Fan, origin)
				fis[g.Fan] = fi
				if g.Fan.Ragged {
					notes = append(notes, fmt.Sprintf(
						"pitchfork --vary sources for %s differ in length; truncated to the shortest (%d)",
						fi.rowName(), len(g.Fan.Variants)))
				}
			}
		}
		groupRole := role(g.BlockIndex)
		group := requestGroup{
			req: req, count: g.Repeat, fan: fi,
			warmup:   groupRole == requestGroupWarmup,
			baseline: groupRole == requestGroupBaseline,
			defaults: g.Block.DefaultHeaderNames(),
		}
		if group.baseline {
			group.supply = supplies[g.BlockIndex]
			baselineGroups = append(baselineGroups, group)
			baselineLabels = append(baselineLabels, g.Labels...)
		} else if group.warmup {
			warmupGroups = append(warmupGroups, group)
			warmupLabels = append(warmupLabels, g.Labels...)
		} else {
			candidateGroups = append(candidateGroups, group)
			candidateLabels = append(candidateLabels, g.Labels...)
		}
	}
	groups := slices.Concat(candidateGroups, baselineGroups, warmupGroups)
	labels := slices.Concat(candidateLabels, baselineLabels, warmupLabels)
	// Project curlblocks' complete labels and group pins onto the tool's views.
	resolved := make([]string, len(labels))
	pins := map[string][]string{}
	for i, l := range labels {
		resolved[i] = l.Display
		for _, key := range l.Pins {
			pins[key] = append(pins[key], l.Display)
		}
	}
	return &assembled{
		groups: groups, labels: resolved, pins: pins, notes: notes,
		measuredCount: len(candidateLabels),
	}, nil
}

func newMultipartBoundary(source io.Reader) (string, error) {
	var random [multipartBoundaryBytes]byte
	if _, err := io.ReadFull(source, random[:]); err != nil {
		return "", fmt.Errorf("generate multipart boundary: %w", err)
	}
	return hex.EncodeToString(random[:]), nil
}

// assembled is the network-free result of assemble: request groups, each
// concrete request's complete label, group pins, and assembly notes.
type assembled struct {
	groups []requestGroup
	labels []string
	pins   map[string][]string
	notes  []string // human notes to print after the summary (e.g. a pitchfork truncation)
	// measuredCount is the candidate or measured-request prefix length. Baseline
	// templates and warmups, when present, follow it in that order.
	measuredCount int
}

// expand flattens the groups into the request slice handed to Send: each
// group's request repeated count times. The same *http.Request backs every
// copy of a group — tth2 reads each body afresh via GetBody and encodes
// headers per stream, so a shared pointer yields independent, identical
// streams.
func expand(groups []requestGroup) []*http.Request {
	var reqs []*http.Request
	for _, g := range groups {
		for range g.count {
			reqs = append(reqs, g.req)
		}
	}
	return reqs
}

func expandRole(groups []requestGroup, warmup bool) []*http.Request {
	var selected []requestGroup
	for _, group := range groups {
		if group.warmup == warmup {
			selected = append(selected, group)
		}
	}
	return expand(selected)
}

// fanInfo describes a fanned block for the summary. Every variant group of one
// fan holds the same *fanInfo, so summarise renders the run as a single row.
type fanInfo struct {
	name         string   // --name, the fan's label in the output, or ""
	tokens       []string // the --vary tokens, in binding order
	source       string   // short description of the value source (from Fan.Source)
	method       string
	templatePath string // template URL's RequestURI, tokens in place (shared origin)
	templateURL  string // origin plus that path, for the no-shared-target fallback
}

// newFanInfo builds a summary descriptor from the pre-substitution block and
// resolved fan. origin is the requests' actual scheme and authority.
func newFanInfo(b curlblocks.Block, fan *curlblocks.Fan, origin string) *fanInfo {
	tmpl := fan.Template
	path, full := tmpl, tmpl
	if u, err := url.Parse(tmpl); err == nil {
		path = u.RequestURI()
		full = origin + path
	}
	return &fanInfo{
		name:         b.Name,
		tokens:       fan.Tokens,
		source:       fan.Source,
		method:       b.HTTPMethod(),
		templatePath: path,
		templateURL:  full,
	}
}

// rowName is the fan's identity on its summary row: its --name if set, else
// its tokens joined by ",". It is a label for the fan's requests, not one of
// the labels naming them.
func (f *fanInfo) rowName() string {
	if f.name != "" {
		return f.name
	}
	return strings.Join(f.tokens, ",")
}

// materialised is the network-free request plan: assembled request sets and
// run-wide labels ordered candidates or measured requests, baseline templates,
// then warmups.
type materialised struct {
	*assembled
	reqs              []*http.Request
	baselineTemplates []baselineRequestTemplate
	warmupReqs        []*http.Request
}

// prepared adds the live client used to execute a materialised plan. The
// caller closes its idle connections.
type prepared struct {
	*materialised
	client *tth2.Client
}

type baselineRequestTemplate struct {
	req    *http.Request
	label  string
	supply baselineSupply
}

// allRequests returns the run-wide request order: candidates or measured
// requests, baseline templates, then distinct warmup requests. The returned
// slice is independent of the fields so callers can append or reorder it
// safely.
func (p *materialised) allRequests() []*http.Request {
	baselines := make([]*http.Request, len(p.baselineTemplates))
	for i := range p.baselineTemplates {
		baselines[i] = p.baselineTemplates[i].req
	}
	return slices.Concat(p.reqs, baselines, p.warmupReqs)
}

// warmupBatchWidth returns the request-operation cost of one priming trial.
// With no distinct batch, priming reuses the measured batch.
func (p *materialised) warmupBatchWidth(warmup int) int {
	if warmup <= 0 {
		return 0
	}
	if len(p.warmupReqs) > 0 {
		return len(p.warmupReqs)
	}
	return len(p.reqs)
}

// activeBatchWidth is the greatest simultaneously active stream count across
// measured and priming trials.
func (p *materialised) activeBatchWidth(measuredWidth, warmup int) int {
	return max(measuredWidth, p.warmupBatchWidth(warmup))
}

// resolveBodyRelease records whether the concrete request sets, including
// distinct priming batches and positional padding, can exercise body-tail
// release.
func (p *materialised) resolveBodyRelease(
	plan bodyReleasePlan,
	padding []tth2.RequestPadding,
) bodyReleasePlan {
	for _, req := range p.allRequests() {
		if req.Body != nil && req.Body != http.NoBody && req.ContentLength != 0 {
			plan.hasBodyBytes = true
			return plan
		}
	}
	for _, pad := range padding {
		if pad.BodyParams > 0 {
			plan.hasBodyBytes = true
			return plan
		}
	}
	return plan
}

func materialiseAssembled(a *assembled) (*materialised, error) {
	reqs := expandRequestGroupRole(a.groups, requestGroupCandidate)
	warmupReqs := expandRequestGroupRole(a.groups, requestGroupWarmup)
	baselineTemplates := collectBaselineTemplates(a)
	if len(reqs) == 0 {
		return nil, fmt.Errorf("at least one measured request is required")
	}
	// Check that the batch points at one origin before anything is set up; the
	// connection itself is dialled lazily on the first send. The requests are
	// built, so this reads the final URLs — a --vary fan's tokens already
	// substituted — rather than the templates the plan holds.
	allReqs := make([]*http.Request, 0,
		len(reqs)+len(baselineTemplates)+len(warmupReqs))
	allReqs = append(allReqs, reqs...)
	for _, baseline := range baselineTemplates {
		allReqs = append(allReqs, baseline.req)
	}
	allReqs = append(allReqs, warmupReqs...)
	if _, err := tth2.BatchOrigin(allReqs); err != nil {
		return nil, err
	}
	return &materialised{
		assembled: a, reqs: reqs,
		baselineTemplates: baselineTemplates, warmupReqs: warmupReqs,
	}, nil
}

func prepareMaterialised(
	materialised *materialised,
	plan *curlblocks.Plan,
	tuning transportTuningConfig,
) (*prepared, error) {
	tlsCfg := &tls.Config{}
	if plan.Globals.Insecure {
		tlsCfg.InsecureSkipVerify = true
	}
	// Fatal rather than a warning: without its TLS key log, a captured run cannot
	// be inspected after the fact.
	keyLog, err := h2tls.KeyLogWriter()
	if err != nil {
		return nil, err
	}
	tlsCfg.KeyLogWriter = keyLog

	client := &tth2.Client{Transport: &tth2.Transport{
		TLSClientConfig:             tlsCfg,
		MaxResponseHeaderBytes:      tturlMaxResponseHeaderBytes,
		MaxConnsPerHost:             tuning.maxConns,
		MaxRequestsPerSecondPerHost: tuning.requestRateMax,
		MinBatchIntervalPerHost:     tuning.minBatchInterval,
	}}

	return &prepared{materialised: materialised, client: client}, nil
}

func expandRequestGroupRole(
	groups []requestGroup,
	role requestGroupRole,
) []*http.Request {
	var reqs []*http.Request
	for _, group := range groups {
		matches := role == requestGroupCandidate && !group.baseline && !group.warmup ||
			role == requestGroupBaseline && group.baseline ||
			role == requestGroupWarmup && group.warmup
		if matches {
			reqs = append(reqs, expand([]requestGroup{group})...)
		}
	}
	return reqs
}

func collectBaselineTemplates(a *assembled) []baselineRequestTemplate {
	templates := make([]baselineRequestTemplate, 0)
	labelIndex := 0
	for _, group := range a.groups {
		if group.baseline {
			templates = append(templates, baselineRequestTemplate{
				req: group.req, label: a.labels[labelIndex], supply: group.supply,
			})
		}
		labelIndex += group.count
	}
	return templates
}

// summarise renders Requests as a section of a friendly command report.
func (p *materialised) summarise(w io.Writer, refs runReferences, verbose bool) {
	summariseSection(w, p.groups, refs, verbose)
	for _, n := range p.notes {
		emitf(w, "\nNote: %s\n", n)
	}
}

// summariseRequestEcho renders an unheaded request summary beside a structured
// report on another stream. Stderr diagnostics are not a second report
// document, so they do not carry a banner or section heading.
func (p *materialised) summariseRequestEcho(
	w io.Writer,
	refs runReferences,
	verbose bool,
) {
	summariseRequests(w, p.groups, refs, verbose)
	for _, n := range p.notes {
		emitf(w, "\nNote: %s\n", n)
	}
}
