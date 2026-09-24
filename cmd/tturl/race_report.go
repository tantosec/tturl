package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/tth2"
)

// RaceReporter receives one race run as records. Execution produces the same
// records regardless of the selected presentation.
type RaceReporter interface {
	Run(RunRecord) error
	Request(RequestRecord) error
	Trial(TrialRecord) error
	Summary(SummaryRecord) error
}

// RunRecord describes the run before any request or trial records.
type RunRecord struct {
	Trials                 int
	TrialsRequested        int
	Warmup                 int
	TrialBatchWidth        int
	WarmupBatchWidth       int
	Connections            int
	Delivery               deliveryPlan
	BuildInfo              buildinfo.Info
	Argv                   []string
	StartedAt              time.Time
	DialAddress            string
	Origin                 *string
	RequestCount           int
	ArrangementPolicy      string
	ReleaseDelay           time.Duration
	BodyBytesWithheld      int
	bodyRelease            bodyReleasePlan
	ResponseLimits         responseLimits
	RunTimeout             runTimeout
	Insecure               bool
	CaptureHeaders         bool
	CaptureBodyBytes       int64
	MaxResponseHeaderBytes int64
	ExtractRegex           []string
	ExtractFields          [][]string
	Padding                []PaddingRecord
}

// RequestRecord identifies one concrete request in run-wide expansion order.
type RequestRecord struct {
	Reference     runReference
	RequestSet    string
	URL           string
	PseudoHeaders map[string]string
	Headers       HeaderRecord
	Body          *EncodedBody
}

// TrialRecord carries one yielded observation.
type TrialRecord struct {
	Trial *tth2.Trial
	// Progress is a run-wide snapshot taken as this yielded trial begins
	// capture. Concurrent completions may already be waiting behind it.
	Progress      *tth2.TrialProgress
	RequestOffset int
	Outcomes      []OutcomeRecord
}

// SummaryRecord closes a run with non-inferential outcome counts.
type SummaryRecord struct {
	Execution     raceExecution
	ResetCount    []int
	Completion    runCompletion
	MeasuredWidth int
	PrimingWidth  int
}

// textRaceReporter renders the append-only race text format.
type textRaceReporter struct {
	w             io.Writer
	run           RunRecord
	refs          []runReference
	warmups       map[int]int
	trials        int
	trialRequests int
}

func newTextRaceReporter(w io.Writer) *textRaceReporter {
	return &textRaceReporter{w: w, warmups: map[int]int{}}
}

func (r *textRaceReporter) Run(rec RunRecord) error {
	r.run = rec
	var b bytes.Buffer
	if err := reportRaceExperiment(&b, rec); err != nil {
		return err
	}
	writeHeading(&b, "Results")
	if len(rec.ExtractRegex) > 0 {
		switch rec.CaptureBodyBytes {
		case tth2.UnlimitedResponseBytes:
			emitln(&b, "Extraction scope: complete response bodies.")
		case 0:
		default:
			emitf(&b,
				"Extraction scope: up to %d B from the start of each response body.\n",
				rec.CaptureBodyBytes)
		}
	}
	for i, pattern := range rec.ExtractRegex {
		label := "Extract"
		if len(rec.ExtractRegex) > 1 {
			label += " " + strconv.Itoa(i)
		}
		emitf(&b, "%s: `%s`\n",
			label, curlblocks.DisplayText(pattern))
	}
	if len(rec.ExtractRegex) > 0 {
		emitln(&b)
	}
	_, err := r.w.Write(b.Bytes())
	return err
}

