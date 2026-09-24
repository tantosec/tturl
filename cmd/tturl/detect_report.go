package main

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
	"github.com/tantosec/tturl/internal/ranking"
)

type detectExperimentPlan struct {
	Candidates                int
	BaselineTemplates         int
	BaselineSupply            baselineSupply
	BaselineEffectiveCap      int
	StrategyRequested         detectStrategy
	StrategyResolved          detectStrategy
	SolverMethod              ranking.OutlierMethodID
	WidthRequested            int
	WidthExplicit             bool
	WidthEffective            int
	FalsePositiveRisk         float64
	FalseNegativeRisk         float64
	Direction                 ranking.OutlierDirection
	ComparisonsMax            int
	ParticipationLimit        int
	MinimumEdge               float64
	Padding                   []PaddingRecord
	ResponseLimits            responseLimits
	RunTimeout                runTimeout
	WarmupTrialsPerConnection int
	WarmupBatchWidth          int
	ConnectionLimit           int
	InFlightComparisonLimit   int
	EvidenceUpdatePolicy      string
	DependencePremise         string
	Delivery                  deliveryPlan
	BodyRelease               bodyReleasePlan
	Insecure                  bool
}

type detectReportContext struct {
	ComparisonsMax         int
	ConnectionLimit        int
	ConfiguredNegativeEdge float64
	BaselineLabels         []string
}

func detectCandidateReferenceWidth() int {
	// Preserve room for both numeric columns and the selected marker. Values
	// wider than their ordinary fields are intrinsically wide result data.
	return textWidth - len("    999999.99  9999999  <- selected")
}

