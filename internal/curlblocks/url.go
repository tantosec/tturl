package curlblocks

import "strings"

// defaultSchemeHTTP is the scheme a Parser prepends to a schemeless URL unless
// a consumer sets another with WithDefaultScheme. It matches curl's plain-HTTP
// assumption; curl's further ftp./imap. hostname heuristics are not reproduced.
const defaultSchemeHTTP = "http"

// ensureScheme returns url unchanged if it already carries a "<scheme>://"
// prefix, else prepends scheme + "://". An empty scheme leaves the URL
// verbatim, so a consumer can opt out of scheme-filling entirely.
func ensureScheme(url, scheme string) string {
	if scheme == "" || hasScheme(url) {
		return url
	}
	return scheme + "://" + url
}

// hasScheme reports whether url begins with a "<scheme>://" prefix, where
// scheme is ALPHA *( ALPHA / DIGIT / "+" / "-" / "." ) per RFC 3986. Checking
// the scheme grammar rather than a bare "://" search avoids mistaking a "://"
// inside a query — e.g. "host/p?next=http://x" — for a scheme.
func hasScheme(url string) bool {
	i := strings.Index(url, "://")
	if i <= 0 {
		return false
	}
	for j := range i {
		c := url[j]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.':
			if j == 0 {
				return false // scheme must start with a letter
			}
		default:
			return false
		}
	}
	return true
}
