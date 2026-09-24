package curlblocks

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// requirePanicContains runs fn and requires its panic to be a string containing
// want. Consumer-misuse panics are part of this package's contract, so their
// tests must distinguish a missing or wrong panic from the expected one.
func requirePanicContains(t *testing.T, want string, fn func()) {
	t.Helper()
	var got any
	func() {
		defer func() { got = recover() }()
		fn()
	}()
	if got == nil {
		t.Fatal("call did not panic")
	}
	msg, ok := got.(string)
	if !ok || !strings.Contains(msg, want) {
		t.Fatalf("panic = %v, want a string containing %q", got, want)
	}
}

// noFiles is a readFile that fails any read, for sources that touch no file.
func noFiles(name string) ([]byte, error) {
	return nil, errors.New("no such file: " + name)
}

// fakeFS returns a readFile serving file contents by name, erroring (with the
// name) on any name absent from the map. fakeFS(nil) always misses.
func fakeFS(files map[string]string) func(string) ([]byte, error) {
	return func(name string) ([]byte, error) {
		if s, ok := files[name]; ok {
			return []byte(s), nil
		}
		return nil, fmt.Errorf("open %s: not found", name)
	}
}

// flattenSpace collapses every run of whitespace to a single space, so an
// assertion about help text holds whatever column Usage wrapped it at.
func flattenSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// acceptBaseline is the Accept default resolveHeaderSet bakes into every block
// it builds, which lands last in the resolved header list.
func acceptBaseline() Header {
	return Header{Name: "Accept", Value: acceptAll, origin: headerBaked}
}

// lookupHeader returns the named header's value (case-insensitive) and whether
// it is present.
func lookupHeader(hs []Header, name string) (string, bool) {
	for _, h := range hs {
		if strings.EqualFold(h.Name, name) {
			return h.Value, true
		}
	}
	return "", false
}

// parseTLS parses against the built-in baseline plus --insecure/-k, which is
// opt-in (see WithInsecureFlag) and which the cases below exercise. It is
// otherwise a plain New() parser, so a plan it builds compares equal to one
// parsed with no options at all.
func parseTLS(args []string) (*Plan, error) {
	return New(WithInsecureFlag()).Parse(args)
}
