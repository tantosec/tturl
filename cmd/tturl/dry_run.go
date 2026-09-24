package main

import (
	"fmt"
)

func validateDryRunReport(dryRun bool, report reportConfig) error {
	if dryRun && report.format != reportText {
		return fmt.Errorf("--dry-run requires --report text")
	}
	return nil
}

func reportDryRun(
	out commandOutput,
	reportCfg reportConfig,
	command string,
	requests *materialised,
	refs runReferences,
	verbose bool,
	experiment func(*reportWriter) error,
) (err error) {
	destination, err := openReportDestination(out, reportCfg.output)
	if err != nil {
		return err
	}
	defer func() { err = destination.finish(err) }()

	report := newReportWriter(destination.writer)
	writeBanner(report, toolName+" "+command)
	writeWrappedASCII(report, "Preview: ",
		"network-free dry run; unresolved delivery facts require execution; not an execution record.")
	requests.summarise(report, refs, verbose)
	if err := report.Err(); err != nil {
		if destination.isClosedPipe(err) {
			return nil
		}
		return err
	}
	if err := experiment(report); err != nil {
		return err
	}
	return report.Err()
}
