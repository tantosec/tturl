package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
)

type analyseReportContext struct {
	Delivery                  deliveryPlan
	WarmupTrialsPerConnection int
	WarmupBatchWidth          int
	Padding                   []PaddingRecord
	BodyRelease               bodyReleasePlan
	ResponseLimits            responseLimits
	RunTimeout                runTimeout
	Insecure                  bool
}

// reportAnalyse renders one compact inferential narrative from the typed
// result. It does not reconstruct decisions from numeric fields.
func reportAnalyse(
	w io.Writer, result analyseRunResult, labels []string,
) error {
	return reportAnalyseDocumentWithContext(
		w, result, labels, true, analyseReportContext{
			Delivery: deliveryPlan{
				connectionLimit:     result.ConnectionLimit,
				connectionCeiling:   result.ConnectionLimit,
				connectionSelection: connectionDefault,
			},
			ResponseLimits: defaultResponseLimits(),
		})
}

func reportAnalyseDocumentWithContext(
	w io.Writer, result analyseRunResult, labels []string, bannered bool,
	context analyseReportContext,
) error {
	if err := validateAnalyseLabels(result, labels); err != nil {
		return err
	}
	if _, err := result.executionAccounting(context.WarmupBatchWidth); err != nil {
		return err
	}
	reportOutput := newReportWriter(w)
	if bannered {
		writeBanner(reportOutput, toolName+" analyse")
	}
	if err := reportAnalyseExperiment(reportOutput, result, context); err != nil {
		return err
	}
	writeHeading(reportOutput, "Analysis result")

	switch result.Global.Outcome {
	case analyseDifference:
		emitln(reportOutput,
			"Finding: Evidence of a request-identity effect on relative arrival order")
		reportAnalyseEvidence(reportOutput, result)
		reportAnalyseGlobalEvidence(reportOutput, result.Global)
		reportAnalysePairs(reportOutput, result.Pairs, labels)
		reportAnalyseRankProfile(reportOutput, result.Requests, labels)
		writeWrappedASCII(reportOutput, "",
			"Interpretation: Evidence supports a relative arrival-order difference "+
				"under this experiment's requests and delivery conditions. It does not "+
				"establish the cause, an elapsed-time gap, or practical significance.")
		reportAnalyseSupportingEvidence(reportOutput, result, labels)
	case analyseNoDetection:
		writeWrappedASCII(reportOutput, "Finding: ",
			"No request-dependent arrival-order effect detected. This is not "+
				"evidence of equivalence.")
		reportAnalyseEvidence(reportOutput, result)
		reportAnalyseGlobalEvidence(reportOutput, result.Global)
		reportAnalyseRankProfile(reportOutput, result.Requests, labels)
		emitln(reportOutput,
			"Pair comparisons: not tested without a field-wide detection.")
		reportAnalyseSupportingEvidence(reportOutput, result, labels)
	case analyseUnavailable:
		writeWrappedASCII(reportOutput, "Finding unavailable: ",
			"measured evidence did not pass fixed-design validation.")
		writeWrappedASCII(reportOutput, "Reason: ", result.Validity.Detail+".")
		emitf(reportOutput, "Code: %s.\n", result.Validity.Code)
		emitln(reportOutput, "No statistical result or pair comparison was produced.")
		reportAnalyseExecutionSection(reportOutput, result)
	default:
		return fmt.Errorf("internal consistency error: unknown analyse outcome %q",
			result.Global.Outcome)
	}
	if result.Validity.Available {
		reportAnalyseMethod(reportOutput, result, labels)
	}
	return reportOutput.Err()
}

