package main

import (
	"cmp"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
)

// rankProfileLabelWidth keeps compact identities stable across the profile and
// exact-count views. Count vectors may deliberately exceed the report width.
const rankProfileLabelWidth = 12

type rankProfileEntry struct {
	label      string
	meanRank   float64
	rankCounts []int
	samples    int
	pinned     bool
}

type rankProfileRow struct {
	entry   int
	elision string
}

// elisionLine describes a hidden run in a measured rank profile. lo and hi
// remain infinite when the run contains no sampled rows.
func elisionLine(n int, lo, hi float64) string {
	if math.IsInf(lo, 1) {
		return fmt.Sprintf(
			"  ... %d more %s, not sampled (--rank-rows limits rows) ...",
			n, plural(n))
	}
	return fmt.Sprintf(
		"  ... %d more %s, mean rank %.2f to %.2f (--rank-rows limits rows) ...",
		n, plural(n), lo, hi)
}

func reportRankProfile(
	w io.Writer,
	entries []rankProfileEntry,
	view rankView,
) {
	if len(entries) == 0 {
		return
	}
	rows := selectRankProfileRows(entries, view)
	width := len(entries[0].rankCounts)
	cells := rankProfileCells(width)
	emitln(w, "Rank profile (first -> last):")
	if width > cells {
		emitf(w, "  Binning: %d cells; up to %d adjacent ranks per cell.\n",
			cells, (width+cells-1)/cells)
	}
	emitln(w, "  Intensity (% upper bound): . 0 | : 12.5 | - 25 | = 37.5 | + 50")
	emitln(w, "                             * 62.5 | # 75 | % 87.5 | @ 100")

	labels := make([]string, len(entries))
	for i := range entries {
		labels[i] = entries[i].label
	}
	labelWidth := max(labelColumn(labels), len("request"))
	profileWidth := cells + 2
	emitf(w, "  %-*s  %-*s  %5s  %10s\n",
		labelWidth, "request", profileWidth, "profile", "mean", "normalised")
	for _, row := range rows {
		if row.elision != "" {
			emitln(w, row.elision)
			continue
		}
		entry := entries[row.entry]
		normalised := entry.meanRank / float64(width-1)
		emitf(w, "  %-*s  %-*s  %5.2f  %10.2f\n",
			labelWidth, entry.label, profileWidth,
			rankHeatStrip(entry.rankCounts, entry.samples),
			entry.meanRank, normalised)
	}

	emitf(w, "\nRank counts (columns are ranks 0..%d; %s):\n",
		width-1, countedNoun(entries[0].samples, "retained trial"))
	countWidths := rankCountWidths(entries, width)
	for _, row := range rows {
		if row.elision != "" {
			emitln(w, row.elision)
			continue
		}
		entry := entries[row.entry]
		emitf(w, "  %-*s  %s\n", labelWidth, entry.label,
			formatRankCountVector(entry.rankCounts, countWidths))
	}
}

func rankProfileCells(width int) int {
	if width > 16 {
		return 16
	}
	return 8
}

func rankHeatStrip(counts []int, samples int) string {
	cells := rankProfileCells(len(counts))
	strip := make([]byte, cells)
	if len(counts) <= cells {
		for cell := range cells {
			rank := cell * len(counts) / cells
			strip[cell] = rankHeatSymbol(counts[rank], samples)
		}
	} else {
		for cell := range cells {
			start := cell * len(counts) / cells
			end := (cell + 1) * len(counts) / cells
			total := 0
			for _, count := range counts[start:end] {
				total += count
			}
			strip[cell] = rankHeatSymbol(total, samples)
		}
	}
	return "[" + string(strip) + "]"
}

func rankHeatSymbol(count, samples int) byte {
	const scale = ".:-=+*#%@"
	if count <= 0 || samples <= 0 {
		return scale[0]
	}
	band := int(math.Ceil(8 * float64(count) / float64(samples)))
	return scale[min(band, len(scale)-1)]
}

func rankCountWidths(entries []rankProfileEntry, width int) []int {
	widths := make([]int, width)
	for _, entry := range entries {
		for rank, count := range entry.rankCounts {
			widths[rank] = max(widths[rank], len(strconv.Itoa(count)))
		}
	}
	return widths
}

func formatRankCountVector(counts, widths []int) string {
	cells := make([]string, len(counts))
	for rank, count := range counts {
		cells[rank] = fmt.Sprintf("%*d", widths[rank], count)
	}
	return "[ " + strings.Join(cells, "  ") + " ]"
}

func selectRankProfileRows(
	entries []rankProfileEntry,
	view rankView,
) []rankProfileRow {
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(entries[a].meanRank, entries[b].meanRank)
	})

	show := make([]bool, len(entries))
	if view.allRows || view.endRows > 0 && view.endRows >= len(entries)/2 {
		for i := range show {
			show[i] = true
		}
	} else {
		for rank, entry := range order {
			if rank < view.endRows || rank >= len(order)-view.endRows ||
				entries[entry].pinned {
				show[entry] = true
			}
		}
	}

	rows := make([]rankProfileRow, 0, len(entries))
	run, runLo, runHi := 0, math.Inf(1), math.Inf(-1)
	flush := func() {
		if run == 0 {
			return
		}
		rows = append(rows, rankProfileRow{
			entry: -1, elision: elisionLine(run, runLo, runHi),
		})
		run, runLo, runHi = 0, math.Inf(1), math.Inf(-1)
	}
	for _, entry := range order {
		if show[entry] {
			flush()
			rows = append(rows, rankProfileRow{entry: entry})
			continue
		}
		run++
		runLo = min(runLo, entries[entry].meanRank)
		runHi = max(runHi, entries[entry].meanRank)
	}
	flush()
	return rows
}
