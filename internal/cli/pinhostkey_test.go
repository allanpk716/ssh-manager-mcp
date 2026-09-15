package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// Plan 48 §3/§8-T11: the owner out-of-band `servers pin-hostkey` six forms.
// Output strings marked 逐字 in the task contract are asserted verbatim; audit
// rows are read back through the store to pin the exact command text.

// genHostKey generates a fresh ed25519 host key and returns its parsed form
// plus the canonical SHA256:<base64> fingerprint.
func genHostKey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return sshPub, ssh.FingerprintSHA256(sshPub)
}

// seedPinServers adds the named servers to the env vault (password cred, so
// each row satisfies the non-null credential shape) and returns name→id.
func seedPinServers(t *testing.T, mk []byte, servers ...models.Server) map[string]string {
	t.Helper()
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ids := map[string]string{}
	for i := range servers {
		id, err := st.AddServerWithCredentials(&servers[i],
			&models.Credential{Type: models.CredPassword, Secret: []byte("x")}, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids[servers[i].Name] = id
	}
	return ids
}

// openPinVault reopens the env vault for post-CLI assertions.
func openPinVault(t *testing.T, mk []byte) *store.Store {
	t.Helper()
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// pinAuditRows returns the vault's audit rows for one action.
func pinAuditRows(t *testing.T, mk []byte, action string) []store.AuditRow {
	t.Helper()
	rows, err := openPinVault(t, mk).QueryAudit(store.AuditFilter{Actions: []string{action}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// pinAt returns the anchor stored at host:port (nil when unpinned).
func pinAt(t *testing.T, mk []byte, host string, port int) *store.Pin {
	t.Helper()
	pin, err := openPinVault(t, mk).GetHostKey(host, port)
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

// tofuPin seeds an automatic-first-trust blob anchor (the SaveHostKey path —
// column defaults render it blob/tofu).
func tofuPin(t *testing.T, mk []byte, host string, port int, key ssh.PublicKey) {
	t.Helper()
	if err := openPinVault(t, mk).SaveHostKey(host, port, key.Marshal()); err != nil {
		t.Fatal(err)
	}
}

// forwardedPin seeds a device-forwarded anchor.
func forwardedPin(t *testing.T, mk []byte, host string, port int, key ssh.PublicKey, device string) {
	t.Helper()
	_, err := openPinVault(t, mk).InsertForwardedPin(host, port, key.Marshal(), device,
		store.AuditRow{TS: time.Now(), Action: "pin-forward", Status: "ok"})
	if err != nil {
		t.Fatal(err)
	}
}

// keyscanFile writes the given known_hosts lines to a temp file.
func keyscanFile(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "kh")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPinHostkey_Display: 来源/格式/设备/时间齐备;无锚明说;未知条目报错。
func TestPinHostkey_Display(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	keyA, fpA := genHostKey(t)
	keyB, _ := genHostKey(t)
	seedPinServers(t, mk,
		models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"},
		models.Server{Name: "gamma", Host: "192.0.2.60", Port: 2222, User: "u"},
		models.Server{Name: "nopin", Host: "192.0.2.70", Port: 22, User: "u"},
	)
	tofuPin(t, mk, "192.0.2.50", 22, keyA)
	forwardedPin(t, mk, "192.0.2.60", 2222, keyB, "dev-pad")

	out := runCli(t, "servers", "pin-hostkey", "alpha")
	if !strings.Contains(out, "host=192.0.2.50:22 fp="+fpA) {
		t.Fatalf("display missing host/fp:\n%s", out)
	}
	if !strings.Contains(out, "(format=blob, source=tofu, device=-, created=") {
		t.Fatalf("display missing format/source/device/created:\n%s", out)
	}

	out = runCli(t, "servers", "pin-hostkey", "gamma")
	if !strings.Contains(out, "(format=blob, source=forward, device=dev-pad, created=") {
		t.Fatalf("forward pin display missing provenance:\n%s", out)
	}

	// 无锚明说。
	out = runCli(t, "servers", "pin-hostkey", "nopin")
	if !strings.Contains(out, "no pin at 192.0.2.70:22") {
		t.Fatalf("unpinned entry must be stated plainly:\n%s", out)
	}

	// 未知条目。
	runCliErr(t, "servers", "pin-hostkey", "ghost")

	// 显示不落审计。
	if n := len(pinAuditRows(t, mk, "pin-manual")) + len(pinAuditRows(t, mk, "pin-clear")); n != 0 {
		t.Fatalf("display must not audit, got %d rows", n)
	}
}

// TestPinHostkey_ListOrphan: --list 全量枚举,无条目指向的锚标 [orphan]。
func TestPinHostkey_ListOrphan(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	keyA, _ := genHostKey(t)
	keyOrphan, fpOrphan := genHostKey(t)
	seedPinServers(t, mk, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})
	tofuPin(t, mk, "192.0.2.50", 22, keyA)
	tofuPin(t, mk, "10.255.0.9", 22, keyOrphan) // no server row points here

	out := runCli(t, "servers", "pin-hostkey", "--list")
	var orphanLine, pinnedLine bool
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "10.255.0.9:22 fp="+fpOrphan):
			orphanLine = strings.Contains(l, " [orphan]")
		case strings.HasPrefix(l, "192.0.2.50:22 fp="):
			pinnedLine = !strings.Contains(l, "[orphan]")
		}
	}
	if !orphanLine || !pinnedLine {
		t.Fatalf("--list orphan marking wrong (orphan=%t pinned=%t):\n%s", orphanLine, pinnedLine, out)
	}

	out = runCli(t, "servers", "pin-hostkey", "--list")
	if strings.Count(out, "[orphan]") != 1 {
		t.Fatalf("exactly one orphan expected:\n%s", out)
	}
}

// TestPinHostkey_FingerprintStrict: SHA256:<base64 无填充> + 解码恰 32 字节双校验;
// 成功输出逐字;审计 pin-manual 命令逐字。
func TestPinHostkey_FingerprintStrict(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	_, fp := genHostKey(t)
	ids := seedPinServers(t, mk, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})

	// 格式错拒(缺前缀)。
	errText := runCliErr(t, "servers", "pin-hostkey", "alpha", "--fingerprint", "notafp")
	if !strings.Contains(errText, "SHA256:<base64") {
		t.Fatalf("format refusal must name the strict form:\n%s", errText)
	}
	// 截一位 → 解码 31 字节拒。
	errText = runCliErr(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fp[:len(fp)-1])
	if !strings.Contains(errText, "decodes to 31 bytes") {
		t.Fatalf("truncated fingerprint must be refused by the 32-byte check:\n%s", errText)
	}
	// 改一字符为非标准字母表 → base64 非法拒。
	errText = runCliErr(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fp[:10]+"_"+fp[11:])
	if !strings.Contains(errText, "base64 is invalid") {
		t.Fatalf("non-standard-alphabet character must be refused:\n%s", errText)
	}
	// 带填充拒。
	errText = runCliErr(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fp+"=")
	if !strings.Contains(errText, "base64 is invalid") {
		t.Fatalf("padded base64 must be refused:\n%s", errText)
	}
	if pinAt(t, mk, "192.0.2.50", 22) != nil {
		t.Fatal("refused fingerprints must not write a pin")
	}

	// 合法指纹 → 成功输出逐字 + 落库 format=fingerprint/source=manual + 审计逐字。
	out := runCli(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fp)
	want := "pinned alpha host=192.0.2.50:22 fp=" + fp + " (format=fingerprint, source=manual, forced=false)"
	if !strings.Contains(out, want) {
		t.Fatalf("success line verbatim mismatch:\nwant: %s\ngot:\n%s", want, out)
	}
	pin := pinAt(t, mk, "192.0.2.50", 22)
	if pin == nil || pin.Format != store.PinFormatFingerprint || string(pin.Blob) != fp {
		t.Fatalf("fingerprint anchor not stored as the string: %+v", pin)
	}
	rows := pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 pin-manual row, got %d", len(rows))
	}
	wantCmd := "host=192.0.2.50:22 fp=" + fp + " via=fingerprint forced=false"
	if rows[0].Command != wantCmd || rows[0].ServerID != ids["alpha"] || rows[0].ProjectID != "" || rows[0].Status != "ok" {
		t.Fatalf("pin-manual audit row mismatch:\nwant cmd %q\n got cmd %q\nrow: %+v", wantCmd, rows[0].Command, rows[0])
	}
}

