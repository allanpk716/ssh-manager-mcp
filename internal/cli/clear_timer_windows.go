//go:build windows

package cli

import (
	"fmt"
	"os/exec"
	"strings"
)

// legacyTimerName is the pre-role-wizard cache auto-refresh scheduled task
// (Plan "cache-auto-refresh"). clear removes it on client machines; the
// machine it was created on had it deleted by hand on 2026-08-15 (spec §4).
const legacyTimerName = "ssh-manager-cache-refresh"

// legacyTimerPresent reports whether the legacy scheduled task exists.
// /Query on a missing task fails — that is the "absent" signal.
func legacyTimerPresent() bool {
	return exec.Command("schtasks", "/Query", "/TN", legacyTimerName).Run() == nil
}

// deleteLegacyTimer ends + deletes the legacy scheduled task. A task that
// does not exist is SUCCESS (clear is idempotent). schtasks output is
// locale-dependent; the "not found" phrasings of the locales this repo's
// operators use (en + zh-CN) are treated as success — other locales surface
// as a non-fatal warning in runClear.
func deleteLegacyTimer() error {
	// End first so a running refresh instance does not outlive its task;
	// failure here is expected (task exists but is not running) — ignored.
	_ = exec.Command("schtasks", "/End", "/TN", legacyTimerName, "/F").Run()
	out, err := exec.Command("schtasks", "/Delete", "/TN", legacyTimerName, "/F").CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.ToLower(string(out))
	if strings.Contains(msg, "cannot find") || strings.Contains(msg, "找不到") {
		return nil // task does not exist = success
	}
	return fmt.Errorf("schtasks /Delete /TN %s: %v: %s", legacyTimerName, err, strings.TrimSpace(string(out)))
}
