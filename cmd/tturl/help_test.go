package main

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tantosec/tturl/internal/curlblocks"
)

func TestHelpInformationArchitecture(t *testing.T) {
	want := []struct {
		title  string
		topics []string
	}{
		{"Learn", []string{"getting-started", "concepts", "commands"}},
		{"Command guides", []string{"race", "measure", "analyse", "detect", "time"}},
		{"Build requests", []string{"requests", "blocks", "headers", "bodies", "vary"}},
		{"Run experiments", []string{"design", "delivery", "padding"}},
		{"Use output", []string{"output", "json", "schema"}},
		{"Utilities", []string{"demo-server", "completion"}},
	}
	if len(helpSections) != len(want) {
		t.Fatalf("help section count = %d, want %d", len(helpSections), len(want))
	}
	for index, section := range helpSections {
		names := make([]string, len(section.topics))
		for topicIndex, topic := range section.topics {
			names[topicIndex] = topic.name
		}
		if section.title != want[index].title ||
			!slices.Equal(names, want[index].topics) {
			t.Errorf("help section %d = %q %q, want %q %q",
				index, section.title, names, want[index].title, want[index].topics)
		}
	}
	if !strings.Contains(topLevelUsage, "Utilities:\n") ||
		strings.Contains(topLevelUsage, "Extras:\n") {
		t.Errorf("root help does not use the shared Utilities vocabulary:\n%s",
			topLevelUsage)
	}
}

func TestHelpCatalogue(t *testing.T) {
	if !strings.Contains(topLevelUsage, "Help:\n") ||
		!strings.Contains(topLevelUsage, "  help") {
		t.Errorf("root help does not advertise detailed help:\n%s",
			topLevelUsage)
	}
	checkHelpTextContract(t, "root", topLevelUsage)
	checkHelpTextContract(t, "index", helpIndex)

	flatIndex := strings.Join(strings.Fields(helpIndex), " ")
	at := 0
	seen := make(map[string]bool)
	for _, section := range helpSections {
		if section.title == "" || len(section.topics) == 0 {
			t.Errorf("help section is incomplete: %+v", section)
			continue
		}
		sectionAt := strings.Index(flatIndex[at:], section.title+":")
		if sectionAt < 0 {
			t.Fatalf("help index omits section %q or lists it out of order:\n%s",
				section.title, helpIndex)
		}
		at += sectionAt + len(section.title) + 1
		for _, topic := range section.topics {
			if seen[topic.name] {
				t.Errorf("help topic %q occurs more than once", topic.name)
			}
			seen[topic.name] = true
			if topic.name == "" || topic.summary == "" || topic.text == "" {
				t.Errorf("help topic is incomplete: %+v", topic)
			}
			entry := topic.name + " " + topic.summary
			pos := strings.Index(flatIndex[at:], entry)
			if pos < 0 {
				t.Fatalf("help index omits %q or lists it out of order:\n%s",
					entry, helpIndex)
			}
			at += pos + len(entry)
			checkHelpTextContract(t, topic.name, topic.text)
			checkHelpTextContract(t, "rendered "+topic.name,
				renderHelpTopic(topic))
		}
	}
	if len(seen) != len(helpTopics) {
		t.Errorf("flattened topic count = %d, want %d unique catalogue topics",
			len(helpTopics), len(seen))
	}
}

func TestHelpDoesNotAdvertiseHiddenVersion(t *testing.T) {
	if commandByName("version") != nil {
		t.Error("hidden version alias is in the command catalogue")
	}
	if _, ok := lookupHelpTopic("version"); ok {
		t.Error("hidden version alias is a help topic")
	}
	for name, text := range map[string]string{
		"root help":  topLevelUsage,
		"help index": helpIndex,
	} {
		for line := range strings.SplitSeq(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "version ") {
				t.Errorf("%s advertises hidden version alias: %q", name, line)
			}
		}
	}
}

