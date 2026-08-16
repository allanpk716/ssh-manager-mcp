package conformance

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"

	"golang.org/x/crypto/ssh"
)

// TestDownloadDifferential proves the broker's Download reconstructs a remote
// tree byte-identical to the local original — the §13 differential for the
// download surface, mirroring TestUploadDifferential's suite MINUS the boundary
// item (that one is upload-cap-specific; this suite downloads with maxBytes=0,
// so the §6 cap dimension is out of scope here — it is unit-covered in
// internal/sshbroker).
//
// Reference path: `scp -r` puts the suite tree onto the conformance server
// (scp's upload fidelity is itself differential-proven by TestUploadDifferential).
// Broker path: the remote structure is discovered via real `ssh find` (the
// broker has no directory-list primitive — Download is single-file by design),
// each directory is recreated locally, and each file is pulled via
// sshbroker.Client.Download (unlimited) and written at its preserved relative
// path. The reconstructed local tree must equal the original fixture tree —
// structure AND per-file bytes — compared cross-platform via filepath.Walk +
// per-file os.ReadFile (the broker host is Windows in the primary dev loop; no
// `diff -r` dependency). Covers the same edges as the upload suite: 3-level
// nesting, empty dir, 0-byte file, unicode + space filenames, all-256-bytes
// binary.
func TestDownloadDifferential(t *testing.T) {
	requireConformance(t)
	if _, err := exec.LookPath("scp"); err != nil {
		t.Skipf("download-differential needs scp on PATH: %v", err)
	}
	privPath, pub := generateKey(t, "ed25519", "")
	host, port, hostKey, _, cleanup := startOpenSSH(t, OpenSSHOpts{AuthorizedPubKey: pub})
	defer cleanup()

	brokerAuth := mustPrivAuth(t, privPath, "")
	sshArgs := sshBinaryKeyAuthArgs(host, port, "sshuser", privPath)
	scpArgs := scpBinaryKeyAuthArgs(port, privPath)
	scpDst := "sshuser@" + host + ":"

	cli, err := sshbroker.Connect(context.Background(), host, port, "sshuser", brokerAuth, ssh.FixedHostKey(hostKey))
	if err != nil {
		t.Fatalf("broker connect: %v", err)
	}
	defer cli.Close()

	// Fixture: the same suite tree the upload differential uses.
	fixture := t.TempDir()
	writeDifferentialSuite(t, fixture)

	// Reference path: scp -r puts the tree onto the server.
	remoteSrc := "/home/sshuser/dl-src"
	scpPutDir(t, scpArgs, fixture, scpDst+remoteSrc)

	// Broker path: discover the remote structure via real ssh, then pull every
	// file via broker Download (unlimited — content fidelity, not the cap).
	dirs, files := sshFindTree(t, sshArgs, remoteSrc)
	if len(dirs) != 5 {
		t.Fatalf("remote suite has %d subdirs, want 5 (a, a/b, a/b/c, empty-dir, pkg) — did scp -r drop the empty dir? got %v", len(dirs), dirs)
	}
	if len(files) != 9 {
		t.Fatalf("remote suite has %d files, want 9 (got %v)", len(files), files)
	}
	mirror := t.TempDir()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(mirror, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		res, err := cli.Download(context.Background(), remoteSrc+"/"+f, 0)
		if err != nil {
			t.Fatalf("broker Download %s: %v", f, err)
		}
		if res.Truncated {
			t.Fatalf("Download %s Truncated=true under maxBytes=0", f)
		}
		if res.Bytes != int64(len(res.Content)) {
			t.Fatalf("Download %s Bytes=%d but len(Content)=%d — dishonest report under maxBytes=0", f, res.Bytes, len(res.Content))
		}
		p := filepath.Join(mirror, filepath.FromSlash(f))
		if err := os.WriteFile(p, []byte(res.Content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Differential: the reconstructed tree equals the original fixture tree.
	assertTreesEqual(t, fixture, mirror)
}

// sshFindTree lists the remote tree under root via real ssh (busybox find on
// the alpine conformance container): returns (dirs, files) as root-relative
// slash paths. The root itself is excluded (the local mirror root pre-exists).
// busybox find prints one unquoted path per line, so space + unicode names
// round-trip through plain line splitting.
func sshFindTree(t *testing.T, sshArgs []string, root string) (dirs, files []string) {
	t.Helper()
	run := func(filter string) []string {
		cmd := fmt.Sprintf("find '%s' %s", root, filter)
		out, _, code := runSSHBinary(t, append(append([]string{}, sshArgs...), cmd)...)
		if code != 0 {
			t.Fatalf("ssh find %s (code=%d):\n%s", filter, code, out)
		}
		var rel []string
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			if line == root { // the find root itself — excluded
				continue
			}
			r := strings.TrimPrefix(line, root+"/")
			if r == line {
				t.Fatalf("find output outside root %q: %q", root, line)
			}
			rel = append(rel, r)
		}
		return rel
	}
	return run("-type d"), run("-type f")
}

// assertTreesEqual fails the test unless the two local directory trees contain
// the identical set of relative paths (dirs AND files — empty dirs included)
// with byte-identical file contents. Cross-platform `diff -r` replacement
// (filepath.Walk + per-file os.ReadFile — no diff dependency on the broker host).
func assertTreesEqual(t *testing.T, want, got string) {
	t.Helper()
	collect := func(root string) map[string]bool { // rel slash path → isDir
		m := map[string]bool{}
		err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			m[filepath.ToSlash(rel)] = info.IsDir()
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
		return m
	}
	wantTree, gotTree := collect(want), collect(got)
	for rel, wantDir := range wantTree {
		gotDir, ok := gotTree[rel]
		if !ok {
			t.Errorf("reconstructed tree is missing %q (dir=%v)", rel, wantDir)
			continue
		}
		if gotDir != wantDir {
			t.Errorf("%q: kind mismatch — want dir=%v, got dir=%v", rel, wantDir, gotDir)
			continue
		}
		if wantDir {
			continue
		}
		wb, err := os.ReadFile(filepath.Join(want, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		gb, err := os.ReadFile(filepath.Join(got, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(wb, gb) {
			t.Errorf("%q: content differs (want %d bytes, got %d bytes)", rel, len(wb), len(gb))
		}
	}
	for rel := range gotTree {
		if _, ok := wantTree[rel]; !ok {
			t.Errorf("reconstructed tree has extra entry %q", rel)
		}
	}
}
