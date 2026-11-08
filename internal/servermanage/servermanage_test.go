package servermanage

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newTestManager 打开一个真实临时 vault 并挂上服务器管理模块；测试只通过
// 模块接口操作，不直接改模块内部状态。
func newTestManager(t *testing.T) *Manager {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(st)
}

func mustAddServer(t *testing.T, m *Manager, srv *models.Server) string {
	t.Helper()
	id, err := m.st.AddServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func reasonsOf(items []ListItem, name string) []string {
	for _, item := range items {
		if item.Server.Name == name {
			return item.AttentionReasons
		}
	}
	return nil
}

// TestListAttentionReasonTruthTable 钉住「需要注意」原因的真值表：未挂凭
// 据、未设角色、待补私钥口令；缺 sudo 密码或缺其他服务器元数据字段不报警。
func TestListAttentionReasonTruthTable(t *testing.T) {
	m := newTestManager(t)
	cid, err := m.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	mustAddServer(t, m, &models.Server{Name: "fine", Host: "192.0.2.1", Port: 22, User: "u",
		CredentialID: cid, Role: "生产数据库", SudoCredentialID: ""})
	mustAddServer(t, m, &models.Server{Name: "no-cred", Host: "192.0.2.2", Port: 22, User: "u", Role: "r"})
	mustAddServer(t, m, &models.Server{Name: "no-role", Host: "192.0.2.3", Port: 22, User: "u", CredentialID: cid})
	mustAddServer(t, m, &models.Server{Name: "need-pass", Host: "192.0.2.4", Port: 22, User: "u",
		CredentialID: cid, Role: "r", Tags: []string{models.TagNeedsPassphrase}})

	all, err := m.List(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("列表应有 4 台服务器，实际 %d", len(all))
	}
	if got := reasonsOf(all, "fine"); len(got) != 0 {
		t.Fatalf("完整服务器不应有注意原因，实际 %v", got)
	}
	if got := reasonsOf(all, "no-cred"); len(got) != 1 || got[0] != ReasonNoCredential {
		t.Fatalf("未挂凭据的原因 = %v", got)
	}
	if got := reasonsOf(all, "no-role"); len(got) != 1 || got[0] != ReasonNoRole {
		t.Fatalf("未设角色的原因 = %v", got)
	}
	if got := reasonsOf(all, "need-pass"); len(got) != 1 || got[0] != ReasonNeedsPassphrase {
		t.Fatalf("待补私钥口令的原因 = %v", got)
	}

	attention, err := m.List(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(attention) != 3 {
		t.Fatalf("只看需要注意应剩 3 台，实际 %d", len(attention))
	}
	for _, item := range attention {
		if item.Server.Name == "fine" {
			t.Fatal("完整服务器不应出现在需要注意视图")
		}
	}
}

// TestDetailShowsNonSensitiveFacts 校验详情按名字或标识符解析，并给出凭据
// 状态、所属设备与注意原因，绝不包含凭据内容。
func TestDetailShowsNonSensitiveFacts(t *testing.T) {
	m := newTestManager(t)
	cid, err := m.st.SetCredential(&models.Credential{
		Type: models.CredPrivateKey, Secret: []byte("PRIVATE KEY BODY"), Passphrase: []byte("key-pass"),
	})
	if err != nil {
		t.Fatal(err)
	}
	sudoID, err := m.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pass")})
	if err != nil {
		t.Fatal(err)
	}
	id := mustAddServer(t, m, &models.Server{
		Name: "web", Host: "192.0.2.10", Port: 2222, User: "deploy",
		CredentialID: cid, SudoCredentialID: sudoID, Role: "网站", Tags: []string{"gpu"},
		Location: "机房 A", ExposeHost: true, ShellType: models.ShellPOSIX,
	})

	detail, err := m.Detail(id)
	if err != nil {
		t.Fatal(err)
	}
	if detail == nil || detail.Server.ID != id {
		t.Fatalf("按标识符应找到服务器，实际 %+v", detail)
	}
	if detail.CredentialType != models.CredPrivateKey || !detail.HasKeyPassphrase || !detail.HasSudoPassword {
		t.Fatalf("凭据摘要不正确：%+v", detail)
	}
	if detail.Server.CredentialID != cid || detail.Server.SudoCredentialID != sudoID {
		t.Fatalf("详情应保留标识符： %+v", detail.Server)
	}

	byName, err := m.Detail("web")
	if err != nil {
		t.Fatal(err)
	}
	if byName == nil || byName.Server.ID != id {
		t.Fatalf("按名字应找到同一台服务器：%+v", byName)
	}
	missing, err := m.Detail("missing")
	if err != nil || missing != nil {
		t.Fatalf("不存在的服务器应返回 nil，实际 detail=%+v err=%v", missing, err)
	}
}

func auditActions(t *testing.T, m *Manager) []store.AuditRow {
	t.Helper()
	rows, err := m.st.QueryAudit(store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestEditPartialUpdateAndRevisionLock 校验部分更新语义、修订号乐观锁与审
// 计：只提交的字段写入；修订号不一致时拒绝并返回当前值；审计带来源与字段
// 名、不带字段值。
func TestEditPartialUpdateAndRevisionLock(t *testing.T) {
	m := newTestManager(t)
	id := mustAddServer(t, m, &models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy", Role: "旧角色",
		Location: "机房 A",
	})
	before, err := m.st.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}

	role := "网站前端"
	result, err := m.Edit(EditRequest{
		Identifier: "web", Role: &role,
		ExpectedRevision: before.Revision, CheckRevision: true,
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if result.Server.Role != "网站前端" || result.Server.Location != "机房 A" {
		t.Fatalf("部分更新不得清空未提交字段：%+v", result.Server)
	}
	if result.Server.Revision != before.Revision+1 {
		t.Fatalf("编辑后修订号应加一：%d -> %d", before.Revision, result.Server.Revision)
	}
	wantChanged := []string{"role"}
	if len(result.ChangedFields) != len(wantChanged) || result.ChangedFields[0] != wantChanged[0] {
		t.Fatalf("变更字段 = %v", result.ChangedFields)
	}

	stale := before.Revision
	_, err = m.Edit(EditRequest{
		Identifier: "web", Role: &role, ExpectedRevision: stale, CheckRevision: true,
	}, CLISource())
	var conflict *RevisionConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("修订号不一致应返回冲突错误，实际 %v", err)
	}
	if conflict.Current == nil || conflict.Current.Revision != before.Revision+1 {
		t.Fatalf("冲突错误应带当前值：%+v", conflict.Current)
	}

	rows := auditActions(t, m)
	if len(rows) < 1 || rows[len(rows)-1].Action != "server.edit" || rows[len(rows)-1].Source != store.AuditSourceCLI {
		t.Fatalf("编辑审计缺失或来源不对：%+v", rows)
	}
}

// TestEditCredentialReplaceCleansTagAndOldCredential 校验更换登录凭据后移除
// 待补私钥口令标签，且旧凭据在无其他引用时被清理。
func TestEditCredentialReplaceCleansTagAndOldCredential(t *testing.T) {
	m := newTestManager(t)
	id := mustAddServer(t, m, &models.Server{
		Name: "key-host", Host: "192.0.2.10", Port: 22, User: "u",
		Tags: []string{models.TagNeedsPassphrase, "gpu"},
	})
	srv, err := m.st.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	oldCred, err := m.st.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: []byte("old-key")})
	if err != nil {
		t.Fatal(err)
	}
	srv.AuthMethod, srv.CredentialID = models.AuthPrivateKey, oldCred
	if err := m.st.UpdateServer(srv); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Edit(EditRequest{Identifier: "key-host", Password: strPtr("new-pass")}, CLISource()); err != nil {
		t.Fatal(err)
	}
	after, err := m.st.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Tags) != 1 || after.Tags[0] != "gpu" {
		t.Fatalf("更换凭据后应移除待补口令标签并保留其他标签：%v", after.Tags)
	}
	if after.AuthMethod != models.AuthPassword {
		t.Fatalf("凭据类型应换成密码：%v", after.AuthMethod)
	}
	if _, _, found, err := m.st.CredentialSummary(oldCred); err != nil || found {
		t.Fatalf("无引用的旧凭据应被删除：found=%v err=%v", found, err)
	}
}

