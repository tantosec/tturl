package curlblocks

import (
	"strings"
	"testing"
)

// TestUsageKeepsEachOptionOnOneLine checks that the formatter preserves the
// authored row. Terminals may soft-wrap it, but redirected output contains no
// formatter-authored continuation.
func TestUsageKeepsEachOptionOnOneLine(t *testing.T) {
	const longUsage = "a description long enough to need several lines when it is " +
		"folded into the column that the flag names leave free, so that the hanging " +
		"indent and the width would both be exercised"
	p := New()
	p.Global.Bool("longwinded", "", false, longUsage)
	usage := p.Usage()

	line := usageLineFor(t, usage, "  --longwinded")
	if !strings.Contains(line, longUsage) {
		t.Errorf("option row = %q, want the complete description", line)
	}
}

func TestUsageLeavesFramingOnOneLineWithoutWidth(t *testing.T) {
	description := strings.TrimSpace(strings.Repeat("long description ", 8))
	p := New()
	p.Name = "demo run"
	p.Description = description
	usage := p.Usage()

	for _, want := range []string{
		p.Name + " - " + description + "\n",
		"  " + p.Name + " [global options] [request options] URL " +
			"[--block [URL...] [block options]]...\n",
		usageScopeNote + "\n",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("Usage does not contain the authored line %q:\n%s",
				strings.TrimSuffix(want, "\n"), usage)
		}
	}
}

func TestUsageAcceptsSynopsisAlternativesAndScopeNote(t *testing.T) {
	p := New()
	p.Name = "demo run"
	p.Synopses = []string{
		"[preamble flags] URL [--block ...]",
		"[preamble flags] --block [URL...] [block flags] ...",
	}
	p.ScopeNote = "CUSTOM SCOPE NOTE"

	usage := p.Usage()
	want := []string{
		"Usage:\n",
		"  demo run [preamble flags] URL [--block ...]\n",
		"  demo run [preamble flags] --block [URL...] " +
			"[block flags] ...\n",
		"CUSTOM SCOPE NOTE\n",
		"Request options:\n",
	}
	at := 0
	for _, text := range want {
		i := strings.Index(usage[at:], text)
		if i < 0 {
			t.Fatalf("usage is missing %q or has it out of order:\n%s",
				strings.TrimSuffix(text, "\n"), usage)
		}
		at += i + len(text)
	}
	if strings.Contains(usage, usageScopeNote) {
		t.Errorf("usage retained the default scope note:\n%s", usage)
	}
}

func TestUsageWrapsOptionDescriptionsAtFixedWidth(t *testing.T) {
	const width = 60
	p := New()
	p.Name = "demo"
	p.Synopses = []string{"[flags] URL"}
	p.ScopeNote = "Placement is documented elsewhere."
	p.UsageWidth = width
	p.Global.Bool("longwinded", "", false,
		"describe one deliberately long option without losing its words or "+
			"its attachment to the flag")

	usage := p.Usage()
	lines := strings.Split(usage, "\n")
	for i, line := range lines {
		if len(line) > width {
			t.Errorf("line %d width = %d, want <= %d: %q",
				i+1, len(line), width, line)
		}
	}

	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "  --longwinded") {
			start = i
			break
		}
	}
	if start < 0 || start+1 >= len(lines) {
		t.Fatalf("usage has no wrapped --longwinded row:\n%s", usage)
	}
	descriptionCol := helpNameColMax + 1
	if !strings.HasPrefix(lines[start+1], strings.Repeat(" ", descriptionCol)) {
		t.Errorf("continuation = %q, want %d-space indent",
			lines[start+1], descriptionCol)
	}
	row := []string{strings.TrimSpace(lines[start])}
	continuation := strings.Repeat(" ", descriptionCol)
	for _, line := range lines[start+1:] {
		if !strings.HasPrefix(line, continuation) ||
			strings.TrimSpace(line) == "" {
			break
		}
		row = append(row, strings.TrimSpace(line))
	}
	joined := strings.Join(row, " ")
	for _, want := range []string{"deliberately long option", "attachment to the flag"} {
		if !strings.Contains(joined, want) {
			t.Errorf("wrapped row lost %q: %q", want, joined)
		}
	}
}

