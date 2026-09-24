package curlblocks

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// TestNewConcurrent checks that Parsers can be built and used concurrently.
// The built-in flags are package-level singletons every Parser shares, so any
// per-construction write to one is a write to memory every other Parser is
// reading; the scopes are fixed once at package load instead (see the init in
// parser.go). The assertion is the race detector's -- this test earns its keep
// under `go test -race`, and CI runs that on every push.
func TestNewConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			p := New(WithInsecureFlag(), WithVersionFlag("1.0"))
			p.Global.Bool("verbose", "v", false, "verbose logging")
			plan, err := p.Parse([]string{"-k", "https://a"})
			if err != nil {
				t.Errorf("Parse returned error: %v", err)
				return
			}
			if !plan.Globals.Insecure {
				t.Error("-k did not reach Globals.Insecure")
			}
		})
	}
	wg.Wait()
}

// TestParseHelp checks that -h/--help is recognised as a help
// request, including with no URL present, where the structural "no URL" check
// would otherwise fire first.
func TestParseHelp(t *testing.T) {
	for _, args := range [][]string{
		{"-h"},
		{"--help"},
		{"--form-escape", "-h"},
		{"--block", "https://a", "--help"},
	} {
		if _, err := New().Parse(args); !errors.Is(err, ErrHelp) {
			t.Errorf("New().Parse(%q) error = %v, want ErrHelp", args, err)
		}
	}
}

// TestParseVersionRequested checks that --version/-V short-circuits to
// ErrVersion, ahead of the structural "no URL" check, so `tool --version` works
// on its own -- and that a Parser given no version has no such flag at all,
// rather than one whose only reply would be a blank line.
func TestParseVersionRequested(t *testing.T) {
	for _, arg := range []string{"--version", "-V"} {
		p := New(WithVersionFlag("mytool 1.0"))
		if _, err := p.Parse([]string{arg}); !errors.Is(err, ErrVersion) {
			t.Errorf("Parse(%q) error = %v, want ErrVersion", arg, err)
		}
		if got := p.Version(); got != "mytool 1.0" {
			t.Errorf("Version() = %q, want the string WithVersionFlag was given", got)
		}
		// Without the option the flag is unknown, like any flag a Parser does
		// not register.
		_, err := New().Parse([]string{arg})
		if err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Errorf("Parse(%q) with no version = %v, want an unknown-flag error", arg, err)
		}
	}
}

func TestSplitSegments(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		preamble []string
		blocks   [][]string
	}{
		{
			name:     "no separator is all preamble",
			args:     []string{"-k", "-H", "A: 1"},
			preamble: []string{"-k", "-H", "A: 1"},
			blocks:   nil,
		},
		{
			name: "separators delimit blocks",
			args: []string{
				"-k", "--block", "https://a",
				"--block", "https://b", "--repeat", "3",
			},
			preamble: []string{"-k"},
			blocks:   [][]string{{"https://a"}, {"https://b", "--repeat", "3"}},
		},
		{
			name:     "leading separator means empty preamble",
			args:     []string{"--block", "https://a"},
			preamble: nil,
			blocks:   [][]string{{"https://a"}},
		},
		{
			// A block with no arguments of its own is still a block; it is empty in
			// the same way an empty preamble is.
			name:     "trailing separator yields an empty block",
			args:     []string{"--block", "https://a", "--block"},
			preamble: nil,
			blocks:   [][]string{{"https://a"}, nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pre, blocks := splitSegments(tt.args)
			if !reflect.DeepEqual(pre, tt.preamble) {
				t.Errorf("preamble = %#v, want %#v", pre, tt.preamble)
			}
			if !reflect.DeepEqual(blocks, tt.blocks) {
				t.Errorf("blocks = %#v, want %#v", blocks, tt.blocks)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantSub string // substring the error message must contain
	}{
		{
			name:    "no block and no url anywhere",
			args:    []string{"-k", "-H", "A: 1"},
			wantSub: "no URL: give a URL or use '--block' URL",
		},
		{
			name:    "block with no url and no preamble url",
			args:    []string{"--block", "-H", "A: 1"},
			wantSub: "no URL",
		},
		{
			name:    "comma shorthand is unknown",
			args:    []string{"-,", "https://a"},
			wantSub: "unknown shorthand flag",
		},
		{
			name:    "insecure inside a block",
			args:    []string{"--block", "https://a", "-k"},
			wantSub: "--insecure (-k) is not valid in block 1",
		},
		{
			name:    "repeat below one",
			args:    []string{"--block", "https://a", "--repeat", "0"},
			wantSub: "integer >= 1",
		},
		{
			name:    "repeat non-integer",
			args:    []string{"--block", "https://a", "--repeat", "lots"},
			wantSub: "integer >= 1",
		},
		{
			name:    "data in preamble conflicts with form in a block",
			args:    []string{"-d", "a=1", "--block", "https://a", "-F", "f=v"},
			wantSub: "only one request body kind",
		},
		{
			name:    "form in preamble conflicts with data in a block",
			args:    []string{"-F", "f=v", "--block", "https://a", "-d", "a=1"},
			wantSub: "only one request body kind",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseTLS(tt.args)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want error containing %q", tt.args, tt.wantSub)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Parse(%q) error = %q, want substring %q", tt.args, err.Error(), tt.wantSub)
			}
		})
	}
}

