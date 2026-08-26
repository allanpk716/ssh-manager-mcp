# Plan 41 设计:sudo 执行完整性(复合命令整体提权)+ 提权可观测性(envelope/exec_context)

> 2026-08-26 grilling 定案(问题反馈会话,owner 全部确认"按你推荐的来"),本文不重议的决策:
> **问题反馈问题 3 归因销案 = ssh-manager-mcp wrapper 的 shell 语义缺陷,非服务器侧拦截层**(E4 鉴别 + 2026-08-26 两次会话全量观察逐条落位,零残留——§0.2);
> **修法 = wrapper 整体提权(`bash -c` 单引号转义包装),批 1 hotfix 先行**;
> **问题 2 修法 = 完整方案(envelope `sudo:{outcome,uid}` + 失败分类 fail-loud + 提示串剥离)**;
> **exec_context 诊断工具加入**;
> **E1-E3 鉴别实验不跑**(无拦截层可鉴别);**pty:true 归 backlog 不做**;
> **问题 1(锚点)三建议全部归位 Plan 40**(建议 1 = Plan 40 §1 P0 同一修复;建议 3 文案搭 P0 PR;建议 2 自愈依赖 Plan 40 实例化 lazy-pull,延后登记)——本 plan 不碰 clientops/锚点任何代码。
>
> **rev1(2026-08-26,闭环修订)**:一轮外部评审 + 闭环查证/实验全部吸收。要点:`UID *int`(原 `int+omitempty` 吞 uid=0)、删 `Requested` 恒真字段、marker 融合防御(空行注入+前缀容忍)、`LC_ALL=C` 前缀、auth-failed/sudo-start-failed 分类 fail-loud、sudoers 前置条件、变量扩展行为表、exec_context sshenv 提权前捕获、marker 时序不变量、stat 0:0 销案。全量实证记录见 rev1 头部(此处不重列)。
>
> **rev2(2026-08-26,复审闭环,硬上限第 2 轮修订)**:一轮复审(2 家,13 条)+ 闭环(第一类 10/10 证实;实验 3 条执行:2 成立、1 不成立留回归钉;1 条不可执行文档级处置)全部吸收。关键修订:
> ① **LC_ALL 穿透改为有意行为**(双家共识:sudo 默认 env_keep 保留 LC_*/LANG,`LC_ALL=C` 到达内层——目标机实测 `lang=[en_US.UTF-8]` 到达内层佐证保留机制;rev1 "只作用于 sudo 进程"声明与真实行为矛盾):**内层命令 locale 恒 C** 登记进行为表与测试预期(§1.1/§1.2/§6-15);
> ② **§3 `exec` 语法修复**:`exec LC_ALL=C sudo` 是语法错误(exec 后的词被当可执行文件名,本机实测 `exec: LC_ALL=C: not found`)→ 改 `exec env LC_ALL=C sudo`;
> ③ **新 wrapper 陷阱登记**(实测):`sudo --` **之后**不识别 `VAR=val` 形态(报 command not found)——wrapper 内环境传递只能经登录 shell 前缀赋值层;
> ④ **时序不变量改述**(复审证实 bash -c 先整体解析、语法错误先于一切执行):不变量收敛为"**执行性输出必在 marker 之后**"(伪造防御不受影响);marker 缺席的 stderr 可能为 sudo 诊断**或 bash 解析错误**——新增 `wrap-failed` 分类(特征族补 bash 语法错误形态,亦 fail-loud);
> ⑤ **NOEXEC 语义修正**:sudo-start-failed 定义收窄为"sudo 无法启动提权命令";NOEXEC 形态按文档语义登记为"bash 可启动、部分 builtin/redirection 已以 root 执行、外部命令失败"——**部分执行风险**写入不兼容登记(非"干净拒绝");真实行为待 sudoers fixture 环境实证(§9 文档级待办);
> ⑥ **sudoers 拒绝文本补**:`Sorry, user … is not allowed to execute …` 形态(原特征族匹配不到);
> ⑦ **剥离限定作用域**:marker 之后的命令输出**绝不触碰**(grep sudo 日志的诊断主场景防腐蚀);空行清理写死(注入空行只可能在 marker 紧前,无条件剥除;命令区空行不动);
> ⑧ §2.3 规则 1 语义写死(marker 在 + 前区命中特征 = 时序异常 → 固定 `unverified`、不升 error);
> ⑨ `comm` 改 `/proc/$$/comm`(原 `$(cat /proc/self/comm)` 读到的是 cat 自己);
> ⑩ 行为表补登录 shell 环境面(.bashrc 不再被读、`$0`→bash、`$$` 变化);超时三层树无孤儿进回归钉(实测干净,防版本变体);**"只采首个 marker"** 写进解析规则(模拟证实 parseLast 会被伪造 marker 劫持)+ 双 marker 测试。
>
> 来源:`D:\SynologyDrive\服务器\ssh-manager-mcp问题反馈-2026-08-26.md`(问题 2/3)+ 2026-08-26 gitlab-urit 排查与两轮闭环验证实证。

