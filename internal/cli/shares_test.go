package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

func seedSharesCommandVault(t *testing.T) (string, []byte) {
	t.Helper()
	dbPath, masterKey := withCliStoreEnv(t)
	s, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AddDevice("NAS", "存放共享数据", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.AddSecret("NAS_MOUNT_PASSWORD", "", []byte("mount-password"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddServer(&models.Server{
		Name: "linux-web", Host: "192.0.2.20", Port: 22, User: "deploy", AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddServer(&models.Server{
		Name: "windows-web", Host: "192.0.2.21", Port: 22, User: "deploy", AuthMethod: models.AuthPassword,
		ShellType: models.ShellPowerShell,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AllowSecrets([]string{"NAS_MOUNT_PASSWORD"}, []string{"linux-web", "windows-web"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	return dbPath, masterKey
}

func seedSharesClientCache(t *testing.T, declaredClientVersion bool) string {
	t.Helper()
	cacheDir := t.TempDir()
	sourceDir := t.TempDir()
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Open(filepath.Join(sourceDir, "source.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	profileID, err := source.AddProfile("共享存储客户端")
	if err != nil {
		t.Fatal(err)
	}
	clientTokenID, _, err := source.AddCacheToken("laptop", profileID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddDevice("NAS", "提供共享存储", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetwork(models.Network{
		Name: "办公局域网", Type: models.NetworkPhysical, SiteName: "办公室",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddNetworkMembership("NAS", "办公局域网", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	for _, server := range []models.Server{
		{Name: "linux-web", Host: "192.0.2.20", Port: 22, User: "deploy", AuthMethod: models.AuthPassword},
		{Name: "windows-web", Host: "192.0.2.21", Port: 22, User: "deploy", AuthMethod: models.AuthPassword, ShellType: models.ShellPowerShell},
		{Name: "island", Host: "192.0.2.22", Port: 22, User: "deploy", AuthMethod: models.AuthPassword},
	} {
		if _, err := source.AddServer(&server); err != nil {
			t.Fatal(err)
		}
	}
	linuxWeb, err := source.GetServerByName("linux-web")
	if err != nil || linuxWeb == nil {
		t.Fatalf("取 linux-web 失败：%v %+v", err, linuxWeb)
	}
	windowsWeb, err := source.GetServerByName("windows-web")
	if err != nil || windowsWeb == nil {
		t.Fatalf("取 windows-web 失败：%v %+v", err, windowsWeb)
	}
	if err := source.GrantServers(profileID, []string{linuxWeb.ID, windowsWeb.ID}); err != nil {
		t.Fatal(err)
	}
	if err := source.AddSecret("NAS_MOUNT_PASSWORD", "共享存储挂载密码", []byte("mount-password"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := source.AllowSecrets([]string{"NAS_MOUNT_PASSWORD"}, []string{"linux-web"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
		Directories: []models.SharedStorageDirectory{{Purpose: "临时数据", Path: "tmp"}},
		Note:        "模型数据集",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := source.AddSecret("OTHER_MOUNT_PASSWORD", "范围外共享的密码", []byte("other-password"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := source.AllowSecrets([]string{"OTHER_MOUNT_PASSWORD"}, []string{"island"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AddSharedStorage(store.SharedStorageInput{
		Name: "scratch", Device: "NAS", User: "other-user",
		PasswordSecret: "OTHER_MOUNT_PASSWORD", Drive: "Y",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	snapshot, err := source.ExportSnapshotForProfile(profileID, declaredClientVersion, clientTokenID)
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	cacheKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := vaultio.EncryptWithKey(cacheKey, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	authBlob, err := json.Marshal(clientops.CacheCred{
		URL: "https://serve.example.internal:7878", Token: "code-laptop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), authBlob, 0o600); err != nil {
		t.Fatal(err)
	}
	memoryKeychain := withDEK(t)
	if err := memoryKeychain.Set(cacheKey); err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(t.TempDir(), "no-vault.db"),
		"SSHMGR_CACHE_DIR":     cacheDir,
		"SSHMGR_MASTERKEY_HEX": "",
		"SSHMGR_FILEKEY_PATH":  filepath.Join(t.TempDir(), "no-such-master.key"),
	})
	return cacheDir
}

func TestSharesClientReadsUndeclaredVersionSnapshot(t *testing.T) {
	seedSharesClientCache(t, false)

	list := runCli(t, "shares", "list")
	for _, text := range []string{"datasets", "NAS", "版本号 1", "可挂载服务器数量"} {
		if !strings.Contains(list, text) {
			t.Fatalf("未声明客户端版本的本地快照仍应能列出共享存储，期待包含 %q：\n%s", text, list)
		}
	}
	if strings.Contains(list, "scratch") {
		t.Fatalf("可挂载范围与授权服务器没有交集的共享存储不应进入客户端快照：\n%s", list)
	}

	shown := runCli(t, "shares", "show", "datasets")
	for _, text := range []string{"共享存储 datasets", "提供方设备：NAS", "挂载账号用户名：mount-user",
		"盘符：Z", "候选访问地址：192.0.2.10", "临时数据=tmp", "备注：模型数据集"} {
		if !strings.Contains(shown, text) {
			t.Fatalf("未声明客户端版本的共享存储详情应包含 %q：\n%s", text, shown)
		}
	}
	if strings.Contains(shown, "mount-password") {
		t.Fatalf("客户端查看输出不应包含挂载密码值：\n%s", shown)
	}
}

func TestSharesClientShowsOnlyIntersectionAndSelectsInstance(t *testing.T) {
	defaultCacheDir := seedSharesClientCache(t, true)
	list := runCli(t, "shares", "list")
	if !strings.Contains(list, "datasets") || strings.Contains(list, "scratch") {
		t.Fatalf("客户端共享存储列表应只显示有交集的共享存储：\n%s", list)
	}
	shown := runCli(t, "shares", "show", "datasets")
	for _, text := range []string{
		"挂载密码机密项：NAS_MOUNT_PASSWORD", "候选访问地址：192.0.2.10",
		"linux-web（挂载点 /mnt/sshmgr/datasets）", "版本号：1",
	} {
		if !strings.Contains(shown, text) {
			t.Fatalf("客户端共享存储详情应包含 %q：\n%s", text, shown)
		}
	}
	if strings.Contains(shown, "windows-web") || strings.Contains(shown, "Z:") {
		t.Fatalf("客户端共享存储详情只应显示机密项可写入列表与授权范围的交集：\n%s", shown)
	}

	userDir := t.TempDir()
	instanceDir := filepath.Join(userDir, "ssh-manager", "instances", "laptop")
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, fileName := range []string{"cache.bin", "cache.auth.json"} {
		if err := os.Rename(filepath.Join(defaultCacheDir, fileName), filepath.Join(instanceDir, fileName)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("APPDATA", userDir)
	t.Setenv("XDG_CONFIG_HOME", userDir)
	t.Setenv("SSHMGR_CONFIG_ROOT", userDir)
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": "", "SSHMGR_CACHE_DEK": ""})

	if _, err := runCLI(t, "shares", "list"); err == nil ||
		!strings.Contains(err.Error(), "--instance") || !strings.Contains(err.Error(), "laptop") {
		t.Fatalf("只有命名实例时不应该静默选择默认缓存槽，实际：%v", err)
	}
	if list := runCli(t, "shares", "list", "--instance", "laptop"); !strings.Contains(list, "datasets") {
		t.Fatalf("--instance 应读取指定客户端实例的共享存储：\n%s", list)
	}
	if shown := runCli(t, "shares", "show", "datasets", "--instance", "laptop"); !strings.Contains(shown, "linux-web") {
		t.Fatalf("--instance 的共享存储详情应读取指定客户端实例：\n%s", shown)
	}
}

func TestSharesAddListAndShow(t *testing.T) {
	dbPath, masterKey := seedSharesCommandVault(t)
	help := runCli(t, "shares", "add", "--help")
	for _, text := range []string{"首版只支持 SMB", "提供方设备需关闭『禁止同一 IP 多个连接』类限制"} {
		if !strings.Contains(help, text) {
			t.Fatalf("shares add 帮助应说明前提 %q：\n%s", text, help)
		}
	}
	if strings.Contains(help, "protocol") || strings.Contains(help, "协议选择") {
		t.Fatalf("首版没有协议选择参数，帮助不应提示协议选择：\n%s", help)
	}

	out := runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "z",
		"--dir", "临时数据=tmp", "--dir", "长期数据=datasets", "--note", "模型数据集")
	for _, text := range []string{"已登记共享存储 datasets", "版本号：1", "首版只支持 SMB", "提供方设备需关闭『禁止同一 IP 多个连接』类限制", "该设备没有网络成员，暂时无法挂载"} {
		if !strings.Contains(out, text) {
			t.Fatalf("新增成功输出应包含 %q：\n%s", text, out)
		}
	}

	list := runCli(t, "shares", "list")
	for _, text := range []string{"datasets", "NAS", "Z", "版本号 1", "可挂载服务器数量 2"} {
		if !strings.Contains(list, text) {
			t.Fatalf("共享存储列表应包含 %q：\n%s", text, list)
		}
	}

	s, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.AddSite("办公室", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNetwork(models.Network{Name: "办公局域网", Type: models.NetworkPhysical, SiteName: "办公室"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNetworkMembership("NAS", "办公局域网", "192.0.2.10"); err != nil {
		t.Fatal(err)
	}
	shown := runCli(t, "shares", "show", "DATASETS")
	for _, text := range []string{
		"共享存储 datasets", "提供方设备：NAS", "挂载账号用户名：mount-user",
		"挂载密码机密项：NAS_MOUNT_PASSWORD", "盘符：Z", "候选访问地址：192.0.2.10",
		"临时数据=tmp", "长期数据=datasets", "备注：模型数据集", "版本号：1",
		"linux-web（挂载点 /mnt/sshmgr/datasets）", "windows-web（挂载点 Z:）",
	} {
		if !strings.Contains(shown, text) {
			t.Fatalf("共享存储详情应包含 %q：\n%s", text, shown)
		}
	}
}

func TestSharesEditUsesPartialUpdateSemantics(t *testing.T) {
	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "old-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z",
		"--dir", "旧用途=old", "--note", "旧备注")

	out := runCli(t, "shares", "edit", "datasets", "--user", "new-user",
		"--note", "新备注", "--change-note", "更新挂载账号")
	if !strings.Contains(out, "已修改共享存储 datasets") || !strings.Contains(out, "版本号：2") {
		t.Fatalf("修改输出应包含新版本号：\n%s", out)
	}
	shown := runCli(t, "shares", "show", "datasets")
	if !strings.Contains(shown, "挂载账号用户名：new-user") ||
		!strings.Contains(shown, "备注：新备注") || !strings.Contains(shown, "变更说明：更新挂载账号") {
		t.Fatalf("修改后的登记不正确：\n%s", shown)
	}

	runCli(t, "shares", "edit", "datasets", "--dir", "临时数据=tmp", "--dir", "长期数据=datasets")
	shown = runCli(t, "shares", "show", "datasets")
	if !strings.Contains(shown, "版本号：3") || !strings.Contains(shown, "临时数据=tmp") ||
		!strings.Contains(shown, "长期数据=datasets") || strings.Contains(shown, "旧用途=old") ||
		!strings.Contains(shown, "变更说明：无") {
		t.Fatalf("--dir 应整体替换目录约定，且未给变更说明时清空旧说明：\n%s", shown)
	}

	runCli(t, "shares", "edit", "datasets", "--clear-dirs", "--change-note", "清空目录约定")
	shown = runCli(t, "shares", "show", "datasets")
	if !strings.Contains(shown, "版本号：4") || !strings.Contains(shown, "变更说明：清空目录约定") {
		t.Fatalf("--clear-dirs 应清空目录约定并推进版本号：\n%s", shown)
	}
	if strings.Contains(shown, "临时数据=tmp") || strings.Contains(shown, "长期数据=datasets") {
		t.Fatalf("清空后的目录约定不应保留旧条目：\n%s", shown)
	}

	out = runCli(t, "shares", "edit", "datasets", "--change-note", "只改变更说明")
	if !strings.Contains(out, "版本号：5") {
		t.Fatalf("只给 --change-note 也应算一次修改：\n%s", out)
	}

	errText := runCliErr(t, "shares", "edit", "datasets", "--dir", "错误格式", "--clear-dirs")
	if !strings.Contains(errText, "--dir 与 --clear-dirs 互斥") {
		t.Fatalf("--dir 与 --clear-dirs 应互斥，实际：\n%s", errText)
	}
}

func TestSharesRemoveRequiresConfirmationAndExplainsScope(t *testing.T) {
	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")

	previous := sharesStdinIsInteractive
	sharesStdinIsInteractive = func() bool { return false }
	defer func() { sharesStdinIsInteractive = previous }()
	errText := runCliErr(t, "shares", "rm", "datasets")
	if !strings.Contains(errText, "非交互终端不能确认删除共享存储") ||
		!strings.Contains(errText, "--yes") {
		t.Fatalf("脚本里删除共享存储必须显式给 --yes，实际：\n%s", errText)
	}

	sharesStdinIsInteractive = func() bool { return true }
	root := NewRootCmd()
	output := &bytes.Buffer{}
	root.SetOut(output)
	root.SetIn(strings.NewReader("n\n"))
	root.SetArgs([]string{"shares", "rm", "datasets"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "已取消，未删除共享存储。") {
		t.Fatalf("交互输入 n 应取消删除：\n%s", output.String())
	}
	runCli(t, "shares", "show", "datasets")

	root = NewRootCmd()
	output = &bytes.Buffer{}
	root.SetOut(output)
	root.SetIn(strings.NewReader("y\n"))
	root.SetArgs([]string{"shares", "rm", "datasets"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	out := output.String()
	for _, text := range []string{"linux-web", "windows-web", "本命令只删除登记，不会去服务器上卸载。", "已删除共享存储 datasets"} {
		if !strings.Contains(out, text) {
			t.Fatalf("删除共享存储输出应包含 %q：\n%s", text, out)
		}
	}
	if errOut := runCliErr(t, "shares", "show", "datasets"); !strings.Contains(errOut, "共享存储不存在") {
		t.Fatalf("删除后查询应报不存在，实际：\n%s", errOut)
	}
}

func TestSecretsRemoveRefusesSharedStoragePassword(t *testing.T) {
	seedSharesCommandVault(t)
	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	runCli(t, "shares", "add", "scratch", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Y")

	errText := runCliErr(t, "secrets", "rm", "NAS_MOUNT_PASSWORD")
	for _, text := range []string{"仍被共享存储引用", "datasets", "scratch"} {
		if !strings.Contains(errText, text) {
			t.Fatalf("删除被共享存储引用的机密项应列出 %q：\n%s", text, errText)
		}
	}
	if strings.Contains(errText, "FOREIGN KEY constraint failed") {
		t.Fatalf("不应暴露数据库外键错误：\n%s", errText)
	}
	if out := runCli(t, "secrets", "list"); !strings.Contains(out, "NAS_MOUNT_PASSWORD") {
		t.Fatalf("被共享存储引用的机密项不应被删除：\n%s", out)
	}
}

func TestSharesClientRefusesWrites(t *testing.T) {
	seedTopologyClientCacheFixture(t)
	writes := [][]string{
		{"shares", "add", "datasets", "--device", "工作站", "--user", "mount-user",
			"--password-secret", "secret", "--drive", "Z"},
		{"shares", "edit", "datasets", "--note", "新备注"},
		{"shares", "rm", "datasets", "--yes"},
	}
	for _, args := range writes {
		errText := runCliErr(t, args...)
		if !strings.Contains(errText, "共享存储在权威端维护") ||
			!strings.Contains(errText, "请在权威端执行") {
			t.Fatalf("客户端写命令 %v 应指向权威端，实际：\n%s", args, errText)
		}
	}
}

func TestSharesAddRejectsMissingReferencesAndDuplicates(t *testing.T) {
	seedSharesCommandVault(t)
	base := []string{"shares", "add", "datasets", "--user", "mount-user", "--drive", "Z"}
	missingDevice := append(append([]string{}, base...), "--device", "missing", "--password-secret", "NAS_MOUNT_PASSWORD")
	if errText := runCliErr(t, missingDevice...); !strings.Contains(errText, "请先执行 sshmgr devices add 登记设备") {
		t.Fatalf("缺失提供方设备时应给出登记设备指引，实际：\n%s", errText)
	}
	missingSecret := append(append([]string{}, base...), "--device", "NAS", "--password-secret", "missing")
	if errText := runCliErr(t, missingSecret...); !strings.Contains(errText, "请先执行 sshmgr secrets add 登记机密项") {
		t.Fatalf("缺失挂载密码机密项时应给出登记机密项指引，实际：\n%s", errText)
	}

	runCli(t, "shares", "add", "datasets", "--device", "NAS", "--user", "mount-user",
		"--password-secret", "NAS_MOUNT_PASSWORD", "--drive", "Z")
	duplicate := append(append([]string{}, base...), "--device", "NAS", "--password-secret", "NAS_MOUNT_PASSWORD")
	duplicate[2] = "DATASETS"
	if errText := runCliErr(t, duplicate...); !strings.Contains(errText, "共享名已存在") {
		t.Fatalf("共享名应不区分大小写去重，实际：\n%s", errText)
	}
	invalidDrive := append(append([]string{}, duplicate...), "--device", "NAS", "--password-secret", "NAS_MOUNT_PASSWORD")
	invalidDrive[2] = "scratch"
	for index := range invalidDrive {
		if invalidDrive[index] == "Z" {
			invalidDrive[index] = "C"
		}
	}
	if errText := runCliErr(t, invalidDrive...); !strings.Contains(errText, "盘符不合法") {
		t.Fatalf("非法盘符应被拒绝，实际：\n%s", errText)
	}
	withProtocol := append(append([]string{}, invalidDrive...), "--protocol", "SMB")
	if errText := runCliErr(t, withProtocol...); !strings.Contains(errText, "unknown flag: --protocol") {
		t.Fatalf("首版不应提供协议选择参数，实际：\n%s", errText)
	}
}
