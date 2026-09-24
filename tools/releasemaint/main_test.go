package main

import (
	"bytes"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestValidateVersion(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"0.0.4", "1.2.3-rc.1", "1.2.3-rc.10"} {
		if err := validateVersion(version); err != nil {
			t.Errorf("validateVersion(%q) error = %v", version, err)
		}
	}
	for _, version := range []string{
		"v0.0.4",
		"01.2.3",
		"1.2",
		"1.2.3+local",
		"1.2.3-beta.1",
		"1.2.3-rc1",
		"1.2.3-rc.0",
		"1.2.3-rc.01",
	} {
		if err := validateVersion(version); err == nil {
			t.Errorf("validateVersion(%q) succeeded", version)
		}
	}
}

func TestReleaseComparisons(t *testing.T) {
	writeVersionFile(t, "1.2.3")
	tests := []struct {
		name    string
		mode    string
		version string
		tags    []string
		wantErr string
	}{
		{"prepare patch", "prepare", "1.2.4", []string{"v1.2.3"}, ""},
		{"prepare rc", "prepare", "1.3.0-rc.1", []string{"v1.2.3"}, ""},
		{"equal baseline", "prepare", "1.2.3", nil, "newer"},
		{"older baseline", "prepare", "1.2.2", nil, "newer"},
		{"duplicate tag", "prepare", "1.2.4", []string{"v1.2.4"}, "does not advance"},
		{"older than tag", "prepare", "1.2.4", []string{"v1.3.0-rc.1"}, "does not advance"},
		{"check release", "check", "1.2.3", []string{"v1.2.2", "v1.2.3"}, ""},
		{"check mismatched intent", "check", "1.2.4", nil, "does not match"},
		{"check stale release", "check", "1.2.3", []string{"v1.2.4"}, "does not advance"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var err error
			switch test.mode {
			case "prepare":
				err = checkPreparation(test.version, test.tags)
			case "check":
				err = checkRelease(test.version, test.tags)
			}
			if test.wantErr == "" && err != nil {
				t.Fatal(err)
			}
			if test.wantErr != "" &&
				(err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestReleaseCandidateOrdering(t *testing.T) {
	writeVersionFile(t, "2.0.0-rc.8")
	if err := checkPreparation("2.0.0-rc.10", []string{"v2.0.0-rc.9"}); err != nil {
		t.Fatal(err)
	}
	writeVersionFile(t, "2.0.0-rc.10")
	if err := checkPreparation("2.0.0", []string{"v2.0.0-rc.10"}); err != nil {
		t.Fatal(err)
	}
}

func TestReadTagsRejectsMalformedReleaseTag(t *testing.T) {
	t.Parallel()
	if _, err := readTags(strings.NewReader("v1.2.3\nv1.2.4-beta.1\n")); err == nil {
		t.Fatal("readTags accepted a malformed release tag")
	}
}

func TestSetVersion(t *testing.T) {
	writeVersionFile(t, "0.0.3")
	before, err := os.ReadFile(citationPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := setVersion(io.Discard, "0.0.4"); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(contents), "0.0.4\n"; got != want {
		t.Errorf("contents = %q, want %q", got, want)
	}
	info, err := os.Stat(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	// Windows exposes only its read-only file attribute through FileMode, so it
	// cannot report the owner-only permissions requested by setVersion.
	got, want := info.Mode().Perm(), os.FileMode(0o600)
	if runtime.GOOS != "windows" && got != want {
		t.Errorf("mode = %v, want %v", got, want)
	}
	after, err := os.ReadFile(citationPath)
	if err != nil {
		t.Fatal(err)
	}
	before0, after0, ok := bytes.Cut(before, []byte("0.0.3"))
	before00, after00, ok0 := bytes.Cut(after, []byte("0.0.4"))
	if !ok || !ok0 {
		t.Fatalf("citation update missing: before %q, after %q", before, after)
	}
	if !bytes.Equal(before0, before00) ||
		!bytes.Equal(after0, after00) {
		t.Fatal("setVersion changed citation bytes outside the version scalar")
	}
}

func TestSetVersionReleaseCandidateAndNoOp(t *testing.T) {
	writeVersionFile(t, "1.2.3")
	if err := setVersion(io.Discard, "1.3.0-rc.1"); err != nil {
		t.Fatal(err)
	}
	beforeVersion, err := os.ReadFile(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeCitation, err := os.ReadFile(citationPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := setVersion(io.Discard, "1.3.0-rc.1"); err != nil {
		t.Fatal(err)
	}
	afterVersion, err := os.ReadFile(versionPath)
	if err != nil {
		t.Fatal(err)
	}
	afterCitation, err := os.ReadFile(citationPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeVersion, afterVersion) || !bytes.Equal(beforeCitation, afterCitation) {
		t.Fatal("no-op update changed version metadata")
	}
}

func TestCitationVersionRepresentation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		contents string
	}{
		{"missing", "cff-version: 1.2.0\n"},
		{"duplicate", "version: 1.2.3\nversion: 1.2.3\n"},
		{"no separator", "version:1.2.3\n"},
		{"quoted", "version: '1.2.3'\n"},
		{"tagged", "version: !!str 1.2.3\n"},
		{"anchored", "version: &release 1.2.3\n"},
		{"commented", "version: 1.2.3 # release\n"},
		{"indented", "  version: 1.2.3\n"},
		{"non-canonical", "version: 01.2.3\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseCitationVersion([]byte(test.contents)); err == nil {
				t.Fatalf("parseCitationVersion(%q) succeeded", test.contents)
			}
		})
	}
}

func TestCurrentVersionRejectsCitationDrift(t *testing.T) {
	writeVersionFile(t, "1.2.3")
	if err := os.WriteFile(citationPath, []byte("version: 1.2.4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := currentVersion(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("currentVersion error = %v, want mismatch", err)
	}
}

func TestVerifyReleaseReport(t *testing.T) {
	t.Parallel()
	const revision = "0123456789abcdef0123456789abcdef01234567"
	report := []byte(
		`{"kind":"run","tool":{"version":"v1.2.3",` +
			`"revision":"` + revision + `","modified":false}}` + "\n" +
			`{"kind":"request","headers":{"user-agent":["tturl/v1.2.3"]}}` + "\n",
	)
	if err := verifyReleaseReport(report, "1.2.3", revision); err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseReport(report, "1.2.4", revision); err == nil {
		t.Fatal("verifyReleaseReport accepted a mismatched version")
	}
}

func writeVersionFile(t *testing.T, version string) {
	t.Helper()
	directory := t.TempDir()
	t.Chdir(directory)
	if err := os.Mkdir("release", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(versionPath, []byte(version+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		citationPath,
		[]byte("cff-version: 1.2.0\nversion: "+version+"\nmessage: example\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}
