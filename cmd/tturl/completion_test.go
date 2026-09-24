package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionCommandGeneratesEachShell(t *testing.T) {
	for _, shell := range completionShells {
		t.Run(shell, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, err := runCompletionCommand([]string{shell}, commandOutput{
				stdout: &stdout, stderr: &stderr,
			})
			if err != nil || code != 0 {
				t.Fatalf("completion %s = code %d, error %v", shell, code, err)
			}
			if got, want := stdout.String(), completionScript(shell); got != want {
				t.Errorf("completion %s output differs from canonical script", shell)
			}
			if stderr.Len() != 0 {
				t.Errorf("completion %s stderr = %q", shell, &stderr)
			}
			if !strings.HasSuffix(stdout.String(), "\n") {
				t.Error("generated script has no final newline")
			}
			for _, b := range stdout.Bytes() {
				if b < '\t' || b > '~' {
					t.Fatalf("generated script contains byte 0x%02x", b)
				}
			}
		})
	}
}

func TestCompletionHelpIsPortableTerminalText(t *testing.T) {
	if !strings.Contains(completionUsage, "Guide:\n  tturl help completion") {
		t.Errorf("completion help does not route to its setup guide:\n%s",
			completionUsage)
	}
	for line := range strings.SplitSeq(completionUsage, "\n") {
		if len(line) > textWidth {
			t.Errorf("completion help has a %d-column line: %q", len(line), line)
		}
		for _, b := range []byte(line) {
			if b < 0x20 || b > 0x7e {
				t.Errorf("completion help contains byte 0x%02x", b)
			}
		}
	}
}

func TestCompletionCommandUsageFailuresDoNotWriteScript(t *testing.T) {
	for _, args := range [][]string{nil, {"powershell"}, {"bash", "zsh"}} {
		var stdout, stderr bytes.Buffer
		code, err := runCompletionCommand(args, commandOutput{
			stdout: &stdout, stderr: &stderr,
		})
		if err == nil || code != 2 {
			t.Errorf("completion %q = code %d, error %v; want usage error",
				args, code, err)
		}
		if stdout.Len() != 0 ||
			!strings.Contains(stderr.String(), diagnosticPrefix(diagnosticError)) ||
			!strings.Contains(stderr.String(), completionUsage) {
			t.Errorf("completion %q stdout = %q, stderr = %q",
				args, &stdout, &stderr)
		}
	}
}

func TestCompletionCommandPreservesWriteFailure(t *testing.T) {
	want := errors.New("write failed")
	code, err := runCompletionCommand([]string{"bash"}, commandOutput{
		stdout: failingWriter{err: want}, stderr: io.Discard,
	})
	if code != 1 || !errors.Is(err, want) {
		t.Fatalf("completion write = code %d, error %v", code, err)
	}
}

func TestCompletionQueryUsesNULTerminatedWords(t *testing.T) {
	input := strings.NewReader("tturl\x00race\x00--arrange\x00r\x00")
	var stdout bytes.Buffer
	code, err := runCompletionQuery(
		[]string{"bash", "3"},
		commandOutput{stdin: input, stdout: &stdout},
	)
	if err != nil || code != 0 {
		t.Fatalf("completion query = code %d, error %v", code, err)
	}
	for _, want := range []string{"random\t", "rotate\t"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("completion query output omits %q: %q", want, &stdout)
		}
	}
}

func TestRunTimeoutCompletionOffersUnlimited(t *testing.T) {
	input := strings.NewReader("tturl\x00race\x00--run-timeout\x00u\x00")
	var stdout bytes.Buffer
	code, err := runCompletionQuery(
		[]string{"bash", "3"},
		commandOutput{stdin: input, stdout: &stdout},
	)
	if err != nil || code != 0 {
		t.Fatalf("completion query = code %d, error %v", code, err)
	}
	if !strings.Contains(stdout.String(), "unlimited\t") {
		t.Fatalf("completion query omits unlimited: %q", &stdout)
	}
}

func TestBashCompletionQueryRemovesShellQuotingFromPaths(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "dir with space")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	want := nested + string(filepath.Separator) + "\tdirectory\n"
	for name, current := range map[string]string{
		"escaped": strings.NewReplacer(
			`\`, `\\`, " ", `\ `,
		).Replace(filepath.Join(dir, "dir with")),
		"double quoted": `"` + filepath.Join(dir, "dir with"),
		"single quoted": `'` + filepath.Join(dir, "dir with"),
	} {
		t.Run(name, func(t *testing.T) {
			input := "tturl\x00race\x00--output\x00" + current + "\x00"
			var stdout bytes.Buffer
			code, err := runCompletionQuery(
				[]string{"bash", "3"},
				commandOutput{stdin: strings.NewReader(input), stdout: &stdout},
			)
			if err != nil || code != 0 {
				t.Fatalf("Bash path query = code %d, error %v", code, err)
			}
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("Bash path query = %q, want candidate %q", &stdout, want)
			}
		})
	}
}

