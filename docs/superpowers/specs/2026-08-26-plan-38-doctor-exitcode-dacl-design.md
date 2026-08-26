# Plan 38 设计：doctor 硬化接线——exit 2 接线（backlog P2 #7）+ Windows DACL readback（backlog P2 #6）

> backlog P2 首批（doctor 系三连的前两个；#5 serve 探活二期是下一个独立 plan，依赖本 plan 的 #7 接线）。2026-08-26 brainstorming 已拍板、本文不重议的决策：**DACL 过松 → WARN（对称 Unix mode 0644→WARN 先例，钉死于 TestDoctorVaultStructural）**、**检测范围只 master.key（API 按 path 泛化，store.db/cache-dek.key/WAL 留 backlog 后续）**、**方案 A：store 深模块读 API（判定语义与 HardenACL 写侧同居）+ cli 侧 ExitCodeError 接口约定**。

## 0. 目标与代码现状事实（全部 2026-08-26 于 0c8d28d 核实）

1. **exit 1 硬编码**：`cmd/ssh-manager/main.go:11-14` 对所有 cobra 错误统一 `os.Exit(1)`。
2. **doctorExitCode 零生产调用者**：三态映射（nil→0 / errDoctorFindings（wrapped 含）→1 / 其他→2）定义于 `internal/cli/doctor.go:58`，仅被 `TestDoctorExitCodes`（doctor_test.go:110-121）钉住——正是 backlog #7 所述「保留码静默烂掉」的双真相源结构。
3. **runDoctor 只产生 nil / errDoctorFindings**（doctor.go:603-626）：无任何运行时路径产生「doctor 内部错误」，exit 2 当前不可达。
4. **doctor 帮助文本只承诺 0/1**（doctor.go:637-638 Long 文本），未提 2。
5. **写侧 HardenACL 完备且接线面广**（全部走 `windows.SetNamedSecurityInfo`，纯 Go 无 icacls 外部依赖，`acl_windows.go:41`）：`FileKeyProvider.Set`（masterkey_file.go:92，master.key）、`store.Open` 首建（store.go:108，store.db）、`hardenWALSidecars`（store.go:151，-wal/-shm）、`roles.Save`（roles.go:175，role.json）、clientops（clientops.go:253，cache-dek.key 侧）、serve 证书三件 + marker（mcpserver/cert.go:122/254/257）、serve 服务目录（serve_service.go:570）。写侧产出的 DACL 形态：SYSTEM+Administrators 全控、当前用户 `READ_CONTROL|DELETE|FILE_GENERIC_READ|FILE_GENERIC_WRITE`（**刻意不给 WRITE_DAC/WRITE_OWNER**）、断继承（SE_DACL_PROTECTED）、无 BUILTIN\Users / Authenticated Users / Everyone。
6. **读侧三件困在 test-helper 块**：`getDACLForTest` / `isDaclProtected` / `trusteeInACL`（acl_windows.go:137-182）——注释自述 "Test helpers (only compiled under the windows build tag)"，生产命名缺失是 backlog #6 字面所述的封锁点。
7. **doctor 的 Windows 保护层校验为空**：`checkVaultKey` default PASS 分支只在非 Windows 查 mode bits（doctor.go:292-298），Windows 跳过——backlog #6：Windows 上 master.key 硬化校验只剩 32 字节长度。
8. **F1 教训（ACL 破损是真实发生过的类别）**：`TestOpen_DoesNotRewriteExistingStoreDBACL`（acl_windows_test.go:241）——serve（LocalSystem）重开用户建的 store.db 时 HardenACL 曾在服务令牌下重跑，SET_ACCESS 去重静默丢用户 ACE。现行 Open 已修（存在即跳过硬化），**但代价是：ACL 一旦被外部改坏，生产路径永不修复也无人知晓**——读侧检测（doctor）是唯一补口。
9. **`trusteeInACL` 不区分 allow/deny ACE**（acl_windows.go:164-182）：对 `Everyone:(deny)R` 这类收紧型 ACE 会误判「含 Everyone」。生产化时必须只统计 allow 类（否则真实误报类）。
10. **seedDoctorVault 直写不触发硬化**（doctor_test.go:234 用 `os.WriteFile` 0600）：Windows 上 t.TempDir() 的默认 DACL 是继承的宽 DACL——新检查上线后 CI windows lane 的现存断言（healthy vault → 0 WARN）会破，seed 必须改走 `FileKeyProvider.Set`。
11. **unlock 不重写既有 master.key**（unlock.go:45-48：文件存在且可读 → `fp.Get()` 直接打印返回；仅 ErrNotFound 才 `Generate + Set`）：「重跑 unlock 恢复硬化 ACL」不可行（已读不写），「删 key 再 unlock」更不可行（生成全新 key，vault 解不开——FINDING A 重演）。修复指引只能是手工 icacls。
12. **无真二进制 e2e 测试先例**：cli_smoke_test.go 为进程内 cobra 驱动（`NewRootCmd().Execute()`），全仓无「编译真二进制跑退出码」的测试形态。
13. **跨平台 stub 先例**：`acl_other.go` 的 `HardenACL` no-op（`!windows` build tag）——读 API 沿用同款双文件模式，doctor.go 单文件可编译。