## 0. 现状事实(2026-08-26 于 v0.10.0 核实;函数名锚定)

1. **wrapper 切断(本 plan 的根因)**:`runSudoSession`(internal/sshbroker/sudo.go:46)拼接 `wrapped := fmt.Sprintf("sudo -S -p '' -- %s", cmd)` 后交远端登录 shell 解析。POSIX shell 语义下 **`--` 只把第一个简单命令置于 sudo 域**,其后所有 `;` / `&&` / `||` / `|` 分隔的段落脱离 sudo、以**原登录用户**执行。前台 `ExecSudo` 与后台特权变体 `ExecSudoWriters` 共用此 kernel——一处缺陷,前后台通吃。
2. **实证证据链**(gitlab-urit):2026-08-26 两次会话中每一例"root 神秘 EACCES"均逐条归因为"该段落在分号之后、以原用户执行、被普通 DAC 拒绝"。原反馈证据 5(stat 0:0)已解释为 GNU stat 对非设备文件的正常行为。**全部观察落位,零残留**。
3. **静默降级的调用方契约背叛**:工具文档承诺 "broker runs `sudo -S` for you",调用方按"整条命令在 root 下"预期构造复合命令,实际后续段静默降权。
4. **envelope 无身份字段**:`ExecOutput`(types.go:35-45)无 sudo 身份/结果信息。
5. **fail-loud 已存在的部分**:`no_sudo` 直接报错;密码错 → sudo exit 1 + stderr 特征,命令不跑。**缺的是成功身份实证、结构化失败语义、启动/包装失败分类**。
6. **`-p ''` 实测无效且提示串无换行**:Ubuntu 18.04 / sudo 1.8.21p2 下提示串仍输出到 stderr 且不带换行(实测 `[sudo] password for uritgitadmin: MARK$` 单行)。任何行首锚定的 stderr 解析必须容忍该前缀;压提示串只能靠 broker 端剥离。
7. **exec 的执行位置**:cache 模式本地直连;http 直连模式 serve 端执行。双端各自生效,沿用 v0.10.0 双端部署纪律。
8. **测试设施**:internal/testsshd + sudo_test.go 已有 fake-sudo 形态。
9. **闭环实验实证(2026-08-26 两轮,gitlab-urit + 本机)**:
   - 提示串无换行融合(见 §0.6);
   - **locale 保留与穿透**:env_reset 后 `LANG=en_US.UTF-8` 到达内层(实测)、`$HOME` 保留(实测)——sudo 默认 env_keep 保留面含 locale 与 HOME;据此 `LC_ALL=C` 前缀会穿透到内层命令(rev2 起为有意行为);
   - **`sudo --` 后不识别 VAR=val**(实测:`sudo -- LC_ALL=C bash …` → `sudo: LC_ALL=C: command not found`);
   - 变量扩展:旧形态(登录 shell)`$USER=uritgitadmin`;新形态(root bash)`$USER=root`(实测)、`$HOME` 该机不变(部署依赖);
   - `SSH_CLIENT`/`SSH_CONNECTION`:登录 shell 非空,root 下为空(实测)——exec_context 捕获必须在提权前;
   - auth-failed 伪造(模拟):marker 缺席前置 + 诊断区限定在可达形态下免疫;**多 marker 无"只采首个"规则时 parseLast 被伪造 marker 劫持**(模拟证实)——首-marker 规则必要;
   - **bash -c 先整体解析**(本机实测:`bash -c $'echo M >&2; )'` 语法错误先于 echo,无 M 输出)——marker 缺席的 stderr 可能是 bash 解析错误而非 sudo 诊断;
   - **exec 语法**(本机实测):`exec LC_ALL=C …` 报 command not found;
   - **超时三层树无孤儿**(实测):真通道 5s timeout 击杀 sudo→bash→sleep 孙进程树,bash 与 sleep 均干净终止。