func TestUsageMovesAnOverflowingFieldAboveItsDescription(t *testing.T) {
	p := New()
	p.Name = "demo"
	p.Synopses = []string{"[flags] URL"}
	p.ScopeNote = "Placement is documented elsewhere."
	p.UsageWidth = 60
	name := strings.Repeat("x", helpNameColMax)
	p.Global.Bool(name, "", false, "description follows on its own line")

	lines := strings.Split(p.Usage(), "\n")
	prefix := "  --" + name
	for i, line := range lines {
		if line != prefix {
			continue
		}
		if i+1 >= len(lines) ||
			!strings.HasPrefix(lines[i+1], strings.Repeat(" ", helpNameColMax+1)+"description") {
			t.Fatalf("overflowing row is not split and aligned:\n%s", p.Usage())
		}
		return
	}
	t.Fatalf("usage has no %q row:\n%s", prefix, p.Usage())
}

func TestUsageKeepsShortDefaultClauseTogether(t *testing.T) {
	p := New()
	p.Name = "demo"
	p.Synopses = []string{"[flags] URL"}
	p.ScopeNote = "Placement is documented elsewhere."
	p.UsageWidth = 60
	p.Global.String("format", "", "FORMAT", "text",
		"select a deliberately wordy rendering style")

	usage := p.Usage()
	if strings.Contains(usage, "(default:\n") ||
		strings.Contains(usage, "(default: text\n") {
		t.Errorf("default clause was split:\n%s", usage)
	}
	if !strings.Contains(usage, "\n"+
		strings.Repeat(" ", helpNameColMax+1)+"(default: text)\n") {
		t.Errorf("default clause did not move intact to a continuation:\n%s",
			usage)
	}
}

// TestUsageFlatRegistrationNeedsNoSections checks the zero-setup presentation
// path: flags registered directly on a scope remain complete and ordered under
// its ordinary heading.
func TestUsageFlatRegistrationNeedsNoSections(t *testing.T) {
	p := New()
	p.Name = "demo"
	p.Global.Bool("alpha", "", false, "first flag")
	p.Global.Section("").Bool("beta", "", false, "second flag")

	usage := p.Usage()
	global := strings.Index(usage, "Global options:")
	alpha := strings.Index(usage, "--alpha")
	beta := strings.Index(usage, "--beta")
	request := strings.Index(usage, "Request options:")
	if global < 0 || alpha < global || beta < alpha || request < beta {
		t.Errorf("flat global flags are missing or out of order:\n%s", usage)
	}
}

// TestUsageSectionsRemainWithinScope checks that request and block sections
// subdivide their owning help scopes without changing parsing or carriers.
func TestUsageSectionsRemainWithinScope(t *testing.T) {
	p := New()
	p.Name = "demo"
	p.Shared.Bool("request-flat", "", false, "flat request flag")
	tag := p.Shared.Section("Request detail").
		String("tag", "", "TAG", "", "request tag")
	p.Block.Bool("block-flat", "", false, "flat block flag")
	retries := p.Block.Section("Block detail").
		Int("retries", "", "N", 1, "retry count")

	plan, err := p.Parse([]string{
		"--tag", "baseline",
		"--block", "https://a.example", "--retries", "2",
		"--block", "https://b.example", "--tag", "local", "--retries", "3",
	})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if got := tag.Get(plan.Blocks[0]); got != "baseline" {
		t.Errorf("inherited tag = %q, want baseline", got)
	}
	if got := tag.Get(plan.Blocks[1]); got != "local" {
		t.Errorf("overridden tag = %q, want local", got)
	}
	if got := retries.Get(plan.Blocks[0]); got != 2 {
		t.Errorf("first retries = %d, want 2", got)
	}
	if got := retries.Get(plan.Blocks[1]); got != 3 {
		t.Errorf("second retries = %d, want 3", got)
	}

	usage := p.Usage()
	want := []string{
		"Request options:", "--request-flat", "Request detail", "--tag",
		"Block options:", "--block-flat", "Block detail", "--retries",
		"Common options:",
	}
	at := 0
	for _, text := range want {
		i := strings.Index(usage[at:], text)
		if i < 0 {
			t.Fatalf("usage is missing %q or has it out of scope:\n%s",
				text, usage)
		}
		at += i + len(text)
	}
}