## 1. `InspectFileACL` 读 API（internal/store）

写/读双子（对齐 `LoadOrCreateServeCert` / `ReadServeCertFingerprint` 先例）。「什么算过松」的判定**在 store**——与 HardenACL 写侧同文件，读写语义不漂移（宽 trustee 清单 = 写侧刻意排除的那批）。

### 1.1 结构与签名

```go
// acl.go（跨平台声明 + FileACLReport 方法）；实现分置 acl_windows.go / acl_other.go
type FileACLReport struct {
    Supported      bool     // false = 非 Windows（mode bits 才是该平台的保护层）
    DaclNull       bool     // 无 DACL = 对所有人放行（信号 1）
    Protected      bool     // SE_DACL_PROTECTED：继承已断（信号 2 取 !Protected）
    BannedTrustees []string // allow ACE 里出现的宽 trustee 人类可读名（信号 3）
}
func (r FileACLReport) TooLoose() bool // DaclNull || !Protected || len(BannedTrustees) > 0

func InspectFileACL(path string) (FileACLReport, error)
```

### 1.2 TooLoose 信号集（只查机密性方向、只查「过松」不查「过严」）

1. **DACL null**：`SE_DACL_PRESENT` 未设或 DACL 指针 null = 无 discretionary 保护，全允许。
2. **继承未断**：`!Protected`——父目录的宽 ACE（如 `C:\ProgramData` 继承链的 Authenticated Users:modify）仍然生效。
3. **allow ACE 含宽 trustee**：Everyone（S-1-1-0）/ Authenticated Users（S-1-5-11）/ BUILTIN\Users（S-1-5-32-545），SID 精确比对（`CreateWellKnownSid` 构造，locale 无关，沿 `trusteeInACL` 的 `SID.Equals` 先例）。**只统计 allow 类 ACE**（`ACCESS_ALLOWED_ACE_HEADER` 型；deny 型 `Everyone:(deny)R` 是收紧不是暴露——现状事实 #9 的修正）。

**刻意不查的两个维度（不告警）**：
- 当前用户 ACE mask 含 WRITE_DAC / WRITE_OWNER / GENERIC_ALL：防篡改方向非机密性；owner 手动维修给自己全控是合法操作，误报面大。Unix 对称面也只查 group/world **read** bits。
- 「过严」（缺 SYSTEM/Admins ACE 等）：不泄密，不告警——安全检查的正确不对称。

### 1.3 错误语义

`GetNamedSecurityInfo` / SD 解析失败 → 返回 error（含 ACCESS_DENIED 类）。硬化态下 user mask 含 `READ_CONTROL`（写侧 userMask 事实 #5），读不回 = ACL 被改到连 SD 都读不了 = 深度异常。error 文本以 `inspect ACL:` 前缀裹底层错误，不吞。

### 1.4 test-helper 三件生产化

`getDACLForTest` → `readDACL`（私有生产名）、`isDaclProtected` / `trusteeInACL` 移出 test-helper 注释块转正（名字本就中性）。`acl_windows_test.go` 全部调用点跟着改。**行为零变化，纯搬家+正名**——`InspectFileACL` 是它们之上的新生产入口（banned-allow 判定新逻辑落在 InspectFileACL 内：walk ACE 时按 `AceType` 过滤）。

### 1.5 非 Windows stub（acl_other.go）

```go
func InspectFileACL(path string) (FileACLReport, error) {
    return FileACLReport{Supported: false}, nil
}
```

对称 HardenACL no-op 先例（事实 #13），doctor.go 单文件可编译、无需自身 build-tag 分文件。

## 2. doctor 接线（checkVaultKey Windows 分支）

`default:` PASS 分支内，与 Unix mode-bit 检查对称：

