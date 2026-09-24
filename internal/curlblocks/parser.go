package curlblocks

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	flag "github.com/spf13/pflag"
)

// ErrHelp is returned by Parse when the command requests help with -h or
// --help. A tool should print Parser.Usage and exit zero.
var ErrHelp = flag.ErrHelp

// ErrVersion is returned by Parse when the command requests its version with
// --version or -V. A tool should print [Parser.Version] and exit zero. Only a
// Parser built with [WithVersionFlag] can return it.
var ErrVersion = errors.New("curlblocks: version requested")

// The built-in flags. Every Parser includes them bar the opt-in globals, which
// a ParserOption registers; tools add their own alongside. Their handles are
// read by the projection methods below. The family flags — fHeaders, fData,
// fForm, and fJSON are declared alongside the code resolving each in header.go,
// data.go, form.go and json.go.
var (
	fInsecure = &Flag[bool]{
		nm: "insecure", sh: "k", us: "skip TLS certificate verification",
		bind: (*flag.FlagSet).BoolVarP,
	}
	fVersion    = &Flag[bool]{nm: "version", sh: "V", us: "show the version and exit", bind: (*flag.FlagSet).BoolVarP}
	fFormEscape = &Flag[bool]{
		nm: "form-escape", us: "backslash-escape multipart field and file names",
		bind: (*flag.FlagSet).BoolVarP,
	}
	fCookie = &Flag[[]string]{
		nm: "cookie", sh: "b",
		us:  "add cookies; repeat or separate them with ';'",
		arg: "COOKIE", takesArg: true,
		bind: (*flag.FlagSet).StringArrayVarP,
	}
	fMethod = &Flag[string]{
		nm: "request", sh: "X", arg: "METHOD",
		us: "set the request method", takesArg: true,
		bind: (*flag.FlagSet).StringVarP,
	}
	fURL = &Flag[[]string]{
		nm: "url", arg: "URL",
		us:       "add a URL; bare URLs are also accepted",
		takesArg: true, bind: (*flag.FlagSet).StringArrayVarP,
	}
	fRepeat = &Flag[string]{
		nm: "repeat", arg: "N",
		us:       "make N copies of each expanded request; positive integer",
		takesArg: true, bind: (*flag.FlagSet).StringVarP,
		helpDefault: "1", defaultSet: true,
	}
	fVary = &Flag[[]string]{
		nm: "vary", arg: "NAME[:ENC...]=SOURCE",
		us:       "substitute values; requires one resolved URL",
		takesArg: true, bind: (*flag.FlagSet).StringArrayVarP,
		completionFiles: CompletionVaryFiles,
	}
	fVaryMode = &Flag[string]{
		nm: "vary-mode", arg: "MODE", def: varyModeClusterbomb,
		us:       "clusterbomb: all combinations; pitchfork: pairs, stopping at shortest binding",
		takesArg: true, bind: (*flag.FlagSet).StringVarP,
		completionValues: varyModeNames,
	}
	fVaryFileContent = &Flag[bool]{
		nm: "vary-file-content", def: true,
		us:   "vary referenced body bytes",
		bind: (*flag.FlagSet).BoolVarP,
	}
	fName = &Flag[string]{
		nm: "name", arg: "NAME",
		us:       "name this block's requests; names must be unique",
		takesArg: true, bind: (*flag.FlagSet).StringVarP,
	}

	// --reset-body is a block-only, curlblocks-native body-subtraction flag.
	fResetBody = &Flag[bool]{
		nm: "reset-body", us: "discard inherited body",
		bind: (*flag.FlagSet).BoolVarP,
	}
)

var (
	builtinShared = builtinHelpSections{
		{
			title: "Headers and method",
			opts:  []option{fHeaders, fCookie, fMethod},
		},
		{
			title: "Bodies",
			notes: []string{"Data flags compose; form flags compose. Data, form and JSON cannot mix."},
			opts:  []option{fData, fForm, fJSON, fFormEscape},
		},
		{
			title: "Requests and expansion",
			notes: []string{
				"Vary with three and two values make six clusterbomb variants or two pitchfork " +
					"variants; repeat multiplies each.",
			},
			opts: []option{fURL, fRepeat, fVary, fVaryMode, fVaryFileContent, fName},
		},
	}
	builtinBlock = builtinHelpSections{{opts: []option{fResetBody}}}
)

