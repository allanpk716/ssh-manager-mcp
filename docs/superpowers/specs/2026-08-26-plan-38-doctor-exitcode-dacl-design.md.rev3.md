# Plan 38 设计：doctor 硬化接线——exit 2 接线（backlog P2 #7）+ Windows DACL readback（backlog P2 #6）

> backlog P2 首批（doctor 系三连的前两个；#5 serve 探活二期是下一个独立 plan，依赖本 plan 的 #7 接线）。2026-08-26 brainstorming 已拍板、本文不重议的决策：**DACL 过松 → WARN（对称 Unix mode 0644→WARN 先例，钉死于 TestDoctorVaultStructural）**、**检测范围只 master.key（API 按 path 泛化，store.db/cache-dek.key/WAL 留 backlog 后续）**、**方案 A：store 深模块读 API（判定语义与 HardenACL 写侧同居）+ cli 侧 ExitCodeError 接口约定**；评审闭环中追加拍板：**白名单 read-capable 判定语义**、**信号 2（继承未断）降级 Detail 辅助**、**exit 2 帮助文本随 #5 上**、**rev3 为 owner 特批第三轮修订（超 close-flow 硬上限），由 owner 人工终审替代第三轮外审**。
>
> 本版 = **rev3**（owner 特批修订轮）。相对 rev2 的关键变更：**WARN Detail/Fix 按触发信号分段渲染**（rev2 单一模板在 DaclNull-only / owner-only 形态下渲染空 SID 列表、fix 命令语法无效——两轮复审两家独立命中的同族缺陷）；**icacls 指引全面 SID 化**（"SYSTEM"/"Administrators" 账户名在非英文 Windows 本地化，icacls 按名解析失败）；**doctor Windows-only 测试腿拆 `doctor_windows_test.go`**（runtime guard 不解决编译隔离）；**信号 4 措辞保守化**（OWNER_RIGHTS 可限制 owner 隐式权——定位为「属主异常的保守告警」）；**信号 3 排除 INHERIT_ONLY_ACE + deny 覆盖场景明示保守误报立场**；**ExitCodeError 的 Error() nil 防御 + ExitCodeFor 上界**（防 os.Exit 低 8 位截断）；null-DACL 探针结果的**回填承诺**；default 分支文件存在前提的显式补注。

## 0. 目标与代码现状事实（全部 2026-08-26 于 0c8d28d 核实；#14-#15 为同日实验实证）

