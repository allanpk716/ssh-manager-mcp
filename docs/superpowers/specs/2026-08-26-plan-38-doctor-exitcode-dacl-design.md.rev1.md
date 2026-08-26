# Plan 38 设计：doctor 硬化接线——exit 2 接线（backlog P2 #7）+ Windows DACL readback（backlog P2 #6）

> backlog P2 首批（doctor 系三连的前两个；#5 serve 探活二期是下一个独立 plan，依赖本 plan 的 #7 接线）。2026-08-26 brainstorming 已拍板、本文不重议的决策：**DACL 过松 → WARN（对称 Unix mode 0644→WARN 先例，钉死于 TestDoctorVaultStructural）**、**检测范围只 master.key（API 按 path 泛化，store.db/cache-dek.key/WAL 留 backlog 后续）**、**方案 A：store 深模块读 API（判定语义与 HardenACL 写侧同居）+ cli 侧 ExitCodeError 接口约定**。
>
> 本版 = **rev1**（2026-08-26 外部评审闭环修订；v1 的 10 条可验证反馈全部证实/实验成立后改写）。相对 v1 的关键变更：**信号 3 黑名单 → 白名单 read-capable**（任何非白名单主体的 allow ACE 带读/提权能力即松——实验实证黑名单检不出「另一用户只读继承 ACE」类暴露）；**信号 2（继承未断）降级为 Detail 辅助**（实验证实现存暴露由继承 ACE 比对覆盖，独立 WARN 只剩「防未来」价值，拍板放弃）；**exit 2 帮助文本随 #5 上**（本 plan 只接管道，消除「承诺 2 但生产零可达」矛盾窗口）；icacls fix 重写（v1 指引移不掉显式宽 ACE，是无效指引）；doctor 去 `runtime.GOOS` guard（收敛进 `Supported`）；`NewExitCodeError` 构造校验；测试矩阵补洞（err→FAIL seam 腿、mask 过滤腿、宽父继承腿）。

## 0. 目标与代码现状事实（全部 2026-08-26 于 0c8d28d 核实；#14 为同日实验实证）

