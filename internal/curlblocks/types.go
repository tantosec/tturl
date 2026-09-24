package curlblocks

// Header is one resolved request header.
type Header struct {
	// Name is the field name as resolved from the command line.
	Name string
	// Value is the field value.
	Value string
	// origin records which command-line flag produced the header, so a --vary
	// token substitutes into the text the user typed and no further (see
	// Header.mapTyped). The zero value is headerTyped, so a Header built by hand
	// is treated as its author's own text.
	origin headerOrigin
}

// Cookie is one resolved request cookie. Cookies are sent joined as
// "name=value" pairs in the request's Cookie header.
type Cookie struct {
	// Name is the cookie name.
	Name string
	// Value is the cookie value.
	Value string
}

// Globals holds the command-wide flags from the preamble. Custom global flags
// are read from it with Flag.Get.
type Globals struct {
	// Insecure is --insecure/-k, and is false unless the Parser registered that
	// flag with WithInsecureFlag.
	Insecure bool
	custom   map[string]any
	changed  map[string]bool
}

func (g Globals) customValues() map[string]any   { return g.custom }
func (g Globals) customChanged() map[string]bool { return g.changed }
func (Globals) carrierScope() flagScope          { return scopeGlobal }

// Block is one resolved set of URLs and their shared request settings. It
// combines the inherited preamble baseline with the block's own options.
// Custom block and shared flags are read with [Flag.Get].
//
// Hand-built Blocks have no parsed body, default scheme, fan limit, or custom
// file reader; their methods still validate the exported state they use.
type Block struct {
	protocol Protocol
	// URLs are the resolved targets in send order.
	URLs []string
	// Name is the block's --name: what its request(s) ask to be called in a
	// tool's output. Empty means unnamed, so a tool draws an auto-label. See
	// NameSpecs and ResolveNames.
	Name string
	// Headers are the resolved user and default request headers.
	Headers []Header
	// SuppressedHeaders names default headers removed with "-H 'Name:'".
	SuppressedHeaders []string
	// Cookies are joined by [Block.CookieHeader].
	Cookies []Cookie
	// Method is the explicit -X value, or empty for automatic selection.
	Method string
	// The block's single request body: at most one of data/form/json is
	// non-empty, enforced at parse (resolveBody). A tool reads the body through
	// HTTPMethod and RequestBody, never these fields.
	data []dataPiece // -d/--data family
	form []formPart  // -F/--form family
	json []jsonPiece // --json; RFC 7386 merge-patch body
	// FormEscape applies --form-escape to multipart field and file names.
	FormEscape bool
	// Repeat is the number of identical request copies and is at least one in a
	// parsed Block.
	Repeat    int
	repeatSet bool
	// Vary contains the bindings that fan this block. Empty means no fan.
	Vary []VarySpec
	// VaryMode controls how multiple bindings combine. The zero value is
	// VaryClusterBomb.
	VaryMode VaryMode
	// VaryFileContent controls substitution in referenced body bytes. Nil means
	// true, including for hand-built Blocks; false preserves all file contents.
	VaryFileContent *bool
	// bodyFiles holds immutable raw or substituted expansion snapshots by path.
	bodyFiles map[string][]byte
	custom    map[string]any
	changed   map[string]bool
	// readFile loads an @file/<file the command line references (a --data @f, -F
	// @f, or --vary NAME=@f source). It is the reader configured on the Parser
	// (see WithFileReader); the parser sets it on every block it builds. See
	// reader for the fallback a hand-built block uses.
	readFile func(name string) ([]byte, error)
	// ReadStdin loads a consumer-owned invocation snapshot. Nil disables stdin.
	ReadStdin func() ([]byte, error)
	// maxFanSize caps how many requests this block's --vary fan may expand to,
	// 0 meaning no ceiling (see WithMaxFanSize). Like readFile it is the
	// Parser's, set on every block it builds; a hand-built block carries 0.
	maxFanSize int
	// defaultScheme is the scheme filled into a schemeless URL. Fan expansion
	// delays the fill until after substitution. A hand-built block uses none.
	defaultScheme string
}

func (b Block) customValues() map[string]any   { return b.custom }
func (b Block) customChanged() map[string]bool { return b.changed }
func (Block) carrierScope() flagScope          { return scopeBlock }

// RepeatIsSet reports whether --repeat was supplied explicitly for this block,
// either in the shared preamble or in the block itself.
func (b Block) RepeatIsSet() bool { return b.repeatSet }

// Plan is a parsed, validated command: the command-wide globals plus the
// resolved blocks. Expand turns the blocks into the requests they call for.
type Plan struct {
	// Globals contains command-wide preamble flags.
	Globals Globals
	// Blocks contains resolved request blocks in command-line order.
	Blocks []Block
	// implicit records that the command line opened no block, so its single
	// block is one the parser supplied rather than one the user wrote. See
	// Plan.BlockError, which must not name a block that was never typed.
	implicit bool
}

// Carrier is a parsed Globals or Block from which a custom flag's value is
// read with Flag.Get. Which of the two carries a given flag's value follows the
// scope it was registered in; Flag.Get says what happens if they are mixed up.
type Carrier interface {
	customValues() map[string]any
	customChanged() map[string]bool
	carrierScope() flagScope
}
