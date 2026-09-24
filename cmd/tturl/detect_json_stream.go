package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
)

//go:embed doc/schemas/tturl-detect-v1.schema.json
var detectSchemaDocument []byte

var detectSchema = schemaDescriptor{
	logical:  "tturl.detect/v1",
	filename: "tturl-detect-v1.schema.json",
	document: detectSchemaDocument,
}

type jsonDetectRunRecord struct {
	structuredRunEnvelope
	Plan        jsonDetectPlan        `json:"plan"`
	Method      jsonDetectMethod      `json:"method"`
	Acquisition jsonDetectAcquisition `json:"acquisition"`
}

type jsonDetectAcquisition struct {
	structuredAcquisition
	InFlightComparisonLimit int `json:"in_flight_comparison_limit"`
}

type jsonDetectRequestRecord struct {
	structuredRequest
	Role        string `json:"role"`
	SupplyLimit *int   `json:"supply_limit,omitempty"`
}

// MarshalJSON preserves null as the baseline's unlimited supply limit.
func (r jsonDetectRequestRecord) MarshalJSON() ([]byte, error) {
	type request jsonDetectRequestRecord
	if r.Role != "baseline" {
		return json.Marshal(request(r))
	}
	return json.Marshal(struct {
		request
		SupplyLimit *int `json:"supply_limit"`
	}{request: request(r), SupplyLimit: r.SupplyLimit})
}

type jsonDetectReporter struct {
	stream             *structuredStream
	width              int
	candidates         []int
	baselines          []int
	comparisonsMaximum int
	plannedConnections int
}

type detectCapturedRequest struct {
	RequestRecord
	Role           string
	BaselineSupply baselineSupply
}

func captureDetectRequestRecords(
	pre *prepared,
	refs runReferences,
) ([]detectCapturedRequest, error) {
	requests := pre.allRequests()
	if len(requests) != len(refs.refs) {
		return nil, fmt.Errorf(
			"detect request capture has %d requests but %d references",
			len(requests), len(refs.refs))
	}
	result := make([]detectCapturedRequest, len(requests))
	baselineStart := len(pre.reqs)
	warmupStart := baselineStart + len(pre.baselineTemplates)
	for id, request := range requests {
		record, err := captureRequestRecord(request, refs.refs[id])
		if err != nil {
			return nil, fmt.Errorf("capture request %d: %w", id, err)
		}
		role := "candidate"
		record.RequestSet = requestSetMeasured
		var supply baselineSupply
		switch {
		case id >= warmupStart:
			role = "warmup"
			record.RequestSet = requestSetWarmup
		case id >= baselineStart:
			role = "baseline"
			supply = pre.baselineTemplates[id-baselineStart].supply
		}
		result[id] = detectCapturedRequest{
			RequestRecord: record, Role: role, BaselineSupply: supply,
		}
	}
	return result, nil
}

func newJSONDetectReporter(w io.Writer, width int) *jsonDetectReporter {
	return &jsonDetectReporter{stream: newStructuredStream(w), width: width, comparisonsMaximum: -1}
}

func (r *jsonDetectReporter) Run(run DetectRunRecord, requests []detectCapturedRequest) error {
	if !run.SolverMethod.Valid() {
		return fmt.Errorf("detect solver method %q is invalid", run.SolverMethod)
	}
	if len(run.Padding) != run.WidthEffective {
		return fmt.Errorf("detect padding has %d positions, want effective width %d", len(run.Padding), run.WidthEffective)
	}
	if err := validateDetectJSONPlan(run, requests); err != nil {
		return err
	}
	acquisition, err := makeStructuredAcquisition(run.Delivery, run.ReleaseDelay,
		run.BodyBytesWithheld, run.Insecure, run.RunTimeout, run.ResponseLimits)
	if err != nil {
		return err
	}
	if acquisition.Delivery.ConnectionLimit != run.ConnectionLimit {
		return fmt.Errorf("detect delivery and planned connection limits disagree")
	}
	r.width = run.WidthEffective
	r.comparisonsMaximum = run.ComparisonsMax
	r.plannedConnections = run.ConnectionLimit
	r.candidates = make([]int, 0, run.CandidateCount)
	r.baselines = make([]int, 0, run.BaselineTemplateCount)
	for _, request := range requests {
		switch request.Role {
		case "candidate":
			r.candidates = append(r.candidates, request.Reference.ID)
		case "baseline":
			r.baselines = append(r.baselines, request.Reference.ID)
		}
	}
	if err := r.stream.Start(jsonDetectRunRecord{
		structuredRunEnvelope: makeStructuredRunEnvelope(detectSchema.logical, "detect", run.BuildInfo,
			run.Argv, run.StartedAt, run.DialAddress, run.Origin, run.RequestCount),
		Plan: makeJSONDetectPlan(run),
		Method: jsonDetectMethod{
			ID:        string(run.SolverMethod),
			Strategy:  jsonDetectStrategy{Requested: string(run.RequestedStrategy), Resolved: string(run.ResolvedStrategy)},
			Direction: detectDirection(run.Direction), FalsePositiveRisk: run.FalsePositiveRisk,
			FalseNegativeRisk: run.FalseNegativeRisk, Edge: run.ConfiguredNegativeEdge,
		},
		Acquisition: jsonDetectAcquisition{
			structuredAcquisition:   acquisition,
			InFlightComparisonLimit: run.InFlightComparisonLimit,
		},
	}, run.RequestCount); err != nil {
		return err
	}
	for _, request := range requests {
		var supply *int
		if request.Role == "baseline" && !request.BaselineSupply.unlimited {
			supply = new(request.BaselineSupply.count)
		}
		if err := r.stream.Request(jsonDetectRequestRecord{
			structuredRequest: makeStructuredRequest(request.RequestRecord), Role: request.Role, SupplyLimit: supply,
		}); err != nil {
			return err
		}
	}
	return nil
}
