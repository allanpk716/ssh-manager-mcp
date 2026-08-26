# Plan 41 设计：sudo 执行完整性（复合命令整体提权）+ 提权可观测性（envelope/exec_context）

> 2026-08-26 grilling 定案（问题反馈会话，owner 全部确认"按你推荐的来"），本文不重议的决策：
> **问题反馈问题 3 归因销案 = ssh-manager-mcp wrapper 的 shell 语义缺陷，非服务器侧拦截层**（E4 鉴别 + 2026-08-26 两次会话全量观察逐条落位，零残留——§0.2）；
> **修法 = wrapper 整体提权（`bash -c` 单引号转义包装），批 1 hotfix 先行**；
> **问题 2 修法 = 完整方案（envelope `sudo:{requested,outcome,uid}` + auth-failed fail-loud + 提示串剥离）**；
> **exec_context 诊断工具加入**（Q4）；
> **E1-E3 鉴别实验不跑**（无拦截层可鉴别，Q5 销案）；**pty:true 归 backlog 不做**（"绕过拦截"动机已消失，剩余动机弱）；
> **问题 1（锚点）三建议全部归位 Plan 40**（建议 1 = Plan 40 §1 P0 同一修复；建议 3 文案搭 P0 PR；建议 2 自愈依赖 Plan 40 实例化 lazy-pull，延后登记）——本 plan 不碰 clientops/锚点任何代码。
>
> 来源：`D:\SynologyDrive\服务器\ssh-manager-mcp问题反馈-2026-08-26.md`（问题 2/3）+ 2026-08-26 gitlab-urit 排查会话实证。

## 0. 现状事实（2026-08-26 于 v0.10.0 核实；函数名锚定）

1. **wrapper 切断（本 plan 的根因）**：`runSudoSession`（internal/sshbroker/sudo.go:46）拼接 `wrapped := fmt.Sprintf("sudo -S -p '' -- %s", cmd)` 后交远端登录 shell 解析。POSIX shell 语义下 **`--` 只把第一个简单命令置于 sudo 域**，其后所有 `;` / `&&` / `||` / `|` 分隔的段落脱离 sudo、以**原登录用户**执行。前台 `ExecSudo`（exec_command sudo=true）与后台特权变体 `ExecSudoWriters`（exec_background sudo=true，Plan 32）共用此 kernel——**一处缺陷，前后台通吃**。
2. **实证证据链（gitlab-urit，user=uritgitadmin，user 不在 docker 组，`/var/lib/docker` 为 `drwx--x--x root:root`）**：2026-08-26 两次会话（反馈作者会话 + 排查会话）中每一例"root 神秘 EACCES"均逐条归因为"该段落在分号之后、以 uritgitadmin 执行、被普通 DAC 拒绝"：
   - `sudo=true` 下 `id; ls /var/lib/docker` → `id`=uid=0（sudo 段）而 `ls` 段 EACCES（other 位 `--x` 允许 stat 拒绝 read）——**uid=0 与被拒从未同段**；
   - `docker system df && docker info | grep …` → 前者（sudo 段，root）成功出表，后者（`&&` 右侧，uritgitadmin）连 docker.sock 被拒、stderr 被 `2>/dev/null` 吞 → grep 空——**同一条命令两种身份**；
   - 反馈"中途开始 EACCES、前几轮正常" = 命令形态从单命令（全提权）变为复合命令（docker 段落在非首段）——**形态变化，非时间翻转**；
   - 单命令形态的后台 `du -x /`（sudo=true）成功穿过 `/var/lib/docker`（150G /var）——"detached 逃逸"假说不再需要；
   - E4 鉴别（systemctl 全量 + ps + `/usr/local` + AppArmor profile 清单）：**无任何 EDR/HIDS**，纯标准 Ubuntu 18.04 + GitLab omnibus + SonarQube 进程集；
   - 本机 root 控制台对照正常（无切断）。
   - 唯一未落位观察：反馈证据 5（`stat -c "%t:%T" /var/lib/docker` 返回 `0:0`）——登记存疑（§9.5），不阻塞。
