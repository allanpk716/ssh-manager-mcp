# Plan 41 设计:sudo 执行完整性(复合命令整体提权)+ 提权可观测性(envelope/exec_context)

> 2026-08-26 grilling 定案(问题反馈会话,owner 全部确认"按你推荐的来"),本文不重议的决策:
> **问题反馈问题 3 归因销案 = ssh-manager-mcp wrapper 的 shell 语义缺陷,非服务器侧拦截层**(E4 鉴别 + 2026-08-26 两次会话全量观察逐条落位,零残留——§0.2);
> **修法 = wrapper 整体提权(`bash -c` 单引号转义包装),批 1 hotfix 先行**;
> **问题 2 修法 = 完整方案(envelope `sudo:{outcome,uid}` + 失败分类 fail-loud + 提示串剥离)**;
> **exec_context 诊断工具加入**;
> **E1-E3 鉴别实验不跑**(无拦截层可鉴别);**pty:true 归 backlog 不做**;
> **问题 1(锚点)三建议全部归位 Plan 40**(建议 1 = Plan 40 §1 P0 同一修复;建议 3 文案搭 P0 PR;建议 2 自愈依赖 Plan 40 实例化 lazy-pull,延后登记)——本 plan 不碰 clientops/锚点任何代码。
>
> **rev1(2026-08-26)**:一轮外部评审 + 闭环查证/实验全部吸收(`UID *int`、删 Requested、marker 融合防御、LC_ALL=C 前缀、失败分类 fail-loud、sudoers 前置条件、变量扩展行为表、exec_context sshenv 提权前捕获、stat 0:0 销案)。
> **rev2(2026-08-26)**:二轮复审 + 闭环(LC_ALL 穿透转有意行为、`exec env` 语法修正、`--` 后 VAR=val 陷阱登记、时序不变量改述 + wrap-failed 分类、NOEXEC 部分执行语义、Sorry-user 特征、剥离双作用域、规则 1 写死、/proc/$$/comm、超时三层树回归钉、首-marker 规则)。
> **rev3(2026-08-26,终版,owner 特批书面级收口——三轮评审轨迹 13→13→11 单调收敛,高危 4→3→0 清零,末轮 11 条全为一行修/文字级/登记级,双家口径"方向成立、无阻塞性 bug")**:三轮复审意见全部吸收,不再审。关键修订:
> ① **marker uid 改 bash 内建 `$EUID`**(双家共识:原 `$(id -u)` 依赖外部命令 + PATH,NOEXEC 拦 execve 时 `id` 正是被拦对象——marker 变 `uid=` 失配,与 NOEXEC 分析自相矛盾;内建免疫,且内建优先成为 marker 协议纪律);
> ② **wrapper 前缀增补 `BASH_ENV=`(置空)**:非交互 bash 启动前执行 `$BASH_ENV` 指向的脚本,经 sudo 环境保留时构成注入面(可在真 marker 前伪造/覆盖)——空值赋值使 bash 跳过启动文件,注入面封死(内层 unset 来不及:注入发生在 -c 脚本执行前);
> ③ **LC_ALL 穿透措辞收窄**:env_keep 保留 LC_* 是 Debian/Ubuntu 打包默认而非 sudo 编译默认,精简 sudoers 可剥离——"内层 locale 恒 C"降级为"**保留形态(本部署)下为 C;剥离形态下由 LANG 决定,部署依赖**"。**特征匹配不依赖穿透**:它匹配的是 sudo 进程自身的诊断文本,由前缀 `LC_ALL=C` 直接保证(sudo 进程层),与内层无关。不采用 `-- env LC_ALL=C` 强制传递:env 为外部命令,与 ① 的内建优先纪律相悖(NOEXEC 下引入新挂点);
> ④ **NOEXEC 语义随 ① 更新**:`$EUID` 内建不被拦 → NOEXEC 下 marker 可正常输出、uid 实证仍有效(原"marker 可能变空"矛盾消除);NOEXEC 仍登记为不兼容形态(外部命令部分被拦,行为不完整);
> ⑤ **不变量措辞按多行场景收窄**(§2.2);"marker 缺席 ⇒ 命令未执行"断言收窄 + 特征匹配限定诊断区(stderr 前 8 行,防极端截断形态下用户输出误判)(§2.3);
> ⑥ 提示串正则尾随空格可选;注入空行剥除限定"marker 紧前最多一行"(§2.4);fail-loud 行为变化与批 1 文档同批进 compat/交付物(§5/§7/§8);多行语法错误、BASH_ENV 注入、EUID 用例进测试(§6)。
>
> 来源:`D:\SynologyDrive\服务器\ssh-manager-mcp问题反馈-2026-08-26.md`(问题 2/3)+ 2026-08-26 gitlab-urit 排查与三轮闭环验证实证。

