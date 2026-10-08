package hotswap_test

// 换手核心库(票 03)的共享测试工具。测试只走库的外部公共面
// (规格 Testing Decisions:只测外部行为,不测内部函数)。

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

// testPayload 生成确定性非均匀字节序列:避开全零/全 0xFF 这类
// 掩盖错位与丢位的取值,内容比对才可信。
func testPayload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i%251) ^ byte(i/3%7)
	}
	return b
}

// writeChunks 把 payload 分块写入 f(7KiB 块,非 2 的幂,便于暴露差一错)。
// 与读取方并发时靠管道背压自然节流。允许在非测试协程调用(只用 Error)。
func writeChunks(t *testing.T, f *os.File, payload []byte) {
	const chunk = 7 * 1024
	for off := 0; off < len(payload); off += chunk {
		end := min(off+chunk, len(payload))
		if _, err := f.Write(payload[off:end]); err != nil {
			t.Errorf("write chunk at offset %d: %v", off, err)
			return
		}
	}
}

// withDeadline 在 d 内等待 fn 完成;超时即判测试失败——坏路径上的
// 管道读写可能永远阻塞,不能裸调后干等 go test 的 10 分钟包超时。
func withDeadline(t *testing.T, what string, d time.Duration, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(d):
		t.Fatalf("%s: timed out after %v", what, d)
	}
}

// readExact 从 f 读出恰好 n 字节(带超时),供内容比对。
func readExact(t *testing.T, f *os.File, n int, d time.Duration) []byte {
	t.Helper()
	got := make([]byte, n)
	withDeadline(t, "read from pipe", d, func() error {
		_, err := io.ReadFull(f, got)
		return err
	})
	return got
}

// assertBytesEqual 比对两段字节并报出首个差异位置。
func assertBytesEqual(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if bytes.Equal(got, want) {
		return
	}
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	t.Fatalf("%s mismatch: len got=%d want=%d, first differing offset %d (got %#x want %#x)",
		what, len(got), len(want), i, got[i], want[i])
}

// mustPipe 建一对 os.Pipe,按(读端, 写端)顺序返回并以参数名显表意,
// 避免接线搞反(os.Pipe 本身就是这个返回顺序)。
func mustPipe(t *testing.T) (read, write *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return r, w
}

// waitForFile 轮询等待文件出现(伪继任的观测标记)。
func waitForFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("file %q did not appear within %v", path, d)
}