## 1. 批 1(建议 v0.10.1 hotfix):wrapper 整体提权——正确性修复

**原则:调用方请求了 sudo,就是请求整条命令以特权执行。`--` 穿不透 shell 分隔符是缺陷,不是可协商的语义。**

### 1.1 变更点

`runSudoSession` 的 wrapper 从

```go
wrapped := fmt.Sprintf("sudo -S -p '' -- %s", cmd)
```

改为

```go
wrapped := fmt.Sprintf("LC_ALL=C sudo -S -p '' -- bash -c %s", shellQuote(cmd))
```

- `shellQuote`:POSIX 单引号转义——`"'" + strings.ReplaceAll(s, "'", `'\''`) + "'"`。单引号内无任何元字符,转义是**完备的**;这是整条修复的安全边界(§4.1)。
- 选 `bash -c` 而非 `sh -c`:与现状登录 shell(bash)语义最接近;`sh`(dash)在 bashism 上引入新失败面。
- **`LC_ALL=C` 前缀(两层语义,rev2 定稿)**:① 固定 **sudo 自身**诊断文本语言(特征匹配前提);② **穿透到内层命令——内层命令 locale 恒为 C,这是有意行为**(sudo 默认 env_keep 保留 LC_*/LANG,目标机实测 locale 与 HOME 均到达内层)。取舍:换取 sudo 诊断稳定 + 命令输出可预期;依赖会话 locale 的命令(日期/排序格式等)输出形态会变,登记 §1.2 行为表 + §7 升级提示。**环境传递纪律**:wrapper 内任何环境变量传递只能经登录 shell 前缀赋值层——`sudo --` 之后不识别 `VAR=val` 形态(实测报 command not found)。
- 远端解析结果:登录 shell 消解外层单引号后,sudo 的 argv 恒为 `[bash, -c, <原始cmd>]`;双层解析保持原命令语义,sudo 域覆盖整条。
- 密码喂入、timeout、watchdog、`(exitCode, timedOut, err)` 分类不变(超时三层树无孤儿已实测钉住,§1.3-9)。前后台共用 kernel。
- `-p ''` 保留(部分版本尊重;其余靠批 2a 剥离)。

**sudoers 前置条件(兼容边界)**:整体提权要求 sudo 授权为**通用命令形态**。**command-specific sudoers / 命令审计 allowlist** 部署下 `sudo bash` 被策略拒绝 → §2.3 `sudo-start-failed`(fail-loud)。**NOEXEC** 形态按文档语义:sudo 允许 bash 启动、拦截后续 execve——**marker 可能已输出、部分 builtin/redirection 已以 root 执行**、外部命令才失败(部分执行风险,非干净拒绝;真实行为待 sudoers fixture 实证,§9 文档级待办)。两种形态均不静默、不自动降级;本部署全为通用授权,无感。

### 1.2 行为变化表(compat-matrix 素材)

