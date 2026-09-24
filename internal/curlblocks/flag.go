package curlblocks

import (
	"fmt"
	"strconv"

	flag "github.com/spf13/pflag"
)

// flagScope is the scope a flag was registered in, which decides
// where its value can be read from: a global flag's value lands in Globals, a
// block or shared flag's in each Block.
type flagScope uint8

const (
	scopeGlobal flagScope = iota
	scopeBlock
	scopeShared
)

func (s flagScope) String() string {
	switch s {
	case scopeGlobal:
		return "global"
	case scopeBlock:
		return "block"
	default:
		return "shared"
	}
}

// carrier describes where a scope's values are read from: the carrier's own
// scope, the type that holds them, and where a consumer finds one. A shared
// flag rides on a Block, as a block flag does. Flag.Get compares the scopes and
// names the rest in its panic.
func (s flagScope) carrier() (scope flagScope, name, source string) {
	if s == scopeGlobal {
		return scopeGlobal, "Globals", "Plan.Globals"
	}
	return scopeBlock, "a Block", "a Plan.Blocks entry"
}

// Flag is a typed handle on a registered flag. Create one through a Parser's
// scope registries
// (p.Global.Bool, p.Block.Int, p.Shared.String, ...) and read its parsed value
// with Get. The generic type T makes Get return a concrete value, so callers
// never type-assert.
type Flag[T any] struct {
	// The registration inputs, abbreviated because *Flag[T] also satisfies the
	// option interface, whose name() method takes the unabbreviated spelling.
	nm               string
	sh               string
	arg              string
	us               string
	def              T
	takesArg         bool
	helpDefault      string
	defaultSet       bool
	completionValues []string
	completionFiles  CompletionFileStyle
	scope            flagScope
	bind             bindFunc[T]
}

// bindFunc binds a typed flag onto a flag set. It is the shape of pflag's
// XxxVarP methods, so a registrar passes the method expression itself
// ((*flag.FlagSet).BoolVarP) rather than a wrapper around it.
type bindFunc[T any] func(fs *flag.FlagSet, p *T, name, shorthand string, defaultValue T, usage string)

// Get returns the flag's value from the carrier its scope puts that value in: a
// global flag's from Globals, a block or shared flag's from a Block. It
// yields the flag's registered default when the carrier did not set it — an
// absent flag and an explicit default read alike, so treat a returned slice as
// read-only: every default read shares one backing array.
//
// Reading a flag from the other kind of carrier is a programming error, not a
// value: no Globals can hold a block flag's value, so there is nothing
// truthful to return. Get panics rather than answer with a default that looks
// like the user's choice.
func (f *Flag[T]) Get(c Carrier) T {
	f.requireCarrier(c)
	if v, ok := c.customValues()[f.nm]; ok {
		return v.(T)
	}
	return f.def
}

// IsSet reports whether the flag was explicitly supplied in the carrier's
// resolved scope. It distinguishes an omitted flag from one set to its default.
func (f *Flag[T]) IsSet(c Carrier) bool {
	f.requireCarrier(c)
	return c.customChanged()[f.nm]
}

func (f *Flag[T]) requireCarrier(c Carrier) {
	if want, wantName, wantSource := f.scope.carrier(); c.carrierScope() != want {
		_, gotName, _ := c.carrierScope().carrier()
		panic(fmt.Sprintf("curlblocks: --%s is a %s option, so its value is carried by %s, not %s; read it from %s",
			f.nm, f.scope, wantName, gotName, wantSource))
	}
}

func (f *Flag[T]) name() string { return f.nm }

func (f *Flag[T]) takesArgument() bool { return f.takesArg }

func (f *Flag[T]) entries() []flagEntry {
	return []flagEntry{{name: f.nm, shorthand: f.sh}}
}

func (f *Flag[T]) helpEntries() []helpEntry {
	defaultText := automaticHelpDefault(f.def)
	if f.defaultSet {
		defaultText = f.helpDefault
	}
	boolean, isBoolean := any(f.def).(bool)
	return []helpEntry{{
		falseOptOut: isBoolean && boolean,
		flags:       f.entries(), argument: f.arg, usage: f.us,
		defaultText:      defaultText,
		completionValues: f.completionValues,
		completionFiles:  f.completionFiles,
	}}
}

// register binds the flag onto fs and returns an extractor that, after parsing,
// records the value and whether it was set into a segment.
func (f *Flag[T]) register(fs *flag.FlagSet) func(*segment) {
	p := new(T)
	f.bind(fs, p, f.nm, f.sh, f.def, f.us)
	return func(seg *segment) {
		seg.values[f.nm] = *p
		seg.changed[f.nm] = fs.Changed(f.nm)
	}
}

