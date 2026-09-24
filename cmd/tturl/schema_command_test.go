package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestSchemaExports(t *testing.T) {
	t.Parallel()
	for _, subject := range []string{"race", "measure", "analyse", "detect", "time", "common"} {
		t.Run(subject, func(t *testing.T) {
			descriptor := schemaForSubject(commandCatalogue, subject)
			canonical, err := os.ReadFile(filepath.Join(schemaDirectory, descriptor.filename))
			if err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]string{
				{"schema", subject},
				{"schema", subject, "-o", "-"},
				{"schema", "--output", "-", subject},
				{"schema", subject, "--output=-"},
				{"schema", "-o", "unused", subject, "--output", "-"},
			} {
				var stdout, stderr bytes.Buffer
				code, err := runCLI(context.Background(), args, commandOutput{
					stdout: &stdout, stderr: &stderr,
					openOutput: func(string) (io.WriteCloser, error) { t.Fatal("stdout export opened a file"); return nil, nil },
				})
				if code != 0 || err != nil || stderr.Len() != 0 {
					t.Fatalf("%v: code %d, err %v, stderr %q", args, code, err, stderr.String())
				}
				if !bytes.Equal(stdout.Bytes(), canonical) {
					t.Fatalf("%v: exported bytes differ", args)
				}

				var identity struct {
					ID string `json:"$id"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &identity); err != nil {
					t.Fatalf("export is not one JSON document: %v", err)
				}
				if !strings.HasSuffix(identity.ID, "/"+descriptor.filename) {
					t.Fatalf("export identity = %s", identity.ID)
				}
			}
			path := filepath.Join(t.TempDir(), "schema.json")
			var stdout, stderr bytes.Buffer
			code, err := runCLI(context.Background(), []string{"schema", subject, "-o", path}, commandOutput{
				stdout: &stdout, stderr: &stderr,
			})
			if code != 0 || err != nil || stdout.Len() != 0 || stderr.Len() != 0 {
				t.Fatalf("file export: code %d, err %v", code, err)
			}
			//nolint:gosec // Path is the selected output in a Go temporary directory.
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, canonical) {
				t.Fatal("file export differs from canonical document")
			}
		})
	}
}

func TestSchemaFileOutput(t *testing.T) {
	t.Parallel()
	for _, options := range []struct {
		name string
		args func(string) []string
	}{
		{"short before", func(path string) []string { return []string{"-o", path, "race"} }},
		{"short after", func(path string) []string { return []string{"race", "-o", path} }},
		{"long before", func(path string) []string { return []string{"--output", path, "race"} }},
		{"long after", func(path string) []string { return []string{"race", "--output", path} }},
		{"attached before", func(path string) []string { return []string{"--output=" + path, "race"} }},
		{"attached after", func(path string) []string { return []string{"race", "--output=" + path} }},
		{"last wins", func(path string) []string {
			return []string{"--output", filepath.Join(filepath.Dir(path), "unused"), "race", "-o", path}
		}},
	} {
		t.Run(options.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schema.json")
			for _, existing := range []bool{false, true} {
				if existing {
					if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(raceSchema.document)+100), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				var stdout, stderr bytes.Buffer
				code, err := runCLI(context.Background(), append([]string{"schema"}, options.args(path)...), commandOutput{
					stdout: &stdout, stderr: &stderr,
				})
				if code != 0 || err != nil || stdout.Len() != 0 || stderr.Len() != 0 {
					t.Fatalf("code %d, err %v, stdout %q, stderr %q", code, err, stdout.String(), stderr.String())
				}
				//nolint:gosec // Path is the selected output in a Go temporary directory.
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(data, raceSchema.document) {
					t.Fatal("file differs from selected document")
				}
			}
		})
	}
}

func TestSchemaArgumentValidation(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{},
		{"RACE"},
		{""},
		{"", "race"},
		{"race", ""},
		{"race", "--help", "common"},
		{"--help", "--unknown"},
		{"unknown"},
		{"race", "common"},
		{"race", "--unknown"},
		{"-o"},
		{"race", "--output"},
		{"-o", "file"},
		{"--output=file", "common", "extra"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "output.json")
			if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			commandArgs := append([]string{"schema", "--output", path}, args...)
			code, err := runCLI(context.Background(), commandArgs, commandOutput{
				stdout: &stdout, stderr: &stderr,
				openOutput: func(string) (io.WriteCloser, error) {
					t.Fatal("invalid arguments opened output")
					return nil, nil
				},
			})
			if code != 2 || err == nil || stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("code %d, err %v, stdout %q, stderr %q", code, err, stdout.String(), stderr.String())
			}
			if !bytes.Contains(stderr.Bytes(), []byte(schemaUsage)) {
				t.Fatal("argument error omitted usage")
			}
			//nolint:gosec // Path is the selected output in a Go temporary directory.
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "existing" {
				t.Fatalf("output changed: %q, %v", data, err)
			}
		})
	}
}

func TestSchemaHelp(t *testing.T) {
	t.Parallel()
	checkHelpTextContract(t, "schema usage", schemaUsage)
	_, subjects, ok := strings.Cut(schemaUsage, "Subjects:\n")
	if !ok {
		t.Fatal("schema subjects section missing")
	}
	subjects, _, ok = strings.Cut(subjects, "\nOptions:\n")
	if !ok {
		t.Fatal("schema options section missing")
	}
	for _, selection := range schemaSubjects(commandCatalogue) {
		if !strings.Contains(subjects, selection.name) {
			t.Errorf("schema usage omits subject %q", selection.name)
		}
	}
	for _, args := range [][]string{
		{"schema", "-h"},
		{"schema", "--help"},
		{"schema", "-o", "unused", "--help"},
		{"help", "schema"},
	} {
		var stdout, stderr bytes.Buffer
		code, err := runCLI(context.Background(), args, commandOutput{
			stdout: &stdout, stderr: &stderr,
			openOutput: func(string) (io.WriteCloser, error) {
				t.Fatal("help opened output")
				return nil, nil
			},
		})
		if code != 0 || err != nil || stdout.Len() == 0 || stderr.Len() != 0 {
			t.Fatalf("%v: code %d, err %v, stderr %q", args, code, err, stderr.String())
		}
		if args[0] == "schema" && stdout.String() != schemaUsage {
			t.Fatal("help did not route to schema usage")
		}

		if args[0] == "help" {
			topic, ok := lookupHelpTopic("schema")
			if !ok || stdout.String() != renderHelpTopic(topic) {
				t.Fatal("help did not route to schema topic")
			}
		}
	}
}

func TestSchemaOutputFailures(t *testing.T) {
	t.Parallel()
	failure := errors.New("output failed")
	tests := []struct {
		name                        string
		file                        bool
		pipe                        bool
		short                       bool
		openErr, writeErr, closeErr error
		code                        int
	}{
		{name: "stdout success", code: 0},
		{name: "open", file: true, openErr: failure, code: 1},
		{name: "file write", file: true, writeErr: failure, code: 1},
		{name: "file close", file: true, closeErr: failure, code: 1},
		{name: "file write and close", file: true, writeErr: failure, closeErr: io.ErrUnexpectedEOF, code: 1},
		{name: "file closed pipe", file: true, pipe: true, writeErr: io.ErrClosedPipe, code: 1},
		{name: "stdout short write", short: true, code: 1},
		{name: "file short write", file: true, short: true, code: 1},
		{name: "stdout write", writeErr: failure, code: 1},
		{name: "pipe other error", pipe: true, writeErr: failure, code: 1},
		{name: "unrecognised closed pipe", writeErr: io.ErrClosedPipe, code: 1},
		{name: "closed stdout pipe", pipe: true, writeErr: io.ErrClosedPipe, code: 0},
		{name: "stdout EPIPE", pipe: true, writeErr: syscall.EPIPE, code: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			file := &schemaOutputFixture{writeErr: test.writeErr, closeErr: test.closeErr, short: test.short}
			args := []string{"schema", "race"}
			if test.file {
				args = append(args, "-o", "selected.json")
			}
			opened := false
			code, err := runCLI(context.Background(), args, commandOutput{
				stdout: file, stderr: &stderr, stdoutPipe: test.pipe,
				openOutput: func(path string) (io.WriteCloser, error) {
					opened = true
					if path != "selected.json" {
						t.Fatalf("path = %q", path)
					}
					return file, test.openErr
				},
			})
			if code != test.code {
				t.Fatalf("code %d, err %v, stderr %q", code, err, stderr.String())
			}
			if code == 0 {
				if err != nil || stderr.Len() != 0 {
					t.Fatalf("quiet success: err %v, stderr %q", err, stderr.String())
				}
			} else {
				if test.short && !errors.Is(err, io.ErrShortWrite) {
					t.Errorf("err = %v, want short write", err)
				}
				if err == nil || stderr.Len() == 0 {
					t.Fatal("output failure has no diagnostic")
				}
				for _, want := range []error{test.openErr, test.writeErr, test.closeErr} {
					if want != nil && !errors.Is(err, want) {
						t.Errorf("err %v does not wrap %v", err, want)
					}
				}
			}
			if opened != test.file || file.closed != (test.file && test.openErr == nil) {
				t.Fatalf("opened %v, closed %v", opened, file.closed)
			}
		})
	}
}

type schemaOutputFixture struct {
	writeErr, closeErr error
	closed             bool
	short              bool
}

func (f *schemaOutputFixture) Write(p []byte) (int, error) {
	if f.short {
		return len(p) / 2, nil
	}
	if f.writeErr != nil {
		return len(p) / 2, f.writeErr
	}
	return len(p), nil
}
func (f *schemaOutputFixture) Close() error { f.closed = true; return f.closeErr }

func TestSchemaCompletion(t *testing.T) {
	assertCompletionValues(t, []string{toolName, "sch"}, "schema")
	assertCompletionValues(t, []string{toolName, "schema", ""},
		"race", "measure", "analyse", "detect", "time", "common",
		"-o", "--output", "-h", "--help")
	for _, subject := range []string{"race", "measure", "analyse", "detect", "time", "common"} {
		assertCompletionValues(t, []string{toolName, "schema", subject[:2]}, subject)
		assertCompletionValues(t, []string{toolName, "schema", "-o", "file", subject[:2]}, subject)
	}
	assertCompletionValues(t, []string{toolName, "schema", "race", "--o"}, "--output")
}

// exportCurrentSchemaDocument exercises the command boundary for resolvers.
func exportCurrentSchemaDocument(descriptor *schemaDescriptor) ([]byte, error) {
	subject := ""
	for _, selection := range schemaSubjects(commandCatalogue) {
		if selection.descriptor == descriptor {
			subject = selection.name
			break
		}
	}
	var stdout, stderr bytes.Buffer
	code, err := runCLI(context.Background(), []string{"schema", subject}, commandOutput{
		stdout: &stdout, stderr: &stderr,
	})
	if err != nil {
		return nil, err
	}
	if code != 0 || stderr.Len() != 0 {
		return nil, fmt.Errorf("schema export status %d: %s", code, stderr.String())
	}
	return stdout.Bytes(), nil
}

func TestSchemaMetaPreservesOutputFiles(t *testing.T) {
	t.Parallel()
	for _, existing := range []bool{false, true} {
		for _, meta := range []struct {
			args []string
			code int
		}{
			{[]string{"--help"}, 0}, {[]string{"race", "extra"}, 2},
		} {
			path := filepath.Join(t.TempDir(), "schema.json")
			if existing {
				if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			args := append([]string{"schema", "-o", path}, meta.args...)
			code, err := runCLI(context.Background(), args, commandOutput{
				stdout: &stdout, stderr: &stderr,
			})
			if code != meta.code {
				t.Fatalf("%v: code %d, err %v", args, code, err)
			}
			//nolint:gosec // Path is the selected output in a Go temporary directory.
			data, readErr := os.ReadFile(path)
			if existing {
				if readErr != nil || string(data) != "existing" {
					t.Fatalf("output changed: %q, %v", data, readErr)
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				t.Fatalf("output created: %q, %v", data, readErr)
			}
		}
	}
}
