package curlblocks

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestVaryReferencedBodyForms(t *testing.T) {
	for _, test := range []struct {
		name  string
		args  []string
		files map[string]string
		want  []byte
	}{
		{
			"data",
			[]string{"-d", "@body", "--data-ascii", "@body"},
			map[string]string{"body": "TOKEN\r\n\x00\xff"},
			[]byte("a\xff&a\xff"),
		},
		{"binary", []string{"--data-binary", "@body"}, map[string]string{"body": "\x00TOKEN\xff\n"}, []byte("\x00a\xff\n")},
		{
			"urlencode",
			[]string{"--data-urlencode", "@body", "--data-urlencode", "TOKEN@body"},
			map[string]string{"body": "TOKEN \x00\xff"},
			[]byte("a+%00%FF&a=a+%00%FF"),
		},
		{
			"json",
			[]string{"--json", "@base", "--json", "@patch"},
			map[string]string{"base": `{"TOKEN":TOKEN,"keep":"TOKEN"}`, "patch": `{"TOKEN":null,"new":"TOKEN"}`},
			[]byte(`{"keep":"1","new":"1"}`),
		},
		{
			"upload",
			[]string{"-F", "TOKEN=@body;filename=TOKEN.txt;type=application/TOKEN"},
			map[string]string{"body": "\x00TOKEN\xff"},
			[]byte("\x00a\xff"),
		},
		{"field", []string{"-F", "TOKEN=<body"}, map[string]string{"body": "TOKEN\n"}, []byte("a\n")},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := "a"
			second := "b"
			if test.name == "json" {
				value = "1"
				second = "2"
			}
			loads := map[string]int{}
			p := New(WithFileReader(func(name string) ([]byte, error) {
				loads[name]++
				if loads[name] != 1 {
					return nil, errors.New("second read")
				}
				return []byte(test.files[name]), nil
			}))
			args := append(test.args, "--vary", "TOKEN={"+value+","+second+"}", "https://example.test/")
			plan, err := p.Parse(args)
			if err != nil {
				t.Fatal(err)
			}
			if len(loads) != 0 {
				t.Fatal("Parse performed I/O")
			}
			fan, err := plan.Blocks[0].Fan()
			if err != nil {
				t.Fatal(err)
			}
			body, _, _, err := fan.Variants[0].Block.requestBody("TOKEN-boundary")
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "upload" || test.name == "field" {
				if !bytes.Contains(body, append(append([]byte("\r\n\r\n"), test.want...), []byte("\r\n")...)) {
					t.Fatalf("body: %q", body)
				}
				if !bytes.Contains(body, []byte("TOKEN-boundary")) {
					t.Fatal("generated boundary varied")
				}
				if test.name == "upload" && (!bytes.Contains(body, []byte(`filename="a.txt"`)) ||
					!bytes.Contains(body, []byte("application/a"))) {
					t.Fatalf("metadata: %s", body)
				}
			} else if !bytes.Equal(body, test.want) {
				t.Fatalf("body %q, want %q", body, test.want)
			}
			expected := bytes.Clone(body)
			body[0] = 'z'
			fresh, _, _, err := fan.Variants[0].Block.requestBody("TOKEN-boundary")
			if err != nil || !bytes.Equal(fresh, expected) {
				t.Fatalf("mutated snapshot: %q, %v", fresh, err)
			}
		})
	}
}

