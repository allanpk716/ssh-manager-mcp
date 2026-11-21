package cli

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// seedEmptyHostPinVault 复用首选地址的拓扑场景（本机与工作站同在物理局域
// 网，首选地址 192.0.2.5），另返回 master key 供审计断言重新打开 vault。
func seedEmptyHostPinVault(t *testing.T) []byte {
	t.Helper()
	seedPreferredAddressVault(t)
	mk, err := hex.DecodeString(os.Getenv("SSHMGR_MASTERKEY_HEX"))
	if err != nil {
		t.Fatal(err)
	}
	return mk
}

// TestPinHostkey_EmptyHostDisplayAndAudit：空主机条目的显示与 --list 用首
// 选网络地址；锚定、清除的审计行 host=-；无候选时退化「user (port N)」。
func TestPinHostkey_EmptyHostDisplayAndAudit(t *testing.T) {
	mk := seedEmptyHostPinVault(t)
	key, _ := genHostKey(t)
	tofuPin(t, mk, "裸条目", key)

	out := runCli(t, "servers", "pin-hostkey", "裸条目")
	if !strings.Contains(out, "entry=裸条目 host=192.0.2.5:22") {
		t.Fatalf("空主机条目的单条显示应用首选地址：\n%s", out)
	}
	if strings.Contains(out, "host=:22") {
		t.Fatalf("显示不得出现假地址 :22：\n%s", out)
	}

	list := runCli(t, "servers", "pin-hostkey", "--list")
	if !strings.Contains(list, "entry=裸条目 host=192.0.2.5:22") {
		t.Fatalf("--list 应用首选地址：\n%s", list)
	}
	if strings.Contains(list, "host=:22") {
		t.Fatalf("--list 不得出现假地址 :22：\n%s", list)
	}

	out = runCli(t, "servers", "pin-hostkey", "裸条目", "--clear")
	if !strings.Contains(out, "unpinned 裸条目 host=192.0.2.5:22") {
		t.Fatalf("清除输出应用首选地址：\n%s", out)
	}
	for _, row := range pinAuditRows(t, mk, "pin-clear") {
		if !strings.Contains(row.Command, "host=- ") {
			t.Fatalf("空主机条目的清除审计行应为 host=-：%s", row.Command)
		}
	}
}

// TestPinHostkey_EmptyHostManualAudit：--fingerprint 锚定空主机条目时显示
// 行用首选地址，pin-manual 审计行 host=-。
func TestPinHostkey_EmptyHostManualAudit(t *testing.T) {
	mk := seedEmptyHostPinVault(t)
	_, fingerprint := genHostKey(t)

	out := runCli(t, "servers", "pin-hostkey", "裸条目", "--fingerprint", fingerprint)
	if !strings.Contains(out, "pinned 裸条目 host=192.0.2.5:22") {
		t.Fatalf("锚定输出应用首选地址：\n%s", out)
	}
	rows := pinAuditRows(t, mk, "pin-manual")
	if len(rows) != 1 {
		t.Fatalf("应恰好一条 pin-manual 审计，实际 %d", len(rows))
	}
	if !strings.Contains(rows[0].Command, "host=- ") {
		t.Fatalf("空主机条目的锚定审计行应为 host=-：%s", rows[0].Command)
	}
}

// TestPinHostkey_EmptyHostWithoutCandidateDegenerates：无局域网候选时空主
// 机条目显示退为「user (port N)」，不出现假地址。
func TestPinHostkey_EmptyHostWithoutCandidateDegenerates(t *testing.T) {
	configRoot := t.TempDir()
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	_, mk := withCliStoreEnv(t)
	seedPinServers(t, mk, models.Server{Name: "孤立条目", Host: "", Port: 22, User: "ops"})
	key, _ := genHostKey(t)
	tofuPin(t, mk, "孤立条目", key)

	out := runCli(t, "servers", "pin-hostkey", "孤立条目")
	if !strings.Contains(out, "host=ops (port 22)") {
		t.Fatalf("无候选的空主机条目应退化显示 host=ops (port 22)：\n%s", out)
	}
	if strings.Contains(out, "host=:22") {
		t.Fatalf("显示不得出现假地址 :22：\n%s", out)
	}
}
