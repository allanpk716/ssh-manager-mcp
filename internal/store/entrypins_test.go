package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// addEntryForTest 登记一个最小可用的服务器条目，返回条目标识符。
func addEntryForTest(t *testing.T, s *Store, name, host string, port int) string {
	t.Helper()
	id, err := s.AddServer(&models.Server{
		Name: name, Host: host, Port: port, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// serverOf 取一个条目的完整模型。
func serverOf(t *testing.T, s *Store, id string) *models.Server {
	t.Helper()
	srv, err := s.GetServer(id)
	if err != nil || srv == nil {
		t.Fatalf("取条目 %s 失败：%v %v", id, srv, err)
	}
	return srv
}

// TestEntryPins_SameAddressIndependent 同一兜底地址上的两个条目各自锚定、
// 各自校验：一个条目的锚不影响另一个（规格 #88 用户故事 15）。
func TestEntryPins_SameAddressIndependent(t *testing.T) {
	s := newTestStore(t)
	a := addEntryForTest(t, s, "a", "192.168.1.10", 22)
	b := addEntryForTest(t, s, "b", "192.168.1.10", 22)
	keyA, _ := pinHostKey(t)
	keyB, _ := pinHostKey(t)

	if err := s.SaveEntryPin(&models.Server{ID: a, Host: "192.168.1.10", Port: 22}, keyA); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveEntryPin(&models.Server{ID: b, Host: "192.168.1.10", Port: 22}, keyB); err != nil {
		t.Fatal(err)
	}
	pinA, err := s.GetEntryPin(a)
	if err != nil {
		t.Fatal(err)
	}
	pinB, err := s.GetEntryPin(b)
	if err != nil {
		t.Fatal(err)
	}
	if pinA == nil || pinB == nil || !pinA.Matches(keyA) || !pinB.Matches(keyB) {
		t.Fatalf("同地址两个条目的锚互相影响：a=%v b=%v", pinA, pinB)
	}
	if pinA.Matches(keyB) || pinB.Matches(keyA) {
		t.Fatal("同地址两个条目的锚被写成了同一把密钥")
	}
}

// TestInsertForwardedEntryPin_OnlyInsert 覆盖转发落锚的仅可新增语义：空位
// 插入并写审计；密钥相同的重复转发返回相同；密钥不同返回不同且绝不覆盖。
func TestInsertForwardedEntryPin_OnlyInsert(t *testing.T) {
	s := newTestStore(t)
	id := addEntryForTest(t, s, "edge", "203.0.113.10", 22)
	srv := &models.Server{ID: id, Name: "edge", Host: "203.0.113.10", Port: 22}
	key, fp := pinHostKey(t)
	other, _ := pinHostKey(t)

	equal, err := s.InsertForwardedEntryPin(srv, "203.0.113.10:22", key, "laptop", AuditRow{
		TS: time.Now(), ServerID: id, Action: "pin-forward", Command: "fp=" + fp, Status: "ok",
	})
	if err != nil || equal {
		t.Fatalf("首次转发应插入新锚：equal=%v err=%v", equal, err)
	}
	pin, err := s.GetEntryPin(id)
	if err != nil || pin == nil || !pin.Matches(key) {
		t.Fatalf("转发锚没有落库：%v %v", pin, err)
	}
	pins, _ := s.ListEntryPins()
	if len(pins) != 1 || pins[0].Source != PinSourceForward || pins[0].Device != "laptop" || pins[0].HandshakeAddr != "203.0.113.10:22" {
		t.Fatalf("转发锚来源信息不完整：%+v", pins)
	}

	if equal, err = s.InsertForwardedEntryPin(srv, "203.0.113.10:22", key, "laptop", AuditRow{TS: time.Now(), Action: "pin-forward"}); err != nil || !equal {
		t.Fatalf("相同密钥的重复转发应返回相同：equal=%v err=%v", equal, err)
	}
	if equal, err = s.InsertForwardedEntryPin(srv, "203.0.113.10:22", other, "laptop", AuditRow{TS: time.Now(), Action: "pin-forward"}); err != nil || equal {
		t.Fatalf("不同密钥的转发应返回不同且不覆盖：equal=%v err=%v", equal, err)
	}
	pin, _ = s.GetEntryPin(id)
	if pin == nil || !pin.Matches(key) {
		t.Fatal("转发通道覆盖了已有锚")
	}
	forwards, err := s.QueryAudit(AuditFilter{Actions: []string{"pin-forward"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(forwards) != 1 {
		t.Fatalf("重复与冲突转发不应再写审计行，行数=%d", len(forwards))
	}
}

// TestManualAndClearEntryPin 覆盖 owner 手工锚与按条目清除：已有锚不加
// --force 拒绝；--force 覆盖；--clear 删除并写审计，重复清除幂等。
func TestManualAndClearEntryPin(t *testing.T) {
	s := newTestStore(t)
	id := addEntryForTest(t, s, "nas", "203.0.113.20", 2200)
	srv := &models.Server{ID: id, Name: "nas", Host: "203.0.113.20", Port: 2200}
	key, fp := pinHostKey(t)
	next, nextFP := pinHostKey(t)

	if _, err := s.UpsertManualEntryPin(srv, "203.0.113.20:2200", key, PinFormatBlob, false, func(old *PinMeta) AuditRow {
		return AuditRow{TS: time.Now(), ServerID: id, Action: "pin-manual"}
	}); err != nil {
		t.Fatal(err)
	}
	old, err := s.UpsertManualEntryPin(srv, "", next, PinFormatBlob, false, func(old *PinMeta) AuditRow {
		return AuditRow{TS: time.Now(), ServerID: id, Action: "pin-manual"}
	})
	if err == nil || old == nil || !old.Matches(key) {
		t.Fatalf("已有锚不加 --force 应拒绝并带回现有锚：%v %v", old, err)
	}
	if pin, _ := s.GetEntryPin(id); pin == nil || !pin.Matches(key) {
		t.Fatal("拒绝路径不应改动锚")
	}
	if _, err = s.UpsertManualEntryPin(srv, "", next, PinFormatBlob, true, func(old *PinMeta) AuditRow {
		return AuditRow{TS: time.Now(), ServerID: id, Action: "pin-manual", Command: "was=" + fp + " fp=" + nextFP}
	}); err != nil {
		t.Fatal(err)
	}
	if pin, _ := s.GetEntryPin(id); pin == nil || !pin.Matches(next) {
		t.Fatal("--force 没有覆盖锚")
	}

	cleared, err := s.ClearEntryPin(id, func(old PinMeta) AuditRow {
		return AuditRow{TS: time.Now(), ServerID: id, Action: "pin-clear", Command: "fp=" + PinFingerprint(old.Blob, old.Format)}
	})
	if err != nil || cleared == nil || !cleared.Matches(next) {
		t.Fatalf("按条目清除失败：%v %v", cleared, err)
	}
	if pin, _ := s.GetEntryPin(id); pin != nil {
		t.Fatal("清除后条目仍有锚")
	}
	if again, err := s.ClearEntryPin(id, func(old PinMeta) AuditRow {
		return AuditRow{TS: time.Now(), ServerID: id, Action: "pin-clear"}
	}); err != nil || again != nil {
		t.Fatalf("重复清除应幂等：%v %v", again, err)
	}
	clears, err := s.QueryAudit(AuditFilter{Actions: []string{"pin-clear"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(clears) != 1 || !strings.Contains(clears[0].Command, nextFP) {
		t.Fatalf("清除审计不正确：%+v", clears)
	}
}

// TestEntryLifecycle_PinFollowsEntry 修改条目地址时锚保留；删除条目时锚在
// 同一事务删除并写可辨认的审计记录（带条目与指纹）。
func TestEntryLifecycle_PinFollowsEntry(t *testing.T) {
	s := newTestStore(t)
	id := addEntryForTest(t, s, "box", "198.51.100.7", 22)
	key, fp := pinHostKey(t)
	if err := s.SaveEntryPin(&models.Server{ID: id, Host: "198.51.100.7", Port: 22}, key); err != nil {
		t.Fatal(err)
	}

	srv, err := s.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	srv.Host = "198.51.100.8"
	srv.Port = 2222
	if err := s.UpdateServer(srv); err != nil {
		t.Fatal(err)
	}
	if pin, err := s.GetEntryPin(id); err != nil || pin == nil || !pin.Matches(key) {
		t.Fatalf("修改地址不应清除锚：%v %v", pin, err)
	}

	if err := s.DeleteServerCascading(id); err != nil {
		t.Fatal(err)
	}
	if pin, err := s.GetEntryPin(id); err != nil || pin != nil {
		t.Fatalf("删除条目后锚应随之删除：%v %v", pin, err)
	}
	deletes, err := s.QueryAudit(AuditFilter{Actions: []string{"pin-delete"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(deletes) != 1 {
		t.Fatalf("随条目删除应写一条锚审计，行数=%d", len(deletes))
	}
	if row := deletes[0]; row.ServerID != id || !strings.Contains(row.Command, fp) {
		t.Fatalf("随条目删除的审计应带条目与指纹：%+v", row)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}

// TestEntryPinMigration_CopiesAndDropsOrphans 覆盖升级迁移的外部行为：一行旧
// 格式「主机加端口」锚复制给所有兜底 Host 与端口都相同的条目；没有任何条目
// 指向的孤立锚被丢弃并写迁移审计；重复打开不重复复制、不重复写审计。
func TestEntryPinMigration_CopiesAndDropsOrphans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old-vault.db")
	mk, err := GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, mk)
	if err != nil {
		t.Fatal(err)
	}
	rootID := addEntryForTest(t, s, "root-box", "10.0.0.5", 22)
	deployID := addEntryForTest(t, s, "deploy-box", "10.0.0.5", 22)
	labID := addEntryForTest(t, s, "lab-box", "10.0.0.9", 2222)

	sharedKey, _ := pinHostKey(t)
	labKey, _ := pinHostKey(t)
	orphanKey, orphanFP := pinHostKey(t)
	for _, row := range []struct {
		hostPort string
		blob     []byte
		source   string
		device   string
	}{
		{"10.0.0.5:22", sharedKey, PinSourceTofu, ""},
		{"10.0.0.9:2222", labKey, PinSourceForward, "laptop"},
		{"192.0.2.1:22", orphanKey, PinSourceTofu, ""},
	} {
		if _, err := s.db.Exec(
			`INSERT INTO host_keys (host_port, key_blob, created_at, pin_format, pin_source, pin_device) VALUES (?,?,?,?,?,?)`,
			row.hostPort, row.blob, int64(1700000000), PinFormatBlob, row.source, row.device); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path, mk)
	if err != nil {
		t.Fatal(err)
	}

	pins, err := upgraded.ListEntryPins()
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 3 {
		t.Fatalf("迁移后按条目的锚数量 = %d，应为 3（孤立锚丢弃）：%+v", len(pins), pins)
	}
	byEntry := map[string]PinMeta{}
	for _, p := range pins {
		byEntry[p.ServerID] = p
	}
	for _, id := range []string{rootID, deployID} {
		p := byEntry[id]
		if p.ServerID == "" || !p.Matches(sharedKey) || p.Source != PinSourceTofu || p.Device != "" {
			t.Fatalf("同一兜底地址上的条目 %s 没有继承旧锚：%+v", id, p)
		}
		if p.HandshakeAddr != "10.0.0.5:22" {
			t.Fatalf("条目 %s 的锚学习地址 = %q，应为 10.0.0.5:22", id, p.HandshakeAddr)
		}
	}
	lab := byEntry[labID]
	if !lab.Matches(labKey) || lab.Source != PinSourceForward || lab.Device != "laptop" || lab.HandshakeAddr != "10.0.0.9:2222" {
		t.Fatalf("转发来源的旧锚没有完整复制给条目：%+v", lab)
	}

	migrations, err := upgraded.QueryAudit(AuditFilter{Actions: []string{"pin-migrate"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 1 {
		t.Fatalf("迁移审计行数 = %d，应为 1：%+v", len(migrations), migrations)
	}
	if cmd := migrations[0].Command; !containsAll(cmd, "192.0.2.1:22", orphanFP) {
		t.Fatalf("迁移审计没有列出被丢弃的地址与主机密钥指纹：%q", cmd)
	}
	if cmd := migrations[0].Command; !containsAll(cmd, rootID, deployID) {
		t.Fatalf("迁移审计应带被复制的条目标识符：%q", cmd)
	}

	// 重复打开：幂等，不重复复制、不重复写审计。
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(path, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	pins2, err := again.ListEntryPins()
	if err != nil {
		t.Fatal(err)
	}
	if len(pins2) != 3 {
		t.Fatalf("重复打开后锚数量 = %d，应为 3", len(pins2))
	}
	migrations2, err := again.QueryAudit(AuditFilter{Actions: []string{"pin-migrate"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations2) != 1 {
		t.Fatalf("重复打开后迁移审计行数 = %d，应为 1", len(migrations2))
	}
}