func TestVaryFileSuffixAndSimultaneousBytes(t *testing.T) {
	raw := append([]byte("flag=TOKEN"), bytes.Repeat([]byte("Z"), 32000)...)
	b := Block{
		URLs: []string{"https://example.test/"}, data: []dataPiece{{Kind: dataBinary, Spec: "@body"}},
		readFile: func(string) ([]byte, error) { return raw, nil }, Vary: []VarySpec{mustSpec(t, "TOKEN=a-z")},
	}
	fan, err := b.Fan()
	if err != nil {
		t.Fatal(err)
	}
	if len(fan.Variants) != 26 {
		t.Fatalf("variants: %d", len(fan.Variants))
	}
	for i, variant := range fan.Variants {
		body, _, _, err := variant.Block.RequestBody()
		if err != nil || len(body) != 32006 || body[5] != byte('a'+i) || !bytes.Equal(body[6:], raw[10:]) {
			t.Fatalf("variant %d: length %d, %v", i, len(body), err)
		}
	}
	for _, mode := range []string{"clusterbomb", "pitchfork"} {
		p := New(WithFileReader(func(string) ([]byte, error) { return []byte("FIRST/SECOND\x00\xff"), nil }))
		plan, err := p.Parse([]string{
			"--vary", "FIRST={SECOND,x}", "--vary", "SECOND:hex={a,b}", "--vary-mode", mode,
			"--repeat", "2", "--data-binary", "@body", "https://example.test/",
		})
		if err != nil {
			t.Fatal(err)
		}
		groups, err := plan.Expand()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"SECOND/61\x00\xff", "SECOND/62\x00\xff", "x/61\x00\xff", "x/62\x00\xff"}
		labels := []string{
			"FIRST=SECOND,SECOND=a", "FIRST=SECOND,SECOND=b",
			"FIRST=x,SECOND=a", "FIRST=x,SECOND=b",
		}
		if mode == "pitchfork" {
			want = []string{want[0], want[3]}
			labels = []string{labels[0], labels[3]}
		}
		var bodies []string
		for i, group := range groups {
			body, _, _, err := group.Block.RequestBody()
			if err != nil {
				t.Fatal(err)
			}
			bodies = append(bodies, string(body))
			if !reflect.DeepEqual(labelDisplays(group.Labels), []string{labels[i] + "~1", labels[i] + "~2"}) {
				t.Fatalf("labels: %q", labelDisplays(group.Labels))
			}
		}
		if !reflect.DeepEqual(bodies, want) {
			t.Fatalf("%s: %q", mode, bodies)
		}
	}
}

func TestVaryFilePolicyInheritance(t *testing.T) {
	for _, policy := range []string{"true", "false"} {
		for _, tokenPre := range []bool{false, true} {
			for _, bodyPre := range []bool{false, true} {
				args := []string{"--vary-file-content=" + policy}
				if tokenPre {
					args = append(args, "--vary", "TOKEN={a,b}")
				}
				if bodyPre {
					args = append(args, "--data-binary", "@body")
				}
				args = append(args, "--block", "https://example.test/TOKEN")
				if !tokenPre {
					args = append(args, "--vary", "TOKEN={a,b}")
				}
				if !bodyPre {
					args = append(args, "--data-binary", "@body")
				}
				plan, err := New(WithFileReader(func(string) ([]byte, error) { return []byte("TOKEN"), nil })).Parse(args)
				if err != nil {
					t.Fatal(err)
				}
				groups, err := plan.Expand()
				if err != nil {
					t.Fatal(err)
				}
				for i, group := range groups {
					body, _, _, err := group.Block.RequestBody()
					want := "TOKEN"
					if policy == "true" {
						want = string(byte('a' + i))
					}
					if err != nil || string(body) != want {
						t.Fatalf("policy %s tokenPre %t bodyPre %t: %q, %v", policy, tokenPre, bodyPre, body, err)
					}
				}
			}
		}
	}
	plan, err := New(WithFileReader(fakeFS(map[string]string{"body": "TOKEN", "extra": "TOKEN!"}))).Parse([]string{
		"--vary-file-content=false", "--vary", "TOKEN={a,b}", "--data-binary", "@body",
		"--block", "--name", "preserved", "https://example.test/TOKEN",
		"--block", "--vary-file-content", "--vary", "TOKEN={c,d}", "--data-binary", "@extra", "https://example.test/TOKEN",
		"--block", "--name", "reset", "--vary-file-content=true", "--reset-body",
		"--data-binary", "@extra", "https://example.test/TOKEN",
		"--block", "--name", "reset-preserved", "--reset-body",
		"--data-binary", "@extra", "https://example.test/TOKEN",
	})
	if err != nil {
		t.Fatal(err)
	}
	groups, err := plan.Expand()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, group := range groups {
		body, _, _, err := group.Block.RequestBody()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(body))
	}
	if !reflect.DeepEqual(got, []string{"TOKEN", "TOKEN", "c&c!", "d&d!", "a!", "b!", "TOKEN!", "TOKEN!"}) {
		t.Fatalf("bodies: %q", got)
	}
}

