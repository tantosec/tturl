package main

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/timing"
)

type timeReporter interface {
	Run() error
	Observe(timing.Event) error
	Summary(timing.Result) error
}

type timeReport struct {
	report      *reportWriter
	cfg         timeCommandConfig
	requests    timeRequests
	resolved    *timing.Resolved
	info        buildinfo.Info
	args        []string
	stream      *structuredStream
	samples     map[int][]int64
	connections []any
	quality     map[int]*timeRequestQuality
	extraction  map[int][]timeExtractionSummary
}

func newTimeReporter(report *reportWriter, cfg timeCommandConfig, requests timeRequests,
	resolved *timing.Resolved, info buildinfo.Info, args []string,
) timeReporter {
	r := &timeReport{
		report:   report,
		cfg:      cfg,
		requests: requests,
		resolved: resolved,
		info:     info,
		args:     args,
		samples:  make(map[int][]int64),
		quality:  make(map[int]*timeRequestQuality),
	}
	if cfg.report.format == reportJSON {
		r.stream = newStructuredStream(report)
	}
	return r
}

func (r *timeReport) Run() error {
	if r.stream == nil {
		return reportTimePreview(r.report, r.cfg, r.requests, r.resolved)
	}
	all := append(append([]timeRequest(nil), r.requests.Measured...), r.requests.Priming...)
	run := map[string]any{
		"kind":          "run",
		"schema":        timeSchema.logical,
		"command":       "time",
		"tool":          makeStructuredTool(r.info),
		"argv":          wireStrings(r.args),
		"started_at":    time.Now().UTC().Format(time.RFC3339Nano),
		"request_count": len(all),
		"experiment":    timeExperiment(r.resolved),
		"extractors":    timeExtractors(r.cfg.capture.extracts),
	}
	if err := r.stream.Start(run, len(all)); err != nil {
		return err
	}
	for _, item := range all {
		request := timePreviewRequest(item)
		set := "measured"
		if item.Warmup {
			set = "priming"
		}
		record := map[string]any{
			"kind":                    "request",
			"request_id":              item.ID,
			"request_set":             set,
			"label":                   wireString(item.Label),
			"protocol":                timeProtocol(item.Protocol),
			"method":                  wireString(request.Method),
			"url":                     wireString(request.URL.String()),
			"headers":                 wireHeaderMap(request.Header),
			"pseudo_headers":          timePseudoHeaders(item),
			"length_mismatch":         item.DeclaredLength != nil && *item.DeclaredLength != int64(len(item.Body)),
			"body":                    timeEncodedBody(item.Body),
			"declared_content_length": item.DeclaredLength,
		}
		if err := r.stream.Request(record); err != nil {
			return err
		}
	}
	return nil
}

func timeExperiment(resolved *timing.Resolved) map[string]any {
	p := resolved.Plan
	measured := make([]int, len(resolved.Requests))
	for i, request := range resolved.Requests {
		measured[i] = request.ID
	}
	priming := make([]int, len(resolved.Priming))
	for i, request := range resolved.Priming {
		priming[i] = request.ID
	}
	pools := make([]any, 0, len(resolved.Pools))
	for _, pool := range resolved.Pools {
		pools = append(pools,
			map[string]any{
				"pool_id":             pool.ID,
				"scheme":              pool.Scheme,
				"host":                wireString(pool.Host),
				"port":                pool.Port,
				"dial_address":        wireString(pool.Address),
				"protocol":            pool.Protocol,
				"request_ids":         pool.RequestIDs,
				"priming_request_ids": pool.PrimingRequestIDs,
				"width":               pool.Width,
			})
	}
	return map[string]any{
		"requested_trials":                  timeTrialBound(resolved.RequestedTrials),
		"measured_request_ids":              measured,
		"priming_request_ids":               priming,
		"distinct_priming_set":              p.Priming != nil,
		"requested_connections":             p.Connections,
		"trials_unlimited":                  p.Trials == 0,
		"tls":                               timeTLSPolicy(p),
		"planned_trials":                    timeTrialBound(resolved.PlannedTrials),
		"width":                             resolved.Width,
		"destinations":                      resolved.Destinations,
		"workers":                           resolved.Workers,
		"connection_ceiling":                resolved.ConnectionCeiling,
		"connections_per_worker":            resolved.ConnectionsPerWorker,
		"effective_connections":             resolved.EffectiveConnections,
		"planned_request_operations":        timeTrialBound(resolved.PlannedRequestOperations),
		"arrangement":                       p.Arrangement,
		"warmup":                            p.Warmup,
		"warmup_width":                      resolved.WarmupWidth,
		"planned_initial_warmup_trials":     resolved.PlannedWarmupTrials,
		"planned_initial_warmup_operations": resolved.PlannedWarmupOperations,
		"synchronise":                       p.Synchronise,
		"single_record":                     p.SingleRecord,
		"last_byte_sync":                    p.LastByteSync,
		"release_delay_ns":                  int64(p.ReleaseDelay),
		"request_timeout_ns":                timeBound(int64(p.RequestTimeout)),
		"run_timeout_ns":                    timeBound(int64(p.RunTimeout)),
		"batch_rate_max_per_second":         timeRateBound(p.BatchRate),
		"request_rate_max_per_second":       timeRateBound(p.RequestRate),
		"receive_header_max_bytes":          p.ReceiveHeaderMax,
		"response_body_max_bytes":           timeBound(p.ResponseBodyMax),
		"capture_headers":                   p.Capture.Headers,
		"capture_body":                      p.Capture.Body,
		"capture_body_max_bytes":            timeCaptureBound(p.Capture),
		"pools":                             pools,
	}
}

