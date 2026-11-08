package store

import (
	"strings"
	"testing"
	"time"
)

func failWebAdminAuditInsert(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`CREATE TRIGGER fail_web_admin_audit
BEFORE INSERT ON audit_log
BEGIN
	SELECT RAISE(ABORT, 'audit failed');
END`); err != nil {
		t.Fatal(err)
	}
}

func TestWebAdminPasswordSetAndAuditCommitTogether(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	if _, err := s.SetWebAdminPassword([]byte("old-password")); err != nil {
		t.Fatal(err)
	}
	failWebAdminAuditInsert(t, s)

	_, err := s.SetWebAdminPasswordWithAudit([]byte("new-password"), AuditRow{
		Action: "admin.password_set", Status: "ok", Source: AuditSourceCLI,
	})
	if err == nil || !strings.Contains(err.Error(), "audit failed") {
		t.Fatalf("审计写入失败应让口令设置失败：%v", err)
	}
	ok, err := s.VerifyWebAdminPassword([]byte("old-password"))
	if err != nil || !ok {
		t.Fatalf("事务回滚后旧口令应仍有效：ok=%v err=%v", ok, err)
	}
	generation, err := s.WebAdminSessionGeneration()
	if err != nil || generation != 1 {
		t.Fatalf("事务回滚后会话代 = %d，应为 1，错误 = %v", generation, err)
	}
}

func TestWebAdminLogoutAllAndAuditCommitTogether(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	if _, err := s.SetWebAdminPassword([]byte("old-password")); err != nil {
		t.Fatal(err)
	}
	failWebAdminAuditInsert(t, s)

	_, err := s.LogoutAllWebAdminSessionsWithAudit(AuditRow{
		Action: "admin.logout_all", Status: "ok", Source: AuditSourceWebUI, SourceIP: "192.0.2.10",
	})
	if err == nil || !strings.Contains(err.Error(), "audit failed") {
		t.Fatalf("审计写入失败应让注销全部会话失败：%v", err)
	}
	generation, err := s.WebAdminSessionGeneration()
	if err != nil || generation != 1 {
		t.Fatalf("事务回滚后会话代 = %d，应为 1，错误 = %v", generation, err)
	}
}