func TestVaryFileOccurrenceAndSnapshots(t *testing.T) {
	for _, args := range [][]string{
		{"--vary-file-content=false", "--data-binary", "@body"},
		{"--data-binary", "@TOKEN"},
		{"-F", "upload=@TOKEN"},
		{"--vary-file-content=false", "--json", "@body"},
	} {
		p := New(WithFileReader(fakeFS(map[string]string{"body": "TOKEN", "TOKEN": "literal"})))
		plan, err := p.Parse(append(args, "--vary", "TOKEN={a}", "https://example.test/"))
		if err == nil {
			_, err = plan.Expand()
		}
		if err == nil ||
			!strings.Contains(err.Error(), "appears nowhere") {
			t.Fatalf("%q: %v", args, err)
		}
	}
	buffer := make([]byte, 5)
	calls := map[string]int{}
	plan, err := New(WithFileReader(func(name string) ([]byte, error) {
		calls[name]++
		if calls[name] > 1 {
			return nil, errors.New("second read")
		}
		copy(buffer, name)
		return buffer, nil
	})).Parse([]string{
		"--data-binary", "@FIRST", "--data-binary", "@OTHER", "--data-binary", "@FIRST",
		"--vary", "FIRST={OTHER,x}", "https://example.test/FIRST",
	})
	if err != nil {
		t.Fatal(err)
	}
	groups, err := plan.Expand()
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"OTHER&OTHER&OTHER", "x&OTHER&x"} {
		body, _, _, err := groups[i].Block.RequestBody()
		if err != nil || string(body) != want {
			t.Fatalf("%q: %v", body, err)
		}
	}
	if calls["FIRST"] != 1 || calls["OTHER"] != 1 {
		t.Fatalf("calls: %v", calls)
	}
}

func TestVaryFileProcessingAndLiteralSources(t *testing.T) {
	for _, test := range []struct {
		args      []string
		raw, want string
	}{
		{[]string{"-d", "@body", "--vary", "TOKEN={a\r\n\x00b}"}, "TOKEN", "ab"},
		{[]string{"--data-urlencode", "TOKEN@body", "--vary", "TOKEN={x=@y}"}, "TOKEN ", "x=@y=x%3D%40y+"},
		{[]string{"--data-binary", "@body", "--vary", "TOKEN:b64:url={a}"}, "TOKEN", "YQ%3D%3D"},
		{[]string{"--data-binary", "@body", "--vary", "TOKEN=@source"}, "TOKEN", "TOKEN"},
	} {
		plan, err := New(WithFileReader(func(name string) ([]byte, error) {
			if name == "source" {
				return []byte("TOKEN\n"), nil
			}
			return []byte(test.raw), nil
		})).Parse(append(test.args, "https://example.test/"))
		if err != nil {
			t.Fatal(err)
		}
		groups, err := plan.Expand()
		if err != nil {
			t.Fatal(err)
		}
		body, _, _, err := groups[0].Block.RequestBody()
		if err != nil || string(body) != test.want {
			t.Fatalf("%q: %q, %v", test.args, body, err)
		}
	}
	for _, field := range []string{"--data-binary", "--json", "-F"} {
		args := []string{field, "@TOKEN"}
		raw := "literal"
		switch field {
		case "--json":
			raw = `"literal"`
		case "-F":
			args[1] = "upload=@TOKEN"
		}
		reads := 0
		plan, err := New(WithFileReader(func(name string) ([]byte, error) {
			reads++
			if name != "TOKEN" {
				t.Fatalf("path %q", name)
			}
			return []byte(raw), nil
		})).Parse(append(args, "--vary", "TOKEN={a,b}", "https://example.test/TOKEN"))
		if err != nil {
			t.Fatal(err)
		}
		groups, err := plan.Expand()
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range groups {
			body, _, _, err := g.Block.requestBody("boundary")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(body, []byte(raw)) {
				t.Fatalf("%q", body)
			}
			if field == "-F" && !bytes.Contains(body, []byte(`filename="TOKEN"`)) {
				t.Fatalf("derived metadata: %q", body)
			}
		}
		if reads != 1 {
			t.Fatalf("reads: %d", reads)
		}
	}
}

