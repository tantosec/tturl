package main

import (
	"time"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/internal/ranking"
)

// DetectRunRecord is the reproducible plan for one adaptive search.
type DetectRunRecord struct {
	BuildInfo                              buildinfo.Info
	Argv                                   []string
	StartedAt                              time.Time
	DialAddress                            string
	Origin                                 *string
	RequestCount                           int
	CandidateCount                         int
	BaselineTemplateCount                  int
	RequestedStrategy                      detectStrategy
	ResolvedStrategy                       detectStrategy
	SolverMethod                           ranking.OutlierMethodID
	FalsePositiveRisk                      float64
	FalseNegativeRisk                      float64
	Direction                              ranking.OutlierDirection
	WidthRequested                         int
	WidthEffective                         int
	ComparisonsMax                         int
	ConfiguredNegativeEdge                 float64
	Padding                                []PaddingRecord
	ResponseLimits                         responseLimits
	RunTimeout                             runTimeout
	WarmupTrialsPerConnection              int
	WarmupBatchWidth                       int
	ConnectionLimit                        int
	InFlightComparisonLimit                int
	PlannedInitialPrimingRequestOperations int
	PlannedMaximumPrimingRequestOperations int
	Delivery                               deliveryPlan
	ReleaseDelay                           time.Duration
	BodyBytesWithheld                      int
	Insecure                               bool
}

type detectReporter interface {
	Result(detectRunResult) error
}

type textDetectReporter struct {
	w       *reportWriter
	context detectReportContext
	width   int
	view    rankView
	refs    runReferences
}

func (r textDetectReporter) Result(result detectRunResult) error {
	if _, err := result.executionAccounting(
		r.context.ComparisonsMax, r.context.ConnectionLimit); err != nil {
		return err
	}
	reportDetectResult(r.w, result, r.context, r.width, r.view, r.refs)
	return r.w.Err()
}