## 0. 现状事实(2026-08-26 于 v0.10.0 核实;函数名锚定)

1. **wrapper 切断(本 plan 的根因)**:`runSudoSession`(internal/sshbroker/sudo.go:46)拼接 `wrapped := fmt.Sprintf("sudo -S -p '' -- %s", cmd)` 后交远端登录 shell 解析。POSIX shell 语义下 **`--` 只把第一个简单命令置于 sudo 域**,其后所有 `;` / `&&` / `||` / `|` 分隔的段落脱离 sudo、以**原登录用户**执行。前台 `ExecSudo` 与后台特权变体 `ExecSudoWriters` 共用此 kernel——一处缺陷,前后台通吃。
2. **实证证据链**(gitlab-urit):2026-08-26 两次会话中每一例"root 神秘 EACCES"均逐条归因为"该段落在分号之后、以原用户执行、被普通 DAC 拒绝"。原反馈证据 5(stat 0:0)已解释为 GNU stat 对非设备文件的正常行为。**全部观察落位,零残留**。
3. **静默降级的调用方契约背叛**:工具文档承诺 "broker runs `sudo -S` for you",调用方按"整条命令在 root 下"预期构造复合命令,实际后续段静默降权。
4. **envelope 无身份字段**:`ExecOutput`(types.go:35-45)无 sudo 身份/结果信息。
5. **fail-loud 已存在的部分**:`no_sudo` 直接报错;密码错 → sudo exit 1 + stderr 特征,命令不跑。**缺的是成功身份实证、结构化失败语义、启动/包装失败分类**。
6. **`-p ''` 实测无效且提示串无换行**:Ubuntu 18.04 / sudo 1.8.21p2 下提示串仍输出到 stderr 且不带换行(实测 `[sudo] password for uritgitadmin: MARK$` 单行)。任何行首锚定的 stderr 解析必须容忍该前缀;压提示串只能靠 broker 端剥离。
7. **exec 的执行位置**:cache 模式本地直连;http 直连模式 serve 端执行。双端各自生效,沿用 v0.10.0 双端部署纪律。
8. **测试设施**:internal/testsshd + sudo_test.go 已有 fake-sudo 形态。
9. **闭环实验实证(2026-08-26 三轮,gitlab-urit + 本机)**:
   - 提示串无换行融合(§0.6);
   - **locale 保留**:env_reset 后 `LANG=en_US.UTF-8` 到达内层、`$HOME` 保留(实测,该部署形态)——env_keep 保留面含 locale 与 HOME;
   - **`sudo --` 后不识别 VAR=val**(实测 command not found);**`exec VAR=val cmd` 非法**(实测)——环境传递仅登录 shell 前缀赋值或 `env` 命令两条路(后者为外部命令,marker 协议纪律弃用);
   - 变量扩展:`$USER`→root(实测,通用)、`$HOME` 部署依赖;
   - `SSH_CLIENT`/`SSH_CONNECTION`:root 下为空(实测)——exec_context 捕获必须在提权前;
   - 伪造模拟:marker 缺席前置 + 诊断区限定在可达形态免疫;**多 marker 无首-marker 规则时 parseLast 被劫持**——"只采首个"必要;
   - **bash 解析先于执行**(实测:语法错误先于 marker echo;多行场景解析边界按行/命令);
   - **超时三层树无孤儿**(实测)。

## 1. 批 1(建议 v0.10.1 hotfix):wrapper 整体提权——正确性修复

**原则:调用方请求了 sudo,就是请求整条命令以特权执行。`--` 穿不透 shell 分隔符是缺陷,不是可协商的语义。**