func timeEncodedBody(body []byte) map[string]any {
	encoded := encodeData(body)
	return map[string]any{
		"bytes":    len(body),
		"sha256":   digestBase64(body),
		"encoding": encoded.Encoding,
		"data":     encodedString(encoded.Data),
	}
}

func timeOffset(offset timing.Offset) any {
	if !offset.Present {
		return nil
	}
	return offset.NS
}

func timeTiming(value timing.Timing) map[string]any {
	fields := map[string]timing.Offset{
		"initial_release_ns":        value.InitialRelease,
		"final_release_ns":          value.FinalRelease,
		"write_complete_ns":         value.WriteComplete,
		"first_response_headers_ns": value.FirstResponseHeaders,
		"final_response_headers_ns": value.FinalResponseHeaders,
		"response_complete_ns":      value.ResponseComplete,
		"duration_ns":               value.Duration,
	}
	result := make(map[string]any, len(fields)+1)
	reasons := make(map[string]wireString)
	for name, offset := range fields {
		result[name] = timeOffset(offset)
		if !offset.Present {
			reasons[name] = wireString(offset.Reason)
		}
	}
	result["missing_reasons"] = reasons
	return result
}

func timeFailure(failure *timing.Failure) any {
	if failure == nil {
		return nil
	}
	phase, code, message := failure.Phase, failure.Code, failure.Error()
	if phase == "" {
		phase = timing.PhaseRun
	}
	if code == "" {
		code = "operational"
	}
	if message == "" {
		message = fmt.Sprintf("%s: %s", phase, code)
	}
	return map[string]any{
		"phase": phase, "code": wireString(code), "message": wireString(message),
	}
}

func timeTrialBound(value uint64) any {
	if value == 0 {
		return nil
	}
	return value
}

func timeBound(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func timeRateBound(value float64) any {
	if value == 0 {
		return nil
	}
	return value
}

func timeCaptureBound(capture timing.Capture) any {
	if !capture.Body {
		return int64(0)
	}
	return timeBound(capture.BodyMax)
}

func timeCompletion(result timing.Result) map[string]any {
	failure := result.Failure
	if result.Completion == timing.CompletionStopped {
		failure = &timing.Failure{
			Phase: timing.PhaseRun, Code: "interrupted", Message: "User stopped unlimited run",
		}
	}
	return map[string]any{"state": result.Completion, "failure": timeFailure(failure)}
}

func timeOptional(value timing.OptionalInt) any {
	if !value.Present {
		return nil
	}
	return value.Value
}

func timeConnectionID(value uint64) any {
	if value == 0 {
		return nil
	}
	return value
}

func timeStarted(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func (r *timeReport) outcomes(values []timing.Outcome) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		response := value.Response
		var status any
		if response.Status != 0 {
			status = response.Status
		}
		observed := value.Timing.FirstResponseHeaders.Present || response.Status != 0 ||
			response.ReceivedBodyBytes > 0 || len(response.Headers) > 0 || len(response.Trailers) > 0 || response.Complete
		var digest any
		if observed && response.Digest != "" {
			digest = response.Digest
		}
		evidence := map[string]any{
			"status": status, "content_length": timeOptional(response.DeclaredContentLength),
			"received_body_bytes": response.ReceivedBodyBytes, "body_bytes": response.AcceptedBodyBytes, "sha256": digest,
			"length_mismatch": response.LengthMismatch, "complete": response.Complete,
			"reset": response.Reset, "truncated": response.Truncated,
			"headers": nil, "trailers": nil, "body": nil,
			"body_captured_bytes": int64(0), "body_captured_complete": false,
			"extracts": []jsonRaceExtract{},
		}
		if r.cfg.capture.headers && observed {
			evidence["headers"] = wireHeaderMap(response.Headers)
			evidence["trailers"] = wireHeaderMap(response.Trailers)
		}
		if r.cfg.capture.bodyBytes != 0 && observed {
			evidence["body"] = map[string]any{
				"encoding": "base64", "data": base64.StdEncoding.EncodeToString(response.Body),
			}
			evidence["body_captured_bytes"] = int64(len(response.Body))
			evidence["body_captured_complete"] = !response.CaptureTruncated
			evidence["extracts"] = timeExtracts(captureExtracts(response.Body,
				r.cfg.capture.extracts, !response.CaptureTruncated))
		}
		result = append(result,
			map[string]any{
				"request_id":     value.RequestID,
				"position":       value.Position,
				"pool_id":        value.PoolID,
				"worker_id":      value.WorkerID,
				"connection_id":  timeConnectionID(value.ConnectionID),
				"protocol":       value.Protocol,
				"attempted":      value.Attempted,
				"early_response": value.EarlyResponse,
				"timing":         timeTiming(value.Timing),
				"response":       evidence,
				"failure":        timeFailure(value.Failure),
			})
	}
	return result
}

