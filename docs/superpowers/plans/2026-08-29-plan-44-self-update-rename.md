# Plan 44 实施计划:sshmgr 自更新 + 改名 ssh-manager → sshmgr

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 一条命令 `sshmgr update` 完成 serve/client 的自更新(GitHub 直连+校验+事务性替换+服务重启),并把二进制从 `ssh-manager` 一刀切改名 `sshmgr`。

**Architecture:** 新增 `internal/updater` 包(version/transport/discover/download/extract/replace/service 七文件,纯函数+可注入 seam),CLI 装配层 `internal/cli/update.go` 复用既有 `ExitCodeError`/`probeServeHTTP` 惯例;改名 sweep 先行(代码面→文档面),后续任务全部直接写终名。零新依赖(`x/sys` 已是直接依赖、kardianos 已有)。

**Tech Stack:** Go 1.25 stdlib(net/http、archive/zip、archive/tar+gzip、crypto/sha256、os/exec)+ golang.org/x/sys(windows QueryServiceConfig)+ kardianos/service。

**Spec:** `docs/superpowers/specs/2026-08-29-plan-44-self-update-rename-design.md.rev2.md`(定稿;本 plan 的一切争议以 spec 为准)

## Global Constraints

- **零新依赖**(spec §2 Q2):不新增任何 go.mod 条目;`golang.org/x/sys` 与 `github.com/kardianos/service` 已是直接依赖
- **module path 不变** `ssh-manager-mcp`(spec §3.1):import 路径零改动
- **无别名**(spec §3.1):不留 `ssh-manager` 兼容名;`.mcp.json` 的 MCP server 名(如 `"ssh"`)与项目名不变
- 服务名改 `sshmgr-serve`(spec §3.1);进程探测名同步(spec §3.1 进程探测行)
- **v0.13.0 捆批1**(spec §9):本 plan 与批1 同一次 breaking 发布
- 中文 conventional commits(仓内惯例)
- 所有新用户可见文案中文为主、与既有 CLI 文案风格一致
- 传输安全规则(spec §4.2(4)):https 强制,环回例外**仅** `{127.0.0.1, ::1}`(`url.Parse→Hostname()` 精确匹配,localhost 不入集);重定向**每一跳**校验;默认白名单 4 宿主;自定义 base 资产 URL 以 base 源重建
- 事务不变式(spec §4.3):替换点之前的失败=零变更;rename 后失败=committed-with-error 如实报告
- 每个任务完成时 `go build ./... && go test ./...` 全绿(Windows 本机跑,`GOOS=linux/darwin go build ./...` 交叉编译验证)

---

### Task 1: 改名 sweep——代码面 + CI + goreleaser

**Files:**
- Modify: `cmd/ssh-manager/main.go` → **git mv** 整目录为 `cmd/sshmgr/main.go`
- Modify: `.goreleaser.yml`、`internal/cli/root.go`、`internal/cli/serve_service.go`、`internal/cli/serve_service_process_windows.go`、`internal/cli/serve_service_process_other.go`
- Modify: `.github/workflows/serve-install.yml`(以及 grep 命中的其他 workflow)
- Modify: grep 命中的全部 Go 文件(用户可见字符串 + 构建引用;清单见步骤)
- Test: 既有全量测试(goldens 同步更新为计划内变更)

**Interfaces:**
- Produces: 全仓二进制名/服务名/进程名/用户文案 = `sshmgr` 系;后续任务引用的常量名不变(`serveServiceName`、`serveDisplayName` 值变)

- [ ] **Step 1: git mv 构建入口**

```bash
git mv cmd/ssh-manager cmd/sshmgr
```

`cmd/sshmgr/main.go` 内 import 全是 `ssh-manager-mcp/...` 不动(module path 不变)。

- [ ] **Step 2: goreleaser 与构建管线**

`.goreleaser.yml`:`project_name: sshmgr`;build id `sshmgr`;`main: ./cmd/sshmgr`;`binary: sshmgr`;release `name_template: "sshmgr {{ .Tag }}"`(ldflags 的 `-X ssh-manager-mcp/internal/buildinfo.Version` **不变**——module path 没变)。

