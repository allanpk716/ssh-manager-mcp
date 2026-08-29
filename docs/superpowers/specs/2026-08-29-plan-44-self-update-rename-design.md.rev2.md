# Plan 44 设计:sshmgr 自更新 + 改名 ssh-manager → sshmgr

- 日期:2026-08-29
- 状态:**定稿**(rev2;第 1 轮盲评 25 条+第 2 轮 27 条[合并 22 项]全闭环;owner 终审通过 2026-08-29)
- 前置:Plan 42 批1 已并入本地 master(5dd51e4,未推送未发版);本 plan 与批1 **捆发 v0.13.0**(Q6 拍板)
- 上游事实(2026-08-29 实测):NUC10 exe 在 `C:\Users\allan716\ssh-manager.exe`(用户目录);服务 `ssh-manager-serve` RUNNING(LocalSystem,BINARY_PATH_NAME 指向该 exe);NUC10 → GitHub API 可达(200);**本仓 v0.12.0 真实资产 302 的 Location 宿主 = `release-assets.githubusercontent.com`(curl 实测)**;发版管线 = goreleaser → GitHub Releases(资产 zip/tar.gz + checksums.txt)

## 1. 背景与诉求

现状更新 = 手工三步(停服务 → 换二进制 → 重启服务),v0.10.0 双端部署即此。诉求:

1. **serve 运行中也能一条命令更新**(`sshmgr update`),不退出不手工换文件;client 端(笔记本)同样。
2. **二进制改名 `ssh-manager` → `sshmgr`**(好打字)。

附带收益:update 的 `--file` 本地包模式还清"exe 分发通道债"(MCP upload 1MiB cap 装不下二进制,MCP 通道永远传不了 exe)。

## 2. 定案总览(grilling Q1-Q9 + 盲评拍板)

