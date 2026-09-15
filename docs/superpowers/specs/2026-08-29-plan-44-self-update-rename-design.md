# Plan 44 设计:sshmgr 自更新 + 改名 ssh-manager → sshmgr

- 日期:2026-08-29
- 状态:rev0(grilling Q1-Q9 全拍板,待盲评)
- 前置:Plan 42 批1 已并入本地 master(5dd51e4,未推送未发版);本 plan 与批1 **捆发 v0.13.0**(Q6 拍板)
- 上游事实(2026-08-29 实测):NUC10 exe 在 `C:\Users\allan716\ssh-manager.exe`(用户目录,**替换文件无需提权**);服务 `ssh-manager-serve` RUNNING(BINARY_PATH_NAME 指向该 exe);NUC10 → GitHub API 可达(200);发版管线 = goreleaser → GitHub Releases(资产 zip/tar.gz + checksums.txt)

## 1. 背景与诉求

现状更新 = 手工三步(停服务 → 换二进制 → 重启服务),v0.10.0 双端部署即此。诉求:

1. **serve 运行中也能一条命令更新**(`sshmgr update`),不退出不手工换文件;client 端(笔记本)同样。
2. **二进制改名 `ssh-manager` → `sshmgr`**(好打字)。

附带收益:update 的 `--file` 本地包模式还清"exe 分发通道债"(MCP upload 1MiB cap 装不下二进制,MCP 通道永远传不了 exe)。

## 2. 定案总览(grilling Q1-Q9)

| # | 决策 | 定案 |
|---|---|---|
| Q1 | 更新源 | GitHub Releases 直连为主 + `--file <本地包>` 兜底 |
| Q2 | 信任链 | 强制 https + 重定向宿主白名单 + 同 release checksums.txt SHA256;不做签名;`SSHMGR_UPDATE_BASE` env seam;零新依赖 |
| Q3 | 替换与重启 | Windows rename-to-`.old` 技巧 / Linux 原子 rename;文件替换免提权(NUC10 用户目录),服务重启经 kardianos `Restart()`,失败打印手工命令;重启前警告(隧道断开+配对作废);client 零中断 |
| Q4 | 命令面 | `update` / `--check` / `--yes` / `--version <tag>`(含降级=回滚通道)/ `--file`+`--sha256`;**无后台自动检查**(升级次序铁律 owner 手动) |
| Q5 | 改名 | 一刀切(cmd 目录/goreleaser/root Use/服务名/文档/.mcp.json 示例/TUI 文案);无别名;module path 不动;一次性迁移(旧服务 uninstall→新 install);**不做 v0.12.1 backport 桥** |
| Q6 | 版本载体 | 捆进 v0.13.0(与批1 同一次 breaking,一次迁移) |
| Q7 | 执行流 | spec → 2 轮盲评 → plan → SDD |
| Q8 | 排序 | Plan 44 先于批2(Web UI→v0.14.0) |
| Q9 | 验收分工 | SAS 肉眼比对 owner;机械验收(update 回路等)我经 MCP 代跑附证据 |

## 3. 改名 sweep(ssh-manager → sshmgr)

### 3.1 范围(一刀切,不留别名)

| 项 | 旧 | 新 |
|---|---|---|
| 构建入口目录 | `cmd/ssh-manager/` | `cmd/sshmgr/` |
| goreleaser `project_name` / build id / `binary` | ssh-manager | sshmgr(资产名随之变 `sshmgr_{ver}_{os}_{arch}.{zip\|tar.gz}`;release 名 `sshmgr {{ .Tag }}`) |
| CLI root `Use` | `ssh-manager` | `sshmgr` |
| 服务名 `serveServiceName` | `ssh-manager-serve` | `sshmgr-serve`(`serveDisplayName` 同步) |
| 文档全部二进制调用处 / `.mcp.json` 示例 command / TUI 文案与 goldens / pair 产物内命令路径 | ssh-manager(.exe) | sshmgr(.exe) |
| Go module path | `ssh-manager-mcp` | **不动**(改=全仓 import 大迁移,零用户价值) |

`.mcp.json` 里 MCP server 名(如 `"ssh"`)与本项目名不变;只变二进制路径/命令名。

### 3.2 一次性迁移(每台机器,≤v0.12.0 → v0.13.0)

```
# 有 serve 服务的机器(NUC10):
ssh-manager serve uninstall                 # 旧 binary 卸旧服务
# 下载 v0.13.0 资产解压,sshmgr.exe 放到位(替换旧 ssh-manager.exe,旧的删除)
sshmgr serve install --addr 0.0.0.0:7878    # 新 binary 注册新服务(参数照旧)
# 无服务的机器(笔记本):替换 exe 即可;注意 .mcp.json 的 command 路径同步改
# 之后:sshmgr update 一条命令自续
```

`update` 检测到旧名服务 `ssh-manager-serve` 仍注册时:**打印上述迁移块并中止**(不半更新;避免新旧服务名并存)。检测方式:kardianos `service.New` 以新名探 `Status()` 为 NotInstalled 时,再以旧名探一次。

## 4. `sshmgr update` 命令设计

