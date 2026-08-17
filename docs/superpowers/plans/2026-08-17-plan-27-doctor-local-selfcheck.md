# Plan 27：doctor 首版本机自检（rev2 P4）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付 `ssh-manager doctor`——零副作用的本机自检命令：角色/路径/密钥/可解性/证书/缓存一次体检，PASS/WARN/FAIL 三态 + remediation 提示 + 稳定退出码。

**Architecture:** 新增 `internal/cli/doctor.go`（检查框架 + 检查项表驱动），复用既有构件：`paths.*`（env-aware 路径解析）、`store.ValidMasterKeyLen`、`store.Open`+`ExportSnapshot`（**在副本上**做全量解密探针——真库零写入）、`mcpserver` 新增只读 `ReadServeCertFingerprint`（不触发生成）、kardianos 服务状态模式（`runServeStatus` 同款，藏在可 stub 的 seam 后）。serve HTTP 探活 = 二期（docs/backlog.md #5）。

**Tech Stack:** Go 标准库 + 既有依赖，零新增。

## Global Constraints

- **铁律不动**：doctor 是 owner 侧 CLI，零 MCP surface 改动，不碰凭据暴露面。
- **无副作用**：不对真实库/证书/缓存做任何写或 open-with-migrate；解密探针只在 `os.MkdirTemp` 副本上做，退出前删除副本；**无网络调用**（版本只打印本地 `buildinfo.Version`，不查 Releases）。
- **不打印 secret**：绝不输出 master key 内容、token、设备码、凭据明文；只输出路径、大小、条数、年龄、公开 pin 指纹。
- **稳定退出码**：`0` = 无 FAIL（WARN 不影响）；`1` = ≥1 FAIL；`2` = doctor 自身内部错误。写进 `--help` 与文档。
- **测试密闭**：所有路径经 `SSHMGR_*` env seam / `t.TempDir()` 钉住（`withClearDirs` 同款纪律）；服务状态查询走可 stub 的包级 func seam（`serveInstalledFn` 先例）。
- **无新依赖**；gofmt/vet 干净；双 lane CI 必绿（符号链接类不需要，纯文件/服务状态）。
- **文案与已实现行为一致**。

## 背景（本会话源码取证）

1. **可复用构件**：`vaultStatusString()`（cli/serve_service.go:676，FileKeyProvider+ValidMasterKeyLen 三态文案）；`ValidMasterKeyLen`（store）；`ExportSnapshot()`（store/export.go:114，**逐条解密全部凭据**——任何 key/密文错配都会在这里爆 GCM 错，即 NUC10 FINDING A 类事故的检测器）；`serveInstalledFn` 等 func-var seam（clear.go stub 先例）；`runServeStatus` 的 kardianos `s.Status()` 模式（serve_service.go:452）。
2. **`store.Open` 有副作用**（建库+迁移）→ 真库上直接 Open 违反无副作用约束 → 探针必须 copy-to-scratch。
3. **`LoadOrCreateServeCert` 会生成证书**（副作用）→ doctor 需要只读孪生 `ReadServeCertFingerprint`：不存在→明确报错；corrupt/错配→报错；**marker 在而 cert 不在→F10 报错**（这是既有语义，只读路径同样要对）。
4. **cache 路径**：`clientops.CachePaths()`（SSHMGR_CACHE_DIR env-aware）返回 dir/bin/meta/audit；`cache.auth.json` = `filepath.Join(dir, "cache.auth.json")`（clear.go 枚举先例）；cache DEK = `paths.CacheDekPath()`。
5. **role**：`roles.Load()` 双目录判定；`SetupComplete=false` = 向导未走完。

## 设计决策（定死，评审按此判）

