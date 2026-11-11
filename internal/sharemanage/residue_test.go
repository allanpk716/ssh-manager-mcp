package sharemanage_test

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemanage"
	"ssh-manager-mcp/internal/sharemounttest"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

func TestResidueReportsOutOfRangeLinuxServerWithCleanupCommand(t *testing.T) {
	fixture := newResidueFixture(t)
	before := fixture.out.State()

	report, err := fixture.manager.Residue(context.Background(), sharemanage.ResidueRequest{})
	if err != nil {
		t.Fatalf("Residue: %v", err)
	}
	row := residueResultFor(report, "linux-out")
	if row == nil {
		t.Fatalf("结果里缺少 linux-out：%+v", report)
	}
	if row.Status != "residue" || row.System != "linux" {
		t.Fatalf("linux-out 的逐台结果 = %+v", row)
	}
	if len(row.Findings) != 1 {
		t.Fatalf("linux-out 的发现数量 = %d：%+v", len(row.Findings), row.Findings)
	}
	finding := row.Findings[0]
	if finding.ShareName != "datasets" || !finding.HasConfigEntry || !finding.HasCredentialFile ||
		!finding.HasMount || finding.RevokedMarkerOnly {
		t.Fatalf("datasets 的残留发现 = %+v", finding)
	}
	if finding.Reason != "server-out-of-range" {
		t.Fatalf("残留原因 = %q", finding.Reason)
	}
	if finding.CleanupCommand != "shares unmount datasets linux-out --yes" {
		t.Fatalf("清理命令 = %q", finding.CleanupCommand)
	}

	after := fixture.out.State()
	if after.Credential != before.Credential ||
		strings.Join(after.FstabLines, "\n") != strings.Join(before.FstabLines, "\n") ||
		after.Mounted != before.Mounted || after.RevokedMarker != before.RevokedMarker ||
		len(after.History) != len(before.History) {
		t.Fatalf("残留检查不应改动服务器状态：before=%+v after=%+v", before, after)
	}
	for _, command := range after.Commands[len(before.Commands):] {
		if strings.TrimSpace(command) != "uname -s" &&
			!strings.Contains(command, sharemounttest.LinuxEnumerateMarker) {
			t.Fatalf("残留检查应只执行只读命令：%s", command)
		}
	}
}

func residueResultFor(report *sharemanage.ResidueReport, serverName string) *sharemanage.ResidueServerResult {
	for index := range report.Servers {
		if report.Servers[index].ServerName == serverName {
			return &report.Servers[index]
		}
	}
	return nil
}

