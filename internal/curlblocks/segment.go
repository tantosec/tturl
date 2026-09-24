package curlblocks

import (
	"fmt"
	"io"
	"slices"

	flag "github.com/spf13/pflag"
)

const blockSeparator = "--block"

// segment holds one preamble or block after parsing: each flag's value and
// whether it was set, plus the bare positional arguments (URLs).
//
// The values are held keyed by flag name and boxed to any, rather than on the
// flags themselves, because a Flag is a package-level singleton (the built-ins)
// or a handle a consumer keeps across parses: a value written onto one would be
// shared by every Parser and every segment. The map is what makes a parse
// re-entrant, and Flag.value re-asserts the type it alone ever stored.
type segment struct {
	values      map[string]any
	changed     map[string]bool
	positionals []string
}

// pieces reads a family flag's accumulated slice from a segment, or nil when
// the flag was not set there. It is the family analogue of Flag.value: the
// -d/-F/--json flags keep their ordered accumulator under one segment key
// rather than as a typed Flag.
func pieces[T any](seg *segment, key string) []T {
	if v, ok := seg.values[key]; ok {
		return v.([]T)
	}
	return nil
}

// accumPieces merges a family flag's pieces from the preamble with the
// block's, in that order, so a block's content appends to the shared baseline
// rather than replacing it. The result is freshly allocated, so the preamble's
// pieces are never aliased across blocks; it is nil when neither scope carried
// the flag.
func accumPieces[T any](pre, blk *segment, key string) []T {
	return slices.Concat(pieces[T](pre, key), pieces[T](blk, key))
}

// accumExtractor returns an extractor that records a family flag's ordered
// accumulator into a segment under key, marking it changed when the accumulator
// is non-empty. The family flags register several pflag names against one acc
// (so pieces keep command-line order across the family's names); this stores
// it.
func accumExtractor[T any](key string, acc *[]T) func(*segment) {
	return func(seg *segment) {
		seg.values[key] = *acc
		seg.changed[key] = len(*acc) > 0
	}
}

// accValue is the pflag.Value backing one flag name of a family flag. Each
// occurrence builds an element and appends it to the accumulator the family's
// names share, so pieces keep command-line order across them; typ is
// the argument name pflag reports for the flag.
type accValue[T any] struct {
	typ   string
	acc   *[]T
	build func(string) T
}

func (accValue[T]) String() string { return "" }
func (v accValue[T]) Type() string { return v.typ }
func (v accValue[T]) Set(s string) error {
	*v.acc = append(*v.acc, v.build(s))
	return nil
}

// splitSegments partitions args into the preamble (everything before the first
// separator) and one []string per block (the tokens between separators). The
// separator tokens themselves are dropped.
//
// The split is positional and happens before any flag is parsed, so an argument
// that is itself spelled "--block" reads as a separator wherever it appears.
// The escape is the attached form, "--flag=--block", which is one token and so
// never a separator.
func splitSegments(args []string) (preamble []string, blocks [][]string) {
	i := 0
	for i < len(args) && !isSeparator(args[i]) {
		preamble = append(preamble, args[i])
		i++
	}
	for i < len(args) {
		i++ // consume the separator
		var blk []string
		for i < len(args) && !isSeparator(args[i]) {
			blk = append(blk, args[i])
			i++
		}
		blocks = append(blocks, blk)
	}
	return preamble, blocks
}

func isSeparator(tok string) bool {
	return tok == blockSeparator
}

// parseSegment parses one segment's tokens (a preamble or a single block)
// against the active flags. Bare arguments become positionals. Every other
// known flag is still recognised but rejected as not valid in this segment,
// so a misplaced flag reads as such rather than as pflag's generic "unknown
// flag"; a genuinely unknown flag keeps pflag's error.
func parseSegment(where string, args []string, active, known []option) (*segment, error) {
	fs := flag.NewFlagSet(where, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.SetInterspersed(true)

	seg := &segment{values: map[string]any{}, changed: map[string]bool{}}
	activeNames := make(map[string]bool, len(active))
	var extractors []func(*segment)
	for _, opt := range active {
		extractors = append(extractors, opt.register(fs))
		activeNames[opt.name()] = true
	}

	var misplaced []flagEntry
	for _, opt := range known {
		if !activeNames[opt.name()] {
			rejectOption(fs, opt, &misplaced)
		}
	}

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if len(misplaced) > 0 {
		return nil, misplacedErr(where, misplaced[0])
	}

	for _, extract := range extractors {
		extract(seg)
	}
	seg.positionals = fs.Args()
	return seg, nil
}

// rejecter is the pflag.Value of a flag recognised in some scope but not the
// one being parsed. Its Set records the flag and returns nil, so pflag
// accepts the token without wrapping our message; parseSegment then reports it.
// The flag is registered to take no argument, so it is recorded the instant it
// appears.
type rejecter struct {
	entry     flagEntry
	misplaced *[]flagEntry
}

func (rejecter) String() string { return "" }
func (rejecter) Type() string   { return "" }
func (r rejecter) Set(string) error {
	*r.misplaced = append(*r.misplaced, r.entry)
	return nil
}

// rejectOption registers every flag name the option defines as a rejecter, so
// a family flag's members (--data-binary as much as -d) each report where they
// are valid rather than falling to pflag's generic "unknown flag".
func rejectOption(fs *flag.FlagSet, opt option, misplaced *[]flagEntry) {
	for _, e := range opt.entries() {
		fs.VarP(rejecter{e, misplaced}, e.name, e.shorthand, "")
		f := fs.Lookup(e.name)
		f.NoOptDefVal = "\x00" // non-empty: the flag never consumes an argument
		f.Hidden = true
	}
}

func misplacedErr(where string, e flagEntry) error {
	if e.shorthand != "" {
		return fmt.Errorf("--%s (-%s) is not valid in %s", e.name, e.shorthand, where)
	}
	return fmt.Errorf("--%s is not valid in %s", e.name, where)
}
