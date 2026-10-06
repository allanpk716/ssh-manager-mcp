# Plan 50 — WAL 边车 DACL 平价（sidecar ACL parity）+ 报错链上 + unlock 平台语法 + doctor 盲区行

日期：2026-10-06 ｜ 状态：已实施（本 worktree），待合并/发版（owner 门）

## 事故记录（NUC10，2026-10-02..06，全程证据在案）

**症状**：NUC10 上普通（非管理员提权）PowerShell 窗口运行 `sshmgr tui` 报
「本机 vault 存在但锁定或不可读：先运行 `sshmgr unlock`」；运行 `unlock`
「成功」打印 `export SSHMGR_MASTERKEY_HEX=<hex>` 后 tui 依旧报同样错误。
serve 服务、`sshmgr doctor`（全绿）、`sshmgr unlock` 各自正常。

**证据链**（逐环独立可查，2026-10-06 经 ssh-manager-mcp 的 SSH 工具面在 NUC10 实测）：

1. 报错文案全仓唯一出处 `internal/roles/roles.go`（无 role.json 的探测分支）；NUC10 的 role.json 在 vault 目录与用户配置目录**两处都不存在**（历史状态，探测分支即其正常运行路径，非缺陷）。
2. `master.key.plain` 存在（32 字节，2026-08-13 创建后未再改动）、ACL 含 `NUC10\allan716:(R,W,D)`、内容与 unlock 打印值完全一致（比对 MATCH，明文未二次输出）——钥匙层无问题。
3. `store.Open` 不校验钥匙内容（凭据惰性解密），故探测失败只能是 `resolveMasterKey` 或 `store.Open` 出错；钥匙已排除 → SQLite 打开失败。
4. `store.db-wal` / `store.db-shm` 的 ACL = `SYSTEM:(F)` + `Administrators:(F)` + `SYSTEM:(R,W,D)`——**无 allan716 条目**（对比 store.db 本体有）。UAC 过滤令牌下普通窗口用不了 Administrators 条目 → 对边车零权限。WAL 模式要求**每个**连接可写 -shm（共享内存索引）→ 打开失败。
5. 时间线：开机 2026-10-02 11:12:27 → `store.db-shm` 诞生 11:13:23。serve 服务（LocalSystem，自启）抢在交互进程前打开库并以 SYSTEM 身份创建了边车。
6. 「一切正常」的旁证各自成立：serve 以 SYSTEM 跑（边车就是它建的）；doctor 的 vault-open 是**拷贝到临时目录再开**的探针（绕开现场边车——这正是检查面盲区）；unlock 只读 master.key.plain 不碰 store.db；诊断用的 SSH 会话持完整管理员令牌（Windows OpenSSH 不过 UAC 滤镜，Administrators 条目可用）。

**代码根因**：Plan 16 F2 的修复 `hardenWALSidecars` → `HardenACL` 按**当前进程身份**重建边车 ACL。serve 场景下"当前用户"= SYSTEM，交互用户永远进不去；反之亦然。F2 只覆盖了同身份场景，没覆盖「服务身份创建、用户身份打开」——而重启竞态必然产生这个方向。

**次要发现（同事故暴露，本 plan 一并修）**：
- `unlock` 打印 POSIX `export` 语法，PowerShell 里是死文本（用户照跑无效）。
- 探测分支报错把底层错误吞掉，把权限类失败误指为「先运行 unlock」。
- 修复当天 NUC10 上的运维解堵（icacls 补 `NUC10\allan716:(R,W,D)` 两条）已即时生效；本 plan 是代码层根治 + 防复发。

## 四件套

### ① 边车 DACL 平价（根治）

- `internal/store/acl_windows.go` 新增 `MirrorDACL(src, dst)`：把 store.db 的 DACL 原样复制到边车（PROTECTED + DACL 写；属主不动）。store.db 的 DACL 在创建时冻结、此后永不改写（Plan 16 F1 纪律），是本机权限的**规范集合**，天然同时包含双方主体。
- `hardenWALSidecars` 由「HardenACL 按当前身份重建」改为「MirrorDACL 与 store.db 平价」。身份无关 → 双向闭死。
- 时序补口（关键）：Open 时边车往往还不存在（已存在的库上 Open 不发生写入，边车要到第一次写入才诞生——NUC10 实测开机后约 1 分钟）。新增 `Store.EnsureSidecarACL()`：先落一条真实的 serve-start 审计写入（在受控时刻、以当前进程身份确定性地让边车诞生），随即镜像。两个 serve 入口（`serve.go` 前台路径、`serve_service.go` 服务路径）在开库后调用。**开机即出生即平价**，竞态窗口关闭。
- 残余边界（登记不修）：进程存活期间边车被 SQLite 重建（需 serve 的唯一连接先干净关闭再重开，常态不发生）→ 由 ④ 的 doctor 行兜底发现 + 下次 serve 启动自愈。非 Windows 部署形态为单身份（serve 与命令行同用户），MirrorDACL 为无操作。
- Plan 16 F1 纪律不动：store.db 本体仍只在创建时硬化一次，本 plan 不碰它。

