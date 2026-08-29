package cli

// sshmgr update — self-update command assembly (Plan 44 T8; spec
// 2026-08-29-plan-44-self-update-rename §4.4/§4.5/§4.6). This file is the
// single home of the command's interaction semantics, evidence lines and
// exit codes; the mechanics live in internal/updater (T3–T7: version
// arithmetic, transport, discovery, download+checksums, extract, replace,
// service awareness) and are consumed verbatim.
//
// Orchestration (spec §4 + §4.3 + §4.4, pinned order):
//
//	 1. flag validation (--file ⊥ --version; --sha256 ⊥ --no-verify; …)
//	 2. DetectHeal → interactive-confirm recovery (--yes is NOT exempt)
//	 3. service gates: probe NEW name (MechanismErr → fail-closed abort —
//	    spec §3.2 勘误, no skip branch; Installed → registered-path precheck),
//	    probe LEGACY name (Installed in ANY state → migration block + abort)
//	 4. version discovery (--version → ByTag, else LatestRelease);
//	    already-latest → exit 0; dev current version handling
//	 5. download checksums+asset (SHA256) → ExtractBinary → StagedVersionCheck
//	    (--file: skip discovery/download; staged output IS the target version)
//	 6. confirm (current → target; non-TTY without --yes errors HERE only)
//	 7. downgrade verdict (target < current): prominent line even with --yes
//	 8. ReplaceBinary (fsync folded inside; rollback/double-fault surface)
//	 9. service branch: running → warn+confirm+Restart (exit 3 on failure);
//	    stopped → start command only; not installed → "next agent session"
//	10. evidence lines interleaved: base / version pair / asset / SHA256 /
//	    staged check / replace path / restart + health probe

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/updater"
)

// defaultProbeAddr is the health-probe target for the post-restart check
// (serve install's default --addr; a serve bound to 0.0.0.0 answers on
// loopback too). A serve installed on a non-default addr yields an honest
// "not responding" line with a hint — the probe is evidence, not a verdict.
const defaultProbeAddr = "127.0.0.1:7878"

// defaultUpdateBase is the default value of updater.BaseURL() (updater's
// defaultBaseURL, mirrored here only for the "non-default → prominent"
// evidence marker; drift would at worst mislabel the marker, never change
// behavior — the actual base always comes from updater.BaseURL()).
const defaultUpdateBase = "https://api.github.com"

// updateBaseEnv is the env seam name (spec §4.6). Referenced (not re-parsed)
// to decide the prominence marker: a value explicitly set in the environment
// is exactly what "非默认" means, with no second parsing of the base URL.
const updateBaseEnv = "SSHMGR_UPDATE_BASE"

// shaHexRe matches a user-supplied --sha256 digest (64 hex chars).
var shaHexRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// --- seams (spec §4.6: 生产路径必须留缝; tests swap these wholesale) --------

var (
	probeService         = updater.ProbeService
	registeredBinaryPath = updater.RegisteredBinaryPath
	detectHeal           = updater.DetectHeal
	serveHTTPProbe       = probeServeHTTP

	// resolveSelf pins the binary the update replaces (canonical form).
	resolveSelf = func() (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		return filepath.EvalSymlinks(exe)
	}

	currentVersionStr = func() string { return buildinfo.Version }

	stdinIsTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

	readConfirmLine = func() (string, error) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}
)

// serviceRestarter is the minimal kardianos surface the restart branch needs.
// kardianos's Service is a concrete struct (same lesson as updater's
// statusProber seam), so the serviceNew seam widens the return to this
// interface; the default wraps the real service.New and error identity flows
// through errors.Is untouched.
type serviceRestarter interface {
	Status() (service.Status, error)
	Restart() error
}

var serviceNew = func(i service.Interface, c *service.Config) (serviceRestarter, error) {
	return service.New(i, c)
}

// --- command ------------------------------------------------------------------

