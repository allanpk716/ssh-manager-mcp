// Package updater — this file is the service-awareness layer of the
// self-update pipeline (spec §3.2 三分法 + §4.4 路径预检): before the update
// touches the binary it must decide whether it is SAFE to act at all —
// whether a serve service is registered (old name or new), where the
// registered binary actually points, and what to print when the one-time
// v0.13.0 rename migration has not happened yet.
//
// Everything here is fail-closed by design: when existence cannot be
// determined (mechanism error) the caller aborts, because the pre-check
// matters most exactly in the unknown environments it cannot classify.
package updater

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kardianos/service"

	"ssh-manager-mcp/internal/buildinfo"
)

// LegacyServiceName is the exported pre-Plan-44 registered service name so
// the T8 update flow (cli package) can probe the legacy service without
// re-hardcoding the literal as a second source of truth. It is a LITERAL on
// purpose: after T1 renamed the cli constant to buildinfo.ServeServiceName
// this string has zero other occurrences in the codebase — probing for it is
// the expected behavior, because a host that still runs the pre-rename
// service is exactly what the migration gate must detect (spec §3.2: 旧名存在
// (任何态)→ 打印迁移块并中止).
const LegacyServiceName = "ssh-manager-serve"

// DescNoServiceSystem is embedded in ProbeResult.Desc when probing hit
// kardianos ErrNoServiceSystemDetected (containers/CI with no service
// manager). The probe itself reports MechanismErr — it genuinely cannot
// classify existence — and the CLI decides what spec §3.2 prescribes for this
// cause: skip service probing and update anyway. Matching on this marker
// keeps the skip decision a string check on the pinned ProbeResult shape.
const DescNoServiceSystem = "no service system detected"

// ProbeState is the trichotomy verdict of a service probe (spec §3.2):
//
//	ProbeNotInstalled — kardianos answered ErrNotInstalled: safe to proceed.
//	ProbeInstalled    — the service exists in ANY state (running/stopped/
//	                    failed/unknown); a failed state is precisely the
//	                    crash-loop that most needs an update, never block on it.
//	ProbeMechanismErr — existence could NOT be determined (fail-closed).
type ProbeState int

const (
	ProbeNotInstalled ProbeState = iota
	ProbeInstalled
	ProbeMechanismErr
)

// ProbeResult is one service probe: the trichotomy verdict plus a
// human-readable description (an install-state label for the installed
// cases, the underlying error text otherwise).
type ProbeResult struct {
	State ProbeState
	Desc  string
}

// statusProber is the minimal surface ProbeService consumes from a kardianos
// service. kardianos's own service.Service is a CONCRETE struct, not an
// interface — a seam typed exactly `var serviceNew = service.New` could never
// be faked in tests — so the seam below widens the return type to this
// interface while keeping the seam's name and constructor role (spec §4.6:
// 服务检测经 service.New 函数变量, 测旧名/双服务/failed 态/无管理器分支).
type statusProber interface {
	Status() (service.Status, error)
}

// serviceNew is the service-construction seam. The default wraps the real
// kardianos service.New verbatim, preserving the error identity
// (ErrNoServiceSystemDetected / ErrNotInstalled flow through errors.Is).
var serviceNew = func(i service.Interface, c *service.Config) (statusProber, error) {
	svc, err := service.New(i, c)
	if err != nil {
		return nil, err
	}
	return svc, nil
}

// nopService satisfies kardianos service.Interface so ProbeService can build
// a service value. ProbeService only ever calls Status(); Start/Stop are
// never invoked on this throwaway instance.
type nopService struct{}

func (nopService) Start(service.Service) error { return nil }
func (nopService) Stop(service.Service) error  { return nil }