// value is the flag's raw parsed value in seg (its default if absent). The
// assertion is safe by construction: a Flag[T] only ever stores a T under its
// own name.
func (f *Flag[T]) value(seg *segment) T {
	if v, ok := seg.values[f.nm]; ok {
		return v.(T)
	}
	return f.def
}

func (f *Flag[T]) set(seg *segment) bool { return seg.changed[f.nm] }

// resolve returns the block's value if the block set it, otherwise the
// baseline's — the inherit-or-override rule for a shared flag.
func (f *Flag[T]) resolve(baseline, block *segment) T {
	if block.changed[f.nm] {
		return f.value(block)
	}
	return f.value(baseline)
}

// resolved is resolve together with whether either scope set the flag, for a
// caller that must tell an unset flag from one set to the zero value — an
// empty -X is an error where no -X at all is not.
func (f *Flag[T]) resolved(baseline, block *segment) (T, bool) {
	return f.resolve(baseline, block), f.set(block) || f.set(baseline)
}

// resolveAny is resolve boxed to any, so a flag erased to its interface can
// resolve its own value for the custom-value map (see carry).
func (f *Flag[T]) resolveAny(baseline, override *segment) any {
	return f.resolve(baseline, override)
}

// option is a flag of any type as the parser sees it. One registration may
// define several flag names. For family flags, entries enumerates every
// accepted spelling for collision checks and out-of-scope rejection.
// helpEntries groups equivalent spellings into rendered rows. name is the
// primary one, the key a segment stores the registration's value under.
type option interface {
	name() string
	takesArgument() bool
	entries() []flagEntry
	helpEntries() []helpEntry
	register(fs *flag.FlagSet) func(*segment)
}

// familyMember is one behaviour within a flag family: every spelling builds the
// same value from its argument and shares one help row.
type familyMember[T any] struct {
	flags            []flagEntry
	argument         string
	usage            string
	completionValues []string
	completionFiles  CompletionFileStyle
	build            func(string) T
}

// familyOption is one registration standing in for several flag names that
// share an ordered accumulator, so the pieces they contribute keep
// command-line order across the family however they are interleaved — curl's
// -H/-A/-u and the --data family both need that. key is the segment key the
// accumulator is stored under and the family's primary name; argType is what
// pflag reports for every member's argument. A family's resolved value is read
// by its own resolver (resolveData, resolveHeaderSet, ...), never Flag.Get, so
// it carries neither a type parameter on the value nor a scope.
type familyOption[T any] struct {
	key     string
	argType string
	members []familyMember[T]
}

func (o familyOption[T]) name() string { return o.key }

func (o familyOption[T]) takesArgument() bool { return true }

func (o familyOption[T]) entries() []flagEntry {
	var out []flagEntry
	for _, m := range o.members {
		out = append(out, m.flags...)
	}
	return out
}

func (o familyOption[T]) helpEntries() []helpEntry {
	out := make([]helpEntry, len(o.members))
	for i, m := range o.members {
		out[i] = helpEntry{
			flags: m.flags, argument: m.argument, usage: m.usage,
			completionValues: m.completionValues,
			completionFiles:  m.completionFiles,
		}
	}
	return out
}

// register binds every member onto fs, all writing to one freshly allocated
// accumulator (so order is preserved across the family's names), and returns
// an extractor recording the ordered pieces under the family's key.
func (o familyOption[T]) register(fs *flag.FlagSet) func(*segment) {
	acc := new([]T)
	for _, m := range o.members {
		for _, f := range m.flags {
			fs.VarP(
				accValue[T]{typ: o.argType, acc: acc, build: m.build},
				f.name, f.shorthand, m.usage)
		}
	}
	return accumExtractor(o.key, acc)
}

// customOption is a flag that can resolve itself into the custom-value map.
// Every consumer-registered flag is a *Flag[T] and satisfies it; a built-in
// familyOption never reaches the custom maps, so it need not.
type customOption interface {
	resolveAny(baseline, override *segment) any
	isSet(baseline, override *segment) bool
}

func (f *Flag[T]) isSet(baseline, override *segment) bool {
	return f.set(baseline) || f.set(override)
}

// scopedOption is a flag that records the scope it was registered in, so
// Flag.Get can refuse a read from the wrong carrier. Only *Flag[T] does; a
// built-in family flag is read through its own resolver, never Get.
type scopedOption interface {
	setScope(flagScope)
}

