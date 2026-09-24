package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/tth2"
)

//go:embed doc/schemas/tturl-common-v1.schema.json
var commonSchemaDocument []byte

var commonSchema = schemaDescriptor{
	logical:  "",
	filename: "tturl-common-v1.schema.json",
	document: commonSchemaDocument,
}

// Current protocol projections describe identical semantics across commands.
// Command-owned extensions compose these explicit fragments.
type structuredTool struct {
	Version  wireString  `json:"version"`
	Revision *wireString `json:"revision"`
	Modified *bool       `json:"modified"`
}

func makeStructuredTool(info buildIdentity) structuredTool {
	tool := structuredTool{Version: wireString(info.Version())}
	if info.Revision() != "" {
		revision := wireString(info.Revision())
		tool.Revision = &revision
	}
	switch info.TreeState() {
	case buildinfo.TreeClean:
		modified := false
		tool.Modified = &modified
	case buildinfo.TreeDirty:
		modified := true
		tool.Modified = &modified
	}
	return tool
}

type structuredTarget struct {
	DialAddress wireString  `json:"dial_address"`
	Origin      *wireString `json:"origin"`
}

type structuredRunEnvelope struct {
	Kind         string           `json:"kind"`
	Schema       string           `json:"schema"`
	Command      string           `json:"command"`
	Tool         structuredTool   `json:"tool"`
	Argv         []wireString     `json:"argv"`
	StartedAt    wireString       `json:"started_at"`
	Target       structuredTarget `json:"target"`
	RequestCount int              `json:"request_count"`
}

func makeStructuredRunEnvelope(schema, command string, info buildIdentity, argv []string,
	startedAt time.Time, dialAddress string, origin *string, requestCount int,
) structuredRunEnvelope {
	return structuredRunEnvelope{
		Kind:      "run",
		Schema:    schema,
		Command:   command,
		Tool:      makeStructuredTool(info),
		Argv:      wireStrings(argv),
		StartedAt: wireString(startedAt.UTC().Format(time.RFC3339Nano)),
		Target: structuredTarget{
			DialAddress: wireString(dialAddress),
			Origin:      wireStringPointer(origin),
		},
		RequestCount: requestCount,
	}
}

type structuredFitMaximum struct {
	Mode        string `json:"mode"`
	Connections *int   `json:"connections,omitempty"`
}

type structuredSelection struct {
	Mode                 string                `json:"mode"`
	RequestedConnections *int                  `json:"requested_connections,omitempty"`
	State                string                `json:"state,omitempty"`
	Maximum              *structuredFitMaximum `json:"maximum,omitempty"`
}

type structuredRateLimits struct {
	BatchMaxPerSecond        *float64 `json:"batch_max_per_second"`
	RequestMaxPerSecond      *float64 `json:"request_max_per_second"`
	EffectiveBatchIntervalNS int64    `json:"effective_batch_interval_ns"`
}

type structuredPathFit struct {
	RTTSamplesNS      []int64 `json:"rtt_samples_ns"`
	RTTBasisNS        int64   `json:"rtt_basis_ns"`
	FittedConnections int     `json:"fitted_connections"`
	Capped            bool    `json:"capped"`
}

type structuredDelivery struct {
	Selection              structuredSelection   `json:"selection"`
	ConnectionCeiling      int                   `json:"connection_ceiling"`
	ConnectionLimit        int                   `json:"connection_limit"`
	PeerStreamLimitIgnored bool                  `json:"peer_stream_limit_ignored"`
	RateLimits             *structuredRateLimits `json:"rate_limits,omitempty"`
	PathFit                *structuredPathFit    `json:"path_fit,omitempty"`
}

