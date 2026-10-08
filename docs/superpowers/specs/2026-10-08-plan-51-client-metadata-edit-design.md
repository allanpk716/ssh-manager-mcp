# Plan 51 设计:客户端元数据编辑——受审计转发写从「仅可新增」扩展到「乐观锁更新」

> 痛点来源:2026-10-08 用户反馈——服务器条目的元数据(硬件/部署了什么/注意事项)今天只能在权威端(NUC10)TUI/CLI 维护,客户端机器只读快照上任何写入都 `ErrReadOnly`;agent 探明一台服务器后无法把发现记回,元数据只能靠人肉登权威端维护,日趋腐化。2026-10-08 两轮 grilling 拍板、本文不重议:**Q1=C 分批(批 1=MCP 工具,批 2=客户端 CLI/TUI 表单,共走同一 broker 路由)**、**Q2=A 仅六个元数据字段(role/services/location/hardware/caveats/description;tags 与连接字段一律不开放)**、**Q3=A 默认开放+全局开关(flag/env/store/默认四层)**、**Q4=A 乐观锁冲突 409**、**Q5=A broker 不可达直接失败(不排队不留状态)**、**Q6=A 写成功后本地窄写口镜像补记**、**Q7=A 部分更新(字段缺席=保持原值,显式空串=清空)**、**Q8=A 新增 revision 单调列做 CAS 令牌(updated_at 是 unix 秒,同秒 ABA)**、**Q9=B 审计行记骨架+旧值截断(每字段 200 字节)**、**Q10=A 批 1 工具只挂客户端缓存脸**。术语以根目录 `CONTEXT.md` 为准(服务器元数据/受审计转发写);取舍记录见 ADR 0005。
>
> 设计大量沿用 Plan 48(锚定转发)的既定纪律:同一 `cacheAuth` 设备码闸门、shape guard 前置、单事务原语+同事务审计、分支文案逐字断言、本地窄写口、401 不触发 Plan-34 隔离。本文只写差异与新增,共有纪律引用不重复展开。

## 0. 目标与缺口

六个元数据字段(`models.Server` 的 `Description/Location/Hardware/Services/Role/Caveats`,各 ≤4 KiB,`validateServerText`)已经通过 `list_servers` 全量给 agent 看;**写**却只有权威端两条路(owner TUI 编辑页 / `sshmgr servers edit`)。缺口=「读面向 agent、写面向 owner」的不对称。

Plan 48 的 `/pin-hostkey` 已证明「客户端持设备码发起、broker 落库+审计」的受审计转发写成立,但其写语义是 **insert-only**(仅可新增,绝不覆盖)。元数据编辑本质是**更新**(同一条目反复改),直接套用 insert-only 不成立——写类别升级,护栏必须相应升级:这是引入 revision 乐观锁的全部动因(Q8)。`updated_at` 是 unix 秒(`scanServer`:`time.Unix(updatedAt,0)`),同一秒内先后两次写分不清先后,拿它做 CAS 令牌会同秒静默互踩,故必须新列。

## 1. 协议:`POST /server-metadata`(serve 侧)

`HTTPHandler` 路径分发新增一条:`/server-metadata` → 与 `/snapshot`、`/pin-hostkey` **同一个** `cacheAuth` 包裹 → `handleServerMetadata`。零新认证形态。

### 1.1 请求 / 响应

```json
// POST /server-metadata   Authorization: Bearer <设备码>
{ "server_id": "srv-xxxx", "expected_revision": 3,
  "fields": { "hardware": "…", "caveats": "" } }
// fields 键白名单 = role/services/location/hardware/caveats/description;
// 键缺席 = 保持原值;显式空串 = 清空该字段;null 值 = 400。至少一个键。

// 200 OK(更新成功)
{ "server_name": "gw", "revision": 4, "updated_at": 1760000000 }

// 409 Conflict(revision 不匹配——条目在客户端列出之后被别人写过)
{ "error": "stale revision", "current_revision": 4,
  "current": { "role": "…", "services": "…", "location": "…",
               "hardware": "…", "caveats": "…", "description": "…" } }

// 400 Bad Request(键不在白名单/值为 null/fields 空/字段超 4 KiB/JSON 不可解码)
{ "error": "<具体成因>" }
```

