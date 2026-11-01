package store

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// pinEntryForTest 取一个条目的锚（含来源信息）。
func pinEntryForTest(t *testing.T, s *Store, id string) PinMeta {
	t.Helper()
	for _, p := range mustEntryPins(t, s) {
		if p.ServerID == id {
			return p
		}
	}
	t.Fatalf("条目 %s 没有锚", id)
	return PinMeta{}
}

func mustEntryPins(t *testing.T, s *Store) []PinMeta {
	t.Helper()
	pins, err := s.ListEntryPins()
	if err != nil {
		t.Fatal(err)
	}
	return pins
}

func findSnapshotEntryPin(snap *Snapshot, serverID string) *SnapshotEntryHostKey {
	for i := range snap.EntryHostKeys {
		if snap.EntryHostKeys[i].ServerID == serverID {
			return &snap.EntryHostKeys[i]
		}
	}
	return nil
}

func snapshotLegacyRows(snap *Snapshot) map[string][]byte {
	out := map[string][]byte{}
	for _, h := range snap.HostKeys {
		out[h.HostPort] = h.KeyBlob
	}
	return out
}

// TestSnapshotEntryPins_ExportAndLegacyDerivation 覆盖快照格式：按条目的锚
// 全量携带；旧格式行只为兜底地址推导，同地址密钥全相同才生成一行，密钥不
// 同时这一行不生成；按 profile 裁剪时按条目裁剪，旧格式行由裁剪后的条目推导。
func TestSnapshotEntryPins_ExportAndLegacyDerivation(t *testing.T) {
	s := newTestStore(t)
	rootID := addEntryForTest(t, s, "root", "10.0.0.5", 22)
	deployID := addEntryForTest(t, s, "deploy", "10.0.0.5", 22)
	splitA := addEntryForTest(t, s, "split-a", "10.0.0.6", 22)
	splitB := addEntryForTest(t, s, "split-b", "10.0.0.6", 22)
	labID := addEntryForTest(t, s, "lab", "10.0.0.7", 2200)
	profID, err := s.AddProfile("team-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GrantServers(profID, []string{rootID}); err != nil {
		t.Fatal(err)
	}

	sharedKey, _ := pinHostKey(t)
	splitKeyA, _ := pinHostKey(t)
	splitKeyB, _ := pinHostKey(t)
	labKey, _ := pinHostKey(t)
	for _, seed := range []struct {
		id  string
		key []byte
	}{
		{rootID, sharedKey}, {deployID, sharedKey},
		{splitA, splitKeyA}, {splitB, splitKeyB}, {labID, labKey},
	} {
		srv, err := s.GetServer(seed.id)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SaveEntryPin(srv, seed.key); err != nil {
			t.Fatal(err)
		}
	}

	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Version != 1 {
		t.Fatalf("快照信封版本 = %d，应为 1", snap.Version)
	}
	if got := len(snap.EntryHostKeys); got != 5 {
		t.Fatalf("按条目的锚数量 = %d，应为 5：%+v", got, snap.EntryHostKeys)
	}
	for _, id := range []string{rootID, deployID, splitA, splitB, labID} {
		p := findSnapshotEntryPin(snap, id)
		if p == nil || p.HandshakeAddr == "" {
			t.Fatalf("快照缺少条目 %s 的锚或握手地址：%+v", id, p)
		}
	}

	legacy := snapshotLegacyRows(snap)
	if len(legacy) != 2 {
		t.Fatalf("旧格式行数量 = %d，应为 2（同地址密钥不同的行不生成）：%+v", len(legacy), legacy)
	}
	if !bytes.Equal(legacy["10.0.0.5:22"], sharedKey) {
		t.Fatalf("同地址密钥相同应生成一行旧格式锚：%+v", legacy)
	}
	if !bytes.Equal(legacy["10.0.0.7:2200"], labKey) {
		t.Fatalf("单条目地址应生成旧格式锚：%+v", legacy)
	}
	if _, ok := legacy["10.0.0.6:22"]; ok {
		t.Fatalf("同地址密钥不同时不应生成旧格式锚：%+v", legacy)
	}

	scoped, err := s.ExportSnapshotForProfile(profID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.EntryHostKeys) != 1 || scoped.EntryHostKeys[0].ServerID != rootID {
		t.Fatalf("按 profile 裁剪后的按条目锚 = %+v", scoped.EntryHostKeys)
	}
	scopedLegacy := snapshotLegacyRows(scoped)
	if len(scopedLegacy) != 1 || !bytes.Equal(scopedLegacy["10.0.0.5:22"], sharedKey) {
		t.Fatalf("裁剪后的旧格式行应由裁剪后的条目推导：%+v", scopedLegacy)
	}
}

