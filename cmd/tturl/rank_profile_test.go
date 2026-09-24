package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tantosec/tturl/tth2"
)

func TestRankHeatSymbolThresholds(t *testing.T) {
	tests := []struct {
		count int
		want  byte
	}{
		{0, '.'},
		{1, ':'},
		{10, ':'},
		{11, '-'},
		{20, '-'},
		{21, '='},
		{30, '='},
		{31, '+'},
		{40, '+'},
		{41, '*'},
		{50, '*'},
		{51, '#'},
		{60, '#'},
		{61, '%'},
		{70, '%'},
		{71, '@'},
		{80, '@'},
	}
	for _, test := range tests {
		if got := rankHeatSymbol(test.count, 80); got != test.want {
			t.Errorf("rankHeatSymbol(%d, 80) = %q, want %q",
				test.count, got, test.want)
		}
	}
}

func TestRankHeatStripWidthAndBinning(t *testing.T) {
	tests := []struct {
		name    string
		counts  []int
		samples int
		want    string
	}{
		{"width two expands ranks", []int{12, 0}, 12, "[@@@@....]"},
		{
			"width sixteen uses eight bins", repeatedInts(16, 1), 16,
			"[::::::::]",
		},
		{
			"width seventeen uses sixteen bins", repeatedInts(17, 1), 17,
			"[::::::::::::::::]",
		},
		{
			"width thirty-two uses sixteen bins", repeatedInts(32, 1), 32,
			"[::::::::::::::::]",
		},
		{
			"width 256 uses sixteen bins", repeatedInts(256, 1), 256,
			"[::::::::::::::::]",
		},
	}
	concentrated := make([]int, 32)
	concentrated[0] = 32
	tests = append(tests, struct {
		name    string
		counts  []int
		samples int
		want    string
	}{
		"wide concentration combines contiguous ranks", concentrated, 32,
		"[@...............]",
	})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := rankHeatStrip(test.counts, test.samples); got != test.want {
				t.Fatalf("rankHeatStrip() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRankProfileSyntheticShapesWithProjectedLabels(t *testing.T) {
	labels := []string{
		"deterministic-response", "overlapping-response",
		"diffuse-response", "multimodal-response",
	}
	refs := projectRunReferences(labels, rankProfileLabelWidth)
	projected := refs.inlineLabels()
	entries := []rankProfileEntry{
		{
			label: projected[0], meanRank: 0,
			rankCounts: []int{80, 0, 0, 0, 0, 0, 0, 0}, samples: 80,
		},
		{
			label: projected[1], meanRank: 2,
			rankCounts: []int{0, 20, 40, 20, 0, 0, 0, 0}, samples: 80,
		},
		{
			label: projected[2], meanRank: 3.5,
			rankCounts: []int{10, 10, 10, 10, 10, 10, 10, 10}, samples: 80,
		},
		{
			label: projected[3], meanRank: 3.5,
			rankCounts: []int{40, 0, 0, 0, 0, 0, 0, 40}, samples: 80,
		},
	}
	var output bytes.Buffer
	reportRankProfile(&output, entries, rankView{allRows: true})

	wantProfiles := []string{
		"[@.......]", "[.-+-....]", "[::::::::]", "[+......+]",
	}
	profileColumn := -1
	for i, entry := range entries {
		want := fmt.Sprintf("  %-*s  %s",
			rankProfileLabelWidth, entry.label, wantProfiles[i])
		if !strings.Contains(output.String(), want) {
			t.Errorf("profile omits synthetic shape %q:\n%s", want, output.String())
			continue
		}
		lineStart := strings.Index(output.String(), want)
		line, _, _ := strings.Cut(output.String()[lineStart:], "\n")
		column := strings.IndexByte(line, '[')
		if profileColumn < 0 {
			profileColumn = column
		} else if column != profileColumn {
			t.Errorf("profile for %q begins in column %d, want %d:\n%s",
				entry.label, column, profileColumn, output.String())
		}
	}
	if !refs.compact {
		t.Fatal("synthetic long labels were not projected")
	}
	if strings.Contains(output.String(), labels[0]) {
		t.Errorf("profile bypassed projected labels:\n%s", output.String())
	}
}

func TestRankProfileReportsBinningAcrossCellThreshold(t *testing.T) {
	for _, test := range []struct {
		width int
		want  string
	}{
		{16, "Binning: 8 cells; up to 2 adjacent ranks per cell."},
		{17, "Binning: 16 cells; up to 2 adjacent ranks per cell."},
	} {
		counts := repeatedInts(test.width, 1)
		var output bytes.Buffer
		reportRankProfile(&output, []rankProfileEntry{{
			label: "request", meanRank: float64(test.width-1) / 2,
			rankCounts: counts, samples: test.width,
		}}, rankView{allRows: true})
		if !strings.Contains(output.String(), test.want) {
			t.Errorf("width %d report omits %q:\n%s",
				test.width, test.want, output.String())
		}
	}
}

func TestReportRankProfileKeepsExactWideVector(t *testing.T) {
	const width = 32
	counts := repeatedInts(width, 31_250_000)
	entry := rankProfileEntry{
		label: "request-name", meanRank: 15.5, rankCounts: counts,
		samples: 1_000_000_000,
	}
	var output bytes.Buffer
	reportRankProfile(&output, []rankProfileEntry{entry}, rankView{allRows: true})

	wantVector := formatRankCountVector(counts, rankCountWidths(
		[]rankProfileEntry{entry}, width))
	if !strings.Contains(output.String(), wantVector) {
		t.Fatalf("wide report omitted its exact vector:\n%s", output.String())
	}
	if !strings.Contains(output.String(),
		"  Binning: 16 cells; up to 2 adjacent ranks per cell.") {
		t.Fatalf("wide report omitted its bin resolution:\n%s", output.String())
	}
	if strings.Count(wantVector, "31250000") != width {
		t.Fatalf("wide vector is incomplete: %s", wantVector)
	}
	for line := range strings.SplitSeq(output.String(), "\n") {
		if strings.Contains(line, "31250000") && len(line) < 300 {
			t.Fatalf("renderer unexpectedly wrapped the wide vector at %d columns", len(line))
		}
	}
}

func TestTextAndJSONRankCountsUseTheSameEvidence(t *testing.T) {
	t.Run("measure", func(t *testing.T) {
		measured := measurementFromRanks(
			[][]int{{0, 1}, {1, 0}, {0, 1}}, 3, 2, tth2.ArrangeRandom)
		refs := projectRunReferences([]string{"A", "B"}, rankProfileLabelWidth)
		var textOutput bytes.Buffer
		reportMeasurement(&textOutput, measured, allRowsView, refs)

		var jsonOutput bytes.Buffer
		if err := startMeasureJSONCatalogue(t, &jsonOutput, measured).Summary(measured); err != nil {
			t.Fatal(err)
		}
		var summary jsonMeasureResultRecord
		if err := json.Unmarshal(lastJSONRecord(t, jsonOutput.Bytes()), &summary); err != nil {
			t.Fatal(err)
		}
		entries := make([]rankProfileEntry, len(summary.Measurement.Requests))
		for i, request := range summary.Measurement.Requests {
			entries[i] = rankProfileEntry{rankCounts: denseMeasureRankCounts(request.RankCounts, measured.width)}
		}
		widths := rankCountWidths(entries, len(entries))
		for request, stats := range summary.Measurement.Requests {
			want := formatRankCountVector(denseMeasureRankCounts(stats.RankCounts, measured.width), widths)
			if !strings.Contains(textOutput.String(), want) {
				t.Errorf("measure text omits JSON rank counts for request %d: %s",
					request, want)
			}
		}
	})

	t.Run("analyse", func(t *testing.T) {
		result := analyseOffline(
			analyseOrderedEvidence(2, 6, func(int) []int { return []int{0, 1} }),
			analyseConfig{RandomisationSeed: 7},
		)
		labels := []string{"A", "B"}
		var textOutput bytes.Buffer
		if err := reportAnalyse(&textOutput, result, labels); err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := reportAnalyseJSON(&output, result, labels); err != nil {
			t.Fatal(err)
		}
		replayed, err := replayAnalyseJSONStream(bytes.NewReader(output.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if len(replayed.Requests) != len(result.Requests) {
			t.Fatalf("replayed requests = %d, want %d",
				len(replayed.Requests), len(result.Requests))
		}
		entries := make([]rankProfileEntry, len(replayed.Requests))
		for i, request := range replayed.Requests {
			entries[i] = rankProfileEntry{rankCounts: request.RankCounts}
		}
		widths := rankCountWidths(entries, len(entries))
		for request := range result.Requests {
			if !reflect.DeepEqual(
				result.Requests[request].RankCounts,
				replayed.Requests[request].RankCounts,
			) {
				t.Errorf("analyse request %d rank counts = %v after JSON replay; want %v",
					request, replayed.Requests[request].RankCounts,
					result.Requests[request].RankCounts)
			}
			want := formatRankCountVector(
				replayed.Requests[request].RankCounts, widths)
			if !strings.Contains(textOutput.String(), want) {
				t.Errorf("analyse text omits replayed JSON rank counts for request %d: %s",
					request, want)
			}
		}
	})
}

func TestReportRankProfileRepeatsSharedElision(t *testing.T) {
	entries := make([]rankProfileEntry, 10)
	for i := range entries {
		counts := make([]int, 10)
		counts[i] = 1
		entries[i] = rankProfileEntry{
			label: fmt.Sprintf("r%d", i), meanRank: float64(i),
			rankCounts: counts, samples: 1,
		}
	}
	var output bytes.Buffer
	reportRankProfile(&output, entries, rankView{endRows: 2})
	if got := strings.Count(output.String(),
		"... 6 more requests, mean rank 2.00 to 7.00"); got != 2 {
		t.Fatalf("shared elision occurred %d times, want profile and matrix:\n%s",
			got, output.String())
	}
}

func TestRankProfileCompactColumnLayout(t *testing.T) {
	tests := []struct {
		name                string
		width               int
		label               string
		wantHeader, wantRow int
	}{
		{"width two", 2, "target", 40, 40},
		{"width eight", 8, "target", 40, 40},
		{"width seventeen", 17, "target", 48, 48},
		{"long label width eight", 8, strings.Repeat("x", 32), 65, 65},
		{"long label width seventeen", 17, strings.Repeat("x", 32), 73, 73},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			counts := make([]int, test.width)
			counts[0] = 1
			var output bytes.Buffer
			reportRankProfile(&output, []rankProfileEntry{{
				label: test.label, meanRank: 0, rankCounts: counts, samples: 1,
			}}, rankView{allRows: true})

			checked := 0
			var headerLine, rowLine string
			for line := range strings.SplitSeq(
				strings.Split(output.String(), "\nRank counts")[0], "\n",
			) {
				isHeader := strings.HasPrefix(line, "  request")
				if isHeader || strings.HasPrefix(line, "  "+test.label+" ") {
					checked++
					if isHeader {
						headerLine = line
					} else {
						rowLine = line
					}
					want := test.wantRow
					if isHeader {
						want = test.wantHeader
					}
					if len(line) != want {
						t.Errorf("table line width = %d, want %d:\n%s",
							len(line), want, line)
					}
					if isHeader && (!strings.Contains(line, "normalised") ||
						strings.Contains(line, "normalised mean rank")) {
						t.Errorf("compact normalised column changed:\n%s", line)
					}
				}
			}
			if checked != 2 {
				t.Fatalf("checked %d profile table lines, want header and row:\n%s",
					checked, output.String())
			}
			if got, want := strings.Index(rowLine, "["),
				strings.Index(headerLine, "profile"); got != want {
				t.Errorf("profile starts in column %d, heading starts in %d:\n%s\n%s",
					got, want, headerLine, rowLine)
			}
			for _, heading := range []string{"mean", "normalised"} {
				headingEnd := strings.Index(headerLine, heading) + len(heading)
				valueEnd := strings.LastIndex(rowLine[:headingEnd], "0") + 1
				if valueEnd != headingEnd {
					t.Errorf("%s value does not end under its heading:\n%s\n%s",
						heading, headerLine, rowLine)
				}
			}
		})
	}
}

func repeatedInts(count, value int) []int {
	result := make([]int, count)
	for i := range result {
		result[i] = value
	}
	return result
}