1. **exit 1 硬编码**：`cmd/ssh-manager/main.go:11-14` 对所有 cobra 错误统一 `os.Exit(1)`。
2. **doctorExitCode 零生产调用者**：三态映射（nil→0 / errDoctorFindings（wrapped 含）→1 / 其他→2）定义于 `internal/cli/doctor.go:58`，仅被 `TestDoctorExitCodes`（doctor_test.go:110-121）钉住——正是 backlog #7 所述「保留码静默烂掉」的双真相源结构。
3. **runDoctor 只产生 nil / errDoctorFindings**（doctor.go:603-626）：无任何运行时路径产生「doctor 内部错误」，exit 2 当前不可达。
4. **doctor 帮助文本只承诺 0/1**（doctor.go:637-638 Long 文本），未提 2（本 plan 维持不动，见 §3.4）。
5. **写侧 HardenACL 完备且接线面广**（全部走 `windows.SetNamedSecurityInfo`，纯 Go 无 icacls 外部依赖，`acl_windows.go:41`）：`FileKeyProvider.Set`（masterkey_file.go:92，master.key）、`store.Open` 首建（store.go:108，store.db）、`hardenWALSidecars`（store.go:151，-wal/-shm）、`roles.Save`（roles.go:175，role.json）、clientops（clientops.go:253，cache-dek.key 侧）、serve 证书三件 + marker（mcpserver/cert.go:122/254/257）、serve 服务目录（serve_service.go:570）。写侧产出的 DACL 形态：SYSTEM+Administrators 全控、当前用户 `READ_CONTROL|DELETE|FILE_GENERIC_READ|FILE_GENERIC_WRITE`（**刻意不给 WRITE_DAC/WRITE_OWNER**）、断继承（SE_DACL_PROTECTED）、无 BUILTIN\Users / Authenticated Users / Everyone。**HardenACL 不改 owner**（SetNamedSecurityInfo 的 owner 参数传 nil）——但写侧路径创建文件时 owner 默认 = 创建进程用户，硬化态 owner 即当前用户（白名单内）。
6. **读侧三件困在 test-helper 块**：`getDACLForTest` / `isDaclProtected` / `trusteeInACL`（acl_windows.go:137-182）——注释自述 "Test helpers (only compiled under the windows build tag)"，生产命名缺失是 backlog #6 字面所述的封锁点。
7. **doctor 的 Windows 保护层校验为空**：`checkVaultKey` default PASS 分支只在非 Windows 查 mode bits（doctor.go:292-298），Windows 跳过——backlog #6：Windows 上 master.key 硬化校验只剩 32 字节长度。
8. **F1 教训（ACL 破损是真实发生过的类别）**：`TestOpen_DoesNotRewriteExistingStoreDBACL`（acl_windows_test.go:241）——serve（LocalSystem）重开用户建的 store.db 时 HardenACL 曾在服务令牌下重跑，SET_ACCESS 去重静默丢用户 ACE。现行 Open 已修（存在即跳过硬化），**但代价是：ACL 一旦被外部改坏，生产路径永不修复也无人知晓**——读侧检测（doctor）是唯一补口。
9. **`trusteeInACL` 不区分 allow/deny ACE**（acl_windows.go:164-182）：对 `Everyone:(deny)R` 这类收紧型 ACE 会误判「含 Everyone」。生产化时必须只统计 allow 类（否则真实误报类）。
10. **seedDoctorVault 直写不触发硬化**（doctor_test.go:234 用 `os.WriteFile` 0600）：Windows 上 t.TempDir() 的默认 DACL 是继承的宽 DACL——新检查上线后 CI windows lane 的现存断言（healthy vault → 0 WARN）会破，seed 必须改走 `FileKeyProvider.Set`。
11. **unlock 不重写既有 master.key**（unlock.go:45-48：文件存在且可读 → `fp.Get()` 直接打印返回；仅 ErrNotFound 才 `Generate + Set`）：「重跑 unlock 恢复硬化 ACL」不可行（已读不写），「删 key 再 unlock」更不可行（生成全新 key，vault 解不开——FINDING A 重演）。修复指引只能是手工 icacls。
12. **无真二进制 e2e 测试先例**：cli_smoke_test.go 为进程内 cobra 驱动（`NewRootCmd().Execute()`），全仓无「编译真二进制跑退出码」的测试形态。
13. **跨平台 stub 先例**：`acl_other.go` 的 `HardenACL` no-op（`!windows` build tag）——读 API 沿用同款双文件模式。
14. **实验实证一（2026-08-26 探针，`.xcheck/20260826-101017/exp/inheritprobe/` 留底）**：① `GetNamedSecurityInfo` 读回的 DACL **包含继承 ACE**（宽父目录下子文件读回 6 ACE 全部 INHERITED_ACE 标志，含 Everyone:FullControl）——父目录宽 → 继承宽 ACE 直接出现在读回 DACL，SID 比对即可抓到；② `%TEMP%` 继承链实测含**另一用户账户的只读 ACE**（S-1-5-21-...-1003，mask 0x1200a9）——「非预期主体获读授权」不是理论暴露类，是本机活例。附注：x/sys `SECURITY_DESCRIPTOR.DACL()` 第二返回值语义（present vs defaulted）实测存疑（false 但 ACL 有效），实现信号 1 时须核实勿读错位。
15. **实验实证二（2026-08-26 探针，`.xcheck/20260826-105712/exp/genericbit/` 留底）**：存入 raw `GENERIC_ALL`（0x80000000）的 ACE，读回 mask = **0x00120089（已展开为 specific rights，含 FILE_READ_DATA）**——存储态 mask 的 GENERIC 位恒展开，读回判定用 specific-rights 位检查即可，危险位集无需并入 GENERIC 位。

## 1. `InspectFileACL` 读 API（internal/store）

