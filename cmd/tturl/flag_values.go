package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tantosec/tturl/internal/curlblocks"
)

const (
	unlimitedFlagValue             = "unlimited"
	completionValuesFlagAnnotation = "tturl-completion-values"
)

func isUnlimitedFlagValue(value string) bool {
	return strings.EqualFold(value, unlimitedFlagValue)
}

// parsePositiveIntOrUnlimited parses the lexical form shared by integer work
// limits. The negative sentinel is private to resolved command configuration;
// command-line input must use the explicit unlimited spelling.
func parsePositiveIntOrUnlimited(value string) (int, error) {
	if isUnlimitedFlagValue(value) {
		return -1, nil
	}
	for _, digit := range []byte(value) {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf(
				"must be a positive decimal integer or %s, got %q",
				unlimitedFlagValue, curlblocks.DisplayText(value))
		}
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf(
			"must be a positive decimal integer or %s, got %q",
			unlimitedFlagValue, curlblocks.DisplayText(value))
	}
	return parsed, nil
}

// parseDurationOrUnlimited parses the lexical value shared by duration limits.
// Callers retain ownership of zero and negative duration semantics.
func parseDurationOrUnlimited(value string) (time.Duration, bool, error) {
	if isUnlimitedFlagValue(value) {
		return 0, true, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, false, fmt.Errorf(
			"must be a Go duration or %s", unlimitedFlagValue)
	}
	return duration, false, nil
}