// errorsAs 是测试里的小包装，避免每个用例重复 import errors。

// genEncryptedKey 生成一把口令加密的测试私钥，返回 PEM 内容。
func genEncryptedKey(t *testing.T, passphrase string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	block, err := x509.EncryptPEMBlock(rand.Reader, "PRIVATE KEY", der, []byte(passphrase), x509.PEMCipherAES256)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(block)
}

// TestSupplementKeyPassphraseSharedCredential 校验只补私钥口令：错误口令被拒
// 且不写入；正确口令写进凭据，共用同一份私钥的多台服务器同时去掉待补口令
// 标签，每台修订号加一。
func TestSupplementKeyPassphraseSharedCredential(t *testing.T) {
	m := newTestManager(t)
	keyPEM := genEncryptedKey(t, "secret-pass")
	credID, err := m.st.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: keyPEM})
	if err != nil {
		t.Fatal(err)
	}
	firstID := mustAddServer(t, m, &models.Server{
		Name: "alpha", Host: "192.0.2.1", Port: 22, User: "u",
		CredentialID: credID, Role: "r", Tags: []string{models.TagNeedsPassphrase},
	})
	secondID := mustAddServer(t, m, &models.Server{
		Name: "beta", Host: "192.0.2.2", Port: 22, User: "u",
		CredentialID: credID, Role: "r", Tags: []string{models.TagNeedsPassphrase, "gpu"},
	})
	firstBefore, secondBefore := serverRevision(t, m, firstID), serverRevision(t, m, secondID)

	if _, err := m.SupplementKeyPassphrase("alpha", "wrong-pass", CLISource()); err == nil {
		t.Fatal("错误口令应被拒绝")
	}
	cred, err := m.st.GetCredential(credID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cred.Passphrase) != 0 {
		t.Fatal("错误口令不得写进凭据")
	}

	result, err := m.SupplementKeyPassphrase("alpha", "secret-pass", WebUISource("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.AffectedServers) != 2 || result.AffectedServers[0] != "alpha" || result.AffectedServers[1] != "beta" {
		t.Fatalf("受影响服务器 = %v", result.AffectedServers)
	}
	cred, err = m.st.GetCredential(credID)
	if err != nil {
		t.Fatal(err)
	}
	if string(cred.Passphrase) != "secret-pass" {
		t.Fatal("正确口令应写进凭据")
	}
	for _, id := range []string{firstID, secondID} {
		srv, err := m.st.GetServer(id)
		if err != nil {
			t.Fatal(err)
		}
		if models.ServerHasTag(srv.Tags, models.TagNeedsPassphrase) {
			t.Fatalf("%s 的待补口令标签应被移除：%v", srv.Name, srv.Tags)
		}
	}
	if beta, _ := m.st.GetServer(secondID); len(beta.Tags) != 1 || beta.Tags[0] != "gpu" {
		t.Fatalf("beta 应保留其他标签：%v", beta.Tags)
	}
	if got := serverRevision(t, m, firstID); got != firstBefore+1 {
		t.Fatalf("alpha 修订号应加一：%d -> %d", firstBefore, got)
	}
	if got := serverRevision(t, m, secondID); got != secondBefore+1 {
		t.Fatalf("beta 修订号应加一：%d -> %d", secondBefore, got)
	}
	var last store.AuditRow
	for _, row := range auditActions(t, m) {
		if row.Status == "ok" {
			last = row
		}
	}
	if last.Action != "server.edit" || last.Source != store.AuditSourceWebUI || last.SourceIP != "127.0.0.1" {
		t.Fatalf("补口令审计来源不正确：%+v", last)
	}
	if strings.Contains(last.Command, "secret-pass") {
		t.Fatalf("审计摘要不得包含口令：%s", last.Command)
	}
}

