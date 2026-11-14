package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kardianos/service"
)

// systemctlResult carries process output and the numeric process exit status.
// The numeric status is a locale-independent signal used to tell a missing
// systemd unit from a real control failure.
type systemctlResult struct {
	exitCode int
	stdout   string
	stderr   string
	err      error
}

// runSystemctlCommand is the command execution seam for the Linux systemd
// backend. Tests replace it with deterministic fake systemctl results instead
// of touching the developer machine's service manager.
var runSystemctlCommand = func(args ...string) systemctlResult {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := systemctlResult{
		stdout: stdout.String(),
		stderr: stderr.String(),
		err:    err,
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.exitCode = exitErr.ExitCode()
	}
	return result
}

// systemdSystemUnitDir points at the directory used by kardianos/service for a
// system-wide systemd unit. It is a variable only so tests can use a temporary
// directory without touching /etc.
var systemdSystemUnitDir = "/etc/systemd/system"

// currentServiceOperatingSystem dispatches service-backend selection and is replaceable in
// tests so the Linux systemd code can be exercised on a macOS developer host.
var currentServiceOperatingSystem = runtime.GOOS

// serveServiceUninstaller is the minimal service-manager surface needed by the
// uninstall flow. stopFailureIsFatal distinguishes the Linux systemd backend
// (where a loaded unit failing to stop must abort clear) from the retained
// best-effort Stop behavior on the other service managers.
type serveServiceUninstaller interface {
	Stop() error
	Uninstall() error
	stopFailureIsFatal() bool
}

// delegateServeServiceUninstaller adapts a full kardianos service to the small
// uninstall interface.
type delegateServeServiceUninstaller struct {
	service.Service
}

func (s delegateServeServiceUninstaller) stopFailureIsFatal() bool { return false }

// newServeServiceUninstaller constructs the platform-selected uninstall
// backend. The default constructs the real kardianos service; tests can inject
// the Linux systemd backend on any development platform.
var newServeServiceUninstaller = func(i service.Interface, c *service.Config) (serveServiceUninstaller, error) {
	svc, err := serviceNewForUninstaller(i, c)
	if err != nil {
		return nil, err
	}
	if currentServiceOperatingSystem == "linux" && svc.Platform() == "linux-systemd" {
		return newSystemdServeUninstaller(c.Name), nil
	}
	return delegateServeServiceUninstaller{Service: svc}, nil
}

// serviceNewForUninstaller is the kardianos service construction seam used by
// backend-selection tests.
var serviceNewForUninstaller = service.New

// systemdServeUninstaller stops and uninstalls the exact system-wide unit used
// by `sshmgr serve install`. It checks unit existence before issuing mutating
// systemctl actions, so a machine that never installed the service is an
// idempotent success rather than an unexplained systemctl exit status.
type systemdServeUninstaller struct {
	unitName string
	unitPath string
	run      func(args ...string) systemctlResult
}

func newSystemdServeUninstaller(serviceName string) *systemdServeUninstaller {
	unitName := serviceName + ".service"
	return &systemdServeUninstaller{
		unitName: unitName,
		unitPath: filepath.Join(systemdSystemUnitDir, unitName),
		run:      runSystemctlCommand,
	}
}

func (s *systemdServeUninstaller) Stop() error {
	installed, err := s.unitInstalled()
	if err != nil {
		return err
	}
	if !installed {
		return service.ErrNotInstalled
	}
	return s.runUnitAction("stop")
}

func (s *systemdServeUninstaller) Uninstall() error {
	installed, err := s.unitInstalled()
	if err != nil {
		return err
	}
	if !installed {
		return service.ErrNotInstalled
	}
	if err := s.runUnitAction("disable"); err != nil {
		return err
	}
	if _, err := os.Stat(s.unitPath); err != nil {
		return fmt.Errorf("find systemd unit file %q before removal: %w", s.unitPath, err)
	}
	if err := os.Remove(s.unitPath); err != nil {
		return fmt.Errorf("remove systemd unit file %q: %w", s.unitPath, err)
	}
	if result := s.run("daemon-reload"); result.err != nil {
		return s.commandError("daemon-reload", result)
	}
	return nil
}

func (s *systemdServeUninstaller) stopFailureIsFatal() bool { return true }

func (s *systemdServeUninstaller) unitInstalled() (bool, error) {
	if _, err := os.Stat(s.unitPath); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat systemd unit file %q: %w", s.unitPath, err)
	}

	result := s.run("show", "-p", "LoadState", "--", s.unitName)
	if result.err != nil {
		if s.resultMeansUnitMissing(result) {
			return false, nil
		}
		return false, s.commandError("show LoadState", result)
	}
	state, ok := parseSystemdLoadState(result.stdout)
	if !ok {
		return false, fmt.Errorf("query systemd load state for %q: unexpected output %q", s.unitName, strings.TrimSpace(result.stdout))
	}
	switch state {
	case "not-found":
		return false, nil
	case "loaded", "masked":
		return true, nil
	default:
		return false, fmt.Errorf("query systemd load state for %q: %s", s.unitName, state)
	}
}

func (s *systemdServeUninstaller) runUnitAction(action string) error {
	result := s.run(action, s.unitName)
	if result.err == nil {
		return nil
	}
	if s.resultMeansUnitMissing(result) {
		return service.ErrNotInstalled
	}
	// A unit can be removed between the existence check and a mutating action.
	// Re-query LoadState rather than guessing from localized error text.
	installed, queryErr := s.unitInstalled()
	if queryErr == nil && !installed {
		return service.ErrNotInstalled
	}
	return s.commandError(action, result)
}

func (s *systemdServeUninstaller) resultMeansUnitMissing(result systemctlResult) bool {
	if result.exitCode == 5 {
		return true
	}
	state, ok := parseSystemdLoadState(result.stdout)
	return ok && state == "not-found"
}

func (s *systemdServeUninstaller) commandError(action string, result systemctlResult) error {
	detail := strings.TrimSpace(result.stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.stdout)
	}
	if detail == "" {
		return fmt.Errorf("systemctl %s %s: %w", action, s.unitName, result.err)
	}
	return fmt.Errorf("systemctl %s %s: %s", action, s.unitName, detail)
}

func parseSystemdLoadState(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && key == "LoadState" {
			return value, true
		}
	}
	return "", false
}
