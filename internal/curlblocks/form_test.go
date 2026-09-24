package curlblocks

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseForm checks the -F/--form family parses into ordered parts: -F and
// --form-string share one accumulator (so order is preserved), --form-string is
// always literal, -F recognises @file / <file / ;type= / ;filename=, and a
// block's parts append to the inherited preamble baseline.
func TestParseForm(t *testing.T) {
	plan, err := New().Parse([]string{
		"-F", "base=shared",
		"--block", "https://a",
		"--block", "https://b",
		"-F", "field=value",
		"--form-string", "lit=@notafile",
		"-F", "upload=@photo.png;type=image/png;filename=p.png",
		"-F", "notes=<note.txt",
	})
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	// Block A carried no form of its own, so it has just the preamble baseline.
	wantA := []formPart{{Name: "base", Kind: formLiteral, Value: "shared"}}
	if got := plan.Blocks[0].form; !reflect.DeepEqual(got, wantA) {
		t.Errorf("block A form = %+v, want %+v", got, wantA)
	}
	// Block B's parts append to the inherited baseline, preamble part first.
	wantB := []formPart{
		{Name: "base", Kind: formLiteral, Value: "shared"},
		{Name: "field", Kind: formLiteral, Value: "value"},
		{Name: "lit", Kind: formLiteral, Value: "@notafile"}, // --form-string: '@' is literal
		{Name: "upload", Kind: formFile, Value: "photo.png", FileName: "p.png", Type: "image/png"},
		{Name: "notes", Kind: formFileText, Value: "note.txt"},
	}
	if got := plan.Blocks[1].form; !reflect.DeepEqual(got, wantB) {
		t.Errorf("block B form = %+v, want %+v", got, wantB)
	}
}

// TestParseFormEscape checks --form-escape as a shared request flag: a
// preamble value is inherited by every block, and a block may set it on its own
// (matching -F's own scope, and curl, which groups --form-escape with the form
// flags).
func TestParseFormEscape(t *testing.T) {
	// A preamble --form-escape is inherited by the block.
	plan, err := New().Parse([]string{"--form-escape", "--block", "https://a", "-F", "f=v"})
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if !plan.Blocks[0].FormEscape {
		t.Error("Blocks[0].FormEscape = false, want true (inherited from the preamble)")
	}

	// Set per block: only the block that sets it is escaped.
	plan, err = New().Parse([]string{
		"--block", "https://a", "-F", "f=v", "--form-escape",
		"--block", "https://b", "-F", "g=w",
	})
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if !plan.Blocks[0].FormEscape {
		t.Error("Blocks[0].FormEscape = false, want true (set on the block)")
	}
	if plan.Blocks[1].FormEscape {
		t.Error("Blocks[1].FormEscape = true, want false (not set on that block)")
	}
}

// TestFormBody asserts the exact multipart/form-data body for a literal field,
// a file upload (with overridden filename and explicit type), and a <file text
// field, under a fixed boundary so the output is fully determined.
func TestFormBody(t *testing.T) {
	read := fakeFS(map[string]string{"dir/file.dat": "FILEDATA", "note.txt": "NOTEDATA"})
	b := Block{form: []formPart{
		{Name: "field", Kind: formLiteral, Value: "value"},
		{Name: "upload", Kind: formFile, Value: "dir/file.dat", FileName: "renamed.dat", Type: "application/x-foo"},
		{Name: "notes", Kind: formFileText, Value: "note.txt"},
	}}
	const boundary = "BoUnDaRy123"

	body, contentType, err := b.formBody(read, boundary, false)
	if err != nil {
		t.Fatalf("FormBody returned error: %v", err)
	}
	if want := "multipart/form-data; boundary=" + boundary; contentType != want {
		t.Errorf("contentType = %q, want %q", contentType, want)
	}
	want := "--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="field"` + "\r\n\r\n" +
		"value" +
		"\r\n--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="upload"; filename="renamed.dat"` + "\r\n" +
		"Content-Type: application/x-foo\r\n\r\n" +
		"FILEDATA" +
		"\r\n--" + boundary + "\r\n" +
		`Content-Disposition: form-data; name="notes"` + "\r\n\r\n" +
		"NOTEDATA" +
		"\r\n--" + boundary + "--\r\n"
	if string(body) != want {
		t.Errorf("body =\n%q\nwant\n%q", body, want)
	}
}