// TestPinHostkey_ExistingRefusalAndForce: 已有锚无 --force 拒并显示现存指纹与来源;
// --force 单事务读旧→覆盖→审计(was= 事务内旧指纹,链式为真)。
func TestPinHostkey_ExistingRefusalAndForce(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	keyA, fpA := genHostKey(t)
	_, fpB := genHostKey(t)
	_, fpC := genHostKey(t)
	seedPinServers(t, mk, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})
	tofuPin(t, mk, "192.0.2.50", 22, keyA)

	// 无 --force → 拒,显示现存 fp+来源,零写入零审计。
	errText := runCliErr(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fpB)
	want := "refusing to replace the existing pin for 192.0.2.50:22: fp=" + fpA + " (source=tofu) — pass --force to replace it"
	if !strings.Contains(errText, want) {
		t.Fatalf("refusal verbatim mismatch:\nwant: %s\ngot:\n%s", want, errText)
	}
	if pin := pinAt(t, mk, "192.0.2.50", 22); pin == nil || string(pin.Blob) == fpB {
		t.Fatal("refusal must leave the incumbent pin untouched")
	}
	if n := len(pinAuditRows(t, mk, "pin-manual")); n != 0 {
		t.Fatalf("refusal must not audit, got %d rows", n)
	}

	// --force → 输出先旧后新,审计 was==事务内读到的旧锚。
	out := runCli(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fpB, "--force")
	if !strings.Contains(out, "replacing existing pin fp="+fpA+" (source=tofu)") {
		t.Fatalf("force output must show the old pin first:\n%s", out)
	}
	if !strings.Contains(out, "pinned alpha host=192.0.2.50:22 fp="+fpB+" (format=fingerprint, source=manual, forced=true)") {
		t.Fatalf("force success line wrong:\n%s", out)
	}
	rows := pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 1 || rows[0].Command != "host=192.0.2.50:22 fp="+fpB+" via=fingerprint forced=true was="+fpA {
		t.Fatalf("force audit must carry in-tx old→new: %+v", rows)
	}

	// 第二次 --force:审计链第二次的 was= 必须是第一次落下的新指纹。
	runCli(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fpC, "--force")
	rows = pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 2 {
		t.Fatalf("want 2 pin-manual rows, got %d", len(rows))
	}
	// rows are newest-first; rows[0] is the SECOND force.
	wantCmd := "host=192.0.2.50:22 fp=" + fpC + " via=fingerprint forced=true was=" + fpB
	if rows[0].Command != wantCmd {
		t.Fatalf("second force was= must be the first force's new fp:\nwant %q\ngot  %q", wantCmd, rows[0].Command)
	}
}