| 情形 | 判定 | Detail / Fix | 对称先例 |
|---|---|---|---|
| `TooLoose()` | **WARN** | 见 §2.1 冻结文案 | Unix 0644 → WARN |
| `InspectFileACL` 出错 | **FAIL** | master.key ACL unreadable + fix 查权限/备份恢复 | `checkVaultStore` stat failed → FAIL |
| 其余（Supported 且 !TooLoose） | **PASS**（Detail 不变，不加 "ACL ok"） | — | Unix PASS 不写 mode |

非 Windows：`acl_other` stub 返回 `Supported=false`，`checkVaultKey` 现行 Unix mode-bit 分支零变化（`runtime.GOOS` guard 维持）。Windows 分支的判定顺序：**长度检查（现行 FAIL 分支）优先于 ACL 检查**——文件本身坏是更高优先级信号，ACL 只在 32 字节合法后追加检查。

### 2.1 WARN 冻结文案

Detail 模板（signals 为命中信号列表，顿号分隔；三个信号的呈现词冻结：`no DACL` / `inheritance enabled` / `broad trustees granted: <names>`，names 逗号并列）：

```
master.key present (32 bytes) but its DACL is too loose (<signals>) — the plaintext key is protected by this ACL alone
```

Fix（手工 icacls 指引；unlock 路线已核实不可行，事实 #11）：

```
restore a restrictive DACL, e.g. icacls <master.key> /inheritance:r /grant "SYSTEM:(F)" /grant "Administrators:(F)" /grant "<user>:(RC,R,W,D)"
```

## 3. exit 2 接线（internal/cli/exit.go 新文件）

### 3.1 类型与映射

```go
// ExitCodeError lets a RunE pin the process exit code that main will honor;
// every other error keeps the generic 1.
type ExitCodeError struct {
    Code int
    Err  error
}
func (e *ExitCodeError) Error() string { return e.Err.Error() }
func (e *ExitCodeError) Unwrap() error { return e.Err }

// ExitCodeFor maps a root-command error to the process exit code: an
// ExitCodeError pins its code, anything else is 1.
func ExitCodeFor(err error) int {
    if err == nil { return 0 }
    var ec *ExitCodeError
    if errors.As(err, &ec) { return ec.Code }
    return 1
}
```

Code 范围不做运行时防御（内部类型，契约由测试钉；Code≤0 是 bug 不是输入）。

### 3.2 接线三处

1. **runDoctor 尾部**（doctor.go:622-625 现行 `return fmt.Errorf("%w (%d) — see the report above", ...)` 改为）：
   ```go
   return &ExitCodeError{Code: 1, Err: fmt.Errorf("%w (%d) — see the report above", errDoctorFindings, fail)}
   ```
   外层带码、内层保持 `errors.Is(err, errDoctorFindings)` 可达——现有测试的 wrapped-findings 断言不破。
2. **main.go**：`os.Exit(1)` → `os.Exit(cli.ExitCodeFor(err))`（stderr 打印维持）。
3. **删除 doctorExitCode**（doctor.go:55-67）：双真相源根治（事实 #2）。其三态语义由新结构承载：0 = Execute 成功（main 不进错误分支）；1 = findings（ExitCodeError 显式）或任意其他命令错误（main 默认）；2 = 未来内部错误源 `&ExitCodeError{Code: 2}`。

### 3.3 exit 2 的产生源边界

**本 plan 不给 doctor 新增运行时内部错误源**——管道接通 + 契约钉死，第一个真实 2 源是 #5 二期探活（backlog #7 原文定位：「doctor 将来长出会内部出错的检查（如二期 HTTP 探活）前，必须先把 2 接进真实退出路径」）。2 的可达性在本 plan 由测试构造（§4），生产可达性登记为 #5 的前提。

### 3.4 用户可见契约更新

doctor Long 帮助文本（doctor.go:637-638）补一行：

```
2 = doctor internal error.
```

## 4. 测试矩阵

### 4.1 store 侧（acl_windows_test.go 增，`windows` build tag，全部真读回无 mock——`TestOpen_DoesNotRewriteExistingStoreDBACL` 种宽 DACL 先例）

| 测试 | 断言 |
|---|---|
| `TestInspectFileACL_Hardened` | `HardenACL` 后 `Supported && !TooLoose`、`Protected`、`BannedTrustees` 空 |
| `TestInspectFileACL_TooLoose_Matrix` | ① 种 null DACL（`SetNamedSecurityInfo` 传 nil dacl——Windows 文档语义即「无 DACL 全允许」；若该形态在 x/sys 包装层不可达，降级为 Everyone-allow + UNPROTECTED 种法，等效覆盖信号面）→ `DaclNull && TooLoose`；② 种宽 DACL 但 `UNPROTECTED`（继承活）→ `!Protected && TooLoose`；③ 种 Everyone-allow ACE → `BannedTrustees` 含 "Everyone"；④ 种 Everyone-**deny** ACE → **不**算危险（allow-only 语义钉子，事实 #9） |
| 不存在路径 | 返回 error（非 panic / 非 false 报告） |

