//go:build !windows

package cli

// legacyTimerName is shared by the enumeration marker text (scanClearTargets)
// and the Windows timer implementation.
const legacyTimerName = "ssh-manager-cache-refresh"

// legacyTimerPresent: the legacy auto-refresh timer was a Windows-only
// schtask — on Unix nothing program-created ever existed.
func legacyTimerPresent() bool { return false }

// deleteLegacyTimer is a no-op on Unix: the legacy refresh was a Windows
// schtask, and user-crafted cron/systemd units are deliberately never
// touched (spec §4.2).
func deleteLegacyTimer() error { return nil }
