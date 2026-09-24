package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sync"
	"time"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

var compileAnalyseReplaySchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(offlineSchemaLoader{})
	compiler.AssertFormat()
	var target string
	for _, descriptor := range []*schemaDescriptor{&commonSchema, &analyseSchema} {
		data, err := exportCurrentSchemaDocument(descriptor)
		if err != nil {
			return nil, err
		}
		document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		var identity struct {
			ID string `json:"$id"`
		}
		if err := json.Unmarshal(data, &identity); err != nil {
			return nil, err
		}
		if err := compiler.AddResource(identity.ID, document); err != nil {
			return nil, err
		}
		target = identity.ID
	}
	return compiler.Compile(target)
})

// replayAnalyseJSONStream validates complete evidence before repeating the
// fixed method and comparing its terminal statistical projection.
func replayAnalyseJSONStream(input io.Reader) (analyseRunResult, error) {
	reader := bufio.NewReader(input)
	first, err := readStructuredJSONRecord(reader)
	if err != nil {
		return analyseRunResult{}, fmt.Errorf("read analyse run: %w", err)
	}
	var header struct {
		Kind   string `json:"kind"`
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(first, &header); err != nil {
		return analyseRunResult{}, err
	}
	if header.Kind != "run" {
		return analyseRunResult{}, fmt.Errorf("analyse replay must start with a run record")
	}
	if header.Schema != analyseSchema.logical {
		return analyseRunResult{}, fmt.Errorf("analyse replay has an unsupported schema or method version")
	}
	schema, err := compileAnalyseReplaySchema()
	if err != nil {
		return analyseRunResult{}, fmt.Errorf("compile analyse replay schema: %w", err)
	}
	validate := func(raw json.RawMessage) error {
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		return schema.Validate(instance)
	}
	if err := validate(first); err != nil {
		return analyseRunResult{}, fmt.Errorf("analyse run schema: %w", err)
	}
	var run jsonAnalyseRunRecord
	if err := json.Unmarshal(first, &run); err != nil {
		return analyseRunResult{}, err
	}
	if run.RequestCount > analyseMaximumOperations+8 {
		return analyseRunResult{}, fmt.Errorf("analyse request catalogue exceeds supported work")
	}
	catalogue := structuredCatalogue{command: "analyse"}
	var requestRole string
	var catalogueRequest structuredRequest
	labels := make(map[string]bool)
	validator := structuredValidator{
		check: func(header structuredHeader) error {
			if header.Kind == "request" {
				if err := validateStructuredRequest(catalogueRequest, labels); err != nil {
					return err
				}
			}
			return catalogue.Accept(header, requestRole)
		},
		terminalKind: "result",
		evidenceKind: func(kind string) bool {
			return kind == "trial"
		},
	}
	if err := validator.Accept(structuredHeader{Kind: "run", RequestCount: &run.RequestCount}); err != nil {
		return analyseRunResult{}, err
	}
	requests := make([]jsonAnalyseRequestRecord, 0, run.RequestCount)
	trials := make([]jsonAnalyseTrial, 0)
	var terminal jsonAnalyseResultRecord
	for {
		raw, err := readStructuredJSONRecord(reader)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return analyseRunResult{}, fmt.Errorf("read analyse record: %w", err)
		}
		if err := validate(raw); err != nil {
			return analyseRunResult{}, fmt.Errorf("analyse record schema: %w", err)
		}
		var header structuredHeader
		if err := json.Unmarshal(raw, &header); err != nil {
			return analyseRunResult{}, err
		}
		switch header.Kind {
		case "request":
			var request jsonAnalyseRequestRecord
			if err := json.Unmarshal(raw, &request); err != nil {
				return analyseRunResult{}, err
			}
			catalogueRequest = request.structuredRequest
			requestRole = request.Role
			requests = append(requests, request)
		case "trial":
			var trial jsonAnalyseTrial
			if err := json.Unmarshal(raw, &trial); err != nil {
				return analyseRunResult{}, err
			}
			if len(trials) >= analyseMaximumOperations {
				return analyseRunResult{}, fmt.Errorf("analyse trials exceed supported work")
			}
			trials = append(trials, trial)
		case "result":
			if err := json.Unmarshal(raw, &terminal); err != nil {
				return analyseRunResult{}, err
			}
		}
		if err := validator.Accept(header); err != nil {
			return analyseRunResult{}, fmt.Errorf("analyse replay framing: %w", err)
		}
	}
	if err := validator.Finish(); err != nil {
		return analyseRunResult{}, err
	}
	evidence, err := nativeAnalyseEvidence(run, requests, trials, terminal)
	if err != nil {
		return analyseRunResult{}, err
	}
	result := analyseOffline(evidence, analyseConfig{
		RandomisationSeed: run.Method.RandomisationSeed, ConnectionLimit: run.Acquisition.Delivery.ConnectionLimit,
	})
	// Preserve acquisition diagnostics only after independently deriving the code.
	if terminal.Analysis.Reason != nil && terminal.Analysis.Reason.Code == result.Validity.Code {
		result.Validity.Detail = string(terminal.Analysis.Reason.Message)
	}
	result.Plan.DefaultCycles = run.Plan.ModelledDefaultCycles
	result.Plan.LowerSensitivity = run.Plan.BelowModelledDefault
	expected := makeJSONAnalyseResultRecord(result, analyseJSONContext{WarmupBatchWidth: run.Plan.Warmup.BatchWidth})
	if !reflect.DeepEqual(terminal, expected) {
		return analyseRunResult{}, fmt.Errorf("analyse replay terminal result does not agree with trial evidence")
	}
	return result, nil
}