func (r *timeReport) Observe(event timing.Event) error {
	if r.stream != nil && ((event.Trial != nil && event.Trial.StartedAt.IsZero()) ||
		(event.Warmup != nil && event.Warmup.StartedAt.IsZero())) {
		return fmt.Errorf("time evidence requires a calendar anchor")
	}

	if event.Connection != nil {
		if r.stream != nil {
			c := event.Connection
			r.connections = append(r.connections,
				map[string]any{
					"connection_id":       timeConnectionID(c.ID),
					"worker_id":           c.WorkerID,
					"pool_id":             c.PoolID,
					"slot":                c.Slot,
					"replacement_of":      timeConnectionID(c.ReplacementOf),
					"protocol":            c.Protocol,
					"negotiated_protocol": wireString(c.NegotiatedProtocol),
					"local_address":       wireString(c.LocalAddress),
					"remote_address":      wireString(c.RemoteAddress),
					"state":               wireString(c.State),
					"reason":              wireString(c.Reason),
					"failure":             timeFailure(c.Failure),
				})
		}
		return nil
	}
	if event.Phase != nil && r.stream != nil {
		p := event.Phase
		return r.evidence(map[string]any{
			"kind": "phase", "phase": p.Phase,
			"worker_id": p.WorkerID, "phase_index": p.PhaseIndex,
		})
	}
	if event.Trial != nil {
		t := event.Trial
		for _, outcome := range t.Outcomes {
			r.recordQuality(outcome)
			r.recordExtraction(outcome)
			if outcome.Timing.Duration.Present {
				r.samples[outcome.RequestID] = append(r.samples[outcome.RequestID], outcome.Timing.Duration.NS)
			}
		}
		if r.stream != nil {
			return r.evidence(map[string]any{
				"kind":                "trial",
				"trial_index":         t.Index,
				"worker_id":           t.WorkerID,
				"started_at":          timeStarted(t.StartedAt),
				"initial_gate_ns":     timeOffset(t.InitialGate),
				"initial_gate_reason": wireString(t.InitialGate.Reason),
				"final_gate_ns":       timeOffset(t.FinalGate),
				"final_gate_reason":   wireString(t.FinalGate.Reason),
				"order":               t.Order,
				"committed":           t.Committed,
				"offered_operations":  t.OfferedOperations,
				"complete":            t.Complete,
				"failure":             timeFailure(t.Failure),
				"outcomes":            r.outcomes(t.Outcomes),
			})
		}
	}
	if event.Warmup != nil && r.stream == nil && event.Warmup.Failure != nil {
		writeWrappedASCII(r.report, "Warmup failure: ",
			curlblocks.DisplayText(event.Warmup.Failure.Error()))
	}
	if event.Warmup != nil && r.stream != nil {
		w := event.Warmup
		return r.evidence(map[string]any{
			"kind":                "warmup",
			"worker_id":           w.WorkerID,
			"phase":               w.Phase,
			"phase_index":         w.PhaseIndex,
			"trial_index":         w.TrialIndex,
			"started_at":          timeStarted(w.StartedAt),
			"initial_gate_ns":     timeOffset(w.InitialGate),
			"initial_gate_reason": wireString(w.InitialGate.Reason),
			"final_gate_ns":       timeOffset(w.FinalGate),
			"final_gate_reason":   wireString(w.FinalGate.Reason),
			"order":               w.Order,
			"committed":           w.Committed,
			"offered_operations":  w.OfferedOperations,
			"complete":            w.Complete,
			"failure":             timeFailure(w.Failure),
			"outcomes":            r.outcomes(w.Outcomes),
		})
	}
	return r.report.Err()
}

func (r *timeReport) evidence(record map[string]any) error {
	record["connections"] = r.connections
	r.connections = nil
	return r.stream.Evidence(record)
}