非 Windows：`acl_other_test.go`（`!windows` tag）断言 stub `Supported=false, err=nil`。

### 4.2 doctor 侧（doctor_test.go）

- **seedDoctorVault 修正**（事实 #10）：master.key 改走 `FileKeyProvider.Set`（触发 HardenACL），Windows lane 现存断言（healthy vault → `masterkey: PASS` / `overall: 0 WARN, 0 FAIL`）保持绿；Unix lane 同样不变——已核实 Set 的 Unix 产出 = `os.CreateTemp`（0600）+ rename（masterkey_file.go:64-81）+ `MkdirAll(dir, 0o700)`，文件 mode 0600 与现行 `os.WriteFile(0o600)` seed 等价，mode-bit WARN 不触发。
- **Windows-only 新腿**（`runtime.GOOS == "windows"` guard，对齐 Case 3 的 Unix-only 模式）：
  - seed 后种宽 DACL（Everyone-allow）→ `masterkey: WARN` + §2.1 Detail 命中 + `overall: 1 WARN, 0 FAIL` + 退出码不受 WARN 影响；
  - （廉价顺带）种 null DACL → 同 WARN 通道。

### 4.3 exit 侧（exit 测试 + TestDoctorExitCodes 扩展）

- `ExitCodeFor` 三态：`ExitCodeError{2}` → 2；普通 error → 1；nil → 0（防御分支）。
- cobra 层注入：临时子命令 RunE 返回 `&ExitCodeError{2}` → `ExitCodeFor` 取 2（任意命令可传码的通路证明）。
- `TestDoctorExitCodes` state 3 重写（doctorExitCode 已删）：findings err → `errors.Is(err, errDoctorFindings)` 且 `ExitCodeFor(err)==1`。
- **无真二进制 e2e**（事实 #12 无先例，本 plan 不首创）：main.go 收敛为三行（打印 + `ExitCodeFor`），全部判定逻辑在已测函数内。

## 5. 文档联动

- doctor Long 帮助文本补 exit 2（§3.4）。
- `backlog.md`：P2 #6/#7 销项画线（留墓碑，编号稳定惯例）。
- `compat-matrix.md` 增两行：doctor exit 2 新可达（0/1 不变，纯契约扩展，无破坏）；Windows master.key 新增 DACL-loose WARN 行。
- threat-model / concepts / agent-facing 文档不动：doctor 是 owner 侧工具，检测能力增强不改威胁面，agent 不可见。

## 6. Scope 边界（明确不做，scope 纪律留痕）

- 不检测 store.db / cache-dek.key / role.json / serve 证书 / WAL sidecars（backlog 后续条目；`InspectFileACL(path)` 已泛化，扩展零接口改动）。
- 不自动修复：doctor side-effect-free 铁律，修复是指引（fix 文案）不是动作。
- 不动 HardenACL 写侧。
- 不新增 doctor 运行时内部错误源（#5 二期的事，§3.3）。
- deny-ACE 过度收紧不告警（§1.2 不对称原则）。
- 不做真二进制 e2e 退出码测试形态首创（§4.3）。

## 7. 残余与风险登记

- **ACL 弥合时机**：doctor 只读不修（§6），ACL 破损的修复永远依赖 owner 手工操作——WARN 的 fix 文案是唯一引导路径。接受：自动修复违反 doctor 铁律，修在 unlock/Open 是另一次 grilling 的话题（预感会撞 F1 的「Open 不重写既有 ACL」裁决，本 plan 不碰）。
- **icacls 指引的精确性**：fix 文案的 icacls 命令是 owner 手工操作的近似指引（`RC,R,W,D` 近似写侧 userMask），非逐位复刻——doctor fix 是提示不是脚本，接受近似。
- **exit 2 生产可达性为零（至 #5 前）**：契约已立、管道已通、测试构造可达——「保留码」不再「静默烂掉」（有测试 + 帮助文本 + ExitCodeError 通路三重钉），但真实生产触发要等 #5。如实登记。
- **BannedTrustees 人类名**：Everyone / Authenticated Users / BUILTIN\Users 三个字符串硬编码于 Windows 实现（SID 构造对应的固定 label），非 LookupAccountSid 本地化名——locale 无关性优先，名可能与非英文系统的显示名不一致，接受。
