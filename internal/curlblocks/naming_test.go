package curlblocks

import (
	"reflect"
	"strings"
	"testing"
)

// displaysOf projects the row labels out of a []Label.
func displaysOf(labels []Label) []string {
	out := make([]string, len(labels))
	for i, l := range labels {
		out[i] = l.Display
	}
	return out
}

func TestResolveNames(t *testing.T) {
	tests := []struct {
		name  string
		specs []NameSpec
		want  []string
	}{
		{
			name:  "empty",
			specs: nil,
			want:  []string{},
		},
		{
			name:  "all unnamed get spreadsheet letters",
			specs: []NameSpec{{Count: 1}, {Count: 1}, {Count: 1}},
			want:  []string{"A", "B", "C"},
		},
		{
			name:  "solo explicit name is used bare",
			specs: []NameSpec{{Name: "only", Count: 1}},
			want:  []string{"only"},
		},
		{
			name:  "repeat numbers with single-digit width",
			specs: []NameSpec{{Name: "fast", Count: 7}},
			want:  []string{"fast~1", "fast~2", "fast~3", "fast~4", "fast~5", "fast~6", "fast~7"},
		},
		{
			name:  "repeat crossing ten pads to two digits",
			specs: []NameSpec{{Name: "x", Count: 12}},
			want: []string{
				"x~01", "x~02", "x~03", "x~04", "x~05", "x~06",
				"x~07", "x~08", "x~09", "x~10", "x~11", "x~12",
			},
		},
		{
			name:  "one source with several specs numbers continuously",
			specs: []NameSpec{{Name: "request", Count: 10}, {Name: "request", Count: 5}},
			want: []string{
				"request~01", "request~02", "request~03", "request~04", "request~05",
				"request~06", "request~07", "request~08", "request~09", "request~10",
				"request~11", "request~12", "request~13", "request~14", "request~15",
			},
		},
		{
			name:  "multi-url block shares the name",
			specs: []NameSpec{{Name: "pair", Count: 1}, {Name: "pair", Count: 1}},
			want:  []string{"pair~1", "pair~2"},
		},
		{
			name:  "auto-label skips an explicitly claimed letter",
			specs: []NameSpec{{Count: 1}, {Name: "A", Count: 1}},
			want:  []string{"B", "A"},
		},
		{
			name:  "repeated unnamed request shares an available base",
			specs: []NameSpec{{Name: "B", Count: 1}, {Count: 3}},
			want:  []string{"B", "A~1", "A~2", "A~3"},
		},
		{
			name:  "named spec then unnamed run",
			specs: []NameSpec{{Count: 1}, {Name: "slow", Count: 1}, {Count: 2}},
			want:  []string{"A", "slow", "B~1", "B~2"},
		},
		{
			name:  "fan values label as token=value",
			specs: []NameSpec{{Tokens: []string{"FUZZ"}, Rows: [][]string{{"0"}, {"50"}}, Count: 1}},
			want:  []string{"FUZZ=0", "FUZZ=50"},
		},
		{
			name: "named fan always includes its name",
			specs: []NameSpec{{
				Name: "sweep", Tokens: []string{"FUZZ"},
				Rows: [][]string{{"0"}, {"50"}}, Count: 1,
			}},
			want: []string{"sweep:FUZZ=0", "sweep:FUZZ=50"},
		},
		{
			name:  "fan repeat numbers each value",
			specs: []NameSpec{{Tokens: []string{"F"}, Rows: [][]string{{"a"}}, Count: 2}},
			want:  []string{"F=a~1", "F=a~2"},
		},
		{
			// A multi-token fan joins each row's "Token=value" pairs with ",", the
			// rows in the odometer order Fan emits (last token varying fastest).
			name: "multi-token fan joins pairs",
			specs: []NameSpec{{
				Tokens: []string{"A", "B"},
				Rows:   [][]string{{"1", "x"}, {"1", "y"}, {"2", "x"}, {"2", "y"}},
				Count:  1,
			}},
			want: []string{"A=1,B=x", "A=1,B=y", "A=2,B=x", "A=2,B=y"},
		},
		{
			// A fan interleaves with a plain unnamed block: fan labels are taken, so
			// the letter steps past nothing it needs and the plain block gets A.
			name: "fan alongside a plain block",
			specs: []NameSpec{
				{Tokens: []string{"N"}, Rows: [][]string{{"1"}, {"2"}}, Count: 1},
				{Count: 1},
			},
			want: []string{"N=1", "N=2", "A"},
		},
		{
			// Two named fans on the same token clash on every value, so each is
			// qualified by its name.
			name: "clashing named fans qualify by name",
			specs: []NameSpec{
				{Name: "alpha", Tokens: []string{"UID"}, Rows: [][]string{{"1"}, {"2"}}, Count: 1},
				{Name: "beta", Tokens: []string{"UID"}, Rows: [][]string{{"1"}, {"2"}}, Count: 1},
			},
			want: []string{"alpha:UID=1", "alpha:UID=2", "beta:UID=1", "beta:UID=2"},
		},
		{
			name: "duplicate fan values get multiplicity suffixes",
			specs: []NameSpec{{
				Tokens: []string{"UID"}, Rows: [][]string{{"1"}, {"9"}, {"1"}},
				Count: 1,
			}},
			want: []string{"UID=1~1", "UID=9", "UID=1~2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			labels, err := ResolveNames(tc.specs)
			if err != nil {
				t.Fatalf("ResolveNames(%v) unexpected error: %v", tc.specs, err)
			}
			if got := displaysOf(labels); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ResolveNames(%v) = %v, want %v", tc.specs, got, tc.want)
			}
			seen := map[string]bool{}
			for _, l := range labels {
				if seen[l.Display] {
					t.Errorf("ResolveNames(%v) produced duplicate label %q", tc.specs, l.Display)
				}
				seen[l.Display] = true
			}
		})
	}
}

