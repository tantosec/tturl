package curlblocks

import (
	"fmt"
	"strings"
)

// resolveCookies folds a sequence of "-b/--cookie" specs left to right into an
// accumulating list of cookies, so cookies stack (the preamble carries the
// cookies shared by every block and a block adds its own). A single spec may
// list several cookies separated by ';' (as a browser's Cookie header does), so
// "a=1; b=2" contributes two. Whitespace around each pair and around its '=' is
// trimmed and an empty pair (e.g. from a trailing ';') is skipped; a pair's
// value may be empty ("foo=").
func resolveCookies(specs []string) ([]Cookie, error) {
	var out []Cookie
	for _, spec := range specs {
		for pair := range strings.SplitSeq(spec, ";") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			before, after, ok := strings.Cut(pair, "=")
			if !ok {
				return nil, fmt.Errorf("cookie %q must be in 'name=value' form", pair)
			}
			name := strings.TrimSpace(before)
			if name == "" {
				return nil, fmt.Errorf("cookie %q has an empty name", pair)
			}
			out = append(out, Cookie{Name: name, Value: strings.TrimSpace(after)})
		}
	}
	return out, nil
}

// CookieHeader renders the block's cookies as a single Cookie header value —
// "name=value" pairs joined by "; ", the form curl and browsers send. Cookies
// collapse into one header (unlike Headers, which map one-to-one to field
// lines), so consumers building a request call this rather than iterating.
// It returns "" when the block carries no cookies, letting a caller skip
// emitting the header entirely.
func (b Block) CookieHeader() string {
	if len(b.Cookies) == 0 {
		return ""
	}
	pairs := make([]string, len(b.Cookies))
	for i, c := range b.Cookies {
		pairs[i] = c.Name + "=" + c.Value
	}
	return strings.Join(pairs, "; ")
}