| 命令形态 | 旧行为(v0.10.0) | 新行为 |
|---|---|---|
| 单命令 `df -h` | 整条 sudo,argv 直达 | 不变(多一层 `bash -c`,argv 等价) |
| 复合 `a; b` | **a=sudo、b=原用户(缺陷)** | **整条 root** |
| 管道/逻辑 `a && b`、`a \| b` | 同上部分降权 | 整条 root |
| exit_code | 最后一段的 exit(可能来自非 sudo 段) | `bash -c` 整体 exit |
| 引号/反引号 | 登录 shell 单层解析 | 外层单引号消解后由 bash 单层解析——**等价** |
| 变量/tilde 扩展 | 登录用户 shell 先扩展 | root bash 扩展:实测 `$USER`→root(通用);`$HOME` 部署依赖;`~` 随 HOME |
| **登录 shell 环境** | 后续段由 sshd 拉起的 shell 解析(bash 经 ssh 非交互**读 `~/.bashrc`**:PATH 增补/export 生效) | 内层 bash -c 在 sudo 下**不读 .bashrc**;`$0`→`bash`;`$$` 为内层 bash pid。依赖 .bashrc 环境的命令行为变化,升级提示登记 |
| **locale** | 命令随会话 locale(en_US.UTF-8 等) | **内层命令 locale 恒 C**(LC_ALL 穿透,有意行为)——依赖 locale 输出格式的命令形态会变 |
| sudo 诊断文本语言 | 随会话 locale | 恒 C |

### 1.3 批 1 回归测试矩阵

1. 复合全程提权:`id; id` 双段 uid=0(fake-sudo 断言 argv + `LC_ALL=C` 前缀)。
2. 转义完备 round-trip:含 `'`、`"`、`$`、`` ` ``、`\`、换行、`;`、`&&` → argv 逐字节等价。
3. 注入不逃逸:`'; touch /tmp/pwn` 无额外执行。
4. 单命令回归:现有 sudo 测试行为断言零改动通过。
5. exit 传播:`exit 7` → exitCode=7。
6. 后台特权同测 1-3。
7. 认证失败回归:exit 1 + stderr 特征不变。
8. 变量扩展对比钉:`echo [$USER]` vs `bash -c 'echo [$USER]'` 差异断言。
9. **超时三层树无孤儿**(实测钉):timeout 击杀 `sudo→bash→sleep` 树后,bash 与孙进程均终止(进程表断言;sshd/sudo 版本变体防御)。

## 2. 批 2a:sudo envelope 可观测性

### 2.1 字段

`ExecOutput` 增加可选字段(批 1 不动 envelope,批 2a 独立合入):

```go
type SudoInfo struct {
    Outcome string `json:"outcome"` // elevated | auth-failed | sudo-start-failed | wrap-failed | unverified
    UID     *int   `json:"uid"`     // 无 omitempty:nil = 未实证;0 = root(实证)。指针避免 omitempty 吞 uid=0
}
// ExecOutput:
Sudo *SudoInfo `json:"sudo,omitempty" jsonschema:"present iff sudo=true; outcome: elevated (marker confirmed this uid), auth-failed (sudo rejected the credential — the command did NOT run), sudo-start-failed (sudo could not start the elevated command: policy/binary — the command did NOT run), wrap-failed (the wrapped shell rejected the command (e.g. bash syntax error) — the command did NOT run), unverified (no marker — treat identity as unproven)"`
```

无 `Requested` 字段(`Sudo` 存在即 requested,owner 拍板)。`UID *int` 无 omitempty;测试含 JSON 断言 `"uid":0` 字面存在。

### 2.2 标记行协议(uid 实证)

- 注入:wrapper 内层字符串 = `echo >&2; echo __SSHMGR_SUDO_<nonce>:uid=$(id -u) >&2; ` + cmd(nonce = crypto/rand 8 字节 hex)→ 经 `shellQuote`。
  - 首个空 `echo >&2` 为融合防御(实测提示串无换行):终结提示串行,保证标记行行首锚定。
  - **时序不变量(rev2 改述)**:bash -c **先整体解析**——用户命令有语法错误时诊断先于一切执行(此时无任何命令输出);解析通过后,两条 echo(空行+标记)先于用户命令。因此**执行性输出必在 marker 之后**——伪造防御基于此(命令输出不可能出现在 marker 之前;"marker 前出现失败特征" = 时序异常,§2.3 规则 3)。
