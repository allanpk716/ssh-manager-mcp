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
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/store"
)

// `servers pin-hostkey`（票 #58）：命令对象是服务器条目，显示、列出学习
// 地址、手工锚定与 --force、按条目清除；旧的按主机端口清除入口不再存在。

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

// seedPinServers adds the named servers to the env vault and returns name→id.
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

func pinAuditRows(t *testing.T, mk []byte, action string) []store.AuditRow {
	t.Helper()
	rows, err := openPinVault(t, mk).QueryAudit(store.AuditFilter{Actions: []string{action}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// pinServerOf 取名字对应的条目模型。
func pinServerOf(t *testing.T, mk []byte, name string) *models.Server {
	t.Helper()
	srv, err := openPinVault(t, mk).GetServerByName(name)
	if err != nil || srv == nil {
		t.Fatalf("取条目 %s 失败：%v %v", name, srv, err)
	}
	return srv
}

// entryPinAt 取条目当前的锚（无锚时为 nil）。
func entryPinAt(t *testing.T, mk []byte, name string) *store.Pin {
	t.Helper()
	pin, err := openPinVault(t, mk).GetEntryPin(pinServerOf(t, mk, name))
	if err != nil {
		t.Fatal(err)
	}
	return pin
}

// tofuPin 给条目种一把首次信任来源的锚。
func tofuPin(t *testing.T, mk []byte, name string, key ssh.PublicKey) {
	t.Helper()
	if err := openPinVault(t, mk).SaveEntryPin(pinServerOf(t, mk, name), key.Marshal()); err != nil {
		t.Fatal(err)
	}
}

// forwardedPin 给条目种一把设备转发来源的锚。
func forwardedPin(t *testing.T, mk []byte, name string, key ssh.PublicKey, device string) {
	t.Helper()
	_, err := openPinVault(t, mk).InsertForwardedEntryPin(pinServerOf(t, mk, name),
		"192.0.2.60:2222", key.Marshal(), device,
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

// TestPinHostkey_Display：按条目显示锚、指纹、来源（经哪个地址学到、由哪台
// 设备转发）和锚定时间；无锚明说；未知条目报错；显示不落审计。
func TestPinHostkey_Display(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	keyA, fpA := genHostKey(t)
	keyB, _ := genHostKey(t)
	seedPinServers(t, mk,
		models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"},
		models.Server{Name: "gamma", Host: "192.0.2.60", Port: 2222, User: "u"},
		models.Server{Name: "nopin", Host: "192.0.2.70", Port: 22, User: "u"},
	)
	tofuPin(t, mk, "alpha", keyA)
	forwardedPin(t, mk, "gamma", keyB, "dev-pad")

	out := runCli(t, "servers", "pin-hostkey", "alpha")
	if !strings.Contains(out, "entry=alpha host=192.0.2.50:22 fingerprint="+fpA) {
		t.Fatalf("display missing entry/host/fp:\n%s", out)
	}
	if !strings.Contains(out, "(format=blob, source=tofu, device=-, handshake=192.0.2.50:22, created=") {
		t.Fatalf("display missing format/source/device/learned/created:\n%s", out)
	}

	out = runCli(t, "servers", "pin-hostkey", "gamma")
	if !strings.Contains(out, "(format=blob, source=forward, device=dev-pad, handshake=192.0.2.60:2222, created=") {
		t.Fatalf("forward pin display missing provenance:\n%s", out)
	}

	out = runCli(t, "servers", "pin-hostkey", "nopin")
	if !strings.Contains(out, "no pin for nopin (unanchored") {
		t.Fatalf("unpinned entry must be stated plainly:\n%s", out)
	}

	runCliErr(t, "servers", "pin-hostkey", "ghost")
	if n := len(pinAuditRows(t, mk, "pin-manual")) + len(pinAuditRows(t, mk, "pin-clear")); n != 0 {
		t.Fatalf("display must not audit, got %d rows", n)
	}
}

// TestPinHostkey_List：--list 按条目列出全部锚并显示握手地址；同地址的两个
// 条目各占一行；不再有「孤立」标记。
func TestPinHostkey_List(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	keyA, fpA := genHostKey(t)
	keyB, _ := genHostKey(t)
	seedPinServers(t, mk,
		models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"},
		models.Server{Name: "beta", Host: "192.0.2.50", Port: 22, User: "u"},
		models.Server{Name: "nopin", Host: "192.0.2.70", Port: 22, User: "u"},
	)
	tofuPin(t, mk, "alpha", keyA)
	tofuPin(t, mk, "beta", keyB)

	out := runCli(t, "servers", "pin-hostkey", "--list")
	var alphaLine, betaLine bool
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		switch {
		case strings.HasPrefix(l, "entry=alpha host=192.0.2.50:22 fingerprint="+fpA):
			alphaLine = strings.Contains(l, "handshake=192.0.2.50:22")
		case strings.HasPrefix(l, "entry=beta host=192.0.2.50:22 fingerprint="):
			betaLine = !strings.Contains(l, fpA)
		}
	}
	if !alphaLine || !betaLine {
		t.Fatalf("--list must show both entries with their own anchors (alpha=%t beta=%t):\n%s", alphaLine, betaLine, out)
	}
	if strings.Contains(out, "orphan") || strings.Contains(out, "nopin") {
		t.Fatalf("--list must not show orphan markers or unpinned entries:\n%s", out)
	}
}

// TestPinHostkey_FingerprintStrict：指纹形态双校验；成功输出与审计逐字。
func TestPinHostkey_FingerprintStrict(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	_, fp := genHostKey(t)
	ids := seedPinServers(t, mk, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})

	errText := runCliErr(t, "servers", "pin-hostkey", "alpha", "--fingerprint", "notafp")
	if !strings.Contains(errText, "SHA256:<base64") {
		t.Fatalf("format refusal must name the strict form:\n%s", errText)
	}
	errText = runCliErr(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fp[:len(fp)-1])
	if !strings.Contains(errText, "decodes to 31 bytes") {
		t.Fatalf("truncated fingerprint must be refused:\n%s", errText)
	}
	if entryPinAt(t, mk, "alpha") != nil {
		t.Fatal("refused fingerprints must not write a pin")
	}

	out := runCli(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fp)
	want := "pinned alpha host=192.0.2.50:22 fingerprint=" + fp + " (format=fingerprint, source=manual, handshake=-, forced=false)"
	if !strings.Contains(out, want) {
		t.Fatalf("success line mismatch:\nwant: %s\ngot:\n%s", want, out)
	}
	pin := entryPinAt(t, mk, "alpha")
	if pin == nil || pin.Format != store.PinFormatFingerprint || string(pin.Blob) != fp {
		t.Fatalf("fingerprint anchor not stored as the string: %+v", pin)
	}
	rows := pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 pin-manual row, got %d", len(rows))
	}
	wantCmd := "entry=alpha host=192.0.2.50:22 fingerprint=" + fp + " via=fingerprint forced=false"
	if rows[0].Command != wantCmd || rows[0].ServerID != ids["alpha"] || rows[0].ProjectID != "" || rows[0].Status != "ok" {
		t.Fatalf("pin-manual audit row mismatch:\nwant cmd %q\n got cmd %q\nrow: %+v", wantCmd, rows[0].Command, rows[0])
	}
}

// TestPinHostkey_ExistingRefusalAndForce：已有锚不加 --force 拒绝并显示现存
// 指纹与来源；--force 覆盖，审计带事务内读到的旧指纹；同一设备、同一端口的其他服务器条目的锚
// 不受影响。
func TestPinHostkey_ExistingRefusalAndForce(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	keyA, fpA := genHostKey(t)
	_, fpB := genHostKey(t)
	_, fpC := genHostKey(t)
	seedPinServers(t, mk,
		models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"},
		models.Server{Name: "beta", Host: "192.0.2.50", Port: 22, User: "u"},
	)
	tofuPin(t, mk, "alpha", keyA)
	tofuPin(t, mk, "beta", keyA)

	errText := runCliErr(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fpB)
	want := "refusing to replace the existing pin for alpha: fingerprint=" + fpA + " (source=tofu) — pass --force to replace it"
	if !strings.Contains(errText, want) {
		t.Fatalf("refusal mismatch:\nwant: %s\ngot:\n%s", want, errText)
	}
	if pin := entryPinAt(t, mk, "alpha"); pin == nil || string(pin.Blob) == fpB {
		t.Fatal("refusal must leave the incumbent pin untouched")
	}
	if n := len(pinAuditRows(t, mk, "pin-manual")); n != 0 {
		t.Fatalf("refusal must not audit, got %d rows", n)
	}

	out := runCli(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fpB, "--force")
	if !strings.Contains(out, "replacing existing pin fingerprint="+fpA+" (source=tofu)") {
		t.Fatalf("force output must show the old pin first:\n%s", out)
	}
	if !strings.Contains(out, "pinned alpha host=192.0.2.50:22 fingerprint="+fpB+" (format=fingerprint, source=manual, handshake=-, forced=true)") {
		t.Fatalf("force success line wrong:\n%s", out)
	}
	rows := pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 1 || rows[0].Command != "entry=alpha host=192.0.2.50:22 fingerprint="+fpB+" via=fingerprint forced=true was="+fpA {
		t.Fatalf("force audit must carry in-tx old→new: %+v", rows)
	}
	// 同设备同端口的其他服务器条目的锚不受覆盖影响。
	if pin := entryPinAt(t, mk, "beta"); pin == nil || !pin.Matches(keyA.Marshal()) {
		t.Fatalf("beta's anchor must survive alpha's force: %+v", pin)
	}

	runCli(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fpC, "--force")
	rows = pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 2 || rows[0].Command != "entry=alpha host=192.0.2.50:22 fingerprint="+fpC+" via=fingerprint forced=true was="+fpB {
		t.Fatalf("second force was= must be the first force's new fp: %+v", rows)
	}
}

// TestPinHostkey_ForceFreeSlot：空槽 --force 无 replacing 行、审计无 was=。
func TestPinHostkey_ForceFreeSlot(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	_, fp := genHostKey(t)
	seedPinServers(t, mk, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})

	out := runCli(t, "servers", "pin-hostkey", "alpha", "--fingerprint", fp, "--force")
	if strings.Contains(out, "replacing") {
		t.Fatalf("free slot must not claim a replacement:\n%s", out)
	}
	rows := pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 1 || rows[0].Command != "entry=alpha host=192.0.2.50:22 fingerprint="+fp+" via=fingerprint forced=true" {
		t.Fatalf("free-slot force audit must omit was=: %+v", rows)
	}
}

// TestPinHostkey_Clear：--clear 按条目删除锚并写审计；同一设备、同一端口的其他服务器条目与其它
// 条目不受影响；重复清除幂等。
func TestPinHostkey_Clear(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	keyA, fpA := genHostKey(t)
	keyG, _ := genHostKey(t)
	ids := seedPinServers(t, mk,
		models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"},
		models.Server{Name: "beta", Host: "192.0.2.50", Port: 22, User: "u"},
		models.Server{Name: "gamma", Host: "192.0.2.60", Port: 2222, User: "u"},
	)
	tofuPin(t, mk, "alpha", keyA)
	tofuPin(t, mk, "beta", keyA)
	tofuPin(t, mk, "gamma", keyG)

	out := runCli(t, "servers", "pin-hostkey", "alpha", "--clear")
	want := "unpinned alpha host=192.0.2.50:22 (was fingerprint=" + fpA + ", source=tofu, handshake=192.0.2.50:22)"
	if !strings.Contains(out, want) {
		t.Fatalf("clear output mismatch:\nwant: %s\ngot:\n%s", want, out)
	}
	if entryPinAt(t, mk, "alpha") != nil {
		t.Fatal("alpha's anchor must be gone after clear")
	}
	if entryPinAt(t, mk, "beta") == nil || entryPinAt(t, mk, "gamma") == nil {
		t.Fatal("clear must not touch other entries' anchors")
	}
	rows := pinAuditRows(t, mk, "pin-clear")
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 pin-clear row, got %d", len(rows))
	}
	wantCmd := "entry=alpha host=192.0.2.50:22 fingerprint=" + fpA + " source=tofu handshake=192.0.2.50:22"
	if rows[0].Command != wantCmd || rows[0].ServerID != ids["alpha"] || rows[0].ProjectID != "" || rows[0].Status != "ok" {
		t.Fatalf("pin-clear audit row mismatch: want cmd %q, got %q (%+v)", wantCmd, rows[0].Command, rows[0])
	}

	out = runCli(t, "servers", "pin-hostkey", "alpha", "--clear")
	if !strings.Contains(out, "no pin for alpha (nothing cleared)") {
		t.Fatalf("idempotent clear output wrong:\n%s", out)
	}
	if n := len(pinAuditRows(t, mk, "pin-clear")); n != 1 {
		t.Fatalf("absent clear must not audit, total rows %d", n)
	}
}

