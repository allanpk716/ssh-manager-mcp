//go:build windows

// Package cli: keychain seam (Windows).
//
// keychain is the master-key source. On Windows the seam binds DpapiKeyProvider
// (user-scope DPAPI + atomic file + ACL) instead of the OS keychain: Credential
// Manager (wincred) fails in sshd/Service sessions with
// ERROR_NO_SUCH_LOGON_SESSION 1312, but DPAPI works across
// RDP/sshd/TaskScheduler sessions (Plan 14 T1/T2 spike, spec 12).
//
// DPAPI does not use keychain service names, so SSHMGR_KEYRING_SERVICE is
// irrelevant here (Windows tests don't depend on the eval service-name
// isolation — they inject a fake via the keychain seam var directly).
//
// Unix builds see keychain_unix.go instead (envKeyringKeyProvider reads
// SSHMGR_KEYRING_SERVICE on every call).
package cli

import "ssh-manager-mcp/internal/store"

// keychain is the master-key source. Windows: DpapiKeyProvider. Unix: see
// keychain_unix.go. Tests override this var with a fake (value receiver on
// DpapiKeyProvider keeps the seam a plain value).
var keychain store.KeyProvider = store.DpapiKeyProvider{}
