package store

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
)

// snapshotSeed 是快照修订号测试的基础库:一台已授权的服务器、一个
// profile、一个项目。各操作表条目可以用 setup 先补充自己的前置状态,
// 让 op 只包含被测的那一次写入。
type snapshotSeed struct {
	serverID        string
	profileID       string
	rebindProfileID string
	projectID       string
}

func seedSnapshotStore(t *testing.T) (*Store, *snapshotSeed) {
	t.Helper()
	s := newTestStore(t)
	cid, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	serverID, err := s.AddServer(&models.Server{
		Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u",
		AuthMethod: models.AuthPassword, CredentialID: cid,
	})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := s.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(profileID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	projectID, _, err := s.AddProject("proj-x", profileID)
	if err != nil {
		t.Fatal(err)
	}
	return s, &snapshotSeed{serverID: serverID, profileID: profileID, projectID: projectID}
}

// snapshotContent 返回当前全库导出内容(把修订号本身清零,内容比较只看
// 配置数据)以及存储层修订号。
func snapshotContent(t *testing.T, s *Store) (int64, string) {
	t.Helper()
	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	rev := snap.SnapshotRevision
	snap.SnapshotRevision = 0
	blob, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	return rev, string(blob)
}

// TestSnapshotRevision_MigrationStartsAtOne: 新库的修订号从 1 开始,并且跨
// 开关持久。
func TestSnapshotRevision_MigrationStartsAtOne(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	mk := make([]byte, 32)
	randRead(t, mk)
	s, err := Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rev, err := s2.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 {
		t.Fatalf("新库快照修订号 = %d,应为 1", rev)
	}
}

// TestSnapshotRevision_LegacyDatabaseMigratesToOne: 旧结构库没有修订号表,
// 第一次打开时迁移为 1,之后的配置写入照常递增。
func TestSnapshotRevision_LegacyDatabaseMigratesToOne(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "store.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(oldShapeServersProjects); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	mk := make([]byte, 32)
	randRead(t, mk)
	migrated, err := Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	rev, err := migrated.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if rev != 1 {
		t.Fatalf("迁移后的快照修订号 = %d,应为 1", rev)
	}
	if _, err := migrated.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", Port: 22, User: "u", AuthMethod: models.AuthPassword}); err != nil {
		t.Fatal(err)
	}
	after, err := migrated.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if after <= rev {
		t.Fatalf("迁移后写入的快照修订号 = %d,应大于 %d", after, rev)
	}
}

