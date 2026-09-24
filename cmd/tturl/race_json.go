package main

import (
	_ "embed"
	"fmt"
	"io"
	"slices"
)

//go:embed doc/schemas/tturl-race-v1.schema.json
var raceSchemaDocument []byte

var raceSchema = schemaDescriptor{
	logical:  "tturl.race/v1",
	filename: "tturl-race-v1.schema.json",
	document: raceSchemaDocument,
}

type jsonRaceReporter struct {
	stream     *structuredStream
	run        RunRecord
	extractors []jsonRaceExtractor
	roles      []string
}

func newJSONRaceReporter(w io.Writer) *jsonRaceReporter {
	return &jsonRaceReporter{stream: newStructuredStream(w)}
}

func (r *jsonRaceReporter) Run(rec RunRecord) error {
	acquisition, err := makeStructuredAcquisition(
		rec.Delivery, rec.ReleaseDelay, rec.BodyBytesWithheld, rec.Insecure, rec.RunTimeout, rec.ResponseLimits,
	)
	if err != nil {
		return err
	}
	if len(rec.ExtractRegex) != len(rec.ExtractFields) {
		return fmt.Errorf("race extractor catalogue has inconsistent fields")
	}
	extractors := make([]jsonRaceExtractor, len(rec.ExtractRegex))
	for i, pattern := range rec.ExtractRegex {
		selection := string(extractWholeMatch)
		if len(rec.ExtractFields[i]) > 0 {
			selection = string(extractNamedGroups)
		}
		extractors[i] = jsonRaceExtractor{
			ExtractorID: i,
			Pattern:     wireString(pattern),
			Selection:   selection,
			Fields:      wireStrings(rec.ExtractFields[i]),
		}
	}
	run := jsonRaceRunRecord{
		structuredRunEnvelope: makeStructuredRunEnvelope(
			raceSchema.logical, "race", rec.BuildInfo, rec.Argv, rec.StartedAt, rec.DialAddress, rec.Origin, rec.RequestCount,
		),
		Plan: jsonRacePlan{
			Work: jsonRaceWork{
				RequestedTrials: positiveIntPointer(rec.TrialsRequested),
				PlannedTrials:   positiveIntPointer(rec.Trials),
			},
			BatchWidth: rec.TrialBatchWidth,
			Warmup: structuredWarmup{
				TrialsPerConnection: rec.Warmup,
				BatchWidth:          rec.WarmupBatchWidth,
			},
			Arrangement: rec.ArrangementPolicy,
			Padding:     makeStructuredPadding(rec.Padding),
			Extractors:  extractors,
		},
		Acquisition: jsonRaceAcquisition{
			structuredAcquisition: acquisition,
			Capture: makeStructuredCapture(rec.CaptureHeaders,
				rec.CaptureBodyBytes,
				rec.MaxResponseHeaderBytes),
		},
	}
	if err := r.stream.Start(run, rec.RequestCount); err != nil {
		return err
	}
	r.run = rec
	r.extractors = extractors
	r.roles = make([]string, 0, rec.RequestCount)
	return nil
}

func (r *jsonRaceReporter) Request(rec RequestRecord) error {
	if rec.Reference.ID != len(r.roles) {
		return fmt.Errorf("race request ID must be %d", len(r.roles))
	}
	role := "measured"
	switch rec.RequestSet {
	case "trial", "measured":
	case "warmup":
		role = "warmup"
	default:
		return fmt.Errorf("race request has an invalid role")
	}
	if err := r.stream.Request(jsonRaceRequestRecord{
		structuredRequest: makeStructuredRequest(rec),
		Role:              role,
	}); err != nil {
		return err
	}
	r.roles = append(r.roles, role)
	return nil
}