func nativeAnalyseEvidence(run jsonAnalyseRunRecord, requests []jsonAnalyseRequestRecord,
	trials []jsonAnalyseTrial, terminal jsonAnalyseResultRecord,
) (analyseEvidence, error) {
	width := run.Plan.BatchWidth
	if err := validateStructuredDelivery(run.Acquisition.Delivery, width); err != nil {
		return analyseEvidence{}, err
	}
	work := run.Plan.Work
	planned, ok := checkedProduct(width, work.PlannedCycles)
	operations, operationOK := checkedProduct(work.PlannedTrials, width)
	if !ok || planned != work.PlannedTrials || !operationOK || operations > analyseMaximumOperations {
		return analyseEvidence{}, fmt.Errorf("analyse planned work is inconsistent")
	}
	if run.Plan.ModelledDefaultCycles != analyseDefaultCycles(width) ||
		run.Plan.BelowModelledDefault != (work.PlannedCycles < run.Plan.ModelledDefaultCycles) {
		return analyseEvidence{}, fmt.Errorf("analyse modelled default is inconsistent")
	}
	if len(requests) < width {
		return analyseEvidence{}, fmt.Errorf("analyse measured catalogue is incomplete")
	}
	for id, request := range requests {
		role := "measured"
		if id >= width {
			role = "warmup"
		}
		if request.RequestID != id || request.Role != role {
			return analyseEvidence{}, fmt.Errorf("analyse request catalogue has inconsistent roles")
		}
	}
	warmup := run.Plan.Warmup
	priming := len(requests) - width
	if (warmup.TrialsPerConnection == 0) != (warmup.BatchWidth == 0) ||
		(priming > 0 && priming != warmup.BatchWidth) ||
		(priming == 0 && warmup.BatchWidth != 0 && warmup.BatchWidth != width) {
		return analyseEvidence{}, fmt.Errorf("analyse priming catalogue is inconsistent")
	}
	padding := make([]PaddingRecord, len(run.Plan.Padding))
	for i, p := range run.Plan.Padding {
		padding[i] = PaddingRecord{
			Position:   p.Position,
			URLParams:  p.QueryFields,
			Headers:    p.HeaderFields,
			BodyParams: p.BodyFields,
		}
	}
	if err := validateRecordedPadding(padding, width); err != nil {
		return analyseEvidence{}, err
	}
	execution := terminal.Execution.accounting()
	if err := validateAnalyseExecution(execution, width, warmup.BatchWidth); err != nil {
		return analyseEvidence{}, err
	}
	if execution.Trials.Planned != work.PlannedTrials || execution.Cycles.Planned != work.PlannedCycles ||
		execution.Connections.Planned != run.Acquisition.Delivery.ConnectionLimit ||
		execution.Trials.Attempted != len(trials) {
		return analyseEvidence{}, fmt.Errorf("analyse execution disagrees with the run plan or trial evidence")
	}
	completion := runCompletion{State: terminal.Completion.State}
	if terminal.Completion.Failure != nil {
		completion.Code = completionCode(terminal.Completion.Failure.Code)
		completion.Detail = string(terminal.Completion.Failure.Message)
	}
	captureBytes := int64(-1)
	if run.Acquisition.Capture.BodyBytes != nil {
		captureBytes = *run.Acquisition.Capture.BodyBytes
	}
	evidence := analyseEvidence{
		Width: width, Completion: completion,
		Capture: raceCaptureConfig{headers: run.Acquisition.Capture.Headers, bodyBytes: captureBytes},
		Execution: analyseExecutionEvidence{
			Recorded: true, PlannedCycles: work.PlannedCycles,
			WarmupTrials: execution.Priming.Batches.Attempted, WarmupRankComplete: execution.Priming.Batches.RankComplete,
			WarmupIncomplete:        execution.Priming.Batches.Incomplete,
			WarmupRequestOperations: execution.Priming.RequestOperations.Attempted,
			AttemptedTrials:         execution.Trials.Attempted, CompletedTrials: execution.Trials.RankComplete,
			AttemptedCycles: execution.Cycles.Attempted,
			Connections:     execution.Connections.Observed,
			Replacements:    execution.Connections.Replacements,
		},
	}
	cycles := make(map[int][]analyseTrialEvidence)
	indexes := make(map[int]bool)
	for _, trial := range trials {
		for _, outcome := range trial.Outcomes {
			if outcome.Response != nil {
				if _, err := validateStructuredResponse(*outcome.Response, run.Acquisition.Capture); err != nil {
					return analyseEvidence{}, err
				}
			}
		}
		if trial.Cycle >= work.PlannedCycles || trial.Shift >= width || indexes[trial.TrialIndex] ||
			trial.TrialIndex != trial.Cycle*width+trial.Shift {
			return analyseEvidence{}, fmt.Errorf("analyse trial coordinates are inconsistent")
		}
		indexes[trial.TrialIndex] = true
		observation, retained, err := nativeAnalyseObservation(trial, width)
		if err != nil {
			return analyseEvidence{}, err
		}
		evidence.Observations = append(evidence.Observations, observation)
		if retained == nil && evidence.Issue == nil {
			evidence.Issue = &analyseEvidenceIssue{
				Code: analyseIncompleteCycle, Detail: "recorded measured trial is not rank-complete",
			}
		}
		if retained != nil {
			cycles[trial.Cycle] = append(cycles[trial.Cycle], *retained)
		}
	}
	ids := make([]int, 0, len(cycles))
	for id := range cycles {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if len(cycles[id]) == width {
			evidence.Cycles = append(evidence.Cycles, analyseCycleEvidence{Trials: cycles[id]})
		}
	}
	if evidence.Issue == nil && completion.Code != "" {
		evidence.Issue = &analyseEvidenceIssue{Code: analyseInterrupted, Detail: completion.Detail}
	}
	if evidence.Issue == nil && len(evidence.Cycles) != work.PlannedCycles {
		evidence.Issue = &analyseEvidenceIssue{Code: analyseIncompleteCycle, Detail: "recorded cycles are incomplete"}
	}

	return evidence, nil
}

