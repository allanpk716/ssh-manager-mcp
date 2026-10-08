// Plan 51 — the metadata-edit write path. Two store primitives:
//
//   - UpdateForwardedMetadata (broker side): the audited CAS landing of
//     POST /server-metadata — one transaction holds the row read, the
//     revision compare, the field writes, the revision bump AND the audit
//     row, so a metadata change never lands without its history (the
//     InsertForwardedPin shape, lifted from insert-only to conditional
//     update).
//   - ApplyForwardedMetadata (client side): the local read-only-store mirror
//     of a write the broker already accepted — the SECOND narrow write gap
//     (after ApplyForwardedHostKey); it never creates local truth of its own.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// metadataFields is the frozen six-field whitelist shared by the serve route
// (request validation) and the store primitives (defense in depth). Keys are
// column names AND JSON keys — identical by design.
var metadataFields = []string{"role", "services", "location", "hardware", "caveats", "description"}

// ValidMetadataField reports whether k is one of the six editable fields.
func ValidMetadataField(k string) bool {
	for _, f := range metadataFields {
		if k == f {
			return true
		}
	}
	return false
}

// MetadataFieldList returns the whitelist (for error text and tests).
func MetadataFieldList() []string {
	out := make([]string, len(metadataFields))
	copy(out, metadataFields)
	return out
}

// ErrStaleRevision is UpdateForwardedMetadata's CAS refusal: the row's
// revision no longer matches the caller's expectation (someone — owner or
// another device — wrote in between). Current carries the row's actual
// revision so the serve handler can compose the one-hop-retry 409 body.
type ErrStaleRevision struct {
	Current int64
}

func (e *ErrStaleRevision) Error() string {
	return fmt.Sprintf("stale revision: row is at %d", e.Current)
}

// ErrServerGone marks the row vanishing between the serve-side grant walk and
// the write transaction (servers rm race). The handler maps it to the same
// deliberately vague 403 as "not granted" — either way the entry is gone.
var ErrServerGone = errors.New("server row vanished before the metadata edit landed")

// ErrMetaFieldTooLarge marks one provided value exceeding the per-field cap.
// Field names the offending JSON/column key.
type ErrMetaFieldTooLarge struct {
	Field string
}

func (e *ErrMetaFieldTooLarge) Error() string {
	return fmt.Sprintf("metadata field %q exceeds %d-byte limit", e.Field, maxServerTextFieldBytes)
}

// metaAuditOldValueBytes caps each old-value fragment in the audit Command
// (Plan 51 Q9-B): enough to hand-roll most fields back, bounded so six fields
// cannot bloat the row (the nightly backup chain owns whole-file recovery).
const metaAuditOldValueBytes = 200

// metaRow is the in-transaction read of the row being edited: everything the
// CAS compare, the audit Command and the 409 body need.
type metaRow struct {
	name     string
	role     string
	services string
	location string
	hardware string
	caveats  string
	descript string
	revision int64
}

func (m *metaRow) valueOf(field string) string {
	switch field {
	case "role":
		return m.role
	case "services":
		return m.services
	case "location":
		return m.location
	case "hardware":
		return m.hardware
	case "caveats":
		return m.caveats
	case "description":
		return m.descript
	}
	return "" // unreachable behind the whitelist; empty keeps %q renderable
}

