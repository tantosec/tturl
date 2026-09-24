package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStructuredResponseIntegrity(t *testing.T) {
	capture := structuredCapture{BodyBytes: new(int64(8))}
	response := structuredResponse{
		BodyBytes: 3, BodyCapturedBytes: 3, BodyCapturedComplete: true,
		SHA256: digestBase64([]byte("abc")), Body: &structuredEncodedData{Encoding: "base64", Data: "YWJj"},
	}
	if _, err := validateStructuredResponse(response, capture); err != nil {
		t.Fatal(err)
	}
	empty := structuredResponse{BodyCapturedComplete: true, SHA256: digestBase64(nil)}
	if _, err := validateStructuredResponse(empty, structuredCapture{BodyBytes: new(int64(0))}); err != nil {
		t.Fatal(err)
	}
	partial := response
	partial.BodyBytes, partial.BodyCapturedComplete = 9, false
	partial.SHA256 = digestBase64([]byte("abcdefghi"))
	if _, err := validateStructuredResponse(partial, capture); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*structuredResponse)
	}{
		{"digest", func(v *structuredResponse) { v.SHA256 = digestBase64([]byte("xyz")) }},
		{"decoded length", func(v *structuredResponse) { v.BodyCapturedBytes = 2 }},
		{"completeness", func(v *structuredResponse) { v.BodyCapturedComplete = false }},
		{"accepted bytes", func(v *structuredResponse) { v.BodyBytes = 2 }},
		{"encoding", func(v *structuredResponse) { v.Body.Encoding, v.Body.Data = "utf-8", "abc" }},
		{"base64", func(v *structuredResponse) { v.Body.Data = "YWJj!" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := response
			body := *response.Body
			value.Body = &body
			test.mutate(&value)
			if _, err := validateStructuredResponse(value, capture); err == nil {
				t.Fatal("corrupt response accepted")
			}
		})
	}
}

func TestStructuredExtractionIntegrity(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(schemaDirectory, "examples", "race-complete.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := jsonLines(t, data)
	var run jsonRaceRunRecord
	var trial jsonRaceTrialRecord
	if err := json.Unmarshal(lines[0], &run); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lines[4], &trial); err != nil {
		t.Fatal(err)
	}
	response := *trial.Outcomes[0].Response
	if err := validateStructuredExtracts(response, run.Acquisition.Capture, run.Plan.Extractors); err != nil {
		t.Fatal(err)
	}
	response.Extracts[0].Matches = response.Extracts[0].Matches[:1]
	if err := validateStructuredExtracts(response, run.Acquisition.Capture, run.Plan.Extractors); err == nil {
		t.Fatal("complete extraction with an omitted match accepted")
	}
}