3. **静默降权的调用方契约背叛**：工具文档承诺 "broker runs `sudo -S` for you — do NOT prepend sudo"，调用方（尤其 AI agent）按"整条命令在 root 下"的预期构造复合命令，实际后续段静默降权。这正是反馈问题 2 中被标为"假设"的**降级执行安全反模式——它不是假设，是每天在发生的现实**。反馈"5+ 轮误判（fakeroot/sudoers/AppArmor 轮番假设）"与排查会话一轮空输出误判，均为其直接代价。
4. **envelope 无身份字段**：`ExecOutput`（internal/mcpserver/types.go:35-45）仅 stdout/stderr/exit_code/timed_out/truncated/bytes/effective_timeout_seconds。调用方无法区分"提权了但命令失败"与"根本没提权"。
5. **fail-loud 已存在的部分**：`SudoCredentialID` 为空时 `sudo=true` 直接报 `no_sudo`（internal/mcpserver/core.go:165-168），不降级；sudo 认证失败（密码错）→ sudo exit 1 + stderr 明确特征，命令不跑。**缺的是成功时的身份实证与结构化失败语义**。
6. **`-p ''` 实测无效**：Ubuntu 18.04 / sudo 1.8.21p2 下 `sudo -S -p ''` 仍向 stderr 输出 `[sudo] password for <user>: `（2026-08-26 两次会话实测）。压提示串只能靠 broker 端剥离，不能靠 `-p`。
7. **exec 的执行位置（升级部署面）**：cache 模式（`mcp --cache`）= 本地 MCP 进程持 snapshot 凭据、**本地直连目标机**执行；http 直连模式 = **serve 端**执行（upload_file 文档"with a remote serve broker it is the serve host"同源事实）。wrapper 改动在两种模式下分别随**客户端二进制**与 **serve 二进制**生效——沿用 v0.10.0 双端部署纪律，两端都要升。
8. **测试设施**：internal/testsshd（sshd.go）+ internal/sshbroker/sudo_test.go 已有 fake-sudo 测试形态，批 1/2 的矩阵在其上扩展。

## 1. 批 1（建议 v0.10.1 hotfix）：wrapper 整体提权——正确性修复

**原则：调用方请求了 sudo，就是请求整条命令以特权执行。`--` 穿不透 shell 分隔符是缺陷，不是可协商的语义。**

### 1.1 变更点

`runSudoSession` 的 wrapper 从

```go
wrapped := fmt.Sprintf("sudo -S -p '' -- %s", cmd)
```

改为

```go
wrapped := fmt.Sprintf("sudo -S -p '' -- bash -c %s", shellQuote(cmd))
```

- `shellQuote`：POSIX 单引号转义——`"'" + strings.ReplaceAll(s, "'", `'\''`) + "'"`。单引号内无任何元字符，转义是**完备的**（不存在逃逸形态）；这是整条修复的安全边界（§4.1）。
- 选 `bash -c` 而非 `sh -c`：现状语义 = 远端登录 shell（本部署全部为 bash）解析整条；`sh`（Ubuntu 下为 dash）会在 bashism 命令上引入**新的失败面**。`bash` 在本部署全部 Linux 目标存在；无 bash 的目标 fail 报错（§9.1 登记，不静默）。
- 远端解析结果：登录 shell 消解外层单引号后，sudo 的 argv 恒为 `[bash, -c, <原始cmd>]`；`<原始cmd>` 再由 bash 完整解析——**双层解析正确保持原命令语义，且 sudo 域覆盖整条**。
- 密码喂入、timeout、watchdog、`(exitCode, timedOut, err)` 分类：全部不变。前后台共用 kernel，**一处改动双通道生效**。
- `-p ''` 保留（部分 sudo 版本尊重它；不尊重的靠批 2a 剥离兜底——批 1 独立成立，不依赖批 2）。

### 1.2 行为变化表（compat-matrix 素材）

| 命令形态 | 旧行为（v0.10.0） | 新行为 |
|---|---|---|
| 单命令 `df -h` | 整条 sudo，argv 直达 | 不变（多一层 `bash -c`，argv 等价；exit/stdout 无差） |
| 复合 `a; b` | **a=sudo、b=原用户（缺陷）** | **整条 root** |
| 管道/逻辑 `a && b`、`a | b` | 同上部分降权 | 整条 root |
| exit_code | 最后一段的 exit（可能来自非 sudo 段） | `bash -c` 整体 exit（语义与"登录 shell 跑整条"一致） |
| 引号/`$`/反引号 | 登录 shell 单层解析 | 外层单引号消解后由 bash 单层解析——**等价** |

