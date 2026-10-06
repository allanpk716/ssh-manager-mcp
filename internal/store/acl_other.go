//go:build !windows

package store

// HardenACL is a no-op on non-Windows platforms: Unix uses file mode bits
// (0600 set by the caller's os.OpenFile / WriteFile) plus directory 0700 for
// protection (spec §5.2). Returns nil so FileKeyProvider.Set can call it
// unconditionally without a runtime.GOOS branch at every call site (which
// would break cross-compilation — HardenACL is only defined under the
// windows build tag).
func HardenACL(path string) error { return nil }

// InspectFileACL reports Supported=false on non-Windows: mode bits are that
// platform's protection layer (see HardenACL's note). Callers branch on
// Supported instead of runtime.GOOS (spec §1.5).
func InspectFileACL(path string) (FileACLReport, error) {
	return FileACLReport{Supported: false}, nil
}

// MirrorDACL is a no-op on non-Windows platforms: SQLite creates the WAL
// sidecars under the operating user inside the 0700 vault directory, and the
// Unix deployment posture is single-identity (serve and CLI run as the same
// user), so there is no cross-identity DACL rewrite to compensate for. See the
// Windows twin in acl_windows.go for the LocalSystem-serve vs
// interactive-user race MirrorDACL exists to close (Plan 50).
func MirrorDACL(src, dst string) error { return nil }

// SidecarACLGap reports parity-unsupported on non-Windows: mode bits are the
// sidecars' protection layer there, so there is nothing to compare (see
// ErrACLParityUnsupported).
func SidecarACLGap(storePath, sidecarPath string) ([]string, error) {
	return nil, ErrACLParityUnsupported
}