// A built-in is a package-level singleton every Parser shares, so each is
// stamped with the scope it belongs to once, as the package loads. Stamping
// per New instead would have every construction write to memory every other
// Parser is concurrently reading — the same value each time, and a data race
// all the same. Only the built-ins need this; a custom flag is stamped by the
// registration that creates it (see Registry.add).
func init() {
	stampScopes(scopeGlobal, fInsecure, fVersion) // opt-in; New takes the subset asked for
	stampScopes(scopeShared, builtinShared.options()...)
	stampScopes(scopeBlock, builtinBlock.options()...)
}

// Parser parses a request command line. Construct one with New, optionally
// register custom flags through Global/Block/Shared, then call Parse. Its
// presentation fields are used only by Usage.
type Parser struct {
	// Name is the command as the help calls it, defaulting to the running
	// binary's base name (see programName). A tool with several commands sets
	// the complete command name itself ("mytool measure").
	Name string
	// Description is the summary shown before the usage forms.
	Description string

	// Synopses are the command forms listed after "Usage:". Each is appended
	// to Name, so a parser named "mytool measure" might use
	// "[flags] URL". The default is curlblocks' compact preamble-URL form.
	Synopses []string
	// Guide is an optional command that leads from routine usage to the
	// command's long-form guide. Usage renders it immediately after the
	// synopses, before the scope note and option listing.
	Guide string
	// ScopeNote replaces the placement paragraph between the synopses and
	// option listing. The default explains curlblocks' three parsing scopes.
	ScopeNote string
	// Examples are complete command examples rendered after the option listing.
	// Lines hold command text without presentation indentation. A multi-line
	// example supplies any shell continuation syntax its lines require; Usage
	// owns their leading indentation and separation.
	Examples []UsageExample
	// DetailedHelp lists routes to longer help. Usage owns the section heading,
	// row alignment, wrapping, and indentation.
	DetailedHelp []HelpRoute
	// UsageWidth wraps option descriptions at this fixed width. Values <= 0
	// retain each authored description and framing paragraph on one line.
	UsageWidth int

	userAgent     string // default User-Agent header baked in (see WithUserAgent)
	defaultScheme string // scheme filled into schemeless URLs (see WithDefaultScheme)
	maxFanSize    int    // ceiling on a block's --vary fan; 0 = none (see WithMaxFanSize)
	insecureFlag  bool   // register --insecure/-k (see WithInsecureFlag)
	version       string // the tool's version, and the -V flag with it (see WithVersionFlag)
	// fileReader loads an @file the command line references, defaulting to
	// os.ReadFile (see WithFileReader).
	fileReader  func(name string) ([]byte, error)
	stdinReader func() ([]byte, error)
	// pacing is the handle PacingFlags returned, held so Parse can check the
	// flags' values; nil when the consumer registered no pacing flags.
	pacing   *PacingFlags
	protocol *ProtocolFlags

	// Global registers process-wide options valid only in the preamble.
	Global *Registry
	// Block registers request options valid only inside a block.
	Block *Registry
	// Shared registers request options valid in the preamble and blocks.
	Shared *Registry
}

// UsageExample is one complete command example.
type UsageExample struct {
	// Lines are rendered as one command in order.
	Lines []string
}

// HelpRoute is one invocation leading to more detailed help.
type HelpRoute struct {
	// Invocation is the command text that selects the detailed help.
	Invocation string
	// Summary is its short description.
	Summary string
}

// A ParserOption configures a Parser at construction. Pass any number to New.
type ParserOption func(*Parser)

// WithUserAgent adds a default User-Agent to every resolved block. User header
// flags may override or remove it. Without this option no User-Agent is added.
func WithUserAgent(userAgent string) ParserOption {
	return func(p *Parser) { p.userAgent = userAgent }
}

// WithDefaultScheme sets the scheme a Parser prepends to a schemeless URL (a
// bare host or host:port). It defaults to "http", matching curl; pass "https"
// for an HTTPS-only tool, or "" to store URLs verbatim and leave scheme filling
// to the consumer.
func WithDefaultScheme(scheme string) ParserOption {
	return func(p *Parser) { p.defaultScheme = scheme }
}

// WithMaxFanSize caps each binding and the clusterbomb product at n variants.
// [Block.Fan] enforces the limit. Values at or below zero mean no limit.
func WithMaxFanSize(n int) ParserOption {
	return func(p *Parser) { p.maxFanSize = n }
}

// WithInsecureFlag registers curl's --insecure/-k flag, reported through
// [Globals.Insecure]. Without it the parser rejects that flag.
func WithInsecureFlag() ParserOption {
	return func(p *Parser) { p.insecureFlag = true }
}