- 解析(broker,按行重组):`^(?:\[sudo\] password for [^:]+: )?__SSHMGR_SUDO_<nonce>:uid=(\d+)$`(容忍提示串前缀;`\d+` 无负号)。**只采首个匹配**:同 nonce 的后续 marker 行全部剥除(命令可经 `/proc/$PPID/cmdline` 读到 nonce 后伪造后续 marker——模拟证实 parseLast 会被劫持,首-marker 规则免疫)。命中 → 剥除标记行、记 UID、`outcome=elevated`。
- 标记缺席 → 依 §2.3 分类。
- 后台路径:kernel 同源携带;聚合侧解析,跨增量行缓冲边缘见 §9.3。

### 2.3 失败分类与 fail-loud(rev2 定稿)

对**原始 stderr**(先于任何剥离)按序判定:

1. **找 marker**(§2.2 正则,取首个)。
2. **marker 在** → 身份实证。仅检查 **marker 之前区域**(正常 = 空行/提示串):命中失败特征 = **时序异常**(伪造或不变量破坏)→ **固定 `outcome=unverified`,不升 error**(命令已实际执行,exit_code 已表达其结果;不与 "command did NOT run" 类别冲突)。
3. **marker 缺席** → 整个 stderr 为包装层诊断区(命令未执行,无用户输出,全域操作安全)。按特征族分类(文本由 `LC_ALL=C` 固定为英文;正则族覆盖变体):
   - **auth-failed 族**:`sudo: \d+ incorrect password attempts?`、`no password was provided`、`Account or password is expired`;
   - **sudo-start-failed 族**:`is not in the sudoers file`、**`Sorry, user .* is not allowed to execute`**(command-specific 拒绝的真实文本形态)、`command not found`、`unable to execute`;
   - **wrap-failed 族**(rev2 新增,bash -c 先解析实测):`bash: -c: .*syntax error`、`syntax error near unexpected token` 等 bash 解析错误形态;
   - 命中任一族 → 对应 `outcome` + **MCP 层 status=error**(fail-loud:命令未执行类失败不以正常结果呈现),错误文本透传诊断摘要;
   - 无命中 → `unverified`(未知形态,显式呈现,不升 error)。
4. **NOEXEC 形态语义**(文档级,待 fixture 实证):NOEXEC 下 bash 可启动、marker 可能已输出、部分 builtin 已以 root 执行后外部命令失败——落**正常结果**(marker 定 outcome,exit_code 表达失败)。**部分执行风险**在 §1.1/§7 登记,不承诺"未运行"。

**语义收束**:auth-failed / sudo-start-failed / wrap-failed 三类**已知"命令未执行"形态**fail-loud;`unverified` 为显式第四态。"看似正常结果实为未提权/未执行"的旧形态被消灭;未知形态显式标注。

### 2.4 提示串剥离(rev2 限定作用域)

- **marker 在**:提示串只可能在 marker 之前区域(或融合行)——**仅处理该区域**:剥除行内 `[sudo] password for [^:]+: ` 子串;**注入的空 echo 行无条件剥除**(它只可能出现在标记行紧前);**marker 之后区域绝不触碰**(命令自身输出,含 grep sudo 日志等合法含提示串字面的诊断场景,防腐蚀)。
- **marker 缺席**(§2.3 第 3 类,命令未执行):整个 stderr 是包装层诊断区,全域剥除提示串子串(无用户输出可腐蚀)。
- 剥除发生在 §2.3 判定之后。

## 3. 批 2b:`exec_context` 诊断工具

新 MCP 工具:`exec_context(server_id, sudo?)` —— 一轮返回执行通道的真实上下文。

- **sshenv 捕获在提权前**(实测 env_reset 后 `SSH_CLIENT`/`SSH_CONNECTION` 为空,域内捕获得误导性空值):
  - `sudo=false`:单条命令分段拼装(分段标记 + `shellQuote`)。
  - `sudo=true`:命令拼装为 `echo __SSHMGR_CTX_<nonce>:sshenv C=[$SSH_CLIENT] S=[$SSH_CONNECTION] >&2; exec env LC_ALL=C sudo -S -p '' -- bash -c <shellQuote(上下文主体)>` ——**`env` 不可省**(实测 `exec LC_ALL=C sudo` 报 command not found:exec 后的词被当可执行文件名;`--` 后亦不识别 VAR=val——环境传递只能走登录 shell 层的 env 命令或前缀赋值)。sshenv 段在登录 shell 层先行,主体经提权执行;密码喂入复用 broker stdin 机制。该工具自构完整命令(不走通用 wrapper),实现细节 plan 阶段收口。
