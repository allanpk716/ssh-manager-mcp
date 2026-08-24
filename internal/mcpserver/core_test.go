package mcpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
	"ssh-manager-mcp/internal/vault"
)

// hostPortRe matches an addr host:port form (digit-dotted v4 or bracketed v6
// + port) — the no-leak assertion net's detector (Plan 31 pattern).
var hostPortRe = regexp.MustCompile(`[0-9]{1,3}(\.[0-9]{1,3}){3}:[0-9]+|\[[0-9a-fA-F:]+\]:[0-9]+`)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	return newStoreAt(t, t.TempDir()+"/t.db")
}

// newStoreAt is newStore at an explicit db path — needed by tests that open a
// second raw connection to the same file (deleteServerRowRaw).
func newStoreAt(t *testing.T, path string) *store.Store {
	t.Helper()
	mk, _ := store.GenerateMasterKey()
	st, err := store.Open(path, mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestClampExecTimeout verifies the pure helper that applies the default (when
// t <= 0) and the MaxExecTimeout ceiling. No server, no waiting — the cap is
// exercised by composition with the broker's timeout-enforcement path (proven
// in Task 1's Exec timeout test), not by running a 5-minute command here.
func TestClampExecTimeout(t *testing.T) {
	cases := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero defaults to defaultTimeout", 0, defaultTimeout},
		{"negative defaults to defaultTimeout", -1, defaultTimeout},
		{"under cap unchanged", 60 * time.Second, 60 * time.Second},
		{"over cap clamped", time.Hour, MaxExecTimeout},
		{"at cap unchanged (boundary)", MaxExecTimeout, MaxExecTimeout},
		{"just over cap clamped", MaxExecTimeout + time.Second, MaxExecTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampExecTimeout(c.in); got != c.want {
				t.Fatalf("clampExecTimeout(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestListServersScopedToProfile(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	b, _ := st.AddServer(&models.Server{Name: "b", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a}) // only a in profile

	got, err := ListServersForProfile(st, pid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("want only [a], got %+v", got)
	}
	_ = b
}

func TestExecCommandRejectsOutOfProfile(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	b, _ := st.AddServer(&models.Server{Name: "b", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a})

	const projectID = "proj-test"
	_, err := ExecCommandForProfile(context.Background(), st, projectID, pid, b, "echo hi", false, time.Second)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("want ErrNotInProfile, got %v", err)
	}

	// Fix 1 lock: the denied branch must produce an audited row attributed to projectID.
	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var denied store.AuditRow
	found := false
	for _, r := range rows {
		if r.Action == "exec" && r.Status == "denied" && r.ServerID == b && r.ProjectID == projectID {
			denied = r
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no denied audit row for project=%s server=%s; rows=%+v", projectID, b, rows)
	}
	if denied.Command != "echo hi" {
		t.Fatalf("denied audit command = %q, want %q", denied.Command, "echo hi")
	}
}

// TestExecCommandNoCredential (Plan 20 C0): exec against a credential-less
// server is refused with the typed sentinel and audited as status
// "no_credential" (not auth_error) — the agent gets the configure-a-credential
// hint, and no connect is attempted.
func TestExecCommandNoCredential(t *testing.T) {
	st := newStore(t)
	srvID, err := st.AddServer(&models.Server{Name: "bare", Host: "192.0.2.7", Port: 22, User: "u"})
	if err != nil {
		t.Fatalf("credential-less AddServer: %v", err)
	}
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	const projectID = "proj-test"
	_, err = ExecCommandForProfile(context.Background(), st, projectID, pid, srvID, "echo hi", false, time.Second)
	if !errors.Is(err, vault.ErrNoCredential) {
		t.Fatalf("want vault.ErrNoCredential, got %v", err)
	}
	if !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("err = %q, want it to mention \"no credential\"", err.Error())
	}

	// audited with the dedicated status (mirrors the no_sudo pattern)
	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Action == "exec" && r.ServerID == srvID && r.ProjectID == projectID && r.Status == "no_credential" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no no_credential audit row for server=%s; rows=%+v", srvID, rows)
	}
}

func TestExecCommandRunsInProfileServer(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "RAN:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	out, err := ExecCommandForProfile(context.Background(), st, "proj-test", pid, srvID, "hello", false, 5*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out.Stdout != "RAN:hello\n" {
		t.Fatalf("stdout = %q", out.Stdout)
	}
}

func TestExecCommandSudoWired(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			if cmd == "whoami" {
				return "root\n", "", 0
			}
			return "", "unknown", 1
		},
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "sudopw")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	out, err := ExecCommandForProfile(context.Background(), st, "proj-test", pid, srvID, "whoami", true, 5*time.Second)
	if err != nil {
		t.Fatalf("sudo exec: %v", err)
	}
	if out.Stdout != "root\n" {
		t.Fatalf("stdout = %q, want root", out.Stdout)
	}
}

func TestExecCommandTruncatesLargeOutput(t *testing.T) {
	big := strings.Repeat("x", int(MaxOutputBytes)*2) // 2 MiB — well over the 1 MiB cap
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return big, "", 0 },
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	out, err := ExecCommandForProfile(context.Background(), st, "proj-test", pid, srvID, "big", false, 5*time.Second)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !out.Truncated {
		t.Fatal("want ExecOutput.Truncated=true (stdout exceeded the cap)")
	}
	if int64(len(out.Stdout)) != MaxOutputBytes {
		t.Fatalf("stdout len=%d want %d (the cap)", len(out.Stdout), MaxOutputBytes)
	}
	if out.StdoutBytes != int64(len(big)) {
		t.Fatalf("stdout_bytes=%d want %d (true total)", out.StdoutBytes, len(big))
	}
}

// TestListServersHostMasking: ExposeHost=false (default) projects Host as the
// literal "hidden"; ExposeHost=true projects the plaintext host. This is the
// v0.9 breaking change itself — v0.8 always returned plaintext (spec §3).
func TestListServersHostMasking(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "masked", Host: "10.0.0.1", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	b, _ := st.AddServer(&models.Server{Name: "exposed", Host: "10.0.0.2", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st), ExposeHost: true})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a, b})

	got, err := ListServersForProfile(st, pid)
	if err != nil {
		t.Fatal(err)
	}
	hosts := map[string]string{}
	for _, s := range got {
		hosts[s.Name] = s.Host
	}
	if hosts["masked"] != "hidden" {
		t.Fatalf("masked server Host = %q, want \"hidden\"", hosts["masked"])
	}
	if hosts["exposed"] != "10.0.0.2" {
		t.Fatalf("exposed server Host = %q, want plaintext", hosts["exposed"])
	}
}

// helpers
func mustCred(t *testing.T, st *store.Store) string {
	t.Helper()
	id, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	return id
}
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return len(s)
}
func portOfAddr(addr string) int {
	i := indexByte(addr, ':')
	var p int
	for _, r := range addr[i+1:] {
		p = p*10 + int(r-'0')
	}
	return p
}