func reportRaceExperiment(w io.Writer, rec RunRecord) error {
	writeHeading(w, "Experiment")
	if rec.Trials < 0 {
		writeWrappedASCII(w, "Trials: ", fmt.Sprintf(
			"until stopped; %s per trial.",
			countedNoun(rec.TrialBatchWidth, "request")))
	} else {
		operations, ok := checkedProduct(rec.Trials, rec.TrialBatchWidth)
		if !ok {
			return fmt.Errorf("race request-operation count cannot be represented")
		}
		if rec.TrialsRequested != rec.Trials {
			cycles := rec.Trials / rec.TrialBatchWidth
			writeWrappedASCII(w, "Trials: ", fmt.Sprintf(
				"%d requested; %d planned as %s (%s).",
				rec.TrialsRequested, rec.Trials,
				countedNoun(cycles, "complete rotation cycle"),
				countedNoun(operations, "request operation")))
		} else {
			writeWrappedASCII(w, "Trials: ", fmt.Sprintf(
				"%d x %s = %s.", rec.Trials,
				countedNoun(rec.TrialBatchWidth, "request"),
				countedNoun(operations, "request operation")))
		}
	}
	if rec.Warmup > 0 {
		operationsPerConnection, ok := checkedProduct(
			rec.Warmup, rec.WarmupBatchWidth)
		if !ok {
			return fmt.Errorf("warmup request-operation count cannot be represented")
		}
		if rec.Delivery.pathFitUnresolved {
			writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
				"%s per connection (%s per connection); total unresolved until execution.",
				countedNoun(rec.Warmup, "priming attempt"),
				countedNoun(operationsPerConnection,
					"priming request operation")))
		} else {
			operations, ok := checkedProduct(
				operationsPerConnection, rec.Connections)
			if !ok {
				return fmt.Errorf("warmup request-operation count cannot be represented")
			}
			writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
				"%s per connection (%s).",
				countedNoun(rec.Warmup, "priming attempt"),
				countedNoun(operations, "priming request operation")))
		}
	}
	if rec.TrialBatchWidth == 1 {
		writeWrappedASCII(w, "Comparison: ",
			"one request supports response inspection, but supplies no comparative arrival order.")
	}
	policy := rec.ArrangementPolicy
	if policy == "" {
		policy = arrangeNone
	}
	emitf(w, "Arrangement: %s.\n", policy)
	reportRacePadding(w, rec.Padding)
	delivery := rec.Delivery
	if delivery.connectionLimit == 0 {
		delivery.connectionLimit = rec.Connections
	}
	if delivery.connectionCeiling == 0 {
		delivery.connectionCeiling = delivery.connectionLimit
	}
	if err := reportDeliveryPolicy(w, delivery); err != nil {
		return err
	}
	if err := reportPrimingPacing(w, delivery, rec.TrialBatchWidth, rec.WarmupBatchWidth); err != nil {
		return err
	}
	reportReleasePlan(w, rec.bodyRelease)
	reportResponseLimits(w, rec.ResponseLimits)
	reportRunTimeout(w, rec.RunTimeout)
	reportTLSPlan(w, rec.Insecure)
	return nil
}

func reportRacePadding(w io.Writer, padding []PaddingRecord) {
	if len(padding) == 0 {
		return
	}
	type component struct {
		name  string
		value func(PaddingRecord) int
	}
	components := []component{
		{name: "query parameters", value: func(p PaddingRecord) int { return p.URLParams }},
		{name: "headers", value: func(p PaddingRecord) int { return p.Headers }},
		{name: "body parameters", value: func(p PaddingRecord) int { return p.BodyParams }},
	}
	var parts []string
	for _, candidate := range components {
		values := make([]string, len(padding))
		active := false
		for i, pad := range padding {
			value := candidate.value(pad)
			values[i] = strconv.Itoa(value)
			active = active || value != 0
		}
		if active {
			parts = append(parts, fmt.Sprintf(
				"%s [%s]", candidate.name, strings.Join(values, " ")))
		}
	}
	if len(parts) == 0 {
		return
	}
	position := "outbound position 0"
	if len(padding) > 1 {
		position = fmt.Sprintf("outbound positions 0..%d", len(padding)-1)
	}
	writeWrappedASCII(w, "Padding: ",
		position+"; "+strings.Join(parts, "; ")+".")
}

func (r *textRaceReporter) Request(rec RequestRecord) error {
	r.refs = append(r.refs, rec.Reference)
	if rec.RequestSet != "warmup" {
		r.trialRequests++
	}
	return nil
}

