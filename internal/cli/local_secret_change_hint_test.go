package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestLocalSecretChangeHintE2E_CommandLineSet: 单机部署的真实 `sshmgr mcp`
// 进程保持运行,owner 用命令行更换机密项值后,同一次会话的下一次工具调用
// 收到只含名字的变更提示;同一会话不重复,提示不含任何值。
func TestLocalSecretChangeHintE2E_CommandLineSet(t *testing.T) {
	const (
		oldValue = "local-change-hint-old-value"
		newValue = "local-change-hint-new-value"
	)
	dbPath, masterKey := newSecretsEnv(t)
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := st.AddServer(&models.Server{
		Name: "local", Host: "192.0.2.70", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("local-profile")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	_, projectToken, err := st.AddProject("local-agent", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("DB_PASSWORD", "数据库口令", []byte(oldValue), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"DB_PASSWORD"}, []string{"local"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	binaryName := "sshmgr"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binary := filepath.Join(t.TempDir(), binaryName)
	build := exec.Command("go", "build", "-o", binary, "ssh-manager-mcp/cmd/sshmgr")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建命令行程序失败:%v\n%s", err, output)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.Command(binary, "mcp", "--token", projectToken)
	transport := &mcp.CommandTransport{Command: command}
	client := mcp.NewClient(&mcp.Implementation{Name: "change-hint-test", Version: "v0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	first := callToolTexts(t, session, "list_servers", map[string]any{})
	if len(first) != 1 || !strings.HasPrefix(first[0], `{"servers":`) {
		t.Fatalf("启动后的第一次工具调用不应有提示,实际为 %#v", first)
	}

	revision := func() int64 {
		current, err := store.Open(dbPath, masterKey)
		if err != nil {
			t.Fatal(err)
		}
		defer current.Close()
		rev, err := current.SnapshotRevision()
		if err != nil {
			t.Fatal(err)
		}
		return rev
	}
	before := revision()
	mustSecretsCli(t, newValue, "secrets", "set", "DB_PASSWORD", "--stdin")
	after := revision()

	second := callToolTexts(t, session, "list_servers", map[string]any{})
	want := fmt.Sprintf(
		"权威端配置已更新（修订号 %d → %d）：机密项 DB_PASSWORD 的值已更改，用到它的文件需要重新部署",
		before, after,
	)
	if len(second) != 2 || second[0] != want || !strings.HasPrefix(second[1], `{"servers":`) {
		t.Fatalf("命令行换值后的工具结果 = %#v,开头应为 %q 再接原结果", second, want)
	}
	if strings.Contains(second[0], oldValue) || strings.Contains(second[0], newValue) {
		t.Fatalf("变更提示不应包含机密项值:%q", second[0])
	}

	third := callToolTexts(t, session, "list_servers", map[string]any{})
	if len(third) != 1 || !strings.HasPrefix(third[0], `{"servers":`) {
		t.Fatalf("同一会话的换值提示只应出现一次,实际为 %#v", third)
	}
}
