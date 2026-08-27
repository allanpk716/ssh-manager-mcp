# Plan 40 第二批（TUI/向导多实例接入 + 自动归位 + cache config）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 Plan 40 第二批五项——TUI client 页实例 picker、连接表单实例字段与前置校验三连、首次 enroll 自动归位（真空 v4）、`cache config` 子命令、换码警告——依据收敛版 spec rev5。

**Architecture:** 归位在 `DoPull` 内实现（响应头到手后 retarget + 门禁/cap/DEK 后置）；TUI 以 `clientModel.instance` 会话态 + picker overlay 驱动 per-instance 读写；auth 写序三分（wizard 后移/面板即时+MkdirAll/CLI 按返回槽）。`DoPull` 签名改返回 `(PullResult, error)`，编译器驱动迁移全部调用点。

**Tech Stack:** Go（bubbletea v2 / charm.land huh v2——`huh.NewNote`/`Group.Description` 可用，已核实）、既有批1 多实例积木（`CachePathsFor`/`ListInstances`/For-variant 家族/`checkInstanceFlag`）。

**Spec:** `docs/superpowers/specs/2026-08-27-plan-40-batch2-tui-instance.md.rev5.md`（**权威版**；基 = `2026-08-26-plan-40-multi-instance-cache-design.md.rev3.md`，下称 rev3）。spec 谱系 rev1–rev5 已入库——评审轨迹与本 plan 的裁决依据全部可溯。

## Global Constraints（rev5 逐字钉死值，每个任务隐含遵守）

1. **真空 v4**：默认槽 `cache.bin`、`cache.auth.json`、`cache.meta.json`、`cache.config.json` **四者均不存在**才归位（spec §1.1 条件 4）；meta/config 任一在场 = 默认槽意图标记。
2. **两 override env 拦归位**：`SSHMGR_CACHE_DIR` 或 `SSHMGR_CACHE_DEK` 任一在场 → 不归位；`SSHMGR_CACHE_DEK_DIR` **不拦**（目录级连贯 seam）（§1.1 条件 5）。
3. **时序 = 批1 + 一处新增**：cap precheck 恒 HTTP 前（批1 不变）；唯一新增 = retarget 后对目标实例目录的校验（post-HTTP、pre-write）（§1.2）。
4. **DEK 与 MkdirAll 双后置**：归位路径任何拒绝分支 = 零写盘、零新增目录、零新增 DEK（§1.2-6/7；§9）。非真空路径保留批1 的 HTTP 前预建默认 DEK。
5. **pull 时文件有效性校验独立于 env**：`cache.config.json` 存在即校验（env 只覆盖生效值）；**load 路径保持批1 env 优先**（§1.2-5，rev5-R）。
6. **比对基准**：门禁身份比对与响应头强一致一律 **exact**；casefold 仅用于表单层**归一到既有目录的规范名**（canonical = 目录名 = 服务器下发名）（§1.2-4，§4）。
7. **auth 写序**：wizard 保存不写盘、pull 成功后 `WriteCacheCredFor(res.Instance)`（失败=WARNING 含恢复路径文案）；面板保存即写表单路由槽（新实例先 `MkdirAll(0700)`，写失败 `os.Remove` 清理本次新建空目录）；CLI 按返回槽写（§5）。
8. **归位 CLI 提示行**（§1.5 逐字）：`first enroll located to instance %s — mcp --cache needs --instance %s in .mcp.json (bare cache pull re-locates idempotently; only the agent's cache-mode launch is affected)`。
9. **换码 runbook v2**：清三件套（`cache.auth.json` + `cache.bin` + `quarantine/`），**保留 meta 与 config**——产品内 `gateDefaultInstance` 拒绝文案同步更新（§6/§12）。
10. **单槽模式**：任一 override env 在场 → TUI `[i]` 禁用、auto-picker 不触发、表单实例字段省略、页顶横幅（§3.5）。
11. **Plan 30 消息门**：新增 client-owned 消息类型必须登记 `clientModel.Update` 的 owned allowlist（§3.4）。
12. **测试纪律**：多实例测试走 `--instance`/内部参数，**不设** `SSHMGR_CACHE_DIR`/`SSHMGR_CACHE_DEK`（rev3 §9.5）；每任务 `gofmt -l` 零输出为证；`go test ./...` 全绿后才 commit。
13. `.xcheck/` 不 commit（已 gitignore）。

---

### Task 1: DoPull 签名变更 + 编译器驱动迁移

**Files:**
- Modify: `internal/clientops/clientops.go`（DoPull 签名 + MaybeLazyPullFor 调用点）
- Modify: `internal/cli/cache.go`（2 个 DoPull 调用点）
- Modify: `internal/cli/multiinstance_e2e_test.go` 及 `internal/clientops/*_test.go` 全部 `err := DoPull(` 调用点（~40 处，机械替换）
- Test: 既有测试零改动语义（只改接收）

**Interfaces:**
- Produces: `type PullResult struct { Instance string }`（""=默认槽）；`func DoPull(url, token, pin string, o PullOpts) (PullResult, error)`
- 后续任务依赖：T2 在 DoPull 内部 retarget 后 `o.Instance = deviceName` 并最终返回 `PullResult{Instance: o.Instance}`；T9 的 TUI 消费 `res.Instance`。

- [ ] **Step 1: 加 PullResult 并改签名**

```go
// PullResult reports where a pull's materials actually landed (Plan 40 batch 2
// §2): "" = the default slot; a name = instances/<name>/ (explicit --instance,
// or the §1.2 first-enroll auto-relocation). Callers that persist pull-side
// state (auth, config) MUST follow this slot — compiler-enforced completeness
// was the reason a return value beat an optional callback (spec §2).
type PullResult struct {
	Instance string
}

func DoPull(url, token, pin string, o PullOpts) (PullResult, error) {
```

DoPull 体内所有 `return err` / `return fmt.Errorf(...)` → `return PullResult{}, err` / `return PullResult{}, fmt.Errorf(...)`；末尾 `return nil` → `return PullResult{Instance: o.Instance}, nil`（本任务不改行为——o.Instance 原样返回；T2 才有 retarget 改写）。

- [ ] **Step 2: 编译器驱动迁移全部调用点**

`go build ./...` 逐个修：`internal/cli/cache.go` 两处（`if err := clientops.DoPull(...)` → `if _, err := clientops.DoPull(...)`）；`internal/clientops/clientops.go` lazy（`err = DoPull(...)` → `_, err = DoPull(...)`——MaybeLazyPullFor 不消费 result，其 Instance 本就是显式传入）；全部测试 `err := DoPull(` → `_, err := DoPull(`（注意 `err =`（非声明）形式与多返回值上下文）。**禁止**顺手改任何行为。

- [ ] **Step 3: 全量回归**

