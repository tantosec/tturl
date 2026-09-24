package curlblocks

import "testing"

// TestEnsureScheme checks scheme-filling: an explicit scheme is left untouched
// whatever the default, a schemeless URL gains the default scheme, a "://"
// inside a query is not mistaken for a scheme, and an empty default opts out.
// RFC 3986 requires a scheme to begin with a letter and permits digits, '+',
// '-', and '.' only after it.
func TestEnsureScheme(t *testing.T) {
	tests := []struct{ in, scheme, want string }{
		{"https://example.com/p", "http", "https://example.com/p"},
		{"http://example.com/p", "https", "http://example.com/p"},
		{"example.com/p", "http", "http://example.com/p"},
		{"example.com/p", "https", "https://example.com/p"},
		{"127.0.0.1:9000/x", "https", "https://127.0.0.1:9000/x"},
		{"user:pass@host/p", "https", "https://user:pass@host/p"},
		{"host/p?next=http://elsewhere", "https", "https://host/p?next=http://elsewhere"},
		{"example.com/p", "", "example.com/p"}, // empty default: verbatim
		{"foo+bar://h/", "https", "foo+bar://h/"},
		{"a.b-c://h/", "https", "a.b-c://h/"},
		{"2fast://h/", "https", "https://2fast://h/"},
		{"+x://h/", "https", "https://+x://h/"},
	}
	for _, tt := range tests {
		if got := ensureScheme(tt.in, tt.scheme); got != tt.want {
			t.Errorf("ensureScheme(%q, %q) = %q, want %q", tt.in, tt.scheme, got, tt.want)
		}
	}
}

// TestDefaultSchemeAtParse checks the default scheme is applied to a block's
// URLs at parse: the built-in default is http (curl), WithDefaultScheme picks
// another, and "" stores URLs verbatim.
func TestDefaultSchemeAtParse(t *testing.T) {
	plan, err := New().Parse([]string{"--block", "example.com/p", "--block", "other/q"})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Blocks[0].URLs[0]; got != "http://example.com/p" {
		t.Errorf("block 0 URL = %q, want http://example.com/p (built-in default)", got)
	}
	if got := plan.Blocks[1].URLs[0]; got != "http://other/q" {
		t.Errorf("block 1 URL = %q, want http://other/q (built-in default)", got)
	}

	plan, err = New(WithDefaultScheme("https")).Parse([]string{"example.com/p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Blocks[0].URLs[0]; got != "https://example.com/p" {
		t.Errorf("URL = %q, want https://example.com/p (WithDefaultScheme)", got)
	}

	plan, err = New(WithDefaultScheme("")).Parse([]string{"example.com/p"})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Blocks[0].URLs[0]; got != "example.com/p" {
		t.Errorf("URL = %q, want example.com/p (verbatim)", got)
	}
}