// seedRealServer creates a server pointing at the testsshd addr, pre-trusts its host key,
// and (if sudoPw != "") attaches a sudo password credential.
func seedRealServer(t *testing.T, st *store.Store, name, addr string, hk ssh.PublicKey, sudoPw string) string {
	t.Helper()
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srv := &models.Server{
		Name: name, Host: addr[:indexByte(addr, ':')], Port: portOfAddr(addr),
		User: "u", AuthMethod: models.AuthPassword, CredentialID: cid,
	}
	if sudoPw != "" {
		sid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(sudoPw)})
		srv.SudoCredentialID = sid
	}
	id, _ := st.AddServer(srv)
	_ = st.SaveHostKey(srv.Host, srv.Port, hk.Marshal()) // pre-trust the testsshd host key
	return id
}

// TestDownloadForProfileDownloadsInProfileServer verifies the in-profile happy path:
// the fixture content round-trips through the SFTP download (no truncation on a
// sub-cap file) and Bytes reports the true size.
func TestDownloadForProfileDownloadsInProfileServer(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	// The in-process testsshd's sftp subsystem serves the host filesystem, so a
	// fixture written under t.TempDir() is readable by Download. (We can't seed
	// the file via broker Exec: testsshd's Exec is a callback, not a real shell.)
	const want = "hello-sftp\nline2\nlast line marker\n"
	remote := filepath.Join(t.TempDir(), "dl.bin")
	if err := os.WriteFile(remote, []byte(want), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	out, err := DownloadForProfile(context.Background(), st, "proj-test", pid, srvID, remote)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if out.Content != want {
		t.Fatalf("content = %q, want %q", out.Content, want)
	}
	if out.Bytes != int64(len(want)) {
		t.Fatalf("Bytes=%d, want %d", out.Bytes, len(want))
	}
	if out.Truncated {
		t.Fatal("Truncated=true on a sub-cap file; want false")
	}
}

// TestDownloadForProfileRejectsOutOfProfile verifies the iron rule: an
// out-of-profile server_id is rejected with ErrNotInProfile AND audited with
// Action="download" Status="denied" attributed to the agent's projectID. The
// path is recorded in the audit Command field (the brief reuses it for the path).
func TestDownloadForProfileRejectsOutOfProfile(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	b, _ := st.AddServer(&models.Server{Name: "b", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a}) // only a in profile

	const projectID = "proj-test"
	const path = "/etc/passwd"
	_, err := DownloadForProfile(context.Background(), st, projectID, pid, b, path)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("want ErrNotInProfile, got %v", err)
	}

	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var denied store.AuditRow
	found := false
	for _, r := range rows {
		if r.Action == "download" && r.Status == "denied" && r.ServerID == b && r.ProjectID == projectID {
			denied = r
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no denied download audit row for project=%s server=%s; rows=%+v", projectID, b, rows)
	}
	if denied.Command != path {
		t.Fatalf("denied audit command (path) = %q, want %q", denied.Command, path)
	}
}

// TestDownloadForProfileNoCredential (Plan 21 A1): download against a
// credential-less server is refused with the typed sentinel AND audited as
// status "no_credential" (not auth_error) — the same unification exec got in
// Plan 20 C0, so the agent never misreads a missing credential as a wrong
// password. No connect is attempted.
func TestDownloadForProfileNoCredential(t *testing.T) {
	st := newStore(t)
	srvID, err := st.AddServer(&models.Server{Name: "bare", Host: "192.0.2.7", Port: 22, User: "u"})
	if err != nil {
		t.Fatalf("credential-less AddServer: %v", err)
	}
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	const projectID = "proj-test"
	_, err = DownloadForProfile(context.Background(), st, projectID, pid, srvID, "/tmp/x")
	if !errors.Is(err, vault.ErrNoCredential) {
		t.Fatalf("want vault.ErrNoCredential, got %v", err)
	}
	if !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("err = %q, want it to mention \"no credential\"", err.Error())
	}

	// audited with the dedicated status (mirrors the exec no_credential test)
	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Action == "download" && r.ServerID == srvID && r.ProjectID == projectID && r.Status == "no_credential" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no no_credential download audit row for server=%s; rows=%+v", srvID, rows)
	}
}

// TestDownloadForProfileTruncatesLargeFile verifies the §6 cap: a file larger
// than MaxOutputBytes yields Truncated=true, Content is the prefix (exactly the
// cap), and Bytes reports the true total size.
func TestDownloadForProfileTruncatesLargeFile(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	big := strings.Repeat("x", int(MaxOutputBytes)*2) // 2 MiB — well over the 1 MiB cap
	remote := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(remote, []byte(big), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}

	out, err := DownloadForProfile(context.Background(), st, "proj-test", pid, srvID, remote)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !out.Truncated {
		t.Fatal("want DownloadOutput.Truncated=true (file exceeded the cap)")
	}
	if int64(len(out.Content)) != MaxOutputBytes {
		t.Fatalf("content len=%d want %d (the cap)", len(out.Content), MaxOutputBytes)
	}
	if out.Content != big[:MaxOutputBytes] {
		t.Fatal("content is not the prefix of the file")
	}
	if out.Bytes != int64(len(big)) {
		t.Fatalf("Bytes=%d want %d (true total)", out.Bytes, len(big))
	}
}

// toSlash is the filepath.ToSlash helper, used to build forward-slash remote
// paths in tests so UploadForProfile's path.Dir (POSIX) computes the parent
// correctly on a Windows broker host too (the in-process testsshd serves the
// host FS, which accepts both separators on Windows; a POSIX path is the native
// case for a real Linux remote).
func toSlash(p string) string { return filepath.ToSlash(p) }