func reportDetectExperiment(w io.Writer, plan detectExperimentPlan) {
	writeHeading(w, "Experiment")
	requested := ""
	if plan.WidthExplicit {
		requested = fmt.Sprintf("; requested '--width' %d", plan.WidthRequested)
	}
	writeWrappedASCII(w, "Search: ", fmt.Sprintf(
		"%s arrival-order outlier among %s; effective width %d%s.",
		directionWord(plan.Direction), countedNoun(plan.Candidates, "candidate"), plan.WidthEffective, requested))
	writeWrappedASCII(w, "Strategy: ", fmt.Sprintf(
		"requested %s; resolved %s; solver method %q.",
		plan.StrategyRequested, plan.StrategyResolved, plan.SolverMethod))
	if plan.Delivery.pathFitUnresolved {
		writeWrappedASCII(w, "Scheduling: ",
			"in-flight comparison limit unresolved until connection fitting.")
	} else {
		writeWrappedASCII(w, "Scheduling: ", fmt.Sprintf("up to %s.",
			countedNoun(plan.InFlightComparisonLimit, "in-flight comparison")))
	}
	updates := "complete atomic waves"
	dependence := "arbitrary within an atomic wave"
	if plan.EvidenceUpdatePolicy == "maximal-contiguous-dispatch-prefix" {
		updates = "maximal contiguous dispatch prefix"
		dependence = "conditional across dispatch indices"
	}
	writeWrappedASCII(w, "Evidence updates: ", updates+".")
	writeWrappedASCII(w, "Dependence: ", dependence+".")
	if plan.BaselineTemplates > 0 {
		supply := strconv.Itoa(plan.BaselineSupply.count)
		if plan.BaselineSupply.unlimited {
			supply = unlimitedFlagValue
		}
		writeWrappedASCII(w, "Baselines: ", fmt.Sprintf(
			"%s; configured aggregate supply %s; effective cap %d at width %d.",
			countedNoun(plan.BaselineTemplates, "template"), supply,
			plan.BaselineEffectiveCap, plan.WidthEffective))
	} else {
		emitln(w, "Baselines: none.")
	}
	writeWrappedASCII(w, "Risk bounds: ", fmt.Sprintf(
		"false-positive %s; false-negative %s.",
		pct(plan.FalsePositiveRisk), pct(plan.FalseNegativeRisk)))
	writeWrappedASCII(w, "Negative boundary: ", fmt.Sprintf(
		"minimum normalised-rank edge %.3g.", plan.MinimumEdge))
	if plan.ComparisonsMax < 0 {
		emitln(w, "Budget: unbounded adaptive comparisons.")
	} else {
		writeWrappedASCII(w, "Budget: ", fmt.Sprintf("up to %s (%s).",
			countedNoun(plan.ComparisonsMax, "adaptive comparison"),
			countedNoun(plan.ParticipationLimit, "measured request participation")))
	}
	if plan.WarmupTrialsPerConnection == 0 {
		writeWrappedASCII(w, "Priming: ",
			"none; early comparisons per connection may include cold-state effects.")
	} else if plan.Delivery.pathFitUnresolved {
		operations, ok := checkedProduct(
			plan.WarmupTrialsPerConnection, plan.WarmupBatchWidth)
		if ok {
			writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
				"%s per connection; %s per connection; "+
					"total unresolved until execution; excluded from inference.",
				countedNoun(plan.WarmupTrialsPerConnection, "distinct-request attempt"),
				countedNoun(operations, "request operation")))
		} else {
			writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
				"%s per connection; total unresolved "+
					"until execution; excluded from inference.",
				countedNoun(plan.WarmupTrialsPerConnection, "distinct-request attempt")))
		}
	} else {
		batches, batchesOK := checkedProduct(
			plan.WarmupTrialsPerConnection, plan.ConnectionLimit)
		operations, operationsOK := checkedProduct(batches, plan.WarmupBatchWidth)
		maximum, maximumOK := checkedProduct(operations, 2)
		if batchesOK && operationsOK && maximumOK {
			writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
				"%s per connection; %s initially and "+
					"up to %s with one replacement per worker; excluded from inference.",
				countedNoun(plan.WarmupTrialsPerConnection, "distinct-request attempt"),
				countedNoun(operations, "request operation"),
				countedNoun(maximum, "request operation")))
		} else {
			writeWrappedASCII(w, "Priming: ", fmt.Sprintf(
				"%s per connection; one replacement per worker; excluded from inference.",
				countedNoun(plan.WarmupTrialsPerConnection, "distinct-request attempt")))
		}
	}
	reportRacePadding(w, plan.Padding)
	if plan.WarmupTrialsPerConnection > 0 && plan.WarmupBatchWidth > plan.WidthEffective {
		_ = reportDeliveryBatchWidths(w, plan.Delivery, plan.WidthEffective, plan.WarmupBatchWidth)
	} else {
		_ = reportDeliveryPolicy(w, plan.Delivery)
	}
	reportReleasePlan(w, plan.BodyRelease)
	reportResponseLimits(w, plan.ResponseLimits)
	reportRunTimeout(w, plan.RunTimeout)
	reportTLSPlan(w, plan.Insecure)
}

// reportDetect is a report-level test seam for a result without response
// evidence. Production reports use reportDetectResult.
func reportDetect(w io.Writer, res ranking.OutlierResult, width int,
	view rankView, refs runReferences,
) {
	configuredEdge := res.MinimumEdge
	if detail, ok := res.Detail.(ranking.OutlierDiagnostics); ok {
		configuredEdge = detail.MinimumEdge
	}
	reportDetectResult(w, detectRunResult{Ranking: res}, detectReportContext{
		ComparisonsMax:         res.Comparisons,
		ConfiguredNegativeEdge: configuredEdge,
	}, width, view, refs)
}

func reportDetectResult(w io.Writer, result detectRunResult,
	context detectReportContext, width int, view rankView, refs runReferences,
) {
	res := result.Ranking
	labels := refs.inlineLabels()
	writeHeading(w, "Detection result")
	if result.Unavailable {
		reportDetectUnavailable(w, result)
		if res.Comparisons > 0 {
			emitln(w)
			reportDetectCandidateMeans(w, res, view, labels)
		}
		if result.Requests != nil {
			reportDetectHTTPStatuses(w, result, labels, context.BaselineLabels)
		}
		reportDetectExecution(w, result, context)
		return
	}
	reportDetectDecision(w, res, context, width, labels)
	reportDetectCoverage(w, res)
	emitln(w)
	reportDetectCandidateMeans(w, res, view, labels)
	if result.Requests != nil {
		reportDetectHTTPStatuses(w, result, labels, context.BaselineLabels)
	}
	reportDetectExecution(w, result, context)
}