写/读双子（对齐 `LoadOrCreateServeCert` / `ReadServeCertFingerprint` 先例）。「谁有权读」的判定**在 store**——与 HardenACL 写侧同文件，读写语义不漂移（白名单主体 = 写侧恰好授权的那三个）。

### 1.1 结构与签名

```go
// acl.go 放跨平台类型与方法（FileACLReport + TooLoose）；InspectFileACL 函数体在
// acl_windows.go / acl_other.go 各定义一份（Go 无分离声明，acl.go 不能只放函数签名）。
type FileACLReport struct {
    Supported              bool     // false = 非 Windows（mode bits 才是该平台的保护层）
    DaclNull               bool     // 无 DACL = 对所有人放行（信号 1）
    Protected              bool     // SE_DACL_PROTECTED：继承已断（辅助呈现，不独立触发，见 §1.2）
    UnexpectedReadGrantors []string // 白名单外主体带危险位 allow ACE 的 SID 字符串，按 SID 去重 + 升序（信号 3）
    OwnerSID               string   // 文件 owner 的 SID 字符串（呈现用）
    OwnerUnexpected        bool     // owner ∉ 白名单（信号 4；属主异常的保守告警，见 §1.2）
}
func (r FileACLReport) TooLoose() bool {
    return r.Supported && (r.DaclNull || r.OwnerUnexpected || len(r.UnexpectedReadGrantors) > 0)
}

func InspectFileACL(path string) (FileACLReport, error)
```

（`UnexpectedReadGrantors` 去 + 升序：同一 SID 多条 allow ACE——如一显式一继承——只出现一次，且 Detail 输出确定。）

### 1.2 判定语义（只查「过松」不查「过严」；白名单 + owner；保守告警方向）

**白名单主体**（= HardenACL 写侧恰好授权的三方，SID 精确比对，`CreateWellKnownSid`/`GetTokenUser` 构造，locale 无关）：
- SYSTEM（S-1-5-18）
- BUILTIN\Administrators（S-1-5-32-544）
- 当前用户（**进程令牌** SID——语义边界见 §7 残差「属主 vs 运行者」）

**信号集**：
1. **DACL null**：无 discretionary DACL = 全允许。DACL 存在但零 ACE = 过严（无人可读，不泄密）→ **不告警**。实现须核实 x/sys `DACL()` 返回位语义（事实 #14 附注）。
2. **继承未断（!Protected）**：**降级为 Detail 辅助**——现存暴露（继承下来的宽 ACE）已被信号 3/4 覆盖（继承 ACE 出现在读回 DACL 中，事实 #14①），独立 WARN 只剩「防父目录未来被改宽」的预警价值，拍板放弃（owner 2026-08-26）。纯 !Protected（继承自收紧父目录，如备份恢复的 master.key）→ PASS，不告警。命中 WARN（因信号 1/3/4）且 !Protected 时在 Detail 附注。
3. **白名单外主体带危险位的 allow ACE**：walk DACL，对每个 **allow 类且非 inherit-only** 的 ACE（`ACCESS_ALLOWED_ACE_HEADER` 型；**deny 型不算**——`Everyone:(deny)R` 是收紧不是暴露，事实 #9 的修正；**`INHERIT_ONLY_ACE` 标志的 ACE 不算**——该标志的 ACE 只作用于子对象，对文件本体不生效），若 trustee **不在白名单**且 mask 含危险位 → 记入 `UnexpectedReadGrantors`（SID 字符串形态，不做 LookupAccountSid 本地化解析）。
   **危险位定义**：`mask & (FILE_READ_DATA | WRITE_DAC | WRITE_OWNER) != 0`——读数据能力，或可改 ACL/改 owner（可自我提权后放开再读）。读回 mask 的 GENERIC 位恒已展开（事实 #15 实证），无需并入 GENERIC_READ/GENERIC_ALL；矩阵⑨钉此回归。
   **deny 覆盖场景的立场**：本设计**不解析有效授权**（不模拟 Windows ACL 评估顺序做 deny 抵消）——allow 即告警，选择保守误报方向（fail-safe：漏报的代价是密钥暴露，误报的代价是 owner 多看一眼 WARN）。
