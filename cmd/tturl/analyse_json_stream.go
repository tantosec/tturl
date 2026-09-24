package main

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tantosec/tturl/internal/buildinfo"
)

//go:embed doc/schemas/tturl-analyse-v1.schema.json
var analyseSchemaDocument []byte

var analyseSchema = schemaDescriptor{
	logical:  "tturl.analyse/v1",
	filename: "tturl-analyse-v1.schema.json",
	document: analyseSchemaDocument,
}

const (
	analyseMethodVersion = "analyse-a-r-holm/4"
)

type analyseJSONStream struct{ stream *structuredStream }

type jsonAnalyseRunRecord struct {
	structuredRunEnvelope
	Plan        jsonAnalysePlan        `json:"plan"`
	Method      jsonAnalyseMethod      `json:"method"`
	Acquisition jsonAnalyseAcquisition `json:"acquisition"`
}

type jsonAnalysePlan struct {
	Work                  jsonAnalyseWork     `json:"work"`
	BatchWidth            int                 `json:"batch_width"`
	Arrangement           string              `json:"arrangement"`
	AssignmentUnit        string              `json:"assignment_unit"`
	Warmup                structuredWarmup    `json:"warmup"`
	Padding               []structuredPadding `json:"padding"`
	ModelledDefaultCycles int                 `json:"modelled_default_cycles"`
	BelowModelledDefault  bool                `json:"below_modelled_default"`
}

type jsonAnalyseWork struct {
	PlannedCycles int `json:"planned_cycles"`
	PlannedTrials int `json:"planned_trials"`
}

type jsonAnalyseMethod struct {
	ID                string `json:"id"`
	RandomisationSeed uint64 `json:"randomisation_seed"`
}

type jsonAnalyseAcquisition struct {
	structuredAcquisition
	Capture structuredCapture `json:"capture"`
}

type jsonAnalyseRequestRecord struct {
	structuredRequest
	Role string `json:"role"`
}

type jsonAnalyseAssignment struct {
	RequestID int `json:"request_id"`
	Position  int `json:"position"`
}

type jsonAnalyseTrial struct {
	Kind         string                  `json:"kind"`
	TrialIndex   int                     `json:"trial_index"`
	Cycle        int                     `json:"cycle"`
	Shift        int                     `json:"shift"`
	ConnectionID int                     `json:"connection_id"`
	Dispatch     structuredDispatch      `json:"dispatch"`
	Assignments  []jsonAnalyseAssignment `json:"assignments"`
	Failure      *structuredFailure      `json:"failure"`
	Outcomes     []jsonAnalyseOutcome    `json:"outcomes"`
}

type jsonAnalyseOutcome struct {
	structuredOutcome
	Response *structuredResponse `json:"response"`
}

