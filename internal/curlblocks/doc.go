// Package curlblocks parses curl-like request arguments into validated request
// plans. It groups URLs into blocks that share request options, supports
// inherited preamble options, and expands parameter variations without sending
// requests.
//
// # Blocks and plans
//
// Arguments before the first --block form the preamble. Global flags apply to
// the command, while request flags establish a baseline inherited by every
// block. A block overrides target settings such as URL, method, repeat count,
// and name; content such as headers, cookies, and body pieces accumulates.
// Repeating --vary for the same token replaces the earlier binding. Explicit
// deletion flags remove inherited headers or bodies.
//
// [Parser.Parse] validates everything that does not require file I/O and
// returns a [Plan]. [Plan.Expand] is the authoritative expansion: it returns
// one [RequestGroup] per URL or fan variant and pairs every request copy with
// its globally unique [Label]. Build a group's request with
// [Block.NewRequest]:
//
//	groups, err := plan.Expand()
//	for _, group := range groups {
//		req, err := group.Block.NewRequest(ctx, group.URL)
//		if err != nil {
//			return plan.BlockError(group.BlockIndex, err)
//		}
//		// Send req once for each entry in group.Labels.
//	}
//
// File-backed variation sources and varied body snapshots are read by
// [Block.Fan]. Other bodies are read by [Block.RequestBody] or
// [Block.NewRequest]. These methods use the configured reader and repeat
// validation for hand-built blocks.
//
// Exact '-' filenames use [WithStdinReader] when explicitly configured;
// otherwise Parse rejects them. The consumer owns the invocation snapshot,
// cancellation and stream lifetime. Ordinary paths, including './-', still use
// [WithFileReader]. Literal --data-raw and --form-string inputs do not read.
//
// # Request construction
//
// curlblocks follows curl's method, authentication, header, cookie, and body
// precedence where those concepts fit this grammar. Content flags accumulate;
// JSON pieces compose by RFC 7386 merge patch. Header-family flags resolve in
// command-line order, while an explicit -H overrides convenience flags and
// built-in defaults. [Block.DefaultHeaderNames] distinguishes derived defaults
// from user content.
//
// Schemeless URLs receive "http" by default; [WithDefaultScheme] changes or
// disables that fill. A varied URL remains a template until [Block.Fan]
// substitutes it, allowing a token to provide the scheme or entire URL.
//
// Optional ProtocolFlags registers shared '--http2' and '--http1.1' switches.
// Each block inherits the preamble selection unless it selects another version.
// Same-scope selections conflict; the default is HTTP/2. Materialisation
// preserves headers for the consumer's protocol validation and framing.
//
// Optional PacingFlags registers a caller-named connection flag and batch
// and request ceilings. Rates accept a positive N/UNIT (s, m, h or d) or
// unlimited; omitted and unlimited rates resolve to zero. Consumers own rate
// composition and scheduling.
//
// The package deliberately omits transfer behaviour: it does not dial, pace, or
// send. It also omits curl features that do not fit independent request
// construction, including redirects, compression, URL globbing,
// cookie jars, and challenge-response authentication.
//
// # Variation and names
//
// --vary NAME[:ENC...]=SOURCE substitutes values into the request. A source is
// a comma-separated brace list, an ascending numeric or alphabetic range, or a
// newline-separated @file. Encoders apply left to right. Multiple bindings use
// a Cartesian product in clusterbomb mode or a shortest-input lockstep zip in
// pitchfork mode. [WithMaxFanSize] bounds expansion.
//
// Referenced body bytes vary by default, before data processing, URL encoding,
// JSON validation/merging or multipart framing. --vary-file-content=false
// preserves all referenced bodies in the resolved block; inline fields remain
// eligible. Blocks inherit the policy independently and --reset-body does not
// reset it. Referenced filesystem paths and derived filenames stay literal;
// explicit multipart filename/type metadata remains eligible. Source paths and
// values are never templates. Substitution is simultaneous without rescanning.
// Each distinct body path is loaded once per block expansion into an immutable
// raw snapshot shared by occurrence checks and every variant. Stdin continues
// to use the consumer's invocation snapshot. Tokens found only in body files
// are checked after loading; Parse remains free of I/O.
//
// Tokens must not overlap by prefix. [Fan] retains display-safe source values
// before encoding so labels describe the user's inputs rather than wire text.
//
// [Label.Display], [Label.Pins], and fan metadata are printable ASCII.
// Arbitrary bytes use the $HEX[...] display form. [DisplayText] applies the
// same rendering to consumer-owned text. Rendering is one-way and not
// injective, so [ResolveNames] rejects label collisions.
//
// # Custom flags and usage
//
// A [Parser] exposes Global, Block, and Shared registries. Their typed
// registration methods return [Flag] handles whose values are read from the
// matching [Globals] or [Block]. Reading from the wrong carrier panics. Parser
// construction options add only facilities the consumer can honour, such as
// pacing, insecure TLS, file reading, and version reporting.
//
// [Parser.CompletionOptions] exposes the registered grammar for shell
// completion. [Flag.CompletionValues] and [Flag.CompletionFiles] add completion
// metadata without changing parsing or file access.
//
// [Parser.Usage] renders the registered scopes, sections, examples, and
// consumer-supplied help routes. Command routing, long-form documentation, and
// output policy belong to the consumer.
package curlblocks