func reportAnalyseExperiment(
	w io.Writer, result analyseRunResult, context analyseReportContext,
) error {
	writeHeading(w, "Experiment")
	writeWrappedASCII(w, "Design: ", fmt.Sprintf(
		"%s (%s, %s).",
		countedNoun(result.Plan.Cycles, "balanced rotation cycle"),
		countedNoun(result.Plan.Trials, "trial"),
		countedNoun(result.Plan.Operations, "measured request operation")))
	writeWrappedASCII(w, "Arrangement: ",
		"rotate; each request occupies every position once per cycle.")
	if context.WarmupTrialsPerConnection == 0 {
		writeWrappedASCII(w, "Priming: ",
			"none; early trials may include cold-state effects.")
	} else if context.Delivery.pathFitUnresolved {
		warmupWidth := context.WarmupBatchWidth
		if warmupWidth == 0 {
			warmupWidth = result.Width
		}
		perConnection, ok := checkedProduct(
			context.WarmupTrialsPerConnection, warmupWidth)
		if !ok {
			return fmt.Errorf("analyse warmup operation count cannot be represented")
		}
		writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
			"%s per connection (%s per connection); total unresolved until execution; excluded from inference.",
			countedNoun(context.WarmupTrialsPerConnection, "priming attempt"),
			countedNoun(perConnection, "initial priming request operation")))
	} else {
		warmupWidth := context.WarmupBatchWidth
		if warmupWidth == 0 {
			warmupWidth = result.Width
		}
		operations, ok := checkedProduct(
			context.WarmupTrialsPerConnection,
			context.Delivery.connectionLimit, warmupWidth)
		if !ok {
			return fmt.Errorf("analyse warmup operation count cannot be represented")
		}
		writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
			"%s per connection (%s), excluded from inference.",
			countedNoun(context.WarmupTrialsPerConnection, "priming attempt"),
			countedNoun(operations, "initial priming request operation")))
	}
	reportRacePadding(w, context.Padding)
	if result.Plan.LowerSensitivity {
		writeWrappedASCII(w, "Sensitivity: ", fmt.Sprintf(
			"%d cycles is below the %d-cycle modelled default; this run has "+
				"lower sensitivity than the validated planning target.",
			result.Plan.Cycles, result.Plan.DefaultCycles))
	}
	writeWrappedASCII(w, "Power: ",
		"the fixed workload is a power choice, not a guarantee for this target.")
	if err := reportDeliveryBatchWidths(
		w, context.Delivery, result.Width,
		context.WarmupBatchWidth); err != nil {
		return err
	}
	reportReleasePlan(w, context.BodyRelease)
	reportResponseLimits(w, context.ResponseLimits)
	reportRunTimeout(w, context.RunTimeout)
	reportTLSPlan(w, context.Insecure)
	return nil
}

func validateAnalyseLabels(result analyseRunResult, labels []string) error {
	if len(labels) != result.Width {
		return fmt.Errorf("internal consistency error: analyse result has %d labels; want %d",
			len(labels), result.Width)
	}
	switch result.Global.Outcome {
	case analyseDifference, analyseNoDetection, analyseUnavailable:
	default:
		return fmt.Errorf("internal consistency error: unknown analyse outcome %q",
			result.Global.Outcome)
	}
	return nil
}

func reportAnalyseEvidence(w io.Writer, result analyseRunResult) {
	emitf(w, "Evidence: %d cycles; %d trials; %d measured request operations.\n",
		result.Cycles, result.Trials, result.Operations)
}