// TestPinHostkey_ForceFreeSlot: --force 落在空槽 → 无 replacing 行、审计 forced=true 且无 was=。
func TestPinHostkey_ForceFreeSlot(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	_, fp := genHostKey(t)
	seedPinServers(t, mk, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})

	out := runCli(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fp, "--force")
	if strings.Contains(out, "replacing") {
		t.Fatalf("free slot must not claim a replacement:\n%s", out)
	}
	if !strings.Contains(out, "forced=true") {
		t.Fatalf("forced flag must show:\n%s", out)
	}
	rows := pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 1 || rows[0].Command != "host=192.0.2.50:22 fp="+fp+" via=fingerprint forced=true" {
		t.Fatalf("free-slot force audit must omit was=: %+v", rows)
	}
}

// TestPinHostkey_ClearShared: 有锚 clear 删全局锚+输出/审计带受影响清单;再 clear 幂等成功零审计。
func TestPinHostkey_ClearShared(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	keyA, fpA := genHostKey(t)
	keyG, _ := genHostKey(t)
	ids := seedPinServers(t, mk,
		models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"},
		models.Server{Name: "beta", Host: "192.0.2.50", Port: 22, User: "u"}, // same address
		models.Server{Name: "gamma", Host: "192.0.2.60", Port: 2222, User: "u"},
	)
	tofuPin(t, mk, "192.0.2.50", 22, keyA)
	tofuPin(t, mk, "192.0.2.60", 2222, keyG)

	out := runCli(t, "servers", "pin-hostkey", "alpha", "--clear")
	want := "unpinned host=192.0.2.50:22 (was fp=" + fpA + ", source=tofu) — affects 2 entries: alpha, beta"
	if !strings.Contains(out, want) {
		t.Fatalf("clear output verbatim mismatch:\nwant: %s\ngot:\n%s", want, out)
	}
	if pinAt(t, mk, "192.0.2.50", 22) != nil {
		t.Fatal("anchor must be gone after clear")
	}
	if pinAt(t, mk, "192.0.2.60", 2222) == nil {
		t.Fatal("clear must not touch other addresses")
	}
	rows := pinAuditRows(t, mk, "pin-clear")
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 pin-clear row, got %d", len(rows))
	}
	wantCmd := "host=192.0.2.50:22 fp=" + fpA + " source=tofu affects=2 entries: alpha, beta"
	if rows[0].Command != wantCmd || rows[0].ServerID != ids["alpha"] || rows[0].ProjectID != "" || rows[0].Status != "ok" {
		t.Fatalf("pin-clear audit row mismatch: want cmd %q, got %q (%+v)", wantCmd, rows[0].Command, rows[0])
	}

	// 幂等:无锚 clear 成功、明说、零新增审计。
	out = runCli(t, "servers", "pin-hostkey", "alpha", "--clear")
	if !strings.Contains(out, "no pin present at 192.0.2.50:22 (nothing cleared)") {
		t.Fatalf("idempotent clear output wrong:\n%s", out)
	}
	if n := len(pinAuditRows(t, mk, "pin-clear")); n != 1 {
		t.Fatalf("absent clear must not audit, total rows %d", n)
	}
}

