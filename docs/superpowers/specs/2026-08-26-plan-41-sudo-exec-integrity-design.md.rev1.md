# Plan 41 设计:sudo 执行完整性(复合命令整体提权)+ 提权可观测性(envelope/exec_context)

> 2026-08-26 grilling 定案(问题反馈会话,owner 全部确认"按你推荐的来"),本文不重议的决策:
> **问题反馈问题 3 归因销案 = ssh-manager-mcp wrapper 的 shell 语义缺陷,非服务器侧拦截层**(E4 鉴别 + 2026-08-26 两次会话全量观察逐条落位,零残留——§0.2);
> **修法 = wrapper 整体提权(`bash -c` 单引号转义包装),批 1 hotfix 先行**;
> **问题 2 修法 = 完整方案(envelope `sudo:{outcome,uid}` + auth-failed/sudo 启动失败 fail-loud + 提示串剥离)**;
> **exec_context 诊断工具加入**;
> **E1-E3 鉴别实验不跑**(无拦截层可鉴别);**pty:true 归 backlog 不做**;
> **问题 1(锚点)三建议全部归位 Plan 40**(建议 1 = Plan 40 §1 P0 同一修复;建议 3 文案搭 P0 PR;建议 2 自愈依赖 Plan 40 实例化 lazy-pull,延后登记)——本 plan 不碰 clientops/锚点任何代码。
>
> **rev1(2026-08-26,闭环修订)**:一轮外部评审(2 家均 SUGGEST_CHANGES,11 条意见)+ 闭环查证/实验(第一类 5/5 证实;实验 5 项:3 成立、1 部分成立、1 半条成立,含目标机实测与本机判定器模拟)全部吸收。关键修订:
> ① **§2.1 `UID` 改 `*int` 且无 omitempty**(评审共识:原 `int + omitempty` 恰好吞掉 uid=0——root 是零值,提权成功场景字段消失,违反"省略 = 未实证"语义;补 JSON 层 `"uid":0` 断言);同时**删除 `Requested` 恒真字段**(owner 拍板:`Sudo` 对象存在即 requested,零信息量字段不保留);
> ② **§2.2 marker 行融合防御**(目标机实测:sudo `-S` 管道场景提示串写 stderr **不带换行**,标记行接续同一行——`[sudo] password for x: __SSHMGR_…:uid=0` 单行形态;标记 echo 前注入空行 + 解析正则容忍提示串前缀,双保险);**marker 时序不变量入设计**(标记必须是 bash -c 内层第一条输出,判定器免疫性依赖它——不变量破坏时免疫失效,已模拟验证,测试钉住);
> ③ **§1.1 wrapper 加 `LC_ALL=C` 前缀**(目标机实测 `AcceptEnv LANG LC_*` 放行、"无 locale 故输出英文"的旧依据不成立;强制 C locale 固定 sudo 诊断文本);
> ④ **§2.3 auth-failed 判定整体加固**:特征族(单/复数、expired 变体)+ marker 缺席前置 + 只分析 marker 之前诊断区(模拟验证:可达形态下无前置会误判、有前置免疫);**新增 `sudo-start-failed` 分类**(not in sudoers / command not allowed / NOEXEC / bash 缺失等启动失败,均 MCP error fail-loud,原稿该面无分类规则);
> ⑤ **§1.1/§5 sudoers 策略兼容面登记**:整体提权要求通用命令授权形态;command-specific sudoers / NOEXEC / 命令审计 allowlist 部署下 `sudo bash` 被策略拒绝 → 落 sudo-start-failed 错误(不静默、不自动降级);
> ⑥ **§1.2 行为表补登变量扩展时序**(目标机实测:新形态 `$USER`→root、`$HOME` 该机不变(sudoers 配置决定,部署依赖);升级文档提示);
> ⑦ **§3 exec_context 的 sshenv 捕获移到提权前**(实测:env_reset 后 `SSH_CLIENT`/`SSH_CONNECTION` 为空,而提权态恰是诊断主场景);
> ⑧ uid 正则收紧 `\d+`;**§9.5 存疑孤例销案**(`stat %t:%T` 对目录恒 `0:0`——GNU stat 对非设备文件无设备号,本机实测目录 `0:0`、字符设备 `1:3`,无需 console 复核)。
>
> 来源:`D:\SynologyDrive\服务器\ssh-manager-mcp问题反馈-2026-08-26.md`(问题 2/3)+ 2026-08-26 gitlab-urit 排查与闭环验证会话实证。

