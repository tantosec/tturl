package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/tth2"
)

// measurementReporter presents the fixed experiment independently of its
// output format. Run is called once the stream's resolved concurrency is known;
// Summary receives the completed descriptive observation.
type measurementReporter interface {
	Run(concurrency int) error
	Summary(*measurement) error
}

type textMeasurementReporter struct {
	w    *reportWriter
	view rankView
	refs runReferences
	plan measurePlan
}

func newTextMeasurementReporter(
	w *reportWriter,
	view rankView,
	refs runReferences,
	plan measurePlan,
) *textMeasurementReporter {
	return &textMeasurementReporter{w: w, view: view, refs: refs, plan: plan}
}

func (r *textMeasurementReporter) Run(concurrency int) error {
	return reportMeasureExperiment(r.w, r.plan, concurrency)
}

func (r *textMeasurementReporter) Summary(measured *measurement) error {
	if _, err := measured.execution(); err != nil {
		return err
	}
	reportMeasurement(r.w, measured, r.view, r.refs)
	return r.w.Err()
}

// reportMeasurement presents one fixed-batch experiment: rank summaries first,
// followed by the execution facts needed to interpret them. It makes no
// inferential finding.
func reportMeasurement(
	w io.Writer,
	measured *measurement,
	view rankView,
	refs runReferences,
) {
	labels := refs.inlineLabels()
	summary := measured.summary
	n := len(summary.RankCounts)
	requestStats := requestStats(summary)
	ran := summary.Trials

	writeHeading(w, "Measurement")
	if ran > 0 {
		entries := make([]rankProfileEntry, n)
		for i := range n {
			entries[i] = rankProfileEntry{
				label: labels[i], meanRank: requestStats[i].MeanRank,
				rankCounts: requestStats[i].RankCounts, samples: requestStats[i].N,
				pinned: view.pinned[labels[i]],
			}
		}
		reportRankProfile(w, entries, view)
		reportPairwisePrecedence(w, measured.pairwise, labels, measured.retained)
		emitln(w, "\nDescriptive result only; no statistical finding is inferred.")
	} else {
		emitln(w,
			"Measurement unavailable: no rank-complete trials were retained.")
	}
	reportMeasureHTTPStatusOutcomes(w, measured, labels)
	if ran == 0 {
		writeHeading(w, "Position check")
		emitln(w,
			"No position estimate is available because no ranks were retained.")
	} else if measured.arrangement == tth2.ArrangeNone {
		reportFixedPositionCheck(w)
	} else {
		reportPositionCheck(
			w, summary, measured.assignments, n, measured.arrangement)
	}

	reportMeasureExecution(w, measured, labels)
}

func reportPairwisePrecedence(
	w io.Writer, counts [][]int, labels []string, trials int,
) {
	emitln(w, "\nPairwise precedence (row-before-column fraction):")
	width := max(labelColumn(labels), len("request"))
	emitf(w, "  %-*s", width, "request")
	for _, label := range labels {
		emitf(w, "  %*s", max(4, len(label)), label)
	}
	emitln(w)
	for row, label := range labels {
		emitf(w, "  %-*s", width, label)
		for column, columnLabel := range labels {
			cellWidth := max(4, len(columnLabel))
			if row == column {
				emitf(w, "  %*s", cellWidth, "-")
				continue
			}
			emitf(w, "  %*.2f", cellWidth,
				float64(counts[row][column])/float64(trials))
		}
		emitln(w)
	}
}