4. **owner 非白名单（保守告警）**：Windows 对象 owner **普遍**隐式持有 WRITE_DAC（微软文档《Owner of a New Object》："An object's owner implicitly has WRITE_DAC access to the object"）——非白名单 owner 通常能改写 DACL 给自己授权后读明文。**保守化措辞**（OWNER_RIGHTS 特殊 trustee 的 ACE 可以限制 owner 的隐式权限，故「恒可提权」非绝对）：信号 4 定位为**「属主异常的保守告警」**——owner 偏离白名单即告警，不解析 OWNER_RIGHTS 语义、不宣称绝对提权能力。`GetNamedSecurityInfo` 加请求 `OWNER_SECURITY_INFORMATION`，读回 owner SID 比对白名单；非白名单 → `OwnerUnexpected`（Detail 呈现 `OwnerSID`）。写侧路径不改 owner（事实 #5），owner = 创建者用户，硬化态恒白名单内；owner 异常即文件被外部重属主。

**刻意不告警的两个方向（不对称原则）**：
- **过严**：白名单主体缺失、零 ACE——不泄密（缺 SYSTEM/Admins 只影响可恢复性，不暴露机密）。
- **白名单主体的超额权限**：当前用户 ACE 含 WRITE_DAC/WRITE_OWNER/全控、owner = 当前用户——那是防篡改方向非机密性；owner 手动维修给自己全控是合法操作。Unix 对称面也只查 group/world read bits。

### 1.3 错误语义

`GetNamedSecurityInfo` / SD 解析失败 → 返回 error（含 ACCESS_DENIED 类）。硬化态下 user mask 含 `READ_CONTROL`（写侧 userMask，事实 #5），读不回 = ACL 被改到连 SD 都读不了 = 深度异常。error 文本以 `inspect ACL:` 前缀裹底层错误，不吞。

### 1.4 test-helper 三件生产化

`getDACLForTest` → `readDACL`（私有生产名）、`isDaclProtected` / `trusteeInACL` 移出 test-helper 注释块转正（名字本就中性）。`acl_windows_test.go` 全部调用点跟着改。**行为零变化，纯搬家+正名**——`InspectFileACL` 是它们之上的新生产入口（allow-ACE 过滤 + inherit-only 过滤 + 白名单比对 + mask 过滤 + owner 判定的新逻辑落在 InspectFileACL 内）。

### 1.5 非 Windows stub（acl_other.go）

```go
func InspectFileACL(path string) (FileACLReport, error) {
    return FileACLReport{Supported: false}, nil
}
```

对称 HardenACL no-op 先例（事实 #13）。**doctor 侧不再持有 `runtime.GOOS` 分支**——平台知识收敛进 API（见 §2），`checkVaultKey` 按 `Supported` 分流。

## 2. doctor 接线（checkVaultKey）

`default:` PASS 分支内重构（原 Unix mode-bit 检查与 Windows 空跳过统一为一个调用点）。**前提注**：default 分支到达时 master.key 必已存在且 ReadFile 已成功——现行 doctor.go 分支序（`fs.ErrNotExist` → unreadable → 长度 FAIL → default）保证，`InspectFileACL(p)` 不存在「文件中途消失」输入。

```go
rep, aerr := inspectFileACL(p)   // var seam = store.InspectFileACL，serveServiceState 先例
switch {
case aerr != nil:
    // FAIL：ACL 深度异常（读不回 SD），对称 checkVaultStore stat failed → FAIL
case !rep.Supported:
    // Unix：mode-bit 检查（现行逻辑原样搬入，0o077 → WARN）
case rep.TooLoose():
    // WARN：见 §2.1 分段文案
default:
    // PASS：Detail 不变（不加 "ACL ok"，对称 Unix PASS 不写 mode）
}
```

- **seam**：`var inspectFileACL = store.InspectFileACL`（包级 var）——err→FAIL 分支的测试注入点（`stubServeServiceState` 先例；真实 SD 读失败种不出来，见 §4.2）。
- **判定顺序**：master.key 长度检查（现行 FAIL 分支）优先于 ACL 检查——文件本身坏是更高优先级信号。
- `runtime` 包依赖从该分支移除（doctor.go 其他处若无使用则 import 一并清）。