- **`server_id` 而非 `server_name`**:与 `exec_command` 等既有工具一致(agent 词汇本是 id;`list_servers` 同场给 name 仅供人读)。授予判定因此从 pin-hostkey 的「逐条 host:port 走」简化为**id 集合成员判定**(`ServersForProfile(bound)` 直接含 id)。
- **409 携带六个字段现值的零泄露论证**(沿 Plan 48 rev3 equal 的论证锚点):请求者持有效设备码,其绑定 profile 已授予该条目——六个字段的现值本就随该设备码的下一次 `/snapshot` 全量可得,409 body 的全部信息 ⊆「同一凭据、同一监听器的一次拉取」可自算的内容。携带现值的意义:**agent 一跳内收敛**——不用 cache pull,直接拿 current_revision+现值合并意图重试(见 §2)。旧缓存(快照无 revision 字段,一律读 0)即使落后多个版本,首次 409 也带全现值,同样一跳收敛——优于「保守失败要求先 pull」的原始设想,且收敛不依赖任何自动重拉。
- **请求体上限 = 新设 32 KiB 常量**(`MaxBytesReader`;六字段×4 KiB+开销;仓库先例 `/pin-hostkey` 64 KiB),谎报 ContentLength 由 `MaxBytesError` 在解码时归 413(pin 同款)。

### 1.2 守卫执行序(钉死)

```
handleServerMetadata:
  ① method != POST → 405;请求体 > 32 KiB → 413;
     JSON 解码失败 → 400;server_id 空 / expected_revision < 0 / fields 空 /
     fields 含白名单外键 / 任一键值为 null → 400(成因入 error 文本)
  ② TokenInfo → GetCacheToken:存储故障 → 500 + stderr(handleSnapshot 教训);
     无 TokenInfo/无该行/绑定 profileID 为空 → 403(fail-closed)
  ③ 开关闸:MetadataEditEnabled() 为假 → 403,文本含 "disabled"
     (客户端据此分支出「owner 关闭」专属文案,见 §2;不是授予问题)
  ④ 授予判定:server_id ∈ ServersForProfile(bound profileID)?
     不在 → 403,故意含措辞(不回显轮廓/条目/数量——Plan 31/39 边界,只回显请求方提供的 id)
     条目在授予集合内但行已消失(删除竞态)→ 同款 403(原语阶段发现的缺失也归并到此,见 ⑥)
  ⑤ 字段长度校验(≤4096 字节/字段)→ 超限 → 400(带字段名)
  ⑥ 原子落库——新 store 原语 UpdateForwardedMetadata(单事务,§4):
       tx: 读行(role…description, revision) → revision != expected → ErrStaleRevision{current} → 回滚
           行缺失 → ErrServerGone → 映射 ④ 同款 403
           应用在场字段 → UPDATE … revision=revision+1, updated_at=now() WHERE id=? AND revision=?
           同事务 writeAuditTx(§5) → commit
  ⑦ 200 { server_name, revision(=期望+1), updated_at }

  每次请求(无论成败)向 serve stderr 打一行(仿 pin-hostkey 惯例):
  "sshmgr serve: server-metadata <server_id> -> <status> (device <名>)"
```

- **401 语义**:无效/已吊销设备码由 `RequireBearerToken` 回 401,**不触发 Plan-34 缓存隔离**(隔离只活在拉取路径;pin-hostkey 同款注释先例)。
- **无限速**:与 `/pin-hostkey` 一致(补偿=审计行+stderr 行+设备码可吊销);`/pair/*` 的 per-IP 限速器不引入。

## 2. 客户端侧:元数据转发器

新文件 `internal/clientops/metadata_forward.go`,`MetadataForwarder` 全范本复刻 `PinForwarder`(同包同款纪律):

- 构造:`NewMetadataForwarder(cred CacheCred)`——由 `internal/cli/mcp.go` 在 `--instance` 解析处与 `NewPinForwarder` 并排构造并注入(读实例永远显式铁律);`pin 为空`(明文)→ 无能力且 `Edit` 拒绝(明文编辑,比 pull 的逃生门严,与 pin 转发同姿态);URL/Token 空(cache.auth.json 被删)→ 无能力 → 主文案。
- 超时 **30 秒**(不在 SSH 握手关键路径上,比 pin 的 10 秒宽);同样 `pinningTransport`+不跟随重定向+`SplitTokenPin` 剥复合令牌。
- `Edit(serverID string, expectedRevision int64, fields map[string]*string) (MetaEditResult, error)`,返回 `MetaEditResult{ServerName, Revision, UpdatedAt}`。

