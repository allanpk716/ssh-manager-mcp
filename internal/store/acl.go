package store

// FileACLReport is the read-side verdict of InspectFileACL — the read-only
// twin of HardenACL (same LoadOrCreateServeCert/ReadServeCertFingerprint
// pairing precedent). The "who may read" semantics live in this package, next
// to the writer, so the whitelist cannot drift from what HardenACL grants.
type FileACLReport struct {
	// Supported is false on non-Windows platforms (file mode bits are the
	// protection layer there; the stub reports this).
	Supported bool
	// DaclNull: no DACL present — every principal is allowed (signal 1).
	DaclNull bool
	// Protected: SE_DACL_PROTECTED set (inheritance cut). ADVISORY ONLY —
	// it never triggers TooLoose by itself (live exposure via inherited ACEs
	// is already caught by the grantor walk; owner-approved downgrade).
	Protected bool
	// UnexpectedReadGrantors: SIDs of non-whitelisted principals holding an
	// allow ACE with a dangerous mask (signal 3), deduped and ascending.
	UnexpectedReadGrantors []string
	// OwnerSID: the file owner's SID (rendering only).
	OwnerSID string
	// OwnerUnexpected: owner outside the whitelist (signal 4, conservative
	// warning — OWNER_RIGHTS ACEs can limit the owner's implicit rights, so
	// this is "owner anomaly" advice, not an absolute privilege claim).
	OwnerUnexpected bool
}

// TooLoose reports whether the protection is looser than the hardened shape.
// A non-supported (stub) report can never be loose.
func (r FileACLReport) TooLoose() bool {
	return r.Supported && (r.DaclNull || r.OwnerUnexpected || len(r.UnexpectedReadGrantors) > 0)
}
