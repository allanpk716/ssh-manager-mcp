# Plan 13 — NAS 定时备份（明文快照）— Design Spec

**Date:** 2026-08-12
**Status:** Design — pending implementation plan
**Worktree/branch:** `plan-13-nas-backup`

> 服务器的**可用性兜底**：把整个 vault 定时备份到挂载的群晖目录。明文 JSON 快照、
> 无变化不备份、按份数轮转、防挂载掉了静默写本地。灾难恢复 = 从 NAS 拷文件 +
> `ssh-manager import`。

## 1. 问题

服务器是 vault 的单点权威（serve 模式常驻）。目前**没有任何自动备份**——vault 丢了就丢了（除非手动 `export`）。需要一个自动化、保数据、可校验、可轮转的备份机制，灾难发生时能从 NAS 恢复。

## 2. 目标

服务器定时把整个 vault 以**明文 JSON 快照**形式写到挂载的群晖目录；vault 无变化时不产新文件；按份数轮转；防"挂载掉了静默写本地"；灾难时从 NAS 拷文件 + 现有 `import` 恢复。

## 3. 关键决策（两轮 xcheck 评审 + 用户拍板）

### 3.1 砍掉加密，存明文 JSON（本 plan 与最初设想的最大分歧，经评审验证成立）

**不加密备份文件，存 `store.Snapshot` 的明文 JSON。** 理由（用户论证，两轮三家异构评审认可）：

1. **职责分离**：SSH 凭据本来就不只在 vault 里——用户在 1Password/其他密码管理器早有副本（那是专业加密凭据库）。**备份的职责是可用性（防丢），机密性由密码管理器负责。** 不在备份里重复加密职责。
2. **避免复杂度根源**：加密 + skip-相同内容 两个需求打架（`vaultio.Encrypt` 随机 salt+nonce → 相同明文每次密文不同 → skip 必须解密比对或存明文 hash → 孵出"口令轮换→零可恢复备份"灾难 bug、明文 hash 指纹、口令离线存活等一堆问题）。砍加密让这些**全部消失**。
3. **NAS 受信**：群晖在 VLAN 内、外网不可达、永不开 Cloud Sync/快照复制/公网（用户确认，作为**部署硬约束**）。
4. **避免单点赌注**：不把所有信任压在 vault 一个程序上。

> **不是 regression**——之前根本没有备份功能。明文备份是"修不动的加密备份 < 一个正确的明文备份"的取舍（"备份你不能恢复的，等于没有备份"）。

### 3.2 明文备份的威胁模型（诚实，必写进文档）

明文文件 = **凭据明文 + 审计日志（可能含 agent 执行过的命令行，历史里或带过 API key/临时密码）+ 服务器拓扑 + profiles + host keys**。这些**不全在 1Password 里**（尤其审计日志）。所以明文备份暴露的范围比"1Password 冗余副本"更广。

**安全性的成立条件（三者同时）**：NAS 受信 ∧ server 受信 ∧ 挂载路径受信。

**适用**：受信 VLAN、无公网同步、目录权限锁死、物理介质单独保管。
**不适用**：开了 Cloud Sync / Drive / Universal Search / Snapshot Replication / 公网共享、多人共用 NAS、备份会拷到不受信介质。**（部署硬约束，违反则必须重新启用加密——见 §10 未来工作。）**

### 3.3 复用 Plan 11，零新加密逻辑

- `ExportSnapshot()`（Plan 11）——整个 vault 的 DTO。
- `ImportSnapshot()`（Plan 11）——恢复路径。
- 不碰 `vaultio`（加密包）。

### 3.4 Windows 一等公民 + 加固降级（用户 Q3，部署目标 = Windows）

核心备份功能全平台（写、skip、轮转、锁文件基础、marker、ORDER BY）。**加固项按平台分级**：

| 加固项 | Linux | Windows |
|---|---|---|
| temp 文件 0600 | ✅（有意义） | `0600` 位被忽略；靠 NTFS ACL（不程序校验，文档说明）|
| `--dir` 0700 校验 | ✅ 校验 `os.Stat` mode | **跳过**（Windows 无 Unix 权限位）；改文档强调"靠文件夹 ACL 隔离" |
| 文件 fsync（`f.Sync()`） | ✅ | ✅（`FlushFileBuffers`） |
| **目录 fsync（`dir.Sync()`）** | ✅ | **不支持**（Windows 无 sync 目录语义；跳过，不报错）|
| 锁回收 pid 判活 | `os.FindProcess` + signal 0 | **`OpenProcess`**（`os.FindProcess` 在 Windows 总成功不验活）|
| 定时器文档 | systemd timer | **任务计划程序（Task Scheduler）** |
| 挂载 NAS | fstab SMB mount | `net use` 映射网络驱动器 |
| `.git` 路径检测 | 跨平台 `filepath.Walk` 向上找 `.git` | 同（无问题） |

