// Package updater implements the sshmgr self-update pipeline: release
// discovery on GitHub, artifact download + checksum verification and
// transactional self-replacement (spec: 2026-08-29-plan-44-self-update-rename,
// §4.1–§4.3). This file is the pure version-arithmetic layer: parsing, SemVer
// comparison, tag validation and release-asset name computation — no I/O, no
// third-party dependencies.
package updater

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a parsed semantic version triple plus its pre-release
// identifiers. Pre is nil for a plain release (no "-" part); otherwise it
// holds the dot-separated identifiers in order, e.g. "1.2.3-rc.1" ->
// ["rc", "1"].
type Version struct {
	Major, Minor, Patch int
	Pre                 []string
}

// tagRe is the authoritative tag format (spec §4.1): optional "v" prefix,
// three dot-separated numbers, optional pre-release suffix. It is the
// URL-safety gate — everything it accepts is safe to embed in a request path
// (the caller still applies url.PathEscape before interpolation).
var tagRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// ParseVersion parses "1.2.3", "v1.2.3" or "1.2.3-rc.1" into a Version. It
// rejects anything else — including the local-build default "dev" — so the
// update flow can fail closed and demand an explicit --version instead of
// guessing an upgrade direction.
func ParseVersion(s string) (Version, error) {
	body := strings.TrimPrefix(s, "v")
	core, pre, hasPre := strings.Cut(body, "-")

	nums := strings.SplitN(core, ".", 3) // "1.2.3.4" -> ["1","2","3.4"]; trailing junk fails Atoi below
	if len(nums) != 3 {
		return Version{}, fmt.Errorf("invalid version %q: want MAJOR.MINOR.PATCH", s)
	}
	out := Version{}
	for i, p := range nums {
		if p == "" {
			return Version{}, fmt.Errorf("invalid version %q: empty numeric field", s)
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, fmt.Errorf("invalid version %q: %q is not a number", s, p)
		}
		switch i {
		case 0:
			out.Major = n
		case 1:
			out.Minor = n
		case 2:
			out.Patch = n
		}
	}
	if hasPre {
		if pre == "" {
			return Version{}, fmt.Errorf("invalid version %q: empty pre-release", s)
		}
		for _, id := range strings.Split(pre, ".") {
			if !isPreIdentifier(id) {
				return Version{}, fmt.Errorf("invalid version %q: bad pre-release identifier %q", s, id)
			}
			// A numeric identifier wider than int64 is rejected (not
			// clamped) — same philosophy as the triple fields, and it keeps
			// CompareVersions's numeric path infallible.
			if isNumericIdentifier(id) {
				if _, err := strconv.Atoi(id); err != nil {
					return Version{}, fmt.Errorf("invalid version %q: numeric pre-release identifier %q overflows int64", s, id)
				}
			}
			out.Pre = append(out.Pre, id)
		}
	}
	return out, nil
}

// CompareVersions orders two parsed versions and returns -1, 0 or 1.
//
// Numeric triple field-wise first; then per SemVer §11.4 a pre-release sorts
// below its own release, and two pre-releases compare identifier by
// identifier: numeric identifiers numerically, alphanumeric identifiers by
// ASCII lexicographic order, numeric < alphanumeric; when all shared
// identifiers are equal, fewer identifiers sorts lower ("1.0.0-alpha" <
// "1.0.0-alpha.1"). This is what pins same-triple pre-releases for
// downgrade/rollback (--version v0.13.0-rc.2 vs installed rc.1).
func CompareVersions(a, b Version) int {
	switch {
	case a.Major != b.Major:
		return sign(a.Major, b.Major)
	case a.Minor != b.Minor:
		return sign(a.Minor, b.Minor)
	case a.Patch != b.Patch:
		return sign(a.Patch, b.Patch)
	}

	// Pre-release presence: none > some.
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}

	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		ai, bi := a.Pre[i], b.Pre[i]
		an, bn := isNumericIdentifier(ai), isNumericIdentifier(bi)
		switch {
		case an && bn:
			// ParseVersion rejects numeric identifiers wider than int64,
			// so Atoi here always succeeds — no string-order fallback (a
			// fallback would break transitivity, e.g. "3999..." < "4").
			x, _ := strconv.Atoi(ai)
			y, _ := strconv.Atoi(bi)
			if x != y {
				return sign(x, y)
			}
		case an != bn:
			if an { // numeric identifier < alphanumeric identifier
				return -1
			}
			return 1
		default: // both alphanumeric: ASCII lexicographic
			if ai != bi {
				return signStr(ai, bi)
			}
		}
	}
	// All shared identifiers equal: more identifiers is greater.
	return sign(len(a.Pre), len(b.Pre))
}

// NormalizeVersionOutput applies the staged-binary self-check contract (spec
// §4.3): trim surrounding whitespace, then strip one leading "v", so the
// binary's `version` output ("v1.2.3", " 1.2.3\n") compares equal to the
// target version string ("1.2.3").
func NormalizeVersionOutput(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "v")
}

// ValidateTag checks a release tag against ^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$
// (spec §4.1). Anything failing this must never reach a request path or a
// filename — shell metacharacters, whitespace, traversal segments are all
// rejected here.
func ValidateTag(s string) error {
	if !tagRe.MatchString(s) {
		return fmt.Errorf("invalid release tag %q: want vMAJOR.MINOR.PATCH[-PRERELEASE] (e.g. v0.13.0-rc.1)", s)
	}
	return nil
}

// supportedReleaseMatrix is the platform set releases are built for
// (.goreleaser.yml builds; spec §4.1).
var (
	supportedGOOS   = map[string]bool{"windows": true, "linux": true, "darwin": true}
	supportedGOARCH = map[string]bool{"amd64": true, "arm64": true}
)

// AssetName computes the release asset file name for a tag on the given
// platform: sshmgr_<ver>_<GOOS>_<GOARCH>.<ext> where <ver> is the tag without
// its "v" prefix, windows -> .zip and everything else -> .tar.gz (mirrors
// .goreleaser.yml project_name/name_template/format_overrides).
//
// Errors: GOOS/GOARCH outside the release matrix (the error lists the
// matrix), or a version failing ValidateTag (fail-closed before the name is
// used for discovery or download).
func AssetName(version, goos, goarch string) (string, error) {
	if err := ValidateTag(version); err != nil {
		return "", err
	}
	if !supportedGOOS[goos] || !supportedGOARCH[goarch] {
		return "", fmt.Errorf(
			"unsupported platform GOOS=%q GOARCH=%q: sshmgr releases cover GOOS {windows, linux, darwin} x GOARCH {amd64, arm64} only",
			goos, goarch)
	}
	return fmt.Sprintf("sshmgr_%s_%s_%s.%s",
		strings.TrimPrefix(version, "v"), goos, goarch, archiveExt(goos)), nil
}

func archiveExt(goos string) string {
	if goos == "windows" {
		return "zip"
	}
	return "tar.gz"
}

// isPreIdentifier reports whether s is a valid SemVer pre-release identifier
// component: non-empty, characters limited to [0-9A-Za-z-].
func isPreIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '-') {
			return false
		}
	}
	return true
}

// isNumericIdentifier reports whether s consists solely of digits (and is
// non-empty), i.e. a SemVer numeric identifier.
func isNumericIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func sign(x, y int) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	default:
		return 0
	}
}

func signStr(x, y string) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	default:
		return 0
	}
}
