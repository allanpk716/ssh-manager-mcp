# Backlog（P0/P1 已裁决待开工，P2 已裁决、未排期）

- 2026-08-21 grilling 缺口分析会话（议题：满足项目目标——接口级不暴露 IP/端口/凭据 + 日常 agent 使用——还缺什么）产出 P0/P1 排期队列（#12-#16 + #3 提级），P2 维持"已裁决、未排期"。
- P2 及历史决策记录在案（xcheck 收敛 2026-08-16 / Plan 25），均为 owner 拍板"暂不改行为"。
- 编号稳定：老条目号码不变，#1/#2/#4 已并入 #15（留墓碑），#9 已随 Plan 30 移除，新条目顺延 #12-#16。
- 排序逻辑：目标债（接口级不暴露承诺正被违反）→ 日常功能解锁 → 安全债 → 便利性。
- 明确不做/暂缓清单见文末（scope 纪律留痕）。

## P0 — 日常使用「没有」级缺口（2026-08-21 新增，已裁决待开工）

12. ~~**list_servers host 掩码 + 错误路径清洗**（v0.9 破坏性变更）~~ **已落地（Plan 31, merge aac8555, 2026-08-21 发版 v0.9.0, 双端验证回写 447435d）**。原文：按 server 粒度 `expose_host` 布尔，默认 false；false 时 `list_servers` 不回明文 host（现状：`core.go:54` 原样返回，全仓无掩码逻辑；`ServerInfo` 无 Port 字段，端口本就未暴露）。错误路径清洗：connect_error 等返回给 agent 的文本不得带 `host:port`。目标定义措辞回写：concepts.md / threat-model.md 写明"接口级不暴露"是承诺边界（agent 主动跑 `ip addr` 探出的不算违约），运行时级隐藏与服务器出网管控明确不做。发布：v0.9 直接翻默认值，compat-matrix.md 登记。验收：expose_host 两态单测 + connect_error 文本无 host 断言 + 手工双端验证。

13. ~~**后台任务三件套**（exec_background / exec_output / exec_stop）~~ **已落地（Plan 32, 2026-08-22 并 master; v0.10.0 发版+双端部署待 owner; spec/plan 见 docs/superpowers/{specs,plans}/2026-08-21-plan-32-background-tasks*）**。原文：`exec_background(server_id, command, sudo, timeout_seconds)` → task_id；`exec_output(task_id, wait_seconds?, since_offset?)` → 增量输出 + 运行/结束状态；`exec_stop(task_id)`。对齐 Claude Code Bash 工具体验，agent 零学习成本。任务表：broker 进程内存（TunnelManager 同款模式），进程重启即失，文档明示。生命周期：运行上限 24h；完成后保留 1h 供取尾输出；每通道滚动保留最后 1 MiB；增量用字节 offset 游标（tail -f / journalctl -f 场景 = 反复 exec_output 拉增量，不做流式推送）。前台 exec_command 保持 5 min 硬顶（现状 `MaxExecTimeout`，长活全走后台），但静默钳制改"响"：返回体加 `effective_timeout_seconds`。文档：agent-tools.md 补 `cd /dir && VAR=x cmd` 惯用法（exec 不加 env/workdir 参数，agent 可自行组合）。验收：后台生命周期（启动/增量/取消/超时/收割）conformance/eval 覆盖 + effective_timeout 钳制响应断言。

14. ~~**upload_content 跨机小文件上传**——新工具：内容内联（≤8 MiB）写入远程路径。~~ **已落地（Plan 33, 2026-08-24 并 master; v0.10.0 未发版——并入 v0.10.0 行还是开 v0.11.0 留 owner 发版拍板（compat-matrix 占位注释回写时删）; spec 三轮 xcheck 收敛 rev3 定稿 + 8 任务 SDD + 整分支终审 Ready; 跨机端到端（笔记本→NUC10→目标机）owner 手工验收 + eval T10/conformance 门内实跑留 owner/CI 门）**。原文：现状缺口：`upload_file` 的 local_path 是 broker 本机路径，笔记本 agent → NUC10 serve 拓扑下无法上传本机文件（download 是内容回传、跨机可用，上/下载不对称，S1 配置下发场景残缺）。download 维持 1 MiB 前缀截断（大文件全文进 agent 上下文是反模式，不鼓励）；agent-tools.md 教 exec head/tail/grep 切片读法。验收：跨机拓扑端到端（笔记本→NUC10→目标机）+ 超限拒绝分支。