func serverRevision(t *testing.T, m *Manager, id string) int64 {
	t.Helper()
	srv, err := m.st.GetServer(id)
	if err != nil || srv == nil {
		t.Fatalf("读取服务器 %s 失败：%v", id, err)
	}
	return srv.Revision
}

func strPtr(s string) *string { return &s }

// TestSharedCredentialServersListsPeers 补私钥口令前要列出共用同一份登录凭
// 据的全部服务器：按名字排序、含目标本身、不混入 sudo 凭据或其他凭据。
func TestSharedCredentialServersListsPeers(t *testing.T) {
	m := newTestManager(t)
	cred, err := m.st.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: []byte("KEY")})
	if err != nil {
		t.Fatal(err)
	}
	other, err := m.st.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: []byte("OTHER")})
	if err != nil {
		t.Fatal(err)
	}
	sudo, err := m.st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("sudo")})
	if err != nil {
		t.Fatal(err)
	}
	zeta := mustAddServer(t, m, &models.Server{Name: "zeta", Host: "192.0.2.1", Port: 22, User: "u", CredentialID: cred})
	mustAddServer(t, m, &models.Server{Name: "alpha", Host: "192.0.2.2", Port: 22, User: "u", CredentialID: cred})
	mustAddServer(t, m, &models.Server{Name: "elsewhere", Host: "192.0.2.3", Port: 22, User: "u", CredentialID: other})
	mustAddServer(t, m, &models.Server{Name: "sudo-peer", Host: "192.0.2.4", Port: 22, User: "u", SudoCredentialID: sudo})

	names, err := m.SharedCredentialServers(zeta)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "alpha,zeta" {
		t.Fatalf("共用凭据服务器 = %v", names)
	}
}