1. **exit 1 硬编码**：`cmd/ssh-manager/main.go:11-14` 对所有 cobra 错误统一 `os.Exit(1)`。
2. **doctorExitCode 零生产调用者**：三态映射（nil→0 / errDoctorFindings（wrapped 含）→1 / 其他→2）定义于 `internal/cli/doctor.go:58`，仅被 `TestDoctorExitCodes`（doctor_test.go:110-121）钉住——正是 backlog #7 所述「保留码静默烂掉」的双真相源结构。
3. **runDoctor 只产生 nil / errDoctorFindings**（doctor.go:603-626）：无任何运行时路径产生「doctor 内部错误」，exit 2 当前不可达。
4. **doctor 帮助文本只承诺 0/1**（doctor.go:637-638 Long 文本），未提 2（本 plan 维持不动，见 §3.4）。
5. **写侧 HardenACL 完备且接线面广**（全部走 `windows.SetNamedSecurityInfo`，纯 Go 无 icacls 外部依赖，`acl_windows.go:41`）：`FileKeyProvider.Set`（masterkey_file.go:92，master.key）、`store.Open` 首建（store.go:108，store.db）、`hardenWALSidecars`（store.go:151，-wal/-shm）、`roles.Save`（roles.go:175，role.json）、clientops（clientops.go:253，cache-dek.key 侧）、serve 证书三件 + marker（mcpserver/cert.go:122/254/257）、serve 服务目录（serve_service.go:570）。写侧产出的 DACL 形态：SYSTEM+Administrators 全控、当前用户 `READ_CONTROL|DELETE|FILE_GENERIC_READ|FILE_GENERIC_WRITE`（**刻意不给 WRITE_DAC/WRITE_OWNER**）、断继承（SE_DACL_PROTECTED）、无 BUILTIN\Users / Authenticated Users / Everyone。
6. **读侧三件困在 test-helper 块**：`getDACLForTest` / `isDaclProtected` / `trusteeInACL`（acl_windows.go:137-182）——注释自述 "Test helpers (only compiled under the windows build tag)"，生产命名缺失是 backlog #6 字面所述的封锁点。
7. **doctor 的 Windows 保护层校验为空**：`checkVaultKey` default PASS 分支只在非 Windows 查 mode bits（doctor.go:292-298），Windows 跳过——backlog #6：Windows 上 master.key 硬化校验只剩 32 字节长度。
8. **F1 教训（ACL 破损是真实发生过的类别）**：`TestOpen_DoesNotRewriteExistingStoreDBACL`（acl_windows_test.go:241）——serve（LocalSystem）重开用户建的 store.db 时 HardenACL 曾在服务令牌下重跑，SET_ACCESS 去重静默丢用户 ACE。现行 Open 已修（存在即跳过硬化），**但代价是：ACL 一旦被外部改坏，生产路径永不修复也无人知晓**——读侧检测（doctor）是唯一补口。
9. **`trusteeInACL` 不区分 allow/deny ACE**（acl_windows.go:164-182）：对 `Everyone:(deny)R` 这类收紧型 ACE 会误判「含 Everyone」。生产化时必须只统计 allow 类（否则真实误报类）。
10. **seedDoctorVault 直写不触发硬化**（doctor_test.go:234 用 `os.WriteFile` 0600）：Windows 上 t.TempDir() 的默认 DACL 是继承的宽 DACL——新检查上线后 CI windows lane 的现存断言（healthy vault → 0 WARN）会破，seed 必须改走 `FileKeyProvider.Set`。
11. **unlock 不重写既有 master.key**（unlock.go:45-48：文件存在且可读 → `fp.Get()` 直接打印返回；仅 ErrNotFound 才 `Generate + Set`）：「重跑 unlock 恢复硬化 ACL」不可行（已读不写），「删 key 再 unlock」更不可行（生成全新 key，vault 解不开——FINDING A 重演）。修复指引只能是手工 icacls。
12. **无真二进制 e2e 测试先例**：cli_smoke_test.go 为进程内 cobra 驱动（`NewRootCmd().Execute()`），全仓无「编译真二进制跑退出码」的测试形态。
13. **跨平台 stub 先例**：`acl_other.go` 的 `HardenACL` no-op（`!windows` build tag）——读 API 沿用同款双文件模式。
14. **实验实证（2026-08-26 探针，`.xcheck/20260826-101017/exp/inheritprobe/` 留底）**：① `GetNamedSecurityInfo` 读回的 DACL **包含继承 ACE**（宽父目录下子文件读回 6 ACE 全部 INHERITED_ACE 标志，含 Everyone:FullControl）——父目录宽 → 继承宽 ACE 直接出现在读回 DACL，SID 比对即可抓到；② `%TEMP%` 继承链实测含**另一用户账户的只读 ACE**（S-1-5-21-...-1003，mask 0x1200a9）——「非预期主体获读授权」不是理论暴露类，是本机活例。附注：x/sys `SECURITY_DESCRIPTOR.DACL()` 第二返回值语义（present vs defaulted）实测存疑（false 但 ACL 有效），实现信号 1 时须核实勿读错位。

## 1. `InspectFileACL` 读 API（internal/store）

写/读双子（对齐 `LoadOrCreateServeCert` / `ReadServeCertFingerprint` 先例）。「谁有权读」的判定**在 store**——与 HardenACL 写侧同文件，读写语义不漂移（白名单主体 = 写侧恰好授权的那三个）。

### 1.1 结构与签名

```go
// acl.go 放跨平台类型与方法（FileACLReport + TooLoose）；InspectFileACL 函数体在
// acl_windows.go / acl_other.go 各定义一份（Go 无分离声明，acl.go 不能只放函数签名）。
type FileACLReport struct {
    Supported               bool     // false = 非 Windows（mode bits 才是该平台的保护层）
    DaclNull                bool     // 无 DACL = 对所有人放行（信号 1）
    Protected               bool     // SE_DACL_PROTECTED：继承已断（辅助呈现，不独立触发，见 §1.2）
    UnexpectedReadGrantors  []string // 白名单外主体带读/提权能力 allow ACE 的 SID 字符串（信号 3）
}
func (r FileACLReport) TooLoose() bool {
    return r.Supported && (r.DaclNull || len(r.UnexpectedReadGrantors) > 0)
}

func InspectFileACL(path string) (FileACLReport, error)
```

