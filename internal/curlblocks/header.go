package curlblocks

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
)

// The header family resolves a block's request headers by folding the
// command-line occurrences of -H, -A/--user-agent, -u/--user, --oauth2-bearer,
// and --delete-header in order (preamble first, then the block). Explicit -H
// headers are curl-exact: an explicit header of a name beats the -A/-u/baked
// User-Agent defaults of that name regardless of order. Those defaults and
// --delete-header resolve positionally: --delete-header drops what precedes it,
// and a later -A/-u/-H of that name re-adds it. See resolveHeaderSet.

// Help text for the header family; each member renders on its own line (see
// fHeaders).
const (
	headerUsageMain = "add a header; 'Name;' sends an empty value and 'Name:' " +
		"suppresses a generated default"
	headerUsageUserAgent = "set User-Agent"
	headerUsageUser      = "set Basic authentication credentials"
	headerUsageOAuth     = "set OAuth 2 bearer token; overrides --user"
	headerUsageDelete    = "remove a header accumulated so far"
)

// The Authorization schemes -u and --oauth2-bearer render, kept as constants
// because mapTyped reads the user's own text back out from behind them.
const (
	basicScheme  = "Basic "
	bearerScheme = "Bearer "
)

// acceptAll is the Accept value curl sends on every request, which every block
// carries as a baked default: overridable with an explicit -H, removable with
// "-H 'Accept:'" or --delete-header, and yielded to by the Accept a body kind
// implies (--json's application/json), all as in curl.
const acceptAll = "*/*"

// headerOrigin records which flag produced a resolved header, which settles
// what text of it a --vary token may substitute into (see Header.mapTyped).
type headerOrigin uint8

const (
	// headerTyped is an explicit -H: the user wrote the name and the value.
	headerTyped headerOrigin = iota
	// headerValueTyped is a header a convenience flag named, carrying the value
	// the user gave verbatim (-A/--user-agent).
	headerValueTyped
	// headerBasic is the Authorization -u/--user implies: basicScheme followed by
	// base64 of the credentials the user typed.
	headerBasic
	// headerBearer is the Authorization --oauth2-bearer implies: bearerScheme
	// followed by the token the user typed.
	headerBearer
	// headerBaked is a default the parser supplied (see WithUserAgent). No part
	// of it is the user's text.
	headerBaked
)

// mapTyped returns the header with f applied to the text the command line
// supplied and to nothing else — the rule --vary substitution follows, so a
// token reaches what the user wrote and never what curlblocks derived from it.
//
// An explicit -H is the user's own name and value, so both are mapped. A header
// a convenience flag derived carries a name curlblocks chose, which is never
// mapped, and a value mapped only through the text behind it: -A's value as
// typed, --oauth2-bearer's token after the scheme, and -u's credentials from
// inside the base64, re-encoded afterwards. A baked default is nobody's typed
// text and is returned untouched.
func (h Header) mapTyped(f func(string) string) Header {
	switch h.origin {
	case headerTyped:
		h.Name, h.Value = f(h.Name), f(h.Value)
	case headerValueTyped:
		h.Value = f(h.Value)
	case headerBasic:
		if creds, ok := basicCredentials(h.Value); ok {
			h.Value = basicScheme + base64.StdEncoding.EncodeToString([]byte(f(creds)))
		}
	case headerBearer:
		if cred, ok := strings.CutPrefix(h.Value, bearerScheme); ok {
			h.Value = bearerScheme + f(cred)
		}
	case headerBaked:
		// A parser default is nobody's typed text.
	}
	return h
}

// basicCredentials returns the "user:password" text inside a Basic
// Authorization value. curlblocks built that value from the credentials the
// user typed, so this recovers exactly that text; ok is false for a value it
// did not build.
func basicCredentials(value string) (string, bool) {
	enc, ok := strings.CutPrefix(value, basicScheme)
	if !ok {
		return "", false
	}
	creds, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", false
	}
	return string(creds), true
}

// headerOccKind tags one recorded header-family flag occurrence, so the fold in
// resolveHeaderSet can replay it in command-line order.
type headerOccKind int

const (
	occHeader    headerOccKind = iota // -H spec: add / empty-value / suppress form
	occUserAgent                      // -A/--user-agent
	occUser                           // -u/--user
	occBearer                         // --oauth2-bearer
	occDelete                         // --delete-header
)

// headerOcc is one header-family flag occurrence, in command-line order.
type headerOcc struct {
	kind headerOccKind
	arg  string
}

