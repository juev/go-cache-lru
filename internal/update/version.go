package update

import (
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"strings"
)

// modulePath is the module whose releases the updater installs.
const modulePath = "github.com/juev/go-cache-lru"

// semver is a stable release version: no pre-release or build suffix.
type semver struct{ major, minor, patch int }

func (v semver) String() string { return fmt.Sprintf("v%d.%d.%d", v.major, v.minor, v.patch) }

// newer reports whether v is a later release than old.
func (v semver) newer(old semver) bool {
	if v.major != old.major {
		return v.major > old.major
	}
	if v.minor != old.minor {
		return v.minor > old.minor
	}
	return v.patch > old.patch
}

// parseVersion parses "1.2.3" or "v1.2.3".
func parseVersion(s string) (semver, bool) {
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	var n [3]int
	for i, p := range parts {
		// Digits only, without leading zeros.
		if p == "" || len(p) > 9 || (len(p) > 1 && p[0] == '0') || strings.TrimLeft(p, "0123456789") != "" {
			return semver{}, false
		}
		n[i], _ = strconv.Atoi(p)
	}
	return semver{n[0], n[1], n[2]}, true
}

// currentVersion returns the release that the running binary was built
// from. ldflags is the version set at link time by release builds; without
// it the module version must come from "go install module@vX.Y.Z". Any
// other build, such as a development, pseudo-version or modified one, is
// not a release, and the error says why.
func currentVersion(ldflags string, bi *debug.BuildInfo) (semver, error) {
	if ldflags != "" {
		v, ok := parseVersion(ldflags)
		if !ok {
			return semver{}, fmt.Errorf("version %q is not a stable release", ldflags)
		}
		return v, nil
	}
	if bi == nil {
		return semver{}, errors.New("development build: no build information")
	}
	if bi.Main.Path != modulePath {
		return semver{}, fmt.Errorf("built from module %q", bi.Main.Path)
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.modified" && s.Value == "true" {
			return semver{}, errors.New("development build: modified source")
		}
	}
	if bi.Main.Version == "" || bi.Main.Version == "(devel)" {
		return semver{}, errors.New("development build")
	}
	v, ok := parseVersion(bi.Main.Version)
	if !ok || !strings.HasPrefix(bi.Main.Version, "v") {
		return semver{}, fmt.Errorf("development build: version %q is not a stable release", bi.Main.Version)
	}
	return v, nil
}