func reportAnalyseGlobalEvidence(w io.Writer, result analyseGlobalResult) {
	calibration := result.Calibration
	switch result.Outcome {
	case analyseDifference:
		if calibration.PValueKind == "exact" {
			emitln(w, "Overall test across all requests:")
			emitf(w, "  Exact randomisation: p=%s <= %.2f.\n",
				analysePValue(calibration.PValue), result.Alpha)
			emitf(w, "  Reference distribution: all %s randomisations evaluated.\n",
				analyseCount(calibration.Draws))
			return
		}
		emitln(w, "Overall test across all requests:")
		emitf(w, "  Monte Carlo randomisation: p=%s <= %.2f.\n",
			analysePValue(calibration.PValue), result.Alpha)
		emitf(w, "  Reference distribution: %s randomisations sampled.\n",
			analyseCount(calibration.Draws))
	case analyseNoDetection:
		if calibration.PValueKind == "exact" {
			emitln(w, "Overall test across all requests:")
			emitf(w, "  Exact randomisation: p=%s > %.2f.\n",
				analysePValue(calibration.PValue), result.Alpha)
			emitf(w, "  Reference distribution: all %s randomisations evaluated.\n",
				analyseCount(calibration.Draws))
			return
		}
		emitln(w, "Overall test across all requests:")
		emitf(w, "  Monte Carlo randomisation: p=%s > %.2f.\n",
			analysePValue(calibration.PValue), result.Alpha)
		emitf(w, "  Reference distribution: %s randomisations sampled.\n",
			analyseCount(calibration.Draws))
	}
}

func analyseCount(value int) string {
	plain := strconv.Itoa(value)
	for index := len(plain) - 3; index > 0; index -= 3 {
		plain = plain[:index] + "," + plain[index:]
	}
	return plain
}

func analysePValue(value float64) string {
	return fmt.Sprintf("%.4g", value)
}

func reportAnalysePairs(
	w io.Writer, result analysePairResults, labels []string,
) {
	selected := make([]analysePairResult, 0, len(result.Pairs))
	for _, pair := range result.Pairs {
		if pair.Selected {
			selected = append(selected, pair)
		}
	}
	if len(selected) == 0 {
		emitf(w, "Holm-adjusted pair evidence: 0/%d pairs supported.\n",
			len(result.Pairs))
		return
	}
	pairWord := "pairs"
	if len(result.Pairs) == 1 {
		pairWord = "pair"
	}
	emitf(w, "Holm-adjusted pair evidence: %d/%d %s supported.\n",
		len(selected), len(result.Pairs), pairWord)

	earlierWidth, laterWidth := len("earlier"), len("later")
	for _, pair := range selected {
		earlierWidth = max(earlierWidth, len(labels[pair.Left]), len(labels[pair.Right]))
		laterWidth = max(laterWidth, len(labels[pair.Left]), len(labels[pair.Right]))
	}
	emitf(w, "  %-*s  %-*s  %13s  %13s  %10s\n",
		earlierWidth, "earlier", laterWidth, "later", "before/trials",
		"mean rank gap", "adjusted p")
	for _, pair := range selected {
		later, earlier := pair.Left, pair.Right
		earlierCount := pair.RightBeforeLeft
		difference := pair.MeanRankDifference
		if difference < 0 {
			later, earlier = pair.Right, pair.Left
			earlierCount = pair.LeftBeforeRight
			difference = -difference
		}
		before := fmt.Sprintf("%d/%d", earlierCount, pair.Trials)
		emitf(w, "  %-*s  %-*s  %13s  %13.2f  %10s\n",
			earlierWidth, labels[earlier], laterWidth, labels[later], before,
			difference, analysePairPValueCell(
				pair.AdjustedP, pair.AdjustedPUpperBound))
	}
}

func analysePairPValueCell(value float64, upperBound bool) string {
	if upperBound {
		return "<=" + analysePValue(value)
	}
	return analysePValue(value)
}

func reportAnalyseMethod(
	w io.Writer, result analyseRunResult, labels []string,
) {
	calibration := result.Global.Calibration
	writeHeading(w, "Method")
	emitf(w,
		"Global A=max(L2,Linf/sqrt(2)): A=%.6g, L2=%.6g, Linf=%.6g.\n",
		result.Global.Statistic, result.Global.Components.L2,
		result.Global.Components.LInfinity)
	emitf(w, "Calibration: %s; alpha=%.2f.\n",
		calibration.Method, result.Global.Alpha)
	emitf(w, "Calibration completion: %s.\n",
		calibration.CompletionReason)
	if calibration.SeedUsed {
		emitf(w, "Reproduction: seed=%d; draws=%d; extremes=%d.\n",
			calibration.RandomisationSeed, calibration.Draws,
			calibration.Extremes)
	}
	if result.Pairs.State != analysePairsPerformed {
		return
	}
	emitln(w, "All pair tests (two-sided R with Holm adjustment):")
	for _, pair := range result.Pairs.Pairs {
		state := "not selected"
		if pair.Selected {
			state = "selected"
		}
		emitf(w, "  %s vs %s: %s; %s; %s.\n",
			labels[pair.Left], labels[pair.Right],
			analysePairPValue("raw p", pair.RawP, pair.RawPUpperBound),
			analysePairPValue("adjusted p", pair.AdjustedP,
				pair.AdjustedPUpperBound), state)
	}
}

