package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRankingQualificationPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want bool
	}{
		{"internal/ranking/solver.go", true},
		{"internal/ranking/doc/qualification.md", true},
		{"stats/binomial.go", true},
		{"go.mod", true},
		{"go.sum", true},
		{"go.work", true},
		{"go.work.sum", true},
		{"Makefile", true},
		{"CITATION.cff", true},
		{"release/version.txt", true},
		{".github/workflows/ci.yaml", true},
		{".github/workflows/ranking-qualification.yaml", true},
		{".github/workflows/release.yaml", true},
		{"tools/ciimpact/main.go", true},
		{"internal/ranking-old/solver.go", false},
		{"statistics/binomial.go", false},
		{"doc/ranking.md", false},
		{"unrelated/sample.go", false},
		{"tth2/client.go", false},
		{"cmd/tturl/main.go", false},
		{".github/workflows/ranking-qualification-weekly.yaml", false},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			normalised, ok := normaliseRepositoryPath(test.path)
			if !ok {
				t.Fatal("valid repository path was rejected")
			}
			if got := rankingQualificationPath(normalised); got != test.want {
				t.Errorf("rankingQualificationPath(%q) = %t, want %t",
					test.path, got, test.want)
			}
		})
	}
}

func TestReleaseIntentDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		input       string
		wantChanged bool
		wantErr     bool
	}{
		{"neither", "doc/releases.md\x00", false, false},
		{"both", "CITATION.cff\x00release/version.txt\x00", true, false},
		{"version only", "release/version.txt\x00", true, false},
		{"citation only", "CITATION.cff\x00", true, false},
		{"invalid input", "CITATION.cff", false, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			changed, err := classifyReleaseIntentInput(strings.NewReader(test.input))
			if changed != test.wantChanged || (err != nil) != test.wantErr {
				t.Fatalf("changed = %t, error = %v", changed, err)
			}
		})
	}
}

func TestRankingQualificationDecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		input      string
		required   bool
		wantReason string
	}{
		{"affected", "doc/readme.md\x00stats/mean.go\x00", true, "stats/mean.go"},
		{"deleted affected path", "internal/ranking/removed.go\x00", true, "internal/ranking/removed.go"},
		{"rename pair", "internal/ranking/old.go\x00doc/new.go\x00", true, "internal/ranking/old.go"},
		{"unaffected", "doc/readme.md\x00unrelated/sample.go\x00", false, "2 changed"},
		{"windows separator", "stats\\mean.go\x00", true, "stats/mean.go"},
		{"empty", "", true, "empty"},
		{"unterminated", "internal/ranking/solver.go", true, "invalid"},
		{"empty path", "\x00", true, "invalid"},
		{"absolute path", "/internal/ranking/solver.go\x00", true, "invalid"},
		{"parent traversal", "../internal/ranking/solver.go\x00", true, "invalid"},
		{"oversized path", strings.Repeat("a", maxPathBytes+1) + "\x00", true, "invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := classifyRankingQualificationInput(strings.NewReader(test.input))
			if decision.Required != test.required ||
				!strings.Contains(decision.Reason, test.wantReason) {
				t.Fatalf("decision = %+v", decision)
			}
		})
	}
}

func TestRun(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if err := run([]string{"ranking-qualification"},
		strings.NewReader("internal/ranking/solver.go\x00"), &output); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(),
		"required=true\nreason=affected path: internal/ranking/solver.go\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if err := run(nil, strings.NewReader(""), &output); err == nil {
		t.Fatal("missing mode succeeded")
	}
}