// TestFormBodyNameEscaping checks the two name-escaping modes: percent-encoding
// (curl default) and backslash-escaping (--form-escape).
func TestFormBodyNameEscaping(t *testing.T) {
	read := noFiles
	part := Block{form: []formPart{{Name: `a"b`, Kind: formLiteral, Value: "x"}}}

	percent, _, err := part.formBody(read, "B", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(percent), `name="a%22b"`) {
		t.Errorf("default escaping: body missing percent-encoded name:\n%s", percent)
	}

	backslash, _, err := part.formBody(read, "B", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backslash), `name="a\"b"`) {
		t.Errorf("--form-escape: body missing backslash-escaped name:\n%s", backslash)
	}
}

// TestFormBodyErrors covers the file-reading failures surfaced by FormBody:
// a missing file errors.
func TestFormBodyErrors(t *testing.T) {
	read := noFiles

	if _, _, err := (Block{
		form: []formPart{{Name: "f", Kind: formFileText, Value: "nope.txt"}},
	}).formBody(read, "B", false); err == nil ||
		!strings.Contains(err.Error(), "nope.txt") {
		t.Errorf("missing-file error = %v, want one naming the file", err)
	}
}

// TestFormBodyFileContentType checks the complete media-type decision: a known
// extension is recognised, an unknown extension falls back to octet-stream,
// and an explicit type overrides the guess. The .json expectation comes from
// Go's built-in table and does not depend on the host configuration.
func TestFormBodyFileContentType(t *testing.T) {
	read := fakeFS(map[string]string{
		"data.unknownext": "x",
		"payload.json":    `{"a":1}`,
	})
	tests := []struct {
		name, file, explicit, want string
	}{
		{"known extension", "payload.json", "", "application/json"},
		{"unknown extension", "data.unknownext", "", "application/octet-stream"},
		{"explicit type", "payload.json", "text/plain", "text/plain"},
	}
	for _, tt := range tests {
		b := Block{form: []formPart{{
			Name: "f", Kind: formFile, Value: tt.file, Type: tt.explicit,
		}}}
		body, _, err := b.formBody(read, "B", false)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		want := "Content-Type: " + tt.want
		if !strings.Contains(string(body), want) {
			t.Errorf("%s: body missing %q:\n%s", tt.name, want, body)
		}
		if n := strings.Count(string(body), "Content-Type: "); n != 1 {
			t.Errorf("%s: body has %d Content-Type fields, want 1:\n%s",
				tt.name, n, body)
		}
	}
}

// TestFormRejectsUnsupportedCurlForms checks the -F extras accepted by curl
// but refused here: a multi-file "@one,two" upload, which would
// otherwise be caught only by the joined name failing to open, and a
// double-quoted content, whose ';' curl protects and this package always reads
// as a sub-option separator. A comma outside an '@file' content is ordinary
// text, so a literal field keeps it.
func TestFormRejectsUnsupportedCurlForms(t *testing.T) {
	rejected := []struct {
		spec string
		want string
	}{
		{"noequals", "name=content"},
		{"n=v;filename=x", "applies only to an '@file'"},
		{"f=@one,two", "multi-file"},
		{`f="a;b"`, "malformed option"},
		{"f=@a.txt;headers=@h.txt", "unsupported option"},
		{"f=@a.txt;encoder=base64", "unsupported option"},
	}
	for _, tt := range rejected {
		if _, err := New().Parse([]string{"https://h/", "-F", tt.spec}); err == nil ||
			!strings.Contains(err.Error(), tt.want) {
			t.Errorf("-F %q error = %v, want one mentioning %q", tt.spec, err, tt.want)
		}
	}

	plan, err := New().Parse([]string{"https://h/", "-F", "f=a,b"})
	if err != nil {
		t.Fatalf("a literal comma is text, not a multi-file spec: %v", err)
	}
	if got := plan.Blocks[0].form[0].Value; got != "a,b" {
		t.Errorf("literal form value = %q, want %q", got, "a,b")
	}

	// A quote is not a delimiter either, so it stays part of the filename rather
	// than being stripped: the spec parses and the name is what fails to open.
	plan, err = New().Parse([]string{"https://h/", "-F", `f=@"sub/up.txt";type=text/x`})
	if err != nil {
		t.Fatalf("a quoted filename parses (quotes are ordinary characters): %v", err)
	}
	if got := plan.Blocks[0].form[0].Value; got != `"sub/up.txt"` {
		t.Errorf("quoted filename = %q, want the quotes kept", got)
	}
}
