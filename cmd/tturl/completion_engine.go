package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/spf13/pflag"

	"github.com/tantosec/tturl/internal/curlblocks"
)

type completionCandidate struct {
	value       string
	description string
	optionGroup string
	optionLabel string
	file        bool
	directory   bool
}

var completionShells = []string{"bash", "zsh", "fish"}

func completeCommandLine(words []string, cursor int) []completionCandidate {
	if cursor < 0 || cursor >= len(words) {
		return nil
	}
	words = words[:cursor+1]
	current := words[cursor]
	if cursor == 0 {
		return nil
	}
	if cursor == 1 {
		return completeRoot(current)
	}

	switch words[1] {
	case helpCommandName:
		if !onlySilentSwitches(words[2:cursor]) {
			return nil
		}
		candidates := optionCompletionCandidates(
			[]string{"-h", "--help"}, "show help topics and exit")
		candidates = append(candidates, silentCompletionCandidates()...)
		for _, topic := range helpTopics {
			candidates = append(candidates, completionCandidate{
				value: topic.name, description: topic.summary,
			})
		}
		return filterCompletionPrefix(candidates, current)
	case commandByID(commandSchema).name:
		return completeSchema(words[2:cursor], current)
	case completionCommandName:
		if !onlySilentSwitches(words[2:cursor]) {
			return nil
		}
		candidates := optionCompletionCandidates(
			[]string{"-h", "--help"}, "show completion help and exit")
		candidates = append(candidates, silentCompletionCandidates()...)
		for _, shell := range completionShells {
			candidates = append(candidates, completionCandidate{
				value: shell, description: "generate " + shell + " completion",
			})
		}
		return filterCompletionPrefix(candidates, current)
	case "version":
		if onlySilentSwitches(words[2:cursor]) {
			return filterCompletionPrefix(silentCompletionCandidates(), current)
		}
		return nil
	case commandByID(commandDemoServer).name:
		return completeOptions(
			demoServerCompletionOptions(), words[2:cursor], current)
	}

	spec := commandByName(words[1])
	if spec == nil || !spec.requestGrammar {
		return nil
	}
	options := describeRequestCommand(spec).CompletionOptions()
	return completeOptions(options, words[2:cursor], current)
}

func completeRoot(prefix string) []completionCandidate {
	var candidates []completionCandidate
	for i := range commandCatalogue {
		spec := &commandCatalogue[i]
		candidates = append(candidates, completionCandidate{
			value:       spec.name,
			description: spec.lifecycle.labelledSummary(spec.menuSummary),
		})
	}
	candidates = append(candidates,
		completionCandidate{
			value: helpCommandName, description: "browse detailed help topics",
		},
		completionCandidate{
			value:       completionCommandName,
			description: "generate shell completion",
		},
	)
	candidates = append(candidates, optionCompletionCandidates(
		[]string{"-h", "--help"}, "show help and exit")...)
	candidates = append(candidates, optionCompletionCandidates(
		[]string{"-V", "--version"}, "show version and exit")...)
	return filterCompletionPrefix(candidates, prefix)
}

func demoServerCompletionOptions() []curlblocks.CompletionOption {
	cfg := defaultDemoServerConfig()
	var help bool
	fs, _, _ := newDemoServerFlagSet(&cfg, &help)
	var options []curlblocks.CompletionOption
	fs.VisitAll(func(flag *pflag.Flag) {
		names := []string{"--" + flag.Name}
		if flag.Shorthand != "" {
			names = append([]string{"-" + flag.Shorthand}, names...)
		}
		option := curlblocks.CompletionOption{
			Names:       names,
			Description: flag.Usage,
			Values: append([]string(nil),
				flag.Annotations[completionValuesFlagAnnotation]...),
			Scope:      curlblocks.CompletionGlobal,
			TakesValue: flag.NoOptDefVal == "",
		}
		options = append(options, option)
	})
	return options
}

func completeOptions(
	options []curlblocks.CompletionOption,
	prior []string,
	current string,
) []completionCandidate {
	byName := make(map[string]*curlblocks.CompletionOption)
	for i := range options {
		for _, name := range options[i].Names {
			byName[name] = &options[i]
		}
	}

	scope := curlblocks.CompletionGlobal
	var expecting *curlblocks.CompletionOption
	for _, word := range prior {
		if expecting != nil {
			expecting = nil
			continue
		}
		// pflag treats a standalone -- as the end of options. tturl does not
		// complete positional URLs, so no candidate is valid after it.
		if word == "--" {
			return nil
		}
		name := word
		option := byName[name]
		if before, _, ok := strings.Cut(word, "="); ok {
			if attached := byName[before]; attached != nil && attached.TakesValue {
				name, option = before, attached
			}
		}
		if option == nil {
			continue
		}
		if !completionScopeAccepts(option.Scope, scope) {
			continue
		}
		if option.OpensBlock {
			scope = curlblocks.CompletionBlock
			continue
		}
		if option.TakesValue && name == word {
			expecting = option
		}
	}
	if expecting != nil {
		return completeOptionValue(*expecting, current, "")
	}

	if name, value, ok := strings.Cut(current, "="); ok {
		if option := byName[name]; option != nil && option.TakesValue &&
			completionScopeAccepts(option.Scope, scope) {
			return completeOptionValue(*option, value, name+"=")
		}
	}
	if current != "" && !strings.HasPrefix(current, "-") {
		return nil
	}

	var candidates []completionCandidate
	for _, option := range options {
		if !completionScopeAccepts(option.Scope, scope) {
			continue
		}
		candidates = append(candidates,
			optionCompletionCandidates(option.Names, option.Description)...)
	}
	return filterCompletionPrefix(candidates, current)
}