func (r *textRaceReporter) Trial(rec TrialRecord) error {
	var b bytes.Buffer
	trial := rec.Trial
	compact := r.run.Trials == 1 && r.run.Warmup == 0 &&
		r.run.Connections == 1

	if r.trials > 0 {
		emitln(&b)
	}
	r.trials++
	if !compact {
		if trial.Warmup {
			r.warmups[trial.Conn]++
			emitf(&b, "Priming %d/%d  conn %d",
				r.warmups[trial.Conn], r.run.Warmup, trial.Conn)
		} else {
			if r.run.Trials < 0 {
				emitf(&b, "Trial %d  conn %d",
					trial.Index+1, trial.Conn)
			} else {
				emitf(&b, "Trial %d/%d  conn %d",
					trial.Index+1, r.run.Trials, trial.Conn)
			}
			if trial.Cycle >= 0 {
				emitf(&b, "  cycle %d  shift %d", trial.Cycle, trial.Shift)
			}
		}
		writeTrialPace(&b, rec.Progress, r.eta())
		if trial.Err != nil {
			emit(&b, "  failed")
		}
		emitln(&b)
		emitln(&b)
	}

	if err := r.writeTrialTable(&b, rec); err != nil {
		return err
	}
	if trial.Err != nil {
		if failure := classifyBatchFailure(trial.Err); failure != nil {
			emitf(&b, "  Failure: %s\n", failure.Code)
		}
		emitf(&b, "  Error: %s\n", curlblocks.DisplayText(trial.Err.Error()))
	}
	_, err := r.w.Write(b.Bytes())
	return err
}

func (r *textRaceReporter) eta() trialETA {
	eta := trialETA{
		limit: r.run.Trials, requestsPerTrial: r.trialRequests,
	}
	eta.maxRequestsPerSecond = r.run.Delivery.requestRateMax
	eta.maxBatchesPerSecond = r.run.Delivery.batchRateMax
	return eta
}

func writeTrialPace(
	w io.Writer,
	progress *tth2.TrialProgress,
	eta trialETA,
) {
	if progress == nil {
		return
	}
	pace := eta.pace(*progress).display()
	emitf(w, "  %s trials/%s  %s req/%s",
		pace.work, pace.unit, pace.requests, pace.unit)
	if estimate := eta.compact(*progress); estimate != "" {
		emitf(w, "  eta ~%s", estimate)
	}
}

func (r *textRaceReporter) Summary(rec SummaryRecord) error {
	if rec.MeasuredWidth == 0 {
		rec.MeasuredWidth = r.run.TrialBatchWidth
	}
	if rec.PrimingWidth == 0 {
		rec.PrimingWidth = r.run.WarmupBatchWidth
	}
	if err := validateRaceExecution(
		rec.Execution, rec.MeasuredWidth, rec.PrimingWidth); err != nil {
		return err
	}
	var b bytes.Buffer
	if r.trials == 0 && rec.Completion.State == completionFailed {
		emitln(&b, "No trial was produced before acquisition failed.")
	}
	writeHeading(&b, "Execution")
	trials := rec.Execution.Trials
	if trials.Maximum == nil {
		emitf(&b, "Trials: %d attempted; %d rank-complete; %d incomplete; "+
			"unlimited maximum.\n", trials.Attempted, trials.RankComplete,
			trials.Incomplete)
	} else {
		emitf(&b, "Trials: %d/%d attempted; %d rank-complete; %d incomplete; "+
			"%d unattempted.\n", trials.Attempted, *trials.Maximum,
			trials.RankComplete, trials.Incomplete, *trials.Unattempted)
	}
	priming := rec.Execution.Priming
	if priming.Batches.Attempted > 0 {
		emitf(&b, "Priming: %d attempted; %d rank-complete; %d incomplete.\n",
			priming.Batches.Attempted, priming.Batches.RankComplete,
			priming.Batches.Incomplete)
		if priming.Batches.Incomplete > 0 {
			writeWrappedASCII(&b, "Caution: ",
				"incomplete priming may leave cold-state effects in measured trials.")
		}
	}
	connections := rec.Execution.Connections
	emitf(&b, "Connections: %d planned; %d observed; %d replacements.\n",
		connections.Planned, connections.Observed, connections.Replacements)
	operations := rec.Execution.RequestOperations
	emitf(&b, "Request operations: %d attempted; %d rank-complete; %d retained.\n",
		operations.Attempted, operations.RankComplete, operations.Retained)
	var resets []string
	for id, count := range rec.ResetCount {
		if count > 0 {
			resets = append(resets,
				fmt.Sprintf("%s x %d", r.reference(id), count))
		}
	}
	if len(resets) > 0 {
		emitf(&b, "Resets: %s\n", strings.Join(resets, ", "))
	}
	completion := normaliseCompletion(rec.Completion)
	if completion.State != completionComplete {
		emitf(&b, "Completion: %s.\n", completion.State)
	}
	if completion.Detail != "" {
		emitf(&b, "Error: %s\n", curlblocks.DisplayText(completion.Detail))
	}
	_, err := r.w.Write(b.Bytes())
	return err
}

