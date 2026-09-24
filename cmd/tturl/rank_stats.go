package main

import "github.com/tantosec/tturl/tth2"

// requestRankStats summarises one request across the trials in a summary. Every
// field derives from arrival ranks.
type requestRankStats struct {
	// N is the number of trials included. Other scalar fields are zero when N is
	// zero.
	N int

	// RankCounts aliases the corresponding summary row and sums to N.
	RankCounts []int

	// MeanRank is the mean arrival rank. Zero means always first, and
	// len(RankCounts)-1 means always last.
	MeanRank float64
}

// requestStats returns per-request statistics, index-aligned with the requests
// the run was given.
//
// The returned slice is newly allocated, but each RankCounts aliases the
// corresponding summary row. With no trials the entries have N == 0; with no
// requests the slice is empty.
func requestStats(s *tth2.TrialSummary) []requestRankStats {
	return statsFromRankCounts(s.RankCounts, s.Trials)
}

// positionStats returns the same statistics keyed by stream position and
// aligned with batch positions. Its ownership matches requestStats.
func positionStats(s *tth2.TrialSummary) []requestRankStats {
	return statsFromRankCounts(s.PositionRankCounts, s.Trials)
}

// statsFromRankCounts turns one rank-count matrix into per-row statistics,
// where a row is whatever the matrix is keyed by. Each row sums to trials.
func statsFromRankCounts(counts [][]int, trials int) []requestRankStats {
	out := make([]requestRankStats, len(counts))
	for i, row := range counts {
		rs := requestRankStats{N: trials, RankCounts: row}
		if trials > 0 {
			rs.MeanRank = meanRank(row, trials)
		}
		out[i] = rs
	}
	return out
}

// meanRank folds a row of rank counts into a mean arrival rank. Callers
// establish trials > 0.
func meanRank(row []int, trials int) float64 {
	var sum int64
	for rank, count := range row {
		sum += int64(rank) * int64(count)
	}
	return float64(sum) / float64(trials)
}
