# Plan 39 · 设备码绑 profile——/snapshot 按授权裁剪 + 服务端 TUI 外部写刷新

> 编号说明:本 plan 编号 39——另一并发线的 "Plan 38-doctor" 已先行占用 38 并入 master。
> 2026-08-26 · 状态:已实施(分支 `allanpk716/修复授权bug`)
> 触发:owner 报修两个 bug——① client 机 TUI 同步后看到 server 侧未授权的服务器(gitlab-urit);② client 同步后 server TUI 设备码页不显示最新拉取时间。

## 根因(线上证据闭环)

### Bug 1:未授权服务器进入 client 机

| 证据 | 结论 |
|---|---|
| NUC10 vault 11 台(含 gitlab-urit);唯一 profile `e2e-profile` 授权 10 台 | gitlab-urit 是唯一未授权服务器 |
| client agent `list_servers`(在线 MCP):10 台 | **MCP 层 profile 过滤正常** |
| client `cache status`:11 台/12 凭据 | **cache.bin 装整库,含未授权服务器凭据明文(信封内)** |

两层根因(均为 Plan 12 原始设计):
1. cache token 是 owner 级、**不绑 profile**(`cachetoken.go` 旧注释自述)→ `/snapshot` 整库 dump;
2. client TUI 面板无裁剪(`syncList` 全量镜像快照服务器)。

授权模型缺口:profile 授权只压在 MCP 工具层;cache 链路(拉取/落盘/显示)没设防。

### Bug 2:server TUI 看不到外部写的 last_pull

DB 里 `last_pull_at` 确实更新(serve 进程 `TouchCacheToken`);但 server TUI 四页是启动时 `FetchAll` 一次性加载,Tab 切页只改 `a.page`,只有本地动作触发 `refetchPages` → 外部进程的写入永不显示。

## 设计决策

| # | 决策 | 理由 |
|---|---|---|
| D1 | 设备码 ↔ profile 一对一绑定;`/snapshot` 返回绑定 profile 的授权子集 | 与向导命名约定一致(客户端机器名同时命名 profile 与设备码);"一机一装箱单"是 domain 本意 |
| D2 | 未绑定码 pull 返回 **403 非 401** | Plan 34 契约:pinned 401 = 客户端销毁 cache。403 非破坏,owner 侧 `bind` 修复即可 |
| D3 | 存量码用 `cache-tokens bind` 补绑 | 免重发码、免 client 重 enroll、保留 last_pull 历史 |
| D4 | 裁剪快照**不含 audit 行** | 离线审计走本地 cache-audit.log 边车;命令历史留 server 侧 |
| D5 | Snapshot JSON 结构不变(纯子集) | 旧 client 零成本兼容 |
| D6 | `ExportSnapshot()`(export/import 备份)不动 | owner 口令加密整库备份与授权无关 |
| D7 | server TUI Tab 切页即 `refetchPages()` | 与既有 actionDoneMsg 语义一致;本地 SQLite 成本可忽略 |
| D8 | 一机多 profile 超范围,文档如实记录 | 一机一码一 cache.bin 的物理模型;多 profile 合并违背"零合并"基线 |

## 落地