## P1 — 安全债 + 第二梯队（2026-08-21 排期）

3. ~~**离线 cache 快照失效机制**（snapshot epoch/serial）~~ **已落地（Plan 34, 2026-08-24 并 master; spec 四轮 xcheck 收敛含 owner scope 降级——A 切断失效落地（pinned-401 回连销毁四件/manifest/DEGRADED/三级降级报文链），**B 时限机器砍出回 backlog 见下方「B 时限快照」条**； spec/plan 见 docs/superpowers/{specs,plans}/2026-08-24-plan-34-cache-invalidation*；owner 真机手工复验（NUC10 revoke→笔记本销毁→重新 enroll）待发版前做）**。原文：现状：revoke/rotate 不擦已落盘快照，唯一失效手段是轮换服务器凭据；`cache-tokens revoke` 只断"拉新"。排期注记（2026-08-21）：威胁模型 (b)（prompt injection）下的**切断失效**缺口——revoke 后笔记本盘上快照仍可用；安全债排在便利性前、P0 之后（兑现需"笔记本失窃/被控 + 已 revoke"复合前提，而 P0 是日常持续疼）。

3b. ~~**B 时限快照（Plan 34 砍出项）**——离线到龄自废（SSHMGR_CACHE_MAX_OFFLINE 形态）。~~ **已落地（Plan 37, 2026-08-25 并 master; spec 三轮 xcheck 收敛 rev3 定稿[owner 突破 C5 ×1，书面级修订免复审] + 3 任务 SDD + 整分支终审; 三件配套落法——服务器时间锚 = 标准 Date 头仅 pinned 采信 + skew 闸 + 缺头 fail-closed、跨进程写串行化 = 设计性消解（锚并入 meta.PulledAt 单写者，无锁）、前向毒化自愈 = 采信口闸门 + 可重写锚; 复审轮实验实证 pinned 302→明文重定向洞 → 全局禁重定向顺带修复; spec/plan 见 docs/superpowers/{specs,plans}/2026-08-25-plan-37-cache-expiry*；owner 真机手工复验（1h 到龄销毁→重拉恢复）待发版前做）**。原文：离线到龄自废（SSHMGR_CACHE_MAX_OFFLINE 形态）。Plan 34 四轮评审实证完整正确性需三件配套：服务器时间锚信任边界（仅 pinned 采信 + skew 校验 + 缺头 fail-closed）+ 跨进程写串行化 + 前向毒化自愈；修复面连续膨胀且为默认关可选件，owner 2026-08-24 拍板砍出。未来要做按 Plan 34 二/三轮评审结论起手（.xcheck/ 评审留底 2026-08-24）。

15. ~~**tunnels 硬化**（吸收原 #1/#2/#4，一个 plan 落地）~~ **已落地（Plan 35, 2026-08-25 并 master; v0.10.0 未发版——与 Plan 32/33/34 同批发版，版本行归属 owner 发版拍板（compat-matrix 占位注释回写时删）; spec 四轮 xcheck 收敛 rev4 定稿 + 7 任务 SDD; 契约翻转钉子（TestRevokedProjectTunnelsTornByControlTick）+ conformance kill 急停端到端（docker 真 OpenSSH + 真 owner CLI，门内实跑绿）+ eval T11 门内用例（gated，SKIP 为正常出口）齐备; owner 真机手工复验待发版前做：① serve 拓扑真机 `tunnels kill` 急停（端口不可达）② revoke/disable 级联拆除 ③ `serve bind rm` 白名单存量收缩）; spec/plan 见 docs/superpowers/{specs,plans}/2026-08-25-plan-35-tunnels-hardening*。原文：急停：revoke 级联拆隧道 + owner CLI（`tunnels kill <id>`）。活动感知回收：持续流量延长回收（落实 Touch，现状回收按创建时间计）。listen_host：serve 配置 owner 预批 bind host 白名单（如 NUC10 VLAN IP），per-tunnel 参数限白名单内，禁 0.0.0.0——绑定非环回 = 攻击面扩张（威胁模型 (b) 被劫持 agent 开隧道打内网），必须与急停同 plan 落地。不做：持久化、自动重连、命名隧道。验收：白名单外 bind 拒绝；急停后端口不可达；持续流量下不回收。

