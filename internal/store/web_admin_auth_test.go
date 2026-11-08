package store

import (
	"strings"
	"testing"
	"time"
)

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