| # | 决策 | 定案 |
|---|---|---|
| Q1 | 更新源 | GitHub Releases 直连为主 + `--file <本地包>` 兜底 |
| Q2 | 信任链 | 强制 https(仅环回字面量 `{127.0.0.1, ::1}` 例外,`url.Parse→Hostname()` 精确匹配)+ 重定向**每一跳**宿主白名单(4 宿主;自定义 base 时资产 URL 以 base 源重建)+ 同 release checksums.txt SHA256;不做签名;`SSHMGR_UPDATE_BASE` env seam;零新依赖 |
| Q3 | 替换与重启 | Windows rename-to-`.old`(代际名)/ 其余原子 rename;staged 文件 fsync 前置;崩溃窗口自愈(交互确认,`--yes` 不豁免);服务路径**替换前预检**(读不到即中止);文件替换免提权(NUC10 用户目录),服务重启经 kardianos `Restart()`(NUC10 需提升;退出码区分"重启待手工"),失败打印手工命令;重启后健康回探;client 零中断 |
| Q4 | 命令面 | `update` / `--check` / `--yes` / `--version <tag>`(降级=回滚通道,显式警告)/ `--file`+`--sha256`/`--no-verify`;**无后台自动检查**;非 TTY 无 `--yes` 报错退出 |
| Q5 | 改名 | 一刀切(cmd 目录/goreleaser/root Use/服务名/docs/.mcp.json 示例/TUI goldens/**CI workflows**/**进程探测名**/**internal 用户可见文案全量**);无别名;module path 不动;单一 v0.13 runbook 迁移;不做 v0.12.1 backport |
| Q6 | 版本载体 | 捆进 v0.13.0(与批1 同一次 breaking,一次迁移) |
| Q7 | 执行流 | spec → 2 轮盲评 → plan → SDD |
| Q8 | 排序 | Plan 44 先于批2(Web UI→v0.14.0) |
| Q9 | 验收分工 | SAS 肉眼比对 owner;机械验收我经 MCP 代跑附证据 |
| R1 | (盲评 C1 拍板) | LocalSystem 服务 exe 在用户可写目录=same-user→SYSTEM 提权面:**维持既有单用户姿态**(master.key 机器域可解,增量小;update 免提权是本 plan 核心卖点),threat-model 诚实登记 + 文档给可选加固指引(迁 Program Files) |

## 3. 改名 sweep(ssh-manager → sshmgr)

### 3.1 范围(一刀切,不留别名)

| 项 | 旧 | 新 |
|---|---|---|
| 构建入口目录 | `cmd/ssh-manager/` | `cmd/sshmgr/` |
| goreleaser `project_name` / build id / `binary` | ssh-manager | sshmgr(资产名随之变 `sshmgr_{ver}_{os}_{arch}.{zip\|tar.gz}`;release 名 `sshmgr {{ .Tag }}`) |
| CLI root `Use` | `ssh-manager` | `sshmgr` |
| 服务名 `serveServiceName` | `ssh-manager-serve` | `sshmgr-serve`(`serveDisplayName` 同步) |
| **serve 进程探测名** | `serve_service_process_windows.go` 匹配 `ssh-manager.exe`;POSIX comm `ssh-manager` | `sshmgr.exe` / `sshmgr`(否则改名后 `serve status` 把运行中的新进程误报 not running) |
| **CI workflows** | `serve-install.yml` 硬编码 `go build -o ssh-manager ./cmd/ssh-manager`、`./ssh-manager[.exe] serve uninstall`、`schtasks /Delete /TN ssh-manager-serve` | 全部同步新名;`goreleaser-check.yml` 的 `cmd/**` 触发路径在新目录下仍命中(确认项) |
| **internal 用户可见文案** | ~110 处/35 文件(internal/cli、internal/mcpserver、internal/clientops、internal/roles 的 help/error/指引字符串) | 全量替换(`ssh-manager <子命令>` → `sshmgr <子命令>`);grep 清单为准,doctor.go/serve_service.go/serve.go/pairserve.go 等 |
| **Go 构建/测试引用** | `internal/eval/broker.go:76`、`internal/conformance/tunnel_kill_test.go:118`、`internal/cli/serve_install_integration_test.go:368` 等构建/引用 `cmd/ssh-manager` 的 Go 代码 | `rg "cmd/ssh-manager|ssh-manager(\\.exe)?"` 清单为准全量替换;三平台 `go build ./...`+`go test` 验证 |
| 文档全部二进制调用处 / `.mcp.json` 示例 command / TUI 文案与 goldens / pair 产物内命令路径 | ssh-manager(.exe) | sshmgr(.exe) |
| Go module path | `ssh-manager-mcp` | **不动**(改=全仓 import 大迁移,零用户价值) |

`.mcp.json` 里 MCP server 名(如 `"ssh"`)与本项目名不变;只变二进制路径/命令名。

### 3.2 一次性迁移 = 单一 v0.13.0 runbook(嵌入批1 铁律)

**顺序不可乱:先迁 client 后升 serve**(批1 移除 ②a——serve 先升会断掉旧 HTTP MCP 客户端):

```
# ① ②a 存量桥迁(旧 serve 还在跑时完成;= 批1 G2):
#    各 client 机 agent 从 ②a HTTP 直连姿态迁到 stdio 桥(--cache)
# ② client 机改名(笔记本):
#    v0.13.0 资产解压,sshmgr.exe 替换旧 ssh-manager.exe(旧的最后删/改名)
#    .mcp.json 的 command 路径同步改指 sshmgr.exe
# ③ serve 机迁移(最后;NUC10,管理员 shell):
#    读旧服务参数(Windows: sc qc ssh-manager-serve 记下 --addr/--tls-cert/--tls-key)
#    旧 binary: ssh-manager serve uninstall
#    curl -LO <v0.13.0 资产> + curl -LO checksums.txt,SHA256 核验(certutil/sha256sum),解压到位
#    sshmgr serve install <照旧参数(--addr 0.0.0.0:7878 及 TLS flags 若有)>
# ④ 之后:sshmgr update 一条命令自续
```

**旧服务检测(三分法)**:update **无条件**探测新旧两名(kardianos `service.New`→`Status()`),结果分三类处理——
- **已装(任何态:Running/Stopped/failed/Unknown)**:按已装继续(failed 态恰是崩溃循环、最需要 update 的场景,不得封死;Linux systemd 对 failed 态 `Status()` 返回错误,kardianos `service_systemd_linux.go:286`——须按"已装"分类而非机制错误)
- **未装**(`ErrNotInstalled`):放行
- **探测机制错误**(无法判定存在性):fail-closed 中止

旧名存在(任何态,无论新名状态)→ 打印迁移块并中止(不半更新,防新旧服务并存);`ErrNoServiceSystemDetected`(容器/CI 无服务管理器)→ 跳过检测直接更新。

## 4. `sshmgr update` 命令设计

### 4.1 版本发现(GitHub 直连)

- `GET {SSHMGR_UPDATE_BASE}/repos/allanpk716/ssh-manager-mcp/releases/latest`(默认 base `https://api.github.com`;该端点天然排除 prerelease/draft)
- `--version <tag>`(如 `v0.13.0`):改走 `GET .../releases/tags/{tag}`(可取 prerelease),用于降级/钉版
- 解析 `tag_name` + `assets[].name/browser_download_url`;目标资产名本地计算:`sshmgr_{ver}_{GOOS}_{GOARCH}.{zip|tar.gz}`,ver=tag 去掉 `v` 前缀,windows→zip 其余→tar.gz;本地 GOOS/GOARCH 不在发布矩阵(windows/linux/darwin × amd64/arm64)→ 明确报错并列出支持矩阵;**tag 拼入 URL 前先过格式校验 `^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$` 再 `url.PathEscape`**
- 版本比较:手写三段数字比较(`vMAJOR.MINOR.PATCH[-pre]`;**pre 按 SemVer 标识符段规则比较**——数字段数值比、字母段字典序、数字段<字母段、段多者大,支持同三元不同 pre 间钉版/回滚)——不引 semver 依赖;`buildinfo` 增 `Owner="allanpk716"`/`Repo="ssh-manager-mcp"` 常量;**当前版本解析失败(含本地构建默认 `dev`)→ 拒绝自动比较,报错要求 `--version` 显式指定;`dev` + `--version` → 打印"无法判定升降级"通用警告(不触发降级专用文案)**
- 未认证 API 限速 60/h/IP——手动更新场景足够,文档注明
- 超时:API 30s;下载流式落盘,**idle 60s 零字节或总超时 10 分钟即中止**(实现手段:包装 io.Reader 计时或 `net.Dialer`+`SetReadDeadline`;stdlib 无一键逐读超时,计划须体现),上限 200MiB 防异常资产

### 4.2 下载与校验

1. 下载 checksums.txt 资产 → 解析(多行 `sha256  文件名`,容 CRLF)→ 取目标资产行(缺行拒绝)
2. 下载目标资产 → SHA256 全量比对,不符即中止(**目标文件零触碰**)
3. **解压策略(钉死,zip slip 不可能)**:流式遍历归档,**只落地名称精确等于 `sshmgr`(windows 为 `sshmgr.exe`)的根条目**,其余条目一律不写盘;拒绝绝对路径/`..`/子目录/重复名/symlink/hardlink/device/非 regular 文件;单成员与总解压字节上限;以 `O_EXCL` 创建写入临时目录,**落盘后立即 `chmod 0755`**(解压默认 0666&^umask 不可执行——staged 自检在 chmod 之后才有意义)
4. 传输安全(判定程序钉死:`url.Parse` → `u.Hostname()`,host 精确匹配,大小写归一;**拒绝** `127.1`/`0x7f.0.0.1`/trailing dot/IPv6 zone/等变体——`Hostname()` 天然剥 userinfo,测试矩阵钉死):**https 强制,唯一例外=该跳 host ∈ 环回字面量 `{127.0.0.1, ::1}`**(测试 seam;localhost **不是**字面量——名字需解析,不入集);`http.Client` 的 `CheckRedirect` 对**初始 URL 与每一跳**校验 scheme+exact host(不只最终宿主):默认白名单 `{api.github.com, github.com, objects.githubusercontent.com, release-assets.githubusercontent.com}`(**实测本仓资产 302 宿主为 release-assets**;GitHub 若再增 `*.githubusercontent.com` 子域属维护点);**`SSHMGR_UPDATE_BASE` 显式设定时**:API 与资产 URL 一律**以 base 为源重建**(取 URL 的 path 查询串、换 host=scheme=base 的;镜像必须照搬 GitHub 的 URL 路径结构,否则镜像用途不成立),白名单换为 base 宿主(仍强制 https,环回除外)
5. `--file <path> [--sha256 <hex> | --no-verify]`:本地包模式,跳过发现/下载;格式按扩展名识别(.zip/.tar.gz/.tgz);包内无精确名根条目→明确报错;`--sha256` 给了则核对,没给必须显式 `--no-verify`(打印未校验警告);`--sha256` 与 `--no-verify` 互斥(同给报错)——owner 手供文件,信任=你下载的它

### 4.3 事务性替换(核心)

统一策略:**临时目录建在 exe 同目录**(同卷,保证 rename 原子):

```
self = EvalSymlinks(os.Executable())
tmpdir = self 同目录/.sshmgr-update-tmp-XXXX(下载/解压/校验全在 tmpdir,落地即 chmod 0755)
校验通过后:
  staged 自检:exec.CommandContext 超时 10s、输出经有限缓冲,执行 staged 二进制 `version`;
    规范化契约 = TrimSpace + 去前缀 v 后与目标版本(去 v)相等
    目标版本:GitHub/--version 路径 = tag;--file 路径 = staged 输出本身即目标版本
      (后续"已装该版本"拒绝与降级警告在 --file 路径均以它为基准)
    失败(超时/输出不符/无法执行)→ 保留原文件,清 staged,中止
  fsync:staged 文件 fsync 成功才算可替换(rename 前,双平台一致)
  GOOS=windows:
    清残留备份 self+".old*"(尽力,失败仅警告;仍被旧进程持有的删不掉,正常)
    os.Rename(self, self+".old.<unixts>")        # 运行中 exe 可改名不可删;代际名防单槽被旧进程占死
    os.Rename(tmp/self, self) 失败 → 回滚 Rename(最新代际 .old→self)
      回滚再失败 → 专用退出码 + 打印手工恢复命令(ren <最新代际>.old sshmgr.exe)+ 跳过重启询问
  其余(Linux/darwin):
    os.Rename(tmp/self, self)                     # 原子,运行中进程持旧 inode
    rename 后 fsync exe 目录;失败 = committed-with-error 状态(不回滚,如实报告)
  Windows 目录持久化:不做目录 sync(Go 只读目录句柄 FlushFileBuffers 必 Access denied),
    诚实文档化崩溃窗口——staged 文件已 fsync,NTFS 元数据日志保证掉电后旧条目或新条目二选一,均为完整二进制
启动自愈(入口可达):update 起手检测——
  self 缺失但 self+".old.<unixts>" 存在 → 提示恢复(两 rename 之间崩溃的窗口)
  执行路径自身带 .old 代际后缀且 canonical 缺失 → 同样触发(此时正在跑的就是备份,renam 回 canonical 即恢复)
  自愈确认 = 交互式 only,--yes 不豁免(非 TTY+--yes 遇自愈 → 报错退出);
  不做 .old sidecar 哈希绑定:能写 .old 的同用户攻击者可直接换 exe(C1 已拍板接受该面),sidecar 无增量防御
```

- 校验/staged 失败在 tmpdir 内中止,写入失败回滚 rename——**不变式:替换点之前的失败=零变更**;rename 之后的失败(目录 fsync)=已提交+如实报告(committed-with-error),不假装零变更
- 正在运行的服务 / `mcp --cache` 桥继续持旧镜像直到下次 spawn;新进程即新版本
- exe 目录不可写(如 /usr/local/bin)→ 明确报错提示 sudo/管理员;**不自动提权**
- 并发两个 update:不设锁,文档注明"不要并发跑"(单人工具 YAGNI)

### 4.4 服务重启

- **路径预检(下载/替换之前,不是替换后)**:新名服务 `sshmgr-serve` 已注册时,读回服务注册二进制路径并与 self 比对——
  - 读回机制(钉死):Windows = `x/sys/windows` `QueryServiceConfig`(go.mod 直接依赖,零新增;**不解析 `sc qc` 文本**——本地化输出会翻车,FINDING E 教训);Linux = 读 kardianos 硬编码的 `/etc/systemd/system/<name>.service` 的 `ExecStart`;darwin = 读 launchd plist;其余 backend → 不支持即中止
  - 比对口径:两侧 `filepath.Abs`+`EvalSymlinks`,Windows 用 `EqualFold`(大小写/解析差异不误伤)
  - **读不到(新名已装但读回失败)→ 中止**(fail-closed;校验最该救场的未知环境里不能 warning 继续)
  - 读到了但不一致 → **中止并打印两端路径**(防"更新 A 路径、服务跑 B 路径"静默旧版)
- 替换成功后询问"重启服务立即生效?[Y/n]"(`--yes` 跳过询问=同意);执行 `s.Restart()`
  - **NUC10 常态声明**:服务跑 LocalSystem,非提升会话 `Restart()` 必 Access denied——正常路径,不算更新失败
- `Restart()` 失败 → 打印平台手工命令 + **专用退出码"替换成功/重启待手工"**(与更新失败区分;脚本可据此分支):
  - Windows:`sc stop sshmgr-serve && sc start sshmgr-serve`(管理员;或 Win11 `sudo sc ...`)
  - Linux:`sudo systemctl restart sshmgr-serve`
- `Restart()` 成功 → 复用仓内 `probeServeHTTP`(serve_service.go)对绑定地址健康回探,证据行输出服务已回活
- 重启确认前打印警告:**"重启将断开活动隧道;进行中的配对请求作废(密钥态在服务内存,既有'重启作废'语义)"**
- 服务已装但 Stopped:只打印启动命令
- 未装服务(client 姿态):打印"新版本下次 agent 会话生效;运行中的桥继续旧版"
- **非 TTY(stdin 非 tty)且无 `--yes` → 报错退出要求 `--yes`**——作用域=**仅在即将发起确认的节点触发**;`--check` 干跑与"已是最新"路径永不触发(脚本/cron 可安全使用)
- **降级**(`--version` 目标 < 当前;`--file` 路径以 staged 输出为基准):即使 `--yes` 也打印醒目行"降级至 vX(回滚通道)";无 `--yes` 时确认文案明示这是降级;dev+`--version` 无法判定升降级 → 通用警告(§4.1)

### 4.5 命令面

```
sshmgr update                        # 检查→显示 当前→最新→确认→下载→校验→staged自检→替换→(服务则重启)
sshmgr update --check                # 干跑:只报 当前/最新/资产名/update base,不改任何东西
sshmgr update --yes                  # 免确认(远程/脚本;非 TTY 必需;服务重启亦视为同意)
sshmgr update --version v0.13.0      # 装指定版(含降级=回滚;校验该版 checksums;降级有显式警告)
sshmgr update --file <包> [--sha256 <hex> | --no-verify]
```

- 已是最新 → 打印后 exit 0;`--version` 目标=当前版 → 拒绝("已安装该版本";**`--file` 路径以 staged 输出为目标版本**);退出码三分:**0=完成 / 专用码"替换成功+重启待手工" / 非零=失败**
- 输出证据行:版本对、资产名、SHA256 命中、**update base(非默认时额外醒目)**、替换路径、staged 自检结果、重启结果(+健康回探行)——供真机验收留档与真假源区分

### 4.6 env seam(生产路径必须留缝,SSHMGR_CACHE_DEK 教训)

- `SSHMGR_UPDATE_BASE`:默认 `https://api.github.com`;显式设定时 **API 与资产 URL 以 base 为源重建**(§4.2(4)),白名单换为 base 宿主;**仅环回字面量 `{127.0.0.1, ::1}` 允许 http**(测试假源,逐跳判定),其余强制 https——一条规则消除"生产镜像 vs 测试 http"的矛盾
- 代码内可注入 seam:版本发现/下载经 package 级函数变量(单测 httptest 替身);服务检测经 `service.New` 函数变量(测旧名/双服务/failed 态/无管理器分支);staged 自检经 exec 函数变量(测超时/假输出)

## 5. 安全声明与 threat-model 增补

- **自更新 = RCE-by-design**:信任链止于 GitHub TLS + 同 release checksums(防传输损坏+绑定资产);**不是供应链硬防**——仓库/账号被攻破则更新器同陷。R13 子风险诚实列全:
  - 仓库/账号被攻破(update 通道即武器)
  - **本地环境注入**:`SSHMGR_UPDATE_BASE` 被继承 env 重定向=信任根迁移——缓解:非环回强制 https+白名单换 base 宿主+证据行显示生效 base
  - `--no-verify`:owner 显式声明信任本地文件,打印警告留档
  - **staged 自检 = 以当前用户执行目标二进制**(已过 checksum,但 checksum 信任链=GitHub;`--file --no-verify` 下更是零校验执行——比"只落盘不执行"多一个执行面;缓解=超时 10s+有限输出,执行后即删)
  - **服务 exe 用户可写目录(NUC10 现状)= same-user→SYSTEM 提权面**:**owner 拍板维持**(v0.10.0 既有姿态;单用户威胁模型 M1 下 master.key 为机器域可解,same-user 妥协≈全丢,SYSTEM 增量小;update 免提权是核心易用性诉求);threat-model 登记残余 + 文档给可选加固指引(迁 `C:\Program Files\sshmgr\` + ACL;加固后 update 文件替换亦需提权)
- 不做签名(cosign/minisign):新依赖+密钥管理,当前威胁模型不背书;将来要加=独立决策
- 权限面:update 对**文件**只需 exe 目录写权;对**服务**重启需提权,失败路径只打印命令由 owner 执行——update 自身永不自动提权
- vault 数据零触碰(master.key/store.db 不读不写)
- 降级无防(有意:`--version` 即回滚通道,但有显式警告);无自动检查(升级次序铁律:先迁 client 后升 serve,owner 手动拍板)

## 6. 测试策略

- **单测**(零新依赖,httptest+注入 seam):
  - 版本比较三段式表测(含 **pre 标识符段规则**、同三元不同 pre 钉版、非法输入拒绝、**`dev` 拒绝自动比较**、dev+`--version` 通用警告)
  - 资产名计算(GOOS/GOARCH 矩阵、v 前缀剥离;**矩阵外平台报错分支**);**tag URL 构造前格式校验+PathEscape**
  - **`version` 输出契约单测**(仅版本值+换行,不做其他输出——staged 比对依赖此契约)
  - checksums.txt 解析(多行/CRLF/缺行拒绝)
  - **解压安全矩阵**:`../evil`、绝对路径、子目录同名条目、symlink/hardlink、重复名、非 regular——一律拒绝且零落地;正常黄金夹具(zip/tar.gz)通过;**落地即 chmod 0755 断言**
  - **事务性**(临时副本+rename 失败注入,单测可跑,不限平台):校验失败→目标字节不变;写入失败→回滚→目标字节不变;**staged version 自检失败(假输出/超时[注入 exec seam])→拒绝**;**--file 路径 staged 输出=目标版本断言**;**回滚 double-fault→专用退出码+恢复指引文案**;**启动自愈分支**(self 缺失+代际 .old 存在→恢复;**非 TTY+--yes 遇自愈→报错退出**;执行路径带 .old 后缀+canonical 缺失→触发);**代际 .old:两代备份并存,回滚取最新**
  - 传输(判定程序=url.Parse→Hostname()):非环回 http 拒绝;**环回 {127.0.0.1,::1} http 放行**;**环回变体拒绝矩阵**(127.1、0x7f.0.0.1、localhost、localhost.、[::1]%zone、127.0.0.1:80@evil、trailing dot);**每一跳白名单**(经 redirect 链造非法中间跳→拒);**自定义 base:资产 URL 以 base 源重建断言**+越界宿主拒;**逐跳环回判定**(base 环回但重定向跳非环回→该跳必须 https)
  - 服务检测(seam 替身):旧名存在→迁移块+中止;**双服务并存→中止**;**failed 态→按已装放行(不封死崩溃循环机的更新)**;**探测机制错误→fail-closed**;无服务管理器→跳过;**路径预检(替换前)**:读不到→中止;不一致→中止打印两端;**比较规范化**(大小写/符号链接口径)
  - **非 TTY 且无 --yes → 报错退出;--check/已是最新路径不触发**
  - `--file`:--sha256 命中/不符;无 --sha256 且无 --no-verify 拒绝;**--sha256 与 --no-verify 同给报错**;格式按扩展名;无精确名根条目报错
  - **降级警告行**(目标<当前,--yes 亦有)
  - **fsync 顺序**:staged 文件 fsync 失败→不进入 rename(注入 seam)
- **集成**:本地假源(httptest + SSHMGR_UPDATE_BASE 环回)全回路——伪造 v0.0.1-test → v0.0.2-test,对 HOME 下**二进制副本**执行完整 update,断言版本翻转+`.old` 清理
- **真机 gate**(§7;单测不覆盖"运行中 exe 锁定语义"——Windows rename 运行中 exe 只能真机验)

## 7. 真机验收 gate(与批1 G1-G8 合并为 v0.13.0 一份清单)

| # | 项 | 执行者 |
|---|---|---|
| G1-G8 | 批1 原有(NUC10 升级/笔记本 ②a→桥迁移/干净目录 pair/三件套比对/在线离线/审计) | owner(SAS 肉眼比对必须人) |
| G9 | NUC10 迁移(§3.2 runbook ③ 完整流:sc qc 读参数→uninstall→**curl 资产+checksums 核验**→解压→install→RUNNING) | 我经 MCP 代跑 |
| G10 | update 回路:NUC10 上 `SSHMGR_UPDATE_BASE` 指环回假源(构造 v0.13.0→v0.13.1-test)`sshmgr update --yes`(非 TTY 规则命中)**提升会话跑**(非提升 Restart 必败为 NUC10 常态)全链,断言替换+服务重启+**probeServeHTTP 健康回探行**+版本翻转+证据行含 base,再指回真源自愈 | 我经 MCP 代跑 |
| G11 | `.old` 残留清理 + 笔记本 client `sshmgr update`;**断言 agent 实际 spawn 的 `mcp --cache` 报新版本**(.mcp.json 指旧 exe 的失败模式);旧 `ssh-manager.exe` 最后删 | 我 + 本机 |
| G12 | 旧服务名场景矩阵:旧名注册→迁移块+中止;**双服务并存→中止**;**failed 态(构造服务崩溃)→按已装放行,更新通道不被封死**;**探测机制错误→fail-closed**(构造) | 我经 MCP 代跑 |

证据:update 输出留档(版本对/SHA256 命中/base/重启结果)随验收报告入 repo。

## 8. 文档与迁移

- **§3.2 runbook = 唯一 v0.13.0 迁移总册**(含批1 ②a 桥迁顺序,先 client 后 serve),置顶 deployment-modes.md
- 改名 sweep 文档面:**以 `grep -r "ssh-manager" docs/ README.md .github/` 全量清单为准**,至少覆盖:agent-access.md、agent-tools.md、concepts.md、backup-restore.md、getting-started.md、scenarios.md、managing-servers.md、tui-single-machine.md、tui-multi-machine.md、quickstart-single-machine.md、multi-machine.md、broker-host-agent.md、deployment-modes.md、README.md+docs/README.md(即"README×2")
- `compat-matrix.md`:v0.13.0 行 breaking 面**列全四项**:②a 移除(批1)、二进制改名、**服务名变更(ssh-manager-serve→sshmgr-serve)**、**.mcp.json command/path 迁移**;版本号回写
- `threat-model.md`:R13 自更新供应链(含 §5 子风险五条)+ §6 更新面收窄声明
- **资产名防漂移=发布 checklist 项**:改 `.goreleaser.yml` 的 `name_template`/`checksum` 名必须同步 update 的资产名计算(模板一改 update 全量 404)
- TUI goldens(`mcpConfigLines` 等)与 pair 产物(`pair.<name>.mcp.json` 的 command)同步新名——既有 goldens 断言更新为计划内变更

## 9. 交付与版本

- **v0.13.0 = 批1(模式缩减+发现+SAS 配对)+ Plan 44(自更新+改名)**,一次 breaking 一次迁移
- v0.14.0 = 批2(Web UI);其 rollout 直接受益于 update 已就位
- Plan 44 spec → 2 轮盲评 → plan → SDD(体量:估 7-9 任务)

## 10. 登记不实施(deferred)

- 并发 update 文件锁(文档"不要并发跑"代替)
- 下载断点续传/进度条(资产 <30MB)
- 后台自动检查/自动更新(明确拒绝——升级次序铁律)
- v0.12.1 backport 桥(明确拒绝——2 台机器一次迁移不值得)
- `sshmgr version --check` 联动(`update --check` 已覆盖)
- goreleaser 资产签名(见 §5,将来独立决策)
- 服务 exe 迁移 Program Files 加固(拍板:可选指引不做默认;threat-model 登记残余)
