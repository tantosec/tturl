package curlblocks

import (
	"fmt"
	"strconv"
	"strings"
)

// NameSpec is one block's requests to label together, in send order. Source
// identifies the source block; specs from one block use the same value.
type NameSpec struct {
	// Name is the requested block name, or empty for automatic naming.
	Name string
	// Count is the number of repeated copies for each row.
	Count int
	// Tokens are the fan's display-safe token names.
	Tokens []string
	// Rows contains one display-safe fan-value row per request.
	Rows [][]string
	// Source identifies the source block.
	Source int
}

// NameSpecs returns the label specs for a resolved block. source identifies
// that block among all blocks whose specs will be passed to [ResolveNames].
func (b Block) NameSpecs(fan *Fan, source int) []NameSpec {
	if fan == nil && len(b.Vary) > 0 {
		panic("curlblocks: NameSpecs(nil) on a block with --vary: pass the Fan from Block.Fan, " +
			"whose variants are the requests these labels name")
	}
	if fan != nil {
		rows := make([][]string, len(fan.Variants))
		for i, v := range fan.Variants {
			rows[i] = v.Values
		}
		return []NameSpec{{
			Name: b.Name, Count: b.Repeat, Tokens: fan.Tokens, Rows: rows,
			Source: source,
		}}
	}
	specs := make([]NameSpec, len(b.URLs))
	for i := range b.URLs {
		specs[i] = NameSpec{Name: b.Name, Count: b.Repeat, Source: source}
	}
	return specs
}

// Label is one concrete request's complete printable-ASCII identity. Pins are
// the group keys that select it: its block name and, for a fan, its tokens.
type Label struct {
	// Display is the unique complete request identity.
	Display string
	// Pins are group keys that select this request.
	Pins []string
}

// ResolveNames assigns one unique complete label to every request represented
// by specs. It never truncates a label or replaces a fan value with an index.
//
// A named fan is "Name:Token=value"; an unnamed fan is "Token=value". Repeated
// requests and duplicate variants within one source receive a ~N suffix,
// padded to the multiplicity's width. Reusing a name or producing the same
// rendered base from different sources is an error.
func ResolveNames(specs []NameSpec) ([]Label, error) {
	checkRows(specs)
	raw := specs
	displayed := displaySpecs(specs)
	if err := checkNameOwners(displayed); err != nil {
		return nil, err
	}
	runs := expandRuns(displayed, raw)
	if err := checkBaseOwners(runs); err != nil {
		return nil, err
	}
	return resolveRuns(runs)
}

func checkRows(specs []NameSpec) {
	for i, s := range specs {
		for k, row := range s.Rows {
			if len(row) != len(s.Tokens) {
				panic(fmt.Sprintf("curlblocks: NameSpec %d row %d has %d values for %d tokens: "+
					"a fan spec's every row carries one value per token",
					i, k, len(row), len(s.Tokens)))
			}
		}
	}
}

func checkNameOwners(specs []NameSpec) error {
	owners := map[string]int{}
	for _, s := range specs {
		if s.Name == "" {
			continue
		}
		if source, ok := owners[s.Name]; ok && source != s.Source {
			return fmt.Errorf("request name %q is used by source blocks %d and %d",
				s.Name, source+1, s.Source+1)
		}
		owners[s.Name] = s.Source
	}
	return nil
}

type nameRun struct {
	base   string
	count  int
	source int
	pins   []string
	auto   bool
	origin string
}

func expandRuns(specs, raw []NameSpec) []nameRun {
	var runs []nameRun
	for i, s := range specs {
		if len(s.Tokens) == 0 {
			pins := []string(nil)
			if s.Name != "" {
				pins = []string{s.Name}
			}
			runs = append(runs, nameRun{
				base: s.Name, count: s.Count, source: s.Source, pins: pins,
				auto: s.Name == "", origin: provenance(raw[i], nil),
			})
			continue
		}
		pins := append([]string{}, s.Tokens...)
		if s.Name != "" {
			pins = append(pins, s.Name)
		}
		prefix := ""
		if s.Name != "" {
			prefix = s.Name + ":"
		}
		for k, row := range s.Rows {
			runs = append(runs, nameRun{
				base:  prefix + joinPairs(s.Tokens, row),
				count: s.Count, source: s.Source, pins: pins,
				origin: provenance(raw[i], raw[i].Rows[k]),
			})
		}
	}
	return runs
}

