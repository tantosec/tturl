package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	const (
		fullRevision = "0123456789abcdef0123456789abcdef01234567"
		pseudo       = "v0.0.0-20260706123456-abcdef123456"
	)
	tests := []struct {
		name     string
		injected injectedFacts
		bi       *debug.BuildInfo
		want     Info
	}{
		{
			name: "injected clean release",
			injected: injectedFacts{
				release: "v1.2.3", revision: fullRevision, tree: "clean",
			},
			want: Info{release: "v1.2.3", revision: fullRevision, tree: TreeClean},
		},
		{
			name: "release candidate",
			injected: injectedFacts{
				release: "v1.2.3-rc.10", revision: fullRevision, tree: "clean",
			},
			want: Info{
				release: "v1.2.3-rc.10", revision: fullRevision, tree: TreeClean,
			},
		},
		{
			name: "injected dirty overrides release",
			injected: injectedFacts{
				release: "v1.2.3", revision: fullRevision, tree: "dirty",
			},
			want: Info{revision: fullRevision, tree: TreeDirty},
		},
		{
			name:     "VCS dirty overrides injected clean release",
			injected: injectedFacts{release: "v1.2.3", tree: "clean"},
			bi: buildInfo(
				"v1.2.3",
				setting("vcs.revision", fullRevision),
				setting("vcs.modified", "true"),
			),
			want: Info{revision: fullRevision, tree: TreeDirty},
		},
		{
			name: "tagged module without VCS metadata",
			bi:   buildInfo("v2.3.4"),
			want: Info{release: "v2.3.4", tree: TreeUnknown},
		},
		{
			name: "clean tagged checkout",
			bi: buildInfo(
				"v2.3.4",
				setting("vcs.revision", fullRevision),
				setting("vcs.modified", "false"),
			),
			want: Info{release: "v2.3.4", revision: fullRevision, tree: TreeClean},
		},
		{
			name: "dirty tagged checkout",
			bi: buildInfo(
				"v2.3.4+dirty",
				setting("vcs.revision", fullRevision),
				setting("vcs.modified", "true"),
			),
			want: Info{revision: fullRevision, tree: TreeDirty},
		},
		{
			name: "clean untagged checkout",
			bi: buildInfo(
				pseudo,
				setting("vcs.revision", fullRevision),
				setting("vcs.modified", "false"),
			),
			want: Info{revision: fullRevision, tree: TreeClean},
		},
		{
			name: "pseudo-version supplies revision",
			bi:   buildInfo(pseudo),
			want: Info{revision: "abcdef123456", tree: TreeUnknown},
		},
		{
			name: "dirty pseudo-version supplies revision",
			bi:   buildInfo(pseudo + "+dirty"),
			want: Info{revision: "abcdef123456", tree: TreeUnknown},
		},
		{
			name:     "snapshot injection",
			injected: injectedFacts{revision: fullRevision, tree: "clean"},
			want:     Info{revision: fullRevision, tree: TreeClean},
		},
		{
			name:     "malformed injection blocks module release fallback",
			injected: injectedFacts{release: "v1.2.3-beta.1"},
			bi:       buildInfo("v2.3.4"),
			want:     Info{tree: TreeUnknown},
		},
		{
			name:     "pseudo injection blocks module release fallback",
			injected: injectedFacts{release: pseudo},
			bi:       buildInfo("v2.3.4"),
			want:     Info{tree: TreeUnknown},
		},
		{
			name: "other module prerelease is development",
			bi:   buildInfo("v1.2.3-beta.1"),
			want: Info{tree: TreeUnknown},
		},
		{
			name: "unknown injected tree does not claim clean",
			injected: injectedFacts{
				release: "v1.2.3", revision: fullRevision, tree: "invalid",
			},
			want: Info{release: "v1.2.3", revision: fullRevision, tree: TreeUnknown},
		},
		{name: "missing BuildInfo", want: Info{tree: TreeUnknown}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := resolve(tt.injected, tt.bi); got != tt.want {
				t.Errorf("resolve() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestInfoProjections(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		info       Info
		version    string
		diagnostic string
	}{
		{
			name:       "release with revision",
			info:       Info{release: "v1.2.3", revision: "0123456789abcdef"},
			version:    "v1.2.3",
			diagnostic: "client v1.2.3 (0123456789ab)",
		},
		{
			name:       "release without revision",
			info:       Info{release: "v1.2.3"},
			version:    "v1.2.3",
			diagnostic: "client v1.2.3",
		},
		{
			name:       "clean development",
			info:       Info{revision: "0123456789abcdef", tree: TreeClean},
			version:    "devel",
			diagnostic: "client devel (0123456789ab)",
		},
		{
			name:       "dirty development",
			info:       Info{revision: "abc", tree: TreeDirty},
			version:    "devel",
			diagnostic: "client devel (abc-dirty)",
		},
		{
			name:       "unknown provenance",
			info:       Info{},
			version:    "devel",
			diagnostic: "client devel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.info.Version(); got != tt.version {
				t.Errorf("Version() = %q, want %q", got, tt.version)
			}
			if got := tt.info.Diagnostic("client"); got != tt.diagnostic {
				t.Errorf("Diagnostic() = %q, want %q", got, tt.diagnostic)
			}
		})
	}
}

func TestIsRelease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    bool
	}{
		{"v1.2.3", true},
		{"v1.2.3-rc.1", true},
		{"v1.2.3-rc.10", true},
		{"v1.2.3-rc.0", false},
		{"v1.2.3-rc1", false},
		{"v1.2.3-beta.1", false},
		{"v1.2.3+build.1", false},
		{"v0.0.0-20260706123456-abcdef123456", false},
		{"(devel)", false},
		{"devel", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			if got := isRelease(tt.version); got != tt.want {
				t.Errorf("isRelease(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func buildInfo(version string, settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{
		Main:     debug.Module{Version: version},
		Settings: settings,
	}
}

func setting(key, value string) debug.BuildSetting {
	return debug.BuildSetting{Key: key, Value: value}
}
