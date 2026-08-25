# Plan 35 · tunnels 硬化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** backlog #15——owner 急停（vault DB kill 单 + revoke/disable 级联）、活动感知回收（落实 Touch）、listen_host 白名单（非环回 bind 受控放行）。

**Architecture:** 三张新 vault 表（`forward_bind_hosts`/`tunnel_orders`/`tunnel_registry`）做 owner↔broker 的既有通道；TunnelManager 加 15s 控制循环（订单状态机/级联/白名单存量复查/lease 心跳，全部幂等）；`ForwardLocal` 加 listenHost 参数与 onActivity 活动钩子（30s 原子节流）。

**Tech Stack:** Go 1.x · modernc.org/sqlite（既有）· golang.org/x/crypto/ssh（既有）· cobra（既有）· google/uuid（既有，tunnel id 不变）。

**Spec:** `docs/superpowers/specs/2026-08-25-plan-35-tunnels-hardening-design.md.rev4.md`（定稿——执行者必读，本计划从它论证；引用记 §N）。

## Global Constraints

- **IP 规范化**：add/rm/gate 三处统一 `net.ParseIP(x).String()` 规范形比对（共用 `store.CanonicalBindIP`）；环回恒允许（无表项）；`0.0.0.0`/`::`（`IsUnspecified`）与 hostname/带 zone 输入一律拒（spec §2）。
- **审计口径**：白名单越权拒绝 `status="bind_denied"`；gate 读失败 `status="error"`；forward 行 `Command = "host:port id=<tunnelID>"`（spec §2/§7）。
- **时间常数**：控制 tick 15s；Touch 节流 30s；CLI 轮询 45s；续约/执法失败阈值 >8 tick；空闲回收 10 min（不变）；registry GC 30 min；orders 行清理 7d（applied；pending 需目标缺席）；心跳陈旧标注阈值 45s（spec §3/§4/§6）。
- **日志原文字**：`lease renewal failed %d ticks — closed %d tunnels` / `enforcement degraded: cascade read failed %d ticks — closed %d tunnels` / `enforcement degraded: whitelist read failed %d ticks — closed %d tunnels` / `cascade: project %s status=%s — closed %d tunnels` / `kill order %d applied — closed tunnel %s`（spec §4/§5，用 `log.Printf`，serve 下经 stderr→serve.log）。
- **只读豁免**：只读 store（离线水合库）跳过职责 3/4、续约豁免不计数、镜像写静默跳过（spec §4）。
- **client 独占**：每 forward 独立 `sshbroker.Connect`，manager 拆除只关独占 client，无连带（spec §3）。
- **弱语义措辞**：project 单 = 拆 pending 期间各 manager 名下存量；首个 COUNT==0 观察点即终态 applied；不承诺阻止重开（spec §4）。
- 所有 SSH 相关测试用既有 `internal/testsshd`；禁止真实网络外呼；`go test ./...` 全绿是每任务收尾门槛。
- 非 ASCII 编辑后逐字节验证（house 规矩）；commit 尾行 `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`。

---

### Task 1: store 层——三表 + 白名单/订单/镜像方法

**Files:**
- Modify: `internal/store/store.go`（schemaSQL 常量追加三表，约 :408 起）
- Create: `internal/store/bindhost.go`、`internal/store/tunnel_orders.go`、`internal/store/tunnel_registry.go`
- Modify: `internal/store/store.go`（加 `IsReadOnly()` 导出，字段 `readOnly` 已存在于 :54 区域）
- Test: `internal/store/tunnels_store_test.go`（新建）

**Interfaces:**
- Consumes: 既有 `Store`/`now()`/`ErrReadOnly` 模式（写方法开头 `if s.readOnly { return ErrReadOnly }`，见 `SetProjectStatus` projects.go:140）。
- Produces（后续任务按此调用，签名不得漂移）:
  - `func CanonicalBindIP(raw string) (string, error)` —— 解析+规范形；非 IP 字面量/带 zone → error
  - `func (s *Store) AddForwardBindHost(rawIP string) error` —— 校验（非环回/非通配）+ 规范形 + `INSERT OR IGNORE`
  - `func (s *Store) RemoveForwardBindHost(rawIP string) (bool, error)`
  - `func (s *Store) ListForwardBindHosts() ([]string, error)`
  - `type TunnelOrder struct { ID int64; TunnelID, ProjectID, CreatedBy string; CreatedAt int64; AppliedAt *int64; Outcome *string }`
  - `func (s *Store) CreateTunnelOrder(tunnelID, projectID, createdBy string) (int64, error)` —— 恰一非空校验
  - `func (s *Store) GetTunnelOrder(id int64) (*TunnelOrder, error)` —— nil,nil 当缺
  - `func (s *Store) PendingTunnelOrders() ([]TunnelOrder, error)` —— `WHERE outcome IS NULL ORDER BY id`
  - `func (s *Store) MarkTunnelOrderApplied(id int64) (bool, error)` —— `UPDATE ... WHERE id=? AND outcome IS NULL`，RowsAffected>0
  - `func (s *Store) CleanupTunnelOrders() error` —— 三条：applied 7d 清；pending+tunnel 目标缺席 7d 清；pending+project 目标缺席 7d 清
  - `type TunnelRegistryRow struct { TunnelID, ProjectID, ServerID, Remote, LocalAddr, ListenHost string; OpenedAt, LastRenewed int64 }`
  - `func (s *Store) InsertTunnelRegistry(row TunnelRegistryRow) error`
  - `func (s *Store) DeleteTunnelRegistry(ids []string) error`
  - `func (s *Store) HasTunnelRegistryRow(tunnelID string) (bool, error)`
  - `func (s *Store) RenewTunnelHeartbeat(tunnelID string, ts int64) (bool, error)` —— RowsAffected>0
  - `func (s *Store) CountTunnelRegistryProject(projectID string) (int64, error)`
  - `func (s *Store) ListTunnelRegistry() ([]TunnelRegistryRow, error)` —— `ORDER BY opened_at`
  - `func (s *Store) GCTunnelRegistry(cutoff int64) error` —— `DELETE WHERE last_renewed < ?`
  - `func (s *Store) IsReadOnly() bool`

- [ ] **Step 1: 写失败测试（表 + 方法全套）**

`internal/store/tunnels_store_test.go`（用既有 `openTestStore(t)`，见 servers_test.go:318）：

```go
package store

import (
	"strings"
	"testing"
	"time"
)

func TestBindHostsCRUDAndCanonical(t *testing.T) {
	st := openTestStore(t)
	// 全写形式入、缩写规范形存
	if err := st.AddForwardBindHost("2001:0db8::0001"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.AddForwardBindHost("192.168.50.10"); err != nil {
		t.Fatalf("add v4: %v", err)
	}
	hosts, err := st.ListForwardBindHosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || hosts[0] != "192.168.50.10" || hosts[1] != "2001:db8::1" {
		t.Fatalf("canonical form wrong: %v", hosts)
	}
	// 幂等 add
	if err := st.AddForwardBindHost("2001:db8::1"); err != nil {
		t.Fatalf("idempotent add: %v", err)
	}
	if hosts, _ := st.ListForwardBindHosts(); len(hosts) != 2 {
		t.Fatalf("idempotent add duplicated: %v", hosts)
	}
	// 拒绝:环回/通配/hostname/带 zone
	for _, bad := range []string{"127.0.0.1", "::1", "0.0.0.0", "::", "example.com", "fe80::1%eth0", "10.0.0.0/8"} {
		if err := st.AddForwardBindHost(bad); err == nil {
			t.Fatalf("add %q must be rejected", bad)
		}
	}
	// rm 用等价形式命中同一行
	ok, err := st.RemoveForwardBindHost("2001:0DB8::1")
	if err != nil || !ok {
		t.Fatalf("rm equivalent form: %v %v", ok, err)
	}
	if ok, _ := st.RemoveForwardBindHost("2001:db8::1"); ok {
		t.Fatal("rm twice must report false")
	}
}

func TestTunnelOrderLifecycle(t *testing.T) {
	st := openTestStore(t)
	id, err := st.CreateTunnelOrder("tun-1", "", "owner\\alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateTunnelOrder("", "", "x"); err == nil {
		t.Fatal("both-empty must be rejected")
	}
	if _, err := st.CreateTunnelOrder("a", "p", "x"); err == nil {
		t.Fatal("both-set must be rejected")
	}
	pend, err := st.PendingTunnelOrders()
	if err != nil || len(pend) != 1 || pend[0].TunnelID != "tun-1" || pend[0].CreatedBy != "owner\\alice" {
		t.Fatalf("pending: %v %v", pend, err)
	}
	if ok, _ := st.MarkTunnelOrderApplied(id); !ok {
		t.Fatal("mark applied must win")
	}
	if ok, _ := st.MarkTunnelOrderApplied(id); ok {
		t.Fatal("second mark must lose (outcome IS NULL guard)")
	}
	if pend, _ := st.PendingTunnelOrders(); len(pend) != 0 {
		t.Fatal("applied order must leave pending set")
	}
	got, err := st.GetTunnelOrder(id)
	if err != nil || got == nil || got.AppliedAt == nil || *got.Outcome != "applied" {
		t.Fatalf("get: %v %v", got, err)
	}
}

func TestTunnelOrderCleanupTargetAware(t *testing.T) {
	st := openTestStore(t)
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	// applied 老行 → 清
	idA, _ := st.CreateTunnelOrder("t-a", "", "o")
	_ = st.MarkTunnelOrderApplied(idA)
	st.db.Exec(`UPDATE tunnel_orders SET created_at=? WHERE id=?`, old, idA)
	// pending + 目标行在(模拟持续重开中的 project 单) → 不清
	idP, _ := st.CreateTunnelOrder("", "proj-1", "o")
	st.db.Exec(`UPDATE tunnel_orders SET created_at=? WHERE id=?`, old, idP)
	_ = st.InsertTunnelRegistry(TunnelRegistryRow{TunnelID: "t-x", ProjectID: "proj-1", ServerID: "s", Remote: "r", LocalAddr: "a", ListenHost: "h", OpenedAt: old, LastRenewed: old})
	// pending + 目标缺席(tunnel 单) → 清
	idT, _ := st.CreateTunnelOrder("t-gone", "", "o")
	st.db.Exec(`UPDATE tunnel_orders SET created_at=? WHERE id=?`, old, idT)

	if err := st.CleanupTunnelOrders(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetTunnelOrder(idA); err == nil {
		t.Fatal("old applied row must be cleaned")
	}
	if got, _ := st.GetTunnelOrder(idP); got == nil {
		t.Fatal("pending order with live target rows must NOT be cleaned")
	}
	if got, _ := st.GetTunnelOrder(idT); got != nil {
		t.Fatal("pending order with absent target must be cleaned")
	}
}

func TestTunnelRegistryOps(t *testing.T) {
	st := openTestStore(t)
	row := TunnelRegistryRow{TunnelID: "t1", ProjectID: "p1", ServerID: "s1", Remote: "127.0.0.1:5432", LocalAddr: "127.0.0.1:9000", ListenHost: "127.0.0.1", OpenedAt: 1, LastRenewed: 1}
	if err := st.InsertTunnelRegistry(row); err != nil {
		t.Fatal(err)
	}
	has, _ := st.HasTunnelRegistryRow("t1")
	if !has {
		t.Fatal("row must exist")
	}
	if ok, _ := st.RenewTunnelHeartbeat("t1", 99); !ok {
		t.Fatal("renew must hit")
	}
	if ok, _ := st.RenewTunnelHeartbeat("nope", 99); ok {
		t.Fatal("renew missing row must miss (zero-row signal)")
	}
	rows, _ := st.ListTunnelRegistry()
	if len(rows) != 1 || rows[0].LastRenewed != 99 {
		t.Fatalf("rows: %+v", rows)
	}
	n, _ := st.CountTunnelRegistryProject("p1")
	if n != 1 {
		t.Fatal("count by project")
	}
	_ = st.GCTunnelRegistry(50) // cutoff=50 → last_renewed(99) 不删
	if rows, _ = st.ListTunnelRegistry(); len(rows) != 1 {
		t.Fatal("fresh row must survive GC")
	}
	_ = st.GCTunnelRegistry(200)
	if rows, _ = st.ListTunnelRegistry(); len(rows) != 0 {
		t.Fatal("stale row must be GC'd")
	}
	_ = st.DeleteTunnelRegistry([]string{"t1"})
	if has, _ := st.HasTunnelRegistryRow("t1"); has {
		t.Fatal("delete")
	}
}

func TestTunnelStoreReadOnlyGates(t *testing.T) {
	st := openTestStore(t)
	st.SetReadOnly(nil)
	if !st.IsReadOnly() {
		t.Fatal("IsReadOnly must reflect SetReadOnly")
	}
	if err := st.AddForwardBindHost("10.1.2.3"); err != ErrReadOnly {
		t.Fatalf("write on read-only: %v", err)
	}
	if _, err := st.CreateTunnelOrder("t", "", "o"); err != ErrReadOnly {
		t.Fatalf("order write on read-only: %v", err)
	}
	if err := st.InsertTunnelRegistry(TunnelRegistryRow{TunnelID: "t"}); err != ErrReadOnly {
		t.Fatalf("registry write on read-only: %v", err)
	}
	// 读不受影响
	if _, err := st.ListForwardBindHosts(); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalBindIP(t *testing.T) {
	if c, err := CanonicalBindIP(" 2001:0db8::0001 "); err != nil || c != "2001:db8::1" {
		t.Fatalf("canonical: %q %v", c, err)
	}
	for _, bad := range []string{"example.com", "fe80::1%eth0", "10.0.0.0/8", ""} {
		if _, err := CanonicalBindIP(bad); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
	if !strings.Contains(func() string { _, err := CanonicalBindIP("x"); return err.Error() }(), "IP literal") {
		t.Fatal("error wording should mention IP literal")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run 'TestBindHosts|TestTunnelOrder|TestTunnelRegistry|TestTunnelStoreReadOnly|TestCanonicalBindIP' -v`