func reportDetectUnavailable(w io.Writer, result detectRunResult) {
	completion := normaliseCompletion(result.Completion)
	if completion.State == completionStopped {
		writeWrappedASCII(w, "Decision unavailable: ",
			"the adaptive search was stopped before a decision.")
	} else {
		writeWrappedASCII(w, "Decision unavailable: ",
			"the adaptive search failed before producing a decision.")
		if result.Reason != nil {
			writeWrappedASCII(w, "Reason: ",
				curlblocks.DisplayText(result.Reason.Error()))
		}
	}
	emitln(w, "No statistical guarantee was produced.")
	if result.Ranking.Comparisons == 0 {
		emitln(w, "No rank-complete adaptive comparisons were observed.")
	}
}

func reportDetectDecision(w io.Writer, res ranking.OutlierResult,
	context detectReportContext, width int, labels []string,
) {
	switch res.Outcome {
	case ranking.OutlierFound:
		writeWrappedASCII(w, "Finding: ", fmt.Sprintf(
			"Evidence identifies request %s as the %s arrival-order outlier",
			labels[res.Index], directionWord(res.Direction)))
		item := res.Items[res.Index]
		threshold := "the configured natural-log commit threshold"
		if detail, ok := res.Detail.(ranking.OutlierDiagnostics); ok {
			threshold = fmt.Sprintf(
				"the %.3g natural-log commit threshold", detail.Threshold)
		}
		writeWrappedASCII(w, "Evidence: ", fmt.Sprintf(
			"positive log evidence reached %s after %s; observed mean rank %.2f of %d.",
			threshold, countedNoun(item.Samples, "sample"),
			item.MeanRank, max(0, width-1)))
		writeWrappedASCII(w, "Guarantee: ", fmt.Sprintf(
			"family-wise false-positive risk at most %s.",
			pct(res.ErrorRiskLimit)))
		writeWrappedASCII(w, "Negative scope: ", fmt.Sprintf(
			"configured negative edge %.3g; positive evidence does not use this edge.",
			context.ConfiguredNegativeEdge))
		reportDetectInference(w, res)
	case ranking.OutlierNegative:
		writeWrappedASCII(w, "Finding: ", fmt.Sprintf(
			"No qualifying %s arrival-order outlier detected",
			directionWord(res.Direction)))
		writeWrappedASCII(w, "Negative scope: ", fmt.Sprintf(
			"normalised-rank edge at least %.3g.", res.MinimumEdge))
		writeWrappedASCII(w, "Evidence: ", fmt.Sprintf(
			"All %d candidates crossed the elimination threshold.",
			len(res.Items)))
		writeWrappedASCII(w, "Guarantee: ", fmt.Sprintf(
			"false-negative risk at most %s for the configured direction; excluded negative edge %.3g.",
			pct(res.ErrorRiskLimit), res.MinimumEdge))
		reportDetectInference(w, res)
	default:
		if context.ComparisonsMax >= 0 {
			writeWrappedASCII(w, "Finding: ", fmt.Sprintf(
				"Inconclusive; no decision before the %d-comparison limit",
				context.ComparisonsMax))
			writeWrappedASCII(w, "Search progress: ",
				"the limit was reached before a positive threshold crossing or full-field elimination.")
		} else {
			writeWrappedASCII(w, "Finding: ",
				"Inconclusive; search ended without a decision")
		}
	}
}

