package main

import (
	"strings"
	"testing"
)

func TestProjectRunReferencesNarrow(t *testing.T) {
	t.Parallel()
	labels := []string{"A", "group:TOKEN=value"}
	refs := projectRunReferences(labels, 24)
	if refs.compact {
		t.Fatal("compact = true, want labels-only projection")
	}
	for i, ref := range refs.refs {
		if ref.ID != i || ref.Label != labels[i] || ref.Inline != labels[i] {
			t.Errorf("ref %d = %+v, want unchanged label %q", i, ref, labels[i])
		}
	}
}

func TestProjectRunReferencesWideSwitchesRun(t *testing.T) {
	t.Parallel()
	const width = 18
	labels := []string{"A", "named:TOKEN=extraordinarily-long-value"}
	refs := projectRunReferences(labels, width)
	if !refs.compact {
		t.Fatal("compact = false, want run-wide compact projection")
	}
	if refs.refs[0].Inline != "#0 A" {
		t.Errorf("short Inline = %q, want %q", refs.refs[0].Inline, "#0 A")
	}
	long := refs.refs[1].Inline
	if len(long) > width {
		t.Errorf("long Inline has width %d, want <= %d: %q", len(long), width, long)
	}
	if !strings.HasPrefix(long, "#1 named:") ||
		!strings.HasSuffix(long, "-value") ||
		!strings.Contains(long, "..") {
		t.Errorf("long Inline = %q, want ID and both label ends", long)
	}
}

func TestProjectRunReferencesAccountsForIDDigits(t *testing.T) {
	t.Parallel()
	labels := make([]string, 101)
	for i := range labels {
		labels[i] = "short"
	}
	labels[0] = strings.Repeat("x", 30)
	refs := projectRunReferences(labels, 12)
	if got := refs.refs[100].Inline; got != "#100 short" {
		t.Errorf("Inline = %q, want %q", got, "#100 short")
	}
	if len(refs.refs[0].Inline) > 12 {
		t.Errorf("wide reference = %q, want <= 12 columns", refs.refs[0].Inline)
	}
}

func TestProjectRunReferencesProjectsPins(t *testing.T) {
	t.Parallel()
	labels := []string{strings.Repeat("x", 30), "B"}
	refs := projectRunReferences(labels, 12)
	view := refs.projectRankView(rankView{
		pinned: map[string]bool{labels[0]: true},
	})

	if !view.pinned[refs.refs[0].Inline] {
		t.Fatalf("projected pin %q is absent", refs.refs[0].Inline)
	}
	if view.pinned[labels[0]] {
		t.Fatalf("complete label %q remains pinned", labels[0])
	}
}

func TestReportersDeriveReferenceWidthsFromLayouts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		width int
	}{
		{name: "race", width: raceReferenceWidth()},
		{name: "measure", width: rankProfileLabelWidth},
		{name: "detect", width: detectCandidateReferenceWidth()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.width < 3 {
				t.Fatalf("reference width = %d, cannot hold an excerpt", tt.width)
			}
			fits := projectRunReferences([]string{strings.Repeat("x", tt.width)}, tt.width)
			if fits.compact {
				t.Fatalf("label of width %d unexpectedly compacted", tt.width)
			}
			wide := projectRunReferences([]string{strings.Repeat("x", tt.width+1)}, tt.width)
			if !wide.compact {
				t.Fatalf("label wider than %d did not compact", tt.width)
			}
		})
	}
}