// TestSnapshotRevision_AdvancesOnEverySnapshotWrite: 存储层性质测试——只要
// 导出快照内容变了,修订号就增加。新写入路径出现时,在这张表里加一行。
func TestSnapshotRevision_AdvancesOnEverySnapshotWrite(t *testing.T) {
	defaultRuleID := policy.DefaultRuleIDs()[0]
	newServer := func(name, host string) *models.Server {
		return &models.Server{Name: name, Host: host, Port: 22, User: "u", AuthMethod: models.AuthPassword}
	}
	cases := []struct {
		name string
		// sameWholeSnapshot marks operations that change which profile a device
		// code can see, rather than the whole-vault export itself.
		sameWholeSnapshot bool
		setup             func(t *testing.T, s *Store, seed *snapshotSeed)
		op                func(t *testing.T, s *Store, seed *snapshotSeed) error
	}{
		{
			name: "新增服务器",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.AddServer(newServer("extra", "192.0.2.11"))
				return err
			},
		},
		{
			name: "更新服务器",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				srv, err := s.GetServer(seed.serverID)
				if err != nil {
					return err
				}
				srv.Role = "edge gateway"
				return s.UpdateServer(srv)
			},
		},
		{
			name: "删除服务器",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.DeleteServer(seed.serverID)
			},
		},
		{
			name: "新增凭据",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("new")})
				return err
			},
		},
		{
			name: "带凭据新增服务器",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.AddServerWithCredentials(newServer("boxed", "192.0.2.12"),
					&models.Credential{Type: models.CredPassword, Secret: []byte("boxed")}, nil)
				return err
			},
		},
		{
			name: "清除服务器凭据",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.ClearServerCredential(seed.serverID)
			},
		},
		{
			name: "新增 profile",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.AddProfile("team-b")
				return err
			},
		},
		{
			name:              "改绑设备码",
			sameWholeSnapshot: true,
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				profileID, err := s.AddProfile("team-b")
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.AddCacheToken("laptop", seed.profileID); err != nil {
					t.Fatal(err)
				}
				seed.rebindProfileID = profileID
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.BindCacheToken("laptop", seed.rebindProfileID)
			},
		},
		{
			name: "授权服务器",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				id, err := s.AddServer(newServer("grant-me", "192.0.2.13"))
				if err != nil {
					t.Fatal(err)
				}
				seed.serverID = id
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.GrantServers(seed.profileID, []string{seed.serverID})
			},
		},
		{
			name: "撤销授权",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, _, err := s.SyncServers(seed.profileID, nil)
				return err
			},
		},
		{
			name: "删除 profile",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				id, err := s.AddProfile("team-b")
				if err != nil {
					t.Fatal(err)
				}
				seed.profileID = id
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.DeleteProfile(seed.profileID)
			},
		},
		{
			name: "新增项目",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, _, err := s.AddProject("proj-y", seed.profileID)
				return err
			},
		},
		{
			name: "停用项目",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.SetProjectStatus(seed.projectID, models.ProjectDisabled)
			},
		},
		{
			name: "轮换项目令牌",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.RotateProject(seed.projectID)
				return err
			},
		},
		{
			name: "删除项目",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if err := s.SetProjectStatus(seed.projectID, models.ProjectRevoked); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.DeleteProject(seed.projectID)
			},
		},
		{
			name: "新增命令策略规则",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.AddPolicyRule(policy.CustomRule{
					ID: "NO-EXAMPLE", Commands: []string{"example"}, Disposition: policy.DispositionBlock,
					Explanation: "示例规则",
				})
			},
		},
		{
			name: "删除命令策略规则",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if err := s.AddPolicyRule(policy.CustomRule{
					ID: "NO-EXAMPLE", Commands: []string{"example"}, Disposition: policy.DispositionBlock,
					Explanation: "示例规则",
				}); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.DeletePolicyRule("NO-EXAMPLE")
				return err
			},
		},
		{
			name: "停用默认命令策略规则",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.DisableDefaultRule(defaultRuleID)
				return err
			},
		},
		{
			name: "启用默认命令策略规则",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.DisableDefaultRule(defaultRuleID); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.EnableDefaultRule(defaultRuleID)
				return err
			},
		},
		{
			name: "手动锚定主机密钥",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.UpsertManualPin("192.0.2.10", 22, []byte("anchor-blob"), PinFormatBlob, false,
					func(old *PinMeta) AuditRow { return AuditRow{Action: "pin-manual"} })
				return err
			},
		},
		{
			name: "首次信任主机密钥",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				return s.SaveHostKey("192.0.2.10", 22, []byte("tofu-blob"))
			},
		},
		{
			name: "清除主机密钥锚",
			setup: func(t *testing.T, s *Store, seed *snapshotSeed) {
				if _, err := s.UpsertManualPin("192.0.2.10", 22, []byte("anchor-blob"), PinFormatBlob, false,
					func(old *PinMeta) AuditRow { return AuditRow{Action: "pin-manual"} }); err != nil {
					t.Fatal(err)
				}
			},
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.ClearPin("192.0.2.10", 22, func(old PinMeta) AuditRow { return AuditRow{Action: "pin-clear"} })
				return err
			},
		},
		{
			name: "客户端转发主机密钥锚",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				_, err := s.InsertForwardedPin("192.0.2.10", 22, []byte("forwarded-blob"), "laptop",
					AuditRow{Action: "pin-forward", Status: "ok"})
				return err
			},
		},
		{
			name: "编辑服务器元数据",
			op: func(t *testing.T, s *Store, seed *snapshotSeed) error {
				role := "edge gateway"
				_, err := s.UpdateForwardedMetadata(seed.serverID, map[string]*string{"role": &role}, 0, "laptop")
				return err
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			s, seed := seedSnapshotStore(t)
			if tc.setup != nil {
				tc.setup(t, s, seed)
			}
			before, beforeContent := snapshotContent(t, s)
			if err := tc.op(t, s, seed); err != nil {
				t.Fatalf("执行写入失败:%v", err)
			}
			after, afterContent := snapshotContent(t, s)
			if !tc.sameWholeSnapshot && beforeContent == afterContent {
				t.Fatalf("操作表条目「%s」没有改变导出快照内容,请修正测试或操作表", tc.name)
			}
			if after <= before {
				t.Fatalf("「%s」后快照修订号 = %d,应大于 %d", tc.name, after, before)
			}
		})
	}
}