`.github/workflows/serve-install.yml`:`go build -o ssh-manager ./cmd/ssh-manager` → `go build -o sshmgr ./cmd/sshmgr`;`./ssh-manager[.exe] serve uninstall` 行、`schtasks /Delete /TN ssh-manager-serve` 行全部同步;注释里的旧名一并改。检查 `release.yml`/`goreleaser-check.yml` 的 `cmd/**` 触发路径与注释。

- [ ] **Step 3: root/服务名/进程探测**

`internal/cli/root.go:13` `Use: "ssh-manager"` → `"sshmgr"`。`internal/cli/serve_service.go:89` `serveServiceName = "ssh-manager-serve"` → `"sshmgr-serve"`;`:97` `serveDisplayName = "ssh-manager serve"` → `"sshmgr serve"`。`internal/cli/serve_service_process_windows.go`:`tasklist.exe /FI "IMAGENAME eq ssh-manager.exe"` → `sshmgr.exe`,注释与 `EqualFold` 行同步。`internal/cli/serve_service_process_other.go`:`target := "ssh-manager"` → `"sshmgr"`(注释里"11 chars"改"6 chars")。

- [ ] **Step 4: Go 构建/测试引用**

`rg -n "cmd/ssh-manager" --type go` 逐处改(已知:`internal/eval/broker.go`、`internal/conformance/tunnel_kill_test.go`、`internal/cli/serve_install_integration_test.go`);`rg -n '"ssh-manager(\.exe)?"' --type go` 中**构建产物名**引用同步。

- [ ] **Step 5: 用户可见字符串全量替换**

```bash
rg -n "ssh-manager " --type go | rg -v "ssh-manager-mcp|import"   # ~110 处/35 文件预期
```

逐文件把 help/Long/Short/错误指引里的 `ssh-manager <子命令>` 改 `sshmgr <子命令>`(`ssh-manager-mcp` module 串、`allanpk716/ssh-manager-mcp` repo 串、URL **不动**)。TUI goldens(`internal/tui/` 中断言含旧名的测试)同步——这是计划内 golden 变更,不是测试放水。

- [ ] **Step 6: 验证 + 提交**

```bash
gofmt -l . && go build ./... && GOOS=linux go build ./... && GOOS=darwin go build ./... && go test ./...
rg -n "ssh-manager" --type go | rg -v "ssh-manager-mcp|allanpk716"   # 期望:零命中(module 串除外)
```

```bash
git add -A && git commit -m "refactor!: 改名 ssh-manager→sshmgr——cmd 目录/goreleaser/CI/服务名 sshmgr-serve/进程探测/用户文案全量 sweep(module path 不变;Plan 44 T1)"
```

---

### Task 2: 改名 sweep——文档面 + threat-model + compat-matrix

**Files:**
- Modify: `docs/` 全部命中文件 + `README.md` + `docs/README.md`(grep 清单见步骤)
- Modify: `docs/threat-model.md`、`docs/compat-matrix.md`、`docs/deployment-modes.md`

**Interfaces:**
- Consumes: Task 1 的终名
- Produces: v0.13.0 迁移 runbook 文本(spec §3.2,后续任务引用其存在)

- [ ] **Step 1: grep 全量清单驱动替换**

```bash
rg -l "ssh-manager" docs/ README.md .github/ | rg -v "ssh-manager-mcp"
```

命中至少:agent-access.md、agent-tools.md、concepts.md、backup-restore.md、getting-started.md、scenarios.md、managing-servers.md、tui-single-machine.md、tui-multi-machine.md、quickstart-single-machine.md、multi-machine.md、broker-host-agent.md、deployment-modes.md、README.md、docs/README.md、compat-matrix.md、threat-model.md、handoff-context-timeout-hardening.md。二进制调用处 `ssh-manager(.exe) <cmd>` → `sshmgr(.exe) <cmd>`;**项目名/repo 名/module 名串不动**;`.mcp.json` 示例 command 路径同步(spec §3.1)。

- [ ] **Step 2: deployment-modes.md 增 v0.13.0 迁移 runbook + update 段**

把 spec §3.2 的单一 runbook(②a 桥迁→client 改名→serve 迁移最后,含 sc qc 读参数/checksums 核验)原样置顶,后续加"`sshmgr update` 一条命令"升级节。

- [ ] **Step 3: threat-model.md 增 R13**

