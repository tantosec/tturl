//go:build !windows

package main

import (
	"os"
	"os/exec"
)

const nativeExecutableSuffix = ""

func configureNativeInterrupt(*exec.Cmd) {}

func interruptNativeProcess(process *os.Process) error {
	return process.Signal(os.Interrupt)
}
