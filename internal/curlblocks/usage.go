package curlblocks

import (
	"fmt"
	"strings"
)

// The flag-name column grows to fit the widest name so every description starts
// in the same place, within these bounds: the minimum keeps the layout steady
// when every name is short, and the maximum stops one long name from narrowing
// every description — such a name overflows its own line instead.
const (
	helpNameColMin = 24
	helpNameColMax = 30
)

// A route summary needs enough room to remain scannable beside its invocation.
// Below that threshold, every route uses a consistently stacked layout.
const (
	helpRouteSummaryMin    = 20
	helpRouteSummaryIndent = "    "
)

// helpFlag stands in for pflag's implicit -h/--help in the listing. No flag
// represents it — pflag defines it itself — so without this the one flag a
// reader most needs is the one the help omits.
var helpFlag = helpEntry{
	flags: []flagEntry{{name: "help", shorthand: "h"}},
	usage: "show this help and exit",
}

// blockFlag stands in for the block separator, which no flag represents
// either: it is found positionally before any flag is parsed (see
// splitSegments). A reader scanning for the flag that opens a block should
// find it among the flags.
var blockFlag = helpEntry{
	flags:    []flagEntry{{name: "block"}},
	argument: "[URL...]",
	usage:    "open a block with optional request URLs",
}

const usageScopeNote = "Placement: global flags are preamble-only. Request flags in " +
	"the preamble set the baseline; inside a block they affect that block. " +
	"Block flags are block-only. -h/--help is accepted anywhere."

// Usage returns help text listing every built-in and custom flag by scope
// and optional help section. [Parser.UsageWidth] optionally wraps prose and
// option descriptions at a fixed width. A tool prints it when Parse returns
// ErrHelp. The Parser's presentation fields frame the listing.
func (p *Parser) Usage() string {
	// Lay out every scope first, so the name column is derived from the lines that
	// are actually drawn rather than guessed at.
	scopes := globalSections(p.Global, false)
	scopes = append(scopes, renderedSection{
		title: "Request options:", items: scopeItems(p.Shared),
	})
	// The separator leads its scope: it is what opens a block, so a reader meets
	// it before the flags that go inside one.
	scopes = append(scopes, renderedSection{
		title: "Block options:",
		items: append([]helpItem{{indent: "  ", entry: blockFlag}},
			scopeItems(p.Block)...),
	})
	scopes = append(scopes, globalSections(p.Global, true)...)
	scopes = append(scopes, renderedSection{
		title: "Common options:", items: builtinItems(p.Global, helpFlag),
	})
	nameCol := helpNameColMin
	for _, s := range scopes {
		for _, it := range s.items {
			if it.isFlag() {
				nameCol = max(nameCol, len(it.field()))
			}
		}
	}
	nameCol = min(nameCol, helpNameColMax)

	var b strings.Builder
	if p.Description != "" {
		writeFramingParagraph(
			&b, p.Name+" - ", p.Description, p.UsageWidth)
		b.WriteByte('\n')
	}
	b.WriteString("Usage:\n")
	synopses := p.Synopses
	if len(synopses) == 0 {
		synopses = []string{
			"[global options] [request options] URL " +
				"[--block [URL...] [block options]]...",
		}
	}
	for _, synopsis := range synopses {
		fmt.Fprintf(&b, "  %s %s\n", p.Name, synopsis)
	}
	b.WriteByte('\n')
	if p.Guide != "" {
		b.WriteString("Guide:\n  ")
		b.WriteString(p.Guide)
		b.WriteString("\n\n")
	}
	scopeNote := p.ScopeNote
	if scopeNote == "" {
		scopeNote = usageScopeNote
	}
	writeFramingParagraph(&b, "", scopeNote, p.UsageWidth)
	b.WriteByte('\n')
	for _, s := range scopes {
		writeScope(&b, s.title, s.items, nameCol, p.UsageWidth)
		for _, table := range s.tables {
			writeHelpTable(&b, table)
		}
		for _, note := range s.notes {
			writeFramingParagraph(&b, "  ", note, p.UsageWidth)
		}
		if len(s.notes) > 0 {
			b.WriteByte('\n')
		}
	}
	writeExamples(&b, p.Examples)
	writeHelpRoutes(&b, p.DetailedHelp, p.UsageWidth)
	return b.String()
}