func makeStructuredDelivery(plan deliveryPlan) (structuredDelivery, error) {
	if plan.preview ||
		plan.pathFitUnresolved ||
		plan.connectionLimit < 1 ||
		plan.connectionCeiling < plan.connectionLimit {
		return structuredDelivery{}, fmt.Errorf("structured delivery requires resolved positive capacity")
	}
	delivery := structuredDelivery{
		ConnectionCeiling:      plan.connectionCeiling,
		ConnectionLimit:        plan.connectionLimit,
		PeerStreamLimitIgnored: plan.peerStreamLimitIgnored,
	}
	switch plan.connectionSelection {
	case connectionDefault, "":
		delivery.Selection.Mode = "default"
	case connectionExplicit:
		delivery.Selection = structuredSelection{
			Mode:                 "fixed",
			RequestedConnections: positiveIntPointer(plan.connectionCeiling),
		}
	case connectionPathPing, connectionPathPingCapped:
		delivery.Selection.Mode = "path_fit"
		delivery.Selection.State = "fitted"
		if len(plan.pathRTTSamples) != 3 || plan.pathRTTBasis <= 0 || plan.pathFittedConnections < 1 {
			return structuredDelivery{}, fmt.Errorf("structured path fitting requires three RTT observations and a fitted count")
		}
		samples := make([]int64, 3)
		for i, rtt := range plan.pathRTTSamples {
			if rtt <= 0 {
				return structuredDelivery{}, fmt.Errorf("structured RTT samples must be positive")
			}
			samples[i] = rtt.Nanoseconds()
		}
		delivery.PathFit = &structuredPathFit{
			RTTSamplesNS:      samples,
			RTTBasisNS:        plan.pathRTTBasis.Nanoseconds(),
			FittedConnections: plan.pathFittedConnections,
			Capped:            plan.pathFitCapped,
		}
	default:
		return structuredDelivery{}, fmt.Errorf("structured delivery has unknown selection")
	}
	if plan.pathFitInactive {
		delivery.Selection = structuredSelection{Mode: "path_fit", State: "skipped_single_trial"}
	}
	if delivery.Selection.Mode == "path_fit" {
		maximum := structuredFitMaximum{Mode: "automatic_review"}
		if plan.parallelFitSet {
			if plan.parallelFitMax < 0 {
				maximum.Mode = "unlimited"
			} else if plan.parallelFitMax > 0 {
				maximum.Mode = "bounded"
				maximum.Connections = positiveIntPointer(plan.parallelFitMax)
			} else {
				return structuredDelivery{}, fmt.Errorf("structured fit maximum must be positive or unlimited")
			}
		}
		delivery.Selection.Maximum = &maximum
	}
	if plan.batchRateMax > 0 || plan.requestRateMax > 0 {
		if plan.effectiveBatchInterval <= 0 {
			return structuredDelivery{}, fmt.Errorf("structured pacing requires a positive measured interval")
		}
		delivery.RateLimits = &structuredRateLimits{
			BatchMaxPerSecond:        positiveFloatPointer(plan.batchRateMax),
			RequestMaxPerSecond:      positiveFloatPointer(plan.requestRateMax),
			EffectiveBatchIntervalNS: plan.effectiveBatchInterval.Nanoseconds(),
		}
	}
	if delivery.Selection.Mode == "path_fit" && delivery.RateLimits == nil {
		return structuredDelivery{}, fmt.Errorf("structured fitting requires a finite rate")
	}
	return delivery, nil
}

type structuredResponseLimits struct {
	BodyBytes      *int64 `json:"body_bytes"`
	BatchTimeoutNS *int64 `json:"batch_timeout_ns"`
}

type structuredAcquisition struct {
	Delivery          structuredDelivery       `json:"delivery"`
	RunTimeoutNS      *int64                   `json:"run_timeout_ns"`
	ReleaseDelayNS    int64                    `json:"release_delay_ns"`
	BodyBytesWithheld int                      `json:"body_bytes_withheld"`
	TLSVerification   string                   `json:"tls_verification"`
	ResponseLimits    structuredResponseLimits `json:"response_limits"`
}