Run: `go test ./...`
Expected: 全绿（机械迁移零语义变化）。

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(clientops): DoPull 返回 PullResult{Instance}——编译器驱动迁移全部调用点（Plan 40 批2 T1，签名先行零行为变化）"
```

---

### Task 2: 真空 v4 + 首次 enroll 自动归位

**Files:**
- Create: `internal/clientops/relocate.go`
- Modify: `internal/clientops/clientops.go`（DoPull 核心段：真空候选判定、DEK 后置、retarget、门禁/cap 复用、CLI 提示行）
- Test: `internal/clientops/relocate_test.go`（新建；复用 `instance_routing_test.go`/`cli/multiinstance_e2e_test.go` 的内联 pinned TLS serve fixture 形态——同包已有 helper 则直接用）

**Interfaces:**
- Consumes: T1 的 `(PullResult, error)`；批1 `gateNamedInstance`/`gateDefaultInstance`/`instname.Valid`。
- Produces:
  - `func defaultSlotVacuum(dir string) bool`（包内）
  - `func singleSlotOverrideEnvSet() bool`（包内）
  - `func DefaultSlotVacuum() (bool, error)`（导出——TUI auto-picker 判定用，内部解析默认目录）
  - `func SingleSlotOverrideEnvSet() bool`（导出——TUI 单槽模式横幅/禁用用）

- [ ] **Step 1: 写失败测试（归位触发 + 七态不触发 + 幂等）**

fixture 形态照抄 `cli/multiinstance_e2e_test.go:30-82`（ed25519 自签 + `mcpserver.NewServeRunner` + `httptest.NewUnstartedServer` + `SPKIFingerprint`；client 侧 `t.Setenv("APPDATA"/"XDG_CONFIG_HOME", tmp)` + `SSHMGR_CACHE_DIR`/`SSHMGR_CACHE_DEK` 显式清空 + `SSHMGR_CACHE_DEK_DIR` 指临时）。测试放 `internal/clientops`（serve fixture 若该包已有等价 helper——见 `instance_routing_test.go`——优先复用）：

```go
// relocate_test.go — Plan 40 批2 §1：真空 v4 自动归位。
// 真空 = 默认槽 bin/auth/meta/config 四文件均缺（rev5 §1.1 条件 4——meta/config
// 是默认槽意图标记，"曾有材料/曾配置"的痕迹）。
func TestRelocate_VacuumV4_BarePullLandsInInstance(t *testing.T) {
	// 真空机 + 裸 pull → 材料+meta 落 instances/<头name>/，默认槽保持全空，
	// PullResult.Instance == 头name，CLI 提示行打到 StatusOut。
	//（serve fixture：一个 profile + 一码 "laptop-agentA"，同 e2e 形态）
	var buf bytes.Buffer
	res, err := DoPull(srv.URL, codeA, pin, PullOpts{StatusOut: &buf})
	if err != nil { t.Fatal(err) }
	if res.Instance != "laptop-agentA" { t.Fatalf("res.Instance = %q", res.Instance) }
	instDir := filepath.Join(userDir, "ssh-manager", "instances", "laptop-agentA")
	for _, f := range []string{"cache.bin", "cache.meta.json"} {
		if _, serr := os.Stat(filepath.Join(instDir, f)); serr != nil { t.Fatalf("%s: %v", f, serr) }
	}
	if !strings.Contains(buf.String(), "first enroll located to instance laptop-agentA") { t.Fatalf("hint missing: %q", buf.String()) }
	defDir := filepath.Join(userDir, "ssh-manager")
	for _, f := range []string{"cache.bin", "cache.auth.json", "cache.meta.json", "cache.config.json"} {
		if _, serr := os.Stat(filepath.Join(defDir, f)); serr == nil { t.Fatalf("default slot must stay vacuum, found %s", f) }
	}
}

func TestRelocate_NonVacuumSevenStates(t *testing.T) {
	// 七态各自断言"材料落默认目录（或 override 目录）、instances/ 不出现"：
	// ① auth 在 bin 无（预置 cache.auth.json）→ 默认目录 + 门禁补记（meta.device_name 记录）
	// ② meta 在（预置 cache.meta.json，无 config——存量形态）→ 默认目录
	// ③ config 在（预置合法 cache.config.json）→ 默认目录
	// ④ SSHMGR_CACHE_DIR 在场 → override 目录
	// ⑤ SSHMGR_CACHE_DEK 在场 → 默认目录（材料用 env DEK；断言 instances/ 无）
	// ⑥ 头缺失（老 serve：用裸 httptest TLS 不带 handler 头——或去掉 serve 后直接 200 空头）→ 默认目录
	// ⑦ plaintext（--allow-plaintext + 明文 serve）→ 默认目录
	// ⑧（正向对照）SSHMGR_CACHE_DEK_DIR 在场 → 归位照常，实例 DEK 落 env dir
}
```

- [ ] **Step 2: 跑测试证 RED**（`go test ./internal/clientops/ -run TestRelocate -v`——`res.Instance` 断言与目录断言失败）

- [ ] **Step 3: 实现 relocate.go**

```go
// Package clientops: Plan 40 batch 2 §1 — first-enroll auto-relocation.
package clientops

import (
	"os"
	"path/filepath"
)

// vacuumMarkerFiles are the default slot's history markers (spec §1.1 cond 4,
// vacuum v4): meta is rewritten on every successful pull (the natural trace of
// "this slot once held material"), config records deliberate cap policy. ANY of
// them present = default-slot intent → a bare pull must NOT relocate.
var vacuumMarkerFiles = []string{"cache.bin", "cache.auth.json", "cache.meta.json", "cache.config.json"}

// defaultSlotVacuum reports whether dir carries none of the marker files.
func defaultSlotVacuum(dir string) bool {
	for _, f := range vacuumMarkerFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return false
		}
	}
	return true
}

// singleSlotOverrideEnvSet reports whether a full-override env is present
// (spec §1.1 cond 5): either one keeps the pull in single-slot semantics, so
// auto-relocation is off. SSHMGR_CACHE_DEK_DIR is a coherent directory-level
// seam (the whole DEK tree moves) and does NOT count.
func singleSlotOverrideEnvSet() bool {
	return os.Getenv("SSHMGR_CACHE_DIR") != "" || os.Getenv("SSHMGR_CACHE_DEK") != ""
}

// DefaultSlotVacuum is the exported form for the TUI (auto-picker trigger uses
// the SAME four-file judgment as relocation, spec §3.2).
func DefaultSlotVacuum() (bool, error) {
	dir, _, _, _, err := CachePaths()
	if err != nil {
		return false, err
	}
	return defaultSlotVacuum(dir), nil
}

// SingleSlotOverrideEnvSet is the exported form for the TUI single-slot mode
// banner/disables (spec §3.5).
func SingleSlotOverrideEnvSet() bool { return singleSlotOverrideEnvSet() }
```

- [ ] **Step 4: DoPull 核心改造**

`clientops.go` DoPull 内三处（锚点为 T1 后的行结构）：

(a) DEK 加载处（原 `dek, err := loadOrCreateDEK(o.Instance)`，HTTP 前）改为真空候选后置：

```go
	// Plan 40 批2 §1.2-6: vacuum-candidate paths DEFER DEK creation to after
	// every gate — a refused relocation must leave zero writes, including no
	// freshly-created default DEK. Non-vacuum paths keep the batch-1 timing.
	vacuumCandidate := o.Instance == "" && !singleSlotOverrideEnvSet() && defaultSlotVacuum(dir)
	var dek []byte
	if !vacuumCandidate {
		dek, err = loadOrCreateDEK(o.Instance)
		if err != nil {
			return PullResult{}, err
		}
	}
```

(b) 头提取后的门禁块（原 `if o.Instance != "" { gateNamedInstance } else if gateDefaultInstance`）中间插入归位分支：

```go
	if o.Instance != "" {
		if err := gateNamedInstance(bin, metaPath, deviceName, o.Instance); err != nil {
			return PullResult{}, err
		}
	} else if pin != "" && deviceName != "" && instname.Valid(deviceName) == nil && vacuumCandidate {
		// Plan 40 批2 §1.2-3: first-enroll auto-relocation. Retarget by re-running
		// the WHOLE CachePathsFor resolution under the header name — the audit
		// sidecar & quarantine subtree follow the same resolution, no path
		// stragglers (spec §1.2-3, rev3 §2.2 全消费面纪律).
		ndir, nbin, nmeta, _, rerr := CachePathsFor(deviceName)
		if rerr != nil {
			return PullResult{}, rerr
		}
		dir, bin, metaPath = ndir, nbin, nmeta
		// §1.2-4: target-slot gate, EXACT identity (header==name trivially holds;
		// physical collision & half-write checks still run — relocation is not a
		// bypass. meta identity match = idempotent re-relocation, allowed).
		if gerr := gateNamedInstance(nbin, nmeta, deviceName, deviceName); gerr != nil {
			return PullResult{}, gerr
		}
		// §1.2-5: target-slot cap check — file validity INDEPENDENT of env (T3's
		// validateCapFileIndependent), then re-resolve the effective value for
		// the target slot.
		if verr := validateCapFileIndependent(ndir); verr != nil {
			return PullResult{}, verr
		}
		if maxOffline, err = resolveMaxOffline(ndir); err != nil {
			return PullResult{}, err
		}
		o.Instance = deviceName
		if o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "first enroll located to instance %s — mcp --cache needs --instance %s in .mcp.json (bare cache pull re-locates idempotently; only the agent's cache-mode launch is affected)\n", deviceName, deviceName)
		}
	} else if err := gateDefaultInstance(bin, metaPath, deviceName, o); err != nil {
		return PullResult{}, err
	}
	if dek == nil {
		// Deferred load (vacuum-candidate path): post-gates per §1.2-6 — a
		// refused pull never created any DEK file.
		dek, err = loadOrCreateDEK(o.Instance)
		if err != nil {
			return PullResult{}, err
		}
	}
