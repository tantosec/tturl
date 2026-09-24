// Package buildinfo resolves a command-neutral identity from packager-provided
// facts and Go build metadata.
package buildinfo

import (
	"regexp"
	"runtime/debug"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const developmentVersion = "devel"

var releaseCandidatePattern = regexp.MustCompile(`^-rc\.[1-9][0-9]*$`)

// These private strings are set by source packagers with -ldflags -X. An empty
// value means that the packager does not know or does not assert that fact.
// injectedTree accepts only "clean" or "dirty"; every other value is unknown.
var (
	injectedRelease  string
	injectedRevision string
	injectedTree     string
)

// TreeState describes whether the source used for a build was modified.
type TreeState uint8

const (
	// TreeUnknown means the build carries no trustworthy modification state.
	TreeUnknown TreeState = iota
	// TreeClean means the build source was known to be unmodified.
	TreeClean
	// TreeDirty means the build source was known to be modified.
	TreeDirty
)

// Info is an immutable resolved build identity. Its zero value is a development
// build with unknown provenance.
type Info struct {
	release  string
	revision string
	tree     TreeState
}

type injectedFacts struct {
	release  string
	revision string
	tree     string
}

// Current resolves the running binary's identity. Packager-injected facts take
// precedence over Go build metadata. A dirty source state suppresses any
// release identity.
func Current() Info {
	bi, _ := debug.ReadBuildInfo()
	return resolve(injectedFacts{
		release:  injectedRelease,
		revision: injectedRevision,
		tree:     injectedTree,
	}, bi)
}

// Release returns the canonical release tag, or "" for a development build.
func (i Info) Release() string { return i.release }

// Revision returns the full available source revision.
func (i Info) Revision() string { return i.revision }

// TreeState returns the source modification state.
func (i Info) TreeState() TreeState { return i.tree }

// Version returns the low-cardinality release or development token suitable
// for a User-Agent product version.
func (i Info) Version() string {
	if i.release != "" {
		return i.release
	}
	return developmentVersion
}

// Diagnostic returns a one-line version string prefixed by name. It includes a
// shortened revision when known and marks a dirty source tree.
func (i Info) Diagnostic(name string) string {
	description := i.Version()
	if i.revision != "" {
		revision := i.revision
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if i.tree == TreeDirty {
			revision += "-dirty"
		}
		description += " (" + revision + ")"
	}
	return name + " " + description
}

// Version resolves the running binary and returns its wire version. Prefer
// retaining [Current] when more than one projection is needed.
func Version() string { return Current().Version() }

// String resolves the running binary and returns its diagnostic display.
// Prefer retaining [Current] when more than one projection is needed.
func String(name string) string { return Current().Diagnostic(name) }

func resolve(injected injectedFacts, bi *debug.BuildInfo) Info {
	moduleVersion, settings := buildMetadata(bi)
	vcsRevision, vcsTree := vcsFacts(settings)

	revision := injected.revision
	if revision == "" {
		revision = vcsRevision
	}
	if revision == "" {
		revision = pseudoRevision(moduleVersion)
	}

	tree := combineTree(parseInjectedTree(injected.tree), vcsTree)

	release := ""
	if injected.release != "" {
		if isRelease(injected.release) {
			release = injected.release
		}
	} else if isRelease(moduleVersion) {
		release = moduleVersion
	}
	if tree == TreeDirty {
		release = ""
	}

	return Info{release: release, revision: revision, tree: tree}
}

func buildMetadata(bi *debug.BuildInfo) (string, []debug.BuildSetting) {
	if bi == nil {
		return "", nil
	}
	return bi.Main.Version, bi.Settings
}

func vcsFacts(settings []debug.BuildSetting) (string, TreeState) {
	revision := ""
	tree := TreeUnknown
	for _, setting := range settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			switch setting.Value {
			case "true":
				tree = TreeDirty
			case "false":
				tree = TreeClean
			}
		}
	}
	return revision, tree
}

func parseInjectedTree(value string) TreeState {
	switch value {
	case "clean":
		return TreeClean
	case "dirty":
		return TreeDirty
	default:
		return TreeUnknown
	}
}

func combineTree(injected, vcs TreeState) TreeState {
	if injected == TreeDirty || vcs == TreeDirty {
		return TreeDirty
	}
	if injected == TreeClean {
		return TreeClean
	}
	return vcs
}

func pseudoRevision(version string) string {
	canonical := semver.Canonical(version)
	if !module.IsPseudoVersion(canonical) {
		return ""
	}
	revision, err := module.PseudoVersionRev(canonical)
	if err != nil {
		return ""
	}
	return revision
}

func isRelease(version string) bool {
	if version == "" || semver.Canonical(version) != version ||
		semver.Build(version) != "" ||
		module.IsPseudoVersion(version) {
		return false
	}
	prerelease := semver.Prerelease(version)
	return prerelease == "" || releaseCandidatePattern.MatchString(prerelease)
}