func TestVaryFileDeferredErrors(t *testing.T) {
	cause := errors.New("unreadable input")
	plan, err := New(WithFileReader(func(string) ([]byte, error) { return nil, cause })).Parse([]string{
		"--block", "--data-binary", "@body", "--vary", "TOKEN={a}", "https://example.test/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Expand(); !errors.Is(err, cause) || !strings.Contains(err.Error(), "block 1") {
		t.Fatalf("read error: %v", err)
	}
	plan, err = New(WithFileReader(fakeFS(map[string]string{"body": "literal"}))).Parse([]string{
		"--block", "--data-binary", "@body", "--vary", "TOKEN={a}", "https://example.test/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Expand(); err == nil || !strings.Contains(err.Error(), "block 1") ||
		!strings.Contains(err.Error(), "appears nowhere") {
		t.Fatalf("occurrence error: %v", err)
	}
}

func TestVaryFileMetadataAndOverwrittenOccurrences(t *testing.T) {
	for _, args := range [][]string{
		{"--vary-file-content=false", "-F", "upload=@body;filename=TOKEN.txt"},
		{"--vary-file-content=false", "-F", "upload=@body;type=application/TOKEN"},
		{"--vary-file-content=false", "--data-urlencode", "TOKEN@body"},
		{"--json", "@base", "--json", `{"gone":null}`},
	} {
		p := New(WithFileReader(fakeFS(map[string]string{
			"body": "literal", "base": `{"gone":"TOKEN"}`,
		})))
		plan, err := p.Parse(append(args, "--vary", "TOKEN={a,b}", "https://example.test/"))
		if err != nil {
			t.Fatal(err)
		}
		groups, err := plan.Expand()
		if err != nil {
			t.Fatal(err)
		}
		body, _, _, err := groups[0].Block.requestBody("boundary")
		if err != nil {
			t.Fatal(err)
		}
		if args[0] == "--json" && string(body) != "{}" {
			t.Fatalf("overwritten body: %q", body)
		}
	}
}

func TestVaryFilePolicyWithoutBindings(t *testing.T) {
	for _, policy := range []string{"true", "false"} {
		reads := 0
		p := New(WithFileReader(func(string) ([]byte, error) {
			reads++
			return bytes.Repeat([]byte("TOKEN"), reads), nil
		}))
		plan, err := p.Parse([]string{
			"--vary-file-content=" + policy, "--data-binary", "@body",
			"--data-binary", "@body", "https://example.test/",
		})
		if err != nil {
			t.Fatal(err)
		}
		groups, err := plan.Expand()
		if err != nil {
			t.Fatal(err)
		}
		if reads != 0 {
			t.Fatal("unvaried expansion read files")
		}
		body, _, _, err := groups[0].Block.RequestBody()
		if err != nil || reads != 2 || string(body) != "TOKEN&TOKEN"+"TOKEN" {
			t.Fatalf("policy %s: %q, reads %d, %v", policy, body, reads, err)
		}
	}
}
