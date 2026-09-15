package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// extractMaxBytes caps one archive member (spec §4.2(3): 单成员 200MiB 上限);
// maxTotalDecompressed caps the cumulative decompressed bytes flowing through
// the tar.gz stream — skipped members included (总解压字节上限), so an
// archive of many members each just under the single-member cap cannot drain
// an unbounded decompression. Package vars solely so tests can shrink them;
// production code must never mutate them.
var (
	extractMaxBytes      = int64(200) << 20
	maxTotalDecompressed = int64(200) << 20
)

// Binary entry names inside release archives (.goreleaser.yml: binary:
// sshmgr, flat archive without wrap_in_directory).
const (
	unixBinaryEntry    = "sshmgr"
	windowsBinaryEntry = "sshmgr.exe"
)

// binaryEntryName maps goos to the archive entry name of the binary. Per the
// task contract the goos argument only selects the .exe suffix — GOOS/GOARCH
// matrix validation is AssetName's job (spec §4.1), not ours.
func binaryEntryName(goos string) string {
	if goos == "windows" {
		return windowsBinaryEntry
	}
	return unixBinaryEntry
}

// ExtractBinary streams the archive at archivePath (zip or tar.gz, chosen by
// file extension — spec §4.2(5) 格式按扩展名识别) and lands exactly one file:
// the root entry whose name precisely equals sshmgr (sshmgr.exe when goos ==
// "windows"). The output is written next to the archive as sshmgr[.exe] via
// O_CREATE|O_EXCL|O_WRONLY and chmod'ed 0755 immediately upon landing (the
// 0666&^umask default is not executable, and the staged self-check of §4.3
// only means something after the chmod).
//
// Everything else in the archive is never written to disk ("其余条目一律不写
// 盘"). Fail-closed rejections, regardless of the entry name:
//   - path-shaped names: absolute paths, any '/' or '\' separator (子目录 /
//     traversal), '.'/'..' elements, and ':' (Windows drive/ADS metachar);
//   - non-regular entries: symlink, hardlink, device, fifo, directory;
//   - a second root entry carrying the target name (duplicate);
//   - a single member larger than extractMaxBytes.
//
// Zip slip is impossible by construction: the only name ever written is the
// literal constant target, so the output path is always
// filepath.Join(dirOfArchive, "sshmgr[.exe]") — no archive-controlled
// segment ever reaches the filesystem path.
//
// Invariants (spec §4.2(3)):
//   - any failure leaves the directory with zero new files: an output file
//     created by this call is removed again; a pre-existing file that made
//     O_EXCL collide is not ours to delete and is left untouched;
//   - an archive without a root entry of the target name is an error;
//   - the declared member size is checked for every member, and the actual
//     bytes copied for the extracted one are limit-capped, so an archive
//     lying about its sizes cannot overshoot the cap either way;
//   - on the tar.gz path the cumulative decompressed stream (headers, data,
//     skipped members) is bounded at maxTotalDecompressed (总解压字节上限).
func ExtractBinary(archivePath, goos string) (string, error) {
	target := binaryEntryName(goos)
	outPath := filepath.Join(filepath.Dir(archivePath), target)
	created, err := extractStreamed(archivePath, target, outPath)
	if err != nil {
		// Zero-residue invariant: 失败 ⇒ 目录零新文件. Only remove what
		// this call created; pre-existing files stay.
		if created {
			os.Remove(outPath)
		}
		return "", err
	}
	return outPath, nil
}

// extractStreamed walks the archive applying the per-entry checks and lands
// the target entry when found. It reports whether the output file exists
// because this call created it (so the caller can clean up on failure
// without ever touching pre-existing files). The archive format is detected
// from the file extension, mirroring the spec's --file rules (.zip, .tar.gz,
// .tgz); anything else is rejected instead of sniffed.
func extractStreamed(archivePath, target, outPath string) (created bool, err error) {
	lower := strings.ToLower(archivePath)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archivePath, target, outPath)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(archivePath, target, outPath)
	default:
		return false, fmt.Errorf("extract %s: unsupported archive format (want .zip, .tar.gz or .tgz)",
			filepath.Base(archivePath))
	}
}

// checkEntryName fails closed on any path-shaped archive entry name. The
// release pipeline emits flat archives, so a flat-names-only rule is both
// the conservative reading of spec §4.2(3) (拒绝绝对路径/`..`/子目录) and
// drift-proof: subdirectories, traversal, absolute paths, Windows separators
// and drive/ADS metacharacters mark a malformed-or-malicious archive —
// something to reject, not to navigate around.
func checkEntryName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("archive entry: empty name")
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("archive entry %q: path separators not allowed (flat archive only)", name)
	case name == "." || name == "..":
		return fmt.Errorf("archive entry %q: relative path element not allowed", name)
	case strings.ContainsRune(name, ':'):
		return fmt.Errorf("archive entry %q: ':' (Windows drive/ADS metacharacter) not allowed", name)
	}
	return nil
}