func makeStructuredAcquisition(delivery deliveryPlan, releaseDelay time.Duration, withheld int,
	insecure bool, timeout runTimeout, limits responseLimits,
) (structuredAcquisition, error) {
	resolved, err := makeStructuredDelivery(delivery)
	if err != nil {
		return structuredAcquisition{}, err
	}
	tlsMode := "enabled"
	if insecure {
		tlsMode = "disabled"
	}
	var bodyBytes *int64
	if limits.maxBodyBytes > 0 {
		n := limits.maxBodyBytes
		bodyBytes = &n
	}
	return structuredAcquisition{
		Delivery:          resolved,
		RunTimeoutNS:      positiveDurationNanos(time.Duration(timeout)),
		ReleaseDelayNS:    releaseDelay.Nanoseconds(),
		BodyBytesWithheld: withheld,
		TLSVerification:   tlsMode,
		ResponseLimits: structuredResponseLimits{
			BodyBytes:      bodyBytes,
			BatchTimeoutNS: positiveDurationNanos(limits.batchTimeout),
		},
	}, nil
}

type structuredCapture struct {
	Headers                 bool   `json:"headers"`
	BodyBytes               *int64 `json:"body_bytes"`
	DecodedHeaderLimitBytes *int64 `json:"decoded_header_limit_bytes,omitempty"`
}

func makeStructuredCapture(headers bool, bodyBytes, headerLimit int64) structuredCapture {
	capture := structuredCapture{Headers: headers}
	if bodyBytes >= 0 {
		capture.BodyBytes = &bodyBytes
	}
	if headers {
		capture.DecodedHeaderLimitBytes = &headerLimit
	}
	return capture
}

type structuredFailure struct {
	Code    string     `json:"code"`
	Message wireString `json:"message"`
}

// makeStructuredFailure classifies recognised causes before command fallbacks.
// The diagnostic preserves the full error chain; messages are not stable codes.
func makeStructuredFailure(err error, fallback string) *structuredFailure {
	if err == nil {
		return nil
	}
	code := fallback
	switch {
	case hasResponseBodyLimit(err):
		code = "response_body_limit"
	case hasBatchTimeout(err):
		code = "batch_timeout"
	case errors.Is(err, errRunTimeout):
		code = "run_timeout"
	case errors.Is(err, context.Canceled):
		code = "interrupted"
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline_exceeded"
	case hasConnectionError(err):
		code = "connection_error"
	case hasStreamError(err):
		code = "transport_error"
	}
	message := err.Error()
	if message == "" {
		message = "Acquisition operation failed."
	}
	return &structuredFailure{Code: code, Message: wireString(message)}
}

func hasResponseBodyLimit(err error) bool {
	_, ok := errors.AsType[*tth2.ResponseBodyLimitError](err)
	return ok
}

func hasBatchTimeout(err error) bool {
	_, ok := errors.AsType[*tth2.BatchTimeoutError](err)
	return ok
}

func hasConnectionError(err error) bool {
	_, ok := errors.AsType[*tth2.ConnectionError](err)
	return ok
}

func hasStreamError(err error) bool {
	_, ok := errors.AsType[*tth2.StreamError](err)
	return ok
}

type structuredCompletion struct {
	State   completionState    `json:"state"`
	Failure *structuredFailure `json:"failure"`
}

func makeStructuredCompletion(completion runCompletion) structuredCompletion {
	completion = normaliseCompletion(completion)
	result := structuredCompletion{State: completion.State}
	if completion.State == completionComplete {
		return result
	}
	if completion.cause != nil {
		result.Failure = makeStructuredFailure(completion.cause, string(completion.Code))
	} else {
		message := completion.Detail
		if message == "" {
			message = "Acquisition interrupted."
		}
		result.Failure = &structuredFailure{Code: string(completion.Code), Message: wireString(message)}
	}
	if result.Failure.Code == "interrupted" {
		result.State = completionStopped
	}
	return result
}

type structuredDispatch struct {
	HeadersSentAt   *wireString `json:"headers_sent_at"`
	BodyFinalisedAt *wireString `json:"body_finalised_at"`
}

