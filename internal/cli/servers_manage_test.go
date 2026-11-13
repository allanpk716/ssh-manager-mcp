package cli

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// runCliWithInput 以指定标准输入驱动命令；返回输出与错误。
func runCliWithInput(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	// 固定按非交互终端处理：交互检测在真实 TTY 里跑测试时 otherwise 会去读
	// 测试进程自己的标准输入。
	prevTTY := serversStdinIsTTY
	serversStdinIsTTY = func() bool { return false }
	t.Cleanup(func() { serversStdinIsTTY = prevTTY })
	root := NewRootCmd()
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	root.SetIn(strings.NewReader(input))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

// seedAttentionServers 准备三台服务器：完整、未挂凭据、带待补口令标签。
func seedAttentionServers(t *testing.T, dbPath string, mk []byte) {
	t.Helper()
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("login-secret-9f21")})
	if err != nil {
		t.Fatal(err)
	}
	servers := []*models.Server{
		{Name: "fine", Host: "192.0.2.1", Port: 22, User: "root", CredentialID: cid, Role: "完整角色"},
		{Name: "bare", Host: "192.0.2.2", Port: 22, User: "root", Role: "r"},
		{Name: "enc", Host: "192.0.2.3", Port: 22, User: "root", CredentialID: cid, Role: "r", Tags: []string{models.TagNeedsPassphrase}},
	}
	for _, srv := range servers {
		if _, err := st.AddServer(srv); err != nil {
			t.Fatal(err)
		}
	}
}

// TestServersListAttentionFilterAndReasonColumn 校验列表的注意原因列与
// --attention 过滤。
func TestServersListAttentionFilterAndReasonColumn(t *testing.T) {
	dbPath, mk := newVaultEnvNoRun(t)
	seedAttentionServers(t, dbPath, mk)

	all := runCli(t, "servers", "ls")
	for _, want := range []string{"fine", "bare", "enc", "未挂凭据", "待补私钥口令"} {
		if !strings.Contains(all, want) {
			t.Fatalf("servers ls 缺少 %q：\n%s", want, all)
		}
	}
	attention := runCli(t, "servers", "ls", "--attention")
	if strings.Contains(attention, "fine") {
		t.Fatalf("--attention 不应列出完整服务器：\n%s", attention)
	}
	if !strings.Contains(attention, "bare") || !strings.Contains(attention, "enc") {
		t.Fatalf("--attention 应列出需要注意的服务器：\n%s", attention)
	}
	if !strings.Contains(attention, "未挂凭据") || !strings.Contains(attention, "待补私钥口令") {
		t.Fatalf("--attention 应显示原因：\n%s", attention)
	}
}

// TestServersShowDetail 校验单机详情输出：连接信息、凭据状态、服务器元数
// 据、标签、主机可见性、所属设备与注意原因，且不含任何凭据内容。
func TestServersShowDetail(t *testing.T) {
	dbPath, mk := newVaultEnvNoRun(t)
	seedAttentionServers(t, dbPath, mk)
	st := openInspect(t, dbPath, mk)
	sudoID, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("sudo-secret-53aa")})
	if err != nil {
		t.Fatal(err)
	}
	fine, err := st.GetServerByName("fine")
	if err != nil || fine == nil {
		t.Fatal(err)
	}
	fine.SudoCredentialID = sudoID
	fine.Tags = []string{"gpu"}
	fine.Location = "机房 A"
	fine.ExposeHost = true
	fine.Description = "说明文字"
	if err := st.UpdateServer(fine); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out := runCli(t, "servers", "show", "fine")
	for _, want := range []string{
		"fine", "root@192.0.2.1:22", "密码", "已设 sudo 密码", "机房 A", "说明文字", "gpu",
		"向 agent 展示主机地址", "完整角色",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("servers show 缺少 %q：\n%s", want, out)
		}
	}
	for _, secret := range []string{"login-secret-9f21", "sudo-secret-53aa"} {
		if strings.Contains(out, secret) {
			t.Fatalf("servers show 不得输出凭据内容：%s", secret)
		}
	}

	encOut := runCli(t, "servers", "show", "enc")
	if !strings.Contains(encOut, "注意原因") || !strings.Contains(encOut, "待补私钥口令") {
		t.Fatalf("详情应显示注意原因：\n%s", encOut)
	}
	if _, err := runCliWithInput(t, "", "servers", "show", "missing"); err == nil {
		t.Fatal("不存在的服务器应报错")
	}
}

