package sshbroker

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestRollingBufferCursorBranches(t *testing.T) {
	b := NewRollingBuffer(8)
	b.Write([]byte("0123456789ABCD")) // total=14, 保留尾 8 字节 "789ABCD" 之外——ring= "7ABCD..."? 断言按三分支:
	// 正常分支: since=10 → chunk="ABCD", next=14
	c, next, start := b.Snapshot(10)
	if string(c) != "ABCD" || next != 14 || start != 6 {
		t.Fatalf("normal: chunk=%q next=%d start=%d", c, next, start)
	}
	// 超前分支: since=99 → 空 chunk + next 回拉 total
	c, next, _ = b.Snapshot(99)
	if len(c) != 0 || next != 14 {
		t.Fatalf("ahead: chunk=%q next=%d", c, next)
	}
	// gap 分支: since=0 (< start=6) → 整窗 + next=total
	c, next, start = b.Snapshot(0)
	if string(c) != "6789ABCD" || next != 14 || start != 6 {
		t.Fatalf("gap: chunk=%q next=%d start=%d", c, next, start)
	}
}

func TestRollingBufferCapZeroRetainsNothing(t *testing.T) {
	b := NewRollingBuffer(0)
	b.Write([]byte("xyz"))
	if _, next, start := b.Snapshot(0); next != 3 || start != 3 {
		t.Fatalf("cap=0: next=%d start=%d (want 3/3)", next, start)
	}
}

func TestRollingBufferSnapshotIsDeepCopy(t *testing.T) {
	b := NewRollingBuffer(4)
	b.Write([]byte("AAAA"))
	c, _, _ := b.Snapshot(0)
	b.Write([]byte("BBBB")) // 丢头滚动——若 Snapshot 返回内部视图, c 已被腐蚀
	if string(c) != "AAAA" {
		t.Fatalf("corroded: %q", c)
	}
	if s, _, _ := b.Snapshot(2); string(s) != "BBBB" {
		t.Fatalf("after roll: %q", s)
	}
}

func TestRollingBufferAllocationBounded(t *testing.T) {
	b := NewRollingBuffer(1024)
	for i := 0; i < 1000; i++ {
		b.Write(bytes.Repeat([]byte("x"), 64)) // 反复写超量
	}
	if cap(b.buf) != 1024 { // 分配恒等于 cap（固定容量承诺, 白盒）
		t.Fatalf("cap grew: %d", cap(b.buf))
	}
}

func TestRollingBufferConcurrentReadWrite(t *testing.T) {
	b := NewRollingBuffer(256)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			b.Write([]byte(strings.Repeat("a", 7)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			b.Snapshot(int64(i * 3))
		}
	}()
	wg.Wait()
	if b.Total() != 2000*7 {
		t.Fatalf("total=%d", b.Total())
	}
}
