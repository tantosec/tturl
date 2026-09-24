package curlblocks

import (
	"bytes"
	"fmt"
	"strings"
)

// dataKind is which member of curl's --data family produced a piece. It selects
// how the piece's Spec becomes request-body bytes; see Block.RequestBody.
type dataKind int

const (
	dataASCII     dataKind = iota // -d/--data and --data-ascii: '@file' loads a file, CR/LF stripped
	dataBinary                    // --data-binary: '@file' loads a file verbatim
	dataRaw                       // --data-raw: posted as-is, a leading '@' is literal
	dataURLEncode                 // --data-urlencode: URL-encoded, with curl's name/@file forms
)

// flag returns the family member's flag name, for an error naming what the
// user typed.
func (k dataKind) flag() string {
	switch k {
	case dataBinary:
		return "--data-binary"
	case dataRaw:
		return "--data-raw"
	case dataURLEncode:
		return "--data-urlencode"
	default:
		return "--data"
	}
}

// dataPiece is one unresolved --data argument: the family member that produced
// it and the raw spec as written on the command line. Preparation loads files
// through the configured reader; Fan snapshots varied files before RequestBody
// applies data processing. Parsing stays free of I/O.
type dataPiece struct {
	Kind dataKind
	Spec string
	// reference and fieldName retain original file grammar through variation.
	reference *fileRef
	fieldName string
}

// The --data family's help text. -d/--data and --data-ascii are identical; the
// others differ only in how a piece is materialised (see Block.RequestBody).
const (
	dataUsageMain      = "add data; '@file' strips CR, LF, and NUL"
	dataUsageBinary    = "add data; '@file' preserves every byte"
	dataUsageRaw       = "add literal data without '@file' interpretation"
	dataUsageURLEncode = "URL-encode and add data"
)

// fData is the single Shared built-in standing in for the whole --data family
// (-d/--data, --data-ascii, --data-binary, --data-raw, --data-urlencode). One
// ordered accumulator behind all five names is what makes the pieces merge in
// the exact command-line order regardless of which member produced each, as
// curl does.
var fData = familyOption[dataPiece]{
	key: "data", argType: "data",
	members: []familyMember[dataPiece]{
		{
			flags: []flagEntry{
				{name: "data", shorthand: "d"},
				{name: "data-ascii"},
			},
			argument: "DATA", usage: dataUsageMain,
			completionFiles: CompletionAtFiles,
			build:           dataPieceOf(dataASCII),
		},
		{
			flags:    []flagEntry{{name: "data-binary"}},
			argument: "DATA", usage: dataUsageBinary,
			completionFiles: CompletionAtFiles,
			build:           dataPieceOf(dataBinary),
		},
		{
			flags:    []flagEntry{{name: "data-raw"}},
			argument: "DATA", usage: dataUsageRaw,
			build: dataPieceOf(dataRaw),
		},
		{
			flags:    []flagEntry{{name: "data-urlencode"}},
			argument: "DATA", usage: dataUsageURLEncode,
			completionFiles: CompletionURLencodeFiles,
			build:           dataPieceOf(dataURLEncode),
		},
	},
}

// dataPieceOf builds one family member's recorder: every occurrence of that
// flag name lands in the shared accumulator tagged with its kind.
func dataPieceOf(kind dataKind) func(string) dataPiece {
	return func(s string) dataPiece { return dataPiece{Kind: kind, Spec: s} }
}

// resolveData accumulates the preamble's --data pieces with the block's, so a
// block's data appends to the shared preamble baseline rather than replacing it
// — the same accumulation as -H headers and -b cookies (the pieces are then
// '&'-merged by dataBody). Returns nil when neither scope carried data.
func resolveData(pre, blk *segment) ([]dataPiece, error) {
	pieces := accumPieces[dataPiece](pre, blk, "data")
	for _, p := range pieces {
		ref, ok := p.fileRef()
		if !ok {
			continue
		}
		if err := ref.validate(); err != nil {
			return nil, err
		}
	}
	return pieces, nil
}

// fileRef reports the '@file' the piece will read, if any. --data-raw never
// reads one; --data-urlencode reads one only in its no-'=' form, where the
// first '@' splits the literal name from the file (see urlencodeSpec).
func (p dataPiece) fileRef() (fileRef, bool) {
	if p.reference != nil {
		return *p.reference, true
	}
	var name string
	var ok bool
	switch p.Kind {
	case dataRaw:
	case dataURLEncode:
		if at := strings.IndexByte(p.Spec, '@'); at >= 0 && strings.IndexByte(p.Spec, '=') < 0 {
			name, ok = p.Spec[at+1:], true
		}
	default:
		name, ok = atFile(p.Spec)
	}
	if !ok {
		return fileRef{}, false
	}
	return fileRef{flag: p.Kind.flag(), sigil: "@", name: name}, true
}

