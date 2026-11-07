package cli

import (
	"bytes"
	"io"
	"net"
	"sync/atomic"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// seedSkippedHostKeyCLI prepares a standalone vault whose local device shares a
// physical local-area network with the target. The first candidate presents a
// different host key; the fallback address is the correct fixture.
func seedSkippedHostKeyCLI(t *testing.T, exec func(command string, stdin io.Reader) (string, string, int)) {
	t.Helper()
	configRoot := t.TempDir()
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	dbPath, masterKey := withCliStoreEnv(t)
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	correctAddr, correctHostKey, correctCleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", Exec: exec,
	})
	t.Cleanup(correctCleanup)
	var authRequests atomic.Int64
	wrongAddr, _, wrongCleanup := testsshd.Start(t, testsshd.Options{
		ListenHost: "::1", ListenPort: portOfAddr(correctAddr), Password: "pw",
		AuthRequests: &authRequests,
	})
	t.Cleanup(wrongCleanup)

	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	local, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(local.ID); err != nil {
		t.Fatal(err)
	}
	host, _, _ := net.SplitHostPort(correctAddr)
	serverResult, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "工作站 SSH", Host: host, Port: portOfAddr(correctAddr), User: "u",
		AuthMethod: models.AuthPassword,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil, "工作站")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := st.GetServer(serverResult.ServerID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(srv, correctHostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
	wrongHost, _, _ := net.SplitHostPort(wrongAddr)
	if _, err := st.AddNetworkMembership("工作站", "家A局域网", wrongHost); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("笔记本", "家A局域网", "127.0.0.2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if n := authRequests.Load(); n != 0 {
			t.Errorf("skipped address authentication requests = %d, want 0", n)
		}
	})
}

func TestOwnerSSHPrintsConnectionNotesToStderr(t *testing.T) {
	seedSkippedHostKeyCLI(t, func(command string, _ io.Reader) (string, string, int) {
		return "REMOTE:" + command + "\n", "", 0
	})
	root := NewRootCmd()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"ssh", "工作站 SSH", "echo", "hello"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("REMOTE:echo hello")) {
		t.Fatalf("stdout missing command result: %q", stdout.String())
	}
	wantNote := "家A局域网 第1个地址呈现了不同的主机密钥，已跳过"
	if !bytes.Contains(stderr.Bytes(), []byte(wantNote)) {
		t.Fatalf("stderr missing connection note %q: %q", wantNote, stderr.String())
	}
}

func TestServersProbeShellPrintsConnectionNotesToStderr(t *testing.T) {
	seedSkippedHostKeyCLI(t, func(command string, _ io.Reader) (string, string, int) {
		switch command {
		case "echo %OS%":
			return "%OS%\n", "", 0
		case "echo $PSVersionTable":
			return "\n", "", 0
		default:
			return "", "", 1
		}
	})
	root := NewRootCmd()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs([]string{"servers", "probe-shell", "工作站 SSH"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stdout.Bytes(), []byte("real shell=posix")) {
		t.Fatalf("stdout missing probe result: %q", stdout.String())
	}
	wantNote := "家A局域网 第1个地址呈现了不同的主机密钥，已跳过"
	if !bytes.Contains(stderr.Bytes(), []byte(wantNote)) {
		t.Fatalf("stderr missing connection note %q: %q", wantNote, stderr.String())
	}
}
