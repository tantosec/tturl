package main

import (
	"fmt"
)

// executionConnections counts command-visible connection identities. A
// connection is observed only after it appears in a recorded batch.
type executionConnections struct {
	Planned      int
	Observed     int
	Replacements int
}

// executionBatches counts command-visible logical batch attempts.
type executionBatches struct {
	Attempted    int
	RankComplete int
	Incomplete   int
}

// executionPrimingOperations separates priming work from measured work.
type executionPrimingOperations struct {
	Attempted    int
	RankComplete int
}

type executionPriming struct {
	Batches           executionBatches
	RequestOperations executionPrimingOperations
}

// executionRequestOperations counts logical positions in measured batches.
type executionRequestOperations struct {
	Attempted    int
	RankComplete int
	Retained     int
}

type sharedExecution struct {
	Connections       executionConnections
	Priming           executionPriming
	RequestOperations executionRequestOperations
}

type raceExecutionTrials struct {
	Maximum      *int
	Attempted    int
	RankComplete int
	Incomplete   int
	Unattempted  *int
}

type raceExecution struct {
	sharedExecution
	Trials raceExecutionTrials
}

func projectRaceExecution(
	maximum, plannedConnections, measuredWidth, primingWidth int,
	connections map[int]bool,
	priming, measured executionBatches,
) (raceExecution, error) {
	var maximumValue, unattempted *int
	if maximum >= 0 {
		maximumCopy := maximum
		unattemptedCopy := maximum - measured.Attempted
		maximumValue, unattempted = &maximumCopy, &unattemptedCopy
	}
	replacements := 0
	for id := range connections {
		if id >= plannedConnections {
			replacements++
		}
	}
	primingAttempted, primingAttemptedOK := checkedProduct(
		priming.Attempted, primingWidth)
	primingComplete, primingCompleteOK := checkedProduct(
		priming.RankComplete, primingWidth)
	measuredAttempted, measuredAttemptedOK := checkedProduct(
		measured.Attempted, measuredWidth)
	measuredComplete, measuredCompleteOK := checkedProduct(
		measured.RankComplete, measuredWidth)
	if !primingAttemptedOK || !primingCompleteOK ||
		!measuredAttemptedOK || !measuredCompleteOK {
		return raceExecution{}, fmt.Errorf(
			"execution race request-operation count cannot be represented")
	}
	execution := raceExecution{
		sharedExecution: sharedExecution{
			Connections: executionConnections{
				Planned: plannedConnections, Observed: len(connections),
				Replacements: replacements,
			},
			Priming: executionPriming{
				Batches: priming,
				RequestOperations: executionPrimingOperations{
					Attempted: primingAttempted, RankComplete: primingComplete,
				},
			},
			RequestOperations: executionRequestOperations{
				Attempted: measuredAttempted, RankComplete: measuredComplete,
				Retained: measuredComplete,
			},
		},
		Trials: raceExecutionTrials{
			Maximum: maximumValue, Attempted: measured.Attempted,
			RankComplete: measured.RankComplete, Incomplete: measured.Incomplete,
			Unattempted: unattempted,
		},
	}
	if err := validateRaceExecution(
		execution, measuredWidth, primingWidth); err != nil {
		return raceExecution{}, err
	}
	return execution, nil
}