按 spec §5 五条子风险(仓库被攻破/本地 env 注入/`--no-verify`/staged 自检执行面/服务 exe 用户可写目录=C1 拍板登记)写入,§6 增更新面收窄声明。

- [ ] **Step 4: compat-matrix.md v0.13.0 行**

breaking 面列全四项:②a 移除(批1)、二进制改名、服务名变更、`.mcp.json` command/path 迁移;版本号回写 v0.13.0。

- [ ] **Step 5: 验证 + 提交**

```bash
rg -n "ssh-manager" docs/ README.md | rg -v "ssh-manager-mcp|allanpk716"   # 期望:零命中
git add -A && git commit -m "docs!: Plan 44 T2——改名 sweep 文档面+v0.13.0 runbook 置顶+threat-model R13+compat-matrix 四 breaking 面"
```

---

### Task 3: `internal/updater/version.go` + buildinfo 常量

**Files:**
- Create: `internal/updater/version.go`、`internal/updater/version_test.go`
- Modify: `internal/buildinfo/buildinfo.go`

**Interfaces:**
- Produces(后续任务依赖的精确签名):

```go
// internal/buildinfo/buildinfo.go 追加:
const Owner = "allanpk716"
const Repo  = "ssh-manager-mcp"

// internal/updater/version.go:
type Version struct{ Major, Minor, Patch int; Pre []string }
func ParseVersion(s string) (Version, error)            // 容 "1.2.3"/"v1.2.3"/"1.2.3-rc.1";拒绝 "dev"/乱串
func CompareVersions(a, b Version) int                  // -1/0/1;pre 按 SemVer 标识符段:数字段数值比、字段字典序、数字段<字母段、段多者大
func NormalizeVersionOutput(s string) string            // TrimSpace + 去前缀 v(staged 自检契约)
func ValidateTag(s string) error                        // ^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$
func AssetName(version, goos, goarch string) (string, error)  // "sshmgr_"+去v+os+arch+扩展;矩阵外返回错误并列矩阵
```

- [ ] **Step 1: 写失败测试**(表测:三元比较/pre 规则含 `1.0.0-rc.1 < 1.0.0-rc.2 < 1.0.0`、同三元不同 pre 可钉版/非法输入含 "dev"/`AssetName` windows→zip 其余→tar.gz/矩阵外 `linux/386` 报错/`NormalizeVersionOutput(" 1.2.3\n")=="1.2.3"`、`("v1.2.3")=="1.2.3"`/`ValidateTag` 合法与 `v1..2`、`v1.2.3;rm` 拒绝)
- [ ] **Step 2: 跑测试确认失败**(`go test ./internal/updater/`)
- [ ] **Step 3: 实现**(纯函数,无依赖)
- [ ] **Step 4: 测试绿 + 提交** `feat(updater): Plan 44 T3——版本三元比较(SemVer pre 标识符段)+tag 校验+资产名计算+buildinfo Owner/Repo`

---

### Task 4: 传输层 + 发现 + 下载校验

**Files:**
- Create: `internal/updater/transport.go`、`internal/updater/discover.go`、`internal/updater/download.go` + 对应 `_test.go`

**Interfaces:**
- Consumes: Task 3 的 `ValidateTag`/`AssetName`/`buildinfo.Owner/Repo`
- Produces:

```go
func BaseURL() string                                  // env SSHMGR_UPDATE_BASE 或 "https://api.github.com"
func IsLoopbackLiteral(host string) bool               // 精确 {127.0.0.1, ::1};localhost=false
func NewHTTPClient() *http.Client                      // CheckRedirect:初始+每跳 scheme/host 校验
type Release struct{ Tag, AssetName, AssetURL, ChecksumsURL string }
func LatestRelease(ctx context.Context) (*Release, error)      // GET {base}/repos/{owner}/{repo}/releases/latest,30s 超时
func ReleaseByTag(ctx context.Context, tag string) (*Release, error)  // 先 ValidateTag+url.PathEscape;GET .../releases/tags/{tag}
func ParseChecksums(data []byte, asset string) (string, error)  // 多行 sha256+两空格+名;容 CRLF;缺行报错
func DownloadAsset(ctx context.Context, url, wantSHA256, destDir string) (string, error)
  // 流式落盘边算 SHA256;idle 60s 零字节或总 10min 中止(包装 Reader 计时);200MiB 上限;不符报错且目标零残留
```