## 0. 现状事实(2026-08-26 于 v0.10.0 核实;函数名锚定)

1. **wrapper 切断(本 plan 的根因)**:`runSudoSession`(internal/sshbroker/sudo.go:46)拼接 `wrapped := fmt.Sprintf("sudo -S -p '' -- %s", cmd)` 后交远端登录 shell 解析。POSIX shell 语义下 **`--` 只把第一个简单命令置于 sudo 域**,其后所有 `;` / `&&` / `||` / `|` 分隔的段落脱离 sudo、以**原登录用户**执行。前台 `ExecSudo`(exec_command sudo=true)与后台特权变体 `ExecSudoWriters`(exec_background sudo=true,Plan 32)共用此 kernel——**一处缺陷,前后台通吃**。
2. **实证证据链**(gitlab-urit,user=uritgitadmin,不在 docker 组,`/var/lib/docker` 为 `drwx--x--x root:root`):2026-08-26 两次会话中每一例"root 神秘 EACCES"均逐条归因为"该段落在分号之后、以 uritgitadmin 执行、被普通 DAC 拒绝"——`id` 段 uid=0 与被拒段从未同段;`docker system df && docker info | grep` 同一条命令两种身份;反馈"中途翻转" = 命令形态从单命令变复合;单命令后台 `du -x /`(sudo=true)root 成功穿过 `/var/lib/docker`;E4 服务清单无任何 EDR/HIDS;本机 root 控制台对照正常。原反馈证据 5(`stat %t:%T` 返回 `0:0`)已解释为 GNU stat 对非设备文件的正常行为(§9.5),**全部观察落位,零残留**。
3. **静默降级的调用方契约背叛**:工具文档承诺 "broker runs `sudo -S` for you — do NOT prepend sudo",调用方(尤其 AI agent)按"整条命令在 root 下"的预期构造复合命令,实际后续段静默降权。反馈"5+ 轮误判"与排查会话空输出误判均为其直接代价。
4. **envelope 无身份字段**:`ExecOutput`(internal/mcpserver/types.go:35-45)仅 stdout/stderr/exit_code/timed_out/truncated/bytes/effective_timeout_seconds。调用方无法区分"提权了但命令失败"与"根本没提权"。
5. **fail-loud 已存在的部分**:`SudoCredentialID` 为空时 `sudo=true` 直接报 `no_sudo`(internal/mcpserver/core.go:165-168),不降级;sudo 认证失败(密码错)→ sudo exit 1 + stderr 明确特征,命令不跑。**缺的是成功时的身份实证、结构化失败语义、与启动失败分类**。
6. **`-p ''` 实测无效**:Ubuntu 18.04 / sudo 1.8.21p2 下 `sudo -S -p ''` 仍向 stderr 输出 `[sudo] password for <user>: `。**且该提示串无换行**(2026-08-26 目标机 `cat -A` 实测:`[sudo] password for uritgitadmin: MARK$` 单行——管道场景提示串不终止,后续 stderr 输出接续同行)。压提示串只能靠 broker 端剥离,且任何行首锚定的 stderr 解析都必须容忍该前缀。
7. **exec 的执行位置(升级部署面)**:cache 模式(`mcp --cache`)= 本地 MCP 进程直连目标机执行;http 直连模式 = serve 端执行。wrapper 改动双端各自生效(§5),沿用 v0.10.0 双端部署纪律。
8. **测试设施**:internal/testsshd(sshd.go)+ internal/sshbroker/sudo_test.go 已有 fake-sudo 测试形态。
9. **闭环实验实证(2026-08-26,gitlab-urit + 本机)**:
   - 提示串无换行融合:见 §0.6,marker 协议的融合防御为必修项;
   - `AcceptEnv LANG LC_*` 放行(当前会话 LANG=en_US.UTF-8 而非 C;本机 locale -a 仅 C/C.UTF-8/en_US.utf8/POSIX,文本碰巧稳定——通用面上 locale 可注入,`LC_ALL=C` 强制必要);
   - 变量扩展:旧形态(登录 shell 扩展)`$USER=uritgitadmin`、`$HOME=/home/uritgitadmin`;新形态(root bash 扩展)实测 `$USER=root`、`$HOME=/home/uritgitadmin`(该机 sudoers 未重置 HOME——**$HOME 为部署依赖,$USER 变化为通用事实**);
   - `SSH_CLIENT`/`SSH_CONNECTION`:登录 shell 非空,root(env_reset 后)为空——exec_context 捕获必须在提权前;
   - auth-failed 伪造(本机判定器模拟,`.xcheck/20260826-171002/exp/authfail_sim.go`):无 marker 缺席前置时,命令输出伪造特征串可致误判;有前置时可达形态(伪造串在 marker 后)免疫、"伪造串在 marker 前"形态依赖"marker 先行"时序不变量方免疫——该不变量必须写入设计并测试钉住。

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