func (f *Flag[T]) setScope(s flagScope) { f.scope = s }

// carry resolves a custom flag across a baseline/override segment pair (the
// inherit-or-override rule) and boxes the result for the custom-value map. The
// assertion holds because the Registry typed methods, the only constructors of
// custom flags, all return *Flag[T].
func carry(o option, baseline, override *segment) any {
	return o.(customOption).resolveAny(baseline, override)
}

func carriedSet(o option, baseline, override *segment) bool {
	return o.(customOption).isSet(baseline, override)
}

// Registry collects the flags valid in one scope. A Parser exposes
// one per scope (Global, Block, Shared); the typed methods register a custom
// flag and return a handle to read it back.
//
// A Registry is a lightweight handle onto the scope's shared parse state plus
// the help section new flags are listed under. [Registry.Section] returns
// another handle onto the same scope whose flags are listed together under
// their own heading. [Registry.TrailingSection] puts a global section after
// the request and block scopes. Sections affect presentation only,
// independently of scope.
type Registry struct {
	shared  *regShared       // parse state shared by handles onto this scope
	section *registrySection // section new registrations are listed under
}

// regShared is the per-scope state every handle onto a scope contributes to.
type regShared struct {
	scope    flagScope           // which carrier this scope's values are read from
	all      []option            // built-in + custom, for parsing
	custom   []option            // custom only, for assembling result values
	builtins builtinHelpSections // built-in presentation groups
	sections []*registrySection  // sections[0] is the default (untitled)
	siblings []*regShared        // scopes parsed together, for collision checks
}

// registrySection records one help section in a Registry. Its flags retain
// registration order; an empty title identifies the default section.
type registrySection struct {
	notes    []string
	tables   [][][]string
	title    string
	trailing bool
	opts     []option
}

// builtinHelpSection gives curlblocks-owned flags a presentation heading
// without changing their parsing scope. An empty title keeps the flags directly
// under the scope heading.
type builtinHelpSection struct {
	notes []string
	title string
	opts  []option
}

type builtinHelpSections []builtinHelpSection

func (sections builtinHelpSections) options() []option {
	var opts []option
	for _, section := range sections {
		opts = append(opts, section.opts...)
	}
	return opts
}

// newRegistry builds a scope's default handle, seeding the flat parse list with
// the scope's built-in flags. Their separate help sections let a tool's own
// flags lead them in the help. They arrive already stamped with their scope
// (see the init in parser.go), so building a Parser writes nothing they share.
func newRegistry(scope flagScope, builtins builtinHelpSections) *Registry {
	builtinOpts := builtins.options()
	for _, o := range builtinOpts {
		validateHelpArguments(o)
	}
	defaultSec := &registrySection{}
	return &Registry{
		shared: &regShared{
			scope: scope, all: append([]option{}, builtinOpts...), builtins: builtins,
			sections: []*registrySection{defaultSec},
		},
		section: defaultSec,
	}
}

// add registers a flag after rejecting names and shorthands that collide in
// any flag set where this scope participates. Registration faults panic because
// they are API misuse.
func (r *Registry) add(o option) {
	validateHelpArguments(o)
	if want, taken, scope, ok := r.shared.collision(o); ok {
		panic(fmt.Sprintf("curlblocks: --%s%s collides with the %s option --%s%s; "+
			"rename it or drop the shorthand (see the curl-name convention in the package doc)",
			want.name, shorthandNote(want.shorthand), scope, taken.name, shorthandNote(taken.shorthand)))
	}
	stampScope(o, r.shared.scope)
	r.shared.all = append(r.shared.all, o)
	r.shared.custom = append(r.shared.custom, o)
	r.section.opts = append(r.section.opts, o)
}

// validateHelpArguments keeps the parse contract and help contract attached to
// the same registration. Every value-taking flag names what follows it, while
// a boolean flag has no argument. Argument names must be printable ASCII
// without whitespace because they are embedded directly in a terminal field.
func validateHelpArguments(o option) {
	for _, e := range o.helpEntries() {
		if len(e.flags) == 0 {
			panic(fmt.Sprintf(
				"curlblocks: option %q has a help row with no flag spelling",
				o.name()))
		}
		name := e.flags[0].name
		switch {
		case o.takesArgument() && e.argument == "":
			panic(fmt.Sprintf(
				"curlblocks: value-taking flag --%s needs a help argument name",
				name))
		case !o.takesArgument() && e.argument != "":
			panic(fmt.Sprintf(
				"curlblocks: boolean flag --%s cannot have a help argument name",
				name))
		case !validHelpArgument(e.argument):
			panic(fmt.Sprintf(
				"curlblocks: flag --%s has invalid help argument name %q",
				name, e.argument))
		}
	}
}

