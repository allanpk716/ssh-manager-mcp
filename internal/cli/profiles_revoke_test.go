package cli

// profiles revoke 命令的薄测：输出、名字或标识符解析、错误退出与
// profile.revoke 审计行。业务规则由 internal/access 的模块测试覆盖。

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func mustProfilesRevokeEnv(t *testing.T) (grantsFor func(t *testing.T, profile string) []string) {
	t.Helper()
	dbPath, key, _ := newProfilesAuditEnv(t)
	runCli(t, "profiles", "add", "team-a")
	runCli(t, "servers", "add", "--name", "web1", "--host", "192.0.2.10", "--user", "u")
	runCli(t, "servers", "add", "--name", "web2", "--host", "192.0.2.11", "--user", "u")
	runCli(t, "servers", "add", "--name", "db3", "--host", "192.0.2.12", "--user", "u")
	runCli(t, "profiles", "grant", "team-a", "web1", "web2")
	grantsFor = func(t *testing.T, profile string) []string {
		t.Helper()
		st, err := store.Open(dbPath, key)
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		profs, err := st.ListProfiles()
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range profs {
			if p.Name == profile {
				ids, err := st.ServersForProfile(p.ID)
				if err != nil {
					t.Fatal(err)
				}
				servers, _ := st.ListServers()
				var names []string
				for _, id := range ids {
					for _, srv := range servers {
						if srv.ID == id {
							names = append(names, srv.Name)
						}
					}
				}
				return names
			}
		}
		t.Fatalf("profile %s 不存在", profile)
		return nil
	}
	return grantsFor
}

func TestProfilesRevokeCommand(t *testing.T) {
	grantsFor := mustProfilesRevokeEnv(t)

	out := runCli(t, "profiles", "revoke", "team-a", "web1")
	if !strings.Contains(out, "revoked 1 server(s) from team-a") {
		t.Fatalf("撤销输出应含撤销计数：\n%s", out)
	}
	if got := grantsFor(t, "team-a"); len(got) != 1 || got[0] != "web2" {
		t.Fatalf("撤销后授权 = %v，应只剩 web2", got)
	}
}

func TestProfilesRevokeSkipsUnauthorized(t *testing.T) {
	grantsFor := mustProfilesRevokeEnv(t)

	out := runCli(t, "profiles", "revoke", "team-a", "db3", "web1")
	if !strings.Contains(out, "db3: 未授权，跳过") {
		t.Fatalf("未授权的服务器应提示跳过：\n%s", out)
	}
	if !strings.Contains(out, "revoked 1 server(s) from team-a") {
		t.Fatalf("跳过不算错误，已授权的照常撤销：\n%s", out)
	}
	if got := grantsFor(t, "team-a"); len(got) != 1 || got[0] != "web2" {
		t.Fatalf("撤销后授权 = %v，应只剩 web2", got)
	}
}

func TestProfilesRevokeErrors(t *testing.T) {
	grantsFor := mustProfilesRevokeEnv(t)

	if out := runCliErr(t, "profiles", "revoke", "ghost", "web1"); !strings.Contains(out, `profile "ghost" not found`) {
		t.Fatalf("profile 不存在应报错：\n%s", out)
	}
	if out := runCliErr(t, "profiles", "revoke", "team-a", "no-such-server"); !strings.Contains(out, `server "no-such-server" not found`) {
		t.Fatalf("服务器不存在应报错：\n%s", out)
	}
	if got := grantsFor(t, "team-a"); len(got) != 2 {
		t.Fatalf("失败撤销不应改变授权：%v", got)
	}
}

func TestProfilesRevokeAuditRows(t *testing.T) {
	_, _, rowsFor := newProfilesAuditEnv(t)
	runCli(t, "profiles", "add", "team-a")
	runCli(t, "servers", "add", "--name", "web1", "--host", "192.0.2.10", "--user", "u")
	runCli(t, "servers", "add", "--name", "web2", "--host", "192.0.2.11", "--user", "u")
	runCli(t, "servers", "add", "--name", "db3", "--host", "192.0.2.12", "--user", "u")
	runCli(t, "profiles", "grant", "team-a", "web1", "web2")
	runCli(t, "profiles", "revoke", "team-a", "web1")
	runCli(t, "profiles", "revoke", "team-a", "web1") // 已撤销：跳过，不算错误
	runCliErr(t, "profiles", "revoke", "team-a", "no-such-server")

	rows := rowsFor(t, "profile.revoke")
	if len(rows) != 3 {
		t.Fatalf("profile.revoke 行数 = %d，应为 3：%+v", len(rows), rows)
	}
	assertProfileAuditRow(t, rows[0], "profile.revoke", "error", `{"profile":"team-a","servers":"1台:no-such-server"}`)
	assertProfileAuditRow(t, rows[1], "profile.revoke", "ok", `{"profile":"team-a","servers":"1台:web1"}`)
	assertProfileAuditRow(t, rows[2], "profile.revoke", "ok", `{"profile":"team-a","servers":"1台:web1"}`)
	for _, r := range rows {
		if r.Source != store.AuditSourceCLI {
			t.Fatalf("命令行撤销的审计来源应为命令行：%+v", r)
		}
	}
}
