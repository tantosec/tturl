package main

import (
	_ "embed"
	"io"
	"slices"
	"time"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/tth2"
)

//go:embed doc/schemas/tturl-measure-v1.schema.json
var measureSchemaDocument []byte

var measureSchema = schemaDescriptor{
	logical:  "tturl.measure/v1",
	filename: "tturl-measure-v1.schema.json",
	document: measureSchemaDocument,
}

// MeasureRunRecord is the reproducible plan for one fixed experiment.
type MeasureRunRecord struct {
	TrialsRequested    int
	TrialsPlanned      int
	Budget             measureBudget
	Warmup             int
	BuildInfo          buildinfo.Info
	Argv               []string
	StartedAt          time.Time
	DialAddress        string
	Origin             *string
	RequestCount       int
	MeasuredBatchWidth int
	WarmupBatchWidth   int
	Delivery           deliveryPlan
	ArrangementPolicy  string
	ReleaseDelay       time.Duration
	BodyBytesWithheld  int
	ResponseLimits     responseLimits
	RunTimeout         runTimeout
	Insecure           bool
	Padding            []PaddingRecord
}

type jsonMeasureReporter struct {
	stream   *structuredStream
	run      MeasureRunRecord
	requests []RequestRecord
}

func newJSONMeasureReporter(
	w io.Writer,
	run MeasureRunRecord,
	requests []RequestRecord,
) *jsonMeasureReporter {
	return &jsonMeasureReporter{
		stream:   newStructuredStream(w),
		run:      run,
		requests: requests,
	}
}

