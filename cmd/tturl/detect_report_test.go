package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/ranking"
)

func TestDetectResultUsesSolverSamplesForCandidateEvidence(t *testing.T) {
	result := detectRunResult{
		Ranking: ranking.OutlierResult{
			Outcome: ranking.OutlierFound,
			Index:   2,
			OutlierGuarantee: ranking.OutlierGuarantee{
				ErrorDirection: ranking.OutlierErrorFalseFind,
				ErrorRiskLimit: 0.05, Direction: ranking.Late,
				Premise: ranking.OutlierPremiseScheduledReferenceMean,
				Regime:  ranking.OutlierInferenceAnytimeValid,
			},
			Cost: ranking.Cost{Comparisons: 51, Participations: 408},
			Items: []ranking.ItemStat{
				{Samples: 1, MeanRank: 0},
				{Samples: 2, MeanRank: 6.5},
				{Samples: 17, MeanRank: 7},
			},
			Detail: ranking.OutlierDiagnostics{
				Threshold: 4.5, MinimumEdge: 0.02,
				Items: []ranking.OutlierItemStat{{}, {}, {}},
			},
		},
		Requests: []detectRequestEvidence{
			{
				Request: 0, Participations: 200,
				StatusCounts: []detectStatusCount{{Status: 204, Count: 200}},
			},
			{
				Request: 1, Participations: 138,
				StatusCounts: []detectStatusCount{{Status: 204, Count: 138}},
			},
			{
				Request: 2, Participations: 70,
				StatusCounts: []detectStatusCount{{Status: 204, Count: 70}},
			},
		},
	}
	var output bytes.Buffer
	reportDetectResult(&output, result, detectReportContext{
		ComparisonsMax: 200, ConfiguredNegativeEdge: 0.02,
	}, 8, rankView{allRows: true}, fullRunReferences(
		[]string{"fast-a", "fast-b", "slow"}))
	report := strings.Join(strings.Fields(output.String()), " ")
	for _, want := range []string{
		"positive log evidence reached the 4.5 natural-log commit threshold after 17 samples",
		"configured negative edge 0.02; positive evidence does not use this edge",
		"family-wise false-positive risk at most 5.0%",
		"observed mean rank 7.00 of 7",
		"slow 7.00 17 <- selected",
		"408 request participations",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(report, "slow 7.00 70") {
		t.Fatalf("candidate evidence was replaced with physical participation data:\n%s",
			output.String())
	}
}

func TestSelectDetectCandidateRows(t *testing.T) {
	rows := func(count int) []detectCandidateRow {
		result := make([]detectCandidateRow, count)
		for index := range result {
			result[index] = detectCandidateRow{
				label: fmt.Sprintf("r%02d", index), meanRank: float64(index),
				samples: index + 1,
			}
		}
		return result
	}
	visible := func(lines []detectCandidateLine) (shown, omitted int) {
		for _, line := range lines {
			if line.row == nil {
				omitted += line.omitted
			} else {
				shown++
			}
		}
		return shown, omitted
	}

	if shown, omitted := visible(selectDetectCandidateRows(
		rows(9), rankView{endRows: 4})); shown != 9 || omitted != 0 {
		t.Fatalf("2N+1 boundary: shown %d, omitted %d", shown, omitted)
	}
	if shown, omitted := visible(selectDetectCandidateRows(
		rows(10), rankView{endRows: 4})); shown != 8 || omitted != 2 {
		t.Fatalf("2N+2 boundary: shown %d, omitted %d", shown, omitted)
	}

	selected := rows(12)
	selected[5].selected = true
	selected[7].pinned = true
	lines := selectDetectCandidateRows(selected, rankView{endRows: 0})
	if shown, omitted := visible(lines); shown != 2 || omitted != 10 {
		t.Fatalf("top zero: shown %d, omitted %d", shown, omitted)
	}
	if all := selectDetectCandidateRows(rows(12), rankView{allRows: true}); len(all) != 12 {
		t.Fatalf("all view has %d lines, want 12", len(all))
	}
	if shown, omitted := visible(selectDetectCandidateRows(
		rows(1), rankView{endRows: 0})); shown != 0 || omitted != 1 {
		t.Fatalf("top zero single row: shown %d, omitted %d", shown, omitted)
	}
}

func TestDetectCandidateMeansPreservesUnobservedIdentities(t *testing.T) {
	labels := []string{
		"sampled", "unobserved-a", "unobserved-b", "unobserved-c",
		"unobserved-d", "unobserved-e",
	}
	result := ranking.OutlierResult{Items: []ranking.ItemStat{
		{Samples: 2, MeanRank: 1}, {}, {}, {}, {}, {},
	}}
	var output bytes.Buffer
	reportDetectCandidateMeans(&output, result, rankView{
		endRows: 4, pinned: map[string]bool{"unobserved-c": true},
	}, labels)
	for _, label := range labels[1:] {
		if strings.Count(output.String(), label) != 1 {
			t.Errorf("unobserved identity %q not preserved exactly once:\n%s",
				label, output.String())
		}
	}
	if !strings.Contains(output.String(), "unobserved-c (pinned)") {
		t.Errorf("pinned unobserved identity is not annotated:\n%s", output.String())
	}
	assertTextLinesAtMost(t, output.String(), textWidth)
}

func TestDetectHTTPStatusSummary(t *testing.T) {
	t.Run("uniform observations ignore unsampled candidates", func(t *testing.T) {
		result := detectRunResult{Requests: []detectRequestEvidence{
			{
				Request: 0, Participations: 2,
				StatusCounts: []detectStatusCount{{Status: 204, Count: 2}},
			},
			{Request: 1},
			{
				Request: 2, Participations: 3,
				StatusCounts: []detectStatusCount{{Status: 204, Count: 3}},
			},
		}}
		var output bytes.Buffer
		reportDetectHTTPStatuses(&output, result, []string{"A", "B", "C"})
		if !strings.Contains(output.String(),
			"HTTP 204 for all 5 ranked request participations across 2 sampled requests.") {
			t.Fatalf("uniform summary:\n%s", output.String())
		}
	})

	t.Run("heterogeneous observations group deterministically", func(t *testing.T) {
		result := detectRunResult{Requests: []detectRequestEvidence{
			{
				Request: 0, Participations: 5,
				StatusCounts: []detectStatusCount{{Status: 204, Count: 5}},
			},
			{
				Request: 1, Participations: 4,
				StatusCounts: []detectStatusCount{{Status: 204, Count: 4}},
			},
			{
				Request: 2, Participations: 2,
				StatusCounts: []detectStatusCount{{Status: 404, Count: 2}},
			},
			{Request: 3, Participations: 3, StatusCounts: []detectStatusCount{
				{Status: 204, Count: 2}, {Status: 429, Count: 1},
			}},
		}, Responses: detectResponseReview{
			Changing: []int{3}, Non2xx: []int{2, 3},
		}}
		var output bytes.Buffer
		reportDetectHTTPStatuses(&output, result,
			[]string{"ordinary-a", "ordinary-b", "missing", "throttled"})
		flat := strings.Join(strings.Fields(output.String()), " ")
		for _, want := range []string{
			"HTTP 204 only 2 requests; 9 participations",
			"HTTP 404 only 1 request; 2 participations: missing",
			"HTTP 204 and 429 1 request; 3 participations: throttled",
			"HTTP status changed across ranked responses for throttled",
			"non-2xx responses contributed ranks for missing and throttled",
		} {
			if !strings.Contains(flat, want) {
				t.Errorf("grouped summary lacks %q:\n%s", want, output.String())
			}
		}
	})
}