func (r *timeReport) Summary(result timing.Result) error {
	if r.stream != nil && result.StartedAt.IsZero() {
		return fmt.Errorf("time result requires a calendar anchor")
	}
	if r.stream != nil {
		switch result.Completion {
		case timing.CompletionComplete, timing.CompletionStopped:
			if result.Failure != nil {
				return fmt.Errorf("successful time completion carries an operational failure")
			}
		case timing.CompletionFailed:
			if result.Failure == nil {
				return fmt.Errorf("failed time completion requires an operational failure")
			}
		default:
			return fmt.Errorf("time result requires a completion state")
		}
	}

	for _, actual := range result.Requests {
		quality := r.ensureQuality(actual.RequestID)
		quality.Attempted, quality.Unattempted = actual.Attempted, actual.Unattempted
	}
	summaries := make([]any, 0, len(r.requests.Measured))
	durations := make(map[int]timeDurationSummary, len(r.requests.Measured))
	for _, request := range r.requests.Measured {
		quality := r.ensureQuality(request.ID)
		if len(result.Requests) == 0 && r.resolved != nil && r.resolved.Plan.Trials > 0 {
			quality.Unattempted = 0
			if quality.Attempted < r.resolved.PlannedTrials {
				quality.Unattempted = r.resolved.PlannedTrials - quality.Attempted
			}
		}
		r.recordExtraction(timing.Outcome{RequestID: request.ID})
		duration := summariseTimeDurations(r.samples[request.ID])
		durations[request.ID] = duration
		summaries = append(summaries,
			map[string]any{
				"request_id": request.ID,
				"protocol":   timeProtocol(request.Protocol),
				"duration":   duration,
				"quality":    r.quality[request.ID],
				"extraction": r.extraction[request.ID],
			})
	}
	if r.stream != nil {
		return r.stream.Finish(map[string]any{
			"kind":        "result",
			"completion":  timeCompletion(result),
			"started_at":  timeStarted(result.StartedAt),
			"accounting":  timeAccounting(result.Accounting),
			"summaries":   summaries,
			"connections": r.connections,
		})
	}
	writeHeading(r.report, "Duration summaries")
	unit := timeDurationUnit(r.samples)
	emitln(r.report, "Initial release to valid final response headers.")
	emitln(r.report, "Population standard deviation; R7 quantiles; warmup excluded.")
	statistics := make(map[int][8]string, len(r.requests.Measured))
	integerWidth := 1
	for _, request := range r.requests.Measured {
		values := unit.statistics(durations[request.ID])
		statistics[request.ID] = values
		for _, value := range values[:6] {
			integerWidth = max(integerWidth, timeTextIntegerWidth(value))
		}
	}
	for _, request := range r.requests.Measured {
		s := durations[request.ID]
		emitln(r.report)
		writeWrappedASCII(r.report, fmt.Sprintf("Request %d: ", request.ID),
			curlblocks.DisplayText(request.Label)+fmt.Sprintf(" (%s), n=%d", timeProtocolName(request.Protocol), s.Count))
		values := statistics[request.ID]
		for index, value := range values[:6] {
			values[index] = strings.Repeat(" ", integerWidth-timeTextIntegerWidth(value)) + value
		}
		rows := []string{
			fmt.Sprintf("  %-3s %s   %-3s %s   %-3s %s", "min", values[0], "p50", values[1], "p90", values[2]),
			fmt.Sprintf("  %-3s %s   %-3s %s   %-3s %s", "p95", values[3], "p99", values[4], "max", values[5]),
			fmt.Sprintf("  mean %s  standard deviation %s", values[6], values[7]),
		}
		for _, row := range rows {
			emitln(r.report, strings.TrimRight(row, " "))
		}
		q := r.ensureQuality(request.ID)
		emitf(r.report, "  %d attempted; %d unattempted\n", q.Attempted, q.Unattempted)
		statuses := make([]int, 0, len(q.Status))
		for status := range q.Status {
			statuses = append(statuses, status)
		}
		slices.Sort(statuses)
		for _, status := range statuses {
			emitf(r.report, "  HTTP %d: %d\n", status, q.Status[status])
		}
		failures := make([]string, 0, len(q.Failures))
		for failure := range q.Failures {
			failures = append(failures, failure)
		}
		slices.Sort(failures)
		for _, failure := range failures {
			writeWrappedASCII(r.report,
				"  ",
				curlblocks.DisplayText(failure)+": "+strconv.FormatUint(q.Failures[failure],
					10))
		}

		emitf(r.report, "  early responses: %d; resets: %d; incomplete responses: %d\n", q.Early, q.Reset, q.Incomplete)
		for _, extract := range r.extraction[request.ID] {
			emitf(r.report,
				"  Extractor %d: %d responses, %d complete captures\n",
				extract.ExtractorID,
				extract.Responses,
				extract.CompleteResponses)
			emitf(r.report, "    %d matched responses; %d matches\n", extract.MatchedResponses, extract.Matches)
			for _, group := range extract.NamedGroups {
				writeWrappedASCII(r.report,
					"    ",
					curlblocks.DisplayText(string(group.Name))+": "+strconv.FormatUint(group.Participations,
						10)+" participations")
			}
		}

	}
	writeHeading(r.report, "Execution")
	a := result.Accounting
	emitf(r.report, "Completion: %s\n", result.Completion)
	writeWrappedASCII(r.report,
		"Trials: ",
		fmt.Sprintf("%d attempted, %d complete, %d incomplete, %d unattempted",
			a.TrialsAttempted,
			a.TrialsComplete,
			a.TrialsIncomplete,
			a.TrialsUnattempted))
	writeWrappedASCII(r.report,
		"Request operations: ",
		fmt.Sprintf("%d attempted, %d complete, %d failed, %d unattempted",
			a.RequestOperationsAttempted,
			a.RequestOperationsComplete,
			a.RequestOperationsFailed,
			a.RequestOperationsUnattempted))
	writeWrappedASCII(r.report,
		"Warmup trials: ",
		fmt.Sprintf("%d attempted, %d complete, %d incomplete",
			a.WarmupTrials,
			a.WarmupTrialsComplete,
			a.WarmupTrialsIncomplete))
	emitf(r.report, "Warmup phases: %d initial trials; %d replacement trials.\n",
		a.InitialWarmupTrials, a.ReplacementWarmupTrials)
	writeWrappedASCII(r.report,
		"Warmup operations: ",
		fmt.Sprintf("%d attempted, %d complete, %d failed, %d unattempted",
			a.WarmupOperationsAttempted,
			a.WarmupOperationsComplete,
			a.WarmupOperationsFailed, a.WarmupOperationsUnattempted))
	writeWrappedASCII(r.report,
		"Connections: ",
		fmt.Sprintf("%d acquired, %d retired, %d replaced; %d acquisition failures",
			a.ConnectionsAcquired,
			a.ConnectionsRetired,
			a.ConnectionsReplaced,
			a.AcquisitionFailures))

	if result.Failure != nil {
		writeWrappedASCII(r.report, "", curlblocks.DisplayText(result.Failure.Error()))
	}
	return r.report.Err()
}

