package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
)

const silentDescription = "suppress progress, errors, warnings and notices"

func isSilentSwitch(arg string) bool { return arg == "-s" || arg == "--silent" }

func silentOutput(out commandOutput) commandOutput {
	out.stderr = io.Discard
	return out
}

// silentPrefix owns the utility grammar: only leading switches are options.
func silentPrefix(args []string, out commandOutput) ([]string, commandOutput) {
	for len(args) > 0 && isSilentSwitch(args[0]) {
		out = silentOutput(out)
		args = args[1:]
	}
	return args, out
}

func silentCompletionCandidates() []completionCandidate {
	return optionCompletionCandidates([]string{"-s", "--silent"}, silentDescription)
}

// commandDiagnosticOutput selects the diagnostic channel before parsing can
// emit output. The owning registrations supply option arity; values and tokens
// after an option terminator remain untouched. Request segmentation precedes
// option parsing, just as it does in curlblocks.
func commandDiagnosticOutput(
	spec *commandSpec, args []string, out commandOutput,
) (commandOutput, error) {
	var options []curlblocks.CompletionOption
	switch {
	case spec.requestGrammar:
		options = describeRequestCommand(spec).CompletionOptions()
		for i, arg := range args {
			if arg == "--block" {
				args = args[:i]
				break
			}
		}
	case spec.id == commandDemoServer:
		options = demoServerCompletionOptions()
	case spec.id == commandSchema:
		options = []curlblocks.CompletionOption{
			{Names: []string{"-o", "--output"}, TakesValue: true},
			{Names: []string{"-h", "--help"}},
			{Names: []string{"-s", "--silent"}},
		}
	}
	byName := make(map[string]curlblocks.CompletionOption)
	for _, option := range options {
		if spec.requestGrammar && option.Scope == curlblocks.CompletionBlock {
			option.TakesValue = false
		}
		for _, name := range option.Names {
			byName[name] = option
		}
	}
	var selectionErr error
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" && spec.id != commandSchema {
			break
		}
		if isSilentSwitch(arg) {
			out = silentOutput(out)
			continue
		}
		name, _, attached := strings.Cut(arg, "=")
		if attached && isSilentSwitch(name) {
			selectionErr = usage(fmt.Errorf("%s takes no value", name))
			continue
		}
		if option, known := byName[name]; known {
			if option.TakesValue && !attached {
				i++
			}
			continue
		}
		// pflag accepts clustered shorthand and attached short values. A
		// value consumes the rest of its token, or the next whole argument.
		if spec.id == commandSchema || !strings.HasPrefix(arg, "-") ||
			strings.HasPrefix(arg, "--") {
			continue
		}
		for j := 1; j < len(arg); j++ {
			short := "-" + arg[j:j+1]
			option, known := byName[short]
			if !known {
				break
			}
			if option.TakesValue {
				if j+1 == len(arg) {
					i++
				}
				break
			}
			if short == "-s" {
				if j+1 < len(arg) && arg[j+1] == '=' {
					selectionErr = usage(fmt.Errorf("-s takes no value"))
					break
				}
				out = silentOutput(out)
			}
		}
	}
	return out, selectionErr
}

func onlySilentSwitches(args []string) bool {
	for _, arg := range args {
		if !isSilentSwitch(arg) {
			return false
		}
	}
	return true
}