### 1.1 变更点

`runSudoSession` 的 wrapper 从

```go
wrapped := fmt.Sprintf("sudo -S -p '' -- %s", cmd)
```

改为

```go
wrapped := fmt.Sprintf("BASH_ENV= LC_ALL=C sudo -S -p '' -- bash -c %s", shellQuote(cmd))
```

- `shellQuote`:POSIX 单引号转义——`"'" + strings.ReplaceAll(s, "'", `'\''`) + "'"`。单引号内无任何元字符,转义是**完备的**;这是整条修复的安全边界(§4.1)。
- 选 `bash -c` 而非 `sh -c`:与现状登录 shell(bash)语义最接近;`sh`(dash)在 bashism 上引入新失败面。
- **`BASH_ENV=`(置空)**:非交互 bash 启动前执行 `$BASH_ENV` 指向的脚本——经 sudo 环境保留时,攻击者可在真 marker 前输出伪造 marker 或覆盖 `echo`。空值使 bash 跳过启动文件,注入面封死。**必须在 bash 启动前清空**(内层 unset 来不及:注入发生在 -c 脚本执行前),故用登录 shell 前缀赋值。
- **`LC_ALL=C`(两层语义,rev3 定稿)**:① 固定 **sudo 进程自身**诊断文本语言——特征匹配的可靠性仅依赖此层,前缀直接保证,**不依赖任何穿透**;② 内层命令 locale:保留形态(env_keep 含 LC_*,Debian/Ubuntu 打包默认,本部署实测)下为 C;**精简 sudoers 剥离形态下由 LANG 决定——部署依赖**,行为表如实登记。不采用 `-- env LC_ALL=C` 强制传递(env 为外部命令,与内建优先纪律相悖,NOEXEC 下引入新挂点)。
- **环境传递纪律**:wrapper 内环境传递只能经登录 shell 前缀赋值——`sudo --` 后不识别 `VAR=val`(实测 command not found);`exec VAR=val cmd` 非法(实测);`env` 命令为外部命令(marker 协议纪律弃用)。
- 远端解析结果:登录 shell 消解外层单引号后,sudo 的 argv 恒为 `[bash, -c, <原始cmd>]`;双层解析保持原命令语义,sudo 域覆盖整条。
- 密码喂入、timeout、watchdog、`(exitCode, timedOut, err)` 分类不变(超时三层树无孤儿已实测钉住,§1.3-9)。前后台共用 kernel。
- `-p ''` 保留(部分版本尊重;其余靠批 2a 剥离)。

**sudoers 前置条件(兼容边界)**:整体提权要求 sudo 授权为**通用命令形态**。**command-specific sudoers / 命令审计 allowlist** 部署下 `sudo bash` 被策略拒绝 → §2.3 `sudo-start-failed`(fail-loud)。**NOEXEC** 形态:sudo 允许 bash 启动、拦截后续 execve——**`$EUID` 等内建不被拦,marker 与 uid 实证仍有效**;外部命令部分被拦(行为不完整),登记为不兼容形态(真实行为待 sudoers fixture 实证,§9 文档级待办)。两种形态均不静默、不自动降级;本部署全为通用授权,无感。

### 1.2 行为变化表(compat-matrix 素材)

| 命令形态 | 旧行为(v0.10.0) | 新行为 |
|---|---|---|
| 单命令 `df -h` | 整条 sudo,argv 直达 | 不变(多一层 `bash -c`,argv 等价) |
| 复合 `a; b` | **a=sudo、b=原用户(缺陷)** | **整条 root** |
| 管道/逻辑 `a && b`、`a \| b` | 同上部分降权 | 整条 root |
| exit_code | 最后一段的 exit(可能来自非 sudo 段) | `bash -c` 整体 exit |
| 引号/反引号 | 登录 shell 单层解析 | 外层单引号消解后由 bash 单层解析——**等价** |
| 变量/tilde 扩展 | 登录用户 shell 先扩展 | root bash 扩展:实测 `$USER`→root(通用);`$HOME` 部署依赖;`~` 随 HOME |
| 登录 shell 环境 | 后续段由 sshd 拉起的 shell 解析(bash 经 ssh 非交互**读 `~/.bashrc`**:PATH 增补/export 生效) | 内层 bash -c 在 sudo 下**不读 .bashrc**;`$0`→`bash`;`$$` 为内层 bash pid |
| **locale** | 命令随会话 locale | **sudo 诊断恒 C**;**内层命令 locale:保留形态(本部署)为 C,剥离形态由 LANG 决定——部署依赖**(rev3 措辞;依赖 locale 输出格式的命令自查) |
| **BASH_ENV** | (登录 shell 段可被 ~/.bashrc 等价物影响) | **置空,启动文件注入面封死**(§1.1) |
| sudo 诊断文本语言 | 随会话 locale | 恒 C |

