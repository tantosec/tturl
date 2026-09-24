package curlblocks

import (
	"bytes"
	"fmt"
	"strings"
	"unicode"
)

// VarySpec is one parsed --vary binding: the token to substitute (the NAME in
// NAME=SOURCE), with the encoder chain and value source held unexported. A
// consumer reads Token to report what a block varies over; the values
// themselves arrive through [Block.Fan], which owns the file reader they need.
type VarySpec struct {
	Token    string
	encoders []encoder
	source   varySource
}

// VaryValue is one enumerated value of a binding.
type VaryValue struct {
	// Substituted is the encoded value placed in the request.
	Substituted string
	// Display is the pre-encoder, printable value used in labels.
	Display string
}

// parseVarySpec parses one --vary argument, "NAME[:enc...]=SOURCE". The first
// '=' splits the head (NAME and any encoders) from the source, so a source may
// itself contain '=' (a filename, a list item); ':' then splits the head into
// the token name and its encoder chain.
func parseVarySpec(arg string) (VarySpec, error) {
	head, src, ok := strings.Cut(arg, "=")
	if !ok {
		return VarySpec{}, fmt.Errorf("--vary %q must be in NAME=SOURCE form", arg)
	}
	token, encNames, _ := strings.Cut(head, ":")
	if err := validateVaryToken(token); err != nil {
		return VarySpec{}, err
	}
	encoders, err := parseEncoders(encNames)
	if err != nil {
		return VarySpec{}, fmt.Errorf("--vary %s: %w", displayValue(token), err)
	}
	source, err := parseVarySource(src)
	if err != nil {
		return VarySpec{}, fmt.Errorf("--vary %s: %w", displayValue(token), err)
	}
	spec := VarySpec{Token: token, encoders: encoders, source: source}
	// A '@file' source's reference is checked here rather than inside
	// parseVarySource, which does not know the token: the message already names
	// the binding, so it needs no wrapping of its own.
	if ref, ok := source.fileRef(spec.subject()); ok {
		if err := ref.validate(); err != nil {
			return VarySpec{}, err
		}
	}
	return spec, nil
}

// validateVaryToken rejects a token that could not be typed unambiguously: it
// must be non-empty and free of whitespace (':' and '=' cannot reach here, the
// spec having split on them).
func validateVaryToken(token string) error {
	if token == "" {
		return fmt.Errorf("--vary: empty token name (want NAME=SOURCE)")
	}
	if strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return fmt.Errorf("--vary token %s must not contain whitespace", displayValue(token))
	}
	return nil
}

// subject names the binding the way the user typed it, for a message about it.
func (v VarySpec) subject() string { return "--vary " + displayValue(v.Token) }

// values enumerates the binding's values, reading a '@file' source via readFile
// (as the body flags do). It errors if the source is empty. Each value's
// Substituted form has the encoder chain applied; its Display holds the
// pre-encoder form.
func (v VarySpec) values(readFile func(string) ([]byte, error)) ([]VaryValue, error) {
	raws, err := v.source.values(v.subject(), readFile)
	if err != nil {
		return nil, err
	}
	if len(raws) == 0 {
		return nil, fmt.Errorf("%s: source produced no values", v.subject())
	}
	out := make([]VaryValue, len(raws))
	for i, raw := range raws {
		out[i] = VaryValue{Substituted: applyEncoders(v.encoders, raw), Display: displayValue(raw)}
	}
	return out, nil
}

// VaryMode selects how a block's --vary bindings combine when it carries more
// than one. With a single binding the mode is immaterial.
type VaryMode int

const (
	varyModeClusterbomb = "clusterbomb"
	varyModePitchfork   = "pitchfork"
)

var varyModeNames = []string{varyModeClusterbomb, varyModePitchfork}

const (
	// VaryClusterBomb pairs every value of each binding with every value of the
	// others: the Cartesian product, so the request count is the product of the
	// bindings' sizes. It is the default and the choice when the bindings are
	// independent.
	VaryClusterBomb VaryMode = iota
	// VaryPitchfork advances the bindings in lockstep: request k takes value k
	// from each binding, so the request count is the shortest binding's size and
	// longer bindings are truncated to it. It is the choice when the bindings
	// are correlated position by position (a value and its matching token).
	VaryPitchfork
)