// TestImportSnapshot_OldShapeAppliesEntryMigration 导入只有旧格式锚的快照
// 或备份时，执行与升级相同的迁移规则：旧锚复制给同地址条目，孤立锚丢弃并
// 写迁移审计。
func TestImportSnapshot_OldShapeAppliesEntryMigration(t *testing.T) {
	rootKey, _ := pinHostKey(t)
	labKey, _ := pinHostKey(t)
	orphanKey, orphanFP := pinHostKey(t)
	snap := &Snapshot{
		Version: 1,
		Servers: []SnapshotServer{
			{ID: "srv-root", Name: "root", Host: "10.0.0.5", Port: 22, User: "u", AuthMethod: "password", CreatedAt: 1700000000, UpdatedAt: 1700000000},
			{ID: "srv-deploy", Name: "deploy", Host: "10.0.0.5", Port: 22, User: "d", AuthMethod: "password", CreatedAt: 1700000000, UpdatedAt: 1700000000},
			{ID: "srv-lab", Name: "lab", Host: "10.0.0.9", Port: 2222, User: "u", AuthMethod: "password", CreatedAt: 1700000000, UpdatedAt: 1700000000},
		},
		HostKeys: []SnapshotHostKey{
			{HostPort: "10.0.0.5:22", KeyBlob: rootKey, CreatedAt: 1700000000, PinFormat: PinFormatBlob, PinSource: PinSourceTofu},
			{HostPort: "10.0.0.9:2222", KeyBlob: labKey, CreatedAt: 1700000000, PinFormat: PinFormatBlob, PinSource: PinSourceForward, PinDevice: "laptop"},
			{HostPort: "192.0.2.1:22", KeyBlob: orphanKey, CreatedAt: 1700000000, PinFormat: PinFormatBlob, PinSource: PinSourceTofu},
		},
		Audit: []SnapshotAudit{{TS: 1699999999, ServerID: "srv-root", Action: "exec", Command: "history-before-migration", Status: "ok"}},
	}

	b := newTestStore(t)
	if err := b.ImportSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	pins := mustEntryPins(t, b)
	if len(pins) != 3 {
		t.Fatalf("导入迁移后的按条目锚 = %+v", pins)
	}
	root := pinEntryForTest(t, b, "srv-root")
	deploy := pinEntryForTest(t, b, "srv-deploy")
	lab := pinEntryForTest(t, b, "srv-lab")
	if !root.Matches(rootKey) || !deploy.Matches(rootKey) {
		t.Fatalf("同地址两个条目没有各自继承旧锚：%+v %+v", root, deploy)
	}
	if !lab.Matches(labKey) || lab.Source != PinSourceForward || lab.Device != "laptop" {
		t.Fatalf("转发来源的旧锚没有完整迁移：%+v", lab)
	}
	migrations, err := b.QueryAudit(AuditFilter{Actions: []string{"pin-migrate"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 1 || !containsAll(migrations[0].Command, "192.0.2.1:22", orphanFP) {
		t.Fatalf("导入迁移审计不正确：%+v", migrations)
	}
	history, err := b.QueryAudit(AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Action != "pin-migrate" || history[1].Command != "history-before-migration" {
		t.Fatalf("导入顺序应先恢复历史审计再写迁移审计：%+v", history)
	}
	var legacy int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM host_keys`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy != 0 {
		t.Fatalf("导入迁移后旧锚表应清空，仍有 %d 行", legacy)
	}
}

// TestImportSnapshot_EntryPinsRoundTrip 新格式快照往返：按条目的锚原样导入，
// 随快照携带的推导旧格式行不再作为事实源重复导入；再导出时推导结果一致。
func TestImportSnapshot_EntryPinsRoundTrip(t *testing.T) {
	a := newTestStore(t)
	rootID := addEntryForTest(t, a, "root", "10.0.0.5", 22)
	deployID := addEntryForTest(t, a, "deploy", "10.0.0.5", 22)
	sharedKey, _ := pinHostKey(t)
	otherKey, _ := pinHostKey(t)
	for _, seed := range []struct {
		id  string
		key []byte
	}{{rootID, sharedKey}, {deployID, otherKey}} {
		srv, err := a.GetServer(seed.id)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.SaveEntryPin(srv, seed.key); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := a.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshotLegacyRows(snap)["10.0.0.5:22"]; ok {
		t.Fatal("同地址密钥不同时导出不应带旧格式行")
	}

	b := newTestStore(t)
	if err := b.ImportSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	if got := mustEntryPins(t, b); len(got) != 2 {
		t.Fatalf("导入后的按条目锚 = %+v", got)
	}
	if p := pinEntryForTest(t, b, rootID); !p.Matches(sharedKey) {
		t.Fatalf("条目 %s 的锚往返后改变：%+v", rootID, p)
	}
	again, err := b.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(again.EntryHostKeys) != 2 || len(again.HostKeys) != 0 {
		t.Fatalf("再导出应与推导规则一致：%+v %+v", again.EntryHostKeys, again.HostKeys)
	}
}

// TestSnapshotEntryPins_OldClientDecode 旧版快照结构解码新快照：新增字段
// 不影响旧客户端，旧格式行按地址不重复。
func TestSnapshotEntryPins_OldClientDecode(t *testing.T) {
	s := newTestStore(t)
	a := addEntryForTest(t, s, "a", "10.0.0.5", 22)
	b := addEntryForTest(t, s, "b", "10.0.0.5", 22)
	c := addEntryForTest(t, s, "c", "10.0.0.6", 22)
	sharedKey, _ := pinHostKey(t)
	otherKey, _ := pinHostKey(t)
	for _, seed := range []struct {
		id  string
		key []byte
	}{{a, sharedKey}, {b, sharedKey}, {c, otherKey}} {
		srv, err := s.GetServer(seed.id)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SaveEntryPin(srv, seed.key); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}

	// 旧版快照结构：没有按条目锚字段，解码时忽略新增字段。
	var legacy struct {
		Version  int               `json:"version"`
		Servers  []SnapshotServer  `json:"servers"`
		HostKeys []SnapshotHostKey `json:"host_keys"`
	}
	if err := json.Unmarshal(wire, &legacy); err != nil {
		t.Fatalf("旧结构解码新快照失败：%v", err)
	}
	seen := map[string]bool{}
	for _, h := range legacy.HostKeys {
		if seen[h.HostPort] {
			t.Fatalf("旧格式行重复：%+v", legacy.HostKeys)
		}
		seen[h.HostPort] = true
	}
	if !seen["10.0.0.5:22"] || !seen["10.0.0.6:22"] {
		t.Fatalf("旧客户端应仍拿到兜底地址的旧格式锚：%+v", legacy.HostKeys)
	}
	_ = models.AuthPassword
	_ = time.Now
}