func TestEveryOptionHasLongFormHelp(t *testing.T) {
	var library strings.Builder
	for _, topic := range helpTopics {
		library.WriteString(topic.text)
	}
	text := library.String()
	seen := make(map[string]bool)
	check := func(surface string, options []curlblocks.CompletionOption) {
		t.Helper()
		for _, option := range options {
			for _, name := range option.Names {
				if !strings.HasPrefix(name, "--") ||
					name == "--help" || name == "--version" || seen[name] {
					continue
				}
				seen[name] = true
				if !strings.Contains(text, name) {
					t.Errorf("%s option %s has no long-form help mention",
						surface, name)
				}
			}
		}
	}
	for i := range commandCatalogue {
		spec := &commandCatalogue[i]
		if spec.requestGrammar {
			check(spec.name, describeRequestCommand(spec).CompletionOptions())
		}
	}
	check("demo-server", demoServerCompletionOptions())
}

func checkHelpTextContract(t *testing.T, name, text string) {
	t.Helper()
	if !strings.HasSuffix(text, "\n") || strings.HasSuffix(text, "\n\n") {
		t.Errorf("help topic %q does not end with exactly one newline", name)
	}
	// Allow (Christina) Pöpper, disallow other non-printable bytes
	asciiText := strings.ReplaceAll(text, "Pöpper", "Popper")
	for _, b := range []byte(asciiText) {
		if b != '\n' && (b < 0x20 || b > 0x7e) {
			t.Errorf("help topic %q contains non-printable byte 0x%02x",
				name, b)
			break
		}
	}
	for line := range strings.SplitSeq(text, "\n") {
		if utf8.RuneCountInString(line) > textWidth &&
			!isIndentedIndivisibleURL(line) {
			t.Errorf("help topic %q has a %d-column line: %q",
				name, utf8.RuneCountInString(line), line)
		}
	}
}

// isIndentedIndivisibleURL recognises the one class of authored help token
// that may exceed the text width without a meaningful place to wrap it.
func isIndentedIndivisibleURL(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	return len(trimmed) < len(line) &&
		!strings.ContainsAny(trimmed, " \t") &&
		(strings.HasPrefix(trimmed, "https://") ||
			strings.HasPrefix(trimmed, "http://"))
}

func TestHelpWidthExceptionIsOnlyAnIndentedIndivisibleURL(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{"  https://example.test/one-unbroken-reference", true},
		{"  http://example.test/reference", true},
		{"https://example.test/not-indented", false},
		{"  https://example.test/reference trailing prose", false},
		{"  ordinary-unbroken-token", false},
	}
	for _, test := range tests {
		if got := isIndentedIndivisibleURL(test.line); got != test.want {
			t.Errorf("isIndentedIndivisibleURL(%q) = %t, want %t",
				test.line, got, test.want)
		}
	}
}

func TestProductHelpTextFilesMatchCatalogue(t *testing.T) {
	entries, err := helpTextFS.ReadDir("helptext")
	if err != nil {
		t.Fatalf("reading embedded help directory: %v", err)
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		filePath := "helptext/" + entry.Name()
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txt") {
			t.Errorf("embedded help entry %q is not a plaintext page", filePath)
			continue
		}
		b, err := helpTextFS.ReadFile(filePath)
		if err != nil {
			t.Errorf("reading %s: %v", filePath, err)
			continue
		}
		text := normaliseHelpText(string(b))
		checkHelpTextContract(t, filePath, text)
		name := strings.TrimSuffix(entry.Name(), ".txt")
		topic, ok := lookupHelpTopic(name)
		if !ok {
			t.Errorf("%s has no catalogue topic", filePath)
			continue
		}
		seen[name] = true
		if topic.text != text {
			t.Errorf("catalogue topic %q does not contain %s", name, filePath)
		}
	}
	for _, topic := range helpTopics {
		if !seen[topic.name] {
			t.Errorf("catalogue topic %q has no plaintext page", topic.name)
		}
	}
}

