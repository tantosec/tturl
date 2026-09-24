package main

import (
	"errors"
	"fmt"
	"sort"

	"github.com/tantosec/tturl/tth2"
)

// analyseCollector retains the complete trial facts needed by the offline
// core. Any measured execution damage is an evidence issue, not a reason to
// select a favourable complete subset.
type analyseCollector struct {
	width              int
	warmupWidth        int
	plannedCycles      int
	cycles             map[int]*analyseCollectedCycle
	attemptedCycles    map[int]bool
	warmupTrials       int
	warmupRankComplete int
	warmupIncomplete   int
	attempted          int
	completed          int
	connections        map[int]bool
	connectionsPlanned int
	observations       []analyseTrialObservation
	capture            raceCaptureConfig
	observationSink    func(analyseTrialObservation) error
	retainObservations bool
	issue              *analyseEvidenceIssue
	completion         runCompletion
}

type analyseCollectedCycle struct {
	trials []analyseTrialEvidence
	shifts map[int]bool
}

func newAnalyseCollector(
	width, plannedCycles int, capture ...raceCaptureConfig,
) *analyseCollector {
	cfg := raceCaptureConfig{}
	if len(capture) > 0 {
		cfg = capture[0]
	}
	return &analyseCollector{
		width: width, warmupWidth: width, plannedCycles: plannedCycles,
		cycles:             make(map[int]*analyseCollectedCycle),
		attemptedCycles:    make(map[int]bool),
		connections:        make(map[int]bool),
		connectionsPlanned: 1,
		capture:            cfg,
		retainObservations: true,
	}
}

func newLiveAnalyseCollector(
	width, plannedCycles, warmupWidth, connectionsPlanned int,
	capture raceCaptureConfig,
	sink func(analyseTrialObservation) error,
	retain bool,
) *analyseCollector {
	collector := newAnalyseCollector(width, plannedCycles, capture)
	collector.warmupWidth = warmupWidth
	collector.connectionsPlanned = connectionsPlanned
	collector.observationSink = sink
	collector.retainObservations = retain
	return collector
}

func (collector *analyseCollector) observe(trial *tth2.Trial) {
	if trial == nil {
		collector.fail(analyseIncompleteCycle, "measured trial is missing")
		return
	}
	if trial.Conn >= 0 {
		collector.connections[trial.Conn] = true
	}
	if trial.Warmup {
		collector.warmupTrials++
		if validateAnalyseRankCompleteTrial(trial, collector.warmupWidth) == nil {
			collector.warmupRankComplete++
		} else {
			collector.warmupIncomplete++
		}
		return
	}
	collector.attempted++
	if trial.Cycle >= 0 && trial.Cycle < collector.plannedCycles {
		collector.attemptedCycles[trial.Cycle] = true
	}
	rankErr := validateAnalyseRankCompleteTrial(trial, collector.width)
	record := TrialRecord{Trial: trial}
	var captureErr error
	if rankErr == nil {
		record, captureErr = captureTrialRecord(trial, 0, collector.capture)
	}
	observation := analyseTrialObservation{
		Index: trial.Index, Cycle: trial.Cycle, Shift: trial.Shift,
		Connection:  trial.Conn,
		Dispatch:    trial.Dispatch,
		Arrangement: append([]int(nil), trial.Arrangement...),
		Outcomes:    append([]OutcomeRecord(nil), record.Outcomes...),
	}
	if trial.Err != nil {
		observation.Error = trial.Err.Error()
		observation.cause = trial.Err
		observation.Failure = classifyBatchFailure(trial.Err)
	}
	if collector.observationSink != nil {
		if err := collector.observationSink(observation); err != nil {
			collector.completion = failedRun(err, completionInternalError)
			collector.fail(analyseIncompleteCycle,
				fmt.Sprintf("record measured trial %d: %v", trial.Index, err))
		}
	}
	if collector.retainObservations {
		collector.observations = append(collector.observations, observation)
	}
	if captureErr != nil {
		collector.completion = failedRun(captureErr, completionInternalError)
		collector.fail(analyseIncompleteCycle,
			fmt.Sprintf("capture measured trial %d: %v", trial.Index, captureErr))
	}
	if rankErr != nil {
		collector.fail(analyseIncompleteCycle,
			fmt.Sprintf("measured trial %d is not rank-complete: %v",
				trial.Index, rankErr))
		return
	}
	collector.completed++
	if trial.Cycle < 0 || trial.Cycle >= collector.plannedCycles {
		collector.fail(analyseIncompleteCycle,
			fmt.Sprintf("measured trial %d has invalid cycle %d",
				trial.Index, trial.Cycle))
		return
	}
	if trial.Shift < 0 || trial.Shift >= collector.width {
		collector.fail(analyseInvalidShift,
			fmt.Sprintf("cycle %d has invalid shift %d", trial.Cycle, trial.Shift))
		return
	}
	cycle := collector.cycles[trial.Cycle]
	if cycle == nil {
		cycle = &analyseCollectedCycle{shifts: make(map[int]bool)}
		collector.cycles[trial.Cycle] = cycle
	}
	if cycle.shifts[trial.Shift] {
		collector.fail(analyseInvalidShift,
			fmt.Sprintf("cycle %d repeats shift %d", trial.Cycle, trial.Shift))
		return
	}
	cycle.shifts[trial.Shift] = true
	ranks := make([]int, collector.width)
	statuses := make([]int, collector.width)
	for request, result := range trial.Results {
		ranks[request] = result.ArrivalRank
		statuses[request] = result.Response.StatusCode
	}
	cycle.trials = append(cycle.trials, analyseTrialEvidence{
		Connection: trial.Conn, Shift: trial.Shift,
		Arrangement: append([]int(nil), trial.Arrangement...),
		Ranks:       ranks, Statuses: statuses,
	})
}

