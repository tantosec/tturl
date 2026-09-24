package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tantosec/tturl/internal/ranking"
)

const schemaDirectory = "doc/schemas"

func TestCurrentSchemaDocumentsCompile(t *testing.T) {
	t.Parallel()
	for _, descriptor := range []*schemaDescriptor{
		&commonSchema, &raceSchema, &measureSchema, &analyseSchema, &detectSchema, &timeSchema,
	} {
		t.Run(descriptor.filename, func(t *testing.T) {
			compileCurrentStructuredSchema(t, descriptor.filename)
		})
	}
}

func TestUnavailableStructuredReportsConformToSchemas(t *testing.T) {
	tests := []struct {
		name   string
		run    func(context.Context, []string, commandOutput) error
		args   []string
		schema string
	}{
		{
			"race", runRace,
			[]string{"--trials", "1"},
			raceSchema.filename,
		},
		{
			"measure", runMeasure,
			[]string{"--trials", "2"},
			measureSchema.filename,
		},
		{
			"analyse", runAnalyse,
			[]string{"--cycles", "6"},
			analyseSchema.filename,
		},
		{
			"detect", runDetect,
			[]string{"--direction", "late", "--comparisons-max", "2"},
			detectSchema.filename,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schema := compileStructuredSchema(t, test.schema)
			out, stdout, _ := captureOutput(t)
			args := append([]string{"--report", "json"}, test.args...)
			args = append(args,
				"--block", "https://127.0.0.1:1/a", "--name", "A",
				"--block", "https://127.0.0.1:1/b", "--name", "B")
			if err := test.run(t.Context(), args, out); err == nil {
				t.Fatal("unavailable run returned no error")
			}
			records := jsonLines(t, stdout.Bytes())
			for index, record := range records {
				instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(record))
				if err != nil {
					t.Fatalf("record %d JSON: %v", index, err)
				}
				if err := schema.Validate(instance); err != nil {
					t.Fatalf("record %d does not conform: %v\n%s",
						index, err, record)
				}
			}
		})
	}
}

func TestStructuredCompletionStateVariants(t *testing.T) {
	schema := compileSchemaDefinition(t, commonSchema.filename, "completion")
	for _, completion := range []runCompletion{
		completedRun(), stoppedRun(),
		failedRun(context.DeadlineExceeded, completionAcquisitionFailed),
		failedRun(errors.New("acquisition failed"), completionAcquisitionFailed),
		failedRun(errors.New("comparison failed"), completionComparisonFailed),
		failedRun(errRunTimeout, completionRunTimeout),
	} {
		encoded, err := json.Marshal(makeStructuredCompletion(completion))
		if err != nil {
			t.Fatal(err)
		}
		instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(instance); err != nil {
			t.Fatalf("completion %s: %v", encoded, err)
		}
	}
}

func TestDetectSchemaEnforcesGuaranteeEdgeMeaning(t *testing.T) {
	schema := compileStructuredSchema(t, detectSchema.filename)
	for _, test := range []struct {
		name       string
		outcome    ranking.OutlierOutcome
		direction  ranking.OutlierErrorDirection
		resultEdge float64
	}{
		{
			name: "found", outcome: ranking.OutlierFound,
			direction: ranking.OutlierErrorFalseFind,
		},
		{
			name: "negative", outcome: ranking.OutlierNegative,
			direction: ranking.OutlierErrorFalseNegative, resultEdge: 0.1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			risk := 0.05
			if test.outcome == ranking.OutlierFound {
				risk = 0.01
			}
			result := ranking.OutlierResult{
				Outcome: test.outcome, Index: 0,
				OutlierGuarantee: ranking.OutlierGuarantee{
					ErrorDirection: test.direction, ErrorRiskLimit: risk,
					MinimumEdge: test.resultEdge, Direction: ranking.Late,
					Premise: ranking.OutlierPremiseScheduledReferenceMean,
					Regime:  ranking.OutlierInferenceAnytimeValid,
				},
				Cost: ranking.Cost{Comparisons: 1, Participations: 2},
				Items: []ranking.ItemStat{
					{Samples: 1, MeanRank: 1}, {Samples: 1, MeanRank: 0},
				},
				Detail: ranking.OutlierDiagnostics{
					Threshold: 3, MinimumEdge: 0.1,
					Items: []ranking.OutlierItemStat{{}, {}},
				},
			}
			var encoded bytes.Buffer
			reporter := startDetectJSONCatalogue(t, &encoded)
			if err := reporter.Result(detectRunResult{
				Ranking: result,
				Requests: []detectRequestEvidence{
					{
						Request: 0, Participations: 1, RankSum: 1,
						StatusCounts: []detectStatusCount{{Status: 204, Count: 1}},
					},
					{
						Request: 1, Participations: 1,
						StatusCounts: []detectStatusCount{{Status: 204, Count: 1}},
					},
				},
				Completion: completedRun(),
			}); err != nil {
				t.Fatal(err)
			}
			var object map[string]any
			if err := json.Unmarshal(lastJSONRecord(t, encoded.Bytes()), &object); err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(object); err != nil {
				t.Fatalf("valid %s result rejected: %v", test.name, err)
			}
			detection := object["detection"].(map[string]any)
			finding := detection["finding"].(map[string]any)
			guarantee := finding["guarantee"].(map[string]any)
			wantDirection, _ := detectErrorDirection(test.direction)
			if guarantee["error_direction"] != wantDirection || guarantee["error_risk_limit"] != risk {
				t.Errorf("guarantee = %+v", guarantee)
			}
			if test.outcome == ranking.OutlierNegative && finding["excluded_edge"] != test.resultEdge {
				t.Errorf("excluded_edge = %v, want %v", finding["excluded_edge"], test.resultEdge)
			}
		})
	}
}

func compileStructuredSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	return compileCurrentStructuredSchema(t, name)
}

func compileSchemaDefinition(
	t *testing.T, filename, definition string,
) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(offlineSchemaLoader{})
	compiler.AssertFormat()
	identifier, document := loadCurrentSchemaDocument(t, currentSchemaByFilename(filename))
	if err := compiler.AddResource(identifier, document); err != nil {
		t.Fatalf("register schema %s: %v", filename, err)
	}
	schema, err := compiler.Compile(identifier + "#/$defs/" + definition)
	if err != nil {
		t.Fatalf("compile %s definition %s: %v", filename, definition, err)
	}
	return schema
}

func compileCurrentStructuredSchema(t *testing.T, name string) *jsonschema.Schema {
	t.Helper()
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(offlineSchemaLoader{})
	compiler.AssertFormat()
	var target string
	filenames := []string{commonSchema.filename}
	if name != commonSchema.filename {
		filenames = append(filenames, name)
	}
	for _, filename := range filenames {
		id, document := loadCurrentSchemaDocument(t, currentSchemaByFilename(filename))
		if err := compiler.AddResource(id, document); err != nil {
			t.Fatal(err)
		}
		target = id
	}
	schema, err := compiler.Compile(target)
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func TestStructuredValueDomains(t *testing.T) {
	t.Parallel()
	id, document := loadCurrentSchemaDocument(t, &commonSchema)
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(offlineSchemaLoader{})
	if err := compiler.AddResource(id, document); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		definition string
		instance   string
		valid      bool
	}{
		{"positive_integer", "0", false},
		{"positive_integer", "1", true},
		{"positive_integer", "null", false},
		{"nullable_positive_integer", "0", false},
		{"nullable_positive_integer", "1", true},
		{"nullable_positive_integer", "null", true},
		{"non_negative_integer", "0", true},
		{"non_negative_integer", "1", true},
		{"non_negative_integer", "null", false},
		{"completion", `{}`, false},
		{"completion", `{"state":"complete","failure":{"code":"interrupted","message":"failed"}}`, false},
		{"completion", `{"state":"stopped","failure":{"code":"interrupted","message":"failed"}}`, true},
		{"completion", `{"state":"stopped"}`, false},
		{"completion", `{"state":"failed","failure":{"code":"interrupted"}}`, false},
		{"completion", `{"state":"failed"}`, false},
		{"response_limits", `{"body_bytes":1,"batch_timeout_ns":1}`, true},
		{"response_limits", `{"body_bytes":null,"batch_timeout_ns":null}`, true},
		{"response_limits", `{"body_bytes":0,"batch_timeout_ns":1}`, false},
		{"response_limits", `{"body_bytes":1,"batch_timeout_ns":0}`, false},
		{"response_limits", `{"body_bytes":1}`, false},
		{"response_limits", `{"batch_timeout_ns":1}`, false},
		{"capture", `{"headers":false,"body_bytes":0}`, true},
		{"capture", `{"headers":false,"body_bytes":1}`, true},
		{"capture", `{"headers":false,"body_bytes":null}`, true},
		{"capture", `{"headers":true,"body_bytes":1,"decoded_header_limit_bytes":1}`, true},
		{"capture", `{"headers":true,"body_bytes":1,"decoded_header_limit_bytes":0}`, false},
		{"capture", `{"headers":true,"body_bytes":1,"decoded_header_limit_bytes":null}`, false},
		{"capture", `{"headers":true,"body_bytes":1}`, false},
		{"capture", `{"headers":false}`, false},
		{"maximum", `{"mode":"bounded","connections":1}`, true},
		{"maximum", `{"mode":"bounded","connections":0}`, false},
		{"maximum", `{"mode":"bounded","connections":null}`, false},
		{"maximum", `{"mode":"bounded"}`, false},
		{"maximum", `{"mode":"automatic_review"}`, true},
		{"maximum", `{"mode":"unlimited"}`, true},
	}
	for _, test := range tests {
		t.Run(test.definition+"/"+test.instance, func(t *testing.T) {
			schema, err := compiler.Compile(id + "#/$defs/" + test.definition)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := jsonschema.UnmarshalJSON(strings.NewReader(test.instance))
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(instance); (err == nil) != test.valid {
				t.Fatalf("validation error = %v, want valid %t", err, test.valid)
			}
		})
	}
}

func loadCurrentSchemaDocument(t *testing.T, descriptor *schemaDescriptor) (string, any) {
	t.Helper()
	data, err := exportCurrentSchemaDocument(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return document.(map[string]any)["$id"].(string), document
}

func currentSchemaByFilename(filename string) *schemaDescriptor {
	if filename == commonSchema.filename {
		return &commonSchema
	}
	for i := range commandCatalogue {
		schema := commandCatalogue[i].schema
		if schema != nil && schema.filename == filename {
			return schema
		}
	}
	return nil
}

// offlineSchemaLoader rejects resources outside the explicitly registered set.
type offlineSchemaLoader struct{}

func (offlineSchemaLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("unregistered schema resource %s", url)
}
