package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// goldenPayload is the fake binary content carried by golden fixtures and
// asserted byte-exact after extraction.
const goldenPayload = "#!/bin/sh\necho fake sshmgr binary payload\n"

type zipEntry struct {
	name string
	data string
	mode fs.FileMode // 0 => regular 0644
}

type tarEntry struct {
	name     string
	typeflag byte
	data     string
	linkname string
	mode     int64 // 0 => 0644
}

func writeZipFixture(t *testing.T, path string, entries []zipEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		fh := &zip.FileHeader{Name: e.name}
		fh.SetMode(mode)
		w, err := zw.CreateHeader(fh)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeTarGzFixture(t *testing.T, path string, entries []tarEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: e.typeflag,
			Mode:     mode,
			Linkname: e.linkname,
			Size:     int64(len(e.data)),
		}
		if e.typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg && len(e.data) > 0 {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
}

// entryNames snapshots dir's entry names.
func entryNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	for _, de := range dirEntries(t, dir) {
		names[de.Name()] = true
	}
	return names
}

// assertDirExact asserts dir contains exactly the given names.
func assertDirExact(t *testing.T, dir string, want map[string]bool) {
	t.Helper()
	got := entryNames(t, dir)
	if len(got) != len(want) {
		t.Fatalf("dir entries = %v, want exactly %v", got, want)
	}
	for n := range got {
		if !want[n] {
			t.Errorf("unexpected file %q in dir (want exactly %v)", n, want)
		}
	}
}

// assertNoNewFiles asserts nothing beyond the `before` snapshot appeared —
// the malicious-matrix invariant 错误⇒目录零新文件.
func assertNoNewFiles(t *testing.T, dir string, before map[string]bool) {
	t.Helper()
	for n := range entryNames(t, dir) {
		if !before[n] {
			t.Errorf("malicious archive left new file %q in dir (零新文件 invariant)", n)
		}
	}
}

func assertExtractedBinary(t *testing.T, outPath string) {
	t.Helper()
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != goldenPayload {
		t.Errorf("extracted content = %q, want the golden payload byte-exact", data)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(outPath)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("mode = %04o, want exactly 0755 (落地即 chmod)", fi.Mode().Perm())
		}
	}
}

func TestExtractBinaryGoldenZip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sshmgr_0.13.1_linux_amd64.zip")
	writeZipFixture(t, archive, []zipEntry{
		{name: "LICENSE", data: "MIT license text"},
		{name: "sshmgr", data: goldenPayload, mode: 0o644}, // mode deliberately non-executable: chmod must overwrite
		{name: "README.md", data: "# sshmgr"},
	})
	got, err := ExtractBinary(archive, "linux")
	if err != nil {
		t.Fatalf("ExtractBinary: %v", err)
	}
	if want := filepath.Join(dir, "sshmgr"); got != want {
		t.Errorf("output path = %q, want %q", got, want)
	}
	assertExtractedBinary(t, got)
	// 其余不落地: dir holds exactly the archive + the binary.
	assertDirExact(t, dir, map[string]bool{filepath.Base(archive): true, "sshmgr": true})
}

func TestExtractBinaryGoldenTarGz(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sshmgr_0.13.1_darwin_arm64.tar.gz")
	writeTarGzFixture(t, archive, []tarEntry{
		{name: "README.md", data: "# sshmgr"},
		{name: "sshmgr", data: goldenPayload, mode: 0o644}, // mode deliberately non-executable: chmod must overwrite
		{name: "LICENSE", data: "MIT license text"},
	})
	got, err := ExtractBinary(archive, "darwin")
	if err != nil {
		t.Fatalf("ExtractBinary: %v", err)
	}
	if want := filepath.Join(dir, "sshmgr"); got != want {
		t.Errorf("output path = %q, want %q", got, want)
	}
	assertExtractedBinary(t, got)
	assertDirExact(t, dir, map[string]bool{filepath.Base(archive): true, "sshmgr": true})
}