func reportDetectInference(w io.Writer, res ranking.OutlierResult) {
	regime := res.Regime.String()
	if regime != "" {
		regime = strings.ToUpper(regime[:1]) + regime[1:]
	}
	writeWrappedASCII(w, "Inference: ", fmt.Sprintf(
		"%s; premise code %q.", regime, res.Premise))
	direction := directionWord(res.Direction)
	var premise, targetPremise string
	switch res.Premise {
	case ranking.OutlierPremiseScheduledReferenceMean:
		premise = fmt.Sprintf(
			"Each non-outlier's conditional mean normalised rank is at most halfway "+
				"toward the %s extreme in every adaptively scheduled peer set.",
			direction)
		targetPremise = fmt.Sprintf(
			"In every such set a qualifying outlier's conditional mean normalised "+
				"rank exceeds halfway by at least %.3g.", res.MinimumEdge)
	case ranking.OutlierPremiseBaselineMean:
		premise = fmt.Sprintf(
			"Each non-outlier's conditional mean normalised rank against selected "+
				"baseline controls is at most halfway toward the %s extreme.", direction)
		targetPremise = fmt.Sprintf(
			"Against those controls a qualifying outlier's conditional mean "+
				"normalised rank exceeds halfway by at least %.3g.", res.MinimumEdge)
	default:
		return
	}
	writeWrappedASCII(w, "Premise: ", premise)
	if res.Outcome == ranking.OutlierNegative {
		writeWrappedASCII(w, "Target premise: ", targetPremise)
	}
}

func reportDetectCoverage(w io.Writer, res ranking.OutlierResult) {
	peerSamples := make([]int, 0, len(res.Items))
	sampled := 0
	for request, item := range res.Items {
		if item.Samples == 0 {
			continue
		}
		sampled++
		if res.Outcome != ranking.OutlierFound || request != res.Index {
			peerSamples = append(peerSamples, item.Samples)
		}
	}
	writeWrappedASCII(w, "Field coverage: ", fmt.Sprintf(
		"%d of %d candidates sampled.", sampled, len(res.Items)))
	writeWrappedASCII(w, "Physical work: ",
		countedNoun(res.Participations, "request participation")+
			" in rank-complete comparisons.")
	writeWrappedASCII(w, "Inferential samples: ",
		"completed comparisons while each candidate was in contention; "+
			"filler reuse after elimination adds physical work, not new samples.")
	switch {
	case sampled == 0:
		writeWrappedASCII(w, "Inferential allocation: ", "no candidates sampled.")
	case len(peerSamples) == 0:
		writeWrappedASCII(w, "Inferential allocation: ",
			"no other candidates sampled.")
	default:
		subject := "sampled candidates"
		if res.Outcome == ranking.OutlierFound {
			subject = "other sampled candidates"
		}
		writeWrappedASCII(w, "Inferential allocation: ", fmt.Sprintf(
			"%s median %s, range %d-%d.", subject,
			formatSampleMedian(peerSamples), slices.Min(peerSamples),
			slices.Max(peerSamples)))
	}
}

func formatSampleMedian(samples []int) string {
	sorted := append([]int(nil), samples...)
	sort.Ints(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return countedNoun(sorted[mid], "sample")
	}
	total := sorted[mid-1] + sorted[mid]
	if total%2 == 0 {
		return countedNoun(total/2, "sample")
	}
	return fmt.Sprintf("%.1f samples", float64(total)/2)
}

type detectCandidateRow struct {
	label    string
	meanRank float64
	samples  int
	selected bool
	pinned   bool
}

type detectCandidateLine struct {
	row     *detectCandidateRow
	omitted int
}

