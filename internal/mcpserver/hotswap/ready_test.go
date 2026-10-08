package hotswap_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
)

// TestWriteReadyThenWaitReady:继任写入含版本号的就绪文件,父侧立刻等到。
func TestWriteReadyThenWaitReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready.json")
	if err := hotswap.WriteReady(path, "v0.20.0"); err != nil {
		t.Fatalf("WriteReady: %v", err)
	}
	r, err := hotswap.WaitReady(path, 2*time.Second, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if r.Version != "v0.20.0" {
		t.Fatalf("ready.Version = %q, want v0.20.0", r.Version)
	}
}

// TestWriteReadyRequiresVersion:就绪文件必须含版本(空版本即协议错误)。
func TestWriteReadyRequiresVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready.json")
	if err := hotswap.WriteReady(path, ""); err == nil {
		t.Fatal("empty version must be rejected by WriteReady")
	}
}

// TestWaitReadyTimeout:就绪文件始终不出现 → 到超时点返回 ErrReadyTimeout。
func TestWaitReadyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-there.json")
	start := time.Now()
	_, err := hotswap.WaitReady(path, 200*time.Millisecond, 20*time.Millisecond)
	if !errors.Is(err, hotswap.ErrReadyTimeout) {
		t.Fatalf("err = %v, want ErrReadyTimeout", err)
	}
	if e := time.Since(start); e < 190*time.Millisecond {
		t.Fatalf("WaitReady returned after %v, before the 200ms deadline", e)
	}
}

// TestWaitReadyPollsThroughGarbage:轮询必须穿过「半写的/非法的」中间态——
// 就绪文件先以垃圾内容出现,稍后才被写成合法内容,等待应继续而非失败。
func TestWaitReadyPollsThroughGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late.json")
	if err := os.WriteFile(path, []byte("not json yet"), 0o600); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(120 * time.Millisecond)
		if err := hotswap.WriteReady(path, "v1"); err != nil {
			t.Errorf("late WriteReady: %v", err)
		}
	}()
	r, err := hotswap.WaitReady(path, 3*time.Second, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if r.Version != "v1" {
		t.Fatalf("ready.Version = %q, want v1", r.Version)
	}
}

// TestWaitReadyTreatsEmptyVersionAsNotReady:空版本视为未就绪,继续等到超时。
func TestWaitReadyTreatsEmptyVersionAsNotReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, []byte(`{"version":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := hotswap.WaitReady(path, 150*time.Millisecond, 20*time.Millisecond)
	if !errors.Is(err, hotswap.ErrReadyTimeout) {
		t.Fatalf("err = %v, want ErrReadyTimeout (empty version is not ready)", err)
	}
}