### 3.5 无变化不备份（skip-if-unchanged）

算当前明文 JSON 的 SHA256，比最新一份备份边车的 `file_sha256`。相同则 skip（不产新文件）。

### 3.6 依赖 marker 文件防"挂载掉了静默写本地"

`--dir/.ssh-manager-backup-marker` 必须存在（只查存在性），否则 fail-closed。文档钉死"**先挂载，后建 marker，marker 必须在挂载的 NAS 上**"（防先建 marker 再 mount 导致 marker 落 shadow、挂载掉时 shadow marker 露出 → fail-open）。

### 3.7 轮转 + UTC 时间戳

文件名 `vault-<UTC时间戳>.json`（`YYYYMMDD-HHMMSS` UTC，避 DST 撞名）；轮转按**文件名字典序**（= 时间序，免疫 NAS/rsync/同步重写 mtime）。留前 `--keep` 份，删其余（边车跟着删，忽略 not-found）。`--keep 0` = 禁用轮转（help 明确）。marker/锁永不动。

### 3.8 锁文件 + 陈旧锁回收（pi 的 blocker 修复，跨平台）

`O_EXCL` 建 `.ssh-manager-backup.lock`。**`O_EXCL` 锁不会进程退出时自动释放**——任何崩溃（SIGKILL/OOM/panic/Ctrl-C）留孤儿锁 → 后续撞锁 → 静默停摆。锁文件写 `<pid> <host> <start-ts>`，撞锁时判：pid 不活（跨平台判活）/ 超时窗口（默认 30 min，= 预期最长备份时长 2×）→ 窃取陈旧锁重建；否则真并发，exit 0 skip。**不用 flock**（SMB 上不可靠）。

### 3.9 边车保留（codex+pi 共识：明文丢了 GCM 免费 bit-rot 防线）

`.sha256` 边车是**唯一**抓"合法 JSON 但字节已坏"（base64/字符串单字节翻转仍合法 JSON → 静默还原错误凭据）的机制。`json.Unmarshal` 抓不到这类。边车字段：`file_sha256=<hex>`（+ `size=<n>` 可选）。砍掉原计划的 `plaintext_sha256`（明文了不需要）。

## 4. 非目标（v1 不做）

- **不挂载/不传输**（运维层 SMB/NFS/net use）。
- **不加密备份文件**（§3.1，核心决策）。
- **不做 `backup restore`**（= 手动拷文件 + 现有 `import`）。
- **不增量**（全量快照；vault 通常 KB 级）。
- **不事件触发**（纯定时，写路径零改动）。
- **CLI 改动后不提醒备份**。
- **cache_tokens 不纳入 Snapshot**（见 §11）。
- **不自动校验历史备份**（`verify` 按需；不塞进 `create` 怕拖慢定时）。

## 5. 设计

### 5.1 命令

- `ssh-manager backup create --dir <挂载点> [--keep 7] [--prefix vault]`
- `ssh-manager backup verify <file>`
- 不做 `backup restore`。

### 5.2 `backup create` 流程

1. **marker 检测**：`--dir/.ssh-manager-backup-marker` 必须存在（只查存在性），否则 fail-closed 报错退出。
2. **`.git` 路径护栏**（用户 Q3 同意）：从 `--dir` 向上查祖先目录是否含 `.git`；命中 → fail-closed（防明文误提交 git）。跨平台 `filepath` 实现。
3. **文件锁 + 陈旧锁回收**（§3.8）：
   - `O_EXCL` 建 `--dir/.ssh-manager-backup.lock`（内容 `<pid> <host> <start-ts>`）。
   - `O_EXCL` 失败（`os.ErrExist`）→ 读锁文件 → 判活（跨平台：Linux `signal 0`，Windows `OpenProcess`）+ 判超时（30 min）→ 孤儿/僵死 → 窃取重建；真并发 → exit 0 + "another backup in progress; skipping"。
