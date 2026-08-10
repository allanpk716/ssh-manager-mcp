package cli

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// extractToken pulls the one-time token out of a `projects add` / `projects rotate` output.
func extractToken(t *testing.T, out *bytes.Buffer) string {
	t.Helper()
	const mark = "Token (shown once): "
	for _, line := range bytes.Split(out.Bytes(), []byte("\n")) {
		if i := bytes.Index(line, []byte(mark)); i >= 0 {
			return string(line[i+len(mark):])
		}
	}
	t.Fatalf("no token in output: %s", out.String())
	return ""
}

func TestProjectsLifecycleAndAudit(t *testing.T) {
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	dbPath := filepath.Join(dir, "test.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
	})

	// seed a profile + a granted server directly via the store
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := st.AddProfile("dev")
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srvID, _ := st.AddServer(&models.Server{
		Name: "gpu", Host: "10.0.0.5", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	if err := st.GrantServers(pid, []string{srvID}); err != nil {
		t.Fatal(err)
	}
	st.Close()

	run := func(args ...string) *bytes.Buffer {
		root := NewRootCmd()
		out := &bytes.Buffer{}
		root.SetOut(out)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("cli %v: %v", args, err)
		}
		return out
	}
	runErr := func(args ...string) error {
		root := NewRootCmd()
		root.SetArgs(args)
		return root.Execute()
	}
	verify := func(token string) bool {
		s, err := store.Open(dbPath, mk)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		p, _ := s.VerifyToken(token)
		return p != nil
	}

	// add → capture token
	addOut := run("projects", "add", "myagent", "--profile", "dev")
	token := extractToken(t, addOut)
	if !verify(token) {
		t.Fatal("fresh token must verify")
	}

	// show prints the profile + the granted server (no secrets)
	if out := run("projects", "show", "myagent"); !bytes.Contains(out.Bytes(), []byte("dev")) || !bytes.Contains(out.Bytes(), []byte("gpu")) {
		t.Fatalf("show missing profile/server: %s", out.String())
	}

	// rotate → old token dies, new token lives
	rotOut := run("projects", "rotate", "myagent")
	newToken := extractToken(t, rotOut)
	if newToken == token {
		t.Fatal("rotate must produce a different token")
	}
	if verify(token) {
		t.Fatal("old token must NOT verify after rotate")
	}
	if !verify(newToken) {
		t.Fatal("new token must verify after rotate")
	}

	// disable → rejected; enable → admitted; revoke → rejected (Lazy gate)
	run("projects", "disable", "myagent")
	if verify(newToken) {
		t.Fatal("disabled token must NOT verify")
	}
	run("projects", "enable", "myagent")
	if !verify(newToken) {
		t.Fatal("re-enabled token must verify")
	}
	run("projects", "revoke", "myagent")
	if verify(newToken) {
		t.Fatal("revoked token must NOT verify")
	}

	// ls default hides revoked; --all shows it with status
	lsOut := run("projects", "ls")
	if bytes.Contains(lsOut.Bytes(), []byte("myagent")) {
		t.Fatalf("revoked project should be hidden from default ls: %s", lsOut.String())
	}
	lsAll := run("projects", "ls", "--all")
	if !bytes.Contains(lsAll.Bytes(), []byte("myagent")) || !bytes.Contains(lsAll.Bytes(), []byte("revoked")) {
		t.Fatalf("--all should show the revoked project with its status: %s", lsAll.String())
	}

	// audit rows include each lifecycle action
	s, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.AuditRows(32)
	s.Close()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Action] = true
	}
	for _, want := range []string{"project.rotate", "project.disable", "project.enable", "project.revoke"} {
		if !got[want] {
			t.Fatalf("audit missing action %q; have %v", want, got)
		}
	}

	// rotate on an unknown project errors
	if err := runErr("projects", "rotate", "nope"); err == nil {
		t.Fatal("rotate unknown project should error")
	}
}