func validateRaceExecution(execution raceExecution, measuredWidth,
	primingWidth int,
) error {
	if err := validateSharedExecution(execution.sharedExecution); err != nil {
		return err
	}
	trials := execution.Trials
	if trials.Attempted < 0 || trials.RankComplete < 0 ||
		trials.Incomplete < 0 {
		return fmt.Errorf("execution race trial counts must be non-negative")
	}
	if err := validateExecutionBatches("race trials", executionBatches{
		Attempted: trials.Attempted, RankComplete: trials.RankComplete,
		Incomplete: trials.Incomplete,
	}); err != nil {
		return err
	}
	if (trials.Maximum == nil) != (trials.Unattempted == nil) {
		return fmt.Errorf(
			"execution race maximum and unattempted must both be null or finite")
	}
	if trials.Maximum != nil {
		if *trials.Maximum < 0 || *trials.Unattempted < 0 ||
			trials.Attempted > *trials.Maximum ||
			*trials.Unattempted != *trials.Maximum-trials.Attempted {
			return fmt.Errorf("execution race finite trial counts have invalid arithmetic")
		}
	}
	measured := execution.RequestOperations
	wantAttempted, ok := checkedProduct(trials.Attempted, measuredWidth)
	if !ok || measured.Attempted != wantAttempted {
		return fmt.Errorf(
			"execution race attempted request operations disagree with trial width")
	}
	wantComplete, ok := checkedProduct(trials.RankComplete, measuredWidth)
	if !ok || measured.RankComplete != wantComplete ||
		measured.Retained != wantComplete {
		return fmt.Errorf(
			"execution race retained request operations disagree with rank-complete trials")
	}
	return validateFixedWidthExecution("priming", execution.Priming.Batches,
		execution.Priming.RequestOperations, primingWidth)
}

type measureExecutionTrials struct {
	Planned      int
	Attempted    int
	RankComplete int
	Incomplete   int
	Unattempted  int
	Retained     int
	Excluded     int
}

type measureExecutionCycles struct {
	Planned     int
	Attempted   int
	Retained    int
	Incomplete  int
	Unattempted int
}

type measureExecution struct {
	sharedExecution
	Trials measureExecutionTrials
	Cycles *measureExecutionCycles
}

func validateMeasureExecution(execution measureExecution, measuredWidth,
	primingWidth int,
) error {
	if err := validateSharedExecution(execution.sharedExecution); err != nil {
		return err
	}
	trials := execution.Trials
	values := []int{
		trials.Planned, trials.Attempted, trials.RankComplete,
		trials.Incomplete, trials.Unattempted, trials.Retained,
		trials.Excluded,
	}
	for _, value := range values {
		if value < 0 {
			return fmt.Errorf("execution measure trial counts must be non-negative")
		}
	}
	if trials.Attempted > trials.Planned ||
		trials.Incomplete != trials.Attempted-trials.RankComplete ||
		trials.Unattempted != trials.Planned-trials.Attempted ||
		trials.Excluded != trials.RankComplete-trials.Retained {
		return fmt.Errorf("execution measure trial counts have invalid arithmetic")
	}
	measured := execution.RequestOperations
	wantAttempted, ok := checkedProduct(trials.Attempted, measuredWidth)
	if !ok || measured.Attempted != wantAttempted {
		return fmt.Errorf(
			"execution measure attempted request operations disagree with trial width")
	}
	wantComplete, ok := checkedProduct(trials.RankComplete, measuredWidth)
	if !ok || measured.RankComplete != wantComplete {
		return fmt.Errorf(
			"execution measure rank-complete request operations disagree with trial width")
	}
	wantRetained, ok := checkedProduct(trials.Retained, measuredWidth)
	if !ok || measured.Retained != wantRetained {
		return fmt.Errorf(
			"execution measure retained request operations disagree with trial width")
	}
	if err := validateFixedWidthExecution("priming", execution.Priming.Batches,
		execution.Priming.RequestOperations, primingWidth); err != nil {
		return err
	}
	if execution.Cycles == nil {
		return nil
	}
	cycles := *execution.Cycles
	cycleValues := []int{
		cycles.Planned, cycles.Attempted, cycles.Retained,
		cycles.Incomplete, cycles.Unattempted,
	}
	for _, value := range cycleValues {
		if value < 0 {
			return fmt.Errorf("execution measure cycle counts must be non-negative")
		}
	}
	if cycles.Attempted > cycles.Planned ||
		cycles.Retained > cycles.Attempted ||
		cycles.Incomplete != cycles.Attempted-cycles.Retained ||
		cycles.Unattempted != cycles.Planned-cycles.Attempted {
		return fmt.Errorf("execution measure cycle counts have invalid arithmetic")
	}
	plannedTrials, ok := checkedProduct(cycles.Planned, measuredWidth)
	if !ok || plannedTrials != trials.Planned {
		return fmt.Errorf(
			"execution measure planned cycles disagree with trial width")
	}
	retainedTrials, ok := checkedProduct(cycles.Retained, measuredWidth)
	if !ok || retainedTrials != trials.Retained {
		return fmt.Errorf(
			"execution measure retained cycles disagree with retained trials")
	}
	maximumAttempted, ok := checkedProduct(cycles.Attempted, measuredWidth)
	if !ok || cycles.Attempted > trials.Attempted ||
		trials.Attempted > maximumAttempted {
		return fmt.Errorf(
			"execution measure attempted cycles disagree with attempted trials")
	}
	return nil
}

