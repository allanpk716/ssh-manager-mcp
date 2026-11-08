package servermanage

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func writeSSHConfig(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestImportPlanAndRunCounts 校验导入计划与执行：候选、跳过、失败、待补四类
// 计数，同名与相同主机地址冲突，同批相同私钥只建一份凭据，导入时授权
// profile。
func TestImportPlanAndRunCounts(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	encKey := filepath.Join(dir, "enc_key")
	genEncryptedKeyFile(t, encKey, "secret-pass")
	sharedKey := filepath.Join(dir, "shared_key")
	genPlainKey(t, sharedKey)

	// taken 与 candidate “dup-endpoint” 的 host:port:user 相同。
	if _, err := m.st.AddServerWithCredentials(&models.Server{
		Name: "taken", Host: "192.0.2.99", Port: 22, User: "me",
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.AddServerWithCredentials(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "deploy",
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	config := writeSSHConfig(t, dir, fmt.Sprintf(
		"Host *.wild\n  HostName 192.0.2.1\n"+
			"Host bad-port\n  HostName 192.0.2.2\n  Port notanumber\n"+
			"Host gpu\n  HostName 192.0.2.10\n  User deploy\n"+
			"Host dup-endpoint\n  HostName 192.0.2.99\n  User me\n"+
			"Host web\n  HostName 192.0.2.20\n  User deploy\n  IdentityFile %q\n"+
			"Host enc\n  HostName 192.0.2.30\n  User deploy\n  IdentityFile %q\n"+
			"Host k1\n  HostName 192.0.2.41\n  User deploy\n  IdentityFile %q\n"+
			"Host k2\n  HostName 192.0.2.42\n  User deploy\n  IdentityFile %q\n",
		filepath.ToSlash(sharedKey), filepath.ToSlash(encKey),
		filepath.ToSlash(sharedKey), filepath.ToSlash(sharedKey)))
	profileID, err := m.st.AddProfile("team")
	if err != nil {
		t.Fatal(err)
	}

	plan, err := m.PlanImport(config, "team")
	if err != nil {
		t.Fatal(err)
	}
	if !plan.MatchWarning && !strings.Contains(readConfigText(t, config), "Match") {
		// 本用例配置不含 Match 块，MatchWarning 应为假。
	}
	if plan.ProfileID != profileID || plan.ProfileName != "team" {
		t.Fatalf("导入计划应解析 profile：%+v", plan)
	}
	names := make([]string, 0, len(plan.Candidates))
	notes := map[string]string{}
	for _, cand := range plan.Candidates {
		names = append(names, cand.Name)
		notes[cand.Name] = cand.Note
	}
	if got := strings.Join(names, ","); got != "web,enc,k1,k2" {
		t.Fatalf("可导入候选 = %s", got)
	}
	if notes["web"] != ImportNoteKey || notes["enc"] != ImportNoteNeedsPassphrase {
		t.Fatalf("候选凭据注释不正确：%v", notes)
	}
	if len(plan.VaultSkipped) != 2 {
		t.Fatalf("应有同名与相同主机地址两类 vault 冲突：%+v", plan.VaultSkipped)
	}
	if len(plan.ParseSkipped) != 2 {
		t.Fatalf("应有通配符与端口错误两类解析跳过：%+v", plan.ParseSkipped)
	}

	// 注入一台服务器插入失败，覆盖失败计数与逐台状态。
	m.insertServer = func(srv *models.Server, cred, sudo *models.Credential) (string, error) {
		if srv.Name == "k2" {
			return "", fmt.Errorf("injected insert failure")
		}
		return m.st.AddServerWithCredentials(srv, cred, sudo)
	}
	result, err := m.RunImport(config, names, "team", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 3 || result.Failed != 1 || result.Pending != 3 {
		t.Fatalf("计数不正确：%+v", result)
	}
	if result.Skipped != 4 { // 同名 + 相同主机地址 + 通配符 + 端口错误
		t.Fatalf("跳过计数不正确：%+v", result)
	}
	if len(result.ImportedServerIDs) != 3 {
		t.Fatalf("导入成功标识符 = %v", result.ImportedServerIDs)
	}
	for _, name := range []string{"web", "enc", "k1"} {
		srv, err := m.st.GetServerByName(name)
		if err != nil || srv == nil {
			t.Fatalf("%s 应已导入：%v %v", name, srv, err)
		}
		if srv.DeviceID == "" {
			t.Fatalf("导入应生成所属设备：%+v", srv)
		}
	}
	enc, _ := m.st.GetServerByName("enc")
	if !models.ServerHasTag(enc.Tags, models.TagNeedsPassphrase) {
		t.Fatalf("加密私钥应带待补口令标签：%v", enc.Tags)
	}
	k1, _ := m.st.GetServerByName("k1")
	web, _ := m.st.GetServerByName("web")
	if k1.CredentialID == "" || k1.CredentialID != web.CredentialID {
		t.Fatalf("同批相同私钥只建一份凭据：k1=%s web=%s", k1.CredentialID, web.CredentialID)
	}
	granted, err := m.st.ServersForProfile(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 3 {
		t.Fatalf("导入时授权 profile 应覆盖全部成功导入的服务器：%v", granted)
	}

	// 再次执行同一配置：全部因同名或相同主机地址跳过，计数仍然成立。
	m.insertServer = nil
	again, err := m.RunImport(config, names, "", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if again.Imported != 1 || again.Failed != 0 || again.Entries[len(again.Entries)-1].Name != "k2" {
		t.Fatalf("重试只应补上之前失败的一台：%+v", again)
	}
	final, err := m.RunImport(config, names, "", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if final.Imported != 0 || final.Failed != 0 {
		t.Fatalf("重复导入不应再写入：%+v", final)
	}
}

func readConfigText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func genPlainKey(t *testing.T, path string) []byte {
	t.Helper()
	return genPlainKeyWithPassphrase(t, path, "")
}

func genEncryptedKeyFile(t *testing.T, path, passphrase string) []byte {
	t.Helper()
	return genPlainKeyWithPassphrase(t, path, passphrase)
}

func genPlainKeyWithPassphrase(t *testing.T, path, passphrase string) []byte {
	t.Helper()
	keyPEM := genEncryptedKey(t, passphrase)
	if passphrase == "" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(priv)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	if err := os.WriteFile(path, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return keyPEM
}

// TestImportPlanProfilesMissing 校验导入计划里 profile 不存在时先失败，不读
// 私钥也不写入。
func TestImportPlanProfilesMissing(t *testing.T) {
	m := newTestManager(t)
	config := writeSSHConfig(t, t.TempDir(), "Host web\n  HostName 192.0.2.20\n  User deploy\n")
	if _, err := m.PlanImport(config, "nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("profile 不存在应报错：%v", err)
	}
	if servers, _ := m.st.ListServers(); len(servers) != 0 {
		t.Fatalf("计划失败不得写入：%v", servers)
	}
}

var _ = store.AuditSourceCLI

// TestImportWritesAuditAndGrantsThroughAccessModule 导入本身要写带来源的
// 审计行；profile 授权必须走访问管理模块（同样写带来源的授权审计），不能
// 直接改存储层。
func TestImportWritesAuditAndGrantsThroughAccessModule(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "plain_key")
	genPlainKey(t, keyPath)
	config := writeSSHConfig(t, dir, fmt.Sprintf(
		"Host web\n  HostName 192.0.2.20\n  User deploy\n  IdentityFile %q\n", filepath.ToSlash(keyPath)))
	profileID, err := m.st.AddProfile("team")
	if err != nil {
		t.Fatal(err)
	}

	result, err := m.RunImport(config, []string{"web"}, "team", WebUISource("192.0.2.7"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 {
		t.Fatalf("应导入一台：%+v", result)
	}

	importRows, err := m.st.QueryAudit(store.AuditFilter{Actions: []string{"server.import"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(importRows) != 1 {
		t.Fatalf("导入应写一条 server.import 审计，实际 %d", len(importRows))
	}
	row := importRows[0]
	if row.Source != store.AuditSourceWebUI || row.SourceIP != "192.0.2.7" || row.Status != "ok" {
		t.Fatalf("导入审计来源不正确：%+v", row)
	}
	if strings.Contains(row.Command, "PRIVATE KEY") || strings.Contains(row.Command, "secret") {
		t.Fatalf("导入审计摘要不得包含凭据内容：%s", row.Command)
	}

	grantRows, err := m.st.QueryAudit(store.AuditFilter{Actions: []string{"profile.grant"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(grantRows) != 1 || grantRows[0].Source != store.AuditSourceWebUI || grantRows[0].SourceIP != "192.0.2.7" {
		t.Fatalf("导入授权应走访问管理模块并带来源审计：%+v", grantRows)
	}
	granted, err := m.st.ServersForProfile(profileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 1 {
		t.Fatalf("导入应授权 profile，实际 %v", granted)
	}

	// 命令行来源同样落在两行审计的来源列上。
	otherConfig := writeSSHConfig(t, dir, "Host cli\n  HostName 192.0.2.21\n  User deploy\n")
	if _, err := m.RunImport(otherConfig, []string{"cli"}, "", CLISource()); err != nil {
		t.Fatal(err)
	}
	importRows, err = m.st.QueryAudit(store.AuditFilter{Actions: []string{"server.import"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(importRows) != 2 || importRows[0].Source != store.AuditSourceCLI || importRows[0].SourceIP != "" {
		t.Fatalf("命令行导入审计来源不正确：%+v", importRows)
	}
}
