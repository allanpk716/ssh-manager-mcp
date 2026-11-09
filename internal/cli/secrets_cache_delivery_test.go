package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

const (
	deliverySecretCurrent  = "delivered-current-secret-value"
	deliverySecretPrevious = "delivered-previous-secret-value"
)

// TestSecretsCacheDeliveryE2E 走真实权威端、真实客户端拉取与真实桥进程装
// 配路径：声明版本的客户端收到裁剪后的机密项；加密缓存、审计文件与桥进程
// 临时目录都找不到值；权威端停止后客户端命令仍能从缓存列出名字。
func TestSecretsCacheDeliveryE2E(t *testing.T) {
	fx := newCacheFwdFixture(t)
	_, err := fx.serve.AddServer(&models.Server{
		Name: "outside", Host: "192.0.2.99", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.AddSecretWithEnvironmentName("DELIVERED_TOKEN", "下发给客户端", []byte(deliverySecretPrevious), "HF_TOKEN", store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.SetSecretValue("DELIVERED_TOKEN", []byte(deliverySecretCurrent), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.AddSecret("OUTSIDE_ONLY", "只写给范围外服务器", []byte("outside-only-secret-value"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.AllowSecrets([]string{"DELIVERED_TOKEN"}, []string{"target", "outside"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := fx.serve.AllowSecrets([]string{"OUTSIDE_ONLY"}, []string{"outside"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	cacheDir := t.TempDir()
	userConfig := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(t.TempDir(), "no-vault.db"),
		"SSHMGR_MASTERKEY_HEX": "",
		"SSHMGR_FILEKEY_PATH":  filepath.Join(t.TempDir(), "no-such-master.key"),
		"SSHMGR_CACHE_DIR":     cacheDir,
		"APPDATA":              userConfig,
		"XDG_CONFIG_HOME":      userConfig,
	})
	authBlob, err := json.Marshal(fx.cred)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), authBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	mem := &store.MemKeyProvider{}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	previousProvider := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = previousProvider })

	if _, err := clientops.DoPull(fx.cred.URL, fx.cred.Token, fx.cred.Pin, clientops.PullOpts{
		StatusOut: io.Discard,
	}); err != nil {
		t.Fatalf("客户端拉取快照失败：%v", err)
	}
	loaded, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatalf("读取客户端加密缓存失败：%v", err)
	}
	if len(loaded.Secrets) != 1 || loaded.Secrets[0].Name != "DELIVERED_TOKEN" {
		t.Fatalf("客户端缓存中的机密项裁剪结果不正确：%+v", loaded.Secrets)
	}
	if len(loaded.SecretAllows) != 1 || loaded.SecretAllows[0].ServerID != fx.targetID {
		t.Fatalf("客户端缓存中的机密项关联裁剪不正确：%+v", loaded.SecretAllows)
	}

	if body := pullSnapshotWithoutClientVersion(t, fx); strings.Contains(body, "DELIVERED_TOKEN") ||
		strings.Contains(body, deliverySecretCurrent) || strings.Contains(body, deliverySecretPrevious) {
		t.Fatal("未声明客户端版本的拉取不应收到任何机密项")
	}

	bridgeTempDir := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, bridgeTempDir)
	}
	auditPath := filepath.Join(cacheDir, "cache-audit.log")
	forwarder, err := clientops.NewPinForwarder(fx.cred)
	if err != nil {
		t.Fatal(err)
	}
	broker, tunnels, tasks, closeBroker, err := mcpserver.NewCacheBroker(
		fx.projToken, loaded, auditPath, nil, clientops.ForwardingHostKeys(forwarder), "laptop", nil)
	if err != nil {
		t.Fatalf("桥进程导入带机密项的快照失败：%v", err)
	}
	t.Cleanup(func() {
		closeBroker()
		tunnels.CloseAll()
		tasks.CloseAll()
	})
	session := attachMCPClient(t, broker)
	if text, isErr := callExec(t, session, fx.targetID, "secret-delivery"); isErr {
		t.Fatalf("带机密项的快照应照常驱动桥进程执行命令：%s", text)
	}

	listCases := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "all", args: map[string]any{}, want: "DELIVERED_TOKEN | 下发给客户端 | HF_TOKEN | target"},
		{name: "query", args: map[string]any{"query": "DELIVERED"}, want: "DELIVERED_TOKEN | 下发给客户端 | HF_TOKEN | target"},
		{name: "server", args: map[string]any{"server": "target"}, want: "DELIVERED_TOKEN | 下发给客户端 | HF_TOKEN | target"},
		{name: "query and server", args: map[string]any{"query": "delivered", "server": "target"}, want: "DELIVERED_TOKEN | 下发给客户端 | HF_TOKEN | target"},
		// outside 在权威端库里真实存在，但不在本客户端的授权范围内：桥进程
		// 的快照里没有它，输出必须与「missing」完全相同。
		{name: "out-of-profile server", args: map[string]any{"server": "outside"}, want: "没有可用的机密项"},
		{name: "missing server", args: map[string]any{"server": "missing"}, want: "没有可用的机密项"},
		{name: "no match", args: map[string]any{"query": "nothing"}, want: "没有可用的机密项"},
	}
	for _, listCase := range listCases {
		t.Run(listCase.name, func(t *testing.T) {
			listText, listErr := callTool(t, session, "list_secrets", listCase.args)
			if listErr {
				t.Fatalf("客户端桥进程应能列出已下发的机密项：%s", listText)
			}
			if listText != listCase.want {
				t.Fatalf("客户端桥进程的机密项列表不正确：\n得到：%q\n想要：%q", listText, listCase.want)
			}
			if strings.Contains(listText, "outside") || strings.Contains(listText, "OUTSIDE_ONLY") ||
				strings.Contains(listText, deliverySecretCurrent) || strings.Contains(listText, deliverySecretPrevious) {
				t.Fatalf("客户端桥进程的机密项列表泄露了范围外信息或值：%s", listText)
			}
		})
	}

	oldAuthoritySnapshot := *loaded
	oldAuthoritySnapshot.Secrets = nil
	oldAuthoritySnapshot.SecretAllows = nil
	oldAuthorityBroker, oldTunnels, oldTasks, closeOldAuthorityBroker, err := mcpserver.NewCacheBroker(
		fx.projToken, &oldAuthoritySnapshot, auditPath, nil, clientops.ForwardingHostKeys(forwarder), "laptop-old-authority", nil)
	if err != nil {
		t.Fatalf("桥进程导入旧权威端的空机密项快照失败：%v", err)
	}
	t.Cleanup(func() {
		closeOldAuthorityBroker()
		oldTunnels.CloseAll()
		oldTasks.CloseAll()
	})
	oldAuthoritySession := attachMCPClient(t, oldAuthorityBroker)
	oldAuthorityText, oldAuthorityErr := callTool(t, oldAuthoritySession, "list_secrets", map[string]any{})
	if oldAuthorityErr || oldAuthorityText != "没有可用的机密项" {
		t.Fatalf("旧权威端快照应返回没有可用的机密项：\n%s\n错误结果：%v", oldAuthorityText, oldAuthorityErr)
	}

	cacheBytes, err := os.ReadFile(filepath.Join(cacheDir, "cache.bin"))
	if err != nil {
		t.Fatal(err)
	}
	assertBytesDoNotContain(t, "加密缓存文件", cacheBytes, []byte(deliverySecretCurrent), []byte(deliverySecretPrevious))
	assertDirectoryFilesDoNotContain(t, bridgeTempDir)
	assertAuditFileHasNoDeliveryValues(t, auditPath)

	fx.serveHTTP.Close()
	offlineText, offlineErr := callTool(t, session, "list_secrets", map[string]any{})
	if offlineErr || offlineText != "DELIVERED_TOKEN | 下发给客户端 | HF_TOKEN | target" {
		t.Fatalf("权威端停止后客户端桥进程应仍能从缓存列出机密项：\n%s\n错误结果：%v", offlineText, offlineErr)
	}
	out, errOut, err := runSecretsRoot(t, "", "secrets", "list")
	if err != nil {
		t.Fatalf("权威端停止后客户端机密项列表失败：%v\n%s", err, errOut)
	}
	if !strings.Contains(out, "DELIVERED_TOKEN") || !strings.Contains(out, "可写入服务器: target") {
		t.Fatalf("客户端应从缓存列出已下发机密项与授权内服务器：\n%s", out)
	}
	if strings.Contains(out, "outside") || strings.Contains(out, "OUTSIDE_ONLY") ||
		strings.Contains(out, deliverySecretCurrent) || strings.Contains(out, deliverySecretPrevious) {
		t.Fatalf("客户端机密项列表泄露了范围外信息或值：\n%s", out)
	}

	filtered, _, err := runSecretsRoot(t, "", "secrets", "list", "--query", "delivered")
	if err != nil {
		t.Fatalf("客户端按文本筛选机密项失败：%v", err)
	}
	if !strings.Contains(filtered, "DELIVERED_TOKEN") {
		t.Fatalf("客户端按文本筛选应保留下发机密项：\n%s", filtered)
	}
	filtered, _, err = runSecretsRoot(t, "", "secrets", "list", "--server", "target")
	if err != nil || !strings.Contains(filtered, "DELIVERED_TOKEN") {
		t.Fatalf("客户端按服务器筛选应保留下发机密项：\n%s\n错误：%v", filtered, err)
	}
	filtered, _, err = runSecretsRoot(t, "", "secrets", "list", "--server", fx.targetID)
	if err != nil || !strings.Contains(filtered, "DELIVERED_TOKEN") {
		t.Fatalf("客户端按服务器条目标识符筛选应保留下发机密项：\n%s\n错误：%v", filtered, err)
	}
	// 客户端快照里没有 outside（它在权威端存在但不在本客户端授权范围内）。
	// 客户端侧范围外与不存在不可区分，也不应区分：都报同一条「不存在」；
	// 真正的授权裁剪门由上面的权威端导出断言覆盖。
	_, _, err = runSecretsRoot(t, "", "secrets", "list", "--server", "outside")
	if err == nil || !strings.Contains(err.Error(), `服务器条目 "outside" 不存在`) {
		t.Fatalf("客户端范围外服务器应报不存在，错误：%v", err)
	}
	_, _, err = runSecretsRoot(t, "", "secrets", "list", "--query", "delivered", "--server", "missing")
	if err == nil || !strings.Contains(err.Error(), `服务器条目 "missing" 不存在`) {
		t.Fatalf("客户端不存在的服务器应报不存在，错误：%v", err)
	}
}

func pullSnapshotWithoutClientVersion(t *testing.T, fx *cacheFwdFixture) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fx.cred.URL+"/snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fx.cred.Token)
	res, err := fx.serveHTTP.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("未声明客户端版本的快照拉取状态码 = %d，正文 = %s", res.StatusCode, body)
	}
	return string(body)
}

func assertDirectoryFilesDoNotContain(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return walkErr
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertBytesDoNotContain(t, "桥进程临时文件 "+path, blob,
			[]byte(deliverySecretCurrent), []byte(deliverySecretPrevious))
		return nil
	})
	if err != nil {
		t.Fatalf("扫描桥进程临时目录失败：%v", err)
	}
}

func assertAuditFileHasNoDeliveryValues(t *testing.T, path string) {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取客户端缓存审计文件失败：%v", err)
	}
	assertBytesDoNotContain(t, "客户端缓存审计文件", blob,
		[]byte(deliverySecretCurrent), []byte(deliverySecretPrevious))
}

func assertBytesDoNotContain(t *testing.T, what string, blob []byte, forbidden ...[]byte) {
	t.Helper()
	for _, value := range forbidden {
		if bytes.Contains(blob, value) {
			t.Fatalf("%s 中出现机密项值明文字节", what)
		}
	}
}