type raceRow struct {
	rank, position       string
	reference            string
	response             string
	status               int
	bodyCapturedBytes    int
	extractionIncomplete bool
	extracts             []ExtractRecord
}

func (r *textRaceReporter) writeTrialTable(
	w io.Writer,
	rec TrialRecord,
) error {
	rows := r.trialRows(rec)
	lw := len("request")
	for _, row := range rows {
		lw = max(lw, len(row.reference))
	}
	emitf(w, "  rank  pos  %-*s  response\n", lw, "request")
	for _, row := range rows {
		emitf(w, "  %4s  %3s  %-*s  %s\n",
			row.rank, row.position, lw, row.reference, row.response)
		writeRowExtractionScope(w, row)
		if err := r.writeRowExtracts(w, row); err != nil {
			return err
		}
	}

	counts := map[int]int{}
	for _, row := range rows {
		if row.status > 0 {
			counts[row.status]++
		}
	}
	statuses := make([]int, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, status)
	}
	slices.Sort(statuses)
	parts := make([]string, len(statuses))
	for i, status := range statuses {
		parts[i] = fmt.Sprintf("%d x %d", status, counts[status])
	}
	if len(parts) == 0 {
		emitln(w, "Status: no responses")
	} else {
		emitf(w, "Status: %s\n", strings.Join(parts, ", "))
	}
	if err := r.writeExtractSummaries(w, rec); err != nil {
		return err
	}
	return nil
}

func (r *textRaceReporter) trialRows(rec TrialRecord) []raceRow {
	trial := rec.Trial
	n := len(trial.Arrangement)
	positions := make([]int, n)
	for position, localID := range trial.Arrangement {
		if localID >= 0 && localID < n {
			positions[localID] = position
		}
	}
	rows := make([]raceRow, 0, n)
	seen := make([]bool, n)
	for _, localID := range trial.ArrivalOrder {
		if localID < 0 || localID >= len(trial.Results) {
			continue
		}
		seen[localID] = true
		rows = append(rows, r.resultRow(
			rec.RequestOffset+localID,
			positions[localID],
			&trial.Results[localID],
			outcomeAt(rec.Outcomes, localID),
		))
	}
	for localID := range n {
		if seen[localID] {
			continue
		}
		var result *tth2.Result
		if localID < len(trial.Results) {
			result = &trial.Results[localID]
		}
		rows = append(rows, r.resultRow(
			rec.RequestOffset+localID,
			positions[localID],
			result,
			outcomeAt(rec.Outcomes, localID),
		))
	}
	return rows
}

func (r *textRaceReporter) resultRow(
	id, position int,
	result *tth2.Result,
	outcome *OutcomeRecord,
) raceRow {
	row := raceRow{
		rank:      "-",
		position:  strconv.Itoa(position),
		reference: r.reference(id),
		response:  "no response",
	}
	if result == nil {
		return row
	}
	if outcome != nil && outcome.Response != nil {
		row.extracts = outcome.Response.Extracts
		row.bodyCapturedBytes = outcome.Response.BodyCapturedBytes
		for _, extract := range row.extracts {
			if !extract.Complete {
				row.extractionIncomplete = true
				break
			}
		}
	}
	if result.ArrivalRank >= 0 {
		row.rank = strconv.Itoa(result.ArrivalRank)
	}
	if result.Arrived() {
		row.status = result.Response.StatusCode
		row.response = fmt.Sprintf("%d  %d B",
			result.Response.StatusCode, result.Response.Body.BytesReceived)
		if declared := result.Response.ContentLength; declared >= 0 &&
			declared != result.Response.Body.BytesReceived {
			row.response += fmt.Sprintf(" (Content-Length: %d)", declared)
		}
	}
	if result.Reset != nil {
		reset := "reset " + result.Reset.Code.String()
		if result.Response == nil {
			row.response = reset
		} else {
			row.response += "; " + reset
		}
	}
	return row
}

