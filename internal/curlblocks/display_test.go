package curlblocks

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// asciiPrintable reports whether s is entirely printable ASCII (0x20..0x7e),
// the guarantee every display-facing string carries.
func asciiPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// TestDisplayTextIsASCII checks every consumer-visible identity path. Control,
// non-ASCII, and edge-whitespace bytes must use $HEX[...] rendering.
func TestDisplayTextIsASCII(t *testing.T) {
	files := fakeFS(map[string]string{"caf\xc3\xa9.txt": "x\ny\n"})
	esc := "a\x1b[31mRED"

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "a non-ASCII token name",
			args: []string{"https://h/CAF\xc3\x89", "--vary", "CAF\xc3\x89={1,2}"},
		},
		{
			name: "a non-ASCII --name",
			args: []string{"https://h/x", "--name", "caf\xc3\xa9"},
		},
		{
			name: "non-ASCII list values",
			args: []string{"https://h/T", "--vary", "T={caf\xc3\xa9,bar}"},
		},
		{
			name: "a non-ASCII file name",
			args: []string{"https://h/T", "--vary", "T=@caf\xc3\xa9.txt"},
		},
		{
			name: "a control byte in a value",
			args: []string{"https://h/T", "--vary", "T={" + esc + ",bar}"},
		},
		{
			name: "a value that is only whitespace",
			args: []string{"https://h/T", "--vary", "T={ ,bar}"},
		},
		{
			name: "a non-ASCII name on a fanned block",
			args: []string{"https://h/T", "--vary", "T={1,2}", "--name", "caf\xc3\xa9"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := New(WithFileReader(files)).Parse(tt.args)
			if err != nil {
				t.Fatalf("Parse(%+q): %v", tt.args, err)
			}
			groups, err := plan.Expand()
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			for _, g := range groups {
				for _, l := range g.Labels {
					check(t, "label", l.Display)
					for _, pin := range l.Pins {
						check(t, "pin", pin)
					}
				}
				if g.Fan != nil {
					check(t, "fan source", g.Fan.Source)
					for _, tok := range g.Fan.Tokens {
						check(t, "fan token", tok)
					}
				}
			}
		})
	}
}

func check(t *testing.T, what, s string) {
	t.Helper()
	if !asciiPrintable(s) {
		t.Errorf("%s %+q is not printable ASCII", what, s)
	}
}

// TestSourceDescriptionElidesWholeCharacters checks that eliding a long list
// description cannot leave a partial character behind. Escaping happens before
// the cut, so what is cut is hex digits; a byte-wise cut of the raw text would
// split the multi-byte rune this case places exactly on the boundary.
func TestSourceDescriptionElidesWholeCharacters(t *testing.T) {
	list := strings.Repeat("a", varySourceDescMax-1) + "\xc3\xa9"
	plan, err := New().Parse([]string{"https://h/T", "--vary", "T={" + list + ",bar}"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	fan, err := plan.Blocks[0].Fan()
	if err != nil {
		t.Fatalf("Fan: %v", err)
	}
	if !asciiPrintable(fan.Source) {
		t.Errorf("Fan.Source = %+q, want printable ASCII", fan.Source)
	}
	if !utf8.ValidString(fan.Source) {
		t.Errorf("Fan.Source = %+q is not valid UTF-8", fan.Source)
	}
	if !strings.HasSuffix(fan.Source, "...}") {
		t.Errorf("Fan.Source = %+q, want a long description elided with '...'", fan.Source)
	}
}

// TestDisplayTextMatchesLabels is the contract DisplayText is exported for: a
// consumer renders what a user typed and compares it with a label the package
// produced. Whichever of the two spellings the user reaches for -- the name as
// typed, or the $HEX[...] form the output showed them -- must land on the
// label, so a consumer needs no second pass and no copy of the predicate.
func TestDisplayTextMatchesLabels(t *testing.T) {
	const name = "caf\xc3\xa9"
	plan, err := New().Parse([]string{"https://h/x", "--name", name})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	groups, err := plan.Expand()
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	label := groups[0].Labels[0].Display
	if label == name {
		t.Fatalf("label %+q was not rendered; invalid test setup", label)
	}
	for _, spelling := range []string{name, label} {
		if got := DisplayText(spelling); got != label {
			t.Errorf("DisplayText(%+q) = %q, want the label %q", spelling, got, label)
		}
	}
}