// TestPinHostkey_ClearHostport: 孤儿锚直达清除(无位置参数,affects 可为 0);
// --hostport 形态的旗标纪律。
func TestPinHostkey_ClearHostport(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	keyO, fpO := genHostKey(t)
	seedPinServers(t, mk, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})
	tofuPin(t, mk, "10.255.0.9", 22, keyO) // orphan: no server row

	out := runCli(t, "servers", "pin-hostkey", "--clear", "--hostport", "10.255.0.9:22")
	// 逐字整行:空清单渲染为裸 "affects 0 entries"(无冒号、无占位符)。
	want := "unpinned host=10.255.0.9:22 (was fp=" + fpO + ", source=tofu) — affects 0 entries\n"
	if !strings.Contains(out, want) {
		t.Fatalf("orphan clear output verbatim mismatch:\nwant: %q\ngot:\n%s", want, out)
	}
	if pinAt(t, mk, "10.255.0.9", 22) != nil {
		t.Fatal("orphan anchor must be gone")
	}
	rows := pinAuditRows(t, mk, "pin-clear")
	if len(rows) != 1 || rows[0].Command != "host=10.255.0.9:22 fp="+fpO+" source=tofu affects=0 entries" {
		t.Fatalf("orphan clear audit wrong: %+v", rows)
	}
	if rows[0].ServerID != "" {
		t.Fatalf("hostport form has no server attribution, got %q", rows[0].ServerID)
	}

	// 旗标纪律。
	runCliErr(t, "servers", "pin-hostkey", "--hostport", "10.255.0.9:22")                     // without --clear
	runCliErr(t, "servers", "pin-hostkey", "alpha", "--clear", "--hostport", "10.255.0.9:22") // positional + hostport
	runCliErr(t, "servers", "pin-hostkey", "--clear", "--hostport", "noport")                 // malformed
}

