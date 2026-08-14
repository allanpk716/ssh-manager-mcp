# 设计 spec — 缓存自动保鲜（Plan 17 候选）：spawn-lazy pull + 会话内定时拉取 + 热加载

> 日期：2026-08-14。状态：设计定稿（grilling 三轮 + xcheck 三家异构评审 kimi/pi/codex 全部意见已吸收）。
> 范围：Stream A（工作机侧缓存保鲜）。Stream B（broker 端 TUI）另立 spec，不在本篇。

## 1. 背景与问题

多机架构（路线乙）中，工作机的离线只读缓存 `cache.bin` 目前有两个保鲜缺口：

1. **不会自动拉**：文档要求用户手配系统定时器（systemd timer / 任务计划 / launchd）跑 `cache pull`（`quickstart-multi-machine.md` Step 3、`multi-machine.md:363-455/512`）。
2. **不会热加载**：运行中的 `mcp --cache` 把快照冻结在 spawn 时刻（`multi-machine.md:513`），外部刷新了缓存，当前会话也看不到。

根因：工作机无常驻进程承担定时刷新（`mcp --cache` 是 Claude Code 按需 spawn 的短命子进程），而快照在 spawn 时一次性载入。

## 2. 设计决策（含评审修订）

| 决策点 | 定案 | 依据 |
|---|---|---|
| 刷新主体 | 撤 daemon；**spawn-lazy pull + 会话内定时拉取 + 热加载**三件套，全部在 `mcp --cache` 进程内 | 工作机零常驻原则；cache.bin 唯一消费者就是本进程 |
| 会话内保鲜 | TTL 过期且本机有凭据 → **工具调用路径上直接拉**（阻塞上限 = client 超时 10s） | 长会话无需任何外部触发即可保鲜，彻底消掉手配定时器 |
| 失败退避 | pull 尝试记 lastAttempt；失败后一个 TTL 窗口内不再重试 | 防离线时「每次工具调用都白等 10s」 |
| 凭据持久化 | 新文件 `cache.auth.json`（0o600 + Windows HardenACL）：url + 裸设备码 + **resolvePin 归一后的最终生效 pin** | 设备码授予拉取**未来**快照的权力（cache.bin 只是过去），非零增量，靠吊销兜底；threat-model §1.1 正式记录 |
| pin 优先级 | lazy 路径：**cred.Pin > token 内嵌** | 证书轮换后手动 `--pin` 重拉能覆盖旧内嵌 pin，防自动路径永久失配 |
| TTL | 默认 30min，`--cache-max-age` 可调；**`0` = 全局禁用（含 cache.bin 缺失场景）** | 缺文件时按原逻辑报错，引导首次手动 pull |
| 吊销语义 | 新快照里 token 失效 / profile 漂移 → 保留旧库服务到本 spawn 结束 + stderr 日志 | 延续「Lazy 生效」既有语义（`multi-machine.md:491`） |
| 判变手段 | file identity（dev/ino）+ size + SHA-256；**baseline 在初始 `loadCacheSnapshot()` 之前采集** | 评审指出裸 mtime+size 在粗分辨率文件系统同 tick 同长度下漏判，且 baseline 后采会吞掉启动瞬间的外部更新 |
| 并发落盘 | cache.bin / meta / auth 全部 `os.CreateTemp` 唯一名 + rename | 现状固定名 `bin+".tmp"` 只保证单进程原子；多会话并发 pull 会撕裂（评审三家共识，推翻了早期「并发无锁幂等」论断）；顺带修 meta 现状非原子直写 |
| 旧库清理 | 换库**不立即 Close/删除**，登记后统一 defer 到进程退出 | SDK 异步派发工具调用，在飞调用可能仍握旧库指针（use-after-close） |
| lazy 网络超时 | lazy 路径专用带整体超时（10s）的 http.Client；手动 `cache pull` 沿用无超时 | lazy 在 spawn/工具调用关键路径上，无界等待 = 卡死；手动场景用户可 Ctrl-C |
| `--allow-plaintext` | 永不进自动路径；显式明文拉取**不写** cred 文件 | 防把无 pin 配置固化成自动明文 |
| cred 写失败 | stderr 必警告（含 ACL 失败） | cred 是自动保鲜的必要条件，静默 = 功能失效不可感知 |

## 3. 架构

```
spawn ──▶ ①lazy pull（cache.bin 缺失或年龄>TTL 且有 cred；10s 超时；失败降级旧缓存 + stderr）
       ──▶ loadCacheSnapshot ──▶ hydrate 只读 store ──▶ 6 工具闭包经 storeFn() 取库
                                    ▲
每次工具调用 ──▶ ②检查 cache.bin（identity+size+hash）
                   ├─ 变了 → 重建新库原子换入（旧库登记延迟清理）
                   └─ 没变但年龄>TTL 且距上次尝试>一个 TTL 窗口 且有 cred
                        → ③会话内 lazy pull（成功则下轮判变自然换库；失败退避）
```

在线模式（stdio 直连 vault / serve）不涉及——它们本来就实时读库。

## 4. 组件设计

### 4.1 `internal/cli/cache.go`

- **`doPull(url, token, pin string, opts pullOpts) error`**：从 `cachePullCmd` RunE（`cache.go:141-224`）抽出唯一拉取实现；hard-fail 校验留在 CLI 层；`pullOpts` 区分手动/自动（client 超时等）。
- **原子落盘**：cache.bin、cache.meta.json、cache.auth.json 一律唯一 temp + rename（修 `cache.go:219` meta 非原子直写）。
- **`cacheCred` / `readCacheCred()`**：`{URL, Token(裸码), Pin(归一后)}`；文件不存在 → `(nil, nil)`；`cache pull` 成功后写入，失败必警告。
- **`maybeLazyPull(maxAge) error`**：TTL + lastAttempt 退避判定，spawn 与会话内共用。

