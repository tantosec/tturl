package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

var sigpipeHelperEnv = strings.ToUpper(toolName) + "_SIGPIPE_HELPER"

func TestClosedStdoutPipeReturnsAnError(t *testing.T) {
	if os.Getenv(sigpipeHelperEnv) == "1" {
		configureProcessSignals()
		block := make([]byte, 32*1024)
		for {
			if _, err := os.Stdout.Write(block); err != nil {
				// This branch runs only in the subprocess. Exit before the test
				// or coverage runtime writes to the deliberately closed stdout.
				os.Exit(0)
			}
		}
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	cmd := exec.CommandContext( //nolint:gosec // fixed re-exec of this test binary
		t.Context(), os.Args[0],
		"-test.run=^TestClosedStdoutPipeReturnsAnError$")
	cmd.Env = append(os.Environ(), sigpipeHelperEnv+"=1")
	cmd.Stdout = writer
	if err := cmd.Start(); err != nil {
		closeTestFile(t, reader)
		closeTestFile(t, writer)
		t.Fatalf("starting helper: %v", err)
	}
	closeTestFile(t, writer)
	closeTestFile(t, reader)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper did not handle its closed stdout pipe: %v", err)
	}
}

func closeTestFile(t *testing.T, file *os.File) {
	t.Helper()
	if err := file.Close(); err != nil {
		t.Fatalf("closing %s: %v", file.Name(), err)
	}
}