// TestPinHostkey_Keyscan: 单行成功(22 裸主机名/[host]:port 各一)、同钥重复去重、
// 多钥拒、|1| 与 @ 行跳过计数、无匹配错误文案、stdin 形态。
func TestPinHostkey_Keyscan(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	keyA, fpA := genHostKey(t)
	keyG, fpG := genHostKey(t)
	seedPinServers(t, mk,
		models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"},
		models.Server{Name: "gamma", Host: "192.0.2.60", Port: 2222, User: "u"},
	)
	lineA := "192.0.2.50 " + keyA.Type() + " " + base64.StdEncoding.EncodeToString(keyA.Marshal())
	lineG := "[192.0.2.60]:2222 " + keyG.Type() + " " + base64.StdEncoding.EncodeToString(keyG.Marshal())
	lineBare := "192.0.2.60 " + keyG.Type() + " " + base64.StdEncoding.EncodeToString(keyG.Marshal())

	// 单行(22 → 裸主机名)成功;key_blob=真实密钥字节;审计 via=keyscan。
	out := runCli(t, "servers", "pin-hostkey", "alpha", "--from-keyscan", keyscanFile(t, lineA))
	if !strings.Contains(out, "pinned alpha host=192.0.2.50:22 fp="+fpA+" (format=blob, source=manual, forced=false)") {
		t.Fatalf("keyscan success line wrong:\n%s", out)
	}
	if pin := pinAt(t, mk, "192.0.2.50", 22); pin == nil || pin.Format != store.PinFormatBlob {
		t.Fatalf("keyscan anchor must be a blob: %+v", pin)
	}
	rows := pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 1 || rows[0].Command != "host=192.0.2.50:22 fp="+fpA+" via=keyscan forced=false" {
		t.Fatalf("keyscan audit wrong: %+v", rows)
	}

	// 非 22 端口只认 [host]:port 形态;裸形态行不匹配。
	runCliErr(t, "servers", "pin-hostkey", "gamma", "--from-keyscan", keyscanFile(t, lineBare))
	out = runCli(t, "servers", "pin-hostkey", "gamma", "--from-keyscan", keyscanFile(t, lineG, lineBare))
	if !strings.Contains(out, "pinned gamma host=192.0.2.60:2222 fp="+fpG) {
		t.Fatalf("bracketed keyscan success wrong:\n%s", out)
	}

	// 多钥(同 patterns 多算法行)→ 拒 + 自愈指引;零改写零审计。
	other, _ := genHostKey(t)
	lineA2 := "192.0.2.50 " + other.Type() + " " + base64.StdEncoding.EncodeToString(other.Marshal())
	errText := runCliErr(t, "servers", "pin-hostkey", "alpha", "--from-keyscan",
		keyscanFile(t, lineA, lineA2))
	for _, want := range []string{"2 different keys match 192.0.2.50", "forwarded automatically", "--fingerprint", "--force"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("multi-key refusal missing %q:\n%s", want, errText)
		}
	}
	pin := pinAt(t, mk, "192.0.2.50", 22)
	if pin == nil || string(pin.Blob) != string(keyA.Marshal()) {
		t.Fatal("multi-key refusal must leave the existing anchor untouched")
	}

	// |1| 哈希行与 @cert-authority 行跳过计数,不误匹配。
	hashed := "|1|c2FsdA==|aGFzaA== " + other.Type() + " " + base64.StdEncoding.EncodeToString(other.Marshal())
	marker := "@cert-authority 192.0.2.50 " + other.Type() + " " + base64.StdEncoding.EncodeToString(other.Marshal())
	_, mkBeta := withCliStoreEnv(t)
	seedPinServers(t, mkBeta, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})
	out = runCli(t, "servers", "pin-hostkey", "alpha", "--from-keyscan", keyscanFile(t, hashed, marker, lineA))
	if !strings.Contains(out, "skipped 1 hashed-hostname (|1|) and 1 marker (@) lines") {
		t.Fatalf("skip counts must be noted:\n%s", out)
	}
	if !strings.Contains(out, "pinned alpha host=192.0.2.50:22 fp="+fpA) {
		t.Fatalf("good line among skipped ones must still pin:\n%s", out)
	}

	// 无匹配 → 错误列出文件内形态与期望形态。
	errText = runCliErr(t, "servers", "pin-hostkey", "alpha", "--from-keyscan", keyscanFile(t, lineBare))
	if !strings.Contains(errText, "no known_hosts line matches 192.0.2.50") ||
		!strings.Contains(errText, "forms found: [192.0.2.60]") ||
		!strings.Contains(errText, "expected form: 192.0.2.50") {
		t.Fatalf("no-match error must list file forms vs expected form:\n%s", errText)
	}

	// 同钥重复行 → 去重后按单钥成功。
	_, mkDup := withCliStoreEnv(t)
	seedPinServers(t, mkDup, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})
	out = runCli(t, "servers", "pin-hostkey", "alpha", "--from-keyscan", keyscanFile(t, lineA, lineA))
	if !strings.Contains(out, "pinned alpha host=192.0.2.50:22 fp="+fpA) {
		t.Fatalf("duplicate same-key lines must dedupe to one pin:\n%s", out)
	}

	// "-" = stdin。
	f, err := os.Open(keyscanFile(t, lineA))
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = oldStdin; f.Close() })
	_, mkStdin := withCliStoreEnv(t)
	seedPinServers(t, mkStdin, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})
	out = runCli(t, "servers", "pin-hostkey", "alpha", "--from-keyscan", "-")
	if !strings.Contains(out, "pinned alpha host=192.0.2.50:22 fp="+fpA) {
		t.Fatalf("stdin keyscan must work:\n%s", out)
	}
}