// TestUploadForProfileUploadsInProfileServer verifies the in-profile happy path:
// an upload round-trips through the broker (upload via UploadForProfile, verify
// via DownloadForProfile — content matches), for both a single file and a
// recursive directory. It also exercises the MkdirAll-parent wiring (the T1
// carry): the single-file destination's parent does NOT pre-exist, yet the
// upload succeeds — UploadForProfile MkdirAll's the parent before the transfer.
func TestUploadForProfileUploadsInProfileServer(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	// --- single file, fresh parent (exercises MkdirAll-parent wiring) ---
	const want = "upload-payload\nline2\nlast marker\n"
	localFile := filepath.Join(t.TempDir(), "local.txt")
	if err := os.WriteFile(localFile, []byte(want), 0644); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	// Destination under a NON-EXISTENT parent ("freshdir") — T1's Client.Upload
	// alone would fail here (sftp.Create needs the parent); UploadForProfile's
	// MkdirAll-parent must create it first.
	remoteSingle := toSlash(filepath.Join(t.TempDir(), "freshdir", "up.txt"))
	out, err := UploadForProfile(context.Background(), st, "proj-test", pid, srvID, localFile, remoteSingle)
	if err != nil {
		t.Fatalf("upload single: %v", err)
	}
	if out.Files != 1 || out.Bytes != int64(len(want)) || out.Truncated {
		t.Fatalf("single result = %+v, want {Files:1 Bytes:%d Truncated:false}", out, len(want))
	}
	// Verify via Download — content round-trips.
	dl, err := DownloadForProfile(context.Background(), st, "proj-test", pid, srvID, remoteSingle)
	if err != nil {
		t.Fatalf("verify download: %v", err)
	}
	if dl.Content != want {
		t.Fatalf("round-trip content = %q, want %q", dl.Content, want)
	}

	// --- recursive directory ---
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, "a.txt"), []byte("file-a\n"), 0644); err != nil {
		t.Fatalf("setup a.txt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(localDir, "sub"), 0755); err != nil {
		t.Fatalf("setup sub: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "sub", "b.txt"), []byte("file-b\n"), 0644); err != nil {
		t.Fatalf("setup b.txt: %v", err)
	}
	remoteDir := toSlash(filepath.Join(t.TempDir(), "updir"))
	out, err = UploadForProfile(context.Background(), st, "proj-test", pid, srvID, localDir, remoteDir)
	if err != nil {
		t.Fatalf("upload dir: %v", err)
	}
	if out.Files != 2 { // a.txt + sub/b.txt
		t.Fatalf("dir Files=%d, want 2 (out=%+v)", out.Files, out)
	}
	if out.Truncated {
		t.Fatalf("dir Truncated=true, want false (out=%+v)", out)
	}
	// Verify both children landed at their preserved relative paths (forward
	// slashes — path.Dir/POSIX convention).
	for _, tc := range []struct{ rel, want string }{
		{"a.txt", "file-a\n"},
		{"sub/b.txt", "file-b\n"},
	} {
		p := remoteDir + "/" + tc.rel
		g, err := DownloadForProfile(context.Background(), st, "proj-test", pid, srvID, p)
		if err != nil {
			t.Fatalf("verify %s: %v", tc.rel, err)
		}
		if g.Content != tc.want {
			t.Fatalf("dir %s content = %q, want %q", tc.rel, g.Content, tc.want)
		}
	}
}

// TestUploadForProfileRejectsOutOfProfile verifies the iron rule: an
// out-of-profile server_id is rejected with ErrNotInProfile AND audited with
// Action="upload" Status="denied" attributed to the agent's projectID. The audit
// Command field records "localPath -> remotePath" (the brief reuses it for the
// transfer direction).
func TestUploadForProfileRejectsOutOfProfile(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	b, _ := st.AddServer(&models.Server{Name: "b", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a}) // only a in profile

	const projectID = "proj-test"
	const localPath = "/tmp/local.txt"
	const remotePath = "/tmp/remote.txt"
	_, err := UploadForProfile(context.Background(), st, projectID, pid, b, localPath, remotePath)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("want ErrNotInProfile, got %v", err)
	}

	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var denied store.AuditRow
	found := false
	for _, r := range rows {
		if r.Action == "upload" && r.Status == "denied" && r.ServerID == b && r.ProjectID == projectID {
			denied = r
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no denied upload audit row for project=%s server=%s; rows=%+v", projectID, b, rows)
	}
	wantCmd := localPath + " -> " + remotePath
	if denied.Command != wantCmd {
		t.Fatalf("denied audit command = %q, want %q", denied.Command, wantCmd)
	}
}

// TestUploadForProfileNoCredential (Plan 21 A1): upload against a
// credential-less server is refused with the typed sentinel AND audited as
// status "no_credential" (not auth_error) — same unification as exec/download.
func TestUploadForProfileNoCredential(t *testing.T) {
	st := newStore(t)
	srvID, err := st.AddServer(&models.Server{Name: "bare", Host: "192.0.2.7", Port: 22, User: "u"})
	if err != nil {
		t.Fatalf("credential-less AddServer: %v", err)
	}
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	const projectID = "proj-test"
	_, err = UploadForProfile(context.Background(), st, projectID, pid, srvID, "/tmp/local.txt", "/tmp/remote.txt")
	if !errors.Is(err, vault.ErrNoCredential) {
		t.Fatalf("want vault.ErrNoCredential, got %v", err)
	}
	if !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("err = %q, want it to mention \"no credential\"", err.Error())
	}

	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Action == "upload" && r.ServerID == srvID && r.ProjectID == projectID && r.Status == "no_credential" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no no_credential upload audit row for server=%s; rows=%+v", srvID, rows)
	}
}

// TestUploadForProfileRefusesOverCapFile (Plan 23 flip of the Plan 6 per-file-
// atomic case, which asserted an over-cap file uploads COMPLETE with
// Bytes > MaxOutputBytes): the §6 cap is now a hard per-file bound — a file
// STRICTLY larger than MaxOutputBytes is refused BEFORE transfer. The error
// names file/size/cap; files completed before the refusal REMAIN remotely
// (verified on the host FS — the in-process testsshd serves it); the refused
// file is absent remotely (zero bytes, not a 0-byte file); UploadOutput comes
// back zero; the audit row records status "error".
func TestUploadForProfileRefusesOverCapFile(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	// Small file sorts first (uploads complete); over-cap file sorts second
	// (refused pre-flight). 2 MiB > 1 MiB cap by a clear margin.
	small := strings.Repeat("s", int(MaxOutputBytes)/2) // 512 KiB
	big := strings.Repeat("x", int(MaxOutputBytes)*2)   // 2 MiB
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, "a-small.bin"), []byte(small), 0644); err != nil {
		t.Fatalf("setup a-small.bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "z-over.bin"), []byte(big), 0644); err != nil {
		t.Fatalf("setup z-over.bin: %v", err)
	}
	remoteDir := toSlash(filepath.Join(t.TempDir(), "up-refuse"))
	out, err := UploadForProfile(context.Background(), st, "proj-test", pid, srvID, localDir, remoteDir)
	if err == nil {
		t.Fatalf("upload: want over-cap refusal error, got nil (out=%+v)", out)
	}
	if !strings.Contains(err.Error(), "exceeds upload cap") || !strings.Contains(err.Error(), "z-over.bin") {
		t.Fatalf("refusal error must name file + cap, got %q", err.Error())
	}
	if want := fmt.Sprintf("(%d bytes) exceeds upload cap %d", int64(len(big)), MaxOutputBytes); !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal error must carry size+cap evidence %q, got %q", want, err.Error())
	}
	if out.Files != 0 || out.Bytes != 0 || out.Truncated {
		t.Fatalf("out = %+v, want zero-value on refusal (the retained small file is on the remote, not in the output)", out)
	}
	// Retention: the small file completed before the refusal and REMAINS.
	if fi, serr := os.Stat(filepath.Join(remoteDir, "a-small.bin")); serr != nil || fi.Size() != int64(len(small)) {
		t.Fatalf("small file must remain remotely (%d bytes): fi=%v err=%v", len(small), fi, serr)
	}
	// The refused file never landed — absent, not zero-byte.
	if _, serr := os.Stat(filepath.Join(remoteDir, "z-over.bin")); !os.IsNotExist(serr) {
		t.Fatalf("over-cap file must be absent remotely, stat err=%v", serr)
	}
	// The refusal path is audited as status "error".
	rows, rerr := st.AuditRows(5)
	if rerr != nil {
		t.Fatalf("read audit: %v", rerr)
	}
	found := false
	for _, r := range rows {
		if r.Action == "upload" && r.ServerID == srvID && r.Status == "error" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error upload audit row for server=%s; rows=%+v", srvID, rows)
	}
}

