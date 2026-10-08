package hotswap_test

import (
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
)

// 伪继任/伪桥角色开关:测试二进制自我再执行。TestMain 拦截 HOTSWAP_TEST_ROLE
// 进入角色分支而非跑测试(先例:internal/cli/update_test.go 的 TestMain 拦截、
// internal/updater/replace_test.go 的 helper 进程)。
const (
	testRoleEnv      = "HOTSWAP_TEST_ROLE"       // 角色名
	testNextReadyEnv = "HOTSWAP_TEST_NEXT_READY" // gen2bridge:三代就绪文件路径
	testEOFMarkerEnv = "HOTSWAP_TEST_EOF_MARKER" // echo:标准输入 EOF 后写的标记文件
	testStopFileEnv  = "HOTSWAP_TEST_STOP_FILE"  // 挂住型角色:此文件出现即退出
)

func TestMain(m *testing.M) {
	role := os.Getenv(testRoleEnv)
	if role == "" {
		os.Exit(m.Run())
	}
	os.Exit(runChildRole(role))
}

// runChildRole 分发伪继任/伪桥角色;全部以本测试二进制的再执行形态运行。
func runChildRole(role string) int {
	_, readyPath, _ := hotswap.ParseEnv(os.Environ())
	switch role {
	case "echo":
		return roleEcho(readyPath)
	case "stuck":
		return roleStuck()
	case "diefast":
		return roleDieFast()
	case "stdinclose":
		return roleStdinClose(readyPath)
	case "gen2bridge":
		return roleGen2Bridge()
	default:
		fmt.Fprintf(os.Stderr, "hotswap test helper: unknown role %q\n", role)
		return 2
	}
}

// roleEcho:健康继任——模拟初始化耗时、写就绪文件(含版本),然后标准
// 输入输出全双工回声;标准输入 EOF 后写观测标记(若要求)并干净退出。
func roleEcho(readyPath string) int {
	time.Sleep(30 * time.Millisecond)
	if err := hotswap.WriteReady(readyPath, "v-test-echo"); err != nil {
		return 3
	}
	if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
		return 4
	}
	if marker := os.Getenv(testEOFMarkerEnv); marker != "" {
		os.WriteFile(marker, []byte("eof"), 0o600)
	}
	return 0
}

// roleStuck:不写就绪文件,挂住直到停止文件出现(测就绪超时回退)。
func roleStuck() int {
	waitForStopFile(60 * time.Second)
	return 0
}

// roleDieFast:不写就绪文件,50ms 后以退出码 7 退出(测继任提前退出回退)。
func roleDieFast() int {
	time.Sleep(50 * time.Millisecond)
	return 7
}

// roleStdinClose:先关自己的标准输入、再写就绪文件——父侧泵化后往泵侧
// 管道写必失败(测泵侧写失败收场)。就绪先于父侧发字节,时序确定。
func roleStdinClose(readyPath string) int {
	time.Sleep(30 * time.Millisecond)
	if err := os.Stdin.Close(); err != nil {
		return 3
	}
	if err := hotswap.WriteReady(readyPath, "v-test-stdinclose"); err != nil {
		return 3
	}
	waitForStopFile(60 * time.Second)
	return 0
}

// roleGen2Bridge:后续代桥——写自己的就绪文件(它本身也是被领养拉起的),
// 再以 GenerationLater 换手给三代(echo 角色,直接继承本进程的标准输入
// 输出),退位前应答钩子往标准输出写一行标记,随后本进程退出——泵对
// 端点换人应当无感知。
func roleGen2Bridge() int {
	_, ownReady, _ := hotswap.ParseEnv(os.Environ())
	if err := hotswap.WriteReady(ownReady, "v-test-gen2"); err != nil {
		return 3
	}
	err := hotswap.Handover(hotswap.Options{
		Exe:        os.Args[0],
		Session:    hotswap.Session{"mcp_protocol": "2025-06-18"},
		Generation: hotswap.GenerationLater,
		Env: []string{
			testRoleEnv + "=echo",
			testEOFMarkerEnv + "=" + os.Getenv(testEOFMarkerEnv),
		},
		ReadyPath:    os.Getenv(testNextReadyEnv),
		ReadyTimeout: 5 * time.Second,
		PollInterval: 20 * time.Millisecond,
		BeforeRetire: func() error {
			_, werr := os.Stdout.WriteString("GEN2HOOK\n")
			return werr
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen2bridge handover: %v\n", err)
		return 5
	}
	return 0
}

func waitForStopFile(max time.Duration) {
	stop := os.Getenv(testStopFileEnv)
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if stop != "" {
			if _, err := os.Stat(stop); err == nil {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}
