package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tantosec/tturl/internal/h2test"
)

func TestVaryFileRequestRunners(t *testing.T) {
	file := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(file, []byte("TOKEN"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		for _, policy := range []string{"true", "false"} {
			t.Run(command+"/"+policy, func(t *testing.T) {
				var output bytes.Buffer
				args := []string{
					command, "--dry-run", "--verbose", "--vary-file-content=" + policy,
					"--data-binary", "@" + file, "--vary", "TOKEN={a,b}", "https://example.invalid/TOKEN",
				}
				if command == "detect" {
					args = append(args, "--direction", "late")
				}
				code, err := runCLI(t.Context(), args, commandOutput{stdout: &output, stderr: io.Discard})
				if code != 0 || err != nil {
					t.Fatalf("status %d: %v", code, err)
				}
				wants := []string{":path: /a", ":path: /b"}
				if policy == "true" {
					wants = append(wants, "\na\n", "\nb\n")
				} else {
					wants = append(wants, "\nTOKEN\n")
				}
				for _, want := range wants {
					if !strings.Contains(output.String(), want) {
						t.Fatalf("missing %q: %s", want, &output)
					}
				}
			})
		}
	}
}

func TestVariedStdinLocalExecution(t *testing.T) {
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		for _, policy := range []string{"true", "false"} {
			t.Run(command+"/"+policy, func(t *testing.T) {
				var requests atomic.Int64
				addr, _ := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					want := strings.TrimPrefix(r.URL.Path, "/")
					if policy == "false" {
						want = "TOKEN"
					}
					if err != nil || string(body) != want {
						t.Errorf("body %q, want %q: %v", body, want, err)
					}
					requests.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}))
				args := []string{
					command, "--insecure", "--vary-file-content=" + policy, "--vary", "TOKEN={a,b}",
					"--data-binary", "@-", "--report", "json", "https://" + addr + "/TOKEN",
				}
				switch command {
				case "race", "measure":
					args = append(args, "--trials", "2")
				case "analyse":
					args = append(args, "--cycles", "6")
				case "detect":
					args = append(args, "--direction", "late", "--comparisons-max", "2")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				var output bytes.Buffer
				code, err := runCLI(ctx, args, commandOutput{
					stdin: strings.NewReader("TOKEN"), stdout: &output, stderr: io.Discard,
				})
				if code != 0 || err != nil {
					t.Fatalf("status %d: %v", code, err)
				}
				if requests.Load() < 4 {
					t.Fatalf("requests: %d", requests.Load())
				}
				lines := jsonLines(t, output.Bytes())
				var envelope structuredRunEnvelope
				if err := json.Unmarshal(lines[0], &envelope); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(envelope.Argv, wireStrings(args)) {
					t.Fatalf("argv %q, want %q", envelope.Argv, args)
				}
				count := 0
				for _, line := range lines {
					var record conformanceRecord
					if err := json.Unmarshal(line, &record); err != nil {
						t.Fatal(err)
					}
					if record.Kind != "request" {
						continue
					}
					count++
					want := strings.TrimPrefix(record.Label, "TOKEN=")
					if policy == "false" {
						want = "TOKEN"
					}
					if record.Body == nil || string(record.Body.Data) != want {
						t.Fatalf("request body: %+v", record.Body)
					}
				}
				if count != 2 {
					t.Fatalf("request evidence: %d", count)
				}
				if !bytes.Contains(output.Bytes(), []byte(`"@-"`)) {
					t.Fatal("raw argv reference missing")
				}
			})
		}
	}
}

func TestVariedStdinAndLiteralSourceReuse(t *testing.T) {
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		var output bytes.Buffer
		args := []string{
			command, "--dry-run", "--verbose", "--data-binary", "@-",
			"--vary", "FIRST=@-", "--vary", "SECOND={z}", "https://example.invalid/",
		}
		if command == "detect" {
			args = append(args, "--direction", "late")
		}
		code, err := runCLI(t.Context(), args, commandOutput{
			stdin: strings.NewReader("FIRST\nSECOND\n"), stdout: &output, stderr: io.Discard,
		})
		if code != 0 || err != nil {
			t.Fatalf("%s status %d: %v", command, code, err)
		}
		for _, want := range []string{"$HEX[46495253540a7a0a]", "$HEX[5345434f4e440a7a0a]"} {
			if !strings.Contains(output.String(), want) {
				t.Fatalf("%s missing %s: %s", command, want, &output)
			}
		}
	}
}

func TestVaryPreparationFailureHasNoNetworkWork(t *testing.T) {
	for _, command := range []string{"race", "measure", "analyse", "detect"} {
		for _, test := range []struct {
			name, input, flag string
			options           []string
		}{
			{"json", "TOKEN", "--json", nil},
			{"unused", "literal", "--data-binary", nil},
			{"origin", "TOKEN", "--data-binary", nil},
		} {
			t.Run(command+"/"+test.name, func(t *testing.T) {
				var requests atomic.Int64
				addr, _ := h2test.Serve(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					requests.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}))
				args := []string{command, "--insecure", test.flag, "@-", "--vary", "TOKEN={1,invalid}", "https://" + addr + "/"}
				if test.name == "origin" {
					args = []string{
						command, "--insecure", "--data-binary", "@-", "--vary",
						"TOKEN={" + addr + ",example.invalid}", "https://TOKEN/",
					}
				}
				args = append(args, test.options...)
				if command == "detect" {
					args = append(args, "--direction", "late")
				}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				code, err := runCLI(ctx, args, commandOutput{
					stdin: strings.NewReader(test.input), stdout: io.Discard, stderr: io.Discard,
				})
				if code == 0 || err == nil {
					t.Fatal("invalid preparation succeeded")
				}
				if requests.Load() != 0 {
					t.Fatalf("preparation sent %d requests", requests.Load())
				}
			})
		}
	}
}