func writeRowExtractionScope(w io.Writer, row raceRow) {
	if row.extractionIncomplete {
		emitf(w, "        extraction: searched %d B prefix only\n",
			row.bodyCapturedBytes)
	}
}

func outcomeAt(outcomes []OutcomeRecord, index int) *OutcomeRecord {
	if index < 0 || index >= len(outcomes) {
		return nil
	}
	return &outcomes[index]
}

func (r *textRaceReporter) writeRowExtracts(
	w io.Writer,
	row raceRow,
) error {
	for i, extract := range row.extracts {
		if err := r.writeRowExtract(w, i, extract); err != nil {
			return err
		}
	}
	return nil
}

func (r *textRaceReporter) writeRowExtract(
	w io.Writer,
	pattern int,
	extract ExtractRecord,
) error {
	fields := r.extractFields(pattern, extract)
	if len(fields) == 0 {
		values := make([]string, 0, len(extract.Matches))
		for _, match := range extract.Matches {
			if match.Value == nil {
				continue
			}
			value, err := displayEncodedData(*match.Value)
			if err != nil {
				return fmt.Errorf("render extract %d: %w", pattern, err)
			}
			values = append(values, value)
		}
		if len(values) > 0 {
			writeWrappedASCII(w, "        "+r.extractLabel(pattern, "")+": ",
				strings.Join(values, ", "))
		}
		return nil
	}
	if len(fields) == 1 {
		values := make([]string, 0, len(extract.Matches))
		for _, match := range extract.Matches {
			capture := extractCapture(match, fields[0])
			if capture == nil || !capture.Participated || capture.Value == nil {
				continue
			}
			value, err := displayEncodedData(*capture.Value)
			if err != nil {
				return fmt.Errorf("render extract %d field %q: %w",
					pattern, fields[0], err)
			}
			values = append(values, value)
		}
		if len(values) > 0 {
			writeWrappedASCII(w,
				"        "+r.extractLabel(pattern, fields[0])+": ",
				strings.Join(values, ", "))
		}
		return nil
	}
	for _, match := range extract.Matches {
		parts := make([]string, 0, len(fields))
		for _, field := range fields {
			capture := extractCapture(match, field)
			if capture == nil || !capture.Participated || capture.Value == nil {
				continue
			}
			value, err := displayEncodedData(*capture.Value)
			if err != nil {
				return fmt.Errorf("render extract %d field %q: %w",
					pattern, field, err)
			}
			parts = append(parts, field+"="+value)
		}
		if len(parts) > 0 {
			writeWrappedASCII(w, "        "+r.extractLabel(pattern, "")+": ",
				strings.Join(parts, ", "))
		}
	}
	return nil
}