// dataBody materialises the block's --data pieces into request-body bytes: each
// piece is loaded and/or encoded per its kind (see dataKind) and the pieces are
// joined with '&'. readFile supplies the bytes for an '@filename' piece — the
// only I/O needed — and is called with the name exactly as written after '@';
// the block reader routes stdin through its configured loader. present reports
// the block carried any data. RequestBody is the public entry point.
func (b Block) dataBody(readFile func(name string) ([]byte, error)) (body []byte, present bool, err error) {
	if len(b.data) == 0 {
		return nil, false, nil
	}
	parts := make([][]byte, 0, len(b.data))
	for _, p := range b.data {
		part, err := p.materialise(readFile)
		if err != nil {
			return nil, false, err
		}
		parts = append(parts, part)
	}
	return bytes.Join(parts, []byte("&")), true, nil
}

func (p dataPiece) materialise(readFile func(string) ([]byte, error)) ([]byte, error) {
	switch p.Kind {
	case dataRaw:
		return []byte(p.Spec), nil
	case dataASCII, dataBinary:
		ref, ok := p.fileRef()
		if !ok {
			return []byte(p.Spec), nil
		}
		data, err := ref.read(readFile)
		if err != nil {
			return nil, err
		}
		if p.Kind == dataBinary {
			return data, nil // --data-binary keeps a file's bytes as they are
		}
		return stripDataFile(data), nil
	case dataURLEncode:
		if p.reference != nil {
			data, err := p.reference.read(readFile)
			if err != nil {
				return nil, err
			}
			return urlencodeContent(p.fieldName, data), nil
		}
		return urlencodeSpec(p.Spec, readFile)
	default:
		return nil, fmt.Errorf("curlblocks: unknown data kind %d", p.Kind)
	}
}

// atFile reports whether spec is an '@filename' reference and returns the name.
// A leading '@' selects a file for every flag that reads one this way: the
// --data family bar --data-raw, which takes its argument literally, and --json.
func atFile(spec string) (string, bool) {
	if len(spec) > 0 && spec[0] == '@' {
		return spec[1:], true
	}
	return "", false
}

// stripDataFile drops every CR, LF, and NUL, the processing curl applies to a
// file read for -d/--data/--data-ascii. --data-binary keeps all three.
func stripDataFile(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c != '\r' && c != '\n' && c != 0 {
			out = append(out, c)
		}
	}
	return out
}

// urlencodeSpec resolves one --data-urlencode argument, matching curl's forms.
// A '=' anywhere splits an optional literal name from inline content, so a '@'
// after it is literal; with no '=', the first '@' instead reads the content
// from a file, the text before it the literal name. The name and its '=' are
// emitted literally and only the content is percent-encoded; with neither '='
// nor '@' the whole argument is the content.
func urlencodeSpec(spec string, readFile func(string) ([]byte, error)) ([]byte, error) {
	eq := strings.IndexByte(spec, '=')
	at := strings.IndexByte(spec, '@')

	var name string
	var content []byte
	switch {
	case eq >= 0:
		name, content = spec[:eq], []byte(spec[eq+1:])
	case at >= 0:
		data, err := fileRef{flag: dataURLEncode.flag(), sigil: "@", name: spec[at+1:]}.read(readFile)
		if err != nil {
			return nil, err
		}
		name, content = spec[:at], data
	default:
		content = []byte(spec)
	}

	return urlencodeContent(name, content), nil
}

func urlencodeContent(name string, content []byte) []byte {
	enc := formEncode(content)
	if name == "" {
		return []byte(enc)
	}
	return []byte(name + "=" + enc)
}

// urlEncode percent-encodes per RFC 3986, leaving only the unreserved set
// (ALPHA / DIGIT / '-' / '.' / '_' / '~') unescaped. It is the encoding for a
// URL, where a space is %20; a form body wants formEncode.
func urlEncode(b []byte) string { return percentEncode(b, "%20") }

// formEncode encodes per application/x-www-form-urlencoded: as urlEncode, but a
// space becomes '+' — curl's --data-urlencode behaviour, and the encoding the
// Content-Type that body carries calls for.
func formEncode(b []byte) string { return percentEncode(b, "+") }

// percentEncode percent-encodes every byte outside RFC 3986's unreserved set,
// emitting a space as the given text. This is where the URL and form encodings
// differ.
func percentEncode(b []byte, space string) string {
	const hex = "0123456789ABCDEF"
	var sb strings.Builder
	sb.Grow(3 * len(b))
	for _, c := range b {
		switch {
		case unreserved(c):
			sb.WriteByte(c)
		case c == ' ':
			sb.WriteString(space)
		default:
			sb.WriteByte('%')
			sb.WriteByte(hex[c>>4])
			sb.WriteByte(hex[c&0x0f])
		}
	}
	return sb.String()
}

func unreserved(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	case c == '-', c == '.', c == '_', c == '~':
		return true
	default:
		return false
	}
}