// ProbeService probes a registered service by name through kardianos and
// classifies the answer into the spec §3.2 trichotomy:
//
//   - ErrNotInstalled → ProbeNotInstalled (放行).
//   - ANY answerable status → ProbeInstalled, including the systemd FAILED
//     state. On Linux systemd, `systemctl is-active` answers "failed" for a
//     unit whose last run failed, and kardianos v1.3.0
//     (service_systemd_linux.go:287) surfaces that as the error
//     "service in failed state" — the unit demonstrably EXISTS, and a
//     crash-looping service is the one that most needs updating, so the
//     failed-state error is classified Installed, never MechanismErr.
//   - No service system at all (ErrNoServiceSystemDetected — containers/CI)
//     → ProbeMechanismErr with DescNoServiceSystem embedded in Desc: the
//     existence question is unanswerable here, and the CLI owns the spec's
//     "skip probing" decision for this specific cause.
//   - Anything else → ProbeMechanismErr (fail-closed; Desc carries the error).
func ProbeService(name string) ProbeResult {
	svc, err := serviceNew(nopService{}, &service.Config{Name: name})
	if err != nil {
		if errors.Is(err, service.ErrNoServiceSystemDetected) {
			// Desc carries only the marker: kardianos's own error text
			// ("No service system detected.") would duplicate it.
			return ProbeResult{State: ProbeMechanismErr, Desc: DescNoServiceSystem}
		}
		return ProbeResult{State: ProbeMechanismErr, Desc: err.Error()}
	}

	status, err := svc.Status()
	switch {
	case err == nil:
		return ProbeResult{State: ProbeInstalled, Desc: statusDesc(status)}
	case errors.Is(err, service.ErrNotInstalled):
		return ProbeResult{State: ProbeNotInstalled, Desc: err.Error()}
	case isFailedStateErr(err):
		return ProbeResult{State: ProbeInstalled, Desc: "installed (failed state): " + err.Error()}
	default:
		return ProbeResult{State: ProbeMechanismErr, Desc: err.Error()}
	}
}

// statusDesc renders the installed-state label. A nil-error Status() means
// the service exists, whatever the state byte says.
func statusDesc(status service.Status) string {
	switch status {
	case service.StatusRunning:
		return "installed (running)"
	case service.StatusStopped:
		return "installed (stopped)"
	default:
		return "installed (status unknown)"
	}
}

// isFailedStateErr recognizes the kardianos systemd failed-state error. The
// string "service in failed state" is emitted verbatim by
// service_systemd_linux.go:287 (kardianos v1.3.0) and is the ONLY "failed"
// classification error in the library — a substring match (case-insensitive,
// defensive against future re-wording that keeps the phrase) is the only
// handle available because kardianos does not export a sentinel for it.
func isFailedStateErr(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "failed state")
}

// RegisteredBinaryPath reads back where the platform's service manager
// actually points a registered service — the spec §4.4 路径预检. It is called
// for a service that is KNOWN installed; every failure mode (unsupported
// platform, unreadable registration, unparsable path) returns an error and
// the caller aborts the update (fail-closed — 校验最该救场的未知环境里不能
// warning 继续).
//
// Mechanism is pinned per platform (spec §4.4, FINDING E lesson: never parse
// localized CLI output like `sc qc`):
//   - windows: SCM API QueryServiceConfig → lpBinaryPathName (x/sys/windows)
//   - linux:   the kardianos-hardcoded /etc/systemd/system/<name>.service unit,
//     ExecStart first field
//   - darwin:  the kardianos-hardcoded /Library/LaunchDaemons/<name>.plist,
//     ProgramArguments first item
func RegisteredBinaryPath(name string) (string, error) {
	switch currentGOOS {
	case "windows":
		// scmQueryBinaryPath is wired by service_windows.go's init on windows
		// builds; nil elsewhere (and in tests that un-wire it).
		if scmQueryBinaryPath == nil {
			return "", fmt.Errorf("registered-binary lookup unavailable on %s (SCM wiring missing)", currentGOOS)
		}
		return scmQueryBinaryPath(name)
	case "linux":
		return systemdRegisteredBinaryPath(name)
	case "darwin":
		return launchdRegisteredBinaryPath(name)
	default:
		return "", fmt.Errorf("registered-binary lookup unsupported on %s", currentGOOS)
	}
}

// scmQueryBinaryPath is the SCM bridge seam. On windows builds
// service_windows.go replaces it in init(); everywhere else it stays nil so
// an accidental windows-branch dispatch fails closed instead of panicking.
var scmQueryBinaryPath func(name string) (string, error)

// systemdUnitDir mirrors kardianos's hardcoded system-unit directory
// (service_systemd_linux.go: configPath() → "/etc/systemd/system/" + name +
// ".service"). Package variable as a test seam.
var systemdUnitDir = "/etc/systemd/system"