// TestPinHostkey_ClearHostportRemoved：按主机端口清除孤立锚的入口不再存在。
func TestPinHostkey_ClearHostportRemoved(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	seedPinServers(t, mk, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})

	errText := runCliErr(t, "servers", "pin-hostkey", "--clear", "--hostport", "10.255.0.9:22")
	if errText == "" {
		t.Fatal("--clear --hostport must no longer exist")
	}
	if !strings.Contains(errText, "hostport") {
		t.Fatalf("the refusal should name the removed flag:\n%s", errText)
	}
	if n := len(pinAuditRows(t, mk, "pin-clear")); n != 0 {
		t.Fatalf("nothing may be cleared, got %d rows", n)
	}
}

// TestPinHostkey_Keyscan：单行成功、同钥重复去重、多钥拒、|1| 与 @ 行跳过
// 计数、无匹配错误文案、stdin 形态。
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

	out := runCli(t, "servers", "pin-hostkey", "alpha", "--from-keyscan", keyscanFile(t, lineA))
	if !strings.Contains(out, "pinned alpha host=192.0.2.50:22 fingerprint="+fpA+" (format=blob, source=manual, handshake=192.0.2.50:22, forced=false)") {
		t.Fatalf("keyscan success line wrong:\n%s", out)
	}
	if pin := entryPinAt(t, mk, "alpha"); pin == nil || pin.Format != store.PinFormatBlob {
		t.Fatalf("keyscan anchor must be a blob: %+v", pin)
	}
	rows := pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 1 || rows[0].Command != "entry=alpha host=192.0.2.50:22 fingerprint="+fpA+" via=keyscan forced=false" {
		t.Fatalf("keyscan audit wrong: %+v", rows)
	}

	runCliErr(t, "servers", "pin-hostkey", "gamma", "--from-keyscan", keyscanFile(t, lineBare))
	out = runCli(t, "servers", "pin-hostkey", "gamma", "--from-keyscan", keyscanFile(t, lineG, lineBare))
	if !strings.Contains(out, "pinned gamma host=192.0.2.60:2222 fingerprint="+fpG) {
		t.Fatalf("bracketed keyscan success wrong:\n%s", out)
	}

	other, _ := genHostKey(t)
	lineA2 := "192.0.2.50 " + other.Type() + " " + base64.StdEncoding.EncodeToString(other.Marshal())
	errText := runCliErr(t, "servers", "pin-hostkey", "alpha", "--from-keyscan",
		keyscanFile(t, lineA, lineA2))
	for _, want := range []string{"2 different keys match 192.0.2.50", "forwarded automatically", "--fingerprint", "--force"} {
		if !strings.Contains(errText, want) {
			t.Fatalf("multi-key refusal missing %q:\n%s", want, errText)
		}
	}
	if pin := entryPinAt(t, mk, "alpha"); pin == nil || string(pin.Blob) != string(keyA.Marshal()) {
		t.Fatal("multi-key refusal must leave the existing anchor untouched")
	}

	hashed := "|1|c2FsdA==|aGFzaA== " + other.Type() + " " + base64.StdEncoding.EncodeToString(other.Marshal())
	marker := "@cert-authority 192.0.2.50 " + other.Type() + " " + base64.StdEncoding.EncodeToString(other.Marshal())
	_, mkBeta := withCliStoreEnv(t)
	seedPinServers(t, mkBeta, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})
	out = runCli(t, "servers", "pin-hostkey", "alpha", "--from-keyscan", keyscanFile(t, hashed, marker, lineA))
	if !strings.Contains(out, "skipped 1 hashed-hostname (|1|) and 1 marker (@) lines") {
		t.Fatalf("skip counts must be noted:\n%s", out)
	}

	errText = runCliErr(t, "servers", "pin-hostkey", "alpha", "--from-keyscan", keyscanFile(t, lineBare))
	if !strings.Contains(errText, "no known_hosts line matches 192.0.2.50") ||
		!strings.Contains(errText, "forms found: [192.0.2.60]") ||
		!strings.Contains(errText, "expected form: 192.0.2.50") {
		t.Fatalf("no-match error must list file forms vs expected form:\n%s", errText)
	}

	_, mkDup := withCliStoreEnv(t)
	seedPinServers(t, mkDup, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})
	out = runCli(t, "servers", "pin-hostkey", "alpha", "--from-keyscan", keyscanFile(t, lineA, lineA))
	if !strings.Contains(out, "pinned alpha host=192.0.2.50:22 fingerprint="+fpA) {
		t.Fatalf("duplicate same-key lines must dedupe to one pin:\n%s", out)
	}

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
	if !strings.Contains(out, "pinned alpha host=192.0.2.50:22 fingerprint="+fpA) {
		t.Fatalf("stdin keyscan must work:\n%s", out)
	}
}