- `shellQuote`:POSIX 单引号转义——`"'" + strings.ReplaceAll(s, "'", `'\''`) + "'"`。单引号内无任何元字符,转义是**完备的**(不存在逃逸形态);这是整条修复的安全边界(§4.1)。
- 选 `bash -c` 而非 `sh -c`:现状语义 = 远端登录 shell(本部署全部为 bash)解析整条;`sh`(Ubuntu 下为 dash)会在 bashism 命令上引入**新的失败面**。`bash` 在本部署全部 Linux 目标存在。
- **`LC_ALL=C` 前缀**:固定 **sudo 自身**的诊断文本语言(评审闭环实测 `AcceptEnv LANG LC_*` 放行、会话 LANG 可为非 C——不强制则特征匹配在非英文环境漏判)。只作用于 sudo 进程,**不改变内层命令自身的 locale**(命令语义归调用方)。注意此为 POSIX shell 的 env 前缀赋值语法,csh 类登录 shell 不支持(§9.2 登记)。
- 远端解析结果:登录 shell 消解外层单引号后,sudo 的 argv 恒为 `[bash, -c, <原始cmd>]`;`<原始cmd>` 再由 bash 完整解析——**双层解析正确保持原命令语义,且 sudo 域覆盖整条**。
- 密码喂入、timeout、watchdog、`(exitCode, timedOut, err)` 分类:全部不变。前后台共用 kernel,一处改动双通道生效。
- `-p ''` 保留(部分 sudo 版本尊重它;不尊重的靠批 2a 剥离兜底——批 1 独立成立)。

**sudoers 前置条件(兼容边界,评审闭环补充)**:整体提权要求目标机的 sudo 授权为**通用命令形态**(密码对即全允许 / `ALL` 命令集)。**command-specific sudoers(allowlist 只放行特定命令)、`NOEXEC` 限定、命令审计策略**等部署形态下,`sudo bash` 会被策略拒绝——本方案与该形态**不兼容**,失败落 §2.3 的 `sudo-start-failed` 错误分类(fail-loud,不静默、**不自动降级**——静默降级正是本 plan 反对的形态)。不做启动期自动探测(owner 单人授权面,YAGNI);compat-matrix 与 managing-servers 文档登记该前置条件。本部署全部目标(gitlab-urit / 3090x2 等)为通用授权形态,无感。

### 1.2 行为变化表(compat-matrix 素材)

| 命令形态 | 旧行为(v0.10.0) | 新行为 |
|---|---|---|
| 单命令 `df -h` | 整条 sudo,argv 直达 | 不变(多一层 `bash -c`,argv 等价;exit/stdout 无差) |
| 复合 `a; b` | **a=sudo、b=原用户(缺陷)** | **整条 root** |
| 管道/逻辑 `a && b`、`a \| b` | 同上部分降权 | 整条 root |
| exit_code | 最后一段的 exit(可能来自非 sudo 段) | `bash -c` 整体 exit(语义与"登录 shell 跑整条"一致) |
| 引号/反引号 | 登录 shell 单层解析 | 外层单引号消解后由 bash 单层解析——**等价** |
| **变量/tilde 扩展**(`$USER`/`$HOME`/`$PATH`/`~`) | 登录用户 shell 先扩展 | **延迟到 root bash 扩展**:实测 `$USER` uritgitadmin→root(通用事实);`$HOME` 该机不变(sudoers 的 env_reset/always_set_home 配置决定,**部署依赖**);`~` 随 HOME。依赖变量扩展值的复合命令需自查(§7 升级提示) |
| sudo 诊断文本语言 | 随会话 locale | 恒 C locale(仅 sudo 自身报错,不含命令输出) |

### 1.3 批 1 回归测试矩阵