16. ~~**audit CLI 读路径**——`ssh-manager audit`，owner-only；**明确不做 MCP 工具**（审计行含其他 agent 完整命令原文、可能带 secret，给 agent 读 = 把同伙作案记录递给被劫持 agent）。过滤 `--since --server --action --status`；首版只读 vault 主库 audit_log 表；离线 sidecar（cache-audit.log）本身是 JSONL，cat/grep 可用，不做跨端聚合。验收：过滤组合断言 + 无 MCP 暴露面。~~ **已落地（Plan 36, 2026-08-25 并 master; v0.10 系第五个 plan——与 32/33/34/35 同批发版，版本行归属 owner 发版拍板; spec 三轮 xcheck 收敛 rev3 定稿[owner 突破 ×1] + 3 任务 SDD 全 Approved + 整分支终审 Ready-with-fixes→修复确认; 验收双落地：过滤组合断言[store/CLI 两层，含多值 IN] + 无 MCP 暴露面[BrokerTools 单源钉子 + e2e tools/list 联锁]; brainstorm 增补 --project/--owner 过滤器、--since 双语法、人读+--json 双形态均 owner 拍板; spec/plan 见 docs/superpowers/{specs,plans}/2026-08-25-plan-36-audit-cli*）**。原文：`ssh-manager audit`，owner-only；不做 MCP 工具；过滤四维；只读 vault 主库；sidecar 不聚合。

## P2 — 已裁决、未排期

1. ~~serve 隧道 owner 急停~~ → 已并入 #15（2026-08-21 合并排序）。
2. ~~隧道回收活动感知~~ → 已并入 #15。
4. ~~受控监听地址~~ → 已并入 #15。
5. **doctor serve 探活二期**（绿/黄/红语义）——现状：doctor 首版只做本机自检（P4，独立 plan）。
6. ~~**Windows DACL readback 检查**~~ **已落地（Plan 38, 2026-08-26 并 master; spec 三轮 xcheck 收敛 rev3[owner 特批第三轮人工终审] + 3 任务 SDD; 白名单 read-capable 授权 + owner 异常信号（null DACL / 非白名单读授权 / owner 越白名单 → masterkey WARN 分段冻结文案 + icacls 星号 SID fix；SD 读不可 = FAIL；WARN 不改退出码），seam 跨平台腿 + Windows 真路径腿双覆盖）**。原文：现状：doctor v1 在 Windows 上对 master.key 的硬化校验只有 32 字节长度（Unix 上是权限位），没有真正的 DACL 读回校验；`internal/store/acl_windows.go` 的 `getDACLForTest` 是 test 名构件，产品化需要生产命名的包装 API 才能进 doctor。
7. ~~**doctor 退出码 2 的接线**~~ **已落地（Plan 38, 2026-08-26 并 master; spec 三轮 xcheck 收敛 rev3[owner 特批第三轮人工终审] + 3 任务 SDD; ExitCodeError 管道（NewExitCodeError / ExitCodeFor + main.go os.Exit 接线）+ doctorExitCode 双真相源根除（删除）；2 的产源/帮助文本留 #5（两项预埋验收））**。原文：现状：`doctorExitCode` 的 2（内部错误）分支保留且被测试钉住，但 main.go 把所有 cobra 错误映射为 exit 1，用户可见契约是 0/1；doctor 将来长出会内部出错的检查（如二期 HTTP 探活）前，必须先把 2 接进真实退出路径，避免保留码静默烂掉。
8. **TestConnectCancelContext Windows wsarecv 间歇 flake**——现状：`internal/sshbroker` 该测试在 Windows 偶发 wsarecv 竞态（2026-08-17 本地全量首跑中 1 次，重跑即绿；CI windows lane 连续绿），修法大概是 retry 式稳定化，与本 repo 任何在飞分支无关。**同族新例（2026-08-27）**：`internal/mcpserver` TestExecOutputStopToolRegistered（bgtools_test.go "task did not reach done within 5 polls"）ubuntu lane CI 首跑红、`--failed` 重跑即绿（30bba37 docs-only commit——同代码树当日两轮绿，纯时序 flake）；修法同方向：固定轮询数改 deadline 轮询。
10. **TUI 测试套件耗时 ~89s**——现状：Plan 30 后 `internal/tui` 从 ~3s 涨到 ~89s（editpage ~71s + wizard 回环 ~7s + e2e ~7s）：drain 测试 helper 同步执行 huh Focus 的 cursor blink `tea.Tick` 闭包（530ms 包级 const），睡完才丢弃 BlinkMsg；纯测试墙钟，零生产影响（生产 cmd 在 runtime goroutine 执行）。跟进方向（Plan 30 终审裁决）：`press()` 对普通字符键免 drain（field 态字符键只产 blink 重臂 cmd）和/或睡眠主导测试加 `t.Parallel()`；**BlinkSpeed 测试 seam 已证不可行**（cursor.Model per-instance 字段，huh 构造器不暴露内部 cursor）。
11. **TUI 表单光标常亮（闪烁宣称撤回）**——现状：Plan 30 曾宣称"表单内光标闪烁恢复"，真终端实测常亮。探针实证（2026-08-19）：blink 消息链活着（自续多轮）但表单视图从不切换——huh v2.0.3 `Group.Update` 对聚焦字段双重更新产生两条竞争 blink 血统，与 bubbles cursor 的 id/tag 防重机制相互作用，Set 相互覆盖致渲染恒定；属 huh/bubbles v2 嵌入式表单上游行为，本仓代码全程未设 cursor 模式。跟进方向：升级 huh/bubbles 后复验（复用探针：连喂 BlinkMsg 比对 form.View() 变化）。

