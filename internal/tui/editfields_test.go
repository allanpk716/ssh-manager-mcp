package tui

import (
	"strings"
	"testing"
)

// freshEditDraft: a fully-populated non-secret draft (secrets empty, clear
// toggle off) — the state an edit page starts from after prefill (prefill
// leaves secrets empty = keep existing).
func freshEditDraft() *serverDraft {
	return &serverDraft{
		Name: "n", Host: "h", User: "u", Port: 22,
		Hardware: "hw", Location: "loc", Role: "r",
		Services: "s", Caveats: "c", Description: "desc",
	}
}

func fieldByKey(t *testing.T, fields []editField, key string) editField {
	t.Helper()
	for _, f := range fields {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("no editField with key %q", key)
	return editField{}
}

// TestEditFieldsTableShape locks the fixed field order (design decision) and
// the per-mode shape: 16 items in edit mode, 14 in add mode (neither 清除凭据
// nor 暴露 Host exists in add mode — no credential to clear, host masking
// defaults on). The save item is NOT part of the table
// (the page appends it as a list sentinel), so 备注 must be the last entry.
func TestEditFieldsTableShape(t *testing.T) {
	wantEdit := []string{
		"名称", "Host", "端口", "SSH 用户", "密码", "私钥路径", "密钥口令",
		"sudo 密码", "清除凭据", "暴露 Host", "硬件", "位置", "角色", "服务", "Caveats", "备注",
	}
	got := editFields(true)
	if len(got) != len(wantEdit) {
		t.Fatalf("editFields(true) returned %d fields, want %d", len(got), len(wantEdit))
	}
	keys := map[string]bool{}
	for i, f := range got {
		if f.Label != wantEdit[i] {
			t.Errorf("editFields(true)[%d] label = %q, want %q", i, f.Label, wantEdit[i])
		}
		if keys[f.Key] {
			t.Errorf("duplicate key %q", f.Key)
		}
		keys[f.Key] = true
		if f.Get == nil || f.Set == nil || f.Build == nil {
			t.Errorf("field %q has a nil Get/Set/Build", f.Key)
		}
		if f.Build(&serverDraft{}) == nil {
			t.Errorf("field %q Build returned nil", f.Key)
		}
	}

	wantAdd := []string{
		"名称", "Host", "端口", "SSH 用户", "密码", "私钥路径", "密钥口令",
		"sudo 密码", "硬件", "位置", "角色", "服务", "Caveats", "备注",
	}
	add := editFields(false)
	if len(add) != len(wantAdd) {
		t.Fatalf("editFields(false) returned %d fields, want %d", len(add), len(wantAdd))
	}
	for i, f := range add {
		if f.Label != wantAdd[i] {
			t.Errorf("editFields(false)[%d] label = %q, want %q", i, f.Label, wantAdd[i])
		}
	}
}

// TestEditFieldsSecretFlags: password/keypass/sudo password are Secret (value
// preview masked); keypath is a PATH, not a secret — it must not be masked.
func TestEditFieldsSecretFlags(t *testing.T) {
	secretWant := map[string]bool{"password": true, "keypass": true, "sudopassword": true}
	for _, f := range editFields(true) {
		if f.Secret != secretWant[f.Key] { // absent key → false
			t.Errorf("field %q Secret = %v, want %v", f.Key, f.Secret, secretWant[f.Key])
		}
	}
}

// TestEditFieldsKeysMatchSnapshot: snapshotDraft must capture exactly the 16
// table keys, and every field's Set must flip exactly its own key dirty —
// the Key↔snapshot correspondence the page's dirty marks rely on.
func TestEditFieldsKeysMatchSnapshot(t *testing.T) {
	mutations := map[string]string{
		"name": "n2", "host": "h2", "port": "2222", "user": "u2",
		"password": "pw", "keypath": "/key", "keypass": "kp", "sudopassword": "sp",
		"clearcredential": "已勾选", "exposehost": "已勾选", "hardware": "hw2",
		"location": "loc2", "role": "r2", "services": "s2", "caveats": "c2",
		"description": "desc2",
	}
	snap0 := snapshotDraft(freshEditDraft())
	if len(snap0) != 16 {
		t.Fatalf("snapshotDraft captured %d keys, want 16", len(snap0))
	}
	for _, f := range editFields(true) {
		if _, ok := snap0[f.Key]; !ok {
			t.Errorf("snapshotDraft missing key %q", f.Key)
		}
	}
	for _, editing := range []bool{true, false} {
		for _, f := range editFields(editing) {
			d := freshEditDraft()
			snap := snapshotDraft(d)
			f.Set(d, mutations[f.Key])
			dirty := dirtyAgainst(d, snap)
			if !dirty[f.Key] {
				t.Errorf("Set(%q) must mark %q dirty", mutations[f.Key], f.Key)
			}
			for k, isDirty := range dirty {
				if isDirty && k != f.Key {
					t.Errorf("Set on %q also marked %q dirty", f.Key, k)
				}
			}
		}
	}
}

// TestEditFieldsGetSetRoundTrip: non-secret fields round-trip Set→Get; port
// Set Atoi's and range-checks (invalid input is a safe no-op); the clear
// toggle accepts both its display status and the canonical bool string.
func TestEditFieldsGetSetRoundTrip(t *testing.T) {
	fields := editFields(true)
	d := &serverDraft{}

	strKeys := []string{"name", "host", "user", "keypath", "hardware", "location", "role", "services", "caveats", "description"}
	for _, key := range strKeys {
		f := fieldByKey(t, fields, key)
		f.Set(d, "RT-VALUE")
		if got := f.Get(d); got != "RT-VALUE" {
			t.Errorf("%q round-trip: Get = %q, want RT-VALUE", key, got)
		}
	}

	pf := fieldByKey(t, fields, "port")
	d.Port = 22
	pf.Set(d, "2222")
	if d.Port != 2222 || pf.Get(d) != "2222" {
		t.Fatalf("port Set(2222): Port=%d Get=%q", d.Port, pf.Get(d))
	}
	for _, bad := range []string{"not-a-number", "0", "70000", "-1"} {
		pf.Set(d, bad)
		if d.Port != 2222 {
			t.Errorf("port Set(%q) must be a no-op, Port=%d", bad, d.Port)
		}
	}
	pf.Set(d, " 2223 ")
	if d.Port != 2223 {
		t.Errorf("port Set must trim: Port=%d, want 2223", d.Port)
	}

	cf := fieldByKey(t, fields, "clearcredential")
	for v, want := range map[string]bool{"已勾选": true, "true": true, "false": false, "": false, "未勾选": false} {
		cf.Set(d, v)
		if d.ClearCredential != want {
			t.Errorf("clearcredential Set(%q) = %v, want %v", v, d.ClearCredential, want)
		}
	}
	cf.Set(d, "true")
	if got := cf.Get(d); got != "已勾选" {
		t.Errorf("clearcredential Get after tick = %q, want 已勾选", got)
	}
}

// TestSecretGetNeverReturnsContent: secret Get returns a STATUS, never the
// stored value (the preview sentinel is pinned separately below).
func TestSecretGetNeverReturnsContent(t *testing.T) {
	d := &serverDraft{Password: "PW-CONTENT", KeyPass: "KP-CONTENT", SudoPassword: "SP-CONTENT"}
	for _, editing := range []bool{true, false} {
		for _, key := range []string{"password", "keypass", "sudopassword"} {
			f := fieldByKey(t, editFields(editing), key)
			if got := f.Get(d); strings.Contains(got, "CONTENT") {
				t.Errorf("secret Get(%q, editing=%v) leaked content: %q", key, editing, got)
			}
		}
	}
}

// TestSecretStatusStrings locks the exact status wording (T2 renders these
// verbatim): edit-mode blank password/sudo means keep-the-existing (prefill
// leaves secrets empty); keypass has no existing value to keep (it only
// rides a newly entered key path), so its blank status is plain （未设）.
func TestSecretStatusStrings(t *testing.T) {
	edit := editFields(true)
	if got := fieldByKey(t, edit, "password").Get(&serverDraft{}); got != "（留空=保持现有）" {
		t.Errorf("edit blank password status = %q, want （留空=保持现有）", got)
	}
	if got := fieldByKey(t, edit, "sudopassword").Get(&serverDraft{}); got != "（留空=保持现有）" {
		t.Errorf("edit blank sudo status = %q, want （留空=保持现有）", got)
	}
	if got := fieldByKey(t, edit, "keypass").Get(&serverDraft{}); got != "（未设）" {
		t.Errorf("edit blank keypass status = %q, want （未设）", got)
	}
	if got := fieldByKey(t, edit, "password").Get(&serverDraft{Password: "x"}); got != "已设（新值）" {
		t.Errorf("edit set password status = %q, want 已设（新值）", got)
	}
	add := editFields(false)
	if got := fieldByKey(t, add, "password").Get(&serverDraft{}); got != "（未设）" {
		t.Errorf("add blank password status = %q, want （未设）", got)
	}
	if got := fieldByKey(t, add, "password").Get(&serverDraft{Password: "x"}); got != "已设" {
		t.Errorf("add set password status = %q, want 已设", got)
	}
}

func countDirty(m map[string]bool) int {
	n := 0
	for _, v := range m {
		if v {
			n++
		}
	}
	return n
}

// TestDirtyAgainstComputation: a fresh draft is all-clean; a single mutation
// dirties exactly that key; reverting restores clean (changed-back = clean).
func TestDirtyAgainstComputation(t *testing.T) {
	d := freshEditDraft()
	snap := snapshotDraft(d)
	for k, dirty := range dirtyAgainst(d, snap) {
		if dirty {
			t.Errorf("fresh draft: %q reported dirty", k)
		}
	}
	d.Hardware = "changed"
	dirty := dirtyAgainst(d, snap)
	if !dirty["hardware"] {
		t.Error("hardware change must be dirty")
	}
	if n := countDirty(dirty); n != 1 {
		t.Errorf("single change marked %d keys dirty, want 1", n)
	}
	d.Hardware = "hw" // changed back
	if dirty := dirtyAgainst(d, snap); countDirty(dirty) != 0 {
		t.Errorf("reverted draft still dirty: %v", dirty)
	}
	d.ClearCredential = true
	if !dirtyAgainst(d, snap)["clearcredential"] {
		t.Error("ticking clear-credential must be dirty")
	}
}

// TestFieldPreviewFormat: title carries the ● dirty prefix, desc carries the
// value preview + （已改） suffix; empty values preview as （空）.
func TestFieldPreviewFormat(t *testing.T) {
	fields := editFields(true)
	d := freshEditDraft()
	hw := fieldByKey(t, fields, "hardware")

	title, desc := fieldPreview(hw, d, false)
	if title != "硬件" || desc != "hw" {
		t.Errorf("clean preview = (%q, %q), want (硬件, hw)", title, desc)
	}
	title, desc = fieldPreview(hw, d, true)
	if title != "● 硬件" || desc != "hw（已改）" {
		t.Errorf("dirty preview = (%q, %q), want (● 硬件, hw（已改）)", title, desc)
	}
	d.Hardware = ""
	if _, desc = fieldPreview(hw, d, true); desc != "（空）（已改）" {
		t.Errorf("empty dirty desc = %q, want （空）（已改）", desc)
	}
	if _, desc = fieldPreview(hw, d, false); desc != "（空）" {
		t.Errorf("empty clean desc = %q, want （空）", desc)
	}
	pf := fieldByKey(t, fields, "port")
	if _, desc = fieldPreview(pf, d, false); desc != "22" {
		t.Errorf("port preview = %q, want 22", desc)
	}
	cf := fieldByKey(t, fields, "clearcredential")
	if _, desc = fieldPreview(cf, d, false); desc != "未勾选" {
		t.Errorf("clear-credential preview = %q, want 未勾选", desc)
	}
}

// TestFieldPreviewMasksSecrets (sentinel): with live sentinel values in the
// draft, no secret field's preview — clean or dirty — may contain them. The
// key PATH is not a secret: its preview shows the value itself.
func TestFieldPreviewMasksSecrets(t *testing.T) {
	d := freshEditDraft()
	d.Password = "SENTINEL-PW"
	d.KeyPass = "SENTINEL-KP"
	d.SudoPassword = "SENTINEL-SP"
	for _, editing := range []bool{true, false} {
		for _, key := range []string{"password", "keypass", "sudopassword"} {
			f := fieldByKey(t, editFields(editing), key)
			for _, dirty := range []bool{false, true} {
				title, desc := fieldPreview(f, d, dirty)
				if strings.Contains(title+desc, "SENTINEL") {
					t.Fatalf("secret %q preview leaked (editing=%v dirty=%v): %q / %q",
						key, editing, dirty, title, desc)
				}
			}
		}
	}
	kf := fieldByKey(t, editFields(true), "keypath")
	d.KeyPath = "/home/u/.ssh/id_ed25519"
	if _, desc := fieldPreview(kf, d, false); desc != "/home/u/.ssh/id_ed25519" {
		t.Errorf("keypath preview = %q, want the raw path (path is not a secret)", desc)
	}
}