type timeTextUnit struct {
	name    string
	divisor int64
}

// Duration location determines the common scale. Dispersion and zero-valued
// samples do not make otherwise millisecond-scale durations use smaller units.
func timeDurationUnit(samples map[int][]int64) timeTextUnit {
	var smallest int64
	for _, values := range samples {
		for _, value := range values {
			if value > 0 && (smallest == 0 || value < smallest) {
				smallest = value
			}
		}
	}
	for _, unit := range []timeTextUnit{{"s", 1e9}, {"ms", 1e6}, {"us", 1e3}} {
		if smallest >= unit.divisor {
			return unit
		}
	}
	return timeTextUnit{"ns", 1}
}

func (unit timeTextUnit) statistics(summary timeDurationSummary) [8]string {
	return [8]string{
		unit.integer(summary.Min), unit.stat(summary.P50), unit.stat(summary.P90),
		unit.stat(summary.P95), unit.stat(summary.P99), unit.integer(summary.Max),
		unit.stat(summary.Mean), unit.stat(summary.SD),
	}
}

// value truncates fixed decimal text toward zero and removes insignificant
// fractional zeros. Unit suffixes immediately follow the number.
func (unit timeTextUnit) value(text string) string {
	whole, fraction, _ := strings.Cut(text, ".")
	fraction = strings.TrimRight(fraction[:min(3, len(fraction))], "0")
	if whole == "-0" && fraction == "" {
		whole = "0"
	}
	decimal := ""
	if fraction != "" {
		decimal = "." + fraction
	}
	return whole + decimal + unit.name
}

func (unit timeTextUnit) stat(value *float64) string {
	if value == nil {
		return "-"
	}
	return unit.value(strconv.FormatFloat(*value/float64(unit.divisor), 'f', -1, 64))
}

func (unit timeTextUnit) integer(value *int64) string {
	if value == nil {
		return "-"
	}
	whole := strconv.FormatInt(*value/unit.divisor, 10)
	remainder := *value % unit.divisor
	if remainder == 0 {
		return unit.value(whole)
	}
	if *value < 0 && whole == "0" {
		whole = "-0"
	}
	// A remainder's magnitude is smaller than the positive unit divisor.
	if remainder < 0 {
		remainder = -remainder
	}
	places := len(strconv.FormatInt(unit.divisor, 10)) - 1
	return unit.value(whole + "." + fmt.Sprintf("%0*d", places, remainder))
}