**关键实现(redirect 策略,spec §4.2(4) 逐字落实):**

```go
var defaultHosts = map[string]bool{
    "api.github.com": true, "github.com": true,
    "objects.githubusercontent.com": true, "release-assets.githubusercontent.com": true,
}

func allowedHop(u *url.URL, custom string) error {
    if u.Scheme == "https" { /* host 检查:custom!="" → u.Hostname()==hostOf(custom);否则 defaultHosts */ }
    if u.Scheme == "http" && IsLoopbackLiteral(u.Hostname()) { return nil } // 环回例外,逐跳
    return fmt.Errorf("blocked redirect/scheme: %s", u.Redacted())
}
// client.CheckRedirect: 对 req.URL 逐跳 allowedHop;错误即中止(Go 停止跟随)
```

自定义 base(`BaseURL()` != 默认)时:discover 解析出的 `browser_download_url` **以 base 为源重建**(`u2 := *base; u2.Path = u.Path`——镜像必须照搬 GitHub 路径结构)。

- [ ] **Step 1: 写失败测试**(httptest + `SSHMGR_UPDATE_BASE=http://127.0.0.1:PORT`):环回 http 放行;非环回 http 拒;**变体矩阵拒**(`127.1`、`0x7f.0.0.1`、`localhost`、`localhost.`、`[::1]%zone`、`127.0.0.1:80@evil`、trailing dot——先过 `IsLoopbackLiteral` 单测再过 client 集成);每跳白名单(redirect 链中插非法中间跳→拒);自定义 base 资产 URL 重建断言 + 越界宿主拒;base 环回但跳非环回→该跳必须 https;checksums 解析(CRLF/缺行);下载 SHA256 不符→目标零残留;注入慢速 Reader→idle 超时
- [ ] **Step 2: 跑测试确认失败**
- [ ] **Step 3: 实现**(seam:`var httpDo = func(c *http.Client, req *http.Request) (*http.Response, error)`)
- [ ] **Step 4: 测试绿 + 提交** `feat(updater): Plan 44 T4——传输安全(每跳白名单+环回例外+自定义base重建)+release发现+SHA256下载校验`

---

### Task 5: 安全解压

**Files:**
- Create: `internal/updater/extract.go`、`internal/updater/extract_test.go`

**Interfaces:**
- Produces: `func ExtractBinary(archivePath, goos string) (string, error)` — 流式遍历,**只落地**名称精确等于 `sshmgr`/`sshmgr.exe` 的**根**条目到 `archivePath` 同目录 `<dir>/sshmgr[.exe]`,落地即 `chmod 0755`;拒绝绝对路径/`..`/子目录/重复名/symlink/hardlink/device/非 regular;单成员 200MiB 上限;`O_CREATE|O_EXCL|O_WRONLY`

- [ ] **Step 1: 写失败测试**:黄金 zip + tar.gz 夹具(programmatic 构造,含多余 LICENSE 条目验证"其余不落地");恶意矩阵:条目名 `../evil`、`/abs/evil`、`sub/sshmgr`、重复 `sshmgr`、symlink header、hardlink header、chardev、目录条目同名;断言恶意夹具→错误且目录零新文件;正常夹具→文件存在且 mode 恰 0755(Unix)
- [ ] **Step 2: 确认失败 → 实现(zip.OpenReader / tar+gzip 流式;`filepath.Base(name)==name` 且非目录且 `typeflag` 为 regular)→ 绿 → 提交** `feat(updater): Plan 44 T5——流式精确名解压(zip slip 不可能;落地即 0755)`

---

### Task 6: 事务性替换 + 自愈 + staged 自检

**Files:**
- Create: `internal/updater/replace.go`、`internal/updater/replace_test.go`

**Interfaces:**
- Consumes: Task 3 `NormalizeVersionOutput`
- Produces:

```go
var ErrRollbackFailed = errors.New("replace: rollback failed — manual recovery required")
func StagedFSync(path string) error                    // rename 前置,双平台
func ReplaceBinary(staged, self string) error          // spec §4.3 伪码逐行:windows 代际 .old+回滚;linux/darwin 原子 rename+目录 fsync(committed-with-error 不回滚)
func CleanOldBackups(self string) error                // 尽力清 self+".old*";失败仅警告返回 nil 语义
func DetectHeal() (healHint string, ok bool)           // self 缺失+代际 .old 存在;或 os.Executable() 自身带 .old.<ts> 后缀且 canonical 缺失
func StagedVersionCheck(staged, wantVersion string) (string, error)
  // seam: var execStaged = exec.CommandContext;10s 超时;输出限 4KiB;NormalizeVersionOutput(got)==NormalizeVersionOutput(want)
```

**关键实现(windows 分支,spec §4.3 逐字):**

```go
// runtime.GOOS == "windows":
//   backup := self + ".old." + strconv.FormatInt(time.Now().Unix(), 10)
//   os.Rename(self, backup)            // 运行中 exe 可改名不可删
//   if err := os.Rename(staged, self); err != nil {
//       if rbErr := os.Rename(backup, self); rbErr != nil {
//           return fmt.Errorf("%w: ren %s %s", ErrRollbackFailed, backup, self)
//       }
//       return err
//   }
// 其余: os.Rename(staged, self); fsyncDir(filepath.Dir(self)) 失败→committed-with-error(返回专用错误类型但已提交)
```

`DetectHeal` 的第二入口(执行路径自身是 `.old.<ts>` 后缀)在 CLI 层消费——canonical 缺失时正在执行的就是备份,rename 回 canonical 即恢复。

- [ ] **Step 1: 写失败测试**(临时目录副本,不限平台):staged fsync 失败(seam 注入)→不进 rename;校验前置→目标字节不变;写入失败注入(seam `osRename` 变量)→回滚→目标字节不变;**double-fault→ErrRollbackFailed 且 Error() 含手工恢复命令**;代际:预置 `x.old.111`+`x.old.222` 两代,回滚取**最新**;CleanOldBackups 清得掉就清、清不掉(EACCES 造不出就 seam)不报错;StagedVersionCheck:假输出(seam 返回 "v1.2.3\n")命中 `1.2.3`;输出不符拒;超时(seam 返回挂起→10s ctx);DetectHeal 三分支
- [ ] **Step 2: 确认失败 → 实现 → 绿 → 提交** `feat(updater): Plan 44 T6——事务性替换(代际.old+回滚+double-fault)+崩溃自愈+staged 自检(10s/4KiB/规范化契约)`

---

### Task 7: 服务检测三分法 + 路径预检 + 迁移块

**Files:**
- Create: `internal/updater/service.go`、`internal/updater/service_test.go`

**Interfaces:**
- Consumes: kardianos `service`;`serveServiceName`(cli 包——为避 import 环,**常量复制为 `updater.ServiceNameNew = "sshmgr-serve"`/`ServiceNameOld = "ssh-manager-serve"` 并加注释与 cli 保持同步**,或放 buildinfo;选**放 buildinfo** 避免双份:Task 1 已把 `serveServiceName` 值改 `sshmgr-serve`,本任务把它**上提**为 `buildinfo.ServeServiceName` 常量,cli 引用之)
- Produces:

```go
type ProbeState int
const (ProbeNotInstalled ProbeState = iota; ProbeInstalled; ProbeMechanismErr)
type ProbeResult struct{ State ProbeState; Desc string }
func ProbeService(name string) ProbeResult
  // seam: var serviceNew = service.New;三分法:ErrNotInstalled→NotInstalled;
  // systemd failed 态(kardianos 返回错误但 unit 存在)→Installed;无法判定→MechanismErr
func RegisteredBinaryPath(name string) (string, error)
  // windows: x/sys/windows OpenSCManager+OpenService+QueryServiceConfig→lpBinaryPathName 解析出 exe 路径(去 " 和参数)
  // linux: 读 /etc/systemd/system/<name>.service 的 ExecStart 首字段
  // darwin: 读 plist ProgramArguments 首项;其余平台/失败→error
func SameBinaryPath(a, b string) bool   // 两侧 filepath.Abs+EvalSymlinks;windows 追加 EqualFold
func MigrationBlock() string            // spec §3.2 runbook 文本(cli 输出与 docs 同源措辞)
```