func validHelpArgument(argument string) bool {
	for i := range len(argument) {
		if argument[i] < '!' || argument[i] > '~' {
			return false
		}
	}
	return true
}

// collision reports the pair of flags at fault when o would redefine a long
// name or shorthand already registered, searching this scope and every scope
// sharing a flag set with it: the entry of o's that clashes, the entry it
// clashes with, and the scope that one belongs to. Both sides are enumerated
// through option.entries, since a family flag defines several names — -H, -A
// and -u are one registration between them — and a check reading only the
// primary name would miss most of the built-ins.
func (r *regShared) collision(o option) (want, taken flagEntry, scope flagScope, ok bool) {
	wants := o.entries()
	for _, s := range append([]*regShared{r}, r.siblings...) {
		for _, have := range s.all {
			for _, h := range have.entries() {
				for _, w := range wants {
					if h.name == w.name || (w.shorthand != "" && h.shorthand == w.shorthand) {
						return w, h, s.scope, true
					}
				}
			}
		}
	}
	return flagEntry{}, flagEntry{}, 0, false
}

// shorthandNote renders a shorthand for a message as "/-x", or "" if there is
// none, so a flag reads as "--insecure/-k".
func shorthandNote(short string) string {
	if short == "" {
		return ""
	}
	return "/-" + short
}

// linkScopes records that two scopes are parsed into one flag set, so a
// registration in either sees the other's names (see regShared.collision).
func linkScopes(a, b *Registry) {
	a.shared.siblings = append(a.shared.siblings, b.shared)
	b.shared.siblings = append(b.shared.siblings, a.shared)
}

// stampScope records on the flag which scope it was registered in, so a later
// Flag.Get knows which carrier holds its value. A registration that carries no
// scope — a family flag, read through its own resolver rather than Get — is
// left alone.
func stampScope(o option, scope flagScope) {
	if s, ok := o.(scopedOption); ok {
		s.setScope(scope)
	}
}

// stampScopes stamps a whole scope's built-ins, for the one-time init that
// fixes them as the package loads.
func stampScopes(scope flagScope, opts ...option) {
	for _, o := range opts {
		stampScope(o, scope)
	}
}

// flags returns every flag in the scope — built-in and custom — for
// parsing.
func (r *Registry) options() []option { return r.shared.all }

// customs returns the scope's custom flags, for assembling result values.
func (r *Registry) customs() []option { return r.shared.custom }

// helpSections returns the scope's consumer-owned help sections in order, the
// default (untitled) section first.
func (r *Registry) helpSections() []*registrySection { return r.shared.sections }

// builtinHelpSections returns the curlblocks-owned presentation sections.
func (r *Registry) builtinHelpSections() builtinHelpSections {
	return r.shared.builtins
}

// builtins returns the scope's built-in flags in parsing order.
func (r *Registry) builtins() []option { return r.shared.builtins.options() }

// Section returns a handle onto the same parsing scope whose newly registered
// flags share a help section titled title. A section changes presentation
// only: its flags parse, resolve, and use carriers exactly as flags registered
// on the Registry directly.
//
//	detect := p.Global.Section("Detect options")
//	conf := detect.String("confidence", "", "P", "0.95", "...")
//
// Sections render in creation order and their flags in registration order. A
// repeated title returns a handle onto the existing section. An empty title
// returns the scope's default section, so declaring a named section is
// optional.
func (r *Registry) Section(title string) *Registry {
	for _, s := range r.shared.sections {
		if s.title == title {
			return &Registry{shared: r.shared, section: s}
		}
	}
	sec := &registrySection{title: title}
	r.shared.sections = append(r.shared.sections, sec)
	return &Registry{shared: r.shared, section: sec}
}

// TrailingSection returns a global-scope section rendered after Request and
// Block options and before Common options. It is useful for presentation
// controls that should trail the inputs they affect. Parsing and carriers are
// unchanged.
//
// A repeated title returns the same section. Calling TrailingSection for a
// section first created by [Registry.Section] moves that section to the
// trailing position.
func (r *Registry) TrailingSection(title string) *Registry {
	if r.shared.scope != scopeGlobal {
		panic("curlblocks: TrailingSection is only valid on the global scope")
	}
	if title == "" {
		panic("curlblocks: TrailingSection needs a title")
	}
	section := r.Section(title)
	section.section.trailing = true
	return section
}

