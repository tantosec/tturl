package curlblocks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// jsonUsage is --json's help text. Terse, but it names the two behaviours a
// reader cannot guess: repeats/blocks merge (rather than concatenate as --data
// does) per RFC 7386, and that a null value deletes a key.
const jsonUsage = "add JSON or '@file'; repeated values merge as RFC 7386 patches"

// fJSON is the single Shared built-in for curl's --json convenience flag. It
// is a one-member family: the accumulator is what keeps repeated occurrences
// in order, and unlike the --data family they do not concatenate but compose
// as an RFC 7386 JSON Merge Patch — the first --json in command-line order is
// the document, each later one (a repeat, or a block's over the preamble's) a
// patch applied on top. It carries no shorthand: curl's --json has none, and
// -j is curl's --junk-session-cookies.
var fJSON = familyOption[jsonPiece]{
	key: "json", argType: "json",
	members: []familyMember[jsonPiece]{
		{
			flags:    []flagEntry{{name: "json"}},
			argument: "JSON", usage: jsonUsage,
			completionFiles: CompletionAtFiles,
			build:           newJSONPiece,
		},
	},
}

// jsonPiece is one --json argument: the JSON text as written, or when IsFile,
// the name of an '@file' whose contents are the JSON (the '@' stripped). File
// loading is deferred to preparation. Fan snapshots varied files; jsonBody
// validates and merges afterwards. Parsing stays free of I/O.
type jsonPiece struct {
	Spec   string
	IsFile bool
}

// newJSONPiece records one --json argument. A leading '@' marks a file — no
// JSON value begins with '@', so the mark is unambiguous.
func newJSONPiece(s string) jsonPiece {
	if name, ok := atFile(s); ok {
		return jsonPiece{Spec: name, IsFile: true}
	}
	return jsonPiece{Spec: s}
}

// resolveJSON accumulates the preamble's --json pieces with the block's, in
// that order. The block's compose on top of the shared preamble baseline, like
// the other content flags. Each inline piece is checked for well-formed JSON
// here, so a typo fails at parse time with the spec in the message, and when
// every piece is inline the merge's object requirement is settled here too. An
// '@file' piece's contents are validated when loaded, in jsonBody. Returns nil
// when neither scope carried --json.
func resolveJSON(pre, blk *segment) ([]jsonPiece, error) {
	all := accumPieces[jsonPiece](pre, blk, "json")
	if len(all) == 0 {
		return nil, nil
	}
	anyFile := false
	for _, p := range all {
		if !p.IsFile {
			if !json.Valid([]byte(p.Spec)) {
				return nil, fmt.Errorf("--json %q is not valid JSON", p.Spec)
			}
			continue
		}
		anyFile = true
		if err := p.fileRef().validate(); err != nil {
			return nil, err
		}
	}
	// With every piece inline, whether the merge has an object base and object
	// patches is settled here rather than waiting for jsonBody: the same check,
	// on the same bytes, with nothing left to read.
	if len(all) > 1 && !anyFile {
		if err := checkMergeable(all); err != nil {
			return nil, err
		}
	}
	return all, nil
}

// checkMergeable runs the merge's object requirement over inline pieces, in the
// order jsonBody would: the first is the base document, the rest are patches.
func checkMergeable(pieces []jsonPiece) error {
	if _, err := decodeObject(pieces[0], []byte(pieces[0].Spec), "the base --json"); err != nil {
		return err
	}
	for _, p := range pieces[1:] {
		if _, err := decodeObject(p, []byte(p.Spec), "a --json patch"); err != nil {
			return err
		}
	}
	return nil
}