func (r *jsonMeasureReporter) Run(concurrency int) error {
	measuredWidth := r.run.MeasuredBatchWidth
	if measuredWidth == 0 {
		measuredWidth = r.run.RequestCount
	}
	warmupWidth := r.run.WarmupBatchWidth
	if r.run.Warmup > 0 && warmupWidth == 0 {
		warmupWidth = measuredWidth
	}
	padding := r.run.Padding
	if padding == nil {
		padding = resolvedPadding(nil, measuredWidth)
	}
	delivery := r.run.Delivery
	delivery.connectionLimit = concurrency
	acquisition, err := makeStructuredAcquisition(
		delivery, r.run.ReleaseDelay, r.run.BodyBytesWithheld, r.run.Insecure, r.run.RunTimeout, r.run.ResponseLimits,
	)
	if err != nil {
		return err
	}
	arrangement := arrangementFromName(r.run.ArrangementPolicy)
	var plannedCycles *int
	if arrangement == tth2.ArrangeRotate && measuredWidth > 0 {
		value := r.run.TrialsPlanned / measuredWidth
		plannedCycles = &value
	}
	budget := r.run.Budget
	if budget.unit == "" {
		budget = measureBudget{unit: "trials", requested: r.run.TrialsRequested}
	}
	if err := r.stream.Start(jsonMeasureRunRecord{
		structuredRunEnvelope: makeStructuredRunEnvelope(
			measureSchema.logical, "measure", r.run.BuildInfo, r.run.Argv, r.run.StartedAt,
			r.run.DialAddress, r.run.Origin, r.run.RequestCount,
		),
		Plan: jsonMeasurePlan{
			Work: jsonMeasureWork{
				Unit: budget.unit, Requested: budget.requested,
				PlannedTrials: r.run.TrialsPlanned, PlannedCycles: plannedCycles,
			},
			BatchWidth: measuredWidth, Warmup: structuredWarmup{
				TrialsPerConnection: r.run.Warmup, BatchWidth: warmupWidth,
			},
			Arrangement: r.run.ArrangementPolicy, Padding: makeStructuredPadding(padding),
			PositionBalance: planPositionBalance(arrangement),
		},
		Acquisition: acquisition,
	}, r.run.RequestCount); err != nil {
		return err
	}
	for _, request := range r.requests {
		if err := r.stream.Request(jsonMeasureRequestRecord{
			structuredRequest: makeStructuredRequest(request),
			Role:              request.RequestSet,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (r *jsonMeasureReporter) Summary(measured *measurement) error {
	execution, err := measured.execution()
	if err != nil {
		return err
	}
	summary := measured.summary
	width := len(summary.RankCounts)
	requests := make([]jsonMeasureRequestStats, width)
	changed, non2xx := responseOutcomeReview(measured.statusCounts)
	for request, stats := range requestStats(summary) {
		requests[request] = jsonMeasureRequestStats{
			RequestID:          request,
			ResetCount:         summary.ResetCounts[request],
			StatusVaried:       slices.Contains(changed, request),
			Non2xxObserved:     slices.Contains(non2xx, request),
			Samples:            stats.N,
			MeanRank:           finiteMean(stats),
			NormalisedMeanRank: finiteNormalisedMeanRank(stats, width),
			RankCounts:         makeMeasureRankCounts(stats.RankCounts),
			StatusCounts: makeJSONMeasureStatusCounts(
				measured.statusCounts[request]),
		}
	}
	positions := make([]jsonMeasurePositionStats, width)
	for position, stats := range positionStats(summary) {
		positions[position] = jsonMeasurePositionStats{
			Position:   position,
			Samples:    stats.N,
			MeanRank:   finiteMean(stats),
			RankCounts: makeMeasureRankCounts(stats.RankCounts),
		}
	}
	batchFailures := make([]jsonMeasurementBatchFailure, len(measured.batchFailures))
	for i, failure := range measured.batchFailures {
		phase := "measured"
		var index *int
		if failure.Warmup {
			phase = "warmup"
		} else {
			value := failure.Index
			index = &value
		}
		batchFailures[i] = jsonMeasurementBatchFailure{
			Phase: phase, TrialIndex: index, ConnectionID: failure.Connection,
			Failure: makeStructuredFailure(failure.Err, "transport_error"),
		}
	}
	measurementState := "unavailable"
	if measured.retained > 0 {
		measurementState = "available"
	}
	measurement := jsonMeasureMeasurement{
		State: measurementState, Requests: requests, Positions: positions,
		PairwisePrecedence: jsonMeasurePairwisePrecedence{
			BeforeRequestIDs: measureAxis(width), AfterRequestIDs: measureAxis(width),
			Counts: cloneMeasureMatrix(measured.pairwise),
		},
		Assignments: jsonMeasureAssignments{
			RequestIDs: measureAxis(width), Positions: measureAxis(width), Counts: cloneMeasureMatrix(measured.assignments),
		},
		PositionBalance: measurementPositionBalance(measured),
		PositionSpread:  makeJSONMeasurePositionSpread(positionSpread(summary)),
	}
	if measurementState == "unavailable" {
		measurement.Reason = "no_retained_trials"
	}
	return r.stream.Finish(jsonMeasureResultRecord{
		Kind: "result", Completion: makeStructuredCompletion(measured.runCompletion()),
		Execution: makeJSONMeasureExecution(execution), Measurement: measurement, AcquisitionFailures: batchFailures,
	})
}

func measureAxis(width int) []int {
	axis := make([]int, width)
	for id := range axis {
		axis[id] = id
	}
	return axis
}

func cloneMeasureMatrix(matrix [][]int) [][]int {
	copy := make([][]int, len(matrix))
	for i, row := range matrix {
		copy[i] = slices.Clone(row)
	}
	return copy
}

func makeMeasureRankCounts(counts []int) []jsonMeasureRankCount {
	result := make([]jsonMeasureRankCount, 0)
	for rank, count := range counts {
		if count > 0 {
			result = append(result, jsonMeasureRankCount{Rank: rank, Count: count})
		}
	}
	return result
}

func makeJSONMeasureStatusCounts(counts map[int]int) []structuredStatusCount {
	statuses := make([]int, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, status)
	}
	slices.Sort(statuses)
	out := make([]structuredStatusCount, len(statuses))
	for i, status := range statuses {
		out[i] = structuredStatusCount{Status: status, Count: counts[status]}
	}
	return out
}

func arrangementFromName(name string) tth2.ArrangementPolicy {
	switch name {
	case "none":
		return tth2.ArrangeNone
	case "random":
		return tth2.ArrangeRandom
	default:
		return tth2.ArrangeRotate
	}
}

func planPositionBalance(arrangement tth2.ArrangementPolicy) string {
	switch arrangement {
	case tth2.ArrangeRotate:
		return "exact_per_complete_cycle"
	case tth2.ArrangeRandom:
		return "in_expectation"
	default:
		return "none"
	}
}

func makeJSONMeasurePositionSpread(
	values positionSpreadValues,
) jsonMeasurePositionSpread {
	return jsonMeasurePositionSpread{
		MinMeanRank: values.min,
		MaxMeanRank: values.max,
		Raw:         values.raw,
		Normalised:  values.normalised,
	}
}

func finiteMean(stats requestRankStats) *float64 {
	if stats.N == 0 {
		return nil
	}
	value := stats.MeanRank
	return &value
}

func finiteNormalisedMeanRank(stats requestRankStats, width int) *float64 {
	if stats.N == 0 || width < 2 {
		return nil
	}
	value := stats.MeanRank / float64(width-1)
	return &value
}

func measurementPositionBalance(measured *measurement) string {
	switch measured.arrangement {
	case tth2.ArrangeRotate:
		cycles := measured.cycleCounts(measured.width)
		if cycles.retained == 0 {
			return "unobserved"
		}
		return "exact"
	case tth2.ArrangeRandom:
		return "in_expectation"
	default:
		return "none"
	}
}

type jsonMeasureRunRecord struct {
	structuredRunEnvelope
	Plan        jsonMeasurePlan       `json:"plan"`
	Acquisition structuredAcquisition `json:"acquisition"`
}

type jsonMeasurePlan struct {
	Work            jsonMeasureWork     `json:"work"`
	BatchWidth      int                 `json:"batch_width"`
	Warmup          structuredWarmup    `json:"warmup"`
	Arrangement     string              `json:"arrangement"`
	Padding         []structuredPadding `json:"padding"`
	PositionBalance string              `json:"position_balance"`
}

type jsonMeasureWork struct {
	Unit          string `json:"unit"`
	Requested     int    `json:"requested"`
	PlannedTrials int    `json:"planned_trials"`
	PlannedCycles *int   `json:"planned_cycles,omitempty"`
}

type jsonMeasureRequestRecord struct {
	structuredRequest
	Role string `json:"role"`
}

type jsonMeasureRequestStats struct {
	RequestID          int                     `json:"request_id"`
	Samples            int                     `json:"samples"`
	MeanRank           *float64                `json:"mean_rank"`
	NormalisedMeanRank *float64                `json:"normalised_mean_rank"`
	RankCounts         []jsonMeasureRankCount  `json:"rank_counts"`
	StatusCounts       []structuredStatusCount `json:"status_counts"`
	StatusVaried       bool                    `json:"status_varied"`
	Non2xxObserved     bool                    `json:"non_2xx_observed"`
	ResetCount         int                     `json:"reset_count"`
}

type jsonMeasureRankCount struct {
	Rank  int `json:"rank"`
	Count int `json:"count"`
}

type jsonMeasurePositionStats struct {
	Position   int                    `json:"position"`
	Samples    int                    `json:"samples"`
	MeanRank   *float64               `json:"mean_rank"`
	RankCounts []jsonMeasureRankCount `json:"rank_counts"`
}

type jsonMeasurePairwisePrecedence struct {
	BeforeRequestIDs []int   `json:"before_request_ids"`
	AfterRequestIDs  []int   `json:"after_request_ids"`
	Counts           [][]int `json:"counts"`
}

type jsonMeasureAssignments struct {
	RequestIDs []int   `json:"request_ids"`
	Positions  []int   `json:"positions"`
	Counts     [][]int `json:"counts"`
}

type jsonMeasurementBatchFailure struct {
	Phase        string             `json:"phase"`
	TrialIndex   *int               `json:"trial_index,omitempty"`
	ConnectionID int                `json:"connection_id"`
	Failure      *structuredFailure `json:"failure"`
}

type jsonMeasureResultRecord struct {
	Kind                string                        `json:"kind"`
	Completion          structuredCompletion          `json:"completion"`
	Execution           jsonMeasureExecution          `json:"execution"`
	Measurement         jsonMeasureMeasurement        `json:"measurement"`
	AcquisitionFailures []jsonMeasurementBatchFailure `json:"acquisition_failures"`
}

type jsonMeasureMeasurement struct {
	State              string                        `json:"state"`
	Reason             string                        `json:"reason,omitempty"`
	Requests           []jsonMeasureRequestStats     `json:"requests"`
	Positions          []jsonMeasurePositionStats    `json:"positions"`
	PairwisePrecedence jsonMeasurePairwisePrecedence `json:"pairwise_precedence"`
	Assignments        jsonMeasureAssignments        `json:"assignments"`
	PositionBalance    string                        `json:"position_balance"`
	PositionSpread     jsonMeasurePositionSpread     `json:"position_spread"`
}

type jsonMeasurePositionSpread struct {
	MinMeanRank *float64 `json:"min_mean_rank"`
	MaxMeanRank *float64 `json:"max_mean_rank"`
	Raw         *float64 `json:"raw"`
	Normalised  *float64 `json:"normalised"`
}

var _ measurementReporter = (*jsonMeasureReporter)(nil)

type jsonMeasureExecutionTrials struct {
	Planned      int `json:"planned"`
	Attempted    int `json:"attempted"`
	RankComplete int `json:"rank_complete"`
	Incomplete   int `json:"incomplete"`
	Unattempted  int `json:"unattempted"`
	Retained     int `json:"retained"`
	Excluded     int `json:"excluded"`
}
type jsonMeasureExecutionCycles struct {
	Planned     int `json:"planned"`
	Attempted   int `json:"attempted"`
	Retained    int `json:"retained"`
	Incomplete  int `json:"incomplete"`
	Unattempted int `json:"unattempted"`
}
type jsonMeasureExecution struct {
	structuredExecution
	Trials jsonMeasureExecutionTrials  `json:"trials"`
	Cycles *jsonMeasureExecutionCycles `json:"cycles,omitempty"`
}

func makeJSONMeasureExecution(value measureExecution) jsonMeasureExecution {
	result := jsonMeasureExecution{structuredExecution: makeStructuredExecution(value.sharedExecution)}
	result.Trials = jsonMeasureExecutionTrials(value.Trials)
	if value.Cycles != nil {
		result.Cycles = new(jsonMeasureExecutionCycles(*value.Cycles))
	}
	return result
}
