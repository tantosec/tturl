package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/mod/semver"
)

var repositoryPattern = regexp.MustCompile(
	`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`,
)

var githubLoginPattern = regexp.MustCompile(
	`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38}[A-Za-z0-9])?(?:\[bot\])?$`,
)

type releaseNotePullRequest struct {
	Number   int       `json:"number"`
	Title    string    `json:"title"`
	HTMLURL  string    `json:"html_url"`
	MergedAt time.Time `json:"merged_at"`
	Base     struct {
		Ref string `json:"ref"`
	} `json:"base"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

func releaseNotesBase(current string, tags []string) (string, error) {
	if !strings.HasPrefix(current, "v") || validateVersion(current[1:]) != nil {
		return "", fmt.Errorf("release notes tag %q is not canonical", current)
	}
	currentPrerelease := semver.Prerelease(current)
	currentCore := strings.TrimSuffix(current, currentPrerelease)
	base := ""
	for _, tag := range tags {
		comparison := semver.Compare(tag, current)
		if comparison > 0 {
			return "", fmt.Errorf(
				"release notes tag %s does not advance %s", current, tag,
			)
		}
		if comparison == 0 {
			continue
		}
		if currentPrerelease == "" &&
			strings.TrimSuffix(tag, semver.Prerelease(tag)) == currentCore {
			continue
		}
		if base == "" || semver.Compare(tag, base) > 0 {
			base = tag
		}
	}
	return base, nil
}

func renderReleaseNotes(r io.Reader, w io.Writer, repository string) error {
	if !repositoryPattern.MatchString(repository) {
		return fmt.Errorf("invalid GitHub repository %q", repository)
	}
	decoder := json.NewDecoder(r)
	pulls := make(map[int]releaseNotePullRequest)
	for {
		var batch []releaseNotePullRequest
		err := decoder.Decode(&batch)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("decode associated pull requests: %w", err)
		}
		for _, pull := range batch {
			if err := validateReleaseNotePullRequest(pull, repository); err != nil {
				return err
			}
			if existing, ok := pulls[pull.Number]; ok && existing != pull {
				return fmt.Errorf(
					"pull request #%d has conflicting metadata", pull.Number,
				)
			}
			pulls[pull.Number] = pull
		}
	}
	ordered := make([]releaseNotePullRequest, 0, len(pulls))
	for _, pull := range pulls {
		ordered = append(ordered, pull)
	}
	slices.SortFunc(ordered, func(a, b releaseNotePullRequest) int {
		if order := a.MergedAt.Compare(b.MergedAt); order != 0 {
			return order
		}
		return a.Number - b.Number
	})

	var notes strings.Builder
	notes.WriteString("## Changes\n\n")
	if len(ordered) == 0 {
		notes.WriteString("No pull requests were merged for this release.\n")
	} else {
		for _, pull := range ordered {
			_, _ = fmt.Fprintf(
				&notes, "- [#%d: %s](%s) by @%s\n", pull.Number,
				escapeMarkdownLabel(pull.Title), pull.HTMLURL, pull.User.Login,
			)
		}
	}
	const maximumReleaseNotesSize = 64 << 10
	if notes.Len() > maximumReleaseNotesSize {
		return errors.New("release notes exceed 64 KiB")
	}
	_, err := io.WriteString(w, notes.String())
	return err
}

func validateReleaseNotePullRequest(
	pull releaseNotePullRequest,
	repository string,
) error {
	if pull.Number < 1 {
		return fmt.Errorf("invalid pull request number %d", pull.Number)
	}
	if pull.Title == "" || strings.TrimSpace(pull.Title) != pull.Title ||
		!utf8.ValidString(pull.Title) || strings.IndexFunc(pull.Title, unicode.IsControl) >= 0 {
		return fmt.Errorf("pull request #%d has an invalid title", pull.Number)
	}
	wantURL := fmt.Sprintf(
		"https://github.com/%s/pull/%d", repository, pull.Number,
	)
	if pull.HTMLURL != wantURL {
		return fmt.Errorf(
			"pull request #%d URL = %q, want %q",
			pull.Number, pull.HTMLURL, wantURL,
		)
	}
	if pull.MergedAt.IsZero() {
		return fmt.Errorf("pull request #%d is not merged", pull.Number)
	}
	if pull.Base.Ref != "main" {
		return fmt.Errorf(
			"pull request #%d targets %q, want main", pull.Number, pull.Base.Ref,
		)
	}
	if !githubLoginPattern.MatchString(pull.User.Login) {
		return fmt.Errorf(
			"pull request #%d has invalid author login %q",
			pull.Number, pull.User.Login,
		)
	}
	return nil
}

func escapeMarkdownLabel(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)
	return replacer.Replace(value)
}