**注意**:`QueryServiceConfig` 在 `golang.org/x/sys/windows`(已是直接依赖)——**不解析 `sc qc` 文本**(FINDING E 教训,spec §4.4)。systemd failed 态识别:kardianos `Status()` 的错误含 "failed"/unit 存在的已知模式归 Installed——实现读 kardianos 行为并在测试里以 seam 模拟三种返回。

- [ ] **Step 1: 写失败测试**(seam 模拟 kardianos 返回):NotInstalled/Running/Stopped/**failed(错误串但已装)**/机制错误四类→各自 ProbeState;`ErrNoServiceSystemDetected`→CLI 层跳过(本任务返回 MechanismErr+Desc 供 CLI 判断);双服务并存(新旧都 Installed)→旧名优先判定;RegisteredBinaryPath:linux 用临时假 unit 文件(路径 seam 化 `systemdUnitDir`);windows 侧逻辑在真机 G 验证,单测覆盖解析函数(输入 `ExecStart=/usr/local/bin/sshmgr serve --addr ...` → `/usr/local/bin/sshmgr`);SameBinaryPath:大小写(仅 win 语义,单测平台跑等值分支)/符号链接归一
- [ ] **Step 2: 确认失败 → 实现 → 绿 → 提交** `feat(updater): Plan 44 T7——服务三分法检测(failed 态放行)+注册路径预检(x/sys QueryServiceConfig/systemd/launchd)+迁移块`

---

### Task 8: `internal/cli/update.go` 命令装配

**Files:**
- Create: `internal/cli/update.go`、`internal/cli/update_test.go`
- Modify: `internal/cli/root.go`(注册 newUpdateCmd)、`internal/cli/exit.go`(注释补 3=update 替换成功/重启待手工)

**Interfaces:**
- Consumes: T3-T7 全部;`probeServeHTTP`(serve_service.go,同包);`ExitCodeError`(退出码 3=重启待手工)
- Produces: `newUpdateCmd() *cobra.Command`,flags:`--check/--yes/--version/--file/--sha256/--no-verify`;env `SSHMGR_UPDATE_BASE`(经 updater.BaseURL())

**编排顺序(RunE 内,spec §4/§4.3/§4.4 逐条):**

```
1. flag 互斥校验(--sha256 与 --no-verify;--file 与 --version 可组合[给 staged 比对基准]→不,spec:--file 目标=staged 输出,互斥也禁)
2. DetectHeal → 命中:交互确认恢复(非 TTY 或 --yes → 报错退出,spec §4.3 自愈永不 --yes 豁免)
3. 非 --file:ProbeService(新名)→MechanismErr 且非 ErrNoServiceSystemDetected→中止;Installed→RegisteredBinaryPath 预检:
     读不到→中止;SameBinaryPath(注册路径, self)==false→中止打印两端
   ProbeService(旧名)Installed(任何态)→打印 MigrationBlock() 并中止
4. 版本发现(--version 走 ByTag;否则 LatestRelease);已是最新 exit 0;
   当前版本 ParseVersion 失败(dev)→报错要求 --version;dev+--version→通用警告"无法判定升降级"
5. 下载 checksums+资产(SHA256)→ ExtractBinary → StagedVersionCheck
   (--file 路径:跳过发现;staged 输出=目标版本;"已装该版本"拒绝与降级警告以它为基准)
6. 确认(当前→最新;--yes 跳过;**非 TTY 且无 --yes→报错退出要求 --yes**;此规则仅在确认节点触发,
   --check/已是最新不触发)
7. 降级判定(目标<当前):即使 --yes 也打印"降级至 vX(回滚通道)"
8. StagedFSync → ReplaceBinary;ErrRollbackFailed→打印手工恢复命令,非零退出
9. 服务 Installed:警告行(隧道断开+配对作废)→Restart() 成功→probeServeHTTP 健康行;
   失败→打印平台手工命令+NewExitCodeError(3, ...)【替换成功/重启待手工】;
   Stopped→打印启动命令;未装→"新版本下次 agent 会话生效"
10. 证据行:版本对/资产名/SHA256 命中/update base(非默认醒目)/替换路径/staged 结果/重启结果(+健康行)
```