### 2.1 冻结文案（**按触发信号分段渲染**）

Detail 与 Fix 均为**分段子句表**：按命中的信号拼接主句（信号 1/3/4 各一句，多信号命中则分号并列），!Protected 命中时追加括注。不再有「单模板 + 空列表」形态。

**Detail 子句表**（`<SIDs>` = UnexpectedReadGrantors 列表；`<OwnerSID>` = OwnerSID）：

```
[1 DaclNull]        master.key present (32 bytes) but it has no DACL — every principal is allowed
[3 grantors]        master.key present (32 bytes) but its DACL grants access to unexpected principals: <SIDs>
[4 owner]           master.key present (32 bytes) but the file owner is <OwnerSID> — the owner can typically rewrite the DACL
[!Protected 附注]   (inheritance also enabled)
[公共尾句]          — the plaintext key is protected by this ACL alone
```

**Fix 子句表**（icacls 全星号 SID 形态——内置账户名在非英文 Windows 本地化，按名解析不可靠；`<you-SID>` = 运行者自己的 SID，`whoami /user` 可查）：

```
[1 DaclNull]        icacls <master.key> /grant:r *S-1-5-18:(F) *S-1-5-32-544:(F) *<you-SID>:(RC,R,W,D)
[3 grantors]        icacls <master.key> /inheritance:r /remove:g <SIDs...> /grant:r *S-1-5-18:(F) *S-1-5-32-544:(F) *<you-SID>:(RC,R,W,D)
[4 owner]           icacls <master.key> /setowner *S-1-5-32-544
[公共尾句]          — replace <SIDs...> with the principals listed above (asterisk-prefixed SID form, e.g. *S-1-1-0) and <you-SID> with your own SID (`whoami /user`)
```

组合规则：命中信号 3 时先断承（/inheritance:r 清继承宽 ACE——继承 ACE 不能直接 /remove）再移除显式宽（/remove:g）；信号 1 无需移除（本无 DACL，直接重授）；信号 4 单独触发时只归位 owner；多信号时按 4→3→1 顺序拼接 icacls 参数段（一条命令完成：/setowner *S-1-5-32-544 /inheritance:r /remove:g ... /grant:r ...）。**§4 测试矩阵钉每种单发形态的 Detail/Fix 渲染断言。**

（unlock 路线已核实不可行，事实 #11；`/grant:r` 替换语义、`/remove` 显式 ACE 移除、继承 ACE 不可直接 remove、icacls 接受 `*S-1-...` 形态均为微软文档/实证确认。）

## 3. exit 2 接线（internal/cli/exit.go 新文件）

### 3.1 类型与映射

```go
// ExitCodeError lets a RunE pin the process exit code that main will honor;
// every other error keeps the generic 1.
type ExitCodeError struct {
    Code int
    Err  error
}
func (e *ExitCodeError) Error() string {
    if e.Err == nil {
        // hand-rolled literal that bypassed NewExitCodeError — never nil-deref
        // at print time; the code alone is still meaningful for the error path.
        return fmt.Sprintf("ssh-manager: exit code %d", e.Code)
    }
    return e.Err.Error()
}
func (e *ExitCodeError) Unwrap() error { return e.Err }

// NewExitCodeError is the sanctioned constructor: code in [1,125] and err != nil
// are internal invariants — violations panic loudly (pinned by test) instead of
// silently producing a zero-code success or a nil-deref at print time. (125:
// exit codes are truncated to the low 8 bits by the OS; >125 risks colliding
// with shell-reserved codes like 126/127.)
func NewExitCodeError(code int, err error) *ExitCodeError {
    if code < 1 || code > 125 || err == nil {
        panic(fmt.Sprintf("NewExitCodeError: invalid code=%d err=%v", code, err))
    }
    return &ExitCodeError{Code: code, Err: err}
}

// ExitCodeFor maps a root-command error to the process exit code: an
// ExitCodeError pins its code, anything else is 1. A hand-rolled literal that
// bypassed NewExitCodeError with a nonsense code (<1 or >125) falls back to 1
// rather than exiting 0 on an error path or being truncated by the OS.
func ExitCodeFor(err error) int {
    if err == nil {
        return 0
    }
    var ec *ExitCodeError
    if errors.As(err, &ec) {
        if ec.Code < 1 || ec.Code > 125 {
            return 1
        }
        return ec.Code
    }
    return 1
}
```