- **检查项全集（v1）**：`env` / `role` / `store` / `masterkey` / `vault-open`(副本探针) / `serve-cert` / `serve-svc` / `client-cache`，每项 `{Name, Status(PASS/WARN/FAIL/INFO), Detail, Fix}`；`INFO` = 有意跳过（如 client 机无 vault）。
- **serve 检查按"存在即查"而非纯 role 门控**：cert 文件存在或 `serveInstalledFn()` 为真才查（standalone 机装了 serve 也能体检）；否则一行 INFO "serve not in use"。
- **client-cache 同理**：cache.bin 存在或 role=client 才查。
- **副本探针跳过条件**：store.db 或 master.key 任一缺失 → `vault-open` = INFO（前置项已 FAIL/记录）。
- **版本行**：头部 `ssh-manager doctor (<buildinfo.Version>)`，不是检查项。

## 任务间接口

- T1 产出检查框架（`doctorCheck` struct + runner + 渲染 + 退出码）——T2/T3/T4 的检查项都往里挂。
- T3 产出 `probeVaultDecrypt() (counts, error)`（cli 包内）。
- T4 产出 `mcpserver.ReadServeCertFingerprint()`（跨包新 API）+ cli 侧 `serveServiceState()` seam。

---

### Task 1: 检查框架 + role/env 检查 + 命令注册

**Files:**
- Create: `internal/cli/doctor.go`
- Create: `internal/cli/doctor_test.go`
- Modify: `internal/cli/root.go`（AddCommand 挂 `doctor`）

**Interfaces:**
- Produces（T2/T3/T4 挂载点）:

```go
type checkStatus string

const (
	statusPass checkStatus = "PASS"
	statusWarn checkStatus = "WARN"
	statusFail checkStatus = "FAIL"
	statusInfo checkStatus = "INFO"
)

type doctorCheck struct {
	Name   string
	Status checkStatus
	Detail string // 一行人类可读；不含任何 secret
	Fix    string // remediation；PASS/INFO 可为空
}

// runDoctor 执行全部检查、渲染、按约定退出码返回 error：
// nil=0(无FAIL) / errDoctorFindings=1(有FAIL) / 其他 error=2(内部错误)。
// runDoctor 本身只收集；各检查函数签名统一为 func() []doctorCheck。
func runDoctor(cmd *cobra.Command, _ []string) error
```

- [ ] **Step 1: 失败测试**——`TestDoctorExitCodes`（三态退出码）+ `TestDoctorEnvSeamsReported`：

```go
func TestDoctorExitCodes(t *testing.T) {
	withDoctorDirs(t) // T1 内 helper：withClearDirs 同款 env 钉法（SSHMGR_STORE/FILEKEY_PATH/CACHE_DIR/...全钉 temp）
	// 全空机器：role 缺失不是 FAIL（INFO，指向向导）；无 FAIL → runDoctor 返回 nil
	if err := doctorAllChecks(); hasFail(err) { /* 见断言形态 */ }
	// 造一个 FAIL（store.db 路径指向不存在文件 + role=server）→ errDoctorFindings
}

func TestDoctorEnvSeamsReported(t *testing.T) {
	withDoctorDirs(t)
	t.Setenv("SSHMGR_MASTERKEY_HEX", strings.Repeat("41", 32))
	got := captureDoctorOutput(t)
	if !strings.Contains(got, "SSHMGR_MASTERKEY_HEX is set") {
		t.Fatalf("dev-affordance env must be flagged:\n%s", got)
	}
}
```

（断言形态实现者按 cobra 输出捕获惯例补全——参照 clear_test.go 的 `driveClear` out-buffer 模式。）