func (r *textRaceReporter) writeExtractSummaries(
	w io.Writer,
	rec TrialRecord,
) error {
	for pattern := range r.run.ExtractRegex {
		fields := r.extractFields(pattern, ExtractRecord{})
		if len(fields) == 0 {
			if err := r.writeExtractSummary(w, rec, pattern, ""); err != nil {
				return err
			}
			continue
		}
		for _, field := range fields {
			if err := r.writeExtractSummary(w, rec, pattern, field); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *textRaceReporter) writeExtractSummary(
	w io.Writer,
	rec TrialRecord,
	pattern int,
	field string,
) error {
	counts := map[string]int{}
	responses := 0
	matched := 0
	multiple := false
	prefixOnly := 0
	for _, outcome := range rec.Outcomes {
		if outcome.Response == nil {
			continue
		}
		responses++
		if pattern >= len(outcome.Response.Extracts) {
			continue
		}
		extract := outcome.Response.Extracts[pattern]
		if !extract.Complete {
			prefixOnly++
		}
		seen := map[string]struct{}{}
		for _, match := range extract.Matches {
			var encoded *EncodedData
			if field == "" {
				encoded = match.Value
			} else if capture := extractCapture(match, field); capture != nil &&
				capture.Participated {
				encoded = capture.Value
			}
			if encoded == nil {
				continue
			}
			value, err := decodeEncodedData(*encoded)
			if err != nil {
				return fmt.Errorf(
					"render extract %d field %q summary: %w",
					pattern, field, err)
			}
			seen[string(value)] = struct{}{}
		}
		if len(seen) > 0 {
			matched++
		}
		if len(seen) > 1 {
			multiple = true
		}
		for value := range seen {
			counts[value]++
		}
	}
	label := r.extractSummaryLabel(pattern, field)
	summary := extractionSummary(
		label, counts, matched, responses, multiple, field != "", prefixOnly)
	writeWrappedASCII(w, label+": ", summary)
	return nil
}

func (r *textRaceReporter) extractFields(
	pattern int,
	extract ExtractRecord,
) []string {
	if pattern >= 0 && pattern < len(r.run.ExtractFields) {
		return r.run.ExtractFields[pattern]
	}
	return extract.Fields
}

func (r *textRaceReporter) extractLabel(pattern int, field string) string {
	label := "extract"
	if len(r.run.ExtractRegex) > 1 {
		label += " " + strconv.Itoa(pattern)
	}
	if field != "" && field != "extract" {
		label += " " + field
	}
	return label
}

func (r *textRaceReporter) extractSummaryLabel(pattern int, field string) string {
	label := r.extractLabel(pattern, field)
	return strings.ToUpper(label[:1]) + label[1:]
}

func extractCapture(match ExtractMatch, name string) *ExtractCapture {
	for i := range match.Captures {
		if match.Captures[i].Name == name {
			return &match.Captures[i]
		}
	}
	return nil
}

func extractionSummary(
	label string,
	counts map[string]int,
	matched, responses int,
	multiple bool,
	namedField bool,
	prefixOnly int,
) string {
	values := make([]string, 0, len(counts))
	for value := range counts {
		values = append(values, value)
	}
	slices.Sort(values)
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = fmt.Sprintf("%s in %s", quoteDisplay([]byte(value)),
			countedNoun(counts[value], "response"))
	}
	coverageVerb := "matched"
	empty := "no matches"
	if namedField {
		coverageVerb = "present in"
		empty = "no values"
	}
	coverage := ""
	if matched != responses {
		coverage = fmt.Sprintf("; %s %d/%d responses", coverageVerb,
			matched, responses)
	}
	detail := strings.Join(parts, ", ") + coverage
	if len(values) == 0 {
		if prefixOnly > 0 {
			empty = "no observed " + strings.TrimPrefix(empty, "no ")
		}
		detail = fmt.Sprintf("%s in %s",
			empty, countedNoun(responses, "response"))
	}
	collapsed := len(values) > 8 ||
		len(label+": ")+len(detail) > 2*textWidth
	if collapsed {
		detail = fmt.Sprintf("%d distinct values; %s %d/%d responses",
			len(values), coverageVerb, matched, responses)
	}
	if multiple {
		detail += "; responses may contain multiple values"
	}
	if prefixOnly > 0 {
		detail += "; " + countedNoun(prefixOnly, "prefix-only search")
	}
	return detail
}

func displayEncodedData(data EncodedData) (string, error) {
	raw, err := decodeEncodedData(data)
	if err != nil {
		return "", err
	}
	return quoteDisplay(raw), nil
}

func decodeEncodedData(data EncodedData) ([]byte, error) {
	switch data.Encoding {
	case "utf-8":
		return []byte(data.Data), nil
	case "base64":
		raw, err := base64.StdEncoding.DecodeString(data.Data)
		if err != nil {
			return nil, fmt.Errorf("decode base64: %w", err)
		}
		return raw, nil
	default:
		return nil, fmt.Errorf("unknown encoding %q", data.Encoding)
	}
}

func quoteDisplay(raw []byte) string {
	return strconv.Quote(curlblocks.DisplayText(string(raw)))
}

func (r *textRaceReporter) reference(id int) string {
	if id >= 0 && id < len(r.refs) {
		return r.refs[id].Inline
	}
	return "#" + strconv.Itoa(id)
}

// raceReferenceWidth returns the room for a request reference in the trial
// table. The text-width layout reserves the other columns and a representative
// compact response.
func raceReferenceWidth() int {
	fixed := len("  rank  pos  ") + len("  response") + len("999  999 B")
	return max(3, textWidth-fixed)
}