// addFlag registers one typed flag in the scope and returns its handle. bind
// is the pflag method that gives the type its flag set binding.
func addFlag[T any](
	r *Registry,
	name, shorthand, argument string,
	defaultValue T,
	usage string,
	bind bindFunc[T],
) *Flag[T] {
	f := &Flag[T]{
		nm: name, sh: shorthand, arg: argument,
		us: usage, def: defaultValue, takesArg: true, bind: bind,
	}
	r.add(f)
	return f
}

// Bool registers a bool flag (shorthand "" for none) and returns its handle.
func (r *Registry) Bool(name, shorthand string, defaultValue bool, usage string) *Flag[bool] {
	f := &Flag[bool]{
		nm: name, sh: shorthand, us: usage,
		def: defaultValue, bind: (*flag.FlagSet).BoolVarP,
	}
	r.add(f)
	return f
}

// HelpDefault replaces the automatic help display of the registered default.
// Use it when the zero value has a domain meaning such as "no limit", or when
// the effective default is adaptive. An empty display suppresses the default.
func (f *Flag[T]) HelpDefault(display string) *Flag[T] {
	for i := range len(display) {
		if display[i] < ' ' || display[i] > '~' {
			panic(fmt.Sprintf(
				"curlblocks: flag --%s has invalid help default %q",
				f.nm, display))
		}
	}
	f.helpDefault = display
	f.defaultSet = true
	return f
}

// CompletionValues records the complete finite set of values accepted by the
// flag for shell completion. Validation remains the consumer's responsibility;
// pass the same canonical values to both rather than maintaining two lists. It
// replaces earlier completion values and copies values before returning.
func (f *Flag[T]) CompletionValues(values ...string) *Flag[T] {
	f.completionValues = append([]string(nil), values...)
	return f
}

// CompletionFiles records how the flag's value can identify a local file.
// It replaces the previous style and affects completion metadata only; parsing
// and file access are unchanged.
func (f *Flag[T]) CompletionFiles(style CompletionFileStyle) *Flag[T] {
	f.completionFiles = style
	return f
}

func automaticHelpDefault[T any](value T) string {
	switch v := any(value).(type) {
	case bool:
		if v {
			return "true"
		}
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
	return ""
}

// String registers a string flag and its help argument name (shorthand "" for
// none), and returns its handle.
func (r *Registry) String(
	name, shorthand, argument, defaultValue, usage string,
) *Flag[string] {
	return addFlag(
		r, name, shorthand, argument, defaultValue, usage,
		(*flag.FlagSet).StringVarP)
}

// Int registers an int flag and its help argument name (shorthand "" for
// none), and returns its handle.
func (r *Registry) Int(
	name, shorthand, argument string,
	defaultValue int,
	usage string,
) *Flag[int] {
	return addFlag(
		r, name, shorthand, argument, defaultValue, usage,
		(*flag.FlagSet).IntVarP)
}

// Float64 registers a float flag and its help argument name (shorthand "" for
// none), and returns its handle. pflag reports a non-numeric argument; a bound
// only the tool knows (a probability in [0,1], a positive rate) is the tool's
// to check on the value it reads back, including whether NaN, which parses as
// a number and then fails every range comparison, is acceptable.
func (r *Registry) Float64(
	name, shorthand, argument string,
	defaultValue float64,
	usage string,
) *Flag[float64] {
	return addFlag(
		r, name, shorthand, argument, defaultValue, usage,
		(*flag.FlagSet).Float64VarP)
}

// StringArray registers a repeatable string flag and its help argument name
// (shorthand "" for none), and returns its handle. Values are not comma-split.
func (r *Registry) StringArray(
	name, shorthand, argument string,
	defaultValue []string,
	usage string,
) *Flag[[]string] {
	return addFlag(
		r, name, shorthand, argument, defaultValue, usage,
		(*flag.FlagSet).StringArrayVarP)
}

// Notes appends explanatory prose after this section's options. Each note is
// wrapped independently at the parser's usage width. Parsing is unchanged.
func (r *Registry) Notes(notes ...string) *Registry {
	r.section.notes = append(r.section.notes, notes...)
	return r
}

// Table appends an ASCII column table after this section's options. Cells
// contain plain text; each column is sized to its widest cell.
func (r *Registry) Table(rows [][]string) *Registry {
	r.section.tables = append(r.section.tables, rows)
	return r
}
