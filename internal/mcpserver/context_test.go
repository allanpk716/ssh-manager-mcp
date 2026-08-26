package mcpserver

// Plan 41 批 2 测试:exec_context 双态 wired、exec_command sudo fail-loud、
// SudoInfo 的 uid=0 JSON 语义、后台任务的 Sudo 元数据透出。

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

// fakeCtxBody recognizes an exec_context body arriving at the (simulated
// privileged or plain) side and renders its section output, echoing the call's
// own nonce so parseCtxOutput can find the sections.
func fakeCtxBody(cmd string, uid int) (string, bool) {
	if !strings.Contains(cmd, "__SSHMGR_CTX_") {
		return "", false
	}
	i := strings.Index(cmd, "__SSHMGR_CTX_")
	n := cmd[i+len("__SSHMGR_CTX_"):]
	if j := strings.IndexByte(n, ':'); j >= 0 {
		n = n[:j]
	}
	m := "__SSHMGR_CTX_" + n + ":"
	idLine := "uid=1000(u) gid=1000(u) groups=1000(u)"
	if uid == 0 {
		idLine = "uid=0(root) gid=0(root) groups=0(root)"
	}
	return strings.Join([]string{
		m + "id", idLine,
		m + "tty", "no-tty",
		m + "uidmap", "0 0 4294967295",
		m + "lsm", "unconfined",
		m + "proc", "pid=4242 ppid=4241 comm=bash",
	}, "\n") + "\n", true
}

func TestExecContextPlainChannel(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			if out, ok := fakeCtxBody(cmd, 1000); ok {
				return out, "", 0
			}
			return "", "unknown: " + cmd + "\n", 1
		},
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	out, err := ExecContextForProfile(context.Background(), st, "proj-test", pid, srvID, false)
	if err != nil {
		t.Fatalf("exec_context: %v", err)
	}
	if out.Elevated || out.UID != 1000 || out.UIDMap != "0 0 4294967295" || out.TTY != "no-tty" || out.LSMLabel != "unconfined" {
		t.Fatalf("out = %+v", out)
	}
	if out.PID != 4242 || out.PPID != 4241 || out.Comm != "bash" {
		t.Fatalf("proc fields = %+v", out)
	}
	if out.SSHClient != "" {
		t.Fatalf("SSHClient = %q, want empty on the plain path (no pre-elevation capture needed)", out.SSHClient)
	}
}

func TestExecContextElevatedChannel(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sudopw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			if out, ok := fakeCtxBody(cmd, 0); ok {
				return out, "", 0
			}
			return "", "unknown: " + cmd + "\n", 1
		},
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "sudopw")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	out, err := ExecContextForProfile(context.Background(), st, "proj-test", pid, srvID, true)
	if err != nil {
		t.Fatalf("exec_context: %v", err)
	}
	// The regression pin of the whole incident: uid=0 AND the SSH provenance
	// in the SAME snapshot — pre-elevation capture means sudo's env_reset can
	// no longer hide where the channel came from.
	if !out.Elevated || out.UID != 0 {
		t.Fatalf("out = %+v, want elevated uid=0", out)
	}
	if out.SSHClient != "fakeclient 1111 22" || out.SSHConnection != "fakeclient 1111 fakeserver 22" {
		t.Fatalf("ssh provenance = %q / %q, want the pre-elevation captured values", out.SSHClient, out.SSHConnection)
	}
	if out.TTY != "no-tty" || out.Comm != "bash" {
		t.Fatalf("out = %+v", out)
	}
}

func TestExecCommandSudoFailLoud(t *testing.T) {
	// Vault holds one sudo password; the simulated sudo expects another — the
	// credential is rejected, and the call must come back as a TOOL ERROR with
	// the structured outcome, not as a normal exit-1 result.
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "mismatch",
		Exec: func(cmd string, _ io.Reader) (string, string, int) { return "RAN\n", "", 0 },
	})
	defer cleanup()
	st := newStore(t)
	srvID := seedRealServer(t, st, "real", addr, hk, "sudopw")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})

	out, err := ExecCommandForProfile(context.Background(), st, "proj-test", pid, srvID, "true", true, 5*time.Second)
	if err == nil {
		t.Fatal("auth-failed must surface as an error (fail-loud), not a normal result")
	}
	if out.Sudo == nil || out.Sudo.Outcome != sshbroker.SudoAuthFailed {
		t.Fatalf("out.Sudo = %+v, want auth-failed", out.Sudo)
	}
	if !strings.Contains(err.Error(), "did NOT run") {
		t.Fatalf("err = %v, want the did-NOT-run wording", err)
	}
	if strings.Contains(out.Stdout, "RAN") {
		t.Fatal("the command must not have run")
	}
}

// TestSudoInfoJSONUidZero pins the pointer-vs-omitempty semantics at the JSON
// layer: uid=0 is an ATTESTATION and must serialize as a present "uid":0 —
// the round-1 review's consensus bug (int+omitempty silently dropped exactly
// the root case).
func TestSudoInfoJSONUidZero(t *testing.T) {
	zero := 0
	b, err := json.Marshal(ExecOutput{Sudo: &SudoInfo{Outcome: sshbroker.SudoElevated, UID: &zero}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"uid":0`) {
		t.Fatalf("json = %s, want literal \"uid\":0 present", b)
	}
	// Unattested: nil pointer serializes as an EXPLICIT null — present but
	// clearly "no attestation", never conflatable with 0.
	b2, _ := json.Marshal(ExecOutput{Sudo: &SudoInfo{Outcome: sshbroker.SudoUnverified}})
	if !strings.Contains(string(b2), `"uid":null`) || strings.Contains(string(b2), `"uid":0`) {
		t.Fatalf("json = %s, want explicit \"uid\":null when unattested", b2)
	}
}

// TestBgSudoMetaInOutput: a background sudo task's terminal snapshot carries
// the elevation metadata (the aggregate-side classification of §2.2).
func TestBgSudoMetaInOutput(t *testing.T) {
	st := newStore(t)
	m := newTestTM(t, 4)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw", SudoPassword: "sp",
		Exec: func(cmd string, _ io.Reader) (string, string, int) { return "root\n", "", 0 },
	})
	defer cleanup()

	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	id, _, serr := m.Start(context.Background(), st, BgStartSpec{
		ProjectID: "proj", ServerID: "srv", Command: "whoami",
		Sudo: true, SudoPass: "sp", TimeoutSec: 60,
		Server:    &models.Server{Host: host, Port: port, User: "u"},
		Auth:      sshbroker.PasswordAuth("pw"),
		HostKeyCb: ssh.FixedHostKey(hk),
	})
	if serr != nil {
		t.Fatalf("start: %v", serr)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		v, ok, err := m.Output(id, 0, 0, time.Second, context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatal("task vanished")
		}
		if v.Status != bgStatusRunning {
			if v.Sudo == nil || v.Sudo.Outcome != sshbroker.SudoElevated || v.Sudo.UID == nil || *v.Sudo.UID != 0 {
				t.Fatalf("v.Sudo = %+v, want elevated/uid=0 at terminal state", v.Sudo)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not finish in time")
		}
	}
}
