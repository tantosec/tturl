package main

import (
	"bytes"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/tantosec/tturl/internal/ranking"
	"github.com/tantosec/tturl/tth2"
)

func TestDetectEvidenceCollectorReviewsHTTPStatuses(t *testing.T) {
	t.Parallel()
	collector := newDetectEvidenceCollector(4)
	collector.observe([]ranking.ItemID{0, 1}, detectBatch(1, 200, 404))
	collector.observe([]ranking.ItemID{0, 1}, detectBatch(2, 500, 404))
	collector.observe([]ranking.ItemID{1, 2}, detectBatch(2, 404, 200))
	rankingResult := ranking.OutlierResult{
		Index: -1,
		Cost:  ranking.Cost{Comparisons: 3, Participations: 6},
		Items: make([]ranking.ItemStat, 4),
	}
	result, err := collector.result(rankingResult)
	if err != nil {
		t.Fatal(err)
	}
	if result.Execution.ConnectionsObserved != 2 ||
		result.Requests[0].Participations != 2 ||
		result.Requests[1].Participations != 3 ||
		result.Requests[2].Participations != 1 ||
		result.Requests[3].Participations != 0 {
		t.Fatalf("collected evidence = %+v", result)
	}
	if !slices.Equal(result.Responses.Changing, []int{0}) ||
		!slices.Equal(result.Responses.Different, []int{0, 1, 2}) ||
		!slices.Equal(result.Responses.Non2xx, []int{0, 1}) {
		t.Fatalf("response review = %+v", result.Responses)
	}

	result.Ranking.Items = []ranking.ItemStat{
		{Samples: 2}, {Samples: 3}, {Samples: 1}, {},
	}
	var output bytes.Buffer
	reportDetectHTTPStatuses(&output, result, []string{"A", "B", "C", "D"})
	report := output.String()
	flat := strings.Join(strings.Fields(report), " ")
	for _, want := range []string{
		"HTTP 200 only 1 request; 1 participation: C",
		"HTTP 200 and 500 1 request; 2 participations: A",
		"HTTP 404 only 1 request; 3 participations: B",
		"HTTP status changed across ranked responses for A",
		"non-2xx responses contributed ranks for A and B",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("status report lacks %q:\n%s", want, report)
		}
	}
}

