// Command ciimpact classifies repository diffs for conditional CI gates.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

const (
	maxChangedPaths = 100_000
	maxPathBytes    = 16 << 10
	usage           = `usage:
  ciimpact ranking-qualification
  ciimpact release-intent`
)

type impactDecision struct {
	Required bool
	Reason   string
}

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ciimpact: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, input io.Reader, output io.Writer) error {
	if len(args) != 1 {
		return errors.New(usage)
	}
	switch args[0] {
	case "ranking-qualification":
		decision := classifyRankingQualificationInput(input)
		_, err := fmt.Fprintf(output, "required=%t\nreason=%s\n",
			decision.Required, decision.Reason)
		return err
	case "release-intent":
		changed, err := classifyReleaseIntentInput(input)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(output, "changed=%t\n", changed)
		return err
	default:
		return errors.New(usage)
	}
}

func classifyReleaseIntentInput(input io.Reader) (bool, error) {
	paths, err := readChangedPaths(input)
	if err != nil {
		return false, fmt.Errorf("changed-path input is invalid: %w", err)
	}
	var versionChanged, citationChanged bool
	for _, changed := range paths {
		normalised, ok := normaliseRepositoryPath(changed)
		if !ok {
			return false, fmt.Errorf("changed path is invalid: %q", changed)
		}
		switch normalised {
		case "release/version.txt":
			versionChanged = true
		case "CITATION.cff":
			citationChanged = true
		}
	}
	return versionChanged || citationChanged, nil
}

func classifyRankingQualificationInput(input io.Reader) impactDecision {
	paths, err := readChangedPaths(input)
	if err != nil {
		return impactDecision{Required: true, Reason: "changed-path input is invalid"}
	}
	if len(paths) == 0 {
		return impactDecision{Required: true, Reason: "changed-path input is empty"}
	}
	for _, changed := range paths {
		normalised, ok := normaliseRepositoryPath(changed)
		if !ok {
			return impactDecision{Required: true, Reason: "changed path is invalid"}
		}
		if rankingQualificationPath(normalised) {
			return impactDecision{
				Required: true, Reason: "affected path: " + normalised,
			}
		}
	}
	return impactDecision{
		Required: false,
		Reason:   fmt.Sprintf("no affected paths among %d changed files", len(paths)),
	}
}

func readChangedPaths(input io.Reader) ([]string, error) {
	reader := bufio.NewReaderSize(input, maxPathBytes)
	var paths []string
	for {
		entry, err := reader.ReadSlice(0)
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("changed path exceeds %d bytes", maxPathBytes)
		}
		if errors.Is(err, io.EOF) {
			if len(entry) != 0 {
				return nil, fmt.Errorf("changed paths are not NUL terminated")
			}
			return paths, nil
		}
		if err != nil {
			return nil, err
		}
		paths = append(paths, string(entry[:len(entry)-1]))
		if len(paths) > maxChangedPaths {
			return nil, fmt.Errorf("more than %d changed paths", maxChangedPaths)
		}
	}
}

func normaliseRepositoryPath(name string) (string, bool) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\r\n\x00") {
		return "", false
	}
	normalised := path.Clean(name)
	if normalised == "." || normalised == ".." || strings.HasPrefix(normalised, "../") {
		return "", false
	}
	return normalised, true
}

func rankingQualificationPath(name string) bool {
	for _, prefix := range []string{"internal/ranking/", "stats/", "tools/ciimpact/"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "go.mod", "go.sum", "go.work", "go.work.sum", "Makefile", "CITATION.cff",
		"release/version.txt", ".github/workflows/ci.yaml",
		".github/workflows/ranking-qualification.yaml",
		".github/workflows/release.yaml":
		return true
	default:
		return false
	}
}