### 1.3 批 1 回归测试矩阵

1. 复合全程提权:`id; id` 双段 uid=0(fake-sudo 断言 argv + `BASH_ENV=`/`LC_ALL=C` 前缀)。
2. 转义完备 round-trip:含 `'`、`"`、`$`、`` ` ``、`\`、换行、`;`、`&&` → argv 逐字节等价。
3. 注入不逃逸:`'; touch /tmp/pwn` 无额外执行。
4. 单命令回归:现有 sudo 测试行为断言零改动通过。
5. exit 传播:`exit 7` → exitCode=7。
6. 后台特权同测 1-3。
7. 认证失败回归:exit 1 + stderr 特征不变。
8. 变量扩展对比钉:`echo [$USER]` vs `bash -c 'echo [$USER]'` 差异断言。
9. 超时三层树无孤儿(实测钉):timeout 击杀 `sudo→bash→sleep` 树后 bash 与孙进程均终止。
10. **BASH_ENV 注入断言**:环境设 `BASH_ENV` 指向伪造 marker 脚本,断言其不执行(marker 只有一条、为真值)。

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

无 `Requested` 字段(`Sudo` 存在即 requested)。`UID *int` 无 omitempty;测试含 JSON 断言 `"uid":0` 字面存在。

### 2.2 标记行协议(uid 实证)

- 注入:wrapper 内层字符串 = `echo >&2; echo __SSHMGR_SUDO_<nonce>:uid=$EUID >&2; ` + cmd(nonce = crypto/rand 8 字节 hex)→ 经 `shellQuote`。
  - 首个空 `echo >&2` 为融合防御(实测提示串无换行):终结提示串行,保证标记行行首锚定。
  - **`$EUID` 为 bash 内建**(rev3):无外部命令依赖、无 PATH 依赖、NOEXEC 的 execve 拦截对内建无效——marker 与 uid 实证在 NOEXEC 形态下仍然有效。**内建优先是 marker 协议纪律**(探针一律用 shell 内建,不用外部命令)。
  - **时序不变量(rev3 收窄措辞)**:注入的空行 + marker 是 wrapper 内层脚本的**第一个可执行输出**;用户命令的执行性输出不可能先于它——解析失败的诊断除外(bash 解析先于执行:此时**无任何执行性输出**;多行场景解析按行/命令边界,同属性)。伪造防御基于此:执行性输出(伪造所需)必在 marker 之后;`BASH_ENV` 注入面已由 wrapper 置空封死(§1.1)。
- 解析(broker,按行重组):`^(?:\[sudo\] password for [^:]+: )?__SSHMGR_SUDO_<nonce>:uid=(\d+)$`(容忍提示串前缀;`\d+` 无负号)。**只采首个匹配**:同 nonce 的后续 marker 行全部剥除(命令可经 `/proc/$PPID/cmdline` 读到 nonce 后伪造后续 marker——模拟证实 parseLast 会被劫持,首-marker 规则免疫)。命中 → 剥除标记行、记 UID、`outcome=elevated`。
- 标记缺席 → 依 §2.3 分类。
- 后台路径:kernel 同源携带;聚合侧解析,跨增量行缓冲边缘见 §9.3。

### 2.3 失败分类与 fail-loud(rev3 定稿)

对**原始 stderr**(先于任何剥离)按序判定:

1. **找 marker**(§2.2 正则,取首个)。
2. **marker 在** → 身份实证。仅检查 marker 之前区域(正常 = 空行/提示串):命中失败特征 = 时序异常(伪造或不变量破坏)→ **固定 `outcome=unverified`,不升 error**(命令已实际执行,exit_code 已表达其结果)。
3. **marker 缺席** → stderr 通常全部为包装层诊断区且命令未执行(认证/启动/解析失败均未执行)。**断言收窄(rev3)**:极端截断形态(通道异常等)下存在"命令已执行而 marker 丢失"的残余——缓解:**特征匹配限定诊断区(stderr 前 8 行)**,把用户大输出误判的概率压到残余级;残余登记 §9。按特征族分类(文本由 sudo 进程层 `LC_ALL=C` 固定为英文,不依赖穿透;正则族覆盖变体):
   - **auth-failed 族**:`sudo: \d+ incorrect password attempts?`、`no password was provided`、`Account or password is expired`;
   - **sudo-start-failed 族**:`is not in the sudoers file`、`Sorry, user .* is not allowed to execute`、`command not found`、`unable to execute`;
   - **wrap-failed 族**:`bash: -c: .*syntax error`、`syntax error near unexpected token` 等 bash 解析错误形态;
   - 命中任一族 → 对应 `outcome` + **MCP 层 status=error**(fail-loud:命令未执行类失败不以正常结果呈现),错误文本透传诊断摘要;
   - 无命中 → `unverified`(未知形态,显式呈现,不升 error)。
4. **NOEXEC 形态**(文档级,待 fixture 实证):bash 可启动、`$EUID` marker 正常输出、外部命令部分被拦——落**正常结果**(marker 定 outcome,exit_code 表达失败)。**部分执行风险**在 §1.1/§7 登记,不承诺"未运行"。

**语义收束**:auth-failed / sudo-start-failed / wrap-failed 三类**已知"命令未执行"形态**fail-loud;`unverified` 为显式第四态。"看似正常结果实为未提权/未执行"的旧形态被消灭;未知形态显式标注。

### 2.4 提示串剥离(rev3 细化)

- **marker 在**:提示串只可能在 marker 之前区域(或融合行)——**仅处理该区域**:剥除行内 `[sudo] password for [^:]+:? ?` 子串(**尾随空格/冒号后空格可选**——sudo 版本变体);**注入的空 echo 行剥除限定为 marker 紧前最多一行**(注入产物唯一位置;前区其余空行保留——sudo lecture 等合法空行不误吃);**marker 之后区域绝不触碰**(命令自身输出,含 grep sudo 日志等合法含提示串字面的诊断场景)。
- **marker 缺席**(§2.3 第 3 类,命令未执行):诊断区(前 8 行,§2.3)剥除提示串子串(无用户输出可腐蚀)。
- 剥除发生在 §2.3 判定之后。

## 3. 批 2b:`exec_context` 诊断工具

新 MCP 工具:`exec_context(server_id, sudo?)` —— 一轮返回执行通道的真实上下文。

- **sshenv 捕获在提权前**(实测 env_reset 后 `SSH_CLIENT`/`SSH_CONNECTION` 为空,域内捕获得误导性空值):
  - `sudo=false`:单条命令分段拼装(分段标记 + `shellQuote`)。
  - `sudo=true`:命令拼装为 `echo __SSHMGR_CTX_<nonce>:sshenv C=[$SSH_CLIENT] S=[$SSH_CONNECTION] >&2; exec env LC_ALL=C sudo -S -p '' -- bash -c <shellQuote(上下文主体)>` ——**`env` 不可省**(实测 `exec LC_ALL=C sudo` 报 command not found;`--` 后亦不识别 VAR=val)。**本处 `env` 为可接受例外**:exec_context 是诊断工具非 marker 协议路径,且此处环境传递目标明确(exec 环境下无法用前缀赋值覆盖)。sshenv 段在登录 shell 层先行;密码喂入复用 broker stdin 机制。实现细节 plan 阶段收口。
- 上下文主体:

```bash
echo __SSHMGR_CTX_<nonce>:id; id
echo __SSHMGR_CTX_<nonce>:tty; [ -t 0 ] && echo tty || echo no-tty
echo __SSHMGR_CTX_<nonce>:uidmap; cat /proc/self/uid_map
echo __SSHMGR_CTX_<nonce>:lsm; cat /proc/self/attr/current 2>/dev/null || echo none
echo __SSHMGR_CTX_<nonce>:proc; echo "pid=$$ ppid=$PPID comm=$(cat /proc/$$/comm)"
```

(`comm` 用 `/proc/$$/comm`——命令替换进程内 `/proc/self` 指向 cat 自己。)
- 输出结构化 JSON:`uid/gid/groups、tty、uid_map、lsm_label、ssh_client、ssh_connection、pid/ppid/comm`。
- 安全:纯自省只读,零新增攻击面。
- 价值锚点:身份/权限类异常一轮拿全鉴别数据。

## 4. 安全分析

- **§4.1 转义即安全边界**:单引号方案 POSIX 完备;对抗性用例钉死(§1.3-2/3);转义函数单点实现。
- **§4.2 提权域扩大(有意,登记)**:复合命令整条 root;envelope 身份可见;升级文档显式提示。
- **§4.3 伪造免疫与不变量**:"执行性输出必在 marker 后"(§2.2 收窄版)+ **BASH_ENV 注入面封死**(§1.1)是免疫性的全部依赖;首-marker 规则封死"读 nonce 伪造后续 marker"路径。测试三钉(时序断言、双 marker fixture、BASH_ENV 注入断言)。
- **§4.4 fail-loud 分类**:三类"命令未执行"形态升 MCP error;unverified 显式呈现。
- **§4.5 标记行 nonce**:每调用新生成;首-marker 规则防同 nonce 伪造。
- **§4.6 不新增凭据暴露**:输出侧加工;密码喂后即弃。

## 5. 兼容性

- **v0.10.1(批 1)× v0.10.0**:单命令等价;复合提权域扩大;变量扩展/$0/$$/.bashrc/locale/BASH_ENV 行为变化(§1.2 表)。双端各自生效,无协议层不一致。
- **v0.11.0(批 2a)调用方可观察行为变化(显式登记)**:auth-failed / sudo-start-failed / wrap-failed 从"exit 非零的正常结果"升格为 **MCP error**——依赖旧行为的调用方(把 exit_code 当唯一信号的脚本)需适配;compat-matrix 独立行登记。
- **sudoers 形态前置条件**(§1.1):通用命令授权形态必需;command-specific / 审计 allowlist → sudo-start-failed 拒绝;NOEXEC → 外部命令部分执行(builtin/marker 有效),登记不兼容。
- envelope 增字段纯增量;exec_context 新工具;与 Plan 40 零文件交集,合并顺序自由。
- 旧调用方读新字段:JSON 向前兼容。

## 6. 测试计划(汇总)

批 1 矩阵见 §1.3;批 2a:

8. uid=0 序列化断言:elevated JSON 字面含 `"uid":0`。
9. 融合形态解析:stderr = `[sudo] password for x: __SSHMGR_SUDO_<n>:uid=0`(单行)→ 命中、uid=0、残余清理。
10. 时序不变量断言:wrapper 前缀形态;**bash 语法错误形态**(marker 缺席 + `syntax error` → wrap-failed);**多行/后置语法错误用例**(rev3 补——多行脚本的解析边界回归)。
11. 失败分类各族:fake-sudo 输出单数/复数/expired/not-in-sudoers/Sorry-user-not-allowed/command-not-found/unable-to-execute/bash-syntax-error → 对应 outcome + error;命令未执行副作用断言。
12. 伪造免疫:elevated + 伪造特征串在 marker 后 → elevated;marker 前命中特征 → unverified 不升 error。
13. 双 marker fixture:首真(uid=0)+ 次伪造(uid=1000)→ 只采首个。
14. 剥离作用域:marker 后区域含提示串字面(grep 日志形态)→ 原样保留;marker 前提示串剥除(含无尾随空格变体);注入空行剥除限定紧前一行;sudo lecture 空行保留。
15. LC_ALL 断言:fake-sudo 捕获自身环境 `LC_ALL=C`(sudo 进程层);内层命令 locale 按部署形态断言(保留形态 C / 剥离形态跳过)。
16. 后台特权:标记解析 + 剥离在聚合路径正确(§9.3)。
17. **EUID 用例**:fake-sudo 环境断言 marker 由内建生成(无 `id` 依赖——PATH 置空时 marker 仍完整)。

