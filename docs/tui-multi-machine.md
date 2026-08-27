# TUI 教程 · 联机版（serve 多机共享）

> **读者**：server 机（常驻 broker）和工作机（client）两边的操作者。
> 架构/runbook 深水区（TLS 迁移、证书轮换、export/import）在
> [multi-machine.md](./multi-machine.md)——本篇只讲 TUI 怎么点。
> 单机场景看 [tui-single-machine.md](./tui-single-machine.md)；CLI 视角的速通看
> [quickstart-multi-machine.md](./quickstart-multi-machine.md)。

## 全景图

```
 ┌──server 机（你在这台操作：向导 + 主控台）──────────────────┐
 │  ssh-manager tui                                      │
 │   ├─ 首跑向导（角色选 server）                          │
 │   │   vault + 服务器清单 + profile + project            │
 │   │   密钥 1/2 project token ──┐   密钥 2/2 设备码 ──┐  │
 │   ├─ serve 服务（安装向导代办，常驻 0.0.0.0:7878）      │  │
 │   └─ 主控台四页签（含设备码页 [a] 签发 / [d] 吊销）      │  │
 └──────────────────────────────┼──────────────────────┼──┘
                        抄到工作机的 .mcp.json │        │ 抄到工作机的向导
                                │            │        │ （拉缓存用，只进 /snapshot）
                                ▼            │        ▼
 ┌──工作机（你在这台操作：client 向导 + client 面板）──────────┐
 │  ssh-manager tui（角色选 client）                       │
 │   ├─ 连接表单：serve 地址 + 设备码 + pin ◀── 接入卡带过来 │
 │   ├─ 首次 pull → cache.bin（本机 DEK 加密，只读快照；    │
 │   │   真空机首次 enroll 自动归位进 instances/<name>/）    │
 │   ├─ finish 屏：.mcp.json 双形态（离线 --cache / 在线 http，
 │   │   归位/显式实例时 args 自动带 --instance）◀─ 密钥 1/2 在这用
 │   └─ client 面板：[s]同步 [i]实例 [c]编辑连接 [t]TTL（零远程写）│
 └─────────────────────────────────────────────────────────┘
```

两台机、两把密钥、两道独立的闸门（与 multi-machine.md 一致）：**project token**
驱动 agent 的 MCP 工具调用（进 `.mcp.json`）；**设备码**只授权拉取 `/snapshot`
缓存。两者永不互通，一台设备码被吊销不影响任何 project token。

## server 侧走查