// TestRepeatGrammar checks --repeat against the positive-integer grammar it
// shares with rate counts: plain ASCII digits, so a sign, a space, or a
// fractional or exponent spelling is refused rather than silently coerced, and
// the two flags accept the same spellings as each other.
func TestRepeatGrammar(t *testing.T) {
	good := map[string]int{"1": 1, "2": 2, "07": 7, "100": 100}
	for arg, want := range good {
		plan, err := New().Parse([]string{"https://a", "--repeat", arg})
		if err != nil {
			t.Errorf("--repeat %q: %v", arg, err)
			continue
		}
		if got := plan.Blocks[0].Repeat; got != want {
			t.Errorf("--repeat %q = %d, want %d", arg, got, want)
		}
	}
	for _, arg := range []string{"0", "-1", "+5", " 5", "5 ", "1.0", "1e2", "", "two", "0x2"} {
		_, err := New().Parse([]string{"https://a", "--repeat", arg})
		if err == nil || !strings.Contains(err.Error(), "--repeat must be an integer >= 1") {
			t.Errorf("--repeat %q error = %v, want the integer-grammar refusal", arg, err)
		}
	}
}

// TestBlockErrorPrefix checks that Parse routes a per-block resolution error
// through Plan.BlockError, so a real command line names its block ("block N:")
// only when the user actually opened one -- the implicit block behind a bare
// preamble URL was never opened, so its errors surface unprefixed. The rule
// itself is pinned at TestBlockError; this is the end-to-end guard, since a
// phantom "block 1:" is what a user would see.
func TestBlockErrorPrefix(t *testing.T) {
	_, err := New().Parse([]string{"https://a", "--repeat", "0"})
	if err == nil {
		t.Fatal("bare preamble with --repeat 0: got nil error, want one")
	}
	if strings.HasPrefix(err.Error(), "block ") {
		t.Errorf("implicit-block error = %q, should not name a phantom block", err)
	}

	_, err = New().Parse([]string{"--block", "https://a", "--repeat", "0"})
	if err == nil {
		t.Fatal("explicit block with --repeat 0: got nil error, want one")
	}
	if !strings.Contains(err.Error(), "block 1:") {
		t.Errorf("explicit-block error = %q, want a \"block 1:\" prefix", err)
	}
}

