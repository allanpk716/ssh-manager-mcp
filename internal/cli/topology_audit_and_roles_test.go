package cli

// 网络拓扑命令的审计与角色守卫测试。审计只从存储层的公开查询接口读回；
// 角色用例覆盖权威端、客户端与既无 vault 又无缓存的机器。

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// topologyAuditRows 打开临时 vault，读回指定动作的全部审计行。
func topologyAuditRows(t *testing.T, dbPath string, masterKey []byte, action string) []store.AuditRow {
	t.Helper()
	st, err := store.Open(dbPath, masterKey)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	rows, err := st.QueryAudit(store.AuditFilter{Actions: []string{action}})
	if err != nil {
		t.Fatalf("query audit %s: %v", action, err)
	}
	return rows
}

// topologyAuditSummary 解开审计行里的 JSON 摘要。
func topologyAuditSummary(t *testing.T, row store.AuditRow) map[string]any {
	t.Helper()
	var summary map[string]any
	if err := json.Unmarshal([]byte(row.Command), &summary); err != nil {
		t.Fatalf("audit summary is not JSON: %v\n%q", err, row.Command)
	}
	return summary
}

// TestTopologyWritesAuditOnce 覆盖每个成功写操作恰好一行审计，并钉住摘要
// 的白名单字段：只有对象名、类型、站点、备注、网段、带宽与计费标记。
func TestTopologyWritesAuditOnce(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	runCli(t, "sites", "add", "家A", "--note", "初始备注")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A",
		"--note", "初始备注", "--cidr", "192.168.1.0/24", "--bandwidth-mbps", "1000", "--metered")
	runCli(t, "sites", "edit", "家A", "--name", "家B", "--note", "新备注")
	runCli(t, "networks", "edit", "家A局域网", "--name", "家A主网", "--site", "家B",
		"--note", "新备注", "--cidr", "10.0.0.0/8", "--bandwidth-mbps", "2000", "--metered=false")
	runCli(t, "devices", "add", "工作站")
	runCli(t, "devices", "member", "add", "工作站", "--network", "家A主网", "--address", "192.168.1.20")
	runCli(t, "networks", "rm", "家A主网", "--yes")
	runCli(t, "sites", "rm", "家B")

	for _, tc := range []struct {
		action string
		want   map[string]any
	}{
		{
			action: "site.add",
			want:   map[string]any{"name": "家A", "note": "初始备注"},
		},
		{
			action: "site.edit",
			want: map[string]any{
				"name": "家A",
				"changes": map[string]any{
					"name": "家B", "note": "新备注",
				},
			},
		},
		{
			action: "site.rm",
			want:   map[string]any{"name": "家B"},
		},
		{
			action: "network.add",
			want: map[string]any{
				"name": "家A局域网", "type": "physical", "site": "家A", "note": "初始备注",
				"cidr": "192.168.1.0/24", "bandwidth-mbps": float64(1000), "metered": true,
			},
		},
		{
			action: "network.edit",
			want: map[string]any{
				"name": "家A局域网",
				"changes": map[string]any{
					"name": "家A主网", "site": "家B", "note": "新备注",
					"cidr": "10.0.0.0/8", "bandwidth-mbps": float64(2000), "metered": false,
				},
			},
		},
		{
			action: "network.rm",
			want:   map[string]any{"name": "家A主网"},
		},
	} {
		rows := topologyAuditRows(t, dbPath, masterKey, tc.action)
		if len(rows) != 1 {
			t.Fatalf("action %s must leave exactly one audit row, got %d", tc.action, len(rows))
		}
		if rows[0].Status != "ok" || rows[0].ProjectID != "" {
			t.Fatalf("action %s 必须是成功的 owner 审计行: %+v", tc.action, rows[0])
		}
		got := topologyAuditSummary(t, rows[0])
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("action %s summary = %#v, want %#v", tc.action, got, tc.want)
		}
	}
	if rows := topologyAuditRows(t, dbPath, masterKey, "device.member-rm"); len(rows) != 0 {
		t.Fatalf("network member cascade must not write member removal audits: %+v", rows)
	}
}

