package curlblocks

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

// TestName covers the built-in --name shared flag: it resolves like any shared
// flag — valid on a bare-preamble request, set per block, inherited from the
// preamble as a baseline, and overridden by a block that sets its own. It is
// empty when unset.
func TestName(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string // resolved Block.Name, in block order
	}{
		{"bare preamble names the single request", []string{"https://x", "--name", "solo"}, []string{"solo"}},
		{
			"set per block",
			[]string{"--block", "https://a", "--name", "one", "--block", "https://b", "--name", "two"},
			[]string{"one", "two"},
		},
		{
			"preamble baseline inherited; a block overrides",
			[]string{"--name", "base", "--block", "https://a", "--block", "https://b", "--name", "own"},
			[]string{"base", "own"},
		},
		{"unset is empty", []string{"https://x"}, []string{""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := New().Parse(tt.args)
			if err != nil {
				t.Fatalf("New().Parse(%q): %v", tt.args, err)
			}
			got := make([]string, len(plan.Blocks))
			for i, b := range plan.Blocks {
				got[i] = b.Name
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("names = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNameCannotBeReusedAcrossBlocks(t *testing.T) {
	_, err := New().Parse([]string{
		"--block", "https://h/a", "--name", "shared",
		"--block", "https://h/b", "--name", "shared",
	})
	if err == nil {
		t.Fatal("Parse returned nil error, want reused --name rejected")
	}
	for _, want := range []string{"--name", "shared", "blocks 1 and 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

// TestCustomFlags exercises the extension path: a tool registers a custom
// global flag, a block-only flag, and shared string and repeatable-string
// flags, then reads them back per scope.
func TestCustomFlags(t *testing.T) {
	p := New()
	verbose := p.Global.Bool("verbose", "v", false, "verbose logging")
	retries := p.Block.Int("retries", "", "N", 3, "retries per request")
	tag := p.Shared.String("tag", "", "TAG", "", "trace tag")
	notes := p.Shared.StringArray(
		"note", "", "TEXT", nil, "free-form note (repeatable)")

	plan, err := p.Parse([]string{
		"--verbose", "--tag", "base",
		"--block", "https://a", "--retries", "5", "--note", "one", "--note", "two",
		"--block", "https://b", "--tag", "override",
	})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	if !verbose.Get(plan.Globals) {
		t.Error("verbose.Get(Globals) = false, want true")
	}
	if !verbose.IsSet(plan.Globals) {
		t.Error("verbose.IsSet(Globals) = false, want explicit flag")
	}
	if got := retries.Get(plan.Blocks[0]); got != 5 {
		t.Errorf("retries[0] = %d, want 5", got)
	}
	if got := retries.Get(plan.Blocks[1]); got != 3 {
		t.Errorf("retries[1] = %d, want 3 (default)", got)
	}
	if !retries.IsSet(plan.Blocks[0]) || retries.IsSet(plan.Blocks[1]) {
		t.Error("retries IsSet does not distinguish explicit value from default")
	}
	if got := tag.Get(plan.Blocks[0]); got != "base" {
		t.Errorf("tag[0] = %q, want %q (inherited baseline)", got, "base")
	}
	if got := tag.Get(plan.Blocks[1]); got != "override" {
		t.Errorf("tag[1] = %q, want %q", got, "override")
	}
	if !tag.IsSet(plan.Blocks[0]) || !tag.IsSet(plan.Blocks[1]) {
		t.Error("tag IsSet does not preserve inherited or overridden setting")
	}
	// A repeatable flag accumulates its occurrences within the scope that set it;
	// a block that set none reads the registered default.
	if got, want := notes.Get(plan.Blocks[0]), []string{"one", "two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("note[0] = %v, want %v", got, want)
	}
	if got := notes.Get(plan.Blocks[1]); len(got) != 0 {
		t.Errorf("note[1] = %v, want none (default)", got)
	}
	if !notes.IsSet(plan.Blocks[0]) || notes.IsSet(plan.Blocks[1]) {
		t.Error("note IsSet does not distinguish explicit values from default")
	}
}

// TestInsecureFlagIsOptIn checks that --insecure/-k exists only for a Parser
// that asked for it: a tool speaking a transport with no certificates to verify
// neither offers the flag nor sees Globals.Insecure move.
func TestInsecureFlagIsOptIn(t *testing.T) {
	for _, arg := range []string{"--insecure", "-k"} {
		plan, err := New(WithInsecureFlag()).Parse([]string{arg, "https://h/"})
		if err != nil {
			t.Fatalf("Parse(%q) with the flag registered: %v", arg, err)
		}
		if !plan.Globals.Insecure {
			t.Errorf("Parse(%q): Globals.Insecure = false, want true", arg)
		}
		if _, err := New().Parse([]string{arg, "https://h/"}); err == nil ||
			!strings.Contains(err.Error(), "unknown") {
			t.Errorf("Parse(%q) without the flag registered = %v, want an unknown-flag error", arg, err)
		}
	}
	// Unset with the flag registered is false, so a consumer reads one field
	// either way.
	plan, err := New(WithInsecureFlag()).Parse([]string{"https://h/"})
	if err != nil || plan.Globals.Insecure {
		t.Errorf("unset --insecure: Insecure = %v (err %v), want false", plan.Globals.Insecure, err)
	}
	// The help lists it only when registered.
	if usage := New(WithInsecureFlag()).Usage(); !strings.Contains(usage, "--insecure") {
		t.Error("usage omits --insecure although the flag is registered")
	}
	if usage := New().Usage(); strings.Contains(usage, "--insecure") {
		t.Error("usage lists --insecure although the flag is not registered")
	}
}

// TestFlagGetWrongCarrier checks that reading a flag from the carrier its scope
// never fills panics instead of returning the registered default -- a default
// there is indistinguishable from the user's own choice, so there is no
// truthful answer to give. Both directions, and a sectioned flag, which is
// scoped like an unsectioned one.
func TestFlagGetWrongCarrier(t *testing.T) {
	p := New()
	global := p.Global.Bool("verbose", "v", false, "verbose logging")
	block := p.Block.Int("retries", "", "N", 3, "retries per request")
	shared := p.Shared.String("tag", "", "TAG", "", "trace tag")
	sectioned := p.Global.Section("Mode options").
		Bool("deep", "", false, "deep mode")

	plan, err := p.Parse([]string{"https://a"})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}

	tests := []struct {
		name string
		read func()
		want string
	}{
		{"a global flag read from a Block", func() { global.Get(plan.Blocks[0]) }, "--verbose is a global option"},
		{"a global flag queried on a Block", func() { global.IsSet(plan.Blocks[0]) }, "--verbose is a global option"},
		{"a block flag read from Globals", func() { block.Get(plan.Globals) }, "--retries is a block option"},
		{"a shared flag read from Globals", func() { shared.Get(plan.Globals) }, "--tag is a shared option"},
		{"a sectioned global flag read from a Block", func() { sectioned.Get(plan.Blocks[0]) }, "--deep is a global option"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirePanicContains(t, tt.want, tt.read)
		})
	}

	// The right carrier still reads normally, panic-free.
	if global.Get(plan.Globals) || block.Get(plan.Blocks[0]) != 3 || shared.Get(plan.Blocks[0]) != "" {
		t.Error("a same-scope read did not return the registered defaults")
	}
}

// TestCustomFlagScopeRejected checks that a custom block-only flag used in the
// preamble is rejected by name, just like a built-in.
func TestCustomFlagScopeRejected(t *testing.T) {
	p := New()
	p.Block.Int("retries", "", "N", 3, "retries per request")

	_, err := p.Parse([]string{"--retries", "5", "--block", "https://a"})
	if err == nil || !strings.Contains(err.Error(), "--retries is not valid in the preamble") {
		t.Fatalf("Parse error = %v, want one mentioning --retries is not valid in the preamble", err)
	}
}

// TestSection checks that a flag registered via Section is parsed and scoped
// exactly like one registered on the Registry directly (sectioning is
// presentation only), and that Usage lists it in its own section.
func TestSection(t *testing.T) {
	p := New()
	p.Name = "demo"
	verbose := p.Global.Bool("verbose", "v", false, "verbose logging")
	section := p.Global.Section("Detect options")
	conf := section.String(
		"confidence", "", "P", "0.95", "commit confidence")

	plan, err := p.Parse([]string{"--verbose", "--confidence", "0.99", "--block", "https://a"})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if !verbose.Get(plan.Globals) {
		t.Error("direct flag not parsed")
	}
	if got := conf.Get(plan.Globals); got != "0.99" {
		t.Errorf("sectioned flag confidence = %q, want %q (sectioning must not change parsing)", got, "0.99")
	}

	usage := p.Usage()
	if !strings.Contains(usage, "\nDetect options:\n") {
		t.Errorf("Usage missing section heading:\n%s", usage)
	}
	if !strings.Contains(usage, "  --confidence") {
		t.Errorf("Usage missing sectioned flag:\n%s", usage)
	}
	if !strings.Contains(usage, "  -v, --verbose") { // default section: shallow indent
		t.Errorf("Usage missing default-section flag:\n%s", usage)
	}
}

// TestSectionOrder checks where a section sits in the listing: unsectioned
// flags lead, sections and their flags retain registration order, and the
// framework's own flags occupy Common options after the request grammar.
func TestSectionOrder(t *testing.T) {
	p := New(WithInsecureFlag(), WithVersionFlag("1.0"))
	p.Name = "demo"
	p.Global.Bool("verbose", "v", false, "verbose logging")
	p.Global.Section("Mode options").String(
		"confidence", "", "P", "0.95", "commit confidence")

	usage := p.Usage()
	want := []string{"-v, --verbose", "Mode options:", "--confidence", "-k, --insecure", "-V, --version", "-h, --help"}
	at := 0
	for _, w := range want {
		i := strings.Index(usage[at:], w)
		if i < 0 {
			t.Fatalf("usage is missing %q or has it out of order (want %v):\n%s", w, want, usage)
		}
		at += i + len(w)
	}
	if !strings.Contains(usage, "\nCommon options:\n  -k, --insecure") {
		t.Errorf("built-ins are not under Common options:\n%s", usage)
	}
}

// TestSectionFoldsByTitle checks that asking for the same title twice returns a
// handle onto one section, so a tool may reach for a section from several
// registration helpers and still get a single heading.
func TestSectionFoldsByTitle(t *testing.T) {
	p := New()
	p.Name = "demo"
	p.Global.Section("Mode options").Bool("deep", "", false, "deep mode")
	p.Global.Section("Mode options").Bool("wide", "", false, "wide mode")

	usage := p.Usage()
	if n := strings.Count(usage, "Mode options:"); n != 1 {
		t.Errorf("section heading appears %d times, want 1:\n%s", n, usage)
	}
	deep, wide := strings.Index(usage, "--deep"), strings.Index(usage, "--wide")
	if deep < 0 || wide < deep {
		t.Errorf("both flags should be listed under the one heading, in registration order:\n%s", usage)
	}
}

// TestSectionCreationOrder checks that section order is fixed when the
// sections are created, independently of when helpers later register flags
// into their handles. Registration order remains the tie-breaker within a
// section.
func TestSectionCreationOrder(t *testing.T) {
	p := New()
	p.Name = "demo"
	first := p.Global.Section("First options")
	second := p.Global.Section("Second options")
	second.Bool("second", "", false, "second flag")
	first.Bool("first-a", "", false, "first flag")
	first.Bool("first-b", "", false, "another first flag")

	usage := p.Usage()
	want := []string{
		"First options:", "--first-a", "--first-b",
		"Second options:", "--second",
	}
	at := 0
	for _, text := range want {
		i := strings.Index(usage[at:], text)
		if i < 0 {
			t.Fatalf("usage is missing %q or has it out of order:\n%s",
				text, usage)
		}
		at += i + len(text)
	}
}

// TestFlagCollisionPanics checks that a custom flag redefining a built-in's
// name or shorthand is refused at the registration that wrote it, naming both
// flags -- left to the flag set it surfaces mid-Parse as a panic naming an
// internal label instead. The cases cover a shorthand within one scope and a
// long name across the two scopes a preamble parses together.
func TestFlagCollisionPanics(t *testing.T) {
	tests := []struct {
		name     string
		register func(*Parser)
		want     string
	}{
		{
			"shorthand of a built-in in the same scope",
			func(p *Parser) { p.Global.Bool("kool", "k", false, "x") },
			"--kool/-k collides with the global option --insecure/-k",
		},
		{
			"long name of a family built-in in a sibling scope",
			func(p *Parser) {
				p.Block.String("user-agent", "", "AGENT", "", "x")
			},
			"--user-agent collides with the shared option --user-agent/-A",
		},
		{
			"collapsed alias of a family built-in",
			func(p *Parser) {
				p.Block.String("data-ascii", "", "DATA", "", "x")
			},
			"--data-ascii collides with the shared option --data-ascii",
		},
		{
			"a name the tool itself already registered",
			func(p *Parser) {
				p.Global.Int("width", "", "N", 8, "x")
				p.Global.Int("width", "", "N", 8, "x")
			},
			"--width collides with the global option --width",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirePanicContains(t, tt.want, func() {
				tt.register(New(WithInsecureFlag()))
			})
		})
	}

	// The same name in scopes that are never parsed together is not a collision:
	// a preamble reads global+shared, a block shared+block.
	p := New()
	p.Global.Int("depth", "", "N", 1, "x")
	p.Block.Int("depth", "", "N", 1, "x")
}

// TestFloat64Flag checks the float registrar parses a value, defaults when
// unset, and leaves a bound to the tool: pflag refuses a non-numeric argument,
// while NaN parses and reaches the consumer, which is why a range check cannot
// be a bare interval test.
func TestFloat64Flag(t *testing.T) {
	p := New()
	edge := p.Global.Float64(
		"edge", "", "P", 0.02, "assumed advantage")

	plan, err := p.Parse([]string{"--edge", "0.125", "https://h/"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := edge.Get(plan.Globals); got != 0.125 {
		t.Errorf("--edge = %v, want 0.125", got)
	}

	plan, err = p.Parse([]string{"https://h/"})
	if err != nil {
		t.Fatalf("Parse without the flag: %v", err)
	}
	if got := edge.Get(plan.Globals); got != 0.02 {
		t.Errorf("unset --edge = %v, want the registered default 0.02", got)
	}

	p = New()
	p.Global.Float64("edge", "", "P", 0.02, "assumed advantage")
	if _, err := p.Parse([]string{"--edge", "abc", "https://h/"}); err == nil ||
		!strings.Contains(err.Error(), "--edge") {
		t.Errorf("Parse with a non-numeric --edge = %v, want an error naming the flag", err)
	}

	p = New()
	nan := p.Global.Float64(
		"edge", "", "P", 0.02, "assumed advantage")
	plan, err = p.Parse([]string{"--edge", "NaN", "https://h/"})
	if err != nil {
		t.Fatalf("Parse with --edge NaN: %v", err)
	}
	if got := nan.Get(plan.Globals); !math.IsNaN(got) {
		t.Errorf("--edge NaN = %v, want NaN reaching the consumer to reject", got)
	}
}

// TestValueFlagHelpArgumentRequired checks the common registrar invariant:
// every flag that consumes a value must say what that value is in help.
func TestValueFlagHelpArgumentRequired(t *testing.T) {
	tests := []struct {
		name     string
		register func(*Registry)
	}{
		{"string", func(r *Registry) { r.String("value", "", "", "", "x") }},
		{"int", func(r *Registry) { r.Int("value", "", "", 0, "x") }},
		{"float", func(r *Registry) { r.Float64("value", "", "", 0, "x") }},
		{
			"string array",
			func(r *Registry) { r.StringArray("value", "", "", nil, "x") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requirePanicContains(t, "--value needs a help argument name",
				func() { tt.register(New().Global) })
		})
	}
}

func TestHelpArgumentMustBeOnePrintableWord(t *testing.T) {
	for _, argument := range []string{"TWO WORDS", "N\n"} {
		t.Run(argument, func(t *testing.T) {
			requirePanicContains(t, "invalid help argument name", func() {
				New().Global.Int("value", "", argument, 0, "x")
			})
		})
	}
}

func TestHelpDefaultMustBePrintableASCII(t *testing.T) {
	f := New().Global.Int("value", "", "N", 0, "x")
	requirePanicContains(t, "invalid help default", func() {
		f.HelpDefault("line\nbreak")
	})
}