func TestZshCompletionQueryRemovesShellQuotingFromPaths(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "dir with space")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	current := strings.NewReplacer(
		`\`, `\\`, " ", `\ `,
	).Replace(filepath.Join(dir, "dir with"))
	input := "tturl\x00race\x00--output\x00" + current + "\x00"
	var stdout bytes.Buffer
	code, err := runCompletionQuery([]string{"zsh", "3"}, commandOutput{
		stdin: strings.NewReader(input), stdout: &stdout,
	})
	if err != nil || code != 0 {
		t.Fatalf("Zsh path query = code %d, error %v", code, err)
	}
	want := nested + string(filepath.Separator) + "\tdirectory\n"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("Zsh path query = %q, want candidate %q", &stdout, want)
	}
}

func TestFishCompletionQueryRemovesShellQuotingFromPaths(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "dir with space")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	current := strings.NewReplacer(
		`\`, `\\`, " ", `\ `,
	).Replace(filepath.Join(dir, "dir with"))
	input := "tturl\x00race\x00--output\x00" + current + "\x00"
	var stdout bytes.Buffer
	code, err := runCompletionQuery([]string{"fish", "3"}, commandOutput{
		stdin: strings.NewReader(input), stdout: &stdout,
	})
	if err != nil || code != 0 {
		t.Fatalf("fish path query = code %d, error %v", code, err)
	}
	want := nested + string(filepath.Separator) + "\n"
	if !strings.Contains(stdout.String(), want) {
		t.Errorf("fish path query = %q, want candidate %q", &stdout, want)
	}
}

func TestUnquoteShellCompletionWordDoesNotEvaluateInput(t *testing.T) {
	for input, want := range map[string]string{
		`plain`:             `plain`,
		`two\ words`:        `two words`,
		`"two words`:        `two words`,
		`'two words`:        `two words`,
		`"literal\$dollar"`: `literal$dollar`,
		`"keep\q"`:          `keep\q`,
		`$(touch BAD)`:      `$(touch BAD)`,
		"trailing\\":        "trailing\\",
	} {
		if got := unquoteShellCompletionWord(input); got != want {
			t.Errorf("unquoteShellCompletionWord(%q) = %q, want %q",
				input, got, want)
		}
	}
}

func TestZshCompletionKeepsOptionNamesAndConsolidatesAliases(t *testing.T) {
	input := strings.NewReader("tturl\x00analyse\x00\x00")
	var stdout bytes.Buffer
	code, err := runCompletionQuery(
		[]string{"zsh", "2"},
		commandOutput{stdin: input, stdout: &stdout},
	)
	if err != nil || code != 0 {
		t.Fatalf("zsh completion query = code %d, error %v", code, err)
	}
	got := stdout.String()
	for _, want := range []string{
		"--insecure\toption\t--insecure  -k:" +
			"skip TLS certificate verification\n",
		"--verbose\toption\t--verbose  -v:",
		"--data\toption\t--data  --data-ascii  -d:add data;",
		`'Name\:' suppresses a generated default`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("zsh completion output omits %q:\n%s", want, got)
		}
	}
	for _, duplicate := range []string{"-k\toption\t", "-v\toption\t"} {
		if strings.Contains(got, duplicate) {
			t.Errorf("zsh completion output retains alias row %q:\n%s",
				duplicate, got)
		}
	}
	if !strings.Contains(zshCompletionScript,
		"_describe -o 'tturl option' option_specs option_values") {
		t.Errorf("Zsh loader does not use option-aware descriptions:\n%s",
			zshCompletionScript)
	}
}

func TestZshCompletionPreservesTypedShortOptionPrefix(t *testing.T) {
	input := strings.NewReader("tturl\x00analyse\x00-k\x00")
	var stdout bytes.Buffer
	code, err := runCompletionQuery(
		[]string{"zsh", "2"},
		commandOutput{stdin: input, stdout: &stdout},
	)
	if err != nil || code != 0 {
		t.Fatalf("zsh short completion = code %d, error %v", code, err)
	}
	want := "-k\toption\t--insecure  -k:skip TLS certificate verification\n"
	if got := stdout.String(); got != want {
		t.Errorf("zsh short completion = %q, want %q", got, want)
	}
}