```

（注：`validateCapFileIndependent` 由 T3 提供——**本任务与 T3 同批实现该函数**，见 T3 Step 3；先行占位会编译失败，故 T2/T3 顺序：先做 T3 的 helper 再回本步，或两任务同分支连续提交。执行者按"T3 → T2"顺序跑亦可，接口不变。）

(c) 换码 runbook 文案（gateDefaultInstance 拒绝文案）更新——见 T10（避免双任务改同一行；本任务不动）。

- [ ] **Step 5: 跑测试证 GREEN + 全量回归**

Run: `go test ./internal/clientops/ ./internal/cli/ -count=1`
Expected: TestRelocate* 全绿 + 既有零回归（含 TestDualInstance_E2E）。

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(clientops): 首次 enroll 自动归位——真空 v4 四文件判定/retarget 整套重解析/目标槽门禁 exact/DEK 后置/CLI 提示行（Plan 40 批2 T2）"
```

---

### Task 3: pull 时 config 文件校验独立于 env

**Files:**
- Modify: `internal/clientops/config.go`（`validateCapFileIndependent` + `EffectiveMaxOffline`）
- Modify: `internal/clientops/clientops.go`（DoPull 顶部 precheck 前插一行——见下）
- Test: `internal/clientops/config_test.go`（追加）

**Interfaces:**
- Consumes: 既有 `parseMaxOffline`。
- Produces:
  - `func validateCapFileIndependent(dir string) error`（包内；T2 消费）
  - `func EffectiveMaxOffline(dir string) (time.Duration, string, error)`（导出；T4 显示态消费——返回 `(cap, "env"|"file"|"off", err)`）

- [ ] **Step 1: 写失败测试**

```go
// config_test.go 追加 — rev5 §1.2-5：pull 写入面的文件校验独立于 env。
func TestValidateCapFileIndependent(t *testing.T) {
	dir := t.TempDir()
	if err := validateCapFileIndependent(dir); err != nil { t.Fatalf("no file must pass: %v", err) }
	os.WriteFile(filepath.Join(dir, "cache.config.json"), []byte(`{"max_offline":"24h"}`), 0o600)
	if err := validateCapFileIndependent(t.TempDir()); err != nil { /* 占位防呆——见下一断言 */ }
	if err := validateCapFileIndependent(dir); err != nil { t.Fatalf("valid file must pass: %v", err) }
	os.WriteFile(filepath.Join(dir, "cache.config.json"), []byte(`{"max_offline":"bogus"}`), 0o600)
	if err := validateCapFileIndependent(dir); err == nil { t.Fatal("invalid file must fail even with a valid env") }
}

// pull 集成：env 有效 + 目标 config 非法 → 写盘前拒、零写盘（§11.6-⑥）；
// 真空候选 + env 非法 → HTTP 前拒（§11.6-⑤，现状回归钉子）。
func TestPull_CapValidationIndependentOfEnv(t *testing.T) {
	// fixture 同 TestRelocate_*；预置 instances/agentA/ 内非法 cache.config.json，
	// t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")，显式 --instance agentA pull →
	// 断言 err 非 nil 且 bin 未落盘（HTTP 前拒——顶部 precheck 也独立校验）。
}
```

- [ ] **Step 2: 跑测试证 RED**

- [ ] **Step 3: 实现**

```go
// validateCapFileIndependent checks the slot's cache.config.json VALIDITY
// regardless of SSHMGR_CACHE_MAX_OFFLINE (rev5 §1.2-5): on the PULL side an
// invalid file must refuse the WRITE even under a valid env — otherwise this
// pull writes what an env-less loader will later refuse ("pulls but won't
// load"). The LOAD side keeps env-wins (batch-1 semantics, §13.14).
func validateCapFileIndependent(dir string) error {
	blob, err := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cache.config.json unreadable: %w", err)
	}
	var c struct {
		MaxOffline string `json:"max_offline"`
	}
	if err := json.Unmarshal(blob, &c); err != nil {
		return fmt.Errorf("corrupt cache.config.json: %w", err)
	}
	_, perr := parseMaxOffline(strings.TrimSpace(c.MaxOffline), `max_offline in cache.config.json`)
	return perr
}
```

DoPull 顶部（`resolveMaxOffline(dir)` 之前）插：

```go
	// rev5 §1.2-5: pull-side file validation is INDEPENDENT of env — applies to
	// the initially-resolved slot here, and to the relocation target after
	// retarget (T2).
	if verr := validateCapFileIndependent(dir); verr != nil {
		return PullResult{}, verr
	}
```

`EffectiveMaxOffline`（T4 用，此处一并落）：

```go
// EffectiveMaxOffline resolves a slot's effective cap with its SOURCE label
// ("env" / "file" / "off") for `cache config` display. Mirrors resolveMaxOffline
// exactly (env wins including its error — display is not a write gate).
func EffectiveMaxOffline(dir string) (time.Duration, string, error) {
	if strings.TrimSpace(os.Getenv("SSHMGR_CACHE_MAX_OFFLINE")) != "" {
		d, err := cacheMaxOffline()
		return d, "env", err
	}
	blob, err := os.ReadFile(filepath.Join(dir, "cache.config.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, "off", nil
	}
	if err != nil {
		return 0, "", fmt.Errorf("cache.config.json unreadable: %w", err)
	}
	var c struct {
		MaxOffline string `json:"max_offline"`
	}
	if err := json.Unmarshal(blob, &c); err != nil {
		return 0, "", fmt.Errorf("corrupt cache.config.json: %w", err)
	}
	d, perr := parseMaxOffline(strings.TrimSpace(c.MaxOffline), `max_offline in cache.config.json`)
	return d, "file", perr
}
```

- [ ] **Step 4: GREEN + 全量回归**（`go test ./...`）
- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(clientops): pull 侧 config 文件校验独立于 env（env 只覆盖生效值）+ EffectiveMaxOffline 显示源（Plan 40 批2 T3, rev5 §1.2-5）"
```

---

### Task 4: `cache config` 子命令

**Files:**
- Modify: `internal/cli/cache.go`（newCacheConfigCmd + 注册）
- Test: `internal/cli/cache_config_cmd_test.go`

**Interfaces:**
- Consumes: T3 `EffectiveMaxOffline`；既有 `ValidateMaxOffline`/`WriteCacheConfig`/`CachePathsFor`/`checkInstanceFlag`。
- Produces: `ssh-manager cache config [--instance <name>] [--max-offline <dur>]`。

- [ ] **Step 1: 写失败测试**

```go
// cache_config_cmd_test.go — §8/§11.14：显示三源 + 写入 + 仅已存在实例 + 互斥。
//（环境形态同 cache_status_instances_test.go：t.Setenv APPDATA/XDG + seedInstanceSlot
// 或手工建 instances/<name>/ 目录）
func TestCacheConfig_DisplaySources(t *testing.T) {
	// ① off：目录在、无 config → "source: off"
	// ② file：写 {"max_offline":"24h"} → "24h (source: file)"
	// ③ env：t.Setenv SSHMGR_CACHE_MAX_OFFLINE=48h → "48h (source: env)"
}
func TestCacheConfig_WriteAndReadback(t *testing.T) { /* --max-offline 24h → 文件内容 + 再显 file 源；env 在场 → WARNING 断言 */ }
func TestCacheConfig_MissingInstanceDir(t *testing.T) {
	// --instance ghost --max-offline 1h → 报错含 "enroll first (cache pull --instance ghost)"
}
func TestCacheConfig_InstanceEnvMutex(t *testing.T) { /* SSHMGR_CACHE_DIR + --instance → checkInstanceFlag 报错 */ }
```

- [ ] **Step 2: RED**
- [ ] **Step 3: 实现（cli/cache.go）**

```go
func cacheConfigCmd() *cobra.Command {
	var instance, maxOffline string
	c := &cobra.Command{
		Use:   "config",
		Short: "Show or set the per-instance offline cap (cache.config.json)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkInstanceFlag(instance); err != nil {
				return err
			}
			dir, _, _, _, err := clientops.CachePathsFor(instance)
			if err != nil {
				return err
			}
			if _, serr := os.Stat(dir); serr != nil {
				name := instance
				if name == "" {
					name = "default"
				}
				return fmt.Errorf("instance %q not found (directory %s does not exist) — enroll first (cache pull --instance %q)", name, dir, name)
			}
			out := cmd.OutOrStdout()
			label := instance
			if label == "" {
				label = "default"
			}
			if maxOffline == "" {
				cap, src, rerr := clientops.EffectiveMaxOffline(dir)
				if rerr != nil {
					return rerr
				}
				if src == "off" {
					fmt.Fprintf(out, "instance: %s (%s)\ncap:      off (no offline limit)\n", label, dir)
				} else {
					fmt.Fprintf(out, "instance: %s (%s)\ncap:      %s (source: %s)\n", label, dir, cap, src)
				}
				return nil
			}
			if _, verr := clientops.ValidateMaxOffline(maxOffline); verr != nil {
				return verr
			}
			if werr := clientops.WriteCacheConfig(dir, maxOffline); werr != nil {
				return werr
			}
			fmt.Fprintf(out, "wrote %s (instance %s)\n", filepath.Join(dir, "cache.config.json"), label)
			if strings.TrimSpace(os.Getenv("SSHMGR_CACHE_MAX_OFFLINE")) != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: SSHMGR_CACHE_MAX_OFFLINE is set — the persisted config takes effect only after the env is cleared")
			}
			// 默认槽警示（rev5 §8）：删默认槽 config 或 meta 都会削弱意图标记。
			if instance == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "note: keep cache.meta.json/cache.config.json in this directory — they mark the DEFAULT slot; deleting them re-routes the next first-enroll into instances/")
			}
			return nil
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "target this named instance (directory must exist; mutually exclusive with SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK)")
	c.Flags().StringVar(&maxOffline, "max-offline", "", "persist this Go duration (e.g. 24h) as the instance's offline cap; omit to display the current effective cap")
	return c
}
```

`newCacheCmd()` 注册：`cmd.AddCommand(cachePullCmd(), cacheStatusCmd(), cacheConfigCmd())`。imports 需 `path/filepath`（若无）。

- [ ] **Step 4: GREEN + 全量**（`go test ./internal/cli/ -count=1`）
- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(cli): cache config 子命令——三源显示/写入(仅已存在实例)/env WARNING/默认槽意图标记警示（Plan 40 批2 T4）"
```

