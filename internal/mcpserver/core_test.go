package mcpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	mk, _ := store.GenerateMasterKey()
	st, err := store.Open(t.TempDir()+"/t.db", mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
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

// TestUploadForProfileTruncatesLargeUpload verifies the §6 cap: a payload larger
// than MaxOutputBytes yields Truncated=true with Bytes reporting the true total
// transferred before the cap halted the walk.
func TestUploadForProfileTruncatesLargeUpload(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	// Build a local dir tree well over the cap: two files, each 2 MiB. The
	// countingWriter trips the cap during the first file's io.Copy and uploadDir
	// halts the walk before the second file starts.
	big := strings.Repeat("x", int(MaxOutputBytes)*2) // 2 MiB each
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, "a.bin"), []byte(big), 0644); err != nil {
		t.Fatalf("setup a.bin: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "b.bin"), []byte(big), 0644); err != nil {
		t.Fatalf("setup b.bin: %v", err)
	}
	remoteDir := toSlash(filepath.Join(t.TempDir(), "up-cap"))
	out, err := UploadForProfile(context.Background(), st, "proj-test", pid, srvID, localDir, remoteDir)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if !out.Truncated {
		t.Fatal("want UploadOutput.Truncated=true (payload exceeded the cap)")
	}
	if out.Bytes <= MaxOutputBytes {
		t.Fatalf("Bytes=%d, want > %d (the cap was exceeded)", out.Bytes, MaxOutputBytes)
	}
	if out.Files == 0 {
		t.Fatal("Files=0, want at least the file that tripped the cap")
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
	if _, err := cli.Exec("anything", time.Second, 64); err == nil {
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

// TestTunnelManagerSweepIdleReapsStaleTunnels verifies the idle-sweeper's
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
	if _, err := cli.Exec("anything", time.Second, 64); err == nil {
		t.Fatal("ssh.Client still usable after SweepIdle (resource leak)")
	}
}
