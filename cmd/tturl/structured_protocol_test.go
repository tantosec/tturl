package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tantosec/tturl/tth2"
)

func TestStructuredFailureClassification(t *testing.T) {
	body := &tth2.ResponseBodyLimitError{Limit: 4, BytesAccepted: 4}
	batch := &tth2.BatchTimeoutError{Limit: time.Second}
	tests := []struct {
		code     string
		err      error
		fallback string
	}{
		{"response_body_limit", body, "acquisition_failed"},
		{"batch_timeout", batch, "acquisition_failed"},
		{"run_timeout", errRunTimeout, "acquisition_failed"},
		{"interrupted", context.Canceled, "acquisition_failed"},
		{"deadline_exceeded", context.DeadlineExceeded, "acquisition_failed"},
		{"connection_error", &tth2.ConnectionError{Err: errors.New("dial failed")}, "acquisition_failed"},
		{"transport_error", &tth2.StreamError{StreamID: 1}, "acquisition_failed"},
		{"comparison_failed", errors.New("comparison failed"), "comparison_failed"},
		{"acquisition_failed", errors.New("collection failed"), "acquisition_failed"},
		{"internal_error", errors.New("invariant failed"), "internal_error"},
		{"response_body_limit", errors.Join(context.Canceled, batch, body), "internal_error"},
		{"batch_timeout", errors.Join(context.Canceled, batch), "internal_error"},
		{"run_timeout", errors.Join(context.Canceled, errRunTimeout), "internal_error"},
		{"interrupted", errors.Join(context.DeadlineExceeded, context.Canceled), "internal_error"},
		{"deadline_exceeded", &tth2.ConnectionError{Err: context.DeadlineExceeded}, "internal_error"},
		{"internal_error", errors.New(""), "internal_error"},
	}
	for i, test := range tests {
		t.Run(fmt.Sprintf("%s_%d", test.code, i), func(t *testing.T) {
			for _, err := range []error{test.err, fmt.Errorf("operation: %w", test.err)} {
				failure := makeStructuredFailure(err, test.fallback)
				if failure == nil || failure.Code != test.code || failure.Message == "" {
					t.Fatalf("failure = %+v, want %s", failure, test.code)
				}
				if err.Error() != "" && string(failure.Message) != err.Error() {
					t.Errorf("diagnostic = %q, want %q", failure.Message, err.Error())
				}
			}
		})
	}
	if failure := makeStructuredFailure(nil, "internal_error"); failure != nil {
		t.Errorf("successful operation failure = %+v", failure)
	}
}

func TestStructuredCompletionPreservesCause(t *testing.T) {
	for _, completion := range []runCompletion{
		completedRun(), stoppedRun(), failedRun(context.DeadlineExceeded, completionAcquisitionFailed),
		failedRun(&tth2.ConnectionError{Err: errors.New("dial failed")}, completionAcquisitionFailed),
		failedRun(&tth2.ResponseBodyLimitError{Limit: 4}, completionAcquisitionFailed),
	} {
		projected := makeStructuredCompletion(completion)
		if projected.State != completion.State {
			t.Errorf("state = %s, want %s", projected.State, completion.State)
		}
		if completion.State == completionComplete {
			if projected.Failure != nil {
				t.Errorf("complete failure = %+v", projected.Failure)
			}
		} else if projected.Failure == nil || projected.Failure.Message == "" {
			t.Errorf("completion = %+v", projected)
		}
	}
}

