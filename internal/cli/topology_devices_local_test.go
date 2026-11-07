package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevicesLocalSetShowClear(t *testing.T) {
	withCliStoreEnv(t)
	runCli(t, "devices", "add", "workstation")

	out := runCli(t, "devices", "local", "show")
	if !strings.Contains(out, "未设定本机设备") {
		t.Fatalf("initial show must say no local device is set:\n%s", out)
	}

	errText := runCliErr(t, "devices", "local", "set", "missing-device")
	if !strings.Contains(errText, "设备不存在") {
		t.Fatalf("setting an unknown device must be refused:\n%s", errText)
	}

	out = runCli(t, "devices", "local", "set", "workstation")
	if !strings.Contains(out, "已设定本机设备：workstation") {
		t.Fatalf("set output must name the local device:\n%s", out)
	}
	out = runCli(t, "devices", "local", "show")
	if !strings.Contains(out, "本机设备：workstation") {
		t.Fatalf("show must name the configured local device:\n%s", out)
	}

	runCli(t, "devices", "rm", "workstation", "--yes")
	out = runCli(t, "devices", "local", "show")
	if !strings.Contains(out, "本机设备指向的设备已不存在，按未设定处理") {
		t.Fatalf("show must explain a deleted local device:\n%s", out)
	}

	out = runCli(t, "devices", "local", "clear")
	if !strings.Contains(out, "已清除本机设备设定") {
		t.Fatalf("clear output must confirm removal:\n%s", out)
	}
	out = runCli(t, "devices", "local", "show")
	if !strings.Contains(out, "未设定本机设备") {
		t.Fatalf("show after clear must say no local device is set:\n%s", out)
	}
}

func TestDevicesLocalSettingStaysOutsideBackup(t *testing.T) {
	withCliStoreEnv(t)
	runCli(t, "devices", "add", "workstation")
	runCli(t, "devices", "local", "set", "workstation")

	backupDir := t.TempDir()
	touchMarker(t, backupDir)
	if _, err := runCreate(t, backupDir); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(backupDir, "vault-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected one backup, got %v (%v)", matches, err)
	}
	blob, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"local_device"`, `"local-device.json"`} {
		if strings.Contains(string(blob), forbidden) {
			t.Fatalf("backup must not carry the machine-local setting (%s):\n%s", forbidden, blob)
		}
	}
}
