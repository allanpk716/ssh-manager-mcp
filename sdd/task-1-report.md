# Task 1 Report — `internal/roles` 包（role.json + 判定链）

**Status: COMPLETE** — commit `5a39e31` on `feat/plan19-role-wizard`.

## What was built

### `internal/roles/roles.go` (new)
- `type Role string` + `RoleStandalone`/`RoleServer`/`RoleClient`；`State{Role, SetupComplete}`（JSON: `{"role":...,"setup_complete":bool}`，Save 恒写两字段）。
- `RolePath(r)` — standalone/server → vault 目录 role.json；client → `os.UserConfigDir()/ssh-manager/role.json`（与 cache 数据同居）。
- `Load()` — vault 位置优先→用户目录；两处皆无 → `(nil,nil)`；非法 JSON / 非法 role 值 → 错误，文案引导 `ssh-manager clear`。
- `Save(s)` — 校验 role → MkdirAll 0700 → unique-temp+rename 原子写（本地复制了 clientops 的 ~20 行 `atomicWriteUnique`，含 Windows rename 重试；注释注明复制来源——roles 不引 clientops 未导出符号，也不为此扩大 clientops API 面）→ `store.HardenACL`。
- `Delete()` — 两处都删，幂等（IsNotExist 忽略）。
- `ResolveMode(force)` — 唯一启动判定：force 护栏（`client`+本机有 vault → 错误「本机已有 vault…`ssh-manager clear` 将删除本机全部 vault 数据…」；非法 force 值报错）→ role.json 分支（异常矩阵：非法值/缺 vault → 引导 clear；client 缺缓存 = 正常 LaunchClient；`ResumeSetup=!SetupComplete`）→ 无 role.json 探测（locked vault fail-closed 文案含 unlock，绝不降级 client → unlocked vault → LaunchBroker，serve-cert.pem 文件启发式定 server|standalone → cache → LaunchClient → 全空 LaunchWizard）。
- `VaultExists`/`VaultUnlocked` + 私有 `vaultStorePath` — 从 tui/mode.go **原样迁移**（stat-first，绝不触发 OpenStore 的建库副作用；v0.6.0 行为逐字保留）。

### `internal/roles/roles_test.go` (new)
Brief 测试逐字落地（已删指定的 2 行 no-op），外加 3 处必要的测试助手修正（见下）。4 个测试全绿：`TestLoad_Empty`、`TestSaveLoad_ClientRoundTrip`、`TestResolve_FullMatrix`（三态+locked fail-closed+serve-cert 启发式+force 护栏）、`TestResolve_RoleFileAnomalies`（非法值/缺 vault/resume）。

### `internal/tui/mode.go` (modified)
`vaultExists`/`vaultUnlocked` 改为一行转发到 `roles.VaultExists`/`roles.VaultUnlocked`；本地 `vaultStorePath` 及探测实现删除（逻辑已迁 roles）。`DetectMode`/`DetectModeWith`/`Run`/`cachePresent` 公开形状不变 → `cli/tui.go` 无需改动、mode_test.go 原样全绿（探测测试现在穿过转发层，钉住 stat-first 行为不回归）。roles 不 import tui（无环）。

## Deviations from the brief（均为 brief 自身缺陷，不修无法通过任何正确实现）

1. **删除了指定 no-op**：`TestSaveLoad_ClientRoundTrip` 中 `os.Stat(filepath.Join(t.TempDir()))` 2 行（binding resolution 明示）。
2. **`withDirs` 增加环境钉死**：brief 版不设 `SSHMGR_FILEKEY_PATH`、不清 `SSHMGR_MASTERKEY_HEX`/`SSHMGR_CACHE_DIR`/`SSHMGR_SERVE_CERT` → 在有真实 vault 的开发机上 `VaultUnlocked` 会读到 `C:\ProgramData\ssh-manager\master.key.plain`（碰巧通过），在干净机器上则误判 locked（失败）。现钉 `SSHMGR_FILEKEY_PATH=vaultDir/master.key.plain` 并清空其余三个 env，测试完全封闭。
3. **`seedVault` 先删旧 store.db 再建库 + 写 master key 文件**：brief 版 (a) 不写 key 文件 → VaultUnlocked 在干净机器上永远 false；(b) locked 子测试残留的 "x" store.db 会让后续 `store.Open` 报 "file is not a database"。
4. **force 护栏子测试 seed `vd2` 而非 `vd`**：cache 子测试的第二次 `withDirs` 已把 `SSHMGR_STORE` 重钉到 vd2，brief 原文 seed vd 后 `VaultExists()` 恒 false → 护栏永不触发。

## Implementation notes for downstream tasks

- **vault 目录 role.json 定位**：`vaultRolePath()` 取 `dir(vaultStorePath())/role.json`（SSHMGR_STORE 钉死时随 store.db 一起搬家）。生产默认无 env 时 = `paths.VaultDir()/role.json`，与 spec §3.1 一致；这样测试才能用 SSHMGR_STORE 重定位。**若 Task 2+ 直接用 `paths.VaultDir()` 拼 role.json 会与此不一致——请走 `roles.RolePath`。**
- **serve-cert 启发式**：`SSHMGR_SERVE_CERT` env 优先，否则 `dir(storePath)/serve-cert.pem`（生产默认 = `paths.ServeCertPath()` 默认目录）。已在注释中说明是文件探测而非 svc.Status，及误判后果（仅影响无 role.json 时的标签，向导写 role.json 后以文件为准）。
- **locked-vault fail-closed 仅在无 role.json 的探测分支**（spec §1.2 判定顺序逐字）；role.json=standalone/server + locked vault → LaunchBroker，解锁失败由后续 broker 启动路径报错（与 tui.Run 现行为一致）。
- **`ResolveMode` force 只接受 `""`/`"client"`**：brief 只定义了 client 护栏。Task 2 重接 cli/tui.go 时若需 broker force 需回来扩这里。

## Verification

- TDD: 先跑测试确认编译失败（符号未定义）→ 实现 → 绿。
- `go build ./...` ✓ `go vet ./...` ✓
- `go test ./internal/roles/ ./internal/tui/ ./internal/cli/ -count=1` → 全 ok（roles 0.25s / tui 1.19s / cli 5.67s）。
- mode_test.go 未改动一行，tui 探测测试经转发层继续钉住 stat-first 行为。

## Concerns

- tui 的 `Mode`/`DetectMode` 与 `roles.Launch` 并存是**过渡态**（Task 2 负责把 cli/tui.go 改调 `roles.ResolveMode` 后收掉）——本任务按约束保持 cli/tui.go 原样编译。
- `store.Open` 不校验 master key（解密时才用）——`VaultUnlocked` 的语义是「能解析出一把 key 且库能打开」，不是「key 正确」。这是 v0.6.0 迁移行为的既有语义，未改。
