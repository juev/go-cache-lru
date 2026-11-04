package update

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for in, want := range map[string]semver{
		"1.2.3": {1, 2, 3}, "v1.2.3": {1, 2, 3}, "v0.10.0": {0, 10, 0}, "v12.0.1": {12, 0, 1},
	} {
		got, ok := parseVersion(in)
		if !ok || got != want {
			t.Errorf("parseVersion(%q) = %v, %v; want %v", in, got, ok, want)
		}
		if s := got.String(); s != "v"+strings.TrimPrefix(in, "v") {
			t.Errorf("%v.String() = %q", want, s)
		}
	}
	for _, in := range []string{"", "v", "1.2", "v1.2.3.4", "v1.2.3-rc.1", "v1.2.3+dirty", "01.2.3", "v1.02.3",
		"v1.2.4-0.20260101000000-abcdefabcdef", "(devel)", "v1.2.3 abcdef123456", "vv1.2.3", "v-1.2.3", "v1.2.3\n"} {
		if v, ok := parseVersion(in); ok {
			t.Errorf("parseVersion(%q) = %v; want rejection", in, v)
		}
	}
	if s := (semver{1, 20, 3}).String(); s != "v1.20.3" {
		t.Errorf("String() = %q, want v1.20.3", s)
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b semver
		want bool
	}{
		{semver{1, 0, 1}, semver{1, 0, 0}, true},
		{semver{1, 1, 0}, semver{1, 0, 9}, true},
		{semver{2, 0, 0}, semver{1, 9, 9}, true},
		{semver{1, 10, 0}, semver{1, 9, 0}, true},
		{semver{1, 0, 0}, semver{1, 0, 0}, false},
		{semver{1, 0, 0}, semver{1, 0, 1}, false}, // never a downgrade
		{semver{0, 9, 9}, semver{1, 0, 0}, false},
	} {
		if got := tc.a.newer(tc.b); got != tc.want {
			t.Errorf("%v.newer(%v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func buildInfo(version string, settings ...string) *debug.BuildInfo {
	bi := &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: version}}
	for i := 0; i+1 < len(settings); i += 2 {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
	}
	return bi
}

func TestCurrentVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ldflags string
		bi      *debug.BuildInfo
		want    semver
	}{
		// GoReleaser sets the version without "v"; its build info is a
		// local build of the tag.
		{"goreleaser", "1.4.2", buildInfo("v1.4.2", "vcs.revision", "abcdefabcdefabcdef", "vcs.modified", "false"), semver{1, 4, 2}},
		{"goreleaser without build info", "1.4.2", nil, semver{1, 4, 2}},
		// go install module@v1.4.2 builds from the module cache: no VCS
		// settings.
		{"go install @tag", "", buildInfo("v1.4.2", "-trimpath", "true"), semver{1, 4, 2}},
		{"clean local build at a tag", "", buildInfo("v1.4.2", "vcs.revision", "abc", "vcs.modified", "false"), semver{1, 4, 2}},
	} {
		got, err := currentVersion(tc.ldflags, tc.bi)
		if err != nil || got != tc.want {
			t.Errorf("%s: currentVersion = %v, %v; want %v", tc.name, got, err, tc.want)
		}
	}
	for _, tc := range []struct {
		name    string
		ldflags string
		bi      *debug.BuildInfo
	}{
		{"no build info", "", nil},
		{"devel", "", buildInfo("(devel)")},
		{"empty", "", buildInfo("")},
		{"pseudo-version", "", buildInfo("v1.4.3-0.20261101120000-abcdefabcdef")},
		{"dirty", "", buildInfo("v1.4.2+dirty", "vcs.modified", "true")},
		{"modified tree", "", buildInfo("v1.4.2", "vcs.modified", "true")},
		{"pre-release", "", buildInfo("v1.5.0-rc.1")},
		{"fork", "", &debug.BuildInfo{Main: debug.Module{Path: "example.com/fork/go-cache-lru", Version: "v9.0.0"}}},
		{"ldflags snapshot", "1.4.3-SNAPSHOT-abcdef", buildInfo("v1.4.2")},
		{"ldflags custom", "replaced-in-test", buildInfo("v1.4.2")},
		{"ldflags with revision", "1.4.2 abcdefabcdef", nil},
	} {
		if got, err := currentVersion(tc.ldflags, tc.bi); err == nil {
			t.Errorf("%s: currentVersion = %v; want an error", tc.name, got)
		}
	}
}