func reportMeasureExperiment(
	w io.Writer,
	plan measurePlan,
	concurrency int,
) error {
	workload, err := plan.workload(concurrency)
	if err != nil {
		return err
	}
	writeHeading(w, "Experiment")
	switch plan.arrangement {
	case tth2.ArrangeRotate:
		cycles := plan.planned / plan.width
		if plan.requested != plan.planned {
			writeWrappedASCII(w, "Design: ", fmt.Sprintf(
				"%s requested; %s planned as %s (%s).",
				countedNoun(plan.requested, "trial"),
				countedNoun(plan.planned, "trial"),
				countedNoun(cycles, "balanced rotation cycle"),
				countedNoun(workload.measured, "measured request operation")))
		} else {
			writeWrappedASCII(w, "Design: ", fmt.Sprintf(
				"%s (%s, %s).",
				countedNoun(cycles, "balanced rotation cycle"),
				countedNoun(plan.planned, "trial"),
				countedNoun(workload.measured, "measured request operation")))
		}
		writeWrappedASCII(w, "Arrangement: ",
			"rotate; each request occupies every position once per cycle.")
	case tth2.ArrangeRandom:
		writeWrappedASCII(w, "Design: ", fmt.Sprintf(
			"%s (%s).",
			countedNoun(plan.planned, "trial"),
			countedNoun(workload.measured, "measured request operation")))
		writeWrappedASCII(w, "Arrangement: ",
			"random; shuffled per trial and balanced only in expectation.")
	default:
		writeWrappedASCII(w, "Design: ", fmt.Sprintf(
			"%s (%s).",
			countedNoun(plan.planned, "trial"),
			countedNoun(workload.measured, "measured request operation")))
		writeWrappedASCII(w, "Arrangement: ",
			"none; request identity remains confounded with outbound position.")
	}
	if plan.warmup == 0 {
		writeWrappedASCII(w, "Priming: ",
			"none; early trials may include cold-state effects.")
	} else if plan.delivery.pathFitUnresolved {
		perConnection, ok := checkedProduct(plan.warmup, plan.warmupWidth)
		if !ok {
			return fmt.Errorf("planned warmup request workload is too large")
		}
		writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
			"%s per connection (%s per connection); total unresolved until execution; excluded from ranks.",
			countedNoun(plan.warmup, "priming attempt"),
			countedNoun(perConnection, "request operation")))
	} else {
		writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
			"%s per connection (%s), excluded from ranks.",
			countedNoun(plan.warmup, "priming attempt"),
			countedNoun(workload.initialWarmup, "request operation")))
	}
	reportRacePadding(w, plan.padding)
	delivery := plan.delivery
	delivery.connectionLimit = concurrency
	if err := reportDeliveryBatchWidths(
		w, delivery, plan.width, plan.warmupWidth); err != nil {
		return err
	}
	reportReleasePlan(w, plan.bodyRelease)
	reportResponseLimits(w, plan.responseLimits)
	reportRunTimeout(w, plan.runTimeout)
	reportTLSPlan(w, plan.insecure)
	if plan.arrangement == tth2.ArrangeRotate && plan.planned/plan.width == 1 {
		writeWrappedASCII(w, "Caution: ",
			"only 1 complete rotation cycle is planned. Position assignment will "+
				"be balanced, but the plan has no replication across rotation blocks.")
	}
	return nil
}

// reportResetEvidence attributes stream resets to request identities. A run
// with no complete ranks can still contain the clearest observation the target
// supplied.
func reportResetEvidence(
	w io.Writer,
	summary *tth2.TrialSummary,
	labels []string,
) {
	resets := 0
	for _, count := range summary.ResetCounts {
		resets += count
	}
	if resets == 0 {
		emitln(w, "No request stream resets recorded.")
		return
	}
	emitln(w, "Reset trials by request:")
	for i, count := range summary.ResetCounts {
		if count > 0 {
			emitf(w, "  %s  %d\n", labels[i], count)
		}
	}
}

