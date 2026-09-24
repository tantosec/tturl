package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestReleaseNotesBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		current string
		tags    []string
		want    string
	}{
		{"first release", "v1.0.0-rc.1", nil, ""},
		{
			"failed tag absent from published releases",
			"v1.0.0",
			[]string{"v0.8.0"},
			"v0.8.0",
		},
		{"first candidate", "v1.0.0-rc.1", []string{"v0.9.0"}, "v0.9.0"},
		{"later candidate", "v1.0.0-rc.3", []string{
			"v0.9.0", "v1.0.0-rc.1", "v1.0.0-rc.2",
		}, "v1.0.0-rc.2"},
		{"stable rolls up candidates", "v1.0.0", []string{
			"v0.9.0", "v1.0.0-rc.1", "v1.0.0-rc.2",
		}, "v0.9.0"},
		{"patch after stable", "v1.0.1", []string{
			"v0.9.0", "v1.0.0", "v1.0.1-rc.1",
		}, "v1.0.0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := releaseNotesBase(test.current, test.tags)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Errorf("base = %q, want %q", got, test.want)
			}
		})
	}
}

func TestReleaseNotesBaseRejectsStaleTag(t *testing.T) {
	t.Parallel()
	if _, err := releaseNotesBase("v1.0.0-rc.1", []string{"v1.0.0"}); err == nil {
		t.Fatal("releaseNotesBase accepted a tag older than repository history")
	}
}

func TestRenderReleaseNotes(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(`[
  {
    "number": 8,
    "title": "Later [change]",
    "html_url": "https://github.com/tantosec/tturl/pull/8",
    "merged_at": "2026-09-02T02:00:00Z",
    "base": {"ref": "main"},
    "user": {"login": "octocat"}
  },
  {
    "number": 7,
    "title": "Earlier change",
    "html_url": "https://github.com/tantosec/tturl/pull/7",
    "merged_at": "2026-09-02T01:00:00Z",
    "base": {"ref": "main"},
    "user": {"login": "maintainer"}
  }
]
[
  {
    "number": 7,
    "title": "Earlier change",
    "html_url": "https://github.com/tantosec/tturl/pull/7",
    "merged_at": "2026-09-02T01:00:00Z",
    "base": {"ref": "main"},
    "user": {"login": "maintainer"}
  }
]`)
	var output bytes.Buffer
	if err := renderReleaseNotes(
		input, &output, "tantosec/tturl",
	); err != nil {
		t.Fatal(err)
	}
	want := `## Changes

- [#7: Earlier change](https://github.com/tantosec/tturl/pull/7) by @maintainer
- [#8: Later \[change\]](https://github.com/tantosec/tturl/pull/8) by @octocat
`
	if output.String() != want {
		t.Errorf("notes = %q, want %q", output.String(), want)
	}
}

func TestRenderEmptyReleaseNotes(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if err := renderReleaseNotes(
		strings.NewReader(""), &output, "tantosec/tturl",
	); err != nil {
		t.Fatal(err)
	}
	want := "## Changes\n\nNo pull requests were merged for this release.\n"
	if output.String() != want {
		t.Errorf("notes = %q, want %q", output.String(), want)
	}
}

func TestRenderReleaseNotesRejectsUntrustedMetadata(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(`[{
  "number": 7,
  "title": "Change",
  "html_url": "https://example.com/steal",
  "merged_at": "2026-09-02T01:00:00Z",
  "base": {"ref": "main"},
  "user": {"login": "attacker"}
}]`)
	if err := renderReleaseNotes(
		input, io.Discard, "tantosec/tturl",
	); err == nil {
		t.Fatal("renderReleaseNotes accepted an unexpected URL")
	}
}
