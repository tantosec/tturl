package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

func TestStructuredExamples(t *testing.T) {
	t.Parallel()
	paths, err := filepath.Glob(filepath.Join(schemaDirectory, "examples", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no protocol examples")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			//nolint:gosec // Paths come from the repository's fixed example directory.
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.HasSuffix(data, []byte("\n")) {
				t.Fatal("complete example needs a final newline")
			}
			lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
			var first conformanceRecord
			if err := json.Unmarshal(lines[0], &first); err != nil {
				t.Fatal(err)
			}
			spec := commandByName(first.Command)
			if spec == nil || spec.schema == nil {
				t.Fatal("unknown example command")
			}
			if first.Schema != spec.schema.logical {
				t.Fatalf("example schema = %s, want %s", first.Schema, spec.schema.logical)
			}
			schema := compileCurrentStructuredSchema(t, spec.schema.filename)
			records := make([]conformanceRecord, len(lines))
			for i, line := range lines {
				instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(line))
				if err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(instance); err != nil {
					t.Fatalf("line %d: %v", i+1, err)
				}
				if err := json.Unmarshal(line, &records[i]); err != nil {
					t.Fatal(err)
				}
			}
			validateConformanceStream(t, records)
			if first.Command == "analyse" {
				if _, err := replayAnalyseJSONStream(bytes.NewReader(data)); err != nil {
					t.Fatalf("example replay: %v", err)
				}
			}
		})
	}
}