func checkBaseOwners(runs []nameRun) error {
	type owner struct {
		source int
		origin string
	}
	owners := map[string]owner{}
	for _, r := range runs {
		if r.auto {
			continue
		}
		if previous, ok := owners[r.base]; ok &&
			(previous.source != r.source || previous.origin != r.origin) {
			return fmt.Errorf("request label %q is produced by distinct inputs in source blocks %d and %d",
				r.base, previous.source+1, r.source+1)
		}
		owners[r.base] = owner{source: r.source, origin: r.origin}
	}
	return nil
}

func provenance(spec NameSpec, row []string) string {
	var b strings.Builder
	b.WriteString(strconv.Quote(spec.Name))
	for _, token := range spec.Tokens {
		b.WriteByte('|')
		b.WriteString(strconv.Quote(token))
	}
	for _, value := range row {
		b.WriteByte('=')
		b.WriteString(strconv.Quote(value))
	}
	return b.String()
}

func displaySpecs(specs []NameSpec) []NameSpec {
	out := make([]NameSpec, len(specs))
	for i, s := range specs {
		s.Name = displayValue(s.Name)
		s.Tokens = displayStrings(s.Tokens)
		if len(s.Rows) > 0 {
			rows := make([][]string, len(s.Rows))
			for k, row := range s.Rows {
				rows[k] = displayStrings(row)
			}
			s.Rows = rows
		}
		out[i] = s
	}
	return out
}

func displayStrings(in []string) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = displayValue(s)
	}
	return out
}

func joinPairs(tokens, row []string) string {
	pairs := make([]string, len(tokens))
	for i := range tokens {
		pairs[i] = tokens[i] + "=" + row[i]
	}
	return strings.Join(pairs, ",")
}

func resolveRuns(runs []nameRun) ([]Label, error) {
	totals := map[string]int{}
	reserved := map[string]bool{}
	for _, r := range runs {
		if !r.auto {
			totals[r.base] += r.count
			reserved[r.base] = true
		}
	}

	for i := range runs {
		if !runs[i].auto {
			continue
		}
		for candidate := 0; ; candidate++ {
			base := autoLabel(candidate)
			if autoBaseAvailable(base, runs[i].count, reserved) {
				runs[i].base = base
				totals[base] = runs[i].count
				reserved[base] = true
				for n := 1; n <= runs[i].count; n++ {
					reserved[multiplicityLabel(base, n, runs[i].count)] = true
				}
				break
			}
		}
	}

	out := make([]Label, 0)
	used := map[string]string{}
	seq := map[string]int{}
	for _, r := range runs {
		for range r.count {
			seq[r.base]++
			label := multiplicityLabel(r.base, seq[r.base], totals[r.base])
			if previous, ok := used[label]; ok && previous != r.base {
				return nil, fmt.Errorf(
					"request label %q is produced by %q and %q", label, previous, r.base)
			}
			used[label] = r.base
			out = append(out, Label{Display: label, Pins: r.pins})
		}
	}
	return out, nil
}

func autoBaseAvailable(base string, count int, reserved map[string]bool) bool {
	if reserved[base] {
		return false
	}
	for n := 1; n <= count; n++ {
		if reserved[multiplicityLabel(base, n, count)] {
			return false
		}
	}
	return true
}

func multiplicityLabel(base string, ordinal, total int) string {
	if total <= 1 {
		return base
	}
	return fmt.Sprintf("%s~%0*d", base, digits(total), ordinal)
}

func digits(n int) int { return len(strconv.Itoa(max(n, 1))) }

// autoLabel maps a zero-based index to spreadsheet-style A-Z, AA, AB, ...
func autoLabel(i int) string {
	name := ""
	for i++; i > 0; i /= 26 {
		i--
		name = string(rune('A'+i%26)) + name
	}
	return name
}
