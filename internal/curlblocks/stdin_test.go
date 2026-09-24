package curlblocks

import (
	"bytes"
	"errors"
	"testing"
)

func TestStdinReferencesMatchFiles(t *testing.T) {
	for _, test := range []struct {
		name        string
		stdin, file []string
		input       []byte
	}{
		{"data", []string{"-d@-"}, []string{"-d@input"}, []byte("a\r\nb\x00\xff")},
		{"ascii", []string{"--data-ascii=@-"}, []string{"--data-ascii=@input"}, []byte("a\r\nb\x00")},
		{"binary", []string{"--data-binary", "@-"}, []string{"--data-binary", "@input"}, []byte("a\x00\xff\r\n")},
		{"urlencode", []string{"--data-urlencode", "@-"}, []string{"--data-urlencode", "@input"}, []byte("a b\x00\xff")},
		{"named", []string{"--data-urlencode", "name@-"}, []string{"--data-urlencode", "name@input"}, []byte("a b\n")},
		{
			"json",
			[]string{"--json", "@-", "--json", `{"b":null}`},
			[]string{"--json", "@input", "--json", `{"b":null}`},
			[]byte(`{"a":1,"b":2}`),
		},
		{
			"upload",
			[]string{"-F", "file=@-;filename=input;type=application/octet-stream"},
			[]string{"-F", "file=@input;filename=input;type=application/octet-stream"},
			[]byte("a\x00\xff"),
		},
		{"field", []string{"-F", "name=<-"}, []string{"-F", "name=<input"}, []byte("a\x00\xff")},
	} {
		t.Run(test.name, func(t *testing.T) {
			loads := 0
			p := New(
				WithStdinReader(func() ([]byte, error) { loads++; return test.input, nil }),
				WithFileReader(func(string) ([]byte, error) { return test.input, nil }))
			var bodies [][]byte
			for _, args := range [][]string{test.stdin, test.file} {
				plan, err := p.Parse(append(args, "https://example.test/"))
				if err != nil {
					t.Fatal(err)
				}
				body, _, _, err := plan.Blocks[0].requestBody("boundary")
				if err != nil {
					t.Fatal(err)
				}
				bodies = append(bodies, body)
			}
			if loads != 1 || !bytes.Equal(bodies[0], bodies[1]) {
				t.Fatalf("loads %d, bodies %q", loads, bodies)
			}
		})
	}
}

func TestStdinLiteralInputs(t *testing.T) {
	calls := 0
	p := New(WithStdinReader(func() ([]byte, error) { calls++; return nil, errors.New("unexpected read") }))
	plan, err := p.Parse([]string{
		"--data-raw", "@-", "--data-urlencode", "name=@-", "https://example.test/",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _, _, err := plan.Blocks[0].RequestBody()
	if err != nil || calls != 0 || string(body) != "@-&name=%40-" {
		t.Fatalf("%q, %v, calls %d", body, err, calls)
	}
	plan, err = New().Parse([]string{"--form-string", "name=@-", "https://example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	body, _, _, err = plan.Blocks[0].requestBody("boundary")
	if err != nil || !bytes.Contains(body, []byte("\r\n\r\n@-\r\n")) {
		t.Fatalf("%q, %v", body, err)
	}
}

func TestStdinUploadDefaults(t *testing.T) {
	p := New(WithStdinReader(func() ([]byte, error) { return []byte("body"), nil }))
	plan, err := p.Parse([]string{"-F", "upload=@-", "https://example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	body, _, _, err := plan.Blocks[0].requestBody("boundary")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`filename="-"`)) ||
		!bytes.Contains(body, []byte("Content-Type: application/octet-stream")) {
		t.Fatalf("%s", body)
	}
}

func TestStdinCapabilityAndPreparation(t *testing.T) {
	loads := 0
	load := func() ([]byte, error) { loads++; return nil, nil }
	p := New(WithStdinReader(load), WithFileReader(func(name string) ([]byte, error) {
		if name != "./-" {
			t.Fatalf("file reader got %q", name)
		}
		return []byte("file"), nil
	}))
	for _, args := range [][]string{
		{"--help", "--data", "@-"},
		{"--data", "@-", "--bad", "https://example.test/"},
		{"--data", "@", "https://example.test/"},
	} {
		_, _ = p.Parse(args)
	}
	if loads != 0 {
		t.Fatalf("Parse loaded stdin %d times", loads)
	}
	plan, err := p.Parse([]string{"--data-binary", "@./-", "https://example.test/"})
	if err != nil {
		t.Fatal(err)
	}
	body, _, _, err := plan.Blocks[0].RequestBody()
	if err != nil || string(body) != "file" || loads != 0 {
		t.Fatalf("%q: %v", body, err)
	}
	b := Block{ReadStdin: load, data: []dataPiece{{Kind: dataBinary, Spec: "@-"}}}
	body, present, _, err := b.RequestBody()
	if err != nil || !present || len(body) != 0 || loads != 1 {
		t.Fatalf("empty body %q: %v", body, err)
	}
	b.data = nil
	b.json = []jsonPiece{{Spec: "-", IsFile: true}}
	if _, _, _, err := b.RequestBody(); err == nil {
		t.Fatal("empty JSON succeeded")
	}
}
