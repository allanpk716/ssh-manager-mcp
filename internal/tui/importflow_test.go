package tui

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// genKeyPEM writes a synthetic private key (plaintext or legacy-encrypted
// PEM — the encrypted form triggers the needs-passphrase path) and returns
// its bytes. Synthetic keys only; this repo is public.
func genKeyPEM(t *testing.T, path, passphrase string) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == "" {
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	} else {
		block, err = x509.EncryptPEMBlock(rand.Reader, "PRIVATE KEY", der, []byte(passphrase), x509.PEMCipherAES256)
		if err != nil {
			t.Fatal(err)
		}
	}
	b := pem.EncodeToMemory(block)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

// writeImportConfig writes a two-candidate config: gpu (key) and bare (no
// key); returns the config path.
func writeImportConfig(t *testing.T, keyPath string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config")
	body := "Host gpu\n  HostName 192.0.2.10\n  User deploy\n" +
		"Host bare\n  HostName 192.0.2.20\n  User deploy\n"
	if keyPath != "" {
		body = fmt.Sprintf("Host gpu\n  HostName 192.0.2.10\n  User deploy\n  IdentityFile %q\n", filepath.ToSlash(keyPath)) +
			"Host bare\n  HostName 192.0.2.20\n  User deploy\n"
	}
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// flowAtPick drives a fresh flow through the path form to the pick state.
func flowAtPick(t *testing.T, st *store.Store, cfg string) *importFlow {
	t.Helper()
	f := newImportFlow(st)
	f.pathVal = cfg
	if _, cmd := f.afterPathForm(); cmd == nil {
		t.Fatal("afterPathForm must hand back the pick form's Init cmd")
	}
	if f.state != statePick {
		t.Fatalf("state = %v, want statePick", f.state)
	}
	return f
}

// runBatch executes the batch cmd and feeds the resulting msg back into the
// flow — the async hop a real tea runtime would do.
func runBatch(t *testing.T, f *importFlow) {
	t.Helper()
	cmd := f.startBatch()
	msg := cmd()
	m, _ := f.Update(msg)
	if mf, ok := m.(*importFlow); !ok || mf != f {
		t.Fatal("importDoneMsg must return the same flow instance")
	}
}

func serverNames(t *testing.T, f *importFlow) []string {
	t.Helper()
	ss, err := f.st.ListServers()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}

// TestImportFlowPathFormEscAborts: Esc on the path form closes the whole
// overlay with ZERO writes.
func TestImportFlowPathFormEscAborts(t *testing.T) {
	st := newStore(t)
	f := newImportFlow(st)
	m, cmd := f.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("Esc must produce a formDoneMsg cmd")
	}
	if done, ok := cmd().(formDoneMsg); !ok || !done.aborted {
		t.Fatalf("Esc must abort the overlay: %#v", done)
	}
	if _, isFlow := m.(*importFlow); !isFlow {
		t.Fatalf("model type %T", m)
	}
	if got := serverNames(t, f); len(got) != 0 {
		t.Fatalf("path-form Esc must write nothing, store has %v", got)
	}
}

// TestImportFlowPickExcludesVaultConflicts: a vault server already occupying
// a candidate's name/endpoint removes it from the pick list (PlanImport seam)
// and counts it as 跳过.
func TestImportFlowPickExcludesVaultConflicts(t *testing.T) {
	st := newStore(t)
	if _, err := st.AddServerWithCredentials(&models.Server{
		Name: "bare", Host: "elsewhere", Port: 2222, User: "other",
	}, nil, nil); err != nil {
		t.Fatal(err)
	}
	f := flowAtPick(t, st, writeImportConfig(t, ""))
	if len(f.cands) != 1 || f.cands[0].Name != "gpu" {
		t.Fatalf("vault conflict must leave only gpu: %+v", f.cands)
	}
	if f.skipN != 1 {
		t.Fatalf("skipN = %d, want 1", f.skipN)
	}
}

// TestImportFlowEmptyPickImportsNothing: an empty multiselect submission
// imports nothing and lands on the result screen.
func TestImportFlowEmptyPickImportsNothing(t *testing.T) {
	st := newStore(t)
	f := flowAtPick(t, st, writeImportConfig(t, ""))
	f.pick = nil // empty selection
	runBatch(t, f)
	if f.state != stateResult {
		t.Fatalf("state = %v, want stateResult", f.state)
	}
	if got := serverNames(t, f); len(got) != 0 {
		t.Fatalf("empty pick must import nothing, store has %v", got)
	}
	if f.importedN != 0 {
		t.Fatalf("importedN = %d, want 0", f.importedN)
	}
}

