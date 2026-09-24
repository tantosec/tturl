// Command releasemaint validates and updates reviewed release intent and
// verifies non-publishing release artefacts. Shell scripts own orchestration;
// this command owns exact version and artefact semantics.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

const (
	versionPath  = "release/version.txt"
	citationPath = "CITATION.cff"
	usage        = `usage:
  releasemaint prepare VERSION
  releasemaint set VERSION
  releasemaint check-metadata
  releasemaint check VERSION
  releasemaint notes-base TAG
  releasemaint render-notes REPOSITORY
  releasemaint verify-dist VERSION REVISION DIRECTORY`
)

var releaseCandidatePattern = regexp.MustCompile(`^-rc\.[1-9][0-9]*$`)

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "releasemaint: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "prepare":
		if len(args) != 2 {
			return errors.New(usage)
		}
		tags, err := readTags(stdin)
		if err != nil {
			return err
		}
		return checkPreparation(args[1], tags)
	case "set":
		if len(args) != 2 {
			return errors.New(usage)
		}
		return setVersion(stdout, args[1])
	case "check-metadata":
		if len(args) != 1 {
			return errors.New(usage)
		}
		version, err := currentVersion()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "%s and %s agree on %s\n",
			versionPath, citationPath, version)
		return err
	case "check":
		if len(args) != 2 {
			return errors.New(usage)
		}
		tags, err := readTags(stdin)
		if err != nil {
			return err
		}
		return checkRelease(args[1], tags)
	case "notes-base":
		if len(args) != 2 {
			return errors.New(usage)
		}
		tags, err := readTags(stdin)
		if err != nil {
			return err
		}
		base, err := releaseNotesBase(args[1], tags)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, base)
		return err
	case "render-notes":
		if len(args) != 2 {
			return errors.New(usage)
		}
		return renderReleaseNotes(stdin, stdout, args[1])
	case "verify-dist":
		if len(args) != 4 {
			return errors.New(usage)
		}
		return verifyDistribution(args[1], args[2], args[3])
	default:
		return fmt.Errorf("unknown command %q\n%s", args[0], usage)
	}
}

func validateVersion(version string) error {
	canonical := "v" + version
	if semver.Canonical(canonical) != canonical || semver.Build(canonical) != "" {
		return fmt.Errorf(
			"version %q is not a canonical stable or rc.N version",
			version,
		)
	}
	prerelease := semver.Prerelease(canonical)
	if prerelease != "" && !releaseCandidatePattern.MatchString(prerelease) {
		return fmt.Errorf(
			"version %q is not a canonical stable or rc.N version",
			version,
		)
	}
	return nil
}

func readTags(r io.Reader) ([]string, error) {
	var tags []string
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		tag := strings.TrimSpace(scanner.Text())
		if tag == "" {
			continue
		}
		if !strings.HasPrefix(tag, "v") || validateVersion(tag[1:]) != nil {
			return nil, fmt.Errorf("existing release tag %q is not canonical", tag)
		}
		tags = append(tags, tag)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read release tags: %w", err)
	}
	return tags, nil
}

func checkPreparation(version string, tags []string) error {
	if err := validateVersion(version); err != nil {
		return err
	}
	current, err := currentVersion()
	if err != nil {
		return err
	}
	if semver.Compare("v"+version, "v"+current) <= 0 {
		return fmt.Errorf(
			"version %s must be newer than reviewed version %s",
			version,
			current,
		)
	}
	return checkAgainstTags(version, tags, false)
}

func checkRelease(version string, tags []string) error {
	if err := validateVersion(version); err != nil {
		return err
	}
	current, err := currentVersion()
	if err != nil {
		return err
	}
	if version != current {
		return fmt.Errorf(
			"version %s does not match reviewed version %s",
			version,
			current,
		)
	}
	return checkAgainstTags(version, tags, true)
}

