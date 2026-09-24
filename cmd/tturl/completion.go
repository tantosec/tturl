package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
)

const (
	completionCommandName = "completion"
	completionQueryName   = "__complete"
	completionInputMax    = 1 << 20
)

var completionUsage = normaliseHelpText(
	toolName + " completion - generate shell completion\n\n" +
		"Usage:\n" +
		"  " + toolName + " completion [OPTIONS] SHELL\n\n" +
		"Options (before SHELL or help selector):\n" +
		"  -s, --silent       " + silentDescription + "\n\n" +
		"Shells:\n" +
		"  bash   generate Bash completion\n" +
		"  zsh    generate Zsh completion\n" +
		"  fish   generate fish completion\n\n" +
		"The generated script is written to stdout. Load it directly or redirect it\n" +
		"to a file.\n\n" +
		"Guide:\n" +
		"  " + toolName + " help completion\n")

func runCompletionCommand(
	args []string,
	out commandOutput,
) (code int, err error) {
	args, out = silentPrefix(args, out)
	if arg, ok := soleArgument(args); ok {
		if arg == "-h" || arg == "--help" {
			_, err := fmt.Fprint(out.stdout, completionUsage)
			if err != nil {
				return 1, err
			}
			return 0, nil
		}
	}
	shell, oneShell := soleArgument(args)
	if !oneShell || !slices.Contains(completionShells, shell) {
		var commandErr error
		switch len(args) {
		case 0:
			commandErr = errors.New("completion requires a shell: bash, zsh, or fish")
		case 1:
			commandErr = fmt.Errorf("unsupported shell %q (want bash, zsh, or fish)",
				curlblocks.DisplayText(shell))
		default:
			commandErr = errors.New("completion accepts exactly one shell")
		}
		writeErr := writeDiagnosticDocument(
			out.stderr, diagnosticError, commandErr.Error(), completionUsage)
		return 2, errors.Join(commandErr, writeErr)
	}

	script := completionScript(shell)
	if _, err := io.WriteString(out.stdout, script); err != nil {
		return 1, err
	}
	return 0, nil
}

func soleArgument(args []string) (string, bool) {
	if len(args) != 1 {
		return "", false
	}
	for _, arg := range args {
		return arg, true
	}
	return "", false
}

func completionScript(shell string) string {
	switch shell {
	case "bash":
		return bashCompletionScript
	case "zsh":
		return zshCompletionScript
	case "fish":
		return fishCompletionScript
	default:
		panic(toolName + ": unsupported completion shell " + shell)
	}
}

const bashCompletionScript = `# tturl completion for Bash
_tturl_completion()
{
    local value kind description paths=0
    COMPREPLY=()
    while IFS=$'\t' read -r value kind description; do
        [[ -n $value ]] && COMPREPLY+=("$value")
        [[ $kind == file || $kind == directory ]] && paths=1
    done < <(
        printf '%s\0' "${COMP_WORDS[@]}" |
            command tturl __complete bash "$COMP_CWORD" 2>/dev/null
    )
    (( paths )) && compopt -o filenames
}
complete -F _tturl_completion tturl
`

const zshCompletionScript = `#compdef tturl
# tturl completion for Zsh
_tturl()
{
    local value kind spec tturl_result=1
    local -a option_values option_specs value_values value_specs
    local -a directory_values file_values
    while IFS=$'\t' read -r value kind spec; do
        if [[ -n $value ]]; then
            case $kind in
                option)
                    option_values+=("$value")
                    option_specs+=("$spec")
                    ;;
                directory)
                    directory_values+=("$value")
                    ;;
                file)
                    file_values+=("$value")
                    ;;
                *)
                    value_values+=("$value")
                    value_specs+=("$spec")
                    ;;
            esac
        fi
    done < <(
        printf '%s\0' "${words[@]}" |
            command tturl __complete zsh "$((CURRENT - 1))" 2>/dev/null
    )
    if (( ${#option_values} )); then
        _describe -o 'tturl option' option_specs option_values && tturl_result=0
    fi
    if (( ${#value_values} )); then
        _describe 'tturl value' value_specs value_values && tturl_result=0
    fi
    if (( ${#directory_values} )); then
        compadd -J tturl-path -S '' -- "${directory_values[@]}" && tturl_result=0
    fi
    if (( ${#file_values} )); then
        compadd -J tturl-path -- "${file_values[@]}" && tturl_result=0
    fi
    return tturl_result
}
if [[ $funcstack[1] == _tturl ]]; then
    _tturl "$@"
else
    compdef _tturl tturl
fi
`

const fishCompletionScript = `# tturl completion for fish
function __tturl_completion
    set -l prior (commandline -opc)
    set -l cursor (count $prior)
    begin
        printf '%s\0' $prior
        printf '%s\0' (commandline -ct)
    end | command tturl __complete fish $cursor 2>/dev/null
end
complete -c tturl -f -a '(__tturl_completion)'
`