// TestSnapshotRevision_AuditOnlyWritesDoNotAdvance: 只写审计与设备码状态的
// 操作不改变快照修订号。
func TestSnapshotRevision_AuditOnlyWritesDoNotAdvance(t *testing.T) {
	s, seed := seedSnapshotStore(t)
	tokenID, _, err := s.AddCacheToken("laptop", seed.profileID)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := snapshotContent(t, s)
	writes := []struct {
		name string
		run  func() error
	}{
		{"普通远程命令审计", func() error {
			return s.WriteAudit(AuditRow{Action: "exec", ServerID: seed.serverID, Command: "uptime", Status: "ok"})
		}},
		{"记录设备码最后拉取时间", func() error { return s.TouchCacheToken(tokenID, "1.2.3") }},
		{"吊销设备码", func() error { return s.RevokeCacheToken("laptop") }},
	}
	for _, w := range writes {
		if err := w.run(); err != nil {
			t.Fatalf("%s:%v", w.name, err)
		}
		after, _ := snapshotContent(t, s)
		if after != before {
			t.Fatalf("%s 后快照修订号 = %d,应保持 %d", w.name, after, before)
		}
	}
}

// TestSnapshotRevision_RidesBothExportPaths: 两条导出路径都携带顶层
// snapshot_revision,信封版本号保持 1;导入后的修订号必须大于导入前的当前
// 值与快照携带值,保持单调递增。
func TestSnapshotRevision_RidesBothExportPaths(t *testing.T) {
	s, seed := seedSnapshotStore(t)
	if _, err := s.AddServer(&models.Server{Name: "extra", Host: "192.0.2.11", Port: 22, User: "u", AuthMethod: models.AuthPassword}); err != nil {
		t.Fatal(err)
	}
	rev, err := s.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	whole, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if whole.Version != 1 || whole.SnapshotRevision != rev {
		t.Fatalf("全库导出:信封版本 = %d,快照修订号 = %d,应为 1/%d", whole.Version, whole.SnapshotRevision, rev)
	}
	scoped, err := s.ExportSnapshotForProfile(seed.profileID, false)
	if err != nil {
		t.Fatal(err)
	}
	if scoped.Version != 1 || scoped.SnapshotRevision != rev {
		t.Fatalf("profile 导出:信封版本 = %d,快照修订号 = %d,应为 1/%d", scoped.Version, scoped.SnapshotRevision, rev)
	}

	mk := make([]byte, 32)
	randRead(t, mk)
	target, err := Open(filepath.Join(t.TempDir(), "import.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	// 目标库先做几轮「新增又删除服务器」的空库写入,让导入前的修订号高于
	// 快照携带值,覆盖「取两者较大值」这一侧。
	for i := 0; i < int(rev)+2; i++ {
		id, aerr := target.AddServer(&models.Server{
			Name: "temporary", Host: "192.0.2.99", Port: 22, User: "u", AuthMethod: models.AuthPassword,
		})
		if aerr != nil {
			t.Fatal(aerr)
		}
		if derr := target.DeleteServer(id); derr != nil {
			t.Fatal(derr)
		}
	}
	currentBefore, err := target.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if currentBefore <= rev {
		t.Fatalf("测试前置失败:导入前修订号 = %d,应大于快照携带值 %d", currentBefore, rev)
	}
	if err := target.ImportSnapshot(whole); err != nil {
		t.Fatal(err)
	}
	imported, err := target.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	if imported <= currentBefore || imported <= rev {
		t.Fatalf("导入后的快照修订号 = %d,应大于导入前值 %d 与快照携带值 %d", imported, currentBefore, rev)
	}
	if imported != currentBefore+1 {
		t.Fatalf("导入后的快照修订号 = %d,应为导入前值与快照携带值中较大者的后一个值 %d", imported, currentBefore+1)
	}
}

// TestSnapshotRevision_LegacySnapshotDecodesAsZero: 旧版快照结构没有
// snapshot_revision 字段,解码为 0,不影响导入。
func TestSnapshotRevision_LegacySnapshotDecodesAsZero(t *testing.T) {
	legacy := `{"version":1,"credentials":[],"servers":[],"profiles":[],"grants":[],"projects":[],"host_keys":[],"audit":[]}`
	var snap Snapshot
	if err := json.Unmarshal([]byte(legacy), &snap); err != nil {
		t.Fatalf("旧版快照解码失败:%v", err)
	}
	if snap.SnapshotRevision != 0 {
		t.Fatalf("旧版快照修订号 = %d,应为 0", snap.SnapshotRevision)
	}
	s := newTestStore(t)
	if err := s.ImportSnapshot(&snap); err != nil {
		t.Fatal(err)
	}
}