func checkAgainstTags(version string, tags []string, allowEqual bool) error {
	candidate := "v" + version
	for _, tag := range tags {
		comparison := semver.Compare(candidate, tag)
		if comparison < 0 || comparison == 0 && !allowEqual {
			return fmt.Errorf("version %s does not advance existing tag %s", version, tag)
		}
	}
	return nil
}

func currentVersion() (string, error) {
	version, err := reviewedVersion()
	if err != nil {
		return "", err
	}
	citation, err := readCitationVersion()
	if err != nil {
		return "", err
	}
	if citation.version != version {
		return "", fmt.Errorf(
			"%s version %s does not match reviewed version %s",
			citationPath,
			citation.version,
			version,
		)
	}
	return version, nil
}

func reviewedVersion() (string, error) {
	contents, err := os.ReadFile(versionPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", versionPath, err)
	}
	version := strings.TrimSpace(string(contents))
	if string(contents) != version+"\n" {
		return "", fmt.Errorf(
			"%s must contain one version followed by a newline",
			versionPath,
		)
	}
	if err := validateVersion(version); err != nil {
		return "", fmt.Errorf("reviewed version: %w", err)
	}
	return version, nil
}

type citationVersion struct {
	contents   []byte
	valueStart int
	valueEnd   int
	version    string
}

func readCitationVersion() (citationVersion, error) {
	contents, err := os.ReadFile(citationPath)
	if err != nil {
		return citationVersion{}, fmt.Errorf("read %s: %w", citationPath, err)
	}
	return parseCitationVersion(contents)
}

func parseCitationVersion(contents []byte) (citationVersion, error) {
	const prefix = "version: "
	found := citationVersion{contents: contents}
	matches := 0
	for start := 0; start <= len(contents); {
		end := start
		for end < len(contents) && contents[end] != '\n' {
			end++
		}
		line := contents[start:end]
		if strings.HasPrefix(string(line), prefix) {
			matches++
			found.valueStart = start + len(prefix)
			found.valueEnd = end
			found.version = string(contents[found.valueStart:found.valueEnd])
		}
		if end == len(contents) {
			break
		}
		start = end + 1
	}
	if matches != 1 {
		return citationVersion{}, fmt.Errorf(
			"%s must contain exactly one canonical version line", citationPath,
		)
	}
	if err := validateVersion(found.version); err != nil {
		return citationVersion{}, fmt.Errorf("%s: %w", citationPath, err)
	}
	return found, nil
}

func setVersion(stdout io.Writer, version string) error {
	if err := validateVersion(version); err != nil {
		return err
	}
	current, err := currentVersion()
	if err != nil {
		return err
	}
	if current == version {
		_, err = fmt.Fprintf(stdout, "%s and %s are current\n", versionPath, citationPath)
		return err
	}
	citation, err := readCitationVersion()
	if err != nil {
		return err
	}
	updatedCitation := make([]byte, 0, len(citation.contents)-len(citation.version)+len(version))
	updatedCitation = append(updatedCitation, citation.contents[:citation.valueStart]...)
	updatedCitation = append(updatedCitation, version...)
	updatedCitation = append(updatedCitation, citation.contents[citation.valueEnd:]...)
	if err := replaceFile(versionPath, []byte(version+"\n")); err != nil {
		return err
	}
	if err := replaceFile(citationPath, updatedCitation); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "updated %s and %s\n", versionPath, citationPath)
	return err
}

func replaceFile(name string, contents []byte) error {
	info, err := os.Stat(name)
	if err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}
	directory := filepath.Dir(name)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(name)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", name, err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary file for %s: %w", name, err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary file for %s: %w", name, err)
	}
	if err := temporary.Chmod(info.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set temporary mode for %s: %w", name, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary file for %s: %w", name, err)
	}
	if err := os.Rename(temporaryName, name); err != nil {
		return fmt.Errorf("replace %s: %w", name, err)
	}
	return nil
}
