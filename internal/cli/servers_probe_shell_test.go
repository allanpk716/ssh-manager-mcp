package cli

// `sshmgr servers probe-shell`(ticket #17,命令策略诊断面)的命令级行为
// 测试:用进程内测试 sshd 脚本化三类 shell 对两条探测命令的真实应答,
// 断言外部行为——不一致时输出提示并给出修正指引、一致时无提示、探测
// 从不改标注。种子写法照抄 ssh_test.go(真 testsshd 地址 + 预信任主机
// 密钥)。

import (
	"bytes"
	"encoding/hex"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// probeScript answers the two probe commands exactly like the requested shell
// family would (posix / powershell / cmd / unknown), and records every command
// it saw — the probe must send exactly the two documented commands and nothing
// else (asserted by the caller after the command returns; the Exec callback
// runs on the test sshd's goroutine, where t.Fatalf is not allowed).
type probeScript struct {
	family string
	mu     sync.Mutex
	seen   []string
}

func (p *probeScript) exec(cmd string, _ io.Reader) (string, string, int) {
	p.mu.Lock()
	p.seen = append(p.seen, cmd)
	p.mu.Unlock()
	switch cmd {
	case "echo %OS%":
		switch p.family {
		case "cmd":
			return "Windows_NT\r\n", "", 0
		default: // POSIX 与 PowerShell 都原样打印
			return "%OS%\r\n", "", 0
		}
	case "echo $PSVersionTable":
		switch p.family {
		case "powershell":
			return "Name                           Value\r\n" +
				"----                           -----\r\n" +
				"PSVersion                      7.4.1\r\n" +
				"PSEdition                      Core\r\n" +
				"GitCommitId                    7.4.1\r\n", "", 0
		case "cmd", "unknown":
			return "$PSVersionTable\r\n", "", 0
		default: // POSIX:未定义变量展开为空
			return "\n", "", 0
		}
	}
	return "", "", 1 // unexpected command — recorded above, asserted by the test
}

func (p *probeScript) commandsSeen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// newProbeEnv pins an isolated vault (never the developer machine's real
// master.key — newHintEnv pattern) and returns db path + master key.
func newProbeEnv(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "test.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
	})
	return dbPath, mk
}

// startProbeTarget starts one scripted test sshd (password auth) and returns
// its addr + host key.
func startProbeTarget(t *testing.T, script *probeScript) (string, interface{ Marshal() []byte }) {
	t.Helper()
	addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw", Exec: script.exec})
	t.Cleanup(cleanup)
	return addr, hostKey
}

// addProbeServerRow adds a server row pointing at the given test sshd addr
// (password credential + pre-trusted host key + shell annotation).
func addProbeServerRow(t *testing.T, st *store.Store, name, addr string, hostKey interface{ Marshal() []byte }, annotateShell string) {
	t.Helper()
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	_, _ = st.AddServer(&models.Server{
		Name: name, Host: addr[:bytesIndex(addr, ':')], Port: portOfAddr(addr), User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
		ShellType: models.ShellType(annotateShell),
	})
	_ = st.SaveHostKey(addr[:bytesIndex(addr, ':')], portOfAddr(addr), hostKey.Marshal())
}

// seedProbeServer wires the whole one-server fixture: env + scripted test sshd
// + server row. Returns db path, master key and the script (for command-list
// assertions).
func seedProbeServer(t *testing.T, name, family, annotateShell string) (string, []byte, *probeScript) {
	t.Helper()
	dbPath, mk := newProbeEnv(t)
	script := &probeScript{family: family}
	addr, hostKey := startProbeTarget(t, script)
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	addProbeServerRow(t, st, name, addr, hostKey, annotateShell)
	st.Close()
	return dbPath, mk, script
}

// runProbeShell drives the CLI with both streams captured.
func runProbeShell(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetArgs(append([]string{"servers", "probe-shell"}, args...))
	err := root.Execute()
	return out.String(), err
}

// storedShellType reopens the vault and reads the server's shell annotation —
// proving the probe never touched it.
func storedShellType(t *testing.T, dbPath string, mk []byte, name string) models.ShellType {
	t.Helper()
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, _ := st.GetServerByName(name)
	if srv == nil {
		t.Fatalf("server %s missing", name)
	}
	return models.NormalizeShellType(string(srv.ShellType))
}

// TestServersProbeShellMismatchHint 是票 #17 的验收主用例:标注为
// POSIX、实际为 PowerShell 的服务器,诊断输出必须提示不一致并给出修正
// 指引,且不改标注;探测只发两条固定命令。
func TestServersProbeShellMismatchHint(t *testing.T) {
	dbPath, mk, script := seedProbeServer(t, "winbox", "powershell", "posix")

	got, err := runProbeShell(t, "winbox")
	if err != nil {
		t.Fatalf("probe-shell must exit 0 on a mismatch (the hint is the product, not a failure): %v\n%s", err, got)
	}
	lower := strings.ToLower(got)
	if !strings.Contains(lower, "mismatch") {
		t.Fatalf("output missing mismatch hint:\n%s", got)
	}
	if !strings.Contains(lower, "powershell") || !strings.Contains(lower, "posix") {
		t.Fatalf("hint must name both the real and the annotated shell type:\n%s", got)
	}
	if !strings.Contains(lower, "servers edit winbox --shell powershell") {
		t.Fatalf("hint must point at the exact fix command:\n%s", got)
	}
	if stored := storedShellType(t, dbPath, mk, "winbox"); stored != models.ShellPOSIX {
		t.Fatalf("probe must never change the annotation, still %q", stored)
	}
	if seen := script.commandsSeen(); strings.Join(seen, ";") != "echo %OS%;echo $PSVersionTable" {
		t.Fatalf("probe must send exactly the two documented echo commands, saw %q", seen)
	}
}

