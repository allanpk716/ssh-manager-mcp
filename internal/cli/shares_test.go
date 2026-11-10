package cli

import (
	"bytes"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
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