- **store**:`cache_tokens.profile_id TEXT NULL REFERENCES profiles(id)`(schemaSQL + `addColumnIfMissing` 迁移;NULL=未绑);`AddCacheToken(name, profileID)`(空 profileID 拒绝)、`BindCacheToken`、`GetCacheToken`;`Verify/List` 读回 ProfileID;`DeleteProfile` 增加 active 设备码守卫(revoked 绑定置 NULL 不挡删)。
- **store**:`ExportSnapshotForProfile(profileID)`——grants→servers IN;凭据只取被引(credential ∪ sudo);profile 行+grants;`projects WHERE profile_id`(同 profile 的 token 离线可验,他 profile 的正确失效);host_keys 按服务器过滤;audit 恒空;profile 缺失显式 error。
- **serve** `handleSnapshot`:auth 后 `GetCacheToken` → 未绑 403(带 bind 指引)→ 否则裁剪导出;`TouchCacheToken` 不变。
- **client**:`DoPull` 403 分支明确文案(不触 quarantine);TUI `classifyPullError` 新类"设备码未绑定 profile";`cache status` 与 client TUI 页头显示快照 profile(恰 1 个时)。
- **CLI**:`cache-tokens add --profile`(必填)、`bind <name> <profile>`、`ls` 显示 profile(unbound 显示 `-`)。
- **TUI server**:签发表单加 profile 下拉(零 profile 拒绝并指引);向导传 `w.data.profileID`;升级段 0/1/N profile 解析(0→中止指引,1→自动绑,N→表单追加 Select);设备码页列表/详情显示绑定 profile;**Tab/Shift+Tab 切页重读**。

## 测试锚点

`store/cachetoken_test`(绑定/bind/Get/迁移未绑/FK 挡删)· `store/export_test`(裁剪矩阵+水合回环)· `mcpserver/serve_snapshot_test`(**ScopedToBoundProfile**/**UnboundToken403Not401**/revoked 401/project-token 非200)· `clientops/quarantine_test`(pinned 403 不毁 cache)· `tui/clientpage_test`(403 分类/页头 profile)· `tui/app_test`(**TabSwitchRefetchesPages**)· `cli/cache_tokens_test`(--profile 必填/bind/ls)· `cli/mcp_cache_test`(**ScopedPull_HydratesAndIronRuleHolds** e2e:真 HTTP 拉取→水合→token 验证→list_servers=授权集→出集 exec=ErrNotInProfile)。

## 升级 runbook(fleet)

1. 替换 NUC10 二进制(serve 未重启);
2. `ssh-manager cache-tokens bind laptop-v040 e2e-profile`;
3. 重启 serve;
4. client re-pull(旧整库 cache.bin 被原子覆盖——**必做**,否则未授权凭据残留);
5. 验收:client `cache status` = 授权台数 + profile 行;NUC10 TUI 设备码页切页见最新 last_pull。

## 残余(如实)

- 未绑码在 bind 前拉取 → 403(拉不动但 cache 不毁)——升级窗口的既定代价,compat-matrix 有 runbook。
- 一台机多 profile(多 agent 不同装箱单)不支持:多码共用一个 cache.bin 会互相覆盖。
- 停留在页上期间的外部写入要切走再切回才可见(不做定时 tick)。
- 永离线机器的旧整库 cache.bin 不受本修复追溯——re-pull 才覆盖;必要时吊销旧码强制重 enroll(revoke → 回连销毁 → 新码重拉)。
- **共享凭据跨授权边界**(code-review 登记):裁剪按服务器行;未授权服务器与已授权服务器共用凭据时该凭据仍下发(已授权服务器需要)。凭据级隔离 = owner 不跨授权边界共享凭据。已登记 threat-model + multi-machine 限制节。
- **bind 错配 footgun**(code-review 登记):bind 到不含本机 project 的 profile → 离线栈搁浅(热加载静默保旧快照 / 新 spawn token 无效),错误不指向错配。已登记 multi-machine 限制节。

## 评审修复轮(2026-08-26 code-review 后)

必修四项已修:向导 resume 多 profile 加 `stepBindProfile` 选择(绝不静默绑字母序第一);serve `GetCacheToken` err→500+stderr(不再混 403 误导 bind);DoPull 403 按 **body 判别**(仅 serve 的 "not bound to a profile" 才给 bind 指引,代理/WAF 403 通用报错+body 摘要),classifyPullError 收紧为仅判别词;**裁剪溯源**:serve 发 `X-Sshmgr-Snapshot-Scope: profile` 头 → `cache.meta.scoped`(ServerAnchored 同款 no-omitempty)→ `cache status` 的 scope: 行与 client TUI 页头只在 scoped 时显示 profile(杜绝单 profile 整库快照冒充已裁剪)。