func analysePairPValue(label string, value float64, upperBound bool) string {
	if upperBound {
		return fmt.Sprintf("%s<=%s", label, analysePValue(value))
	}
	return fmt.Sprintf("%s=%s", label, analysePValue(value))
}

func reportAnalyseRankProfile(
	w io.Writer, effects []analyseRequestEffect, labels []string,
) {
	entries := make([]rankProfileEntry, len(effects))
	for i, effect := range effects {
		entries[i] = rankProfileEntry{
			label: labels[effect.Request], meanRank: effect.MeanRank,
			rankCounts: effect.RankCounts, samples: effect.Samples,
		}
	}
	emitln(w)
	reportRankProfile(w, entries, rankView{allRows: true})
	emitln(w)
}

func reportAnalyseSupportingEvidence(
	w io.Writer, result analyseRunResult, labels []string,
) {
	writeHeading(w, "HTTP status outcomes")
	reportAnalyseHTTPStatuses(w, result, labels)
	writeHeading(w, "Position check")
	reportAnalysePositionCheck(w, result.Positions)
	reportAnalyseExecutionSection(w, result)
}

func reportAnalyseHTTPStatuses(
	w io.Writer, result analyseRunResult, labels []string,
) {
	if status, uniform := analyseUniformStatus(result.Requests); uniform {
		emitf(w, "HTTP %d for all %d ranked responses.\n",
			status, result.Operations)
	} else {
		emitln(w, "Ranked responses by request:")
		width := labelColumn(labels)
		for _, request := range result.Requests {
			writeWrappedASCII(w,
				fmt.Sprintf("  %-*s  ", width, labels[request.Request]),
				analyseStatusDistribution(request))
		}
	}

	review := result.Responses
	if len(review.Changing) > 0 {
		possessive := "Their rank distributions combine"
		if len(review.Changing) == 1 {
			possessive = labels[review.Changing[0]] +
				"'s rank distribution combines"
		}
		writeWrappedASCII(w, "Review: ", fmt.Sprintf(
			"%s different HTTP outcomes.", possessive))
	}
	if len(review.Different) > 0 {
		writeWrappedASCII(w, "Review: ", fmt.Sprintf(
			"status distributions differ across %s; inspect responses before "+
				"linking arrival-order behaviour to request identity.",
			joinRequestLabels(labels, review.Different)))
	}
	if len(review.Non2xx) == len(result.Requests) &&
		len(result.Requests) > 0 {
		emitln(w, "Review: non-2xx responses contributed ranks for every request.")
	} else if len(review.Non2xx) > 0 {
		writeWrappedASCII(w, "Review: ", fmt.Sprintf(
			"non-2xx responses contributed ranks for %s.",
			joinRequestLabels(labels, review.Non2xx)))
	}
}

func analyseUniformStatus(requests []analyseRequestEffect) (int, bool) {
	status := 0
	for index, request := range requests {
		if len(request.StatusCounts) != 1 ||
			request.StatusCounts[0].Count != request.Samples {
			return 0, false
		}
		if index > 0 && request.StatusCounts[0].Status != status {
			return 0, false
		}
		status = request.StatusCounts[0].Status
	}
	return status, len(requests) > 0
}