func TestResidueClassifiesMixedServers(t *testing.T) {
	fixture := newResidueFixture(t)
	darwinBefore := fixture.darwin.State()
	markerBefore := fixture.marker.State()

	report, err := fixture.manager.Residue(context.Background(), sharemanage.ResidueRequest{})
	if err != nil {
		t.Fatalf("Residue: %v", err)
	}

	inRange := residueResultFor(report, "linux-in")
	if inRange == nil || inRange.Status != sharemanage.ResidueStatusNoResidue ||
		len(inRange.Findings) != 0 || inRange.System != "linux" {
		t.Fatalf("范围内服务器的结论 = %+v", inRange)
	}

	markerOnly := residueResultFor(report, "linux-marker")
	if markerOnly == nil || markerOnly.Status != sharemanage.ResidueStatusMarkerOnly ||
		len(markerOnly.Findings) != 1 {
		t.Fatalf("仅有撤销标记服务器的结论 = %+v", markerOnly)
	}
	markerFinding := markerOnly.Findings[0]
	if !markerFinding.RevokedMarkerOnly || markerFinding.HasConfigEntry ||
		markerFinding.HasCredentialFile || markerFinding.HasMount ||
		markerFinding.CleanupCommand != "" || markerFinding.ShareName != "datasets" {
		t.Fatalf("仅有撤销标记的发现 = %+v", markerFinding)
	}

	deletedShare := residueResultFor(report, "darwin-out")
	if deletedShare == nil || deletedShare.Status != sharemanage.ResidueStatusResidue ||
		deletedShare.System != "darwin" || len(deletedShare.Findings) != 1 {
		t.Fatalf("已删除共享服务器的结论 = %+v", deletedShare)
	}
	deletedFinding := deletedShare.Findings[0]
	if deletedFinding.ShareName != "archive" || !deletedFinding.HasConfigEntry ||
		!deletedFinding.HasMount || deletedFinding.HasCredentialFile ||
		deletedFinding.Reason != sharemanage.ResidueReasonShareUnregistered ||
		deletedFinding.CleanupCommand != "shares unmount archive darwin-out --yes" {
		t.Fatalf("已删除共享的发现 = %+v", deletedFinding)
	}

	unchecked := residueResultFor(report, "linux-offline")
	if unchecked == nil || unchecked.Status != sharemanage.ResidueStatusUnchecked ||
		unchecked.Reason != "ssh-error" || len(unchecked.Findings) != 0 {
		t.Fatalf("连不上服务器的结论 = %+v", unchecked)
	}

	windows := residueResultFor(report, "windows-web")
	if windows == nil || windows.Status != sharemanage.ResidueStatusSkipped ||
		windows.System != "windows" || !containsText([]string{windows.Notice}, "sshmgr 不在其上留下任何东西") {
		t.Fatalf("Windows 服务器的结论 = %+v", windows)
	}

	darwinAfter := fixture.darwin.State()
	if len(darwinAfter.MapLines) != len(darwinBefore.MapLines) ||
		darwinAfter.Mounted != darwinBefore.Mounted ||
		darwinAfter.RevokedMarker != darwinBefore.RevokedMarker ||
		len(darwinAfter.History) != len(darwinBefore.History) {
		t.Fatalf("macOS 服务器状态不应变化：before=%+v after=%+v", darwinBefore, darwinAfter)
	}
	markerAfter := fixture.marker.State()
	if len(markerAfter.History) != len(markerBefore.History) ||
		markerAfter.RevokedMarker != markerBefore.RevokedMarker {
		t.Fatalf("仅有撤销标记的服务器状态不应变化：before=%+v after=%+v", markerBefore, markerAfter)
	}
}

func TestResidueFiltersByShareName(t *testing.T) {
	fixture := newResidueFixture(t)

	report, err := fixture.manager.Residue(context.Background(), sharemanage.ResidueRequest{
		Share: "datasets",
	})
	if err != nil {
		t.Fatalf("Residue: %v", err)
	}
	for _, row := range report.Servers {
		for _, finding := range row.Findings {
			if finding.ShareName != "datasets" {
				t.Fatalf("指定 datasets 时不应报告 %q：%+v", finding.ShareName, row)
			}
		}
	}
	media := residueResultFor(report, "linux-media")
	if media == nil || media.Status != sharemanage.ResidueStatusNoResidue ||
		len(media.Findings) != 0 {
		t.Fatalf("指定 datasets 时 linux-media 的结论 = %+v", media)
	}
	out := residueResultFor(report, "linux-out")
	if out == nil || out.Status != sharemanage.ResidueStatusResidue ||
		len(out.Findings) != 1 || out.Findings[0].ShareName != "datasets" {
		t.Fatalf("指定 datasets 时 linux-out 的结论 = %+v", out)
	}

	full, err := fixture.manager.Residue(context.Background(), sharemanage.ResidueRequest{})
	if err != nil {
		t.Fatalf("Residue 全量: %v", err)
	}
	mediaFull := residueResultFor(full, "linux-media")
	if mediaFull == nil || mediaFull.Status != sharemanage.ResidueStatusResidue ||
		len(mediaFull.Findings) != 1 || mediaFull.Findings[0].ShareName != "media" {
		t.Fatalf("全量检查时 linux-media 的结论 = %+v", mediaFull)
	}
}