func makeStructuredDispatch(dispatch tth2.DispatchTimes) structuredDispatch {
	return structuredDispatch{
		HeadersSentAt:   optionalWireTime(dispatch.HeadersSentAt),
		BodyFinalisedAt: optionalWireTime(dispatch.BodyFinalisedAt),
	}
}

type structuredRequestBody struct {
	Bytes    int           `json:"bytes"`
	SHA256   string        `json:"sha256"`
	Encoding string        `json:"encoding"`
	Data     encodedString `json:"data"`
}

type structuredRequest struct {
	Kind          string                 `json:"kind"`
	RequestID     int                    `json:"request_id"`
	Label         string                 `json:"label"`
	URL           wireString             `json:"url"`
	PseudoHeaders wireStringMap          `json:"pseudo_headers"`
	Headers       wireHeaderMap          `json:"headers"`
	Body          *structuredRequestBody `json:"body"`
}

func makeStructuredRequest(rec RequestRecord) structuredRequest {
	request := structuredRequest{
		Kind:          "request",
		RequestID:     rec.Reference.ID,
		Label:         rec.Reference.Label,
		URL:           wireString(rec.URL),
		PseudoHeaders: wireStringMap(rec.PseudoHeaders),
		Headers:       wireHeaderMap(rec.Headers),
	}
	if rec.Body != nil {
		request.Body = &structuredRequestBody{
			Bytes:    rec.Body.Bytes,
			SHA256:   rec.Body.SHA256,
			Encoding: rec.Body.Encoding,
			Data:     encodedString(rec.Body.Data),
		}
	}
	return request
}

type structuredResponse struct {
	Status               int                    `json:"status"`
	ContentLength        *int64                 `json:"content_length"`
	BodyBytes            int64                  `json:"body_bytes"`
	BodyCapturedBytes    int                    `json:"body_captured_bytes"`
	BodyCapturedComplete bool                   `json:"body_captured_complete"`
	SHA256               string                 `json:"sha256"`
	Headers              *wireHeaderMap         `json:"headers"`
	Trailers             *wireHeaderMap         `json:"trailers"`
	Body                 *structuredEncodedData `json:"body"`
}

func makeStructuredResponse(response ResponseRecord) structuredResponse {
	r := structuredResponse{
		Status:               response.Status,
		ContentLength:        response.ContentLength,
		BodyBytes:            response.BodyBytes,
		BodyCapturedBytes:    response.BodyCapturedBytes,
		BodyCapturedComplete: response.BodyCapturedComplete,
		SHA256:               response.SHA256,
	}
	if r.ContentLength != nil && *r.ContentLength < 0 {
		r.ContentLength = nil
	}
	if response.Headers != nil {
		headers := wireHeaderMap(response.Headers)
		r.Headers = &headers
	}
	if response.Trailers != nil {
		trailers := wireHeaderMap(response.Trailers)
		r.Trailers = &trailers
	}
	if response.Body != nil {
		r.Body = &structuredEncodedData{Encoding: response.Body.Encoding, Data: encodedString(response.Body.Data)}
	}
	return r
}

type structuredReset struct {
	Code       string `json:"code"`
	CodeNumber uint32 `json:"code_number"`
}

// structuredOutcome contains identity and ordinal facts. Each command supplies
// its response payload, extending the common response where applicable.
type structuredOutcome struct {
	RequestID int              `json:"request_id"`
	Position  int              `json:"position"`
	Rank      *int             `json:"rank"`
	StreamID  *uint32          `json:"stream_id"`
	Reset     *structuredReset `json:"reset"`
}

func makeStructuredOutcome(outcome OutcomeRecord) structuredOutcome {
	r := structuredOutcome{
		RequestID: outcome.Request,
		Position:  outcome.Position,
		Rank:      outcome.Rank,
		StreamID:  outcome.StreamID,
	}
	if outcome.Reset != nil {
		r.Reset = &structuredReset{Code: outcome.Reset.Code, CodeNumber: outcome.Reset.CodeNumber}
	}
	return r
}

