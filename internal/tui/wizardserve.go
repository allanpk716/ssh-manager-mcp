// wizardserve.go holds the SERVER-role wizard pieces (Plan 19 T4, spec §2.4):
// the LAN-address picker, the serve install step + post-install probe, the
// result banners, and the client pair card — plus the serve-install hook
// that bridges tui to cli's install core without an import cycle. The flow
// composition itself (shared steps → serve segment) lives in wizard.go;
// everything here is an independently testable constructor or pure command.
package tui

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
)

// serveInstall is the programmatic entry into cli's installServeService,
// injected by the cobra `tui` command via SetServeInstaller. tui cannot import
// internal/cli (cli imports tui for that command — import cycle), so the
// wizard reaches the install core through this hook. A nil hook means "not
// wired" (only reachable outside the real CLI); installServeStep then fails
// with a clear error instead of silently skipping the service registration.
var serveInstall func(addr, tlsCert, tlsKey string, out io.Writer) error

// SetServeInstaller wires cli's installServeService core into the wizard.
// Called by cli.newTUICmd immediately before tui.Run.
func SetServeInstaller(fn func(addr, tlsCert, tlsKey string, out io.Writer) error) {
	serveInstall = fn
}

// ---------------------------------------------------------------------------
// device-code issuance message (standalone→server upgrade segment)
// ---------------------------------------------------------------------------

// deviceCodeIssuedMsg carries the one-time device code + the serve cert's SPKI
// fingerprint from App.issueDeviceCode (upgrade.go) into the one-time screen.
// Plan 42 批1 T8: the SERVER WIZARD no longer mints device codes (pair mints
// them at approval) — the upgrade segment's on-demand mint is the one remaining
// producer, feeding the manual/CI path. The plaintext code transits this
// message ONCE and then lives only inside the overlay; the fingerprint is NOT
// secret and is also kept in the segment for the pair card.
type deviceCodeIssuedMsg struct {
	code        string
	fingerprint string
}

// ---------------------------------------------------------------------------
// ⑥a LAN address picker
// ---------------------------------------------------------------------------

// lanAddrOptions turns the host's non-loopback IPv4s into huh options whose
// VALUE and DISPLAY string are both "https://<ip>:7878" — the exact string the
// access card later shows (value=display discipline: no re-derivation between
// what was selected and what the client is told). IPv6 and loopback are
// filtered (a server wizard address must be reachable from the LAN). The
// returned default is the first option's value (spec §2.4 ⑥: 默认第一项).
func lanAddrOptions(ips []net.IP) (opts []huh.Option[string], def string) {
	for _, ip := range ips {
		if ip == nil || ip.To4() == nil || ip.IsLoopback() {
			continue
		}
		s := "https://" + ip.String() + ":7878"
		opts = append(opts, huh.NewOption(s, s))
	}
	if len(opts) > 0 {
		def = opts[0].Value
	}
	return opts, def
}

// wizAddrForm asks which LAN address clients will use to reach this server.
// With IPv4s present it is a select (chosen pre-set to the first option —
// Enter accepts it); with none (unusual host / VPN-only setups) it degrades to
// a manual input validated to be an https:// URL (serve is TLS-only, so an
// http:// address would be wrong on its face).
func wizAddrForm(ips []net.IP, chosen *string) *huh.Form {
	opts, def := lanAddrOptions(ips)
	if len(opts) == 0 {
		return huh.NewForm(huh.NewGroup(
			huh.NewInput().Title("serve 对外地址（https://<局域网IP>:7878，client 将连它）").
				Placeholder("https://192.168.1.10:7878").
				Value(chosen).Validate(func(s string) error {
				if err := nonEmpty(s); err != nil {
					return err
				}
				if !strings.HasPrefix(strings.TrimSpace(s), "https://") {
					return errors.New("必须以 https:// 开头（serve 只说 TLS）")
				}
				return nil
			}),
		))
	}
	*chosen = def
	return huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().
			Title("client 通过哪个地址访问这台 server？（选定地址会写进客户端接入卡）").
			Options(opts...).Value(chosen),
	))
}

// ---------------------------------------------------------------------------
// ⑥b serve install + probe
// ---------------------------------------------------------------------------

// serveInstalledMsg carries the outcome of the service registration. err != nil
// does NOT block the flow (spec §2.4 ⑥: 失败不阻断) — the result screen shows
// the error + the manual elevated command and the flow proceeds to the probe.
type serveInstalledMsg struct{ err error }

// installServeStep registers the serve service. BINDING (plan constraint): the
// wizard ALWAYS binds 0.0.0.0:7878 — 严禁 127.0.0.1 默认 on the wizard path,
// because a loopback bind would silently defeat the entire server role; the
// chosen addr is display-only (it feeds the probe + access card). Install
// chatter goes to io.Discard: the failure surface is the error + manualInstallCmd
// rendered on the result screen, not raw install output.
func installServeStep(addr string) tea.Cmd {
	_ = addr // display-only; the registration binds 0.0.0.0:7878 (see above)
	return func() tea.Msg {
		if serveInstall == nil {
			return serveInstalledMsg{err: errors.New("serve 安装核心未接线（应通过 `sshmgr tui` 启动向导）")}
		}
		return serveInstalledMsg{err: serveInstall("0.0.0.0:7878", "", "", io.Discard)}
	}
}