func (r *jsonRaceReporter) Trial(rec TrialRecord) error {
	if rec.Trial == nil {
		return fmt.Errorf("race evidence requires a trial")
	}
	trial := rec.Trial
	outcomes := make([]jsonRaceOutcome, len(rec.Outcomes))
	role := "measured"
	if trial.Warmup && slices.Contains(r.roles, "warmup") {
		role = "warmup"
	}
	for i, outcome := range rec.Outcomes {
		if outcome.Request < 0 || outcome.Request >= len(r.roles) || r.roles[outcome.Request] != role {
			return fmt.Errorf("race outcome has an unresolved request role")
		}
		outcomes[i].structuredOutcome = makeStructuredOutcome(outcome)
		if outcome.Response != nil {
			extracts, err := r.projectExtracts(outcome.Response.Extracts)
			if err != nil {
				return err
			}
			outcomes[i].Response = &jsonRaceResponse{
				structuredResponse: makeStructuredResponse(*outcome.Response),
				Extracts:           extracts,
			}
		}
	}
	evidence := jsonRaceEvidence{
		ConnectionID: trial.Conn,
		Dispatch:     makeStructuredDispatch(trial.Dispatch),
		Failure: makeStructuredFailure(trial.Err,
			"transport_error"),
		Outcomes: outcomes,
	}
	if trial.Warmup {
		return r.stream.Evidence(jsonRaceWarmupRecord{Kind: "warmup", jsonRaceEvidence: evidence})
	}
	if trial.Index < 0 {
		return fmt.Errorf("race measured evidence requires a non-negative trial index")
	}
	var cycle, shift *int
	if trial.Cycle >= 0 {
		cycle = &trial.Cycle
	}
	if trial.Shift >= 0 {
		shift = &trial.Shift
	}
	return r.stream.Evidence(jsonRaceTrialRecord{
		Kind:             "trial",
		TrialIndex:       trial.Index,
		Cycle:            cycle,
		Shift:            shift,
		jsonRaceEvidence: evidence,
	})
}

func (r *jsonRaceReporter) projectExtracts(extracts []ExtractRecord) ([]jsonRaceExtract, error) {
	if len(extracts) != len(r.extractors) {
		return nil, fmt.Errorf("race response extraction count differs from the run catalogue")
	}
	result := make([]jsonRaceExtract, len(extracts))
	for i, extract := range extracts {
		definition := r.extractors[i]
		if wireString(extract.Pattern) != definition.Pattern ||
			string(extract.Selection) != definition.Selection ||
			!slices.Equal(wireStrings(extract.Fields), definition.Fields) {
			return nil, fmt.Errorf("race extractor %d differs from the run catalogue", i)
		}
		matches := make([]jsonRaceExtractMatch, len(extract.Matches))
		for j, match := range extract.Matches {
			projected := jsonRaceExtractMatch{
				Offset: match.Offset,
				Captures: make([]jsonRaceExtractCapture,
					len(match.Captures)),
			}
			if match.Value != nil {
				encoding := match.Value.Encoding
				data := encodedString(match.Value.Data)
				projected.Encoding = &encoding
				projected.Data = &data
			}
			if definition.Selection == string(extractWholeMatch) {
				if match.Value == nil || len(match.Captures) != 0 {
					return nil, fmt.Errorf("race whole-match extraction is inconsistent")
				}
			} else {
				if match.Value != nil || len(match.Captures) != len(definition.Fields) {
					return nil, fmt.Errorf("race named extraction is inconsistent")
				}
			}
			for k, capture := range match.Captures {
				if wireString(capture.Name) != definition.Fields[k] {
					return nil, fmt.Errorf("race extraction capture has an inconsistent name")
				}
				projected.Captures[k] = makeRaceExtractCapture(capture)
			}
			matches[j] = projected
		}
		result[i] = jsonRaceExtract{ExtractorID: definition.ExtractorID, Complete: extract.Complete, Matches: matches}
	}
	return result, nil
}

func makeRaceExtractCapture(capture ExtractCapture) jsonRaceExtractCapture {
	result := jsonRaceExtractCapture{
		Name:         wireString(capture.Name),
		Participated: capture.Participated,
		Offset:       capture.Offset,
	}
	if capture.Value != nil {
		encoding := capture.Value.Encoding
		data := encodedString(capture.Value.Data)
		result.Encoding = &encoding
		result.Data = &data
	}
	return result
}

func (r *jsonRaceReporter) Summary(rec SummaryRecord) error {
	if rec.MeasuredWidth == 0 {
		rec.MeasuredWidth = r.run.TrialBatchWidth
	}
	if rec.PrimingWidth == 0 {
		rec.PrimingWidth = r.run.WarmupBatchWidth
	}
	if err := validateRaceExecution(rec.Execution, rec.MeasuredWidth, rec.PrimingWidth); err != nil {
		return err
	}
	resets := make([]structuredResetCount, 0)
	for id, count := range rec.ResetCount {
		if count > 0 {
			resets = append(resets, structuredResetCount{RequestID: id, Count: count})
		}
	}
	return r.stream.Finish(jsonRaceResultRecord{
		Kind:      "result",
		Execution: makeJSONRaceExecution(rec.Execution),
		Aggregates: jsonRaceAggregates{
			ResetCounts: resets,
		},
		Completion: makeStructuredCompletion(rec.Completion),
	})
}