func TestDetectEvidenceCollectorSeparatesBaselineEvidence(t *testing.T) {
	collector := newDetectEvidenceCollector(2, 2)
	collector.observeResolved([]detectEvidenceRef{
		{role: requestGroupCandidate, index: 0},
		{role: requestGroupBaseline, index: 1},
		{role: requestGroupBaseline, index: 0},
	}, detectBatch(1, 200, 404, 204))
	collector.observeResolved([]detectEvidenceRef{
		{role: requestGroupBaseline, index: 0},
		{role: requestGroupCandidate, index: 1},
		{role: requestGroupBaseline, index: 1},
	}, detectBatch(1, 500, 200, 404))
	result, err := collector.result(ranking.OutlierResult{
		Index: -1,
		Cost:  ranking.Cost{Comparisons: 2, Participations: 6},
		Items: make([]ranking.ItemStat, 2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Requests[0].Participations != 1 ||
		result.Requests[1].Participations != 1 ||
		result.Baselines[0].Participations != 2 ||
		result.Baselines[1].Participations != 2 {
		t.Fatalf("role evidence = %+v", result)
	}
	if !slices.Equal(result.Responses.BaselineChanging, []int{0}) ||
		!slices.Equal(result.Responses.BaselineNon2xx, []int{0, 1}) ||
		!slices.Equal(result.Responses.BaselineDifferent, []int{0, 1}) {
		t.Errorf("baseline response review = %+v", result.Responses)
	}
	var output bytes.Buffer
	reportDetectHTTPStatuses(
		&output, result, []string{"candidate-a", "candidate-b"},
		[]string{"control-a", "control-b"})
	flat := strings.Join(strings.Fields(output.String()), " ")
	for _, want := range []string{
		"Baseline control HTTP status patterns:",
		"HTTP 204 and 500: control-a",
		"HTTP 404 only: control-b",
		"status changed across ranked baseline responses for control-a",
		"non-2xx baseline responses contributed ranks for control-a and control-b",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("baseline status report lacks %q:\n%s", want, output.String())
		}
	}
}

func TestDetectUnavailableResultUsesObservedRanksOnly(t *testing.T) {
	collector := newDetectEvidenceCollector(3)
	collector.observe(
		[]ranking.ItemID{0, 1, 2}, detectBatch(1, 200, 200, 200))
	collector.observe(
		[]ranking.ItemID{2, 1, 0}, detectBatch(1, 200, 200, 200))
	wantErr := errors.New("comparison failed")
	result := collector.unavailable(
		wantErr, failedRun(wantErr, completionComparisonFailed))
	if !result.Unavailable || result.Ranking.Comparisons != 2 ||
		result.Ranking.Participations != 6 ||
		result.Ranking.Items[0].Samples != 2 ||
		result.Ranking.Items[0].MeanRank != 1 ||
		result.Ranking.Items[1].MeanRank != 1 ||
		result.Ranking.Items[2].MeanRank != 1 {
		t.Fatalf("unavailable evidence = %+v", result)
	}
	var output bytes.Buffer
	reportDetectResult(&output, result, detectReportContext{}, 3,
		rankView{allRows: true}, fullRunReferences([]string{"A", "B", "C"}))
	text := output.String()
	for _, want := range []string{
		"Decision unavailable:", "Exploratory candidate means", "1.00",
		"Completion: failed.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("unavailable report omits %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "<-") || strings.Contains(text, "[===") {
		t.Errorf("unavailable report fabricates a selection or bar:\n%s", text)
	}
}

func TestDetectEvidenceCollectorConcurrent(t *testing.T) {
	t.Parallel()
	const comparisons = 96
	collector := newDetectEvidenceCollector(3)
	connections := [...]tth2.ConnectionID{1, 2, 3}
	var wait sync.WaitGroup
	for comparison := range comparisons {
		wait.Add(1)
		go func(comparison int) {
			defer wait.Done()
			collector.observe(
				[]ranking.ItemID{ranking.ItemID(comparison % 3), ranking.ItemID((comparison + 1) % 3)},
				detectBatch(connections[comparison%3], 204, 204))
		}(comparison)
	}
	wait.Wait()
	result, err := collector.result(ranking.OutlierResult{
		Index: -1,
		Cost: ranking.Cost{
			Comparisons: comparisons, Participations: comparisons * 2,
		},
		Items: make([]ranking.ItemStat, 3),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Execution.ConnectionsObserved != 3 ||
		len(result.Responses.Changing) != 0 ||
		len(result.Responses.Different) != 0 ||
		len(result.Responses.Non2xx) != 0 {
		t.Fatalf("concurrent evidence = %+v", result)
	}
	for request := range result.Requests {
		if result.Requests[request].Participations != 64 ||
			!slices.Equal(result.Requests[request].StatusCounts,
				[]detectStatusCount{{Status: 204, Count: 64}}) {
			t.Errorf("request %d evidence = %+v", request,
				result.Requests[request])
		}
	}
}

func TestDetectEvidenceCollectorRejectsAccountingMismatch(t *testing.T) {
	t.Parallel()
	collector := newDetectEvidenceCollector(2)
	collector.observe([]ranking.ItemID{0, 1}, detectBatch(1, 200, 200))
	_, err := collector.result(ranking.OutlierResult{
		Cost: ranking.Cost{Comparisons: 2, Participations: 4},
	})
	if err == nil || !strings.Contains(err.Error(), "internal consistency error") {
		t.Fatalf("mismatch error = %v", err)
	}
}

func detectBatch(connection tth2.ConnectionID, statuses ...int) *tth2.BatchResult {
	results := make([]tth2.Result, len(statuses))
	order := make([]int, len(statuses))
	for position, status := range statuses {
		results[position] = tth2.Result{
			Response: &tth2.Response{
				Status: http.StatusText(status), StatusCode: status,
			},
			ArrivalRank: position,
		}
		order[position] = position
	}
	return &tth2.BatchResult{
		Connection: connection, Results: results, ArrivalOrder: order,
	}
}

func TestDetectStatusDistributionsCompareProportions(t *testing.T) {
	for _, test := range []struct {
		name  string
		right []detectStatusCount
		equal bool
	}{
		{"same proportions", []detectStatusCount{{200, 4}, {404, 2}}, true},
		{"different proportions", []detectStatusCount{{200, 1}, {404, 2}}, false},
		{"different support", []detectStatusCount{{200, 3}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			left := []detectStatusCount{{200, 2}, {404, 1}}
			if got := equalStatusDistributions(left, test.right); got != test.equal {
				t.Fatalf("equal distributions = %t, want %t", got, test.equal)
			}
			review := reviewDetectResponses([]detectRequestEvidence{
				{Request: 0, Participations: 3, StatusCounts: left},
				{Request: 1, Participations: 3, StatusCounts: test.right},
			})
			if (len(review.Different) == 0) != test.equal {
				t.Fatalf("response distribution review = %+v", review)
			}
		})
	}
}
