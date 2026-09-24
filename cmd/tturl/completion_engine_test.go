package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/tantosec/tturl/internal/curlblocks"
)

func TestCompletionRootAndMetaCommands(t *testing.T) {
	assertCompletionValues(t, []string{toolName, ""},
		"race", "measure", "analyse", "detect", "demo-server",
		"help", "completion", "-h", "--help", "-V", "--version")
	assertCompletionValues(t, []string{toolName, "completion", ""},
		"bash", "zsh", "fish", "-h", "--help")

	for _, topic := range helpTopics {
		assertCompletionValues(t, []string{toolName, "help", topic.name[:1]},
			topic.name)
	}
}

func TestCompletionDoesNotAdvertiseHiddenVersion(t *testing.T) {
	assertCompletionOmits(t, []string{toolName, ""}, "version")
	assertCompletionOmits(t, []string{toolName, "v"}, "version")
}

func TestCompletionRespectsRequestScopesAndValues(t *testing.T) {
	assertCompletionValues(t, []string{toolName, "race", ""},
		"--trials", "--header", "--block", "--help")
	assertCompletionOmits(t, []string{toolName, "race", ""},
		"--warmup-only")

	block := []string{toolName, "race", "--block", "https://example.test", ""}
	assertCompletionValues(t, block,
		"--header", "--warmup-only", "--block", "--help")
	assertCompletionOmits(t, block, "--trials", "--output")
	multipleBlocks := []string{
		toolName, "race", "--block", "https://one.example",
		"--data", "one", "--block", "https://two.example", "",
	}
	assertCompletionValues(t, multipleBlocks,
		"--header", "--warmup-only", "--block", "--help")
	assertCompletionOmits(t, multipleBlocks, "--trials", "--output")
	assertCompletionOmits(t,
		append(multipleBlocks[:len(multipleBlocks)-1], "--report=j"),
		"--report=json")

	assertCompletionValues(t,
		[]string{toolName, "measure", "--arrange", "r"},
		"rotate", "random")
	assertCompletionOmits(t,
		[]string{toolName, "measure", "--arrange", "r"}, "--report")
	assertCompletionValues(t,
		[]string{toolName, "detect", "--direction=e"},
		"--direction=early", "--direction=either")
	assertCompletionValues(t,
		[]string{toolName, "detect", "--direction", ""},
		"early", "late", "either", "fast", "slow")
	assertCompletionValues(t,
		[]string{toolName, "detect", "--strategy", ""},
		"auto", "peer-first", "rolling-peer-first", "baseline-confirmed",
		"rolling-baseline-confirmed", "baseline-reserved",
		"rolling-baseline-reserved")
	assertCompletionValues(t,
		[]string{toolName, "detect", "--block", "https://example.test", ""},
		"--baseline-only", "--baseline-supply")
	assertCompletionValues(t,
		[]string{
			toolName, "detect", "--block", "https://example.test",
			"--baseline-only", "--baseline-supply", "u",
		},
		unlimitedFlagValue)
	assertCompletionValues(t,
		[]string{toolName, "detect", "--rank-rows", "a"}, "all")
	assertCompletionValues(t,
		[]string{toolName, "measure", "--rank-rows=a"}, "--rank-rows=all")
	assertCompletionValues(t,
		[]string{toolName, "race", "--report", "j"}, "json")
	assertCompletionValues(t,
		[]string{toolName, "race", "--capture-body", "u"}, unlimitedFlagValue)
	assertCompletionValues(t,
		[]string{toolName, "analyse", "--capture-body=u"},
		"--capture-body="+unlimitedFlagValue)
	assertCompletionValues(t,
		[]string{toolName, "race", "--trials", "u"}, unlimitedFlagValue)
	assertCompletionValues(t,
		[]string{toolName, "detect", "--comparisons-max=u"},
		"--comparisons-max="+unlimitedFlagValue)
	assertCompletionValues(t,
		[]string{toolName, "measure", "--connections-fit-max", "u"},
		unlimitedFlagValue)
	assertCompletionValues(t,
		[]string{toolName, "measure", "--batch-rate-max", "u"}, unlimitedFlagValue)
	assertCompletionValues(t,
		[]string{toolName, "detect", "--request-rate-max=u"},
		"--request-rate-max="+unlimitedFlagValue)
	assertCompletionValues(t,
		[]string{toolName, "race", "--vary-mode", "p"}, "pitchfork")
}

func TestCompletionHandlesRepeatedFamilyFlags(t *testing.T) {
	words := []string{
		toolName, "race", "--data", "one", "--header", "A: b",
		"--data-binary", "two", "",
	}
	assertCompletionValues(t, words,
		"--data", "--data-ascii", "--data-binary", "--header")
}

func TestCompletionOffersPeerStreamLimitOptOut(t *testing.T) {
	assertCompletionValues(t,
		[]string{toolName, "race", "--ignore-p"},
		"--ignore-peer-stream-limit")
}

func TestCompletionStopsAfterOptionTerminator(t *testing.T) {
	assertCompletionOmits(t,
		[]string{toolName, "race", "--", "--r"},
		"--report", "--request", "--repeat", "--release-delay")
	if got := completionValues(
		[]string{toolName, "race", "--", "--r"}); len(got) != 0 {
		t.Errorf("completion after -- = %q, want no positional candidates", got)
	}
}

