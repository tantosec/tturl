package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/tantosec/tturl/internal/curlblocks"
)

type diagnosticNature string

const (
	diagnosticNotice  diagnosticNature = "notice"
	diagnosticWarning diagnosticNature = "warning"
	diagnosticError   diagnosticNature = "error"
)

func diagnosticPrefix(nature diagnosticNature) string {
	return toolName + ": " + string(nature) + ": "
}

// writeDiagnostic writes one attributable, classified diagnostic using the
// shared terminal width and hanging indentation. It accepts an existing
// reportWriter when callers need its blank-line tracking; otherwise it adds a
// local one so write failures remain observable.
func writeDiagnostic(w io.Writer, nature diagnosticNature, message string) error {
	report, ok := w.(*reportWriter)
	if !ok {
		report = newReportWriter(w)
	}
	writeWrappedASCII(
		report, diagnosticPrefix(nature), curlblocks.DisplayText(message))
	return report.Err()
}

// writeDiagnosticDocument keeps a following usage or help document outside
// diagnostic grammar while preserving one blank line between them.
func writeDiagnosticDocument(
	w io.Writer,
	nature diagnosticNature,
	message, document string,
) error {
	report := newReportWriter(w)
	if err := writeDiagnostic(report, nature, message); err != nil {
		return err
	}
	report.ensureBlankLine()
	emit(report, document)
	return report.Err()
}

// diagnosticLogWriter adapts line-oriented subsystem logs to the same
// diagnostic grammar as command-owned messages.
type diagnosticLogWriter struct {
	w       io.Writer
	nature  diagnosticNature
	context string
}

func (w diagnosticLogWriter) Write(p []byte) (int, error) {
	message := strings.TrimSuffix(string(p), "\n")
	for line := range strings.SplitSeq(message, "\n") {
		if err := writeDiagnostic(w.w, w.nature, w.context+line); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// reportPreflightWarnings writes decision-relevant cautions before acquisition.
// Callers reuse the request-echo writer for JSON so the echo and warnings have
// one deliberate blank-line boundary.
func reportPreflightWarnings(w *reportWriter, warnings []string) error {
	if len(warnings) == 0 {
		return w.Err()
	}
	w.ensureBlankLine()
	for _, warning := range warnings {
		if err := writeDiagnostic(w, diagnosticWarning, warning); err != nil {
			return err
		}
	}
	return w.Err()
}

func deliveryPreflightWarnings(plan deliveryPlan) []string {
	if !plan.pathFitCapped {
		return nil
	}
	return []string{fmt.Sprintf(
		"path fit requested %d connections; --connections-fit-max %d caps the "+
			"run at %d and may make the requested batch cadence unsustainable.",
		plan.pathFittedConnections, plan.parallelFitMax, plan.parallelFitMax)}
}

func measurePreflightWarnings(plan measurePlan) []string {
	var warnings []string
	for _, code := range measurePlanWarnings(plan) {
		switch code {
		case "fixed_positions":
			warnings = append(warnings,
				"fixed request positions confound request identity with outbound position.")
		case "random_balance_in_expectation":
			warnings = append(warnings,
				"random position assignment balances only in expectation.")
		case "one_rotation_cycle_planned":
			warnings = append(warnings,
				"only 1 complete rotation cycle is planned; the design has no "+
					"replication across rotation blocks.")
		}
	}
	return append(warnings, deliveryPreflightWarnings(plan.delivery)...)
}

func analysePreflightWarnings(
	workload analyseWorkload,
	connectionLimit int,
	delivery deliveryPlan,
	structured bool,
) []string {
	var warnings []string
	if workload.LowerSensitivity {
		warnings = append(warnings, fmt.Sprintf(
			"%s is below the %d-cycle modelled default; this run has lower "+
				"sensitivity than the validated planning target.",
			countedNoun(workload.Cycles, "cycle"), workload.DefaultCycles))
	}
	if !structured {
		return warnings
	}
	if connectionLimit > 1 {
		warnings = append(warnings, fmt.Sprintf(
			"up to %d concurrent connections can change the target signal; "+
				"use --batch-rate-max to cap throughput.", connectionLimit))
	}
	return append(warnings, deliveryPreflightWarnings(delivery)...)
}

func jsonPreflightWarnings(delivery deliveryPlan) []string {
	return deliveryPreflightWarnings(delivery)
}