### 1.2 判定语义（只查「过松」不查「过严」；白名单 read-capable）

**白名单主体**（= HardenACL 写侧恰好授权的三方，SID 精确比对，`CreateWellKnownSid`/`GetTokenUser` 构造，locale 无关）：
- SYSTEM（S-1-5-18）
- BUILTIN\Administrators（S-1-5-32-544）
- 当前用户（进程令牌 SID）

**信号集**：
1. **DACL null**：无 discretionary DACL = 全允许。DACL 存在但零 ACE = 过严（无人可读，不泄密）→ **不告警**。实现须核实 x/sys `DACL()` 返回位语义（事实 #14 附注）。
2. **继承未断（!Protected）**：**降级为 Detail 辅助**——现存暴露（继承下来的宽 ACE）已被信号 3 覆盖（继承 ACE 出现在读回 DACL 中，事实 #14①），独立 WARN 只剩「防父目录未来被改宽」的预警价值，拍板放弃（owner 2026-08-26）。纯 !Protected（继承自收紧父目录，如备份恢复的 master.key）→ PASS，不告警。命中 WARN（因信号 1/3）且 !Protected 时在 Detail 附注。
3. **白名单外主体带危险能力的 allow ACE**：walk DACL，对每个 allow 类 ACE（`ACCESS_ALLOWED_ACE_HEADER` 型；**deny 型不算**——`Everyone:(deny)R` 是收紧不是暴露，事实 #9 的修正），若 trustee **不在白名单**且 mask 含危险位 → 记入 `UnexpectedReadGrantors`（SID 字符串形态，不做 LookupAccountSid 本地化解析）。
   **危险位定义**：`mask & (FILE_READ_DATA | WRITE_DAC | WRITE_OWNER) != 0`——读数据能力，或可改 ACL/改 owner（可自我提权后放开再读）。实测存储态 ACE 的 GENERIC 位已展开（FullControl = 0x1f01ff，事实 #14），若实现期发现读回 mask 含未展开 GENERIC 位（GENERIC_READ/GENERIC_ALL），把这两个 generic 位并入危险位集（以 §4.1 矩阵 ⑤ 实证为准）。

**刻意不告警的两个方向（不对称原则）**：
- **过严**：白名单主体缺失、零 ACE——不泄密（缺 SYSTEM/Admins 只影响可恢复性，不暴露机密）。
- **白名单主体的超额权限**：当前用户 ACE 含 WRITE_DAC/WRITE_OWNER/全控——那是防篡改方向非机密性；owner 手动维修给自己全控是合法操作。Unix 对称面也只查 group/world read bits。

### 1.3 错误语义

`GetNamedSecurityInfo` / SD 解析失败 → 返回 error（含 ACCESS_DENIED 类）。硬化态下 user mask 含 `READ_CONTROL`（写侧 userMask，事实 #5），读不回 = ACL 被改到连 SD 都读不了 = 深度异常。error 文本以 `inspect ACL:` 前缀裹底层错误，不吞。

### 1.4 test-helper 三件生产化

`getDACLForTest` → `readDACL`（私有生产名）、`isDaclProtected` / `trusteeInACL` 移出 test-helper 注释块转正（名字本就中性）。`acl_windows_test.go` 全部调用点跟着改。**行为零变化，纯搬家+正名**——`InspectFileACL` 是它们之上的新生产入口（allow-ACE 过滤 + 白名单比对 + mask 过滤的新逻辑落在 InspectFileACL 内）。

### 1.5 非 Windows stub（acl_other.go）

```go
func InspectFileACL(path string) (FileACLReport, error) {
    return FileACLReport{Supported: false}, nil
}
```

对称 HardenACL no-op 先例（事实 #13）。**doctor 侧不再持有 `runtime.GOOS` 分支**——平台知识收敛进 API（见 §2），`checkVaultKey` 按 `Supported` 分流。

## 2. doctor 接线（checkVaultKey）

`default:` PASS 分支内重构（原 Unix mode-bit 检查与 Windows 空跳过统一为一个调用点）：