func reportDetectCandidateMeans(w io.Writer, res ranking.OutlierResult,
	view rankView, labels []string,
) {
	rows := make([]detectCandidateRow, 0, len(res.Items))
	var unobserved []string
	for request, item := range res.Items {
		pinned := view.pinned[labels[request]]
		if item.Samples == 0 {
			label := labels[request]
			if pinned {
				label += " (pinned)"
			}
			unobserved = append(unobserved, label)
			continue
		}
		rows = append(rows, detectCandidateRow{
			label: labels[request], meanRank: item.MeanRank,
			samples: item.Samples, pinned: pinned,
			selected: res.Outcome == ranking.OutlierFound && request == res.Index,
		})
	}
	slices.SortStableFunc(rows, func(a, b detectCandidateRow) int {
		return cmp.Compare(a.meanRank, b.meanRank)
	})

	if len(rows) == 0 {
		emitln(w, "Exploratory candidate means: no candidates sampled.")
	} else {
		lines := selectDetectCandidateRows(rows, view)
		shown := 0
		for _, line := range lines {
			if line.row != nil {
				shown++
			}
		}
		if shown == 0 {
			emitln(w, "Exploratory candidate means: no rows selected (--rank-rows 0).")
		} else {
			emitln(w,
				"Exploratory candidate means (adaptive samples are not directly comparable):")
			labelWidth := 7
			for _, line := range lines {
				if line.row != nil {
					labelWidth = max(labelWidth, len(line.row.label))
				}
			}
			emitf(w, "  %-*s  %9s  %7s\n",
				labelWidth, "request", "mean rank", "samples")
			for _, line := range lines {
				if line.row == nil {
					emitf(w,
						"  ... %s omitted; use --rank-rows N or --rank-rows all ...\n",
						countedNoun(line.omitted, "sampled request"))
					continue
				}
				marker := ""
				if line.row.selected {
					marker = "  <- selected"
				}
				emitf(w, "  %-*s  %9.2f  %7d%s\n", labelWidth,
					line.row.label, line.row.meanRank, line.row.samples, marker)
			}
		}
	}
	if len(unobserved) > 0 {
		emitln(w)
		writeWrappedASCII(w, fmt.Sprintf("Unobserved (%d): ", len(unobserved)),
			strings.Join(unobserved, ", ")+".")
	}
}

func selectDetectCandidateRows(rows []detectCandidateRow,
	view rankView,
) []detectCandidateLine {
	show := make([]bool, len(rows))
	if view.allRows ||
		(view.endRows > 0 && view.endRows >= len(rows)/2) {
		for index := range show {
			show[index] = true
		}
	} else {
		for index, row := range rows {
			show[index] = index < view.endRows ||
				index >= len(rows)-view.endRows || row.selected || row.pinned
		}
	}
	var lines []detectCandidateLine
	for index := 0; index < len(rows); {
		if show[index] {
			row := rows[index]
			lines = append(lines, detectCandidateLine{row: &row})
			index++
			continue
		}
		start := index
		for index < len(rows) && !show[index] {
			index++
		}
		lines = append(lines, detectCandidateLine{omitted: index - start})
	}
	return lines
}

type detectStatusGroup struct {
	statuses       []int
	requests       []int
	participations int
}