func TestZshCompletionShowsNamesForLongOptionPrefix(t *testing.T) {
	input := strings.NewReader("tturl\x00analyse\x00--b\x00")
	var stdout bytes.Buffer
	code, err := runCompletionQuery(
		[]string{"zsh", "2"},
		commandOutput{stdin: input, stdout: &stdout},
	)
	if err != nil || code != 0 {
		t.Fatalf("zsh long completion = code %d, error %v", code, err)
	}
	got := stdout.String()
	for _, want := range []string{
		"--batch-timeout\toption\t--batch-timeout:",
		"--batch-rate-max\toption\t--batch-rate-max:",
		"--body-bytes-withheld\toption\t--body-bytes-withheld:",
		"--block\toption\t--block:",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("zsh long completion omits %q: %q", want, got)
		}
	}
	if lines := strings.Count(got, "\n"); lines != 4 {
		t.Errorf("zsh long completion has %d rows, want 4:\n%s", lines, got)
	}
}

func TestCompletionQueryMarksPathsWithoutDescriptions(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	input := "tturl\x00race\x00--output\x00\"" +
		dir + string(filepath.Separator) + "\x00"

	var zsh bytes.Buffer
	code, err := runCompletionQuery([]string{"zsh", "3"}, commandOutput{
		stdin: strings.NewReader(input), stdout: &zsh,
	})
	if err != nil || code != 0 {
		t.Fatalf("zsh path query = code %d, error %v", code, err)
	}
	for _, want := range []string{
		nested + string(filepath.Separator) + "\tdirectory\n",
		file + "\tfile\n",
	} {
		if !strings.Contains(zsh.String(), want) {
			t.Errorf("zsh path query omits %q:\n%s", want, &zsh)
		}
	}

	for _, shell := range []string{"bash", "fish"} {
		var stdout bytes.Buffer
		code, err := runCompletionQuery([]string{shell, "3"}, commandOutput{
			stdin: strings.NewReader(input), stdout: &stdout,
		})
		if err != nil || code != 0 {
			t.Fatalf("%s path query = code %d, error %v", shell, code, err)
		}
		want := []string{
			nested + string(filepath.Separator) + "\n",
			file + "\n",
		}
		if shell == "bash" {
			want = []string{
				nested + string(filepath.Separator) + "\tdirectory\n",
				file + "\tfile\n",
			}
		}
		for _, want := range want {
			if !strings.Contains(stdout.String(), want) {
				t.Errorf("%s path query omits %q: %q", shell, want, &stdout)
			}
		}
	}
	if !strings.Contains(bashCompletionScript, "compopt -o filenames") {
		t.Errorf("Bash loader does not enable filename completion:\n%s",
			bashCompletionScript)
	}
}

func TestGeneratedCompletionShellSyntax(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is not installed", shell)
			}
			dir := t.TempDir()
			name := filepath.Join(dir, "completion")
			if shell == "zsh" {
				name = filepath.Join(dir, "_tturl")
			}
			if err := os.WriteFile(name, []byte(completionScript(shell)), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"-n", name}
			//nolint:gosec // LookPath resolved one of the three fixed shell names.
			cmd := exec.CommandContext(t.Context(), path, args...)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s syntax: %v\n%s", shell, err, output)
			}

			var loadArgs []string
			switch shell {
			case "bash":
				// A reduced Bash may omit programmable completion while
				// retaining the parser needed to validate the script.
				//nolint:gosec // LookPath resolved the fixed Bash shell name.
				probe := exec.CommandContext(t.Context(), path,
					"--noprofile", "--norc", "-c", "type complete")
				if err := probe.Run(); err != nil {
					t.Skip("Bash programmable completion is not available")
				}
				loadArgs = []string{
					"--noprofile", "--norc", "-c",
					`source "$1"; complete -p tturl >/dev/null`, "_", name,
				}
			case "zsh":
				loadArgs = []string{
					"-f", "-c",
					`autoload -Uz compinit; compinit -D; source "$1"; ` +
						`whence -w _tturl >/dev/null; ` +
						`(PATH=/nonexistent; words=(tturl); CURRENT=1; ` +
						`_tturl; [[ $? -eq 1 ]])`, "_", name,
				}
			case "fish":
				loadArgs = []string{
					"-N", "-c",
					`source $argv[1]; functions -q __tturl_completion`, name,
				}
			}
			//nolint:gosec // LookPath resolved one of the three fixed shell names.
			load := exec.CommandContext(t.Context(), path, loadArgs...)
			if output, err := load.CombinedOutput(); err != nil {
				t.Fatalf("load %s completion: %v\n%s", shell, err, output)
			}
			if shell == "zsh" {
				autoloadArgs := []string{
					"-f", "-c",
					`fpath=("$1" $fpath); autoload -Uz compinit; compinit -D -i; ` +
						`(PATH=/nonexistent; words=(tturl); CURRENT=1; ` +
						`_tturl; [[ $? -eq 1 ]])`, "_", dir,
				}
				//nolint:gosec // LookPath resolved the fixed Zsh shell name.
				autoload := exec.CommandContext(t.Context(), path, autoloadArgs...)
				if output, err := autoload.CombinedOutput(); err != nil {
					t.Fatalf("autoload Zsh completion: %v\n%s", err, output)
				}
			}
		})
	}
}