// TestPinHostkey_FlagConflicts: 互斥与形态纪律全部在触库前拒绝。
func TestPinHostkey_FlagConflicts(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	_, fp := genHostKey(t)
	seedPinServers(t, mk, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})

	cases := [][]string{
		{"servers", "pin-hostkey", "alpha", "--clear", "--fingerprint", fp},
		{"servers", "pin-hostkey", "alpha", "--clear", "--from-keyscan", "x"},
		{"servers", "pin-hostkey", "alpha", "--clear", "--force"},
		{"servers", "pin-hostkey", "--list", "--clear"},
		{"servers", "pin-hostkey", "alpha", "--fingerprint", fp, "--from-keyscan", "x"},
		{"servers", "pin-hostkey", "--hostport", "1.2.3.4:22"},
		{"servers", "pin-hostkey", "alpha", "--force"},
		{"servers", "pin-hostkey", "--list", "alpha"},
		{"servers", "pin-hostkey"},
		{"servers", "pin-hostkey", "alpha", "beta", "--fingerprint", fp},
	}
	for _, args := range cases {
		errText := runCliErr(t, args...)
		if strings.TrimSpace(errText) == "" {
			t.Fatalf("expected a refusal for %v", args)
		}
	}
	if pinAt(t, mk, "192.0.2.50", 22) != nil {
		t.Fatal("conflict refusals must not write any pin")
	}
	for _, action := range []string{"pin-manual", "pin-clear"} {
		if n := len(pinAuditRows(t, mk, action)); n != 0 {
			t.Fatalf("conflict refusals must not audit %s, got %d rows", action, n)
		}
	}
}