1. **复合全程提权**:`id; id` 两段均 uid=0(fake-sudo 记录 argv,断言 `bash -c` 包装 + 转义完整 + `LC_ALL=C` 前缀存在)。
2. **转义完备 round-trip**:cmd 含 `'`、`"`、`$`、`` ` ``、`\`、换行、`;`、`&&` → 远端收到的 argv 与原始串逐字节等价。
3. **注入不逃逸**:`'; touch /tmp/pwn` 形态 → 转义后原样传入、无额外命令执行。
4. **单命令回归**:现有 sudo 测试(sudo_test.go)行为断言零改动通过(argv 形态变化允许)。
5. **exit 传播**:`exit 7` → exitCode=7 透传。
6. **后台特权同测**:exec_background sudo=true 路径复测 1-3(kernel 共享,钉住双通道一致)。
7. **认证失败回归**:错误密码 → exit 1 + stderr 特征不变。
8. **变量扩展对比钉**(§0.9 实测回归):`echo [$USER]` 旧形态 vs `bash -c 'echo [$USER]'` 新形态断言差异(登录用户值 vs root 值)。

## 2. 批 2a:sudo envelope 可观测性

### 2.1 字段

`ExecOutput` 增加可选字段(批 1 不动 envelope,批 2a 独立合入):

```go
type SudoInfo struct {
    Outcome string `json:"outcome"` // elevated | auth-failed | sudo-start-failed | unverified
    UID     *int   `json:"uid"`     // 无 omitempty:nil = 未实证;0 = root(实证)。int 零值恰为 root,
                                    // 用指针避免 encoding/json 的 omitempty 语义吞掉 uid=0(闭环共识 bug)
}
// ExecOutput:
Sudo *SudoInfo `json:"sudo,omitempty" jsonschema:"present iff sudo=true; outcome: elevated (marker confirmed this uid), auth-failed (sudo rejected the credential — the command did NOT run), sudo-start-failed (sudo could not start the elevated command: policy/binary reasons — the command did NOT run), unverified (no marker — treat identity as unproven)"`
```

- **无 `Requested` 字段**(owner 拍板删除):`Sudo` 对象存在即 requested,恒真字段是 API 噪音。
- **`UID *int` 无 omitempty**:nil/缺席 = 未实证;`0` 是合法实证值(root)——测试必须含 JSON 层断言 `"uid":0` 字面存在。

### 2.2 标记行协议(uid 实证)

- 注入:wrapper 内层字符串 = `echo >&2; echo __SSHMGR_SUDO_<nonce>:uid=$(id -u) >&2; ` + cmd(nonce = crypto/rand 8 字节 hex,每调用生成;nonce 为 hex 无元字符,内层无需引号)→ 完整内层再经 §1.1 `shellQuote`。
  - **首个空 `echo >&2` 是融合防御**(闭环实测 §0.6:sudo 提示串无换行,首个标记行会接续成 `[sudo] password for x: __SSHMGR_…` 单行)——空行终结提示串,保证标记行行首锚定。
  - **marker 时序不变量**:标记(两条 echo)必须是 bash -c 内层**最前**的输出——判定器对"命令输出伪造特征串"的免疫性依赖该不变量(闭环模拟:伪造串在 marker 后 → 免疫;在 marker 前 → 免疫失效)。实现与测试双钉:wrapper 构造器单元断言前缀形态;e2e 断言 stderr 中标记行先于任何命令输出。
- 解析(broker,前台路径):stderr 按行重组后匹配 `^(?:\[sudo\] password for [^:]+: )?__SSHMGR_SUDO_<nonce>:uid=(\d+)$`——**正则容忍提示串前缀**(与空行注入构成双保险:任一生效即命中);`(\d+)` 不含负号(`id -u` 输出域无负数,uid_t 无符号)。命中 → 剥除该行、记 UID、`outcome=elevated`。
- **标记行缺席 = 信号**:无标记且非失败特征 → `outcome=unverified`(提示调用方提权未实证——防御纵深)。
- 后台路径(exec_background):kernel 同源自动携带标记;解析在任务聚合侧做(bgtools),行缓冲跨增量读的边缘见 §9.3。

### 2.3 失败分类与 fail-loud(闭环重构)

判定顺序(对原始 stderr,**先于任何剥离**):