### 4.1 版本发现(GitHub 直连)

- `GET {SSHMGR_UPDATE_BASE}/repos/allanpk716/ssh-manager-mcp/releases/latest`(默认 base `https://api.github.com`;该端点天然排除 prerelease/draft)
- `--version <tag>`(如 `v0.13.0`):改走 `GET .../releases/tags/{tag}`(可取 prerelease),用于降级/钉版
- 解析 `tag_name` + `assets[].name/browser_download_url`;目标资产名本地计算:`sshmgr_{ver}_{GOOS}_{GOARCH}.{zip|tar.gz}`,ver=tag 去掉 `v` 前缀,windows→zip 其余→tar.gz
- 版本比较:手写三段数字比较(`vMAJOR.MINOR.PATCH[-pre]`,pre 使同号偏旧)——不引 semver 依赖;`buildinfo` 增 `Owner="allanpk716"`/`Repo="ssh-manager-mcp"` 常量
- 未认证 API 限速 60/h/IP——手动更新场景足够,文档注明
- 超时:API 30s;下载流式落盘,上限 200MiB 防异常资产

### 4.2 下载与校验

1. 下载 checksums.txt 资产 → 解析(多行 `sha256  文件名`,容 CRLF)→ 取目标资产行
2. 下载目标资产 → SHA256 全量比对,不符即中止(**目标文件零触碰**)
3. 解压(zip / tar.gz)取根下 `sshmgr`(windows 为 `sshmgr.exe`)
4. 传输安全:强制 https;`http.Client` 跟随重定向时校验**最终宿主白名单** `{api.github.com, github.com, objects.githubusercontent.com}`(GitHub 资产 302 → objects.githubusercontent.com);**`SSHMGR_UPDATE_BASE` 显式设定时**:仍强制 https,白名单换为 base 宿主(operator 显式信任=测试 seam/镜像两用)
5. `--file <path> [--sha256 <hex>]`:本地包模式,跳过发现/下载;给了 `--sha256` 则核对,没给则必须显式 `--no-verify`(打印未校验警告)——owner 手供文件,信任=你下载的它

### 4.3 事务性替换(核心)

统一策略:**临时目录建在 exe 同目录**(同卷,保证 rename 原子):

```
self = EvalSymlinks(os.Executable())
tmpdir = self 同目录/.sshmgr-update-tmp-XXXX(下载/解压/校验全在 tmpdir)
校验通过后:
  Linux:   chmod 0755 → os.Rename(tmp/self, self)          # 原子,运行中进程持旧 inode
  Windows: 先清残留 self+".old"(尽力,失败仅警告)
           os.Rename(self, self+".old")                     # 运行中 exe 可改名不可删
           os.Rename(tmp/self, self)                        # 失败则回滚:Rename(.old→self),中止
           残留 .old 留给下次 update 起手清理(仍被旧进程持有时删不掉)
```

- 任一步失败,目标路径字节不变(校验失败在 tmpdir 内中止;写入失败回滚 rename)——**不变式:失败=零变更**
- 正在运行的服务 / `mcp --cache` 桥继续持旧镜像直到下次 spawn;新进程即新版本
- exe 目录不可写(如 /usr/local/bin)→ 明确报错提示 sudo/管理员;**不自动提权**
- 并发两个 update:不设锁,文档注明"不要并发跑"(单人工具 YAGNI)

### 4.4 服务重启

- 新名服务 `sshmgr-serve` 已注册(kardianos `Status()`):替换成功后询问"重启服务立即生效?[Y/n]"(`--yes` 跳过询问=同意);执行 `s.Restart()`
- `Restart()` 失败(典型:非提升会话操作 SCM/systemctl)→ 打印平台手工命令退出码非零:
  - Windows:`sc stop sshmgr-serve && sc start sshmgr-serve`(管理员;或 Win11 `sudo sc ...`)
  - Linux:`sudo systemctl restart sshmgr-serve`
- 重启确认前打印警告:**"重启将断开活动隧道;进行中的配对请求作废(密钥态在服务内存,既有'重启作废'语义)"**
- 服务已装但 Stopped:只打印启动命令
- 未装服务(client 姿态):打印"新版本下次 agent 会话生效;运行中的桥继续旧版"

### 4.5 命令面

```
sshmgr update                        # 检查→显示 当前→最新→确认→下载→校验→替换→(服务则重启)
sshmgr update --check                # 干跑:只报 当前/最新(与资产名),不改任何东西
sshmgr update --yes                  # 免确认(远程/脚本;服务重启也视为同意)
sshmgr update --version v0.13.0      # 装指定版(含降级=回滚;校验该版 checksums)
sshmgr update --file <包> [--sha256 <hex> | --no-verify]
```

- 已是最新 → 打印后 exit 0;`--version` 目标=当前版 → 拒绝("已安装该版本")
- 输出证据行:版本对、资产名、SHA256 命中、替换路径、重启结果——供真机验收留档

### 4.6 env seam(生产路径必须留缝,SSHMGR_CACHE_DEK 教训)

