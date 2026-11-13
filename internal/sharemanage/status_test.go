package sharemanage

import (
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestCheckReportsEveryMountableServerWithoutAudit(t *testing.T) {
	st := newCheckTestStore(t)
	if err := st.AddSecret(
		"NAS_MOUNT_PASSWORD", "", []byte("mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}

	linux := scriptedCheckServer(t, "Linux", strings.Join([]string{
		"SSHMGR-LINUX-CHECK mode=full",
		"SSHMGR-LINUX-CHECK index=1 managed=yes share=datasets version=1 mounted=yes",
	}, "\n"))
	darwin := scriptedCheckServer(t, "Darwin", strings.Join([]string{
		"SSHMGR-DARWIN-CHECK mode=limited",
		"SSHMGR-DARWIN-CHECK index=1 mounted=no",
		"SSHMGR-DARWIN-CHECK auto-master=no",
	}, "\n"))
	addRemoteCheckServer(t, st, "linux-web", linux, true)
	addRemoteCheckServer(t, st, "darwin-web", darwin, false)
	addLocalCheckServer(t, st, "windows-web", models.ShellPowerShell)
	addLocalCheckServer(t, st, "offline-web", models.ShellPOSIX)
	if err := st.AllowSecrets(
		[]string{"NAS_MOUNT_PASSWORD"},
		[]string{"linux-web", "darwin-web", "windows-web", "offline-web"},
		store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	share, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	auditBefore, err := st.QueryAudit(store.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}

	report, err := New(st).Check(t.Context(), CheckRequest{Share: share.Name})
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]CheckServerResult, len(report.Servers))
	for _, result := range report.Servers {
		byName[result.ServerName] = result
		if result.ObservedAt.IsZero() {
			t.Fatalf("服务器 %s 应带观察时间：%+v", result.ServerName, result)
		}
	}
	if got := byName["linux-web"]; got.Status != CheckStatusMounted || got.System != "linux" ||
		!got.Managed || got.Version != 1 {
		t.Fatalf("Linux 结论不正确：%+v", got)
	}
	if got := byName["darwin-web"]; got.Status != CheckStatusUnmounted || got.System != "darwin" ||
		!got.Restricted || !strings.Contains(got.Notice, "没有配置 sudo 凭据") ||
		!strings.Contains(got.Notice, "macOS 系统主配置已被重置") {
		t.Fatalf("macOS 受限结论不正确：%+v", got)
	}
	if got := byName["windows-web"]; got.Status != CheckStatusSkipped ||
		!strings.Contains(got.Notice, "Windows 服务器不检测") {
		t.Fatalf("Windows 应说明不检测：%+v", got)
	}
	if got := byName["offline-web"]; got.Status != CheckStatusUnchecked ||
		!strings.Contains(got.Notice, "连接服务器") {
		t.Fatalf("不可达结论不正确：%+v", got)
	}
	for _, result := range report.Servers {
		if strings.Contains(result.Notice, "192.0.2.") {
			t.Fatalf("服务器 %s 的现场探测说明不得包含具体地址：%+v",
				result.ServerName, result)
		}
	}
	rows, err := st.QueryAudit(store.AuditFilter{})
	if err != nil || len(rows) != len(auditBefore) {
		t.Fatalf("现场探测不应写审计：%v %+v", err, rows)
	}
}

func TestCheckRunsServersConcurrently(t *testing.T) {
	st := newCheckTestStore(t)
	if err := st.AddSecret(
		"NAS_MOUNT_PASSWORD", "", []byte("mount-password"), store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	var running atomic.Int32
	release := make(chan struct{})
	slowServer := func(command string, stdin io.Reader) (string, string, int) {
		switch {
		case strings.TrimSpace(command) == "uname -s":
			return "Linux\n", "", 0
		case strings.Contains(command, "SSHMGR-LINUX-CHECK"):
			running.Add(1)
			<-release
			return "SSHMGR-LINUX-CHECK mode=full\n" +
				"SSHMGR-LINUX-CHECK index=1 managed=no mounted=no\n", "", 0
		default:
			return "", "unexpected command", 90
		}
	}
	first := startCheckSSH(t, slowServer)
	second := startCheckSSH(t, slowServer)
	addRemoteCheckServer(t, st, "first", first, true)
	addRemoteCheckServer(t, st, "second", second, true)
	if err := st.AllowSecrets(
		[]string{"NAS_MOUNT_PASSWORD"}, []string{"first", "second"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "NAS_MOUNT_PASSWORD", Drive: "Z",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	done := make(chan *CheckReport, 1)
	go func() {
		report, err := New(st).Check(t.Context(), CheckRequest{Share: "datasets"})
		if err != nil {
			t.Error(err)
		}
		done <- report
	}()
	deadline := time.After(2 * time.Second)
	for running.Load() < 2 {
		select {
		case <-deadline:
			t.Fatal("两台服务器没有并发执行现场检测")
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(release)
	select {
	case report := <-done:
		if len(report.Servers) != 2 {
			t.Fatalf("应返回两台服务器结论：%+v", report)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("现场检测未在并发执行后完成")
	}
}

type checkSSHTarget struct {
	handler  func(string, io.Reader) (string, string, int)
	address  string
	hostKey  ssh.PublicKey
	password string
	sudo     string
}

func newCheckTestStore(t *testing.T) *store.Store {
	t.Helper()
	key, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir()+"/store.db", key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	return st
}

func scriptedCheckServer(t *testing.T, system, output string) checkSSHTarget {
	t.Helper()
	return startCheckSSH(t, func(command string, _ io.Reader) (string, string, int) {
		if strings.TrimSpace(command) == "uname -s" {
			return system + "\n", "", 0
		}
		return output + "\n", "", 0
	})
}

func startCheckSSH(
	t *testing.T, handler func(string, io.Reader) (string, string, int),
) checkSSHTarget {
	t.Helper()
	address, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "login-pw", SudoPassword: "sudo-pw", Exec: handler,
	})
	t.Cleanup(cleanup)
	return checkSSHTarget{
		handler: handler, address: address, hostKey: hostKey,
		password: "login-pw", sudo: "sudo-pw",
	}
}

func addRemoteCheckServer(
	t *testing.T, st *store.Store, name string, target checkSSHTarget, withSudo bool,
) {
	t.Helper()
	loginCredential := &models.Credential{
		Type: models.CredPassword, Secret: []byte(target.password),
	}
	var sudoCredential *models.Credential
	if withSudo {
		sudoCredential = &models.Credential{
			Type: models.CredPassword, Secret: []byte(target.sudo),
		}
	}
	server := &models.Server{
		Name: name, Host: target.address[:strings.LastIndex(target.address, ":")],
		Port: portOfCheckAddress(target.address), User: "deploy",
		ShellType: models.ShellPOSIX,
	}
	added, err := st.AddServerWithCredentialsToDevice(
		server, loginCredential, sudoCredential, "NAS",
	)
	if err != nil {
		t.Fatal(err)
	}
	server.ID = added.ServerID
	if err := st.SaveEntryPin(server, target.hostKey.Marshal()); err != nil {
		t.Fatal(err)
	}
}

func addLocalCheckServer(
	t *testing.T, st *store.Store, name string, shell models.ShellType,
) {
	t.Helper()
	credential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: name, Host: "127.0.0.1", Port: 1, User: "deploy",
		ShellType: shell,
	}, credential, nil, "NAS"); err != nil {
		t.Fatal(err)
	}
}

func portOfCheckAddress(address string) int {
	value := address[strings.LastIndex(address, ":")+1:]
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}
	return parsed
}
