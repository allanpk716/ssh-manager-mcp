package sshbroker

import (
	"context"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/testsshd"

	"golang.org/x/crypto/ssh"
)

// TestHostKeyAlgoKnob covers the SSHMGR_SSH_HOST_KEY_ALGORITHMS parsing rules:
// a comma list parses verbatim; an unknown value fails-closed at validation
// (BEFORE any dial); empty/whitespace means default (nil).
func TestHostKeyAlgoKnob(t *testing.T) {
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", "rsa-sha2-512")
	if l := hostKeyAlgos(); !reflect.DeepEqual(l, []string{"rsa-sha2-512"}) {
		t.Fatalf("hostKeyAlgos() = %v, want [rsa-sha2-512]", l)
	}
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", "not-an-algo")
	if _, err := hostKeyAlgosChecked(); err == nil {
		t.Fatal("非法值必须 fail-closed")
	}
	// The error text must name the env var and the allowed list (actionable).
	_, err := hostKeyAlgosChecked()
	if err == nil || !strings.Contains(err.Error(), "SSHMGR_SSH_HOST_KEY_ALGORITHMS") || !strings.Contains(err.Error(), "ssh-rsa") {
		t.Fatalf("err = %v, want it naming the env var and the allowed list", err)
	}
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", "")
	if l := hostKeyAlgos(); l != nil {
		t.Fatalf("空=默认, got %v", l)
	}
	// Whitespace-only value also means default (TrimSpace before the empty check).
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", "   ")
	if l := hostKeyAlgos(); l != nil {
		t.Fatalf("whitespace = default, got %v", l)
	}
	// Multi-value with spaces after commas (common style) validates AND returns
	// the trimmed list — a stray " ssh-rsa" element would poison the kexinit
	// offer with an unknown algorithm name and fail the handshake confusingly.
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", "ssh-ed25519, rsa-sha2-512")
	l, err := hostKeyAlgosChecked()
	if err != nil {
		t.Fatalf("spaced list: %v", err)
	}
	if !reflect.DeepEqual(l, []string{"ssh-ed25519", "rsa-sha2-512"}) {
		t.Fatalf("hostKeyAlgosChecked() = %v, want trimmed [ssh-ed25519 rsa-sha2-512]", l)
	}
}

// TestHostKeyAlgoKnobRSAEndToEnd connects to the testsshd (which presents an
// RSA host key) with the knob set to rsa-sha2-512 — the modern RSA signature
// algorithm gossh actually offers for RSA keys — and asserts the handshake
// succeeds and the host key still verifies via FixedHostKey. This proves the
// knob value is actually applied to the ClientConfig, not merely validated.
func TestHostKeyAlgoKnobRSAEndToEnd(t *testing.T) {
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", "rsa-sha2-512")
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	cli, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), ssh.FixedHostKey(hk))
	if err != nil {
		t.Fatalf("connect with rsa-sha2-512 against RSA-host-key sshd: %v", err)
	}
	cli.Close()
}

// TestConnectKeepAliveExecRoundTrip proves the keepalive connect variant
// handshakes against testsshd and round-trips an Exec (Plan 32 T4)。判死行为
// (3 次无响应关连接) 属真连接行为, 归 conformance 层, 不在此测——本用例只钉
// 「keepalive 客户端不破坏正常会话」。
//
// 注: x/crypto (go.mod v0.41.0 起至最新版) 无 KeepAliveConfig——spec §1 该名
// 已被证伪。ConnectKeepAlive 按 OpenSSH 同型机制实现: 周期发
// keepalive@openssh.com 全局请求 (wantReply), 连续 3 次无响应判死关连接。
func TestConnectKeepAliveExecRoundTrip(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(cmd string, _ io.Reader) (string, string, int) { return "ka:" + cmd + "\n", "", 0 },
	})
	defer cleanup()
	cli, err := ConnectKeepAlive(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), ssh.FixedHostKey(hk))
	if err != nil {
		t.Fatalf("ConnectKeepAlive: %v", err)
	}
	defer cli.Close()
	res, err := cli.Exec(context.Background(), "ping", 0, 0)
	if err != nil {
		t.Fatalf("exec over keepalive client: %v", err)
	}
	if res.Stdout != "ka:ping\n" {
		t.Fatalf("stdout=%q, want %q", res.Stdout, "ka:ping\n")
	}
}

// TestHostKeyAlgoKnobTypoFailsBeforeDial proves a typo'd knob fails-closed
// BEFORE the dial: the target listener accepts but never sends the SSH banner,
// so a late validation would block in ssh.Dial. The error must name the env
// var and arrive immediately — the ctx deadline is only a safety net, and
// hitting it would mean validation ran after the dial started.
func TestHostKeyAlgoKnobTypoFailsBeforeDial(t *testing.T) {
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", "not-an-algo")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			_ = conn // intentionally do NOT send the SSH banner — hold any dial open
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	_, err = Connect(ctx, hostOf(ln.Addr().String()), portOf(ln.Addr().String()), "u", PasswordAuth("pw"), ssh.InsecureIgnoreHostKey())
	if err == nil {
		t.Fatal("typo'd knob must fail-closed")
	}
	if !strings.Contains(err.Error(), "SSHMGR_SSH_HOST_KEY_ALGORITHMS") {
		t.Fatalf("err = %v, want error naming SSHMGR_SSH_HOST_KEY_ALGORITHMS", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("validation took %v, want immediate (fail-closed BEFORE dial)", elapsed)
	}
}