（字段导出供 errors.As 与测试断言；构造器校验 + `Error()` nil 防御 + `ExitCodeFor` 上下界兜底三重防线——绕过构造器的字面量最多退化为 1，不会「错误但 exit 0」、不会 print 时 panic、不会被 OS 截断成意外码。）

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

「承诺 2 但生产零可达」的矛盾窗口由帮助文本与生产 2 源同步上线消除。拍板（owner 2026-08-26）：帮助文本 exit 2 行**随 #5 一起上**，本 plan 只接管道，契约钉在代码 + 测试层。**#5 的设计约束（本 spec 预埋）**：#5 新增的内部错误检查必须经 `NewExitCodeError(2, ...)` 包装返回，且其 plan 须包含帮助文本补行——两项都是 #5 的验收项，防「裸错误退化 exit 1」与「文档滞后」复发。

## 4. 测试矩阵

### 4.1 store 侧（acl_windows_test.go 增，`windows` build tag，全部真读回无 mock——种 DACL 先例 `TestOpen_DoesNotRewriteExistingStoreDBACL`）

| 测试 | 断言 |
|---|---|
| `TestInspectFileACL_Hardened` | `HardenACL` 后 `Supported && !TooLoose`、`Protected`、`UnexpectedReadGrantors` 空、`OwnerUnexpected=false`（白名单三主体与创建者 owner 不误报） |
| `TestInspectFileACL_TooLoose_Matrix` | ① **null DACL**（实现期 TDD 首步 = 先跑种法探针：`SetNamedSecurityInfo` 传 nil dacl；**可行**则强制断言 `DaclNull && TooLoose`；**不可行**则降级腿「显式 Everyone-allow + UNPROTECTED」只覆盖信号 3，信号 1 的 DaclNull 判定分支以解析层构造单测钉，§7 登记残差——不宣称等效覆盖；**探针结果无论可行与否都回填本 spec §4.1 或 compat-matrix，不留悬空**。**✅ 探针已回填（2026-08-26 实测，Plan 38 T1）：可种**——种入成功，读回形态 = `SE_DACL_PRESENT=1` + DACL 指针 null（null DACL 形，非 present 位清零形），走强制腿；**判定口径**：DaclNull ⇔ `Control()&SE_DACL_PRESENT` 位清零，或该位置位而 DACL 指针 null——两形皆「无 discretionary 保护 = 全允许」）；② **宽父继承腿**（复刻事实 #14 实验）→ 子文件 `UnexpectedReadGrantors` 含 Everyone SID、`TooLoose`；③ 显式种 Everyone-allow 读 ACE → 抓到；④ 显式种 BUILTIN\Users-allow 读 ACE → 抓到；⑤ 显式种 Everyone-allow **仅写** mask（WRITE_DATA）→ 不算（钉 mask 过滤）；⑥ 种 Everyone-**deny** 读 → 不算（allow-only 钉子）；⑦ Everyone-allow **仅 WRITE_DAC** → 抓到（提权位正向）；⑧ Everyone-allow **仅 WRITE_OWNER** → 抓到（提权位正向）；⑨ 种 raw `GENERIC_ALL`(0x80000000) → 读回已展开含 FILE_READ_DATA → 抓到（钉事实 #15 回归）；⑩ Everyone-allow 读 + **INHERIT_ONLY_ACE** 标志 → 不算（文件本体不生效钉子） |
| `TestInspectFileACL_Owner` | 种白名单外 owner（`OWNER_SECURITY_INFORMATION`，owner 设为 Everyone SID）→ `OwnerUnexpected && TooLoose` 且 `OwnerSID` 呈现；owner 归位 Administrators → 不触发 |
| 不存在路径 | 返回 error |

非 Windows：`acl_other_test.go`（`!windows` tag）断言 stub `Supported=false, err=nil` 且 `TooLoose()==false`（Supported 前置钉子）。

### 4.2 doctor 侧（**两个文件**——编译隔离）