```go
rep, aerr := inspectFileACL(p)   // var seam = store.InspectFileACL，serveServiceState 先例
switch {
case aerr != nil:
    // FAIL：ACL 深度异常（读不回 SD），对称 checkVaultStore stat failed → FAIL
case !rep.Supported:
    // Unix：mode-bit 检查（现行逻辑原样搬入，0o077 → WARN）
case rep.TooLoose():
    // WARN：见 §2.1 冻结文案
default:
    // PASS：Detail 不变（不加 "ACL ok"，对称 Unix PASS 不写 mode）
}
```

- **seam**：`var inspectFileACL = store.InspectFileACL`（包级 var）——err→FAIL 分支的测试注入点（`stubServeServiceState` 先例；真实 SD 读失败种不出来，见 §4.2）。
- **判定顺序**：master.key 长度检查（现行 FAIL 分支）优先于 ACL 检查——文件本身坏是更高优先级信号。
- `runtime` 包依赖从该分支移除（doctor.go 其他处若无使用则 import 一并清）。

### 2.1 冻结文案

WARN Detail 模板（SIDs 为 `UnexpectedReadGrantors` 列表，逗号并列；命中时 !Protected 则追加括注）：

```
master.key present (32 bytes) but its DACL grants access to unexpected principals: <SIDs> — the plaintext key is protected by this ACL alone[ (inheritance also enabled)]
```

WARN Fix（v1 的 icacls 指引是**无效指引**——`/inheritance:r` 只移除继承 ACE，微软文档证实显式宽 ACE 保留。重写为「先移除、再断承、再重授」三步，SID 形式防本地化账户名，`/remove:g` 接受 `*S-1-...` 数字形式）：

```
remove the unexpected grantees listed above, then disable inheritance and re-grant only the hardened set — e.g. icacls <master.key> /remove:g <SIDs> /inheritance:r /grant:r "SYSTEM:(F)" /grant:r "Administrators:(F)" /grant:r "<user>:(RC,R,W,D)"
```

（unlock 路线已核实不可行，事实 #11；`/grant:r` 替换语义、`/remove` 显式 ACE 移除均为微软文档实证。）

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

// NewExitCodeError is the only sanctioned constructor: code >= 1 and err != nil
// are internal invariants — violations panic loudly (pinned by test) instead of
// silently producing a zero-code success or a nil-deref at print time.
func NewExitCodeError(code int, err error) *ExitCodeError {
    if code < 1 || err == nil {
        panic(fmt.Sprintf("NewExitCodeError: invalid code=%d err=%v", code, err))
    }
    return &ExitCodeError{Code: code, Err: err}
}