func nativeAnalyseObservation(
	trial jsonAnalyseTrial, width int,
) (analyseTrialObservation, *analyseTrialEvidence, error) {
	observation := analyseTrialObservation{
		Index:      trial.TrialIndex,
		Cycle:      trial.Cycle,
		Shift:      trial.Shift,
		Connection: trial.ConnectionID,
	}
	if len(trial.Assignments) != width {
		return observation, nil, fmt.Errorf("analyse trial assignment cardinality is inconsistent")
	}
	arrangement := make([]int, width)
	seen := make([]bool, width)
	for position, assignment := range trial.Assignments {
		if assignment.Position != position || assignment.Position >= width ||
			assignment.RequestID >= width || seen[assignment.Position] {
			return observation, nil, fmt.Errorf("analyse trial assignment is outside the measured catalogue or repeated")
		}
		seen[assignment.Position] = true
		arrangement[assignment.Position] = assignment.RequestID
	}
	if !analysePermutation(arrangement, width) {
		return observation, nil, fmt.Errorf("analyse trial assignments must form a permutation")
	}
	observation.Arrangement = arrangement
	if trial.Failure != nil {
		observation.Error = string(trial.Failure.Message)
		if trial.Failure.Code == "response_body_limit" || trial.Failure.Code == "batch_timeout" {
			if len(trial.Outcomes) != 0 {
				return observation, nil, fmt.Errorf("analyse local limit carries response evidence")
			}
			observation.Failure = &batchFailure{Code: batchFailureCode(trial.Failure.Code), Detail: observation.Error}
		}
	}
	ranks := make([]int, width)
	statuses := make([]int, width)
	rankSeen := make([]bool, width)
	clear(seen)
	complete := trial.Failure == nil && len(trial.Outcomes) == width
	for i, outcome := range trial.Outcomes {
		if i > 0 && outcome.RequestID <= trial.Outcomes[i-1].RequestID {
			return observation, nil, fmt.Errorf("analyse trial outcomes must increase by request ID")
		}
		if outcome.RequestID >= width || outcome.Position >= width || seen[outcome.RequestID] ||
			arrangement[outcome.Position] != outcome.RequestID {
			return observation, nil, fmt.Errorf("analyse trial outcome identity is inconsistent")
		}
		seen[outcome.RequestID] = true
		if outcome.Rank != nil {
			if *outcome.Rank >= width || rankSeen[*outcome.Rank] {
				return observation, nil, fmt.Errorf("analyse trial ranks are inconsistent")
			}
			rankSeen[*outcome.Rank] = true
			ranks[outcome.RequestID] = *outcome.Rank
		} else {
			complete = false
		}
		if outcome.Response != nil {
			statuses[outcome.RequestID] = outcome.Response.Status
		} else {
			complete = false
		}
		if outcome.Reset != nil {
			complete = false
		}
		observation.Outcomes = append(observation.Outcomes, nativeAnalyseOutcome(outcome))
	}
	if trial.Dispatch.HeadersSentAt != nil {
		observation.Dispatch.HeadersSentAt, _ = time.Parse(time.RFC3339Nano, string(*trial.Dispatch.HeadersSentAt))
	}
	if trial.Dispatch.BodyFinalisedAt != nil {
		observation.Dispatch.BodyFinalisedAt, _ = time.Parse(time.RFC3339Nano, string(*trial.Dispatch.BodyFinalisedAt))
	}
	if !complete {
		return observation, nil, nil
	}
	return observation, &analyseTrialEvidence{
		Connection:  trial.ConnectionID,
		Shift:       trial.Shift,
		Arrangement: arrangement,
		Ranks:       ranks,
		Statuses:    statuses,
	}, nil
}