// Variant is one request produced by fanning a block over --vary bindings.
type Variant struct {
	// Block is the substituted request block with Vary cleared.
	Block Block
	// Values are aligned with Fan.Tokens and are safe to display.
	Values []string
}

// Fan describes a block expanded over its --vary bindings. Tokens and Source
// are printable display text. Variants are in expansion order, with the last
// token varying fastest in clusterbomb mode.
type Fan struct {
	// Tokens are the display-safe binding names in declaration order.
	Tokens []string
	// Source is a short display-safe description of the combined sources.
	Source string
	// Template is the block's URL with its tokens still in place and a missing
	// scheme filled, for a summary that shows where the values land. It is the
	// display form of what Block.URLs holds as written. Where a token supplies
	// the scheme itself, the fill is cosmetic.
	Template string
	// Variants are in expansion order.
	Variants []Variant
	// Ragged reports that pitchfork mode truncated unequal binding lengths.
	Ragged bool
}

// Fan expands the block's --vary bindings. It substitutes all tokens
// simultaneously into user-authored request text, without rescanning encoded
// values or derived text. Sources and body snapshots use the configured reader.
//
// Clusterbomb mode produces the Cartesian product; pitchfork mode zips to the
// shortest source and reports unequal lengths through [Fan.Ragged]. Fan
// validates hand-built Blocks and enforces the configured fan-size ceiling
// before constructing rejected variants. With no bindings it returns nil, nil.
func (b Block) Fan() (*Fan, error) {
	if len(b.Vary) == 0 {
		return nil, nil
	}
	if err := b.validateVary(); err != nil {
		return nil, err
	}
	if err := b.validateFileRefs(); err != nil {
		return nil, err
	}
	// Snapshot body paths before occurrence validation or variant construction.
	// Clone immediately: configured readers may reuse their own buffers.
	readFile := b.reader()
	snapshots := make(map[string][]byte)
	for _, ref := range b.bodyFileRefs() {
		if _, loaded := snapshots[ref.name]; loaded {
			continue
		}
		data, err := ref.read(readFile)
		if err != nil {
			return nil, err
		}
		snapshots[ref.name] = bytes.Clone(data)
	}
	b.bodyFiles = snapshots
	for _, spec := range b.Vary {
		if !b.mentions(spec.Token) {
			return nil, unusedVaryToken(spec.Token)
		}
	}
	readFile = b.reader()
	tokens := varyTokens(b.Vary)
	perToken := make([][]VaryValue, len(b.Vary))
	for i, spec := range b.Vary {
		vals, err := spec.values(readFile)
		if err != nil {
			return nil, err
		}
		if err := b.checkFanSize(spec.Token, len(vals)); err != nil {
			return nil, err
		}
		perToken[i] = vals
	}

	var tuples [][]VaryValue
	var ragged bool
	switch b.VaryMode {
	case VaryPitchfork:
		// A zip is as long as its shortest source, already checked above.
		tuples, ragged = zipValues(perToken)
	default:
		counts := make([]int, len(perToken))
		for i, vals := range perToken {
			counts[i] = len(vals)
		}
		if err := b.checkFanProduct(counts); err != nil {
			return nil, err
		}
		tuples = cartesianValues(perToken)
	}

	variants := make([]Variant, len(tuples))
	for i, tuple := range tuples {
		reps := make([]string, 0, 2*len(tokens))
		display := make([]string, len(tokens))
		for t, v := range tuple {
			reps = append(reps, tokens[t], v.Substituted)
			display[t] = v.Display
		}
		variants[i] = Variant{Block: b.substitute(reps), Values: display}
		if err := checkVariantURL(variants[i].Block.URLs[0], tokens, display); err != nil {
			return nil, err
		}
	}
	return &Fan{
		Tokens:   displayStrings(tokens),
		Source:   combinedSource(b.Vary, perToken, b.VaryMode),
		Template: ensureScheme(b.URLs[0], b.defaultScheme),
		Variants: variants,
		Ragged:   ragged,
	}, nil
}