复合命令提权域扩大是**有意的**：这正是调用方的请求语义。风险面：复合命令中原本"顺带"以普通用户执行的段（调用方未曾察觉的降权段）将变为 root 执行——升级说明中显式提示（§7）。

### 1.3 批 1 回归测试矩阵

1. **复合全程提权**：`id; id` 两段均 uid=0（fake-sudo 记录 argv，断言 `bash -c` 包装 + 转义完整）。
2. **转义完备 round-trip**：cmd 含 `'`、`"`、`$`、`` ` ``、`\`、换行、`;`、`&&` → 远端收到的 argv 与原始串逐字节等价（`printf '%s'` 回显断言）。
3. **注入不逃逸**：`'; touch /tmp/pwn` 形态 → 转义后原样传入、无额外命令执行（fake-sudo argv 断言 + 目标文件不存在断言）。
4. **单命令回归**：现有 sudo 测试（sudo_test.go）零改动通过（argv 形态变化允许，行为断言不变）。
5. **exit 传播**：`exit 7` / `sh -c 'exit 7'` 类命令 → exitCode=7 透传。
6. **后台特权同测**：exec_background sudo=true 路径复测 1-3（kernel 共享，钉住双通道一致）。
7. **认证失败回归**：错误密码 → exit 1 + stderr 特征不变（fail-loud 语义未被包装破坏）。

## 2. 批 2a：sudo envelope 可观测性

### 2.1 字段

`ExecOutput` 增加可选字段（批 1 不动 envelope，批 2a 独立合入）：

```go
type SudoInfo struct {
    Requested bool   `json:"requested"`            // 恒 true（sudo=true 时 Sudo 对象才存在）
    Outcome   string `json:"outcome"`              // elevated | auth-failed | unverified
    UID       int    `json:"uid,omitempty"`        // 标记行实证的 uid；省略 = 未实证
}
// ExecOutput:
Sudo *SudoInfo `json:"sudo,omitempty" jsonschema:"present when sudo=true; outcome: elevated (marker line confirmed), auth-failed (sudo rejected the credential — the command did NOT run), unverified (no marker line — treat with suspicion)"`
```

### 2.2 标记行协议（uid 实证）

- 注入：wrapper 内层字符串前缀一段 `echo __SSHMGR_SUDO_<nonce>:uid=$(id -u) >&2; `（nonce = crypto/rand 8 字节 hex，每调用生成；nonce 为 hex 无元字符，内层无需引号）→ 完整内层再经 §1.1 `shellQuote`。
- 解析（broker，前台路径）：stderr 缓冲按行重组后匹配 `^__SSHMGR_SUDO_<nonce>:uid=(-?\d+)$` → 剥除该行、记 UID、`outcome=elevated`。
- **标记行缺席 = 信号**：无标记且非 auth-failed → `outcome=unverified`（提示调用方提权未实证——sh -c 逃逸/异常 shell 形态的防御纵深）。
- 后台路径（exec_background）：kernel 同源自动携带标记行；解析在任务聚合侧做（bgtools），行缓冲跨增量读的边缘见 §9.3。

### 2.3 auth-failed 判定与 fail-loud

- 特征匹配（在提示串剥离**之前**对原始 stderr 做）：`sudo: 1 incorrect password attempt`、`sudo: no password was provided`、`sudo: a password is required`。SSH exec 通道无 locale，sudo 输出稳定英文（依据：两次会话实测）。
- 特征命中 → `outcome=auth-failed` 且 **MCP 层 status=error**（fail-loud 升格：认证失败不再仅以 exit_code=1 的正常结果呈现，工具调用报错，agent 无法忽视）。特征采取精确全串匹配（保守，避免把命令自身的 exit-1 失败误判为认证失败）。
- 未配置 sudo（`SudoCredentialID` 空）维持现有 `no_sudo` 错误路径不变。

### 2.4 提示串剥离

