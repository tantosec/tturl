package curlblocks

import (
	"reflect"
	"strings"
	"testing"
)

// TestParseHeaderSpec exercises the -H trichotomy: "Name: value" is a header
// (name trimmed, leading OWS after the colon dropped, trailing whitespace kept,
// as curl does), "Name;" an empty-valued header, and "Name:" (nothing but
// optional whitespace after the colon) a default-suppression rather than a
// header.
func TestParseHeaderSpec(t *testing.T) {
	tests := []struct {
		spec         string
		want         Header
		wantSuppress bool
	}{
		{"Accept: a", Header{Name: "Accept", Value: "a"}, false},
		{"X-Empty;", Header{Name: "X-Empty", Value: ""}, false},
		{"Accept:", Header{Name: "Accept"}, true},
		{"  X-Trim :  v  ", Header{Name: "X-Trim", Value: "v  "}, false}, // trailing kept, curl-style
		{"X-Trail:keep   ", Header{Name: "X-Trail", Value: "keep   "}, false},
		{"Accept:   ", Header{Name: "Accept"}, true}, // whitespace-only value suppresses
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			h, suppress, err := parseHeaderSpec(tt.spec)
			if err != nil {
				t.Fatalf("parseHeaderSpec(%q) error: %v", tt.spec, err)
			}
			if suppress != tt.wantSuppress {
				t.Fatalf("suppress = %v, want %v", suppress, tt.wantSuppress)
			}
			if suppress {
				if !strings.EqualFold(h.Name, tt.want.Name) {
					t.Errorf("suppressed name = %q, want %q", h.Name, tt.want.Name)
				}
			} else if h != tt.want {
				t.Errorf("header = %+v, want %+v", h, tt.want)
			}
		})
	}
}

// TestParseHeaderSpecErrors covers the malformed-spec rejections: a bare token
// (no colon, no terminating ';') and an empty name in either form.
func TestParseHeaderSpecErrors(t *testing.T) {
	tests := []struct {
		spec    string
		wantSub string
	}{
		{"no-colon", "Name: value"},
		{": v", "empty name"},
		{";", "empty name"},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			if _, _, err := parseHeaderSpec(tt.spec); err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("parseHeaderSpec(%q) error = %v, want substring %q", tt.spec, err, tt.wantSub)
			}
		})
	}
}

// TestResolveHeaderSet exercises the header fold through Parse: -H stacks (with
// duplicates), the "Name;"/"Name:" forms, and --delete-header resolves
// positionally against a later -H of the same name. A New() parser bakes no
// User-Agent, so Headers holds the -H results plus curl's Accept baseline.
func TestResolveHeaderSet(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		wantHeaders    []Header
		wantSuppressed []string
	}{
		{
			name:        "no header flags: the baked Accept baseline alone",
			args:        []string{"https://x"},
			wantHeaders: []Header{acceptBaseline()},
		},
		{
			// An explicit -H of that name displaces the baked baseline, so Accept
			// appears twice here rather than three times.
			name:        "stack in order, duplicates kept",
			args:        []string{"https://x", "-H", "Accept: a", "-H", "Accept: b"},
			wantHeaders: []Header{{Name: "Accept", Value: "a"}, {Name: "Accept", Value: "b"}},
		},
		{
			// "-H 'Accept:'" removes the baseline and leaves nothing in its place.
			name:           "mixed forms: header, empty value, suppression",
			args:           []string{"https://x", "-H", "X: 1", "-H", "Y;", "-H", "Accept:"},
			wantHeaders:    []Header{{Name: "X", Value: "1"}, {Name: "Y", Value: ""}},
			wantSuppressed: []string{"Accept"},
		},
		{
			name:           "--delete-header drops the baked Accept too",
			args:           []string{"https://x", "--delete-header", "Accept"},
			wantHeaders:    nil,
			wantSuppressed: []string{"Accept"},
		},
		{
			name:        "delete then re-add keeps the later -H",
			args:        []string{"https://x", "--delete-header", "X", "-H", "X: new"},
			wantHeaders: []Header{{Name: "X", Value: "new"}, acceptBaseline()},
		},
		{
			name:           "add then delete removes it (positional)",
			args:           []string{"https://x", "-H", "X: old", "--delete-header", "X"},
			wantHeaders:    []Header{acceptBaseline()},
			wantSuppressed: []string{"X"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := New().Parse(tt.args)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.args, err)
			}
			b := plan.Blocks[0]
			if !reflect.DeepEqual(b.Headers, tt.wantHeaders) {
				t.Errorf("Headers = %+v, want %+v", b.Headers, tt.wantHeaders)
			}
			if !reflect.DeepEqual(b.SuppressedHeaders, tt.wantSuppressed) {
				t.Errorf("SuppressedHeaders = %+v, want %+v", b.SuppressedHeaders, tt.wantSuppressed)
			}
		})
	}
}