// TestUploadForProfileTruncatesCumulativeOverCap keeps the OTHER §6 layer at
// the MCP boundary (unchanged by Plan 23): every file within the per-file cap,
// but the cumulative total crosses it → Truncated=true, NO error, Bytes reports
// the honest total. Replaces the old two-2MiB-files fixture (whose first file
// is now refused pre-flight) with three 512 KiB files (each ≤ cap, 1.5 MiB
// total > 1 MiB cap).
func TestUploadForProfileTruncatesCumulativeOverCap(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	part := strings.Repeat("x", int(MaxOutputBytes)/2) // 512 KiB each
	localDir := t.TempDir()
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		if err := os.WriteFile(filepath.Join(localDir, n), []byte(part), 0644); err != nil {
			t.Fatalf("setup %s: %v", n, err)
		}
	}
	remoteDir := toSlash(filepath.Join(t.TempDir(), "up-cum"))
	out, err := UploadForProfile(context.Background(), st, "proj-test", pid, srvID, localDir, remoteDir)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !out.Truncated {
		t.Fatal("want UploadOutput.Truncated=true (cumulative total exceeded the cap)")
	}
	if out.Bytes != int64(3*len(part)) {
		t.Fatalf("Bytes=%d, want %d (honest total: all three files within the per-file cap landed)", out.Bytes, 3*len(part))
	}
	if out.Files != 3 {
		t.Fatalf("Files=%d, want 3 (each file ≤ cap; the tripwire file lands complete per-file-atomic)", out.Files)
	}
}

// startEchoListener opens a loopback TCP listener on a random port that echoes
// every byte read back to the writer. It is the "remote service" that the
// forward_port tunnel targets: because the in-process testsshd shares the test's
// loopback, 127.0.0.1:<port> from the sshd's perspective IS this listener. The
// returned port is what ForwardForProfile asks the sshd to dial. The listener is
// wired to t.Cleanup so it always tears down with the test.
func startEchoListener(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed (test cleanup)
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	return portOfAddr(ln.Addr().String())
}

// TestForwardForProfileOpensInProfileServer verifies the in-profile happy path
// for the first STATEFUL broker op: ForwardForProfile connects a FRESH long-lived
// *sshbroker.Client for the tunnel (NOT defer-closed — the TunnelManager owns
// it), opens the -L forward, registers it with the manager, and returns
// {tunnel_id, local_port}. The tunnel is live (a byte round-trip works through
// the manager-held LocalAddr) and the manager's registry holds both the tunnel
// and the client. Audited with Action="forward" Status="ok".
func TestForwardForProfileOpensInProfileServer(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	echoPort := startEchoListener(t)
	mgr := NewTunnelManager()
	defer mgr.CloseAll()

	out, err := ForwardForProfile(context.Background(), st, "proj-test", pid, srvID, "127.0.0.1", echoPort, 0, mgr)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}
	if out.TunnelID == "" {
		t.Fatal("TunnelID empty")
	}
	if out.LocalPort <= 0 {
		t.Fatalf("LocalPort=%d, want > 0", out.LocalPort)
	}

	// The TunnelManager holds the tunnel + the long-lived client (stateful).
	mgr.mu.Lock()
	mt, ok := mgr.tunnels[out.TunnelID]
	mgr.mu.Unlock()
	if !ok {
		t.Fatal("tunnel not in manager registry after open")
	}
	if mt.client == nil {
		t.Fatal("managed client is nil")
	}

	// Byte round-trip through the manager-held tunnel — the forward is live.
	conn, err := net.DialTimeout("tcp", mt.tunnel.LocalAddr(), 3*time.Second)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read echo through tunnel: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q, want %q", buf, "ping")
	}

	// Audit: Action="forward" Status="ok" attributed to projectID + serverID.
	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Action == "forward" && r.Status == "ok" && r.ServerID == srvID && r.ProjectID == "proj-test" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no ok forward audit row for project=%s server=%s; rows=%+v", "proj-test", srvID, rows)
	}
}

// TestForwardForProfileRejectsOutOfProfile verifies the iron rule on forward_port:
// an out-of-profile server_id is rejected with ErrNotInProfile AND audited with
// Action="forward" Status="denied" attributed to projectID. The audit Command
// field records the forward target (remoteHost:remotePort).
func TestForwardForProfileRejectsOutOfProfile(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "a", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	b, _ := st.AddServer(&models.Server{Name: "b", Host: "h", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a}) // only a in profile

	mgr := NewTunnelManager()
	defer mgr.CloseAll()

	const projectID = "proj-test"
	_, err := ForwardForProfile(context.Background(), st, projectID, pid, b, "127.0.0.1", 8080, 0, mgr)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("want ErrNotInProfile, got %v", err)
	}

	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	var denied store.AuditRow
	found := false
	for _, r := range rows {
		if r.Action == "forward" && r.Status == "denied" && r.ServerID == b && r.ProjectID == projectID {
			denied = r
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no denied forward audit row for project=%s server=%s; rows=%+v", projectID, b, rows)
	}
	if denied.Command != "127.0.0.1:8080" {
		t.Fatalf("denied audit command = %q, want %q", denied.Command, "127.0.0.1:8080")
	}
}

// TestForwardForProfileNoCredential (Plan 21 A1): forward against a
// credential-less server is refused with the typed sentinel AND audited as
// status "no_credential" (not auth_error) — same unification as exec/download/
// upload. The manager never receives a client (refusal precedes any connect).
func TestForwardForProfileNoCredential(t *testing.T) {
	st := newStore(t)
	srvID, err := st.AddServer(&models.Server{Name: "bare", Host: "192.0.2.7", Port: 22, User: "u"})
	if err != nil {
		t.Fatalf("credential-less AddServer: %v", err)
	}
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	mgr := NewTunnelManager()
	defer mgr.CloseAll()

	const projectID = "proj-test"
	_, err = ForwardForProfile(context.Background(), st, projectID, pid, srvID, "127.0.0.1", 8080, 0, mgr)
	if !errors.Is(err, vault.ErrNoCredential) {
		t.Fatalf("want vault.ErrNoCredential, got %v", err)
	}
	if !strings.Contains(err.Error(), "no credential") {
		t.Fatalf("err = %q, want it to mention \"no credential\"", err.Error())
	}

	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Action == "forward" && r.ServerID == srvID && r.ProjectID == projectID && r.Status == "no_credential" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no no_credential forward audit row for server=%s; rows=%+v", srvID, rows)
	}
}

// TestCloseForwardTearsDown verifies close_port's resource cleanup — the
// load-bearing concern for the first stateful broker op. After
// CloseForwardForProfile: (1) the tunnel is gone from the registry, AND (2) the
// long-lived *sshbroker.Client is actually closed (an Exec on it must fail — no
// leak). The deferred CloseAll is a safety net; the explicit close_port is the
// agent-driven teardown path.
func TestCloseForwardTearsDown(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	echoPort := startEchoListener(t)
	mgr := NewTunnelManager()
	defer mgr.CloseAll()

	out, err := ForwardForProfile(context.Background(), st, "proj-test", pid, srvID, "127.0.0.1", echoPort, 0, mgr)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}

	// Capture the long-lived client ref BEFORE close (same package — reach into
	// the registry). After Close, this client must be unusable.
	mgr.mu.Lock()
	cli := mgr.tunnels[out.TunnelID].client
	mgr.mu.Unlock()

	if err := CloseForwardForProfile(context.Background(), st, "proj-test", out.TunnelID, mgr); err != nil {
		t.Fatalf("close-forward: %v", err)
	}

	// (1) Registry is empty.
	mgr.mu.Lock()
	_, stillThere := mgr.tunnels[out.TunnelID]
	mgr.mu.Unlock()
	if stillThere {
		t.Fatal("tunnel still in registry after close (registry leak)")
	}

	// (2) The long-lived ssh.Client is actually closed — an op on it must error.
	//     (*sshbroker.Client).Exec opens a session on the ssh.Client; on a closed
	//     client the session-open fails immediately.
	if _, err := cli.Exec(context.Background(), "anything", time.Second, 64); err == nil {
		t.Fatal("ssh.Client still usable after close — mgr.Close did NOT close the owning client (resource leak)")
	}

	// Audit: Action="close-forward" Status="ok".
	rows, err := st.AuditRows(10)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Action == "close-forward" && r.Status == "ok" && r.ProjectID == "proj-test" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no ok close-forward audit row; rows=%+v", rows)
	}
}

