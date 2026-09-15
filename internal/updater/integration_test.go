package updater

// Plan 44 T9: fake-source full-loop integration tests (spec
// 2026-08-29-plan-44-self-update-rename §6 集成段). Entry-point choice: the
// T8 cobra assembly is already covered in-process by internal/cli
// (update_test.go, seam-faked); this file drives the CHAIN itself through
// the updater orchestration — LatestRelease → DownloadAsset(checksums) →
// ParseChecksums → DownloadAsset(asset) → ExtractBinary → StagedVersionCheck
// → ReplaceBinary — with ZERO seams swapped. Everything below is production
// code: real HTTP over the SSHMGR_UPDATE_BASE env seam (§4.6) to a loopback
// httptest fake of the GitHub Releases surface, and REAL child processes for
// the staged `version` self-check.
//
// The binaries are real too: both the "currently installed" copy and the
// release payload are tiny mains compiled by `go build` at fixture time
// (brief Step 1: 内建 tiny main 经 os/exec 落盘), each with its own version
// baked in — so the version flip is asserted by EXECUTING the updated copy,
// not by marker bytes alone.

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/buildinfo"
)

// mustMkdir creates a named subdirectory under the test's temp dir (the
// tiny-main build workspaces live outside `home` so the residue assertions
// only ever see the update's own artifacts).
func mustMkdir(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp(t.TempDir(), name+"-*")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// integrationBinName mirrors the release binary name for the running platform.
func integrationBinName() string {
	if runtime.GOOS == "windows" {
		return "sshmgr.exe"
	}
	return "sshmgr"
}

// buildTinyMain compiles a minimal `version`-printing main into dir and
// returns the binary path. Baking the version into the source (vs ldflags)
// keeps each fixture byte-distinct, so flip/untouched assertions are
// unambiguous.
func buildTinyMain(t *testing.T, dir, version string) string {
	t.Helper()
	src := filepath.Join(dir, "main.go")
	prog := fmt.Sprintf(`package main

import (
	"fmt"
	"os"
)

// version is baked per fixture build; changing it changes the binary bytes.
const version = %q

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
	}
}
`, version)
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, integrationBinName())
	cmd := exec.Command("go", "build", "-o", bin, src)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build tiny main (%s): %v\n%s", version, err, out)
	}
	return bin
}

// execVersion runs bin with the `version` argument and returns its trimmed
// stdout — the same output contract StagedVersionCheck normalizes against.
func execVersion(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.Command(bin, "version").Output()
	if err != nil {
		t.Fatalf("exec %s version: %v", bin, err)
	}
	return strings.TrimSpace(string(out))
}

// copyBinFile copies the src binary to dst (executable).
func copyBinFile(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// buildReleaseArchive packs payload as the flat single-entry release archive
// shape ExtractBinary expects: root entry `sshmgr.exe` (zip) on windows,
// `sshmgr` (tar.gz) elsewhere — format chosen from the asset name extension.
// Store/Mode keep member bytes == payload byte-for-byte and executable.
func buildReleaseArchive(t *testing.T, assetName string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if strings.HasSuffix(assetName, ".zip") {
		zw := zip.NewWriter(&buf)
		zh := &zip.FileHeader{Name: "sshmgr.exe", Method: zip.Store}
		zh.SetMode(0o755)
		w, err := zw.CreateHeader(zh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: "sshmgr", Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// integrationSource is a loopback fake of the GitHub Releases surface: the
// releases API endpoint (latest and tags/<tag> answer the same document, as
// only the latest path is exercised here) plus a flat asset directory with
// checksums.txt, mirroring how goreleaser ships a release.
type integrationSource struct {
	srv     *httptest.Server
	tag     string
	asset   string
	archive []byte
	sha     string
}

// startIntegrationSource serves tag with payload as the release asset and
// points SSHMGR_UPDATE_BASE at the loopback server (t.Setenv restores the
// env on cleanup). The browser_download_url deliberately names an
// off-whitelist origin host: with a custom base the updater must rebuild
// asset URLs onto the base (spec §4.2(4)), so the fake only answers on its
// own paths — any origin leak turns into a connection error.
func startIntegrationSource(t *testing.T, tag string, payload []byte, shaOverride string) *integrationSource {
	t.Helper()
	asset, err := AssetName(tag, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatalf("asset name: %v", err)
	}
	s := &integrationSource{tag: tag, asset: asset, archive: buildReleaseArchive(t, asset, payload)}
	sum := sha256.Sum256(s.archive)
	s.sha = hex.EncodeToString(sum[:])
	if shaOverride != "" {
		s.sha = shaOverride
	}

	relJSON := fmt.Sprintf(`{"tag_name":%q,"assets":[`+
		`{"name":%q,"browser_download_url":"http://origin.invalid/%s"},`+
		`{"name":"checksums.txt","browser_download_url":"http://origin.invalid/checksums.txt"}]}`,
		s.tag, s.asset, s.asset)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/"+buildinfo.Owner+"/"+buildinfo.Repo+"/releases/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, relJSON)
	})
	mux.HandleFunc("/"+s.asset, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(s.archive)
	})
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", s.sha, s.asset)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	t.Setenv(updateBaseEnv, s.srv.URL)
	return s
}