// WithVersionFlag registers curl's --version/-V flag with the value returned by
// [Parser.Version]. An empty value registers nothing.
func WithVersionFlag(version string) ParserOption {
	return func(p *Parser) { p.version = version }
}

// Version returns the version string [WithVersionFlag] was given, which a tool
// prints when Parse returns [ErrVersion]. It is "" for a Parser built without
// that option, which can never return ErrVersion.
func (p *Parser) Version() string { return p.version }

// WithFileReader sets the function used to read body and variation file
// references. It defaults to [os.ReadFile].
func WithFileReader(readFile func(name string) ([]byte, error)) ParserOption {
	return func(p *Parser) { p.fileReader = readFile }
}

// WithStdinReader enables exact '-' file references through a neutral loader.
// The consumer owns snapshot reuse, cancellation and stream lifetime. Parse
// never calls the loader; nil keeps stdin references disabled.
func WithStdinReader(load func() ([]byte, error)) ParserOption {
	return func(p *Parser) { p.stdinReader = load }
}

// New returns a Parser seeded with the built-in flags and configured by the
// given options. Register custom flags through Global/Block/Shared, then call
// Parse.
func New(opts ...ParserOption) *Parser {
	p := &Parser{
		defaultScheme: defaultSchemeHTTP,
		Name:          programName(),
	}
	for _, opt := range opts {
		opt(p)
	}
	// The scope registries come last: which flags the global scope's built-ins
	// hold is itself one of the options (see WithInsecureFlag).
	var global []option
	if p.insecureFlag {
		global = append(global, fInsecure)
	}
	if p.version != "" {
		global = append(global, fVersion)
	}
	p.Global = newRegistry(scopeGlobal, builtinHelpSections{{opts: global}})
	p.Block = newRegistry(scopeBlock, builtinBlock)
	p.Shared = newRegistry(scopeShared, builtinShared)
	// A preamble accepts the global and shared scopes together, a block the shared
	// and block ones, so those pairs share a name space (see Registry.add).
	linkScopes(p.Global, p.Shared)
	linkScopes(p.Block, p.Shared)
	return p
}

// Parse turns a raw argument list into a Plan. It returns [ErrHelp] if the
// command requests help (-h/--help) and [ErrVersion] if it requests its version
// (-V/--version).
func (p *Parser) Parse(args []string) (*Plan, error) {
	preArgs, blockArgs := splitSegments(args)
	known := slices.Concat(p.Global.options(), p.Block.options(), p.Shared.options())

	// Parse the preamble before the structural checks below so that -h/--help
	// (and any flag error) is reported even when no block separator is present.
	// A URL (bare or --url) here is the shared default every block inherits; both
	// are Shared flags, so the preamble accepts them like any other baseline.
	preActive := slices.Concat(p.Global.options(), p.Shared.options())
	pre, err := parseSegment("the preamble", preArgs, preActive, known)
	if err != nil {
		return nil, err
	}

	// --version/-V short-circuits like --help: report it before the structural
	// "no block" check so `tool --version` works on its own. The flag exists
	// only when WithVersionFlag supplied something to print.
	if p.version != "" && fVersion.value(pre) {
		return nil, ErrVersion
	}
	if _, err := p.protocol.selection(pre); err != nil {
		return nil, err
	}

	// With no --block opened, the preamble is itself the single request: an
	// implicit empty block inherits every baseline request flag (URL, --repeat,
	// headers, ...). A preamble that carries no URL has nothing to send, caught
	// here with URL-focused guidance rather than deferred to toBlock's per-block
	// message.
	implicit := len(blockArgs) == 0
	if implicit {
		if len(segURLs(pre)) == 0 {
			return nil, errors.New("no URL: give a URL or use '--block' URL")
		}
		blockArgs = [][]string{{}}
	}

	plan := &Plan{Globals: p.globals(pre), implicit: implicit}
	if err := p.pacing.validate(plan.Globals); err != nil {
		return nil, err
	}
	blockActive := slices.Concat(p.Shared.options(), p.Block.options())
	for i, ba := range blockArgs {
		blk, err := parseSegment(fmt.Sprintf("block %d", i+1), ba, blockActive, known)
		if err != nil {
			return nil, err
		}
		b, err := p.toBlock(pre, blk)
		if err != nil {
			return nil, plan.BlockError(i, err)
		}
		plan.Blocks = append(plan.Blocks, b)
	}
	if err := validateBlockNames(plan.Blocks); err != nil {
		return nil, err
	}

	return plan, nil
}

