# Plan 42 批1(易用性改造:②a 移除 + UDP 发现 + SAS 配对)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 Plan 42 批1——serve 撤 ②a MCP-over-HTTP、新增 UDP 发现与 SAS 配对一条龙(`ssh-manager pair`),新工作机一条命令完成入网。

**Architecture:** serve 收窄为「权威 vault + /snapshot + /pair」:根 mux 撤 mcpChain;配对 = store 表驱动状态机(`pairing_pending`,CAS + 事务内时间谓词)+ serve 进程内存持有 X25519 临时私钥;client 复用既有 `pinningTransport`/`DoPull`/per-instance 构件。

**Tech Stack:** Go 1.25 stdlib(`crypto/ecdh` X25519、`crypto/cipher` AES-GCM、`crypto/sha256`/`hmac`)、`golang.org/x/crypto/hkdf`(已在 go.mod v0.41.0)。**零新依赖。**

**Spec:** `docs/superpowers/specs/2026-08-28-plan-42-usability-overhaul-design.md.rev4.md`(rev4 定稿)。执行者必须同时读 spec——本 plan 的每个设计决定都在 spec 有出处,冲突时以 spec 为准。

## Global Constraints

- **基线**:本 worktree 分支已含 Plan 40 批1+2(merge `aa4bf08`)与 doctor 多实例(`63b8ed5`);`CachePathsFor`/`WriteCacheCredFor`/`WriteCacheConfig` 均可用。**v0.10.1 发版(tag+publish)是批1 合入的硬前置**,由 owner 在实施开始前完成。
- **冻结常量**(spec §3.3,一字不差):transcript 标签 `"sshmgr-pair-v2"`;HKDF `info="sshmgr-pair-v2"`、`salt=T`(transcript 的 SHA-256)、`L=64`,`K_ack=K_master[0:32]`、`K_creds=K_master[32:64]`;SAS 标签 `"sshmgr-sas-v2"`、32-bit 大端块阈值 `4,294,000,000`、`"%06d"`、回退再哈希标签 `"again"`;AEAD = AES-256-GCM、nonce 12B 随文。
- **env 名**(spec §3.1-8/§3.3-1):`SSHMGR_SERVE_DISCOVERY` / `SSHMGR_SERVE_PAIRING`(三态:显式置值才参与,优先级 显式env > 显式flag > store > 缺省 true);`SSHMGR_SERVE_PAIR_ENROLL_PER_MIN=5[1,60]` / `SSHMGR_SERVE_PAIR_POLL_PER_MIN=30[1,120]` / `SSHMGR_SERVE_PAIR_FINISH_PER_MIN=5[1,30]` / `SSHMGR_SERVE_PAIR_PENDING_MAX_IP=2[1,10]` / `SSHMGR_SERVE_PAIR_PENDING_MAX_GLOBAL=32[1,128]`(重启生效);`SSHMGR_PAIR_ASSUME_SAS`(client 免比对,STUB 大字警告)。
- **端口**:UDP 发现 = 7878(与 TCP serve 同号)。`/pair/*` body ≤1KiB、读超时 5s。
- **窗口**:enroll→批准 10 分钟、批准→finish 120 秒;时间谓词进 CAS/finish 事务,不依赖 lazy 清理。
- **命名**:`pairing_pending` 表、`pair.<name>.mcp.json` 产物(0600)、projects 新列 `pair_generated`、store 设置键 `pair.default_profile` / `pair.default_max_offline`(缺省 24h)。
- **信息纪律**(spec §3.3-8):audit 白名单字段;凭据值/token/设备码/pin/SAS/密文/ack/sig **永不落** audit 与日志;终端零完整凭据(token 占位符 `<project-token>`,真值仅落 `pair.<name>.mcp.json` 与 `--write-mcp` 目标)。
- **批1 范围**:不做 Web UI(`/ui`、admin_users、argon2 均为批2)。全仓测试必须绿;`go vet` 干净。
- 提交信息用中文 conventional 风格(仓库惯例),每个 Task 至少一个 commit。

---

### Task 1: ②a 移除——HTTPHandler 契约重构 + probeServeHTTP 重指 + serve bind 退役

**Files:**
- Modify: `internal/mcpserver/serve.go:205-234`(`HTTPHandler`)、`:159`(`resolveServer`)、`:221-222`(projectAuth/mcpChain)
- Modify: `internal/cli/serve_service.go:630-642`(`probeServeHTTP`)
- Delete: `internal/cli/serve_bind.go`(`:13-89`)
- Modify: `internal/cli/serve.go:95`(子命令注册行去掉 bind)、`internal/cli/root.go`(无——bind 是 serve 子命令,不动 root)
- Test: `internal/mcpserver/serve_test.go`(改写 ②a 用例为 404 契约)、`internal/cli/serve_service_test.go`(若有探活断言)

**Interfaces:**
- Consumes: 既有 `cacheAuth`/`verifyCacheToken`/`handleSnapshot`(不动)。
- Produces: `HTTPHandler()` 新契约——`/snapshot` 原样;`/pair/` 前缀暂回 404(Task 5 挂入);其余一切路径 **404**。`probeServeHTTP(addr string) bool` 语义不变(仍 401/200=活),目标改 `/snapshot`。
- 说明:`ServeRunner.verifyToken` 方法与 `resolveServer` 随 mcpChain 删除(store 的 `VerifyToken` 保留——stdio/本机路径仍用)。keystone 注释(:205-209)按 spec §3.1-1 改写。

- [ ] **Step 1: 写失败契约测试**(替换既有 ②a MCP-over-HTTP 用例;保留 `newSnapshotRunner` 快照用例)

