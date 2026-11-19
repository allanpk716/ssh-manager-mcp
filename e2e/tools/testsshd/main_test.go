package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStandardInputCloseStopsServer 验证 #318 的自清理路径：标准输入管道
// 关闭（父进程退出或崩溃）时服务自行关监听退出。子进程用测试二进制本身
// 扮演，靠 TESTSSHD_CHILD 环境变量区分，进入真实 run()。
func TestStandardInputCloseStopsServer(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "test-key.pem")
	child := exec.Command(os.Args[0], "-test.run=TestStandardInputCloseChildProcess")
	child.Env = append(os.Environ(), "TESTSSHD_CHILD=1", "TESTSSHD_KEY_PATH="+keyPath)
	standardInput, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer standardInput.Close()
	standardOutput, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() {
		exited <- child.Wait()
	}()
	address := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(standardOutput).ReadString('\n')
		address <- strings.TrimSpace(line)
	}()
	select {
	case line := <-address:
		if !strings.HasPrefix(line, "127.0.0.1:") {
			t.Fatalf("子进程应先打印监听地址，实际输出首行：%q", line)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("等待子进程打印监听地址超时")
	}
	if err := standardInput.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("标准输入关闭后子进程应以退出码 0 退出，实际：%v", err)
		}
	case <-time.After(10 * time.Second):
		_ = child.Process.Kill()
		t.Fatal("标准输入关闭后子进程 10 秒内没有自行退出")
	}
}

// TestStandardInputCloseChildProcess 只在子进程模式运行：调用真实 run()，
// 阻塞在 Accept 循环里，直到父测试进程关闭标准输入管道。
func TestStandardInputCloseChildProcess(t *testing.T) {
	if os.Getenv("TESTSSHD_CHILD") != "1" {
		t.Skip("只作为 TestStandardInputCloseStopsServer 的子进程运行")
	}
	if err := run(os.Getenv("TESTSSHD_KEY_PATH")); err != nil {
		t.Fatal(err)
	}
}