// fHeaders is the single Shared built-in for the header family (-H, -A, -u,
// --oauth2-bearer, --delete-header). One ordered accumulator behind all five
// names is what lets resolveHeaderSet replay them in command-line order, so
// --delete-header composes positionally with -A/-u/-H.
var fHeaders = familyOption[headerOcc]{
	key: "header", argType: "string",
	members: []familyMember[headerOcc]{
		{
			flags:    []flagEntry{{name: "header", shorthand: "H"}},
			argument: "HEADER", usage: headerUsageMain,
			build: headerOccOf(occHeader),
		},
		{
			flags:    []flagEntry{{name: "user-agent", shorthand: "A"}},
			argument: "AGENT", usage: headerUsageUserAgent,
			build: headerOccOf(occUserAgent),
		},
		{
			flags:    []flagEntry{{name: "user", shorthand: "u"}},
			argument: "USER[:PASSWORD]", usage: headerUsageUser,
			build: headerOccOf(occUser),
		},
		{
			flags:    []flagEntry{{name: "oauth2-bearer"}},
			argument: "TOKEN", usage: headerUsageOAuth,
			build: headerOccOf(occBearer),
		},
		{
			flags:    []flagEntry{{name: "delete-header"}},
			argument: "NAME", usage: headerUsageDelete,
			build: headerOccOf(occDelete),
		},
	},
}

// headerOccOf builds one family member's recorder: every occurrence of that
// flag name lands in the shared accumulator tagged with its kind.
func headerOccOf(kind headerOccKind) func(string) headerOcc {
	return func(s string) headerOcc { return headerOcc{kind: kind, arg: s} }
}

func headerOccs(seg *segment) []headerOcc { return pieces[headerOcc](seg, "header") }

// SuppressesHeader reports whether the block asked, via a "-H 'Name:'" spec, to
// drop an auto-added header of the given name (matched case-insensitively, as
// HTTP field names are). A consumer that injects a default header, such as
// Content-Type for a request body, should skip it when suppressed. This
// mirrors curl, where "-H 'Name:'" removes an internal header rather than
// sending an empty one ("-H 'Name;'" sends an empty value); it never removes a
// user-supplied header.
func (b Block) SuppressesHeader(name string) bool {
	return slices.ContainsFunc(b.SuppressedHeaders, func(n string) bool { return strings.EqualFold(n, name) })
}

// resolveHeaderSet folds the preamble and block header-family occurrences into
// the resolved header list plus the names suppressed for a body's default
// headers. The baked defaults seed the fold, so any occurrence of one of those
// names overrides or removes it: curl's baked Accept always, and the
// parser's User-Agent (defaultUA, "" for none).
func resolveHeaderSet(pre, blk *segment, defaultUA string) ([]Header, []string, error) {
	f := newHeaderFold()
	f.setDefault(Header{Name: "Accept", Value: acceptAll, origin: headerBaked})
	if defaultUA != "" {
		f.setDefault(Header{Name: "User-Agent", Value: defaultUA, origin: headerBaked})
	}
	if err := f.applySegment(headerOccs(pre)); err != nil {
		return nil, nil, err
	}
	if err := f.applySegment(headerOccs(blk)); err != nil {
		return nil, nil, err
	}
	headers, suppressed := f.result()
	return headers, suppressed, nil
}

// headerCanon folds a header name to its case-insensitive key.
func headerCanon(name string) string { return strings.ToLower(name) }

// nameSet is an insertion-ordered set of header names: folded
// case-insensitively, but keeping the spelling first seen. Header names reach
// us from several places in one order and must come back in it without
// duplicates, which neither a map nor a plain slice gives on its own. The zero
// value is an empty set.
type nameSet struct {
	names []string
	seen  map[string]bool
}

func (s *nameSet) add(name string) {
	c := headerCanon(name)
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if !s.seen[c] {
		s.seen[c] = true
		s.names = append(s.names, name)
	}
}

// headerFold is the running state of the header fold. explicit holds the -H
// headers in order (duplicates kept); defaults holds the pending User-Agent and
// Authorization defaults; hSuppress records names suppressed by "-H 'Name:'"
// (sticky, curl-exact); deleteRemoved records names removed by --delete-header
// or an empty -A (positional, cleared by a later add or default). touched
// preserves the first-seen order of suppressed/removed names so result emits
// them stably.
type headerFold struct {
	explicit      []Header
	defaults      map[string]Header
	hSuppress     map[string]bool
	deleteRemoved map[string]bool
	touched       nameSet
}

func newHeaderFold() *headerFold {
	return &headerFold{
		defaults:      map[string]Header{},
		hSuppress:     map[string]bool{},
		deleteRemoved: map[string]bool{},
	}
}

func (f *headerFold) add(h Header) {
	f.explicit = append(f.explicit, h)
	delete(f.deleteRemoved, headerCanon(h.Name))
}

func (f *headerFold) suppressDefault(name string) {
	f.hSuppress[headerCanon(name)] = true
	f.touched.add(name)
}

func (f *headerFold) setDefault(h Header) {
	c := headerCanon(h.Name)
	f.defaults[c] = h
	delete(f.deleteRemoved, c)
}

// clearDefault drops a pending default and marks the name suppressed
// positionally; it backs an empty -A, which removes User-Agent without touching
// an explicit -H of that name. A later add or default re-establishes it.
func (f *headerFold) clearDefault(name string) {
	c := headerCanon(name)
	delete(f.defaults, c)
	f.deleteRemoved[c] = true
	f.touched.add(name)
}