---

### Task 5: clientModel per-instance 核心

**Files:**
- Modify: `internal/tui/clientpage.go`（instance 字段、refreshDataCmdFor、syncCmdMode 带 instance、dataReadyMsg/syncDoneMsg/pullSucceededMsg/connSavedMsg 扩字段、过期槽回复丢弃、[s] 接线）
- Modify: `internal/clientops/clientops.go`（`CacheScopeVerifiedFor` 导出包装）
- Test: `internal/tui/clientpage_instance_test.go`

**Interfaces:**
- Consumes: 批1 `ReadCacheCredFor`/`LoadCacheSnapshotFor`/`CachePathsFor`。
- Produces: `clientModel.instance string`；`refreshDataCmdFor(instance string) tea.Cmd`；`syncCmdMode(cred, instance string, wizard bool) tea.Cmd`；消息字段 `dataReadyMsg.instance`、`pullSucceededMsg.instance`、`connSavedMsg.instance`；`clientops.CacheScopeVerifiedFor(instance string) bool`。T6/T8/T9 消费。

- [ ] **Step 1: 写失败测试**

```go
// clientpage_instance_test.go — §3.3 per-instance 动作核心。
// seed 形态仿 cli/cache_status_instances_test.go seedInstanceSlot（APPDATA/XDG 重定向
// + instances/<name>/{cache.bin(真加密或占位), cache.meta.json, cache.auth.json}）。
func TestRefreshDataCmdFor_InstanceSlot(t *testing.T) {
	// seed agentA 槽（meta device_name=agentA + auth）→ refreshDataCmdFor("agentA")
	// 返回 dataReadyMsg{instance:"agentA", cred 非 nil}；空目录名 → 默认槽 msg.instance == ""
}
func TestSyncCmdMode_PassesInstance(t *testing.T) {
	// 无法直接断言 PullOpts——改为集成断言：seed 槽 + 假 serve 过重；
	// 用轻量法：syncCmdMode(cred, "agentA", false) 对无 serve 环境返回 syncDoneMsg{err 非 nil}
	// 即仅钉"不 panic/路径路由"；真正 Instance 断言由 T11 e2e 承担。
}
func TestDataReady_StaleSlotDropped(t *testing.T) {
	// m := newClientModel(); m.instance = "agentA"; 收 dataReadyMsg{instance: ""} →
	// m.snap 保持 nil（过期槽回复被丢弃）
}
```

- [ ] **Step 2: RED**
- [ ] **Step 3: 实现（clientpage.go）**

```go
type clientModel struct {
	cred *clientops.CacheCred
	snap *store.Snapshot
	scoped bool
	cacheAge time.Duration
	instance string // Plan 40 批2 §3.1: selected slot ("" = default), session-only
	panelList
	width, height int
	status, err string
	busy bool
	overlay overlay
	wizard bool
	draft *connDraft
	finish bool
}
```

```go
type dataReadyMsg struct {
	instance string // which slot this reply belongs to (stale replies are dropped)
	cred *clientops.CacheCred
	snap *store.Snapshot
	scoped bool
	age time.Duration
}

// refreshDataCmdFor re-reads ONE slot's cred + snapshot + cache.bin mtime.
func refreshDataCmdFor(instance string) tea.Cmd {
	return func() tea.Msg {
		cred, err := clientops.ReadCacheCredFor(instance)
		if err != nil || cred == nil {
			if err == nil {
				err = fmt.Errorf("读取连接配置失败: cache.auth.json 不存在")
			} else {
				err = fmt.Errorf("读取连接配置失败: %w", err)
			}
			return errMsg{err}
		}
		snap, err := clientops.LoadCacheSnapshotFor(instance)
		if err != nil {
			return errMsg{err}
		}
		_, bin, _, _, err := clientops.CachePathsFor(instance)
		if err != nil {
			return errMsg{err}
		}
		var age time.Duration
		if fi, serr := os.Stat(bin); serr == nil {
			age = time.Since(fi.ModTime())
		}
		return dataReadyMsg{instance: instance, cred: cred, snap: snap, scoped: clientops.CacheScopeVerifiedFor(instance), age: age}
	}
}
var refreshDataCmd = refreshDataCmdFor("") // zero-change wrapper for existing callers
```

（原 `refreshDataCmd` 是 `func() tea.Msg` 函数值——改为上述形态后既有调用点 `return refreshDataCmd` 语义不变。）

`syncCmdMode` 加 instance 参数并透传 `PullOpts{Timeout: clientops.LazyPullTimeout, Instance: instance}`；`pullSucceededMsg`/`connSavedMsg` 加 `instance string` 字段。Update 处理：

```go
	case dataReadyMsg:
		if kp.instance != m.instance {
			return m, nil // stale slot reply (user switched mid-flight)
		}
		m.cred, m.snap, m.scoped, m.cacheAge = kp.cred, kp.snap, kp.scoped, kp.age
		...
	case tea.KeyPressMsg:
		...
		case k.Text == "s" && !m.busy:
			m.busy, m.err, m.status = true, nil, ""
			return m, syncCmdMode(m.cred, m.instance, m.wizard)
```

`CacheScopeVerifiedFor`（clientops.go，`CacheScopeVerified` 旁）：

```go
// CacheScopeVerifiedFor is the per-instance form of CacheScopeVerified.
func CacheScopeVerifiedFor(instance string) bool {
	_, _, metaPath, _, err := CachePathsFor(instance)
	if err != nil {
		return false
	}
	m, err := readCacheMeta(metaPath)
	if err != nil {
		return false
	}
	return m.Scoped
}
```