func timeTextIntegerWidth(value string) int {
	for index, char := range value {
		if char != '-' && (char < '0' || char > '9') {
			return index
		}
	}
	return len(value)
}

func reportTimePreview(report *reportWriter,
	cfg timeCommandConfig,
	requests timeRequests,
	resolved *timing.Resolved,
) error {
	writeBanner(report, toolName+" time")
	if cfg.dryRun {
		writeWrappedASCII(report, "Preview: ",
			"network-free dry run; unresolved delivery facts require execution; not an execution record.")
	}
	writeHeading(report, "Requests")
	refs := echoTimeRequests(report, requests, cfg.verbose)
	protocols := make([]string, 0, len(requests.Measured)+len(requests.Priming))
	hasHTTP11 := false
	for _, items := range [][]timeRequest{requests.Measured, requests.Priming} {
		for _, item := range items {
			label := curlblocks.DisplayText(refs.refs[len(protocols)].Inline)
			protocols = append(protocols, label+" "+timeProtocolName(item.Protocol))
			hasHTTP11 = hasHTTP11 || item.Protocol == curlblocks.HTTP11
		}
	}
	if hasHTTP11 {
		writeWrappedASCII(report, "Protocols: ", strings.Join(protocols, "; ")+".")
	}
	writeHeading(report, "Experiment")
	emitf(report,
		"Width: %d; connection pools: %d; workers: %d.\nConnections: %d effective; ceiling: %d.\n",
		resolved.Width,
		resolved.Destinations,
		resolved.Workers,
		resolved.EffectiveConnections,
		resolved.ConnectionCeiling)
	requested := strconv.FormatUint(resolved.RequestedTrials, 10)
	planned := strconv.FormatUint(resolved.PlannedTrials, 10)
	if resolved.Plan.Trials == 0 {
		requested, planned = "unlimited", "unlimited"
	}
	operations := strconv.FormatUint(resolved.PlannedRequestOperations, 10)
	if resolved.Plan.Trials == 0 {
		operations = "unlimited"
	}
	writeWrappedASCII(report, "Trials: ", fmt.Sprintf("%s x %d %s = %s request operations.",
		planned, resolved.Width, plural(resolved.Width), operations))
	if requested != planned {
		emitf(report, "Trial allocation: %s requested, %s planned.\n", requested, planned)
	}
	emitf(report, "Arrangement: %s.\n", resolved.Plan.Arrangement)
	if resolved.Plan.Synchronise {
		writeWrappedASCII(report, "Execution: ", "Each trial uses one connection per request and common release gates.")
	} else {
		writeWrappedASCII(report, "Execution: ",
			"Requests execute sequentially within each trial; workers may run separate trials concurrently.")
	}
	emitf(report, "Warmup: %d %s per worker; width: %d.\n",
		resolved.Plan.Warmup, countedPlural(resolved.Plan.Warmup, "trial"), resolved.WarmupWidth)
	writeWrappedASCII(report, "Planned initial warmup: ",
		fmt.Sprintf("%d trials; %d request operations.",
			resolved.PlannedWarmupTrials, resolved.PlannedWarmupOperations))
	writeWrappedASCII(report, "Lifecycle: ", "Acquire all initial connections, warm every worker, then measure.")
	writeWrappedASCII(report, "Replacement warmup: ",
		"additional worker-local trials; observed costs appear in execution accounting.")

	emitf(report,
		"Synchronise: %t; single record: %t; last-byte sync: %t.\n",
		cfg.synchronise,
		cfg.singleRecord,
		cfg.lastByte)
	emitf(report,
		"Release delay: %s; request timeout: %s; run timeout: %s.\n",
		formatDurationASCII(cfg.releaseDelay),
		timeTextTimeout(cfg.requestTimeout),
		timeTextTimeout(cfg.runTimeout))
	emitf(report,
		"Trial start limits: batch %s; request operations %s.\n",
		timeTextRate(cfg.batchRate),
		timeTextRate(cfg.requestRate))
	emitf(report,
		"Response limits: body %s; headers %s.\n",
		timeTextBytes(resolved.Plan.ResponseBodyMax),
		timeTextBytes(resolved.Plan.ReceiveHeaderMax))
	verify := true
	if resolved.Plan.TLSConfig != nil {
		verify = !resolved.Plan.TLSConfig.InsecureSkipVerify
	}
	reportTLSPlan(report, !verify)
	emitln(report, "Protocol fallback: disabled.")
	bodyCapture := "disabled"
	if resolved.Plan.Capture.Body {
		bodyCapture = timeTextBytes(resolved.Plan.Capture.BodyMax)
	}
	emitf(report, "Capture: headers %t; body %s.\n", resolved.Plan.Capture.Headers, bodyCapture)
	for i, extract := range cfg.capture.extracts {
		writeWrappedASCII(report, fmt.Sprintf("Extractor %d: ", i), curlblocks.DisplayText(extract.pattern))
	}
	if cfg.dryRun {
		emitln(report, "Assigned HTTP/2 encoding is checked during execution.")
	}
	return report.Err()
}

