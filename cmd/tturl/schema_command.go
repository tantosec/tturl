package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
)

var schemaUsage = normaliseHelpText(toolName + ` schema - export a shipped JSON Schema document

Usage:
  ` + toolName + ` schema [OPTIONS] SUBJECT

Options may precede or follow SUBJECT.

Subjects:
  race, measure, analyse, detect, time   current command record schema
  common                                shared definitions resource

Options:
  -o, --output FILE   write schema; - means stdout (default: -)
  -s, --silent        suppress progress, errors, warnings and notices
  -h, --help          show usage and exit

Guide:
  ` + toolName + ` help schema
`)

type schemaSubject struct {
	name       string
	descriptor *schemaDescriptor
}

func schemaSubjects(catalogue []commandSpec) []schemaSubject {
	subjects := make([]schemaSubject, 0, len(catalogue)+1)
	for i := range catalogue {
		spec := &catalogue[i]
		if spec.schema != nil {
			subjects = append(subjects, schemaSubject{spec.name, spec.schema})
		}
	}
	return append(subjects, schemaSubject{"common", &commonSchema})
}

func schemaForSubject(catalogue []commandSpec, subject string) *schemaDescriptor {
	for _, selection := range schemaSubjects(catalogue) {
		if selection.name == subject {
			return selection.descriptor
		}
	}
	return nil
}

func parseSchemaArguments(catalogue []commandSpec, args []string) (
	descriptor *schemaDescriptor, output string, help bool, err error,
) {
	output = "-"
	var subject string
	subjectSeen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case isSilentSwitch(arg):
		case arg == "-h" || arg == "--help":
			help = true
		case arg == "-o" || arg == "--output":
			if i+1 == len(args) {
				return nil, "", false, fmt.Errorf("%s requires FILE", arg)
			}
			i++
			output = args[i]
		case strings.HasPrefix(arg, "--output="):
			output = strings.TrimPrefix(arg, "--output=")
		case strings.HasPrefix(arg, "-"):
			return nil, "", false, fmt.Errorf("unknown option %q", curlblocks.DisplayText(arg))
		default:
			if subjectSeen {
				return nil, "", false, errors.New("schema accepts exactly one subject")
			}
			subject = arg
			subjectSeen = true
		}
	}
	if help && !subjectSeen {
		return nil, output, true, nil
	}
	descriptor = schemaForSubject(catalogue, subject)
	if descriptor == nil {
		if subject == "" {
			return nil, "", false, errors.New("schema requires a subject")
		}
		return nil, "", false, fmt.Errorf("unknown schema subject %q", curlblocks.DisplayText(subject))
	}
	return descriptor, output, help, nil
}

func runSchemaCommand(
	_ context.Context, spec *commandSpec, catalogue []commandSpec,
	args []string, out commandOutput,
) (err error) {
	out, err = commandDiagnosticOutput(spec, args, out)
	if err != nil {
		return err
	}
	descriptor, path, help, err := parseSchemaArguments(catalogue, args)
	if err != nil {
		writeErr := writeDiagnosticDocument(out.stderr, diagnosticError, err.Error(), schemaUsage)
		return reportedCommandFailure(usage(errors.Join(err, writeErr)))
	}
	if help {
		_, err := io.WriteString(out.stdout, schemaUsage)
		return err
	}
	destination, err := openReportDestination(out, path)
	if err != nil {
		return err
	}
	defer func() { err = destination.finish(err) }()
	n, err := destination.writer.Write(descriptor.document)
	if err == nil && n != len(descriptor.document) {
		err = io.ErrShortWrite
	}
	if destination.isClosedPipe(err) {
		return nil
	}
	return err
}

func completeSchema(prior []string, current string) []completionCandidate {
	outputOption := curlblocks.CompletionOption{Description: "write schema to FILE", FileStyle: curlblocks.CompletionFiles}
	candidates := optionCompletionCandidates([]string{"-h", "--help"}, "show schema usage and exit")
	candidates = append(candidates, optionCompletionCandidates([]string{"-o", "--output"}, "write schema to FILE")...)
	candidates = append(candidates, silentCompletionCandidates()...)
	subjectSeen := false
	for i := 0; i < len(prior); i++ {
		switch arg := prior[i]; {
		case arg == "-o" || arg == "--output":
			if i+1 == len(prior) {
				return completeOptionValue(outputOption, current, "")
			}
			i++
		case strings.HasPrefix(arg, "-"):
		default:
			subjectSeen = true
		}
	}
	if value, ok := strings.CutPrefix(current, "--output="); ok {
		return completeOptionValue(outputOption, value, "--output=")
	}
	if !subjectSeen {
		for _, selection := range schemaSubjects(commandCatalogue) {
			description := "export current record schema"
			if selection.descriptor == &commonSchema {
				description = "export shared definitions"
			}
			candidates = append(candidates, completionCandidate{value: selection.name, description: description})
		}
	}
	return filterCompletionPrefix(candidates, current)
}