type updateOpts struct {
	check     bool
	yes       bool
	wantTag   string // --version <tag>
	file      string // --file <包>
	sha256Hex string // --sha256 <hex>
	noVerify  bool   // --no-verify
}

func newUpdateCmd() *cobra.Command {
	var o updateOpts
	c := &cobra.Command{
		Use:   "update",
		Short: "Self-update sshmgr from GitHub Releases (or a local --file package)",
		Long: `Self-update the sshmgr binary in place, restarting the serve service if one is
registered.

  sshmgr update                          check → confirm → download → verify →
                                         staged self-check → replace → restart
  sshmgr update --check                  dry run: report current/latest/asset/
                                         update base; touches nothing
  sshmgr update --yes                    skip confirmations (REQUIRED for
                                         non-TTY/scripted runs)
  sshmgr update --version v0.13.0        install a pinned tag (downgrade =
                                         rollback channel, explicit warning)
  sshmgr update --file <pkg> [--sha256 <hex> | --no-verify]
                                         install from a local archive
                                         (.zip/.tar.gz; owner-supplied file =
                                         you downloaded it)

Trust chain: HTTPS only (loopback literals excepted) + per-hop host whitelist
+ same-release checksums.txt SHA256. Update source is
` + buildinfo.Owner + `/` + buildinfo.Repo + ` on GitHub Releases; override the base with
SSHMGR_UPDATE_BASE (a mirror must mirror GitHub's URL path layout; the
effective base is always shown in the output). Unauthenticated GitHub API
rate limit is 60/h/IP — ample for manual updates.

Interaction: two confirmation points (update, then service restart). On a
non-TTY stdin --yes is required AT THE CONFIRM POINTS; --check and the
already-latest path never ask. A crash-window self-heal prompt is
interactive-only — --yes does NOT exempt it.

Exit codes: 0 = done (or already latest, or declined cleanly);
3 = binary replaced OK but the service restart is pending manual action
(the manual command is printed); anything else = failure (zero changes,
except a failed rollback — recovery command is printed).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUpdateCmd(cmd, o)
		},
	}
	f := c.Flags()
	f.BoolVar(&o.check, "check", false, "dry run: report current/latest/asset/update base, change nothing")
	f.BoolVar(&o.yes, "yes", false, "assume yes at confirmation points (required on non-TTY stdin)")
	f.StringVar(&o.wantTag, "version", "", "install this release tag (e.g. v0.13.0) instead of latest; downgrade = rollback channel")
	f.StringVar(&o.file, "file", "", "install from a local archive (.zip/.tar.gz/.tgz) instead of GitHub discovery")
	f.StringVar(&o.sha256Hex, "sha256", "", "expected sha256 of --file (hex); with --file only")
	f.BoolVar(&o.noVerify, "no-verify", false, "skip --file integrity check (explicit trust in the local file); with --file only")
	return c
}

// runUpdateCmd is the orchestration body. See the package-block order at the
// top of this file; step numbers below match it.
func runUpdateCmd(cmd *cobra.Command, o updateOpts) error {
	out := cmd.OutOrStdout()
	errw := cmd.ErrOrStderr()
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// --- 1. flag validation ---------------------------------------------------
	if o.file != "" && o.wantTag != "" {
		return errors.New("--file 与 --version 互斥:--file 的目标版本以包内 staged 输出为准,请二选一")
	}
	if o.sha256Hex != "" && o.noVerify {
		return errors.New("--sha256 与 --no-verify 互斥:要么核对该 hex,要么显式声明不校验")
	}
	if o.file == "" {
		if o.sha256Hex != "" {
			return errors.New("--sha256 仅在 --file 模式下有效")
		}
		if o.noVerify {
			return errors.New("--no-verify 仅在 --file 模式下有效")
		}
	} else {
		if o.check {
			return errors.New("--check 与 --file 不兼容:本地包模式没有远端版本可查")
		}
		if o.sha256Hex == "" && !o.noVerify {
			return errors.New("--file 需要显式二选一:--sha256 <hex> 或 --no-verify(不许静默跳过校验)")
		}
		if o.sha256Hex != "" && !shaHexRe.MatchString(o.sha256Hex) {
			return errors.New("--sha256 不是 64 位十六进制 sha256 摘要")
		}
	}

	// 证据行:update base(spec §4.5;非默认时额外醒目)。
	if strings.TrimSpace(os.Getenv(updateBaseEnv)) == "" {
		fmt.Fprintf(out, "update base: %s\n", updater.BaseURL())
	} else {
		fmt.Fprintf(out, "update base(非默认,已重定向!): %s\n", updater.BaseURL())
	}

	// --- 2. crash-window self-heal -------------------------------------------
	self, err := resolveSelf()
	if err != nil {
		return fmt.Errorf("定位自身可执行文件: %w", err)
	}
	if hint, heal := detectHeal(); heal {
		// 自愈确认 = 交互式 only,--yes 不豁免(spec §4.3);非 TTY 直接报错。
		if o.yes {
			return fmt.Errorf("检测到待自愈状态,但 --yes 不豁免自愈确认(防误恢复):\n%s", hint)
		}
		if !stdinIsTTY() {
			return fmt.Errorf("检测到待自愈状态且当前为非交互终端,请交互式运行 update 或按提示手工恢复:\n%s", hint)
		}
		fmt.Fprintf(out, "检测到上次更新中断(可自愈):\n%s\n", hint)
		ok, cerr := askConfirm(errw, "是否立即恢复(将备份移回正式路径)?")
		if cerr != nil {
			return fmt.Errorf("自愈确认失败:\n%s\n%w", hint, cerr)
		}
		if !ok {
			return fmt.Errorf("未执行自愈;请按上方提示手工恢复后重跑 update:\n%s", hint)
		}
		healed, herr := performHeal(self)
		if herr != nil {
			return fmt.Errorf("自愈失败,请手工恢复:\n%s\n%w", hint, herr)
		}
		fmt.Fprintf(out, "自愈完成: %s\n", healed)
		self = healed
	}

	// --- 3. service gates (both paths; spec §3.2 无条件探测新旧两名) ----------
	prNew := probeService(buildinfo.ServeServiceName)
	switch prNew.State {
	case updater.ProbeMechanismErr:
		// 勘误(spec §3.2 末):MechanismErr 一律 fail-closed 中止,无跳过分支。
		return fmt.Errorf("服务 %s 探测机制错误,fail-closed 中止(无法判定存在性): %s",
			buildinfo.ServeServiceName, prNew.Desc)
	case updater.ProbeInstalled:
		reg, rerr := registeredBinaryPath(buildinfo.ServeServiceName)
		if rerr != nil {
			return fmt.Errorf("服务 %s 已注册但读回二进制路径失败(中止;fail-closed): %w",
				buildinfo.ServeServiceName, rerr)
		}
		if !updater.SameBinaryPath(reg, self) {
			// spec §3.2:旧名存在(任何态,无论新名状态)优先出迁移块——比
			// "路径不一致"更有行动力的消息;两态都是 fail-closed 中止。
			if probeService(updater.LegacyServiceName).State == updater.ProbeInstalled {
				fmt.Fprint(out, updater.MigrationBlock())
				return fmt.Errorf("检测到旧版服务 %s 仍注册(任何态)——本次 update 中止", updater.LegacyServiceName)
			}
			fmt.Fprintf(out, "服务注册路径: %s\n本程序路径:   %s\n", reg, self)
			return fmt.Errorf("服务 %s 注册路径与本程序不一致(中止;防「更新 A 路径、服务跑 B 路径」静默旧版)",
				buildinfo.ServeServiceName)
		}
	}
	prOld := probeService(updater.LegacyServiceName)
	switch prOld.State {
	case updater.ProbeMechanismErr:
		return fmt.Errorf("旧服务 %s 探测机制错误,fail-closed 中止(无法判定存在性): %s",
			updater.LegacyServiceName, prOld.Desc)
	case updater.ProbeInstalled:
		// 旧名存在(任何态,无论新名状态)→ 迁移块 + 中止(不半更新)。
		fmt.Fprint(out, updater.MigrationBlock())
		return fmt.Errorf("检测到旧版服务 %s 仍注册(任何态)——本次 update 中止", updater.LegacyServiceName)
	}

	// --- 4. version discovery + comparison ------------------------------------
	curStr := currentVersionStr()
	curVer, curErr := updater.ParseVersion(curStr)

	var (
		rel        *updater.Release // nil on the --file path
		targetVer  string           // "v…" tag, or the staged output (--file)
		assetLabel string
		downgrade  bool
	)

	if o.file == "" {
		var derr error
		if o.wantTag != "" {
			rel, derr = updater.ReleaseByTag(ctx, o.wantTag)
		} else {
			rel, derr = updater.LatestRelease(ctx)
		}
		if derr != nil {
			return derr
		}
		targetVer = rel.Tag
		assetLabel = rel.AssetName

		if curErr != nil {
			if o.wantTag == "" {
				// dev / 本地构建:拒绝自动比较,要求显式 --version(spec §4.1)。
				return fmt.Errorf("无法解析当前版本 %q(本地构建?):自动比较已拒绝;请用 --version 显式指定目标版本", curStr)
			}
			fmt.Fprintf(out, "警告: 无法判定升降级(当前版本 %q 无法解析);将按指定目标 %s 继续\n", curStr, rel.Tag)
		} else {
			tgt, terr := updater.ParseVersion(rel.Tag)
			if terr != nil {
				return fmt.Errorf("目标 tag %s 不是可比较的版本: %w", rel.Tag, terr)
			}
			switch updater.CompareVersions(tgt, curVer) {
			case 0:
				if o.wantTag != "" {
					return fmt.Errorf("已安装该版本(%s);如需重装请先卸载或选择其他版本", rel.Tag)
				}
				fmt.Fprintf(out, "已是最新(%s);无事可做\n", rel.Tag)
				return nil
			case -1:
				downgrade = true
			}
		}

		if o.check {
			// 干跑:只报 当前/最新/资产名/update base,零下载零替换。
			which := "最新版本"
			if o.wantTag != "" {
				which = "目标版本"
			}
			fmt.Fprintf(out, "当前版本: %s\n%s: %s\n资产: %s\n干跑:未下载、未替换、未重启任何东西\n",
				curStr, which, rel.Tag, rel.AssetName)
			return nil
		}
		fmt.Fprintf(out, "版本: %s → %s\n资产: %s\n", curStr, rel.Tag, rel.AssetName)
	} else {
		assetLabel = "--file " + o.file
		if curErr != nil {
			fmt.Fprintf(out, "警告: 无法判定升降级(当前版本 %q 无法解析);目标版本以 staged 输出为准\n", curStr)
		}
	}

	// --- 5. download (--file: none) → extract → staged self-check --------------
	tmpdir, err := os.MkdirTemp(filepath.Dir(self), ".sshmgr-update-tmp-*")
	if err != nil {
		// exe 目录不可写的确定性出错点(spec §4.3:明确报错提示 sudo/管理员,
		// 不自动提权——给一条有出路的指引)。
		return fmt.Errorf("在 exe 同目录创建临时目录(同卷保证 rename 原子): %w\n"+
			"该目录不可写——用管理员/sudo 运行,或将二进制移至用户可写目录;update 不会自动提权", err)
	}
	defer os.RemoveAll(tmpdir) // 零残留:staged 成功时已被 rename 走,失败时整目录清理

	var (
		archivePath string
		shaLabel    string
	)
	if o.file == "" {
		// checksums.txt 自举(空 hash 仅限它),再以解析出的 hash 下资产(T4 硬约束)。
		sumPath, derr := updater.DownloadAsset(ctx, rel.ChecksumsURL, "", tmpdir)
		if derr != nil {
			return derr
		}
		data, rerr := os.ReadFile(sumPath)
		if rerr != nil {
			return fmt.Errorf("读取 %s: %w", filepath.Base(sumPath), rerr)
		}
		want, perr := updater.ParseChecksums(data, rel.AssetName)
		if perr != nil {
			return perr
		}
		archivePath, derr = updater.DownloadAsset(ctx, rel.AssetURL, want, tmpdir)
		if derr != nil {
			return derr
		}
		shaLabel = "SHA256: 命中 " + want
	} else {
		// --sha256 在复制前就核对(尽早失败);--no-verify 打印未校验警告留档。
		if o.sha256Hex != "" {
			got, herr := fileSHA256(o.file)
			if herr != nil {
				return herr
			}
			if got != strings.ToLower(o.sha256Hex) {
				return fmt.Errorf("--file sha256 不匹配: got %s want %s(目标文件零触碰)", got, o.sha256Hex)
			}
			shaLabel = "SHA256: 命中(--sha256) " + strings.ToLower(o.sha256Hex)
		} else {
			fmt.Fprintln(out, "警告: --file 未做任何完整性校验(--no-verify;owner 手供文件,信任=你下载的它)")
			shaLabel = "SHA256: 未校验(--no-verify)"
		}
		// 复制进 exe 同目录的 tmpdir:ExtractBinary 把 staged 落在归档旁边,
		// 不能在用户的包目录里写文件。
		archivePath = filepath.Join(tmpdir, filepath.Base(o.file))
		if cerr := copyFile(o.file, archivePath, 0o755); cerr != nil {
			return fmt.Errorf("复制本地包到临时目录: %w", cerr)
		}
	}

	staged, xerr := updater.ExtractBinary(archivePath, runtime.GOOS)
	if xerr != nil {
		return xerr
	}

	stagedReport := ""
	if o.file == "" {
		got, serr := updater.StagedVersionCheck(staged, targetVer)
		if serr != nil {
			return fmt.Errorf("staged 自检未通过(保留原文件,零变更): %w", serr)
		}
		stagedReport = got
	} else {
		// --file 路径:staged 输出本身即目标版本,没有先行可比的 want。
		// 以 want="" 探测——StagedVersionCheck 契约保证:got=="" ⟺ staged
		// 没能回答(无法执行/超时/超限/空输出),此时中止;got!="" 时的
		// mismatch 错误是本路径的设计产物(目标在它回答之前不存在),取 got。
		got, serr := updater.StagedVersionCheck(staged, "")
		if got == "" {
			return fmt.Errorf("staged 自检未通过(保留原文件,零变更): %w", serr)
		}
		targetVer = got
		stagedReport = got

		// "已装该版本"拒绝与降级判定以 staged 输出为基准(spec §4.2(5))。
		if targetVer == curStr {
			return fmt.Errorf("已装该版本(%s);--file 提供的包与当前版本相同", targetVer)
		}
		if curErr == nil {
			if fv, ferr := updater.ParseVersion(targetVer); ferr == nil {
				switch updater.CompareVersions(fv, curVer) {
				case 0:
					return fmt.Errorf("已装该版本(%s);--file 提供的包与当前版本相同", targetVer)
				case -1:
					downgrade = true
				}
			} else {
				fmt.Fprintf(out, "警告: 无法判定升降级(staged 输出 %q 不是版本号);按 owner 指定继续\n", targetVer)
			}
		}
		fmt.Fprintf(out, "版本: %s → %s(staged)\n资产: %s\n", curStr, targetVer, assetLabel)
	}
	fmt.Fprintf(out, "%s\nstaged 自检: OK (%s)\n", shaLabel, stagedReport)

	// --- 6+7. confirm + downgrade line -----------------------------------------
	if downgrade {
		// 降级 = 回滚通道:即使 --yes 也打印醒目行(spec §4.4)。
		fmt.Fprintf(out, "⚠ 降级至 %s(回滚通道)\n", targetVer)
	}
	if !o.yes {
		action := "更新"
		if downgrade {
			action = "降级"
		}
		prompt := fmt.Sprintf("将%s sshmgr:%s → %s(资产 %s)。继续?[Y/n]", action, curStr, targetVer, assetLabel)
		ok, cerr := askConfirm(errw, prompt)
		if cerr != nil {
			return cerr
		}
		if !ok {
			fmt.Fprintln(out, "已取消(零变更)")
			return nil
		}
	}

	// --- 8. transactional replace ----------------------------------------------
	if rerr := updater.ReplaceBinary(staged, self); rerr != nil {
		var cwe *updater.CommittedWithError
		if errors.As(rerr, &cwe) {
			// committed-with-error:替换已生效,不回滚不掩饰,如实报告后继续。
			fmt.Fprintf(out, "警告(已提交): %v\n", cwe)
		} else {
			// 含 ErrRollbackFailed(其消息自带平台手工恢复命令)与零变更失败。
			return fmt.Errorf("替换失败: %w", rerr)
		}
	}
	fmt.Fprintf(out, "替换: %s\n", self)

	// --- 9. service branch -------------------------------------------------------
	if prNew.State != updater.ProbeInstalled {
		fmt.Fprintln(out, "未安装服务(client 姿态):新版本下次 agent 会话生效;运行中的桥继续旧版")
		return nil
	}

	svc, serr := serviceNew(&program{}, &service.Config{
		Name:   buildinfo.ServeServiceName,
		Option: platformServiceOptions(),
	})
	if serr != nil {
		return restartPending(out, fmt.Errorf("构造服务句柄失败: %w", serr))
	}
	st, sterr := svc.Status()
	if sterr == nil && st == service.StatusStopped {
		fmt.Fprintf(out, "服务 %s 已停止:新二进制将在下次启动时生效。手动启动: %s\n",
			buildinfo.ServeServiceName, manualServiceCommand("start"))
		return nil
	}
	fmt.Fprintf(out, "⚠ 重启将断开活动隧道;进行中的配对请求作废(密钥态在服务内存,既有「重启作废」语义)\n")
	if !o.yes {
		ok, cerr := askConfirm(errw, fmt.Sprintf("重启服务 %s 立即生效?[Y/n]", buildinfo.ServeServiceName))
		// 确认点已过替换:任何"未能重启"(确认报错/拒绝)都是
		// 替换成功+重启待手工 = 退出码 3,并打印平台手工命令。
		if cerr != nil {
			return restartPending(out, fmt.Errorf("重启确认失败: %w", cerr))
		}
		if !ok {
			return restartPending(out, errors.New("已选择暂不重启"))
		}
	}
	if rerr := svc.Restart(); rerr != nil {
		return restartPending(out, rerr)
	}
	fmt.Fprintf(out, "重启: OK (%s)\n", buildinfo.ServeServiceName)
	healthy := serveHTTPProbe(defaultProbeAddr)
	if healthy {
		fmt.Fprintf(out, "健康回探(%s): responding — 服务已回活\n", defaultProbeAddr)
	} else {
		fmt.Fprintf(out, "健康回探(%s): not responding(若 serve 使用非默认 --addr,此探测可能误报;可稍后用 serve status 复核)\n", defaultProbeAddr)
	}
	return nil
}

// askConfirm asks a [Y/n] question: default yes on empty/affirmative input.
// The non-TTY rule (spec §4.4) fires only here — the confirm nodes — never
// on --check or the already-latest exit-0 path.
func askConfirm(errw io.Writer, prompt string) (bool, error) {
	if !stdinIsTTY() {
		return false, errors.New("非交互终端(非 TTY)且未提供 --yes:无法确认;请加 --yes 重跑")
	}
	fmt.Fprintf(errw, "%s ", prompt)
	line, err := readConfirmLine()
	if err != nil {
		return false, fmt.Errorf("读取确认输入: %w", err)
	}
	switch strings.ToLower(line) {
	case "", "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// restartPending reports the exit-code-3 state: the binary replacement
// succeeded but the service restart is pending manual action. The manual
// command is printed for the owner (and the message carries it for scripts).
func restartPending(out io.Writer, cause error) error {
	cmd := manualServiceCommand("restart")
	fmt.Fprintf(out, "替换已成功,但服务重启未完成(待手工): %v\n手工命令: %s\n", cause, cmd)
	return NewExitCodeError(3, fmt.Errorf("替换成功/重启待手工: %w(手工命令: %s)", cause, cmd))
}

// manualServiceCommand renders the platform-specific manual service command
// (spec §4.4 pins Windows `sc stop X && sc start X` — sc has no restart —
// and Linux `systemctl restart`).
func manualServiceCommand(op string) string {
	name := buildinfo.ServeServiceName
	switch runtime.GOOS {
	case "windows":
		if op == "restart" {
			return "sc stop " + name + " && sc start " + name + "(管理员;或 Win11 sudo sc …)"
		}
		return "sc " + op + " " + name + "(管理员;或 Win11 sudo sc …)"
	case "darwin":
		if op == "restart" {
			return "sudo launchctl kickstart -k system/" + name
		}
		return "sudo launchctl bootstrap system/" + name
	default:
		return "sudo systemctl " + op + " " + name
	}
}

// fileSHA256 returns the lowercase hex sha256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("打开 --file %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("计算 --file %s 的 sha256: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// --- crash-window self-heal action -------------------------------------------

// oldGenSep is the generational backup suffix updater's replace produces:
// <self>.old.<unixts>. Mirrored here (the updater keeps it unexported) purely
// so the CONFIRMED heal can act; DetectHeal remains the authoritative gate.
const oldGenSep = ".old."

// performHeal executes the confirmed recovery (spec §4.3): the running image
// (entry: the executable itself carries the .old.<ts> suffix) or the newest
// self+".old.<ts>" generation is renamed back onto the canonical path, and
// the canonical path becomes the self the rest of the update flow replaces.
func performHeal(self string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if stem, _, isGen := splitOldGenerationName(filepath.Base(exe)); isGen {
		canonical := filepath.Join(filepath.Dir(exe), stem)
		if merr := os.Rename(exe, canonical); merr != nil {
			return "", fmt.Errorf("rename %s -> %s: %w", exe, canonical, merr)
		}
		return canonical, nil
	}
	backup, ok := newestOldGeneration(self)
	if !ok {
		return "", fmt.Errorf("未找到可恢复的 %s.old.<ts> 备份", self)
	}
	if merr := os.Rename(backup, self); merr != nil {
		return "", fmt.Errorf("rename %s -> %s: %w", backup, self, merr)
	}
	return self, nil
}

// splitOldGenerationName mirrors updater's generational naming parse:
// "<stem>.old.<digits>" → (stem, ts, true); everything else is not a
// generation (non-digit or overflowing suffixes included).
func splitOldGenerationName(name string) (stem string, ts int64, ok bool) {
	i := strings.LastIndex(name, oldGenSep)
	if i <= 0 {
		return "", 0, false
	}
	digits := name[i+len(oldGenSep):]
	if digits == "" {
		return "", 0, false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return "", 0, false
		}
	}
	ts, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return name[:i], ts, true
}

// newestOldGeneration returns the freshest self+".old.<ts>" sibling (newest
// timestamp wins — it holds the most recent complete image).
func newestOldGeneration(self string) (string, bool) {
	dir, base := filepath.Dir(self), filepath.Base(self)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	best, bestTS := "", int64(-1)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), base) {
			continue
		}
		stem, ts, ok := splitOldGenerationName(e.Name())
		if !ok || stem != base || ts <= bestTS {
			continue
		}
		best, bestTS = filepath.Join(dir, e.Name()), ts
	}
	return best, best != ""
}