func TestExtractBinaryWindowsSuffix(t *testing.T) {
	t.Run("zip with sshmgr.exe", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "sshmgr_0.13.1_windows_amd64.zip")
		writeZipFixture(t, archive, []zipEntry{
			{name: "sshmgr.exe", data: goldenPayload},
			{name: "LICENSE", data: "MIT"},
		})
		got, err := ExtractBinary(archive, "windows")
		if err != nil {
			t.Fatalf("ExtractBinary: %v", err)
		}
		if want := filepath.Join(dir, "sshmgr.exe"); got != want {
			t.Errorf("output path = %q, want %q", got, want)
		}
		assertExtractedBinary(t, got)
		assertDirExact(t, dir, map[string]bool{filepath.Base(archive): true, "sshmgr.exe": true})
	})
	t.Run("tar.gz with sshmgr.exe", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "x.tar.gz")
		writeTarGzFixture(t, archive, []tarEntry{
			{name: "sshmgr.exe", data: goldenPayload},
		})
		if _, err := ExtractBinary(archive, "windows"); err != nil {
			t.Fatalf("ExtractBinary: %v", err)
		}
		assertExtractedBinary(t, filepath.Join(dir, "sshmgr.exe"))
	})
	t.Run("windows goos ignores extensionless sshmgr", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "mixed.zip")
		writeZipFixture(t, archive, []zipEntry{
			{name: "sshmgr", data: goldenPayload},
			{name: "sshmgr.exe", data: goldenPayload + "windows variant\n"},
		})
		if _, err := ExtractBinary(archive, "windows"); err != nil {
			t.Fatalf("ExtractBinary: %v", err)
		}
		// Only sshmgr.exe lands; the extensionless sshmgr is another entry that
		// must not be written.
		assertDirExact(t, dir, map[string]bool{filepath.Base(archive): true, "sshmgr.exe": true})
	})
	t.Run("windows goos with only extensionless sshmgr errors", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "only.tar.gz")
		writeTarGzFixture(t, archive, []tarEntry{{name: "sshmgr", data: goldenPayload}})
		before := entryNames(t, dir)
		if _, err := ExtractBinary(archive, "windows"); err == nil {
			t.Fatal("want error: archive without sshmgr.exe root entry")
		}
		assertNoNewFiles(t, dir, before)
	})
}

// okTar is a valid root sshmgr regular entry prepended to malicious fixtures:
// the extractor may already have landed the binary when it meets the malicious
// entry, so every case below also exercises the zero-residue cleanup path.
var okTar = tarEntry{name: "sshmgr", typeflag: tar.TypeReg, data: goldenPayload, mode: 0o644}

var okZip = zipEntry{name: "sshmgr", data: goldenPayload, mode: 0o644}

