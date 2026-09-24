package main

import (
	"os/signal"
	"syscall"
)

// configureProcessSignals applies the command's process-wide signal policy.
func configureProcessSignals() {
	signal.Ignore(syscall.SIGPIPE)
}