func reportMeasureHTTPStatusOutcomes(
	w io.Writer,
	measured *measurement,
	labels []string,
) {
	writeHeading(w, "HTTP status outcomes")
	if measured.retained == 0 {
		emitln(w,
			"No ranked responses; no trial produced a complete set of ranks.")
		reportPartialResponseExclusion(w, measured.incomplete)
		return
	}

	uniformStatus, uniform := uniformResponseStatus(measured.statusCounts)
	if uniform {
		emitf(w, "Ranked responses: HTTP %d in all %s for every request.\n",
			uniformStatus, countedNoun(measured.retained, "ranked trial"))
	} else {
		emitln(w, "Complete ranked trials only; warmup excluded.")
		width := labelColumn(labels)
		for request, counts := range measured.statusCounts {
			writeWrappedASCII(w, fmt.Sprintf("  %-*s  ", width, labels[request]),
				formatStatusCounts(counts))
		}
	}
	reportPartialResponseExclusion(w, measured.incomplete)

	changed, non2xx := responseOutcomeReview(measured.statusCounts)
	if len(changed) > 0 {
		requestWord := "requests"
		if len(changed) == 1 {
			requestWord = "request"
		}
		writeWrappedASCII(w, "Review: ", fmt.Sprintf(
			"HTTP status changed across ranked trials for %d %s: %s.",
			len(changed), requestWord, joinRequestLabels(labels, changed)))
		if len(changed) == 1 {
			emitln(w, "Its aggregate rank combines different response outcomes.")
		} else {
			emitln(w, "Their aggregate ranks combine different response outcomes.")
		}
	}
	if len(non2xx) == len(measured.statusCounts) && uniform {
		emitln(w, "Review: all ranked responses had a non-2xx HTTP status.")
	} else if len(non2xx) > 0 {
		requestWord := "requests"
		if len(non2xx) == 1 {
			requestWord = "request"
		}
		writeWrappedASCII(w, "Review: ", fmt.Sprintf(
			"non-2xx responses contributed to the ranking for %d %s: %s.",
			len(non2xx), requestWord, joinRequestLabels(labels, non2xx)))
	}
	if len(non2xx) > 0 {
		emitln(w, "HTTP responses receive ranks regardless of status.")
	}
}

func uniformResponseStatus(counts []map[int]int) (int, bool) {
	status := 0
	for request, distribution := range counts {
		if len(distribution) != 1 {
			return 0, false
		}
		for candidate := range distribution {
			if request > 0 && candidate != status {
				return 0, false
			}
			status = candidate
		}
	}
	return status, len(counts) > 0
}

func formatStatusCounts(counts map[int]int) string {
	statuses := make([]int, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, status)
	}
	slices.Sort(statuses)
	parts := make([]string, len(statuses))
	for i, status := range statuses {
		parts[i] = fmt.Sprintf("%d x %d", status, counts[status])
	}
	return strings.Join(parts, ", ")
}

func reportPartialResponseExclusion(w io.Writer, incomplete int) {
	if incomplete == 0 {
		return
	}
	emitf(w, "Note: %s excluded from these counts and the ranking.\n",
		countedNoun(incomplete, "incomplete trial"))
	pronoun := "those trials"
	if incomplete == 1 {
		pronoun = "that trial"
	}
	writeWrappedASCII(w, "", fmt.Sprintf(
		"Partial responses from %s are not summarised here; see Execution "+
			"and reset counts.", pronoun))
}

// reportPositionCheck prints every position mean and its overall spread.
func reportPositionCheck(
	w io.Writer,
	summary *tth2.TrialSummary,
	assignments [][]int,
	n int,
	arrangement tth2.ArrangementPolicy,
) {
	writeHeading(w, "Position check")
	reportPositionTable(w, summary, n)
	if arrangement == tth2.ArrangeRandom {
		minimum, maximum := assignmentCountRange(assignments)
		writeWrappedASCII(w, "Random assignment: ", fmt.Sprintf(
			"request-position counts ranged %d-%d (expected %.2f).",
			minimum, maximum, float64(summary.Trials)/float64(n)))
	}
}

// reportFixedPositionCheck explains why request and position statistics are
// the same grouping under ArrangeNone instead of printing duplicate numbers.
func reportFixedPositionCheck(w io.Writer) {
	writeHeading(w, "Position check")
	writeWrappedASCII(w, "",
		"No separate position estimate is available: every request kept one "+
			"outbound position.")
}