func TestWebAdminUnlockAndAuditCommitTogether(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()
	base := time.Unix(1800000000, 0)
	ip := "192.0.2.10"
	for i := 0; i < 5; i++ {
		if _, err := s.RecordWebAdminLoginFailure(ip, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	failWebAdminAuditInsert(t, s)

	err := s.ClearWebAdminLoginFailuresWithAudit(ip, AuditRow{
		Action: "admin.unlock", Status: "ok", Source: AuditSourceCLI,
	})
	if err == nil || !strings.Contains(err.Error(), "audit failed") {
		t.Fatalf("审计写入失败应让解除锁定失败：%v", err)
	}
	state, err := s.WebAdminLoginState(ip, base.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !state.Locked {
		t.Fatalf("事务回滚后锁定应仍生效：%+v", state)
	}
}

func TestVerifyWebAdminPasswordHashErrorOmitsHash(t *testing.T) {
	encoded := "$argon2id$unsupported$not$hash"
	_, err := verifyWebAdminPasswordHash([]byte("password"), encoded)
	if err == nil {
		t.Fatal("格式不支持的哈希应返回错误")
	}
	if strings.Contains(err.Error(), encoded) || strings.Contains(err.Error(), "not$hash") {
		t.Fatalf("错误信息不应包含口令哈希：%v", err)
	}
}

func TestWebAdminPasswordUsesArgon2idAndMovesSessionGeneration(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	set, err := s.WebAdminPasswordIsSet()
	if err != nil {
		t.Fatal(err)
	}
	if set {
		t.Fatal("新 vault 不应已经设置管理员口令")
	}
	generation, err := s.SetWebAdminPassword([]byte("first-password"))
	if err != nil {
		t.Fatal(err)
	}
	if generation != 1 {
		t.Fatalf("第一次设置口令后的会话代 = %d，应为 1", generation)
	}
	ok, err := s.VerifyWebAdminPassword([]byte("first-password"))
	if err != nil || !ok {
		t.Fatalf("正确口令应校验通过：ok=%v err=%v", ok, err)
	}
	ok, err = s.VerifyWebAdminPassword([]byte("wrong-password"))
	if err != nil || ok {
		t.Fatalf("错误口令不应校验通过：ok=%v err=%v", ok, err)
	}
	hash, err := s.WebAdminPasswordHash()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("管理员口令哈希应是自描述 argon2id 格式：%q", hash)
	}
	if strings.Contains(hash, "first-password") {
		t.Fatal("管理员口令哈希不应包含明文口令")
	}

	generation, err = s.SetWebAdminPassword([]byte("second-password"))
	if err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("重置口令后的会话代 = %d，应为 2", generation)
	}
	generation, err = s.LogoutAllWebAdminSessions()
	if err != nil {
		t.Fatal(err)
	}
	if generation != 3 {
		t.Fatalf("注销全部会话后的会话代 = %d，应为 3", generation)
	}
	ok, err = s.VerifyWebAdminPassword([]byte("second-password"))
	if err != nil || !ok {
		t.Fatalf("重置后的口令应校验通过：ok=%v err=%v", ok, err)
	}
}

func TestWebAdminLoginFailuresPersistLockAndGlobalDelay(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	base := time.Unix(1800000000, 0)
	ip := "192.0.2.10"
	for i := time.Duration(1); i <= 4; i++ {
		state, err := s.RecordWebAdminLoginFailure(ip, base.Add(i*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if state.Locked {
			t.Fatalf("第 %d 次失败不应锁定", i)
		}
		if want := i * time.Second; state.Delay != want {
			t.Fatalf("第 %d 次失败后的全局延迟 = %v，应为 %v", i, state.Delay, want)
		}
	}
	state, err := s.RecordWebAdminLoginFailure(ip, base.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !state.Locked {
		t.Fatal("15 分钟内第 5 次失败应锁定来源 IP")
	}
	if want := 15 * time.Minute; state.LockRemaining != want {
		t.Fatalf("锁定剩余时间 = %v，应为 %v", state.LockRemaining, want)
	}

	state, err = s.WebAdminLoginState(ip, base.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !state.Locked || state.Delay != 5*time.Second {
		t.Fatalf("锁定状态应持久化：state=%+v", state)
	}
	other, err := s.WebAdminLoginState("192.0.2.11", base.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if other.Delay != 5*time.Second {
		t.Fatalf("失败应产生全局延迟，其他 IP 延迟 = %v，应为 5 秒", other.Delay)
	}

	if err := s.ClearWebAdminLoginFailures(ip); err != nil {
		t.Fatal(err)
	}
	state, err = s.WebAdminLoginState(ip, base.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if state.Locked {
		t.Fatalf("解除锁定后不应仍处于锁定状态：state=%+v", state)
	}
	if state.Delay != 5*time.Second {
		t.Fatalf("解除锁定只清除该 IP 锁定，15 分钟内失败仍应全局延迟：state=%+v", state)
	}
}

func TestListWebAdminLoginLocksShowsRemainingTime(t *testing.T) {
	s := newTestStore(t)
	defer s.Close()

	base := time.Unix(1800000000, 0)
	for i := 0; i < 5; i++ {
		if _, err := s.RecordWebAdminLoginFailure("192.0.2.10", base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	locks, err := s.ListWebAdminLoginLocks(base.Add(5 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 1 || locks[0].IP != "192.0.2.10" || locks[0].Remaining != 14*time.Minute {
		t.Fatalf("锁定列表 = %+v，应包含来源 IP 与 14 分钟剩余时间", locks)
	}
	locks, err = s.ListWebAdminLoginLocks(base.Add(21 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 0 {
		t.Fatalf("过期锁定不应继续显示：%+v", locks)
	}
}