func TestParsePlans(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want *Plan
	}{
		{
			name: "single bare url defaults to repeat 1",
			args: []string{"--block", "https://example.com/"},
			want: &Plan{
				Blocks: []Block{{URLs: []string{"https://example.com/"}, Repeat: 1}},
			},
		},
		{
			name: "bare preamble url is the single request, no separator needed",
			args: []string{"https://example.com/"},
			want: &Plan{
				Blocks:   []Block{{URLs: []string{"https://example.com/"}, Repeat: 1}},
				implicit: true, // no --block opened: the block is the parser's, not the user's
			},
		},
		{
			name: "bare preamble url with --repeat sends that many",
			args: []string{"--insecure", "https://example.com/", "--repeat=10"},
			want: &Plan{
				Globals: Globals{Insecure: true},
				Blocks: []Block{{
					URLs: []string{"https://example.com/"}, Repeat: 10,
					repeatSet: true,
				}},
				implicit: true,
			},
		},
		{
			name: "bare and --url forms mix within a block",
			args: []string{
				"--insecure", "--block", "https://a", "https://b",
				"--url", "https://c", "-H", "X-Test: 1", "--repeat=10",
			},
			want: &Plan{
				Globals: Globals{Insecure: true},
				Blocks: []Block{{
					URLs:      []string{"https://a", "https://b", "https://c"},
					Headers:   []Header{{Name: "X-Test", Value: "1"}},
					Repeat:    10,
					repeatSet: true,
				}},
			},
		},
		{
			name: "baseline header inherited; block overrides method and body",
			args: []string{
				"-H", "Authorization: Bearer base",
				"--block", "https://a",
				"--block", "https://b", "-X", "PATCH", "-H", "Content-Type: application/json", "-d", `{"status":"shipped"}`,
			},
			want: &Plan{
				Blocks: []Block{
					{
						URLs:    []string{"https://a"},
						Headers: []Header{{Name: "Authorization", Value: "Bearer base"}},
						Repeat:  1,
					},
					{
						URLs: []string{"https://b"},
						Headers: []Header{
							{Name: "Authorization", Value: "Bearer base"},
							{Name: "Content-Type", Value: "application/json"},
						},
						Method: "PATCH",
						data:   []dataPiece{{Kind: dataASCII, Spec: `{"status":"shipped"}`}},
						Repeat: 1,
					},
				},
			},
		},
		{
			name: "preamble cookies inherited by every block; block cookies stack on top",
			args: []string{
				"-b", "session=shared; theme=dark",
				"--block", "https://a",
				"--block", "https://b", "-b", "ab=1",
			},
			want: &Plan{
				Blocks: []Block{
					{
						URLs:    []string{"https://a"},
						Cookies: []Cookie{{"session", "shared"}, {"theme", "dark"}},
						Repeat:  1,
					},
					{
						URLs:    []string{"https://b"},
						Cookies: []Cookie{{"session", "shared"}, {"theme", "dark"}, {"ab", "1"}},
						Repeat:  1,
					},
				},
			},
		},
		{
			name: "preamble header inherited by every block; block headers stack on top",
			args: []string{
				"-H", "Authorization: Bearer base-token",
				"--block", "https://api.example.com/a",
				"--block", "https://api.example.com/b", "-H", "X-Extra: 1",
			},
			want: &Plan{
				Blocks: []Block{
					{
						URLs:    []string{"https://api.example.com/a"},
						Headers: []Header{{Name: "Authorization", Value: "Bearer base-token"}},
						Repeat:  1,
					},
					{
						URLs: []string{"https://api.example.com/b"},
						Headers: []Header{
							{Name: "Authorization", Value: "Bearer base-token"},
							{Name: "X-Extra", Value: "1"},
						},
						Repeat: 1,
					},
				},
			},
		},
		{
			name: "preamble method inherited; a block overrides it",
			args: []string{
				"-X", "PUT", "https://base/",
				"--block",
				"--block", "https://other/", "-X", "DELETE",
			},
			want: &Plan{
				Blocks: []Block{
					{URLs: []string{"https://base/"}, Method: "PUT", Repeat: 1},
					{URLs: []string{"https://other/"}, Method: "DELETE", Repeat: 1},
				},
			},
		},
		{
			name: "preamble URL and --repeat inherited; a block overrides each",
			args: []string{
				"https://base/", "--repeat", "3",
				"--block", "--data", "a=1",
				"--block", "https://other/", "--repeat", "1", "--data", "a=2",
			},
			want: &Plan{
				Blocks: []Block{
					{
						URLs:      []string{"https://base/"},
						data:      []dataPiece{{Kind: dataASCII, Spec: "a=1"}},
						Repeat:    3,
						repeatSet: true,
					},
					{
						URLs:      []string{"https://other/"},
						data:      []dataPiece{{Kind: dataASCII, Spec: "a=2"}},
						Repeat:    1,
						repeatSet: true,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTLS(tt.args)
			if err != nil {
				t.Fatalf("Parse(%q) returned error: %v", tt.args, err)
			}
			// Every block a parser builds carries that parser's configuration; stamp
			// it on the expectations so each case above states only the folding it is
			// about. The comparison is still whole-struct, unexported fields included.
			// No case above sets or suppresses Accept, so every block carries the
			// baked baseline, last in the list (see resolveHeaderSet).
			for i := range tt.want.Blocks {
				tt.want.Blocks[i].defaultScheme = defaultSchemeHTTP
				tt.want.Blocks[i].Headers = append(tt.want.Blocks[i].Headers, acceptBaseline())
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("New().Parse(%q)\n got = %+v\nwant = %+v", tt.args, got, tt.want)
			}
		})
	}
}

// TestWithFileReader checks that the reader configured on a Parser is the one
// every @file on the command line is read through: a --data body and a --vary
// source both draw their bytes from the injected reader, proving the
// Parser -> Block -> body/fan threading.
func TestWithFileReader(t *testing.T) {
	p := New(WithFileReader(fakeFS(map[string]string{
		"body.txt":  "a=1&b=2",
		"users.txt": "alice\nbob\n",
	})))
	plan, err := p.Parse([]string{
		"https://h/FUZZ", "--data", "@body.txt", "--vary", "FUZZ=@users.txt",
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	b := plan.Blocks[0]

	body, present, _, err := b.RequestBody()
	if err != nil || !present {
		t.Fatalf("RequestBody: present=%v err=%v", present, err)
	}
	if got := string(body); got != "a=1&b=2" {
		t.Errorf("body = %q, want the injected file's contents", got)
	}

	fan, err := b.Fan()
	if err != nil {
		t.Fatalf("Fan: %v", err)
	}
	want := []string{"https://h/alice", "https://h/bob"}
	got := make([]string, len(fan.Variants))
	for i, v := range fan.Variants {
		got[i] = v.Block.URLs[0]
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fan URLs = %v, want %v", got, want)
	}
}

// TestDefaultFileReader checks the fallback a Parser built without
// WithFileReader relies on: it configures no reader, and a block from it reads
// an @file from the filesystem.
func TestDefaultFileReader(t *testing.T) {
	if New().fileReader != nil {
		t.Error("New() configured a file reader; the os.ReadFile fallback would never run")
	}

	path := filepath.Join(t.TempDir(), "body.txt")
	if err := os.WriteFile(path, []byte("a=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := New().Parse([]string{"https://h/", "--data", "@" + path})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	body, present, _, err := plan.Blocks[0].RequestBody()
	if err != nil || !present {
		t.Fatalf("RequestBody: present=%v err=%v", present, err)
	}
	if got := string(body); got != "a=1" {
		t.Errorf("body = %q, want a=1 read from disk", got)
	}
}
