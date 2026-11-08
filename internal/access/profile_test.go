package access

import (
	"errors"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newTestManager 打开一个全新的临时加密库，并返回挂在它上面的访问管理
// 模块与存储句柄（存储句柄只用于按测试意图准备数据和读回最终状态）。
func newTestManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st), st
}

// mustServer 新增一台带密码凭据的服务器，返回它的名字与标识符。
func mustServer(t *testing.T, st *store.Store, name string) models.Server {
	t.Helper()
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddServer(&models.Server{
		Name: name, Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	if err != nil {
		t.Fatal(err)
	}
	return models.Server{ID: id, Name: name}
}

// mustGrant 直接经存储层授予授权（准备数据用，被测行为不经过这里）。
func mustGrant(t *testing.T, st *store.Store, profileID string, serverIDs ...string) {
	t.Helper()
	if err := st.GrantServers(profileID, serverIDs); err != nil {
		t.Fatal(err)
	}
}

// TestListProfilesShowsGrantsAndReferences 钉住 profile 列表的完整形状：
// 每个 profile 带授权服务器清单，以及引用它的项目令牌与设备码（含状态，
// 供页面区分仍在使用与已吊销）。
func TestListProfilesShowsGrantsAndReferences(t *testing.T) {
	m, st := newTestManager(t)
	db := mustServer(t, st, "db1")
	web := mustServer(t, st, "web1")
	ops, err := st.AddProfile("ops")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddProfile("dev"); err != nil {
		t.Fatal(err)
	}
	mustGrant(t, st, ops, web.ID, db.ID)
	if _, _, err := st.AddProject("proj-web", ops); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop", ops); err != nil {
		t.Fatal(err)
	}

	profiles, err := m.ListProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("profile 数量 = %d，应为 2：%+v", len(profiles), profiles)
	}
	var got *ProfileSummary
	for i := range profiles {
		if profiles[i].Name == "ops" {
			got = &profiles[i]
		}
	}
	if got == nil {
		t.Fatalf("列表里找不到 ops：%+v", profiles)
	}
	if len(got.Servers) != 2 || got.Servers[0].Name != "db1" || got.Servers[1].Name != "web1" {
		t.Fatalf("ops 授权服务器 = %+v，应按名字排序为 db1、web1", got.Servers)
	}
	if got.Servers[0].ID != db.ID || got.Servers[1].ID != web.ID {
		t.Fatalf("ops 授权服务器标识符 = %+v，应与存储一致", got.Servers)
	}
	if len(got.Projects) != 1 || got.Projects[0].Name != "proj-web" || got.Projects[0].Status != models.ProjectActive {
		t.Fatalf("ops 引用的项目令牌 = %+v，应为 active 的 proj-web", got.Projects)
	}
	if len(got.DeviceCodes) != 1 || got.DeviceCodes[0].Name != "laptop" || got.DeviceCodes[0].Status != models.CacheTokenActive {
		t.Fatalf("ops 引用的设备码 = %+v，应为 active 的 laptop", got.DeviceCodes)
	}
	for i := range profiles {
		if profiles[i].Name == "dev" && (len(profiles[i].Servers) != 0 || len(profiles[i].Projects) != 0 || len(profiles[i].DeviceCodes) != 0) {
			t.Fatalf("dev 不应有授权与引用者：%+v", profiles[i])
		}
	}
}