func runCompletionQuery(args []string, out commandOutput) (code int, err error) {
	if len(args) != 2 {
		return 2, errors.New("invalid completion query")
	}
	shell := args[0]
	if !slices.Contains(completionShells, shell) {
		return 2, errors.New("invalid completion query")
	}
	cursor, err := strconv.Atoi(args[1])
	if err != nil {
		return 2, fmt.Errorf("invalid completion cursor: %w", err)
	}
	if out.stdin == nil {
		return 1, errors.New("completion query has no input")
	}
	data, err := io.ReadAll(io.LimitReader(out.stdin, completionInputMax+1))
	if err != nil {
		return 1, fmt.Errorf("read completion query: %w", err)
	}
	if len(data) > completionInputMax {
		return 2, errors.New("completion query is too large")
	}
	words := bytes.Split(data, []byte{0})
	if len(words) > 0 && len(words[len(words)-1]) == 0 {
		words = words[:len(words)-1]
	}
	textWords := make([]string, len(words))
	for i := range words {
		textWords[i] = unquoteShellCompletionWord(string(words[i]))
	}
	candidates := completeCommandLine(textWords, cursor)
	if shell == "zsh" {
		candidates = consolidateZshCompletionCandidates(candidates)
	}
	for _, candidate := range candidates {
		if candidate.value == "" || !safeCompletionValue(candidate.value) {
			continue
		}
		description := curlblocks.DisplayText(candidate.description)
		if shell == "zsh" {
			kind := "value"
			if candidate.optionGroup != "" {
				kind = "option"
			} else if candidate.directory {
				kind = "directory"
			} else if candidate.file {
				kind = "file"
			}
			if kind == "directory" || kind == "file" {
				if _, err := fmt.Fprintf(
					out.stdout, "%s\t%s\n", candidate.value, kind,
				); err != nil {
					return 1, err
				}
				continue
			}
			label := candidate.optionLabel
			if label == "" {
				label = candidate.value
			}
			spec := zshDescribeText(label) + ":" +
				zshDescribeText(description)
			if _, err := fmt.Fprintf(
				out.stdout, "%s\t%s\t%s\n", candidate.value, kind, spec,
			); err != nil {
				return 1, err
			}
			continue
		}
		if shell == "bash" {
			kind := "value"
			if candidate.directory {
				kind = "directory"
			} else if candidate.file {
				kind = "file"
			}
			if _, err := fmt.Fprintf(
				out.stdout, "%s\t%s", candidate.value, kind,
			); err != nil {
				return 1, err
			}
		} else if _, err := fmt.Fprint(out.stdout, candidate.value); err != nil {
			return 1, err
		}
		if description != "" {
			if _, err := fmt.Fprintf(out.stdout, "\t%s", description); err != nil {
				return 1, err
			}
		}
		if _, err := fmt.Fprintln(out.stdout); err != nil {
			return 1, err
		}
	}
	return 0, nil
}

// unquoteShellCompletionWord removes quoting that interactive shells retain
// in completion words. It deliberately implements only quote removal, rather
// than evaluating expansions or substitutions from a command line.
func unquoteShellCompletionWord(word string) string {
	const (
		unquoted = iota
		singleQuoted
		doubleQuoted
	)
	state := unquoted
	var out strings.Builder
	for i := 0; i < len(word); i++ {
		char := word[i]
		switch state {
		case unquoted:
			switch char {
			case '\\':
				if i+1 < len(word) {
					i++
					out.WriteByte(word[i])
				} else {
					out.WriteByte(char)
				}
			case '\'':
				state = singleQuoted
			case '"':
				state = doubleQuoted
			default:
				out.WriteByte(char)
			}
		case singleQuoted:
			if char == '\'' {
				state = unquoted
			} else {
				out.WriteByte(char)
			}
		case doubleQuoted:
			switch char {
			case '"':
				state = unquoted
			case '\\':
				if i+1 < len(word) && strings.ContainsRune("$`\"\\\n", rune(word[i+1])) {
					i++
					if word[i] != '\n' {
						out.WriteByte(word[i])
					}
				} else {
					out.WriteByte(char)
				}
			default:
				out.WriteByte(char)
			}
		}
	}
	return out.String()
}

func consolidateZshCompletionCandidates(
	candidates []completionCandidate,
) []completionCandidate {
	positions := make(map[string]int)
	consolidated := make([]completionCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.optionGroup == "" {
			consolidated = append(consolidated, candidate)
			continue
		}
		if position, ok := positions[candidate.optionGroup]; ok {
			if strings.HasPrefix(candidate.value, "--") &&
				!strings.HasPrefix(consolidated[position].value, "--") {
				consolidated[position].value = candidate.value
			}
			continue
		}
		positions[candidate.optionGroup] = len(consolidated)
		consolidated = append(consolidated, candidate)
	}
	return consolidated
}

func zshDescribeText(text string) string {
	return strings.NewReplacer(`\`, `\\`, `:`, `\:`).Replace(text)
}
