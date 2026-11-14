package sshbroker

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// openHostKeyAlgorithmStore 打开一个真实存储，用生产锚写回路径验证算法学习。
func openHostKeyAlgorithmStore(t *testing.T) *store.Store {
	t.Helper()
	masterKey, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "store.db"), masterKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// multiHostKeyFixture 启动同时持有 ECDSA 与 Ed25519 主机密钥的测试服务器。
func multiHostKeyFixture(t *testing.T) (string, ssh.PublicKey, ssh.PublicKey, *atomic.Int64, func()) {
	t.Helper()
	_, ed25519Private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ed25519Signer, err := ssh.NewSignerFromKey(ed25519Private)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaPrivate, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaSigner, err := ssh.NewSignerFromKey(ecdsaPrivate)
	if err != nil {
		t.Fatal(err)
	}
	var authRequests atomic.Int64
	addr, firstKey, cleanup := testsshd.Start(t, testsshd.Options{
		HostKeys: []ssh.Signer{ed25519Signer, ecdsaSigner}, Password: "pw",
		AuthRequests: &authRequests,
	})
	return addr, ed25519Signer.PublicKey(), firstKey, &authRequests, cleanup
}

// TestConnectServerEntryFingerprintPinLearnsEd25519Algorithm 覆盖带外指纹锚
// 的关键场景：默认握手先呈现 ECDSA 时，必须限定算法族重试，只有指纹完全
// 一致才接受，并把实际算法写回锚。
func TestConnectServerEntryFingerprintPinLearnsEd25519Algorithm(t *testing.T) {
	addr, ed25519Key, _, authRequests, cleanup := multiHostKeyFixture(t)
	defer cleanup()
	st := openHostKeyAlgorithmStore(t)
	srv := &models.Server{
		Name: "target", Host: hostOf(addr), Port: portOf(addr), User: "u",
		AuthMethod: models.AuthPassword,
	}
	var err error
	srv.ID, err = st.AddServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := ssh.FingerprintSHA256(ed25519Key)
	if _, err := st.UpsertManualEntryPin(
		srv, "", []byte(fingerprint), store.PinFormatFingerprint, false,
		func(*store.PinMeta) store.AuditRow { return store.AuditRow{Action: "pin-manual"} },
	); err != nil {
		t.Fatal(err)
	}

	client, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st,
	})
	if err != nil {
		t.Fatalf("带外 Ed25519 指纹锚应能通过算法重试连接：%v", err)
	}
	defer client.Close()
	if got := authRequests.Load(); got != 1 {
		t.Fatalf("只有指纹一致的握手才应发送登录凭据，认证请求数 = %d", got)
	}
	pin, err := st.GetEntryPinMeta(srv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pin.KeyAlgorithm != ssh.KeyAlgoED25519 {
		t.Fatalf("连接后应写回实际主机密钥算法，得到 %q", pin.KeyAlgorithm)
	}

	original := lowLevelDial
	var secondAlgorithms []string
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, cb ssh.HostKeyCallback, ka keepAliveSpec, algos []string) (*Client, error) {
		secondAlgorithms = append(secondAlgorithms, algos...)
		return connectWith(ctx, host, port, user, auth, cb, ka, algos)
	}
	t.Cleanup(func() { lowLevelDial = original })
	client2, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st,
	})
	if err != nil {
		t.Fatalf("写回算法后的连接应直接限定 Ed25519：%v", err)
	}
	defer client2.Close()
	if fmt.Sprint(secondAlgorithms) != fmt.Sprint([]string{ssh.KeyAlgoED25519}) {
		t.Fatalf("有算法的锚应只请求 Ed25519，得到 %v", secondAlgorithms)
	}
}