func validateAnalyseRankCompleteTrial(trial *tth2.Trial, width int) error {
	if trial.Err != nil {
		return trial.Err
	}
	if len(trial.Arrangement) != width || len(trial.Results) != width {
		return fmt.Errorf("trial facts have width %d/%d, want %d",
			len(trial.Arrangement), len(trial.Results), width)
	}
	if _, ok := trial.FullArrivalOrder(); !ok {
		return errors.New("incomplete arrival order")
	}
	seen := make([]bool, width)
	for _, request := range trial.Arrangement {
		if request < 0 || request >= width || seen[request] {
			return errors.New("invalid arrangement")
		}
		seen[request] = true
	}
	for _, result := range trial.Results {
		if result.Response == nil || result.Response.StatusCode < 100 ||
			result.Response.StatusCode > 999 {
			return errors.New("invalid HTTP response status")
		}
	}
	return nil
}

func (collector *analyseCollector) finish(runErr error) analyseEvidence {
	completion := normaliseCompletion(collector.completion)
	if runErr != nil {
		collector.fail(analyseInterrupted,
			fmt.Sprintf("fixed experiment did not finish: %v", runErr))
		if completion.State == completionComplete {
			completion = failedRun(runErr, completionAcquisitionFailed)
		}
	}
	cycleIDs := make([]int, 0, len(collector.cycles))
	for cycleID := range collector.cycles {
		cycleIDs = append(cycleIDs, cycleID)
	}
	sort.Ints(cycleIDs)
	replacements := 0
	for connection := range collector.connections {
		if connection >= collector.connectionsPlanned {
			replacements++
		}
	}
	evidence := analyseEvidence{
		Width:   collector.width,
		Capture: collector.capture,
		Observations: append(
			[]analyseTrialObservation(nil), collector.observations...),
		Execution: analyseExecutionEvidence{
			Recorded: true, PlannedCycles: collector.plannedCycles,
			WarmupTrials:            collector.warmupTrials,
			WarmupRankComplete:      collector.warmupRankComplete,
			WarmupIncomplete:        collector.warmupIncomplete,
			WarmupRequestOperations: collector.warmupTrials * collector.warmupWidth,
			AttemptedTrials:         collector.attempted,
			CompletedTrials:         collector.completed,
			AttemptedCycles:         len(collector.attemptedCycles),
			Connections:             len(collector.connections),
			Replacements:            replacements,
		},
		Completion: completion,
	}
	if !collector.retainObservations {
		evidence.Observations = []analyseTrialObservation{}
	}
	for _, cycleID := range cycleIDs {
		cycle := collector.cycles[cycleID]
		if len(cycle.trials) != collector.width ||
			len(cycle.shifts) != collector.width {
			collector.fail(analyseIncompleteCycle,
				fmt.Sprintf("cycle %d is incomplete", cycleID))
			continue
		}
		evidence.Cycles = append(evidence.Cycles, analyseCycleEvidence{
			Trials: append([]analyseTrialEvidence(nil), cycle.trials...),
		})
	}
	if len(collector.cycles) != collector.plannedCycles {
		collector.fail(analyseIncompleteCycle,
			fmt.Sprintf("received %d of %d planned cycles", len(collector.cycles),
				collector.plannedCycles))
	}
	if collector.issue != nil {
		issue := *collector.issue
		evidence.Issue = &issue
	}
	return evidence
}

func (collector *analyseCollector) fail(code analyseValidityCode, detail string) {
	if collector.issue == nil {
		collector.issue = &analyseEvidenceIssue{Code: code, Detail: detail}
	}
}