func newAnalyseJSONStream(w io.Writer, workload analyseWorkload, connectionLimit int,
	labels []string, context analyseJSONContext, capture raceCaptureConfig,
) (*analyseJSONStream, error) {
	acquisition, err := makeStructuredAcquisition(context.Delivery, context.ReleaseDelay,
		context.BodyBytesWithheld, context.Insecure, context.RunTimeout, context.ResponseLimits)
	if err != nil {
		return nil, err
	}
	if len(labels) != workload.Width || len(context.Requests) < workload.Width {
		return nil, fmt.Errorf("analyse catalogue does not match the measured width")
	}
	requests := makeJSONAnalyseRequests(context)
	run := jsonAnalyseRunRecord{
		structuredRunEnvelope: makeStructuredRunEnvelope(analyseSchema.logical, "analyse", context.BuildInfo,
			context.Argv, context.StartedAt, context.DialAddress, context.Origin, len(requests)),
		Plan: jsonAnalysePlan{
			Work:       jsonAnalyseWork{PlannedCycles: workload.Cycles, PlannedTrials: workload.Trials},
			BatchWidth: workload.Width, Arrangement: arrangeRotate, AssignmentUnit: "rotation_cycle",
			Warmup: structuredWarmup{
				TrialsPerConnection: context.WarmupTrialsPerConnection,
				BatchWidth:          context.WarmupBatchWidth,
			},
			Padding: makeStructuredPadding(context.Padding), ModelledDefaultCycles: workload.DefaultCycles,
			BelowModelledDefault: workload.LowerSensitivity,
		},
		Method: jsonAnalyseMethod{ID: analyseMethodVersion, RandomisationSeed: context.RandomisationSeed},
		Acquisition: jsonAnalyseAcquisition{
			structuredAcquisition: acquisition,
			Capture:               makeStructuredCapture(capture.headers, capture.bodyBytes, context.MaxResponseHeaderBytes),
		},
	}
	if acquisition.Delivery.ConnectionLimit != connectionLimit {
		return nil, fmt.Errorf("analyse delivery capacity is inconsistent")
	}
	stream := &analyseJSONStream{stream: newStructuredStream(w)}
	if err := stream.stream.Start(run, len(requests)); err != nil {
		return nil, fmt.Errorf("write analyse JSON run: %w", err)
	}
	for _, request := range requests {
		if err := stream.stream.Request(request); err != nil {
			return nil, fmt.Errorf("write analyse JSON request: %w", err)
		}
	}
	return stream, nil
}

func makeJSONAnalyseRequests(context analyseJSONContext) []jsonAnalyseRequestRecord {
	requests := make([]jsonAnalyseRequestRecord, len(context.Requests))
	for i, request := range context.Requests {
		requests[i] = jsonAnalyseRequestRecord{structuredRequest: makeStructuredRequest(request), Role: request.RequestSet}
	}
	return requests
}

func (s *analyseJSONStream) Trial(observation analyseTrialObservation) error {
	if err := s.stream.Evidence(makeJSONAnalyseTrial(observation)); err != nil {
		return fmt.Errorf("write analyse JSON trial: %w", err)
	}
	return nil
}

func makeJSONAnalyseTrial(observation analyseTrialObservation) jsonAnalyseTrial {
	assignments := make([]jsonAnalyseAssignment, len(observation.Arrangement))
	for position, request := range observation.Arrangement {
		assignments[position] = jsonAnalyseAssignment{RequestID: request, Position: position}
	}
	outcomes := make([]jsonAnalyseOutcome, len(observation.Outcomes))
	for i, outcome := range observation.Outcomes {
		outcomes[i].structuredOutcome = makeStructuredOutcome(outcome)
		if outcome.Response != nil {
			response := makeStructuredResponse(*outcome.Response)
			outcomes[i].Response = &response
		}
	}
	failure := makeStructuredFailure(observation.cause, "transport_error")
	if failure == nil && observation.Error != "" {
		failure = makeStructuredFailure(errors.New(observation.Error), "transport_error")
		if observation.Failure != nil {
			failure.Code = string(observation.Failure.Code)
		}
	}
	return jsonAnalyseTrial{
		Kind: "trial", TrialIndex: observation.Index, Cycle: observation.Cycle,
		Shift:        observation.Shift,
		ConnectionID: observation.Connection,
		Dispatch:     makeStructuredDispatch(observation.Dispatch),
		Assignments:  assignments, Failure: failure, Outcomes: outcomes,
	}
}

type analyseJSONContext struct {
	BuildInfo                 buildinfo.Info
	Argv                      []string
	StartedAt                 time.Time
	DialAddress               string
	Origin                    *string
	Requests                  []RequestRecord
	Delivery                  deliveryPlan
	ReleaseDelay              time.Duration
	BodyBytesWithheld         int
	Insecure                  bool
	MaxResponseHeaderBytes    int64
	WarmupTrialsPerConnection int
	MeasuredBatchWidth        int
	WarmupBatchWidth          int
	Padding                   []PaddingRecord
	ResponseLimits            responseLimits
	RunTimeout                runTimeout
	RandomisationSeed         uint64
}