- `SSHMGR_UPDATE_BASE`:默认 `https://api.github.com`;设为本地假源(如 `http://127.0.0.1:PORT`——**测试场景允许 http**,仅显式设定时)即可整链路测试/镜像加速
- 代码内可注入 seam:版本发现/下载经 package 级函数变量(单测 httptest 替身);服务检测经 `service.New` 函数变量(测"检测到旧服务名→中止"分支)

## 5. 安全声明与 threat-model 增补

- **自更新 = RCE-by-design**:信任链止于 GitHub TLS + 同 release checksums(防传输损坏+绑定资产);**不是供应链硬防**——仓库/账号被攻破则更新器同陷。诚实入 threat-model 新风险条(R13 自更新供应链),不粉饰
- 不做签名(cosign/minisign):新依赖+密钥管理,当前威胁模型(个人工具+公开仓库+TLS 通道)不背书;将来要加=K_master 之外的独立决策
- 权限面:update 对**文件**只需 exe 目录写权(NUC10=用户目录,免提权);对**服务**重启需提权,且失败路径只打印命令由 owner 执行——update 自身永不自动提权
- vault 数据零触碰(master.key/store.db 不读不写)
- 降级无防(有意:`--version` 即回滚通道);无自动检查(升级次序铁律:先迁 client 后升 serve,owner 手动拍板)

## 6. 测试策略

- **单测**(零新依赖,httptest+注入 seam):
  - 版本比较三段式表测(含 pre 后缀、非法输入拒绝)
  - 资产名计算(GOOS/GOARCH 矩阵、v 前缀剥离)
  - checksums.txt 解析(多行/CRLF/缺行拒绝)
  - zip/tar.gz 解压取根二进制(黄金小夹具)
  - **事务性**:临时副本上跑替换——校验失败后目标字节不变;写入失败回滚 rename(Linux 语义;Windows 运行中 exe rename 仅真机验)
  - http 拒绝;重定向宿主白名单外拒绝;`SSHMGR_UPDATE_BASE` 自定义宿主放行
  - 旧服务名检测 → 打印迁移块并中止(service seam 替身)
  - `--file`:--sha256 命中/不符;无 --sha256 且无 --no-verify 拒绝
- **集成**:本地假源(httptest + SSHMGR_UPDATE_BASE)全回路——伪造 v0.0.1-test → v0.0.2-test,对 HOME 下**二进制副本**执行完整 update,断言版本翻转+`.old` 清理
- **真机 gate**(§7)

## 7. 真机验收 gate(与批1 G1-G8 合并为 v0.13.0 一份清单)

| # | 项 | 执行者 |
|---|---|---|
| G1-G8 | 批1 原有(NUC10 升级/笔记本迁移/干净目录 pair/三件套比对/在线离线/审计) | owner(SAS 肉眼比对必须人) |
| G9 | NUC10 迁移:旧 binary `serve uninstall` → curl -LO v0.13.0 资产解压到位 → `sshmgr serve install` → RUNNING | 我经 MCP 代跑 |
| G10 | update 回路:NUC10 上 `SSHMGR_UPDATE_BASE` 指本地假源(构造 v0.13.0→v0.13.1-test)跑 `sshmgr update --yes` 全链,断言替换+服务重启+版本翻转,再指回真源自愈 | 我经 MCP 代跑 |
| G11 | `.old` 残留清理 + 笔记本 client `sshmgr update`(下次 agent 会话新版生效) | 我 + 本机 |
| G12 | 旧服务名残留场景:构造 `ssh-manager-serve` 注册态 → `update` 打印迁移块并中止(不半更新) | 我经 MCP 代跑 |

证据:update 输出留档(版本对/SHA256 命中/重启结果)随验收报告入 repo。

## 8. 文档与迁移

- `deployment-modes.md`:升级节重写为"`sshmgr update` 一条命令";一次性迁移块(§3.2)置顶
- `broker-host-agent.md` / `quickstart` / `multi-machine.md` / README×2:改名 sweep + update 段
- `compat-matrix.md`:v0.13.0 行 breaking=②a 移除(批1)+ 二进制改名;版本号回写
- `threat-model.md`:R13 自更新供应链条 + §6 更新面收窄声明
- TUI goldens(`mcpConfigLines` 等)与 pair 产物(`pair.<name>.mcp.json` 的 command)同步新名——既有 goldens 断言更新为计划内变更

## 9. 交付与版本

- **v0.13.0 = 批1(模式缩减+发现+SAS 配对)+ Plan 44(自更新+改名)**,一次 breaking 一次迁移
- v0.14.0 = 批2(Web UI);其 rollout 直接受益于 update 已就位
- Plan 44 spec → 2 轮盲评 → plan → SDD(体量:估 6-8 任务)

## 10. 登记不实施(deferred)

- 并发 update 文件锁(文档"不要并发跑"代替)
- 下载断点续传/进度条(资产 <30MB)
- 后台自动检查/自动更新(明确拒绝——升级次序铁律)
- v0.12.1 backport 桥(明确拒绝——2 台机器一次迁移不值得)
- `sshmgr version --check` 联动(`update --check` 已覆盖)
- goreleaser 资产签名(见 §5,将来独立决策)
