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
//	 4. role decision + version discovery (--version → ByTag; client
//	    default → cached recommended version; otherwise LatestRelease);
//	    already-latest → exit 0; dev current version handling
//	 5. download checksums+asset (SHA256) → ExtractBinary → StagedVersionCheck
//	    (--file: skip discovery/download; staged output IS the target version)
//	 6. confirm (current → target; non-TTY without --yes errors HERE only)
//	 7. downgrade verdict (target < current): prominent line even with --yes
//	 8. ReplaceBinary (fsync folded inside; rollback/double-fault surface)
//	 9. generation signal: write sshmgr.update-gen next to the replaced
//	    binary (bridge hot-upgrade decision 1 — both the GitHub and --file
//	    paths converge here; failure warns, never fails the committed update)
//	10. service branch: running → warn+confirm+Restart (exit 3 on failure);
//	    stopped → start command only; not installed → "next agent session"
//	11. evidence lines interleaved: base / version pair / asset / SHA256 /
//	    staged check / replace path / generation signal / restart + health probe

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/kardianos/service"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/updater"
)

// defaultProbeAddr is the health-probe FALLBACK target for the post-restart
// check — used only when the serve registration's --addr could not be read
// back (updater.RegisteredServeAddr is the primary source; a serve bound to
// 0.0.0.0 answers on loopback too). The probe is evidence, not a verdict.
const defaultProbeAddr = "127.0.0.1:7878"

// loopbackProbeAddr rewrites a registered bind host that cannot be dialed
// from this machine ("0.0.0.0", "::", or empty) to loopback, keeping the
// port — a wildcard bind answers on 127.0.0.1 too. A concrete host (e.g.
// the authoritative broker's LAN address) is returned as-is: dialing the
// real registered address is the honest probe. An unparsable value is
// returned unchanged — the probe then reports not-responding, which is the
// correct evidence for a broken registration.
func loopbackProbeAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

// updateBaseEnv is the env seam name (spec §4.6). Referenced (not re-parsed)
// to decide the prominence marker: a value explicitly set in the environment
// is exactly what "非默认" means, with no second parsing of the base URL —
// the default value lives only in updater.BaseURL() (no mirrored constant
// here to drift; see the evidence-line comment at the marker site below).
const updateBaseEnv = "SSHMGR_UPDATE_BASE"

// shaHexRe matches a user-supplied --sha256 digest (64 hex chars).
var shaHexRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// --- seams (spec §4.6: 生产路径必须留缝; tests swap these wholesale) --------

var (
	probeService         = updater.ProbeService
	registeredBinaryPath = updater.RegisteredBinaryPath
	registeredServeAddr  = updater.RegisteredServeAddr
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
	instance  string // --instance <name>
}

// updateTargetSource 表示不带 --file 时目标版本的来源；这个判定只做一次，
// 后续版本发现、比较与输出都复用同一结果。
type updateTargetSource int

const (
	targetFromSpecifiedVersion updateTargetSource = iota
	targetFromRecommendedVersion
	targetFromGitHubLatest
)

func (s updateTargetSource) String() string {
	switch s {
	case targetFromSpecifiedVersion:
		return "指定版本"
	case targetFromRecommendedVersion:
		return "推荐版本"
	default:
		return "GitHub 最新版本"
	}
}