- 上下文主体:

```bash
echo __SSHMGR_CTX_<nonce>:id; id
echo __SSHMGR_CTX_<nonce>:tty; [ -t 0 ] && echo tty || echo no-tty
echo __SSHMGR_CTX_<nonce>:uidmap; cat /proc/self/uid_map
echo __SSHMGR_CTX_<nonce>:lsm; cat /proc/self/attr/current 2>/dev/null || echo none
echo __SSHMGR_CTX_<nonce>:proc; echo "pid=$$ ppid=$PPID comm=$(cat /proc/$$/comm)"
```

(`comm` 用 `/proc/$$/comm`——命令替换进程内 `/proc/self` 指向 cat 自己,实测语义。)
- 输出结构化 JSON:`uid/gid/groups、tty、uid_map、lsm_label、ssh_client、ssh_connection、pid/ppid/comm`。
- 安全:纯自省只读,零新增攻击面。
- 价值锚点:身份/权限类异常一轮拿全鉴别数据。

## 4. 安全分析

- **§4.1 转义即安全边界**:单引号方案 POSIX 完备;对抗性用例钉死(§1.3-2/3);转义函数单点实现。
- **§4.2 提权域扩大(有意,登记)**:复合命令整条 root;envelope 身份可见;升级文档显式提示。
- **§4.3 伪造免疫与不变量**:"执行性输出必在 marker 后"(§2.2 改述版)是免疫性的全部依赖——bash 解析错误无执行性输出,伪造需执行、必在 marker 后;首-marker 规则封死"读 nonce 伪造后续 marker"路径(模拟证实劫持与免疫两态)。测试双钉(时序断言 + 双 marker fixture)。
- **§4.4 fail-loud 分类**:三类"命令未执行"形态升 MCP error;unverified 显式呈现。
- **§4.5 标记行 nonce**:每调用新生成,防输出碰撞;首-marker 规则防同 nonce 伪造。
- **§4.6 不新增凭据暴露**:输出侧加工;密码喂后即弃。

## 5. 兼容性

- **v0.10.1(批 1)× v0.10.0**:单命令等价;复合提权域扩大;变量扩展/$0/$$/.bashrc/locale 行为变化(§1.2 表)。双端各自生效,无协议层不一致。
- **sudoers 形态前置条件**(§1.1):通用命令授权形态必需;command-specific / 审计 allowlist → sudo-start-failed 拒绝;NOEXEC → **部分执行后失败**(非干净拒绝,风险登记)——compat-matrix 独立行。
- **v0.11.0(批 2a/2b)**:envelope 纯增量;exec_context 新工具;与 Plan 40 零文件交集,合并顺序自由。
- 旧调用方读新字段:JSON 向前兼容。

## 6. 测试计划(汇总)

批 1 矩阵见 §1.3;批 2a:

8. uid=0 序列化断言:elevated JSON 字面含 `"uid":0`。
9. 融合形态解析:stderr = `[sudo] password for x: __SSHMGR_SUDO_<n>:uid=0`(单行)→ 命中、uid=0、残余清理。
10. 时序不变量断言:wrapper 前缀形态(空 echo + 标记先行);**bash 语法错误形态**(marker 缺席 + `syntax error` 诊断 → wrap-failed,不误判 auth-failed)。
11. 失败分类各族:fake-sudo 输出单数/复数/expired/not-in-sudoers/**Sorry-user-not-allowed**/command-not-found/unable-to-execute/bash-syntax-error → 对应 outcome + error;命令未执行副作用断言。
12. 伪造免疫:elevated + 伪造特征串在 marker 后 → elevated;**marker 前命中特征 → unverified 不升 error**(规则 2)。
13. **双 marker fixture**:首真(uid=0)+ 次伪造(uid=1000)→ 只采首个(模拟已证 parseLast 劫持,此为回归钉)。
14. 剥离作用域:marker 后区域含提示串字面(grep 日志形态)→ **原样保留**;marker 前提示串剥除;无 marker 全域剥除;注入空行剥除、命令区空行不动。
15. LC_ALL 断言:fake-sudo 捕获自身环境 `LC_ALL=C`;**内层命令 `[$LC_ALL]` = C**(穿透有意行为的回归钉)。
16. 后台特权:标记解析 + 剥离在聚合路径正确(§9.3)。