1. **找 marker**(§2.2 正则)。有 marker → 身份实证;再仅检查 **marker 之前的诊断区**是否含失败特征(正常路径为空或仅提示串)——命中则该失败标记可疑(时序不变量破坏),按对应类别处理并置 `unverified` 倾向。
2. **无 marker → 整个 stderr 为 sudo 诊断区**,按特征族分类(全部要求 `LC_ALL=C` 已由 wrapper 固定为英文文本;正则族覆盖单/复数与变体——exec 通道 `-S` 单次喂密码不重试,复数形态按不可达处理但正则零成本覆盖):
   - **auth-failed 族**:`sudo: \d+ incorrect password attempts?`、`no password was provided`、`Account or password is expired`;
   - **sudo-start-failed 族**:`is not in the sudoers file`、`command not found`、`unable to execute`、`not authorized to execute`(涵盖 §1.1 的 command-specific/NOEXEC 策略拒绝与 bash 缺失);
   - 命中任一族 → 对应 `outcome` + **MCP 层 status=error**(fail-loud 升格:认证/启动失败不再以 exit_code=1 的正常结果呈现)。错误文本透传 sudo 诊断摘要。
   - 无命中 → `unverified`(未知形态,防御性呈现,不升 error——避免误判把真失败标成认证失败)。
3. **伪造防御**(闭环模拟验证):特征匹配以 **marker 缺席为前提**、且只分析 marker 之前区域——命令自身输出伪造精确特征串时(必然在 marker 之后)不触发误判;可达形态下免疫已验证,不变量由测试钉住(§2.2)。

**语义收束**(替代原稿"消灭最后形态"的过强表述):auth-failed 与 sudo-start-failed 两类**已知失败形态**fail-loud;`unverified` 是显式第三态而非静默——"看似正常结果实为未提权"的旧形态被消灭,未知形态显式标注。

### 2.4 提示串剥离

broker 对最终 stderr 做**中缀子串剥除**(非整行匹配——闭环实测提示串无换行、可与标记行融合):逐行剥除行内 `[sudo] password for [^:]+: ` 子串,行剩余内容保留(空行删除)。剥离发生在 §2.3 判定之后。

## 3. 批 2b:`exec_context` 诊断工具

新 MCP 工具:`exec_context(server_id, sudo?)` —— 一轮返回执行通道的真实上下文,替代 agent 手工拼 `id; cat /proc/self/uid_map; …`(正是反馈 5+ 轮误判的手工形态,且拼法本身踩 §0.1 切断缺陷)。

- **sshenv 捕获必须在提权前**(闭环实测:env_reset 后 `SSH_CLIENT`/`SSH_CONNECTION` 恒空,而提权态恰是诊断主场景——域内捕获会得到误导性空值):
  - `sudo=false` 形态:单条命令分段拼装(分段标记 + 复用 `shellQuote`)。
  - `sudo=true` 形态:命令拼装为 `echo __SSHMGR_CTX_<nonce>:sshenv C=[$SSH_CLIENT] S=[$SSH_CONNECTION] >&2; exec LC_ALL=C sudo -S -p '' -- bash -c <shellQuote(上下文主体)>` ——sshenv 段在**登录 shell 层**(sudo 域外)先行捕获,主体经提权执行;密码喂入复用 broker 的 stdin 机制(对内嵌 sudo 同样有效)。该工具走非通用 wrapper 通道(自构完整命令),实现时与 `runSudoSession` 的喂入逻辑对接,细节在 plan 阶段收口。
- 上下文主体(分段标记 `__SSHMGR_CTX_<nonce>:<field>` 拼装):

```bash
echo __SSHMGR_CTX_<nonce>:id; id
echo __SSHMGR_CTX_<nonce>:tty; [ -t 0 ] && echo tty || echo no-tty
echo __SSHMGR_CTX_<nonce>:uidmap; cat /proc/self/uid_map
echo __SSHMGR_CTX_<nonce>:lsm; cat /proc/self/attr/current 2>/dev/null || echo none
echo __SSHMGR_CTX_<nonce>:proc; echo "pid=$$ ppid=$PPID comm=$(cat /proc/self/comm)"
```

- 输出结构化 JSON:`uid/gid/groups、tty、uid_map、lsm_label、ssh_client、ssh_connection、pid/ppid/comm`。
- 安全:纯自省只读,零新增攻击面;不暴露任何凭据材料。
- 价值锚点:任何"身份/权限"类异常一轮拿全鉴别数据——本反馈 5+ 轮误判与排查会话空输出误判均可一轮终结。

