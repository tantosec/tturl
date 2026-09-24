package curlblocks

import (
	"reflect"
	"strings"
	"testing"
)

// TestResolveCookies exercises the cookie fold directly: specs stack in order,
// a single spec may list several ';'-separated pairs, whitespace around pairs
// and the '=' is trimmed, empty pairs (e.g. a trailing ';') are skipped, and an
// empty value yields an empty-valued cookie.
func TestResolveCookies(t *testing.T) {
	tests := []struct {
		name  string
		specs []string
		want  []Cookie
	}{
		{
			name:  "separate specs stack in order",
			specs: []string{"a=1", "b=2"},
			want:  []Cookie{{"a", "1"}, {"b", "2"}},
		},
		{
			name:  "one spec splits on ';' into several cookies",
			specs: []string{"a=1; b=2 ;c=3"},
			want:  []Cookie{{"a", "1"}, {"b", "2"}, {"c", "3"}},
		},
		{
			name:  "duplicate names stack",
			specs: []string{"a=1", "a=2"},
			want:  []Cookie{{"a", "1"}, {"a", "2"}},
		},
		{
			name:  "empty value yields an empty-valued cookie",
			specs: []string{"a=1", "a="},
			want:  []Cookie{{"a", "1"}, {"a", ""}},
		},
		{
			name:  "empty pairs from stray semicolons are skipped",
			specs: []string{"a=1;;", " ; b=2"},
			want:  []Cookie{{"a", "1"}, {"b", "2"}},
		},
		{
			name:  "value keeps interior '=' (split on first only)",
			specs: []string{"t=ab=="},
			want:  []Cookie{{"t", "ab=="}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveCookies(tt.specs)
			if err != nil {
				t.Fatalf("resolveCookies(%q) returned error: %v", tt.specs, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("resolveCookies(%q) = %+v, want %+v", tt.specs, got, tt.want)
			}
		})
	}
}

// TestResolveCookiesErrors covers the two malformed-pair rejections.
func TestResolveCookiesErrors(t *testing.T) {
	tests := []struct {
		name    string
		specs   []string
		wantSub string
	}{
		{name: "pair with no '='", specs: []string{"a=1; junk"}, wantSub: "must be in 'name=value' form"},
		{name: "pair with empty name", specs: []string{"=value"}, wantSub: "has an empty name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveCookies(tt.specs)
			if err == nil || !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("resolveCookies(%q) error = %v, want substring %q", tt.specs, err, tt.wantSub)
			}
		})
	}

	if _, err := New().Parse([]string{
		"--block", "https://a", "-b", "no-equals",
	}); err == nil || !strings.Contains(err.Error(), "name=value") {
		t.Errorf("Parse malformed cookie = %v, want a name=value error", err)
	}
}

// TestCookieHeader checks the resolved-cookies-to-header rendering: pairs join
// with "; ", an empty value is kept, and a block with no cookies yields "" so a
// caller can skip the header.
func TestCookieHeader(t *testing.T) {
	tests := []struct {
		name    string
		cookies []Cookie
		want    string
	}{
		{name: "no cookies yields empty", cookies: nil, want: ""},
		{name: "single pair", cookies: []Cookie{{"a", "1"}}, want: "a=1"},
		{name: "pairs join with '; '", cookies: []Cookie{{"a", "1"}, {"b", "2"}}, want: "a=1; b=2"},
		{name: "empty value is kept", cookies: []Cookie{{"a", ""}, {"b", "2"}}, want: "a=; b=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (Block{Cookies: tt.cookies}).CookieHeader(); got != tt.want {
				t.Errorf("CookieHeader() = %q, want %q", got, tt.want)
			}
		})
	}
}