// ownerAuditRows 读回指定动作的全部 owner 审计行（最新在前）。
func ownerAuditRows(t *testing.T, st *store.Store, actions ...string) []store.AuditRow {
	t.Helper()
	rows, err := st.QueryAudit(store.AuditFilter{OwnerOnly: true, Actions: actions})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// assertAuditShape 钉住访问管理模块写出的 owner 审计行形状：动作、状态、
// 白名单摘要逐字一致，来源为命令行或 Web UI（带来源地址）。
func assertAuditShape(t *testing.T, r store.AuditRow, action, status, command, source, sourceIP string) {
	t.Helper()
	if r.Action != action || r.Status != status || r.Command != command {
		t.Fatalf("审计行 = %s/%s/%s，应为 %s/%s/%s", r.Action, r.Status, r.Command, action, status, command)
	}
	if r.Source != source || r.SourceIP != sourceIP {
		t.Fatalf("审计来源 = %q/%q，应为 %q/%q", r.Source, r.SourceIP, source, sourceIP)
	}
}

// TestAddProfileWritesAudit 新增与名称重复失败都留一条 profile.add 审计行，
// 成功行走命令行来源，失败行走 Web UI 来源并带来源地址。
func TestAddProfileWritesAudit(t *testing.T) {
	m, st := newTestManager(t)
	id, err := m.AddProfile("team-a", CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("新增 profile 应返回标识符")
	}
	if _, err := m.AddProfile("team-a", WebUISource("192.0.2.7")); err == nil {
		t.Fatal("名称重复的新增应报错")
	}

	rows := ownerAuditRows(t, st, "profile.add")
	if len(rows) != 2 {
		t.Fatalf("profile.add 审计行数量 = %d，应为 2：%+v", len(rows), rows)
	}
	assertAuditShape(t, rows[0], "profile.add", "error", `{"name":"team-a"}`, store.AuditSourceWebUI, "192.0.2.7")
	assertAuditShape(t, rows[1], "profile.add", "ok", `{"name":"team-a"}`, store.AuditSourceCLI, "")
}

// TestGrantServersIdempotentAndAudited 授予是幂等追加：重复授予不新增授
// 权行但仍写审计；服务器可按名字或标识符解析；profile 或服务器不存在时
// 整体报错并写失败审计行。
func TestGrantServersIdempotentAndAudited(t *testing.T) {
	m, st := newTestManager(t)
	db := mustServer(t, st, "db1")
	mustServer(t, st, "web1")
	pid, err := m.AddProfile("team-a", CLISource())
	if err != nil {
		t.Fatal(err)
	}

	granted, err := m.GrantServers("team-a", []string{"web1", "db1"}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 2 || granted[0].Name != "web1" || granted[1].Name != "db1" {
		t.Fatalf("授予结果 = %+v，应为 web1、db1", granted)
	}
	// 重复授予同一台：存储仍只有一条授权行。
	if _, err := m.GrantServers("team-a", []string{"web1"}, WebUISource("192.0.2.7")); err != nil {
		t.Fatal(err)
	}
	ids, err := st.ServersForProfile(pid)
	if err != nil || len(ids) != 2 {
		t.Fatalf("重复授予后授权行 = %v（错误 %v），应仍为 2 行", ids, err)
	}
	// 按标识符解析：命令行与 Web UI 表单都能用同一个入口。
	if _, err := m.GrantServers("team-a", []string{db.ID}, CLISource()); err != nil {
		t.Fatal(err)
	}

	// profile 不存在：报错、写失败行、不产生授权。
	if _, err := m.GrantServers("ghost", []string{"web1"}, CLISource()); err == nil {
		t.Fatal("profile 不存在时应报错")
	}
	// 服务器不存在：报错、写失败行、不产生授权。
	if _, err := m.GrantServers("team-a", []string{"web1", "no-such"}, CLISource()); err == nil {
		t.Fatal("服务器不存在时应报错")
	}
	ids, err = st.ServersForProfile(pid)
	if err != nil || len(ids) != 2 {
		t.Fatalf("失败授予不应改变授权行，实际 = %v（错误 %v）", ids, err)
	}

	rows := ownerAuditRows(t, st, "profile.grant")
	if len(rows) != 5 {
		t.Fatalf("profile.grant 审计行数量 = %d，应为 5：%+v", len(rows), rows)
	}
	assertAuditShape(t, rows[0], "profile.grant", "error", `{"profile":"team-a","servers":"2台:web1,no-such"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, rows[1], "profile.grant", "error", `{"profile":"ghost","servers":"1台:web1"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, rows[2], "profile.grant", "ok", `{"profile":"team-a","servers":"1台:`+db.ID+`"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, rows[3], "profile.grant", "ok", `{"profile":"team-a","servers":"1台:web1"}`, store.AuditSourceWebUI, "192.0.2.7")
	assertAuditShape(t, rows[4], "profile.grant", "ok", `{"profile":"team-a","servers":"2台:web1,db1"}`, store.AuditSourceCLI, "")
}

// TestRevokeServersAndSnapshot 撤销已授权的服务器会删除授权行并写
// profile.revoke 审计；未授权的服务器跳过不算错误；撤销后该 profile 的
// 下一次快照导出里不再有这台服务器。
func TestRevokeServersAndSnapshot(t *testing.T) {
	m, st := newTestManager(t)
	web := mustServer(t, st, "web1")
	mustServer(t, st, "db1")
	pid, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	mustGrant(t, st, pid, web.ID)

	res, err := m.RevokeServers("team-a", []string{"web1"}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Revoked) != 1 || res.Revoked[0].Name != "web1" || len(res.Skipped) != 0 {
		t.Fatalf("撤销结果 = %+v，应只撤销 web1", res)
	}
	ids, err := st.ServersForProfile(pid)
	if err != nil || len(ids) != 0 {
		t.Fatalf("撤销后授权行 = %v（错误 %v），应为空", ids, err)
	}
	snap, err := st.ExportSnapshotForProfile(pid, false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, srv := range snap.Servers {
		if srv.ID == web.ID {
			t.Fatalf("撤销后的快照仍含 web1：%+v", snap.Servers)
		}
	}

	// 未授权的服务器：跳过、不算错误，但请求仍写审计行。
	res, err = m.RevokeServers("team-a", []string{"db1"}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Revoked) != 0 || len(res.Skipped) != 1 || res.Skipped[0].Name != "db1" {
		t.Fatalf("未授权撤销结果 = %+v，应跳过 db1", res)
	}

	// profile 不存在与服务器不存在：报错并写失败审计行。
	if _, err := m.RevokeServers("ghost", []string{"web1"}, CLISource()); err == nil {
		t.Fatal("profile 不存在时应报错")
	}
	if _, err := m.RevokeServers("team-a", []string{"no-such"}, CLISource()); err == nil {
		t.Fatal("服务器不存在时应报错")
	}

	rows := ownerAuditRows(t, st, "profile.revoke")
	if len(rows) != 4 {
		t.Fatalf("profile.revoke 审计行数量 = %d，应为 4：%+v", len(rows), rows)
	}
	assertAuditShape(t, rows[0], "profile.revoke", "error", `{"profile":"team-a","servers":"1台:no-such"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, rows[1], "profile.revoke", "error", `{"profile":"ghost","servers":"1台:web1"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, rows[2], "profile.revoke", "ok", `{"profile":"team-a","servers":"1台:db1"}`, store.AuditSourceCLI, "")
	assertAuditShape(t, rows[3], "profile.revoke", "ok", `{"profile":"team-a","servers":"1台:web1"}`, store.AuditSourceCLI, "")
}

// TestDeleteProfileRefusedWhileReferenced 仍被未吊销项目令牌或活跃设备码
// 引用时拒绝删除并返回引用者清单；引用全部退场后可删除。
func TestDeleteProfileRefusedWhileReferenced(t *testing.T) {
	m, st := newTestManager(t)
	web := mustServer(t, st, "web1")
	pid, err := st.AddProfile("ops")
	if err != nil {
		t.Fatal(err)
	}
	mustGrant(t, st, pid, web.ID)
	projID, _, err := st.AddProject("proj-web", pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.AddCacheToken("laptop", pid); err != nil {
		t.Fatal(err)
	}

	var blocked *ProfileInUseError
	err = m.DeleteProfile("ops", CLISource())
	if !errors.As(err, &blocked) {
		t.Fatalf("被项目引用的删除应返回引用者错误，实际 %v", err)
	}
	if len(blocked.Projects) != 1 || blocked.Projects[0] != "proj-web" || len(blocked.DeviceCodes) != 1 || blocked.DeviceCodes[0] != "laptop" {
		t.Fatalf("引用者清单 = %+v，应列 proj-web 与 laptop", blocked)
	}
	if profiles, _ := st.ListProfiles(); len(profiles) != 1 {
		t.Fatalf("被拒绝的删除不应生效，剩余 profile = %+v", profiles)
	}

	// 项目吊销后不再阻塞；活跃设备码仍阻塞。
	if err := st.SetProjectStatus(projID, models.ProjectRevoked); err != nil {
		t.Fatal(err)
	}
	err = m.DeleteProfile("ops", CLISource())
	if !errors.As(err, &blocked) {
		t.Fatalf("被活跃设备码引用的删除应返回引用者错误，实际 %v", err)
	}
	if len(blocked.Projects) != 0 || len(blocked.DeviceCodes) != 1 || blocked.DeviceCodes[0] != "laptop" {
		t.Fatalf("引用者清单 = %+v，应只列 laptop", blocked)
	}

	// 设备码吊销后可删除；profile 与授权行一并消失。
	if err := st.RevokeCacheToken("laptop"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteProfile("ops", CLISource()); err != nil {
		t.Fatal(err)
	}
	if profiles, _ := st.ListProfiles(); len(profiles) != 0 {
		t.Fatalf("删除后不应剩 profile：%+v", profiles)
	}
	if ids, _ := st.ServersForProfile(pid); len(ids) != 0 {
		t.Fatalf("删除后不应剩授权行：%v", ids)
	}

	rows := ownerAuditRows(t, st, "profile.rm")
	if len(rows) != 3 {
		t.Fatalf("profile.rm 审计行数量 = %d，应为 3：%+v", len(rows), rows)
	}
	assertAuditShape(t, rows[0], "profile.rm", "ok", `{"name":"ops"}`, store.AuditSourceCLI, "")
	if rows[1].Status != "error" || rows[2].Status != "error" {
		t.Fatalf("两次被拒的删除应留 error 行：%+v", rows)
	}

	// 不存在的 profile：报错并写失败审计行。
	if err := m.DeleteProfile("ghost", CLISource()); err == nil {
		t.Fatal("profile 不存在时应报错")
	}
	rows = ownerAuditRows(t, st, "profile.rm")
	if len(rows) != 4 || rows[0].Status != "error" {
		t.Fatalf("不存在的删除应补一条 error 行：%+v", rows[0])
	}
}

// TestSetServerGrantsAppliesDiff 按完整集合调整：expected 与当前一致时按
// 差异逐项授予/撤销，返回新增与撤销清单；审计拆成一条 profile.grant 和
// 一条 profile.revoke，没有变化的一侧不写。
func TestSetServerGrantsAppliesDiff(t *testing.T) {
	m, st := newTestManager(t)
	web := mustServer(t, st, "web1")
	db := mustServer(t, st, "db1")
	pid, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	mustGrant(t, st, pid, web.ID)

	res, err := m.SetServerGrants("team-a", []string{web.ID}, []string{web.ID, db.ID}, WebUISource("192.0.2.7"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 || res.Added[0].Name != "db1" || len(res.Removed) != 0 {
		t.Fatalf("调整结果 = %+v，应只新增 db1", res)
	}
	if ids, _ := st.ServersForProfile(pid); len(ids) != 2 {
		t.Fatalf("调整后授权行 = %v，应为 2 行", ids)
	}

	// 反向调整：取消 web1，只留 db1。
	res, err = m.SetServerGrants("team-a", []string{web.ID, db.ID}, []string{db.ID}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 || len(res.Removed) != 1 || res.Removed[0].Name != "web1" {
		t.Fatalf("调整结果 = %+v，应只撤销 web1", res)
	}
	if ids, _ := st.ServersForProfile(pid); len(ids) != 1 || ids[0] != db.ID {
		t.Fatalf("调整后授权行 = %v，应只剩 db1", ids)
	}

	grantRows := ownerAuditRows(t, st, "profile.grant")
	revokeRows := ownerAuditRows(t, st, "profile.revoke")
	if len(grantRows) != 1 {
		t.Fatalf("集合调整的 profile.grant 行 = %d，应为 1：%+v", len(grantRows), grantRows)
	}
	if len(revokeRows) != 1 {
		t.Fatalf("集合调整的 profile.revoke 行 = %d，应为 1：%+v", len(revokeRows), revokeRows)
	}
	assertAuditShape(t, grantRows[0], "profile.grant", "ok", `{"profile":"team-a","servers":"1台:db1"}`, store.AuditSourceWebUI, "192.0.2.7")
	assertAuditShape(t, revokeRows[0], "profile.revoke", "ok", `{"profile":"team-a","servers":"1台:web1"}`, store.AuditSourceCLI, "")
}

// TestSetServerGrantsConflictAndRevision 打开表单后被改过时拒绝并返回最新
// 集合；没有变化时不写入、快照修订号不变；有变化时快照修订号前进。
func TestSetServerGrantsConflictAndRevision(t *testing.T) {
	m, st := newTestManager(t)
	web := mustServer(t, st, "web1")
	db := mustServer(t, st, "db1")
	pid, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	mustGrant(t, st, pid, web.ID)
	revAfterSeed, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}

	// 没有变化：不写入，快照修订号不变。
	res, err := m.SetServerGrants("team-a", []string{web.ID}, []string{web.ID}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 || len(res.Removed) != 0 {
		t.Fatalf("无变化结果 = %+v", res)
	}
	rev, err := st.SnapshotRevision()
	if err != nil || rev != revAfterSeed {
		t.Fatalf("无变化提交后快照修订号 = %d（错误 %v），应保持 %d", rev, err, revAfterSeed)
	}

	// 打开表单后别的入口改过：拒绝提交并返回最新集合。
	mustGrant(t, st, pid, db.ID)
	var conflict *GrantSetConflictError
	_, err = m.SetServerGrants("team-a", []string{web.ID}, []string{db.ID}, CLISource())
	if !errors.As(err, &conflict) {
		t.Fatalf("被改过的提交应返回冲突错误，实际 %v", err)
	}
	if len(conflict.Current) != 2 {
		t.Fatalf("冲突应带最新集合，实际 %+v", conflict.Current)
	}
	revAfterConflict, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	// 冲突与无变化都不写审计行。
	if rows := ownerAuditRows(t, st, "profile.grant", "profile.revoke"); len(rows) != 0 {
		t.Fatalf("冲突与无变化不应写授权审计行：%+v", rows)
	}

	// 有变化：快照修订号前进。
	if _, err := m.SetServerGrants("team-a", []string{web.ID, db.ID}, []string{db.ID}, CLISource()); err != nil {
		t.Fatal(err)
	}
	revAfterChange, err := st.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if revAfterChange <= revAfterConflict {
		t.Fatalf("有变化的提交应推进快照修订号：%d -> %d", revAfterConflict, revAfterChange)
	}
	if ids, _ := st.ServersForProfile(pid); len(ids) != 1 || ids[0] != db.ID {
		t.Fatalf("调整后授权行 = %v，应只剩 db1", ids)
	}

	// profile 不存在与 desired 里的未知服务器：报错且不改数据。
	if _, err := m.SetServerGrants("ghost", []string{web.ID}, []string{db.ID}, CLISource()); err == nil {
		t.Fatal("profile 不存在时应报错")
	}
	if _, err := m.SetServerGrants("team-a", []string{db.ID}, []string{db.ID, "no-such"}, CLISource()); err == nil {
		t.Fatal("desired 含未知服务器时应报错")
	}
	if ids, _ := st.ServersForProfile(pid); len(ids) != 1 || ids[0] != db.ID {
		t.Fatalf("失败提交不应改变授权行：%v", ids)
	}
}

// TestRevokeServersByIdentifier 撤销与授予共用同一套解析：服务器可以按
// 标识符传入（Web UI 表单与脚本都会用标识符）。
func TestRevokeServersByIdentifier(t *testing.T) {
	m, st := newTestManager(t)
	web := mustServer(t, st, "web1")
	pid, err := st.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	mustGrant(t, st, pid, web.ID)

	res, err := m.RevokeServers("team-a", []string{web.ID}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Revoked) != 1 || res.Revoked[0].ID != web.ID || res.Revoked[0].Name != "web1" {
		t.Fatalf("按标识符撤销结果 = %+v", res)
	}
	if ids, _ := st.ServersForProfile(pid); len(ids) != 0 {
		t.Fatalf("撤销后不应剩授权行：%v", ids)
	}
}