// runUpdateChain drives the updater orchestration the way the T8 CLI does
// (§4.3 pinned order, minus the interaction layer): checksums bootstrap →
// parse → asset download+verify → extract → staged self-check → replace.
// tmpdir lives in self's directory (same-volume rename, spec §4.3).
func runUpdateChain(t *testing.T, self string) (rel *Release, stagedVersion string, err error) {
	t.Helper()
	ctx := context.Background()
	rel, err = LatestRelease(ctx)
	if err != nil {
		return nil, "", err
	}
	tmpdir, err := os.MkdirTemp(filepath.Dir(self), ".sshmgr-update-tmp-*")
	if err != nil {
		return rel, "", err
	}
	defer os.RemoveAll(tmpdir)

	sumPath, err := DownloadAsset(ctx, rel.ChecksumsURL, "", tmpdir)
	if err != nil {
		return rel, "", err
	}
	data, err := os.ReadFile(sumPath)
	if err != nil {
		return rel, "", err
	}
	want, err := ParseChecksums(data, rel.AssetName)
	if err != nil {
		return rel, "", err
	}
	archivePath, err := DownloadAsset(ctx, rel.AssetURL, want, tmpdir)
	if err != nil {
		return rel, "", err
	}
	staged, err := ExtractBinary(archivePath, runtime.GOOS)
	if err != nil {
		return rel, "", err
	}
	got, err := StagedVersionCheck(staged, rel.Tag)
	if err != nil {
		return rel, got, err
	}
	return rel, got, ReplaceBinary(staged, self)
}