// TestServersShowOnClientSnapshot 校验客户端 `servers show` 只读本地快照。
func TestServersShowOnClientSnapshot(t *testing.T) {
	seedClientCacheSnapshot(t, &store.Snapshot{Version: 1, Servers: []store.SnapshotServer{
		{ID: "s1", Name: "snapshot-web", User: "root", Host: "192.0.2.10", Port: 22,
			CredentialID: "c1", Role: "快照角色", TagsRaw: `["needs-passphrase"]`, ShellType: "posix"},
	}}, "")
	if err := roles.Save(roles.State{Role: roles.RoleClient, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSHMGR_STORE", filepath.Join(t.TempDir(), "no-vault.db"))

	out := runCli(t, "servers", "show", "snapshot-web")
	for _, want := range []string{"snapshot-web", "root@192.0.2.10:22", "快照角色", "待补私钥口令"} {
		if !strings.Contains(out, want) {
			t.Fatalf("客户端详情缺少 %q：\n%s", want, out)
		}
	}
	if _, err := runCliWithInput(t, "", "servers", "show", "不在快照里"); err == nil {
		t.Fatal("快照外的服务器应报错")
	}
}

// TestServersShowReverseGroups 从根命令进入，校验单机 `servers show` 输出与
// 详情页同一份反向查询结果，且不包含机密项值。
func TestServersShowReverseGroups(t *testing.T) {
	dbPath, mk := newVaultEnvNoRun(t)
	st := openInspect(t, dbPath, mk)
	device, err := st.AddDevice("workstation", "同一台设备", "")
	if err != nil {
		t.Fatal(err)
	}
	primary, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "deploy", DeviceID: device.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{
		Name: "worker", Host: "192.0.2.11", Port: 22, User: "deploy", DeviceID: device.ID,
		ShellType: models.ShellPowerShell,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecretWithEnvironmentName(
		"API_TOKEN", "接口令牌", []byte("ordinary-secret-value"), "API_TOKEN", store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(
		"DATASET_PASSWORD", "挂载密码", []byte("share-secret-value"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(
		"WORKER_PASSWORD", "工作机挂载密码", []byte("worker-secret-value"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets(
		[]string{"API_TOKEN", "DATASET_PASSWORD"}, []string{"web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"WORKER_PASSWORD"}, []string{"worker"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "workstation", User: "mount-user",
		PasswordSecret: "DATASET_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "worker-data", Device: "workstation", User: "mount-user",
		PasswordSecret: "WORKER_PASSWORD", Drive: "Y",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("运维")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{primary}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	out := runCli(t, "servers", "show", "web")
	for _, want := range []string{
		"机密项：", "API_TOKEN", "接口令牌", "环境变量名 API_TOKEN",
		"共享存储：", "datasets", "提供方设备 workstation", "挂载用户 mount-user",
		"挂载密码机密项 DATASET_PASSWORD", "挂载点 /mnt/sshmgr/datasets",
		"profile 授权：", "运维",
		"同设备其他服务器条目带来的共享存储：", "worker-data", "来源服务器条目 worker",
		"这台机器上的挂载点 Y:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("servers show 反向视图缺少 %q：\n%s", want, out)
		}
	}
	for _, secretValue := range []string{"ordinary-secret-value", "share-secret-value", "worker-secret-value"} {
		if strings.Contains(out, secretValue) {
			t.Fatalf("servers show 不得输出机密项值 %q：\n%s", secretValue, out)
		}
	}
}

// TestServersRmPreviewAndYes 校验删除预览、非交互保护与 --yes 执行。
func TestServersRmPreviewAndYes(t *testing.T) {
	dbPath, mk := newVaultEnvNoRun(t)
	st := openInspect(t, dbPath, mk)
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentials(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root", CredentialID: cid, Role: "r",
	}, &models.Credential{ID: cid, Type: models.CredPassword}, nil); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("team")
	if err != nil {
		t.Fatal(err)
	}
	web, _ := st.GetServerByName("web")
	if err := st.GrantServers(profileID, []string{web.ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveEntryPin(&models.Server{ID: web.ID, Name: "web", Host: "192.0.2.10", Port: 22}, []byte("anchor")); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := runCliWithInput(t, "", "servers", "rm", "web"); err == nil {
		t.Fatal("非交互且没有 --yes 时应报错")
	}
	if still, _ := openInspect(t, dbPath, mk).GetServerByName("web"); still == nil {
		t.Fatal("拒绝执行时不得删除服务器")
	}

	out := runCli(t, "servers", "rm", "--yes", "web")
	for _, want := range []string{"team", "主机密钥锚", "登录凭据（密码）", "设备保留"} {
		if !strings.Contains(out, want) {
			t.Fatalf("删除预览缺少 %q：\n%s", want, out)
		}
	}
	if still, _ := openInspect(t, dbPath, mk).GetServerByName("web"); still != nil {
		t.Fatal("--yes 应删除服务器")
	}
}

// TestServersEditKeyPassphraseAlone 校验 `servers edit --key-passphrase` 不带
// --key：直接补 vault 已存私钥的口令。
func TestServersEditKeyPassphraseAlone(t *testing.T) {
	dbPath, mk := newVaultEnvNoRun(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "enc_key")
	keyPEM := genKeyFile(t, keyPath, "secret-pass")
	st := openInspect(t, dbPath, mk)
	if _, err := st.AddServerWithCredentials(&models.Server{
		Name: "enc", Host: "192.0.2.3", Port: 22, User: "root", Role: "r",
	}, &models.Credential{Type: models.CredPrivateKey, Secret: keyPEM}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := runCliWithInput(t, "", "servers", "edit", "enc", "--key-passphrase", "wrong"); err == nil {
		t.Fatal("错误口令应报错")
	}

	out := runCli(t, "servers", "edit", "enc", "--key-passphrase", "secret-pass")
	if !strings.Contains(out, "enc") {
		t.Fatalf("补口令成功提示应带服务器名：\n%s", out)
	}
	inspect := openInspect(t, dbPath, mk)
	srv, _ := inspect.GetServerByName("enc")
	if srv == nil || models.ServerHasTag(srv.Tags, models.TagNeedsPassphrase) {
		t.Fatalf("补口令后应移除待补标签：%+v", srv)
	}
	cred, err := inspect.GetCredential(srv.CredentialID)
	if err != nil || cred == nil || string(cred.Passphrase) != "secret-pass" {
		t.Fatalf("口令应写进凭据：%+v %v", cred, err)
	}
}

// TestServersImportCounts 校验导入结束时的四类计数与补全提示。
func TestServersImportCounts(t *testing.T) {
	dbPath, mk := newVaultEnvNoRun(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	genKeyFile(t, keyPath, "")
	config := filepath.Join(dir, "config")
	body := fmt.Sprintf("Host web\n  HostName 192.0.2.20\n  User deploy\n  IdentityFile %q\nHost bare\n  HostName 192.0.2.21\n  User deploy\n", filepath.ToSlash(keyPath))
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out := runCli(t, "servers", "import", "--file", config)
	for _, want := range []string{"导入 2", "跳过 0", "失败 0", "待补 2", "servers complete"} {
		if !strings.Contains(out, want) {
			t.Fatalf("导入输出缺少 %q：\n%s", want, out)
		}
	}
	if n := serverCount(t, openInspect(t, dbPath, mk)); n != 2 {
		t.Fatalf("导入后服务器数量 = %d，应为 2", n)
	}
}

// TestServersCompleteSkipAndEnd 校验逐台补全的跳过与结束：都不写入。
func TestServersCompleteSkipAndEnd(t *testing.T) {
	dbPath, mk := newVaultEnvNoRun(t)
	seedAttentionServers(t, dbPath, mk)
	before := serverRevisions(t, dbPath, mk, "bare", "enc")

	blank := "\n\n\n\n\n\n\n\n"
	input := blank + "k\n" + blank + "e\n"
	out, err := runCliWithInput(t, input, "servers", "complete")
	if err != nil {
		t.Fatalf("complete: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bare") || !strings.Contains(out, "enc") {
		t.Fatalf("补全流程应逐台展示服务器：\n%s", out)
	}
	if !strings.Contains(out, "已结束") {
		t.Fatalf("结束补全应显示结果：\n%s", out)
	}
	after := serverRevisions(t, dbPath, mk, "bare", "enc")
	for name, rev := range before {
		if after[name] != rev {
			t.Fatalf("%s 被跳过时修订号不得变化：%d -> %d", name, rev, after[name])
		}
	}
}

// TestServersCompleteSavesMetadata 校验补全保存路径：填写角色后警告消失。
func TestServersCompleteSavesMetadata(t *testing.T) {
	dbPath, mk := newVaultEnvNoRun(t)
	seedAttentionServers(t, dbPath, mk)
	input := "\n\n\n\n裸机计算节点\n\nnew-login-pass\ns\n" + "\n\n\n\n\n\n\n\n" + "e\n"
	out, err := runCliWithInput(t, input, "servers", "complete", "bare")
	if err != nil {
		t.Fatalf("complete: %v\n%s", err, out)
	}
	st := openInspect(t, dbPath, mk)
	srv, _ := st.GetServerByName("bare")
	if srv == nil || srv.Role != "裸机计算节点" {
		t.Fatalf("补全应保存角色：%+v", srv)
	}
	if got := models.ServerAttentionReasons(srv.CredentialID, srv.Role, srv.Tags); len(got) != 0 {
		t.Fatalf("补全后不应再有注意原因：%v", got)
	}
}

// newVaultEnvNoRun 建一个新 vault 环境并返回路径与主密钥，不预置命令运行
// 函数，便于本文件用带标准输入的运行器驱动交互命令。
func newVaultEnvNoRun(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         dbPath,
		"SSHMGR_MASTERKEY_HEX": hex.EncodeToString(mk),
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
	})
	return dbPath, mk
}

func serverRevisions(t *testing.T, dbPath string, mk []byte, names ...string) map[string]int64 {
	t.Helper()
	st := openInspect(t, dbPath, mk)
	out := map[string]int64{}
	for _, name := range names {
		srv, err := st.GetServerByName(name)
		if err != nil || srv == nil {
			t.Fatalf("读取 %s 失败：%v", name, err)
		}
		out[name] = srv.Revision
	}
	return out
}

// TestServersListSharedCredentialBeforeWrite 补私钥口令影响共用凭据的全部
// 服务器：命令行在询问口令或写入前列出受影响的服务器名。
func TestServersListSharedCredentialBeforeWrite(t *testing.T) {
	dbPath, mk := newVaultEnvNoRun(t)
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "enc_key")
	keyPEM := genKeyFile(t, keyPath, "secret-pass")
	st := openInspect(t, dbPath, mk)
	cred := &models.Credential{Type: models.CredPrivateKey, Secret: keyPEM}
	if _, err := st.AddServerWithCredentials(&models.Server{
		Name: "enc", Host: "192.0.2.3", Port: 22, User: "root", Role: "r",
		Tags: []string{models.TagNeedsPassphrase},
	}, cred, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentials(&models.Server{
		Name: "peer", Host: "192.0.2.4", Port: 22, User: "root", Role: "r",
	}, &models.Credential{ID: cred.ID, Type: models.CredPrivateKey}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out, err := runCliWithInput(t, "\n\n\n\n\n\n\nsecret-pass\ns\ne\n", "servers", "complete")
	if err != nil {
		t.Fatalf("complete: %v\n%s", err, out)
	}
	if !strings.Contains(out, "补私钥口令会影响共用这把私钥的全部服务器：enc,peer") {
		t.Fatalf("complete 应在询问口令前列出共用服务器：\n%s", out)
	}

	editOut := runCli(t, "servers", "edit", "enc", "--key-passphrase", "secret-pass")
	if !strings.Contains(editOut, "补私钥口令会影响共用这把私钥的全部服务器：enc,peer") {
		t.Fatalf("edit 应在写入前列出共用服务器：\n%s", editOut)
	}
}

// TestServersRmPreviewListsDeliveryRecords `servers rm` 的删除影响预览列出
// 会随条目删除的下发记录；确认删除后记录消失，输出不含机密项值。
func TestServersRmPreviewListsDeliveryRecords(t *testing.T) {
	dbPath, mk := newVaultEnvNoRun(t)
	st := openInspect(t, dbPath, mk)
	webID, err := st.AddServer(&models.Server{
		Name: "web", Host: "192.0.2.10", Port: 22, User: "root", Role: "应用服务器",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret("API_TOKEN", "接口令牌", []byte("login-secret-value-9f21"), store.CLISource()); err != nil {
		t.Fatal(err)
	}
	profileID, err := st.AddProfile("agents")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.GrantServers(profileID, []string{webID}); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"API_TOKEN"}, []string{"web"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordSecretDelivery(store.SecretDeliveryRecordInput{
		ServerID: webID, Path: "/etc/app/config.yml",
		Template:   []byte("token: {{sshmgr-secret:API_TOKEN}}"),
		FileSHA256: strings.Repeat("4", 64), WrittenAt: time.Unix(1760000900, 0),
		Secrets:              []store.SecretDeliverySecret{{Name: "API_TOKEN", ValueVersion: 1}},
		InitiatingDeviceName: "office-laptop", InitiatingInstanceName: "agent-a",
		AuthorizationProfileID: profileID,
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	out := runCli(t, "servers", "rm", "--yes", "web")
	for _, want := range []string{
		"会随之删除的下发记录", "web：/etc/app/config.yml", "API_TOKEN",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("servers rm 预览缺少 %q：\n%s", want, out)
		}
	}
	if strings.Contains(out, "login-secret-value-9f21") {
		t.Fatalf("servers rm 输出不得出现机密项值：\n%s", out)
	}
	after := openInspect(t, dbPath, mk)
	records, err := after.SecretDeliveryRecords(store.SecretDeliveryRecordFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("确认删除后下发记录应随服务器条目删除：%+v", records)
	}
}