## 明确不做 / 暂缓（2026-08-21 grilling 留痕）

- **PTY 交互式会话**：LLM agent 不擅长无界交互流，输出无界 + 安全面暴涨；长活一律走 #13 后台任务。
- **按 server 的 token 授权**：更正记录（2026-08-21 grilling 曾误判为缺口，代码核实后撤回）——**已存在**：Project（token）→ Profile → Server 三层，token 解析出 project 即限定其 profile（`NewServer(st, project.ProfileID, ...)` 构造即生效，run.go / serve.go / 热重载 drift guard，eval T5 对抗覆盖）。无需新增工作。
- **大文件分块续传 API**：#14 的 8 MiB 内联覆盖配置/脚本/小产物；更大的先落到 broker 可达位置或服务器侧拉取。
- **跨端审计聚合**：等真实取证需求再说（sidecar 本身是 JSONL）。
- **隧道持久化 / 自动重连 / 命名**：见 #15 不做项。
- **运行时级 IP 隐藏**（命令过滤/输出脱敏/网络盲化）与**服务器出网管控**：不可行且杀死可用性；"不暴露"承诺边界 = 接口级（vault/工具层不主动披露）。
- **(d) 类加固**（agent 宿主机整体沦陷，anti-debug/内存加密等）：现有 broker 架构（本机零明文凭据、凭据只在权威端 vault）即该层最优解，不再追加投入。

## Plan 39 残余登记（2026-08-26）

- **一机多 profile 不支持**：设备码一对一绑定 profile（一机一码一 cache.bin）；要不同授权范围的 agent 需在不同机器或共用同一 profile。多 profile 快照合并会引入合并语义，违背"零合并"基线——暂缓，等真实需求。
- **server TUI 停留页上时的外部写入不自动刷新**：Tab 切页重读已落地（Plan 39）；定时 tick（停留即实时）收益边际，暂缓。
- **永离线机器的旧整库 cache.bin 不受 Plan 39 追溯**：re-pull 才被裁剪快照原子覆盖；必要时吊销旧码强制重 enroll（revoke → 回连销毁 → 新码重拉）。

## Plan 39 code-review 残余（2026-08-26，用户裁决：进 backlog）

- **Tab 切页重读为同步 FetchAll**（app.go Tab 分支）：在 bubbletea 事件循环内同步跑 4+ 查询（MaxOpenConns(1) + busy_timeout 5s），serve 进程并发写时按键最坏卡 ~5s；且每次切页清掉各页 `/` 过滤与光标（仅 servers.warnOnly 保留——actionDoneMsg 既有语义的延续）。跟进：异步化（tea.Cmd）+ 按页保留过滤/光标。
- **refetchPages 吞 FetchAll 错误**（失败时静默显示旧页当新的）：应置 a.err/status 提示重读失败。
- **resolveProfileID/profileNameByID 与 projects.go 的 profileIDByName/profileNameMap 重复实现**（cli/cache_tokens.go）：同一包内两套解析器会漂移；且 `cache-tokens ls` 对 T 个码做 T+1 次全表 ListProfiles（N+1）。
- **profile 存在性预检三处粘贴**（AddCacheToken / BindCacheToken / ExportSnapshotForProfile 各一份 `SELECT COUNT(*) FROM profiles`）：GetProfile(nil,nil) 已有；语义变化时三处易漏一。
- **profileLabel 双实现**（cachetokens.go 页方法 + 条目方法，逐字节相同）+ 条目冗余携带 names map：应合并单 helper。
- **0-profile 守卫双写**（app.go 设备码页 [a] vs upgrade.go 升级段），文案尾部已漂移：应抽共享。
- **clientHeader 的 nServers 参数与 snap 冗余**（恒等于 len(snap.Servers)）：删参改内部计算（nil-safe）。
- **pre-Plan-39 cache_tokens DDL 在两个测试文件裸抄**（cli/cache_tokens_test.go + mcpserver/serve_snapshot_test.go）：store 侧已有具名 oldShapeCacheTokens；跨包共享 fixture（testsshd 先例）防下次迁移漏改。
- **e2e 测试忽略 CreateTemp/GenerateMasterKey 错误**（mcp_cache_test.go，沿用既有测试风格）：环境故障时 nil 解引用掩盖根因。