4. **解锁 vault**（master key，和 serve 同路径：`SSHMGR_MASTERKEY_HEX` env 或 keychain）→ `ExportSnapshot()` → `json.MarshalIndent` → 算明文 SHA256。**复用同一 `[]byte` 算 hash 和写盘，不 re-marshal**。
5. **skip 判定**：找最新一份 `<prefix>-*.json`（按文件名字典序最大，glob 严格 `*.json` 不匹配 `.sha256`），读其 `.sha256` 边车的 `file_sha256` → 相同 → "vault unchanged; skipping"，删锁退出 0。
   - **边车缺失/读不了 → 当作"无可比对"出 new 备份**（fail-open，不报错停摆）。
   - **可选自愈**：最新 .json 无边车时，直接 hash 那个 .json 做比对（省一份冗余写入）。
6. **原子写新备份**：`vault-<UTC时间戳>.json`（temp + `os.Rename`，0600；**Windows 上 0600 位被忽略，靠 ACL**）。同秒撞名 → 加 `-2`/`-3` 后缀（既保持原子写又避免覆盖）。
7. **fsync 耐久性**（codex）：`f.Sync()` temp 文件 → rename → `os.Open(dir).Sync()`（**Linux**；Windows 跳过 dir fsync，不报错）。
8. **写边车** `<file>.sha256`（0600，key=value 多行）：`file_sha256=<hex>`（+ 可选 `size=<n>`）。
9. **写后验证（hash 往返）**（codex/pi）：重读落盘 `.json` → 重算 SHA256 → 断言 == 边车 `file_sha256`。+ `json.Unmarshal` 回 `store.Snapshot`（查结构损坏）。**不做字段级校验**（数据源是 live vault，已强不变量；重做会漂移）。
10. **轮转**：列 `<prefix>-*.json`（glob 严格 `*.json`）按文件名降序留前 `--keep` 份，删其余（边车 `jsonpath + ".sha256"` 跟着删，`os.Remove` 忽略 not-found）。`--keep 0` = 不删。marker/锁不匹配 glob，永不动。
11. 删锁，退出 0。

### 5.3 `backup verify <file>` 流程

- 读 `<file>.sha256` 的 `file_sha256` → 重算落盘文件 SHA256 → 比对（检测 bit-rot / 文件坏）。
- `json.Unmarshal` 回 `store.Snapshot`（查结构损坏）。
- 不一致 → 报错退出非零。
- 不用口令（明文）。

### 5.4 `import` 加格式嗅探（支持明文 .json）

`import <file>` 开头嗅探前 8 字节：
- `SSHMGRV1` → Plan 11 加密文件 → 走原口令解密路径（`vaultio.Decrypt`）。
- 否则 → 当明文 JSON 直接 `json.Unmarshal`。
- JSON 首字节必是 `{`（0x7B），`SSHMGRV1` 首字节 `S`（0x53），正向零误报。
- **歧义注释**（pi）：Plan 12 cache 文件也用 `SSHMGRV1` magic（`EncryptWithKey`），误把 cache 文件喂 import → 当口令文件解 → GCM auth 失败（安全，但不是用户意图）。`import.go` sniff 处加注释说明此假设。
- 嗅探逻辑放 `vaultio` 导出 helper（如 `vaultio.IsEncrypted(data []byte) bool`），不内联 magic 到 `cli/import.go`。

### 5.5 ExportSnapshot ORDER BY 修复（三家共识，独立必修）

`internal/store/export.go` 的 credentials/profile_servers(grants)/projects 三查询加 `ORDER BY id`（grants 是 `ORDER BY profile_id, server_id`）。这是 ExportSnapshot 自身的**确定性 bug**——不修则相同 vault 每次序列化字节序可能不同 → skip 永远失效 → NAS 堆满相同备份。也修了 export/import/cache 的确定性。3 行改动。

### 5.6 数据模型

无新表。复用 `store.Snapshot`（Plan 11）。**cache_tokens 不进 Snapshot**（§11）。

### 5.7 文件布局

- `<--dir>/vault-<UTC>.json` — 明文快照（0600/ACL）
- `<--dir>/vault-<UTC>.json.sha256` — 边车（`file_sha256=`）
- `<--dir>/.ssh-manager-backup-marker` — marker（运维建，一次性）
- `<--dir>/.ssh-manager-backup.lock` — 锁（运行时，含 pid/host/start-ts）

## 6. Windows 适配细节（§3.4 展开）