// serveProbeMsg is the post-install liveness verdict: ok = the listener is up
// AND speaking TLS (ANY HTTP status counts — 401/400 are serve answering);
// !ok carries the error detail for the yellow troubleshooting banner.
type serveProbeMsg struct {
	ok     bool
	detail string
}

// probeServe checks "listening + speaking TLS" on the chosen address with a
// 3-second budget: a TLS GET to <addr>/snapshot, cert verification skipped —
// the pin question is a client-side concern; this probe only verifies that
// something answered HTTPS at all (spec §2.4 ⑥ 探活).
func probeServe(addr string) tea.Cmd {
	return func() tea.Msg {
		client := &http.Client{
			Timeout:   3 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // probe-only client: pin checks are a client concern
		}
		resp, err := client.Get(strings.TrimRight(strings.TrimSpace(addr), "/") + "/snapshot")
		if err != nil {
			return serveProbeMsg{ok: false, detail: err.Error()}
		}
		defer resp.Body.Close()
		return serveProbeMsg{ok: true, detail: resp.Status}
	}
}

// manualInstallCmd is the raw, copy-pasteable elevated command shown when the
// in-wizard install fails (spec §2.4 ⑥: 失败给出可执行的原文命令，含提权方式).
func manualInstallCmd() string {
	if runtime.GOOS == "windows" {
		return "sshmgr serve install --addr 0.0.0.0:7878\n（在管理员终端中执行）"
	}
	return "sudo sshmgr serve install --addr 0.0.0.0:7878"
}

// serveAdminNotice is the pre-install screen (spec §2.4 ⑥ admin 前置提示):
// the operator is told BEFORE the attempt that registration needs elevated
// privileges, and that a failure will not trap them here.
func serveAdminNotice() overlay {
	body := strings.Join([]string{
		"接下来把 serve 注册为系统服务：",
		"  Windows → Windows 服务（开机自启、失败自动重启）",
		"  Linux   → systemd unit      /  macOS → launchd",
		"服务绑定 0.0.0.0:7878，局域网内 client 机可访问。",
		"",
		"安装通常需要管理员 / root 权限。",
		"若失败：向导不会阻断 —— 会显示可手动提权执行的原文命令，",
		"并继续验证服务状态。",
		"",
		"按任意键开始安装",
	}, "\n")
	return &wizStaticView{title: "安装 serve 服务（需要管理员权限）", body: body}
}

// serveResultScreen shows BOTH verdicts of the serve segment — the install
// outcome (red banner + manual command on failure, green on success) and the
// probe outcome (green 已就绪 / yellow 未验证 with troubleshooting hints). A
// failure on either line is deliberately NON-blocking: the flow continues to
// the access card (the operator can fix the service later with the shown
// command; `serve status` gives the four-signal diagnosis).
func serveResultScreen(installErr error, probe serveProbeMsg) overlay {
	var b strings.Builder
	if installErr != nil {
		b.WriteString(errStyle.Render("✗ serve 服务安装失败："+installErr.Error()) + "\n")
		b.WriteString("不阻断 —— 手动安装（原文命令）：\n  " + manualInstallCmd() + "\n\n")
	} else {
		b.WriteString(selStyle.Render("✓ serve 服务已安装并启动") + "\n\n")
	}
	if probe.ok {
		b.WriteString(selStyle.Render("✓ serve 已就绪（"+probe.detail+"）") + "\n")
	} else {
		b.WriteString(warnStyle.Render("⚠ serve 未验证，client 可能连不上（"+probe.detail+"）") + "\n")
		b.WriteString("排查：7878 端口防火墙是否放行；`sshmgr serve status` 查四项信号；服务可能仍在启动，稍候重试。\n")
	}
	b.WriteString("\n按任意键查看 client 入网卡\n")
	return &wizStaticView{title: "serve 安装结果", body: b.String()}
}

// ---------------------------------------------------------------------------
// ⑦ client pair card
// ---------------------------------------------------------------------------

// clientPairCard is the flow's closing card (Plan 42 批1 T8 — the old 双密钥
// 接入卡's replacement): the REAL chosen address + the server fingerprint
// (pin), and the ONE guided onboarding command for a new machine —
// `sshmgr pair`, whose device code / project token the wizard no longer
// pre-mints (they are minted at approval, spec §3.3-6). The manual
// cache-pull path stays documented for CI/automation; a needed device code is
// issued from the 设备码页/CLI on demand.
func clientPairCard(addr, fp string) overlay {
	body := strings.Join([]string{
		"把下面这张卡带到 client 机：",
		"",
		"地址    " + addr,
		"指纹    " + fp,
		"",
		"新机入网（pair 为新机唯一入网路径）：",
		"  sshmgr pair --instance <本机实例名> --url " + addr + " --pin " + fp,
		"  （也可省略 --url/--pin，用 LAN 发现自动找到本 serve）",
		"",
		"在该机上批准配对（其 TUI Pairing 页 / serve pair approve）并对照双方",
		"屏幕的 SAS 码后，设备码、project token 与缓存自动落到 client 机。",
		"",
		"手工路径（CI/自动化，文档化保留）：",
		"  设备码 → 主控台 设备码页 [a]（或 cache-tokens add），然后",
		fmt.Sprintf("  sshmgr cache pull --url %s --token '<设备码>:%s'", addr, fp),
		"",
		"project token 丢失重发：主控台 Projects 页 [a]",
		"",
		"按任意键完成设置",
	}, "\n")
	return &wizStaticView{title: "client 入网卡", body: body}
}