func TestUsageGroupsBuiltInRequestFlagsByPurpose(t *testing.T) {
	usage := New().Usage()
	want := []string{
		"Request options:",
		"Headers and method:", "--header", "--request",
		"Bodies:", "--data", "--form", "--json",
		"Requests and expansion:", "--url", "--vary", "--name",
		"Block options:", "--reset-body",
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

// TestUsageTrailingGlobalSection checks that a global section can trail the
// request-building scopes without changing where its flags parse.
func TestUsageTrailingGlobalSection(t *testing.T) {
	p := New()
	p.Name = "demo"
	p.Global.Section("Mode options").
		Bool("mode", "", false, "mode flag")
	output := p.Global.TrailingSection("Output options").
		String("output", "", "FILE", "-", "output file")

	plan, err := p.Parse([]string{
		"--mode", "--output", "results.txt", "https://example.com",
	})
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if got := output.Get(plan.Globals); got != "results.txt" {
		t.Errorf("output = %q, want results.txt", got)
	}

	usage := p.Usage()
	want := []string{
		"Mode options:", "--mode",
		"Request options:",
		"Block options:",
		"Output options:", "--output",
		"Common options:",
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

// TestUsageNameColumnGrowsForLongFlag checks the name column: it widens to fit
// a name longer than helpNameColMin so descriptions still align, and a name
// past helpNameColMax overflows its own line instead of pushing every
// description across the page.
func TestUsageNameColumnGrowsForLongFlag(t *testing.T) {
	// A built-in alias row reaches the maximum name column, so this shorter
	// custom field shares the established description column.
	grown := strings.Repeat("x", helpNameColMax-1-len("  --"))
	p := New()
	p.Global.Bool(grown, "", false, "some text")
	line := usageLineFor(t, p.Usage(), "  --"+grown)
	if col := strings.Index(line, "some text"); col != helpNameColMax+1 {
		t.Errorf("description starts at column %d, want %d",
			col, helpNameColMax+1)
	}

	over := strings.Repeat("x", helpNameColMax+10)
	p = New()
	p.Global.Bool(over, "", false, "some text")
	line = usageLineFor(t, p.Usage(), "  --"+over)
	if col := strings.Index(line, "some text"); col != len("  --"+over)+1 {
		t.Errorf("description starts at column %d, want %d (one past the overflowing name)",
			col, len("  --"+over)+1)
	}
}

// usageLineFor returns the single rendered help line starting with prefix.
func usageLineFor(t *testing.T, usage, prefix string) string {
	t.Helper()
	for line := range strings.SplitSeq(usage, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("Usage has no line starting %q:\n%s", prefix, usage)
	return ""
}

// TestUsageFramesStructuredGuidance checks that Usage owns trailing headings,
// indentation, alignment, and whitespace. The consumer supplies only command
// and route content.
func TestUsageFramesStructuredGuidance(t *testing.T) {
	p := New()
	p.Name = "demo run"
	p.Description = "DESCRIPTION LINE"
	p.Guide = "demo help run"
	p.Examples = []UsageExample{{Lines: []string{
		"demo run --count 2 \\",
		"https://example.com/a https://example.com/b",
	}}}
	p.DetailedHelp = []HelpRoute{
		{Invocation: "demo help requests", Summary: "Request construction"},
		{Invocation: "demo help output", Summary: "Output formats"},
	}
	usage := p.Usage()

	for _, want := range []string{
		"demo run", "DESCRIPTION LINE", "Example:", "Detailed help:",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage omits %q:\n%s", want, usage)
		}
	}
	// Order: description, guide, flags, example, then routes.
	iName := strings.Index(usage, "demo run")
	iDesc := strings.Index(usage, "DESCRIPTION LINE")
	iGuide := strings.Index(usage, "Guide:")
	iFlag := strings.Index(usage, "--url")
	iExample := strings.Index(usage, "Example:")
	iRoutes := strings.Index(usage, "Detailed help:")
	if iName >= iDesc || iDesc >= iGuide || iGuide >= iFlag || iFlag >= iExample ||
		iExample >= iRoutes {
		t.Errorf("frame out of order (name %d, description %d, guide %d, flags %d, example %d, routes %d):\n%s",
			iName, iDesc, iGuide, iFlag, iExample, iRoutes, usage)
	}
	if !strings.Contains(usage, "Usage:\n  demo run "+
		"[global options] [request options] URL "+
		"[--block [URL...] [block options]]...\n\n"+
		"Guide:\n  demo help run\n\n") {
		t.Errorf("guide does not immediately follow Usage with matching indentation:\n%s", usage)
	}
	wantSuffix := "Example:\n" +
		"  demo run --count 2 \\\n" +
		"    https://example.com/a https://example.com/b\n\n" +
		"Detailed help:\n" +
		"  demo help requests   Request construction\n" +
		"  demo help output     Output formats\n"
	if !strings.HasSuffix(usage, wantSuffix) {
		t.Errorf("usage does not render structured guidance:\n%s", usage)
	}

	// Unset: no stray blank framing where the fields would have been.
	bare := New()
	bare.Name = "demo"
	if got := bare.Usage(); strings.Contains(got, "\n\n\n") {
		t.Errorf("usage with no optional framing has a blank run:\n%q", got)
	}
}

func TestUsageSeparatesMultipleExamples(t *testing.T) {
	p := New()
	p.Name = "demo"
	p.Examples = []UsageExample{
		{Lines: []string{"demo first"}},
		{Lines: []string{"demo second --flag \\", "value"}},
	}

	want := "Examples:\n" +
		"  demo first\n\n" +
		"  demo second --flag \\\n" +
		"    value\n\n"
	if usage := p.Usage(); !strings.Contains(usage, want) {
		t.Errorf("usage did not separate multiple examples:\n%s", usage)
	}
}

func TestUsageWrapsFramingAndHelpRoutesAtFixedWidth(t *testing.T) {
	p := New()
	p.Name = "demo"
	p.Description = "describe a command with enough words to wrap"
	p.Synopses = []string{"[flags] URL"}
	p.ScopeNote = "Explain placement in ordinary prose without authored line breaks."
	p.UsageWidth = 48
	p.DetailedHelp = []HelpRoute{{
		Invocation: "demo help requests",
		Summary:    "Explain request construction with enough words to wrap",
	}}

	usage := p.Usage()
	for i, line := range strings.Split(usage, "\n") {
		if len(line) > p.UsageWidth {
			t.Errorf("line %d width = %d, want <= %d: %q",
				i+1, len(line), p.UsageWidth, line)
		}
	}
	if !strings.Contains(usage, "demo - describe a command with enough words to\n"+
		"       wrap\n") {
		t.Errorf("description did not wrap under its renderer-owned prefix:\n%s", usage)
	}
	if !strings.Contains(usage, "  demo help requests   Explain request\n"+
		"                       construction with enough\n"+
		"                       words to wrap\n") {
		t.Errorf("route did not wrap under its renderer-owned column:\n%s", usage)
	}
}

func TestUsageStacksHelpRoutesWhenTheSummaryColumnIsNarrow(t *testing.T) {
	p := New()
	p.Name = "demo"
	p.Synopses = []string{"[flags]"}
	p.ScopeNote = "Placement is documented elsewhere."
	p.UsageWidth = 48
	p.DetailedHelp = []HelpRoute{{
		Invocation: "demo help a-very-long-topic-name",
		Summary:    "Explain this topic in readable prose",
	}}

	want := "Detailed help:\n" +
		"  demo help a-very-long-topic-name\n" +
		"    Explain this topic in readable prose\n"
	if usage := p.Usage(); !strings.Contains(usage, want) {
		t.Errorf("usage did not stack a narrow help route:\n%s", usage)
	}
}

// TestUsageArgumentNames covers every source of a help row: consumer flags,
// built-ins, a family member, and synthetic help and block entries. Short and
// long spellings share one field, while booleans carry no argument.
func TestUsageArgumentNames(t *testing.T) {
	p := New()
	p.Global.Int("attempts", "n", "N", 1, "attempt count")
	p.Global.Bool("verbose", "v", false, "verbose output")
	usage := p.Usage()

	for _, want := range []string{
		"-n, --attempts N",
		"-v, --verbose",
		"-X, --request METHOD",
		"-H, --header HEADER",
		"-d, --data, --data-ascii DATA",
		"-h, --help",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("Usage omits %q:\n%s", want, usage)
		}
	}
	if strings.Contains(usage, "--verbose BOOL") ||
		strings.Contains(usage, "--help BOOL") {
		t.Errorf("Usage gives a boolean flag an argument:\n%s", usage)
	}
	block := usage[strings.Index(usage, "Block options:"):]
	if !strings.Contains(block, "--block [URL...]") {
		t.Errorf("Block options omit the separator entry:\n%s", block)
	}
}

// TestUsageDefaults checks that ordinary defaults come from the registered
// value and exceptional domain wording can replace or suppress that display.
func TestUsageDefaults(t *testing.T) {
	p := New()
	p.Global.Int("attempts", "", "N", 3, "attempt count")
	p.Global.Float64("threshold", "", "P", 0.25, "decision threshold")
	p.Global.String("format", "", "FORMAT", "text", "report format")
	p.Global.Bool("enabled", "", true, "enable the feature")
	p.Global.String("adaptive", "", "MODE", "", "selection policy").
		HelpDefault("automatic")
	p.Global.Int("quiet", "", "N", 7, "hidden default").
		HelpDefault("")

	usage := flattenSpace(p.Usage())
	for _, want := range []string{
		"--attempts N attempt count (default: 3)",
		"--threshold P decision threshold (default: 0.25)",
		"--format FORMAT report format (default: text)",
		"--enabled[=false] enable the feature (default: true)",
		"--adaptive MODE selection policy (default: automatic)",
	} {
		if !strings.Contains(usage, want) {
			t.Errorf("Usage omits %q:\n%s", want, usage)
		}
	}
	if strings.Contains(usage, "hidden default (default:") {
		t.Errorf("Usage did not suppress the explicit default:\n%s", usage)
	}
}

// TestDataASCIIIsAHelpAlias checks that equivalent spellings share one row
// while remaining independently accepted flags.
func TestDataASCIIIsAHelpAlias(t *testing.T) {
	usage := New().Usage()
	if strings.Count(usage, "--data-ascii") != 1 ||
		!strings.Contains(usage, "-d, --data, --data-ascii DATA") {
		t.Errorf("--data-ascii is not shown once with --data:\n%s", usage)
	}
	for _, spelling := range []string{"--data", "--data-ascii"} {
		p, err := New().Parse([]string{
			spelling, "a=b", "https://h/",
		})
		if err != nil {
			t.Fatalf("Parse(%s): %v", spelling, err)
		}
		body, _, _, err := p.Blocks[0].RequestBody()
		if err != nil || string(body) != "a=b" {
			t.Errorf("Parse(%s) body = %q, %v", spelling, body, err)
		}
	}
}

func TestUsageListsEveryRegisteredSpelling(t *testing.T) {
	p := New(WithInsecureFlag(), WithVersionFlag("1.0"))
	p.PacingFlags(p.Global, p.Global, "parallel-max", "", "", "")
	usage := p.Usage()

	for _, registry := range []*Registry{p.Global, p.Shared, p.Block} {
		for _, option := range registry.options() {
			for _, entry := range option.entries() {
				if !strings.Contains(usage, "--"+entry.name) {
					t.Errorf("Usage omits --%s:\n%s", entry.name, usage)
				}
				if entry.shorthand != "" &&
					!strings.Contains(usage, "-"+entry.shorthand) {
					t.Errorf("Usage omits -%s:\n%s",
						entry.shorthand, usage)
				}
			}
		}
	}
}