type jsonRaceRunRecord struct {
	structuredRunEnvelope
	Plan        jsonRacePlan        `json:"plan"`
	Acquisition jsonRaceAcquisition `json:"acquisition"`
}
type jsonRacePlan struct {
	Work        jsonRaceWork        `json:"work"`
	BatchWidth  int                 `json:"batch_width"`
	Warmup      structuredWarmup    `json:"warmup"`
	Arrangement string              `json:"arrangement"`
	Padding     []structuredPadding `json:"padding"`
	Extractors  []jsonRaceExtractor `json:"extractors"`
}
type jsonRaceWork struct {
	RequestedTrials *int `json:"requested_trials"`
	PlannedTrials   *int `json:"planned_trials"`
}
type jsonRaceExtractor struct {
	ExtractorID int          `json:"extractor_id"`
	Pattern     wireString   `json:"pattern"`
	Selection   string       `json:"selection"`
	Fields      []wireString `json:"fields"`
}
type jsonRaceAcquisition struct {
	structuredAcquisition
	Capture structuredCapture `json:"capture"`
}
type jsonRaceRequestRecord struct {
	structuredRequest
	Role string `json:"role"`
}
type jsonRaceEvidence struct {
	ConnectionID int                `json:"connection_id"`
	Dispatch     structuredDispatch `json:"dispatch"`
	Failure      *structuredFailure `json:"failure"`
	Outcomes     []jsonRaceOutcome  `json:"outcomes"`
}
type jsonRaceWarmupRecord struct {
	Kind string `json:"kind"`
	jsonRaceEvidence
}
type jsonRaceTrialRecord struct {
	Kind       string `json:"kind"`
	TrialIndex int    `json:"trial_index"`
	Cycle      *int   `json:"cycle"`
	Shift      *int   `json:"shift"`
	jsonRaceEvidence
}
type jsonRaceOutcome struct {
	structuredOutcome
	Response *jsonRaceResponse `json:"response"`
}
type jsonRaceResponse struct {
	structuredResponse
	Extracts []jsonRaceExtract `json:"extracts"`
}
type jsonRaceExtract struct {
	ExtractorID int                    `json:"extractor_id"`
	Complete    bool                   `json:"complete"`
	Matches     []jsonRaceExtractMatch `json:"matches"`
}
type jsonRaceExtractMatch struct {
	Offset   [2]int                   `json:"offset"`
	Encoding *string                  `json:"encoding"`
	Data     *encodedString           `json:"data"`
	Captures []jsonRaceExtractCapture `json:"captures"`
}
type jsonRaceExtractCapture struct {
	Name         wireString     `json:"name"`
	Participated bool           `json:"participated"`
	Offset       *[2]int        `json:"offset"`
	Encoding     *string        `json:"encoding"`
	Data         *encodedString `json:"data"`
}
type jsonRaceAggregates struct {
	ResetCounts []structuredResetCount `json:"reset_counts"`
}
type jsonRaceResultRecord struct {
	Kind       string               `json:"kind"`
	Execution  jsonRaceExecution    `json:"execution"`
	Aggregates jsonRaceAggregates   `json:"aggregates"`
	Completion structuredCompletion `json:"completion"`
}

var _ RaceReporter = (*jsonRaceReporter)(nil)

type jsonRaceExecutionTrials struct {
	Maximum      *int `json:"maximum"`
	Attempted    int  `json:"attempted"`
	RankComplete int  `json:"rank_complete"`
	Incomplete   int  `json:"incomplete"`
	Unattempted  *int `json:"unattempted"`
}
type jsonRaceExecution struct {
	structuredExecution
	Trials jsonRaceExecutionTrials `json:"trials"`
}

func makeJSONRaceExecution(value raceExecution) jsonRaceExecution {
	result := jsonRaceExecution{structuredExecution: makeStructuredExecution(value.sharedExecution)}
	result.Trials = jsonRaceExecutionTrials(value.Trials)
	return result
}