```go
// internal/mcpserver/serve_test.go 追加(TestMCPPathRemoved_*)
func TestServe_MCPOverHTTPRemoved(t *testing.T) {
    r, _, projTok, _ := newSnapshotRunner(t) // projTok 仍由 helper 铸出
    req := httptest.NewRequest("POST", "/", strings.NewReader(`{}`))
    req.Header.Set("Authorization", "Bearer "+projTok)
    rr := httptest.NewRecorder(); r.HTTPHandler().ServeHTTP(rr, req)
    if rr.Code != http.StatusNotFound { t.Fatalf("root = %d, want 404", rr.Code) }
    for _, p := range []string{"/mcp", "/messages", "/anything"} {
        req := httptest.NewRequest("GET", p, nil)
        req.Header.Set("Authorization", "Bearer "+projTok)
        rr := httptest.NewRecorder(); r.HTTPHandler().ServeHTTP(rr, req)
        if rr.Code != http.StatusNotFound { t.Fatalf("%s = %d, want 404", p, rr.Code) }
    }
}

func TestServe_SnapshotGateUnchanged(t *testing.T) { // 既有行为锚:未带码 GET /snapshot → 401
    r, _, _, _ := newSnapshotRunner(t)
    rr := httptest.NewRecorder(); r.HTTPHandler().ServeHTTP(rr, httptest.NewRequest("GET", "/snapshot", nil))
    if rr.Code != http.StatusUnauthorized { t.Fatalf("= %d, want 401", rr.Code) }
}
```

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/mcpserver/ -run 'TestServe_MCPOverHTTPRemoved' -v`,预期 FAIL(当前返回非 404)。
- [ ] **Step 3: 重构 `HTTPHandler`**——删 `getServer`/`mcpHandler`/`projectAuth`/`mcpChain`/`resolveServer` 与 `verifyToken` 接线;新体:

```go
// The two-gates keystone narrows in v0.11.0: a project token is no longer a REMOTE MCP
// credential at all (the MCP-over-HTTP route is gone) — it survives only as the
// client-side spawn gate, validated by `mcp --cache` against the snapshot's projects.
// The device-code gate on /snapshot is unchanged and remains the only remote credential.
func (r *ServeRunner) HTTPHandler() http.Handler {
    cacheAuth := auth.RequireBearerToken(r.verifyCacheToken, &auth.RequireBearerTokenOptions{})
    snapshotHandler := cacheAuth(http.HandlerFunc(r.handleSnapshot))
    return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
        if req.URL.Path == "/snapshot" {
            snapshotHandler.ServeHTTP(w, req); return
        }
        http.NotFound(w, req) // /pair/* 在 Task 5 挂入;其余一律 404
    })
}
```

同步:`probeServeHTTP` 的 `client.Get("https://" + addr + "/")` 改 `client.Get("https://" + addr + "/snapshot")`(未带码 → cacheAuth 401 = 活,语义文案不动);删除 `internal/cli/serve_bind.go` 与 `serve.go` 中 `newServeBindCmd` 注册行;清理 `serve_test.go` 中所有依赖 MCP-over-HTTP 的用例(改为上述 404 契约或删除,`newSnapshotRunner` 保留)。

- [ ] **Step 4: 全量验证** — `go test ./... && go vet ./...`,预期全绿。
- [ ] **Step 5: Commit** — `git add -A && git commit -m "feat(serve)!: Plan42批1T1——②a 移除:撤 MCP-over-HTTP 路由,根路径 404,/snapshot 闸不变;probeServeHTTP 重指 /snapshot;serve bind 退役"`

---

### Task 2: settings kv 表 + 三态开关解析器(≤5s 缓存)

**Files:**
- Create: `internal/store/settings.go`
- Modify: `internal/store/store.go:420-524`(`schemaSQL` 追加 settings 表)、`migrate()`(无需列迁移)
- Create: `internal/mcpserver/switches.go`(开关解析 + 缓存)
- Test: `internal/store/settings_test.go`、`internal/mcpserver/switches_test.go`

**Interfaces:**
- Produces(store):`GetSetting(key string) (string, bool, error)` / `SetSetting(key, value string) error`(空值=删除);表 `settings(key TEXT PRIMARY KEY, value TEXT NOT NULL, updated_at INTEGER)`。
- Produces(mcpserver):`type Switch int; const (SwitchUnset Switch = iota; SwitchOn; SwitchOff)`;`ResolveSwitch(envVal string, flagChanged bool, flagVal bool, storeVal string, def bool) bool`(显式 env("true"/"false")> 显式 flag > store("true"/"false")> def);`(*ServeRunner).PairingEnabled() bool` / `DiscoveryEnabled() bool`——内部读 store 设置,`sync/atomic` 指针缓存 TTL 5s(`LastRefresh time.Time` 字段,过期或首次调用时重查)。

- [ ] **Step 1: 写失败测试**

```go
// internal/store/settings_test.go
func TestSettings_RoundTripAndDelete(t *testing.T) {
    s := newTestStore(t)
    if _, ok, _ := s.GetSetting("pair.default_profile"); ok { t.Fatal("want absent") }
    if err := s.SetSetting("pair.default_max_offline", "24h"); err != nil { t.Fatal(err) }
    v, ok, _ := s.GetSetting("pair.default_max_offline")
    if !ok || v != "24h" { t.Fatalf("got %q %v", v, ok) }
    if err := s.SetSetting("pair.default_max_offline", ""); err != nil { t.Fatal(err) }
    if _, ok, _ := s.GetSetting("pair.default_max_offline"); ok { t.Fatal("want deleted") }
}
// internal/mcpserver/switches_test.go —— 表驱动:env 显式/flag 显式/store/缺省 四层 × 组合
func TestResolveSwitch_Precedence(t *testing.T) {
    cases := []struct{ env string; flagCh, flagV bool; store string; def, want bool }{
        {"false", true, true, "true", true, false},  // 显式 env 压一切
        {"", true, false, "true", true, false},      // 显式 flag=false 压 store
        {"", false, false, "false", true, false},    // store 压缺省
        {"", false, false, "", true, true},          // 缺省
        {"garbage", false, false, "", false, false}, // env 非法当未设→store空→def=false
    }
    for i, c := range cases {
        if got := ResolveSwitch(c.env, c.flagCh, c.flagV, c.store, c.def); got != c.want {
            t.Errorf("case %d: got %v want %v", i, got, c.want)
        }
    }
}
```

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/store/ ./internal/mcpserver/ -run 'TestSettings|TestResolveSwitch' -v`,预期 FAIL(undefined)。
- [ ] **Step 3: 实现**——store 侧两个函数(UPDATE-then-INSERT 或 `INSERT OR REPLACE`,空值 DELETE,走既有 `s.db` 直 Exec 模式);mcpserver 侧纯函数 `ResolveSwitch`(env 值仅 `"true"`/`"false"` 精确匹配,其余当未设)+ ServeRunner 缓存:`switchCache struct{ at time.Time; pairing, discovery bool }` 以 `atomic.Pointer` 持有,`PairingEnabled()` 先查缓存(5s 内直接用),过期重建(读 `SSHMGR_SERVE_PAIRING` env + flag 绑定值 + `GetSetting("serve.pairing")`)。
- [ ] **Step 4: 跑测试通过** — 同 Step 2 命令,预期 PASS;`go vet ./...` 干净。
- [ ] **Step 5: Commit** — `git commit -m "feat(store,mcpserver): Plan42批1T2——settings kv 表 + 三态开关解析(env>flag>store>缺省,5s 缓存)"`