func TestRelatedHelpRoutesResolve(t *testing.T) {
	for _, topic := range helpTopics {
		for line := range strings.SplitSeq(topic.text, "\n") {
			fields := strings.Fields(line)
			if !strings.HasPrefix(line, "  "+toolName+" help ") ||
				len(fields) < 4 {
				continue
			}
			if _, ok := lookupHelpTopic(fields[2]); !ok {
				t.Errorf("%s links to unknown help topic %q",
					topic.name, fields[2])
			}
		}
	}
}

func TestRunCLIHelpCommand(t *testing.T) {
	for _, args := range [][]string{
		{"help"},
		{"help", "-h"},
		{"help", "--help"},
	} {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(t.Context(), args, commandOutput{
			stdout: &stdout,
			stderr: &stderr,
		})
		if err != nil || code != 0 {
			t.Errorf("runCLI(%q) = code %d, error %v; want success",
				args, code, err)
		}
		if stdout.String() != helpIndex {
			t.Errorf("runCLI(%q) stdout differs from help index:\n%s",
				args, &stdout)
		}
		if stderr.Len() != 0 {
			t.Errorf("runCLI(%q) stderr = %q, want empty", args, &stderr)
		}
	}

	for _, topic := range helpTopics {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(t.Context(), []string{"help", topic.name},
			commandOutput{stdout: &stdout, stderr: &stderr})
		if err != nil || code != 0 {
			t.Errorf("help %s = code %d, error %v; want success",
				topic.name, code, err)
		}
		if got, want := stdout.String(), renderHelpTopic(topic); got != want {
			t.Errorf("help %s output differs:\ngot:\n%s\nwant:\n%s",
				topic.name, got, want)
		}
		if stderr.Len() != 0 {
			t.Errorf("help %s stderr = %q, want empty",
				topic.name, &stderr)
		}
	}
}

func TestRunCLIHelpHelpIsHidden(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runCLI(t.Context(), []string{"help", "help"}, commandOutput{
		stdout: &stdout,
		stderr: &stderr,
	})
	if err != nil || code != 0 {
		t.Fatalf("help help = code %d, error %v; want success", code, err)
	}
	if got := stdout.String(); got != "There's only so much I can do\n" {
		t.Errorf("help help stdout = %q", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("help help stderr = %q, want empty", &stderr)
	}
	if strings.Contains(helpIndex, "  help ") {
		t.Errorf("help index advertises hidden help topic:\n%s", helpIndex)
	}
}

func TestRunCLIHelpCommandErrors(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{
			args: []string{"help", "absent"},
			want: `unknown help topic "absent"`,
		},
		{
			args: []string{"help", "blocks", "extra"},
			want: "help accepts at most one topic",
		},
		{
			args: []string{"help", "m\u00e9chant"},
			want: `unknown help topic "$HEX[6dc3a96368616e74]"`,
		},
	}
	for _, test := range tests {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(t.Context(), test.args, commandOutput{
			stdout: &stdout,
			stderr: &stderr,
		})
		if err == nil || code != 2 {
			t.Errorf("runCLI(%q) = code %d, error %v; want code 2",
				test.args, code, err)
		}
		if stdout.Len() != 0 ||
			!strings.Contains(stderr.String(), diagnosticPrefix(diagnosticError)) ||
			!strings.Contains(stderr.String(), test.want) ||
			!strings.Contains(stderr.String(), helpIndex) {
			t.Errorf("runCLI(%q) stdout = %q, stderr = %q",
				test.args, &stdout, &stderr)
		}
		for _, b := range stderr.Bytes() {
			if b > 0x7f {
				t.Errorf("runCLI(%q) stderr contains non-ASCII byte 0x%02x",
					test.args, b)
				break
			}
		}
	}
}

func TestRunCLIHelpCommandPreservesWriteFailures(t *testing.T) {
	want := errors.New("write failed")
	for _, args := range [][]string{
		{"help"},
		{"help", "blocks"},
	} {
		code, err := runCLI(t.Context(), args, commandOutput{
			stdout: failingWriter{err: want},
			stderr: io.Discard,
		})
		if code != 1 || !errors.Is(err, want) {
			t.Errorf("runCLI(%q) = code %d, error %v; want code 1 and %v",
				args, code, err, want)
		}
	}
}
