# TUI 教程 · 联机版（serve 多机共享）

> **读者**：server 机（常驻 broker）和工作机（client）两边的操作者。
> 架构/runbook 深水区（TLS 迁移、证书轮换、export/import）在
> [multi-machine.md](./multi-machine.md)——本篇只讲 TUI 怎么点。
> 单机场景看 [tui-single-machine.md](./tui-single-machine.md)；CLI 视角的速通看
> [quickstart-multi-machine.md](./quickstart-multi-machine.md)。

## 全景图

```
 ┌──server 机（你在这台操作：向导 + 主控台）──────────────────┐
 │  sshmgr tui                                      │
 │   ├─ 首跑向导（角色选 server）                          │
 │   │   vault + 服务器清单 + profile + project token     │
 │   ├─ serve 服务（安装向导代办，常驻 0.0.0.0:7878）      │
 │   └─ 主控台五页签（含设备码页 [a]/[d] 与 Pairing 批准页）│
 └──────────────────────────────┬───────────────────────┘
                        pair 入网 │ 批准（对照 client 屏 SAS）
                                ▼
 ┌──工作机（你在这台操作：pair/TUI [c] 向导 + client 面板）────┐
 │  sshmgr pair --instance <名>                       │
 │   ├─ LAN 发现 serve（或 --url 直指）→ 屏显三件套          │
 │   │   <名> @ <url> SAS <6位>（等 owner 批准）             │
 │   ├─ 批准后自动：凭据下发 → 首拉 cache.bin（只读快照；     │
 │   │   真空机自动归位进 instances/<name>/）                │
 │   └─ 产物 pair.<名>.mcp.json 抄进 agent 配置              │
 │  sshmgr tui → client 面板：[c]配对向导 [s]同步 [i]实例    │
 │   （零远程写；TUI 入网=同一配对流程的点选版，见下）        │
 └─────────────────────────────────────────────────────────┘
```

两台机、两把密钥、两道独立的闸门（与 multi-machine.md 一致）：**project token**
驱动 agent 的 MCP 工具调用（进 `.mcp.json`）；**设备码**只授权拉取 `/snapshot`
缓存。两者永不互通，一台设备码被吊销不影响任何 project token。

> **Plan 42 批1 起**：client 的连接编辑表单已退役；**Plan 45 起** `[c]` 以 **SAS 配对
> 向导**形态复活——工作机入网从此有两条等价路径：CLI `sshmgr pair` 一条龙，或
> client 面板 `[c]` 配对向导（下有逐屏走查）。连接配置、设备码、指纹仍全部由配对
> 自动交付；写操作只在 server 机主控台。

## server 侧走查