type analyseExecutionTrials struct {
	Planned      int
	Attempted    int
	RankComplete int
	Incomplete   int
	Unattempted  int
	Retained     int
	Excluded     int
}

type analyseExecutionCycles struct {
	Planned     int
	Attempted   int
	Complete    int
	Incomplete  int
	Unattempted int
	Retained    int
}

type analyseExecution struct {
	sharedExecution
	Trials analyseExecutionTrials
	Cycles analyseExecutionCycles
}

func validateAnalyseExecution(execution analyseExecution, measuredWidth,
	primingWidth int,
) error {
	if err := validateSharedExecution(execution.sharedExecution); err != nil {
		return err
	}
	trials := execution.Trials
	values := []int{
		trials.Planned, trials.Attempted, trials.RankComplete,
		trials.Incomplete, trials.Unattempted, trials.Retained,
		trials.Excluded,
	}
	for _, value := range values {
		if value < 0 {
			return fmt.Errorf("execution analyse trial counts must be non-negative")
		}
	}
	if trials.Attempted > trials.Planned ||
		trials.Incomplete != trials.Attempted-trials.RankComplete ||
		trials.Unattempted != trials.Planned-trials.Attempted ||
		trials.Excluded != trials.RankComplete-trials.Retained {
		return fmt.Errorf("execution analyse trial counts have invalid arithmetic")
	}
	measured := execution.RequestOperations
	wantAttempted, ok := checkedProduct(trials.Attempted, measuredWidth)
	if !ok || measured.Attempted != wantAttempted {
		return fmt.Errorf(
			"execution analyse attempted request operations disagree with trial width")
	}
	wantComplete, ok := checkedProduct(trials.RankComplete, measuredWidth)
	if !ok || measured.RankComplete != wantComplete {
		return fmt.Errorf(
			"execution analyse rank-complete request operations disagree with trial width")
	}
	wantRetained, ok := checkedProduct(trials.Retained, measuredWidth)
	if !ok || measured.Retained != wantRetained {
		return fmt.Errorf(
			"execution analyse retained request operations disagree with trial width")
	}
	if err := validateFixedWidthExecution("priming", execution.Priming.Batches,
		execution.Priming.RequestOperations, primingWidth); err != nil {
		return err
	}
	cycles := execution.Cycles
	cycleValues := []int{
		cycles.Planned, cycles.Attempted, cycles.Complete,
		cycles.Incomplete, cycles.Unattempted, cycles.Retained,
	}
	for _, value := range cycleValues {
		if value < 0 {
			return fmt.Errorf("execution analyse cycle counts must be non-negative")
		}
	}
	if cycles.Attempted > cycles.Planned || cycles.Complete > cycles.Attempted ||
		cycles.Retained > cycles.Complete ||
		cycles.Incomplete != cycles.Attempted-cycles.Complete ||
		cycles.Unattempted != cycles.Planned-cycles.Attempted {
		return fmt.Errorf("execution analyse cycle counts have invalid arithmetic")
	}
	plannedTrials, ok := checkedProduct(cycles.Planned, measuredWidth)
	if !ok || plannedTrials != trials.Planned {
		return fmt.Errorf(
			"execution analyse planned cycles disagree with trial width")
	}
	completeTrials, ok := checkedProduct(cycles.Complete, measuredWidth)
	if !ok || completeTrials > trials.RankComplete {
		return fmt.Errorf(
			"execution analyse complete cycles exceed rank-complete trials")
	}
	retainedTrials, ok := checkedProduct(cycles.Retained, measuredWidth)
	if !ok || retainedTrials != trials.Retained {
		return fmt.Errorf(
			"execution analyse retained cycles disagree with retained trials")
	}
	maximumAttempted, ok := checkedProduct(cycles.Attempted, measuredWidth)
	if !ok || cycles.Attempted > trials.Attempted ||
		trials.Attempted > maximumAttempted {
		return fmt.Errorf(
			"execution analyse attempted cycles disagree with attempted trials")
	}
	return nil
}