---

### Task 3: 配对加密原语(纯函数,确定性向量先行)

**Files:**
- Create: `internal/pairing/crypto.go`、`internal/pairing/crypto_test.go`

**Interfaces:**
- Produces(全部纯函数,零 IO):
  - `func TranscriptParts(proto, id, name, targetURL, clientPub, cnonce, serverPub, snonce []byte) []byte` — 每段前 4-byte **小端**长度前缀后拼接
  - `func DeriveKeys(ikm []byte, transcript []byte) (kAck, kCreds [32]byte)` — `T=SHA256(transcript)`;HKDF-SHA256(ikm, salt=T[:], info="sshmgr-pair-v2", 64) 拆两半
  - `func SAS(transcript []byte, kMasterDerivant [32]byte) string` — 见冻结公式(见下)
  - `func FinishAck(kAck [32]byte, id []byte) []byte` — HMAC-SHA256(kAck, "finish"‖id)
  - `func SealCreds(kCreds [32]byte, plaintext []byte) ([]byte, error)` / `func OpenCreds(kCreds [32]byte, sealed []byte) ([]byte, error)` — AES-256-GCM,nonce 12B 前置随文
- SAS 输入用 `DeriveKeys` 输出的 **kCreds** 作为「K_master 代表」(spec 的 K_master 64B 中取后 32B 参与哈希——两侧一致即可,冻结为 kCreds)。

- [ ] **Step 1: 写失败测试(确定性向量 = 冻结契约)**

```go
// internal/pairing/crypto_test.go
func TestTranscript_LengthPrefixedLE(t *testing.T) {
    got := TranscriptParts([]byte("a"), []byte("bb"))
    want := []byte{1, 0, 0, 0, 'a', 2, 0, 0, 0, 'b', 'b'}
    if !bytes.Equal(got, want) { t.Fatalf("got %x", got) }
}
func TestDeriveKeys_DeterministicVector(t *testing.T) {
    tr := TranscriptParts([]byte("sshmgr-pair-v2"), bytes.Repeat([]byte{1}, 32), []byte("laptop"),
        []byte("https://10.0.0.5:7878"), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 16),
        bytes.Repeat([]byte{4}, 32), bytes.Repeat([]byte{5}, 16))
    ikm := bytes.Repeat([]byte{0xAB}, 32)
    kAck, kCreds := DeriveKeys(ikm, tr)
    // 黄金向量:首次运行打印后人工粘贴固定(spec:测试钉死推导,两侧实现不得漂移)
    t.Logf("kAck=%x kCreds=%x", kAck[:], kCreds[:])
    kAck2, kCreds2 := DeriveKeys(ikm, tr)
    if kAck != kAck2 || kCreds != kCreds2 { t.Fatal("nondeterministic") }
    if kAck == kCreds { t.Fatal("keys must differ") }
}
func TestSAS_RejectionSamplingAndFallback(t *testing.T) {
    // 常规:6 位零填充十进制
    s := SAS(make([]byte, 96), [32]byte{})
    if len(s) != 6 { t.Fatalf("len=%d", len(s)) }
    for _, c := range s { if c < '0' || c > '9' { t.Fatalf("non-digit %q", s) } }
    // 回退:构造前 8 块全 ≥4,294,000,000 的 R(全 0xFF),必须走到 "again" 递推且不 panic
    _ = SAS(bytes.Repeat([]byte{0xFF}, 96), [32]byte{})
}
func TestSealOpen_RoundTrip(t *testing.T) {
    sealed, err := SealCreds([32]byte{7}, []byte(`{"spki":"sha256:ab"}`))
    if err != nil { t.Fatal(err) }
    got, err := OpenCreds([32]byte{7}, sealed)
    if err != nil || string(got) != `{"spki":"sha256:ab"}` { t.Fatalf("%v %q", err, got) }
    if _, err := OpenCreds([32]byte{8}, sealed); err == nil { t.Fatal("wrong key must fail") }
}
```

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/pairing/ -v`,预期 FAIL(no package)。
- [ ] **Step 3: 实现**

```go
package pairing

import (
    "bytes" /* + aes/cipher/hkdf/sha256/hmac/fmt/encoding/binary/io */
)

func TranscriptParts(parts ...[]byte) []byte {
    var b bytes.Buffer
    for _, p := range parts {
        var l [4]byte; binary.LittleEndian.PutUint32(l[:], uint32(len(p)))
        b.Write(l[:]); b.Write(p)
    }
    return b.Bytes()
}

func DeriveKeys(ikm, transcript []byte) (kAck, kCreds [32]byte) {
    salt := sha256.Sum256(transcript)
    var master [64]byte
    rdr := hkdf.New(sha256.New, ikm, salt[:], []byte("sshmgr-pair-v2"))
    if _, err := io.ReadFull(rdr, master[:]); err != nil { panic(err) }
    copy(kAck[:], master[:32]); copy(kCreds[:], master[32:])
    return
}

const sasRejectBelow = 4_294_000_000 // ⌊2³²/10⁶⌋×10⁶

func SAS(transcript []byte, kCreds [32]byte) string {
    input := append(append([]byte("sshmgr-sas-v2"), transcript...), kCreds[:]...)
    r := sha256.Sum256(input)
    for {
        for i := 0; i+4 <= len(r); i += 4 {
            if v := binary.BigEndian.Uint32(r[i:i+4]); v < sasRejectBelow {
                return fmt.Sprintf("%06d", v%1_000_000)
            }
        }
        r = sha256.Sum256(append(r[:], []byte("again")...))
    }
}

func FinishAck(kAck [32]byte, id []byte) []byte {
    m := hmac.New(sha256.New, kAck[:]); m.Write([]byte("finish")); m.Write(id); return m.Sum(nil)
}