func timeProtocol(protocol curlblocks.Protocol) timing.Protocol {
	if protocol == curlblocks.HTTP11 {
		return timing.HTTP11
	}
	return timing.HTTP2
}

func timeAccounting(value timing.Accounting) map[string]uint64 {
	return map[string]uint64{
		"trials_attempted":               value.TrialsAttempted,
		"trials_complete":                value.TrialsComplete,
		"trials_incomplete":              value.TrialsIncomplete,
		"trials_unattempted":             value.TrialsUnattempted,
		"cycles_attempted":               value.CyclesAttempted,
		"cycles_complete":                value.CyclesComplete,
		"cycles_incomplete":              value.CyclesIncomplete,
		"cycles_unattempted":             value.CyclesUnattempted,
		"request_operations_attempted":   value.RequestOperationsAttempted,
		"request_operations_complete":    value.RequestOperationsComplete,
		"request_operations_failed":      value.RequestOperationsFailed,
		"request_operations_unattempted": value.RequestOperationsUnattempted,
		"warmup_trials":                  value.WarmupTrials,
		"warmup_trials_complete":         value.WarmupTrialsComplete,
		"warmup_trials_incomplete":       value.WarmupTrialsIncomplete,
		"warmup_operations_attempted":    value.WarmupOperationsAttempted,
		"warmup_operations_complete":     value.WarmupOperationsComplete,
		"warmup_operations_failed":       value.WarmupOperationsFailed,
		"warmup_operations_unattempted":  value.WarmupOperationsUnattempted,
		"initial_warmup_trials":          value.InitialWarmupTrials,
		"replacement_warmup_trials":      value.ReplacementWarmupTrials,
		"measured_commitments":           value.MeasuredCommitments,
		"warmup_commitments":             value.WarmupCommitments,
		"measured_offered_operations":    value.MeasuredOfferedOperations,
		"warmup_offered_operations":      value.WarmupOfferedOperations,
		"connections_acquired":           value.ConnectionsAcquired,
		"connections_retired":            value.ConnectionsRetired,
		"connections_replaced":           value.ConnectionsReplaced,
		"acquisition_failures":           value.AcquisitionFailures,
	}
}

type timeRequestQuality struct {
	Attempted   uint64            `json:"attempted"`
	Unattempted uint64            `json:"unattempted"`
	Early       uint64            `json:"early_responses"`
	Reset       uint64            `json:"resets"`
	Incomplete  uint64            `json:"incomplete_responses"`
	Status      map[int]uint64    `json:"statuses"`
	Failures    map[string]uint64 `json:"failures"`
}

func (r *timeReport) ensureQuality(id int) *timeRequestQuality {
	if r.quality == nil {
		r.quality = make(map[int]*timeRequestQuality)
	}
	if r.quality[id] == nil {
		r.quality[id] = &timeRequestQuality{
			Status:   make(map[int]uint64),
			Failures: make(map[string]uint64),
		}
	}
	return r.quality[id]
}

func (r *timeReport) recordQuality(outcome timing.Outcome) {
	q := r.ensureQuality(outcome.RequestID)
	if outcome.Attempted {
		q.Attempted++
	} else {
		q.Unattempted++
	}
	if outcome.EarlyResponse {
		q.Early++
	}
	if outcome.Response.Reset {
		q.Reset++
	}
	if !outcome.Response.Complete && (outcome.Attempted || outcome.Response.Status != 0 ||
		outcome.Response.Reset || outcome.Response.ReceivedBodyBytes != 0 ||
		outcome.Timing.FirstResponseHeaders.Present ||
		outcome.Timing.FinalResponseHeaders.Present || outcome.Timing.ResponseComplete.Present) {
		q.Incomplete++
	}
	if outcome.Response.Status != 0 {
		q.Status[outcome.Response.Status]++
	}
	if outcome.Failure != nil {
		q.Failures[outcome.Failure.Code]++
	}
}

func timeExtractors(patterns []extractionPattern) []jsonRaceExtractor {
	result := make([]jsonRaceExtractor, len(patterns))
	for i, pattern := range patterns {
		fields := make([]wireString, len(pattern.fields))
		for j, field := range pattern.fields {
			fields[j] = wireString(field.name)
		}
		selection := string(extractWholeMatch)
		if len(fields) > 0 {
			selection = string(extractNamedGroups)
		}
		result[i] = jsonRaceExtractor{
			ExtractorID: i,
			Pattern:     wireString(pattern.pattern),
			Selection:   selection,
			Fields:      fields,
		}
	}
	return result
}