批 2b:

17. exec_context 双态字段齐全;sudo=true 态 **sshenv 非空**(提权前捕获)+ uid=0 与 no-tty 同轮可见;`comm` 字段为 bash(非 cat)。

## 7. 文档联动

- `README.md` / `docs/agent-access.md`:sudo 语义更新(复合命令整条 root;变量/tilde 由 root bash 扩展;**内层 locale 恒 C**;$0/$$/.bashrc 变化;升级提示)。
- `docs/managing-servers.md`:sudo 通道前置条件(通用授权形态;command-specific 拒绝形态;**NOEXEC 部分执行风险**;失败分类语义)。
- `docs/compat-matrix.md`:v0.10.1 行为变化(§1.2 表)+ sudoers 形态行 + v0.11.0。
- `docs/backlog.md`:销项(反馈问题 2/3、原证据 5);登记(§9 全部 + 问题 1 归位 Plan 40 指针)。
- `docs/threat-model.md`:提权域 + 转义边界 + 首-marker 规则 + 时序不变量。

## 8. 交付物与批次

- **批 1(v0.10.1 hotfix)**:wrapper(`bash -c` + `LC_ALL=C`)+ `shellQuote` + §1.3 矩阵。最小 diff,独立可发,**建议优先发**。
- **批 2a**:envelope(五态 outcome)+ 标记协议(融合防御/首-marker)+ 分类 fail-loud + 作用域剥离 + §2 测试。
- **批 2b**:exec_context(env 语法修正版)+ §3 测试。依赖批 1。
- 批 2a/2b 并入 v0.11.0(与 Plan 40 并行、零交集)。

## 9. 残余与登记

1. **无 bash / bash 不被 sudoers 策略允许**:command-specific / 审计 allowlist → `sudo-start-failed`(fail-loud);**NOEXEC → 部分执行后失败**(bash 可启动、builtin 已 root 执行、外部命令被拦——文档级措辞,**真实行为待 sudoers fixture 环境实证**,生产机不可动 sudoers,登记为待办)。出现新形态目标再议探测/降级(YAGNI)。
2. **登录 shell 非 bash(csh 类)**:外层单引号基本一致;`LC_ALL=C` 前缀赋值语法 csh 不支持 → wrapper 语法错(fail 不静默)。
3. **后台标记行跨增量边界**:按行重组直至换行才匹配;残缺不误判(落 unverified)。
4. bashism:与现状一致;仅非 bash 登录 shell 目标漂移(并入 §9.2)。
5. ~~存疑孤例~~ 已销案(rev1):stat %t:%T 对目录恒 0:0,GNU stat 正常行为。
6. **`-p ''` 无效 + 提示串无换行**:远端 sudo 版本行为;三层防御(空行注入/前缀容忍/作用域剥除)。
7. **问题反馈问题 1 归位 Plan 40 指针**(不变)。
8. **eval/评分影响**(不变)。
9. **错密码文本变化面未实测**:无喂错密码通道;LC_ALL=C + 正则族防御性覆盖,未覆盖变体落 unverified 显式呈现,首次生产命中后补特征。
10. **wrapper 环境传递纪律**(rev2 实测登记):`sudo --` 后不识别 `VAR=val`(command not found);`exec VAR=val cmd` 非法。环境传递仅两条路:登录 shell 前缀赋值(`VAR=val sudo …`)或 `env` 命令(`exec env VAR=val sudo …`)。