func TestStructuredDeliveryPolicies(t *testing.T) {
	tests := []struct {
		name        string
		selection   connectionFit
		fit         bool
		cap         int
		capSet      bool
		batchRate   float64
		requestRate float64
		ceiling     int
		limit       int
		fitted      int
		wantMode    string
	}{
		{name: "default", ceiling: 1, limit: 1, wantMode: "default"},
		{name: "fixed narrowed", selection: connectionExplicit, ceiling: 8, limit: 2, wantMode: "fixed"},
		{name: "fractional rate", batchRate: 0.5, ceiling: 1, limit: 1, wantMode: "default"},
		{name: "request rate", requestRate: 25, ceiling: 1, limit: 1, wantMode: "default"},
		{
			name: "automatic fit", selection: connectionPathPing, fit: true, batchRate: 10,
			ceiling: 13, limit: 2, fitted: 13, wantMode: "path_fit",
		},
		{
			name: "unlimited maximum", selection: connectionPathPing, fit: true, cap: -1, capSet: true,
			requestRate: 25, ceiling: 7, limit: 2, fitted: 7, wantMode: "path_fit",
		},
		{
			name: "bounded maximum", selection: connectionPathPingCapped, fit: true, cap: 4, capSet: true,
			batchRate: 10, requestRate: 25, ceiling: 4, limit: 2, fitted: 7, wantMode: "path_fit",
		},
		{
			name: "skipped fit", selection: connectionDefault, fit: true, cap: 4, capSet: true, batchRate: 10,
			ceiling: 1, limit: 1, wantMode: "path_fit",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tuning := transportTuningConfig{
				maxConns: test.ceiling, connectionFit: test.selection, fitSelected: test.fit,
				batchRate: test.batchRate, requestRateMax: test.requestRate,
				parallelFitMax: test.cap, parallelFitSet: test.capSet,
				pathFittedConns: test.fitted, pathFitCapped: test.selection == connectionPathPingCapped,
			}
			if test.batchRate > 0 {
				tuning.minBatchInterval = time.Duration(1e9 / test.batchRate)
			}
			if test.fitted > 0 {
				tuning.pathRTTSamples = []time.Duration{time.Second, time.Second, time.Second}
				tuning.pathRTT = time.Second
			}
			delivery, err := makeStructuredDelivery(resolveDeliveryPlan(tuning, 5, test.limit, false))
			if err != nil {
				t.Fatal(err)
			}
			if delivery.Selection.Mode != test.wantMode || delivery.ConnectionCeiling != test.ceiling ||
				delivery.ConnectionLimit != test.limit {
				t.Fatalf("delivery = %+v", delivery)
			}
			if test.wantMode == "path_fit" {
				wantMaximum := "automatic_review"
				if test.capSet && test.cap < 0 {
					wantMaximum = "unlimited"
				}
				if test.capSet && test.cap > 0 {
					wantMaximum = "bounded"
				}
				if delivery.Selection.Maximum == nil || delivery.Selection.Maximum.Mode != wantMaximum {
					t.Errorf("maximum = %+v, want %s", delivery.Selection.Maximum, wantMaximum)
				}
				wantState := "fitted"
				if test.fitted == 0 {
					wantState = "skipped_single_trial"
				}
				if delivery.Selection.State != wantState {
					t.Errorf("selection = %+v", delivery.Selection)
				}
			}
			encoded, err := json.Marshal(delivery)
			if err != nil {
				t.Fatal(err)
			}
			var decoded structuredDelivery
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			validateConformanceDelivery(t, decoded, 5)
			var output bytes.Buffer
			reporter := newJSONRaceReporter(&output)
			planned := 2
			if test.fit && test.fitted == 0 {
				planned = 1
			}
			if err := reporter.Run(RunRecord{
				RequestCount: 5, TrialBatchWidth: 5, TrialsRequested: planned, Trials: planned,
				Argv: []string{"race", "", "two words"}, DialAddress: "example.test:443",
				ArrangementPolicy: "none", Padding: resolvedPadding(nil, 5),
				Delivery:       resolveDeliveryPlan(tuning, 5, test.limit, false),
				ResponseLimits: defaultResponseLimits(),
			}); err != nil {
				t.Fatal(err)
			}
			for id := range 5 {
				if err := reporter.Request(RequestRecord{
					Reference:  runReference{ID: id, Label: fmt.Sprintf("Request %d", id)},
					RequestSet: "trial", URL: "https://example.test/",
				}); err != nil {
					t.Fatal(err)
				}
			}
			execution, err := projectRaceExecution(planned, test.limit, 5, 0,
				nil, executionBatches{}, executionBatches{})
			if err != nil {
				t.Fatal(err)
			}
			if err := reporter.Summary(SummaryRecord{Execution: execution, Completion: stoppedRun()}); err != nil {
				t.Fatal(err)
			}
			schema := compileCurrentStructuredSchema(t, raceSchema.filename)
			lines := jsonLines(t, output.Bytes())
			records := make([]conformanceRecord, len(lines))
			for i, line := range lines {
				var instance any
				if err := json.Unmarshal(line, &instance); err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(instance); err != nil {
					t.Fatalf("record %d: %v", i, err)
				}
				if err := json.Unmarshal(line, &records[i]); err != nil {
					t.Fatal(err)
				}
			}
			validateConformanceStream(t, records)
			if test.fit && test.fitted == 0 {
				return
			}
			output.Reset()
			requests := make([]RequestRecord, 5)
			for id := range requests {
				requests[id] = RequestRecord{
					Reference:  runReference{ID: id, Label: fmt.Sprintf("Request %d", id)},
					RequestSet: "measured", URL: "https://example.test/",
				}
			}
			measureReporter := newJSONMeasureReporter(&output, MeasureRunRecord{
				TrialsRequested: 10, TrialsPlanned: 10, Budget: measureBudget{unit: "cycles", requested: 2},
				MeasuredBatchWidth: 5, RequestCount: 5, ArrangementPolicy: "rotate",
				Argv: []string{"measure"}, DialAddress: "example.test:443",
				Delivery:       resolveDeliveryPlan(tuning, 5, test.limit, false),
				ResponseLimits: defaultResponseLimits(),
			}, requests)
			if err := measureReporter.Run(test.limit); err != nil {
				t.Fatal(err)
			}
			measured := newMeasurement(10, 10, 5, test.limit, tth2.ArrangeRotate)
			measured.finish(measured.summary, context.Canceled)
			if err := measureReporter.Summary(measured); err != nil {
				t.Fatal(err)
			}
			schema = compileCurrentStructuredSchema(t, measureSchema.filename)
			lines = jsonLines(t, output.Bytes())
			for i, line := range lines {
				var instance any
				if err := json.Unmarshal(line, &instance); err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(instance); err != nil {
					t.Fatalf("measure record %d: %v", i, err)
				}
			}
			validateStructuredRecords(t, lines)
			output.Reset()
			reportContext := analyseJSONContext{
				Requests: requests, Argv: []string{"analyse"}, DialAddress: "example.test:443",
				MeasuredBatchWidth: 5, Padding: resolvedPadding(nil, 5), ResponseLimits: defaultResponseLimits(),
				Delivery: resolveDeliveryPlan(tuning, 5, test.limit, false),
			}
			analyseStream, err := newAnalyseJSONStream(&output, analyseWorkload{
				Width: 5, Cycles: 2, Trials: 10, Operations: 50,
				DefaultCycles: analyseDefaultCycles(5), LowerSensitivity: true,
			}, test.limit, []string{"Request 0", "Request 1", "Request 2", "Request 3", "Request 4"},
				reportContext, raceCaptureConfig{})
			if err != nil {
				t.Fatal(err)
			}
			collector := newAnalyseCollector(5, 2)
			collector.completion = stoppedRun()
			analysis := analyseOffline(collector.finish(context.Canceled), analyseConfig{ConnectionLimit: test.limit})

			if err := analyseStream.Finish(analysis, reportContext); err != nil {
				t.Fatal(err)
			}
			if _, err := replayAnalyseJSONStream(bytes.NewReader(output.Bytes())); err != nil {
				t.Fatal(err)
			}
		})
	}
}