// validateBlockNames enforces the group-handle contract before any deferred
// fan source is read. Rendering happens first because rendered names are what
// labels, pins, and diagnostics compare.
func validateBlockNames(blocks []Block) error {
	owners := map[string]int{}
	for i, block := range blocks {
		if block.Name == "" {
			continue
		}
		name := displayValue(block.Name)
		if previous, ok := owners[name]; ok {
			return fmt.Errorf("--name %q is used by blocks %d and %d; names identify one block",
				name, previous+1, i+1)
		}
		owners[name] = i
	}
	return nil
}

func (p *Parser) globals(pre *segment) Globals {
	return Globals{
		Insecure: fInsecure.value(pre),
		custom:   customValues(customSet{p.Global.customs(), pre, pre}),
		changed:  customChanges(customSet{p.Global.customs(), pre, pre}),
	}
}

func (p *Parser) toBlock(pre, blk *segment) (Block, error) {
	protocol, err := p.protocol.resolve(pre, blk)
	if err != nil {
		return Block{}, err
	}
	// Targeting overrides: a block's own URLs win, else it inherits the
	// preamble's.
	urls := resolveURLs(pre, blk)
	if len(urls) == 0 {
		return Block{}, errors.New("no URL: give one in the block, or in the preamble for all blocks to share")
	}
	repeatRaw, repeatSet := fRepeat.resolved(pre, blk)
	repeat, err := resolveRepeat(repeatRaw, repeatSet)
	if err != nil {
		return Block{}, err
	}
	headers, suppressed, err := resolveHeaderSet(pre, blk, p.userAgent)
	if err != nil {
		return Block{}, err
	}
	cookies, err := resolveCookies(slices.Concat(fCookie.value(pre), fCookie.value(blk)))
	if err != nil {
		return Block{}, err
	}
	data, form, jsonPieces, err := resolveBody(pre, blk)
	if err != nil {
		return Block{}, err
	}
	vary, err := resolveVary(pre, blk)
	if err != nil {
		return Block{}, err
	}
	varyMode, err := resolveVaryMode(fVaryMode.resolve(pre, blk))
	if err != nil {
		return Block{}, err
	}
	method, err := resolveMethod(pre, blk)
	if err != nil {
		return Block{}, err
	}
	// Fill a missing scheme (curl-style) so URLs leave the parser canonical. A
	// fanned block's URL is not yet one, so it keeps what the user wrote and Fan
	// fills each variant after substitution.
	if len(vary) == 0 {
		for i, u := range urls {
			urls[i] = ensureScheme(u, p.defaultScheme)
		}
	}
	fileContent, fileContentSet := fVaryFileContent.resolved(pre, blk)
	var fileContentPolicy *bool
	if fileContentSet {
		fileContentPolicy = &fileContent
	}
	b := Block{
		protocol:          protocol,
		URLs:              urls,
		Name:              fName.resolve(pre, blk),
		Headers:           headers,
		SuppressedHeaders: suppressed,
		Cookies:           cookies,
		Method:            method,
		data:              data,
		form:              form,
		json:              jsonPieces,
		FormEscape:        fFormEscape.resolve(pre, blk),
		Repeat:            repeat,
		repeatSet:         repeatSet,
		Vary:              vary,
		VaryMode:          varyMode,
		VaryFileContent:   fileContentPolicy,
		// Block-only flags take the block's own value; shared ones inherit the
		// preamble baseline unless the block set its own.
		custom: customValues(
			customSet{p.Block.customs(), blk, blk},
			customSet{p.Shared.customs(), pre, blk},
		),
		changed: customChanges(
			customSet{p.Block.customs(), blk, blk},
			customSet{p.Shared.customs(), pre, blk},
		),
		readFile:      p.fileReader,
		ReadStdin:     p.stdinReader,
		defaultScheme: p.defaultScheme,
		maxFanSize:    p.maxFanSize,
	}
	// The --vary rules are checkable only against the resolved block, since they
	// ask where a token lands; a Plan is validated, so they run here rather than
	// waiting for the consumer to call Fan.
	if err := b.validateFileRefs(); err != nil {
		return Block{}, err
	}
	if err := b.validateVary(); err != nil {
		return Block{}, err
	}
	return b, nil
}