broker 对最终 stderr 逐行剥离 `^\[sudo\] password for [^:]+: $`（§0.6 实测 `-p ''` 压不掉）。剥离发生在特征匹配之后。提示串不保留到 envelope（对调用方零信息量）。

## 3. 批 2b：`exec_context` 诊断工具

新 MCP 工具（Q4 定案）：`exec_context(server_id, sudo?)` —— 一轮返回执行通道的真实上下文，替代 agent 手工拼 `id; cat /proc/self/uid_map; …`（正是反馈 5+ 轮误判的手工形态，且拼法本身就踩 §0.1 的切断缺陷）。

- 实现：单次 exec，内层命令由 broker 用分段标记拼装（复用 §1.1 `shellQuote`；`sudo=true` 时整条走批 1 wrapper——**本工具依赖批 1 落地**，其 root 形态才有正确语义）：

```bash
echo __SSHMGR_CTX_<nonce>:id; id
echo __SSHMGR_CTX_<nonce>:tty; [ -t 0 ] && echo tty || echo no-tty
echo __SSHMGR_CTX_<nonce>:uidmap; cat /proc/self/uid_map
echo __SSHMGR_CTX_<nonce>:lsm; cat /proc/self/attr/current 2>/dev/null || echo none
echo __SSHMGR_CTX_<nonce>:sshenv; echo "client=$SSH_CLIENT conn=$SSH_CONNECTION"
echo __SSHMGR_CTX_<nonce>:proc; echo "pid=$$ ppid=$PPID comm=$(cat /proc/self/comm)"
```

- 输出结构化为 JSON 字段：`uid/gid/groups、tty、uid_map、lsm_label、ssh_client、ssh_connection、pid/ppid/comm`。
- 安全：纯自省只读，零新增攻击面；不暴露任何凭据材料。
- 价值锚点：下次任何"身份/权限"类异常，调用方一轮拿到全部鉴别数据——本反馈 5+ 轮误判与排查会话的空输出误判，该工具均可一轮终结。

## 4. 安全分析

- **§4.1 转义即安全边界**：复合命令整体进入 sudo 域后，任何转义缺陷 = 特权注入面。单引号方案在 POSIX 语义下完备（单引号内无元字符）；§1.3-2/3 以对抗性用例钉死。转义函数放 sshbroker 包内单点实现，禁止各处自行拼接。
- **提权域扩大（有意，登记）**：复合命令从部分降权变整体提权——这是契约修复，但意味着调用方复合命令中的每一段都会 root 执行。缓解：envelope（批 2a）让身份可见；升级文档显式提示行为变化（§7）。`sudo=true` 本就是显式特权请求，无静默提权（默认 sudo=false 路径零变化）。
- **auth-failed fail-loud**：消灭"看似正常结果实为未提权"的最后形态（exit_code=1 但无结构化标注的旧形态）。
- **标记行 nonce**：防命令输出与标记行碰撞（固定串会被命令自身输出伪造，nonce 每调用新生成）。
- **不新增凭据暴露**：标记行/剥离/上下文均为输出侧加工；密码喂入路径不变（喂后即弃，不落记录——Plan 32 语义保持）。

## 5. 兼容性

- **v0.10.1（批 1）× v0.10.0**：单命令行为逐字节等价；复合命令提权域扩大（bug 修复，§1.2 表）。client 与 serve 双端各自升级各自生效（§0.7：cache 模式随客户端、http 模式随 serve）；一端旧一端新无协议层不一致（wrapper 是执行侧内部构造，不涉及双方协议）。
- **v0.11.0（批 2a/2b）**：envelope 增字段纯增量（老消费方忽略）；`exec_context` 新工具；与 Plan 40（clientops/cli/store/tui）**零文件交集**，合并顺序互不阻塞（grilling 已核）。
- 旧版调用方（手写 driver）读新增字段：JSON 向前兼容，无破坏。

## 6. 测试计划（汇总）

批 1 矩阵见 §1.3；批 2a：

