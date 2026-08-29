package updater

// Plan 44 Task 3 — table tests for the pure version-arithmetic layer.
// TDD: written before version.go; the classic SemVer ordering sequence
// (§11.4) is pinned verbatim as the comparison oracle.

import (
	"reflect"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/buildinfo"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want Version
	}{
		{"1.2.3", Version{Major: 1, Minor: 2, Patch: 3}},
		{"v1.2.3", Version{Major: 1, Minor: 2, Patch: 3}},
		{"v0.13.0", Version{Major: 0, Minor: 13, Patch: 0}},
		{"1.2.3-rc.1", Version{Major: 1, Minor: 2, Patch: 3, Pre: []string{"rc", "1"}}},
		{"v1.2.3-rc.1", Version{Major: 1, Minor: 2, Patch: 3, Pre: []string{"rc", "1"}}},
		{"1.0.0-alpha.beta.1", Version{Major: 1, Minor: 0, Patch: 0, Pre: []string{"alpha", "beta", "1"}}},
		{"1.0.0-x-1", Version{Major: 1, Minor: 0, Patch: 0, Pre: []string{"x-1"}}},
		{"1.0.0-0", Version{Major: 1, Minor: 0, Patch: 0, Pre: []string{"0"}}},
	}
	for _, tc := range tests {
		got, err := ParseVersion(tc.in)
		if err != nil {
			t.Fatalf("ParseVersion(%q) unexpected error: %v", tc.in, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseVersionInvalid(t *testing.T) {
	// "dev" first: the local-build default must be rejected so the caller
	// fails closed to an explicit --version instead of guessing direction.
	tests := []string{
		"dev",
		"",
		"v",
		"1",
		"1.2",
		"1.2.3.4",
		"abc",
		"1.2.x",
		" 1.2.3",
		"1.2.3 ",
		"v1.2.3-",
		"1.2.3-alpha..1",
		"1.2.3-alpha;rm",
		"1.2.3-rc.1\n",
		"1.2.3@rc",
		"99999999999999999999.0.0", // int overflow -> reject, not clamp
	}
	for _, in := range tests {
		if got, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) = %+v, want error", in, got)
		}
	}
}

func TestCompareVersionsTable(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"v1.0.0", "1.0.0", 0}, // v prefix carries no weight after parse
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.10.0", "1.9.0", 1},     // numeric, not lexicographic
		{"0.13.0", "0.12.0", 1},    // repo-realistic pair
		{"1.0.0", "1.0.0-rc.1", 1}, // release > any pre of same triple
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0-rc.1", "1.0.0-rc.2", -1},
		{"1.0.0-rc.2", "1.0.0-rc.10", -1},         // numeric pre, not lexicographic
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},      // fewer pre ids < more
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1}, // numeric id < alpha id
		{"1.0.0-beta", "1.0.0-alpha", 1},
		{"1.0.0-alpha.1", "1.0.0-beta.2", -1},
		{"1.0.0-1", "1.0.0-2", -1},
		{"1.0.0-1", "1.0.0-alpha", -1}, // numeric id < alpha id
		{"1.0.0-alpha", "1.0.0-alpha", 0},
		{"1.0.0-beta.1", "1.0.0-beta.1", 0},
	}
	for _, tc := range tests {
		a, err := ParseVersion(tc.a)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tc.a, err)
		}
		b, err := ParseVersion(tc.b)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tc.b, err)
		}
		if got := CompareVersions(a, b); got != tc.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestSemVerClassicOrdering pins the §11.4 example sequence (plus rc.2)
// verbatim: every consecutive pair must compare -1 forward and +1 reversed.
func TestSemVerClassicOrdering(t *testing.T) {
	seq := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0-rc.2",
		"1.0.0",
	}
	parsed := make([]Version, len(seq))
	for i, s := range seq {
		v, err := ParseVersion(s)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", s, err)
		}
		parsed[i] = v
	}
	for i := 0; i+1 < len(seq); i++ {
		if got := CompareVersions(parsed[i], parsed[i+1]); got != -1 {
			t.Errorf("CompareVersions(%q, %q) = %d, want -1", seq[i], seq[i+1], got)
		}
		if got := CompareVersions(parsed[i+1], parsed[i]); got != 1 {
			t.Errorf("CompareVersions(%q, %q) = %d, want 1", seq[i+1], seq[i], got)
		}
	}
}