// TestPinHostkey_FlagConflicts：互斥与形态纪律全部在触库前拒绝。
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
	if entryPinAt(t, mk, "alpha") != nil {
		t.Fatal("conflict refusals must not write any pin")
	}
	for _, action := range []string{"pin-manual", "pin-clear"} {
		if n := len(pinAuditRows(t, mk, action)); n != 0 {
			t.Fatalf("conflict refusals must not audit %s, got %d rows", action, n)
		}
	}
}

// TestPinHostkeyListMatchesWebUIAnchorWrites：Web UI 详情页的锚覆盖与删除走
// 服务器管理模块的同一份存储写入口；pin-hostkey --list 的输出必须与页面背
// 后的数据一致。
func TestPinHostkeyListMatchesWebUIAnchorWrites(t *testing.T) {
	_, mk := withCliStoreEnv(t)
	seedPinServers(t, mk, models.Server{Name: "alpha", Host: "192.0.2.50", Port: 22, User: "u"})
	_, fp := genHostKey(t)

	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mk)
	if err != nil {
		t.Fatal(err)
	}
	manager := servermanage.New(st)
	found, err := manager.ReplaceHostKeyPin("alpha", fp, false, servermanage.WebUISource("127.0.0.1:12345"))
	if err != nil || !found {
		t.Fatalf("Web UI 覆盖锚失败：found=%t err=%v", found, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	out := runCli(t, "servers", "pin-hostkey", "--list")
	for _, want := range []string{"entry=alpha", "fingerprint=" + fp, "source=manual", "handshake=-"} {
		if !strings.Contains(out, want) {
			t.Fatalf("--list 应与 Web UI 覆盖结果一致，缺少 %q：\n%s", want, out)
		}
	}
	rows := pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 1 || rows[0].Source != store.AuditSourceWebUI {
		t.Fatalf("Web UI 覆盖审计应标记 Web UI 来源：%+v", rows)
	}

	st, err = store.Open(os.Getenv("SSHMGR_STORE"), mk)
	if err != nil {
		t.Fatal(err)
	}
	found, err = servermanage.New(st).ClearHostKeyPin("alpha", servermanage.WebUISource("127.0.0.1:12345"))
	if err != nil || !found {
		t.Fatalf("Web UI 删除锚失败：found=%t err=%v", found, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	out = runCli(t, "servers", "pin-hostkey", "--list")
	if !strings.Contains(out, "no pins recorded") || strings.Contains(out, fp) {
		t.Fatalf("Web UI 删除后 --list 不应再列出这把锚：\n%s", out)
	}
}
