package cli

import (
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// servers 三命令(owner 变更面)的审计行测试:add / rm / edit 各自在 vault
// 审计表留一行,动作名 server.add / server.rm / server.edit,摘要严格按白
// 名单——server.add 记 name/host/port/user,server.rm 记 name/host/port,
// server.edit 只记变更字段名(绝不含值),口令类参数值任何行都不出现。

// newServersAuditEnv 把 SSHMGR_STORE 钉到一个全新临时 vault(servers_test.go
// 的 newVaultEnv 形态:SSHMGR_FILEKEY_PATH 指向不存在的文件,开发机的真
// master.key 永远不会被捡到),并返回 rowsFor:直接开 vault 读回某动作名的
// 全部审计行(最新在前)。
func newServersAuditEnv(t *testing.T) (dbPath string, mk []byte, rowsFor func(t *testing.T, action string) []store.AuditRow) {
	t.Helper()
	dir := t.TempDir()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	dbPath = filepath.Join(dir, "test.db")
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(key),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
	})
	rowsFor = func(t *testing.T, action string) []store.AuditRow {
		t.Helper()
		st, err := store.Open(dbPath, key)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		rows, err := st.AuditRows(200)
		if err != nil {
			t.Fatal(err)
		}
		var out []store.AuditRow
		for _, r := range rows {
			if r.Action == action {
				out = append(out, r)
			}
		}
		return out
	}
	return dbPath, key, rowsFor
}

// auditSummary 解出审计行 Command 里的 JSON 摘要。
func auditSummary(t *testing.T, r store.AuditRow) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Command), &m); err != nil {
		t.Fatalf("audit command %q is not a JSON summary: %v", r.Command, err)
	}
	return m
}

// TestServersAuditAdd:成功 add 留一行 server.add(ok),摘要恰好是白名单四
// 字段 name/host/port/user,口令值零出现;同名重复(名称冲突)的失败 add
// 也留行,状态 error,摘要同形。
func TestServersAuditAdd(t *testing.T) {
	_, _, rowsFor := newServersAuditEnv(t)

	runCli(t, "servers", "add", "--name", "web", "--host", "192.0.2.5",
		"--port", "2222", "--user", "ops", "--password", "S3cret-pw!")

	rows := rowsFor(t, "server.add")
	if len(rows) != 1 {
		t.Fatalf("one add must leave exactly one server.add row, got %d", len(rows))
	}
	r := rows[0]
	if r.Status != "ok" {
		t.Fatalf("status must be ok, got %q", r.Status)
	}
	if r.ProjectID != "" {
		t.Fatalf("owner row must carry no project id, got %q", r.ProjectID)
	}
	m := auditSummary(t, r)
	want := map[string]any{"name": "web", "host": "192.0.2.5", "port": float64(2222), "user": "ops"}
	if len(m) != len(want) {
		t.Fatalf("summary keys must be exactly the whitelist %v, got %v", want, m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("summary[%q] = %v, want %v (full summary %v)", k, m[k], v, m)
		}
	}
	if strings.Contains(r.Command, "S3cret-pw!") {
		t.Fatalf("summary must never carry the password value: %q", r.Command)
	}

	// 失败路径:同名重复 → 命令失败 + error 行,摘要仍是白名单形状。
	runCliErr(t, "servers", "add", "--name", "web", "--host", "192.0.2.5",
		"--port", "2222", "--user", "ops", "--password", "x")
	rows = rowsFor(t, "server.add")
	if len(rows) != 2 {
		t.Fatalf("the failed add must also leave a row, got %d total", len(rows))
	}
	if rows[0].Status != "error" { // 最新在前,失败行应是最新一行
		t.Fatalf("the newest row must be the failure (status=error), got %q", rows[0].Status)
	}
	if m := auditSummary(t, rows[0]); m["name"] != "web" || m["host"] != "192.0.2.5" {
		t.Fatalf("failure row must still carry the whitelist summary, got %v", m)
	}
}