// TestHostKeyAlgorithmsForPinMapsFamilies 钉住算法族到 SSH 主机密钥算法列
// 表的映射；证书算法不在本次范围内。
func TestHostKeyAlgorithmsForPinMapsFamilies(t *testing.T) {
	cases := []struct {
		name string
		pin  store.Pin
		want []string
	}{
		{
			name: "ed25519",
			pin:  store.Pin{KeyAlgorithm: ssh.KeyAlgoED25519},
			want: []string{ssh.KeyAlgoED25519},
		},
		{
			name: "ecdsa family",
			pin:  store.Pin{KeyAlgorithm: "ecdsa"},
			want: []string{ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521},
		},
		{
			name: "exact ecdsa curve",
			pin:  store.Pin{KeyAlgorithm: ssh.KeyAlgoECDSA384},
			want: []string{ssh.KeyAlgoECDSA384},
		},
		{
			name: "rsa family",
			pin:  store.Pin{KeyAlgorithm: "rsa"},
			want: []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA},
		},
	}
	for _, tc := range cases {
		got := hostKeyAlgorithmsForPin(&tc.pin)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("%s: hostKeyAlgorithmsForPin = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestConnectServerEntryFingerprintMismatchRetriesWithoutCredentials 确认没有
// 算法的指纹锚会按算法族重试，但所有指纹都不同时不发送登录凭据。
func TestConnectServerEntryFingerprintMismatchRetriesWithoutCredentials(t *testing.T) {
	addr, _, _, authRequests, cleanup := multiHostKeyFixture(t)
	defer cleanup()
	wrongKey := testPublicKey(t)
	st := openHostKeyAlgorithmStore(t)
	var err error
	srv := &models.Server{
		Name: "target", Host: hostOf(addr), Port: portOf(addr), User: "u",
		AuthMethod: models.AuthPassword,
	}
	srv.ID, err = st.AddServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	wrongPin := ssh.FingerprintSHA256(wrongKey)
	if _, err := st.UpsertManualEntryPin(
		srv, "", []byte(wrongPin), store.PinFormatFingerprint, false,
		func(*store.PinMeta) store.AuditRow { return store.AuditRow{Action: "pin-manual"} },
	); err != nil {
		t.Fatal(err)
	}

	_, _, err = ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st,
	})
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("全部算法族指纹不同时仍应拒绝连接，得到：%v", err)
	}
	if got := authRequests.Load(); got != 0 {
		t.Fatalf("主机密钥全部不一致时不能发送登录凭据，认证请求数 = %d", got)
	}
}

// TestConnectServerEntryEnvironmentOverridesPinAlgorithm 确认环境变量显式
// 指定主机密钥算法时不做锚算法探测，用户指定的列表原样交给底层连接。
func TestConnectServerEntryEnvironmentOverridesPinAlgorithm(t *testing.T) {
	t.Setenv("SSHMGR_SSH_HOST_KEY_ALGORITHMS", ssh.KeyAlgoECDSA256)
	addr, ed25519Key, _, authRequests, cleanup := multiHostKeyFixture(t)
	defer cleanup()
	st := openHostKeyAlgorithmStore(t)
	srv := &models.Server{
		Name: "target", Host: hostOf(addr), Port: portOf(addr), User: "u",
		AuthMethod: models.AuthPassword,
	}
	var err error
	srv.ID, err = st.AddServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := ssh.FingerprintSHA256(ed25519Key)
	if _, err := st.UpsertManualEntryPin(
		srv, "", []byte(fingerprint), store.PinFormatFingerprint, false,
		func(*store.PinMeta) store.AuditRow { return store.AuditRow{Action: "pin-manual"} },
	); err != nil {
		t.Fatal(err)
	}

	original := lowLevelDial
	var offered [][]string
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, cb ssh.HostKeyCallback, ka keepAliveSpec, algos []string) (*Client, error) {
		offered = append(offered, append([]string(nil), algos...))
		return connectWith(ctx, host, port, user, auth, cb, ka, algos)
	}
	t.Cleanup(func() { lowLevelDial = original })
	_, _, err = ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st,
	})
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("环境变量强制 ECDSA 时应保持既有语义并拒绝不同指纹锚：%v", err)
	}
	if len(offered) != 1 || len(offered[0]) != 0 {
		t.Fatalf("环境变量设置时不传入锚算法探测列表，且只允许一次连接，实际调用=%v", offered)
	}
	if got := authRequests.Load(); got != 0 {
		t.Fatalf("指纹不同时不能发送登录凭据，认证请求数 = %d", got)
	}
}
