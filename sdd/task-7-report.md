# Task 7 Report — `ssh-manager clear`

**Status: COMPLETE**（含一次测试事故 + 修复，见「事故」节 — 必读）。

## What was built

### `internal/cli/clear.go` (new)
- `enumClearTargets(role) []string` / 内部 `scanClearTargets` — 按实存枚举（每个候选 os.Stat），类别前缀 `vault:/serve:/client:/task:/role:`：
  - vault 目录（`storePath()` 的 dir，SSHMGR_STORE-aware，与 roles.vaultRolePath 同一解析）：store.db / -wal / -shm / serve.log / cache-dek.key（vault-dir 版 + `paths.CacheDekPath()` 版，去重）；master.key.plain 走 `paths.MasterKeyPath()`（SSHMGR_FILEKEY_PATH-aware）；serve-cert/key/marker 先 env（SSHMGR_SERVE_CERT/KEY/MARKER）再 vault-dir 相对。
  - client 目录（`clientops.CachePaths()` dir）：cache.bin / cache.auth.json / cache.meta.json / cache-audit.log。
  - role.json 两个位置（`roles.RolePath(server)` + `(client)`）。
  - 非文件标记行：serve 服务安装态（`serveInstalledFn`）、遗留计划任务（`legacyTimerPresentFn`）。
  - role 参数按契约保留但**不**门控扫描（server 机可能有 client 残留、反之亦然——clear 要抓所有残留）；role 由调用方用于头行与安全网决策。
- `makeSafetyNet() (path, passphrase, err)` — `openUnlockedStore`（失败→「请先 ssh-manager unlock（clear 不提供无备份删除）」）→ `ExportSnapshot` → json → `store.GenerateToken()` 口令 → `vaultio.Encrypt` → `~/ssh-manager-backup-<UTC时间戳>.sme`（unique-temp+rename、Sync）→ **回读校验**（ReadFile → Decrypt → json.Unmarshal `store.Snapshot`）。store 在返回前 Close（Windows 文件锁会让后续 os.Remove 失败）。任何失败 = 零改动错误。
- `clearResolveRole()` — `roles.ResolveMode("")` 优先；**出错时回退文件系统探测**（vault 存在→server、否则 client）。原因：role.json 损坏/非法正是 clear 存在的意义，clear 不能被它要修的坏状态 dead-end。
- `newClearCmd()` 交互时序（spec §3 v2 逐字对齐）：非 TTY 拒绝（「clear 需要交互式终端」）→ 角色头行 + 实存清单 → 「输入 DELETE 确认：」（非 DELETE → 已取消，exit 0，零改动）→ vault 角色：makeSafetyNet（失败中止零改动）→ 打印口令 + 「⚠ 此口令仅显示一次」→「按 y 确认已抄录口令」（非 y → 已取消，exit 0，零改动）→ 幂等执行：`serveUninstallFn`（失败→提权重跑指引中止）→ 逐文件 os.Remove（ENOENT 容忍，其它错误中止+重跑指引）→ `deleteLegacyTimerFn`（失败=warning 不中止）→ `roles.Delete` → 「已清理。下次 ssh-manager tui 将重新进入首次向导。」
- 服务「已装？」探测选型：`svc.Status()` err==nil（ErrNotInstalled/无服务管理器/探测错误→false）。**探测只驱动清单标记行；执行阶段无条件调用幂等的 `uninstallServeService`**——探测假阴性绝不能留下一个指向已删文件的开机自启服务（crash-loop）。「not installed」结果是打印 no-op，无害。已选型并注释在 `serveServiceInstalled`。
- 测试注入 seam（均包级 var，生产默认=真实实现）：`clearStdinIsTTY`（含 NUL-device 误报 caveat 注释，与 tui.isTTY 同弱點、行为一致）、`serveInstalledFn`、`serveUninstallFn`、`deleteLegacyTimerFn`、`legacyTimerPresentFn`、`safetyNetHomeDir`。

### `internal/cli/clear_timer_windows.go` / `clear_timer_other.go` (new)
Windows：`schtasks /End`（忽略错误）→ `/Delete /F`；「任务不存在」= 成功（en「cannot find」+ zh「找不到」双 locale 匹配，其它 locale 降级为 warning，非致命）。`legacyTimerPresent` = `/Query` 成功与否。Unix：双 no-op（遗留刷新是 Windows schtask；用户自建 unit 永不触碰，spec §4.2）。