func reportDetectHTTPStatuses(w io.Writer, result detectRunResult,
	labels []string,
	baselineLabels ...[]string,
) {
	writeHeading(w, "HTTP status outcomes")
	groups, observed, participations := groupDetectHTTPStatuses(result.Requests)
	if len(groups) == 0 {
		emitln(w, "No ranked HTTP responses observed.")
	} else if len(groups) == 1 && len(groups[0].statuses) == 1 {
		status := groups[0].statuses[0]
		if observed == len(result.Requests) {
			emitf(w, "HTTP %d for all %s.\n", status,
				countedNoun(participations, "request participation"))
		} else {
			writeWrappedASCII(w, "", fmt.Sprintf(
				"HTTP %d for all %s across %s.",
				status, countedNoun(participations, "ranked request participation"),
				countedNoun(observed, "sampled request")))
		}
	} else {
		emitln(w, "Observed HTTP status patterns:")
		patterns := make([]string, len(groups))
		patternWidth := 0
		for index, group := range groups {
			patterns[index] = detectStatusPattern(group.statuses)
			patternWidth = max(patternWidth, len(patterns[index]))
		}
		uniqueLargest := len(groups) == 1 ||
			len(groups[0].requests) > len(groups[1].requests)
		for index, group := range groups {
			prefix := fmt.Sprintf("  %-*s  %s; %s", patternWidth,
				patterns[index], countedNoun(len(group.requests), "request"),
				countedNoun(group.participations, "participation"))
			if index == 0 && uniqueLargest {
				emitln(w, prefix)
				continue
			}
			requestLabels := make([]string, len(group.requests))
			for i, request := range group.requests {
				requestLabels[i] = labels[request]
			}
			writeWrappedASCII(w, prefix+": ", strings.Join(requestLabels, ", "))
		}
	}
	if len(result.Responses.Changing) > 0 {
		writeWrappedASCII(w, "Review: ", fmt.Sprintf(
			"HTTP status changed across ranked responses for %s; their aggregate ranks combine different response outcomes.",
			joinRequestLabels(labels, result.Responses.Changing)))
	}
	if len(result.Responses.Non2xx) > 0 {
		writeWrappedASCII(w, "Review: ", fmt.Sprintf(
			"non-2xx responses contributed ranks for %s.",
			joinRequestLabels(labels, result.Responses.Non2xx)))
	}
	if len(result.Baselines) > 0 {
		resolved := make([]string, len(result.Baselines))
		for i := range resolved {
			resolved[i] = fmt.Sprintf("baseline %d", i)
		}
		if len(baselineLabels) > 0 &&
			len(baselineLabels[0]) == len(result.Baselines) {
			resolved = baselineLabels[0]
		}
		emitln(w)
		reportDetectBaselineHTTPStatuses(w, result, resolved)
	}
}

func reportDetectBaselineHTTPStatuses(
	w io.Writer,
	result detectRunResult,
	labels []string,
) {
	groups, observed, participations := groupDetectHTTPStatuses(result.Baselines)
	if len(groups) == 0 {
		emitln(w, "Baseline controls: no ranked HTTP responses observed.")
	} else if len(groups) == 1 && len(groups[0].statuses) == 1 {
		writeWrappedASCII(w, "Baseline controls: ", fmt.Sprintf(
			"HTTP %d across %s from %s.", groups[0].statuses[0],
			countedNoun(participations, "participation"),
			countedNoun(observed, "template")))
	} else {
		emitln(w, "Baseline control HTTP status patterns:")
		for _, group := range groups {
			groupLabels := make([]string, len(group.requests))
			for i, request := range group.requests {
				groupLabels[i] = labels[request]
			}
			writeWrappedASCII(w, "  "+detectStatusPattern(group.statuses)+": ",
				strings.Join(groupLabels, ", "))
		}
	}
	if len(result.Responses.BaselineChanging) > 0 {
		writeWrappedASCII(w, "Review: ", fmt.Sprintf(
			"HTTP status changed across ranked baseline responses for %s; "+
				"their aggregate ranks combine different response outcomes.",
			joinRequestLabels(labels, result.Responses.BaselineChanging)))
	}
	if len(result.Responses.BaselineNon2xx) > 0 {
		writeWrappedASCII(w, "Review: ", fmt.Sprintf(
			"non-2xx baseline responses contributed ranks for %s.",
			joinRequestLabels(labels, result.Responses.BaselineNon2xx)))
	}
}

func groupDetectHTTPStatuses(requests []detectRequestEvidence) (
	groups []detectStatusGroup, observedRequests, participations int,
) {
	bySignature := map[string]int{}
	for _, request := range requests {
		if request.Participations == 0 {
			continue
		}
		observedRequests++
		participations += request.Participations
		statuses := make([]int, len(request.StatusCounts))
		parts := make([]string, len(request.StatusCounts))
		for index, count := range request.StatusCounts {
			statuses[index], parts[index] = count.Status, strconv.Itoa(count.Status)
		}
		key := strings.Join(parts, ",")
		group, ok := bySignature[key]
		if !ok {
			group = len(groups)
			bySignature[key] = group
			groups = append(groups, detectStatusGroup{statuses: statuses})
		}
		groups[group].requests = append(groups[group].requests, request.Request)
		groups[group].participations += request.Participations
	}
	slices.SortStableFunc(groups, func(a, b detectStatusGroup) int {
		if result := cmp.Compare(len(b.requests), len(a.requests)); result != 0 {
			return result
		}
		return slices.Compare(a.statuses, b.statuses)
	})
	return groups, observedRequests, participations
}

