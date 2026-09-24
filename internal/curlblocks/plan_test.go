package curlblocks

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// TestBlockError checks the rule only the parse knows: a block the user opened
// is named, the implicit block the parser supplies for a bare preamble is not.
func TestBlockError(t *testing.T) {
	sentinel := errors.New("boom")

	opened, err := New().Parse([]string{"--block", "https://a", "--block", "https://b"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := opened.BlockError(1, sentinel).Error(); got != "block 2: boom" {
		t.Errorf("BlockError(1) = %q, want %q", got, "block 2: boom")
	}
	if !errors.Is(opened.BlockError(1, sentinel), sentinel) {
		t.Error("BlockError does not wrap the original error")
	}

	implicit, err := New().Parse([]string{"https://a"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := implicit.BlockError(0, sentinel); got.Error() != "boom" {
		t.Errorf("BlockError on an implicit block = %q, want the error unchanged", got)
	}

	if opened.BlockError(0, nil) != nil {
		t.Error("BlockError(nil) did not stay nil")
	}
}

// TestExpandPairsLabelsWithRequests is the point of Expand: every group gets
// exactly the labels naming its own requests. The expectations are derived from
// the labelling rules, not from a run: two URLs in the first block and three
// fan variants in the second, each --repeat 2, so labels arrive in group order
// with two per group, --repeat suffixes numbered within a name and fan members
// named by their value.
func TestExpandPairsLabelsWithRequests(t *testing.T) {
	plan, err := New().Parse([]string{
		"--block", "https://a/1", "https://a/2", "--repeat", "2", "--name", "plain",
		"--block", "https://b/FUZZ", "--vary", "FUZZ={x,y,z}", "--repeat", "2",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	groups, err := plan.Expand()
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}

	type want struct {
		url    string
		labels []string
		fanned bool
		block  int
	}
	wants := []want{
		{url: "https://a/1", labels: []string{"plain~1", "plain~2"}, block: 0},
		{url: "https://a/2", labels: []string{"plain~3", "plain~4"}, block: 0},
		{url: "https://b/x", labels: []string{"FUZZ=x~1", "FUZZ=x~2"}, fanned: true, block: 1},
		{url: "https://b/y", labels: []string{"FUZZ=y~1", "FUZZ=y~2"}, fanned: true, block: 1},
		{url: "https://b/z", labels: []string{"FUZZ=z~1", "FUZZ=z~2"}, fanned: true, block: 1},
	}
	if len(groups) != len(wants) {
		t.Fatalf("got %d groups, want %d", len(groups), len(wants))
	}
	for i, w := range wants {
		g := groups[i]
		if g.URL != w.url {
			t.Errorf("group %d URL = %q, want %q", i, g.URL, w.url)
		}
		if g.Repeat != 2 {
			t.Errorf("group %d Repeat = %d, want 2", i, g.Repeat)
		}
		if got := labelDisplays(g.Labels); !equalStrings(got, w.labels) {
			t.Errorf("group %d labels = %v, want %v", i, got, w.labels)
		}
		if (g.Fan != nil) != w.fanned {
			t.Errorf("group %d fanned = %v, want %v", i, g.Fan != nil, w.fanned)
		}
		if g.BlockIndex != w.block {
			t.Errorf("group %d BlockIndex = %d, want %d", i, g.BlockIndex, w.block)
		}
		// Every group's block carries that group's URL alone -- the substituted
		// variant for a fan member, the block narrowed to one URL otherwise -- so
		// the target is the same read either way.
		if len(g.Block.URLs) != 1 || g.Block.URLs[0] != w.url {
			t.Errorf("group %d block URLs = %v, want [%s]", i, g.Block.URLs, w.url)
		}
	}
	// Every variant of one fan shares a *Fan, so a consumer collapses the fan by
	// pointer identity.
	if groups[2].Fan != groups[3].Fan || groups[3].Fan != groups[4].Fan {
		t.Error("the fan's groups do not share one *Fan")
	}
	if groups[0].Fan != nil {
		t.Error("a plain group carries a Fan")
	}
}

// TestExpandErrors checks that Expand reports a block's own failure the way the
// parse would name it, and that a Plan built by hand with no Repeat is refused
// rather than silently expanding to nothing.
func TestExpandErrors(t *testing.T) {
	t.Run("a fan whose file cannot be read, inside a block", func(t *testing.T) {
		plan, err := New(WithFileReader(fakeFS(nil))).Parse(
			[]string{"--block", "https://a/FUZZ", "--block", "https://b/FUZZ", "--vary", "FUZZ=@missing.txt"})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		_, err = plan.Expand()
		if err == nil || !strings.HasPrefix(err.Error(), "block 2: ") {
			t.Errorf("Expand: %v, want an error prefixed with the block it came from", err)
		}
	})

	t.Run("a bare preamble names no block", func(t *testing.T) {
		plan, err := New(WithFileReader(fakeFS(nil))).Parse(
			[]string{"https://a/FUZZ", "--vary", "FUZZ=@missing.txt"})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		_, err = plan.Expand()
		if err == nil || strings.HasPrefix(err.Error(), "block ") {
			t.Errorf("Expand: %v, want an error naming no block", err)
		}
	})

	t.Run("a hand-built block with no Repeat", func(t *testing.T) {
		plan := &Plan{Blocks: []Block{{URLs: []string{"https://a"}}}}
		_, err := plan.Expand()
		if err == nil || !strings.Contains(err.Error(), "Repeat 0") {
			t.Errorf("Expand: %v, want the zero Repeat refused", err)
		}
	})
}

func labelDisplays(ls []Label) []string {
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = l.Display
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAssignLabelsShortfall verifies that total label count is checked before
// assignment, including a shortfall exactly on a group boundary.
func TestAssignLabelsShortfall(t *testing.T) {
	labels := []Label{{Display: "A"}, {Display: "B"}, {Display: "C"}, {Display: "D"}}
	tests := []struct {
		name    string
		repeats []int
		labels  []Label
	}{
		{"short on a group boundary", []int{2, 2, 2}, labels},
		{"short mid-group", []int{2, 3}, labels},
		{"too many labels", []int{2}, labels},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups := make([]RequestGroup, len(tt.repeats))
			for i, n := range tt.repeats {
				groups[i].Repeat = n
			}
			if err := assignLabels(groups, tt.labels); err == nil {
				t.Fatalf("assignLabels(%v repeats, %d labels) = nil, want an error",
					tt.repeats, len(tt.labels))
			}
			for i, g := range groups {
				if g.Labels != nil {
					t.Errorf("group %d was labelled anyway (%v); a refused assignment labels nothing", i, g.Labels)
				}
			}
		})
	}
}

// TestExpandLabelsPerGroup checks Expand's own invariant across the shapes a
// plan takes -- several URLs in a block, a fan, --repeat, and blocks of each
// mixed together: every group carries exactly Repeat labels, every label is
// used once, and none is shared between groups.
func TestExpandLabelsPerGroup(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"one bare URL", []string{"https://a"}},
		{"several URLs in one block", []string{"https://a", "https://b", "https://c"}},
		{"repeat", []string{"https://a", "--repeat", "3"}},
		{"a fan", []string{"https://h/F", "--vary", "F=1-4"}},
		{"a fan under repeat", []string{"https://h/F", "--vary", "F=1-4", "--repeat", "2"}},
		{"blocks of mixed shapes", []string{
			"--block", "https://a", "https://b",
			"--block", "https://h/F", "--vary", "F={x,y,z}",
			"--block", "https://c", "--repeat", "3", "--name", "tail",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := New().Parse(tt.args)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.args, err)
			}
			groups, err := plan.Expand()
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			seen := map[string]bool{}
			total := 0
			for i, g := range groups {
				if len(g.Labels) != g.Repeat {
					t.Errorf("group %d has %d labels for Repeat %d", i, len(g.Labels), g.Repeat)
				}
				for _, l := range g.Labels {
					if seen[l.Display] {
						t.Errorf("group %d reuses label %q", i, l.Display)
					}
					seen[l.Display] = true
				}
				total += g.Repeat
			}
			if len(seen) != total {
				t.Errorf("%d distinct labels across %d requests", len(seen), total)
			}
		})
	}
}

// TestExpandLabelSlicesDoNotOverlap checks that each group's Labels slice owns
// its append boundary. A caller extending one group must not overwrite a label
// already assigned to the next group.
func TestExpandLabelSlicesDoNotOverlap(t *testing.T) {
	plan, err := New().Parse([]string{"https://a", "https://b"})
	if err != nil {
		t.Fatal(err)
	}
	groups, err := plan.Expand()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("Expand produced %d groups, want 2", len(groups))
	}
	want := groups[1].Labels[0]
	groups[0].Labels = append(groups[0].Labels, Label{Display: "extra"})
	if got := groups[1].Labels[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("appending to group 0 changed group 1 label to %+v, want %+v",
			got, want)
	}
}