// TestEditCombinesKeyPassphraseWithOtherFields 编辑页把「只补私钥口令」和其
// 他字段一起提交时，同一请求要同时生效：口令写进凭据并清理共用服务器的标
// 签，其他字段也保存。
func TestEditCombinesKeyPassphraseWithOtherFields(t *testing.T) {
	m := newTestManager(t)
	keyPEM := genEncryptedKey(t, "secret-pass")
	cred, err := m.st.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: keyPEM})
	if err != nil {
		t.Fatal(err)
	}
	target, err := m.st.AddServer(&models.Server{
		Name: "enc", Host: "192.0.2.3", Port: 22, User: "root", Role: "旧角色",
		CredentialID: cred, Tags: []string{models.TagNeedsPassphrase},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.AddServer(&models.Server{
		Name: "peer", Host: "192.0.2.4", Port: 22, User: "root", CredentialID: cred,
		Role: "同伴", Tags: []string{models.TagNeedsPassphrase},
	}); err != nil {
		t.Fatal(err)
	}

	newRole := "新角色"
	result, err := m.Edit(EditRequest{
		Identifier: target, Role: &newRole, KeyPassphrase: strPtr("secret-pass"),
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := m.st.GetServerByName("enc")
	if srv == nil || srv.Role != "新角色" || models.ServerHasTag(srv.Tags, models.TagNeedsPassphrase) {
		t.Fatalf("组合提交应同时保存角色与私钥口令：%+v", srv)
	}
	savedCred, _ := m.st.GetCredential(srv.CredentialID)
	if savedCred == nil || string(savedCred.Passphrase) != "secret-pass" {
		t.Fatalf("口令应写进凭据：%+v", savedCred)
	}
	peer, _ := m.st.GetServerByName("peer")
	if models.ServerHasTag(peer.Tags, models.TagNeedsPassphrase) {
		t.Fatalf("共用凭据的服务器应一起去掉待补标签：%+v", peer)
	}
	if len(result.AffectedServers) != 2 {
		t.Fatalf("结果应列出受影响服务器：%v", result.AffectedServers)
	}
}
