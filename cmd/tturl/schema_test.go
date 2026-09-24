package main

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCurrentSchemaDeclarations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		subject, version, owner string
		descriptor              *schemaDescriptor
	}{
		{"race", "v1", "race_json.go", &raceSchema},
		{"measure", "v1", "measure_json.go", &measureSchema},
		{"analyse", "v1", "analyse_json_stream.go", &analyseSchema},
		{"detect", "v1", "detect_json_stream.go", &detectSchema},
		{"time", "v1", "time_schema.go", &timeSchema},
		{"common", "v1", "structured_protocol.go", &commonSchema},
	}
	const base = "https://raw.githubusercontent.com/tantosec/tturl/main/cmd/tturl/doc/schemas/"
	for _, test := range tests {
		t.Run(test.subject, func(t *testing.T) {
			filename := "tturl-" + test.subject + "-" + test.version + ".schema.json"
			if test.descriptor.filename != filename {
				t.Fatalf("filename = %s, want %s", test.descriptor.filename, filename)
			}
			//nolint:gosec // Paths come from the fixed canonical schema family.
			data, err := os.ReadFile(filepath.Join(schemaDirectory, filename))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, test.descriptor.document) {
				t.Fatal("embedded document differs from canonical resource")
			}
			var doc map[string]any
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if doc["$id"] != base+filename || doc["$schema"] != "https://json-schema.org/draft/2020-12/schema" {
				t.Fatalf("document identity = %v, dialect = %v", doc["$id"], doc["$schema"])
			}
			if test.subject != "common" {
				spec := commandByName(test.subject)
				if spec.schema != test.descriptor {
					t.Fatal("catalogue selects a different descriptor")
				}
				logical := "tturl." + test.subject + "/" + test.version
				if test.descriptor.logical != logical {
					t.Fatalf("logical identifier = %s, want %s", test.descriptor.logical, logical)
				}
				run := doc["$defs"].(map[string]any)["run"].(map[string]any)
				properties := run["allOf"].([]any)[1].(map[string]any)["properties"].(map[string]any)
				if properties["schema"].(map[string]any)["const"] != logical {
					t.Fatal("run schema identifier differs from declaration")
				}
				references := 0
				var walk func(any)
				walk = func(v any) {
					switch v := v.(type) {
					case map[string]any:
						if ref, ok := v["$ref"].(string); ok && !strings.HasPrefix(ref, "#") {
							resource, _, _ := strings.Cut(ref, "#")
							if resource != commonSchema.filename {
								t.Errorf("unexpected dependency %s", ref)
							}
							references++
						}
						for _, value := range v {
							walk(value)
						}
					case []any:
						for _, value := range v {
							walk(value)
						}
					}
				}
				walk(doc)
				if references == 0 {
					t.Fatal("command has no common references")
				}
			}
			source, err := parser.ParseFile(token.NewFileSet(), test.owner, nil, parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			embeds := []string{}
			for _, group := range source.Comments {
				for _, comment := range group.List {
					if path, ok := strings.CutPrefix(comment.Text, "//go:embed "); ok {
						embeds = append(embeds, path)
					}
				}
			}
			if len(embeds) != 1 || embeds[0] != "doc/schemas/"+filename {
				t.Fatalf("embed paths = %v", embeds)
			}
		})
	}
}