// validateVary checks every rule about a block's --vary bindings that needs no
// I/O: that the block carries the single URL a fan substitutes into, that no
// token is a prefix of another (which would shadow it under simultaneous
// substitution), and that statically known source sizes fit the ceiling.
// Parse runs it so a resolved
// Block is validated whether or not the consumer goes on to call Fan; Fan runs
// it too, since a Block may be built by hand. Token occurrence is checked here
// unless enabled body files could supply it; Fan checks again after loading.
//
// Source-file value counts are checked after loading.
func (b Block) validateVary() error {
	if len(b.Vary) == 0 {
		return nil
	}
	if len(b.URLs) != 1 {
		return fmt.Errorf("--vary needs a single URL per block, got %d", len(b.URLs))
	}
	if err := overlappingToken(varyTokens(b.Vary)); err != nil {
		return err
	}
	canDefer := b.varyBodyFiles() && len(b.bodyFileRefs()) > 0
	static := make([]int, len(b.Vary))
	allStatic := true
	for i, spec := range b.Vary {
		if !b.mentions(spec.Token) && !canDefer {
			return unusedVaryToken(spec.Token)
		}
		n, ok := spec.source.staticSize()
		if !ok {
			allStatic = false
			continue
		}
		if err := b.checkFanSize(spec.Token, n); err != nil {
			return err
		}
		static[i] = n
	}
	// Every size known means the product is too, so a clusterbomb combination is
	// settled here as well; one file among the sources defers it to Fan.
	if allStatic && b.VaryMode != VaryPitchfork {
		return b.checkFanProduct(static)
	}
	return nil
}

// overlappingToken reports an error when one --vary token is a prefix of
// another. Substitution applies all tokens in one strings.Replacer pass, which
// resolves two keys that match at the same position by registration order, not
// by length; a shorter token would then silently shadow the longer wherever the
// longer appears. Rejecting the overlap keeps substitution unambiguous; rename
// one token to a distinct, non-prefixing spelling.
//
// Two bindings on the same token are the degenerate overlap, reported as the
// duplicate it is. The parser cannot produce one (resolveVary folds bindings by
// token, so a repeated --vary keeps only the last), but a hand-built Block can.
func overlappingToken(tokens []string) error {
	for i, short := range tokens {
		for j, long := range tokens {
			switch {
			case i == j:
			case short == long:
				return fmt.Errorf("--vary token %s is bound twice; give each token one source",
					displayValue(short))
			case strings.HasPrefix(long, short):
				s, l := displayValue(short), displayValue(long)
				return fmt.Errorf("--vary tokens %s and %s overlap: %s is a prefix of %s and "+
					"would shadow it; rename one to a distinct spelling", s, l, s, l)
			}
		}
	}
	return nil
}

// cartesianValues returns every combination of one value from each binding, in
// odometer order: the last binding varies fastest. An empty input yields one
// empty tuple, so a caller always gets at least the no-token case.
func cartesianValues(perToken [][]VaryValue) [][]VaryValue {
	tuples := [][]VaryValue{{}}
	for _, vals := range perToken {
		next := make([][]VaryValue, 0, len(tuples)*len(vals))
		for _, prefix := range tuples {
			for _, v := range vals {
				tuple := make([]VaryValue, len(prefix)+1)
				copy(tuple, prefix)
				tuple[len(prefix)] = v
				next = append(next, tuple)
			}
		}
		tuples = next
	}
	return tuples
}

// zipValues pairs the bindings position by position, truncating to the
// shortest, and reports whether any binding was longer (so a caller can note
// the truncation).
func zipValues(perToken [][]VaryValue) (tuples [][]VaryValue, ragged bool) {
	n := len(perToken[0])
	for _, vals := range perToken {
		if len(vals) < n {
			n = len(vals)
		}
	}
	for _, vals := range perToken {
		if len(vals) != n {
			ragged = true
		}
	}
	tuples = make([][]VaryValue, n)
	for k := range tuples {
		tuple := make([]VaryValue, len(perToken))
		for t := range perToken {
			tuple[t] = perToken[t][k]
		}
		tuples[k] = tuple
	}
	return tuples, ragged
}

