package ranking_test

import (
	"testing"

	"github.com/tantosec/tturl/internal/ranking"
)

// TestProgressCadenceExternalDrive verifies that an external solver can drive a
// cadence and receive metered progress.
func TestProgressCadenceExternalDrive(t *testing.T) {
	cadence := ranking.EveryComparisons(2)
	run := cadence.Start()
	var cores []ranking.ProgressCore
	deliver := func(core ranking.ProgressCore) { cores = append(cores, core) }

	// Ten comparisons at width 4: EveryComparisons(2) is due on 2,4,6,8,10.
	for i := 1; i <= 10; i++ {
		run.ReportIfDue(i, i*4, deliver)
	}
	run.ReportFinal(10, 40, deliver)

	if len(cores) != 6 {
		t.Fatalf("got %d reports, want 6 (5 periodic + 1 final)", len(cores))
	}
	for i, core := range cores {
		wantComparisons := 2 * (i + 1)
		if i == len(cores)-1 {
			wantComparisons = 10
		}
		if core.Comparisons != wantComparisons || core.Participations != wantComparisons*4 {
			t.Errorf("report %d core = %d/%d, want %d/%d", i,
				core.Comparisons, core.Participations, wantComparisons, wantComparisons*4)
		}
	}

	// A nil deliver is inert: no delivery, no panic.
	before := len(cores)
	run.ReportIfDue(11, 44, nil)
	run.ReportFinal(11, 44, nil)
	if len(cores) != before {
		t.Errorf("nil deliver should be inert; reports went %d -> %d", before, len(cores))
	}
}