// sortedEditKeys iterates edits deterministically (map order is random; the
// audit Command and the SET clause order must be stable).
func sortedEditKeys(edits map[string]*string) []string {
	keys := make([]string, 0, len(edits))
	for k := range edits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// validateMetadataEdits checks whitelist membership, nil values (JSON null)
// and the per-field byte cap. One shared gate for both primitives — the serve
// route validates before the vault opens, the store re-validates in defense
// in depth.
func validateMetadataEdits(edits map[string]*string) error {
	if len(edits) == 0 {
		return fmt.Errorf("metadata edit with no fields")
	}
	for _, k := range sortedEditKeys(edits) {
		if !ValidMetadataField(k) {
			return fmt.Errorf("unknown metadata field %q (allowed: %s)", k, strings.Join(metadataFields, ", "))
		}
		v := edits[k]
		if v == nil {
			return fmt.Errorf("metadata field %q is null (values must be strings — empty string clears)", k)
		}
		if len(*v) > maxServerTextFieldBytes {
			return &ErrMetaFieldTooLarge{Field: k}
		}
	}
	return nil
}

// UpdateForwardedMetadata lands a device-forwarded partial metadata edit in
// ONE transaction: read row → compare revision (CAS) → apply the present
// fields → bump revision/updated_at → write the audit row in the same tx. A
// stale token, an oversized value or an audit-write failure rolls everything
// back — a metadata change and its history commit atomically or not at all.
// device is the authenticated cache-token NAME (attribution; project_id stays
// empty — a client-asserted project is unverifiable and is refused). Returns
// the new revision.
func (s *Store) UpdateForwardedMetadata(serverID string, edits map[string]*string, expected int64, device string) (int64, error) {
	if s.readOnly {
		return 0, ErrReadOnly
	}
	if err := validateMetadataEdits(edits); err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() // no-op after Commit; IS the rollback on every refusal below

	var old metaRow
	err = tx.QueryRow(
		`SELECT name,role,services,location,hardware,caveats,description,revision FROM servers WHERE id=?`, serverID,
	).Scan(&old.name, &old.role, &old.services, &old.location, &old.hardware, &old.caveats, &old.descript, &old.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrServerGone
	}
	if err != nil {
		return 0, err
	}
	if old.revision != expected {
		return 0, &ErrStaleRevision{Current: old.revision}
	}

	sets := make([]string, 0, len(edits)+2)
	args := make([]any, 0, len(edits)+4)
	for _, k := range sortedEditKeys(edits) {
		sets = append(sets, k+"=?")
		args = append(args, *edits[k])
	}
	newRev := old.revision + 1
	sets = append(sets, "revision=?", "updated_at=?")
	args = append(args, newRev, now(), serverID, old.revision)
	res, err := tx.Exec(`UPDATE servers SET `+strings.Join(sets, ", ")+` WHERE id=? AND revision=?`, args...)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The row moved between the SELECT and the UPDATE. MaxOpenConns(1)
		// serializes store access so this is unreachable in-process — resolve
		// honestly anyway rather than guess which refusal fits.
		var cur int64
		rerr := tx.QueryRow(`SELECT revision FROM servers WHERE id=?`, serverID).Scan(&cur)
		if errors.Is(rerr, sql.ErrNoRows) {
			return 0, ErrServerGone
		}
		if rerr != nil {
			return 0, rerr
		}
		return 0, &ErrStaleRevision{Current: cur}
	}

	// The audit row rides the SAME transaction: a metadata change never lands
	// without its history, and a rolled-back conflict leaves no audit row.
	if err := writeAuditTx(tx, AuditRow{
		TS:       s.nowTime(),
		ServerID: serverID,
		Action:   "meta-edit",
		Command:  buildMetaEditCommand(&old, edits, device, newRev),
		Status:   "ok",
	}); err != nil {
		return 0, err
	}
	return newRev, tx.Commit()
}

// buildMetaEditCommand composes the meta-edit audit line: skeleton (server,
// fields, device, via, rev old→new) plus each changed field's OLD value
// truncated — Q9-B's hand-rollback insurance. Old values are
// control-sanitized (an authenticated rogue device can send arbitrary bytes;
// without this an embedded newline forges audit lines) and cut at a UTF-8
// boundary.
func buildMetaEditCommand(old *metaRow, edits map[string]*string, device string, newRev int64) string {
	keys := sortedEditKeys(edits)
	parts := []string{
		"server=" + old.name,
		"fields=" + strings.Join(keys, ","),
		"device=" + device,
		"via=meta-edit",
		fmt.Sprintf("rev=%d->%d", old.revision, newRev),
	}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("old.%s=%q", k, truncateForAudit(old.valueOf(k))))
	}
	return strings.Join(parts, " ")
}

// truncateForAudit sanitizes control characters (C0 + DEL → space) and clips
// to at most metaAuditOldValueBytes bytes at a UTF-8 rune boundary, marking a
// clip with an ellipsis. Values are forensic fragments, never parsed back.
func truncateForAudit(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) <= metaAuditOldValueBytes {
		return s
	}
	cut := s[:metaAuditOldValueBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1] // back off to the last complete rune
	}
	return cut + "…"
}

// ApplyForwardedMetadata mirrors a write the broker ALREADY accepted onto
// this (read-only) cache store — the second narrow write gap, twin of
// ApplyForwardedHostKey. Only the fields this edit carried are touched;
// revision/updated_at take the BROKER's response values (not local now());
// the WHERE revision<? guard makes the mirror monotonic: a hot rebuild that
// swapped in a generation already at or past this edit is a no-op success
// (never a downgrade), while a local row past the response's revision was
// written by someone newer and stays. No local audit row — the broker's
// meta-edit row IS the history. Not gated by read-only mode BY DESIGN
// (UpdateServer itself still returns ErrReadOnly there — the gap is this
// narrow).
func (s *Store) ApplyForwardedMetadata(serverID string, edits map[string]*string, revision, updatedAt int64) error {
	if err := validateMetadataEdits(edits); err != nil {
		return err
	}
	sets := make([]string, 0, len(edits)+2)
	args := make([]any, 0, len(edits)+4)
	for _, k := range sortedEditKeys(edits) {
		sets = append(sets, k+"=?")
		args = append(args, *edits[k])
	}
	sets = append(sets, "revision=?", "updated_at=?")
	args = append(args, revision, updatedAt, serverID, revision)
	res, err := s.db.Exec(`UPDATE servers SET `+strings.Join(sets, ", ")+` WHERE id=? AND revision<?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Absent row (cache predates the entry) is the actionable failure;
		// a present row means the local copy is already at least as fresh.
		var tmp int
		rerr := s.db.QueryRow(`SELECT 1 FROM servers WHERE id=?`, serverID).Scan(&tmp)
		if errors.Is(rerr, sql.ErrNoRows) {
			return fmt.Errorf("server %s not in local cache — cache pull and retry", serverID)
		}
		return rerr
	}
	return nil
}