// TestServersProbeShellMatchNoHint 是票 #17 的另一半验收:标注与实际
// 一致时无提示。
func TestServersProbeShellMatchNoHint(t *testing.T) {
	dbPath, mk, _ := seedProbeServer(t, "nix", "posix", "posix")

	got, err := runProbeShell(t, "nix")
	if err != nil {
		t.Fatalf("probe-shell: %v\n%s", err, got)
	}
	if strings.Contains(strings.ToLower(got), "mismatch") {
		t.Fatalf("matching annotation must not produce a mismatch hint:\n%s", got)
	}
	if !strings.Contains(got, "matches") {
		t.Fatalf("output should state the match:\n%s", got)
	}
	if stored := storedShellType(t, dbPath, mk, "nix"); stored != models.ShellPOSIX {
		t.Fatalf("annotation changed to %q", stored)
	}
}

// TestServersProbeShellCmdFamilyAndUndetermined:cmd 家族端到端一次(标
// 注一致);无法判定的应答如实输出「无法判定」并带判定依据,不算失败。
func TestServersProbeShellCmdFamilyAndUndetermined(t *testing.T) {
	t.Run("cmd matches", func(t *testing.T) {
		dbPath, mk, _ := seedProbeServer(t, "win", "cmd", "cmd")
		got, err := runProbeShell(t, "win")
		if err != nil {
			t.Fatalf("probe-shell: %v\n%s", err, got)
		}
		if !strings.Contains(got, "matches") {
			t.Fatalf("cmd-annotated cmd-real server must report a match:\n%s", got)
		}
		if stored := storedShellType(t, dbPath, mk, "win"); stored != models.ShellWindowsCmd {
			t.Fatalf("annotation changed to %q", stored)
		}
	})
	t.Run("undetermined is honest, not a failure", func(t *testing.T) {
		dbPath, mk, _ := seedProbeServer(t, "odd", "unknown", "posix")
		got, err := runProbeShell(t, "odd")
		if err != nil {
			t.Fatalf("an undetermined probe is a report, not a command failure: %v\n%s", err, got)
		}
		if !strings.Contains(got, "could not determine") {
			t.Fatalf("output must state it could not determine the real shell type:\n%s", got)
		}
		if !strings.Contains(got, "posix") {
			t.Fatalf("output must still name the annotation:\n%s", got)
		}
		if stored := storedShellType(t, dbPath, mk, "odd"); stored != models.ShellPOSIX {
			t.Fatalf("annotation changed to %q", stored)
		}
	})
}

// TestServersProbeShellAllServers: 不给名字时探测全部服务器,每台一行;
// 一台不一致不影响另一台的结论。
func TestServersProbeShellAllServers(t *testing.T) {
	dbPath, mk := newProbeEnv(t)
	psScript := &probeScript{family: "powershell"}
	nixScript := &probeScript{family: "posix"}
	psAddr, psKey := startProbeTarget(t, psScript)
	nixAddr, nixKey := startProbeTarget(t, nixScript)
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	addProbeServerRow(t, st, "nixbox", nixAddr, nixKey, "posix")
	addProbeServerRow(t, st, "winbox", psAddr, psKey, "posix")
	st.Close()

	got, err := runProbeShell(t)
	if err != nil {
		t.Fatalf("probe-shell (all): %v\n%s", err, got)
	}
	nixLine, winLine := "", ""
	for _, line := range strings.Split(got, "\n") {
		switch {
		case strings.HasPrefix(line, "nixbox:"):
			nixLine = line
		case strings.HasPrefix(line, "winbox:"):
			winLine = line
		}
	}
	if nixLine == "" || winLine == "" {
		t.Fatalf("no args must probe every server, one line each:\n%s", got)
	}
	if strings.Contains(strings.ToLower(nixLine), "mismatch") || !strings.Contains(nixLine, "matches") {
		t.Fatalf("nixbox line must be a match:\n%s", nixLine)
	}
	if !strings.Contains(strings.ToLower(winLine), "mismatch") {
		t.Fatalf("winbox line must be a mismatch:\n%s", winLine)
	}
	if stored := storedShellType(t, dbPath, mk, "winbox"); stored != models.ShellPOSIX {
		t.Fatalf("annotation changed to %q", stored)
	}
}

// TestServersProbeShellUnknownNameAndFailure: 名字拼错在探测前报错;连不
// 上的服务器打失败行且命令以非零退出(诊断诚实——连接失败不是「一致」)。
func TestServersProbeShellUnknownNameAndFailure(t *testing.T) {
	dbPath, mk := newProbeEnv(t)
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	// 127.0.0.1:1 —— 连接必然立刻被拒,不用等超时。
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	_, _ = st.AddServer(&models.Server{
		Name: "down", Host: "127.0.0.1", Port: 1, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid, ShellType: models.ShellPOSIX,
	})
	st.Close()

	got, err := runProbeShell(t, "no-such-server")
	if err == nil || !strings.Contains(err.Error(), `"no-such-server" not found`) {
		t.Fatalf("unknown name must fail with the not-found error, got %v\n%s", err, got)
	}

	got, err = runProbeShell(t, "down")
	if err == nil {
		t.Fatalf("an unproberable server must exit non-zero:\n%s", got)
	}
	if !strings.Contains(got, "down: probe failed:") {
		t.Fatalf("failure line must name the server:\n%s", got)
	}
}
