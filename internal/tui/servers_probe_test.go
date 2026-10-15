package tui

// 服务器页 `p` 键(票 #17,命令策略诊断面):探测当前服务器的实际
// shell 类型并在状态行报告——CLI `servers probe-shell` 的 TUI 等价面
// (ADR 0003)。走真实 Update 派发路径 + 进程内测试 sshd 脚本化应答,
// 断言外部行为:按键不打开 overlay、返回的 cmd 执行后状态消息如实报告
// 一致/不一致;空列表时是无害空操作。

import (
	"io"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/testsshd"
)

// newProbeApp builds a broker App whose store holds ONE server pointing at a
// scripted test sshd answering like the given shell family.
func newProbeApp(t *testing.T, family, annotateShell string) App {
	t.Helper()
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			switch cmd {
			case "echo %OS%":
				if family == "cmd" {
					return "Windows_NT\r\n", "", 0
				}
				return "%OS%\r\n", "", 0
			case "echo $PSVersionTable":
				if family == "powershell" {
					return "Name                           Value\r\n" +
						"----                           -----\r\n" +
						"PSVersion                      7.4.1\r\n" +
						"PSEdition                      Core\r\n", "", 0
				}
				if family == "cmd" {
					return "$PSVersionTable\r\n", "", 0
				}
				return "\n", "", 0
			}
			return "", "", 1
		},
	})
	t.Cleanup(cleanup)

	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	_, _ = st.AddServer(&models.Server{
		Name: "box", Host: addr[:strings.IndexByte(addr, ':')], Port: portOf(addr), User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
		ShellType: models.ShellType(annotateShell),
	})
	_ = st.SaveHostKey(addr[:strings.IndexByte(addr, ':')], portOf(addr), hostKey.Marshal())

	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatalf("NewBrokerApp: %v", err)
	}
	a.backupCfgPath = t.TempDir() + "/backup.json"
	pages, err := FetchAll(st, a.backupCfgPath)
	if err != nil {
		t.Fatalf("FetchAll: %v", err)
	}
	a.pages = pages
	return a
}

func portOf(addr string) int {
	i := strings.IndexByte(addr, ':')
	n := 0
	for _, c := range addr[i+1:] {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestServersPageProbeShellKey(t *testing.T) {
	t.Run("mismatch surfaces in the status message", func(t *testing.T) {
		a := newProbeApp(t, "powershell", "posix")
		m, cmd := a.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
		got := m.(App)
		if got.overlay != nil {
			t.Fatalf("p must not open an overlay, got %q", got.overlay.Title())
		}
		if cmd == nil {
			t.Fatal("p must return the probe cmd (doAction)")
		}
		msg := cmd()
		done, ok := msg.(actionDoneMsg)
		if !ok {
			t.Fatalf("probe cmd must finish with actionDoneMsg, got %#v", msg)
		}
		lower := strings.ToLower(done.desc)
		if !strings.Contains(lower, "powershell") || !strings.Contains(lower, "posix") {
			t.Fatalf("status message must name both shells: %q", done.desc)
		}
		if !strings.Contains(lower, "不一致") {
			t.Fatalf("status message must flag the mismatch: %q", done.desc)
		}
		// 探测不改标注。
		sp, _ := got.pages[pageServers].(*serversPage)
		srv := sp.items[0]
		if got := models.NormalizeShellType(string(srv.ShellType)); got != models.ShellPOSIX {
			t.Fatalf("probe must never change the annotation, still %q", got)
		}
	})
	t.Run("match reports agreement", func(t *testing.T) {
		a := newProbeApp(t, "posix", "posix")
		_, cmd := a.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
		if cmd == nil {
			t.Fatal("p must return the probe cmd")
		}
		done, ok := cmd().(actionDoneMsg)
		if !ok {
			t.Fatalf("probe cmd must finish with actionDoneMsg, got %#v", cmd())
		}
		if !strings.Contains(done.desc, "一致") || strings.Contains(done.desc, "不一致") {
			t.Fatalf("matching annotation must report agreement: %q", done.desc)
		}
	})
	t.Run("empty list is a no-op", func(t *testing.T) {
		a := newProbeApp(t, "posix", "posix")
		sp, _ := a.pages[pageServers].(*serversPage)
		sp.items = nil
		sp.rebuild()
		m, cmd := a.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
		got := m.(App)
		if got.overlay != nil || cmd != nil {
			t.Fatalf("p with no current server must be a silent no-op: overlay=%v cmd=%v", got.overlay, cmd)
		}
	})
}

// TestServersPageProbeShellKeyUnreachable: 连不上的服务器把错误送进
// errMsg 路径(诊断诚实——连接失败不装成一致)。
func TestServersPageProbeShellKeyUnreachable(t *testing.T) {
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	_, _ = st.AddServer(&models.Server{
		Name: "down", Host: "127.0.0.1", Port: 1, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, ShellType: models.ShellPOSIX,
	})
	a, err := NewBrokerApp(st)
	if err != nil {
		t.Fatal(err)
	}
	a.backupCfgPath = t.TempDir() + "/backup.json"
	pages, err := FetchAll(st, a.backupCfgPath)
	if err != nil {
		t.Fatal(err)
	}
	a.pages = pages

	_, cmd := a.Update(tea.KeyPressMsg{Code: 'p', Text: "p"})
	if cmd == nil {
		t.Fatal("p must return the probe cmd")
	}
	if _, ok := cmd().(errMsg); !ok {
		t.Fatalf("unreachable server must surface as errMsg, got %#v", cmd())
	}
}