// TestResolveNamesKeepsCompleteFanLabels covers the canonical-label invariant:
// width does not change identity and every fan value remains visible.
func TestResolveNamesKeepsCompleteFanLabels(t *testing.T) {
	long := strings.Repeat("a", 20)
	labels, err := ResolveNames([]NameSpec{{
		Name: "set", Tokens: []string{"ID"},
		Rows: [][]string{{"short"}, {long}}, Count: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"set:ID=short", "set:ID=" + long}
	if got := displaysOf(labels); !reflect.DeepEqual(got, want) {
		t.Errorf("displays = %v, want %v", got, want)
	}

	multi, err := ResolveNames([]NameSpec{{
		Tokens: []string{"USER", "PASS"},
		Rows:   [][]string{{"alice", "pw"}, {"bob", "secret"}},
		Count:  1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"USER=alice,PASS=pw", "USER=bob,PASS=secret"}
	if got := displaysOf(multi); !reflect.DeepEqual(got, want) {
		t.Errorf("multi displays = %v, want %v", got, want)
	}
}

// TestResolveNamesPins covers the group keys a caller uses for a whole-group
// pin: a fan's token and name, and a named plain group's name.
func TestResolveNamesPins(t *testing.T) {
	labels, err := ResolveNames([]NameSpec{
		{Name: "sweep", Tokens: []string{"FUZZ"}, Rows: [][]string{{"0"}}, Count: 1},
		{Name: "solo", Count: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"FUZZ", "sweep"}; !reflect.DeepEqual(labels[0].Pins, want) {
		t.Errorf("fan pins = %v, want %v", labels[0].Pins, want)
	}
	if want := []string{"solo"}; !reflect.DeepEqual(labels[1].Pins, want) {
		t.Errorf("plain-named pins = %v, want %v", labels[1].Pins, want)
	}

	// A multi-token fan pins on each token, in order, then its name.
	multi, err := ResolveNames([]NameSpec{
		{Name: "grid", Tokens: []string{"A", "B"}, Rows: [][]string{{"1", "x"}}, Count: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A", "B", "grid"}; !reflect.DeepEqual(multi[0].Pins, want) {
		t.Errorf("multi-token pins = %v, want %v", multi[0].Pins, want)
	}
}

// TestResolveNamesCollision covers the hard error: an explicit name colliding
// with a suffix another name generates names both, not a silent rename.
func TestResolveNamesCollision(t *testing.T) {
	for _, specs := range [][]NameSpec{
		{{Name: "request", Count: 10}, {Name: "request~07", Count: 1}},
		{{Name: "request~07", Count: 1}, {Name: "request", Count: 10}}, // order-independent
	} {
		_, err := ResolveNames(specs)
		if err == nil {
			t.Fatalf("ResolveNames(%v) = nil error, want a collision error", specs)
		}
		for _, want := range []string{"request~07", `"request"`, `"request~07"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ResolveNames(%v) error = %q, want it to mention %s", specs, err, want)
			}
		}
	}

	t.Run("same rendered fan label from different sources", func(t *testing.T) {
		_, err := ResolveNames([]NameSpec{
			{Tokens: []string{"T"}, Rows: [][]string{{"x"}}, Count: 1, Source: 0},
			{Tokens: []string{"T"}, Rows: [][]string{{"x"}}, Count: 1, Source: 1},
		})
		if err == nil || !strings.Contains(err.Error(), "source blocks 1 and 2") {
			t.Errorf("error = %v, want collision naming both source blocks", err)
		}
	})

	t.Run("different raw values with one display spelling", func(t *testing.T) {
		_, err := ResolveNames([]NameSpec{{
			Tokens: []string{"T"},
			Rows:   [][]string{{"\x00"}, {"$HEX[00]"}},
			Count:  1,
		}})
		if err == nil || !strings.Contains(err.Error(), "distinct inputs") {
			t.Errorf("error = %v, want distinct-input collision", err)
		}
	})
}

// TestBlockNameSpecs checks Block.NameSpecs builds label groups at the
// granularity a consumer expands requests: one spec per URL for a plain block,
// each carrying the block's Name and Repeat; and one spec over a fan's
// variants, carrying the fan's tokens and per-variant rows.
func TestBlockNameSpecs(t *testing.T) {
	plain := Block{Name: "p", URLs: []string{"https://a", "https://b"}, Repeat: 3}
	if got, want := plain.NameSpecs(nil, 0), []NameSpec{
		{Name: "p", Count: 3}, {Name: "p", Count: 3},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("plain NameSpecs = %+v, want %+v", got, want)
	}

	fan := &Fan{
		Tokens:   []string{"X"},
		Variants: []Variant{{Values: []string{"1"}}, {Values: []string{"2"}}},
	}
	blk := Block{Name: "f", URLs: []string{"https://h/X"}, Repeat: 2}
	if got, want := blk.NameSpecs(fan, 0), []NameSpec{
		{Name: "f", Count: 2, Tokens: []string{"X"}, Rows: [][]string{{"1"}, {"2"}}},
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("fan NameSpecs = %+v, want %+v", got, want)
	}

	// A varying block has one request per variant, not per URL, so labelling it
	// without its fan would misname every request after the first.
	t.Run("a varying block without its fan", func(t *testing.T) {
		varying := Block{URLs: []string{"https://h/X"}, Repeat: 1, Vary: []VarySpec{mustSpec(t, "X=1-3")}}
		requirePanicContains(t, "NameSpecs(nil) on a block with --vary",
			func() { varying.NameSpecs(nil, 0) })
	})
}

func TestAutoLabel(t *testing.T) {
	tests := []struct {
		i    int
		want string
	}{
		{0, "A"}, {25, "Z"}, {26, "AA"}, {27, "AB"}, {51, "AZ"}, {52, "BA"}, {701, "ZZ"}, {702, "AAA"},
	}
	for _, tc := range tests {
		if got := autoLabel(tc.i); got != tc.want {
			t.Errorf("autoLabel(%d) = %q, want %q", tc.i, got, tc.want)
		}
	}
}

// TestResolveNamesShortRow checks the guard on a hand-built fan spec: a row
// carrying fewer values than the spec has tokens would label requests with the
// wrong tokens, so ResolveNames panics naming the offending row rather than
// dying on an index deep in the labelling.
func TestResolveNamesShortRow(t *testing.T) {
	requirePanicContains(t, "row 1 has 1 values for 2 tokens", func() {
		_, _ = ResolveNames([]NameSpec{{
			Tokens: []string{"A", "B"},
			Rows:   [][]string{{"1", "x"}, {"2"}},
			Count:  1,
		}})
	})
}