func analyseStatusDistribution(request analyseRequestEffect) string {
	if len(request.StatusCounts) == 1 &&
		request.StatusCounts[0].Count == request.Samples {
		return fmt.Sprintf("HTTP %d in all %d trials",
			request.StatusCounts[0].Status, request.Samples)
	}
	parts := make([]string, len(request.StatusCounts))
	for index, count := range request.StatusCounts {
		parts[index] = fmt.Sprintf("%d x %d", count.Status, count.Count)
	}
	return strings.Join(parts, ", ")
}

func reportAnalysePositionCheck(w io.Writer, positions []analysePositionEffect) {
	minimum, maximum := positions[0].MeanRank, positions[0].MeanRank
	for _, position := range positions[1:] {
		minimum = min(minimum, position.MeanRank)
		maximum = max(maximum, position.MeanRank)
	}
	spread := maximum - minimum
	detail := fmt.Sprintf(
		"%.2f-%.2f across outbound positions (spread %.2f).",
		minimum, maximum, spread)
	if len(positions) > 2 {
		detail = fmt.Sprintf(
			"%.2f-%.2f across outbound positions "+
				"(spread %.2f; normalised %.2f).",
			minimum, maximum, spread, spread/float64(len(positions)-1))
	}
	writeWrappedASCII(w, "Mean ranks: ", detail)
}

func reportAnalyseExecutionSection(w io.Writer, result analyseRunResult) {
	execution, err := result.executionAccounting(0)
	if err != nil {
		writeHeading(w, "Execution")
		writeWrappedASCII(w, "Execution unavailable: ", err.Error()+".")
		return
	}
	writeHeading(w, "Execution")
	writeWrappedASCII(w, "Trials: ", fmt.Sprintf(
		"%d/%d attempted; %d rank-complete; %d incomplete; %d retained; "+
			"%d excluded; %d unattempted.", execution.Trials.Attempted,
		execution.Trials.Planned, execution.Trials.RankComplete,
		execution.Trials.Incomplete, execution.Trials.Retained,
		execution.Trials.Excluded, execution.Trials.Unattempted))
	writeWrappedASCII(w, "Cycles: ", fmt.Sprintf(
		"%d/%d attempted; %d complete; %d incomplete; %d retained; "+
			"%d unattempted.", execution.Cycles.Attempted,
		execution.Cycles.Planned, execution.Cycles.Complete,
		execution.Cycles.Incomplete, execution.Cycles.Retained,
		execution.Cycles.Unattempted))
	writeWrappedASCII(w, "Connections: ", fmt.Sprintf(
		"%d planned; %d observed; %d replacements.",
		execution.Connections.Planned, execution.Connections.Observed,
		execution.Connections.Replacements))
	if execution.Priming.Batches.Attempted > 0 {
		writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
			"%d attempted; %d rank-complete; %d incomplete; excluded from inference.",
			execution.Priming.Batches.Attempted,
			execution.Priming.Batches.RankComplete,
			execution.Priming.Batches.Incomplete))
		writeWrappedASCII(w, "Priming work: ", fmt.Sprintf(
			"%d request operations attempted; %d rank-complete; excluded from inference.",
			execution.Priming.RequestOperations.Attempted,
			execution.Priming.RequestOperations.RankComplete))
		if execution.Priming.Batches.Incomplete > 0 {
			writeWrappedASCII(w, "Caution: ",
				"incomplete priming may leave cold-state effects in measured trials.")
		}
	}
	writeWrappedASCII(w, "Measured request operations: ", fmt.Sprintf(
		"%d attempted; %d rank-complete; %d retained.",
		execution.RequestOperations.Attempted,
		execution.RequestOperations.RankComplete,
		execution.RequestOperations.Retained))
	completion := normaliseCompletion(result.Completion)
	if completion.State != completionComplete {
		emitf(w, "Completion: %s.\n", completion.State)
	}
	if completion.Detail != "" {
		emitf(w, "Error: %s\n", curlblocks.DisplayText(completion.Detail))
	}
}