// combinedSource renders the summary heading for the bindings' value source. A
// single binding keeps its bare description (e.g. "1..3"); several are each
// prefixed with their token and joined by the mode's operator, " x " for the
// product and " + " for the zip (e.g. "FOO=1..3 x BAR={x,y}").
func combinedSource(specs []VarySpec, perToken [][]VaryValue, mode VaryMode) string {
	if len(specs) == 1 {
		return specs[0].source.describe(perToken[0])
	}
	sep := " x "
	if mode == VaryPitchfork {
		sep = " + "
	}
	parts := make([]string, len(specs))
	for i, s := range specs {
		parts[i] = displayValue(s.Token) + "=" + s.source.describe(perToken[i])
	}
	return strings.Join(parts, sep)
}

// checkFanSize rejects a single binding that alone would carry the fan past the
// block's ceiling (see WithMaxFanSize). A block with no ceiling takes any size.
func (b Block) checkFanSize(token string, n int) error {
	if b.maxFanSize > 0 && n > b.maxFanSize {
		return fmt.Errorf("--vary %s: %d values exceeds the maximum fan size of %d",
			displayValue(token), n, b.maxFanSize)
	}
	return nil
}

// checkFanProduct rejects a combination whose Cartesian product would carry the
// fan past the block's ceiling (see WithMaxFanSize), naming each binding's size
// so the arithmetic is visible. It runs before the product is built, and
// multiplies by division so no accumulated total can overflow.
func (b Block) checkFanProduct(counts []int) error {
	if b.maxFanSize <= 0 {
		return nil
	}
	if _, ok := fanProduct(counts, b.maxFanSize); ok {
		return nil
	}
	sizes := make([]string, len(counts))
	for i, n := range counts {
		sizes[i] = fmt.Sprintf("%s=%d", displayValue(b.Vary[i].Token), n)
	}
	return fmt.Errorf("--vary %s: combining %s exceeds the maximum fan size of %d; "+
		"narrow a source, or combine in lockstep with --vary-mode pitchfork",
		strings.Join(displayStrings(varyTokens(b.Vary)), " x "), strings.Join(sizes, " x "), b.maxFanSize)
}

// fanProduct multiplies counts, reporting whether the product stays within max.
// It compares by division rather than multiplying, so several large sources
// cannot overflow the running total and wrap into an acceptable-looking one; it
// stops at the first count that puts the product over. A count below 1 is
// reported as over rather than divided by, since a fan of nothing is not a fan
// (VarySpec.Values already rejects a source that produced none).
func fanProduct(counts []int, max int) (total int, ok bool) {
	total = 1
	for _, n := range counts {
		if n < 1 || total > max/n {
			return 0, false
		}
		total *= n
	}
	return total, true
}

// varyTokens is the bindings' token names, in binding order.
func varyTokens(specs []VarySpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Token
	}
	return out
}