- [ ] **Step 4: GREEN + 全量**（`go test ./internal/tui/ -count=1`；既有 clientpage 测试零回归）
- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(tui): clientModel per-instance 核心——instance 会话态/refreshFor/sync 透传/过期槽回复丢弃 + CacheScopeVerifiedFor（Plan 40 批2 T5）"
```

---

### Task 6: 实例 picker overlay + `[i]` + auto-picker

**Files:**
- Create: `internal/tui/instancepicker.go`
- Modify: `internal/tui/clientpage.go`（[i] 键、消息门登记、auto-picker、footer/header）
- Test: `internal/tui/instancepicker_test.go`

**Interfaces:**
- Consumes: T5 `clientModel.instance`/`refreshDataCmdFor`；T2 `DefaultSlotVacuum`/`SingleSlotOverrideEnvSet`；批1 `ListInstances`。
- Produces: `instancePickedMsg{instance string}`、`instancePickerClosedMsg{}`；`newInstancePicker() *instancePicker`。footer 逐字：`[s]同步 [i]实例 [c]编辑连接 [t]TTL  q 退出`。

- [ ] **Step 1: 写失败测试**

```go
// instancepicker_test.go — §3.1/§3.2/§11.9。
func TestInstancePicker_RowsAndPick(t *testing.T) {
	// seed instances/agentA + instances/agentB → newInstancePicker().rows:
	// 首行 label "（默认实例）" name ""；实例行 label == 目录名；行不解密（无 DEK 也构造成功）
	// 模拟 Enter → Update 产出 instancePickedMsg{instance:"agentA"}
}
func TestInstancePicker_EscCloses(t *testing.T) { // Esc → instancePickerClosedMsg
}
func TestClientModel_InstanceKeyOpensPicker(t *testing.T) {
	// m := newClientModelForGate(t) 形态（clientpage_routing_test.go:19 helper）+ 实例 seed
	// 送 tea.KeyPressMsg{k:"i"} → m.overlay 是 *instancePicker；busy=true 时按键为 no-op
}
func TestClientModel_PickedSwitchesSlot(t *testing.T) {
	// instancePickedMsg{instance:"agentA"} → m.instance == "agentA" 且返回 refreshDataCmdFor("agentA")
}
func TestClientModel_AutoPickerOnTrueVacuum(t *testing.T) {
	// 默认槽四文件全缺 + instances 非空 → 首个 errMsg/dataReadyMsg 后 m.overlay 为 picker（一次）
	// bin 在 auth 缺（半残）→ 不弹；meta 在 → 不弹；SSHMGR_CACHE_DIR 在场 → 不弹
}
func TestClientGate_RegistersPickerMsgs(t *testing.T) {
	// Plan 30 owned allowlist：overlay 开着时 instancePickedMsg/instancePickerClosedMsg
	// 必须落到 clientModel 的 switch（不被 overlay 吞）——仿 clientpage_routing_test.go TestClientGateOwnedFallsThrough
}
```

- [ ] **Step 2: RED**
- [ ] **Step 3: 实现 instancepicker.go**

```go
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"ssh-manager-mcp/internal/clientops"
)

// instancePicker lists the default slot + every named instance (spec §3.1).
// Rows are LIGHT — name + cache.bin age + profile from meta, NEVER decrypted
// (a DEK fault must not break listing). The default row's label is the literal
// 「（默认实例）」 so a legal instance literally named "default" stays
// distinguishable (spec §0.14/§3.1).
type instancePicker struct {
	rows   []pickerRow
	cursor int
}

type pickerRow struct {
	label   string // display label
	instance string // routing value: "" = default
	age     string
	profile string
}

// instancePickedMsg carries the chosen routing value to clientModel.
type instancePickedMsg struct{ instance string }

// instancePickerClosedMsg is the Esc path (no change).
type instancePickerClosedMsg struct{}

func newInstancePicker() *instancePicker {
	p := &instancePicker{}
	p.rows = append(p.rows, pickerRow{label: "（默认实例）", instance: ""})
	// default-slot age/profile
	p.rows[0].age, p.rows[0].profile = pickerRowMeta("")
	if names, err := clientops.ListInstances(); err == nil {
		for _, n := range names {
			r := pickerRow{label: n, instance: n}
			r.age, r.profile = pickerRowMeta(n)
			p.rows = append(p.rows, r)
		}
	}
	return p
}

// pickerRowMeta reads a slot's bin mtime + meta profile WITHOUT decrypting.
func pickerRowMeta(instance string) (age, profile string) {
	_, bin, metaPath, _, err := clientops.CachePathsFor(instance)
	if err != nil {
		return "-", ""
	}
	if fi, serr := os.Stat(bin); serr == nil {
		age = time.Since(fi.ModTime()).Round(time.Minute).String() + " 前"
	} else {
		age = "无缓存"
	}
	if b, rerr := os.ReadFile(metaPath); rerr == nil {
		var m struct {
			Scoped bool   `json:"scoped"`
			Device string `json:"device_name"`
		}
		if json.Unmarshal(b, &m) == nil && m.Scoped && m.Device != "" {
			profile = m.Device
		}
	}
	return age, profile
}

func (p *instancePicker) Title() string      { return "选择实例" }
func (p *instancePicker) Init() tea.Cmd      { return nil }

func (p *instancePicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	kp, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil
	}
	k := kp.Key()
	switch {
	case k.Code == tea.KeyUp, k.Text == "k":
		if p.cursor > 0 { p.cursor-- }
	case k.Code == tea.KeyDown, k.Text == "j":
		if p.cursor < len(p.rows)-1 { p.cursor++ }
	case k.Code == tea.KeyEnter, k.Code == tea.KeySpace:
		row := p.rows[p.cursor]
		return p, func() tea.Msg { return instancePickedMsg{instance: row.instance} }
	case k.Code == tea.KeyEsc, k.Text == "q":
		return p, func() tea.Msg { return instancePickerClosedMsg{} }
	}
	return p, nil
}

func (p *instancePicker) View() tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render(" 选择实例") + "\n（↑/↓ 选择，Enter 确认，Esc 取消）\n\n")
	for i, r := range p.rows {
		cursor := "  "
		if i == p.cursor {
			cursor = "> "
		}
		line := fmt.Sprintf("%s%-14s %-14s %s", cursor, r.label, r.age, r.profile)
		b.WriteString(clip(0, line) + "\n")
	}
	return altScreen(tea.NewView(b.String()))
}
```

（`clip(0, …)` 传 0 时原样返回——核对 clip 实现签名；若 clip(0) 行为不符就直拼。）

clientpage.go：

```go
// Plan 30 gate 的 owned allowlist 登记两种新消息：
	case dataReadyMsg, syncDoneMsg, pullSucceededMsg, connSavedMsg,
		clientStatusMsg, errMsg, formDoneMsg, instancePickedMsg, instancePickerClosedMsg:

// [i] 键（[s] 分支旁）：
		case k.Text == "i" && !m.busy:
			if clientops.SingleSlotOverrideEnvSet() {
				m.status = "单槽模式（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK 覆盖中）——多实例 UI 已禁用"
				return m, nil
			}
			m.overlay = newInstancePicker()
			return m, m.overlay.Init()

// instancePickedMsg / instancePickerClosedMsg 处理：
	case instancePickedMsg:
		m.instance, m.overlay, m.err = kp.instance, nil, nil
		return m, refreshDataCmdFor(kp.instance)
	case instancePickerClosedMsg:
		m.overlay = nil
		return m, nil

// auto-picker（§3.2，真真空四文件判定；一次为限）：
// clientModel 增字段 pickerChecked bool；在 errMsg 与 dataReadyMsg 两个 case 的
// 开头（stale-guard 之后）调用：
	if !m.pickerChecked {
		m.pickerChecked = true
		if vac, verr := clientops.DefaultSlotVacuum(); verr == nil && vac &&
			!clientops.SingleSlotOverrideEnvSet() && m.overlay == nil {
			if names, lerr := clientops.ListInstances(); lerr == nil && len(names) > 0 {
				m.overlay = newInstancePicker()
				return m, m.overlay.Init()
			}
		}
	}

// footer（View 末行）：
	b.WriteString(clip(m.width, footerStyle.Render("[s]同步 [i]实例 [c]编辑连接 [t]TTL  q 退出")))
// header（clientHeader 已有参数表——在 m.instance != "" 时 View 里前缀一行）：
	if m.instance != "" {
		b.WriteString(warnStyle.Render("· 实例 " + m.instance) + "\n")
	}
```

- [ ] **Step 4: GREEN + 全量**
- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(tui): [i] 实例 picker overlay——轻量行不解密/（默认实例）行区分/会话内切换/auto-picker 真空触发/消息门登记（Plan 40 批2 T6）"
```