空机第一次运行 `sshmgr tui` 进入首跑向导（启动方式、mintty/non-TTY 注意事项
同 [tui-single-machine.md §1](./tui-single-machine.md#1-启动)）。首屏问后果：

> **这台电脑要保管所有 SSH 凭据吗？**
> - 是——凭据只存这台机（其他机器不能用了它就都用不了）
> - 否——凭据在另一台机器上，这台只连它 → client（需先在 server 机完成设置）

选**是**；第二问选**要给其他机器共享 → server**。**选定的瞬间 role.json 已落盘**，
此后任何时刻 `q` / `Esc` / `Ctrl+C` 都是安全暂停，重跑 `tui` 从断点继续。
（已配好单机的机器不用重跑向导：主控台页脚直接有 `[u]升级为 server`。）

1. **服务器录入循环**——「现在录入第一台服务器？」可跳过（跳过 = profile 暂无成员，
   agent 将看不到任何服务器；之后可在主控台随时补录）；录了可「继续添加下一台？」。
   表单字段与单机版完全一致（密码/私钥互斥、可都留空 = 无凭据 ⚠，见
   [tui-single-machine.md §2](./tui-single-machine.md#2-首跑向导走查可中断续配)）。
2. **Profile 名称 + 授权多选**——名称默认 = 本机主机名（重名自动加 `-2`）；
   服务器多选空格勾选、回车提交。（配对批准时 owner 还会给每台设备选绑定
   profile——设备码与 profile 的绑定在批准一刻才发生，pair 时代设备码不再预签发。）
3. **项目名称**（默认 = 本机主机名；即发给 agent 的「通行证」身份）。token 屏
   一次性全屏显示，**当场抄下来**——pair 批准会为每台新设备自动建 `pair-<设备名>`
   project 并签发新 token，手工路径（CI/迁移）才用得上这把预建的。
4. **serve 地址**——「client 通过哪个地址访问这台 server？」：本机非环回 IPv4
   列表单选，默认第一项（形如 `https://192.168.1.10:7878`）；无 IPv4 的特殊环境
   退化为手输，校验必须 `https://` 开头（serve 只说 TLS）。选定地址写进
   **client pair 卡**。
5. **安装 serve 服务（需要管理员权限）**——前置提示原文要点：服务绑定
   `0.0.0.0:7878`（不是选定的那个地址——选定的地址只写进 pair 卡，监听永远全网卡）；
   Windows → Windows 服务、Linux → systemd、macOS → launchd；**若失败向导不阻断**
   ——会显示可手动提权执行的原文命令并继续验证服务状态。安装结果**不中断流程**：
   失败横幅给出手动命令（Windows：管理员终端跑 `sshmgr serve install
   --addr 0.0.0.0:7878`；Linux/macOS：`sudo` 同款命令），随后照常探活。
6. **serve 安装结果屏**——两行判定：安装（✓ 已装并启动 / ✗ 失败+手动命令）+
   探活（✓ 已就绪 / ⚠ 未验证：「排查：7878 端口防火墙是否放行；`sshmgr
   serve status` 查四项信号；服务可能仍在启动，稍候重试」）。**任一行失败都不拦路**，
   流程照走。
7. **客户端 pair 卡**——把这张卡带到 client 机：`地址` + `指纹` 两个明文值 +
   入网命令 `sshmgr pair --instance <名>`（LAN 发现自动带指纹；`--url`
   直指时配 `--pin`）。按任意键完成设置，直接进入主控台。

主控台页签的完整键位见 [tui-single-machine.md §3](./tui-single-machine.md#3-主控台四页签)。
**Tab 切页即重读**（Plan 39）：页签数据可能被本进程之外的写入改动（serve 进程在每次
client pull 后更新设备码的「最近拉取」、其它 TUI/CLI 会话增删实体）——切到哪页就重读哪
份，所见即 DB 当前值；已停留在页上期间的外部写入需切走再切回才可见。
联机部署下的关键页签：

| 页签/键 | 动作 | 说明 |
|---|---|---|
| **Pairing 页**（Plan 42 批1 新增） | 批准/拒绝配对 | 待批准队列每行：`name @ target_url · 来源IP · hint · 剩余秒 · ⚠标记`；`a` = 批准（选 profile，`pair.default_profile` 预选；⚠目标≠本机地址的行需键入大写 `OVERRIDE`）；`d` = 拒绝；`r` = 刷新。**批准面同屏显示三件套（含 serve 在 enroll 时落行的真 SAS）——与 client 屏 SAS 逐位比对一致后再批**（行缺 SAS → ⚠ 警示建议拒绝；详见下「典型任务」）。 |
| 设备码页 `a` / `d` | 手工签发/吊销设备码 | pair 时代日常签发由批准自动完成；这两键留给**手工路径**（CI/迁移）：表单选绑定 profile，设备码 + 指纹 + `cache pull` 示例命令一次性全屏显示；`d` 吊销（该机下次拉取被拒——机器失窃处置第一步，见下） |

## client 侧走查

**前提：server 机已完成上面的设置**（serve 在跑、手上有 pair 卡或知道 serve 地址）。
入网有两条**等价**路径：CLI `sshmgr pair` 一条龙（下 ①），或 client 面板 `[c]`
配对向导（下 ②，Plan 45）——同一条配对管线（发现 → enroll → SAS 人闸 → 批准 →
finish → 落盘首拉），选顺手的即可；CI/无人值守走 ①。

### 路径 ①：CLI `sshmgr pair` 一条龙

```bash
sshmgr pair --instance laptop
```

1. **发现/连接**——默认 LAN 广播发现 serve（同网段即中）；拿不到 offer（防火墙挡
   UDP/跨网段）按提示 `--url https://<serve>:7878` 直指，**显式 `--url` 不带
   `--pin` 默认拒连**（无锚通道需 `--allow-tofu` 显式接受，仅限受控环境）。
2. **三件套屏显**——`laptop @ https://192.0.2.5:7878 SAS 482913`。**停在批准
   等待**（10 分钟窗口）；此时去 server 机批准。
3. **批准**（server 机）——TUI Pairing 页（或 `serve pair approve laptop
   --profile team-a`）：对照 **client 屏的 SAS** 与**批准行的 name@url** 一致后
   选 profile 批准。
4. **自动收尾**——批准后 120 秒内 client 自动完成：凭据解密下发 → 先落盘
   （`cache.auth.json` + `cache.config.json` + 产物 `pair.laptop.mcp.json`，0600）
   → 首拉 cache.bin。终端只显示 `<project-token>` 占位符——真值只在产物文件里。
5. **配 agent**——产物片段抄进 `.mcp.json`（`mcp --cache` + env token 形态；
   `--write-mcp <path>` 可让 pair 直落目标路径），重启 Claude Code 即用。

### 路径 ②：TUI 配对向导（client 面板 `[c]`，Plan 45）

`sshmgr tui`（选 client 角色，或 `sshmgr tui --mode client`）进入 **client 面板**
后按 **`[c]`** 启动**配对向导**——全键盘走完与 CLI 同一条管线：

1. **表单**——四个字段：`实例名`（必填，设备名即实例槽名，白名单校验）、
   `broker 地址`（**留空 = LAN 自动发现**（udp/7878）；跨网段/防火墙挡 UDP 时填
   `https://<serve>:7878` 直连）、`pin`（发现流 offer 自动携带；直连时必填——
   TUI **没有** TOFU 开关，`--allow-tofu` 逃生门只在 CLI）、`profile hint`
   （可选，显示在 broker 批准面）。表单不会静默覆盖已配对的实例名——同名重配
   走实例 picker 的 `p`（见下「重配」）。
2. **发现/选择**——地址留空时短窗口广播发现（几秒，**不可取消**——Esc 仅弃结果
   并退出向导）；发现多台 serve 列清单 `↑`/`↓` + Enter 选（单台直接进下一步）；
   拿不到 offer 回到表单并提示改填直连地址。
3. **（仅重配）force 确认屏**——见下「重配已配对实例」；新机入网无此屏。
4. **enroll**——生成临时密钥对、注册申请（Esc 可取消；serve 侧 pending 即入
   Pairing 页队列）。
5. **SAS 等待屏**——**6 位 SAS 大字常显**（不是批准后才出现）：批准者在 server
   机 Pairing 页**同屏看到三件套**（`name @ url` + **六位 SAS**——serve 在 enroll
   时就算好 SAS 落入待批行，批准面直读真值，v0.13.2 起），**批准前请逐位比对
   两边一致**；屏上同时有剩余审批窗口倒计时（10 分钟）与轮询状态行。此刻去 server
   机 **Pairing 页按 `a`**（或 `serve pair approve laptop --profile team-a`）。
6. **批准到达 → 最终核对门**——SAS 再次放大复核，**Enter 才真正完成**（finish +
   首次拉取发生在 Enter 之后）；Esc = 放弃本次配对（broker 侧申请自行过期）。
   之后的**写入期（落盘 + 首拉）不可取消**——屏显「写入中，请稍候」，几秒即过。
7. **结果屏**——成功：实例名 + 已授权 profile + 产物 `pair.<名>.mcp.json` 落点
   （0600，含真值 token，勿外发）+ `.mcp.json` 后续指引；Enter/Esc 返回面板，
   **自动切到新实例槽刷新**（`· 实例 <名>` 行与页头同步更新）。

**结束态与重试**：批准被拒、申请过期（serve 对被拒/过期/已送达返回同一终态）或
其它失败 → 结果屏「**本次申请已结束（被拒或过期）**」；本地 10 分钟窗口到点则
显示超时措辞。两种结局都按 **`r`** 以相同参数重新申请（全新 generation、enroll
新 id——旧申请不可能复活）。

**重配已配对实例**：`[i]` 打开实例 picker，**任意具名行（完整或残缺——残缺行恰
是最需要重配的）按 `p`** → 同一向导以 force 预填进入，**先过 force 确认屏**
（Plan 46 起如实化——不再「预删除」：确认的是「重配成功后，新凭据将原子覆盖本
实例旧材料；重配成功前，旧材料一律不动」，Esc = 零残留）。确认屏按槽的本地
四要素（auth/bin/meta/DEK）**分档给 419 提示**：完整槽 =「该实例已拉取过，重配
前需 owner 在 broker 吊销其设备码」（确定性）；残缺槽 =「该实例材料不完整，无法
本地预判远端状态；若重跑撞 419 见错误指引」（可能性）。**serve 侧旧设备码两态
如实**：旧码**从未拉取过** → finish 时自动收编吊销；**在用旧码（已拉取过）需
owner 先在 broker `sshmgr cache-tokens revoke <名>` 吊销再重配，否则 enroll 被
419「device name in use」拒**（见排错表）。默认实例行按 `p` 显示钉死提示（默认
槽为本机原始身份，不支持 picker 重配；不指 `--instance`——该槽无名可指）。

**Esc 退出纪律**：**任何一步都能全身而退**——表单/选择屏直接退；等待/核对门中
= 取消本次申请；唯一例外是写入期不可打断（写入是原子覆盖语义，屏显「写入中，
请稍候」，几秒即过——半途无 Esc 可按，也就不存在半写悬挂）。

**边界（如实）**：

- **单槽覆盖模式下向导不可用**——`SSHMGR_CACHE_DIR` / `SSHMGR_CACHE_DEK` 任一
  在场时 `[c]` 拒绝启动（页脚与入网指引行都不再宣传该键），入网/重配走 CLI
  `sshmgr pair`（与 CLI `--instance` 的互斥同语义）。
- **AssumeSAS 仍 CLI-only**——env `SSHMGR_PAIR_ASSUME_SAS=1` 的无人值守跳比对只
  在 `sshmgr pair` 生效；TUI 向导不读该 env，永远人闸比对。
- 零远程写边界不变——批准永远在 server 机的管理面（Pairing 页 / `serve pair`
  CLI / 批2 Web UI），client 机的向导只发起申请。

## client 面板参考

页头一行连接摘要（`clientHeader` 渲染）：**连接 `<host:port>` · pin `<指纹>` ·
profile `<名称>` · `<N>` 服务器 · 缓存于 `<时长>` 前**——五个字段分别是 serve 地址主机、钉死的证书
指纹、快照绑定的 profile（即这台机的授权范围，Plan 39）、快照里服务器台数、缓存年龄。当前面板停在
命名实例时页头下方多一行 **`· 实例 <name>`** 标注（Plan 40 批2）。正文是快照服务器列表 + 详情两栏（`↑`/`↓` 或
`j`/`k` 移动光标，`/` 过滤，详情逐字段只读展示）。

| 键 | 动作 | 语义 |
|---|---|---|
| `s` | 同步（手动 pull） | 10 秒超时；**失败保留旧缓存**（快照只在拉取成功后原子替换）。作用于**当前选中的实例槽**（默认槽起步）。本界面**永不走明文拉取**——连接配置缺 pin 会直接报错（缺 pin 时入网/换码走 `pair`；TUI：实例 picker `p`） |
| `i` | 实例切换（picker，Plan 40 批2；Plan 46 重做） | 弹「选择实例」overlay：第一行恒为「**（默认实例）**」（有实例恰好合法名叫 `default` 也靠它区分），其后每个命名实例一行。每行 = 名字 + **四要素状态列**（`auth+bin+meta+DEK` 齐 = **完整**；槽目录在而任缺 = **残缺**且行内点名缺什么；无目录 = 空）+ 缓存年龄 + profile + 已配对标记；**★ 前缀** = 当前会话所在槽，**⚠ 前缀** = 半态槽（用户事故形态优先可见）；列宽按显示宽度对齐（中文实例名不破列）。**轻量行不解密**，DEK 坏了也不影响列表；完整/已配对判定均为**本地视角**——远端吊销状态不可见（列表底部尾注原样提示）。键位：`↑`/`↓` 移动；**Enter** 选中 → 面板即刻切到该实例的槽位重读数据；**具名行按 `p`** = force 重配（Plan 45 起；Plan 46 扩到残缺行，见上「重配已配对实例」）；**具名行按 `d`** = 删除实例（Plan 46，见下）；默认槽行按 `p`/`d` 均只显示提示（`p`：默认槽不支持 picker 重配；`d`：清空本机全部 sshmgr 数据请用 `sshmgr clear`）。Esc 取消不动。**会话内有效**：不落盘、重启进程回到默认实例 |
| `c` | 配对向导（Plan 45） | 打开 **SAS 配对向导** overlay（表单 → 发现 → SAS 常显等待 → 批准后 Enter 完成 → 写入 → 自动换槽刷新；逐屏语义见上「路径 ②」）。连接编辑表单仍退役——连接参数在向导表单里填/由配对自动交付；CLI `sshmgr pair` 保留并列，手工路径 `cache pull` 见 docs |
| `q` | 退出 | `Ctrl+C` 同 |

页脚键位原样是 `[s]同步 [i]实例 [c]入网 [t]TTL  q 退出`；**单槽模式（override env 覆盖中）没有 `[i]` 也没有 `[c]`**——见下。

**[d] 删除实例与并发边界（Plan 46，如实）**：picker 具名行 `d` = 确认 overlay（双根清单 + 两件配套提示，与 CLI `sshmgr cache instances rm` 同语义——broker 侧吊销与槽外 `--write-mcp` 副本两件事本机删不了，见 [multi-machine.md](./multi-machine.md)「实例删除」节）→ 确认后**后台删除**（busy 提示「删除实例 X…」，事件循环不冻结）。成功刷新列表；删的是**当前槽**则自动回落默认槽并清空内存态（`[s]` 绝不拿已删槽的凭据打默认槽）；失败错误（含残留物清单）挂在重开的列表下方、**槽与会话路由一律不动（不回落）**。**进程内互斥**：删除或 force 清理进行中，同进程并发的 pull / pair 写盘**被拒绝**（明确报错——拒绝而非排队）；跨进程并发不由文件锁拦截，由原子写 + 删除幂等可重跑兜底。

**自动弹 picker（批2）**：启动落在默认实例；若默认槽**真真空**（四文件 `cache.bin` / `cache.auth.json` / `cache.meta.json` / `cache.config.json` 全缺）而 `instances/` 下已有命名实例，首个数据到达后自动打开 picker 并提示——这是"材料全在实例槽、启动却对着空默认槽"的引导路径。"部分缺件"形态（bin 在 auth 缺 / meta 在 / config 在）一律不弹——默认槽有意图或材料，不把你从恢复路径引开。

**单槽横幅（override env 模式）**：`SSHMGR_CACHE_DIR` 或 `SSHMGR_CACHE_DEK` 任一在场时页面顶栏显示

> ⚠ 单槽模式（SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK 覆盖中）——多实例 UI 已禁用

并统一禁用多实例与配对面：`[i]` 与 `[c]` 从页脚消失（空面板的入网指引行同步改指
CLI `sshmgr pair`——向导在该模式下拒绝启动，页上不留一个按了没用的键）、自动
picker 不触发——**禁用而非适配**（env 是单槽完全覆盖语义，混用会静默路由错实例）。`SSHMGR_CACHE_DEK_DIR` 只搬 DEK 根目录，不触发横幅。

**零远程写**：client 角色不碰任何 vault——这个面板和整个角色只能读缓存、只能经
`cache.auth.json` 拉新快照；agent 侧的写操作（加改删服务器等）只能在 server 机的
**管理面**（主控台 / `serve pair`）做，client 机的 TUI 里**没有任何写入口**。

**换码/新实例 = `pair --force` / 新 `pair`**（Plan 42 批1；**Plan 45 起 TUI 等价路径**；**Plan 46 起 force 零清理先行**）：CLI 对既有实例换设备码 =
`sshmgr pair --instance <名> --force`——**enroll 前零清理**（校验/确认屏/enroll
任何一步失败，旧槽材料一字不动）；重配成功 = 新凭据**原子覆盖**旧材料
（`cache.config.json` 始终不动——时效策略原地继承）；`quarantine/` 在成功尾部
清理（失败仅警告，下次成功时重清）。第二个 agent = 直接再跑一条
`pair --instance <新名>`（自动归位新实例槽）。TUI：`[i]` picker 具名实例行按
`p` = force 重配（先过确认屏），`[c]` 向导填新实例名 = 新实例入网。实例整个
不要了 = `cache instances rm <名>`（CLI）或 picker 行 `d`（Plan 46）。
runbook 深水区见 [multi-machine.md](./multi-machine.md)。

## 典型任务

### 新工作机接入全流程

1. **工作机**：装好二进制后 `sshmgr pair --instance laptop`（同网段自动发现
   serve；跨网段用 `--url` + `--pin`，pair 卡上有现成命令）→ 屏显三件套
   `laptop @ <url> SAS <6位>`，等待批准。不想敲命令就在工作机 `sshmgr tui` 按
   `[c]` 走配对向导（同一条流程的点选版，见上「路径 ②」）。
2. **server 机**：主控台切到 **Pairing 页**按 `a` → 选 profile（授权范围）→
   **对照 client 屏 SAS 与批准行 name@url 一致**后提交（⚠目标≠本机地址的行需键入
   `OVERRIDE`）。批准后 client 自动完成凭据下发与首拉（真空机自动归位进
   `instances/<名>/`——设备名即实例名，起名时就想好）。
3. **收尾**：产物 `pair.laptop.mcp.json` 的片段抄进 `.mcp.json`（或当初
   `--write-mcp` 已直落）→ 重启 Claude Code → agent 调 `list_servers` 验证。

### 机器失窃处置

在 **server 机**主控台两步：

1. 设备码页 `[d]` 吊销那台机的码（该机下次 pull 被 pinned 401 拒 → 本地缓存
   就地销毁，再也拉不到新快照）；
2. Projects 页 `[e]` 轮换那台机 `.mcp.json` 用的 project token（旧 token 立即
   失效）——该机下次保鲜拉到的新快照已无此 project，本地 spawn 闸即拒。

**如实注明**：已拉下的 `cache.bin` 仍能被那台机的 DEK 解密——吊销的销毁要**回连**
才兑现。敏感时把那台机接触过的**服务器凭据**轮换掉（server 机服务器页 `e`
编辑换密码/私钥）。永离线设备的兜底 = `max_offline` 硬上限（pair 下发默认 24h）
+ 必要时轮换凭据。完整**吊销三路径**见
[agent-tools.md](./agent-tools.md#部署形态你通常无需分辨) 与
[agent-access.md](./agent-access.md#project-生命周期轮换--暂停--恢复--吊销)。

### 换码 / 换实例

`pair` 一条命令覆盖（Plan 42 批1）：既有实例换设备码 = `sshmgr pair
--instance <名> --force`（Plan 46 起零清理先行——失败旧槽完好，成功 = 新凭据
原子覆盖；`cache.config.json` 不动——时效策略原地继承）；第二个 agent =
`pair --instance <新名>`（自动归位新实例槽，产物 args 自动带 `--instance`）。
TUI 等价（Plan 45）：client 面板 `[i]` picker 具名行 `p`（force 重配，先过确认
屏）/ `[c]` 向导配新实例；实例整个不要了 = `cache instances rm <名>` 或 picker
行 `d`（Plan 46）。runbook 深水区见 [multi-machine.md](./multi-machine.md)。

## 排错

| 症状 | 处置 |
|---|---|
| pair 报「指纹失配」/ TLS 错 | **失配 ≠ 泄露**：意味着对端证书公钥变了——可能是 server 机重装/迁移重签了证书（正常），也可能是中间人（异常）。server 机跑 `sshmgr serve cert-info` 拿新指纹，`pair --pin <新指纹>` 重新入网。重签证书的全量交接 runbook 见 [multi-machine.md](./multi-machine.md) |
| pair 吃 ⚠「目标 ≠ 本机地址」 | client 声明的连接地址不属于 serve 机的非环回地址集/hostname——疑似中继/假 discovery/拿错了地址。核对 pair 卡上的地址重跑；确属故意（受控中继）才用 `serve pair approve --allow-foreign-url` 显式覆盖 |
| `pair --url` 被拒「refusing TOFU」 | 直指又不带 `--pin` 是**默认拒绝**（默认安全）。带上 pair 卡里的 `--pin sha256:...`；`--allow-tofu` 只留给受控环境的无锚通道 |
| client 面板 `[s]` 同步报缺 pin | 本界面永不走明文拉取（缺 pin 直接报错是**默认安全**的设计）。缺 pin 的实例用 `pair --force` 重新入网即可补全 pin（TUI：实例 picker 按 `p`） |
| 向导结果屏「本次申请已结束（被拒或过期）」 | serve 对被拒/过期/已送达的申请返回同一终态——本次申请已死，按 `r` 以相同参数重新申请即可（enroll 新 id；若是 owner 误拒，重新批准新的即可）。反复失败查 Pairing 页的 ⚠标记（目标地址 ≠ 本机地址会被机械校验拦下） |
| 配对失败「device name in use」（419） | 实例名的旧设备码还在 serve 上且**在用**（已拉取过）——同名重配在 enroll 一步即被拒。**Plan 46 起 enroll 前零清理：被拒瞬间旧槽材料完好无损**（无恢复动作要做）。owner 在 broker 先 `sshmgr cache-tokens revoke <名>` 吊销旧码再重配（回到向导按 `r` 同参重申即可）；从未拉取过的旧码无需此步（finish 事务自动收编吊销） |
| 配对/重配在 finish 后失败（写盘 / 首拉 / 传输中断） | 错误文案统一尾缀**双路径恢复指引**（Plan 46，如实——client 无法可靠分辨 serve 端是否已提交）：直接重跑 `sshmgr pair --force`（或 TUI 重配）；若重跑报设备名占用（419），请 owner 在 broker 侧执行 `sshmgr cache-tokens revoke <实例名>` 后再重跑。没有「必定自愈」式承诺；按指引重跑（撞 419 先 revoke）是目前唯一的已知恢复路径，失败会再次给出同样指引 |
| 同步失败但面板还有数据 | 失败保留旧缓存——这是特性不是 bug。修好网络/serve 后 `[s]` 重拉 |
| 缓存多久算旧 / 怎么自动保鲜 | TTL 由 `.mcp.json` 的 `--cache-max-age` 控制（默认 30m；0=关闭自动拉取）。`mcp --cache` 进程内 spawn 惰性拉取 + 会话内懒检查 + 热加载，无需 OS 定时器；细节见 [multi-machine.md](./multi-machine.md#离线只读缓存plan-12) |
| server 机 serve 探活失败 | 安装结果屏的排查行：7878 端口防火墙是否放行（TCP + UDP 都要）；`sshmgr serve status` 查四项信号（service/process/http/vault）；服务可能仍在启动，稍候重试 |
| 吊销了 token/设备码，agent 还在跑 | 三路径：设备码吊销 → 该机下次 pull 即 quarantine（在线 ≤30min）；project token 吊销 → 下次保鲜的新快照已无该 project；永离线设备 → `max_offline` 到期拒载。已建立的隧道 revoke/disable 后 ~15s 内级联拆除（`tunnels kill` 可急停）——完整语义见 [agent-access.md](./agent-access.md#project-生命周期轮换--暂停--恢复--吊销) |
| 向导中途退出了 | 什么都不用做，重跑 `sshmgr tui` 从断点续配（server 侧会盘点已建的 profile/project，跳过已完成步骤） |
| client 机想改连别的 server | CLI：重新 `pair`（`--force` 换码 / 换 `--instance` 换槽）；TUI：`[i]` picker 具名行 `p` 重配 / `[c]` 向导配新实例（不要的实例 picker 行 `d` 删除）。连接编辑表单已退役——连接参数一律由配对交付或在向导表单里填 |

## 相关文档

- 架构 / 运营深水区（pair 协议、TLS 迁移、证书轮换 runbook、export/import、备份）：
  [multi-machine.md](./multi-machine.md)
- 单机版 TUI 教程（首跑向导、主控台页签、导入 ssh config）：
  [tui-single-machine.md](./tui-single-machine.md)
- CLI 视角的多机速通（pair 版）：[quickstart-multi-machine.md](./quickstart-multi-machine.md)
- token / 设备码生命周期与吊销三路径：[agent-access.md](./agent-access.md) / [agent-tools.md](./agent-tools.md)
- 概念模型图解（vault / profile / project / 设备码谁是谁的）：[concepts.md](./concepts.md)
- client↔serve 版本兼容矩阵（升级任何一端前先看）：[compat-matrix.md](./compat-matrix.md)