type structuredResetCount struct {
	RequestID int `json:"request_id"`
	Count     int `json:"count"`
}
type structuredPadding struct {
	Position     int `json:"position"`
	QueryFields  int `json:"query_fields"`
	HeaderFields int `json:"header_fields"`
	BodyFields   int `json:"body_fields"`
}

func makeStructuredPadding(padding []PaddingRecord) []structuredPadding {
	result := make([]structuredPadding, len(padding))
	for i, p := range padding {
		result[i] = structuredPadding{
			Position:     p.Position,
			QueryFields:  p.URLParams,
			HeaderFields: p.Headers,
			BodyFields:   p.BodyParams,
		}
	}
	return result
}

type structuredWarmup struct {
	TrialsPerConnection int `json:"trials_per_connection"`
	BatchWidth          int `json:"batch_width"`
}

type buildIdentity interface {
	Version() string
	Revision() string
	TreeState() buildinfo.TreeState
}

func optionalWireTime(at time.Time) *wireString {
	if at.IsZero() {
		return nil
	}
	value := wireString(at.UTC().Format(time.RFC3339Nano))
	return &value
}

func positiveIntPointer(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}

func positiveFloatPointer(value float64) *float64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func positiveDurationNanos(value time.Duration) *int64 {
	if value <= 0 {
		return nil
	}
	nanos := value.Nanoseconds()
	return &nanos
}

// structuredStatusCount counts retained observations of one HTTP status.
type structuredStatusCount struct {
	Status int `json:"status"`
	Count  int `json:"count"`
}

type structuredExecutionConnections struct {
	Planned      int `json:"planned"`
	Observed     int `json:"observed"`
	Replacements int `json:"replacements"`
}
type structuredExecutionBatches struct {
	Attempted    int `json:"attempted"`
	RankComplete int `json:"rank_complete"`
	Incomplete   int `json:"incomplete"`
}
type structuredExecutionPrimingOperations struct {
	Attempted    int `json:"attempted"`
	RankComplete int `json:"rank_complete"`
}
type structuredExecutionPriming struct {
	Batches           structuredExecutionBatches           `json:"batches"`
	RequestOperations structuredExecutionPrimingOperations `json:"request_operations"`
}
type structuredExecutionRequestOperations struct {
	Attempted    int `json:"attempted"`
	RankComplete int `json:"rank_complete"`
	Retained     int `json:"retained"`
}
type structuredExecution struct {
	Connections       structuredExecutionConnections       `json:"connections"`
	Priming           structuredExecutionPriming           `json:"priming"`
	RequestOperations structuredExecutionRequestOperations `json:"request_operations"`
}

func makeStructuredExecution(value sharedExecution) structuredExecution {
	return structuredExecution{
		Connections: structuredExecutionConnections(value.Connections),
		Priming: structuredExecutionPriming{
			Batches:           structuredExecutionBatches(value.Priming.Batches),
			RequestOperations: structuredExecutionPrimingOperations(value.Priming.RequestOperations),
		},
		RequestOperations: structuredExecutionRequestOperations(value.RequestOperations),
	}
}

func (value structuredExecution) accounting() sharedExecution {
	return sharedExecution{
		Connections: executionConnections(value.Connections),
		Priming: executionPriming{
			Batches:           executionBatches(value.Priming.Batches),
			RequestOperations: executionPrimingOperations(value.Priming.RequestOperations),
		},
		RequestOperations: executionRequestOperations(value.RequestOperations),
	}
}

func structuredHeaderRecord(value *wireHeaderMap) HeaderRecord {
	if value == nil {
		return nil
	}
	record := make(HeaderRecord, len(*value))
	for name, values := range *value {
		record[name] = slices.Clone(values)
	}
	return record
}

type structuredEncodedData struct {
	Encoding string        `json:"encoding"`
	Data     encodedString `json:"data"`
}