### 2.1 分支映射表(钉死;错误文本逐字进 §11 测试)

| broker 响应 / 错误 | 客户端行为 |
|---|---|
| **200** | 返回 result;调用方(`server.go` 工具处理器)以**HTTP 调用之后实时解析的当前存储**执行 `ApplyForwardedMetadata`(§3) |
| **400 / 413** | 硬错;透传服务端文本并附 `<server_id>`(缺陷态) |
| **401** | 硬错,文案与 pin 转发共用(包内既有 `forwardMsg401`,一字不改) |
| **403 且 body 含 "disabled"** | 硬错:`metadata editing is disabled on the broker — ask the owner to enable it (serve --metadata-edit)` |
| **403(其余)** | 硬错:`the broker refused the metadata edit for <server_id> — the entry is probably not granted to this device's bound profile; ask the owner to grant it (or rebind via cache-tokens bind) and cache pull again` |
| **404** | 硬错:`the broker does not support metadata editing (the owner must upgrade the broker to >= v0.19.0, then retry)` |
| **409** | 硬错但**可自愈**:解析 body 得 current_revision+六字段现值,错误文本:`stale revision for <server_id> — the entry changed since your listing (current revision <N>); current values: role=<v> services=<v> …; merge your intent and retry with expected_revision=<N>`。调用方把现值完整透出给 agent(工具错误输出),agent 一跳内重试 |
| **5xx / 不可达 / 超时(30 秒)** | 主文案:`metadata edit for <server_id> could not reach the broker — the edit was NOT applied anywhere; retry while the broker is reachable (your local cache is unchanged)` |
| **pin 为空(明文 http)** | 拒绝发起:`metadata editing requires a pinned TLS server — no plaintext editing (set the server pin used by cache pull); nothing was changed` |
| **cache.auth.json 缺失/不可读** | 主文案 flavor(同 pin:无能力=不可达姿态,不静默降级) |

HTTP 请求体由工具输入构造:`fields` 仅含在场字段(§7 schema 天然白名单,键名即六个 json 字段)。

## 3. 本地镜像补记:第二个窄写口

`Store` 新增 `ApplyForwardedMetadata(serverID string, fields map[string]*string, revision, updatedAt int64) error`:**唯一**在只读态被允许写 `servers` 表的方法,契约钉死——

- 只应用**本次写入的字段**(在场键),`revision`/`updated_at` 取 **broker 响应值**(不是本地 now());
- **单调守卫**:`UPDATE servers SET <在场字段>, revision=?, updated_at=? WHERE id=? AND revision<?`——本地行 revision ≥ 响应值(热重建换入了别人已写过的更新代际)时 **RowsAffected=0 → 幂等成功**(本地已比响应新,不回退);行不存在 → 错误(`server <id> not in local cache — cache pull and retry`);
- 不写本地审计行(broker 的 meta-edit 行 IS the history,hostkey 同款);
- 不进 `ErrReadOnly` 分支(`ApplyForwardedHostKey` 同款窄缝;`UpdateServer` 本体在只读态**仍然** `ErrReadOnly`,护栏不松)。

镜像失败(行缺失/异常)**不影响结果语义**:broker 已提交=权威成功,工具输出改报 `mirrored=false` + 警示「本地缓存未能补记——run cache pull」。

## 4. 存储与快照:revision 列(跨版本协议)