func TestExtractBinaryMaliciousMatrix(t *testing.T) {
	cases := []struct {
		name       string
		kind       string // "zip" | "tar.gz"
		zips       []zipEntry
		tars       []tarEntry
		wantErrSub string
	}{
		// --- tar.gz matrix ---
		{name: "tar/traversal_parent", kind: "tar.gz",
			tars:       []tarEntry{okTar, {name: "../evil", data: "x"}},
			wantErrSub: "../evil"},
		{name: "tar/absolute_path", kind: "tar.gz",
			tars:       []tarEntry{okTar, {name: "/abs/evil", data: "x"}},
			wantErrSub: "/abs/evil"},
		{name: "tar/subdir_same_name", kind: "tar.gz",
			tars:       []tarEntry{okTar, {name: "sub/sshmgr", data: "downgraded binary"}},
			wantErrSub: "sub/sshmgr"},
		{name: "tar/backslash_separator", kind: "tar.gz",
			tars:       []tarEntry{okTar, {name: `..\evil`, data: "x"}},
			wantErrSub: "path separators"},
		{name: "tar/duplicate_target", kind: "tar.gz",
			tars:       []tarEntry{okTar, {name: "sshmgr", data: "second copy"}},
			wantErrSub: "duplicate"},
		{name: "tar/symlink_named_target", kind: "tar.gz",
			tars:       []tarEntry{{name: "sshmgr", typeflag: tar.TypeSymlink, linkname: "/etc/passwd"}},
			wantErrSub: "not a regular file"},
		{name: "tar/hardlink_named_target", kind: "tar.gz",
			tars:       []tarEntry{{name: "sshmgr", typeflag: tar.TypeLink, linkname: "LICENSE"}},
			wantErrSub: "not a regular file"},
		{name: "tar/chardev_named_target", kind: "tar.gz",
			tars:       []tarEntry{{name: "sshmgr", typeflag: tar.TypeChar}},
			wantErrSub: "not a regular file"},
		{name: "tar/dir_named_target", kind: "tar.gz",
			tars:       []tarEntry{{name: "sshmgr", typeflag: tar.TypeDir}},
			wantErrSub: "not a regular file"},
		{name: "tar/unrelated_symlink_also_rejected", kind: "tar.gz",
			tars:       []tarEntry{okTar, {name: "LICENSE", typeflag: tar.TypeSymlink, linkname: "/etc/shadow"}},
			wantErrSub: "not a regular file"},

		// --- zip matrix ---
		{name: "zip/traversal_parent", kind: "zip",
			zips:       []zipEntry{okZip, {name: "../evil", data: "x"}},
			wantErrSub: "../evil"},
		{name: "zip/absolute_path", kind: "zip",
			zips:       []zipEntry{okZip, {name: "/abs/evil", data: "x"}},
			wantErrSub: "/abs/evil"},
		{name: "zip/subdir_same_name", kind: "zip",
			zips:       []zipEntry{okZip, {name: "sub/sshmgr", data: "downgraded binary"}},
			wantErrSub: "sub/sshmgr"},
		{name: "zip/backslash_separator", kind: "zip",
			zips:       []zipEntry{okZip, {name: `..\evil`, data: "x"}},
			wantErrSub: "path separators"},
		{name: "zip/duplicate_target", kind: "zip",
			zips:       []zipEntry{okZip, {name: "sshmgr", data: "second copy"}},
			wantErrSub: "duplicate"},
		{name: "zip/symlink_named_target", kind: "zip",
			zips:       []zipEntry{{name: "sshmgr", data: "/etc/passwd", mode: fs.ModeSymlink | 0o777}},
			wantErrSub: "not a regular file"},
		{name: "zip/dir_named_target", kind: "zip",
			zips:       []zipEntry{{name: "sshmgr", mode: fs.ModeDir | 0o755}},
			wantErrSub: "not a regular file"},
		{name: "zip/dir_entry_trailing_slash", kind: "zip",
			zips:       []zipEntry{{name: "sshmgr/", mode: fs.ModeDir | 0o755}},
			wantErrSub: "path separators"},
		{name: "zip/unrelated_symlink_also_rejected", kind: "zip",
			zips:       []zipEntry{okZip, {name: "LICENSE", data: "/etc/shadow", mode: fs.ModeSymlink | 0o777}},
			wantErrSub: "not a regular file"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var archive string
			if tc.kind == "zip" {
				archive = filepath.Join(dir, "case.zip")
				writeZipFixture(t, archive, tc.zips)
			} else {
				archive = filepath.Join(dir, "case.tar.gz")
				writeTarGzFixture(t, archive, tc.tars)
			}
			before := entryNames(t, dir)
			out, err := ExtractBinary(archive, "linux")
			if err == nil {
				t.Fatalf("malicious archive extracted successfully (out=%q), want rejection", out)
			}
			if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantErrSub)
			}
			assertNoNewFiles(t, dir, before)
		})
	}
}

func TestExtractBinaryNoTargetEntry(t *testing.T) {
	cases := []struct {
		name    string
		build   func(t *testing.T, path string)
		extName string
	}{
		{name: "zip with only docs", extName: "docs.zip", build: func(t *testing.T, path string) {
			writeZipFixture(t, path, []zipEntry{{name: "LICENSE", data: "MIT"}, {name: "README.md", data: "# x"}})
		}},
		{name: "empty zip", extName: "empty.zip", build: func(t *testing.T, path string) {
			writeZipFixture(t, path, nil)
		}},
		{name: "tar.gz with only docs", extName: "docs.tar.gz", build: func(t *testing.T, path string) {
			writeTarGzFixture(t, path, []tarEntry{{name: "LICENSE", data: "MIT"}})
		}},
		{name: "empty tar.gz", extName: "empty.tar.gz", build: func(t *testing.T, path string) {
			writeTarGzFixture(t, path, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, tc.extName)
			tc.build(t, archive)
			before := entryNames(t, dir)
			_, err := ExtractBinary(archive, "linux")
			if err == nil {
				t.Fatal("want error: archive without exact-name root entry")
			}
			if !strings.Contains(err.Error(), "no root entry") {
				t.Errorf("error %q does not mention missing root entry", err)
			}
			assertNoNewFiles(t, dir, before)
		})
	}
}