空机第一次运行 `ssh-manager tui` 进入首跑向导（启动方式、mintty/non-TTY 注意事项
同 [tui-single-machine.md §1](./tui-single-machine.md#1-启动)）。首屏问后果：

> **这台电脑要保管所有 SSH 凭据吗？**
> - 是——凭据只存这台机（其他机器不能用了它就都用不了）
> - 否——凭据在另一台机器上，这台只连它 → client（需先在 server 机完成设置）

选**是**；第二问选**要给其他机器共享 → server**。**选定的瞬间 role.json 已落盘**，
此后任何时刻 `q` / `Esc` / `Ctrl+C` 都是安全暂停，重跑 `tui` 从断点继续。
（已配好单机的机器不用重跑向导：主控台页脚直接有 `[u]升级为 server`。）

1. **客户端机器名**——「客户端机器名（将命名 profile 与设备码；填对方电脑的名字）」。
   一个名字两处用：它就是后面 profile 的默认名，也是设备码的名字（吊销时靠它认人）。
2. **服务器录入循环**——「现在录入第一台服务器？」可跳过（跳过 = profile 暂无成员，
   agent 将看不到任何服务器；之后可在主控台随时补录）；录了可「继续添加下一台？」。
   表单字段与单机版完全一致（密码/私钥互斥、可都留空 = 无凭据 ⚠，见
   [tui-single-machine.md §2](./tui-single-machine.md#2-首跑向导走查可中断续配)）。
3. **Profile 名称 + 授权多选**——名称默认 = 第 1 步填的客户端机器名（重名自动加
   `-2`）；服务器多选空格勾选、回车提交。
4. **项目名称**（默认 = 本机主机名；即发给 agent 的「通行证」身份）。
5. **密钥 1/2：project token**（一次性全屏）——用途行原文「贴到 **client 机**
   .mcp.json 的 SSHMGR_TOKEN 字段」；「⚠ 仅此一次。丢失 → 主控台 Projects 页
   [a] 重发」。**当场抄下来**。
6. **密钥 2/2：设备码**（一次性全屏）——用途行原文「填到 client 机向导；或拼
   cache pull --token '<设备码>:<指纹>'」（设备码旁附 serve 证书 SPKI 指纹，即
   client 表单里的 pin）；丢失 → 主控台 设备码页 [a] 重发。同样**当场抄下来**。
   向导签的码**自动绑定第 3 步建的 profile**（Plan 39：该设备拉到的就是那份装箱单）。
7. **serve 地址**——「client 通过哪个地址访问这台 server？（选定地址会写进客户端
   接入卡）」：本机非环回 IPv4 列表单选，默认第一项（形如 `https://192.168.1.10:7878`）；
   无 IPv4 的特殊环境退化为手输，校验必须 `https://` 开头（serve 只说 TLS）。
8. **安装 serve 服务（需要管理员权限）**——前置提示原文要点：服务绑定
   `0.0.0.0:7878`（不是选定的那个地址——选定的地址只写进接入卡，监听永远全网卡）；
   Windows → Windows 服务、Linux → systemd、macOS → launchd；**若失败向导不阻断**
   ——会显示可手动提权执行的原文命令并继续验证服务状态。安装结果**不中断流程**：
   失败横幅给出手动命令（Windows：管理员终端跑 `ssh-manager serve install
   --addr 0.0.0.0:7878`；Linux/macOS：`sudo` 同款命令），随后照常探活。
9. **serve 安装结果屏**——两行判定：安装（✓ 已装并启动 / ✗ 失败+手动命令）+
   探活（✓ 已就绪 / ⚠ 未验证：「排查：7878 端口防火墙是否放行；`ssh-manager
   serve status` 查四项信号；服务可能仍在启动，稍候重试」）。**任一行失败都不拦路**，
   流程照走。
10. **客户端接入卡**——把这张卡带到 client 机：`地址` + `指纹` 两个明文值 + 两个
    密钥的去向表（密钥本身不再重显）+ 命令式备选 `ssh-manager cache pull --url
    <地址> --token '<设备码>:<指纹>'`。按任意键完成设置，直接进入主控台。

主控台四页签的完整键位见 [tui-single-machine.md §3](./tui-single-machine.md#3-主控台四页签)。
**Tab 切页即重读**（Plan 39）：页签数据可能被本进程之外的写入改动（serve 进程在每次
client pull 后更新设备码的「最近拉取」、其它 TUI/CLI 会话增删实体）——切到哪页就重读哪
份，所见即 DB 当前值；已停留在页上期间的外部写入需切走再切回才可见。
联机部署下**设备码页签这时有用了**：

| 键 | 动作 | 说明 |
|---|---|---|
| `a` | 签发设备码 | 表单三字段：设备名称（如 laptop，吊销后可重发同名）+ **绑定 profile**（下拉——决定该设备能拉到的服务器范围，Plan 39）+ serve 地址（仅用于生成使用提示，不保存）。提交后设备码 + 指纹 + `cache pull` 示例命令一次性全屏显示 |
| `d` | 吊销设备码 | 确认「该设备下次拉取将被拒绝」（Lazy 生效）——机器失窃处置第一步，见下 |

Projects 页的 `[a]` 新增 / `[e]` 轮换 / `[d]` 吊销在联机场景就是给各工作机发/换
project token 的入口（见「典型任务」）。

## client 侧走查

**前提：server 机已完成上面的设置**（serve 在跑、手上有接入卡或两把密钥的抄录）。
工作机上空机第一次运行 `ssh-manager tui`，首屏选**否——凭据在另一台机器上，这台
只连它 → client（需先在 server 机完成设置）**。

1. **连接表单**（立即打开，表单上方有源提示行原文「ℹ 设备码与服务器指纹在
   server 机 TUI『设备码』页签发」）：
   - `serve 地址`——`https://` 开头才收（明文 `http://` 永远进不了本机配置）；
   - `设备码（留空=保持不变）`——**掩码显示、从不预填**（重开表单时留空 = 沿用
     本机已存的码）；
   - `pin（SPKI 指纹，公开信息）`——明文显示并预填，格式 `sha256:<64 位十六进制>`。
2. **首次 pull**（提交表单自动触发，10 秒超时）——成功则落一份本机 DEK 加密的
   只读快照 `cache.bin`，拉取凭据自动存入本机 `cache.auth.json`。**失败不丢输入**：
   表单带原值重开（设备码照旧留空），横幅按四类给出诊断 + 原始错误：
   地址不通（检查 serve 地址拼写与网络/防火墙）/ 设备码无效（核对 server 机签发的
   设备码，丢失可在其主控台重发）/ 指纹失配（核对 server 机接入卡上的 pin 指纹）/
   超时（server 可能未启动或网络不通，稍后重试）。
3. **finish 屏双形态**——「配置 agent 的 .mcp.json（client 模式）」，同一屏给两种
   接法（**同一个 project token**，按机器的在线习惯选一个）：

   ```
   —— 离线为主（默认推荐）——
   {
     "mcpServers": {
       "ssh": {
         "command": "ssh-manager",
         "args": ["mcp", "--cache"],
         "env": { "SSHMGR_TOKEN": "<TOKEN>" }
       }
     }
   }

   —— 在线为主 ——
   {
     "mcpServers": {
       "ssh": {
         "type": "http",
         "url": "https://192.0.2.5:7878/",
         "headers": { "Authorization": "Bearer <TOKEN>" }
       }
     }
   }
   ```

   - 屏上的 `SSHMGR_TOKEN` / `Bearer` 位置都是**占位**——真 token 是 server 机
     Projects 页 `[a]` 新增 / `[e]` 轮换签发的 project token（即「密钥 1/2」抄录
     的那把）。**不是设备码**：设备码只用于拉取缓存，刚才已存进 `cache.auth.json`。
   - 在线块屏上注明 `"type": "http"` 必填（漏了会被当 stdio 处理并拒绝该条目）；
     url 位置显示的是刚才连接表单里填的 serve 地址实值。
   - **实例槽自动接线（Plan 40 批2）**：首拉把材料自动归位进命名实例、或表单「实例名」字段非空时，离线块的
     `args` 自动变为 `"args": ["mcp", "--cache", "--instance", "<name>"]`
     并附注释行「本机 cache 位于实例槽 instances/<name>/——args 必须带
     --instance <name>。」——照抄即可；漏了 `--instance` 的启动只认默认槽。
   - 离线块是默认推荐——刚拉完缓存的机器选它，断网也有兜底；笔记本常出门选这个。
   - 两条通用提醒：Windows 建议把 `command` 写成 exe 绝对路径；`.mcp.json` 含
     token，**不要提交进 git**。
4. 按任意键进入 **client 面板**（无需重启）。

## client 面板参考

页头一行连接摘要（`clientHeader` 渲染）：**连接 `<host:port>` · pin `<指纹>` ·
profile `<名称>` · `<N>` 服务器 · 缓存于 `<时长>` 前**——五个字段分别是 serve 地址主机、钉死的证书
指纹、快照绑定的 profile（即这台机的授权范围，Plan 39）、快照里服务器台数、缓存年龄。当前面板停在
命名实例时页头下方多一行 **`· 实例 <name>`** 标注（Plan 40 批2）。正文是快照服务器列表 + 详情两栏（`↑`/`↓` 或
`j`/`k` 移动光标，`/` 过滤，详情逐字段只读展示）。

| 键 | 动作 | 语义 |
|---|---|---|
| `s` | 同步（手动 pull） | 10 秒超时；**失败保留旧缓存**（快照只在拉取成功后原子替换）。作用于**当前选中的实例槽**（默认槽起步）。本界面**永不走明文拉取**——连接配置缺 pin 会直接报错，提示 `[c]` 补上 |
| `i` | 实例切换（picker，Plan 40 批2） | 弹「选择实例」overlay：第一行恒为「**（默认实例）**」（有实例恰好合法名叫 `default` 也靠它区分），其后每个命名实例一行（名字 + 缓存年龄 + profile；**轻量行不解密**，DEK 坏了也不影响列表）。`↑`/`↓` 移动、Enter 选中 → 面板即刻切到该实例的槽位重读数据；Esc 取消不动。**会话内有效**：不落盘、重启进程回到默认实例 |
| `c` | 编辑连接 | 重开第 1 步表单：url/pin 预填原值，设备码掩码且**从不预填**，留空 = 保持不变；本机没有任何已存码时留空会在提交时被拒。保存落的是表单路由的实例槽并随之切槽（见下节三连校验） |
| `t` | TTL 说明 | 状态行原文「TTL 由 .mcp.json 的 --cache-max-age 控制（默认 30m；0=关闭自动拉取）」 |
| `q` | 退出 | `Ctrl+C` 同 |

页脚键位原样是 `[s]同步 [i]实例 [c]编辑连接 [t]TTL  q 退出`；**单槽模式（override env 覆盖中）没有 `[i]`**——见下。

**自动弹 picker（批2）**：启动落在默认实例；若默认槽**真真空**（四文件 `cache.bin` / `cache.auth.json` / `cache.meta.json` / `cache.config.json` 全缺）而 `instances/` 下已有命名实例，首个数据到达后自动打开 picker 并提示——这是"材料全在实例槽、启动却对着空默认槽"的引导路径。"部分缺件"形态（bin 在 auth 缺 / meta 在 / config 在）一律不弹——默认槽有意图或材料，不把你从恢复路径引开。

**单槽横幅（override env 模式）**：`SSHMGR_CACHE_DIR` 或 `SSHMGR_CACHE_DEK` 任一在场时页面顶栏显示

> ⚠ 单槽模式（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK 覆盖中）——多实例 UI 已禁用

并统一禁用多实例面：`[i]` 从页脚消失、自动 picker 不触发、表单实例字段隐藏/禁用——**禁用而非适配**（env 是单槽完全覆盖语义，混用会静默路由错实例）。`SSHMGR_CACHE_DEK_DIR` 只搬 DEK 根目录，不触发横幅。

**零远程写**：client 角色不碰任何 vault——这个面板和整个角色只能读缓存、只能经
`cache.auth.json` 拉新快照；agent 侧的写操作（加改删服务器等）在线也走 serve 的
HTTP 路由、由 server 机的 vault 落盘，client 机的 TUI 里**没有任何写入口**。

## 多实例表单与换码警告（Plan 40 批2）

client 向导与面板 `[c]` 共用同一张连接表单，表单里多一个**可选「实例名」字段**：留空 = 默认实例（向导首拉真空机自动归位进 `instances/<设备码名>/`）；非空 = 显式路由到命名实例。规范名原则一句话：**大小写归一只用于找到既有实例的真实名字，真正的路由/校验一律用那个名字（= 服务器下发名 = 目录名）精确比对**——填 `agenta` 撞上既有 `instances/AGENTA/` 时路由值取 `AGENTA`。

**保存前置校验三连**（提交时判定；比较在大小写归一后进行）：

1. **实例名 ≠ 当前选中槽 → 设备码必填**（跨实例没有"保持不变"可言）；选中槽没有已存码时同样必填。
2. **实例名撞上已有其他实例 → 直接拒**——文案指明碰撞对象并指引"对其换码请先 `[i]` 切换到该实例（跨槽路由被拒绝）"。对既有实例换码的唯一 TUI 路径 = `[i]` 切到它再 `[c]` 同槽换码。
3. **实例名 == 选中槽（含大小写变体）→ 允许**——同槽显式换码，路由用规范名。

外加两条守卫：

- **面板模式 × 默认槽真空 × 实例名留空 → 拒**：「默认实例无材料——首次 enroll 请走向导流程（自动归位），或填实例名显式路由」；
- **override env 单槽模式下实例名根本不在场**，真空守卫换成单槽版文案：「override env（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK）覆盖中：单槽语义下无多实例路由，请清除 env 或按单槽使用」（该模式下"走向导归位 / 填实例名"两条出路都不可用，不误导）。

**换码预防性警告**（打开表单时按选中槽静态判定，实际拦截仍由门禁兜底）：

- 选中默认槽且有 bin：「⚠ 默认实例已绑定设备 `<device_name>`——更换设备码前须清三件套（cache.auth.json + cache.bin + quarantine/，保留 cache.meta.json 与 cache.config.json——它们是默认槽意图标记，删了重 enroll 会被归位到实例槽）重 enroll，否则下次同步将被门禁拒绝；若是本机第二个 agent，请在"实例名"字段填新实例名。」
- 选中的是命名实例且有 bin：「⚠ 实例 `<name>` 已绑定设备 `<device_name>`——换码须删除该实例目录重 enroll，否则同步将被拒。」
- 选中槽无 bin：不显警告（新实例 enroll 是正常路径）。

runbook 与归位语义的深水区见 [multi-machine.md](./multi-machine.md)。

## 典型任务

### 新工作机接入全流程

1. **server 机**：主控台切到设备码页按 `a` → 填设备名称（用机器名，如
   `laptop`）+ **绑定 profile**（该机的授权范围）+ serve 地址 → 设备码 + 指纹 + 示例命令一次性全屏显示，抄下来。
   （project token 已有的不用动；没有就 Projects 页 `[a]` 再发一把。）
2. **工作机**：`ssh-manager tui` → 否 → client → 连接表单填 `https://<serve>:7878`
   + 设备码 + 指纹 pin → 首次 pull 成功（真空机会**自动归位**进
   `instances/<设备码名>/`——设备码名即实例名，起名时就想好）→ finish 屏抄
   `.mcp.json`（归位实例的离线形态自动带 `--instance <name>`；project token
   填 server 机签发的那把）→ client 面板确认页头服务器数。
3. **agent 验证**：工作机 Claude Code 重启后让 agent 调 `list_servers`——列出的
   就是缓存快照里 profile 范围内的服务器，接入完成。

### 机器失窃处置

在 **server 机**主控台两步：

1. 设备码页 `[d]` 吊销那台机的码（Lazy 生效：该机下次 pull 被拒，再也拉不到新
   快照）；
2. Projects 页 `[e]` 轮换那台机 `.mcp.json` 用的 project token（旧 token 立即
   失效）——在线 http 形态下一个请求即拒，无需动作；离线 `--cache` 形态是 Lazy，
   该机重启客户端时接管。

**如实注明**：已拉下的 `cache.bin` 仍能被那台机的 DEK 解密——**吊销只断拉新**，
不擦已拉。敏感时把那台机接触过的**服务器凭据**轮换掉（server 机服务器页 `e`
编辑换密码/私钥）。完整语义（四层断连）见
[agent-access.md](./agent-access.md#project-生命周期轮换--暂停--恢复--吊销)。

### 在线/离线 .mcp.json 互切

同一个 project token，两种形态只差 `.mcp.json` 内容：在线把 `"ssh"` 对象换成
http 三件套（`"type": "http"` + `"url": "https://192.0.2.5:7878/"` +
`"headers": { "Authorization": "Bearer <TOKEN>" }`）；离线换回 `command` +
`"args": ["mcp", "--cache"]` + `env` 三件套（命名实例的机器离线块里
`--instance <name>` 要原样保留——它指定读哪个实例槽；在线 http 形态不涉及实例路由）。
**改完重启 Claude Code** 生效——vault 内容、token、profile 授权范围完全一样；
在线可写，离线只读。两套也可长期并存于不同机器，互不冲突。

## 排错

| 症状 | 处置 |
|---|---|
| client 首次 pull 报「指纹失配」 | **失配 ≠ 泄露**：意味着对端证书公钥变了——可能是 server 机重装/迁移重签了证书（正常），也可能是中间人（异常）。server 机跑 `ssh-manager serve cert-info` 拿新指纹，`[c]` 更新 pin 后重拉。重签证书的全量交接 runbook 见 [multi-machine.md](./multi-machine.md) |
| client 面板 `[s]` 同步报缺 pin | 本界面永不走明文拉取（缺 pin 直接报错是**默认安全**的设计）。`[c]` 把接入卡上的指纹补进 pin 字段即可。「无 pin 默认拒连 / `--allow-plaintext`」是 **CLI 专用**逃生口，TUI 里没有也不该有 |
| 同步失败但面板还有数据 | 失败保留旧缓存——这是特性不是 bug。按横幅的四类分类修（地址/设备码/指纹/超时），修好 `[s]` 重拉 |
| 缓存多久算旧 / 怎么自动保鲜 | `[t]` 那行：TTL 由 `.mcp.json` 的 `--cache-max-age` 控制（默认 30m；0=关闭自动拉取）。`mcp --cache` 进程内 spawn 惰性拉取 + 会话内懒检查 + 热加载，无需 OS 定时器；细节见 [multi-machine.md](./multi-machine.md#离线只读缓存plan-12) |
| server 机 serve 探活失败 | 安装结果屏的排查行：7878 端口防火墙是否放行；`ssh-manager serve status` 查四项信号（service/process/http/vault）；服务可能仍在启动，稍候重试 |
| 吊销了 token/设备码，agent 还在跑 | 断连语义按部署模式分四层：serve 远程逐请求即拒；stdio/离线缓存 Lazy（重启客户端接管）；已建立的隧道 revoke/disable 后 ~15s 内级联拆除（`tunnels kill` 可急停）——完整语义见 [agent-access.md](./agent-access.md#project-生命周期轮换--暂停--恢复--吊销) |
| 向导中途退出了 | 两边都一样：什么都不用做，重跑 `ssh-manager tui` 从断点续配（server 侧会盘点已建的 profile/project/设备码，跳过已完成步骤） |
| client 机想改连别的 server | `[c]` 编辑连接（地址/设备码/pin 全换），`[s]` 重拉即可；不需要重跑向导 |

## 相关文档

- 架构 / 运营深水区（TLS 迁移、证书轮换 runbook、export/import、备份）：
  [multi-machine.md](./multi-machine.md)
- 单机版 TUI 教程（首跑向导、主控台四页签、导入 ssh config）：
  [tui-single-machine.md](./tui-single-machine.md)
- CLI 视角的多机速通：[quickstart-multi-machine.md](./quickstart-multi-machine.md)
- token / 设备码生命周期与断连语义（四层）：[agent-access.md](./agent-access.md)
- 概念模型图解（vault / profile / project / 设备码谁是谁的）：[concepts.md](./concepts.md)
- client↔serve 版本兼容矩阵（升级任何一端前先看）：[compat-matrix.md](./compat-matrix.md)