- `servers` 加一列(守卫式 `ALTER TABLE ADD COLUMN … DEFAULT` 迁移,host_keys 三列先例;新建库的 CREATE TABLE 同步含列):`revision INTEGER NOT NULL DEFAULT 0`——既有行与既有快照缺省 0,天然回填。
- **单调不变式(全部写路径统一)**:一切 `UPDATE servers` 语句一律 `revision=revision+1`(`updateServerTx` 与 `UpdateServerWithCredentials` 的内嵌 UPDATE 都改);`insertServerTx` 写 0;`UpdateForwardedMetadata` 自身也走 `WHERE id=? AND revision=?` 的 CAS。owner CLI/TUI 编辑与设备转发写因此**共用同一把锁互斥**——这正是 Q4 的目的。
- `models.Server` 加 `Revision int64`;`scanServer` 与全部 SELECT 列清单(GetServerByName/ListServers/getServerTx/两条导出)补列。
- `SnapshotServer` 加 `Revision int64 `json:"revision"` `;`ExportSnapshot`(owner 备份)与 `ExportSnapshotForProfile`(缓存拉取)两条 SELECT 补列;`ImportSnapshot` 的 INSERT 补列**原样带回**(缓存水合必须保留 broker 的 revision,否则每次 pull 归零造成永久假 409)。旧快照(无字段)导入=0,兼容。
- `ServerInfo`(`list_servers` 输出)加 `Revision int64`,字段名 `revision`——agent 构造写请求的令牌来源。

## 5. 审计

- **broker 侧权威行**(与 UPDATE 同事务,回滚则都不成):`action="meta-edit"`,`server_id`=条目 id(`audit --server` 可查),`status="ok"`。Command 格式(在场字段按键名排序,确定性):
  `server=<name> fields=<k1,k2> device=<设备码名> via=meta-edit rev=<old>-><new> old.<k>=<旧值截断>`
- **旧值截断规则(Q9-B)**:每个被改字段取写前旧值,控制字符(C0+DEL)先净化为空格,再截到 ≤200 字节并按 UTF-8 边界回退,再以 `%q` 包裹。审计行总量有界(~六个字段×≤410B)。价值:agent 写坏数据时 owner 多半可照审计行手工改回;隔夜灾难由 Plan 49 每夜备份兜底。**新值不记**(行本体即新值)。
- **project_id 为空**:路由的已认证身份是设备码;「哪个项目在编辑」是客户端自称、服务端不可验证,拒绝采信(反伪造)。设备码名进 Command。OwnerOnly 巡检面混入设备发起行——与 pin-forward 同款,有意,以 action 前缀可筛。
- owner 侧 `server.edit` 审计今日已存在(白名单 summary),本 plan 只加 revision bump,审计格式不动。

## 6. serve 开关(第三元)

沿用四层解析(显式 env > 显式 flag > store 设置 > 默认),全部钉死:

| 层 | 键 |
|---|---|
| flag | `serve --metadata-edit / --metadata-edit=false`(经 `ServeOpts.MetadataEditFlag *bool`,Changed() 语义) |
| env | `SSHMGR_SERVE_METADATA_EDIT`(仅接受精确 "true"/"false") |
| store | `serve.metadata_edit`(Settings 面,批 1 不做写入面,owner 可直接 `settings` 命令/后续面) |
| 默认 | **true**(Q3-A——发版即生效;写这条是 spec 层面再确认一次) |

`RefreshSwitches` 签名扩为六参(+envMeta/flagMeta),`switchInputs`/`switchCache`/`rebuildSwitches` 各加一元,新增 `MetadataEditEnabled()`(≤5s TTL 缓存,与 Pairing/Discovery 同款)。闸在守卫 ③(认证之后、授予判定之前),关闭时 403 body 含 "disabled"。

## 7. MCP 工具:`update_server_metadata`(仅缓存脸)

- `BrokerTools` 追加 `[12] "update_server_metadata"`(eval 的 `isBrokerTool` 白名单自动含它,无 scorer 变化);**注册条件 = 转发器非 nil**:`NewServerFromSource` 签名加 `meta *clientops.MetadataForwarder`,nil → 不注册(权威脸 `NewServer` 传 nil,Q10-A;权威脸将来要加=传非 nil,共享处理器)。
- 输入 schema(六个可选字段**指针**语义,缺席=nil=保持,在场空串=清空——MCP 工具参数的缺席/在场空串端到端无损):
```go
type UpdateServerMetadataInput struct {
  ServerID         string  `json:"server_id"`
  ExpectedRevision int64   `json:"expected_revision"`
  Role        *string `json:"role,omitempty"`
  Services    *string `json:"services,omitempty"`
  Location    *string `json:"location,omitempty"`
  Hardware    *string `json:"hardware,omitempty"`
  Caveats     *string `json:"caveats,omitempty"`
  Description *string `json:"description,omitempty"`
}
```
  六指针全 nil → 工具错(`provide at least one of the six metadata fields`);构造 HTTP `fields` 映射时仅含非 nil 项。