func nativeAnalyseOutcome(outcome jsonAnalyseOutcome) OutcomeRecord {
	record := OutcomeRecord{
		Request:  outcome.RequestID,
		Position: outcome.Position,
		Rank:     outcome.Rank,
		StreamID: outcome.StreamID,
	}
	if outcome.Response != nil {
		r := outcome.Response
		record.Response = &ResponseRecord{
			Status: r.Status, ContentLength: r.ContentLength, BodyBytes: r.BodyBytes,
			BodyCapturedBytes: r.BodyCapturedBytes, BodyCapturedComplete: r.BodyCapturedComplete, SHA256: r.SHA256,
			Headers: structuredHeaderRecord(r.Headers), Trailers: structuredHeaderRecord(r.Trailers),
		}
		if r.Body != nil {
			record.Response.Body = &EncodedData{Encoding: r.Body.Encoding, Data: string(r.Body.Data)}
		}
	}
	if outcome.Reset != nil {
		record.Reset = &ResetRecord{Code: outcome.Reset.Code, CodeNumber: outcome.Reset.CodeNumber}
	}
	return record
}

func validateRecordedPadding(padding []PaddingRecord, width int) error {
	if len(padding) != width {
		return fmt.Errorf("schedule width is %d; want %d", len(padding), width)
	}
	for position, entry := range padding {
		if entry.Position != position || entry.URLParams < 0 ||
			entry.Headers < 0 || entry.BodyParams < 0 {
			return fmt.Errorf("invalid entry at position %d", position)
		}
	}
	return nil
}