// jsonBody materialises the block's --json pieces into the request body. A
// single piece is emitted verbatim, so a lone --json is byte-exact as written.
// With more than one piece the first is the base
// document and each subsequent piece is applied as an RFC 7386 JSON Merge Patch
// (see mergePatch): object keys merge recursively, a null value deletes its
// key, and any non-object value (array, scalar) replaces wholesale; the merged
// result is then re-serialised, so its object keys come out ordered and its
// whitespace normalised (number literals are preserved). Merging requires the
// base and every patch to be JSON objects. readFile loads an '@file' piece,
// using the configured block reader. RequestBody is the public
// entry point.
func (b Block) jsonBody(readFile func(name string) ([]byte, error)) ([]byte, error) {
	if len(b.json) == 0 {
		return nil, nil
	}
	raw := make([][]byte, len(b.json))
	for i, p := range b.json {
		data, err := p.load(readFile)
		if err != nil {
			return nil, err
		}
		raw[i] = data
	}

	if len(raw) == 1 {
		if !json.Valid(raw[0]) {
			return nil, jsonInvalid(b.json[0])
		}
		return raw[0], nil
	}

	merged, err := decodeObject(b.json[0], raw[0], "the base --json")
	if err != nil {
		return nil, err
	}
	var doc any = merged
	for i := 1; i < len(raw); i++ {
		patch, err := decodeObject(b.json[i], raw[i], "a --json patch")
		if err != nil {
			return nil, err
		}
		doc = mergePatch(doc, patch)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("curlblocks: serialising merged --json: %w", err)
	}
	return out, nil
}

// fileRef is the '@file' the piece loads from. It is meaningful only for a
// piece marked IsFile; an inline piece never reads one.
func (p jsonPiece) fileRef() fileRef {
	return fileRef{flag: "--json", sigil: "@", name: p.Spec}
}

// load returns the piece's JSON bytes: the inline spec, or the contents of its
// '@file' (which wraps a read error).
func (p jsonPiece) load(readFile func(string) ([]byte, error)) ([]byte, error) {
	if p.IsFile {
		return p.fileRef().read(readFile)
	}
	return []byte(p.Spec), nil
}

// mergePatch applies an RFC 7386 JSON Merge Patch to target and returns the
// result. An object patch merges key by key into an object target (a non-object
// target is first replaced by an empty object), with a null value deleting its
// key; any non-object patch (array, scalar) replaces target wholesale. Nested
// values recurse through the same rule.
func mergePatch(target, patch any) any {
	patchObj, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	targetObj, ok := target.(map[string]any)
	if !ok {
		targetObj = map[string]any{}
	}
	for k, v := range patchObj {
		if v == nil {
			delete(targetObj, k)
			continue
		}
		targetObj[k] = mergePatch(targetObj[k], v)
	}
	return targetObj
}

// decodeObject decodes a --json piece as a JSON object for merging. Numbers are
// decoded as json.Number, so a merge re-serialises them with their original
// literals rather than reformatting through float64. subject names the piece
// ("the base --json" / "a --json patch") for the not-an-object error.
func decodeObject(p jsonPiece, data []byte, subject string) (map[string]any, error) {
	v, err := decodeJSON(p, data)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be a JSON object to merge, got %s", subject, jsonKind(v))
	}
	return obj, nil
}

// decodeJSON decodes data as a single JSON value, preserving number literals
// (json.Number) and rejecting trailing tokens after the value.
func decodeJSON(p jsonPiece, data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, jsonInvalid(p)
	}
	if dec.Decode(new(any)) != io.EOF {
		return nil, jsonInvalid(p) // more than one value, or trailing junk
	}
	return v, nil
}

// jsonKind names a decoded JSON value's type for an error message.
func jsonKind(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "a boolean"
	case json.Number:
		return "a number"
	case string:
		return "a string"
	case []any:
		return "an array"
	default:
		return "a value"
	}
}

// jsonInvalid reports a piece whose bytes are not valid JSON, naming the
// '@file' or the inline spec. Both forms are bare: reading the file was our
// work (and fileRef.read reports a failure there, prefixed), but judging what
// it holds is the same verdict on the command line as written.
func jsonInvalid(p jsonPiece) error {
	if p.IsFile {
		return fmt.Errorf("--json file %q does not contain valid JSON", p.Spec)
	}
	return fmt.Errorf("--json %q is not valid JSON", p.Spec)
}