// TestImportFlowBatchImportAndQueue: a full batch lands both servers, retains
// the key bytes for encrypted targets, and queues EVERY imported server for
// supplement (imports carry no Role → all ⚠).
func TestImportFlowBatchImportAndQueue(t *testing.T) {
	dir := t.TempDir()
	encPEM := genKeyPEM(t, filepath.Join(dir, "enc"), "secret-pass")
	cfg := writeImportConfig(t, filepath.Join(dir, "enc"))
	st := newStore(t)
	f := flowAtPick(t, st, cfg)
	f.pick = []string{"gpu", "bare"}
	runBatch(t, f)

	if f.state != stateSupplement {
		t.Fatalf("state = %v, want stateSupplement", f.state)
	}
	if f.importedN != 2 || len(f.supp) != 2 {
		t.Fatalf("importedN=%d supp=%d, want 2/2", f.importedN, len(f.supp))
	}
	got := serverNames(t, f)
	if len(got) != 2 || got[0] != "bare" || got[1] != "gpu" {
		t.Fatalf("store servers: %v", got)
	}
	// gpu: encrypted key → credential minted + needs-passphrase tag + key retained
	gpu, _ := st.GetServerByName("gpu")
	if gpu.CredentialID == "" || gpu.AuthMethod != models.AuthPrivateKey {
		t.Fatalf("gpu must carry its key credential: %+v", gpu)
	}
	if !hasTag(gpu, "needs-passphrase") {
		t.Fatalf("encrypted-key import must tag needs-passphrase: %+v", gpu)
	}
	if string(f.suppKeys["gpu"]) != string(encPEM) {
		t.Fatal("in-memory key bytes not retained for the re-mint")
	}
	// bare: credential-less, no tag
	bare, _ := st.GetServerByName("bare")
	if bare.CredentialID != "" || hasTag(bare, "needs-passphrase") {
		t.Fatalf("bare must be credential-less and untagged: %+v", bare)
	}
}

// TestImportFlowBatchKeyDedup: two candidates sharing one key file mint ONE
// credential row (the shared PickKey seam's batch dedup).
func TestImportFlowBatchKeyDedup(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "shared")
	genKeyPEM(t, key, "")
	cfgPath := filepath.Join(dir, "config")
	body := fmt.Sprintf(
		"Host k1\n  HostName 192.0.2.1\n  User u\n  IdentityFile %q\n"+
			"Host k2\n  HostName 192.0.2.2\n  User u\n  IdentityFile %q\n",
		filepath.ToSlash(key), filepath.ToSlash(key))
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	st := newStore(t)
	f := flowAtPick(t, st, cfgPath)
	f.pick = []string{"k1", "k2"}
	runBatch(t, f)
	k1, _ := st.GetServerByName("k1")
	k2, _ := st.GetServerByName("k2")
	if k1 == nil || k2 == nil || k1.CredentialID == "" || k1.CredentialID != k2.CredentialID {
		t.Fatalf("shared key must mint ONE credential row: %q vs %q", k1.CredentialID, k2.CredentialID)
	}
}

// TestSupplementFormFieldSets: the conditional secret field differs per
// target kind — normal (structured+sudo only), credless (+密码（现在设置）),
// needs-pass (+密钥口令（补全加密私钥）).
func TestSupplementFormFieldSets(t *testing.T) {
	titles := func(condPass, condKey bool) []string {
		fields := supplementFields(&serverDraft{}, condPass, condKey)
		out := make([]string, 0, len(fields))
		for _, fld := range fields {
			in, ok := fld.(*huh.Input)
			if !ok {
				t.Fatalf("supplement field %T, want *huh.Input", fld)
			}
			out = append(out, inputTitle(in))
		}
		return out
	}
	base := 7 // 6 structured + sudo
	if got := titles(false, false); len(got) != base {
		t.Fatalf("normal target field count = %d, want %d: %v", len(got), base, got)
	}
	got := titles(true, false)
	if len(got) != base+1 || got[len(got)-1] != "密码（现在设置）" {
		t.Fatalf("credless target must add the password field: %v", got)
	}
	got = titles(false, true)
	if len(got) != base+1 || got[len(got)-1] != "密钥口令（补全加密私钥）" {
		t.Fatalf("needs-pass target must add the passphrase field: %v", got)
	}
}

// supplementTarget returns the flow positioned at the named supplement
// entry (the queue holds the given names in order).
func supplementTarget(t *testing.T, f *importFlow, name string) {
	t.Helper()
	for i, s := range f.supp {
		if s.Name == name {
			f.suppIdx = i
			f.openSupplement()
			if f.srv.Name != name {
				t.Fatalf("openSupplement landed on %s", f.srv.Name)
			}
			return
		}
	}
	t.Fatalf("supplement queue has no %q: %+v", name, f.supp)
}