func TestNormalizeVersionOutput(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{" 1.2.3\n", "1.2.3"},
		{"v1.2.3", "1.2.3"},
		{"  v0.13.0  ", "0.13.0"},
		{"1.2.3", "1.2.3"},
		{"", ""},
		{"dev", "dev"}, // pure string transform; no semantic judgement here
	}
	for _, tc := range tests {
		if got := NormalizeVersionOutput(tc.in); got != tc.want {
			t.Errorf("NormalizeVersionOutput(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateTag(t *testing.T) {
	valid := []string{
		"v0.13.0",
		"0.13.0",
		"v1.0.0-rc.1",
		"1.0.0-alpha.beta.1",
		"v1.0.0-0",
		"v1.0.0-x-1",
	}
	invalid := []string{
		"v1..2",
		"v1.2.3;rm",
		"dev",
		"",
		"v1.2",
		"1.2.3.4",
		"1.2.3-",
		"v 1.2.3",
		"v1.2.3\n",
		"1.2.3 ",
		"x1.2.3",
		"1.2.3;rm",
	}
	for _, s := range valid {
		if err := ValidateTag(s); err != nil {
			t.Errorf("ValidateTag(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range invalid {
		if err := ValidateTag(s); err == nil {
			t.Errorf("ValidateTag(%q) = nil, want error", s)
		}
	}
}

func TestAssetName(t *testing.T) {
	tests := []struct {
		version, goos, goarch string
		want                  string
		wantErr               bool
	}{
		// Full release matrix, both tag spellings on representative rows.
		{"v0.13.0", "windows", "amd64", "sshmgr_0.13.0_windows_amd64.zip", false},
		{"0.13.0", "windows", "arm64", "sshmgr_0.13.0_windows_arm64.zip", false},
		{"v0.13.0", "linux", "amd64", "sshmgr_0.13.0_linux_amd64.tar.gz", false},
		{"v0.13.0", "linux", "arm64", "sshmgr_0.13.0_linux_arm64.tar.gz", false},
		{"v0.13.0", "darwin", "amd64", "sshmgr_0.13.0_darwin_amd64.tar.gz", false},
		{"0.13.0", "darwin", "arm64", "sshmgr_0.13.0_darwin_arm64.tar.gz", false},
		// Outside the matrix -> error.
		{"v0.13.0", "linux", "386", "", true},
		{"v0.13.0", "freebsd", "amd64", "", true},
		{"v0.13.0", "windows", "armv7", "", true},
		{"v0.13.0", "", "", "", true},
		// Invalid version -> error (fail-closed before any URL/filename use).
		{"dev", "linux", "amd64", "", true},
		{"v1.2.3;rm", "linux", "amd64", "", true},
	}
	for _, tc := range tests {
		got, err := AssetName(tc.version, tc.goos, tc.goarch)
		if tc.wantErr {
			if err == nil {
				t.Errorf("AssetName(%q, %q, %q) = %q, want error", tc.version, tc.goos, tc.goarch, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("AssetName(%q, %q, %q) unexpected error: %v", tc.version, tc.goos, tc.goarch, err)
		}
		if got != tc.want {
			t.Errorf("AssetName(%q, %q, %q) = %q, want %q", tc.version, tc.goos, tc.goarch, got, tc.want)
		}
	}

	// Out-of-matrix error must list the supported matrix (spec §4.1).
	_, err := AssetName("v0.13.0", "linux", "386")
	if err == nil {
		t.Fatal("AssetName linux/386: want error")
	}
	for _, hint := range []string{"windows", "linux", "darwin", "amd64", "arm64"} {
		if !strings.Contains(err.Error(), hint) {
			t.Errorf("AssetName linux/386 error %q: missing matrix hint %q", err.Error(), hint)
		}
	}
}

// TestBuildinfoOwnerRepo pins the constants Task 4+ will build the GitHub API
// URL from (spec §4.1: repos/allanpk716/ssh-manager-mcp/releases/...).
func TestBuildinfoOwnerRepo(t *testing.T) {
	if buildinfo.Owner != "allanpk716" {
		t.Errorf("buildinfo.Owner = %q, want %q", buildinfo.Owner, "allanpk716")
	}
	if buildinfo.Repo != "ssh-manager-mcp" {
		t.Errorf("buildinfo.Repo = %q, want %q", buildinfo.Repo, "ssh-manager-mcp")
	}
}