// TestDeleteHeader checks --delete-header: it drops every header of that name
// accumulated so far (case-insensitively) — inherited, an earlier -H, or an
// auto-added default — and a later -H/-A/-u of that name re-adds it, so order
// decides. A name not present is a no-op, and the flag is shared (valid in the
// preamble too).
func TestDeleteHeader(t *testing.T) {
	t.Run("drops an inherited preamble header, keeps the rest", func(t *testing.T) {
		plan, err := New().Parse([]string{
			"-H", "Authorization: Bearer shared", "-H", "X-Keep: yes",
			"--block", "https://a",
			"--block", "https://b", "--delete-header", "Authorization",
		})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if !plan.Blocks[0].setsHeader("Authorization") || !plan.Blocks[0].setsHeader("X-Keep") {
			t.Errorf("block A headers = %+v, want both inherited", plan.Blocks[0].Headers)
		}
		if plan.Blocks[1].setsHeader("Authorization") {
			t.Errorf("block B still has Authorization: %+v", plan.Blocks[1].Headers)
		}
		if !plan.Blocks[1].setsHeader("X-Keep") {
			t.Errorf("block B lost the unrelated header: %+v", plan.Blocks[1].Headers)
		}
	})

	t.Run("drops all inherited duplicates of a name, case-insensitively", func(t *testing.T) {
		plan, err := New().Parse([]string{
			"-H", "X-Dup: 1", "-H", "x-dup: 2", "-H", "Other: keep",
			"--block", "https://a", "--delete-header", "X-DUP",
		})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if plan.Blocks[0].setsHeader("X-Dup") {
			t.Errorf("inherited duplicates of X-Dup survived: %+v", plan.Blocks[0].Headers)
		}
		if !plan.Blocks[0].setsHeader("Other") {
			t.Errorf("unrelated header dropped: %+v", plan.Blocks[0].Headers)
		}
	})

	t.Run("delete composes positionally with the block's own -H", func(t *testing.T) {
		// The inherited Baseline is always dropped; whether the block's own -H
		// survives depends on the order: delete-then-set keeps it,
		// set-then-delete drops it.
		cases := []struct {
			args []string
			want []string
		}{
			{
				[]string{
					"-H", "Baseline: Foo", "--block", "https://a",
					"--delete-header", "Baseline", "-H", "Baseline: new",
				},
				[]string{"new"},
			},
			{
				[]string{
					"-H", "Baseline: Foo", "--block", "https://a",
					"-H", "Baseline: new", "--delete-header", "Baseline",
				},
				nil,
			},
		}
		for _, c := range cases {
			plan, err := New().Parse(c.args)
			if err != nil {
				t.Fatalf("args %v: parse error: %v", c.args, err)
			}
			var got []string
			for _, h := range plan.Blocks[0].Headers {
				if strings.EqualFold(h.Name, "Baseline") {
					got = append(got, h.Value)
				}
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("args %v: Baseline = %v, want %v", c.args, got, c.want)
			}
		}
	})

	t.Run("suppresses an auto-added default (Content-Type)", func(t *testing.T) {
		plan, err := New().Parse([]string{
			"--block", "https://a", "-d", "x=1", "--delete-header", "Content-Type",
		})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if !plan.Blocks[0].SuppressesHeader("Content-Type") {
			t.Error("Content-Type not marked suppressed by --delete-header")
		}
		_, present, hs, err := plan.Blocks[0].RequestBody()
		if err != nil || !present {
			t.Fatalf("RequestBody err=%v present=%v", err, present)
		}
		for _, h := range hs {
			if strings.EqualFold(h.Name, "Content-Type") {
				t.Errorf("Content-Type default survived --delete-header: %+v", hs)
			}
		}
	})

	t.Run("absent name is a no-op", func(t *testing.T) {
		plan, err := New().Parse([]string{
			"--block", "https://a", "-H", "X-Keep: yes", "--delete-header", "Nonexistent",
		})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if !plan.Blocks[0].setsHeader("X-Keep") {
			t.Errorf("no-op delete removed a header: %+v", plan.Blocks[0].Headers)
		}
	})

	t.Run("valid in the preamble, removing a preamble header positionally", func(t *testing.T) {
		// --delete-header is shared; in the preamble it removes an earlier
		// preamble -H of that name.
		plan, err := New().Parse([]string{"-H", "X: a", "--delete-header", "X", "https://a"})
		if err != nil {
			t.Fatalf("parse error: %v", err)
		}
		if plan.Blocks[0].setsHeader("X") {
			t.Errorf("preamble --delete-header did not remove X: %+v", plan.Blocks[0].Headers)
		}
	})
}