func newUpdateCmd() *cobra.Command {
	var o updateOpts
	c := &cobra.Command{
		Use:   "update",
		Short: "Self-update sshmgr from GitHub Releases (or a local --file package)",
		Long: `Self-update the sshmgr binary in place, restarting the serve service if one is
registered.

  sshmgr update                          check → download → verify →
                                         staged self-check → confirm → replace → restart
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
	f.StringVar(&o.instance, "instance", "", "read the recommended version from this named cache instance (default slot when omitted)")
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
	if err := checkInstanceFlag(o.instance); err != nil {
		return err
	}
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

	// 证据行:update base(spec §4.5)。「非默认→醒目」的判定 = SSHMGR_UPDATE_BASE
	// env 在场,而非值比对——不对 base URL 做第二次解析、不镜像默认常量
	// (默认值只活在 updater.BaseURL() 一处);env 显式设置即"非默认"。
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

	// --- 4-9. 版本发现、下载、校验、试运行、确认、替换与代际信号 -----------------
	// 这条链已抽到运维模块的升级服务里；命令行只注入输出、确认、角色与版本
	// 依赖，HTTPS、SHA-256 校验与回滚语义与抽离前完全一致。
	updateService := ops.NewUpdateService(ops.UpdateDependencies{
		SelfPath:       func() (string, error) { return self, nil },
		CurrentVersion: currentVersionStr,
		ClientState: func(instance string) (ops.UpdateClientState, error) {
			state, readErr := readClientVersionState(instance, errw)
			if readErr != nil && state.IsClient && errors.Is(readErr, errAmbiguousClientCacheInstance) {
				return ops.UpdateClientState{IsClient: true}, &ops.InstanceAmbiguousError{Err: readErr}
			}
			recommended := ""
			if state.Snapshot != nil {
				recommended = state.Snapshot.RecommendedVersion
			}
			return ops.UpdateClientState{IsClient: state.IsClient, RecommendedVersion: recommended}, readErr
		},
		Audit: writeUpdateAudit,
	})
	var confirm func(string) (bool, error)
	if !o.yes {
		confirm = func(prompt string) (bool, error) { return askConfirm(errw, prompt) }
	}
	runResult, runErr := updateService.Run(ctx, ops.UpdateRunRequest{
		CheckOnly: o.check, WantTag: o.wantTag, File: o.file, SHA256Hex: o.sha256Hex,
		NoVerify: o.noVerify, Instance: o.instance, Source: ops.CLISource(),
		Output:  func(format string, args ...any) { fmt.Fprintf(out, format, args...) },
		Confirm: confirm,
	})
	if runErr != nil {
		return runErr
	}
	clientRole := runResult.ClientRole

	// --- 10. service branch -------------------------------------------------------
	if clientRole || prNew.State != updater.ProbeInstalled {
		fmt.Fprintln(out, "未安装服务(客户端):运行中的桥空闲时自动热升级(约 30 秒内;≤v0.19 的旧桥仍需下次 agent 会话或 /mcp 重连上车)")
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
	// --- post-restart health probe ---------------------------------------
	// Target: the --addr the service was REGISTERED with (read back from the
	// same per-platform records the binary-path pre-check uses — backlog #17:
	// a probe hardcoded to the default addr misreported "not responding" on
	// every non-default install). Wildcard binds probe on loopback. A read
	// failure or a missing --addr falls back to the install default — the
	// probe is evidence, not a verdict.
	probeTarget := defaultProbeAddr
	switch addr, aerr := registeredServeAddr(buildinfo.ServeServiceName); {
	case aerr != nil:
		// The read failed on a machine whose service JUST restarted — the
		// registration record is anomalous (externally edited / foreign
		// writer). Say so: without this line the operator reads "not
		// responding" as a slow TLS listener and chases the wrong cause
		// (review M1 — the probe target must not lie, and neither may the
		// read failure).
		fmt.Fprintf(out, "警告: 读取注册 --addr 失败(%v);健康回探回退默认目标\n", aerr)
	case addr != "":
		probeTarget = loopbackProbeAddr(addr)
	}
	healthy := serveHTTPProbe(probeTarget)
	if healthy {
		fmt.Fprintf(out, "健康回探(%s): responding — 服务已回活\n", probeTarget)
	} else {
		fmt.Fprintf(out, "健康回探(%s): not responding(刚重启时 TLS 监听可能需要一两秒;可稍后用 serve status 复核)\n", probeTarget)
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
// for the running GOOS (spec §4.4 pins Windows `sc stop X && sc start X` — sc
// has no restart — and Linux `systemctl restart`).
func manualServiceCommand(op string) string {
	return manualServiceCommandFor(runtime.GOOS, op)
}

// manualServiceCommandFor is the pure per-GOOS renderer, split from
// manualServiceCommand so the darwin/linux forms are assertable on any
// platform (the launchd/systemctl shapes otherwise hide behind runtime.GOOS
// in tests).
func manualServiceCommandFor(goos, op string) string {
	name := buildinfo.ServeServiceName
	switch goos {
	case "windows":
		if op == "restart" {
			return "sc stop " + name + " && sc start " + name + "(管理员;或 Win11 sudo sc …)"
		}
		return "sc " + op + " " + name + "(管理员;或 Win11 sudo sc …)"
	case "darwin":
		// start 也用 kickstart:bootstrap 需要显式 plist 路径,照抄不可执行;
		// kickstart(无 -k)对已注册未运行的服务即"启动",与 restart 的
		// kickstart -k(杀掉重启)区分。
		if op == "restart" {
			return "sudo launchctl kickstart -k system/" + name
		}
		return "sudo launchctl kickstart system/" + name
	default:
		return "sudo systemctl " + op + " " + name
	}
}

// writeUpdateAudit 把升级动作写进本机 vault 的审计表；写失败只返回错误，由升
// 级服务以警告形式报告，不影响已经提交的替换结果。
func writeUpdateAudit(source ops.Source, entry ops.UpdateAuditEntry) error {
	st, err := openUnlockedStore()
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.WriteAudit(updateAuditRow(source, entry)); err != nil {
		return err
	}
	return nil
}

// --- crash-window self-heal action -------------------------------------------

// performHeal executes the confirmed recovery (spec §4.3): the running image
// (entry: the executable itself carries the .old.<ts> suffix) or the newest
// self+".old.<ts>" generation is renamed back onto the canonical path, and
// the canonical path becomes the self the rest of the update flow replaces.
// The generational naming is updater's single source of truth
// (SplitOldGeneration / OldGenerations); DetectHeal remains the
// authoritative gate.
func performHeal(self string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if stem, _, isGen := updater.SplitOldGeneration(filepath.Base(exe)); isGen {
		canonical := filepath.Join(filepath.Dir(exe), stem)
		if merr := os.Rename(exe, canonical); merr != nil {
			return "", fmt.Errorf("rename %s -> %s: %w", exe, canonical, merr)
		}
		return canonical, nil
	}
	gens, gerr := updater.OldGenerations(self)
	if gerr != nil || len(gens) == 0 {
		return "", fmt.Errorf("未找到可恢复的 %s.old.<ts> 备份", self)
	}
	backup := gens[len(gens)-1].Path // oldest→newest sorted; freshest image wins
	if merr := os.Rename(backup, self); merr != nil {
		return "", fmt.Errorf("rename %s -> %s: %w", backup, self, merr)
	}
	return self, nil
}