### ② 报错链上（误指修复）

- `roles.VaultUnlocked()` 拆出错误保留核 `roles.VaultOpenErr()`；探测分支报错改为 `fmt.Errorf("...：%w — ...", oerr)` 链上底层原因，并补「权限或数据库打开失败则运行 `sshmgr doctor`」指引。
- `internal/tui/wizardsteps.go` 同文案同修。

### ③ unlock/lock 平台语法

- `masterKeyEnvLine` / `masterKeyUnsetLine`：Windows 打印 PowerShell 语法（`$env:SSHMGR_MASTERKEY_HEX = '<hex>'` / `Remove-Item Env:...`），其余平台维持 `export` / `unset`。按构建平台选择（发布二进制本就分平台），不探测父 shell。

### ④ doctor 盲区行

- 新检查行 `store-sidecar`：对每个存在的边车比对「store.db 的可写主体集合 ⊆ 边车可写主体集合」，缺口 = FAIL（明细列缺失的 SID，Fix 给提权 icacls 修复命令与升级指引）；边车不存在 = PASS（干净检查点状态）；非 Windows = INFO 跳过（模式位是那边的保护层）。
- 实现于 `store.SidecarACLGap`（Windows 走真实 DACL 遍历，走 `isWalkedAllowAceType` 同一套 ACE 类型过滤；其余平台返回 `ErrACLParityUnsupported`）。
- 本次事故里 doctor 全绿而真机不可用，即此行要堵的盲区。

## 测试

- `TestHardenWALSidecars_MirrorsStoreDBDACL`：store.db 种入**外来 SID**（Everyone 全控）作标记——任何按当前身份推导的 ACL 都不可能产生它；断言镜像后边车与 store.db 的 DACL 逐字节一致（SDDL 相等）。身份无关性的直接证明。
- `TestHardenWALSidecars_NoStoreDBIsNoOp`：无 store.db 时边车原样不动（不再从当前身份推导）。
- `TestSidecarACLGap`：NUC10 形态（store.db 有用户+外来主体、边车只有 SYSTEM+Administrators）→ 缺口恰为缺失两主体；Mirror 后缺口为空。
- `TestEnsureSidecarACL_MaterializesAndMirrors`：**已存在的库**重开（建表无写入、边车缺席——即 NUC10 开机形态）→ EnsureSidecarACL 后两边车在场且与 store.db 平价、缺口为空。开机竞态闭环的等价复现。
- `TestResolveMode_LockedVaultErrorChainsCause`（roles）：锁定库的探测报错必须链上 `vault locked: run \`sshmgr unlock\`` 原文并含 doctor 指引。
- `TestDoctorStoreSidecarACL`（doctor，seam 驱动四分支）、`TestMasterKeyEnvLineMatchesPlatform` / `TestUnlockPassphraseFallbackDerivesKey`（平台语法）。
- 既有测试订正：`TestHardenWALSidecars`（旧 F2 语义）按平价语义重写；`TestHardenWALSidecars_NoOpOnFreshStore` 注释订正（干净 Close 会删边车——平价前提「重开时边车缺席」正建立于此实测）。

## 部署与验证门（owner）

1. 持续集成两平台绿。
2. 合并 master → 发版（版本号 owner 定）→ NUC10 自更新 → **重启 serve**（EnsureSidecarACL 在启动时生效）。
3. 真机验证：NUC10 普通窗口 `sshmgr tui` 直接过；`sshmgr doctor` 出现 `store-sidecar: PASS` 两行（-shm/-wal 与 store.db 平价）；重启复验不复发。
4. 已解堵的 icacls 补丁条目与镜像结果兼容（同款 userMask），无需回滚。