- [ ] **Step 1: 写失败测试**(cobra 驱动,httptest 假源+环回 base+seam):`--check` 干跑零副作用且**非 TTY 不报错**;已是最新 exit 0;确认节点非 TTY 无 `--yes`→报错;`--yes` 全链(临时 HOME 二进制副本)版本翻转;旧服务名 Installed→MigrationBlock+中止;路径不一致→中止打印两端;`--file --no-verify`→未校验警告+执行;`--sha256`+`--no-verify` 同给→报错;降级→警告行;Restart 失败(seam)→退出码 3;证据行含 base
- [ ] **Step 2: 确认失败 → 实现 → 绿 → 提交** `feat(cli): Plan 44 T8——sshmgr update 命令装配(确认/非TTY/降级/证据行/退出码3=重启待手工/健康回探)`

---

### Task 9: 集成测试 + 真机 gate 清单

**Files:**
- Create: `internal/updater/integration_test.go`(或 cli 层,视 Step 1 结论)
- Modify: 本 plan 文件尾部(gate 清单勾选留档)

**Interfaces:**
- Consumes: 全部

- [ ] **Step 1: fake 源全回路集成测试**(build tag `integration` 或常规):httptest 起假 GitHub(serve 真实 JSON 结构:latest+资产+checksums,zip 内塞一个真实编译的小二进制或 stub);`SSHMGR_UPDATE_BASE` 指环回;对**编译产物副本**(test 自建 tiny main)执行完整 update;断言:版本翻转、`.old` 代际清理、目标原字节在失败回路不变
- [ ] **Step 2: 全量回归** `go build ./... && go test ./...`
- [ ] **Step 3: 提交** `test(updater): Plan 44 T9——fake 源全回路集成(版本翻转+代际清理+失败零变更)`

- [ ] **Step 4: 真机 gate 清单(发布后执行;G1-G8 为批1 既有,此处补 G9-G12;SAS 比对=owner,机械项=助手经 ssh MCP 代跑附证据)**

| # | 项 | 执行者 | 完成 |
|---|---|---|---|
| G9 | NUC10 迁移 runbook 全流(sc qc 读参→uninstall→curl 资产+checksums 核验→解压→install→RUNNING) | 助手(MCP) | ☐ |
| G10 | update 回路:环回假源 v0.13.0→v0.13.1-test `--yes` **提升会话**全链;断言替换+重启+probeServeHTTP 健康行+版本翻转+证据行含 base;指回真源自愈 | 助手(MCP) | ☐ |
| G11 | `.old` 清理+笔记本 client update;**断言 agent 实际 spawn 的 mcp --cache 报新版**;旧 exe 最后删 | 助手+本机 | ☐ |
| G12 | 旧服务名矩阵:旧名注册→迁移块中止;双服务并存→中止;failed 态→放行;机制错误→fail-closed | 助手(MCP) | ☐ |

---

## Self-Review(已执行)

1. **Spec 覆盖**:§3.1 sweep→T1/T2;§3.2 runbook+旧名检测→T2/T7/T8;§4.1→T3/T4/T8;§4.2→T4/T5/T8;§4.3→T5/T6/T9;§4.4→T7/T8;§4.5/§4.6→T8;§5→T2;§6 各断言散布各任务 Step 1;§7→T9 Step 4;§8→T2;§10 不实施项零任务命中。无缺口。
2. **占位扫描**:无 TBD/TODO;关键算法(redirect/替换/三分法)给了代码;机械 sweep 给了 grep 命令与逐文件清单来源。
3. **类型一致性**:`Version/ParseVersion/CompareVersions/AssetName/Release/LatestRelease/ParseChecksums/DownloadAsset/ExtractBinary/ReplaceBinary/DetectHeal/StagedVersionCheck/ProbeService/RegisteredBinaryPath/SameBinaryPath/MigrationBlock` 跨任务签名一致;`serveServiceName` 上提 buildinfo 的决定在 T1/T7 间无环(T1 只改值,T7 上提,cli 改引用——**修订:T1 不动定义位置,T7 做 git-free 上提**)。

## 执行选项

**1. Subagent-Driven(推荐)** — 每任务派新实现者+独立评审,迭代快
**2. Inline** — 本会话按 executing-plans 批量执行