- 处理器流程:①全 nil 检查 → ②`meta.Edit(...)` → 错误 → IsError 输出分支文案(409 时含 current_revision+现值,agent 一跳重试) → ③成功 → `storeFn()`(HTTP 调用后实时解析,T5b 纪律)`ApplyForwardedMetadata` → 失败 → 输出 `mirrored:false`+警示 → ④输出 `{server_id, server_name, revision, updated_at, mirrored}`。
- 工具描述文案(教 agent 语义,实现时定稿进 `server.go`):省略=不改、空串=清空、revision 取自 list_servers、409 时按返回现值合并意图后带 current_revision 重试、本工具只在缓存模式存在。
- 接线:`cli/mcp.go` `--instance` 解析处并排构造(`NewPinForwarder` 同点),穿 `RunStdioCache`/`NewCacheBroker`/`newCacheBroker` 三签名到 `NewServerFromSource`;`mcp --cache` 非 cache 路径不受影响。

## 8. 版本与打包

- 捆发 **v0.19.0**(假定;发版是 owner 决策)。
- 兼容矩阵:新客户端+旧 broker → 工具 404 → §2.1 升级文案;旧客户端+新 broker → 无变化(新路由/新列/新 JSON 字段全部可忽略);新新 → 全功能。快照 revision 字段对旧端可忽略(`SnapshotServer` 加列不破坏旧解码)。

## 9. 测试矩阵

| # | 测试(归属包) | 断言 |
|---|---|---|
| T1(store) | 迁移加列;`updateServerTx`/`UpdateServerWithCredentials` bump revision;`UpdateForwardedMetadata`:CAS 成功(revision+1/updated_at 变/在场字段改/缺席字段不动)、stale → `ErrStaleRevision{Current}`、行缺失 → ErrServerGone、字段超限 → 带字段名错误、审计行与 UPDATE 同事务(注入审计失败 → 两者皆不落)、Command 含旧值截断(rune 边界/控制字符净化/%q) | — |
| T2(store) | `ApplyForwardedMetadata`:只读态可用(第二个窄缝)、只应用在场字段、revision/updated_at 用响应值、本地 revision≥响应值 → 幂等成功不回退、行缺失 → 错误、不写本地审计行;`UpdateServer` 只读态仍 `ErrReadOnly` | — |
| T3(store) | 快照:revision round-trip(两条导出+Import 原样带回);旧形态快照(无字段)导入=0;水合后 CAS 与 broker 一致 | — |
| T4(mcpserver serve) | 路由:200 happy(字段/审计行/stderr 行/device 归属);409 body=error+current_revision+current 六字段;403 授予外(含措辞,不回显轮廓);403 未绑定;403 开关关(body 含 disabled);400 白名单外键/null 值/fields 空/超 4 KiB/JSON 坏;401 坏令牌;405;413;GetCacheToken 故障 → 500 | — |
| T5(mcpserver switches) | meta 开关四层优先级;默认 true;TTL 缓存;RefreshSwitches 六参注入 | — |
| T6(clientops) | 转发器分支文案逐字(§2.1 全表);409 解析出 current_revision+现值;明文拒绝;无凭据主文案;30s 超时主文案;不跟随重定向 | — |
| T7(mcpserver 工具面) | 缓存脸(meta≠nil):工具注册、happy path(转发+本地镜像+输出 mirrored:true)、409 错误输出含现值、镜像失败 → mirrored:false+警示、全 nil → 工具错;权威脸(NewServer):**工具不注册**(面数断言) | — |
| T8(cli) | `mcp --cache` 构造注入(实例解析处);cache.auth.json 缺失 → 工具在但 Edit 走主文案;`serve --metadata-edit` flag 穿到 RefreshSwitches | — |
| T9 | 回归:`go test ./...` 全绿;`gofmt -l` 空;`go vet` 净;list_servers 输出含 revision(listmetadata 系扩展);eval 零改动过 | — |

