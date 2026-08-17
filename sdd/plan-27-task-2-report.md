# Plan 27 Task 2 Report — vault 结构检查（store.db / master.key / 权限位）

**Branch:** `worktree-plan-27-doctor`
**Commits:** `bc98939` (feat, TDD main) + `856b650` (self-review wording fix)
**Files changed:** `internal/cli/doctor.go`, `internal/cli/doctor_test.go`（均在本 worktree：`C:\WorkSpace\agent\ssh-manager-mcp\.claude\worktrees\plan-27-doctor\internal\cli\`）

## What was implemented

Two new read-only checks appended to `doctorCheckFuncs`（挂在 T1 框架上）:

### `checkVaultStore` → row `store`
- `paths.StorePath()`（env-aware）解析路径；解析失败（vaultRoot 不可解析）→ FAIL。
- `os.Stat` 成功 → **PASS**，Detail 带大小：`store.db present (N bytes)`。
- 缺失 + role 属 vault 持有方（server/standalone）→ **FAIL**，Fix：`run `ssh-manager unlock` or the setup wizard`。
- 缺失 + role=client → **INFO**（cache-only 正常）。
- 缺失 + 无可用 role → **INFO**（fresh machine，role 行已覆盖）。
- Stat 其它错误（权限等）→ FAIL。
- **绝不 `store.Open`**（有建库+迁移副作用）；仅 Stat。

### `checkVaultKey` → row `masterkey`
- `paths.MasterKeyPath()`（env-aware）解析；失败 → FAIL。
- 读文件（`os.ReadFile`，无 Open）：
  - 缺失 + vault 持有方 role → **FAIL**（unlock/wizard）；缺失但 store.db 存在 → **FAIL**（`the vault cannot be decrypted`，restore from backup）；缺失 + client → INFO；缺失 + 无 role → INFO。
  - 长度非法（`!store.ValidMasterKeyLen`）→ **FAIL**，Detail：`master.key is N bytes, expected 32 — corrupt or wrong file`。
  - 合法 32B → **PASS**（Detail 带长度，无 secret 值）。
  - Unix（`runtime.GOOS != "windows"` 守卫，非 build tag——单一测试文件）`Perm()&0o077 != 0` → **WARN**，Detail 含 `group/world readable` + 八进制 mode；Fix `chmod 600`。Windows 上跳过（ACL 是保护层，mode 位无意义）。
- 文案参照 `vaultStatusString`（serve_service.go:676）语义但为 doctor 自有 remediation——未调用它（其硬编码 FileKeyProvider + LOCKED 措辞）。

辅助：`vaultHoldingRole(role)`、`doctorRole()`（Load 错误映射为"无可用 role"，role 检查负责报告损坏）。每检查自包含，各自 `roles.Load()`。

## TDD evidence

### RED（先写测试，`go test ./internal/cli/ -count=1 -run TestDoctorVaultStructural`）

```
--- FAIL: TestDoctorVaultStructural (0.05s)
    doctor_test.go:248: missing "store:  PASS" in:
        ssh-manager doctor (dev)
        env:  INFO  SSHMGR_* env overrides in effect: ...
        role:  PASS  role=server setup_complete=true
        overall: 0 WARN, 0 FAIL
FAIL
```

测试先落盘（仅断言行为、不引用新符号，可编译→行为性 RED）。

### 实现后首次全包运行（中间态，预期内的 T1 fixture 破坏）

`TestDoctorRoleStates` FAIL：其 fixture 是"server role + 无 vault"，新语义下 store+masterkey 正确 FAIL（`overall: 1 WARN, 2 FAIL`）——**绑定语义如此**（missing + server/standalone → FAIL，与 SetupComplete 无关），T1 fixture 需补种 vault。修法：`TestDoctorRoleStates` 开头 `seedDoctorVault(t, vd)`，隔离 role 行（server 机器无 store.db 的 FAIL 场景由 `TestDoctorVaultStructural` case 4 专门测试）。

### GREEN（`go test ./internal/cli/ -count=1`，两次提交后各跑一次）

```
ok  	ssh-manager-mcp/internal/cli	8.193s
```

`gofmt -l internal/cli` 无输出；`go vet ./internal/cli/` 干净；`go build ./...` 通过。

### 测试内容（`TestDoctorVaultStructural`，比 brief 的 3 case 多一个矩阵 case）

1. **健康 vault**（真 store.db + 32B key + server role complete）→ `store:  PASS`（带大小）、`masterkey:  PASS`（`(32 bytes)`）、`overall: 0 WARN, 0 FAIL`。
2. **17 字节 key** → `masterkey:  FAIL` + `master.key is 17 bytes, expected 32` + fix 行 + `overall: 0 WARN, 1 FAIL` + err 为 `errDoctorFindings`；store 行仍 PASS。
3. **Unix 权限 0644**（`runtime.GOOS == "windows"` 时 `t.Log` 跳过）→ `masterkey:  WARN` + `group/world readable` + `overall: 1 WARN, 0 FAIL` + exit 0。
4. **缺失矩阵**：server role + 无任何 vault 文件 → 两行 FAIL（`unlock` 出现）+ `overall: 0 WARN, 2 FAIL`；client role + 无文件 → 两行 INFO + exit 0。

fixture `seedDoctorVault`（doctor_test.go）＝ seedClearVault 同款：测试内 `store.Open` 建真库合法；doctor 代码本体零写入。

## Self-review

- 语义逐条对绑定文本核过：store/masterkey 全分支、FIX/INFO 条件、"expected 32"、runtime.GOOS 守卫（非 build tag）、不调 vaultStatusString。
- 无副作用：doctor.go 仅 Stat/ReadFile/Load；无 store.Open、无写、无网络。
- 无 secret 泄漏：Detail 仅含大小/长度/role/mode 位，无 key 值、无路径值（env 行仍只报名字）。
- 命名沿 T1 先例（checkVaultStore/checkVaultKey）；`doctorCheckFuncs` 注释更新（T2 已落地，T3/T4 待挂）。
- 无新依赖（仅 stdlib `io/fs`、`runtime` + internal paths/store）。
- 发现并修复一处自审问题（856b650）：`doctorRole()` 把损坏 role.json 与缺失同归 nil 分支，原 INFO 文案 "no role.json on this machine" 在损坏机器上与 role 行矛盾 → 改为 "no usable role"（两种情况皆准确，测试不依赖该串）。

## Concerns

1. **Unix 权限 WARN 分支在本机（Windows dev）未被真执行**——测试 case 3 在 Windows 跳过（`t.Log`），需 Unix CI/Linux 跑一次才算闭环（repo CI 基线在 backlog，见 memory）。代码路径已编译覆盖（runtime 守卫非 build tag）。
2. `TestDoctorRoleStates` fixture 变更（补种 vault）是 T1 测试对新全表现实的必要适配——已在本报告与 commit message 说明；若 T3/T4 再加行，同类"overall 计数"断言可能需再适配（T1 的计数断言粒度较脆，可考虑未来改为按行断言）。
3. `checkVaultKey` 缺失分支里 role 判定优先于 store.db 存在性——server role + store.db 同时在的 Detail 只提 role，两因同现时不重复报 store（可接受，一行诊断）。