---

### Task 7: env 单槽模式横幅与禁用

**Files:**
- Modify: `internal/tui/clientpage.go`（View 横幅、footer 变体、auto-picker 抑制在 T6 已含）
- Test: `internal/tui/instancepicker_test.go`（追加）

**Interfaces:** Consumes T2 `SingleSlotOverrideEnvSet`、T6 picker。

- [ ] **Step 1: 写失败测试**

```go
func TestClientView_SingleSlotBanner(t *testing.T) {
	// t.Setenv SSHMGR_CACHE_DIR → View() 含 "单槽模式（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK 覆盖中）"
	// footer 不含 "[i]实例"
}
func TestClientSingleSlot_NoAutoPicker(t *testing.T) {
	// env 在场 + 真空 + instances 非空 → 不弹（T6 的 auto-picker 条件已含，钉测试）
}
func TestClientSingleSlot_DEKDirExempt(t *testing.T) {
	// 只设 SSHMGR_CACHE_DEK_DIR → View 无横幅、footer 含 [i]
}
```

- [ ] **Step 2: RED**
- [ ] **Step 3: 实现**：View 顶部（title 行后）：

```go
	if clientops.SingleSlotOverrideEnvSet() {
		b.WriteString(warnStyle.Render("⚠ 单槽模式（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK 覆盖中）——多实例 UI 已禁用") + "\n")
	}
```

footer 变体：单槽模式渲染 `"[s]同步 [c]编辑连接 [t]TTL  q 退出"`（原批1 文案），非单槽渲染 T6 新文案。

- [ ] **Step 4: GREEN + 全量**
- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(tui): env 单槽模式——横幅/[i] 禁用/footer 变体（Plan 40 批2 T7, spec §3.5）"
```

---

### Task 8: 连接表单实例字段 + 规范名前置校验三连 + 换码警告

**Files:**
- Modify: `internal/tui/clientpage.go`（editConnForm、connDraft）
- Test: `internal/tui/clientpage_form_instance_test.go`

**Interfaces:**
- Consumes: 批1 `instname.Valid`/`clientops.ListInstances`；T5 `m.instance`；T2 `SingleSlotOverrideEnvSet`/`DefaultSlotVacuum`。
- Produces: `connDraft.Instance string`；表单三连规则（§4 逐条）；警告文案（§6 逐字）。T9 消费 draft/routed target。

- [ ] **Step 1: 写失败测试**

```go
// clientpage_form_instance_test.go — §4/§6/§11.11/§11.13。
// helper：seedFormSlot(t, name string, withBin bool) —— APPDATA/XDG 重定向 +
// instances/<name>/ 目录 + cache.meta.json（device_name）+ 可选 cache.bin 占位。
func TestEditConnForm_InstanceFieldValidation(t *testing.T) {
	// 非法名（"bad name!"）→ 表单字段校验拒（提交被 huh.Validate 拦）
}
func TestFormRules_CanonicalAndCross(t *testing.T) {
	// ① 选中 agentB + 字段 AGENTB（casefold 命中他实例 agentB，≠ 选中槽）→ 拒（rule 2，文案含冲突对象）
	// ② 选中 AGENTA（seed 目录名大写）+ 字段 agenta → 同槽允许且 target == "AGENTA"（规范名，rule 3）
	// ③ 选中 A + 字段 agentC（新名）+ 空码 → 拒（rule 1 跨槽必填）
	// ④ 选中无 auth 的实例槽 + 空码 → 拒（rule 1 前提：无"保持"可言）
	// ⑤ 选中 A（有 auth）+ 字段空 + 空码 → 放行（同槽保持语义）
}
func TestFormRules_PanelVacuumEmptyField(t *testing.T) {
	// 面板模式 + 默认槽四文件真空 + 字段空 → 拒；文案（常规版）含"走向导流程"
	// env 单槽模式（SSHMGR_CACHE_DIR）同场景 → 文案含"override env"
}
func TestEditConnForm_WarningCopy(t *testing.T) {
	// 默认槽有 bin + meta device_name=X → 表单视图含"已绑定设备 X"与"保留 cache.meta.json 与 cache.config.json"
	// 命名实例槽有 bin → 含"实例 agentA 已绑定设备"
	// 无 bin 槽 → 不含"已绑定"
}
```

- [ ] **Step 2: RED**
- [ ] **Step 3: 实现**

connDraft 加字段：`Instance string`。

editConnForm 主体（在既有 prefill 逻辑上扩展；关键新增段）：

```go
// Plan 40 批2 §4: optional instance field. Absent entirely under a single-slot
// override env (spec §3.5 — disabled, and the field is omitted rather than
// rendered inert).
singleSlot := clientops.SingleSlotOverrideEnvSet()
instances, _ := clientops.ListInstances()
selected := m.instance // already canonical (it names an on-disk dir or "")

// canonicalInstance casefold-matches the typed value against existing instance
// dirs and returns the CANONICAL dir name (spec §4 rev5: fold is for
// normalizing to the on-disk slot; pull-side comparisons stay exact).
canonicalInstance := func(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	for _, n := range instances {
		if strings.EqualFold(n, v) {
			return n
		}
	}
	return v
}

d := &connDraft{URL: urlVal, Pin: pinVal, Instance: selected}

var group *huh.Group
inputFields := []huh.Field{
	huh.NewInput().Title("serve 地址").Value(&d.URL).Validate(validServeURL),
}
if !singleSlot {
	inputFields = append(inputFields,
		huh.NewInput().Title("实例名（可选——默认实例留空）").Value(&d.Instance).Validate(func(v string) error {
			v = strings.TrimSpace(v)
			if v == "" {
				return nil
			}
			return instname.Valid(v)
		}))
}
inputFields = append(inputFields,
	huh.NewInput().Title("设备码（留空=保持不变）").Value(&d.Code).EchoMode(huh.EchoModePassword),
	huh.NewInput().Title("pin（SPKI 指纹，公开信息）").Value(&d.Pin).Validate(validPin),
)
group = huh.NewGroup(inputFields...).Description(swapWarning(selected, singleSlot))
```

swapWarning（构建时静态，§6）：

```go
// swapWarning renders the build-time-static 换码 warning for the SELECTED slot
// (spec §6 — never reacts to field input; the gate stays the only enforcer).
func swapWarning(selected string, singleSlot bool) string {
	dir, bin, metaPath, _, err := clientops.CachePathsFor(selected)
	if err != nil {
		return ""
	}
	if _, serr := os.Stat(bin); serr != nil {
		return "" // no bin: vacuum/new slot — no warning
	}
	device := "(旧 cache 未登记)"
	if b, rerr := os.ReadFile(metaPath); rerr == nil {
		var m struct {
			Device string `json:"device_name"`
		}
		if json.Unmarshal(b, &m) == nil && m.Device != "" {
			device = m.Device
		}
	}
	if selected == "" {
		return fmt.Sprintf("⚠ 默认实例已绑定设备 %s——更换设备码前须清三件套（cache.auth.json + cache.bin + quarantine/，保留 cache.meta.json 与 cache.config.json——它们是默认槽意图标记，删了重 enroll 会被归位到实例槽）重 enroll，否则下次同步将被门禁拒绝；若是本机第二个 agent，请在\"实例名\"字段填新实例名。", device)
	}
	_ = dir
	return fmt.Sprintf("⚠ 实例 %s 已绑定设备 %s——换码须删除该实例目录重 enroll，否则同步将被拒。", selected, device)
}
```

提交闭包内的**前置校验三连**（放在既有 token 解析之前；`m` 是值接收者模型——闭包捕获 selected/instances/singleSlot）：

```go
return newFormOverlay("编辑连接", form, func() tea.Cmd {
	return func() tea.Msg {
		target := canonicalInstance(d.Instance)
		sameSlot := strings.EqualFold(target, selected)
		// rule 2: fold-hit on an EXISTING instance that is NOT the selected slot
		// → hard refuse (NTFS collision / cross-slot re-route; to re-code an
		// existing instance, [i]-switch to it first — spec §4 rev5).
		if target != "" && !sameSlot {
			for _, n := range instances {
				if strings.EqualFold(n, target) {
					return errMsg{fmt.Errorf("实例名与已存在实例 %s 冲突——对其换码请先 [i] 切换到该实例（跨槽路由被拒绝）", n)}
				}
			}
		}
		code := strings.TrimSpace(d.Code)
		// rule 1: cross-slot, or a selected slot with no stored auth → code REQUIRED
		if (!sameSlot || token0Empty()) && code == "" {
			return errMsg{errors.New("设备码不能为空——跨实例路由或本槽无已保存设备码时不存在\"保持不变\"")}
		}
		// panel-mode vacuum guard (spec §4): empty field on a vacuum default
		// would silently become an auto-relocation trigger surface — refuse with
		// mode-dependent guidance.
		if !wizard && target == "" && !singleSlot {
			if vac, verr := clientops.DefaultSlotVacuum(); verr == nil && vac {
				return errMsg{errors.New("默认实例无材料——首次 enroll 请走向导流程（自动归位），或填实例名显式路由")}
			}
		}
		if singleSlot && target != "" {
			return errMsg{fmt.Errorf("--instance and %s are mutually exclusive — unset the env or clear the 实例名 field", overrideEnvName())}
		}
		// …（既有 token 保持/写入逻辑由 T9 改造——本任务先保留现状的
		// WriteCacheCred 调用，T9 再替换为写序三分；三连先行落地）
	}
})
```

（`token0Empty()` = 既有 `token0 == ""` 的闭包捕获；`overrideEnvName()` 返回在场的那一个 env 名——小 helper。）

- [ ] **Step 4: GREEN + 全量**（既有 `TestEditConnFormRequiresCodeWhenNoToken` 等不回归）
- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(tui): 连接表单实例字段+规范名前置校验三连+换码静态警告（三件套/保留 meta/config 文案）（Plan 40 批2 T8, spec §4/§6）"
```