### 6.1 权限位降级
- temp 文件 `os.OpenFile(..., 0600)`：Linux 有意义；Windows 位被忽略（`FileMode` 在 Windows 只区分 readonly vs rw）。**Windows 不程序校验目录权限**，文档强调靠 NTFS ACL（文件夹属性 + 共享权限）。
- 不做"校验 `--dir` 是 0700"——Linux 上改为"建议但 warn"，Windows 跳过。

### 6.2 fsync 分平台
- 文件 `f.Sync()`：全平台（Windows = `FlushFileBuffers`）。
- 目录 `os.Open(dir).Sync()`：Linux 有效；**Windows 不支持（目录句柄不能 FlushFileBuffers），跳过 + 不报错**。代码用 `runtime.GOOS == "windows"` 分流，或 `dir.Sync()` 忽略特定错误。

### 6.3 锁回收 pid 判活分平台
- Linux/macOS：`os.FindProcess(pid)` + `syscall.Kill(pid, 0)` → errno `ESRCH` = 不活。
- Windows：`os.FindProcess` 总成功（不验活）。用 `golang.org/x/sys/windows.OpenProcess`（或 `windows.OpenProcess` + 关句柄）→ 句柄为 nil = 不活。
- build-tag 分流（`lock_unix.go` / `lock_windows.go`）。

### 6.4 定时器文档（任务计划程序）
Windows 用 `schtasks` 注册：
```cmd
schtasks /Create /SC DAILY /ST 03:30 /TN ssh-manager-backup ^
  /TR "ssh-manager.exe backup create --dir Z:\backups --keep 7" ^
  /RU <user> /RP <password>
```
（`Z:` = `net use` 映射的群晖盘；`/RU` 用户的 master key 要么在 keychain 要么用 `setx SSHMGR_MASTERKEY_HEX`。）

Linux 仍给 systemd timer 模板（`Type=oneshot` 防 timer 自身重叠）。

## 7. 安全考虑（写进文档，诚实）

- **明文 = 凭据明文**：适用/不适用见 §3.2。
- **NAS 受信是部署硬约束**：违反（开 Cloud Sync/公网）→ 必须重新启用加密（v2，见 §10）。
- **运维 footgun**：禁 `cat`/`grep -r password`（用 `backup verify` 或恢复到测试 vault 后 `servers ls`）；`--dir` 绝对路径非 git；仓库 `.gitignore` 发模板 `vault-*.json` + `*.sha256`。
- **master key 来源**（部署文档）：backup unit（systemd 或 schtasks）要能解锁 vault——要么和 serve 同 user 会话（keychain 可达），要么 `SSHMGR_MASTERKEY_HEX` 注入（systemd `Environment=`/`EnvironmentFile=`，或 Windows `setx`）。
- **长期静态 vault**：skip 让你只握 1 份不刷新文件 → 依赖定期 `backup verify` + NAS 快照做底层兜底（文档点明）。
- **审计日志明文**（pi/codex）：`audit_log.command` 可能含历史命令行 secret，明文裸放 NAS——文档点明，不改设计（量级是 blast radius 扩张，不是新暴露）。

## 8. 测试

- **`internal/store/export_test.go`**（修 ORDER BY）：相同 vault 连续 `ExportSnapshot` → JSON 字节完全一致（确定性）；删除+重插一个 credential 后 → 仍一致（顺序由 ORDER BY 保证，非 rowid）。
- **`internal/cli/backup_test.go`**（新）：
  - `create` → 产 `.json` + `.sha256` 边车；marker 缺失 → fail-closed；`.git` 祖先 → fail-closed。
  - **skip**：第二次 `create`（vault 未变）→ 不产新文件（exit 0）；改 vault 后 → 产新文件。
  - **轮转**：`--keep 2` + 3 次变化 → 留最新 2 份（边车跟着删）。
  - **锁**：两个 `create` 并发（goroutine）→ 一个 exit 0 skip、一个正常完成。
  - **陈旧锁回收**：手动写一个孤儿锁（pid 不存在）→ 下次 `create` 窃取并正常完成（不 stuck）。
  - **同秒撞名**：模拟 → `-2` 后缀。
  - **写后验证**：落盘文件被篡改（flip byte）→ 边车 hash 比对失败（不静默）。
  - **Windows 跳过**：`runtime.GOOS == "windows"` 时 dir fsync 跳过（不报错）——用单元测试 + build-tag 隔离。
