package main

import (
	"context"
	"os"
)

// processInput borrows process stdin for an exclusive reader. Its platform
// adapter interrupts inherited blocking input without closing the file or
// leaving a read goroutine behind. Construction performs no input operations.
// The platform-specific code is confined to this process boundary.
type processInput struct {
	ctx  context.Context
	file *os.File
}