---

### Task 9: auth 写序三分 + 向导接入卡 + 首拉自动选中

**Files:**
- Modify: `internal/tui/clientpage.go`（editConnForm 提交闭包、syncCmdMode 写序、connSavedMsg/pullSucceededMsg 处理、clientFinishScreen 签名、wizard 自动选中）
- Test: `internal/tui/clientpage_writeorder_test.go`

**Interfaces:**
- Consumes: T1 `PullResult.Instance`；T8 的三连/target。
- Produces: wizard 后移写（含 WARNING 恢复文案）；面板即时写（MkdirAll + `os.Remove` 空目录清理 + m.instance 切换）；`clientFinishScreen(serveURL, instance string) overlay`。

- [ ] **Step 1: 写失败测试**

```go
// clientpage_writeorder_test.go — §5/§7/§11.12/§11.15。
func TestWizardSubmit_DoesNotWriteAuthBeforePull(t *testing.T) {
	// 真空 seed：提交表单 → 默认槽与实例槽均无 cache.auth.json（connSavedMsg 只带 draft/cred）
}
func TestPanelSubmit_NewInstanceMkdirAllAndAuth(t *testing.T) {
	// 选中默认 + 字段 agentC + 码 → 提交 → instances/agentC/ 建立 + cache.auth.json 落该槽
	// + connSavedMsg.instance == "agentC" → Update 后 m.instance == "agentC"
}
func TestPanelSubmit_AuthWriteFailureCleansEmptyDir(t *testing.T) {
	// instances/agentC/cache.auth.json 预放【目录】占位 → 提交失败 errMsg
	// + agentC 目录被清理（ListInstances 不含）——os.Remove 仅空目录语义
	// 对照：既有目录（预置 meta）写失败 → 目录保留
}
func TestFinishScreen_InstanceForm(t *testing.T) {
	// clientFinishScreen("https://x", "agentA") 视图含 `"--instance", "agentA"` 与
	// 实例槽注释行；instance == "" 时为既有双形态（零回归）
}
func TestPullSucceeded_AutoSelectsEffectiveInstance(t *testing.T) {
	// pullSucceededMsg{instance:"agentA"} → m.instance == "agentA" + finish overlay instance 传入
}
```

- [ ] **Step 2: RED**
- [ ] **Step 3: 实现**

(a) 提交闭包三分（替换 T8 保留的原 WriteCacheCred 段）：

```go
		token := token0
		if code != "" {
			token = code
		}
		if token == "" {
			return errMsg{errors.New("设备码不能为空（本机没有已保存的设备码可保持）")}
		}
		cred := &clientops.CacheCred{URL: strings.TrimSpace(d.URL), Token: token, Pin: strings.TrimSpace(d.Pin)}
		if wizard {
			// §5 wizard row: NOTHING is persisted at save time — the pull reveals
			// the effective slot (auto-relocation), auth lands after success.
			return connSavedMsg{cred: cred, draft: d, instance: target}
		}
		// §5 panel row: write NOW to the form-routed slot. A NEW instance slot
		// needs its directory first (WriteCacheCredFor never creates parents —
		// §0.13); on write failure clean up the dir we just created IF empty
		// (os.Remove fails on non-empty = exactly the guard we want).
		created := false
		if target != "" {
			tdir, _, _, _, derr := clientops.CachePathsFor(target)
			if derr != nil {
				return errMsg{derr}
			}
			if _, serr := os.Stat(tdir); os.IsNotExist(serr) {
				if mkerr := os.MkdirAll(tdir, 0o700); mkerr != nil {
					return errMsg{mkerr}
				}
				created = true
			}
			if werr := clientops.WriteCacheCredFor(target, cred); werr != nil {
				if created {
					os.Remove(tdir) // best-effort; only removes when empty
				}
				return errMsg{werr}
			}
		} else if werr := clientops.WriteCacheCredFor("", cred); werr != nil {
			return errMsg{werr}
		}
		return connSavedMsg{cred: cred, draft: d, instance: target}
```

(b) syncCmdMode 写序（wizard 分支）：

```go
		res, err := clientops.DoPull(cred.URL, cred.Token, cred.Pin, clientops.PullOpts{Timeout: clientops.LazyPullTimeout, Instance: instance})
		if err != nil {
			return syncDoneMsg{err}
		}
		if wizard {
			// §5: auth lands on the EFFECTIVE slot after a successful pull. A
			// write failure is a WARNING (pull succeeded; refresh chain down
			// until the next successful pull) with the TUI-honest recovery path.
			if werr := clientops.WriteCacheCredFor(res.Instance, cred); werr != nil {
				fmt.Fprintf(os.Stderr, "WARNING: auth 未落盘——本 TUI 的 [s] 同步不可用；恢复 = CLI cache pull 或重跑向导表单（输入已保留）: %v\n", werr)
			}
			return pullSucceededMsg{instance: res.Instance}
		}
		return syncDoneMsg{nil}
```

(c) 消息处理：

```go
	case syncDoneMsg:
		m.busy = false
		if kp.err != nil {
			if m.wizard {
				m.err, m.status = errors.New(classifyPullError(kp.err)), ""
				m.overlay = m.editConnForm()
				return m, m.overlay.Init()
			}
			m.err, m.status = kp.err, ""
		} else {
			m.err, m.status = nil, "同步完成"
		}
		return m, refreshDataCmdFor(m.instance)

	case pullSucceededMsg:
		m.busy = false
		m.err, m.status = nil, "首次同步完成"
		m.finish = true
		m.instance = kp.instance // R2-Q2a: land on the effective slot
		serveURL := ""
		if m.cred != nil {
			serveURL = m.cred.URL
		}
		m.overlay = clientFinishScreen(serveURL, kp.instance)
		return m, tea.Batch(m.overlay.Init(), refreshDataCmdFor(kp.instance))

	case connSavedMsg:
		m.err, m.status = nil, ""
		m.cred, m.draft = kp.cred, kp.draft
		if m.wizard {
			m.busy = true
			return m, syncCmdMode(kp.cred, kp.instance, true)
		}
		m.instance = kp.instance // §3.3: UI/auth/[s] follow the form-routed slot
		m.status = "连接配置已保存"
		return m, refreshDataCmdFor(kp.instance)
```

(d) clientFinishScreen 加 instance 参数与 `--instance` 段：