// TestDevicesWriteAuditOnce 覆盖设备与网络成员的每个成功写操作恰好一行
// 审计；删除网络的成员级联不额外写成员审计。
func TestDevicesWriteAuditOnce(t *testing.T) {
	dbPath, masterKey := withCliStoreEnv(t)
	seedTopologyClient(t, dbPath, masterKey, "laptop")
	seedTopologySites(t, "家A")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")
	runCli(t, "devices", "add", "工作站", "--note", "初始备注", "--client", "laptop")
	runCli(t, "devices", "member", "add", "工作站", "--network", "家A局域网", "--address", "192.168.1.20")
	runCli(t, "devices", "member", "edit", "工作站", "--network", "家A局域网", "--address", "192.168.1.21")
	runCli(t, "devices", "member", "rm", "工作站", "--network", "家A局域网")
	runCli(t, "devices", "edit", "工作站", "--name", "新工作站", "--note", "新备注", "--no-client")
	runCli(t, "devices", "rm", "新工作站", "--yes")

	for _, tc := range []struct {
		action string
		want   map[string]any
	}{
		{
			action: "device.add",
			want:   map[string]any{"name": "工作站", "note": "初始备注", "client": "laptop"},
		},
		{
			action: "device.member-add",
			want:   map[string]any{"device": "工作站", "network": "家A局域网", "address": "192.168.1.20"},
		},
		{
			action: "device.member-edit",
			want:   map[string]any{"device": "工作站", "network": "家A局域网", "address": "192.168.1.21"},
		},
		{
			action: "device.member-rm",
			want:   map[string]any{"device": "工作站", "network": "家A局域网", "address": "192.168.1.21"},
		},
		{
			action: "device.edit",
			want: map[string]any{
				"name": "工作站",
				"changes": map[string]any{
					"name": "新工作站", "note": "新备注", "client": "",
				},
			},
		},
		{
			action: "device.rm",
			want: map[string]any{
				"name": "新工作站", "client": "", "members": []any{},
			},
		},
	} {
		rows := topologyAuditRows(t, dbPath, masterKey, tc.action)
		if len(rows) != 1 {
			t.Fatalf("action %s must leave exactly one audit row, got %d", tc.action, len(rows))
		}
		got := topologyAuditSummary(t, rows[0])
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("action %s summary = %#v, want %#v", tc.action, got, tc.want)
		}
	}
}

// TestTopologyClientRoleRefused 覆盖客户端角色：写命令统一拒绝并提示权威
// 端；查看命令明确说明拓扑数据这一版尚未随快照下发。
func TestTopologyClientRoleRefused(t *testing.T) {
	seedClientCacheFixture(t)
	writes := [][]string{
		{"sites", "add", "家A"},
		{"sites", "edit", "家A", "--note", "新备注"},
		{"sites", "rm", "家A"},
		{"networks", "add", "家A局域网", "--type", "physical", "--site", "家A"},
		{"networks", "edit", "家A局域网", "--note", "新备注"},
		{"networks", "rm", "家A局域网", "--yes"},
		{"devices", "add", "工作站"},
		{"devices", "edit", "工作站", "--note", "新备注"},
		{"devices", "rm", "工作站", "--yes"},
		{"devices", "move-server", "web", "--to", "工作站"},
		{"devices", "member", "add", "工作站", "--network", "家A局域网", "--address", "192.168.1.20"},
		{"devices", "member", "edit", "工作站", "--network", "家A局域网", "--address", "192.168.1.21"},
		{"devices", "member", "rm", "工作站", "--network", "家A局域网"},
	}
	for _, args := range writes {
		errText := runCliErr(t, args...)
		if !strings.Contains(errText, "权威端") {
			t.Fatalf("client write %v must point at the authoritative machine, got:\n%s", args, errText)
		}
	}
	for _, args := range [][]string{{"sites", "list"}, {"networks", "list"}, {"devices", "list"}, {"devices", "show", "工作站"}} {
		errText := runCliErr(t, args...)
		if !strings.Contains(errText, "网络拓扑这一版尚未随快照下发") {
			t.Fatalf("client view %v must explain that topology is not delivered yet, got:\n%s", args, errText)
		}
	}
}

// TestTopologyWithoutVaultOrCacheGuidesInitialization 覆盖既没有 vault 也
// 没有客户端缓存的机器：写与查看都给初始化引导，不伪装成空拓扑。
func TestTopologyWithoutVaultOrCacheGuidesInitialization(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_STORE":         filepath.Join(dir, "no-vault.db"),
		"SSHMGR_CACHE_DIR":     dir,
		"SSHMGR_CACHE_DEK":     "",
		"SSHMGR_MASTERKEY_HEX": "",
		"SSHMGR_FILEKEY_PATH":  filepath.Join(dir, "no-such-master.key"),
	})
	for _, args := range [][]string{
		{"sites", "add", "家A"},
		{"sites", "list"},
		{"networks", "add", "家A局域网", "--type", "virtual"},
		{"networks", "list"},
		{"devices", "add", "工作站"},
		{"devices", "list"},
		{"devices", "show", "工作站"},
	} {
		errText := runCliErr(t, args...)
		if !strings.Contains(errText, "请先运行初始化向导") {
			t.Fatalf("%v must guide initialization, got:\n%s", args, errText)
		}
	}
}