func optionCompletionCandidates(
	names []string,
	description string,
) []completionCandidate {
	labelNames := make([]string, 0, len(names))
	for _, name := range names {
		if strings.HasPrefix(name, "--") {
			labelNames = append(labelNames, name)
		}
	}
	for _, name := range names {
		if !strings.HasPrefix(name, "--") {
			labelNames = append(labelNames, name)
		}
	}
	label := strings.Join(labelNames, "  ")
	group := strings.Join(names, "\x00")
	candidates := make([]completionCandidate, 0, len(names))
	for _, name := range names {
		candidates = append(candidates, completionCandidate{
			value:       name,
			description: description,
			optionGroup: group,
			optionLabel: label,
		})
	}
	return candidates
}

func completionScopeAccepts(
	optionScope, currentScope curlblocks.CompletionScope,
) bool {
	switch optionScope {
	case curlblocks.CompletionGlobal:
		return currentScope == curlblocks.CompletionGlobal
	case curlblocks.CompletionShared:
		return true
	case curlblocks.CompletionBlock:
		return currentScope == curlblocks.CompletionBlock
	default:
		return false
	}
}

func completeOptionValue(
	option curlblocks.CompletionOption,
	current, prefix string,
) []completionCandidate {
	var candidates []completionCandidate
	for _, value := range option.Values {
		// A leading dash is useful in the flag reference but noisy beside an
		// ordinary directory listing. Users can still enter it directly.
		if option.FileStyle != curlblocks.CompletionNoFiles && value == "-" {
			continue
		}
		if strings.HasPrefix(value, current) {
			candidates = append(candidates, completionCandidate{
				value:       prefix + value,
				description: option.Description,
			})
		}
	}
	head, path, ok := completionFilePath(option.FileStyle, current)
	if ok {
		for _, candidate := range completeLocalFiles(path) {
			candidate.value = prefix + head + candidate.value
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func completionFilePath(
	style curlblocks.CompletionFileStyle,
	value string,
) (head, path string, ok bool) {
	switch style {
	case curlblocks.CompletionFiles:
		return "", value, true
	case curlblocks.CompletionAtFiles:
		if strings.HasPrefix(value, "@") {
			return "@", value[1:], true
		}
	case curlblocks.CompletionURLencodeFiles:
		if !strings.Contains(value, "=") {
			if at := strings.IndexByte(value, '@'); at >= 0 {
				return value[:at+1], value[at+1:], true
			}
		}
	case curlblocks.CompletionFormFiles:
		if eq := strings.IndexByte(value, '='); eq >= 0 && eq+1 < len(value) &&
			(value[eq+1] == '@' || value[eq+1] == '<') {
			return value[:eq+2], value[eq+2:], true
		}
	case curlblocks.CompletionVaryFiles:
		if eq := strings.IndexByte(value, '='); eq >= 0 &&
			strings.HasPrefix(value[eq+1:], "@") {
			return value[:eq+2], value[eq+2:], true
		}
	}
	return "", "", false
}

func completeLocalFiles(path string) []completionCandidate {
	if path == "~" {
		return []completionCandidate{{
			value:     "~" + string(filepath.Separator),
			directory: true,
		}}
	}
	writtenPath := path
	searchPath := path
	if strings.HasPrefix(path, "~"+string(filepath.Separator)) {
		searchPath = filepath.Join(userHomeDir(), path[2:])
	}
	dir, base := filepath.Split(searchPath)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	_, writtenBase := filepath.Split(writtenPath)
	writtenDir := strings.TrimSuffix(writtenPath, writtenBase)
	var candidates []completionCandidate
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, base) ||
			(strings.HasPrefix(name, ".") && !strings.HasPrefix(base, ".")) ||
			!safeCompletionValue(name) {
			continue
		}
		value := writtenDir + name
		candidate := completionCandidate{value: value, file: true}
		if entry.IsDir() {
			candidate.value += string(filepath.Separator)
			candidate.file = false
			candidate.directory = true
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

// safeCompletionValue rejects terminal controls while preserving ordinary
// Unicode and shell-significant characters for the shell to quote. Completion
// candidates can include untrusted directory entries and are displayed by the
// interactive shell, so emitting controls would let a file name affect the
// terminal rather than merely name a file.
func safeCompletionValue(value string) bool {
	for _, r := range value {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func userHomeDir() string {
	dir, err := os.UserHomeDir()
	if err != nil {
		return "~"
	}
	return dir
}

func filterCompletionPrefix(
	candidates []completionCandidate,
	prefix string,
) []completionCandidate {
	return slices.DeleteFunc(candidates, func(candidate completionCandidate) bool {
		return !strings.HasPrefix(candidate.value, prefix)
	})
}