```go
func clientFinishScreen(serveURL, instance string) overlay {
	if serveURL == "" {
		serveURL = "<serve URL>"
	}
	args := []string{`"args": ["mcp", "--cache"]`}
	notes := []string{
		"client 角色用 --cache 离线缓存模式启动；SSHMGR_TOKEN 填 server 机 Projects 页签发的 project token（不是设备码——设备码只用于拉取缓存，刚才已保存）。",
		`Windows 建议写绝对路径，如 "command": "C:\\Tools\\ssh-manager.exe"。`,
		".mcp.json 含 token，不要提交进 git。",
	}
	if instance != "" {
		// Plan 40 批2 §7: the cache landed in instances/<name>/ — the config must
		// route the agent there or mcp --cache reports "default cache missing".
		args = []string{fmt.Sprintf(`"args": ["mcp", "--cache", "--instance", %q]`, instance)}
		notes = append([]string{fmt.Sprintf("本机 cache 位于实例槽 instances/%s/——args 必须带 --instance %s。", instance, instance)}, notes...)
	}
	offline := mcpConfigLines(args,
		[]string{stdioEnvLine("<project token>")}, notes)
	...（online 段与既有完全一致）
}
```

既有调用点两处（pullSucceededMsg 旧 handler、测试）同步改签名。

- [ ] **Step 4: GREEN + 全量**（`TestClientFinishScreen_DualForms` 等按新签名机械更新——instance 传 ""）
- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(tui): auth 写序三分（wizard 后移含恢复文案/面板 MkdirAll+空目录清理+切槽）+ 接入卡 --instance 段 + 首拉自动选中（Plan 40 批2 T9, spec §5/§7）"
```

---

### Task 10: gateDefaultInstance 换码文案更新（runbook v2）

**Files:**
- Modify: `internal/clientops/clientops.go`（gateDefaultInstance 拒绝文案 + gateNamedInstance 不动）
- Test: `internal/clientops/gate_test.go`（追加文案断言）

- [ ] **Step 1: 失败测试**：既有异码拒绝用例断言新文案——含 `KEEP cache.meta.json and cache.config.json`（或中文等价）与 `instances/`；不含旧串 `cache.meta.json + the quarantine`（四件套形态）。
- [ ] **Step 2: RED → Step 3: 改文案**：

```go
		return fmt.Errorf("refusing pull: this cache belongs to device %q but the presented device code is %q — pick one:\n"+
			"  1. this is a SECOND device on this machine: re-run the pull with --instance %q\n"+
			"  2. replace the default instance's device code: delete cache.auth.json + cache.bin + the quarantine/ dir in this cache directory (KEEP cache.meta.json and cache.config.json — they mark this as the DEFAULT slot; deleting them re-routes the re-enroll into instances/) and re-enroll\n"+
			"  3. owner: verify which device this code was issued for (`cache-tokens ls` on the server)", m.DeviceName, deviceName, deviceName)
```

- [ ] **Step 4: GREEN + 全量** → **Step 5: Commit**：`fix(clientops): 换码 runbook v2 文案——三件套清理+保留 meta/config 意图标记（Plan 40 批2 T10, spec §6/§12）`

---

### Task 11: e2e——双实例 enroll 全程（含归位路径）

**Files:**
- Test: `internal/cli/multiinstance_e2e_test.go`（追加 `TestDualInstanceEnroll_E2E`）

- [ ] **Step 1: 写测试（§11.16 + §11.3/7 关键行）**

复用既有 fixture（同文件 ：30-82 形态，码 A/B + 两 profile + pinned serve）：

```go
// TestDualInstanceEnrollEnroll_E2E — §11.16：真空机 A 裸 pull 归位（无 flag）；
// 默认槽占用机 B 显式 --instance enroll；两实例各自 mcp --cache --instance 可载。
// 附带钉：§11.3 换码三件套清理回默认槽；§11.7 归位幂等。
func TestDualInstanceEnroll_E2E(t *testing.T) {
	// ① 真空机 + codeA 裸 pull → instances/laptop-agentA/ + 默认槽全空（TestRelocate 已覆盖单元面，此处 e2e 形态）
	// ② auth 断言：CLI 写序——手工模拟 cli 路径：DoPull 成功后 WriteCacheCredFor(res.Instance, cred)
	//    → instances/laptop-agentA/cache.auth.json 存在（lazy 链闭合）
	// ③ 显式 enroll B：pull --instance laptop-agentB → 独立槽（批1 已测，保留组合形态）
	// ④ 幂等：再裸 pull codeA → 同实例目录、DEK 不重复（dekDir 下 cache-dek-laptop-agentA.key mtime/内容不变）
	// ⑤ 换码回默认槽：默认槽先造材料（meta+config 在）→ 清三件套（保 meta/config）→ 裸 pull codeB
	//    → 材料落默认目录（非归位）+ meta.device_name 补记 laptop-agentB
	// ⑥ 两实例 LoadCacheSnapshotFor 各自 1 台且互不串
}
```

- [ ] **Step 2: RED（若 T2 已 GREEN 则本步应为 GREEN——以 e2e 形态复验）→ Step 3: 必要修正 → Step 4: 全量 → Step 5: Commit**：`test(e2e): 双实例 enroll 全程——裸 pull 归位/auth 连动落槽/幂等/换码回默认槽（Plan 40 批2 T11）`

---

### Task 12: 文档波

**Files:**
- Modify: `docs/multi-machine.md`（自动归位语义 + 换码 runbook v2 + 双 agent TUI 流程 + `cache config` + CLI-first 指引）
- Modify: `docs/tui-multi-machine.md`（`[i]` picker/表单字段与三连/换码警告/单槽横幅）
- Modify: `README.md`（`cache config` + `[i]` + CLI 归位后 `--instance` 指引）
- Modify: `docs/agent-access.md`（一句话指向）
- Modify: `docs/compat-matrix.md`（v0.12.0 行——**发版后**才填"已验证"，本批先占位说明）
- Modify: `docs/backlog.md`（销项第二批五项；doctor 登记）

- [ ] **Step 1: 按下列要点逐文件更新**（每条均为 rev5 逐字锚）：
  - 归位：真空 v4 四文件定义、七态不归位（含双 override env 与 meta/config 意图标记）、归位提示行含义。
  - runbook v2：清三件套保留 meta/config；`rm -rf` 全清 = 机器重置语义（归位）。
  - CLI-first：CLI 归位后手工 `.mcp.json` 必须加 `--instance <name>`（无向导卡）。
  - TUI：`[i]` 会话内切换、「（默认实例）」行、单槽模式横幅、表单三连、换码警告文案、`[t]` 不变。
  - `cache config`：三源显示、仅已存在实例、无 off 开关（手动删文件）、默认槽 meta/config 勿删。
  - backlog：五项销项 + "doctor 多实例仍跟随 Plan 38"。
- [ ] **Step 2: 全量 `go test ./...`（docs-only 也跑——CI 同 lane）→ gofmt 证 → Commit**：

```bash
git add -A && git commit -m "docs: Plan 40 批2 文档波——归位语义/runbook v2/TUI picker 与表单/cache config/CLI-first 指引/backlog 销项（T12）"
```

---

## Self-Review（写 plan 后自查记录）

1. **Spec 覆盖**：rev5 §1（T2/T3/T10/T11）、§2（T1）、§3.1-3.2（T6）、§3.3（T5/T6）、§3.4（T6）、§3.5（T7）、§4+§6（T8）、§5+§7（T9）、§8（T4）、§11 各行落对应任务（11.1-7→T2/T3/T11；11.9→T6；11.10→T7；11.11→T8；11.12/15→T9；11.13→T8；11.14→T4；11.16→T11）、§12（T12）。§13 残余 = 登记非任务 ✓。
2. **占位符扫描**：T2 测试骨架中 fixture 细节标注"同 e2e 形态"并指向精确锚（`cli/multiinstance_e2e_test.go:30-82`）——执行者可照抄；无 TBD/TODO。
3. **类型一致性**：`PullResult.Instance`（T1 产出 → T2 改写 → T9 消费）；`dataReadyMsg.instance`（T5 → T6 消费）；`connSavedMsg.instance`（T8/T9）；`clientFinishScreen(serveURL, instance)`（T9 内两处调用同步）；`validateCapFileIndependent`（T3 产出 → T2 消费——执行顺序注记已写）。
4. **已知裁剪**：T8 的 `token0Empty()`/`overrideEnvName()` 为小 helper（内联一行可实现）；`clip(0,…)` 行为需执行者按实现核对（注记已写）。