// mapStrings returns a copy of the block with f applied to every field a --vary
// token may occupy: the URL, each cookie name and value, each suppressed header
// name, the method, each inline body spec (--data, --json, and every -F form
// field, excluding filesystem paths), and each header's own typed text (see
// Header.mapTyped: a derived header exposes only the part the user wrote).
// Occurrence checks and substitution share this enumeration. Rebuilt slices
// are freshly allocated, so the copy never aliases the original. A fanned
// block has exactly one URL; the read-only custom-flag map is shared.
func (b Block) mapStrings(f func(string) string) Block {
	out := b
	out.URLs = []string{f(b.URLs[0])}
	if len(b.Headers) > 0 {
		out.Headers = make([]Header, len(b.Headers))
		for i, h := range b.Headers {
			out.Headers[i] = h.mapTyped(f)
		}
	}
	// A suppressed name is a name the user typed ("-H 'X-FUZZ:'"), so it varies
	// with the -H headers it must keep matching.
	if len(b.SuppressedHeaders) > 0 {
		out.SuppressedHeaders = make([]string, len(b.SuppressedHeaders))
		for i, name := range b.SuppressedHeaders {
			out.SuppressedHeaders[i] = f(name)
		}
	}
	if len(b.Cookies) > 0 {
		out.Cookies = make([]Cookie, len(b.Cookies))
		for i, c := range b.Cookies {
			out.Cookies[i] = Cookie{Name: f(c.Name), Value: f(c.Value)}
		}
	}
	out.Method = f(b.Method)
	if len(b.data) > 0 {
		out.data = make([]dataPiece, len(b.data))
		for i, d := range b.data {
			out.data[i] = d
			if ref, ok := d.fileRef(); ok {
				if d.Kind == dataURLEncode {
					at := strings.IndexByte(d.Spec, '@')
					out.data[i].reference = &ref
					out.data[i].fieldName = f(d.Spec[:at])
					out.data[i].Spec = out.data[i].fieldName + "@" + ref.name
				}
			} else {
				out.data[i].Spec = f(d.Spec)
			}
		}
	}
	if len(b.json) > 0 {
		out.json = make([]jsonPiece, len(b.json))
		for i, j := range b.json {
			out.json[i] = j
			if !j.IsFile {
				out.json[i].Spec = f(j.Spec)
			}
		}
	}
	if len(b.form) > 0 {
		out.form = make([]formPart, len(b.form))
		for i, p := range b.form {
			out.form[i] = p
			out.form[i].Name = f(p.Name)
			out.form[i].FileName = f(p.FileName)
			out.form[i].Type = f(p.Type)
			if p.Kind == formLiteral {
				out.form[i].Value = f(p.Value)
			}
		}
	}
	return out
}

// mentions reports whether token occurs in any field Fan substitutes (see
// mapStrings for that field set). A block naming a --vary token that appears
// nowhere is a likely typo, which Fan rejects.
func (b Block) mentions(token string) bool {
	found := false
	b.mapStrings(func(s string) string {
		if strings.Contains(s, token) {
			found = true
		}
		return s
	})
	if b.varyBodyFiles() {
		for _, raw := range b.bodyFiles {
			if bytes.Contains(raw, []byte(token)) {
				found = true
			}
		}
	}
	return found
}

func (b Block) varyBodyFiles() bool {
	return b.VaryFileContent == nil || *b.VaryFileContent
}

func unusedVaryToken(token string) error {
	t := displayValue(token)
	return fmt.Errorf("--vary %s: the token %s appears nowhere in the request", t, t)
}

// checkVariantURL refuses a variant left with nothing after its scheme, which
// is what an empty value produces for a token standing for the whole URL or its
// host: "http://". The alternative is one dead request per such value, failing
// in the consumer far from the value that caused it. The check is deliberately
// narrow. Whether anything more elaborate is a URL is the consumer's business,
// and a scheme-less template is legitimate when a Parser fills no scheme.
func checkVariantURL(url string, tokens, values []string) error {
	rest := url
	if _, after, ok := strings.Cut(url, "://"); ok {
		rest = after
	}
	if rest == "" {
		return fmt.Errorf("--vary %s substitutes to %q, which requests nothing",
			joinPairs(displayStrings(tokens), values), url)
	}
	return nil
}

// substitute returns a copy of the block with the token->value replacements in
// reps (a flat [tok1, val1, tok2, val2, ...] slice) applied throughout its
// substitutable fields (see mapStrings), its single URL kept, and its Vary
// cleared so the variant is not re-fanned. A strings.Replacer applies every
// token in one pass, so a value substituted for one token is never rescanned as
// another.
//
// The substituted URL is a URL only now that its tokens carry values, so this
// is where the scheme fill the parser deferred happens.
func (b Block) substitute(reps []string) Block {
	replace := strings.NewReplacer(reps...).Replace
	out := b.mapStrings(replace)
	if b.varyBodyFiles() && len(b.bodyFiles) > 0 {
		out.bodyFiles = make(map[string][]byte, len(b.bodyFiles))
		for path, raw := range b.bodyFiles {
			out.bodyFiles[path] = []byte(replace(string(raw)))
		}
	}
	out.Vary = nil
	out.URLs[0] = ensureScheme(out.URLs[0], b.defaultScheme)
	return out
}