// launchdPlistDir mirrors kardianos's hardcoded LaunchDaemon plist directory
// (service_darwin.go: getServiceFilePath() → "/Library/LaunchDaemons/" + name
// + ".plist"; system service — the serve install runs as root). Package
// variable as a test seam.
var launchdPlistDir = "/Library/LaunchDaemons"

// systemdRegisteredBinaryPath reads the systemd unit kardianos wrote at
// install time and returns the ExecStart binary (first field, args stripped).
func systemdRegisteredBinaryPath(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(systemdUnitDir, name+".service"))
	if err != nil {
		return "", fmt.Errorf("read systemd unit for service %q: %w", name, err)
	}
	exe, ok := execStartBinary(string(data))
	if !ok {
		return "", fmt.Errorf("systemd unit for service %q has no parsable ExecStart", name)
	}
	return exe, nil
}

// launchdRegisteredBinaryPath reads the launchd plist kardianos wrote at
// install time and returns the ProgramArguments binary (first item).
func launchdRegisteredBinaryPath(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(launchdPlistDir, name+".plist"))
	if err != nil {
		return "", fmt.Errorf("read launchd plist for service %q: %w", name, err)
	}
	exe, ok := plistProgramArgumentsFirst(string(data))
	if !ok {
		return "", fmt.Errorf("launchd plist for service %q has no parsable ProgramArguments first item", name)
	}
	return exe, nil
}

// execStartBinary extracts the executable from a systemd unit body: the first
// line whose key is exactly ExecStart=, first field, with quoting and systemd
// exec-prefix modifiers (- ignore-failure, + privilege, ! ! full-privilege)
// stripped. Commented (#) lines and ExecStartPre/ExecStartPost keys do not
// match. ok=false when there is no usable binary token.
func execStartBinary(unit string) (exe string, ok bool) {
	for _, line := range strings.Split(unit, "\n") {
		value, isExecStart := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !isExecStart {
			continue
		}
		return firstCommandToken(value)
	}
	return "", false
}

// firstCommandToken parses the first argv token of a systemd command value:
// strip exec-prefix modifier characters, then a double- or single-quoted
// token (paths with spaces), else the whitespace-delimited head.
func firstCommandToken(value string) (string, bool) {
	value = strings.TrimLeft(strings.TrimSpace(value), "-+!")
	if value == "" {
		return "", false
	}
	if q := value[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(value[1:], q)
		if end < 0 {
			return "", false // unterminated quote: nothing reliable to return
		}
		return value[1 : 1+end], true
	}
	token := value
	if i := strings.IndexAny(value, " \t"); i >= 0 {
		token = value[:i]
	}
	return token, token != ""
}

// plistProgramArgumentsFirst extracts the first <string> of the
// ProgramArguments array in an XML plist (the shape kardianos writes), XML
// entity-unescaped. ok=false when ProgramArguments is absent or its array
// carries no string item. The search is bounded by the array's closing tag so
// an empty ProgramArguments cannot leak a later key's string.
func plistProgramArgumentsFirst(plist string) (string, bool) {
	const keyTag = "<key>ProgramArguments</key>"
	i := strings.Index(plist, keyTag)
	if i < 0 {
		return "", false
	}
	rest := plist[i+len(keyTag):]

	arrayStart := strings.Index(rest, "<array")
	if arrayStart < 0 {
		return "", false
	}
	rest = rest[arrayStart:]

	arrayEnd := strings.Index(rest, "</array>")
	if arrayEnd < 0 {
		return "", false // unterminated / self-closing <array/> — no items
	}
	rest = rest[:arrayEnd]

	open := strings.Index(rest, "<string>")
	if open < 0 {
		return "", false
	}
	rest = rest[open+len("<string>"):]
	close := strings.Index(rest, "</string>")
	if close < 0 {
		return "", false
	}
	value := strings.TrimSpace(rest[:close])
	if value == "" {
		return "", false
	}
	return xmlEntityReplacer.Replace(value), true
}