func TestResidueWritesSafeOwnerAudit(t *testing.T) {
	fixture := newResidueFixture(t)

	if _, err := fixture.manager.Residue(context.Background(), sharemanage.ResidueRequest{
		Share: "datasets",
	}); err != nil {
		t.Fatalf("Residue: %v", err)
	}
	rows, err := fixture.store.QueryAudit(store.AuditFilter{
		OwnerOnly: true, Actions: []string{"share-residue-check"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Status != "ok" {
		t.Fatalf("残留检查的 owner 审计 = %+v", rows)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(rows[0].Command), &summary); err != nil {
		t.Fatal(err)
	}
	if summary["share"] != "datasets" {
		t.Fatalf("审计摘要 = %#v", summary)
	}
	servers, ok := summary["servers"].([]any)
	if !ok {
		t.Fatalf("审计摘要缺少逐台结果：%#v", summary)
	}
	results := make([]map[string]any, 0, len(servers))
	for _, item := range servers {
		result, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("逐台审计摘要 = %#v", item)
		}
		results = append(results, result)
	}
	var sawResidue, sawUnchecked, sawSkipped bool
	for _, result := range results {
		switch result["result"] {
		case "residue":
			sawResidue = true
		case "unchecked":
			sawUnchecked = true
		case "skipped":
			sawSkipped = true
		}
	}
	if !sawResidue || !sawUnchecked || !sawSkipped {
		t.Fatalf("审计摘要应包含逐台固定分类：%#v", summary)
	}
	auditText := rows[0].Command
	for _, secret := range []string{
		"datasets-password", "archive-password", "media-password",
		"mount-user", "192.0.2.10", "192.0.2.11", "192.0.2.12", "login-pw", "sudo-pw",
	} {
		if strings.Contains(auditText, secret) {
			t.Fatalf("审计泄露 %q：%s", secret, auditText)
		}
	}
}

type residueFixture struct {
	store   *store.Store
	manager *sharemanage.Manager
	out     *sharemounttest.LinuxServer
	in      *sharemounttest.LinuxServer
	marker  *sharemounttest.LinuxServer
	darwin  *sharemounttest.DarwinServer
	media   *sharemounttest.LinuxServer
	offline *sharemounttest.LinuxServer
}

func newResidueFixture(t *testing.T) residueFixture {
	t.Helper()
	databasePath := filepath.Join(t.TempDir(), "share-residue-test.db")
	masterKey, _ := store.GenerateMasterKey()
	st, err := store.Open(databasePath, masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := st.AddDevice("NAS", "共享存储提供方", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("挂载目标", "接受共享存储的服务器", ""); err != nil {
		t.Fatal(err)
	}

	loginCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("login-pw")}
	sudoCredential := &models.Credential{Type: models.CredPassword, Secret: []byte("sudo-pw")}
	addServer := func(name string, handler func(string, io.Reader) (string, string, int)) {
		t.Helper()
		addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
			Password: "login-pw", SudoPassword: "sudo-pw", Exec: handler,
		})
		t.Cleanup(cleanup)
		server := &models.Server{
			Name: name, Host: addr[:strings.LastIndex(addr, ":")], Port: portOfAddr(addr),
			User: "deploy", ShellType: models.ShellPOSIX,
		}
		added, err := st.AddServerWithCredentialsToDevice(server, loginCredential, sudoCredential, "挂载目标")
		if err != nil {
			t.Fatal(err)
		}
		server.ID = added.ServerID
		if err := st.SaveEntryPin(server, hostKey.Marshal()); err != nil {
			t.Fatal(err)
		}
	}

	out := &sharemounttest.LinuxServer{ShareName: "datasets"}
	in := &sharemounttest.LinuxServer{ShareName: "datasets"}
	marker := &sharemounttest.LinuxServer{ShareName: "datasets"}
	media := &sharemounttest.LinuxServer{ShareName: "media"}
	offline := &sharemounttest.LinuxServer{ShareName: "datasets"}
	darwin := &sharemounttest.DarwinServer{ShareName: "archive"}

	addServer("darwin-out", darwin.Handler)
	addServer("linux-in", in.Handler)
	addServer("linux-marker", marker.Handler)
	addServer("linux-media", media.Handler)
	addServer("linux-out", out.Handler)
	var offlineClose func()
	{
		addr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
			Password: "login-pw", SudoPassword: "sudo-pw", Exec: offline.Handler,
		})
		t.Cleanup(cleanup)
		server := &models.Server{
			Name: "linux-offline", Host: addr[:strings.LastIndex(addr, ":")], Port: portOfAddr(addr),
			User: "deploy", ShellType: models.ShellPOSIX,
		}
		added, err := st.AddServerWithCredentialsToDevice(server, loginCredential, sudoCredential, "挂载目标")
		if err != nil {
			t.Fatal(err)
		}
		server.ID = added.ServerID
		if err := st.SaveEntryPin(server, hostKey.Marshal()); err != nil {
			t.Fatal(err)
		}
		offlineClose = cleanup
	}
	if _, err := st.AddServer(&models.Server{
		Name: "windows-web", Host: "127.0.0.1", Port: 1, User: "deploy",
		AuthMethod: models.AuthPassword, ShellType: models.ShellPowerShell,
	}); err != nil {
		t.Fatal(err)
	}

	passwords := map[string][]byte{
		"DATASETS_MOUNT_PASSWORD": []byte("datasets-password"),
		"ARCHIVE_MOUNT_PASSWORD":  []byte("archive-password"),
		"MEDIA_MOUNT_PASSWORD":    []byte("media-password"),
	}
	for name, value := range passwords {
		if err := st.AddSecret(name, "共享存储挂载密码", value, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	shares := map[string]struct {
		secret string
		drive  string
	}{
		"datasets": {secret: "DATASETS_MOUNT_PASSWORD", drive: "Z"},
		"archive":  {secret: "ARCHIVE_MOUNT_PASSWORD", drive: "Y"},
		"media":    {secret: "MEDIA_MOUNT_PASSWORD", drive: "X"},
	}
	for name, setup := range shares {
		if _, err := st.AddSharedStorage(store.SharedStorageInput{
			Name: name, Device: "NAS", User: "mount-user",
			PasswordSecret: setup.secret, Drive: setup.drive,
		}, store.CLISource()); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AllowSecrets([]string{"DATASETS_MOUNT_PASSWORD"}, []string{"linux-in"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"MEDIA_MOUNT_PASSWORD"}, []string{"linux-in"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	if err := st.AllowSecrets([]string{"ARCHIVE_MOUNT_PASSWORD"}, []string{"darwin-out"}, store.CLISource()); err != nil {
		t.Fatal(err)
	}

	datasets, err := st.GetSharedStorage("datasets")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := st.GetSharedStorage("archive")
	if err != nil {
		t.Fatal(err)
	}
	mediaShare, err := st.GetSharedStorage("media")
	if err != nil {
		t.Fatal(err)
	}

	out.ShareID = datasets.ID
	out.SeedManagedConfiguration("192.0.2.10", datasets.Version)
	in.ShareID = datasets.ID
	in.SeedManagedConfiguration("192.0.2.10", datasets.Version)
	media.ShareID = mediaShare.ID
	media.SeedManagedConfiguration("192.0.2.11", mediaShare.Version)
	darwin.SeedManagedConfiguration("192.0.2.12", archive.Version)
	offline.ShareID = datasets.ID
	offline.SeedManagedConfiguration("192.0.2.10", datasets.Version)
	offlineClose()

	marker.ShareVersion = 3
	marker.RevokedMarker = "sshmgr-share=datasets;sshmgr-version=3;sshmgr-state=revoked"
	marker.RevokedMarkerPath = "/etc/sshmgr/shares/datasets.revoked"

	if _, err := st.DeleteSharedStorage("archive", store.CLISource()); err != nil {
		t.Fatal(err)
	}

	return residueFixture{
		store: st, manager: sharemanage.New(st),
		out: out, in: in, marker: marker, darwin: darwin, media: media, offline: offline,
	}
}