func TestExtractBinaryBadArchive(t *testing.T) {
	t.Run("unsupported extension", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "payload.rar")
		if err := os.WriteFile(archive, []byte("irrelevant"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ExtractBinary(archive, "linux"); err == nil || !strings.Contains(err.Error(), "unsupported archive format") {
			t.Errorf("err = %v, want unsupported archive format", err)
		}
	})
	t.Run("zip suffix with non-zip bytes", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "fake.zip")
		if err := os.WriteFile(archive, []byte("definitely not a zip container"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ExtractBinary(archive, "linux"); err == nil {
			t.Fatal("want error for non-zip bytes behind .zip suffix")
		}
	})
	t.Run("tar.gz suffix with non-gzip bytes", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "fake.tar.gz")
		if err := os.WriteFile(archive, []byte("definitely not gzip"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ExtractBinary(archive, "linux"); err == nil {
			t.Fatal("want error for non-gzip bytes behind .tar.gz suffix")
		}
	})
	t.Run("tar.gz suffix with gzip non-tar payload", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "notatar.tar.gz")
		f, err := os.Create(archive)
		if err != nil {
			t.Fatal(err)
		}
		gw := gzip.NewWriter(f)
		if _, err := gw.Write([]byte("gzip is fine, tar is not here")); err != nil {
			t.Fatal(err)
		}
		if err := gw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := ExtractBinary(archive, "linux"); err == nil {
			t.Fatal("want error for gzip payload without tar structure")
		}
	})
}

func TestExtractBinaryTotalDecompressedCap(t *testing.T) {
	// Single-member cap stays at its default (200MiB) — members below it
	// must pass every per-member check; only the cumulative decompressed
	// stream (tar header 512B + padded data, per member) crosses the
	// shrunken total cap, during member 2.
	origTotal := maxTotalDecompressed
	maxTotalDecompressed = 1060 // member1 hdr+data=1024 ok; member2 header read trips 1536 > 1060
	defer func() { maxTotalDecompressed = origTotal }()

	dir := t.TempDir()
	archive := filepath.Join(dir, "many.tar.gz")
	member := strings.Repeat("B", 20) // far below the single-member cap
	writeTarGzFixture(t, archive, []tarEntry{
		{name: "sshmgr", data: member},  // extracted, then cleaned up on abort
		{name: "LICENSE", data: member}, // skipped member still counts toward the total
	})
	before := entryNames(t, dir)
	_, err := ExtractBinary(archive, "linux")
	if err == nil {
		t.Fatal("want abort: cumulative decompressed bytes exceed the total cap")
	}
	if !strings.Contains(err.Error(), "cumulative decompressed") {
		t.Errorf("err = %v, want cumulative decompressed limit abort", err)
	}
	// 零残留: member 1's already-landed output is removed again.
	assertNoNewFiles(t, dir, before)
}

func TestExtractBinaryOutputCollision(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "sshmgr_0.13.1_linux_amd64.zip")
	writeZipFixture(t, archive, []zipEntry{{name: "sshmgr", data: goldenPayload}})
	sentinel := filepath.Join(dir, "sshmgr")
	sentinelBytes := []byte("pre-existing file, not ours to touch")
	if err := os.WriteFile(sentinel, sentinelBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	before := entryNames(t, dir)
	if _, err := ExtractBinary(archive, "linux"); err == nil {
		t.Fatal("want error: O_EXCL must refuse to overwrite the pre-existing file")
	}
	// Pre-existing file preserved byte-exact, nothing else added.
	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sentinelBytes) {
		t.Errorf("pre-existing file was modified: %q", got)
	}
	assertNoNewFiles(t, dir, before)
}

func TestExtractBinaryMemberSizeCap(t *testing.T) {
	origCap := extractMaxBytes
	extractMaxBytes = 16
	defer func() { extractMaxBytes = origCap }()

	big := make([]byte, 32)
	for i := range big {
		big[i] = 'A'
	}
	t.Run("zip", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "big.zip")
		writeZipFixture(t, archive, []zipEntry{{name: "sshmgr", data: string(big)}})
		before := entryNames(t, dir)
		_, err := ExtractBinary(archive, "linux")
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Errorf("err = %v, want member size limit rejection", err)
		}
		assertNoNewFiles(t, dir, before)
	})
	t.Run("tar.gz", func(t *testing.T) {
		dir := t.TempDir()
		archive := filepath.Join(dir, "big.tar.gz")
		writeTarGzFixture(t, archive, []tarEntry{{name: "sshmgr", data: string(big)}})
		before := entryNames(t, dir)
		_, err := ExtractBinary(archive, "linux")
		if err == nil || !strings.Contains(err.Error(), "limit") {
			t.Errorf("err = %v, want member size limit rejection", err)
		}
		assertNoNewFiles(t, dir, before)
	})
}
