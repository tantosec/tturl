package curlblocks

import (
	"bytes"
	"fmt"
	"mime"
	"mime/multipart"
	"net/textproto"
	"path/filepath"
	"strings"
)

// formKind is how a multipart form part's content is sourced, set when a
// -F/--form or --form-string spec is parsed.
type formKind int

const (
	formLiteral  formKind = iota // content is the literal Value
	formFile                     // "@file": upload Value as a file (filename + content type)
	formFileText                 // "<file": a text field whose value is read from file Value
)

// formPart is one resolved multipart form field. Value holds the literal
// content for formLiteral, or the file path for formFile/formFileText. FileName
// (formFile only) is the Content-Disposition filename, defaulting to the base
// of Value unless a ";filename=" overrode it. Type is an explicit ";type="
// content type, or "" to let formFile guess from the file extension.
type formPart struct {
	Name     string
	Kind     formKind
	Value    string
	FileName string
	Type     string
}

// The form family's help text. -F/--form parses curl's content forms;
// --form-string takes the value literally.
const (
	formUsageMain   = "add a multipart field; '@file' uploads and '<file' reads a field value"
	formUsageString = "add a literal multipart field"
)

// fForm is the single Shared built-in for the multipart form family, -F/--form
// and --form-string. One ordered accumulator behind both names keeps the parts
// in command-line order however the two are interleaved.
var fForm = familyOption[formSpec]{
	key: "form", argType: "form",
	members: []familyMember[formSpec]{
		{
			flags:    []flagEntry{{name: "form", shorthand: "F"}},
			argument: "NAME=CONTENT", usage: formUsageMain,
			completionFiles: CompletionFormFiles,
			build:           formSpecOf(false),
		},
		{
			flags:    []flagEntry{{name: "form-string"}},
			argument: "NAME=VALUE", usage: formUsageString,
			build: formSpecOf(true),
		},
	},
}

// formSpecOf builds one family member's recorder: --form-string's specs are
// marked literal, so parseFormSpec takes their content verbatim.
func formSpecOf(literal bool) func(string) formSpec {
	return func(s string) formSpec { return formSpec{literal: literal, spec: s} }
}

// formSpec is one raw form argument: the spec string and whether it came from
// --form-string (literal, no special-character parsing).
type formSpec struct {
	literal bool
	spec    string
}