// TestServersAuditRm:按名删与按 id 删都留 server.rm 行,摘要是白名单三字段
// name/host/port;目标不存在时是幂等空操作(与 pin-clear 先例一致:什么都没
// 删的成功不留行)。
func TestServersAuditRm(t *testing.T) {
	dbPath, mk, rowsFor := newServersAuditEnv(t)

	runCli(t, "servers", "add", "--name", "web", "--host", "192.0.2.5", "--user", "u")
	runCli(t, "servers", "add", "--name", "db", "--host", "192.0.2.6", "--port", "2200", "--user", "u")

	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	web, _ := st.GetServerByName("web")
	if web == nil {
		t.Fatal("server web not found after add")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// 按 id 删:行里记的是条目自己的 name/host/port。
	runCli(t, "servers", "rm", web.ID)
	rows := rowsFor(t, "server.rm")
	if len(rows) != 1 {
		t.Fatalf("one rm must leave exactly one server.rm row, got %d", len(rows))
	}
	m := auditSummary(t, rows[0])
	want := map[string]any{"name": "web", "host": "192.0.2.5", "port": float64(22)}
	if len(m) != len(want) {
		t.Fatalf("summary keys must be exactly the whitelist %v, got %v", want, m)
	}
	for k, v := range want {
		if m[k] != v {
			t.Fatalf("summary[%q] = %v, want %v (full summary %v)", k, m[k], v, m)
		}
	}

	// 按名删:同形一行。
	runCli(t, "servers", "rm", "db")
	rows = rowsFor(t, "server.rm")
	if len(rows) != 2 || auditSummary(t, rows[0])["name"] != "db" {
		t.Fatalf("rm by name must leave a row naming the entry, got %d rows: %v", len(rows), rows)
	}

	// 目标不存在:命令成功(幂等空操作),但不留行。
	runCli(t, "servers", "rm", "no-such-entry")
	if n := len(rowsFor(t, "server.rm")); n != 2 {
		t.Fatalf("rm of an unknown target is a no-op success and must leave no row, got %d", n)
	}
}

// TestServersAuditEdit:server.edit 行只记变更字段名(name + changed),绝不
// 含值;改名行记的是改动前持久化的条目名;换凭据只出现字段名 password/key,
// 值零出现;失败(条目不存在、互斥旗标)留 error 行;--clear-credential 成功
// 行的 changed 只有 clear-credential(随行旗标按契约不生效,不算变更)。
func TestServersAuditEdit(t *testing.T) {
	_, _, rowsFor := newServersAuditEnv(t)

	runCli(t, "servers", "add", "--name", "web", "--host", "192.0.2.5", "--user", "u", "--password", "pw0")

	// 字段编辑:changed 是旗标名的字典序,值不出现。
	runCli(t, "servers", "edit", "web", "--host", "192.0.2.9", "--port", "2222", "--role", "prod api")
	rows := rowsFor(t, "server.edit")
	if len(rows) != 1 {
		t.Fatalf("one edit must leave exactly one server.edit row, got %d", len(rows))
	}
	r := rows[0]
	if r.Status != "ok" {
		t.Fatalf("status must be ok, got %q", r.Status)
	}
	m := auditSummary(t, r)
	if m["name"] != "web" {
		t.Fatalf("summary must name the edited entry, got %v", m)
	}
	changed, ok := m["changed"].([]any)
	if !ok || len(changed) != 3 || changed[0] != "host" || changed[1] != "port" || changed[2] != "role" {
		t.Fatalf(`changed must be ["host","port","role"] (field names only), got %v`, m["changed"])
	}
	if strings.Contains(r.Command, "192.0.2.9") || strings.Contains(r.Command, "prod api") {
		t.Fatalf("edit summary must carry field names, never values: %q", r.Command)
	}

	// 改名:行记改动前持久化的条目名,changed 只有字段名 name。
	runCli(t, "servers", "edit", "web", "--name", "web2")
	r = rowsFor(t, "server.edit")[0]
	m = auditSummary(t, r)
	if m["name"] != "web" {
		t.Fatalf("rename row must name the entry as persisted BEFORE the edit, got %v", m)
	}
	if changed, _ := m["changed"].([]any); len(changed) != 1 || changed[0] != "name" {
		t.Fatalf(`rename changed must be ["name"], got %v`, m["changed"])
	}

	// 换凭据:字段名 password 在 changed 里,新口令值绝不出现。
	runCli(t, "servers", "edit", "web2", "--password", "Brand-new-pw")
	r = rowsFor(t, "server.edit")[0]
	m = auditSummary(t, r)
	if changed, _ := m["changed"].([]any); len(changed) != 1 || changed[0] != "password" {
		t.Fatalf(`re-credential changed must be ["password"], got %v`, m["changed"])
	}
	if strings.Contains(r.Command, "Brand-new-pw") {
		t.Fatalf("the new password value must never appear in the summary: %q", r.Command)
	}

	// 失败:条目不存在 → error 行,changed 记本次实际传入的旗标名。
	runCliErr(t, "servers", "edit", "ghost", "--host", "x")
	r = rowsFor(t, "server.edit")[0]
	if r.Status != "error" {
		t.Fatalf("not-found edit must leave a status=error row, got %q", r.Status)
	}
	m = auditSummary(t, r)
	if m["name"] != "ghost" {
		t.Fatalf("not-found row must name the requested entry, got %v", m)
	}
	if changed, _ := m["changed"].([]any); len(changed) != 1 || changed[0] != "host" {
		t.Fatalf(`not-found changed must be ["host"], got %v`, m["changed"])
	}

	// 失败:--password 与 --key 互斥 → error 行,changed 含两个旗标名。
	runCliErr(t, "servers", "edit", "web2", "--password", "a-pw", "--key", "not-a-real-key-file")
	r = rowsFor(t, "server.edit")[0]
	if r.Status != "error" {
		t.Fatalf("mutex edit must leave a status=error row, got %q", r.Status)
	}
	m = auditSummary(t, r)
	if changed, _ := m["changed"].([]any); len(changed) != 2 || changed[0] != "key" || changed[1] != "password" {
		t.Fatalf(`mutex changed must be ["key","password"], got %v`, m["changed"])
	}
	if strings.Contains(r.Command, "a-pw") {
		t.Fatalf("the attempted password value must never appear: %q", r.Command)
	}

	// --clear-credential 成功:changed 只有 clear-credential。
	runCli(t, "servers", "edit", "web2", "--clear-credential")
	r = rowsFor(t, "server.edit")[0]
	if r.Status != "ok" {
		t.Fatalf("clear-credential success must be status=ok, got %q", r.Status)
	}
	m = auditSummary(t, r)
	if changed, _ := m["changed"].([]any); len(changed) != 1 || changed[0] != "clear-credential" {
		t.Fatalf(`clear-credential changed must be ["clear-credential"], got %v`, m["changed"])
	}
}