func extractZip(archivePath, target, outPath string) (created bool, err error) {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return false, fmt.Errorf("extract %s: %w", filepath.Base(archivePath), err)
	}
	defer zr.Close()
	found := false
	for _, zf := range zr.File {
		if err := checkEntryName(zf.Name); err != nil {
			return created, err
		}
		// Format limitation, documented (fail-open corner): zf.Mode() decodes
		// unix type bits only when the entry's CreatorVersion high byte marks
		// a unix creator; archives from unknown/other creators yield mode 0
		// and IsRegular()==true even for a symlink entry. This is not
		// exploitable here — a misread type never turns into a filesystem
		// operation (no os.Symlink/Chown on entry data; the only written
		// path is the literal constant sshmgr[.exe]) — and rejecting
		// unknown-creator archives would break legitimate heterogeneous
		// zips, so the residual risk is accepted as-is.
		if !zf.Mode().IsRegular() {
			return created, fmt.Errorf("archive entry %q: mode %s is not a regular file", zf.Name, zf.Mode())
		}
		if zf.UncompressedSize64 > uint64(extractMaxBytes) {
			return created, fmt.Errorf("archive entry %q: declared size %d exceeds %d byte member limit",
				zf.Name, zf.UncompressedSize64, extractMaxBytes)
		}
		if zf.Name != target {
			continue // 其余条目一律不写盘 — never even opened for decompression
		}
		if found {
			return created, fmt.Errorf("archive: duplicate root entry %q", target)
		}
		rc, err := zf.Open()
		if err != nil {
			return created, fmt.Errorf("archive entry %q: %w", zf.Name, err)
		}
		created, err = writeExtracted(outPath, rc)
		rc.Close()
		if err != nil {
			return created, err
		}
		found = true
	}
	if !found {
		return created, fmt.Errorf("archive %s: no root entry named %q", filepath.Base(archivePath), target)
	}
	return created, nil
}

// countingReader bounds decompression throughput (总解压字节上限). Once the
// cumulative count crosses maxTotalDecompressed it latches: every subsequent
// Read fails again, so a caller that swallows a single error (io.ReadAtLeast
// clears the error of a full read) cannot stream past the cap — the abort
// surfaces at the next short read at the latest.
type countingReader struct {
	r     io.Reader
	name  string // archive base name, for the error message
	total int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.total += int64(n)
	if c.total > maxTotalDecompressed {
		return n, fmt.Errorf("extract %s: cumulative decompressed bytes exceed %d byte limit",
			c.name, maxTotalDecompressed)
	}
	return n, err
}

func extractTarGz(archivePath, target, outPath string) (created bool, err error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return false, fmt.Errorf("extract %s: %w", filepath.Base(archivePath), err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return created, fmt.Errorf("extract %s: not a gzip stream: %w", filepath.Base(archivePath), err)
	}
	defer gz.Close()
	// Counting wrapper bounds cumulative decompressed bytes (headers, member
	// data, skipped members' drained payloads) at maxTotalDecompressed.
	tr := tar.NewReader(&countingReader{r: gz, name: filepath.Base(archivePath)})
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return created, fmt.Errorf("extract %s: read tar: %w", filepath.Base(archivePath), err)
		}
		if err := checkEntryName(hdr.Name); err != nil {
			return created, err
		}
		// Allow-list on the raw typeflag — deliberately NOT
		// hdr.FileInfo().Mode().IsRegular(): stdlib's headerFileInfo.Mode
		// has no case for TypeLink (hardlink, '1') and would misclassify
		// hardlinks as regular files. Only TypeReg and the legacy
		// TypeRegA ('\x00') are regular.
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return created, fmt.Errorf("archive entry %q: typeflag %q is not a regular file",
				hdr.Name, rune(hdr.Typeflag))
		}
		if hdr.Size > extractMaxBytes {
			return created, fmt.Errorf("archive entry %q: declared size %d exceeds %d byte member limit",
				hdr.Name, hdr.Size, extractMaxBytes)
		}
		if hdr.Name != target {
			continue // skipped members are drained by tar.Reader on Next, never written
		}
		if found {
			return created, fmt.Errorf("archive: duplicate root entry %q", target)
		}
		created, err = writeExtracted(outPath, tr)
		if err != nil {
			return created, err
		}
		found = true
	}
	if !found {
		return created, fmt.Errorf("archive %s: no root entry named %q", filepath.Base(archivePath), target)
	}
	return created, nil
}

// writeExtracted creates outPath (O_CREATE|O_EXCL|O_WRONLY) and streams r
// into it, chmod 0755 right after creation (on the open handle, before the
// first copied byte) and with the copy capped at extractMaxBytes+1 so an
// oversized member is detected from actual bytes, not just declared sizes.
// It reports created=true from the moment the file exists on disk —
// including when a later step fails — so the caller's zero-residue cleanup
// removes the partial output.
func writeExtracted(outPath string, r io.Reader) (created bool, err error) {
	f, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if err != nil {
		return false, fmt.Errorf("extract %s: %w", filepath.Base(outPath), err)
	}
	created = true
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return created, fmt.Errorf("extract %s: chmod 0755: %w", filepath.Base(outPath), err)
	}
	n, err := io.Copy(f, io.LimitReader(r, extractMaxBytes+1))
	if err != nil {
		f.Close()
		return created, fmt.Errorf("extract %s: %w", filepath.Base(outPath), err)
	}
	if n > extractMaxBytes {
		f.Close()
		return created, fmt.Errorf("extract %s: member exceeds %d byte limit (%d bytes)",
			filepath.Base(outPath), extractMaxBytes, n)
	}
	if err := f.Close(); err != nil {
		return created, fmt.Errorf("extract %s: %w", filepath.Base(outPath), err)
	}
	return created, nil
}
