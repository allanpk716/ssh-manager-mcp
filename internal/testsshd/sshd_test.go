package testsshd

import (
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
)

// fixedKeySigner 生成一把用于固定主机密钥测试的 Ed25519 签名密钥。
func fixedKeySigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return signer
}

// TestStartFixedHostKeyAndAuthRequestCounter: 夹具可指定固定主机密钥（返回值
// 就是那把密钥），并可记录是否收到过用户认证请求——主机密钥不一致的客户端
// 在密钥交换阶段即被拒绝，计数必须保持为零；认证成功至少计一次。
func TestStartFixedHostKeyAndAuthRequestCounter(t *testing.T) {
	fixed := fixedKeySigner(t)
	other := fixedKeySigner(t)
	var authRequests atomic.Int64
	addr, hostKey, cleanup := Start(t, Options{
		HostKey:      fixed,
		Password:     "pw",
		AuthRequests: &authRequests,
	})
	defer cleanup()

	if !bytesEqual(hostKey.Marshal(), fixed.PublicKey().Marshal()) {
		t.Fatal("Start must present the caller-specified fixed host key")
	}

	badCfg := &ssh.ClientConfig{
		User:            "u",
		Auth:            []ssh.AuthMethod{ssh.Password("pw")},
		HostKeyCallback: ssh.FixedHostKey(other.PublicKey()),
	}
	if _, err := ssh.Dial("tcp", addr, badCfg); err == nil {
		t.Fatal("client presenting a different host key must be rejected")
	}
	if n := authRequests.Load(); n != 0 {
		t.Fatalf("host-key mismatch must reach zero auth requests, got %d", n)
	}

	goodCfg := &ssh.ClientConfig{
		User:            "u",
		Auth:            []ssh.AuthMethod{ssh.Password("pw")},
		HostKeyCallback: ssh.FixedHostKey(fixed.PublicKey()),
	}
	cli, err := ssh.Dial("tcp", addr, goodCfg)
	if err != nil {
		t.Fatalf("dial with the fixed host key: %v", err)
	}
	defer cli.Close()
	if n := authRequests.Load(); n == 0 {
		t.Fatal("a successful password authentication must be recorded")
	}
}

func TestStartExecutesCommand(t *testing.T) {
	addr, hostKey, cleanup := Start(t, Options{
		Password: "pw",
		Exec: func(cmd string, _ io.Reader) (string, string, int) {
			if cmd == "echo hi" {
				return "hi\n", "", 0
			}
			return "", "unknown cmd", 1
		},
	})
	defer cleanup()

	cfg := &ssh.ClientConfig{
		User:            "u",
		Auth:            []ssh.AuthMethod{ssh.Password("pw")},
		HostKeyCallback: ssh.FixedHostKey(hostKey),
	}
	cli, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	sess, err := cli.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	out, err := sess.CombinedOutput("echo hi")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("got %q", out)
	}
}