Expected: FAIL（undefined: 相关方法/表不存在——测试编译期即红）。

- [ ] **Step 3: schemaSQL 追加三表 + IsReadOnly**

`internal/store/store.go` schemaSQL 常量（:408 起）末尾追加（注意保持既有内容不动）：

```sql
CREATE TABLE IF NOT EXISTS forward_bind_hosts (
  ip         TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tunnel_orders (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  tunnel_id  TEXT,
  project_id TEXT,
  created_by TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  applied_at INTEGER,
  outcome    TEXT,
  CHECK ((tunnel_id IS NULL) <> (project_id IS NULL))
);
CREATE TABLE IF NOT EXISTS tunnel_registry (
  tunnel_id    TEXT PRIMARY KEY,
  project_id   TEXT NOT NULL,
  server_id    TEXT NOT NULL,
  remote       TEXT NOT NULL,
  local_addr   TEXT NOT NULL,
  listen_host  TEXT NOT NULL,
  opened_at    INTEGER NOT NULL,
  last_renewed INTEGER NOT NULL
);
```

同文件加（挨着 `SetReadOnly`）：

```go
// IsReadOnly reports whether this store rejects mutations (offline hydrated
// cache store). Tunnel control-loop duties 3/4/5 and mirror writes key off it.
func (s *Store) IsReadOnly() bool { return s.readOnly }
```

- [ ] **Step 4: bindhost.go 实现**

```go
package store

import (
	"database/sql"
	"fmt"
	"net"
	"strings"
)

// CanonicalBindIP validates a forward-bind host candidate and returns its
// canonical text form (net.IP.String()). Rules (spec §2): must be an IP
// literal — hostnames, CIDR ranges and zone-suffixed IPv6 (fe80::1%eth0) all
// fail net.ParseIP and are rejected here. Loopback/wildcard policy is the
// CALLER's (add rejects both; the forward gate always allows loopback and
// rejects wildcards).
func CanonicalBindIP(raw string) (string, error) {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil {
		return "", fmt.Errorf("listen_host %q is not an IP literal (hostnames, CIDR ranges and zoned IPv6 are not allowed)", raw)
	}
	return ip.String(), nil
}

// AddForwardBindHost owner-approves a non-loopback, non-wildcard bind IP,
// stored in canonical form. Idempotent (INSERT OR IGNORE). Spec §2.
func (s *Store) AddForwardBindHost(rawIP string) error {
	if s.readOnly {
		return ErrReadOnly
	}
	ip := net.ParseIP(strings.TrimSpace(rawIP))
	if ip == nil {
		return fmt.Errorf("%q is not an IP literal", rawIP)
	}
	if ip.IsLoopback() {
		return fmt.Errorf("%q is loopback — loopback is always allowed and needs no whitelist entry", rawIP)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("%q is a wildcard address — binding 0.0.0.0/:: is forbidden", rawIP)
	}
	_, err := s.db.Exec(`INSERT OR IGNORE INTO forward_bind_hosts(ip, created_at) VALUES (?, ?)`, ip.String(), now())
	return err
}

// RemoveForwardBindHost removes a whitelisted IP (any equivalent text form
// hits the canonical row). Returns false when no such row existed.
func (s *Store) RemoveForwardBindHost(rawIP string) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	canonical, err := CanonicalBindIP(rawIP)
	if err != nil {
		return false, err
	}
	res, err := s.db.Exec(`DELETE FROM forward_bind_hosts WHERE ip=?`, canonical)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListForwardBindHosts returns the whitelist in canonical form. Read path —
// works on read-only stores (empty on offline hydrated stores: the table is
// not exported into snapshots, which is the mechanism that keeps offline
// stdio loopback-only, spec §2).
func (s *Store) ListForwardBindHosts() ([]string, error) {
	rows, err := s.db.Query(`SELECT ip FROM forward_bind_hosts ORDER BY ip`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			return nil, err
		}
		out = append(out, ip)
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: tunnel_orders.go 实现**

```go
package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// TunnelOrder is an owner kill order (spec §4). Exactly one of TunnelID /
// ProjectID is set (enforced app-side here + by a CHECK constraint).
// outcome ∈ {NULL(pending), 'applied'} — the 'expired' terminal state was
// removed in spec rev3 (unreachable by construction).
type TunnelOrder struct {
	ID        int64
	TunnelID  string
	ProjectID string
	CreatedBy string
	CreatedAt int64
	AppliedAt *int64
	Outcome   *string
}