type detectExecutionComparisons struct {
	Maximum      *int
	Attempted    int
	RankComplete int
	Incomplete   int
}

type detectExecution struct {
	sharedExecution
	Comparisons detectExecutionComparisons
}

func validateDetectExecution(execution detectExecution) error {
	if err := validateSharedExecution(execution.sharedExecution); err != nil {
		return err
	}
	comparisons := execution.Comparisons
	if comparisons.Attempted < 0 || comparisons.RankComplete < 0 ||
		comparisons.Incomplete < 0 ||
		comparisons.RankComplete > comparisons.Attempted ||
		comparisons.Incomplete != comparisons.Attempted-comparisons.RankComplete {
		return fmt.Errorf("execution detect comparison counts have invalid arithmetic")
	}
	if comparisons.Maximum != nil &&
		(*comparisons.Maximum < 0 || comparisons.Attempted > *comparisons.Maximum) {
		return fmt.Errorf("execution detect comparison maximum is invalid")
	}
	return nil
}

func validateSharedExecution(execution sharedExecution) error {
	values := []struct {
		name  string
		value int
	}{
		{"connections.planned", execution.Connections.Planned},
		{"connections.observed", execution.Connections.Observed},
		{"connections.replacements", execution.Connections.Replacements},
		{"priming.batches.attempted", execution.Priming.Batches.Attempted},
		{"priming.batches.rank_complete", execution.Priming.Batches.RankComplete},
		{"priming.batches.incomplete", execution.Priming.Batches.Incomplete},
		{
			"priming.request_operations.attempted",
			execution.Priming.RequestOperations.Attempted,
		},
		{
			"priming.request_operations.rank_complete",
			execution.Priming.RequestOperations.RankComplete,
		},
		{"request_operations.attempted", execution.RequestOperations.Attempted},
		{
			"request_operations.rank_complete",
			execution.RequestOperations.RankComplete,
		},
		{"request_operations.retained", execution.RequestOperations.Retained},
	}
	for _, value := range values {
		if value.value < 0 {
			return fmt.Errorf("execution %s must be non-negative", value.name)
		}
	}
	if execution.Connections.Replacements > execution.Connections.Observed {
		return fmt.Errorf(
			"execution connection replacements exceed observed connections")
	}
	if err := validateExecutionBatches(
		"priming", execution.Priming.Batches); err != nil {
		return err
	}
	priming := execution.Priming.RequestOperations
	if priming.RankComplete > priming.Attempted {
		return fmt.Errorf(
			"execution priming rank-complete request operations exceed attempts")
	}
	measured := execution.RequestOperations
	if measured.Retained > measured.RankComplete ||
		measured.RankComplete > measured.Attempted {
		return fmt.Errorf(
			"execution measured request operations have invalid retention arithmetic")
	}
	return nil
}

func validateExecutionBatches(name string, batches executionBatches) error {
	if batches.RankComplete > batches.Attempted ||
		batches.Incomplete != batches.Attempted-batches.RankComplete {
		return fmt.Errorf("execution %s batches have invalid arithmetic", name)
	}
	return nil
}

func validateFixedWidthExecution(
	name string,
	batches executionBatches,
	operations executionPrimingOperations,
	width int,
) error {
	if width < 0 {
		return fmt.Errorf("execution %s batch width must be non-negative", name)
	}
	attempted, ok := checkedProduct(batches.Attempted, width)
	if !ok || attempted != operations.Attempted {
		return fmt.Errorf(
			"execution %s attempted request operations disagree with batch width",
			name)
	}
	rankComplete, ok := checkedProduct(batches.RankComplete, width)
	if !ok || rankComplete != operations.RankComplete {
		return fmt.Errorf(
			"execution %s rank-complete request operations disagree with batch width",
			name)
	}
	return nil
}