- [ ] **Step 2: 确认红** — `go test ./internal/cli/ -count=1 -run TestDoctor` FAIL（函数不存在）。
- [ ] **Step 3: 实现** doctor.go：检查项表 `[]func() []doctorCheck`；渲染 `name:  STATUS  detail` + `       fix: <Fix>`（WARN/FAIL 才渲染 Fix）+ 尾行 `overall: N WARN, M FAIL`；`env` 检查枚举 `SSHMGR_STORE/FILEKEY_PATH/CACHE_DIR/CACHE_DEK/SERVE_CERT/SERVE_KEY/SERVE_MARKER/SERVE_LOG/CACHE_URL/CACHE_TOKEN/MASTERKEY_HEX`——任何已设置 → 一条 INFO 列出（值只显示 env 名，**不显示值**——值可能是 key/token）；唯 `SSHMGR_MASTERKEY_HEX` → WARN（"dev/test affordance — production should not rely on it"）。`role` 检查：`roles.Load()` 三态（nil→INFO "no role.json — fresh machine, run the wizard"；SetupComplete=false→WARN "wizard incomplete, re-run"；双位置并存→WARN dual-role residue）。
- [ ] **Step 4: 绿 + commit** `feat(cli): doctor skeleton — check framework, exit codes, env/role checks (Plan 27 T1)`。

### Task 2: vault 结构检查（store.db / master.key / 权限位）

**Files:**
- Modify: `internal/cli/doctor.go` + `doctor_test.go`

- [ ] **Step 1: 失败测试**：`TestDoctorVaultStructural`——① store.db+master.key 齐且 32B → 两项 PASS；② master.key 17 字节 → FAIL 且 Detail 含 "expected 32"；③ （`//go:build !windows` 分支或 runtime.GOOS 守卫跳过 Windows）master.key 权限 0644 → WARN 含 "group/world readable"。fixture：`seedClearVault` 同款（store.Open 建真库写 temp——注意**测试里**建库合法，doctor 探针读副本）。
- [ ] **Step 2: 红**。
- [ ] **Step 3: 实现**：`store` 检查（`paths.StorePath()` Stat→PASS 带大小 / role 属 vault 持有方（server/standalone）且缺→FAIL "run unlock/wizard" / client 且缺→INFO）；`masterkey` 检查（`paths.MasterKeyPath()` 读内容→`ValidMasterKeyLen`；Unix `info.Mode().Perm()&0o077 != 0`→WARN；文案参照 `vaultStatusString` 语义但保持 doctor 自己的 remediation）。
- [ ] **Step 4: 绿 + commit** `feat(cli): doctor vault structural checks (Plan 27 T2)`。

### Task 3: 副本解密探针（NUC10 FINDING A 检测器）

**Files:**
- Modify: `internal/cli/doctor.go` + `doctor_test.go`

**Interfaces:**
- Produces: `probeVaultDecrypt(storePath, keyPath string) (servers, creds int, err error)`——T4 不依赖它，但 `vault-open` 检查项的 Detail 用其计数。

- [ ] **Step 1: 失败测试**：`TestProbeVaultDecrypt`——① 正常库+正确 key → counts 与 seed 一致、err=nil、**副本目录已删**（Stat IsNotExist）；② 同库+错误 key（GenerateMasterKey 另生成）→ err 非 nil 含 "decrypt"；③ store.db 缺失 → err 含 "not found"。**副作用断言**：探针前后真库文件 mtime/size 不变。
- [ ] **Step 2: 红**。
- [ ] **Step 3: 实现**：`os.MkdirTemp("", "sshmgr-doctor-*")` + defer `os.RemoveAll`；`os.ReadFile` 真库与 key → 写副本文件（0600）；`store.Open(副本库, key)` → `ExportSnapshot()` → `len(snap.Servers)`/`len(snap.Credentials)`；错误包装 `fmt.Errorf("vault decrypt probe: %w", err)`（**不回显任何明文**——错误只含记录 ID 与 GCM 错误类别）。挂成 `vault-open` 检查项：探针 err→FAIL（Fix="key/ciphertext mismatch — restore from backup (.sme) or re-unlock + import; see docs/backup-restore.md"）；PASS Detail=`"copy-probe decrypted N servers / M credentials"`。
- [ ] **Step 4: 绿 + commit** `feat(cli): doctor copy-to-scratch decrypt probe (Plan 27 T3)`。

### Task 4: mcpserver.ReadServeCertFingerprint（只读）+ serve/cache 检查