8. 标记行：fake-sudo 场景断言 stderr 出现 `__SSHMGR_SUDO_<nonce>:uid=…`、envelope `sudo.outcome=elevated`、`uid` 正确、返回 stderr 中标记行已剥离。
9. auth-failed：fake-sudo 输出 `sudo: 1 incorrect password attempt` → `outcome=auth-failed` + 工具 error；命令未执行的副作用断言。
10. unverified：fake-sudo 吞掉标记（不回显）→ `outcome=unverified`（uid 省略）。
11. 提示串剥离：stderr 混入 `[sudo] password for x: ` → 返回 stderr 无该行；auth-failed 特征匹配在剥离前完成（顺序测试）。
12. 后台特权：exec_background sudo=true 的标记行在聚合输出中可解析、剥离正确（§9.3 边缘钉测试）。

批 2b：

13. exec_context：sudo=false / sudo=true 双态字段齐全（uid_map/lsm_label/tty 解析正确；`sudo=true` 时 uid=0 与无 tty 同轮可见——正是本反馈场景的回归钉）。

## 7. 文档联动

- `README.md` / `docs/agent-access.md`：sudo 语义更新——"复合命令（`;`/`&&`/管道）**整条**以 root 执行（v0.10.1 起）；升级提示：旧版后续段以登录用户执行的依赖（不应存在但需声明）已失效"。
- `docs/compat-matrix.md`：登记 v0.10.1 行为变化（§1.2 表）+ v0.11.0 envelope/exec_context。
- `docs/backlog.md`：销项（反馈问题 2、问题 3 归因与修复）；登记（§9 全部 + 反馈问题 1 三建议归位 Plan 40 的指针）。
- `docs/threat-model.md`：登记"提权域=整条命令 + 转义完备性为安全边界"（§4.1/4.2）。

## 8. 交付物与批次

- **批 1（v0.10.1 hotfix）**：`runSudoSession` wrapper 改造 + `shellQuote` + §1.3 矩阵。最小 diff（单文件核心），独立可发。**这是安全相关正确性缺陷，建议优先发**。
- **批 2a**：envelope `SudoInfo` + 标记行注入/解析 + auth-failed fail-loud + 提示串剥离（sshbroker + mcpserver types/core/bgtools）+ §2 测试。
- **批 2b**：`exec_context` 工具（mcpserver 新 handler，复用 shellQuote）+ §3 测试。依赖批 1。
- 批 2a/2b 并入 v0.11.0 节奏（与 Plan 40 并行开发、零交集，合并顺序自由）。

## 9. 残余与登记

1. **无 bash 的远端目标**（Alpine/busybox 类）：`bash -c` 报 command not found → fail 报错（不静默）。本部署全部 Linux 目标为 Ubuntu 系，接受为残余；出现该类目标时再议 fallback（`sh -c` 或可配置 shell），YAGNI。
2. **登录 shell 非 bash 的目标**（csh 类）：外层单引号消解在 csh 下语义基本一致（单引号同为强引用），但未实证；登记。出现时用 exec_context 鉴别。
3. **后台标记行的增量读边缘**：SSH channel 流无行界保证，标记行可能跨 `exec_output` 增量边界；聚合侧按行重组直至换行才匹配，残缺行不误判（落为 unverified 而非错误）。测试 §6-12 钉住。
4. **bashism 语义**：复合命令由 bash -c 解析，与现状（登录 shell=bash）一致——非变化；仅当目标登录 shell 非 bash 时存在漂移（并入 §9.2）。
5. **反馈证据 5 存疑**（`stat %t:%T` 返回 `0:0`）：主体归因（wrapper 切断）已解释其余全部观察，此条不阻塞；留待 console 对照复核（owner 手边顺带），复核无果则按孤例闭卷。
6. **`-p ''` 无效**（§0.6）：根因在远端 sudo 版本行为，不在本仓库；broker 剥离为永久兜底，不再追 sudo 侧。
7. **问题反馈问题 1 三建议的归位指针**：建议 1 = Plan 40 §1 P0（同一修复）；建议 3 文案 = 搭 Plan 40 P0 PR；建议 2 自愈 = 依赖 Plan 40 §2.5 lazy-pull 实例化，Plan 40 第一批后另立小项。本 plan 不碰锚点代码（grilling 定案）。
8. **eval/评分影响**：xcheck 默认 agent 集（codex+kimi）若评 sudo 语义，行为变化（§1.2）应在 eval task 描述中同步——登记给 eval 维护者。