// TestCloseForwardNotFound verifies close_port on an unknown tunnel id: the
// manager returns false (nothing to close), the call errors, and the audit row
// records Action="close-forward" Status="error".
func TestCloseForwardNotFound(t *testing.T) {
	st := newStore(t)
	mgr := NewTunnelManager()
	defer mgr.CloseAll()

	err := CloseForwardForProfile(context.Background(), st, "proj-test", "no-such-tunnel", mgr)
	if err == nil {
		t.Fatal("close unknown tunnel must error")
	}

	rows, err := st.AuditRows(5)
	if err != nil {
		t.Fatalf("read audit: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Action == "close-forward" && r.Status == "error" && r.Command == "no-such-tunnel" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no error close-forward audit row; rows=%+v", rows)
	}
}

// TestTunnelManagerSweepIdleReapsStaleTunnels verifies the tunnel sweeper's
// resource cleanup directly: a managed tunnel whose lastActivity is older than
// forwardIdleTimeout is reaped by SweepIdle (tunnel + client closed + removed
// from the registry). This is the background safety-net for tunnels the agent
// forgot to close_port.
func TestTunnelManagerSweepIdleReapsStaleTunnels(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	echoPort := startEchoListener(t)
	mgr := NewTunnelManager()
	defer mgr.CloseAll()

	out, err := ForwardForProfile(context.Background(), st, "proj-test", pid, srvID, "127.0.0.1", echoPort, 0, mgr)
	if err != nil {
		t.Fatalf("forward: %v", err)
	}

	// Force the tunnel's lastActivity into the past (beyond the idle timeout) and
	// capture the client ref to verify SweepIdle closed it.
	mgr.mu.Lock()
	cli := mgr.tunnels[out.TunnelID].client
	mgr.tunnels[out.TunnelID].lastActivity = time.Now().Add(-(forwardIdleTimeout + time.Second))
	mgr.mu.Unlock()

	closed := mgr.SweepIdle()
	if len(closed) != 1 || closed[0] != out.TunnelID {
		t.Fatalf("SweepIdle closed=%v, want [%s]", closed, out.TunnelID)
	}

	mgr.mu.Lock()
	_, stillThere := mgr.tunnels[out.TunnelID]
	mgr.mu.Unlock()
	if stillThere {
		t.Fatal("stale tunnel still in registry after SweepIdle")
	}
	if _, err := cli.Exec(context.Background(), "anything", time.Second, 64); err == nil {
		t.Fatal("ssh.Client still usable after SweepIdle (resource leak)")
	}
}

// TestForwardRejectsMaskedLiteral: remoteHost == "hidden" (any case) is the
// one channel where the masked literal could be "used" — a malicious
// server-side resolver record for "hidden" would capture mistyped traffic.
// DNS is case-insensitive, so the guard must be too (spec §3).
func TestForwardRejectsMaskedLiteral(t *testing.T) {
	st := newStore(t)
	a, _ := st.AddServer(&models.Server{Name: "s", Host: "10.0.0.1", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{a})
	mgr := NewTunnelManager()

	for _, rh := range []string{"hidden", "Hidden", "HIDDEN", "hidden.", " Hidden"} {
		_, err := ForwardForProfile(context.Background(), st, "proj", pid, a, rh, 8080, 0, mgr)
		if err == nil {
			t.Fatalf("remoteHost %q must be rejected", rh)
		}
		if !strings.Contains(err.Error(), "hidden") {
			t.Fatalf("error should name the masked literal: %v", err)
		}
	}
	if len(mgr.tunnels) != 0 {
		t.Fatal("no tunnel should be registered for a rejected forward")
	}
}

// ---- Plan 31 Task 6: MCP error-branch regression net (spec §6) ----

// leakAddrRe is the address-shape half of the regression net. The host
// substring alone is blind for hostname servers (the leak would be the
// RESOLVED ip); the regex alone would circularly share the detector with the
// redaction code under test. Both, independently. (No "lookup" term here on
// purpose: the DEGRADED DNS phrase legitimately contains the word "lookup" —
// raw lookup forms are pinned by Task 5's sshbroker goldens.)
var leakAddrRe = regexp.MustCompile(`(\b\d{1,3}(?:\.\d{1,3}){3}\b|\[[0-9a-fA-F:.]+\]|::)`)

// assertNoLeak is the regression net's shared check — the error text must
// contain neither the vault host substring NOR any address-shape literal.
func assertNoLeak(t *testing.T, err error, host string) {
	t.Helper()
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), host) {
		t.Fatalf("error text leaks host %q: %q", host, err.Error())
	}
	if leakAddrRe.MatchString(err.Error()) {
		t.Fatalf("error text leaks an address shape: %q", err.Error())
	}
}

// assertBranch pins WHICH branch fired before the no-leak check. A fixture
// that silently degrades onto an earlier branch (e.g. denied — its text is
// clean too) would otherwise pass assertNoLeak vacuously, so each call site
// names the branch-signature substring its error must carry.
func assertBranch(t *testing.T, err error, wantSub string) {
	t.Helper()
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), wantSub) {
		t.Fatalf("error %q does not reach its intended branch (want substring %q)", err.Error(), wantSub)
	}
}

