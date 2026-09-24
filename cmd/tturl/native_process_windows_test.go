package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

const nativeExecutableSuffix = ".exe"

// Go does not implement Process.Signal(os.Interrupt) on Windows. A separate
// process group lets GenerateConsoleCtrlEvent target only the demo-server
// child.
func configureNativeInterrupt(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP,
	}
}

func interruptNativeProcess(process *os.Process) error {
	dll, err := syscall.LoadDLL("kernel32.dll")
	if err != nil {
		return fmt.Errorf("load kernel32.dll: %w", err)
	}
	defer func() { _ = dll.Release() }()
	generate, err := dll.FindProc("GenerateConsoleCtrlEvent")
	if err != nil {
		return fmt.Errorf("find GenerateConsoleCtrlEvent: %w", err)
	}
	result, _, callErr := generate.Call(
		syscall.CTRL_BREAK_EVENT, uintptr(process.Pid))
	if result == 0 {
		return fmt.Errorf("GenerateConsoleCtrlEvent: %w", callErr)
	}
	return nil
}