### 4.2 `internal/cli/mcp.go`

- cache 分支顺序：`maybeLazyPull` → `loadCacheSnapshot`（**判变 baseline 在此之前采集**）→ 构造 reloader → `RunStdioCache(token, snap, auditPath, reload)`。
- 新 flag `--cache-max-age duration`（默认 `30m`）。

### 4.3 `internal/mcpserver/server.go`

- **`NewServerFromSource(storeFn func() *store.Store, profileID, projectID string)`**：6 个工具闭包改为调用时经 `storeFn()` 取库；`NewServer(st, ...)` 委托之，既有调用方（RunStdio、ServeRunner、测试）零改动。

### 4.4 `internal/mcpserver/run.go`

- **`hydrateCacheStore(snap) (*store.Store, *models.Project, tmpPath, error)`**：现「temp store + ImportSnapshot + SetReadOnly + VerifyToken」序列抽出复用。
- **`cacheStoreHolder`**：`atomic.Pointer[store.Store]` + `sync.Mutex`（串行化重建）+ reload 回调 + token + auditFile + tmp 登记表。`Current()`：
  - reload 未变 → 返回现值（快路径）；
  - 变了 → hydrate 新库 → VerifyToken 失败 / **profile 漂移**（新 project.ProfileID ≠ 启动时）→ 保旧库 + stderr；成功 → 原子换入，旧库不 Close 不删（登记后退出统一清理）；
  - 读到损坏 cache.bin → 保旧库 + stderr（temp+rename 下属防御性兜底）。
- 复用同一 audit sidecar `*os.File`（O_APPEND 无状态）；tunnels 不重建（已建立隧道续命，新调用走新库凭据）。
- `RunStdioCache` 签名加 `reload func() (*store.Snapshot, bool, error)`（nil = 不热加载）。

### 4.5 cli 侧 reloader 闭包

- 判变：stat（identity/size）→ 必要时 SHA-256；变化 → `loadCacheSnapshot()` 返回 `(snap, true, nil)`；未变 → `(nil, false, nil)`；失败 → `(nil, false, err)`。
- 未变且年龄 > TTL 且退避窗口已过且有 cred → 触发 `maybeLazyPull`（③），成功后返回「未变」——本轮调用仍用旧库，**下一次**工具调用经判变换入新库（避免同一调用内半程旧半程新）。

## 5. 错误处理汇总

| 场景 | 行为 |
|---|---|
| lazy pull 失败（离线/401/pin 失配/超时） | 旧缓存继续 + stderr 一行 + 退避一个 TTL 窗口 |
| 新快照 token 失效 | 保旧库到 spawn 结束 + 日志（Lazy 语义） |
| 新快照 profile 漂移 | 保旧库 + 日志 |
| cache.bin 损坏 | 保旧库 + 日志 |
| cred 写失败 / ACL 失败 | pull 仍算成功，但 stderr 警告 |
| 并发多会话同时 pull | 唯一 temp 名 + rename，各自独立成功 |

## 6. 测试计划

单测/集成（TDD，复用 `withEnv` / `withDEK` / `standUpServe` 基建）：

1. `doPull` 行为不回归；2. cred 写/读/缺三态 + 写失败警告 + `--allow-plaintext` 不落盘；3. 并发 doPull 撕裂回归（双 goroutine ×N 验解密）；4. TTL 判定（`os.Chtimes` backdate）+ maxAge=0 时缺文件也不拉；5. 证书轮换 pin 优先级（env-pin 首拉 → 换证书 → lazy 仍成功）；6. 判变未变 → 同一 store 指针；7. 重写 cache.bin（显式 `os.Chtimes` 推未来 mtime）→ 新库生效、`ListServersForProfile` 见新机器；8. 损坏文件 / token 失效 / profile 漂移 → 保旧库；9. 并发 `Current()` 恰一次重建 + **在飞调用 × reload 不报 closed DB**；10. **真闭包链路冒烟**（真 in-process server 调工具 → 换库 → 再调 + source-call 计数）；11. 会话内定时拉取 + 失败退避。

端到端手工验收（NUC10 真 broker）：broker 改清单 → 已开的 Claude 会话在 TTL 过期后下一次 `list_servers` 直接看到新清单，全程无人手动 pull（核心验收点）；断网场景降级 + stderr 日志。

## 7. 文档改写

- `quickstart-multi-machine.md`：Step 3 整节重写（定时器降级为可选）；line 69 注释。
- `multi-machine.md`：`212/226/309/363-455/491/504/512/515` + `260` 架构图 + `526`/`542` 证书轮换 runbook（改「重新 pull 覆盖 cred 内 pin」）。
- `threat-model.md` §1.1：正式记录 `cache.auth.json` artifact 及处置（0600+ACL、失窃 revoke、证书轮换后重拉覆盖 pin）。
- Plan 12 归档文档不动；README / getting-started 已核实无需改。

## 8. 边界与不做

不做 daemon / fsnotify / 推送协议；不动 serve 侧任何代码；不改变 pull 的 TLS+pin hard-fail 语义；`--allow-plaintext` 永不进自动路径；Stream B（TUI）另立 spec。