func TestCompletionOffersOnlySelectedFileForms(t *testing.T) {
	dir := t.TempDir()
	name := "needed.txt"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(dir, "nee")

	assertCompletionValues(t,
		[]string{toolName, "race", "--output", prefix},
		filepath.Join(dir, name))
	assertCompletionOmits(t,
		[]string{toolName, "race", "--output", ""}, "-")
	assertCompletionValues(t,
		[]string{toolName, "race", "--data", "@" + prefix},
		"@"+filepath.Join(dir, name))
	assertCompletionValues(t,
		[]string{toolName, "race", "--form", "body=@" + prefix},
		"body=@"+filepath.Join(dir, name))
	assertCompletionValues(t,
		[]string{toolName, "race", "--vary", "TOKEN=@" + prefix},
		"TOKEN=@"+filepath.Join(dir, name))
	assertCompletionOmits(t,
		[]string{toolName, "race", "--data", prefix},
		filepath.Join(dir, name))
	assertCompletionOmits(t,
		[]string{toolName, "race", prefix}, filepath.Join(dir, name))
}

func TestCompletionDistinguishesFilesAndDirectories(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	candidates := completeCommandLine([]string{
		toolName, "race", "--output", dir + string(filepath.Separator),
	}, 3)
	assertPathCandidate(t, candidates,
		nested+string(filepath.Separator), false, true)
	assertPathCandidate(t, candidates, file, true, false)
}

func assertPathCandidate(
	t *testing.T,
	candidates []completionCandidate,
	value string,
	file, directory bool,
) {
	t.Helper()
	for _, candidate := range candidates {
		if candidate.value != value {
			continue
		}
		if candidate.description != "" || candidate.file != file ||
			candidate.directory != directory {
			t.Errorf("path candidate %#v, want file=%t directory=%t and no description",
				candidate, file, directory)
		}
		return
	}
	t.Errorf("path candidates %#v omit %q", candidates, value)
}

func TestCompletionRejectsTerminalControlsInFileNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not permit control characters in file names")
	}
	dir := t.TempDir()
	for _, name := range []string{
		"escape-\x1b[31m.txt",
		"delete-\x7f.txt",
		"c1-\u009b.txt",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		assertCompletionOmits(t,
			[]string{toolName, "race", "--output", filepath.Join(dir, name[:2])},
			filepath.Join(dir, name))
	}

	ordinary := "caf\u00e9.txt"
	if err := os.WriteFile(filepath.Join(dir, ordinary), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	assertCompletionValues(t,
		[]string{toolName, "race", "--output", filepath.Join(dir, "caf")},
		filepath.Join(dir, ordinary))
}

func TestSafeCompletionValue(t *testing.T) {
	for _, value := range []string{"escape\x1b", "delete\x7f", "c1\u009b"} {
		if safeCompletionValue(value) {
			t.Errorf("safeCompletionValue(%q) = true", value)
		}
	}
	for _, value := range []string{"plain name", "caf\u00e9", `quote'"\\`} {
		if !safeCompletionValue(value) {
			t.Errorf("safeCompletionValue(%q) = false", value)
		}
	}
}

func TestEveryParserOptionCanBeCompletedInItsScope(t *testing.T) {
	for i := range commandCatalogue {
		spec := &commandCatalogue[i]
		if !spec.requestGrammar {
			continue
		}
		options := describeRequestCommand(spec).CompletionOptions()
		for _, option := range options {
			words := []string{toolName, spec.name, ""}
			if option.Scope == curlblocks.CompletionBlock {
				words = []string{toolName, spec.name, "--block", ""}
			}
			for _, name := range option.Names {
				if !slices.Contains(completionValues(words), name) {
					t.Errorf("%s %s does not complete in scope %d",
						spec.name, name, option.Scope)
				}
			}
		}
	}
}

func TestDemoServerCompletionComesFromItsFlagSet(t *testing.T) {
	for _, option := range demoServerCompletionOptions() {
		for _, name := range option.Names {
			assertCompletionValues(t,
				[]string{toolName, "demo-server", name[:1]}, name)
		}
	}
	assertCompletionValues(t,
		[]string{toolName, "demo-server", "--timing-work-max", "u"},
		unlimitedFlagValue)
	assertCompletionValues(t,
		[]string{toolName, "demo-server", "--timing-work-max=u"},
		"--timing-work-max="+unlimitedFlagValue)
}

func assertCompletionValues(t *testing.T, words []string, want ...string) {
	t.Helper()
	got := completionValues(words)
	for _, value := range want {
		if !slices.Contains(got, value) {
			t.Errorf("complete(%q) = %q, want %q", words, got, value)
		}
	}
}

func assertCompletionOmits(t *testing.T, words []string, unwanted ...string) {
	t.Helper()
	got := completionValues(words)
	for _, value := range unwanted {
		if slices.Contains(got, value) {
			t.Errorf("complete(%q) = %q, unexpectedly contains %q",
				words, got, value)
		}
	}
}

func completionValues(words []string) []string {
	candidates := completeCommandLine(words, len(words)-1)
	values := make([]string, len(candidates))
	for i, candidate := range candidates {
		values[i] = candidate.value
		if strings.ContainsAny(candidate.description, "\r\n") {
			panic("completion description contains a line break")
		}
	}
	return values
}