**Files:**
- Modify: `internal/mcpserver/cert.go`（新增导出函数，复用 `loadServeCertFingerprint`）
- Test: `internal/mcpserver/cert_test.go`（追加）
- Modify: `internal/cli/doctor.go` + `doctor_test.go`

**Interfaces:**
- Produces:

```go
// ReadServeCertFingerprint 是 LoadOrCreateServeCert 的只读孪生：绝不生成。
// cert 缺失→错误（指明未初始化）；marker 在而 cert 不在→F10 错误（同 Load 语义）；
// corrupt/错配→错误。成功返回路径+SPKI pin。
func ReadServeCertFingerprint() (certPath, keyPath, fingerprint string, err error)
```

- cli 侧 seam：`var serveServiceState = func() string`（默认实现抄 `runServeStatus` 的 kardianos `s.Status()` 五态映射 + ErrNotInstalled；测试 stub）。

- [ ] **Step 1: 失败测试**（mcpserver 侧）：`TestReadServeCertFingerprintReadOnly`——① cert+key 在→返回 pin 且**文件 mtime 不变、marker 数不变**；② 全缺→错误含 "not initialized" 且**未生成任何文件**（目录仍空）；③ marker 在 cert 缺→错误含 "out-of-band"（F10 文案）。
- [ ] **Step 2: 红 → 实现**：照 `LoadOrCreateServeCert` 的三态骨架删掉生成分支；错误文案与既有保持一致措辞。
- [ ] **Step 3: cli 侧失败测试**：`TestDoctorServeAndCache`——serve-cert PASS（seed cert 后）/ F10 FAIL；`serve-svc`（stub `serveServiceState` 返回 "Running"→PASS、"Stopped"→WARN、"NOT INSTALLED"→INFO when role≠server / WARN when role=server）；client-cache（cache.bin+meta 在→PASS 带年龄 / role=client 且缺→FAIL / cache.bin 在而 DEK 缺→FAIL / cache.bin 在而 cache.auth.json 缺→WARN "no auto-refresh credential"）。
- [ ] **Step 4: 红 → 实现** doctor 挂载三项检查。
- [ ] **Step 5: 绿 + commit** `feat: doctor serve-cert (read-only fingerprint) + serve-svc + client-cache checks (Plan 27 T4)`。

### Task 5: 文档 + backlog 补项 + 全量验证

**Files:**
- Modify: `README.md`（命令表加 doctor 行，一行用法+退出码）
- Modify: `docs/concepts.md` 或 `docs/getting-started.md`（grep 定位"排障/运维"最自然的一处，加 3-5 行 doctor 说明：检查项清单、退出码、无副作用承诺）
- Modify: `docs/backlog.md`（追加第 6 项：**Windows DACL readback 检查**——`getDACLForTest` 是 test 名构件，产品化需要生产命名包装；doctor v1 用 Unix 权限位 + 32B 长度做代理）

- [ ] **Step 1: 三处文档**（文案与实现一致；退出码 0/1/2 必须写全）。
- [ ] **Step 2: 全量验证**：`go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -count=1` 全绿。
- [ ] **Step 3: 手工冒烟**（本机，dev 环境 env 已钉 temp 不会碰真 vault——直接跑 `go run ./cmd/ssh-manager doctor` 看输出形态，贴进报告）。
- [ ] **Step 4: commit** `docs: doctor usage + backlog DACL readback item (Plan 27 T5)`。

---

## 验收（整 plan）

1. `ssh-manager doctor` 在空机/健康 server/健康 client/错配 key 四种 fixture 下输出与退出码全部符合设计决策节。
2. 解密探针前后真库字节不变（测试断言）。
3. `ReadServeCertFingerprint` 在任何输入下零写盘（测试断言）。
4. 全量测试双 lane 绿（push 后 CI）；gofmt/vet 零输出。
5. 文档三处落地，backlog 第 6 项入册。
