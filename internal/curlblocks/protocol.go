package curlblocks

import "fmt"

// Protocol identifies the explicitly selected HTTP version. HTTP/2 is the
// default when neither protocol switch selects a version.
type Protocol uint8

const (
	// HTTP2 selects HTTP/2.
	HTTP2 Protocol = iota
	// HTTP11 selects HTTP/1.1.
	HTTP11
)

// ProtocolFlags is the opt-in shared protocol grammar. It resolves selections
// within each scope before applying block inheritance.
type ProtocolFlags struct {
	http2  *Flag[bool]
	http11 *Flag[bool]
}

// ProtocolFlags registers '--http2' and '--http1.1' on a shared registry.
// A block selection overrides the preamble without affecting other blocks.
// Selecting both versions in one scope is an argument error. Explicit false
// disables that switch within its scope; it does not select another version.
// Registration does not add transport or scheme validation.
func (p *Parser) ProtocolFlags(r *Registry) *ProtocolFlags {
	if r == nil || r.shared != p.Shared.shared {
		panic("curlblocks: ProtocolFlags needs this parser's shared registry")
	}
	if p.protocol != nil {
		panic("curlblocks: ProtocolFlags already registered")
	}
	p.protocol = &ProtocolFlags{
		http2: r.Bool("http2", "", false,
			"select HTTP/2; default; preamble or block scope").HelpDefault("HTTP/2"),
		http11: r.Bool("http1.1", "", false,
			"select HTTP/1.1; preamble or block scope"),
	}
	return p.protocol
}

// Get returns a block's resolved version. Materialisation retains ordinary
// HTTP headers; the consumer applies the selected protocol's framing rules.
func (*ProtocolFlags) Get(b Block) Protocol { return b.protocol }

func (pf *ProtocolFlags) selection(seg *segment) (*Protocol, error) {
	if pf == nil {
		return nil, nil
	}
	h2 := pf.http2.set(seg) && pf.http2.value(seg)
	h1 := pf.http11.set(seg) && pf.http11.value(seg)
	if h2 && h1 {
		return nil, fmt.Errorf("--http2 and --http1.1 cannot both select a protocol in one scope")
	}
	if h2 {
		protocol := HTTP2
		return &protocol, nil
	}
	if h1 {
		protocol := HTTP11
		return &protocol, nil
	}
	return nil, nil
}

func (pf *ProtocolFlags) resolve(pre, blk *segment) (Protocol, error) {
	selected, err := pf.selection(blk)
	if err != nil {
		return HTTP2, err
	}
	if selected == nil {
		selected, err = pf.selection(pre)
	}
	if selected == nil {
		return HTTP2, err
	}
	return *selected, err
}