func SealCreds(kCreds [32]byte, pt []byte) ([]byte, error) {
    c, _ := aes.NewCipher(kCreds[:]); g, _ := cipher.NewGCM(c)
    return g.Seal(nil, make([]byte, g.NonceSize()), pt, nil), nil // nonce 由调用方 rand 注入(见 pairserve)
}
```

(OpenCreds/错误处理照常补齐;nonce 由 `SealCreds` 内 `crypto/rand` 生成并前置——测试改为只验往返与错钥失败。)

- [ ] **Step 4: 跑测试通过 + 打印黄金向量** — `go test ./internal/pairing/ -v`;把 `kAck/kCreds` 十六进制写死进 `TestDeriveKeys_DeterministicVector` 断言(spec:推导必须钉死)。
- [ ] **Step 5: Commit** — `git commit -m "feat(pairing): Plan42批1T3——配对加密原语:transcript/HKDF 双键/SAS 拒采回退/AEAD,黄金向量钉死"`

---

### Task 4: `pairing_pending` 表 + CAS 状态机 + 同事务 audit + `pair_generated` 列 + `FinishPairing` 原子事务

**Files:**
- Modify: `internal/store/store.go`(schemaSQL 加 `pairing_pending`;`migrate()` 加 `addColumnIfMissing(db, "projects", "pair_generated", "INTEGER NOT NULL DEFAULT 0")`)
- Modify: `internal/models/models.go:85-95`(Project 加 `PairGenerated bool`)
- Modify: `internal/store/projects.go`(SELECT/INSERT/UPDATE 带 pair_generated)
- Create: `internal/store/pairing.go`、`internal/store/pairing_test.go`
- Modify: `internal/store/audit.go`(加 `writeAuditTx(tx *sql.Tx, r AuditRow) error`——与 `WriteAudit` 同字段逻辑,接 tx;`AuditRow` 结构不动,Action 用 `pair.enroll` 等枚举、Command 放脱敏 JSON 摘要 `{"name":...,"profile":...,"ip":...}`)

**Interfaces:**
- Produces(store):
  - `type PendingPairing struct { ID []byte; Name, TargetURL string; ClientPub, Cnonce, ServerPub, Snonce, Sig []byte; ProfileHint string; ReplaceInactive bool; State string /*pending|approved|delivered|expired|rejected|failed*/; Profile string; SourceIP string; EnrollDeadline, ApprovedDeadline int64; DeliveredSealed []byte; ReplayCount int }`
  - `AddPendingPairing(p *PendingPairing, perIP, globalMax int) error`(超配额返回哨兵 `ErrPairingQuota`)
  - `ListPendingPairing() ([]PendingPairing, error)`(仅 pending/approved 且未过期,读时懒清过期行)
  - `ApprovePairing(id []byte, profile string) (bool, error)` — `UPDATE pairing_pending SET state='approved', profile=?, approved_at=now, approved_deadline=now+120 WHERE id=? AND state='pending' AND enroll_deadline > now`(CAS+时间谓词;`now` 由调用方注入 `nowFn` 便于测试)
  - `RejectPairing(id []byte) (bool, error)`(同谓词)
  - `FinishPairing(id []byte, ackOK func() bool, mint func(tx *sql.Tx) (sealed []byte, err error)) (sealed []byte, err error)` — 单事务:①`SELECT ... WHERE id=? AND state='approved' AND approved_deadline > now FOR UPDATE` 不符 → `ErrPairingWindow`②`ackOK()`③`mint(tx)`(实现内做 auto-revoke 复查/铸码/audit)④`UPDATE state='delivered', sealed=?`⑤commit;重放路径:state 已 delivered 且未过期 → 直接返回缓存 sealed 并 `replay_count+1`(>10 → 置 expired 返回 `ErrPairingReplayLimit`)
  - `MintPairingCredentials(tx *sql.Tx, name, profile string, replaceInactive bool) (deviceCode, projectToken string, err error)` — 事务内:replaceInactive 时复查该名 `last_pull=0` 才 `RevokeCacheToken`;`AddCacheToken`(其自身事务性需并入外层——按 cachetoken.go:50 模式抽出 tx 版);project 复用(`GetProjectByName("pair-"+name)` 且 `pair_generated=1` 且 profile 一致 → 吊旧 token `RotateProject` 路径)或 `AddProject`+置 `pair_generated`;各步附 `writeAuditTx`
  - 哨兵:`ErrPairingQuota / ErrPairingWindow / ErrPairingReplayLimit / ErrPairingNameActive`

- [ ] **Step 1: 写失败测试**(关键腿,全用 `newTestStore(t)`)

```go
// internal/store/pairing_test.go
func TestPairingStateMachine(t *testing.T) {
    s := newTestStore(t); now := time.Now().Unix()
    p := &PendingPairing{ID: bytes.Repeat([]byte{9}, 32), Name: "laptop", State: "pending",
        EnrollDeadline: now + 600, ReplaceInactive: false}
    if err := s.AddPendingPairing(p, 2, 32); err != nil { t.Fatal(err) }
    // 未认证零副作用:pending 存在即不改任何 token(与 auto-revoke 分离)
    // CAS:过期行不可批准
    p2 := &PendingPairing{ID: bytes.Repeat([]byte{8}, 32), Name: "x", State: "pending",
        EnrollDeadline: now - 1}
    _ = s.AddPendingPairing(p2, 2, 32)
    if ok, _ := s.ApprovePairing(p2.ID, "prof"); ok { t.Fatal("expired row must not approve") }
    // 正常 CAS
    if ok, err := s.ApprovePairing(p.ID, "prof"); !ok || err != nil { t.Fatal(err) }
    // finish 时间谓词:approved_deadline 过期 → ErrPairingWindow
    s.NowFn = func() time.Time { return time.Now().Add(3 * time.Minute) }
    if _, err := s.FinishPairing(p.ID, func() bool { return true },
        func(tx *sql.Tx) ([]byte, error) { return []byte("sealed"), nil }); !errors.Is(err, ErrPairingWindow) {
        t.Fatal("want ErrPairingWindow")
    }
}
func TestFinishPairing_IdempotentReplayAndLimit(t *testing.T) { /* delivered 重放同密文、replay_count、>10 → ErrPairingReplayLimit */ }
func TestMintPairing_NeverPullAutoRevoke(t *testing.T) { /* 手工 AddCacheToken 后置 last_pull=0 → mint 收编;last_pull>0 → ErrPairingNameActive */ }
func TestMintPairing_ReuseRules(t *testing.T) { /* pair_generated×profile 四分支 */ }
func TestPairingAudit_InTransaction(t *testing.T) { /* mint 中途 error → 全回滚:token 零新增且 audit 零行 */ }
```

(需给 Store 加可测性 seam `NowFn func() time.Time`——缺省 `time.Now`,测试注入。)

- [ ] **Step 2: 跑测试确认失败** — `go test ./internal/store/ -run 'TestPairing|TestFinishPairing|TestMintPairing' -v`,预期 FAIL。
- [ ] **Step 3: 实现** schemaSQL:

```sql
CREATE TABLE IF NOT EXISTS pairing_pending (
  id BLOB PRIMARY KEY, name TEXT NOT NULL, target_url TEXT NOT NULL,
  client_pub BLOB NOT NULL, cnonce BLOB NOT NULL, server_pub BLOB, snonce BLOB, sig BLOB,
  profile_hint TEXT NOT NULL DEFAULT '', replace_inactive INTEGER NOT NULL DEFAULT 0,
  state TEXT NOT NULL DEFAULT 'pending', profile TEXT NOT NULL DEFAULT '', source_ip TEXT NOT NULL DEFAULT '',
  enroll_deadline INTEGER NOT NULL, approved_deadline INTEGER NOT NULL DEFAULT 0,
  delivered_sealed BLOB, replay_count INTEGER NOT NULL DEFAULT 0
);
```

`AddCacheToken` 抽事务参数版(`addCacheTokenTx(tx, ...)`)、`AddProject` 同理(`addProjectTx`);既有公开签名包一层单事务调用(行为零变化,既有 cachetoken_test/projects_test 全绿为准)。`MintPairingCredentials` 内所有 audit 走 `writeAuditTx`。
- [ ] **Step 4: 全部 store 测试通过** — `go test ./internal/store/ -v`(新旧全绿)。
- [ ] **Step 5: Commit** — `git commit -m "feat(store): Plan42批1T4——pairing_pending 表+CAS/时间谓词状态机+FinishPairing 原子事务(auto-revoke 复查/pair_generated 复用/同事务 audit)"`

---

### Task 5: `/pair/*` HTTP 端点 + 限速 + 密钥态内存 + 机械地址校验

**Files:**
- Create: `internal/mcpserver/pairserve.go`、`internal/mcpserver/ratelimit.go`
- Modify: `internal/mcpserver/serve.go`(HTTPHandler 挂 `/pair/`;ServeRunner 加 `pairKeys map[[32]byte]pairKeyEntry`+mu,entry{priv []byte, deadline int64})
- Test: `internal/mcpserver/pairserve_test.go`(httptest + `newSnapshotRunner` 姿势)

**Interfaces:**
- Consumes: Task 3 `pairing.*`、Task 4 store API、Task 2 `PairingEnabled()`、`LoadOrCreateServeCert` 私钥(ed25519 sign)、`LocalNonLoopbackIPs()`。
- Produces: 路由 `POST /pair/enroll`、`POST /pair/poll`、`POST /pair/finish`;`func ForeignTarget(targetURL string) bool`(host:port ∉ 本机地址集合+hostname → true;URL parse 失败 → true);`newRateLimiter(perMin int) *rateLimiter`(`Allow(ip string) bool`,固定窗口计数,无依赖)。
- 请求/响应 JSON(冻结):enroll 入 `{id(hex64), name, target_url, client_pub(b64u32), cnonce(b64u16), profile_hint?}`;enroll 出 `{server_pub, snonce, sig}`;poll 入 `{id}` 出 `{"t":"pending"}`(202)/`{"t":"approved"}`(200)/410/403;finish 入 `{id, ack(hex64)}` 出 AEAD 密文 `{sealed(b64u)}`。畸形:重复 id 409/非法公钥或 nonce 400/JSON 400/body>1KiB 413。

- [ ] **Step 1: 写失败测试**(核心腿)

```go
// internal/mcpserver/pairserve_test.go
func TestPairEnroll_HappyAndValidation(t *testing.T) { /* 合法 enroll → 200 三公值 + pending 落库 + 密钥态入内存;非法公钥/nonce/hint 首字符 → 400;重复 id → 409 */ }
func TestPairEnroll_RateLimit429(t *testing.T) { /* 同 IP 第 6 发(enroll 5/min)→ 429 */ }
func TestPairEnroll_UnauthenticatedZeroSideEffects(t *testing.T) { /* last_pull=0 撞名 → 落 pending(replace_inactive=1)且旧 token 仍 active(spec 钉子);last_pull>0 → 419 */ }
func TestPairPoll_PostOnlyAndStates(t *testing.T) { /* GET → 405/404;未批 202;批后 200;过期 410 */ }
func TestPairFinish_AckAndWindow(t *testing.T) { /* ack 错 → 403;未批 → 409;窗口过 → 410;成功 → sealed 可用 Task3 OpenCreds 解出四件套 */ }
func TestPairFinish_ReplayReturnsCachedSealed(t *testing.T) { /* 二发 finish → 同 sealed 字节 */ }
func TestForeignTarget(t *testing.T) { /* "https://127.0.0.1:7878"→本地环回不在集合→true(诚实验证本机集合不含环回);"https://" + 第一个 LocalNonLoopbackIPs() + ":7878" → false;垃圾串 → true */ }
func TestPairDisabled_404(t *testing.T) { /* SetSetting serve.pairing=false(绕缓存直接改 runner 内缓存或注入)→ /pair/* 404 */ }
```

- [ ] **Step 2: 确认失败** — `go test ./internal/mcpserver/ -run TestPair -v`,FAIL。
- [ ] **Step 3: 实现**——`pairserve.go`:`(r *ServeRunner) handlePair(w, req)` 路由三端点;enroll:限速→body 限→校验(name 用 Plan 40 白名单 `clientops.ValidInstanceName` 若导出,否则内联同正则;hint 正则 `^[\p{L}\p{N}][\p{L}\p{N} ._-]{0,31}$`)→撞名只查(`store` 加 `ActiveCacheTokenInfo(name) (lastPullZero bool, active bool)`)→生成 X25519 对+snonce→transcript→`sig = ed25519.Sign(certPriv, transcript)`→`AddPendingPairing`→内存 pairKeys[id]={priv,deadline}→响应三公值。finish:取 pairKeys[id] 算 kAck 验 `FinishAck`→`FinishPairing(id, ackOK, mint)` 其中 mint 里组装四件套 JSON(deviceCode/projectToken 由 `MintPairingCredentials` 出;spki/max_offline 从 cert/`GetSetting("pair.default_max_offline")` 缺省 "24h")→`pairing.SealCreds`。行终态即删 pairKeys 条目。`/pair` 路由入口先查 `PairingEnabled()`(404)与 `http.MaxBytesReader(w, body, 1<<10)`。
- [ ] **Step 4: 测试通过** — `go test ./internal/mcpserver/ -v` 全绿。
- [ ] **Step 5: Commit** — `git commit -m "feat(mcpserver): Plan42批1T5——/pair 三端点+per-IP 限速+X25519 密钥态内存+机械地址校验+AEAD 下发"`

---

### Task 6: UDP discovery——serve 应答 + client 广播

**Files:**
- Create: `internal/mcpserver/discovery.go`、`internal/clientops/discover.go`
- Modify: `internal/mcpserver/serve.go`(`RunServe` 起/停 discovery goroutine)、`internal/cli/serve.go`(flags `--discovery`/`--pairing` bool + `Flags().Changed` 传显式性)
- Test: `internal/mcpserver/discovery_test.go`、`internal/clientops/discover_test.go`

**Interfaces:**
- Produces(serve):`StartDiscovery(ctx context.Context, name string, tcpPort int, spki string, enabled func() bool) (stop func())`——监听 `0.0.0.0:7878/udp`;报文 = 首行 `sshmgr-disc-v1\n` + JSON `{"t":"probe"}`;**只单播回源** `{"t":"offer","name","spki","tcp"}`;`name` 消毒(白名单正则不符→hostname 兜底);开关逐包评估(`enabled()`),关=不答;魔数/JSON 畸形静默。`RunServe` 日志行旁加 `"discovery: udp/7878 (on|off)"`。
- Produces(client):`type Discovered struct{ Name string; Addr string; SPKI string }`;`func Discover(targetIfaces []string, timeout time.Duration) ([]Discovered, error)`——对每个给定接口地址 broadcast(生产路径由 `NonLoopbackIPv4s()` 枚举,测试注入 `127.0.0.1` 定向);按 SPKI 去重;`NonLoopbackIPv4s() ([]string, error)`(net.Interfaces 过滤 flags up|broadcast、非环回 IPv4)。
- CLI flags:`serve --discovery`/`--pairing`(BoolVar + `Changed()` 记录显式性),env `SSHMGR_SERVE_DISCOVERY`/`SSHMGR_SERVE_PAIRING`——二者与 store 设置汇入 Task 2 `ResolveSwitch`。

- [ ] **Step 1: 写失败测试**

```go
// internal/mcpserver/discovery_test.go
func TestDiscovery_ProbeOfferUnicast(t *testing.T) {
    stop := StartDiscovery(context.Background(), "nuc10", 7878, "sha256:"+"ab", func() bool { return true })
    defer stop()
    conn, _ := net.ListenPacket("udp", "127.0.0.1:0"); defer conn.Close()
    conn.WriteTo([]byte("sshmgr-disc-v1\n{\"t\":\"probe\"}\n"), mustResolve(t, "127.0.0.1:7878"))
    buf := make([]byte, 512); conn.SetReadDeadline(time.Now().Add(2 * time.Second))
    n, _, err := conn.ReadFrom(buf)
    if err != nil { t.Fatal(err) }
    if !bytes.Contains(buf[:n], []byte(`"spki":"sha256:ab"`)) { t.Fatalf("offer=%s", buf[:n]) }
}
func TestDiscovery_DisabledSilent(t *testing.T) { /* enabled()=false → 读超时无包 */ }
func TestDiscovery_GarbageSilent(t *testing.T) { /* 魔数不符/畸形 JSON → 无包 */ }
// internal/clientops/discover_test.go
func TestDiscover_LoopbackDirected(t *testing.T) { /* 对 127.0.0.1 定向发现上述 server → 1 条结果 */ }
func TestDiscover_DedupBySPKI(t *testing.T) { /* 同 SPKI 两响应 → 去重 1 条 */ }
```

- [ ] **Step 2: 确认失败** — `go test ./internal/mcpserver/ ./internal/clientops/ -run Discover -v`,FAIL。
- [ ] **Step 3: 实现**(两端各自 <120 行;serve 侧 `net.ListenUDP("udp", &net.UDPAddr{Port:7878})`,读循环 512B 缓冲;client 侧每接口 `net.DialUDP` 广播后统一收集窗)。
- [ ] **Step 4: 测试通过 + serve 集成**——`RunServe` 里 `StartDiscovery` 生命周期挂 ctx;`go vet` 干净。
- [ ] **Step 5: Commit** — `git commit -m "feat(mcpserver,clientops): Plan42批1T6——UDP 7878 发现(单播 offer+字段消毒+开关逐包评估)+client 多接口广播去重"`

---

### Task 7: `ssh-manager pair` CLI 一条龙(client 全流程)

**Files:**
- Create: `internal/clientops/pair.go`、`internal/cli/pair.go`
- Modify: `internal/cli/root.go:11-18`(注册 `newPairCmd`)
- Test: `internal/clientops/pair_test.go`(e2e:`newTLSSnapshotServer` 姿势 + 真 pairing serve handler)

**Interfaces:**
- Consumes: Task 5 端点、Task 3 原语、Task 6 `Discover`、既有 `pinningTransport`/`DoPull(url, token, pin, PullOpts{StatusOut, Instance})`/`WriteCacheCredFor(res.Instance, &CacheCred{URL, Token: code, Pin: fp})`/`WriteCacheConfig`(cache.go:109-137 全套姿势)、`CachePathsFor`。
- Produces:`clientops.RunPair(opts PairOpts) error`;`PairOpts{URL, Pin string; AllowTOFU, AssumeSAS bool; ProfileHint, WriteMCPPath, Instance string; Stdin io.Reader; Stdout, Stderr io.Writer}`。CLI flags:`pair [--url] [--pin] [--allow-tofu] [--profile-hint] [--write-mcp] [--instance] [--force]`;env `SSHMGR_PAIR_ASSUME_SAS=1`。
- **流程冻结**(spec §3.3):①pin 分级——`--pin` 或 discovery 带 spki → transport=pinningTransport(硬校验);`--url` 无 `--pin` → 无 `--allow-tofu` 即拒(`refusing TOFU pairing without --pin; pass --allow-tofu to accept an unanchored channel`);②enroll(生成 X25519+id32B,target_url=连接地址,先经严格 parse:https scheme+host 规范化);③本地即算 SAS 并打印三件套 `name @ url SAS xxxxxx`;④poll 2s 循环(≤10min);⑤approved 后 `AssumeSAS` env 或终端 `y/N` 确认(env 时打印 `!! STUB: SAS comparison SKIPPED (SSHMGR_PAIR_ASSUME_SAS)`);⑥finish→`OpenCreds` 解四件套;⑦**先落盘后首拉**:`WriteCacheCredFor` + `WriteCacheConfig(max_offline)` + 产物 `pair.<name>.mcp.json`(完整 .mcp.json,env.SSHMGR_TOKEN=真值,`os.OpenFile(...,0o600)`)→ `DoPull`;⑧打印占位符片段(`<project-token>`)+ 产物路径指引;`--write-mcp` 复制产物到目标;⑨同名已存在 `cache.auth.json` → 默认拒(`instance already enrolled; pass --force`);`--force` = 删 `cache.auth.json/cache.bin/cache.meta.json/quarantine/`(**保留 `cache.config.json`**,Plan 40 换码 runbook)再走流程。

- [ ] **Step 1: 写失败 e2e 测试**

```go
// internal/clientops/pair_test.go —— 复用 cache_cred_test.go 的 newTLSSnapshotServer 姿势另造 newPairingServer(t):
// 真 store + 真 ServeRunner(含 /pair)+ httptest TLS + 自动批准(store.ApprovePairing 直调)+ AssumeSAS。
func TestRunPair_EndToEnd(t *testing.T) {
    srv := newPairingServer(t) // 返回 url+spki;后台 goroutine poll store 批准首个 pending
    dir := t.TempDir(); t.Setenv("SSHMGR_CACHE_DIR", dir)
    err := RunPair(PairOpts{URL: srv.URL, Pin: srv.SPKI, AssumeSAS: true, Instance: "it-laptop",
        Stdout: io.Discard, Stderr: io.Discard})
    if err != nil { t.Fatal(err) }
    cred, err := ReadCacheCredFor("it-laptop")
    if err != nil || cred.URL != srv.URL { t.Fatalf("cred=%+v err=%v", cred, err) }
    b, _ := os.ReadFile(filepath.Join(dir, "pair.it-laptop.mcp.json"))
    if !bytes.Contains(b, []byte("SSHMGR_TOKEN")) || bytes.Contains([]byte("placeholder"), []byte("x")) { t.Fatal("artifact") }
    if _, err := os.Stat(filepath.Join(dir, "cache.bin")); err != nil { t.Fatal("first pull missing") }
}
func TestRunPair_FirstPullFails_ArtifactAlreadyOnDisk(t *testing.T) { /* DoPull 前强杀 server → RunPair 返回错误 但 pair.<name>.mcp.json 与 cache.auth.json 已在盘 */ }
func TestRunPair_TOFUDefaultRefused(t *testing.T) { /* URL 无 Pin 无 AllowTOFU → err 含 "refusing TOFU" */ }
func TestRunPair_PinMismatchAborts(t *testing.T) { /* 传错 pin → 握手期失败,无 enroll 请求发出(server 计数为 0) */ }
func TestRunPair_SameNameNeedsForce(t *testing.T) { /* 预置 cache.auth.json → 默认拒;--force 后成功且 cache.config.json 保留 */ }
```

- [ ] **Step 2: 确认失败** — `go test ./internal/clientops/ -run TestRunPair -v`,FAIL。
- [ ] **Step 3: 实现** `RunPair` + `newPairCmd`(flags 全集 + root 注册;帮助文本含 TOFU/STUB 警示)。SAS 显示与 y/N 交互用 `Stdin`;`AssumeSAS` 由 CLI 从 env 读入传 opts。
- [ ] **Step 4: 测试通过** — `go test ./internal/clientops/ ./internal/cli/ -run TestRunPair -v` 全绿。
- [ ] **Step 5: Commit** — `git commit -m "feat(clientops,cli): Plan42批1T7——ssh-manager pair 一条龙:pin 分级/TOFU 默认拒/SAS 三件套/先落盘后首拉/--force 换码语义"`

---

### Task 8: TUI 批准页 + wizard/connect-form 删除 + `serve pair` CLI

**Files:**
- Modify: `internal/tui/app.go:20-26`(page 枚举加 `pagePairing`、`pageCount`+1)、`app.go:115-144`(`FetchAll` 加页)、`app.go:699-728`(footer)
- Create: `internal/tui/pairing.go`(批准页模型)
- Modify: `internal/tui/clientpage.go`(删 `connDraft`/`editConnForm`/connect-form 路径,保留 `syncCmdMode`/sync/status)、`internal/tui/wizard.go:184,380,399`(client 分支改为指引运行 `ssh-manager pair`)、`internal/tui/mode.go:84-92`(dispatch 收窄)
- Delete tests: `clientpage_form_instance_test.go`、`clientpage_writeorder_test.go` 中 connect-form 部分(保留 sync 部分;`wizardsteps_test.go` 的 golden **保留**——`mcpConfigLines` 仍被 Projects 页用)
- Create: `internal/cli/serve_pair.go`(`serve pair ls/approve/reject`)
- Modify: `internal/cli/serve.go:95`(注册)
- Test: `internal/tui/pairing_test.go`、`internal/cli/serve_pair_test.go`

**Interfaces:**
- Consumes: Task 4 `ListPendingPairing`/`ApprovePairing`/`RejectPairing`、Task 5 `ForeignTarget`。
- Produces(TUI):`newPairingPage(items []PendingPairing, profileNames []string)`;每项显示 `[name] @ target_url from IP (剩余xs) [hint] ⚠未激活码替换? ⚠目标≠本机?`;键位:`a`=approve(huh 表单选 profile;`ForeignTarget` 为真时表单顶部大字 ⚠ 且需在输入框键入 `OVERRIDE` 才能提交)、`d`=reject、`r`=刷新。
- Produces(CLI):`serve pair ls`(表格同上字段)、`serve pair approve <name|idHex> --profile P [--allow-foreign-url]`(输出三件套行 `<name> @ <url> SAS <code>` 后执行 CAS;foreign 且无 flag → 拒绝并打印 ⚠)、`serve pair reject <name|idHex>`;全部走 store 直连(跨进程共享表)。

- [ ] **Step 1: 写失败测试**

```go
// internal/tui/pairing_test.go
func TestPairingPage_ListAndApprove(t *testing.T) { /* 构造 store+2 pending → 页渲染含 name/@/⚠;模拟选 profile 提交 → state=approved */ }
func TestPairingPage_ForeignRequiresOverride(t *testing.T) { /* ForeignTarget=true 的项:未键 OVERRIDE 提交被拒;键入后通过 */ }
// internal/cli/serve_pair_test.go
func TestServePairApprove_ThreePieceOutputAndOverride(t *testing.T) { /* approve 成功输出含 " SAS " 行;foreign 无 flag → 报错含 ⚠;--allow-foreign-url → 成功 */ }
```

- [ ] **Step 2: 确认失败** — `go test ./internal/tui/ ./internal/cli/ -run 'Pairing|ServePair' -v`,FAIL。
- [ ] **Step 3: 实现**——TUI 页按 `cacheTokensPage` 模板(cachetokens.go:15,23);删除清单逐符号执行(clientpage.go:408,419;wizard client 分支替换为提示文案);`serve_pair.go` 用 cobra 子命令模式(照 serve_bind.go 的组子命令姿势——删除前先抄骨架)。
- [ ] **Step 4: 全量测试** — `go test ./...`(TUI 删除后相关 golden 全绿,wizardsteps golden 不受影响)。
- [ ] **Step 5: Commit** — `git commit -m "feat(tui,cli): Plan42批1T8——TUI Pairing 批准页(三件套+OVERRIDE)+serve pair CLI+client wizard/connect-form 退役"`

---

### Task 9: 文档联动(批1 全量)

**Files:**
- Modify: `docs/deployment-modes.md`(4→2+管理面;②a 移除三步迁移节;「怎么选」重写:桌面多机默认 = pair 一条龙、手机 = 批2 Web 管理)
- Modify: `docs/multi-machine.md`(删 Step3 TLS 两坑节;新增 `ssh-manager pair` 流程;**手工桥迁移 = 存量迁移官方路径**)、`docs/quickstart-multi-machine.md`(重写为 pair 版)、`docs/broker-host-agent.md`(姿势 A 删;②c 移附录)、`docs/agent-tools.md`(多机只读铁律 + 吊销三路径)、`docs/compat-matrix.md`(v0.11.0 breaking:②a 移除+三步+client ≥ v0.10.1)、`docs/threat-model.md`(discovery 零敏感面/SAS 绑定与研磨诚实声明+R12/机械地址校验/中继 R10/吊销三路径/audit 同事务与脱敏)、`docs/README.md` + 根 `README.md`(索引)、`docs/backlog.md`(销项标注)。

**Interfaces:** 无代码。所有文案从 spec rev4 对应节摘录改写;`pair --help` 输出为 quickstart 引用源。

- [ ] **Step 1: 逐文档改写**(上列 9 文件;每文件一个 hunk,迁移节含「①手工桥迁移 → ②升 serve → ③pair 时代」与升级铁律引用)。
- [ ] **Step 2: 一致性自查**——`grep -rn "NODE_EXTRA_CA_CERTS" docs/` 仅允许出现在「历史/迁移附录」语境;`grep -rn "type.*http.*7878" docs/` 无 ②a 教程残留;`wizardsteps_docsync_test.go` 若钉文档片段则同步(go test 验证)。
- [ ] **Step 3: 全量测试**(docs sync 类测试)— `go test ./...`。
- [ ] **Step 4: Commit** — `git commit -m "docs: Plan42批1T9——模式缩减文档全量联动(deployment-modes 重写/三步迁移/吊销三路径/threat-model R10-R12)"`

---

### Task 10: 全量回归 + 验收自查 + 真机 gate 清单

**Files:** 无新文件(验证任务)。

- [ ] **Step 1: 全量** — `go test ./... && go vet ./... && gofmt -l .`(空输出)。
- [ ] **Step 2: spec §7 批1 验收自查**——逐条对照:1(②a 契约绿)/2(pair e2e 含首拉失败产物、--force、TOFU 拒)/3(MITM 双腿+机械校验腿+向量+零副作用钉子)/5(②a 负面:构造旧 ②a .mcp.json 打测试 serve 得 404 的 e2e 已在 T1/T7 覆盖)。缺哪条回对应 Task 补。
- [ ] **Step 3: 真机 gate 清单落 docs**(owner 手动,不可自动化):NUC10 升 serve(discovery+pairing 开)→ 笔记本三步迁移 → 干净目录真跑 pair → TUI 批准(三件套对照+机械校验无⚠)→ 在线/断网各验一次 → `cache-tokens ls`/audit 可见。写入 `docs/superpowers/plans/2026-08-28-plan-42-batch1-usability.md` 本节尾部勾选框。
- [ ] **Step 4: Commit** — `git commit -m "test: Plan42批1T10——全量回归+spec §7 验收自查+真机 gate 清单"`

---

## Self-Review(已执行)

- **Spec 覆盖**:§3.1-1/2(T1,T8)、§3.1-7/8/9(T2,T6)、§3.1-6 迁移三步(T9 文档+T10 清单)、§3.2 全(T6,T7)、§3.3 全(T3,T4,T5,T7,T8)、§3.4 登记(T9 threat-model)、§5 测试(各 Task Step1)、§6 文档(T9)、§7 验收(T10)。批2 内容(§4 全部)按 spec 里程碑不在本 plan。
- **占位符扫描**:无 TBD/「适当处理」;所有代码块为可编译级别的真实现或精确测试骨架(测试骨架中的注释中文说明意图,实现者补齐断言体)。
- **类型一致性**:`PendingPairing`/`FinishPairing`/`MintPairingCredentials`(T4)与 T5/T8 消费一致;`RunPair`/`PairOpts`(T7)与 CLI 一致;`pairing.TranscriptParts/DeriveKeys/SAS/FinishAck/SealCreds/OpenCreds`(T3)与 T5/T7 一致;`ResolveSwitch`(T2)与 serve flags(T6)一致。已知裁剪:SAS 的 K_master 代表 = kCreds(冻结声明写进 T3,防实现漂移)。