## Plan 40 多实例：第一批销项与登记（2026-08-26）

- ~~**多实例离线缓存（第一批 · CLI 闭环）**~~ **已落地（Plan 40 batch 1, 2026-08-26 实现完毕；v0.11.0 未发版——与先行合并的 P0 锚修复（§1，merge 8643858）同船发版；spec = `docs/superpowers/specs/2026-08-26-plan-40-multi-instance-cache-design.md.rev3.md`，plan 见 `docs/superpowers/plans/2026-08-26-plan-40-multi-instance-batch1.md`）**。原文（2026-08-26 grilling Q3-Q11 定案）：同机多 agent 各授权各 profile 的多 cache 实例一等公民（owner 明确否掉"仅文档化 workaround"）——实例 = 设备码 name = profile 三位一体；命名实例落 `UserConfigDir/ssh-manager/instances/<name>/`，per-instance DEK `VaultDir/cache-dek-<name>.key`（新 seam `SSHMGR_CACHE_DEK_DIR`）；`--instance` flag（pull/status/mcp）+ env×flag 互斥；serve 下发 `X-Sshmgr-Device-Name` 头；默认实例身份门禁三分支（异码写盘前拒 / 存量空 `device_name` 补记 / meta 损坏拒）；MAX_OFFLINE 持久化 `cache.config.json`（env > file > off）；`clear` 清实例树 + DEK 变体、`roles` 认命名实例机；双实例 e2e 钉 DEK 隔离 / 交叉 fail-closed / 吊销只毁本实例。
- **第二批（已裁决待开工）**：① TUI client 页实例列表 + 切换；② 角色向导 `--instance` 接入卡（生成带 `--instance` 的 `.mcp.json`）；③ **首次 enroll 自动归位**（落 `instances/<响应头name>/`）——与向导卡同批，消除"归位先行则新机开箱即起不来"的中间窗口；④ TUI 连接编辑表单换码**预防性警告**（第一批该路径由门禁拒绝文案兜底——非静默但体验欠佳）；⑤ 独立 `cache config [--instance] --max-offline` 子命令（现只有 `pull --max-offline` 顺带写）。
- **doctor 二批（跟随 Plan 38-doctor 体系）**：doctor 感知命名实例（枚举全部实例诊断）——第一批不感知：仅命名实例的机器 doctor 的 client-cache 检查会报"cache 缺失"（roles 已修为 client；不静默但属误报）。
- **明确不做（YAGNI，Q11 拍板）**：`--cache-max-age`（cacheMaxAge）**不进 `cache.config.json`**——持久化只搬 MAX_OFFLINE。理由：cacheMaxAge 是进程内 lazy-pull 节流参数，无"跨进程策略一致性"病灶（MAX_OFFLINE 的 env 铺不满所有进程问题与它不同构）。
- **残余登记（如实）**：① 存量机器**永不自动迁移**实例目录——自动归位仅第二批起、且只作用于首次 enroll；要进实例形态需显式 `--instance` 重新 enroll（零迁移承诺的有意代价）；② 同机恶意进程可读全部实例材料——与单实例现状同级（L1+ 文件 ACL 不挡同机），非新增；③ 跨进程 casefold 双插窗口——查重已入事务 + 进程内 `MaxOpenConns(1)` 串行，两个独立进程恰好同时 add 互为大小写变体的理论窗口仍在（owner 单人操作面接受；发生时 client 物理碰撞检测兜底拦截）。
