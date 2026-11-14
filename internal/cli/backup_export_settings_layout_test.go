package cli

import (
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

var backupExportWebUISettings = map[string]string{
	"serve.pairing":        "false",
	"serve.discovery":      "false",
	"serve.metadata_edit":  "false",
	"serve.ui_listen":      "127.0.0.1",
	"serve.ui_port":        "18790",
	"serve.discovery_name": "release87-display-name",
}

// seedSettingsAndLayouts 建一个只含设置与拓扑布局的源 vault。三种视图都保
// 存同一组实体但使用不同坐标，避免只测到某一个视图。
func seedSettingsAndLayouts(t *testing.T) *store.Snapshot {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "source.db")
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate source master key: %v", err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(masterKey),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
		"SSHMGR_CACHE_DIR":     filepath.Join(dir, "cache"),
	})
	source, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatalf("open source store: %v", err)
	}

	for key, value := range backupExportWebUISettings {
		if err := source.SetSetting(key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}
	site, err := source.AddSite("办公室", "")
	if err != nil {
		t.Fatal(err)
	}
	physicalNetwork, err := source.AddNetwork(models.Network{
		Name: "办公局域网", Type: models.NetworkPhysical, SiteName: "办公室",
	})
	if err != nil {
		t.Fatal(err)
	}
	virtualNetwork, err := source.AddNetwork(models.Network{
		Name: "虚拟局域网", Type: models.NetworkVirtual,
	})
	if err != nil {
		t.Fatal(err)
	}
	device, err := source.AddDevice("工作站", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetworkMembership("工作站", "虚拟局域网", "100.64.0.20"); err != nil {
		t.Fatal(err)
	}

	views := []struct {
		key string
		x   float64
		y   float64
	}{
		{key: store.LayoutViewPhysical, x: 11, y: -3},
		{key: store.LayoutViewVirtual, x: 29, y: 41},
		{key: store.LayoutViewOverlay, x: -17, y: 59},
	}
	for _, view := range views {
		input := store.ViewLayoutInput{
			Devices: []store.DeviceLayoutInput{{
				DeviceID: device.ID, X: view.x, Y: view.y,
			}},
			Containers: []store.ContainerLayoutInput{
				{EntityType: store.LayoutEntitySite, EntityID: site.ID, X: view.x + 10, Y: view.y + 20},
				{EntityType: store.LayoutEntityNetwork, EntityID: physicalNetwork.ID, X: view.x + 30, Y: view.y + 40},
				{EntityType: store.LayoutEntityNetwork, EntityID: virtualNetwork.ID, X: view.x + 50, Y: view.y + 60},
			},
		}
		if err := source.SaveViewLayout(view.key, input, "127.0.0.1"); err != nil {
			t.Fatalf("save %s layout: %v", view.key, err)
		}
	}
	snapshot, err := source.ExportSnapshot()
	if err != nil {
		t.Fatalf("export source snapshot: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close source store: %v", err)
	}
	return snapshot
}

func TestBackupAndExportRestoreWebUISettingsAndThreeLayouts(t *testing.T) {
	sourceSnapshot := seedSettingsAndLayouts(t)

	backupDir := t.TempDir()
	touchMarker(t, backupDir)
	backupPassFile := writePassFile(t, t.TempDir(), "backup-passphrase")
	if _, err := runCreate(t, backupDir, "--passphrase-file", backupPassFile); err != nil {
		t.Fatalf("create encrypted backup: %v", err)
	}
	backupFiles := smeFiles(t, backupDir)
	if len(backupFiles) != 1 {
		t.Fatalf("encrypted backup files = %v, want one file", backupFiles)
	}

	exportDir := t.TempDir()
	exportFile := filepath.Join(exportDir, "vault.sme")
	exportPassFile := writePassFile(t, t.TempDir(), "export-passphrase")
	if _, err := runCLI(t, "export", "--out", exportFile, "--passphrase-file", exportPassFile); err != nil {
		t.Fatalf("create export file: %v", err)
	}

	products := []struct {
		name     string
		file     string
		passFile string
	}{
		{name: "encrypted backup", file: backupFiles[0], passFile: backupPassFile},
		{name: "export", file: exportFile, passFile: exportPassFile},
	}
	for _, product := range products {
		t.Run(product.name, func(t *testing.T) {
			targetDir := t.TempDir()
			targetDB := filepath.Join(targetDir, "target.db")
			targetKey, err := store.GenerateMasterKey()
			if err != nil {
				t.Fatalf("generate target master key: %v", err)
			}
			withEnv(t, map[string]string{
				"SSHMGR_STORE":         targetDB,
				"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(targetKey),
				"SSHMGR_FILEKEY_PATH":  filepath.Join(targetDir, "no-such-master.key"),
				"SSHMGR_CACHE_DIR":     filepath.Join(targetDir, "cache"),
			})
			if _, err := runCLI(t, "import", product.file, "--passphrase-file", product.passFile); err != nil {
				t.Fatalf("import %s: %v", product.name, err)
			}

			target, err := store.Open(targetDB, targetKey)
			if err != nil {
				t.Fatalf("open target store: %v", err)
			}
			defer target.Close()
			for key, want := range backupExportWebUISettings {
				got, exists, err := target.GetSetting(key)
				if err != nil {
					t.Fatalf("read setting %s: %v", key, err)
				}
				if !exists || got != want {
					t.Fatalf("setting %s after import = %q exists=%v, want %q", key, got, exists, want)
				}
			}
			targetSnapshot, err := target.ExportSnapshot()
			if err != nil {
				t.Fatalf("export target snapshot: %v", err)
			}
			if !reflect.DeepEqual(targetSnapshot.DeviceLayoutCoordinates, sourceSnapshot.DeviceLayoutCoordinates) {
				t.Fatalf("device layout coordinates after import = %+v, want %+v",
					targetSnapshot.DeviceLayoutCoordinates, sourceSnapshot.DeviceLayoutCoordinates)
			}
			if !reflect.DeepEqual(targetSnapshot.ContainerLayoutCoordinates, sourceSnapshot.ContainerLayoutCoordinates) {
				t.Fatalf("container layout coordinates after import = %+v, want %+v",
					targetSnapshot.ContainerLayoutCoordinates, sourceSnapshot.ContainerLayoutCoordinates)
			}

			// 恢复后的布局不是只读残留；按公开保存入口整体替换后仍能提交。
			replacement := store.ViewLayoutInput{
				Devices: []store.DeviceLayoutInput{{
					DeviceID: sourceSnapshot.DeviceLayoutCoordinates[0].DeviceID,
					X:        sourceSnapshot.DeviceLayoutCoordinates[0].X + 101,
					Y:        sourceSnapshot.DeviceLayoutCoordinates[0].Y + 103,
				}},
			}
			if err := target.SaveViewLayout(store.LayoutViewPhysical, replacement, "127.0.0.1"); err != nil {
				t.Fatalf("save restored layout again: %v", err)
			}
			devices, err := target.ListDeviceLayoutCoordinates()
			if err != nil {
				t.Fatalf("list layout after saving again: %v", err)
			}
			var found bool
			for _, coordinate := range devices {
				if coordinate.ViewKey == store.LayoutViewPhysical && coordinate.DeviceID == replacement.Devices[0].DeviceID {
					found = coordinate.X == replacement.Devices[0].X && coordinate.Y == replacement.Devices[0].Y
				}
			}
			if !found {
				t.Fatalf("layout after saving again = %+v, want %+v", devices, replacement.Devices)
			}
		})
	}
}

func TestImportOldSnapshotWithoutSettingsAndLayoutsKeepsDefaults(t *testing.T) {
	var snapshot store.Snapshot
	oldJSON := []byte(`{"version":1}`)
	if err := json.Unmarshal(oldJSON, &snapshot); err != nil {
		t.Fatalf("decode old snapshot: %v", err)
	}

	dir := t.TempDir()
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	target, err := store.Open(filepath.Join(dir, "target.db"), masterKey)
	if err != nil {
		t.Fatalf("open target store: %v", err)
	}
	defer target.Close()
	if err := target.ImportSnapshot(&snapshot); err != nil {
		t.Fatalf("import old snapshot: %v", err)
	}
	for key := range backupExportWebUISettings {
		if value, exists, err := target.GetSetting(key); err != nil || exists {
			t.Fatalf("old snapshot must leave setting %s at default; value=%q exists=%v err=%v",
				key, value, exists, err)
		}
	}
	devices, err := target.ListDeviceLayoutCoordinates()
	if err != nil {
		t.Fatalf("list device layouts: %v", err)
	}
	containers, err := target.ListContainerLayoutCoordinates()
	if err != nil {
		t.Fatalf("list container layouts: %v", err)
	}
	if len(devices) != 0 || len(containers) != 0 {
		t.Fatalf("old snapshot layouts = devices:%+v containers:%+v, want both empty",
			devices, containers)
	}
}