- **`doctor_test.go`（跨平台单文件维持）**：
  - **seedDoctorVault 修正**（事实 #10）：master.key 改走 `FileKeyProvider.Set`（触发 HardenACL；Set 本身跨平台，Windows 硬化 / Unix mode 0600——masterkey_file.go:64-81 CreateTemp+rename 已核实），Windows lane 现存断言保持绿，Unix lane mode-bit 语义不变。
  - **err→FAIL 腿（seam 注入，跨平台可跑）**：stub `inspectFileACL` 返回 error → `masterkey: FAIL`（`stubServeServiceState` 同款 var seam）。
  - **非 Windows 腿**：`Supported=false` → mode-bit 老逻辑照旧（现有 Case 3 断言原样保留）；`checkVaultKey` 无 `runtime.GOOS` 分支的结构由编译保证。
- **`doctor_windows_test.go`（新文件，`//go:build windows`）**——Windows-only 腿需要种 DACL（`SetNamedSecurityInfo` 等 Windows API），runtime guard 不解决编译隔离，必须 build-tag 文件（acl_windows_test.go 先例）：
  - seed 后显式种 Everyone-allow 读 ACE → `masterkey: WARN` + **信号 3 子句** Detail/Fix 渲染断言（含星号 SID 形态 fix）+ `overall: 1 WARN, 0 FAIL` + 退出码不受 WARN 影响；
  - 种 owner 异常（白名单外 owner）→ WARN 含**信号 4 子句**（owner-SID 呈现、fix 含 /setowner）；
  - 种 null DACL（若 §4.1 ① 探针证实可行）→ WARN 含**信号 1 子句**（"no DACL" 措辞、fix 无 /remove 段）；
  - （组合腿，廉价）信号 3+4 同发 → 分号并列、参数段拼接正确。

### 4.3 exit 侧（exit 测试 + TestDoctorExitCodes 扩展）

- `ExitCodeFor` 五态：`NewExitCodeError(2, err)` → 2；普通 error → 1；nil → 0（防御分支）；`&ExitCodeError{Code: 0, Err: errors.New("x")}`（字面量绕过）→ 1（下界兜底钉子）；`&ExitCodeError{Code: 999, Err: errors.New("x")}` → 1（上界兜底钉子）。
- `NewExitCodeError` 违约 panic：`code=0` / `code=999` / `err=nil` 三腿均 panic（内部不变量显式炸，测试钉）。
- `(&ExitCodeError{Code: 1, Err: nil}).Error()` 不 panic（nil 防御钉子）。
- cobra 层注入：临时子命令 RunE 返回 `NewExitCodeError(2, err)` → `ExitCodeFor` 取 2（任意命令可传码的通路证明）。
- `TestDoctorExitCodes` state 3 重写（doctorExitCode 已删）：findings err → `errors.Is(err, errDoctorFindings)` 且 `ExitCodeFor(err)==1`。
- **无真二进制 e2e**（事实 #12 无先例，本 plan 不首创）：main.go 收敛为三行（打印 + `ExitCodeFor`），全部判定逻辑在已测函数内。

## 5. 文档联动

- `backlog.md`：P2 #6/#7 销项画线（留墓碑，编号稳定惯例）。
- `compat-matrix.md` 增两行：**doctor exit 2 管道已接线、契约已在代码/测试层定义（0/1 不变），生产触发待 #5 二期**；Windows master.key 新增 DACL-loose WARN 行（owner 异常 / 非白名单读授权 / null DACL）。
- **null-DACL 探针结果回填**：§4.1 ① 的实现期探针结论（可行/不可行）回填本 spec 或 compat-matrix，不留悬空。
- doctor Long 帮助文本：本 plan 不动（§3.4）。
- threat-model / concepts / agent-facing 文档不动：doctor 是 owner 侧工具，检测能力增强不改威胁面，agent 不可见。

## 6. Scope 边界（明确不做，scope 纪律留痕）