- **`internal/cli/import_test.go`**（加嗅探）：明文 `.json` → 走 unmarshal；`SSHMGRV1` 加密文件 → 走原口令路径。
- **`internal/cli/backup_verify_test.go`**（新）：`verify` 检测 bit-rot（flip byte）→ 非零退出。
- **跨平台锁回收**：`lock_unix_test.go` / `lock_windows_test.go`（build-tag），各测本平台 pid 判活。
- **No-regression**：`go test ./...` green；`gofmt -l .` 干净；`go vet ./...` clean。

## 9. 实现触点（file-by-file）

| 文件 | 改动 |
|---|---|
| `internal/store/export.go` | credentials/profile_servers/projects 三查询加 `ORDER BY`（§5.5）|
| `internal/cli/backup.go`（新）| `newBackupCmd()`（create + verify）；marker 检测；`.git` 护栏；锁；skip；原子写；轮转；写后验证 |
| `internal/cli/backup_test.go`（新）| 见 §8 |
| `internal/cli/lock_unix.go`（新）| `pidAlive(pid) bool`（signal 0）|
| `internal/cli/lock_windows.go`（新）| `pidAlive(pid) bool`（OpenProcess）|
| `internal/cli/lock.go`（新）| 共用：锁文件读写、陈旧锁回收逻辑（调 `pidAlive`）|
| `internal/cli/import.go` | 格式嗅探（`SSHMGRV1` vs 明文 JSON）|
| `internal/vaultio/vaultio.go` | `IsEncrypted(data []byte) bool`（导出 magic 检测 helper）|
| `internal/cli/root.go` | 注册 `newBackupCmd()` |
| `docs/multi-machine.md` 或新 `docs/备份.md` | §11 文档（部署硬约束、marker、systemd/任务计划、恢复、Windows 适配、限制）|

## 10. 未来工作（显式 deferred）

- **重新启用加密**：若违反部署硬约束（需 Cloud Sync/公网），回加密版（codex 第 1 轮的 decrypt-and-compare skip 方案）。
- **cache_tokens 纳入 Snapshot**（§11）：若要"恢复到出事前一模一样"。
- **fsync 根治 Windows**：探索 Windows 替代（如 `FILE_FLAG_WRITE_THROUGH`）。
- **字段级写后校验**：若 vault 不变量演化。
- **backup restore 一条龙**：若恢复高频（目前 = 拷 + import）。

## 11. cache_tokens 范围（v1 决策）

**cache_tokens 不纳入 Snapshot（v1 文档化为显式非目标）。** 恢复后需 `cache-tokens add` 重发各工作机设备授权码。

理由：cache_tokens 是**设备身份**（语义上不属于"保险柜内容"）；纳入需动 Plan 11 的 Snapshot 结构（version bump）；重发成本极低（owner 一条命令 + 每台机 `cache pull` 一次）。

文档（recovery 章节）明确："DR 恢复 vault + project token（agent 的 .mcp.json 不用动）；cache-tokens 需 `cache-tokens add` 重发，各工作机 `cache pull` 重拉。"

## 12. 落地前 checklist（评审必修项映射）

- [ ] O_EXCL 锁 + 陈旧锁回收（§3.8 / §5.2.3 / §6.3）—— pi blocker
- [ ] ExportSnapshot ORDER BY（§5.5）—— 三家共识
- [ ] `.sha256` 边车保留（§3.9）—— codex+pi
- [ ] fsync 文件（§5.2.7）—— codex
- [ ] 写后 hash 往返（§5.2.9）—— codex/pi
- [ ] temp 文件 0600（§5.2.6，Linux）/ ACL（Windows，§6.1）—— codex
- [ ] skip + rotation 统一文件名排序（§3.7 / §5.2.5）—— 共识
- [ ] UTC 时间戳（§3.7）—— 三家
- [ ] marker 文档钉死顺序（§3.6）—— 三家
- [ ] `.git` 路径护栏（§5.2.2）—— claude Q3
- [ ] Windows 分平台（fsync/pid/权限/调度）（§6）—— 用户 Q3
- [ ] 安全模型文档（§7）—— 三家（audit 命令行 secret、server→NAS 拓扑、Cloud Sync 当 checklist）

## 13. 参考

- 两轮 xcheck 评审综合：`.xcheck/20260812-071531/SUMMARY.md`（第 1 轮，加密版）、`.xcheck/20260812-074658/SUMMARY.md`（第 2 轮，明文版）
- Plan 11（export/import）：`internal/store/export.go`、`internal/vaultio/vaultio.go`
- Plan 12（cache）：`internal/store/cachetoken.go`（Snapshot 不含 cache_tokens）