批 2b:

18. exec_context 双态字段齐全;sudo=true 态 sshenv 非空(提权前捕获)+ uid=0 与 no-tty 同轮可见;comm 字段为 bash(非 cat)。

## 7. 文档联动

- `README.md` / `docs/agent-access.md`:sudo 语义更新(复合命令整条 root;变量/tilde 由 root bash 扩展;$0/$$/.bashrc/locale/BASH_ENV 变化;**失败升 MCP error 的调用方适配提示**)。**批 1 的文档随 v0.10.1 同批发布**(rev3:hotfix 引入的行为变化不能只归后续批次)。
- `docs/managing-servers.md`:sudo 通道前置条件(通用授权形态;command-specific 拒绝形态;NOEXEC 部分执行风险;失败分类语义)。
- `docs/compat-matrix.md`:v0.10.1 行为变化(§1.2 表)+ sudoers 形态行 + **v0.11.0 fail-loud 行为变化行**(rev3)+ envelope/exec_context。
- `docs/backlog.md`:销项(反馈问题 2/3、原证据 5);登记(§9 全部 + 问题 1 归位 Plan 40 指针)。
- `docs/threat-model.md`:提权域 + 转义边界 + 首-marker 规则 + 时序不变量 + BASH_ENV 封死。

## 8. 交付物与批次

- **批 1(v0.10.1 hotfix)**:wrapper(`bash -c` + `BASH_ENV=` + `LC_ALL=C`)+ `shellQuote` + §1.3 矩阵 + **文档同批**(§7 rev3)。最小 diff,独立可发,**建议优先发**。
- **批 2a**:envelope(五态 outcome)+ 标记协议(EUID/融合防御/首-marker)+ 分类 fail-loud + 作用域剥离 + §2 测试。
- **批 2b**:exec_context(env 语法修正版)+ §3 测试。依赖批 1。
- 批 2a/2b 并入 v0.11.0(与 Plan 40 并行、零交集)。

## 9. 残余与登记

1. **无 bash / bash 不被 sudoers 策略允许**:command-specific / 审计 allowlist → `sudo-start-failed`(fail-loud);**NOEXEC → 外部命令部分执行、内建与 marker 有效**(文档级措辞,真实行为待 sudoers fixture 环境实证,生产机不可动 sudoers,登记为待办)。出现新形态目标再议探测/降级(YAGNI)。
2. **登录 shell 非 bash(csh 类)**:外层单引号基本一致;`BASH_ENV= LC_ALL=C` 前缀赋值语法 csh 不支持(`setenv` 形态不同)→ wrapper 语法错(fail 不静默)。
3. **后台标记行跨增量边界**:按行重组直至换行才匹配;残缺不误判(落 unverified)。
4. bashism:与现状一致;仅非 bash 登录 shell 目标漂移(并入 §9.2)。
5. ~~存疑孤例~~ 已销案(rev1):stat %t:%T 对目录恒 0:0,GNU stat 正常行为。
6. **`-p ''` 无效 + 提示串无换行**:远端 sudo 版本行为;三层防御(空行注入/前缀容忍/作用域剥除)+ 尾随空格可选。
7. **问题反馈问题 1 归位 Plan 40 指针**(不变)。
8. **eval/评分影响**(不变)。
9. **错密码文本变化面未实测**:无喂错密码通道;LC_ALL=C(sudo 进程层)+ 正则族防御性覆盖,未覆盖变体落 unverified 显式呈现。
10. **wrapper 环境传递纪律**:`sudo --` 后不识别 VAR=val;`exec VAR=val` 非法;`env` 为外部命令(marker 协议纪律弃用,exec_context 诊断路径例外 §3)。
11. **极端截断残余**(rev3 登记):通道异常截断可致"命令已执行而 marker 丢失"——§2.3 特征匹配已限诊断区(前 8 行)缓解,残余概率极低且误判形态落 unverified/显式 error(可观察、可上报),不再加机制。