- 不检测 store.db / cache-dek.key / role.json / serve 证书 / WAL sidecars（backlog 后续条目；`InspectFileACL(path)` 已泛化，扩展零接口改动）。
- 不自动修复：doctor side-effect-free 铁律，修复是指引（fix 文案）不是动作。
- 不动 HardenACL 写侧（owner 检测是读侧判定，不反向要求写侧设 owner）。
- 不新增 doctor 运行时内部错误源（#5 二期的事，§3.3）。
- deny-ACE / 白名单主体超额权限 / 过严形态不告警（§1.2 不对称原则）。
- 不解析有效授权（deny 抵消模拟）与 OWNER_RIGHTS 语义——保守告警方向（§1.2 立场）。
- 不做真二进制 e2e 退出码测试形态首创（§4.3）。

## 7. 残余与风险登记

- **ACL 弥合时机**：doctor 只读不修（§6），ACL 破损的修复永远依赖 owner 手工操作——WARN 的 fix 文案是唯一引导路径。
- **白名单误报面（owner 已知情采纳）**：owner 出于自身运维目的手动加的第三方账户（备份服务、监控）会触发 WARN——那确实是「该账户可读明文密钥」的真实暴露告知，不是误报；若 owner 判定某账户可信，选择忽略该 WARN（WARN 不改退出码）。
- **保守误报的两处显式选择**：① allow-ACE 即告警，不解析 deny 抵消（§1.2 立场）——被 deny 完全覆盖的宽 allow ACE 会告警，属 fail-safe 方向；② owner 非白名单即告警，不解析 OWNER_RIGHTS 限制——owner 实际无提权能力时也会告警（保守告警）。两处都是「漏报代价 >> 误报代价」的取舍，登记接受。
- **属主 vs 运行者**：白名单的「当前用户」= **运行 doctor 的进程令牌 SID**，非文件属主。跨账户运行 doctor（如另一管理员账户）时，真属主的 ACE/owner 会被点名、fix 照做会把真属主锁在 vault 外——doctor 是 owner 侧工具、通常属主自跑；若必须跨账户跑，以属主身份跑或人工核对点名名单。登记接受。
- **icacls 指引的精确性**：fix 文案的 icacls 序列是 owner 手工操作的近似指引（`RC,R,W,D` 近似写侧 userMask），非逐位复刻——doctor fix 是提示不是脚本，接受近似。
- **exit 2 生产可达性为零（至 #5 前）**：契约钉在代码 + 测试 + compat-matrix 三层，帮助文本与生产 2 源同步随 #5 上（§3.4 含 #5 的两项预埋验收）。评审提醒的退化风险（未来内部错误裸返回 → 静默 exit 1）以 §3.4 的 #5 设计约束缓解，不在本 plan 加运行时防御。
- **`SECURITY_DESCRIPTOR.DACL()` 返回位语义**（事实 #14 附注）：~~实现信号 1 时核实 x/sys 的 present/defaulted 位~~ **已核实（2026-08-26 探针实测，Plan 38 T1）**：第二返回位在有效硬化 DACL（AceCount=3、SE_DACL_PRESENT=1）与 null DACL（SE_DACL_PRESENT=1 + 指针 null）两态下均实测 false——与 x/sys 命名一致，是 `SE_DACL_DEFAULTED` 位，**非 present 位**。DACL null 与「存在但零 ACE」的区分实现为：`SE_DACL_PRESENT` 位清零、或 present 位置位而指针 null → `DaclNull`（全允许，告警）；present + 非 nil 且零 ACE → 过严，不告警。DACL() 第二返回值不参与判定。
- **null-DACL 强制测试未决**（§4.1 ①）：nil-dacl 种法探针留作实现期 TDD 首步；若证实不可种，信号 1 的覆盖降级为解析层构造单测 + 代码审查，登记不宣称端到端；探针结论按 §5 回填，不留悬空。**✅ 已决（2026-08-26 探针，Plan 38 T1）：可种**（读回 = present 位 1 + 指针 null 形），矩阵①走强制腿，本条销项。
- **UnexpectedReadGrantors 呈现为 SID 字符串**：不做 LookupAccountSid 本地化解析（locale 无关性优先），owner 看到的是 `S-1-5-21-...` 形态而非账户名——精确但可读性差，接受（fix 用 `icacls /remove:g` 时正好需要 `*S-1-...` SID 形态）。