// ExitCodeFor maps a root-command error to the process exit code: an
// ExitCodeError pins its code, anything else is 1.
func ExitCodeFor(err error) int {
    if err == nil {
        return 0
    }
    var ec *ExitCodeError
    if errors.As(err, &ec) {
        return ec.Code
    }
    return 1
}
```

（v1 直构 `&ExitCodeError{...}` 有 Err==nil 时 `Error()` panic 且无校验的暗雷——评审指出后收敛为构造函数。）

### 3.2 接线三处

1. **runDoctor 尾部**（doctor.go:622-625 现行 `return fmt.Errorf("%w (%d) — see the report above", ...)` 改为）：
   ```go
   return NewExitCodeError(1, fmt.Errorf("%w (%d) — see the report above", errDoctorFindings, fail))
   ```
   外层带码、内层保持 `errors.Is(err, errDoctorFindings)` 可达——现有测试的 wrapped-findings 断言不破。
2. **main.go**：`os.Exit(1)` → `os.Exit(cli.ExitCodeFor(err))`（stderr 打印维持）。
3. **删除 doctorExitCode**（doctor.go:55-67）：双真相源根治（事实 #2）。其三态语义由新结构承载：0 = Execute 成功（main 不进错误分支）；1 = findings（ExitCodeError 显式）或任意其他命令错误（main 默认）；2 = 未来内部错误源 `NewExitCodeError(2, ...)`。

### 3.3 exit 2 的产生源边界

**本 plan 不给 doctor 新增运行时内部错误源**——管道接通 + 契约钉死，第一个真实 2 源是 #5 二期探活。2 的可达性在本 plan 由测试构造（§4.3），生产可达性登记为 #5 的前提。

### 3.4 用户可见契约：帮助文本**本 plan 不动**

v1 曾计划在 doctor Long 帮助文本补 "2 = internal error"——评审证实这会制造「承诺 2 但生产零可达」的矛盾窗口（真出未预期错误走 main 默认得 1）。拍板（owner 2026-08-26）：**帮助文本 exit 2 行随 #5 一起上**（其首个真实 2 源落地时同步补文档），本 plan 只接管道，契约钉在代码 + 测试层。**#5 的设计约束（本 spec 预埋）**：#5 新增的内部错误检查必须经 `NewExitCodeError(2, ...)` 包装返回，且其 plan 须包含帮助文本补行——两项都是 #5 的验收项，防「裸错误退化 exit 1」与「文档滞后」复发。

## 4. 测试矩阵

### 4.1 store 侧（acl_windows_test.go 增，`windows` build tag，全部真读回无 mock——种 DACL 先例 `TestOpen_DoesNotRewriteExistingStoreDBACL`）

| 测试 | 断言 |
|---|---|
| `TestInspectFileACL_Hardened` | `HardenACL` 后 `Supported && !TooLoose`、`Protected`、`UnexpectedReadGrantors` 空（白名单三主体不误报） |
| `TestInspectFileACL_TooLoose_Matrix` | ① 种 null DACL（`SetNamedSecurityInfo` 传 nil dacl，Windows 文档语义 = 无 DACL 全允许；x/sys 包装层不可达则降级为「显式 Everyone-allow + UNPROTECTED」，等效覆盖）→ `DaclNull && TooLoose`；② **宽父继承腿**（复刻事实 #14 实验：临时目录种宽可继承 DACL（Everyone-allow FullControl，`SUB_CONTAINERS_AND_OBJECTS_INHERIT`）→ 其下建子文件不硬化）→ 子文件 `UnexpectedReadGrantors` 含 Everyone SID、`TooLoose`；③ 显式种 Everyone-allow 读 ACE → 抓到；④ 显式种 BUILTIN\Users-allow 读 ACE → 抓到（非白名单任意 SID 代表）；⑤ 显式种 Everyone-allow **仅写** mask（WRITE_DATA，无 FILE_READ_DATA）→ **不**算危险（钉 mask 过滤）；⑥ 种 Everyone-**deny** 读 → 不算（allow-only 语义钉子） |
| 不存在路径 | 返回 error |

非 Windows：`acl_other_test.go`（`!windows` tag）断言 stub `Supported=false, err=nil` 且 `TooLoose()==false`（Supported 前置钉子）。

### 4.2 doctor 侧（doctor_test.go）

- **seedDoctorVault 修正**（事实 #10）：master.key 改走 `FileKeyProvider.Set`（触发 HardenACL），Windows lane 现存断言（healthy vault → `masterkey: PASS` / `overall: 0 WARN, 0 FAIL`）保持绿；Unix lane 同样不变——已核实 Set 的 Unix 产出 = `os.CreateTemp`（0600）+ rename（masterkey_file.go:64-81）+ `MkdirAll(dir, 0o700)`，文件 mode 0600 与现行 `os.WriteFile(0o600)` seed 等价，mode-bit WARN 不触发。
- **Windows-only 腿**（`runtime.GOOS == "windows"` guard，对齐 Case 3 的 Unix-only 模式）：
  - seed 后显式种 Everyone-allow 读 ACE → `masterkey: WARN` + §2.1 Detail 命中 + `overall: 1 WARN, 0 FAIL` + 退出码不受 WARN 影响；
  - （廉价顺带）种 null DACL → 同 WARN 通道。
- **err→FAIL 腿（seam 注入）**：stub `inspectFileACL` 返回 error → `masterkey: FAIL`（真实 SD 读失败在本机/CI 种不出来——hardened 态 user mask 含 READ_CONTROL——故走 `stubServeServiceState` 同款 var seam 钉分支，接线错误逃不过）。
- **非 Windows 腿**：`Supported=false` → mode-bit 老逻辑照旧（现有 Case 3 断言原样保留），`checkVaultKey` 无 `runtime.GOOS` 分支的结构由编译保证。

### 4.3 exit 侧（exit 测试 + TestDoctorExitCodes 扩展）

- `ExitCodeFor` 三态：`NewExitCodeError(2, err)` → 2；普通 error → 1；nil → 0（防御分支）。
- `NewExitCodeError` 违约 panic：`code=0` / `err=nil` 两腿均 panic（内部不变量显式炸，测试钉）。
- cobra 层注入：临时子命令 RunE 返回 `NewExitCodeError(2, err)` → `ExitCodeFor` 取 2（任意命令可传码的通路证明）。
- `TestDoctorExitCodes` state 3 重写（doctorExitCode 已删）：findings err → `errors.Is(err, errDoctorFindings)` 且 `ExitCodeFor(err)==1`。
- **无真二进制 e2e**（事实 #12 无先例，本 plan 不首创）：main.go 收敛为三行（打印 + `ExitCodeFor`），全部判定逻辑在已测函数内。

## 5. 文档联动

- `backlog.md`：P2 #6/#7 销项画线（留墓碑，编号稳定惯例）。
- `compat-matrix.md` 增两行：**doctor exit 2 管道已接线、契约已在代码/测试层定义（0/1 不变），生产触发待 #5 二期**（措辞注意：不是「新可达」——帮助文本与生产 2 源同步随 #5 上）；Windows master.key 新增 DACL-loose WARN 行。
- doctor Long 帮助文本：本 plan 不动（§3.4）。
- threat-model / concepts / agent-facing 文档不动：doctor 是 owner 侧工具，检测能力增强不改威胁面，agent 不可见。

## 6. Scope 边界（明确不做，scope 纪律留痕）

- 不检测 store.db / cache-dek.key / role.json / serve 证书 / WAL sidecars（backlog 后续条目；`InspectFileACL(path)` 已泛化，扩展零接口改动）。
- 不自动修复：doctor side-effect-free 铁律，修复是指引（fix 文案）不是动作。
- 不动 HardenACL 写侧。
- 不新增 doctor 运行时内部错误源（#5 二期的事，§3.3）。
- deny-ACE / 白名单主体超额权限 / 过严形态不告警（§1.2 不对称原则）。
- 不做真二进制 e2e 退出码测试形态首创（§4.3）。

## 7. 残余与风险登记

- **ACL 弥合时机**：doctor 只读不修（§6），ACL 破损的修复永远依赖 owner 手工操作——WARN 的 fix 文案是唯一引导路径。
- **白名单误报面（owner 已知情采纳）**：owner 出于自身运维目的手动加的第三方账户（备份服务、监控）会触发 WARN——那确实是「该账户可读明文密钥」的真实暴露告知，不是误报；若 owner 判定某账户可信，选择忽略该 WARN（WARN 不改退出码）。
- **icacls 指引的精确性**：fix 文案的 icacls 序列是 owner 手工操作的近似指引（`RC,R,W,D` 近似写侧 userMask），非逐位复刻——doctor fix 是提示不是脚本，接受近似。
- **exit 2 生产可达性为零（至 #5 前）**：契约钉在代码 + 测试 + compat-matrix 三层，帮助文本与生产 2 源同步随 #5 上（§3.4 含 #5 的两项预埋验收）。**评审提醒的退化风险**（未来内部错误裸返回 → 静默 exit 1）以 §3.4 的 #5 设计约束缓解，不在本 plan 加运行时防御。
- **`SECURITY_DESCRIPTOR.DACL()` 返回位语义**（事实 #14 附注）：实现信号 1 时核实 x/sys 的 present/defaulted 位，DACL null 与「存在但零 ACE」必须分清（前者全允许告警、后者过严不告警）。
- **GENERIC 位展开假设**：危险位定义假设存储态 mask 的 GENERIC 位已展开（实验佐证）；若 §4.1 矩阵 ⑤ 实证反例，把 GENERIC_READ/GENERIC_ALL 并入危险位集（§1.2 已预写）。
- **UnexpectedReadGrantors 呈现为 SID 字符串**：不做 LookupAccountSid 本地化解析（locale 无关性优先），owner 看到的是 `S-1-5-21-...` 形态而非账户名——精确但可读性差，接受（fix 用 `icacls /remove:g` 时正好需要 SID 形态）。
