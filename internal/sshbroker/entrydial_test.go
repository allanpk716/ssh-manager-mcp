package sshbroker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// fixedSigner 生成一把可交给测试夹具固定呈现的主机密钥签名密钥。
func fixedSigner(t *testing.T) ssh.Signer {
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

// mapFallbackToFixture 把统一入口交给低层函数的地址映射到进程内 SSH
// 服务器，并核对低层函数收到的正是服务器条目的兜底主机与端口。
func mapFallbackToFixture(t *testing.T, srv *models.Server, addr string) {
	t.Helper()
	original := lowLevelDial
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec) (*Client, error) {
		if host != srv.Host || port != srv.Port {
			t.Errorf("low-level dial must receive the entry fallback address %s:%d, got %s:%d", srv.Host, srv.Port, host, port)
		}
		return connectWith(ctx, hostOf(addr), portOf(addr), user, auth, hostKeyCb, ka)
	}
	t.Cleanup(func() { lowLevelDial = original })
}

// TestConnectServerEntryDefaultDialRecordsThenVerifiesHostKey: 统一入口默认
// 注入现有的按「主机:端口」拨号的底层函数——不替换任何注入点时，用条目的
// 兜底地址直连进程内 SSH 服务器：首次连接按服务器条目记录主机密钥，第二次
// 连接校验同一把锚并成功。本票的连接说明恒为空。
func TestConnectServerEntryDefaultDialRecordsThenVerifiesHostKey(t *testing.T) {
	st := &fakeHostKeyStore{}
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	srv := entryAt("srv-a", hostOf(addr), portOf(addr))

	req := ServerDialRequest{Server: srv, Auth: PasswordAuth("pw"), HostKeys: st}
	cli, notes, err := ConnectServerEntry(context.Background(), req)
	if err != nil {
		t.Fatalf("first connect: %v", err)
	}
	defer cli.Close()
	if len(notes) != 0 {
		t.Fatalf("connection notes must be empty in this ticket, got %q", notes)
	}
	if st.saves != 1 {
		t.Fatalf("first connect must record one host key, got %d saves", st.saves)
	}

	cli2, _, err := ConnectServerEntry(context.Background(), req)
	if err != nil {
		t.Fatalf("second connect (verify recorded key): %v", err)
	}
	defer cli2.Close()
	if st.saves != 1 {
		t.Fatalf("second connect must verify without re-saving, got %d saves", st.saves)
	}
}

// TestConnectServerEntryInjectedDialUsesFallbackAddress: 注入低层连接函数，
// 把条目的兜底地址映射到进程内 SSH 服务器——经映射连上后，首次连接按条目
// 记录主机密钥，第二次连接校验同一把锚并成功。
func TestConnectServerEntryInjectedDialUsesFallbackAddress(t *testing.T) {
	st := &fakeHostKeyStore{}
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	srv := entryAt("srv-a", "192.0.2.10", 2202)
	mapFallbackToFixture(t, srv, addr)

	req := ServerDialRequest{Server: srv, Auth: PasswordAuth("pw"), HostKeys: st}
	cli, _, err := ConnectServerEntry(context.Background(), req)
	if err != nil {
		t.Fatalf("first connect via injected dial: %v", err)
	}
	defer cli.Close()
	if st.saves != 1 {
		t.Fatalf("first connect must record one host key, got %d saves", st.saves)
	}

	cli2, _, err := ConnectServerEntry(context.Background(), req)
	if err != nil {
		t.Fatalf("second connect via injected dial: %v", err)
	}
	defer cli2.Close()
	if st.saves != 1 {
		t.Fatalf("second connect must verify without re-saving, got %d saves", st.saves)
	}
}

// TestConnectServerEntryMismatchFailsWithoutSendingCredentials: 条目锚与
// 夹具固定主机密钥不一致时，统一入口必须失败；主机密钥校验发生在发送登录
// 凭据之前，因此服务器收不到任何用户认证请求，锚也不被覆盖。
func TestConnectServerEntryMismatchFailsWithoutSendingCredentials(t *testing.T) {
	fixed := fixedSigner(t)
	var authRequests atomic.Int64
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{
		HostKey:      fixed,
		Password:     "pw",
		AuthRequests: &authRequests,
	})
	defer cleanup()
	pinned := testPublicKey(t) // 与夹具固定密钥必然不同的另一把公钥
	st := &fakeHostKeyStore{pins: map[string]*store.Pin{
		"srv-a": {Blob: pinned.Marshal(), Format: store.PinFormatBlob},
	}}
	srv := entryAt("srv-a", "192.0.2.11", 2203)
	mapFallbackToFixture(t, srv, addr)

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st,
	})
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("mismatched host key must fail with ErrHostKeyMismatch, got %v", err)
	}
	if n := authRequests.Load(); n != 0 {
		t.Fatalf("server must not receive any user authentication request, got %d", n)
	}
	if st.saves != 0 {
		t.Fatalf("a rejected connect must not overwrite the pinned key, got %d saves", st.saves)
	}
}