## 10. Non-goals(批 1 明确不做)

批 2 客户端 CLI/TUI 编辑表单(同路由,backlog 登记);tags/连接字段开放(Q2-A);按设备或按 profile 的细粒度开关(全局一档);离线暂存/排队(Q5-A);审计记旧值全文或新值(Q9);值历史表(回滚=审计截断+每夜备份);GET 单条元数据路由(409 已带现值);409 自动重拉(现值自愈已覆盖);元数据写限速;权威脸工具注册(Q10-A);TUI Settings 面的开关写入位(store 键已就绪,面属批 2)。

## 11. 实施触点(文件级)

| 文件 | 变更 |
|---|---|
| `internal/store/store.go` | `servers` 加 `revision` 列(守卫迁移+新库 CREATE TABLE) |
| `internal/store/servers.go` | `models.Server`→scanServer/SELECT 列清单补列(注:struct 在 models);`insertServerTx` 调用处带 0 |
| `internal/store/tx.go` | `updateServerTx`+`UpdateServerWithCredentials` 的 UPDATE 语句 `revision=revision+1`;`insertServerTx` 列清单 |
| `internal/store/metadata.go`(新) | `UpdateForwardedMetadata`(单事务 CAS+同事务审计+旧值截断 Command 构造)、`ApplyForwardedMetadata`(只读窄缝+单调守卫)、`ErrStaleRevision` |
| `internal/store/export.go` | `SnapshotServer.Revision`;两条导出 SELECT;`ImportSnapshot` INSERT |
| `internal/models/models.go` | `Server.Revision int64` |
| `internal/mcpserver/types.go` | `ServerInfo.Revision int64` |
| `internal/mcpserver/core.go` | `ListServersForProfile` 填 Revision |
| `internal/mcpserver/serve.go` | 路由分发+`handleServerMetadata`(守卫①-⑦+stderr 行)+`metaMaxBodyBytes`/请求响应类型 |
| `internal/mcpserver/switches.go` | 第三开关全套(键名/默认/RefreshSwitches 六参/MetadataEditEnabled) |
| `internal/mcpserver/server.go` | `BrokerTools[12]`;`NewServerFromSource` 加 meta 参(条件注册);工具 schema/描述/处理器;`UpdateServerMetadataInput/Output` |
| `internal/mcpserver/run.go` | `RunStdioCache`/`NewCacheBroker`/`newCacheBroker` 穿参 |
| `internal/clientops/metadata_forward.go`(新) | `MetadataForwarder` 全范本(§2.1 分支表+文案) |
| `internal/cli/mcp.go` | `--instance` 处并排构造注入 |
| `internal/cli/serve.go`(或 serve 命令所在) | `--metadata-edit` flag → `ServeOpts.MetadataEditFlag` |
| 测试 | T1–T8 各就各位 |
| `docs/agent-tools.md`、`docs/multi-machine.md` | 工具文档+路由/开关说明 |
| `docs/backlog.md` | 批 2 表单+Settings 开关面登记 |

## 12. 真机验收(发版后)

NUC10(升级)+ 笔记本(同步):① 笔记本 agent `update_server_metadata` 改一笔 → NUC10 `sshmgr audit` 见 meta-edit 行(设备名+旧值截断+rev 箭头)→ `servers ls`/TUI 见新值;② 并发互踩:owner 在 NUC10 TUI 改同一字段与 agent 写交错 → 后到者 409/被拦,无静默覆盖;③ 开关:NUC10 `serve --metadata-edit=false` 重启(或 env)→ 笔记本工具得 disabled 文案 → 恢复;④ 断 broker → 主文案,本地缓存无恙;⑤ NUC10 自身 agent 面(`mcp` 权威脸)确认**无**此工具;⑥ 拉取一致性:写成功后另一台设备 `cache pull` → revision 与 broker 一致。

## 13. 参照

- Plan 48 spec rev3(锚定转发——闸门/守卫/窄缝/文案纪律全部母版)与 ADR 0002;Plan 39(profile 范围);Plan 34(401 隔离语义边界);Plan 31(不泄露库形状);Plan 49(每夜备份=灾难兜底);`internal/mcpserver/switches.go`(四层开关);`internal/clientops/forward.go`(转发器范本)。
