// tturl runs timeless timing attacks over HTTP/2. Its request grammar is
// provided by curlblocks, with command-wide execution and reporting flags added
// here. The request commands are:
//
//   - race: stream batch trials and report every response and arrival order
//     without inference.
//   - measure: run a fixed trial or rotation-cycle budget and report an
//     aggregated, descriptive mean-rank ranking.
//   - analyse: run a fixed balanced rotation and test whether request identity
//     changes relative response arrival order.
//   - detect: adaptively test for one early or late arrival-order outlier.
//   - time: describe initial-release-to-final-response-headers durations with
//     one active request per connection over HTTP/1.1 or HTTP/2.
//
// The independent demo-server command runs a local HTTP/2 education and
// research target with optional propagation delay below TLS.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"

	"github.com/tantosec/tturl/internal/buildinfo"
	"github.com/tantosec/tturl/internal/curlblocks"
)

// toolName is the product identity and canonical command spelling. Generated
// output uses it deliberately; renaming the executable does not rename the
// product or alter its help vocabulary.
const toolName = "tturl"

// reportedCommandError asks runCLI for a nonzero status after the command has
// already represented the condition in its terminal report. It retains the
// underlying error for direct callers and status tests without adding a second
// stderr diagnostic.
type reportedCommandError struct {
	err error
}

func (e *reportedCommandError) Error() string { return e.err.Error() }
func (e *reportedCommandError) Unwrap() error { return e.err }

func reportedCommandFailure(err error) error {
	return &reportedCommandError{err: err}
}

func main() {
	configureProcessSignals()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	stdoutInfo, _ := os.Stdout.Stat()
	out := commandOutput{
		stdin:  &processInput{ctx: ctx, file: os.Stdin},
		stdout: os.Stdout, stderr: os.Stderr,
		stdoutPipe: stdoutInfo != nil &&
			stdoutInfo.Mode()&os.ModeNamedPipe != 0,
	}

	// runCLI owns all stream output. Its error return keeps write and command
	// failures observable to direct callers; the process needs only its status.
	code, _ := runCLI(ctx, os.Args[1:], out)
	if code != 0 {
		os.Exit(code)
	}
}

func runCLI(
	ctx context.Context,
	args []string,
	out commandOutput,
) (code int, err error) {
	return runCLIForPlatform(ctx, args, out, runtime.GOOS)
}

func runCLIForPlatform(ctx context.Context, args []string, out commandOutput, platform string) (code int, err error) {
	if len(args) == 0 {
		commandErr := errors.New("command required")
		err := writeDiagnosticDocument(
			out.stderr, diagnosticError, commandErr.Error(), topLevelUsage)
		if err != nil {
			return 1, err
		}
		return 2, commandErr
	}

	command, rest := args[0], args[1:]
	switch command {
	case "-h", "--help":
		if _, err := fmt.Fprint(out.stdout, topLevelUsage); err != nil {
			return 1, err
		}
		return 0, nil
	case "-V", "--version":
		if err := writeVersion(out.stdout); err != nil {
			return 1, err
		}
		return 0, nil
	case "version":
		rest, out = silentPrefix(rest, out)
		if len(rest) != 0 {
			commandErr := errors.New("version accepts no arguments")
			writeErr := writeDiagnostic(
				out.stderr, diagnosticError, commandErr.Error())
			return 2, errors.Join(commandErr, writeErr)
		}
		if err := writeVersion(out.stdout); err != nil {
			return 1, err
		}
		return 0, nil
	case helpCommandName:
		return runHelpCommand(rest, out)
	case completionCommandName:
		return runCompletionCommand(rest, out)
	case completionQueryName:
		return runCompletionQuery(rest, out)
	}

	spec := commandByName(command)
	if spec == nil {
		commandErr := fmt.Errorf(
			"unknown command %q", curlblocks.DisplayText(command))
		writeErr := writeDiagnosticDocument(
			out.stderr, diagnosticError, commandErr.Error(), topLevelUsage)
		return 2, errors.Join(commandErr, writeErr)
	}
	out, selectionErr := commandDiagnosticOutput(spec, rest, out)
	if spec.id == commandTime {
		if startupErr := timeStartupAllowed(platform, rest); startupErr != nil {
			writeErr := writeDiagnostic(out.stderr, diagnosticError, startupErr.Error())
			return 1, errors.Join(startupErr, writeErr)
		}
	}
	if selectionErr != nil {
		writeErr := writeDiagnostic(out.stderr, diagnosticError, selectionErr.Error())
		return 2, errors.Join(selectionErr, writeErr)
	}
	runErr := spec.run(ctx, spec, commandCatalogue, rest, out)
	if runErr == nil {
		return 0, nil
	}
	code = 1
	if _, ok := errors.AsType[*usageError](runErr); ok {
		code = 2
	}
	var writeErr error
	if _, reported := errors.AsType[*reportedCommandError](runErr); !reported {
		writeErr = writeDiagnostic(out.stderr, diagnosticError, runErr.Error())
	}
	return code, errors.Join(runErr, writeErr)
}

func writeVersion(w io.Writer) error {
	_, err := fmt.Fprintln(w, buildinfo.String(toolName))
	return err
}

func announceCommand(stderr io.Writer, spec *commandSpec) error {
	notice := spec.lifecycle.notice(spec.name)
	if notice == "" {
		return nil
	}
	return writeDiagnostic(stderr, diagnosticNotice, notice)
}
