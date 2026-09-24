package h2tls

import (
	"fmt"
	"io"
	"os"
)

// KeyLogEnv names the environment variable containing the TLS key-log path.
const KeyLogEnv = "SSLKEYLOGFILE"

// KeyLogWriter opens the file named by [KeyLogEnv] for appending TLS session
// secrets. It returns nil, nil when the variable is unset. The result may be
// assigned directly to tls.Config.KeyLogWriter.
//
// The file requests mode 0600, although the operating system may apply
// different permissions. It remains open for the process lifetime. Anyone who
// can read it can decrypt captured sessions and must be trusted accordingly.
func KeyLogWriter() (io.Writer, error) {
	path := os.Getenv(KeyLogEnv)
	if path == "" {
		return nil, nil
	}
	//nolint:gosec // the path is operator-supplied, for TLS debugging
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%s %q: %w", KeyLogEnv, path, err)
	}
	return f, nil
}
