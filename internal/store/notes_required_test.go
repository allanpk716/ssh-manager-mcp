package store

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestSecretNoteRequired 覆盖机密项备注必填：新增与编辑时空白备注都被拒
// 绝；存量空备注不强制迁移，只修改环境变量名仍然允许。
func TestSecretNoteRequired(t *testing.T) {
	st := openTestStore(t)
	if err := st.AddSecret("API_TOKEN", "  ", []byte("a-secret-value"), WriteSource{}); err == nil || !strings.Contains(err.Error(), "备注") {
		t.Fatalf("空白备注新增机密项应被拒绝，得到：%v", err)
	}
	if err := st.AddSecret("API_TOKEN", "接口令牌", []byte("a-secret-value"), WriteSource{}); err != nil {
		t.Fatal(err)
	}
	blank := " "
	if err := st.EditSecretMetadata("API_TOKEN", SecretMetadataEdit{Note: &blank}, WriteSource{}); err == nil || !strings.Contains(err.Error(), "备注") {
		t.Fatalf("把备注改成空白应被拒绝，得到：%v", err)
	}
	if err := st.EditSecretNote("API_TOKEN", " ", WriteSource{}); err == nil || !strings.Contains(err.Error(), "备注") {
		t.Fatalf("只改备注为空白应被拒绝，得到：%v", err)
	}
	environmentName := "API_TOKEN_FILE"
	if err := st.EditSecretMetadata("API_TOKEN", SecretMetadataEdit{EnvironmentName: &environmentName}, WriteSource{}); err != nil {
		t.Fatalf("只修改环境变量名不应被备注必填拦截：%v", err)
	}
}

// TestListSecretsWithoutNote 返回备注为空的机密项，供主页待处理清单使用。
func TestListSecretsWithoutNote(t *testing.T) {
	st := openTestStore(t)
	if err := st.AddSecret("BLANK_NOTE", "临时备注", []byte("a-secret-value"), WriteSource{}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("FILLED_NOTE", "部署密钥", []byte("a-secret-value"), WriteSource{}); err != nil {
		t.Fatal(err)
	}
	// 存量空备注不走新增入口，直接模拟旧数据。
	if _, err := st.db.Exec(`UPDATE secrets SET note='' WHERE name='BLANK_NOTE'`); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListSecretsWithoutNote()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "BLANK_NOTE" {
		t.Fatalf("空备注机密项 = %+v，应只有 BLANK_NOTE", rows)
	}
}

// TestSharedStorageNoteRequired 覆盖共享存储备注必填：新增与编辑时空白备
// 注都被拒绝。
func TestSharedStorageNoteRequired(t *testing.T) {
	st := openTestStore(t)
	if err := st.AddSecret("MOUNT_PASSWORD", "挂载密码", []byte("a-secret-value"), WriteSource{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("storage-nas", "", ""); err != nil {
		t.Fatal(err)
	}
	input := SharedStorageInput{
		Name: "datasets", Device: "storage-nas", User: "mount",
		PasswordSecret: "MOUNT_PASSWORD", Drive: "D",
		Directories: []models.SharedStorageDirectory{{Purpose: "数据", Path: "datasets"}},
		Note:        "  ",
	}
	if _, err := st.AddSharedStorage(input, WriteSource{}); err == nil || !strings.Contains(err.Error(), "备注") {
		t.Fatalf("空白备注新增共享存储应被拒绝，得到：%v", err)
	}
	input.Note = "模型数据集"
	if _, err := st.AddSharedStorage(input, WriteSource{}); err != nil {
		t.Fatal(err)
	}
	blank := ""
	if _, err := st.EditSharedStorage("datasets", SharedStorageChanges{Note: &blank}, WriteSource{}); err == nil || !strings.Contains(err.Error(), "备注") {
		t.Fatalf("把共享存储备注改成空白应被拒绝，得到：%v", err)
	}
}

// TestListSharedStoragesWithoutNote 返回备注为空的共享存储，供主页待处理
// 清单使用。
func TestListSharedStoragesWithoutNote(t *testing.T) {
	st := openTestStore(t)
	if err := st.AddSecret("MOUNT_PASSWORD", "挂载密码", []byte("a-secret-value"), WriteSource{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("storage-nas", "", ""); err != nil {
		t.Fatal(err)
	}
	base := SharedStorageInput{
		Device: "storage-nas", User: "mount", PasswordSecret: "MOUNT_PASSWORD",
		Directories: []models.SharedStorageDirectory{{Purpose: "数据", Path: "datasets"}},
	}
	for _, share := range []struct {
		name, note, drive string
	}{
		{"blank-note", "临时备注", "D"},
		{"filled-note", "备份数据集", "E"},
	} {
		input := base
		input.Name, input.Note, input.Drive = share.name, share.note, share.drive
		if _, err := st.AddSharedStorage(input, WriteSource{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.db.Exec(`UPDATE shared_storages SET note='' WHERE name='blank-note'`); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListSharedStoragesWithoutNote()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "blank-note" {
		t.Fatalf("空备注共享存储 = %+v，应只有 blank-note", rows)
	}
}