// remove is clearDefault plus dropping every explicit header of the name so
// far; it backs --delete-header, which takes the user's own headers with it
// where an empty -A only clears the default it would have set.
func (f *headerFold) remove(name string) {
	c := headerCanon(name)
	f.explicit = slices.DeleteFunc(f.explicit, func(h Header) bool { return headerCanon(h.Name) == c })
	f.clearDefault(name)
}

// applySegment folds one segment's occurrences in order. Authorization is
// computed per segment (--oauth2-bearer beats -u, order-independent, matching
// curl) and applied at the segment's last auth-flag occurrence, so it composes
// positionally with --delete-header while -u/--oauth2-bearer keep
// bearer-always-wins.
func (f *headerFold) applySegment(occs []headerOcc) error {
	lastAuth := -1
	var user, bearer string
	// -u appearing at all is what counts, not its argument being non-empty: an
	// empty -u authenticates as an empty user and password, as in curl.
	userSet := false
	for i, o := range occs {
		switch o.kind {
		case occUser:
			user, userSet, lastAuth = o.arg, true, i
		case occBearer:
			bearer, lastAuth = o.arg, i
		}
	}
	auth, hasAuth := authHeader(user, userSet, bearer)

	for i, o := range occs {
		switch o.kind {
		case occHeader:
			h, suppress, err := parseHeaderSpec(o.arg)
			if err != nil {
				return err
			}
			if suppress {
				f.suppressDefault(h.Name)
			} else {
				f.add(h)
			}
		case occUserAgent:
			if o.arg == "" {
				f.clearDefault("User-Agent")
			} else {
				f.setDefault(Header{Name: "User-Agent", Value: o.arg, origin: headerValueTyped})
			}
		case occUser, occBearer:
			if i == lastAuth && hasAuth {
				f.setDefault(auth)
			}
		case occDelete:
			f.remove(o.arg)
		}
	}
	return nil
}

// result assembles the resolved headers (explicit, then each live, unsuppressed
// default whose name no explicit -H claims — curl-exact) and the
// suppressed-name list a body's default headers must skip.
func (f *headerFold) result() ([]Header, []string) {
	var headers []Header
	headers = append(headers, f.explicit...)
	for _, name := range []string{"Authorization", "User-Agent", "Accept"} {
		c := headerCanon(name)
		claimed := slices.ContainsFunc(f.explicit, func(h Header) bool { return headerCanon(h.Name) == c })
		if h, ok := f.defaults[c]; ok && !f.hSuppress[c] && !claimed {
			headers = append(headers, h)
		}
	}
	var suppressed []string
	for _, name := range f.touched.names {
		c := headerCanon(name)
		if f.hSuppress[c] || f.deleteRemoved[c] {
			suppressed = append(suppressed, name)
		}
	}
	return headers, suppressed
}

// authHeader renders the Authorization header a segment's auth flags imply:
// --oauth2-bearer wins over -u (as in curl, regardless of order), yielding
// "Bearer <token>"; otherwise a -u that was given (userSet) yields "Basic
// <base64(user:password)>", a bare "user" taking an empty password and an empty
// -u an empty pair, both as in curl. The header records which flag it came
// from, so a --vary token reaches the token or credentials the user typed
// rather than the encoding of them (see Header.mapTyped). ok is false when
// neither flag is set.
func authHeader(user string, userSet bool, bearer string) (h Header, ok bool) {
	switch {
	case bearer != "":
		return Header{Name: "Authorization", Value: bearerScheme + bearer, origin: headerBearer}, true
	case userSet:
		if !strings.Contains(user, ":") {
			user += ":"
		}
		enc := base64.StdEncoding.EncodeToString([]byte(user))
		return Header{Name: "Authorization", Value: basicScheme + enc, origin: headerBasic}, true
	}
	return Header{}, false
}

// parseHeaderSpec parses one -H spec into a header or a default-suppression,
// matching curl's three forms. "Name: value" is a header (leading OWS after the
// colon dropped, trailing whitespace kept as curl does); "Name;" is an
// empty-valued header; "Name:" (nothing but optional whitespace after the
// colon) suppresses an auto-added header of that name rather than sending one.
func parseHeaderSpec(spec string) (h Header, suppress bool, err error) {
	if before, after, ok := strings.Cut(spec, ":"); ok {
		name := strings.TrimSpace(before)
		if name == "" {
			return Header{}, false, fmt.Errorf("header %q has an empty name", spec)
		}
		if value := strings.TrimLeft(after, " \t"); value != "" {
			return Header{Name: name, Value: value}, false, nil
		}
		return Header{Name: name}, true, nil
	}
	name, ok := strings.CutSuffix(strings.TrimSpace(spec), ";")
	if !ok {
		return Header{}, false, fmt.Errorf("header %q must be in 'Name: value' form "+
			"('Name;' for an empty value, 'Name:' to drop an auto-added header)", spec)
	}
	if name = strings.TrimSpace(name); name == "" {
		return Header{}, false, fmt.Errorf("header %q has an empty name", spec)
	}
	return Header{Name: name, Value: ""}, false, nil
}
