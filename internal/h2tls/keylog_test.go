package h2tls_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tantosec/tturl/internal/h2tls"
)

func closeTestWriter(t *testing.T, w io.Writer) {
	t.Helper()
	if closer, ok := w.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			t.Fatalf("close key-log writer: %v", err)
		}
	}
}

func TestKeyLogWriter(t *testing.T) {
	t.Run("unset returns nil writer", func(t *testing.T) {
		t.Setenv(h2tls.KeyLogEnv, "")
		w, err := h2tls.KeyLogWriter()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Must be a true nil interface (not a typed nil *os.File) so the caller
		// can assign it straight to tls.Config.KeyLogWriter.
		if w != nil {
			t.Errorf("want nil writer when SSLKEYLOGFILE unset, got %T", w)
		}
	})

	t.Run("set appends, does not truncate", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.log")
		if err := os.WriteFile(path, []byte("EXISTING\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(h2tls.KeyLogEnv, path)

		w, err := h2tls.KeyLogWriter()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if w == nil {
			t.Fatal("want a writer when SSLKEYLOGFILE is set, got nil")
		}
		if _, err := io.WriteString(w, "SECRET\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
		closeTestWriter(t, w)
		got, err := os.ReadFile(path) //nolint:gosec // test reads its own temp file
		if err != nil {
			t.Fatal(err)
		}
		// curl appends key material to any existing capture log.
		if want := "EXISTING\nSECRET\n"; string(got) != want {
			t.Errorf("file = %q, want %q", got, want)
		}
	})

	t.Run("missing file is created", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keys.log")
		t.Setenv(h2tls.KeyLogEnv, path)

		w, err := h2tls.KeyLogWriter()
		if err != nil {
			t.Fatalf("KeyLogWriter: %v", err)
		}
		if w == nil {
			t.Fatal("want a writer when SSLKEYLOGFILE is set, got nil")
		}
		if _, err := io.WriteString(w, "SECRET\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
		closeTestWriter(t, w)

		got, err := os.ReadFile(path) //nolint:gosec // test reads its own temp file
		if err != nil {
			t.Fatalf("read created key log: %v", err)
		}
		if want := "SECRET\n"; string(got) != want {
			t.Errorf("file = %q, want %q", got, want)
		}
	})

	t.Run("unopenable path errors", func(t *testing.T) {
		// A path whose parent is a regular file cannot be created.
		parent := filepath.Join(t.TempDir(), "notadir")
		if err := os.WriteFile(parent, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "keys.log")
		t.Setenv(h2tls.KeyLogEnv, path)
		w, err := h2tls.KeyLogWriter()
		if err == nil {
			closeTestWriter(t, w)
			t.Fatal("want error for unopenable SSLKEYLOGFILE path, got nil")
		}
		if w != nil {
			t.Errorf("writer = %T, want nil on open error", w)
		}
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			t.Fatalf("error type = %T, want wrapped *os.PathError", err)
		}
		if pathErr.Op != "open" || pathErr.Path != path {
			t.Errorf("PathError = {Op:%q Path:%q}, want {Op:open Path:%q}",
				pathErr.Op, pathErr.Path, path)
		}
	})
}