// TestUserAndBearer checks -u/--user and --oauth2-bearer resolve to an
// Authorization header: Basic base64 (with a ':' appended to a bare user),
// Bearer for a token, --oauth2-bearer winning over -u regardless of order, an
// explicit -H Authorization winning over both regardless of order (it is
// curl-exact, unlike the positional native flags), and "-H 'Authorization:'"
// suppressing them. base64 values are curl's (verified against curl 8.14.1).
func TestUserAndBearer(t *testing.T) {
	authOf := func(b Block) string {
		v, _ := lookupHeader(b.Headers, "Authorization")
		return v
	}
	parse := func(t *testing.T, args ...string) *Plan {
		t.Helper()
		plan, err := New().Parse(args)
		if err != nil {
			t.Fatalf("parse error for %v: %v", args, err)
		}
		return plan
	}

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"basic with password", []string{"-u", "user:pass", "https://x"}, "Basic dXNlcjpwYXNz"},        // base64("user:pass")
		{"basic bare user gets empty password", []string{"-u", "user", "https://x"}, "Basic dXNlcjo="}, // base64("user:")
		// An empty -u still authenticates, as an empty user and password: curl
		// sends the same "Basic Og==" for -u '' as for -u ':'.
		{"empty -u authenticates as an empty pair", []string{"-u", "", "https://x"}, "Basic Og=="},
		{"-u ':' is the same empty pair", []string{"-u", ":", "https://x"}, "Basic Og=="},
		{"bearer token", []string{"--oauth2-bearer", "TOK", "https://x"}, "Bearer TOK"},
		{
			"bearer wins over user, user first",
			[]string{"-u", "user:pass", "--oauth2-bearer", "TOK", "https://x"},
			"Bearer TOK",
		},
		{
			"bearer wins over user, bearer first",
			[]string{"--oauth2-bearer", "TOK", "-u", "user:pass", "https://x"},
			"Bearer TOK",
		},
		{
			"explicit -H Authorization wins over -u",
			[]string{"-u", "user:pass", "-H", "Authorization: Custom xyz", "https://x"},
			"Custom xyz",
		},
		{
			// The native flags resolve positionally, but -H is curl-exact and
			// order-independent: it wins even when it precedes the flag it beats.
			"explicit -H Authorization wins over -u, -H first",
			[]string{"-H", "Authorization: Custom xyz", "-u", "user:pass", "https://x"},
			"Custom xyz",
		},
		{
			"explicit -H Authorization wins over --oauth2-bearer, -H first",
			[]string{"-H", "Authorization: Custom xyz", "--oauth2-bearer", "TOK", "https://x"},
			"Custom xyz",
		},
		{"'-H Authorization:' suppresses -u", []string{"-u", "user:pass", "-H", "Authorization:", "https://x"}, ""},
		{"no auth flags means no Authorization", []string{"https://x"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := authOf(parse(t, tt.args...).Blocks[0]); got != tt.want {
				t.Errorf("Authorization = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("a block's auth overrides the preamble's", func(t *testing.T) {
		// Preamble bearer; block A inherits it, block B switches to its own Basic.
		plan := parse(t, "--oauth2-bearer", "TOK",
			"--block", "https://a",
			"--block", "https://b", "-u", "user:pass")
		if got := authOf(plan.Blocks[0]); got != "Bearer TOK" {
			t.Errorf("block A auth = %q, want inherited Bearer TOK", got)
		}
		if got := authOf(plan.Blocks[1]); got != "Basic dXNlcjpwYXNz" {
			t.Errorf("block B auth = %q, want its own Basic", got)
		}
	})

	t.Run("--delete-header Authorization removes -u auth", func(t *testing.T) {
		plan := parse(t, "-u", "user:pass",
			"--block", "https://a",
			"--block", "https://b", "--delete-header", "Authorization")
		if authOf(plan.Blocks[0]) == "" {
			t.Error("block A lost the inherited auth header")
		}
		if got := authOf(plan.Blocks[1]); got != "" {
			t.Errorf("block B still has auth after --delete-header: %q", got)
		}
	})

	t.Run("--delete-header composes positionally with the block's own -u", func(t *testing.T) {
		// delete-then-set re-adds the block's own auth; set-then-delete removes
		// it.
		keep := parse(t, "https://x", "--delete-header", "Authorization", "-u", "user:pass")
		if got := authOf(keep.Blocks[0]); got != "Basic dXNlcjpwYXNz" {
			t.Errorf("delete-then-u Authorization = %q, want the re-added Basic", got)
		}
		drop := parse(t, "https://x", "-u", "user:pass", "--delete-header", "Authorization")
		if got := authOf(drop.Blocks[0]); got != "" {
			t.Errorf("u-then-delete Authorization = %q, want none", got)
		}
	})
}

// TestUserAgent checks the User-Agent default baking and -A/--user-agent: the
// parser's default is applied unless overridden by -A or -H; an empty -A value
// removes the header (curl), "-A ' '" sends a blank value, -H wins over -A and
// the default regardless of order (it is curl-exact, unlike the positional -A),
// and --delete-header / -H 'User-Agent:' remove it. A parser with no
// WithUserAgent bakes none.
func TestUserAgent(t *testing.T) {
	uaOf := func(b Block) string {
		v, _ := lookupHeader(b.Headers, "User-Agent")
		return v
	}
	parse := func(t *testing.T, ua string, args ...string) *Plan {
		t.Helper()
		plan, err := New(WithUserAgent(ua)).Parse(args)
		if err != nil {
			t.Fatalf("parse error for %v: %v", args, err)
		}
		return plan
	}

	t.Run("default baked when nothing overrides", func(t *testing.T) {
		if got := uaOf(parse(t, "client/v1", "https://x").Blocks[0]); got != "client/v1" {
			t.Errorf("User-Agent = %q, want the default", got)
		}
	})
	t.Run("New empty bakes no default", func(t *testing.T) {
		if got := uaOf(parse(t, "", "https://x").Blocks[0]); got != "" {
			t.Errorf("User-Agent = %q, want none", got)
		}
	})
	t.Run("-A overrides the default", func(t *testing.T) {
		if got := uaOf(parse(t, "def", "-A", "custom/9", "https://x").Blocks[0]); got != "custom/9" {
			t.Errorf("User-Agent = %q, want custom/9", got)
		}
	})
	t.Run("-A single space sends a blank value", func(t *testing.T) {
		if got := uaOf(parse(t, "def", "-A", " ", "https://x").Blocks[0]); got != " " {
			t.Errorf("User-Agent = %q, want a single space", got)
		}
	})
	t.Run("-A empty removes the header", func(t *testing.T) {
		b := parse(t, "def", "-A", "", "https://x").Blocks[0]
		if got := uaOf(b); got != "" {
			t.Errorf("User-Agent = %q, want none", got)
		}
		if !b.SuppressesHeader("User-Agent") {
			t.Error("-A '' should suppress User-Agent")
		}
	})
	t.Run("-H wins over -A and the default", func(t *testing.T) {
		if got := uaOf(parse(t, "def", "-A", "from-A", "-H", "User-Agent: from-H", "https://x").Blocks[0]); got != "from-H" {
			t.Errorf("User-Agent = %q, want from-H", got)
		}
	})
	t.Run("-H wins over -A regardless of order (-H first)", func(t *testing.T) {
		// -A is positional, but -H is curl-exact and order-independent: it wins
		// even when it precedes the -A it beats.
		if got := uaOf(parse(t, "def", "-H", "User-Agent: from-H", "-A", "from-A", "https://x").Blocks[0]); got != "from-H" {
			t.Errorf("User-Agent = %q, want from-H", got)
		}
	})
	t.Run("-H 'User-Agent:' suppresses the default", func(t *testing.T) {
		if got := uaOf(parse(t, "def", "-H", "User-Agent:", "https://x").Blocks[0]); got != "" {
			t.Errorf("User-Agent = %q, want none (suppressed)", got)
		}
	})
	t.Run("--delete-header removes the default", func(t *testing.T) {
		plan := parse(t, "def", "--block", "https://a", "--block", "https://b", "--delete-header", "User-Agent")
		if got := uaOf(plan.Blocks[0]); got != "def" {
			t.Errorf("block A User-Agent = %q, want the default", got)
		}
		if got := uaOf(plan.Blocks[1]); got != "" {
			t.Errorf("block B User-Agent = %q, want none (deleted)", got)
		}
	})
	t.Run("a block's -A overrides the preamble's", func(t *testing.T) {
		plan := parse(t, "def", "-A", "pre/1", "--block", "https://a", "--block", "https://b", "-A", "blk/2")
		if got := uaOf(plan.Blocks[0]); got != "pre/1" {
			t.Errorf("block A User-Agent = %q, want pre/1 (inherited)", got)
		}
		if got := uaOf(plan.Blocks[1]); got != "blk/2" {
			t.Errorf("block B User-Agent = %q, want blk/2 (override)", got)
		}
	})
	t.Run("--delete-header composes positionally with -A", func(t *testing.T) {
		// delete-then-A keeps Foobar; A-then-delete drops it.
		keep := parse(t, "def", "--delete-header", "User-Agent", "-A", "Foobar", "https://x")
		if got := uaOf(keep.Blocks[0]); got != "Foobar" {
			t.Errorf("delete-then-A User-Agent = %q, want Foobar", got)
		}
		drop := parse(t, "def", "-A", "Foobar", "--delete-header", "User-Agent", "https://x")
		if got := uaOf(drop.Blocks[0]); got != "" {
			t.Errorf("A-then-delete User-Agent = %q, want none", got)
		}
	})
}

// TestResolveHeaderSetPreambleErrors covers the preamble arm of the header
// fold: a malformed -H is rejected wherever written, not only inside a block.
// The two scopes are resolved by separate passes, so a check that held for one
// says nothing about the other.
func TestResolveHeaderSetPreambleErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"in the preamble, with a block", []string{"-H", "JustAName", "--block", "https://a"}},
		{"in the preamble, no block opened", []string{"-H", "JustAName", "https://a"}},
		{"in a block", []string{"--block", "https://a", "-H", "JustAName"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New().Parse(tt.args)
			if err == nil || !strings.Contains(err.Error(), "must be in 'Name: value' form") {
				t.Errorf("Parse(%q) = %v, want the malformed header rejected", tt.args, err)
			}
		})
	}
}