// TestImportFlowSupplementEscSkips: Esc advances to the next server WITHOUT
// writing the current one (its ⚠ stays).
func TestImportFlowSupplementEscSkips(t *testing.T) {
	st := newStore(t)
	f := flowAtPick(t, st, writeImportConfig(t, ""))
	f.pick = []string{"gpu", "bare"}
	runBatch(t, f)
	supplementTarget(t, f, "gpu")

	_, cmd := f.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if f.state != stateSupplement || f.srv.Name != "bare" {
		t.Fatalf("Esc must skip to the next target: state=%v srv=%v", f.state, f.srv)
	}
	if cmd == nil {
		t.Fatal("Esc must return the next form's Init cmd")
	}
	gpu, _ := st.GetServerByName("gpu")
	if gpu.Role != "" {
		t.Fatalf("Esc skip must not write the target: %+v", gpu)
	}
	// skipping the LAST entry lands on the result screen
	f.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if f.state != stateResult {
		t.Fatalf("skip past the end must reach the result, got %v", f.state)
	}
}

// TestImportFlowSupplementQEndsLoop: q jumps straight to the result screen.
func TestImportFlowSupplementQEndsLoop(t *testing.T) {
	st := newStore(t)
	f := flowAtPick(t, st, writeImportConfig(t, ""))
	f.pick = []string{"gpu", "bare"}
	runBatch(t, f)
	f.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if f.state != stateResult {
		t.Fatalf("q must end the loop: %v", f.state)
	}
	// nothing was supplemented: both stay ⚠
	if f.pendingCount() != 2 {
		t.Fatalf("pending after q = %d, want 2", f.pendingCount())
	}
}

// TestImportFlowSupplementSubmitPassphrase: the condKey target re-mints its
// credential from the in-memory key bytes with the new passphrase, drops the
// needs-passphrase tag, and persists the structured fields.
func TestImportFlowSupplementSubmitPassphrase(t *testing.T) {
	dir := t.TempDir()
	encPEM := genKeyPEM(t, filepath.Join(dir, "enc"), "secret-pass")
	cfg := writeImportConfig(t, filepath.Join(dir, "enc"))
	st := newStore(t)
	f := flowAtPick(t, st, cfg)
	f.pick = []string{"gpu", "bare"}
	runBatch(t, f)
	supplementTarget(t, f, "gpu")
	if !f.condKey || f.condPass {
		t.Fatalf("gpu must be a condKey target: pass=%v key=%v", f.condPass, f.condKey)
	}

	before, _ := st.GetServerByName("gpu")
	oldCred := before.CredentialID
	f.d.Role = "prod gpu box"
	f.d.KeyPass = "secret-pass"
	f.submitSupplement()

	gpu, _ := st.GetServerByName("gpu")
	if gpu.Role != "prod gpu box" {
		t.Fatalf("structured fields not persisted: %+v", gpu)
	}
	if hasTag(gpu, "needs-passphrase") {
		t.Fatalf("passphrase supplied must drop the tag: %+v", gpu)
	}
	if gpu.CredentialID == "" || gpu.CredentialID == oldCred {
		t.Fatalf("credential must be re-minted: old=%s new=%s", oldCred, gpu.CredentialID)
	}
	cred, err := st.GetCredential(gpu.CredentialID)
	if err != nil || cred == nil {
		t.Fatalf("GetCredential: %v %v", cred, err)
	}
	if string(cred.Secret) != string(encPEM) || string(cred.Passphrase) != "secret-pass" {
		t.Fatal("re-minted credential must carry the retained key + new passphrase")
	}
	if c, _ := st.GetCredential(oldCred); c != nil {
		t.Fatal("replaced (unreferenced) credential row must be dropped in the same tx")
	}
}

// TestSupplementCondKeyEmptyPassphraseKeepsTag: a condKey target submitted
// with an EMPTY passphrase takes the nil-cred path — UpdateServerWithCredentials
// keeps the existing key credential row, and the needs-passphrase tag STAYS
// (the missing-passphrase fact is unchanged, so the ⚠ must not silently
// disappear; the tag drops only when a passphrase is actually supplied — the
// sibling test above). Mirrors the supplement tests' direct-call shape.
func TestSupplementCondKeyEmptyPassphraseKeepsTag(t *testing.T) {
	dir := t.TempDir()
	genKeyPEM(t, filepath.Join(dir, "enc"), "secret-pass")
	cfg := writeImportConfig(t, filepath.Join(dir, "enc"))
	st := newStore(t)
	f := flowAtPick(t, st, cfg)
	f.pick = []string{"gpu", "bare"}
	runBatch(t, f)
	supplementTarget(t, f, "gpu")
	if !f.condKey || f.condPass {
		t.Fatalf("gpu must be a condKey target: pass=%v key=%v", f.condPass, f.condKey)
	}

	before, _ := st.GetServerByName("gpu")
	if before.CredentialID == "" {
		t.Fatal("precondition: encrypted-key import must carry a credential row")
	}
	f.d.Role = "prod gpu box"
	f.d.KeyPass = "" // left empty — "later"
	f.submitSupplement()

	gpu, _ := st.GetServerByName("gpu")
	if gpu.CredentialID != before.CredentialID {
		t.Fatalf("empty passphrase must keep the existing credential (nil-cred path): old=%s new=%s", before.CredentialID, gpu.CredentialID)
	}
	if !hasTag(gpu, "needs-passphrase") {
		t.Fatalf("needs-passphrase tag must stay — the missing-passphrase fact is unchanged: %+v", gpu)
	}
	if gpu.Role != "prod gpu box" {
		t.Fatalf("structured fields must still persist on the nil-cred path: %+v", gpu)
	}
	// both targets remain ⚠ (gpu: tag kept; bare: credential-less) — the
	// result-screen 待补 count reflects it.
	if f.pendingCount() != 2 {
		t.Fatalf("pending = %d, want 2 (empty passphrase must NOT clear the ⚠)", f.pendingCount())
	}
}