## 4. 安全分析

- **§4.1 转义即安全边界**:复合命令整体进入 sudo 域后,任何转义缺陷 = 特权注入面。单引号方案在 POSIX 语义下完备;§1.3-2/3 以对抗性用例钉死。转义函数放 sshbroker 包内单点实现,禁止各处自行拼接。
- **§4.2 提权域扩大(有意,登记)**:复合命令从部分降权变整体提权——契约修复,但复合命令中每一段都会 root 执行。缓解:envelope 身份可见;升级文档显式提示(§7)。`sudo=true` 本就是显式特权请求,无静默提权(默认 sudo=false 路径零变化)。
- **§4.3 伪造免疫与不变量依赖**(闭环模拟):marker 缺席前置 + 诊断区限定使"命令输出伪造 auth-failed"在可达形态下免疫;该免疫依赖"marker 先行"时序不变量(§2.2),测试钉死。
- **§4.4 fail-loud 分类**:auth-failed / sudo-start-failed 两类已知失败升 MCP error;unverified 显式第三态。消灭"看似正常结果实为未提权"。
- **§4.5 标记行 nonce**:防命令输出与标记行碰撞;每调用新生成。
- **§4.6 不新增凭据暴露**:标记/剥离/上下文均为输出侧加工;密码喂入路径不变(喂后即弃,Plan 32 语义保持)。

## 5. 兼容性

- **v0.10.1(批 1)× v0.10.0**:单命令行为逐字节等价;复合命令提权域扩大(bug 修复,§1.2 表);变量扩展时序变化(§1.2 表,$USER 通用变化、$HOME 部署依赖)。client 与 serve 双端各自升级各自生效(§0.7);wrapper 是执行侧内部构造,不涉及双方协议,一端旧一端新无协议层不一致。
- **sudoers 形态前置条件**(§1.1):通用命令授权形态必需;command-specific / NOEXEC / 命令审计形态不兼容(失败落 sudo-start-failed,不静默)——compat-matrix 独立行登记。
- **v0.11.0(批 2a/2b)**:envelope 增字段纯增量(老消费方忽略);`exec_context` 新工具;与 Plan 40(clientops/cli/store/tui)**零文件交集**,合并顺序互不阻塞。
- 旧版调用方(手写 driver)读新增字段:JSON 向前兼容,无破坏。

## 6. 测试计划(汇总)

批 1 矩阵见 §1.3;批 2a:

8. **uid=0 序列化断言**:elevated 场景 envelope JSON 字面含 `"uid":0`(闭环共识 bug 的直接回归钉)。
9. **融合形态解析**(闭环实测 fixture):stderr = `[sudo] password for x: __SSHMGR_SUDO_<n>:uid=0`(单行)→ 解析命中(空行注入与正则容忍双路径各测一)、uid=0、剥离后该行残余为空。
10. **marker 时序不变量断言**:wrapper 构造器单元断言前缀 = 空 echo + 标记 echo;e2e 断言 stderr 标记行先于命令输出;模拟"伪造串在 marker 前"的反例用例(期望判定器不免疫 → 提示不变量破坏,引用 exp/authfail_sim.go fixture1b)。
11. auth-failed / sudo-start-failed:fake-sudo 输出各族特征(单数/复数/expired/not-in-sudoers/command-not-found/unable-to-execute)→ 对应 outcome + 工具 error;命令未执行副作用断言。
12. 伪造免疫:elevated(marker 在)+ stderr 含伪造特征串(在 marker 后)→ outcome=elevated(不误判)。
13. 提示串剥离:中缀剥除后无 `[sudo] password for``;判定在剥离前完成(顺序测试)。
14. 后台特权:标记行在聚合输出中可解析、剥离正确(§9.3 边缘钉测试)。
15. LC_ALL=C:fake-sudo 捕获自身环境断言 `LC_ALL=C`(仅 sudo 进程,内层命令 locale 不受影响的双向断言)。

批 2b:

16. exec_context:sudo=false / sudo=true 双态字段齐全;**sudo=true 态 sshenv 非空**(提权前捕获的回归钉)+ uid=0 与 no-tty 同轮可见(本反馈场景回归钉)。

## 7. 文档联动

- `README.md` / `docs/agent-access.md`:sudo 语义更新——"复合命令(`;`/`&&`/管道)**整条**以 root 执行(v0.10.1 起);**变量/tilde 由 root bash 扩展($USER→root;$HOME/PATH 视目标 sudoers 配置)**,依赖扩展值的复合命令请自查;升级提示:旧版后续段以登录用户执行的依赖(不应存在但需声明)已失效。
- `docs/managing-servers.md`:sudo 通道前置条件(通用命令授权形态;command-specific/NOEXEC 不兼容,失败形态说明)。
- `docs/compat-matrix.md`:登记 v0.10.1 行为变化(§1.2 表)+ sudoers 形态前置条件行 + v0.11.0 envelope/exec_context。
- `docs/backlog.md`:销项(反馈问题 2、问题 3 归因与修复、原反馈证据 5 存疑——stat 行为已解释);登记(§9 全部 + 反馈问题 1 三建议归位 Plan 40 的指针)。
- `docs/threat-model.md`:登记"提权域=整条命令 + 转义完备性为安全边界 + marker 先行不变量"(§4.1-4.3)。

## 8. 交付物与批次

- **批 1(v0.10.1 hotfix)**:`runSudoSession` wrapper 改造(`bash -c` + `LC_ALL=C`)+ `shellQuote` + §1.3 矩阵。最小 diff(单文件核心),独立可发。**安全相关正确性缺陷,建议优先发**。
- **批 2a**:envelope `SudoInfo` + 标记行注入/解析(融合防御)+ 失败分类 fail-loud + 提示串剥离(sshbroker + mcpserver types/core/bgtools)+ §2 测试。
- **批 2b**:`exec_context` 工具(sshenv 提权前捕获,复用 shellQuote)+ §3 测试。依赖批 1。
- 批 2a/2b 并入 v0.11.0 节奏(与 Plan 40 并行开发、零交集,合并顺序自由)。

## 9. 残余与登记

1. **无 bash 的远端目标**(Alpine/busybox 类):`bash -c` 报 command not found → 落 §2.3 `sudo-start-failed`(fail-loud,不静默)。**sudoers 策略形态前置条件**(§1.1):command-specific / NOEXEC / 命令审计部署不兼容,同落 `sudo-start-failed`;本部署全为通用授权形态,无感。出现新形态目标时再议探测/降级设计(YAGNI)。
2. **登录 shell 非 bash 的目标**(csh 类):外层单引号消解基本一致,但 **`LC_ALL=C` env 前缀赋值语法 csh 不支持**(rev1 升级登记)——该形态下 wrapper 报语法错(fail,不静默);出现时用 exec_context 鉴别并另议。
3. **后台标记行的增量读边缘**:SSH channel 流无行界保证,标记行可能跨 `exec_output` 增量边界;聚合侧按行重组直至换行才匹配,残缺行不误判(落 unverified 而非错误)。测试 §6-14 钉住。
4. bashism 语义:与现状(登录 shell=bash)一致,非变化;仅非 bash 登录 shell 目标存在漂移(并入 §9.2)。
5. ~~存疑孤例~~ **已销案(rev1)**:`stat -c "%t:%T" /var/lib/docker` 返回 `0:0` = GNU stat 对目录这类非设备文件的正常行为(%t/%T 仅对字符/块设备输出设备号;2026-08-26 本机实测:目录 `0:0`、字符设备 `/dev/null` `1:3`)。原"console 复核"待办取消,按已解释闭卷。
6. **`-p ''` 无效 + 提示串无换行**(§0.6):根因在远端 sudo 版本行为;空行注入 + 前缀容忍正则 + 中缀剥除三层防御,不再追 sudo 侧。
7. **问题反馈问题 1 三建议的归位指针**:建议 1 = Plan 40 §1 P0;建议 3 文案 = 搭 Plan 40 P0 PR;建议 2 自愈 = Plan 40 第一批后另立小项。本 plan 不碰锚点代码。
8. **eval/评分影响**:xcheck 默认 agent 集若评 sudo 语义,行为变化(§1.2)应在 eval task 描述同步。
9. **错密码文本变化面未实测**(闭环实验 4 半条):vault 只存正确密码,无喂错密码通道;`LC_ALL=C` + 正则族为防御性覆盖,实际文本以首次生产命中为准——若出现未覆盖变体,落 `unverified`(显式,不静默),按新形态补特征。