// xmlEntityReplacer unescapes the five XML predefined entities in reverse
// dependency order (&amp; last: one-pass replacement means "&amp;lt;" yields
// "&lt;", never "<").
var xmlEntityReplacer = strings.NewReplacer(
	"&lt;", "<",
	"&gt;", ">",
	"&quot;", `"`,
	"&apos;", "'",
	"&amp;", "&",
)

// parseWindowsBinaryPath extracts the executable path from a Windows service
// ImagePath / lpBinaryPathName value: a leading quoted section when present
// (paths with spaces MUST be quoted by SCM convention), else the
// whitespace-delimited head. Pure string logic so every platform's tests can
// pin it; the SCM call that feeds it is verified on a real machine (T8).
//
//	`"C:\path\sshmgr.exe" serve --addr 0.0.0.0:7878` → C:\path\sshmgr.exe
//	`C:\path\sshmgr.exe serve`                       → C:\path\sshmgr.exe
func parseWindowsBinaryPath(imagePath string) string {
	s := strings.TrimSpace(imagePath)
	if s == "" {
		return ""
	}
	if s[0] == '"' {
		if end := strings.IndexByte(s[1:], '"'); end >= 0 {
			return s[1 : 1+end]
		}
		return s[1:] // unterminated quote: degrade to prefix strip
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

// SameBinaryPath reports whether two executable paths denote the same
// location, per the spec §4.4 比对口径: both sides canonicalized with
// filepath.Abs + EvalSymlinks (a failed EvalSymlinks — e.g. the path does not
// exist yet — falls back to the Abs result), then compared case-insensitively
// on Windows (EqualFold — casing/resolution differences must not false-positive
// the mismatch abort) and byte-exact elsewhere.
func SameBinaryPath(a, b string) bool {
	ca, ok := canonicalBinPath(a)
	if !ok {
		return false
	}
	cb, ok := canonicalBinPath(b)
	if !ok {
		return false
	}
	if currentGOOS == "windows" {
		return strings.EqualFold(ca, cb)
	}
	return ca == cb
}

// canonicalBinPath canonicalizes one executable path. Abs failure (a
// pathological input, or a relative path with no resolvable cwd) reports
// !ok — equality cannot be established, and the caller treats that as
// not-same (fail-closed). EvalSymlinks failure keeps the Abs form.
func canonicalBinPath(p string) (string, bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, true
	}
	return abs, true
}

// MigrationBlock returns the v0.13.0 one-time migration guidance printed when
// the update finds the LEGACY service still registered (spec §3.2: 旧名存在 →
// 打印迁移块并中止, 不半更新防新旧服务并存). This is a CONDENSED rendering —
// it names both service names and the three serve-side commands; the
// authoritative full runbook is docs/deployment-modes.md's ⭐ section (CLI
// wording follows it, docs stay the single 总册). The block ends by pointing
// at that doc so the download/SHA256 step of runbook ③ is never missed.
func MigrationBlock() string {
	return "检测到旧版服务「" + LegacyServiceName + "」仍注册在本机——v0.13.0 一次性迁移尚未完成,本次 update 中止" +
		"(不半更新,防新旧服务并存;迁移完成后新服务 " + buildinfo.ServeServiceName + " 由 update 自动接管)。\n" +
		"\n" +
		"按 v0.13.0 迁移 runbook 执行(顺序不可乱:先迁 client 后升 serve):\n" +
		"  1. ②a 存量桥迁:各 client 机 agent 从 ②a HTTP 直连姿态迁到 stdio 桥(--cache)(趁旧 serve 还在跑时完成)\n" +
		"  2. client 机改名:sshmgr 二进制替换旧 ssh-manager(旧的最后删/改名),.mcp.json 的 command 同步改指 sshmgr\n" +
		"  3. serve 机迁移(最后;管理员 shell),三条命令:\n" +
		"       sc qc " + LegacyServiceName + "        # Windows;记下 --addr/--tls-cert/--tls-key\n" +
		"       ssh-manager serve uninstall\n" +
		"       sshmgr serve install <照旧参数(--addr 0.0.0.0:7878 及 TLS flags 若有)>\n" +
		"  4. 之后:sshmgr update 一条命令自续\n" +
		"\n" +
		"完整迁移手册:docs/deployment-modes.md(含资产下载与 SHA256 核验步)\n"
}
