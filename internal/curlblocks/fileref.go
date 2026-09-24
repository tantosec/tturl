package curlblocks

import "fmt"

// fileRef is a file the command line asked to be read: the flag that named it
// (as the user typed it), the sigil written before the name, and the name
// itself. curl spells such a reference two ways, both here: '@name' loads a
// file's bytes, and -F's '<name' reads a form field's value from one.
//
// Every flag family that takes a file makes one — the --data family, -F,
// --json, a --vary source — so the checks that reference must pass, and the
// wording they are refused in, are the same wherever it came from.
type fileRef struct {
	flag  string // "--data", "-F", "--json", "--vary FUZZ"
	sigil string // "@" or "<"
	name  string
}

// validate rejects an empty filename without reading. Stdin capability is
// checked against the resolved block by validateFileRefs.
func (r fileRef) validate() error {
	switch r.name {
	case "":
		return fmt.Errorf("%s: empty filename after '%s'", r.flag, r.sigil)
	}
	return nil
}

// read loads the reference through readFile, which is the block's (see
// WithFileReader) and is called with the name exactly as written after the
// sigil. A refusal is the user's own error, bare; a failure to read a file we
// accepted is ours, and carries the package prefix.
func (r fileRef) read(readFile func(name string) ([]byte, error)) ([]byte, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	data, err := readFile(r.name)
	if err != nil {
		return nil, fmt.Errorf("curlblocks: reading %s file %q: %w", r.flag, r.name, err)
	}
	return data, nil
}

// bodyFileRefs lists original body references without performing I/O.
func (b Block) bodyFileRefs() []fileRef {
	var refs []fileRef
	for _, p := range b.data {
		if ref, ok := p.fileRef(); ok {
			refs = append(refs, ref)
		}
	}
	for _, p := range b.form {
		if ref, ok := p.fileRef(); ok {
			refs = append(refs, ref)
		}
	}
	for _, p := range b.json {
		if p.IsFile {
			refs = append(refs, p.fileRef())
		}
	}
	return refs
}

// fileRefs includes body references and literal variation sources.
func (b Block) fileRefs() []fileRef {
	refs := b.bodyFileRefs()
	for _, v := range b.Vary {
		if ref, ok := v.source.fileRef(v.subject()); ok {
			refs = append(refs, ref)
		}
	}
	return refs
}

func (b Block) validateFileRefs() error {
	for _, ref := range b.fileRefs() {
		if err := ref.validate(); err != nil {
			return err
		}
		if ref.name == "-" && b.ReadStdin == nil {
			return fmt.Errorf("%s: reading from stdin ('%s-') is not supported", ref.flag, ref.sigil)
		}
	}
	return nil
}