func timeExtracts(extracts []ExtractRecord) []jsonRaceExtract {
	result := make([]jsonRaceExtract, len(extracts))
	for i, extract := range extracts {
		matches := make([]jsonRaceExtractMatch, len(extract.Matches))
		for j, match := range extract.Matches {
			captures := make([]jsonRaceExtractCapture, len(match.Captures))
			for k, capture := range match.Captures {
				captures[k] = makeRaceExtractCapture(capture)
			}
			projected := jsonRaceExtractMatch{Offset: match.Offset, Captures: captures}
			if match.Value != nil {
				encoding := match.Value.Encoding
				data := encodedString(match.Value.Data)
				projected.Encoding = &encoding
				projected.Data = &data
			}
			matches[j] = projected
		}
		result[i] = jsonRaceExtract{ExtractorID: i, Complete: extract.Complete, Matches: matches}
	}
	return result
}

type (
	timeExtractionGroup struct {
		Name           wireString `json:"name"`
		Participations uint64     `json:"participations"`
	}
	timeExtractionSummary struct {
		ExtractorID       int                   `json:"extractor_id"`
		Responses         uint64                `json:"responses"`
		CompleteResponses uint64                `json:"complete_responses"`
		MatchedResponses  uint64                `json:"matched_responses"`
		Matches           uint64                `json:"matches"`
		NamedGroups       []timeExtractionGroup `json:"named_groups"`
	}
)

func (r *timeReport) recordExtraction(outcome timing.Outcome) {
	if r.extraction == nil {
		r.extraction = make(map[int][]timeExtractionSummary)
	}
	summaries := r.extraction[outcome.RequestID]
	if summaries == nil {
		summaries = make([]timeExtractionSummary, len(r.cfg.capture.extracts))
		for i, pattern := range r.cfg.capture.extracts {
			summaries[i].ExtractorID = i
			summaries[i].NamedGroups = make([]timeExtractionGroup,
				len(pattern.fields))
			for j, field := range pattern.fields {
				summaries[i].NamedGroups[j].Name = wireString(field.name)
			}
		}
	}
	if outcome.Attempted {
		for i, extract := range captureExtracts(outcome.Response.Body,
			r.cfg.capture.extracts,
			!outcome.Response.CaptureTruncated) {
			summary := &summaries[i]
			summary.Responses++
			if extract.Complete {
				summary.CompleteResponses++
			}
			if len(extract.Matches) > 0 {
				summary.MatchedResponses++
			}
			for _, match := range extract.Matches {
				summary.Matches++
				for j, capture := range match.Captures {
					if capture.Participated {
						summary.NamedGroups[j].Participations++
					}
				}
			}
		}
	}
	r.extraction[outcome.RequestID] = summaries
}

func timePseudoHeaders(item timeRequest) map[string]wireString {
	fields := make(map[string]wireString)
	if item.Protocol == curlblocks.HTTP2 {
		authority := item.Request.Host
		if authority == "" {
			authority = item.Request.URL.Host
		}
		fields[":method"] = wireString(item.Request.Method)
		fields[":scheme"] = wireString(item.Request.URL.Scheme)
		fields[":authority"] = wireString(authority)
		fields[":path"] = wireString(item.Request.URL.RequestURI())
	}
	return fields
}

func timeTLSPolicy(plan timing.Plan) map[string]any {
	verify := true
	serverName := ""
	var minimum, maximum uint16
	keyLog := false
	if plan.TLSConfig != nil {
		verify = !plan.TLSConfig.InsecureSkipVerify
		serverName = plan.TLSConfig.ServerName
		minimum = plan.TLSConfig.MinVersion
		maximum = plan.TLSConfig.MaxVersion
		keyLog = plan.TLSConfig.KeyLogWriter != nil
	}
	return map[string]any{
		"verify_peer":               verify,
		"server_name":               wireString(serverName),
		"minimum_version":           max(minimum, tls.VersionTLS12),
		"requested_minimum_version": minimum,
		"maximum_version":           maximum,
		"key_log_enabled":           keyLog,
		"dynamic_record_sizing":     false,
		"protocol_fallback":         false,
	}
}

func timeTextTimeout(value time.Duration) string {
	if value == 0 {
		return unlimitedFlagValue
	}
	return formatDurationASCII(value)
}

func timeTextRate(value float64) string {
	if value == 0 {
		return unlimitedFlagValue
	}
	return strconv.FormatFloat(value, 'g', 6, 64) + "/s"
}

func timeTextBytes(value int64) string {
	if value < 0 {
		return unlimitedFlagValue
	}
	return formatResponseBodyMax(value)
}

func timeProtocolName(value curlblocks.Protocol) string {
	if value == curlblocks.HTTP11 {
		return "HTTP/1.1"
	}
	return "HTTP/2"
}