### `internal/paths/paths.go` (modified — 事故修复)
`CacheDekPath()` 增加 `SSHMGR_CACHE_DEK` env 覆盖（与 SSHMGR_FILEKEY_PATH 同款 seam）。原因见下节事故。

### `internal/cli/root.go` (modified)
注册 `newClearCmd()`。

## ⚠ 事故：一次测试运行删除了开发机真实的 cache-dek.key（已修复 + 可恢复）

**事实**：初版 `scanClearTargets` 除 vault-dir 相对路径外还枚举 `paths.CacheDekPath()`（belt-and-braces 覆盖 SSHMGR_STORE 迁移机），但该路径当时**无 env 覆盖**、恒指向 `C:\ProgramData\ssh-manager\cache-dek.key`。`TestClear_ServerFullFlow` 的 teardown 循环删除所有枚举文件 → 真实 DEK 被删（目录 mtime 佐证；文件确认消失）。**没有任何测试触碰真实 vault/store.db/凭据**——serve-cert/key 走了 env 分支，AppData 被 t.Setenv 隔离。

**修复**：`CacheDekPath` 增加 `SSHMGR_CACHE_DEK` seam；`withClearDirs` 钉死之。`TestEnumClearTargets_EmptyMachine` 正是抓住泄漏的测试（它 FAIL 在真实文件被枚举）。

**恢复**：本机（client 角色）运行 `ssh-manager cache pull` 即可——`loadOrCreateDEK` 会在 DEK 缺失时重新生成并落盘，cache.bin 随之用新 DEK 重写；cache.auth.json（设备码+pin）未被触碰，在线重拉一步完成。**运维需执行这一步。**

## Tests (`internal/cli/clear_test.go`) — TDD red→green
1. `TestEnumClearTargets_ServerMachine` — brief 原文场景：wal/cert/marker/master.key + 同机 client 残留 + 双 role.json 全枚举、前缀齐、未实存（shm/serve-key/cache.bin/…）绝不出现。
2. `TestEnumClearTargets_EmptyMachine` — 空机零行（即抓住事故泄漏的测试）。
3. `TestMakeSafetyNet` — 路径名形状 + 口令非空 + Decrypt+Unmarshal 回读通过。
4. `TestClear_CancelZeroMutation` — 输入 "nope" → 已取消 exit 0，全部 seed 文件（含 role.json、client 残留）仍在，且无安全网文件写出。
5. `TestClear_YConfirmAbortsAfterSafetyNet` — 钉死 v2 时序：口令展示先于 y 确认；n → 零改动但安全网已存在。
6. `TestClear_ServerFullFlow` — DELETE+y：安全网恰好 1 份且可用输出中解析出的口令解开；服务卸载+timer 均被调用；8 个文件全删；⚠/已清理/首次向导文案齐。
7. `TestClear_IdempotentRerun` — client 角色全流程（无安全网），预删 cache.meta.json 后仍全绿，其余（含 vault-dir 的 cache-dek.key、client role.json）全删，timer fn 被调。
8. `TestClear_NonTTYRefused` — 拒绝文案 + 文件仍在。
9. `TestClear_LockedVaultRefuses` — 锁定 vault → unlock 指引中止、零改动、无备份。
10. `TestClear_CorruptRoleJSONStillClears` — role.json 损坏（clear 的存在意义）仍走通：回退探测→安全网→全清。

## Verification
`go build ./... && go vet ./... && go test ./... -count=1` 全绿；gofmt 无输出；`GOOS=linux`/`GOOS=darwin` 交叉编译通过（build-tag 文件两面都编）。

## Notes / deviations
- cache-dek.key 位置：任务指令把它列在 user dir，实际它按 `paths.CacheDekPath()` 活在 vault dir（ProgramData）。枚举按**真实路径**处理，前缀用语义类别 `client:`。
- spec 示例里的「（N 台服务器的全部凭据）」计数行未实现——计数需开库（锁定时拿不到），清单+安全网已承载该信息。
- timer 删除失败=warning 不中止（brief 未定失败语义；遗留清理不应卡住主 teardown，且失败信息带手动 schtasks 指引）。