func detectStatusPattern(statuses []int) string {
	parts := make([]string, len(statuses))
	for index, status := range statuses {
		parts[index] = strconv.Itoa(status)
	}
	switch len(parts) {
	case 1:
		return "HTTP " + parts[0] + " only"
	case 2:
		return "HTTP " + parts[0] + " and " + parts[1]
	default:
		return "HTTP " + strings.Join(parts[:len(parts)-1], ", ") +
			", and " + parts[len(parts)-1]
	}
}

func reportDetectExecution(
	w io.Writer, result detectRunResult, contexts ...detectReportContext,
) {
	context := detectReportContext{ComparisonsMax: -1}
	if len(contexts) > 0 {
		context = contexts[0]
	}
	if context.ComparisonsMax == 0 && (result.Ranking.Comparisons > 0 ||
		len(result.Execution.ComparisonFailures) > 0) {
		context.ComparisonsMax = -1
	}
	execution, err := result.executionAccounting(
		context.ComparisonsMax, context.ConnectionLimit)
	if err != nil {
		writeWrappedASCII(w, "Execution unavailable: ", err.Error()+".")
		return
	}
	writeHeading(w, "Execution")
	comparisons := execution.Comparisons
	comparisonText := fmt.Sprintf(
		"%d attempted; %d rank-complete; %d incomplete",
		comparisons.Attempted, comparisons.RankComplete, comparisons.Incomplete)
	if comparisons.Maximum == nil {
		comparisonText += "; unlimited maximum"
	} else {
		comparisonText += fmt.Sprintf("; maximum %d", *comparisons.Maximum)
	}
	writeWrappedASCII(w, "Comparisons: ", comparisonText+".")
	writeWrappedASCII(w, "Measured request operations: ",
		fmt.Sprintf("%d attempted; %d rank-complete; %d retained.",
			execution.RequestOperations.Attempted,
			execution.RequestOperations.RankComplete,
			execution.RequestOperations.Retained))
	if result.Execution.Recorded {
		connections := execution.Connections
		writeWrappedASCII(w, "Connections: ", fmt.Sprintf(
			"%d planned; %d observed; %d replacements.",
			connections.Planned, connections.Observed, connections.Replacements))
		if execution.Priming.Batches.Attempted > 0 {
			writeWrappedASCII(w, "Priming batches: ", fmt.Sprintf(
				"%d attempted; %d rank-complete; %d incomplete.",
				execution.Priming.Batches.Attempted,
				execution.Priming.Batches.RankComplete,
				execution.Priming.Batches.Incomplete))
			writeWrappedASCII(w, "Priming request operations: ", fmt.Sprintf(
				"%d attempted; %d rank-complete; excluded from inference.",
				execution.Priming.RequestOperations.Attempted,
				execution.Priming.RequestOperations.RankComplete))
		}
		reportDetectFailureGroups(w, result)
		if execution.Priming.Batches.Incomplete > 0 {
			writeWrappedASCII(w, "Caution: ",
				"incomplete priming may leave cold-state effects in adaptive comparisons.")
		}
		if connections.Replacements > 0 {
			writeWrappedASCII(w, "Caution: ", fmt.Sprintf(
				"%s changed connection state.",
				countedNoun(connections.Replacements, "connection replacement")))
		}
	}
	completion := normaliseCompletion(result.Completion)
	if completion.State != completionComplete {
		emitf(w, "Completion: %s.\n", completion.State)
	}
	if completion.Detail != "" && completion.State == completionFailed {
		emitf(w, "Error: %s\n", curlblocks.DisplayText(completion.Detail))
	}
}