// TestImportFlowSupplementSubmitPassword: the condPass target mints a
// password credential; an EMPTY password keeps it credential-less (⚠ stays).
func TestImportFlowSupplementSubmitPassword(t *testing.T) {
	st := newStore(t)
	f := flowAtPick(t, st, writeImportConfig(t, ""))
	f.pick = []string{"bare"}
	runBatch(t, f)
	supplementTarget(t, f, "bare")
	if !f.condPass || f.condKey {
		t.Fatalf("bare must be a condPass target: pass=%v key=%v", f.condPass, f.condKey)
	}

	// empty conditional input = later: role persists, credential stays absent
	f.d.Role = "app"
	f.submitSupplement()
	bare, _ := st.GetServerByName("bare")
	if bare.Role != "app" || bare.CredentialID != "" {
		t.Fatalf("empty password must keep the server credential-less: %+v", bare)
	}
	if f.pendingCount() != 1 { // role set but still no credential → still ⚠
		t.Fatalf("pending = %d, want 1", f.pendingCount())
	}

	// now supplement the SAME server with a password: re-open queue entry 0
	// (its snapshot carries the persisted role; the empty credential keeps
	// condPass true), fill the password, submit — credential minted, ⚠ clears.
	f.suppIdx = 0
	f.openSupplement()
	if !f.condPass || f.d.Role != "app" {
		t.Fatalf("re-opened entry must prefill role and stay condPass: %+v", f.d)
	}
	f.d.Password = "pw123"
	f.submitSupplement()
	bare2, _ := st.GetServerByName("bare")
	if bare2.CredentialID == "" || bare2.AuthMethod != models.AuthPassword {
		t.Fatalf("password must mint a credential: %+v", bare2)
	}
	if f.pendingCount() != 0 {
		t.Fatalf("complete supplement must clear ⚠: %d", f.pendingCount())
	}
}

// TestImportFlowResultScreen: the result view carries the counts and the
// later-path hint for whatever is still ⚠.
func TestImportFlowResultScreen(t *testing.T) {
	st := newStore(t)
	f := flowAtPick(t, st, writeImportConfig(t, ""))
	f.pick = []string{"gpu", "bare"}
	runBatch(t, f)
	f.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if f.state != stateResult {
		t.Fatal("precondition: result screen")
	}
	v := f.View().Content
	for _, want := range []string{"导入 2 / 跳过 0 / 待补 2", "! 过滤", "任意键关闭"} {
		if !strings.Contains(v, want) {
			t.Fatalf("result view missing %q:\n%s", want, v)
		}
	}
	// any key dismisses into formDoneMsg{after: actionDoneMsg}
	_, cmd := f.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("result keypress must produce a cmd")
	}
	if done, ok := cmd().(formDoneMsg); !ok || done.after == nil {
		t.Fatalf("result dismiss must be formDoneMsg with a refetch action: %#v", done)
	}
}

// TestImportFlowNoCandidatesLandsResult: a config whose every candidate
// conflicts with the vault goes straight to the result screen (no pick step).
func TestImportFlowNoCandidatesLandsResult(t *testing.T) {
	st := newStore(t)
	cfg := writeImportConfig(t, "")
	for _, n := range []string{"gpu", "bare"} {
		if _, err := st.AddServer(&models.Server{Name: n, Host: "x", User: "u", Port: 22}); err != nil {
			t.Fatal(err)
		}
	}
	f := newImportFlow(st)
	f.pathVal = cfg
	f.afterPathForm()
	if f.state != stateResult || f.importedN != 0 || f.skipN != 2 {
		t.Fatalf("all-conflict config: state=%v imported=%d skip=%d", f.state, f.importedN, f.skipN)
	}
}