// resolveForm accumulates the preamble's form specs with the block's (a block
// appends to the shared preamble baseline, like -H/-b/-d) and parses them into
// form parts in order. Returns nil when neither scope carried a form. Malformed
// specs are reported here, at parse time; file reading is deferred to formBody.
func resolveForm(pre, blk *segment) ([]formPart, error) {
	specs := accumPieces[formSpec](pre, blk, "form")
	if len(specs) == 0 {
		return nil, nil
	}
	parts := make([]formPart, 0, len(specs))
	for _, s := range specs {
		p, err := parseFormSpec(s)
		if err != nil {
			return nil, err
		}
		if ref, ok := p.fileRef(); ok {
			if err := ref.validate(); err != nil {
				return nil, err
			}
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// fileRef reports the file the part reads, if it reads one, in the spelling
// the user wrote it: '@' for an upload, '<' for a field value taken from a
// file. A literal part reads none.
func (p formPart) fileRef() (fileRef, bool) {
	switch p.Kind {
	case formFile:
		return fileRef{flag: "-F", sigil: "@", name: p.Value}, true
	case formFileText:
		return fileRef{flag: "-F", sigil: "<", name: p.Value}, true
	default:
		return fileRef{}, false
	}
}

// parseFormSpec parses one "name=content" spec. For --form-string the content
// is taken verbatim. For -F the content may be "@file" (upload), "<file" (field
// value from a file), or a literal, optionally followed by ";type=" and (for an
// upload) ";filename=" sub-options. curl's extras are intentionally
// unsupported, and each is rejected rather than silently dropped: a multi-file
// "@one,two" upload (a comma is special only in an "@file" content),
// ";headers=" and ";encoder=". Nor is curl's double-quoting honoured: a quote
// is an ordinary character, so a ";" always separates sub-options and a quoted
// filename keeps its quotes.
func parseFormSpec(s formSpec) (formPart, error) {
	eq := strings.IndexByte(s.spec, '=')
	if eq < 0 {
		return formPart{}, fmt.Errorf("form %q must be in 'name=content' form", s.spec)
	}
	name := s.spec[:eq]
	if name == "" {
		return formPart{}, fmt.Errorf("form %q has an empty name", s.spec)
	}
	rhs := s.spec[eq+1:]

	if s.literal {
		return formPart{Name: name, Kind: formLiteral, Value: rhs}, nil
	}

	segs := strings.Split(rhs, ";")
	part := formPart{Name: name}
	switch content := segs[0]; {
	case strings.HasPrefix(content, "@"):
		part.Kind, part.Value = formFile, content[1:]
	case strings.HasPrefix(content, "<"):
		part.Kind, part.Value = formFileText, content[1:]
	default:
		part.Kind, part.Value = formLiteral, content
	}
	if part.Kind == formFile && strings.Contains(part.Value, ",") {
		return formPart{}, fmt.Errorf("form %q: curl's multi-file '@one,two' upload is unsupported "+
			"(use one -F per file); a comma in a filename cannot be expressed", s.spec)
	}

	for _, opt := range segs[1:] {
		if opt = strings.TrimSpace(opt); opt == "" {
			continue
		}
		key, val, ok := strings.Cut(opt, "=")
		if !ok {
			return formPart{}, fmt.Errorf("form %q has a malformed option %q (want 'key=value')", s.spec, opt)
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "type":
			part.Type = strings.TrimSpace(val)
		case "filename":
			if part.Kind != formFile {
				return formPart{}, fmt.Errorf("form %q: ';filename=' applies only to an '@file' upload", s.spec)
			}
			part.FileName = strings.TrimSpace(val)
		default:
			return formPart{}, fmt.Errorf("form %q has an unsupported option %q (only 'type' and 'filename' are "+
				"supported)", s.spec, key)
		}
	}
	return part, nil
}

// formBody encodes the block's form parts into a multipart/form-data body and
// returns it together with the Content-Type (including the boundary) the
// request must carry. readFile loads an '@file' upload or '<file' field — the
// only I/O needed, including configured stdin references. boundary fixes
// the multipart boundary (use "" to generate a random one). escape selects how
// field and file names are escaped: false percent-encodes (curl's default),
// true backslash-escapes (curl's --form-escape). RequestBody is the public
// entry point.
func (b Block) formBody(
	readFile func(name string) ([]byte, error),
	boundary string,
	escape bool,
) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if boundary != "" {
		if err := w.SetBoundary(boundary); err != nil {
			return nil, "", fmt.Errorf("curlblocks: invalid multipart boundary %q: %w", boundary, err)
		}
	}

	for _, p := range b.form {
		content, err := p.content(readFile)
		if err != nil {
			return nil, "", err
		}
		hdr := textproto.MIMEHeader{}
		disp := `form-data; name="` + formEscapeName(p.Name, escape) + `"`
		if p.Kind == formFile {
			name := p.FileName
			if name == "" {
				name = filepath.Base(p.Value)
			}
			disp += `; filename="` + formEscapeName(name, escape) + `"`
		}
		hdr.Set("Content-Disposition", disp)
		if ct := p.contentType(); ct != "" {
			hdr.Set("Content-Type", ct)
		}
		part, err := w.CreatePart(hdr)
		if err != nil {
			return nil, "", fmt.Errorf("curlblocks: multipart part %q: %w", p.Name, err)
		}
		if _, err := part.Write(content); err != nil {
			return nil, "", fmt.Errorf("curlblocks: multipart part %q: %w", p.Name, err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("curlblocks: closing multipart body: %w", err)
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// content returns the part's body bytes: the literal value, or the file
// contents for an '@file'/'<file' part, read via the configured reader.
func (p formPart) content(readFile func(string) ([]byte, error)) ([]byte, error) {
	ref, ok := p.fileRef()
	if !ok {
		return []byte(p.Value), nil
	}
	return ref.read(readFile)
}

// contentType is the part's Content-Type: an explicit ";type=" wins; otherwise
// a file upload is guessed from its extension (octet-stream when unknown) and a
// text field carries none.
func (p formPart) contentType() string {
	if p.Type != "" {
		return p.Type
	}
	if p.Kind != formFile {
		return ""
	}
	if ct := mime.TypeByExtension(filepath.Ext(p.Value)); ct != "" {
		// curl reports a bare media type; drop any "; charset=..." parameter.
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = strings.TrimSpace(ct[:i])
		}
		return ct
	}
	return "application/octet-stream"
}

// formEscapeName escapes a field or file name for a Content-Disposition
// parameter. backslash selects curl's --form-escape mode (backslash-escape '\'
// and '"'); otherwise curl's default percent-encodes '"', CR and LF.
func formEscapeName(s string, backslash bool) string {
	if backslash {
		return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
	}
	return strings.NewReplacer(`"`, "%22", "\r", "%0D", "\n", "%0A").Replace(s)
}
