package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tantosec/tturl/tth2"
)

func TestWriteDiagnosticHouseStyle(t *testing.T) {
	t.Parallel()
	for _, nature := range []diagnosticNature{
		diagnosticNotice, diagnosticWarning, diagnosticError,
	} {
		t.Run(string(nature), func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			message := "A classified diagnostic with enough explanatory text " +
				"to exercise its hanging continuation indentation."
			if err := writeDiagnostic(&output, nature, message); err != nil {
				t.Fatal(err)
			}
			got := output.String()
			prefix := diagnosticPrefix(nature)
			lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
			if len(lines) != 2 || !strings.HasPrefix(lines[0], prefix) ||
				!strings.HasPrefix(lines[1], strings.Repeat(" ", len(prefix))) {
				t.Errorf("diagnostic lacks prefix or hanging indent:\n%s", got)
			}
			assertTextLinesAtMost(t, got, textWidth)
		})
	}

	want := errors.New("write failed")
	if err := writeDiagnostic(
		failingWriter{err: want}, diagnosticError, "message",
	); !errors.Is(err, want) {
		t.Errorf("writeDiagnostic error = %v, want %v", err, want)
	}
}

func TestReportPreflightWarnings(t *testing.T) {
	var output bytes.Buffer
	w := newReportWriter(&output)
	emitln(w, "Target: https://example.test")
	if err := reportPreflightWarnings(w, []string{
		"a decision-relevant warning long enough to require a hanging continuation " +
			"aligned beneath its diagnostic text",
	}); err != nil {
		t.Fatal(err)
	}
	got := output.String()
	prefix := diagnosticPrefix(diagnosticWarning)
	if !strings.Contains(got, "example.test\n\n"+prefix) {
		t.Errorf("warning is not separated from request echo:\n%s", got)
	}
	assertTextLinesAtMost(t, got, textWidth)
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) < 4 || !strings.HasPrefix(
		lines[3], strings.Repeat(" ", len(prefix)),
	) {
		t.Errorf("warning lacks hanging indentation:\n%s", got)
	}
}

func TestDeliveryPreflightWarnings(t *testing.T) {
	if got := deliveryPreflightWarnings(deliveryPlan{}); got != nil {
		t.Errorf("uncapped delivery warnings = %q, want none", got)
	}
	got := deliveryPreflightWarnings(deliveryPlan{
		pathFitCapped: true, pathFittedConnections: 12, parallelFitMax: 4,
	})
	want := []string{
		"path fit requested 12 connections; --connections-fit-max 4 caps the run " +
			"at 4 and may make the requested batch cadence unsustainable.",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("capped delivery warnings = %q, want %q", got, want)
	}
}

func TestMeasurePreflightWarnings(t *testing.T) {
	tests := []struct {
		name string
		plan measurePlan
		want string
	}{
		{
			name: "fixed positions", plan: measurePlan{
				arrangement: tth2.ArrangeNone,
			},
			want: "fixed request positions confound request identity",
		},
		{
			name: "random arrangement", plan: measurePlan{
				arrangement: tth2.ArrangeRandom,
			},
			want: "random position assignment balances only in expectation",
		},
		{
			name: "one rotation", plan: measurePlan{
				arrangement: tth2.ArrangeRotate, width: 2, planned: 2,
			},
			want: "only 1 complete rotation cycle is planned",
		},
		{
			name: "replicated rotation", plan: measurePlan{
				arrangement: tth2.ArrangeRotate, width: 2, planned: 4,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := strings.Join(measurePreflightWarnings(test.plan), "\n")
			if test.want == "" && got != "" {
				t.Errorf("warnings = %q, want none", got)
			}
			if test.want != "" && !strings.Contains(got, test.want) {
				t.Errorf("warnings = %q, want %q", got, test.want)
			}
		})
	}
}

func TestAnalysePreflightWarnings(t *testing.T) {
	workload := analyseWorkload{
		Cycles: 6, DefaultCycles: 150, LowerSensitivity: true,
	}
	delivery := deliveryPlan{
		pathFitCapped: true, pathFittedConnections: 12, parallelFitMax: 4,
	}
	textWarnings := analysePreflightWarnings(workload, 4, delivery, false)
	if len(textWarnings) != 1 ||
		!strings.Contains(textWarnings[0], "6 cycles is below the 150-cycle") {
		t.Errorf("text warnings = %q, want sensitivity only", textWarnings)
	}
	jsonWarnings := analysePreflightWarnings(workload, 4, delivery, true)
	if got := strings.Join(jsonWarnings, "\n"); len(jsonWarnings) != 3 ||
		!strings.Contains(got, "up to 4 concurrent connections") ||
		!strings.Contains(got, "path fit requested 12 connections") {
		t.Errorf("JSON warnings = %q, want sensitivity, load, and path fit", jsonWarnings)
	}
	workload.LowerSensitivity = false
	if got := analysePreflightWarnings(workload, 1, deliveryPlan{}, true); got != nil {
		t.Errorf("default workload warnings = %q, want none", got)
	}
}