// resolveRepeat turns a block's raw --repeat string into a count, defaulting to
// 1 when unset. It is held as a raw string rather than bound as an int so that
// this check, not pflag, reports a non-integer or a value < 1 — against the
// text the user typed, and by the same grammar pacing counts use, so
// the two flags accept the same spellings of a positive integer.
func resolveRepeat(raw string, set bool) (int, error) {
	if !set {
		return 1, nil
	}
	m, ok := parsePositiveInt(raw)
	if !ok {
		return 0, fmt.Errorf("--repeat must be an integer >= 1, got %q", raw)
	}
	return m, nil
}

// resolveMethod resolves -X/--request, rejecting a blank argument as curl does.
// An unset -X means "let the body decide" (see Block.HTTPMethod), so accepting
// "" would make a blank -X a silent GET on a bodyless block -- the one blank
// argument that changes the request rather than merely emptying a value.
func resolveMethod(pre, blk *segment) (string, error) {
	m, set := fMethod.resolved(pre, blk)
	if m == "" && set {
		return "", errors.New("-X/--request needs a method, got an empty argument")
	}
	return m, nil
}

// resolveVaryMode maps the --vary-mode flag value to a VaryMode governing how a
// block's --vary bindings combine (see Fan). An unrecognised value is an error.
func resolveVaryMode(raw string) (VaryMode, error) {
	switch raw {
	case varyModeClusterbomb:
		return VaryClusterBomb, nil
	case varyModePitchfork:
		return VaryPitchfork, nil
	default:
		return 0, fmt.Errorf("--vary-mode must be clusterbomb or pitchfork, got %q", raw)
	}
}

// resolveVary parses the --vary bindings from the preamble and block, the
// block's appended to the preamble's, and folds duplicates by token name so a
// block's binding overrides an inherited one for the same NAME. Returns nil
// when neither scope carried a --vary.
func resolveVary(pre, blk *segment) ([]VarySpec, error) {
	raws := slices.Concat(fVary.value(pre), fVary.value(blk))
	if len(raws) == 0 {
		return nil, nil
	}
	byToken := map[string]int{}
	var specs []VarySpec
	for _, raw := range raws {
		spec, err := parseVarySpec(raw)
		if err != nil {
			return nil, err
		}
		if i, ok := byToken[spec.Token]; ok {
			specs[i] = spec // a later binding (a block's) overrides an inherited one
			continue
		}
		byToken[spec.Token] = len(specs)
		specs = append(specs, spec)
	}
	return specs, nil
}

// customSet is one scope's custom flags together with the segments they
// resolve across: the baseline a value is inherited from and the override that
// may replace it. A scope with no inheritance of its own passes the same
// segment twice.
type customSet struct {
	opts               []option
	baseline, override *segment
}

// customValues resolves the sets into the map a carrier hands to Flag.Get,
// each flag under its own name. Returns nil where there are no custom flags at
// all, so a carrier holding none holds no map either.
func customValues(sets ...customSet) map[string]any {
	n := 0
	for _, s := range sets {
		n += len(s.opts)
	}
	if n == 0 {
		return nil
	}
	m := make(map[string]any, n)
	for _, s := range sets {
		for _, o := range s.opts {
			m[o.name()] = carry(o, s.baseline, s.override)
		}
	}
	return m
}

func customChanges(sets ...customSet) map[string]bool {
	n := 0
	for _, s := range sets {
		n += len(s.opts)
	}
	if n == 0 {
		return nil
	}
	m := make(map[string]bool, n)
	for _, s := range sets {
		for _, o := range s.opts {
			m[o.name()] = carriedSet(o, s.baseline, s.override)
		}
	}
	return m
}

// segURLs is a segment's URLs: bare positionals followed by --url values.
func segURLs(seg *segment) []string {
	return slices.Concat(seg.positionals, fURL.value(seg))
}

// resolveURLs applies the override rule for request targeting: a block's own
// URLs (bare or --url) win; a block that names none inherits the preamble's
// shared default. Unlike content (headers, cookies, data, form) it does not
// accumulate. A block targets its own endpoints, or the default, never both.
func resolveURLs(pre, blk *segment) []string {
	if u := segURLs(blk); len(u) > 0 {
		return u
	}
	return segURLs(pre)
}

// programName is the default Parser.Name: the running binary's base name. This
// and the os.ReadFile default behind WithFileReader are the package's only
// reads of ambient state, both being what a command-line parser is expected to
// assume and both overridable — Name is a plain field, and the reader is an
// option.
func programName() string {
	if len(os.Args) > 0 && os.Args[0] != "" {
		return filepath.Base(os.Args[0])
	}
	return "command"
}