func writeFramingParagraph(
	b *strings.Builder,
	prefix, prose string,
	width int,
) {
	lines := wrapFramingProse(prose, width-len(prefix))
	if len(lines) == 0 {
		return
	}
	b.WriteString(prefix)
	b.WriteString(lines[0])
	b.WriteByte('\n')
	continuation := strings.Repeat(" ", len(prefix))
	for _, line := range lines[1:] {
		b.WriteString(continuation)
		b.WriteString(line)
		b.WriteByte('\n')
	}
}

func wrapFramingProse(prose string, width int) []string {
	if width <= 0 {
		if line := strings.Join(strings.Fields(prose), " "); line != "" {
			return []string{line}
		}
		return nil
	}
	return wrapHelpWords(prose, width)
}

func writeExamples(b *strings.Builder, examples []UsageExample) {
	nonEmpty := make([]UsageExample, 0, len(examples))
	for _, example := range examples {
		if len(example.Lines) > 0 {
			nonEmpty = append(nonEmpty, example)
		}
	}
	if len(nonEmpty) == 0 {
		return
	}
	heading := "Example:"
	if len(nonEmpty) > 1 {
		heading = "Examples:"
	}
	b.WriteString(heading)
	b.WriteByte('\n')
	for i, example := range nonEmpty {
		if i > 0 {
			b.WriteByte('\n')
		}
		for j, line := range example.Lines {
			indent := "  "
			if j > 0 {
				indent = "    "
			}
			b.WriteString(indent)
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
}

func writeHelpRoutes(
	b *strings.Builder,
	routes []HelpRoute,
	width int,
) {
	if len(routes) == 0 {
		return
	}
	b.WriteString("Detailed help:\n")
	invocationWidth := 0
	for _, route := range routes {
		invocationWidth = max(invocationWidth, len(route.Invocation))
	}
	const (
		indent  = "  "
		spacing = "   "
	)
	descriptionCol := len(indent) + invocationWidth + len(spacing)
	stacked := width > 0 && width-descriptionCol < helpRouteSummaryMin
	for _, route := range routes {
		if stacked {
			b.WriteString(indent)
			b.WriteString(route.Invocation)
			b.WriteByte('\n')
			for _, line := range wrapRouteSummary(
				route.Summary, width-len(helpRouteSummaryIndent)) {
				b.WriteString(helpRouteSummaryIndent)
				b.WriteString(line)
				b.WriteByte('\n')
			}
			continue
		}
		lines := wrapRouteSummary(route.Summary, width-descriptionCol)
		if len(lines) == 0 {
			lines = []string{""}
		}
		fmt.Fprintf(b, "%s%-*s%s%s\n",
			indent, invocationWidth, route.Invocation, spacing, lines[0])
		for _, line := range lines[1:] {
			fmt.Fprintf(b, "%*s%s\n", descriptionCol, "", line)
		}
	}
}

func wrapRouteSummary(summary string, width int) []string {
	if width <= 0 {
		if line := strings.Join(strings.Fields(summary), " "); line != "" {
			return []string{line}
		}
		return nil
	}
	return wrapHelpWords(summary, width)
}

type renderedSection struct {
	title  string
	items  []helpItem
	notes  []string
	tables [][][]string
}

// globalSections renders every non-empty section in the global parsing scope
// at the top level. The unnamed section uses the generic Global options
// heading.
func globalSections(r *Registry, trailing bool) []renderedSection {
	var sections []renderedSection
	for _, s := range r.helpSections() {
		if len(s.opts) == 0 || s.trailing != trailing {
			continue
		}
		title := s.title
		if title == "" {
			title = "Global options"
		}
		sections = append(sections, renderedSection{
			title:  title + ":",
			items:  optionItems(s.opts, "  "),
			notes:  s.notes,
			tables: s.tables,
		})
	}
	return sections
}

func optionItems(opts []option, indent string) []helpItem {
	var items []helpItem
	for _, o := range opts {
		for _, e := range o.helpEntries() {
			items = append(items, helpItem{indent: indent, entry: e})
		}
	}
	return items
}

func builtinItems(r *Registry, extra ...helpEntry) []helpItem {
	items := optionItems(r.builtins(), "  ")
	for _, e := range extra {
		items = append(items, helpItem{indent: "  ", entry: e})
	}
	return items
}

// helpItem is one line of a scope's listing: a subsection heading, a blank
// line closing one, or one flag's entry at the indent its nesting gives it.
type helpItem struct {
	note    string // explanatory prose within a subsection
	heading string // non-empty for a subsection heading ([Registry.Section])
	spacer  bool   // a blank line, closing the subsection above it
	indent  string
	entry   helpEntry
}

// isFlag reports whether the item draws a flag's line, as opposed to a heading
// or a separator — the lines the name column is measured from.
func (it helpItem) isFlag() bool { return it.heading == "" && it.note == "" && !it.spacer }

// field is the rendered name column of a flag's line: its indent, an optional
// shorthand, and the long name. Padding it as a whole lines descriptions up in
// one column whatever the nesting depth, so a sectioned flag indented deeper
// than a top-level one still starts its description in the same place.
func (it helpItem) field() string {
	spellings := make([]string, 0, len(it.entry.flags)*2)
	for _, f := range it.entry.flags {
		if f.shorthand != "" {
			spellings = append(spellings, "-"+f.shorthand)
		}
		long := "--" + f.name
		if it.entry.falseOptOut {
			long += "[=false]"
		}
		spellings = append(spellings, long)
	}
	col := strings.Join(spellings, ", ")
	if it.entry.argument != "" {
		col += " " + it.entry.argument
	}
	return it.indent + col
}

// scopeItems lays out one scope's listing: the tool's own flags, then the
// curlblocks-owned built-in sections. A registration standing in for related
// flags (e.g. --data) contributes one line per member, since each is a flag a
// reader can type. It is the single enumeration of a scope's lines, so the
// column measured is the column drawn.
func scopeItems(r *Registry) []helpItem {
	var items []helpItem
	grouped := false
	for _, s := range r.helpSections() {
		indent := "  "
		if s.title != "" {
			items = append(items, helpItem{heading: s.title})
			indent = "    " // nest a named subsection's options under its heading
			grouped = true
		}
		for _, o := range s.opts {
			for _, e := range o.helpEntries() {
				items = append(items, helpItem{indent: indent, entry: e})
			}
		}
		for _, note := range s.notes {
			items = append(items, helpItem{note: note, indent: indent})
		}
	}

	builtins := r.builtinHelpSections()
	if grouped && len(builtins) > 0 && builtins[0].title == "" {
		items = append(items, helpItem{spacer: true})
	}
	for _, section := range builtins {
		indent := "  "
		if section.title != "" {
			items = append(items, helpItem{heading: section.title})
			indent = "    "
		}
		items = append(items, optionItems(section.opts, indent)...)
		for _, note := range section.notes {
			items = append(items, helpItem{note: note, indent: indent})
		}
	}
	return items
}

// writeScope renders one scope's items under title, each description wrapped
// into the column nameCol sets. A scope with no items is skipped entirely.
func writeScope(
	b *strings.Builder,
	title string,
	items []helpItem,
	nameCol, width int,
) {
	if len(items) == 0 {
		return
	}
	b.WriteString(title)
	b.WriteByte('\n')
	for _, it := range items {
		switch {
		case it.heading != "":
			fmt.Fprintf(b, "\n  %s:\n", it.heading)
		case it.note != "":
			writeFramingParagraph(b, it.indent, it.note, width)
		case it.spacer:
			b.WriteByte('\n')
		default:
			writeOptionLine(
				b, it.field(), it.entry, nameCol, width)
		}
	}
	b.WriteByte('\n')
}

// writeOptionLine renders one flag. With no width, it preserves the original
// one-line layout. At a fixed width it folds the description; a field wider
// than nameCol takes a line of its own instead of shifting every other row.
func writeOptionLine(
	b *strings.Builder,
	field string,
	entry helpEntry,
	nameCol, width int,
) {
	if width <= 0 {
		pad := max(nameCol, len(field))
		fmt.Fprintf(b, "%-*s %s\n", pad, field, entry.description())
		return
	}

	descriptionCol := nameCol + 1
	lines := wrapOptionDescription(
		entry.usage, entry.defaultText, width-descriptionCol)
	if len(lines) == 0 {
		lines = []string{""}
	}
	if len(field) > nameCol {
		b.WriteString(field)
		b.WriteByte('\n')
		fmt.Fprintf(b, "%*s%s\n", descriptionCol, "", lines[0])
	} else {
		fmt.Fprintf(b, "%-*s %s\n", nameCol, field, lines[0])
	}
	for _, line := range lines[1:] {
		fmt.Fprintf(b, "%*s%s\n", descriptionCol, "", line)
	}
}

// wrapOptionDescription keeps a short default clause together when it fits in
// the description column. The default moves intact to a continuation instead
// of leaving a fragment such as "(default: no" on the preceding line.
func wrapOptionDescription(usage, defaultText string, width int) []string {
	lines := wrapHelpWords(usage, width)
	if defaultText == "" {
		return lines
	}
	clause := "(default: " + defaultText + ")"
	if len(clause) > width {
		return append(lines, wrapHelpWords(clause, width)...)
	}
	if len(lines) == 0 {
		return []string{clause}
	}
	last := len(lines) - 1
	if len(lines[last])+1+len(clause) <= width {
		lines[last] += " " + clause
	} else {
		lines = append(lines, clause)
	}
	return lines
}

// wrapHelpWords folds one option description into lines no wider than width.
// Descriptions are single-line prose, so collapsing internal whitespace is
// intentional. A word wider than the available column remains whole and lets
// the output-width test identify the authoring problem.
func wrapHelpWords(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 || width <= 0 {
		return words
	}
	lines := []string{words[0]}
	for _, word := range words[1:] {
		last := len(lines) - 1
		if len(lines[last])+1+len(word) <= width {
			lines[last] += " " + word
		} else {
			lines = append(lines, word)
		}
	}
	return lines
}

type flagEntry struct {
	name, shorthand string
}

// helpEntry is one rendered row: one or more equivalent flag spellings, their
// semantic argument, usage text, and optional default display.
type helpEntry struct {
	// falseOptOut displays attached boolean syntax for default-on options.
	falseOptOut      bool
	flags            []flagEntry
	argument, usage  string
	defaultText      string
	completionValues []string
	completionFiles  CompletionFileStyle
}

func (e helpEntry) description() string {
	if e.defaultText == "" {
		return e.usage
	}
	return e.usage + " (default: " + e.defaultText + ")"
}

func writeHelpTable(b *strings.Builder, rows [][]string) {
	var widths []int
	for _, row := range rows {
		for column, cell := range row {
			for len(widths) <= column {
				widths = append(widths, 0)
			}
			widths[column] = max(widths[column], len(cell))
		}
	}
	for _, row := range rows {
		b.WriteString("  ")
		for column, cell := range row {
			b.WriteString(cell)
			if column < len(row)-1 {
				b.WriteString(strings.Repeat(" ", widths[column]-len(cell)+2))
			}
		}
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
}