// CreateTunnelOrder places a kill order. createdBy is the OS user running the
// CLI (owner-action traceability, spec §7).
func (s *Store) CreateTunnelOrder(tunnelID, projectID, createdBy string) (int64, error) {
	if s.readOnly {
		return 0, ErrReadOnly
	}
	if (tunnelID == "") == (projectID == "") {
		return 0, errors.New("exactly one of tunnel_id / project_id must be set")
	}
	res, err := s.db.Exec(
		`INSERT INTO tunnel_orders(tunnel_id, project_id, created_by, created_at) VALUES (?, ?, ?, ?)`,
		nullable(tunnelID), nullable(projectID), createdBy, now(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// nullable maps "" → SQL NULL for the exactly-one-non-empty columns.
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func scanTunnelOrder(row interface{ Scan(...any) error }) (*TunnelOrder, error) {
	var o TunnelOrder
	var tun, proj sql.NullString
	if err := row.Scan(&o.ID, &tun, &proj, &o.CreatedBy, &o.CreatedAt, &o.AppliedAt, &o.Outcome); err != nil {
		return nil, err
	}
	o.TunnelID, o.ProjectID = tun.String, proj.String
	return &o, nil
}

const tunnelOrderCols = `id, tunnel_id, project_id, created_by, created_at, applied_at, outcome`

// GetTunnelOrder returns the order by id (nil, nil when absent) — the kill
// CLI polls it after placing an order.
func (s *Store) GetTunnelOrder(id int64) (*TunnelOrder, error) {
	o, err := scanTunnelOrder(s.db.QueryRow(`SELECT `+tunnelOrderCols+` FROM tunnel_orders WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return o, nil
}

// PendingTunnelOrders returns orders with outcome IS NULL (spec §4 — every
// read AND every marking UPDATE carries this guard).
func (s *Store) PendingTunnelOrders() ([]TunnelOrder, error) {
	rows, err := s.db.Query(`SELECT ` + tunnelOrderCols + ` FROM tunnel_orders WHERE outcome IS NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TunnelOrder
	for rows.Next() {
		o, err := scanTunnelOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// MarkTunnelOrderApplied flips a pending order to its only terminal state.
// Returns false when the order was not pending (already applied).
func (s *Store) MarkTunnelOrderApplied(id int64) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	res, err := s.db.Exec(
		`UPDATE tunnel_orders SET applied_at=?, outcome='applied' WHERE id=? AND outcome IS NULL`,
		now(), id,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CleanupTunnelOrders (spec §4 rev4, three statements — the naive
// "delete all old rows" tautology bug is gone): applied rows after 7d;
// pending rows only when their TARGET is also absent from tunnel_registry
// (an order whose target rows still exist — e.g. a project order grinding
// through an agent's continuous reopens — is still in effect and is never
// silently dropped).
func (s *Store) CleanupTunnelOrders() error {
	if s.readOnly {
		return ErrReadOnly
	}
	cutoff := now() - 7*24*3600
	stmts := []string{
		`DELETE FROM tunnel_orders WHERE outcome IS NOT NULL AND created_at < ?`,
		`DELETE FROM tunnel_orders WHERE outcome IS NULL AND created_at < ?
		   AND tunnel_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM tunnel_registry WHERE tunnel_registry.tunnel_id = tunnel_orders.tunnel_id)`,
		`DELETE FROM tunnel_orders WHERE outcome IS NULL AND created_at < ?
		   AND project_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM tunnel_registry WHERE tunnel_registry.project_id = tunnel_orders.project_id)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q, cutoff); err != nil {
			return fmt.Errorf("tunnel_orders cleanup: %w", err)
		}
	}
	return nil
}
```

- [ ] **Step 6: tunnel_registry.go 实现**

```go
package store

// TunnelRegistryRow mirrors a live broker-held tunnel (spec §6). last_renewed
// is a LEASE HEARTBEAT written every control tick by the owning manager — it
// is NOT traffic time (traffic time lives only in the manager's memory, spec
// §3). "row present ⇔ tunnel killable" is the state machine's foundation
// (fail-the-Open + fail-the-renewal keep it true).
type TunnelRegistryRow struct {
	TunnelID    string
	ProjectID   string
	ServerID    string
	Remote      string
	LocalAddr   string
	ListenHost  string // canonical form (spec §2)
	OpenedAt    int64
	LastRenewed int64
}

// InsertTunnelRegistry registers a just-opened tunnel. Called by the owning
// TunnelManager (fail-the-Open: an insert failure on a writable store closes
// the tunnel — spec §6).
func (s *Store) InsertTunnelRegistry(row TunnelRegistryRow) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.Exec(
		`INSERT INTO tunnel_registry(tunnel_id, project_id, server_id, remote, local_addr, listen_host, opened_at, last_renewed)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		row.TunnelID, row.ProjectID, row.ServerID, row.Remote, row.LocalAddr, row.ListenHost, row.OpenedAt, row.LastRenewed,
	)
	return err
}

// DeleteTunnelRegistry removes mirror rows (tunnel torn down). No-op on
// unknown ids.
func (s *Store) DeleteTunnelRegistry(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if s.readOnly {
		return ErrReadOnly
	}
	// MaxOpenConns(1) + tiny id sets: one statement per call is fine.
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	q := `DELETE FROM tunnel_registry WHERE tunnel_id IN (` + placeholders(len(ids)) + `)`
	_, err := s.db.Exec(q, args...)
	return err
}

func placeholders(n int) string {
	out := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
	}
	return string(out)
}

// HasTunnelRegistryRow reports global presence (any process's tunnel) — the
// "absent target ⇒ order achieved" signal for tunnel kill orders (spec §4).
func (s *Store) HasTunnelRegistryRow(tunnelID string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM tunnel_registry WHERE tunnel_id=?`, tunnelID).Scan(&one)
	if err == errNoRows() {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// RenewTunnelHeartbeat refreshes the lease heartbeat. Returns false when the
// row is gone (zero-row ⇒ the tunnel fell out of the kill domain and must
// self-close, spec §4 duty 5).
func (s *Store) RenewTunnelHeartbeat(tunnelID string, ts int64) (bool, error) {
	if s.readOnly {
		return false, ErrReadOnly
	}
	res, err := s.db.Exec(`UPDATE tunnel_registry SET last_renewed=? WHERE tunnel_id=?`, ts, tunnelID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountTunnelRegistryProject counts live mirror rows of a project — the
// project kill order's completion signal (first zero-count observation marks
// the order applied, spec §4).
func (s *Store) CountTunnelRegistryProject(projectID string) (int64, error) {
	var n int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tunnel_registry WHERE project_id=?`, projectID).Scan(&n)
	return n, err
}

// ListTunnelRegistry returns all mirror rows (owner `tunnels ls`).
func (s *Store) ListTunnelRegistry() ([]TunnelRegistryRow, error) {
	rows, err := s.db.Query(`SELECT tunnel_id, project_id, server_id, remote, local_addr, listen_host, opened_at, last_renewed FROM tunnel_registry ORDER BY opened_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TunnelRegistryRow
	for rows.Next() {
		var r TunnelRegistryRow
		if err := rows.Scan(&r.TunnelID, &r.ProjectID, &r.ServerID, &r.Remote, &r.LocalAddr, &r.ListenHost, &r.OpenedAt, &r.LastRenewed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GCTunnelRegistry deletes rows whose heartbeat went stale (crashed-owner
// ghosts). Idempotent; any writable broker may run it (spec §6).
func (s *Store) GCTunnelRegistry(cutoff int64) error {
	if s.readOnly {
		return ErrReadOnly
	}
	_, err := s.db.Exec(`DELETE FROM tunnel_registry WHERE last_renewed < ?`, cutoff)
	return err
}
```

注：`errNoRows()` 若仓内无此助手，直接用 `errors.Is(err, sql.ErrNoRows)`（看 cachetoken.go 同款写法照抄）。`scanTunnelOrder` 的 `Outcome`/`AppliedAt` 扫描用 `*int64`/`*string` 直接扫 NULL——若 modernc 驱动对 NULL→指针扫描有出入，改 `sql.NullInt64`/`sql.NullString` 中转（照 cachetoken.go 的既有模式）。

- [ ] **Step 7: 跑测试确认全绿**

Run: `go test ./internal/store/ -v -run 'TestBindHosts|TestTunnelOrder|TestTunnelRegistry|TestTunnelStoreReadOnly|TestCanonicalBindIP'`
Expected: PASS 全部。然后 `go test ./internal/store/` 全包绿。

- [ ] **Step 8: Commit**

```bash
git add internal/store/
git commit -m "feat(store): Plan 35 T1 — bind-host 白名单 + tunnel_orders + tunnel_registry 三表与方法"
```

---

### Task 2: sshbroker——ForwardLocal listenHost + onActivity 活动钩子

**Files:**
- Modify: `internal/sshbroker/tunnel.go`（ForwardLocal 签名、Tunnel 字段、serve/handle 钩子点）
- Modify: `internal/sshbroker/tunnel_test.go`（既有 3 参调用点机械改 5 参 + 新用例）
- Modify: `internal/mcpserver/core.go:524`（唯一生产调用点同步改参——本任务只改调用形状，gate 逻辑在 Task 5；传 `"127.0.0.1"` 与 `nil` 占位）

**Interfaces:**
- Consumes: 无（独立）。
- Produces:
  - `func (c *Client) ForwardLocal(localPort int, listenHost, remoteHost string, remotePort int, onActivity func()) (*Tunnel, error)` —— listenHost 为已验证 IP 字面量；onActivity 可为 nil
  - Tunnel 内部 30s 节流（`activityThrottle = 30 * time.Second`，`atomic.Int64` unixnano）；触发点 = Accept 成功后 + 双向 pipe 每次读到字节

- [ ] **Step 1: 写失败测试**

`tunnel_test.go` 追加（沿用该文件既有的测试 harness——local echo/sshd 桩照同文件现有用例的搭法；若现有用例用 testsshd 则照抄搭法）：

```go
func TestForwardLocalListenHostBindsNonLoopback(t *testing.T) {
	// 与既有用例同款 client 搭法;此处用伪 client 结构占位说明断言核心
	c := newTestClient(t) // 既有 helper 或照 TestForwardLocal 的搭法
	// 找一个本机存在的非环回地址(没有则 skip)
	ip := pickLocalNonLoopbackIP(t)
	tun, err := c.ForwardLocal(0, ip.String(), "127.0.0.1", echoPortOf(t), nil)
	if err != nil {
		t.Skipf("bind %s unavailable on this host: %v", ip, err)
	}
	defer tun.Close()
	if got := tun.LocalAddr(); !strings.HasPrefix(got, "["+ip.String()+"]") && !strings.HasPrefix(got, ip.String()+":") {
		t.Fatalf("LocalAddr = %q, want bind on %s", got, ip)
	}
}

func TestForwardLocalActivityHookThrottled(t *testing.T) {
	c := newTestClient(t)
	var calls atomic.Int32
	tun, err := c.ForwardLocal(0, "127.0.0.1", "127.0.0.1", echoPortOf(t), func() { calls.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	defer tun.Close()
	// 连打 5 条连接(每条 Accept 触发一次活动上报,但 30s 节流内只该有 1 次回调)
	for i := 0; i < 5; i++ {
		conn, err := net.DialTimeout("tcp", tun.LocalAddr(), 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte("ping\n"))
		buf := make([]byte, 16)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		conn.Read(buf)
		conn.Close()
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("throttle failed: %d callbacks in one 30s window, want 1", n)
	}
}

func pickLocalNonLoopbackIP(t *testing.T) net.IP {
	t.Helper()
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			return ipn.IP
		}
	}
	t.Skip("no non-loopback IPv4 on this host")
	return nil
}
```

（`newTestClient`/`echoPortOf` 换成该测试文件里实际存在的搭法名——实现者以文件内既有用例为准，断言逻辑保持。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/sshbroker/ -run 'TestForwardLocalListenHost|TestForwardLocalActivity' -v`
Expected: FAIL（参数数量不对，编译红）。

- [ ] **Step 3: 实现 tunnel.go 改动**

```go
// activityThrottle bounds how often the onActivity callback actually fires
// (spec §3): the read path stays lock- and allocation-free — one atomic
// compare per event, a real callback at most once per window.
const activityThrottle = 30 * time.Second

type Tunnel struct {
	ID        string
	localAddr string
	listener  net.Listener
	client    *ssh.Client

	onActivity func()      // nil = no hook
	touchNano  atomic.Int64 // unix-nano of last real callback (0 = never)

	closeOnce sync.Once
	closeErr  error
}

// activity reports tunnel activity, throttled to one real callback per
// activityThrottle window (spec §3). Races between concurrent pipes can
// occasionally fire two callbacks at a window edge — harmless, the manager's
// Touch is idempotent.
func (t *Tunnel) activity() {
	if t.onActivity == nil {
		return
	}
	now := time.Now().UnixNano()
	if now-t.touchNano.Load() < int64(activityThrottle) {
		return
	}
	t.touchNano.Store(now)
	t.onActivity()
}

// ForwardLocal opens a local TCP listener on listenHost:localPort (0 = free
// port) and forwards each accepted connection to remoteHost:remotePort —
// the `ssh -L` semantic. listenHost must be an already-VALIDATED IP literal
// (the mcpserver gate owns policy; spec §2); IPv6 bracketing is handled by
// net.JoinHostPort. onActivity (optional) receives throttled activity
// pings: on each accepted connection and on every read of both pipe
// directions (spec §3).
func (c *Client) ForwardLocal(localPort int, listenHost, remoteHost string, remotePort int, onActivity func()) (*Tunnel, error) {
	addr := net.JoinHostPort(listenHost, strconv.Itoa(localPort))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("local listen %s: %w", addr, err)
	}
	t := &Tunnel{
		ID:         uuid.NewString(),
		localAddr:  ln.Addr().String(),
		listener:   ln,
		client:     c.c,
		onActivity: onActivity,
	}
	go t.serve(remoteHost, remotePort)
	return t, nil
}
```

`serve` 的 Accept 成功后加一行：

```go
		t.activity() // new connection = activity (spec §3)
		go t.handle(local, remote)
```

`handle` 的两个 copy 改为计数 reader（读路径上报）：

```go
type countingReader struct {
	r io.Reader
	t *Tunnel
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.t.activity()
	}
	return n, err
}
```

handle 内：

```go
	go func() {
		_, _ = io.Copy(rem, &countingReader{r: local, t: t})
		...CloseWrite 不变...
	}()
	go func() {
		_, _ = io.Copy(local, &countingReader{r: rem, t: t})
		...CloseWrite 不变...
	}()
```

同文件头注释清理：`ForwardLocal ... binds the local listener on the broker host's loopback only` 一句改为 `binds the local listener on the given host (validated by the caller's gate; loopback by default)`。

- [ ] **Step 4: 调用点机械同步 + 全包绿**

`internal/mcpserver/core.go:524` 改为 `cli.ForwardLocal(localPort, "127.0.0.1", remoteHost, remotePort, nil)`（占位，Task 5 换真值）；`tunnel_test.go` 既有调用点全部改 5 参（`"127.0.0.1"` 插在 localPort 后、`nil` 尾参）。
Run: `go test ./internal/sshbroker/ ./internal/mcpserver/ -count=1`
Expected: PASS（mcpserver 现有用例不因占位参数变红）。

- [ ] **Step 5: Commit**

```bash
git add internal/sshbroker/ internal/mcpserver/core.go
git commit -m "feat(sshbroker): Plan 35 T2 — ForwardLocal listenHost 参数 + onActivity 30s 节流活动钩子"
```

---

### Task 3: TunnelManager 镜像管道——AttachStore/Open 元数据/fail-the-Open/关闭路径镜像删/DELETE 重试

**Files:**
- Modify: `internal/mcpserver/tunnels.go`
- Modify: `internal/mcpserver/core.go:531`（Open 调用点同步——Task 5 再补 gate，本任务先接元数据与错误路径）
- Test: `internal/mcpserver/tunnels_mirror_test.go`（新建）

**Interfaces:**
- Consumes: Task 1 全部 store 方法；Task 2 的 `ForwardLocal` 5 参形态。
- Produces:
  - `type TunnelMeta struct { ProjectID, ServerID, Remote, ListenHost string }`
  - `func (m *TunnelManager) Open(t *sshbroker.Tunnel, c *sshbroker.Client, meta TunnelMeta) (string, error)` —— 签名变更（原 2 参返 string）；fail-the-Open：可写库 INSERT 失败 → 立即关 tunnel+client、返 error
  - `func (m *TunnelManager) AttachStore(storeFn func() *store.Store, projectID string)`
  - 内部（Task 4 消费）：`m.closeAllTunnels(reason string) int`（关全部+镜像删，**不**走 quit 通道——控制循环自用）；`m.mirrorDelete(ids []string)`（失败入 `pendingDeletes` 重试集）；`m.retryPendingDeletes()`；`m.tunnelIDsLocked() []string`；manager 新字段 `storeFn func() *store.Store`、`projectID string`、`pendingDeletes map[string]struct{}`、meta 存入 `managedTunnel.meta TunnelMeta`

- [ ] **Step 1: 写失败测试**

`internal/mcpserver/tunnels_mirror_test.go`（mcpserver 包内测试可摸私有字段；`newStore(t)` 既有 helper）：

```go
package mcpserver

import (
	"context"
	"errors"
	"testing"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/testsshd"
)

func mirrorMgr(t *testing.T) (*TunnelManager, *store.Store, string, func()) {
	t.Helper()
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	projID, _, _ := st.AddProject("proj", pid)
	mgr := NewTunnelManager()
	mgr.AttachStore(func() *store.Store { return st }, projID)
	return mgr, st, srvID, cleanup
}

func openTestTunnel(t *testing.T, mgr *TunnelManager, st *store.Store, projID, srvID string) (string, error) {
	t.Helper()
	out, err := ForwardForProfile(context.Background(), st, projID, "p", srvID, "127.0.0.1", echoPortForMirror(t), 0, "127.0.0.1", mgr)
	return out.TunnelID, err
}

func TestMirrorInsertOnOpenAndDeleteOnClose(t *testing.T) {
	mgr, st, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()

	id, err := openTestTunnel(t, mgr, st, mgr.projectID, srvID)
	if err != nil {
		t.Fatal(err)
	}
	has, _ := st.HasTunnelRegistryRow(id)
	if !has {
		t.Fatal("registry row must exist right after Open (event-driven insert)")
	}
	rows, _ := st.ListTunnelRegistry()
	if len(rows) != 1 || rows[0].ProjectID != mgr.projectID || rows[0].ServerID != srvID || rows[0].ListenHost != "127.0.0.1" {
		t.Fatalf("mirror row: %+v", rows)
	}
	if !mgr.Close(id) {
		t.Fatal("close")
	}
	if has, _ := st.HasTunnelRegistryRow(id); has {
		t.Fatal("row must be deleted on Close")
	}
}

func TestMirrorFailTheOpen(t *testing.T) {
	mgr, st, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	// 注入:表先 DROP → INSERT 必败
	if _, err := st.(interface{ Exec(string, ...any) (sql.Result, error) }).Exec(`DROP TABLE tunnel_registry`); err != nil {
		t.Skipf("cannot inject drop: %v", err)
	}
	// 等价注入法(若上法类型断言不成立): st.db 不可摸时,改用 SQLite 锁——
	// 实现者按仓内 store 测试注入惯例选一种;断言不变:
	id, err := openTestTunnel(t, mgr, st, mgr.projectID, srvID)
	if err == nil {
		t.Fatalf("fail-the-Open must surface error (got tunnel %s)", id)
	}
	if id != "" {
		t.Fatal("no id on failure")
	}
	// 隧道不可留:内存注册表必须为空
	mgr.mu.Lock()
	n := len(mgr.tunnels)
	mgr.mu.Unlock()
	if n != 0 {
		t.Fatalf("failed open left %d tunnels registered", n)
	}
}

func TestMirrorDeleteFailureRetries(t *testing.T) {
	mgr, st, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	id, err := openTestTunnel(t, mgr, st, mgr.projectID, srvID)
	if err != nil {
		t.Fatal(err)
	}
	// 先手工删行(模拟 DELETE 已不可能命中)再关——验证 DELETE 失败路径用锁注入:
	// 实现者用仓内惯用注入(如临时把 storeFn 换成会返回错误的包装 store 不现实——
	// 改为: 先 DROP tunnel_registry, Close 走 DELETE 必败 → pendingDeletes 记账,
	// 恢复表后 retryPendingDeletes 清账。
	dropRegistry(t, st)
	if !mgr.Close(id) {
		t.Fatal("close")
	}
	if len(mgr.pendingDeletes) == 0 {
		t.Fatal("failed mirror DELETE must land in retry set")
	}
	restoreRegistry(t, st) // 重建空表
	mgr.retryPendingDeletes()
	if len(mgr.pendingDeletes) != 0 {
		t.Fatal("retry must drain the set once the store accepts writes again")
	}
}
```

（`dropRegistry`/`restoreRegistry`/`echoPortForMirror` 为小 helper：DROP/CREATE 表的 Exec + 复用 `startEchoListener`——实现者按 store 包暴露面落 helper；若 store 未暴露 Exec，在 store 包加一个 `testonly` 导出 `ExecForTest(q string) error`（仅 testing 用，参考仓内 `getDACLForTest` 命名先例）。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcpserver/ -run 'TestMirror' -v`
Expected: FAIL（Open 签名/AttachStore 不存在，编译红）。

- [ ] **Step 3: 实现 tunnels.go 改动**

头注释块（:10-16 的 creation-based NOTE 段）整段替换为：

```go
// forwardIdleTimeout is how long a tunnel may stay IDLE before the sweeper
// reaps it (spec §3): lastActivity advances on real traffic via the Tunnel's
// onActivity hook (30s-throttled) wired into Touch — a busy tunnel survives
// indefinitely; an idle one dies after 10 min.
```

结构变更：

```go
type managedTunnel struct {
	tunnel       *sshbroker.Tunnel
	client       *sshbroker.Client
	lastActivity time.Time
	meta         TunnelMeta
}

// TunnelMeta carries the registry-mirror fields an Open must persist
// (spec §6). ListenHost is the canonical form.
type TunnelMeta struct {
	ProjectID  string
	ServerID   string
	Remote     string
	ListenHost string
}

type TunnelManager struct {
	mu             sync.Mutex
	tunnels        map[string]*managedTunnel
	quit           chan struct{}
	startOnce      sync.Once
	stopOnce       sync.Once
	wg             sync.WaitGroup
	storeFn        func() *store.Store // nil = bare manager (tests): control loop no-ops
	projectID      string
	pendingDeletes map[string]struct{} // mirror DELETEs that failed; retried each control tick (spec §6)
}
```

`NewTunnelManager` 补 `pendingDeletes: map[string]struct{}{}`。

新方法：

```go
// AttachStore wires the live store source + project scope for mirror writes
// and the control loop (spec §4 接线点). Called by NewServerFromSource; bare
// managers (existing tests) stay control-inert.
func (m *TunnelManager) AttachStore(storeFn func() *store.Store, projectID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storeFn = storeFn
	m.projectID = projectID
}

// mirrorDelete removes registry rows for ids; on writable-store failure the
// ids land in the per-tick retry set (ghost rows also self-heal via the
// 30-min GC — spec §6).
func (m *TunnelManager) mirrorDelete(ids []string) {
	if len(ids) == 0 || m.storeFn == nil {
		return
	}
	st := m.storeFn()
	if st.IsReadOnly() {
		return // offline hydrated store: no mirror to maintain
	}
	if err := st.DeleteTunnelRegistry(ids); err != nil {
		for _, id := range ids {
			m.pendingDeletes[id] = struct{}{}
		}
	}
}

// retryPendingDeletes drains failed mirror DELETEs (control tick duty 5).
func (m *TunnelManager) retryPendingDeletes() {
	if len(m.pendingDeletes) == 0 || m.storeFn == nil {
		return
	}
	st := m.storeFn()
	if st.IsReadOnly() {
		return
	}
	ids := make([]string, 0, len(m.pendingDeletes))
	for id := range m.pendingDeletes {
		ids = append(ids, id)
	}
	if err := st.DeleteTunnelRegistry(ids); err == nil {
		for _, id := range ids {
			delete(m.pendingDeletes, id)
		}
	}
}
```

`Open` 签名改造（fail-the-Open）：

```go
// Open registers a tunnel + its owning client and mirrors it into
// tunnel_registry. lastActivity seeds to now; the activity hook keeps it
// fresh. fail-the-Open (spec §6): when a writable store is attached and the
// registry INSERT fails, the tunnel is closed immediately and the error is
// returned — a tunnel that cannot be killed is not allowed to exist.
func (m *TunnelManager) Open(t *sshbroker.Tunnel, c *sshbroker.Client, meta TunnelMeta) (string, error) {
	m.mu.Lock()
	m.tunnels[t.ID] = &managedTunnel{tunnel: t, client: c, lastActivity: time.Now(), meta: meta}
	storeFn := m.storeFn
	m.mu.Unlock()
	if storeFn != nil {
		st := storeFn()
		if !st.IsReadOnly() {
			err := st.InsertTunnelRegistry(store.TunnelRegistryRow{
				TunnelID: t.ID, ProjectID: meta.ProjectID, ServerID: meta.ServerID,
				Remote: meta.Remote, LocalAddr: t.LocalAddr(), ListenHost: meta.ListenHost,
				OpenedAt: time.Now().Unix(), LastRenewed: time.Now().Unix(),
			})
			if err != nil {
				// fail-the-Open: tear down what we just registered
				m.mu.Lock()
				delete(m.tunnels, t.ID)
				m.mu.Unlock()
				_ = t.Close()
				_ = c.Close()
				return "", fmt.Errorf("tunnel registry mirror failed, tunnel closed (fail-the-Open): %w", err)
			}
		}
	}
	return t.ID, nil
}
```

`Close`/`SweepIdle`/`CloseAll` 三个拆除点在 `delete(m.tunnels, id)` 后补 `mirrorDelete([]string{id})`（SweepIdle 收尾批量一次 `mirrorDelete(closed)`）；新增内部拆全助手（控制循环用，**不**停 sweeper）：

```go
// closeAllTunnels tears down every tunnel + mirror rows WITHOUT stopping the
// sweeper goroutine (CloseAll owns the quit path). Returns how many were
// closed. Used by the control loop (kill --project / cascade /
// fail-the-renewal) — spec §4.
func (m *TunnelManager) closeAllTunnels(reason string) int {
	m.mu.Lock()
	ids := make([]string, 0, len(m.tunnels))
	for id, mt := range m.tunnels {
		_ = mt.tunnel.Close()
		_ = mt.client.Close()
		delete(m.tunnels, id)
		ids = append(ids, id)
	}
	m.mu.Unlock()
	m.mirrorDelete(ids)
	if len(ids) > 0 {
		log.Printf("%s — closed %d tunnels", reason, len(ids))
	}
	return len(ids)
}
```

文件 import 补 `"log"`、`"ssh-manager-mcp/internal/store"`、`"fmt"`。

`core.go:531-533` 调用点同步（gate 在 Task 5，此处先把元数据与错误路径接上）：

```go
	id, oerr := mgr.Open(tun, cli, TunnelMeta{
		ProjectID:  projectID,
		ServerID:   serverID,
		Remote:     net.JoinHostPort(remoteHost, strconv.Itoa(remotePort)),
		ListenHost: "127.0.0.1", // Task 5 换成 gate 产出的规范形
	})
	if oerr != nil {
		status = "error"
		err = oerr
		return // deferred cleanup closes cli (unregistered)
	}
```

（既有直接调 `mgr.Open(tun, cli)` 的测试全部补 `TunnelMeta{ProjectID: "p", ServerID: "s", Remote: "r", ListenHost: "127.0.0.1"}` 与双返回值——机械扫 `grep -rn "\.Open(tun" internal/`。）

- [ ] **Step 4: 跑测试确认通过 + 全包绿**

Run: `go test ./internal/mcpserver/ -run 'TestMirror' -v` → PASS；`go test ./internal/mcpserver/ -count=1` → 全绿（含 revoke_semantics——翻转在 Task 7，此阶段 revoke 不驱动控制循环，测试保持原语义绿）。

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/ internal/store/
git commit -m "feat(mcpserver): Plan 35 T3 — TunnelManager 镜像管道(AttachStore/fail-the-Open/关闭镜像删/DELETE 重试集)"
```

---

### Task 4: TunnelManager 控制循环——订单状态机/级联/白名单复查/双计数/心跳零行自关闭

**Files:**
- Create: `internal/mcpserver/tunnels_control.go`
- Modify: `internal/mcpserver/tunnels.go`（sweepLoop 加第二 ticker；StartSweeper 不变）
- Test: `internal/mcpserver/tunnels_control_test.go`（新建）

**Interfaces:**
- Consumes: Task 1 store 方法、Task 3 的 `closeAllTunnels`/`mirrorDelete`/`retryPendingDeletes`/meta 字段。
- Produces:
  - `const controlInterval = 15 * time.Second`
  - `func (m *TunnelManager) runControlTick()` —— 测试直接驱动的入口（同 SweepIdle 直驱惯例）
  - manager 新字段：`renewalFail, cascadeFail, whitelistFail int`（连续失败计数；进程内内存态）

- [ ] **Step 1: 写失败测试**

`internal/mcpserver/tunnels_control_test.go`（复用 Task 3 的 `mirrorMgr`/`openTestTunnel` helper）：

```go
package mcpserver

import (
	"net"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func TestControlTickKillsTunnelOrder(t *testing.T) {
	mgr, st, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	id, err := openTestTunnel(t, mgr, st, mgr.projectID, srvID)
	if err != nil {
		t.Fatal(err)
	}
	oid, _ := st.CreateTunnelOrder(id, "", "tester")
	mgr.runControlTick()
	// 隧道死 + 端口不可达 + 单 applied
	if has, _ := st.HasTunnelRegistryRow(id); has {
		t.Fatal("mirror row must be gone after kill")
	}
	o, _ := st.GetTunnelOrder(oid)
	if o == nil || o.Outcome == nil || *o.Outcome != "applied" {
		t.Fatalf("order must be applied, got %+v", o)
	}
	if _, err := net.DialTimeout("tcp", localAddrOf(t, mgr, id), 500*time.Millisecond); err == nil {
		t.Fatal("port must be unreachable after kill")
	}
}

func TestControlTickAbsentTargetAchievesOrder(t *testing.T) {
	mgr, st, _, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	// 目标从未存在(或已被 sweep):单应被标 applied
	oid, _ := st.CreateTunnelOrder("no-such-tunnel", "", "tester")
	mgr.runControlTick()
	o, _ := st.GetTunnelOrder(oid)
	if o == nil || o.Outcome == nil || *o.Outcome != "applied" {
		t.Fatal("absent target ⇒ applied")
	}
}

func TestControlTickProjectOrderFanOut(t *testing.T) {
	// 两个 manager 同 project(模拟 serve + 在线 stdio),各有隧道 → 一单全拆
	mgrA, st, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgrA.CloseAll()
	mgrB := NewTunnelManager()
	mgrB.AttachStore(func() *store.Store { return st }, mgrA.projectID)
	defer mgrB.CloseAll()
	if _, err := openTestTunnel(t, mgrA, st, mgrA.projectID, srvID); err != nil {
		t.Fatal(err)
	}
	if _, err := openTestTunnel(t, mgrB, st, mgrA.projectID, srvID); err != nil {
		t.Fatal(err)
	}
	oid, _ := st.CreateTunnelOrder("", mgrA.projectID, "tester")
	mgrA.runControlTick() // A 拆自己的;完成判定 count==0? B 的行还在 → 不标
	mgrB.runControlTick() // B 拆自己的;此后 count==0 → 标
	if n, _ := st.CountTunnelRegistryProject(mgrA.projectID); n != 0 {
		t.Fatalf("all project tunnels must be torn down, %d left", n)
	}
	o, _ := st.GetTunnelOrder(oid)
	if o == nil || o.Outcome == nil || *o.Outcome != "applied" {
		t.Fatal("project order applied after registry drained")
	}
}

func TestControlTickCascadeOnRevokeAndDisable(t *testing.T) {
	mgr, st, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	id, token, _ := st.ProjectTokenForTest() // 或直接留钩子:实现者按仓内测试取 token 的既有方式
	_ = token
	id2, err := openTestTunnel(t, mgr, st, mgr.projectID, srvID)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetProjectStatus(mgr.projectID, models.ProjectRevoked)
	mgr.runControlTick()
	if has, _ := st.HasTunnelRegistryRow(id2); has {
		t.Fatal("revoked project's tunnel must be torn down ≤tick")
	}
	// disable 同型(再开一条 → disable → tick → 拆)
	_ = id
}
```

（`ProjectTokenForTest` 若不存在——`AddProject` 返回 (id, token, err)，`mirrorMgr` 里 token 被丢弃了；把 `mirrorMgr` 的 `projID, _, _ :=` 改成返回 token 也行——实现者按最小改动落。级联 disable 断言补全为独立用例。）

补三个纪律用例（骨架，断言核心齐全）：

```go
func TestControlTickWhitelistShrinkClosesExisting(t *testing.T) {
	mgr, st, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	_ = st.AddForwardBindHost("127.0.0.2") // 环回外测法受限——见下注
	// Windows/Linux 上 127.0.0.2 通常可 bind 且 IsLoopback=true ⇒ 白名单复查不辖它。
	// 本用例改用「非环回本机地址」: pickLocalNonLoopbackIP(见 sshbroker 测试同款 helper,复制到本包)。
	ip := pickLocalNonLoopbackIP(t)
	_ = st.AddForwardBindHost(ip.String())
	out, err := ForwardForProfile(ctxBg(), st, mgr.projectID, "p", srvID, "127.0.0.1", echoPortForMirror(t), 0, ip.String(), mgr)
	if err != nil {
		t.Skipf("cannot bind %s here: %v", ip, err)
	}
	_, _ = st.RemoveForwardBindHost(ip.String())
	mgr.runControlTick()
	if has, _ := st.HasTunnelRegistryRow(out.TunnelID); has {
		t.Fatal("whitelist-revoked tunnel must close ≤tick")
	}
}

func TestControlTickRenewalFailuresSelfClose(t *testing.T) {
	mgr, st, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	if _, err := openTestTunnel(t, mgr, st, mgr.projectID, srvID); err != nil {
		t.Fatal(err)
	}
	dropRegistry(t, st) // 心跳 UPDATE 必败
	for i := 0; i < 9; i++ {
		mgr.runControlTick()
	}
	if n := mgr.liveCount(); n != 0 {
		t.Fatalf("fail-the-renewal must self-close after >8 failed ticks, %d alive", n)
	}
}

func TestControlTickZeroRowHeartbeatSelfClose(t *testing.T) {
	mgr, st, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	id, err := openTestTunnel(t, mgr, st, mgr.projectID, srvID)
	if err != nil {
		t.Fatal(err)
	}
	// 行被外部删除(模拟 GC 抢先) → 心跳零行 → 自关闭
	if err := st.DeleteTunnelRegistry([]string{id}); err != nil {
		t.Fatal(err)
	}
	mgr.runControlTick()
	if n := mgr.liveCount(); n != 0 {
		t.Fatal("zero-row heartbeat must self-close the tunnel")
	}
}

func TestControlTickReadOnlyExemptions(t *testing.T) {
	// 只读库:orders 恒空、跳职责 3/4/5、续约不计数 → 隧道全活
	mgr, st, srvID, cleanup := mirrorMgr(t)
	defer cleanup()
	defer mgr.CloseAll()
	if _, err := openTestTunnel(t, mgr, st, mgr.projectID, srvID); err != nil {
		t.Fatal(err)
	}
	st.SetReadOnly(nil)
	for i := 0; i < 12; i++ {
		mgr.runControlTick() // 不得 panic、不得拆隧道、不得计数
	}
	if n := mgr.liveCount(); n != 1 {
		t.Fatalf("read-only store must exempt lease discipline, %d alive", n)
	}
}
```

（`mgr.liveCount()`/`ctxBg()` 为测试小 helper；`dropRegistry` 复用 Task 3 的注入 helper。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcpserver/ -run 'TestControlTick' -v`
Expected: FAIL（runControlTick 未定义，编译红）。

- [ ] **Step 3: 实现 tunnels_control.go**

```go
package mcpserver

import (
	"log"
	"time"

	"ssh-manager-mcp/internal/store"
)

// controlInterval is the control-loop tick (spec §4): order state machine,
// row cleanup, cascade recheck, whitelist recheck, lease heartbeat — all
// idempotent, all safe to re-run every 15s.
const controlInterval = 15 * time.Second

// renewalFailThreshold: consecutive failed/incomplete renewal ticks before
// fail-the-renewal fires (spec §4; ~2min at 15s ticks). Enforcement (cascade
// / whitelist) reads use the same threshold but INDEPENDENT counters.
const renewalFailThreshold = 8

// runControlTick executes one control cycle (spec §4 duties 1-5). Called by
// the sweepLoop ticker every controlInterval; tests drive it directly.
func (m *TunnelManager) runControlTick() {
	if m.storeFn == nil {
		return // bare manager (existing tests): control-inert
	}
	st := m.storeFn()
	if st.IsReadOnly() {
		// Offline hydrated store (spec §4 只读豁免): orders table is empty
		// (never exported), duties 3/4 would act on stale snapshot status,
		// and the lease duty has no mirror to renew — skip everything.
		return
	}

	// duty 1 — order state machine (spec §4)
	orders, err := st.PendingTunnelOrders()
	if err != nil {
		log.Printf("tunnel control: read pending orders failed, retry next tick: %v", err)
		return // transient: abandon this tick entirely (spec §4 瞬时错误语义)
	}
	for _, o := range orders {
		switch {
		case o.TunnelID != "":
			if m.closeTunnelIfOwned(o.TunnelID) {
				// owner path: Close → mirror delete → mark applied (标记后置)
				m.markApplied(o.ID)
				continue
			}
			if has, herr := st.HasTunnelRegistryRow(o.TunnelID); herr == nil && !has {
				m.markApplied(o.ID) // absent target ⇒ achieved (spec §4)
			}
		case o.ProjectID == m.projectID:
			// 幂等扇出:拆本 manager 名下该 project 全部(弱语义——仅 pending 期间)
			m.closeAllTunnels("kill order " + itoa(o.ID) + " (project)")
		}
	}
	// project orders completion: first zero-count observation marks applied
	// (spec §4 — count check runs for every pending project order).
	for _, o := range orders {
		if o.ProjectID == "" {
			continue
		}
		if n, cerr := st.CountTunnelRegistryProject(o.ProjectID); cerr == nil && n == 0 {
			m.markApplied(o.ID)
		}
	}

	// duty 2 — row cleanup (applied 7d / pending target-absent 7d) + registry 30min GC
	if err := st.CleanupTunnelOrders(); err != nil {
		log.Printf("tunnel control: order cleanup failed: %v", err)
	}
	if err := st.GCTunnelRegistry(time.Now().Add(-30*time.Minute).Unix()); err != nil {
		log.Printf("tunnel control: registry GC failed: %v", err)
	}

	// duty 3 — cascade recheck (spec §5)
	m.cascadeCheck(st)

	// duty 4 — whitelist shrink recheck (spec §2/§4)
	m.whitelistCheck(st)

	// duty 5 — lease heartbeat + zero-row self-close + DELETE retries (spec §4/§6)
	m.heartbeat(st)
	m.retryPendingDeletes()
}

func (m *TunnelManager) markApplied(orderID int64) {
	if m.storeFn == nil {
		return
	}
	if st := m.storeFn(); st != nil && !st.IsReadOnly() {
		_, _ = st.MarkTunnelOrderApplied(orderID)
	}
}

// closeTunnelIfOwned tears down the tunnel when THIS manager owns it (Close →
// mirror delete). Returns ownership.
func (m *TunnelManager) closeTunnelIfOwned(id string) bool {
	m.mu.Lock()
	mt, ok := m.tunnels[id]
	if ok {
		_ = mt.tunnel.Close()
		_ = mt.client.Close()
		delete(m.tunnels, id)
	}
	m.mu.Unlock()
	if ok {
		m.mirrorDelete([]string{id})
		log.Printf("kill order applied — closed tunnel %s", id)
	}
	return ok
}

func (m *TunnelManager) cascadeCheck(st *store.Store) {
	m.mu.Lock()
	live := len(m.tunnels)
	pid := m.projectID
	m.mu.Unlock()
	if live == 0 {
		m.cascadeFail = 0 // nothing to enforce: counter rests
		return
	}
	p, err := st.GetProject(pid)
	if err != nil {
		m.cascadeFail++
		log.Printf("tunnel control: cascade GetProject failed (%d consecutive): %v", m.cascadeFail, err)
	} else if p == nil || p.Status != models.ProjectActive {
		reason := "revoked/deleted"
		if p != nil {
			reason = string(p.Status)
		}
		m.closeAllTunnels("cascade: project "+pid+" status="+reason)
		m.cascadeFail = 0
		return
	} else {
		m.cascadeFail = 0
	}
	if m.cascadeFail > renewalFailThreshold {
		m.closeAllTunnels("enforcement degraded: cascade read failed " + itoa(m.cascadeFail) + " ticks")
		m.cascadeFail = 0
	}
}

func (m *TunnelManager) whitelistCheck(st *store.Store) {
	m.mu.Lock()
	var candidates []string // tunnel ids with non-loopback listen_host
	for id, mt := range m.tunnels {
		if ip := netParseLoopback(mt.meta.ListenHost); ip == nil { // non-loopback (or unparseable — treat as suspect too? NO: meta is gate-validated; unparseable impossible)
			_ = id // placeholder — real body below
		}
	}
	m.mu.Unlock()
	// (实作提示见 Step 3b)
}
```

**Step 3b（whitelistCheck 完整体——上面的骨架别照抄，以此为准）**：

```go
func (m *TunnelManager) whitelistCheck(st *store.Store) {
	m.mu.Lock()
	type victim struct{ id, host string }
	var suspects []victim
	liveNonLoopback := 0
	for id, mt := range m.tunnels {
		if !isLoopbackHost(mt.meta.ListenHost) {
			liveNonLoopback++
			suspects = append(suspects, victim{id, mt.meta.ListenHost})
		}
	}
	m.mu.Unlock()
	if liveNonLoopback == 0 {
		m.whitelistFail = 0
		return
	}
	hosts, err := st.ListForwardBindHosts()
	if err != nil {
		m.whitelistFail++
		log.Printf("tunnel control: whitelist read failed (%d consecutive): %v", m.whitelistFail, err)
		if m.whitelistFail > renewalFailThreshold {
			m.closeAllTunnelsNonLoopback("enforcement degraded: whitelist read failed " + itoa(m.whitelistFail) + " ticks")
			m.whitelistFail = 0
		}
		return
	}
	m.whitelistFail = 0
	allowed := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		allowed[h] = true
	}
	for _, s := range suspects {
		if !allowed[s.host] {
			m.closeTunnelIfOwned(s.id) // reuses close+mirror+log
		}
	}
}

// closeAllTunnelsNonLoopback: enforcement-downgrade variant — closes only
// non-loopback tunnels (spec §4: whitelist read failure cannot verify the
// approval for exposed binds; loopback stays — no approval needed).
func (m *TunnelManager) closeAllTunnelsNonLoopback(reason string) {
	m.mu.Lock()
	var ids []string
	for id, mt := range m.tunnels {
		if !isLoopbackHost(mt.meta.ListenHost) {
			_ = mt.tunnel.Close()
			_ = mt.client.Close()
			delete(m.tunnels, id)
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	m.mirrorDelete(ids)
	if len(ids) > 0 {
		log.Printf("%s — closed %d tunnels", reason, len(ids))
	}
}

func isLoopbackHost(host string) bool {
	ip := netParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// heartbeat renews every live tunnel's lease. ANY failure (write error on one
// row) marks the tick incomplete (counter advances — spec §4 部分失败);
// a zero-row renew means the mirror row is gone → self-close (spec §4 duty 5).
func (m *TunnelManager) heartbeat(st *store.Store) {
	m.mu.Lock()
	ids := make([]string, 0, len(m.tunnels))
	for id := range m.tunnels {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	if len(ids) == 0 {
		return // no active tunnels: counter suspended (spec §4)
	}
	failed := false
	ts := time.Now().Unix()
	for _, id := range ids {
		ok, err := st.RenewTunnelHeartbeat(id, ts)
		if err != nil {
			failed = true
			continue
		}
		if !ok {
			m.closeTunnelIfOwned(id) // zero-row ⇒ fell out of kill domain
			failed = true            // the tick did not fully complete its duty
		}
	}
	if failed {
		m.renewalFail++
		if m.renewalFail > renewalFailThreshold {
			m.closeAllTunnels("lease renewal failed " + itoa(m.renewalFail) + " ticks")
			m.renewalFail = 0
		}
	} else {
		m.renewalFail = 0 // ALL live tunnels renewed ⇒ complete (spec §4)
	}
}

func itoa(n int64) string { return strconvFormatInt(n) }
```

（`netParseIP`/`netParseLoopback`/`strconvFormatInt` 直接用标准库 `net.ParseIP`/`strconv.FormatInt` 起真名——上面为示意占位，实作一律标准库实名。）

sweepLoop 改造（tunnels.go）：

```go
func (m *TunnelManager) sweepLoop() {
	defer m.wg.Done()
	idle := time.NewTicker(forwardSweepInterval)
	defer idle.Stop()
	ctrl := time.NewTicker(controlInterval)
	defer ctrl.Stop()
	for {
		select {
		case <-idle.C:
			m.SweepIdle()
		case <-ctrl.C:
			m.runControlTick()
		case <-m.quit:
			return
		}
	}
}
```

`SweepIdle` 尾部补 `m.mirrorDelete(closed)`（若有 reap）。

- [ ] **Step 4: 跑测试确认通过 + 全包绿**

Run: `go test ./internal/mcpserver/ -run 'TestControlTick' -v` → PASS；`go test ./internal/mcpserver/ -count=1` 全绿（revoke_semantics 的 keeps-forwarding 用例此阶段仍绿——它不驱动控制 tick；Task 7 才翻转它）。

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/
git commit -m "feat(mcpserver): Plan 35 T4 — 15s 控制循环(订单状态机/级联/白名单复查/双计数/心跳零行自关闭/只读豁免)"
```

---

### Task 5: core gate + 工具层——listen_host 白名单 gate/audit/Input-Output/AttachStore 接线/工具描述

**Files:**
- Modify: `internal/mcpserver/core.go`（ForwardForProfile gate 块 + audit Command + Open 元数据真值）
- Modify: `internal/mcpserver/types.go`（ForwardInput/ForwardOutput）
- Modify: `internal/mcpserver/server.go`（:52-55 AttachStore + :130-146 forward_port 工具注册/描述 + :133 描述更新）
- Test: `internal/mcpserver/core_test.go`（追加 gate 用例）+ `internal/mcpserver/server_test.go`（若有工具层断言）

**Interfaces:**
- Consumes: Task 1 `CanonicalBindIP`/`ListForwardBindHosts`；Task 3 `TunnelMeta`；Task 2 `ForwardLocal` 5 参。
- Produces:
  - `func ForwardForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, remoteHost string, remotePort, localPort int, listenHost string, mgr *TunnelManager) (out ForwardOutput, err error)` —— 最终签名
  - `ForwardInput{... ListenHost string `json:"listen_host,omitempty"` ...}`；`ForwardOutput{... ListenHost string `json:"listen_host"` ...}`

- [ ] **Step 1: 写失败测试（gate 四态 + audit 两态 + Command 加 id）**

`core_test.go` 追加（`newStore`/`seedRealServer`/`startEchoListener` 既有）：

```go
func TestForwardListenHostGate(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	projID, _, _ := st.AddProject("proj", pid)
	mgr := NewTunnelManager()
	mgr.AttachStore(func() *store.Store { return st }, projID)
	defer mgr.CloseAll()
	echo := startEchoListener(t)
	ctx := context.Background()

	// 1. 空_listenHost → 环回,过
	if _, err := ForwardForProfile(ctx, st, projID, pid, srvID, "127.0.0.1", echo, 0, "", mgr); err != nil {
		t.Fatalf("default loopback must pass: %v", err)
	}
	// 2. 环回显式 → 过
	if _, err := ForwardForProfile(ctx, st, projID, pid, srvID, "127.0.0.1", echo, 0, "127.0.0.1", mgr); err != nil {
		t.Fatalf("explicit loopback must pass: %v", err)
	}
	// 3. 非法值(hostname/通配/zone) → bind_denied,错误文本不含白名单内容
	for _, bad := range []string{"example.com", "0.0.0.0", "::", "fe80::1%eth0"} {
		_, err := ForwardForProfile(ctx, st, projID, pid, srvID, "127.0.0.1", echo, 0, bad, mgr)
		if err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
		if !strings.Contains(err.Error(), bad) || strings.Contains(err.Error(), "whitelist {") {
			t.Fatalf("error must quote agent input only: %v", err)
		}
	}
	// 4. 非环回不在表 → bind_denied,文本为 spec 钉死的原句
	_, err := ForwardForProfile(ctx, st, projID, pid, srvID, "127.0.0.1", echo, 0, "10.99.99.99", mgr)
	if err == nil || !strings.Contains(err.Error(), "not in the owner-approved bind host whitelist") {
		t.Fatalf("not-in-whitelist error: %v", err)
	}
	// 5. 在表(规范形入,缩写传) → 过 + ForwardOutput.ListenHost 回报
	_ = st.AddForwardBindHost("2001:0db8::0001")
	ip := "2001:db8::1"
	if _, err := netListen Supports(t, ip); false { // 见下——bind 可用性 guard
	}
	out, err := ForwardForProfile(ctx, st, projID, pid, srvID, "127.0.0.1", echo, 0, ip, mgr)
	if err != nil {
		t.Skipf("bind %s unavailable here: %v", ip, err)
	}
	if out.ListenHost != ip {
		t.Fatalf("ListenHost = %q, want %q", out.ListenHost, ip)
	}
	mgr.Close(out.TunnelID)
	// 6. audit 断言:bind_denied / error 两态 + forward 行 Command 带 id=
	rows := auditRows(t, st, "forward")
	if len(rows) == 0 || !strings.Contains(rows[len(rows)-1].Command, " id=") {
		t.Fatalf("forward audit Command must carry id=: %+v", rows[len(rows)-1])
	}
	denied := auditRows(t, st, "forward") // filter status
	// 实作:auditRows helper 按 Action 过滤;断言存在 status=bind_denied 行
	_ = denied
}
```

（`auditRows` 小 helper：`st.db` 不可摸时用 `ListAudit` 之类既有读法；若无现成读法，在 store 加 `AuditRowsForTest(action string) []AuditRow`（testonly 命名先例）。bind 可用性 guard：IPv6 非环回在本机不可 bind 时整段 t.Skip——spec §8 行 4 口径。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcpserver/ -run 'TestForwardListenHostGate' -v`
Expected: FAIL（签名不含 listenHost，编译红）。

- [ ] **Step 3: 实现 core.go gate**

在 hidden-guard 块（:475-479）之后、`GetServer` 之前插入：

```go
	// listen_host gate (spec §2): default loopback; loopback always allowed;
	// non-loopback must be owner-whitelisted (canonical compare, per-call
	// read). Read failure on a non-loopback request fails CLOSED.
	bindHost := "127.0.0.1"
	if strings.TrimSpace(listenHost) != "" {
		canonical, cerr := store.CanonicalBindIP(listenHost)
		if cerr != nil {
			status = "bind_denied"
			err = fmt.Errorf("listen_host %q must be a specific IP literal (not a hostname, wildcard 0.0.0.0/::, or zoned address)", listenHost)
			return
		}
		if ip := net.ParseIP(canonical); ip.IsUnspecified() {
			status = "bind_denied"
			err = fmt.Errorf("listen_host %q is a wildcard address — binding 0.0.0.0/:: is forbidden", listenHost)
			return
		} else if !ip.IsLoopback() {
			hosts, lerr := st.ListForwardBindHosts()
			if lerr != nil {
				status = "error" // fail-closed (spec §2): DB read failure must not open a non-loopback bind
				err = fmt.Errorf("cannot read bind host whitelist, refusing non-loopback listen_host: %w", lerr)
				return
			}
			if !contains(hosts, canonical) {
				status = "bind_denied"
				err = fmt.Errorf("listen_host %q is not in the owner-approved bind host whitelist", listenHost)
				return
			}
		}
		bindHost = canonical
	}
```

（`contains` 已存在于 core.go:463 的同款助手——若签名是 `contains([]string, string)` 直接复用。）

`ForwardLocal` 调用换真值 + onActivity 接 Touch：

```go
	tun, ferr2 := cli.ForwardLocal(localPort, bindHost, remoteHost, remotePort, func() { mgr.Touch(tunIDPlaceholder) })
```

——注意闭包要引用 Open 之后的 id；实作顺序：先 `ForwardLocal` 返回 tun，再定义 `onActivity := func() { mgr.Touch(tun.ID) }` 需要 ForwardLocal 之后设置。**实作手法**：`ForwardLocal` 先传 `nil`，拿到 `tun` 后无法再注入……所以 Task 2 的钩子注入点改为：`Tunnel` 加 `SetOnActivity(func())`（ForwardLocal 参数保留 `onActivity func()`，同时提供 setter 供 Open 之后补挂）——**计划裁定**：`mgr.Open` 成功后调 `tun.SetOnActivity(func() { mgr.Touch(id) })`（Tunnel setter 原子赋值一次；Task 2 已有的构造参数路径保留给测试直用）。Task 2 文件补这个 setter（若 Task 2 已合并，此处为对 tunnel.go 的一行追加 + 注释）：

```go
// SetOnActivity attaches (or replaces) the activity callback after
// construction — the TunnelManager wires Touch here after Open returns the id.
func (t *Tunnel) SetOnActivity(fn func()) { t.onActivity = fn }
```

core.go 顺序：

```go
	tun, ferr2 := cli.ForwardLocal(localPort, bindHost, remoteHost, remotePort, nil)
	if ferr2 != nil { ...原错误路径... }
	id, oerr := mgr.Open(tun, cli, TunnelMeta{ProjectID: projectID, ServerID: serverID,
		Remote: net.JoinHostPort(remoteHost, strconv.Itoa(remotePort)), ListenHost: bindHost})
	if oerr != nil { status = "error"; err = oerr; return }
	tun.SetOnActivity(func() { mgr.Touch(id) }) // 活动感知回收接线 (spec §3)
	status = "ok"
	out = ForwardOutput{TunnelID: id, LocalPort: localPortOfAddr(tun.LocalAddr()), ListenHost: hostOfAddr(tun.LocalAddr())}
```

`hostOfAddr`（core.go 新小助手，紧挨 `localPortOfAddr`）：

```go
func hostOfAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
```

audit Command 改造（defer 块内引用变量）——函数头部加 `var tunID string`，Open 成功后 `tunID = id`；defer 里：

```go
		command: net.JoinHostPort(remoteHost, strconv.Itoa(remotePort)),
```
改为：

```go
		Command:    joinAuditCommand(remoteHost, remotePort, tunID),
```

```go
// joinAuditCommand renders the forward audit correlation key (spec §7):
// "host:port" until the tunnel id is known, "host:port id=<tunnelID>" after.
func joinAuditCommand(remoteHost string, remotePort int, tunID string) string {
	base := net.JoinHostPort(remoteHost, strconv.Itoa(remotePort))
	if tunID == "" {
		return base
	}
	return base + " id=" + tunID
}
```

`types.go`：

```go
type ForwardInput struct {
	ServerID   string `json:"server_id" jsonschema:"server id from list_servers (the SSH endpoint to forward through)"`
	RemoteHost string `json:"remote_host" jsonschema:"the host TO forward to, FROM THE SERVER'S PERSPECTIVE (usually '127.0.0.1' to reach a service on the server's own loopback)"`
	RemotePort int    `json:"remote_port" jsonschema:"the port on remote_host to reach"`
	LocalPort  int    `json:"local_port,omitempty" jsonschema:"optional local listen port (omit / 0 = let the broker pick a free port)"`
	ListenHost string `json:"listen_host,omitempty" jsonschema:"optional local address to bind (IP literal only; default 127.0.0.1; loopback always allowed — a non-loopback address must be owner-approved via the bind host whitelist)"`
}

type ForwardOutput struct {
	TunnelID   string `json:"tunnel_id" jsonschema:"opaque id; pass to close_port when done with the forward"`
	LocalPort  int    `json:"local_port" jsonschema:"the local port now forwarding to remote_host:remote_port"`
	ListenHost string `json:"listen_host" jsonschema:"the local address the forward is bound to (127.0.0.1 unless you passed a whitelisted listen_host)"`
}
```

`server.go`：
- :54-55 构造后接 `tunnels.AttachStore(storeFn, projectID)`（放在 `StartSweeper()` **之前**）；
- :137 调用改 `ForwardForProfile(ctx, st, projectID, profileID, in.ServerID, in.RemoteHost, in.RemotePort, in.LocalPort, in.ListenHost, tunnels)`；
- forward_port 描述（:133）末段改：`...holds an SSH connection open in the broker for the tunnel's life — call close_port with tunnel_id when done (tunnels auto-close after ~10 minutes of INACTIVITY — a tunnel carrying traffic stays alive).`（activity 口径，spec §3 措辞清理）；
- close_port 描述（:151）`~10-minutes-after-creation auto-close` → `~10-minutes-of-inactivity auto-close`。

- [ ] **Step 4: 跑测试确认通过 + 全包绿（含 revoke_semantics 现状语义）**

Run: `go test ./internal/mcpserver/ -count=1`
Expected: PASS 全部（翻转前的 keeps-forwarding 用例不驱动 tick，仍绿）。

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/ internal/sshbroker/tunnel.go
git commit -m "feat(mcpserver): Plan 35 T5 — listen_host 白名单 gate(规范形/fail-closed) + bind_denied audit + forward audit 加 id + Touch 接线"
```

---

### Task 6: owner CLI——serve bind ×3 + tunnels ls/kill/kill --project

**Files:**
- Create: `internal/cli/serve_bind.go`、`internal/cli/tunnels.go`
- Modify: `internal/cli/serve.go:95`（AddCommand 加 bind 组）、`internal/cli/root.go:16`（加 newTunnelsCmd()）
- Test: `internal/cli/serve_bind_test.go`、`internal/cli/tunnels_test.go`

**Interfaces:**
- Consumes: Task 1 store 方法；`openUnlockedStore()`（cli 既有，servers.go:194 同款）。
- Produces:
  - `ssh-manager serve bind add <ip>` / `rm <ip>` / `ls`
  - `ssh-manager tunnels ls` / `kill <tunnel_id>` / `kill --project <name>`
  - 内部 helper `func osUser() string`（`os/user`.Current().Username，出错回退 `unknown`）

- [ ] **Step 1: 写失败测试**

`internal/cli/serve_bind_test.go`（照 `cache_tokens_test.go` 的 root 执行模式——`root.SetArgs` + `Execute`）：

```go
package cli

import (
	"strings"
	"testing"
)

func TestServeBindCmd(t *testing.T) {
	// 环境搭法照 cache_tokens_test.go:openUnlockedStore 的测试环境注入(实现者对齐既有 helper)
	// 1. add 非法值拒
	for _, bad := range []string{"127.0.0.1", "0.0.0.0", "example.com"} {
		out := runCli(t, "serve", "bind", "add", bad)
		if !strings.Contains(out, "error") && !strings.Contains(out, "拒绝") {
			// 以实际错误输出风格断言——实现者对齐仓内 CLI 错误断言惯例
			t.Fatalf("add %q must fail, got: %s", bad, out)
		}
	}
	// 2. add 成功 + 幂等 + ls
	runCli(t, "serve", "bind", "add", "192.168.50.10")
	runCli(t, "serve", "bind", "add", "192.168.50.10") // 幂等不报错
	ls := runCli(t, "serve", "bind", "ls")
	if !strings.Contains(ls, "192.168.50.10") {
		t.Fatalf("ls must list entry: %s", ls)
	}
	// 3. rm 等价形式 + ls 空
	runCli(t, "serve", "bind", "rm", "192.168.50.10")
	if ls = runCli(t, "serve", "bind", "ls"); strings.Contains(ls, "192.168.50.10") {
		t.Fatalf("entry must be gone: %s", ls)
	}
}
```

`internal/cli/tunnels_test.go`：

```go
package cli

import (
	"strings"
	"testing"
	"time"
)

func TestTunnelsLsAndKillFlow(t *testing.T) {
	// 搭:store + echo 隧道行手工 INSERT(不经 mcpserver——CLI 只读 registry)
	st := cliTestStore(t) // 照既有 CLI 测试的 store 注入
	row := stInsertRegistry(t, st, "tun-abc", "proj-1")
	_ = row
	// ls:显示 tunnel_id/project_id/疑似残留口径
	out := runCli(t, "tunnels", "ls")
	if !strings.Contains(out, "tun-abc") || !strings.Contains(out, "proj-1") {
		t.Fatalf("ls output: %s", out)
	}
	// kill 预检:no-such → 快失败,不下单
	out = runCli(t, "tunnels", "kill", "no-such")
	if !strings.Contains(out, "no open tunnel") {
		t.Fatalf("precheck: %s", out)
	}
	if n := countOrders(t, st); n != 0 {
		t.Fatalf("precheck failure must not place an order, got %d", n)
	}
	// kill 真单:下单 + 模拟 broker 立即认领(手工 Mark) → CLI 报 applied
	go func() {
		time.Sleep(300 * time.Millisecond)
		stMarkFirstOrderApplied(t, st)
	}()
	out = runCli(t, "tunnels", "kill", "tun-abc")
	if !strings.Contains(out, "applied") {
		t.Fatalf("kill outcome: %s", out)
	}
	// created_by 记录
	if by := stFirstOrderCreatedBy(t, st); by == "" {
		t.Fatal("order must record created_by")
	}
}
```

（`runCli`/`cliTestStore`/`stInsertRegistry`/`countOrders`/`stMarkFirstOrderApplied`/`stFirstOrderCreatedBy` 为本文件小 helper——照 `cache_tokens_test.go` 的 root 注入法搭 store 环境，registry 行与订单用 store 公开方法直接写读。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run 'TestServeBind|TestTunnelsLs' -v`
Expected: FAIL（命令不存在）。

- [ ] **Step 3: 实现 serve_bind.go**

```go
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"ssh-manager-mcp/internal/store"
)

// newServeBindCmd: `ssh-manager serve bind add|rm|ls <ip>` — the owner's
// pre-approved non-loopback bind host whitelist (spec §2). Reads happen
// per-call in ForwardForProfile, so a change takes effect on the next
// forward_port without restarting serve.
func newServeBindCmd() *cobra.Command {
	c := &cobra.Command{Use: "bind", Short: "Manage the owner-approved non-loopback bind host whitelist (forward_port listen_host)"}
	c.AddCommand(serveBindAddCmd(), serveBindRmCmd(), serveBindLsCmd())
	return c
}

func serveBindAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <ip>",
		Args:  cobra.ExactArgs(1),
		Short: "Approve a non-loopback IP for forward_port listen_host",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.AddForwardBindHost(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "approved %s\n", args[0])
			return nil
		},
	}
}

func serveBindRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <ip>",
		Args:  cobra.ExactArgs(1),
		Short: "Revoke an approved bind host (existing tunnels bound to it close within ~15s)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			ok, err := s.RemoveForwardBindHost(args[0])
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintf(cmd.OutOrStdout(), "no whitelist entry for %s\n", args[0])
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %s (existing tunnels on it close within ~15s; loopback is unaffected)\n", args[0])
			return nil
		},
	}
}

func serveBindLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Args:  cobra.NoArgs,
		Short: "List the approved bind hosts",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			hosts, err := s.ListForwardBindHosts()
			if err != nil {
				return err
			}
			if len(hosts) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(empty — only loopback binds allowed)")
				return nil
			}
			for _, h := range hosts {
				fmt.Fprintln(cmd.OutOrStdout(), h)
			}
			return nil
		},
	}
}
```

`serve.go:95` 追加 `newServeBindCmd()`。

- [ ] **Step 4: 实现 tunnels.go**

```go
package cli

import (
	"fmt"
	"os/user"
	"time"

	"github.com/spf13/cobra"
	"ssh-manager-mcp/internal/store"
)

// killPollBudget: how long `tunnels kill` waits for a broker tick to apply
// the order (spec §4: 3× the 15s control interval).
const killPollBudget = 45 * time.Second

// staleHeartbeatSec: a registry row whose lease heartbeat is older than this
// is flagged as a probable ghost in `tunnels ls` (spec §6: 15s tick ×3).
const staleHeartbeatSec = 45

func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

func newTunnelsCmd() *cobra.Command {
	c := &cobra.Command{Use: "tunnels", Short: "Owner emergency stop for forward_port tunnels (kill / list live tunnels)"}
	c.AddCommand(tunnelsLsCmd(), tunnelsKillCmd())
	return c
}

func tunnelsLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Args:  cobra.NoArgs,
		Short: "List live tunnels (broker-held forwards, ≤45s mirror freshness)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			rows, err := s.ListTunnelRegistry()
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no open tunnels — note: tunnels held by OFFLINE cache clients are not in this domain; handle those on the client machine)")
				return nil
			}
			now := time.Now().Unix()
			for _, r := range rows {
				flag := ""
				if now-r.LastRenewed > staleHeartbeatSec {
					flag = "  [stale heartbeat — probable ghost of a dead broker process, auto-cleared ≤30min]"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s  project=%s  server=%s  %s -> %s  bind=%s  opened=%s%s\n",
					r.TunnelID, r.ProjectID, r.ServerID, r.LocalAddr, r.Remote, r.ListenHost,
					time.Unix(r.OpenedAt, 0).Format(time.RFC3339), flag)
			}
			return nil
		},
	}
}

func tunnelsKillCmd() *cobra.Command {
	var project string
	c := &cobra.Command{
		Use:   "kill [<tunnel_id>]",
		Short: "Tear down a tunnel (or all of a project's tunnels via --project) — surgical, does not revoke the token",
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 1) == (project == "") { // exactly one target form
				return fmt.Errorf("pass exactly one of: a tunnel_id argument, or --project <name>")
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			var oid int64
			if len(args) == 1 {
				has, err := s.HasTunnelRegistryRow(args[0])
				if err != nil {
					return err
				}
				if !has {
					return fmt.Errorf("no open tunnel %s (this command covers brokers that write the authoritative vault — serve and online stdio; tunnels held by OFFLINE cache clients must be handled on that machine)", args[0])
				}
				oid, err = s.CreateTunnelOrder(args[0], "", osUser())
			} else {
				p, err := resolveProject(s, project)
				if err != nil {
					return err
				}
				if n, _ := s.CountTunnelRegistryProject(p.ID); n == 0 {
					return fmt.Errorf("no open tunnels for project %s", project)
				}
				oid, err = s.CreateTunnelOrder("", p.ID, osUser())
			}
			if err != nil {
				return err
			}
			return waitForOrder(cmd, s, oid, project != "")
		},
	}
	c.Flags().StringVar(&project, "project", "", "project name or id — kill ALL its tunnels")
	return c
}

func resolveProject(s *store.Store, nameOrID string) (*models.Project, error) {
	if p, err := s.GetProjectByName(nameOrID); err == nil && p != nil {
		return p, nil
	}
	if p, err := s.GetProject(nameOrID); err == nil && p != nil {
		return p, nil
	}
	return nil, fmt.Errorf("project %q not found", nameOrID)
}

func waitForOrder(cmd *cobra.Command, s *store.Store, oid int64, isProject bool) error {
	deadline := time.Now().Add(killPollBudget)
	for time.Now().Before(deadline) {
		o, err := s.GetTunnelOrder(oid)
		if err != nil {
			return err
		}
		if o != nil && o.Outcome != nil && *o.Outcome == "applied" {
			fmt.Fprintln(cmd.OutOrStdout(), "applied")
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	if isProject {
		fmt.Fprintln(cmd.OutOrStdout(), "order pending — no broker applied it within 45s (a broker may be offline). The order only tears down tunnels that existed while it was pending; an agent that keeps RE-opening tunnels needs `projects disable/revoke` to stop it.")
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), "order pending — no broker applied it within 45s (target may belong to an offline/dead process; it will complete when a writable broker ticks)")
	return nil
}
```

（`models` import 按仓内路径 `ssh-manager-mcp/internal/models`。`root.go:16` 追加 `newTunnelsCmd()`。）

- [ ] **Step 5: 跑测试确认通过 + CLI 全包绿**

Run: `go test ./internal/cli/ -count=1` → PASS。

- [ ] **Step 6: Commit**

```bash
git add internal/cli/
git commit -m "feat(cli): Plan 35 T6 — serve bind add/rm/ls + tunnels ls/kill/kill --project(45s 轮询/created_by/离线域话术)"
```

---

### Task 7: 契约翻转 + conformance/eval + 全量文档 + backlog 销项

**Files:**
- Modify: `internal/mcpserver/revoke_semantics_test.go`（翻转 `TestRevokedProjectKeepsOpenTunnelForwarding`；另两个测试不动）
- Create: `internal/conformance/tunnel_kill_test.go`、`internal/eval/t11_test.go`
- Modify: `docs/agent-access.md`、`docs/threat-model.md`、`docs/agent-tools.md`、`docs/multi-machine.md`、`docs/compat-matrix.md`、`README.md`、`docs/backlog.md`

**Interfaces:**
- Consumes: Task 4 `runControlTick`；Task 6 CLI。
- Produces: 契约翻转后的钉子测试 + T11 eval 门 + 文档口径。

- [ ] **Step 1: 翻转 revoke 钉子测试（先写红）**

`revoke_semantics_test.go`——`TestRevokedProjectKeepsOpenTunnelForwarding` 整函数替换为：

```go
// TestRevokedProjectTunnelsTornByControlTick flips the OLD Plan-25 pin
// ("revoked project's tunnel keeps forwarding — owner decision, kill CLI was
// backlog"). Plan 35 contract (spec §1): revoke cascades into tunnel teardown
// within one control tick (~15s; tests drive runControlTick directly for
// determinism). The HTTP per-request 401 layer above is unchanged (see
// TestServeHTTPRejectsRevokedTokenPerRequest); background-task survival is
// unchanged (TestRevokedProjectKeepsBackgroundTaskRunning, Plan 32 pin).
func TestRevokedProjectTunnelsTornByControlTick(t *testing.T) {
	st := newStore(t)
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	srvID := seedRealServer(t, st, "real", addr, hk, "")
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, []string{srvID})
	projID, token, err := st.AddProject("proj", pid)
	if err != nil {
		t.Fatal(err)
	}
	echoPort := startEchoListener(t)
	mgr := NewTunnelManager()
	mgr.AttachStore(func() *store.Store { return st }, projID)
	defer mgr.CloseAll()

	out, err := ForwardForProfile(context.Background(), st, projID, pid, srvID, "127.0.0.1", echoPort, 0, "", mgr)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := st.VerifyToken(token); p == nil {
		t.Fatal("sanity: token must verify before revoke")
	}
	if err := st.SetProjectStatus(projID, models.ProjectRevoked); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.VerifyToken(token); p != nil {
		t.Fatal("layer 1: VerifyToken must reject a revoked token immediately")
	}
	mgr.runControlTick() // deterministic stand-in for the ≤15s tick
	// layer 3 (flipped): the tunnel is TORN DOWN — port unreachable + mirror row gone
	if _, derr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", out.LocalPort), 500*time.Millisecond); derr == nil {
		t.Fatal("port must be unreachable after revoke+tick")
	}
	if has, _ := st.HasTunnelRegistryRow(out.TunnelID); has {
		t.Fatal("mirror row must be gone after cascade teardown")
	}
}
```

文件头注释块同步改写（"already-open forward keeps forwarding" 段 → 新契约三句）。**另两个测试一行不动。**

- [ ] **Step 2: 跑翻转测试确认绿 + 后台任务钉子仍绿**

Run: `go test ./internal/mcpserver/ -run 'TestRevoked|TestServeHTTP' -v`
Expected: PASS 全部（新翻转用例 + Plan 32 后台任务钉子 + 401 测试）。

- [ ] **Step 3: conformance kill 端到端**

`internal/conformance/tunnel_kill_test.go`——照 `upload_forward_test.go` 的 harness 模式（docker/sshd 桩 + 真 broker 路径），断言链：forward_port 开隧道（dial 通）→ `ssh-manager tunnels kill <id>`（45s 内）→ dial 拒绝 + `tunnels ls` 无行。实现者以 `upload_forward_test.go` 的搭法为模板，差异仅工具名与断言（dial 不可达为主断言；环境不可用按该目录既有 skip 惯例）。

Run: `go test ./internal/conformance/ -run TestTunnelKill -v`（环境缺 docker/sshd 时按既有 SKIP 惯例——SKIP 为正常出口）。

- [ ] **Step 4: eval T11**

`internal/eval/t11_test.go`——照 `t10_test.go` 的门内用例模式，做「kill 急停」：agent 侧 forward_port 成功 → owner 侧 tunnels kill → 断言端口不可达 + 隧道下线（评分器挂接照 t10 的 scorer 接线）。

Run: `go test ./internal/eval/ -run T11 -v`（eval 门 SKIP 惯例同上）。

- [ ] **Step 5: 文档全量联动（spec §10 逐条）**

- `docs/agent-access.md`：第 3 层（:110）整段按 spec §1 引文重写（含白名单撤回收缩 + 时效口径 + 进程 hang 边界）；应急表 :182 行与排障表 :204 行补 `tunnels kill` / 「DB 故障 ~2min 批量关闭（看 lease renewal failed / enforcement degraded 日志）」/ project kill 弱语义（expired→「持续 pending = 持续重开，防重开 disable/revoke」口径按 rev4 已无 expired 的最终措辞）。
- `docs/threat-model.md`：(b) 节补急停 + 白名单现状三句；「接口级不暴露」清单加一行 listen_host 拒绝文本不披露白名单内容。
- `docs/agent-tools.md`：forward_port 段补 listen_host 参数说明 + auto-close 口径改 activity。
- `docs/multi-machine.md`：NUC10 VLAN bind 惯用法（`serve bind add <VLAN-IP>` → agent `listen_host`）+ 笔记本恒 loopback + 离线客户端隧道不在 kill/ls 域。
- `docs/compat-matrix.md`：契约变更行（revoke/disable→隧道 ≤15s 拆[store 健康+控制循环存活前提]；forward_port 新参数 listen_host；新表×3；新 CLI serve bind/tunnels；audit forward 行 Command 追加 `id=`；kill/ls 域完整性要求全部可写 broker 升级）——按文件既有表格格式插入，版本行归属留 owner 发版拍板（写占位注释）。
- `README.md` 命令清单补 `serve bind` 与 `tunnels` 两组。
- `docs/backlog.md`：#15 划线销项（照 #3 的销项格式：~~原文~~ + 已落地标注 + spec/plan 路径 + owner 手工复验项）。

- [ ] **Step 6: 全量回归**

Run: `go build ./... && go test ./... -count=1`
Expected: 全绿（conformance/eval 按 SKIP 惯例）。

- [ ] **Step 7: Commit**

```bash
git add internal/ docs/ README.md
git commit -m "test+docs: Plan 35 T7 — revoke 契约翻转钉子 + conformance/eval T11 + 六文档联动 + backlog #15 销项"
```

---

## Self-Review 记录

- **Spec coverage**：§1（T4 级联 + T7 翻转/文档）、§2（T1 表/规范化 + T5 gate + T6 bind CLI + T4 存量复查）、§3（T2 钩子 + T5 Touch 接线 + 措辞清理）、§4（T1 订单方法 + T4 状态机/双计数/心跳/豁免 + T6 kill CLI）、§5（T4 cascadeCheck + T7 翻转）、§6（T1 registry + T3 镜像管道 + T4 GC/心跳）、§7（T5 audit/Command + T6 created_by + T4 日志）、§8（各任务内嵌用例 + T7 conformance/eval）、§9/§10（T7 文档 + 不做清单已在 spec，无需任务）、§11 即本计划——无缺口。
- **Placeholder 扫描**：测试 helper（`newTestClient`/`runCli`/`auditRows` 等）均注明「以仓内既有同名/同型 helper 为准」并给出断言核心——属指向既有惯例而非占位；`whitelistCheck` 明确标注以 Step 3b 完整体为准（防实现者抄到骨架）。可接受。
- **类型一致性**：`ForwardForProfile` 最终签名 T5 与 T3/T4 测试调用一致（10 参）；`Open(t, c, meta) (string, error)` T3 产出与 T5 调用一致；`RenewTunnelHeartbeat(string, int64) (bool, error)` T1/T4 一致；`runControlTick` T4 产出与 T7 调用一致。