// TestIntegrationFakeSourceFullLoopFlipsVersion is the §6 集成段 happy path:
// fake v0.0.1-test copy → fake v0.0.2-test source → full update → the copy
// reports the new version when executed, the stale generation backup is
// cleaned and exactly one fresh generation holds the previous image
// (windows), / the unix branch flips atomically with zero generations and a
// 0755 mode.
func TestIntegrationFakeSourceFullLoopFlipsVersion(t *testing.T) {
	home := t.TempDir()
	self := filepath.Join(home, integrationBinName())

	oldBin := buildTinyMain(t, mustMkdir(t, "old-build"), "v0.0.1-test")
	newBin := buildTinyMain(t, mustMkdir(t, "new-build"), "v0.0.2-test")
	copyBinFile(t, oldBin, self)
	payload, err := os.ReadFile(newBin)
	if err != nil {
		t.Fatal(err)
	}

	// The copy is a REAL binary: it executes and reports the old version.
	if got := execVersion(t, self); got != "v0.0.1-test" {
		t.Fatalf("before update: self version = %q, want v0.0.1-test", got)
	}

	// Pre-seed a stale generation (the replace's 起手清理 target). The
	// generational mechanism is windows-only — unix has nothing to clean.
	staleGen := self + ".old.1700000000"
	if runtime.GOOS == "windows" {
		if err := os.WriteFile(staleGen, []byte("stale generation\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	src := startIntegrationSource(t, "v0.0.2-test", payload, "")

	rel, stagedGot, err := runUpdateChain(t, self)
	if err != nil {
		t.Fatalf("full loop: %v", err)
	}
	if rel.Tag != "v0.0.2-test" {
		t.Errorf("tag = %q, want v0.0.2-test", rel.Tag)
	}
	if !strings.HasPrefix(rel.AssetURL, src.srv.URL+"/") {
		t.Errorf("asset URL %q was not rebuilt onto the loopback base %s (spec §4.2(4))", rel.AssetURL, src.srv.URL)
	}
	if stagedGot != "0.0.2-test" {
		t.Errorf("staged check = %q, want 0.0.2-test (v-prefix normalized)", stagedGot)
	}

	// Version flip: the copy's bytes ARE the payload, and it really executes
	// reporting the new version.
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, payload) {
		t.Errorf("self was not flipped to the new payload (%d vs %d bytes)", len(b), len(payload))
	}
	if got := execVersion(t, self); got != "v0.0.2-test" {
		t.Errorf("after update: self version = %q, want v0.0.2-test", got)
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// 代际清理:the stale generation is gone and exactly ONE fresh backup
		// remains, carrying the previous binary image.
		var gens []string
		for _, e := range entries {
			if strings.Contains(e.Name(), ".old.") {
				gens = append(gens, e.Name())
			}
		}
		if len(gens) != 1 {
			t.Fatalf("generations after update = %v, want exactly the fresh backup (stale one cleaned)", gens)
		}
		gb, err := os.ReadFile(filepath.Join(home, gens[0]))
		if err != nil {
			t.Fatal(err)
		}
		oldPayload, err := os.ReadFile(oldBin)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(gb, oldPayload) {
			t.Error(".old backup does not carry the previous binary image")
		}
	} else {
		// Unix atomic rename: zero generations; the extracted-then-renamed
		// binary keeps its 落地即 chmod 0755 mode.
		for _, e := range entries {
			if strings.Contains(e.Name(), ".old.") {
				t.Errorf("unix branch must not create generations, found %s", e.Name())
			}
		}
		fi, err := os.Stat(self)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("flipped binary mode = %s, want 0755", fi.Mode())
		}
	}
}

// TestIntegrationWrongChecksumLeavesCopyUntouched is the failure loop: the
// fake source advertises a well-formed but WRONG sha256 for the asset, the
// chain must reject at the download-verification step, and the installed
// copy stays byte-identical (and still executes its old version) with zero
// residue in its directory.
func TestIntegrationWrongChecksumLeavesCopyUntouched(t *testing.T) {
	home := t.TempDir()
	self := filepath.Join(home, integrationBinName())

	oldBin := buildTinyMain(t, mustMkdir(t, "old-build"), "v0.0.1-test")
	copyBinFile(t, oldBin, self)
	newBin := buildTinyMain(t, mustMkdir(t, "new-build"), "v0.0.2-test")
	payload, err := os.ReadFile(newBin)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}

	// 假源给错 SHA256:格式合法、与真实资产摘要不符。
	startIntegrationSource(t, "v0.0.2-test", payload, strings.Repeat("ab", 32))

	_, _, err = runUpdateChain(t, self)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("want sha256-mismatch rejection, got %v", err)
	}

	after, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("copy bytes changed on the checksum-failure loop")
	}
	if got := execVersion(t, self); got != "v0.0.1-test" {
		t.Errorf("copy reports %q after the failed loop, want untouched v0.0.1-test", got)
	}

	// 零残留:目录里只有副本本身(无 .old、无 tmpdir、无半截下载)。
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != integrationBinName() {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("home residue after failed loop: want only %q, got %v", integrationBinName(), names)
	}
}