// reportPositionTable prints the full position means.
func reportPositionTable(w io.Writer, summary *tth2.TrialSummary, n int) {
	emitln(w, "  position  observations  mean rank  normalised mean rank")
	for k, ps := range positionStats(summary) {
		emitf(w, "  %8d  %12d  %9.2f  %20.2f\n",
			k, ps.N, ps.MeanRank, ps.MeanRank/float64(n-1))
	}
	spread := positionSpread(summary)
	if spread.raw != nil {
		if n == 2 {
			emitf(w, "  spread: %.2f\n", *spread.raw)
		} else {
			emitf(w, "  spread: %.2f (normalised %.2f)\n",
				*spread.raw, *spread.normalised)
		}
	}
}

func assignmentCountRange(counts [][]int) (int, int) {
	if len(counts) == 0 || len(counts[0]) == 0 {
		return 0, 0
	}
	minimum, maximum := counts[0][0], counts[0][0]
	for _, row := range counts {
		for _, count := range row {
			minimum = min(minimum, count)
			maximum = max(maximum, count)
		}
	}
	return minimum, maximum
}

func reportMeasureExecution(w io.Writer, measured *measurement, labels []string) {
	summary := measured.summary
	execution, _ := measured.execution()
	trials := execution.Trials
	writeHeading(w, "Execution")
	healthy := trials.Attempted == trials.Planned &&
		trials.RankComplete == trials.Attempted &&
		trials.Retained == trials.RankComplete && trials.Incomplete == 0 &&
		trials.Excluded == 0 && trials.Unattempted == 0
	if healthy {
		if measured.arrangement == tth2.ArrangeRotate {
			emitf(w, "%d/%d trials were rank-complete and retained in %d/%d "+
				"balanced cycles.\n", trials.Retained, trials.Planned,
				execution.Cycles.Retained, execution.Cycles.Planned)
		} else {
			emitf(w, "%d/%d trials were rank-complete and retained.\n",
				trials.Retained, trials.Planned)
		}
	} else {
		emitf(w, "Trials: %d attempted; %d rank-complete; %d incomplete; "+
			"%d retained.\n", trials.Attempted, trials.RankComplete,
			trials.Incomplete, trials.Retained)
	}
	if trials.Excluded > 0 || trials.Unattempted > 0 {
		details := make([]string, 0, 2)
		if trials.Excluded > 0 {
			cycleScope := "incomplete cycles"
			if execution.Cycles != nil && execution.Cycles.Incomplete == 1 {
				cycleScope = "an incomplete cycle"
			}
			details = append(details, fmt.Sprintf("%d excluded with %s",
				trials.Excluded, cycleScope))
		}
		if trials.Unattempted > 0 {
			details = append(details,
				fmt.Sprintf("%d unattempted", trials.Unattempted))
		}
		writeWrappedASCII(w, "        ", strings.Join(details, "; ")+".")
	}

	if measured.arrangement == tth2.ArrangeRotate {
		if !healthy {
			cycles := execution.Cycles
			cycleText := fmt.Sprintf("%d/%d retained", cycles.Retained, cycles.Planned)
			if cycles.Incomplete > 0 {
				cycleText += fmt.Sprintf("; %d incomplete", cycles.Incomplete)
			}
			if cycles.Unattempted > 0 {
				cycleText += fmt.Sprintf("; %d unattempted", cycles.Unattempted)
			}
			emitf(w, "Cycles: %s.\n", cycleText)
		}
	}

	connections := execution.Connections
	observed := connections.Observed
	resets := 0
	for _, count := range summary.ResetCounts {
		resets += count
	}
	if replacements := connections.Replacements; replacements > 0 {
		emitf(w, "Connections: %d observed vs %d planned; %s.\n",
			observed, connections.Planned,
			countedNoun(replacements, "replacement"))
	} else if observed == connections.Planned {
		if connections.Planned == 1 {
			emit(w, "One planned connection was observed")
		} else {
			emitf(w, "%d planned connections were observed",
				connections.Planned)
		}
		if resets == 0 {
			emitln(w, "; no resets or replacements.")
		} else {
			emitln(w, "; no replacements.")
		}
	} else {
		emitf(w, "Connections: %d observed vs %d planned; no replacements.\n",
			observed, connections.Planned)
	}
	expectedWarmups := measured.warmupPerConn * measured.connsPlanned
	if extra := measured.warmups - expectedWarmups; extra > 0 {
		emitf(w, "Priming: %d attempted; %d added for replacements.\n",
			measured.warmups, extra)
	} else if measured.warmups != expectedWarmups {
		emitf(w, "Priming: %d attempted versus %d planned.\n",
			measured.warmups, expectedWarmups)
	}
	if execution.Priming.Batches.Incomplete > 0 {
		writeWrappedASCII(w, "Caution: ",
			"incomplete priming may leave cold-state effects in measured trials.")
	}
	if execution.Priming.Batches.Attempted > 0 {
		emitf(w, "Priming work: %s attempted; %d rank-complete; "+
			"excluded from measured ranks.\n", countedNoun(
			execution.Priming.RequestOperations.Attempted,
			"request operation"),
			execution.Priming.RequestOperations.RankComplete)
	}
	operations := execution.RequestOperations
	emitf(w, "Measured request operations: %d attempted; %d rank-complete; "+
		"%d retained.\n", operations.Attempted, operations.RankComplete,
		operations.Retained)
	if summary.FailedTrials > 0 {
		emitf(w, "Caution: %s did not contribute ranks.\n",
			countedNoun(summary.FailedTrials, "incomplete trial"))
	}
	if len(measured.batchFailures) > 0 {
		bodyLimits, timeouts := 0, 0
		for _, failure := range measured.batchFailures {
			local := classifyBatchFailure(failure.Err)
			if local == nil {
				continue
			}
			switch local.Code {
			case failureResponseBodyLimit:
				bodyLimits++
			case failureBatchTimeout:
				timeouts++
			}
		}
		parts := make([]string, 0, 2)
		if bodyLimits > 0 {
			parts = append(parts,
				countedNoun(bodyLimits, "response-body limit failure"))
		}
		if timeouts > 0 {
			parts = append(parts,
				countedNoun(timeouts, "batch-timeout failure"))
		}
		if len(parts) > 0 {
			writeWrappedASCII(w, "Local limits: ", strings.Join(parts, "; ")+".")
		}
	}
	for _, count := range summary.ResetCounts {
		if count > 0 {
			reportResetEvidence(w, summary, labels)
			break
		}
	}
	if measured.arrangement == tth2.ArrangeRotate {
		cycles := execution.Cycles
		if cycles.Incomplete > 0 {
			emitf(w, "Caution: %s excluded from the ranking.\n",
				countedNoun(cycles.Incomplete, "incomplete rotation cycle"))
		}
		switch cycles.Retained {
		case 0:
			writeWrappedASCII(w, "Caution: ",
				"no complete rotation cycle was retained; no position-balanced "+
					"measurement is available.")
		case 1:
			writeWrappedASCII(w, "Caution: ",
				"only 1 complete rotation cycle was retained. Position assignment "+
					"is balanced, but cycle-to-cycle variation cannot be observed.")
		}
	}
	if replacements := measured.replacements(); replacements > 0 {
		emitf(w, "Caution: %s changed connection state.\n",
			countedNoun(replacements, "connection replacement"))
	}
	completion := measured.runCompletion()
	if completion.State != completionComplete {
		emitf(w, "Completion: %s.\n", completion.State)
	}
	if completion.Detail != "" {
		emitf(w, "Error: %s\n", curlblocks.DisplayText(completion.Detail))
	}
}