// deleteServerRowRaw removes ONLY the servers row through a second raw
// connection. The schema's ON DELETE CASCADE on profile_servers does NOT fire
// here: foreign_keys is a per-connection pragma and this handle leaves it at
// the SQLite default (OFF). The id stays granted with no server row — the
// real-world race the "server %s not found" branch defends against (the row
// vanishing between the profile gate and GetServer). DeleteServer /
// DeleteServerCascading cannot build this fixture: both DELETE FROM servers
// under FK enforcement, which cascades the grant away and degrades the case
// onto the denied branch.
func deleteServerRowRaw(t *testing.T, dbPath, id string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM servers WHERE id=?`, id); err != nil {
		t.Fatalf("raw servers-row delete: %v", err)
	}
}

// TestErrorBranchesNeverLeakHost: every reachable error branch of the five
// *ForProfile operations must return text free of the vault host / address
// shapes (spec §6 — the structural form of the "connect_error etc." promise).
// Branches covered: denied (all five ops, incl. upload_content — Plan 33),
// server-not-found, no_credential, no_sudo, connect_error (real dial, exec +
// forward), the forward hidden guard (Task 4), and close-forward not-found.
func TestErrorBranchesNeverLeakHost(t *testing.T) {
	const vh = "vault.example.internal"
	dir := t.TempDir()
	st := newStoreAt(t, dir+"/t.db") // explicit path: the not-found fixture raw-deletes a row in-place
	granted, _ := st.AddServer(&models.Server{Name: "g", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	nocred, _ := st.AddServer(&models.Server{Name: "n", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword}) // credential-less
	unreach, _ := st.AddServer(&models.Server{Name: "u", Host: "127.0.0.1", Port: 1, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{granted, nocred, unreach})
	mgr := NewTunnelManager()

	// denied (all five ops, out-of-profile id) — the gate fires before any
	// server lookup, so the vault host is never even read.
	_, err := ExecCommandForProfile(context.Background(), st, "proj", pid, "bogus-id", "true", false, time.Second)
	assertBranch(t, err, "not in your profile")
	assertNoLeak(t, err, vh)
	_, err = DownloadForProfile(context.Background(), st, "proj", pid, "bogus-id", "/x")
	assertBranch(t, err, "not in your profile")
	assertNoLeak(t, err, vh)
	_, err = UploadForProfile(context.Background(), st, "proj", pid, "bogus-id", "/x", "/y")
	assertBranch(t, err, "not in your profile")
	assertNoLeak(t, err, vh)
	_, err = UploadContentForProfile(context.Background(), st, "proj", pid, "bogus-id", "data", "/x", "", 1<<20)
	assertBranch(t, err, "not in your profile")
	assertNoLeak(t, err, vh)
	_, err = ForwardForProfile(context.Background(), st, "proj", pid, "bogus-id", "127.0.0.1", 80, 0, mgr)
	assertBranch(t, err, "not in your profile")
	assertNoLeak(t, err, vh)

	// not found (granted id whose server row vanished — see deleteServerRowRaw
	// for why the public delete APIs cannot build this fixture)
	gone, _ := st.AddServer(&models.Server{Name: "gone", Host: vh, Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st)})
	_ = st.GrantServers(pid, []string{gone})
	deleteServerRowRaw(t, dir+"/t.db", gone)
	_, err = ExecCommandForProfile(context.Background(), st, "proj", pid, gone, "true", false, time.Second)
	assertBranch(t, err, "not found")
	assertNoLeak(t, err, vh)

	// no_credential (refused pre-connect, so vh is never dialed)
	_, err = ExecCommandForProfile(context.Background(), st, "proj", pid, nocred, "true", false, time.Second)
	assertBranch(t, err, "no credential")
	assertNoLeak(t, err, vh)

	// no_sudo (exec only). The branch fires AFTER a successful connect (the
	// sudo lookup is post-connect in core.go), so the fixture must be a real
	// reachable testsshd without a sudo credential — pointing it at vh would
	// degrade onto connect_error and never reach the branch under test.
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	nosudo := seedRealServer(t, st, "nosudo", addr, hk, "") // no sudo credential attached
	_ = st.GrantServers(pid, []string{nosudo})
	_, err = ExecCommandForProfile(context.Background(), st, "proj", pid, nosudo, "true", true, 5*time.Second)
	assertBranch(t, err, "sudo not configured")
	assertNoLeak(t, err, "127.0.0.1")

	// connect_error — real dial to a refused port; the host is an IP here so
	// BOTH checks bite (substring + shape). Task 5 washes the text at the
	// source (Connect returns redactAddr's output), so exec and forward both
	// surface one of the frozen "ssh dial: ..." phrases.
	_, err = ExecCommandForProfile(context.Background(), st, "proj", pid, unreach, "true", false, 2*time.Second)
	assertBranch(t, err, "ssh dial:")
	assertNoLeak(t, err, "127.0.0.1")
	_, err = ForwardForProfile(context.Background(), st, "proj", pid, unreach, "10.255.255.1", 65001, 0, mgr)
	assertBranch(t, err, "ssh dial:")
	assertNoLeak(t, err, "127.0.0.1")

	// forward hidden guard (Task 4) — fires pre-connect; the guard text names
	// the masked literal, never the vault host.
	_, err = ForwardForProfile(context.Background(), st, "proj", pid, granted, "hidden", 80, 0, mgr)
	assertBranch(t, err, "masked-host literal")
	assertNoLeak(t, err, vh)

	// close-forward not-found
	err = CloseForwardForProfile(context.Background(), st, "proj", "no-such-tunnel", mgr)
	assertBranch(t, err, "no open tunnel")
	assertNoLeak(t, err, vh)
}

// TestConnectErrorHostKeyMismatchNoLeak: the hostkey-mismatch branch surfaces
// THROUGH the connect error path (HostKeyTOFU's constructor never errors —
// core.go's standalone herr branch is dead code), so its text must be washed
// the same way. Pre-trust garbage bytes: TOFU compares marshaled key bytes,
// so any non-matching blob triggers the mismatch branch. The mismatch is
// genuinely triggered — testsshd performs a real handshake, and its real key
// differs from the garbage we stored.
func TestConnectErrorHostKeyMismatchNoLeak(t *testing.T) {
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srv := &models.Server{
		Name: "mismatch", Host: addr[:indexByte(addr, ':')], Port: portOfAddr(addr),
		User: "u", AuthMethod: models.AuthPassword, CredentialID: mustCred(t, st),
	}
	id, _ := st.AddServer(srv)
	_ = st.SaveHostKey(srv.Host, srv.Port, []byte("not-the-real-host-key"))
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{id})

	_, err := ExecCommandForProfile(context.Background(), st, "proj", pid, id, "true", false, 5*time.Second)
	if err == nil {
		t.Fatal("want hostkey mismatch error")
	}
	assertBranch(t, err, "host key mismatch")
	assertNoLeak(t, err, srv.Host)
}

// TestExecCommandEffectiveTimeoutEcho (Plan 32 T7 / spec §6 前台钳制改响):
// effective_timeout_seconds 恒存在 (无 omitempty) 且等于 clamp 后生效秒数
// ——0/缺省 → 120 (defaultTimeout)、400 → 300 (MaxExecTimeout 硬顶)、
// 90 → 90 (中值直通)。前台行为零变化, 唯一增量是响式回显。
func TestExecCommandEffectiveTimeoutEcho(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "ok\n", "", 0 },
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	cases := []struct {
		name string
		in   time.Duration
		want int
	}{
		{"zero defaults to 120", 0, 120},
		{"over cap clamped to 300", 400 * time.Second, 300},
		{"mid passthrough 90", 90 * time.Second, 90},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := ExecCommandForProfile(context.Background(), st, "proj-test", pid, srvID, "hi", false, c.in)
			if err != nil {
				t.Fatalf("exec: %v", err)
			}
			if out.EffectiveTimeoutSeconds != c.want {
				t.Fatalf("effective_timeout_seconds = %d, want %d", out.EffectiveTimeoutSeconds, c.want)
			}
			// 恒存在锚: 序列化文本必含该字段 (no omitempty 的回归钉)。
			b, jerr := json.Marshal(out)
			if jerr != nil {
				t.Fatal(jerr)
			}
			if !strings.Contains(string(b), `"effective_timeout_seconds":`) {
				t.Fatalf("serialized output missing constant field: %s", b)
			}
		})
	}
}

// TestResolveUploadContentCap pins the env seam's fail-closed contract (spec
// rev3 §3.1): unset → 8 MiB default; legal value passes verbatim; unparsable /
// non-positive / over the 1 GiB ceiling → error (a startup refusal, never a
// silent clamp).
func TestResolveUploadContentCap(t *testing.T) {
	cases := []struct {
		name    string
		env     string
		want    int64
		wantErr bool
	}{
		{"unset → 8 MiB default", "", 8 << 20, false},
		{"explicit legal value", "1048576", 1048576, false},
		{"exactly 1 GiB ceiling", "1073741824", 1073741824, false},
		{"one over the ceiling", "1073741825", 0, true},
		{"non-numeric", "8MiB", 0, true},
		{"zero", "0", 0, true},
		{"negative", "-5", 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SSHMGR_UPLOAD_CONTENT_MAX", c.env)
			got, err := resolveUploadContentCap()
			if c.wantErr {
				if err == nil {
					t.Fatalf("env=%q: want error, got cap=%d", c.env, got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("env=%q: got cap=%d err=%v, want %d nil", c.env, got, err, c.want)
			}
		})
	}
}

// ---- Plan 33: UploadContentForProfile unit battery (spec rev3 §1/§2/§5) ----

// ucSeed spins up testsshd + a profile-granted server, returning (st, pid, srvID, rootSlash).
// rootSlash is a slash-form temp root for remote targets (testsshd serves the host FS).
func ucSeed(t *testing.T) (*store.Store, string, string, string) {
	t.Helper()
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	t.Cleanup(cleanup)
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	return st, pid, srvID, toSlash(t.TempDir())
}

func TestUploadContentForProfileHappyPaths(t *testing.T) {
	st, pid, srvID, root := ucSeed(t)
	ctx := context.Background()

	// text: byte-exact UTF-8 landing + Bytes echo + audit ok row format.
	p1 := root + "/txt/conf.yaml"
	out, err := UploadContentForProfile(ctx, st, "proj-test", pid, srvID, "key: value\n", p1, "", 1<<20)
	if err != nil {
		t.Fatalf("text: %v", err)
	}
	if out.Bytes != int64(len("key: value\n")) {
		t.Fatalf("text Bytes = %d, want %d", out.Bytes, len("key: value\n"))
	}
	if got, _ := os.ReadFile(filepath.FromSlash(p1)); string(got) != "key: value\n" {
		t.Fatalf("text content = %q", got)
	}

	// base64: binary fixture (0x00/0xFF/GBK) lands byte-exact.
	bin := []byte{0x00, 0x01, 0xFF, 0xFE, 0xD6, 0xD0, 0x41, 0x7F} // D6 D0 = GBK "中"
	p2 := root + "/bin/blob.bin"
	out, err = UploadContentForProfile(ctx, st, "proj-test", pid, srvID, base64.StdEncoding.EncodeToString(bin), p2, "base64", 1<<20)
	if err != nil || out.Bytes != int64(len(bin)) {
		t.Fatalf("base64: err=%v out=%+v", err, out)
	}
	if got, _ := os.ReadFile(filepath.FromSlash(p2)); !bytes.Equal(got, bin) {
		t.Fatalf("base64 bytes = %x, want %x", got, bin)
	}

	// empty content (both encodings) → 0-byte file.
	p3 := root + "/empty.txt"
	if out, err = UploadContentForProfile(ctx, st, "proj-test", pid, srvID, "", p3, "", 16); err != nil || out.Bytes != 0 {
		t.Fatalf("empty text: err=%v out=%+v", err, out)
	}
	if fi, serr := os.Stat(filepath.FromSlash(p3)); serr != nil || fi.Size() != 0 {
		t.Fatalf("empty file: fi=%v err=%v", fi, serr)
	}
	if out, err = UploadContentForProfile(ctx, st, "proj-test", pid, srvID, "", p3, "base64", 16); err != nil || out.Bytes != 0 {
		t.Fatalf("empty base64: err=%v out=%+v", err, out)
	}

	// deep parent creation.
	p4 := root + "/a/b/c/d/e.txt"
	if _, err = UploadContentForProfile(ctx, st, "proj-test", pid, srvID, "deep", p4, "", 16); err != nil {
		t.Fatalf("deep: %v", err)
	}
	if got, _ := os.ReadFile(filepath.FromSlash(p4)); string(got) != "deep" {
		t.Fatalf("deep content = %q", got)
	}

	// decoded == cap boundary (spec rev3 §7): text and base64 must SUCCEED at
	// exactly cap. The base64 case is the padding anchor: 8 bytes → 12 chars
	// "AAAAAAAAAAA=", naive len/4*3 = 9 > 8 (would falsely refuse), est = 8.
	t8 := strings.Repeat("t", 8)
	if _, err = UploadContentForProfile(ctx, st, "proj-test", pid, srvID, t8, root+"/eq/text8.txt", "", 8); err != nil {
		t.Fatalf("text ==cap: %v", err)
	}
	b8 := base64.StdEncoding.EncodeToString(make([]byte, 8)) // "AAAAAAAAAAA="
	if _, err = UploadContentForProfile(ctx, st, "proj-test", pid, srvID, b8, root+"/eq/bin8.bin", "base64", 8); err != nil {
		t.Fatalf("base64 ==cap (padding anchor): %v", err)
	}

	// audit ok row: action + Command template with the decoded byte count.
	rows, _ := st.AuditRows(10)
	foundOK := false
	for _, r := range rows {
		if r.Action == "upload-content" && r.Status == "ok" && r.ProjectID == "proj-test" {
			if r.Command == "inline 11 bytes -> "+p1 { // "key: value\n" = 11 bytes
				foundOK = true
			}
		}
	}
	if !foundOK {
		t.Fatalf("no ok audit row with Command \"inline 11 bytes -> %s\"; rows=%+v", p1, rows)
	}
}

func TestUploadContentForProfileRefusals(t *testing.T) {
	st, pid, srvID, root := ucSeed(t)
	ctx := context.Background()

	// text over cap → refusal with size+cap evidence, ZERO remote file.
	p1 := root + "/ref/text.txt"
	_, err := UploadContentForProfile(ctx, st, "proj-test", pid, srvID, strings.Repeat("x", 9), p1, "", 8)
	if err == nil || !strings.Contains(err.Error(), "content (9 bytes) exceeds upload-content cap 8") {
		t.Fatalf("text over: err=%v", err)
	}
	if _, serr := os.Stat(filepath.FromSlash(p1)); !os.IsNotExist(serr) {
		t.Fatalf("text over: remote file must be absent, stat err=%v", serr)
	}

	// base64 over cap (coarse est) → "(9 bytes decoded)" + zero remote file.
	nine := base64.StdEncoding.EncodeToString(make([]byte, 9)) // 12 chars, est = 9
	p2 := root + "/ref/bin.bin"
	_, err = UploadContentForProfile(ctx, st, "proj-test", pid, srvID, nine, p2, "base64", 8)
	if err == nil || !strings.Contains(err.Error(), "content (9 bytes decoded) exceeds upload-content cap 8") {
		t.Fatalf("base64 over: err=%v", err)
	}
	if _, serr := os.Stat(filepath.FromSlash(p2)); !os.IsNotExist(serr) {
		t.Fatalf("base64 over: remote file must be absent, stat err=%v", serr)
	}

	// audit %d value table (spec rev3 §5): text-refusal row carries len(content),
	// base64 coarse-refusal row carries est.
	rows, _ := st.AuditRows(10)
	wantRows := map[string]bool{"inline 9 bytes -> " + p1: false, "inline 9 bytes -> " + p2: false}
	for _, r := range rows {
		if r.Action == "upload-content" && r.Status == "error" {
			if _, ok := wantRows[r.Command]; ok {
				wantRows[r.Command] = true
			}
		}
	}
	for cmd, seen := range wantRows {
		if !seen {
			t.Fatalf("missing error audit row %q; rows=%+v", cmd, rows)
		}
	}
}

func TestUploadContentForProfileParamValidation(t *testing.T) {
	st, pid, srvID, root := ucSeed(t)
	ctx := context.Background()

	// invalid encoding enum.
	if _, err := UploadContentForProfile(ctx, st, "proj-test", pid, srvID, "x", root+"/p/f", "hex", 8); err == nil || !strings.Contains(err.Error(), `encoding must be "text" or "base64"`) {
		t.Fatalf("encoding enum: err=%v", err)
	}
	// empty + relative remote_path.
	if _, err := UploadContentForProfile(ctx, st, "proj-test", pid, srvID, "x", "", "", 8); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("empty path: err=%v", err)
	}
	if _, err := UploadContentForProfile(ctx, st, "proj-test", pid, srvID, "x", "tmp/rel.txt", "", 8); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("relative path: err=%v", err)
	}
	// multiline base64 → single-line rejection.
	if _, err := UploadContentForProfile(ctx, st, "proj-test", pid, srvID, "QUJD\r\nREVG", root+"/p/ml", "base64", 8); err == nil || !strings.Contains(err.Error(), "single-line standard base64") {
		t.Fatalf("multiline: err=%v", err)
	}
	// invalid base64 (decoder error only — NEVER a content fragment).
	if _, err := UploadContentForProfile(ctx, st, "proj-test", pid, srvID, "QU!J", root+"/p/bad", "base64", 8); err == nil || !strings.Contains(err.Error(), "invalid base64 content") {
		t.Fatalf("invalid base64: err=%v", err)
	} else if strings.Contains(err.Error(), "QU!J") {
		t.Fatalf("invalid base64 error leaks content fragment: %q", err.Error())
	}
	// param error PRECEDES the gate: out-of-profile server + bad path → param
	// error, not denied (spec rev3 §2 ①).
	if _, err := UploadContentForProfile(ctx, st, "proj-test", pid, "not-granted", "x", "rel.txt", "", 8); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("param-before-gate: err=%v", err)
	}

	// audit rows for param failures carry %d = 0.
	rows, _ := st.AuditRows(10)
	for _, r := range rows {
		// Suffix is " -> rel.txt" (with the separator) so the relative-path case
		// ("tmp/rel.txt", also a 0-byte param-failure row) is not netted here.
		if r.Action == "upload-content" && r.Status == "error" && strings.HasSuffix(r.Command, " -> rel.txt") {
			if r.Command != "inline 0 bytes -> rel.txt" {
				t.Fatalf("param-failure audit Command = %q, want \"inline 0 bytes -> rel.txt\"", r.Command)
			}
		}
	}
}

func TestUploadContentForProfileDeniedAndAuditExclusion(t *testing.T) {
	st, pid, _, _ := ucSeed(t)
	ctx := context.Background()

	// denied: out-of-profile server id → ErrNotInProfile + denied audit row.
	_, err := UploadContentForProfile(ctx, st, "proj-test", pid, "not-granted", "data", "/tmp/x.txt", "", 8)
	if !errors.Is(err, ErrNotInProfile) {
		t.Fatalf("denied: err=%v", err)
	}
	rows, _ := st.AuditRows(5)
	found := false
	for _, r := range rows {
		if r.Action == "upload-content" && r.Status == "denied" && r.Command == "inline 0 bytes -> /tmp/x.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no denied row; rows=%+v", rows)
	}

	// CONTENT NEVER ENTERS THE AUDIT (secret-shaped payload, reverse assertion).
	st2, pid2, srv2, root2 := ucSeed(t)
	secret := "SUPERSECRETTOKEN-a1b2c3d4e5"
	if _, err := UploadContentForProfile(ctx, st2, "proj2", pid2, srv2, secret, root2+"/sec/token", "", 1<<20); err != nil {
		t.Fatalf("secret upload: %v", err)
	}
	rows2, _ := st2.AuditRows(5)
	for _, r := range rows2 {
		if strings.Contains(r.Command, secret) || strings.Contains(fmt.Sprint(r), secret) {
			t.Fatalf("audit leak: row=%+v", r)
		}
	}
}

func TestUploadContentForProfileNoLeakConnectError(t *testing.T) {
	// unreachable server → connect_error; the error text must carry no host:port
	// (Plan 31 no-leak net extension to this tool's branches, spec §5).
	// The dead server is seeded CREDENTIALED (a real testsshd host key, but
	// pointed at 127.0.0.1:1 — TCP-unreachable): a bare AddServer with no
	// credential would die at no_credential before any dial and never reach
	// connect_error.
	_, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	t.Cleanup(cleanup)
	st := newStore(t)
	srvID := seedRealServer(t, st, "dead", "127.0.0.1:1", hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	_, err := UploadContentForProfile(context.Background(), st, "proj-test", pid, srvID, "data", "/tmp/x", "", 8)
	if err == nil {
		t.Fatal("dead server: want error")
	}
	if hostPortRe.MatchString(err.Error()) {
		t.Fatalf("connect error leaks host:port: %q", err.Error())
	}
	rows, _ := st.AuditRows(5)
	found := false
	for _, r := range rows {
		if r.Action == "upload-content" && r.Status == "connect_error" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no connect_error row; rows=%+v", rows)
	}
}

// TestUploadContentForProfileNoLeakSFTPMidFailure (Plan 33 终审 fix wave, spec
// §5): the no-leak net must also cover the SFTP-layer MID-FAILURE path — a
// WriteFile failure AFTER a successful connect, not a connect error.
// Deterministic construction on both Windows and Linux (testsshd serves the
// host FS): plant a normal FILE `blocker` under the test root, then target
// <root>/blocker/sub/f.txt — the parent chain crosses that file, so the
// sc.MkdirAll inside WriteFile fails (not-a-directory class), a genuine
// SFTP-runtime failure on a live connection. The error text must carry no
// host:port shape and the audit must hold one error-status row.
func TestUploadContentForProfileNoLeakSFTPMidFailure(t *testing.T) {
	st, pid, srvID, root := ucSeed(t)

	blocker := root + "/blocker"
	if err := os.WriteFile(filepath.FromSlash(blocker), []byte("i am a regular file"), 0o600); err != nil {
		t.Fatalf("plant blocker file: %v", err)
	}

	_, err := UploadContentForProfile(context.Background(), st, "proj-test", pid, srvID, "data", blocker+"/sub/f.txt", "", 1<<20)
	if err == nil {
		t.Fatal("regular file in parent chain: want SFTP mkdir failure")
	}
	// Pin WHICH branch fired (anti-vacuous, assertBranch's philosophy): the
	// mkdir-parent failure inside WriteFile — not an earlier connect-free branch
	// whose text would be clean too.
	assertBranch(t, err, "mkdir parent")
	if hostPortRe.MatchString(err.Error()) {
		t.Fatalf("SFTP mid-failure error leaks host:port: %q", err.Error())
	}
	rows, _ := st.AuditRows(5)
	found := false
	for _, r := range rows {
		if r.Action == "upload-content" && r.Status == "error" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no error-status row; rows=%+v", rows)
	}
}
