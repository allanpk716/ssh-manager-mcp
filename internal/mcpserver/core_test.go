package mcpserver

import (
	"context"
	"errors"
	"io"
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
