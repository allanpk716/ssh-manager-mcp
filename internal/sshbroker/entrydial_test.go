package sshbroker

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
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
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec, _ []string) (*Client, error) {
		if host != srv.Host || port != srv.Port {
			t.Errorf("low-level dial must receive the entry fallback address %s:%d, got %s:%d", srv.Host, srv.Port, host, port)
		}
		return connectWith(ctx, hostOf(addr), portOf(addr), user, auth, hostKeyCb, ka, nil)
	}
	t.Cleanup(func() { lowLevelDial = original })
}

// TestConnectServerEntryDefaultDialRecordsThenVerifiesHostKey: 统一入口默认
// 注入现有的按「主机:端口」拨号的底层函数——不替换任何注入点时，用条目的
// 兜底地址直连进程内 SSH 服务器：首次连接按服务器条目记录主机密钥，第二次
// 连接校验同一把锚并成功。没有地址跳过或成功地址缓存警告时，连接说明为空。
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
	want := "全部候选地址连接失败：\n兜底地址：主机密钥不同，已跳过"
	if err.Error() != want {
		t.Fatalf("mismatch error text:\n got %q\nwant %q", err.Error(), want)
	}
	if n := authRequests.Load(); n != 0 {
		t.Fatalf("server must not receive any user authentication request, got %d", n)
	}
	if st.saves != 0 {
		t.Fatalf("a rejected connect must not overwrite the pinned key, got %d saves", st.saves)
	}
}

// TestConnectServerEntrySkipsMismatchedHostKeyThenConnectsNextAddress: 第一
// 个候选地址呈现的锚外主机密钥只让这个地址失败；统一入口继续尝试兜底地址，
// 成功后返回不泄露地址的连接说明，且错误地址从未收到登录认证请求。
func TestConnectServerEntrySkipsMismatchedHostKeyThenConnectsNextAddress(t *testing.T) {
	wrongSigner, correctSigner := fixedSigner(t), fixedSigner(t)
	var wrongAuthRequests, correctAuthRequests atomic.Int64
	wrongAddr, _, wrongCleanup := testsshd.Start(t, testsshd.Options{
		HostKey: wrongSigner, Password: "pw", AuthRequests: &wrongAuthRequests,
	})
	defer wrongCleanup()
	correctAddr, correctHostKey, correctCleanup := testsshd.Start(t, testsshd.Options{
		HostKey: correctSigner, Password: "pw", AuthRequests: &correctAuthRequests,
	})
	defer correctCleanup()

	st := &fakeHostKeyStore{pins: map[string]*store.Pin{
		"srv-a": {Blob: correctHostKey.Marshal(), Format: store.PinFormatBlob},
	}}
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	fixtureByAddress := map[string]string{
		"192.0.2.10":    wrongAddr,
		"198.51.100.10": correctAddr,
	}
	original := lowLevelDial
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec, _ []string) (*Client, error) {
		fixtureAddr, ok := fixtureByAddress[host]
		if !ok {
			return nil, errors.New("unexpected candidate address")
		}
		return connectWith(ctx, hostOf(fixtureAddr), portOf(fixtureAddr), user, auth, hostKeyCb, ka, nil)
	}
	t.Cleanup(func() { lowLevelDial = original })

	cli, notes, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st,
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("target", "home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 1),
			},
			DialerKnown: true,
			DialerMemberships: []*models.NetworkMembership{
				membershipAt("dialer", "home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 2),
			},
		},
	})
	if err != nil {
		t.Fatalf("connect through the second candidate: %v", err)
	}
	defer cli.Close()

	wantNotes := []string{"家A局域网 第1个地址呈现了不同的主机密钥，已跳过"}
	if strings.Join(notes, "\n") != strings.Join(wantNotes, "\n") {
		t.Fatalf("connection notes = %q, want %q", notes, wantNotes)
	}
	if n := wrongAuthRequests.Load(); n != 0 {
		t.Fatalf("the mismatched address must not receive user authentication requests, got %d", n)
	}
	if n := correctAuthRequests.Load(); n != 1 {
		t.Fatalf("the successful address must receive one user authentication request, got %d", n)
	}
	if st.saves != 0 {
		t.Fatalf("the existing anchor must not be overwritten, got %d saves", st.saves)
	}
}

// TestConnectServerEntryTriesPlannedAddressesInOrder: 统一拨号入口从拓扑来源
// 现划候选地址，前一个地址失败后继续下一个；注入的低层连接函数把虚拟局域
// 网地址映射到进程内 SSH 服务器，证明 IPv6 主机交给底层时保持独立字段，
// 由底层用带括号的形式拼接。
func TestConnectServerEntryTriesPlannedAddressesInOrder(t *testing.T) {
	fixtureAddr, fixtureKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := &fakeHostKeyStore{pins: map[string]*store.Pin{
		"srv-a": {Blob: fixtureKey.Marshal(), Format: store.PinFormatBlob},
	}}
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	var attempted []string

	original := lowLevelDial
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec, _ []string) (*Client, error) {
		attempted = append(attempted, host)
		if host == "2001:db8::1" {
			return nil, errors.New("connect failed")
		}
		return connectWith(ctx, hostOf(fixtureAddr), portOf(fixtureAddr), user, auth, hostKeyCb, ka, nil)
	}
	t.Cleanup(func() { lowLevelDial = original })

	cli, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st,
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "2001:db8::1", 2),
				membershipAt("device-target", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.5", 1),
			},
			DialerKnown: true,
			DialerMemberships: []*models.NetworkMembership{
				membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "2001:db8::2", 3),
				membershipAt("device-dialer", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.2", 4),
			},
		},
	})
	if err != nil {
		t.Fatalf("connect via second address: %v", err)
	}
	defer cli.Close()

	want := []string{"2001:db8::1", "100.64.10.5"}
	if len(attempted) != len(want) {
		t.Fatalf("attempted addresses = %v, want %v", attempted, want)
	}
	for i := range want {
		if attempted[i] != want[i] {
			t.Fatalf("attempted addresses = %v, want %v", attempted, want)
		}
	}
	if st.saves != 0 {
		t.Fatalf("an already anchored entry must verify without saving, got %d saves", st.saves)
	}
}

// TestConnectServerEntryFirstTrustWithoutVirtualLanKeepsAllCandidates：条目
// 所属设备没有虚拟局域网地址时，首次锚定仍可按普通候选顺序使用任何地址。
func TestConnectServerEntryFirstTrustWithoutVirtualLanKeepsAllCandidates(t *testing.T) {
	st := &fakeHostKeyStore{}
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	remoteKey := testPublicKey(t)
	var attempted []string
	original := lowLevelDial
	lowLevelDial = func(_ context.Context, host string, _ int, _ string, _ ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, _ keepAliveSpec, _ []string) (*Client, error) {
		attempted = append(attempted, host)
		if host == "192.0.2.10" {
			return nil, errors.New("physical address unavailable")
		}
		if err := hostKeyCb(host, nil, remoteKey); err != nil {
			return nil, err
		}
		return &Client{}, nil
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st,
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 1),
			},
			DialerKnown: true,
			DialerMemberships: []*models.NetworkMembership{
				membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 2),
			},
		},
	})
	if err != nil {
		t.Fatalf("first trust may use any candidate when there is no virtual LAN address: %v", err)
	}
	want := []string{"192.0.2.10", "198.51.100.10"}
	if len(attempted) != len(want) || attempted[0] != want[0] || attempted[1] != want[1] {
		t.Fatalf("attempted addresses = %v, want %v", attempted, want)
	}
	if st.saves != 1 || st.lastHandshake != "198.51.100.10:2202" {
		t.Fatalf("fallback first trust must save over the real handshake address: saves=%d address=%q", st.saves, st.lastHandshake)
	}
}

// TestConnectServerEntryFirstTrustUsesOnlyVirtualLan：条目尚无锚且所属设备
// 有虚拟局域网地址时，首次锚定不能尝试物理局域网地址与兜底地址。
func TestConnectServerEntryFirstTrustUsesOnlyVirtualLan(t *testing.T) {
	st := &fakeHostKeyStore{}
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	remoteKey := testPublicKey(t)
	var attempted []string

	original := lowLevelDial
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec, _ []string) (*Client, error) {
		attempted = append(attempted, host)
		if err := hostKeyCb(host, nil, remoteKey); err != nil {
			return nil, err
		}
		return &Client{}, nil
	}
	t.Cleanup(func() { lowLevelDial = original })

	cli, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st,
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 2),
				membershipAt("device-target", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.5", 1),
			},
			DialerKnown: true,
			DialerMemberships: []*models.NetworkMembership{
				membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 3),
				membershipAt("device-dialer", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.2", 4),
			},
		},
	})
	if err != nil {
		t.Fatalf("first trust over virtual LAN: %v", err)
	}
	_ = cli

	want := []string{"100.64.10.5"}
	if len(attempted) != len(want) || attempted[0] != want[0] {
		t.Fatalf("attempted addresses = %v, want %v", attempted, want)
	}
	if st.saves != 1 || st.lastHandshake != "100.64.10.5:2202" {
		t.Fatalf("first trust must save over the real handshake address: saves=%d address=%q", st.saves, st.lastHandshake)
	}
}

// TestConnectServerEntryFirstTrustWithoutCommonVirtualLanFails：拨号设备与
// 目标没有共同虚拟局域网时，首次锚定不能退回物理局域网或兜底地址。
func TestConnectServerEntryFirstTrustWithoutCommonVirtualLanFails(t *testing.T) {
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	var attempted []string
	original := lowLevelDial
	lowLevelDial = func(_ context.Context, host string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec, _ []string) (*Client, error) {
		attempted = append(attempted, host)
		return nil, errors.New("must not dial")
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: &fakeHostKeyStore{},
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 2),
				membershipAt("device-target", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.5", 1),
			},
			DialerKnown: true,
			DialerMemberships: []*models.NetworkMembership{
				membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 3),
			},
		},
	})
	if err == nil {
		t.Fatal("first trust without a common virtual LAN must fail")
	}
	want := "首次锚定只能经虚拟局域网；当前拨号设备与目标没有共同虚拟局域网。请用带外锚定，或先从虚拟局域网内的机器连接一次。"
	if err.Error() != want {
		t.Fatalf("error text:\n got %q\nwant %q", err.Error(), want)
	}
	if len(attempted) != 0 {
		t.Fatalf("no address may be attempted, got %v", attempted)
	}
}

// TestConnectServerEntryFirstTrustEquivalentPhysicalAndVirtualAddressFails：
// 同一个等价地址同时登记在共同物理局域网与共同虚拟局域网时，首次锚定仍
// 拒绝；错误必须说明冲突地址与两个网络名，并且不能开始任何拨号。
func TestConnectServerEntryFirstTrustEquivalentPhysicalAndVirtualAddressFails(t *testing.T) {
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	attempts := 0
	original := lowLevelDial
	lowLevelDial = func(_ context.Context, host string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec, _ []string) (*Client, error) {
		attempts++
		return nil, fmt.Errorf("must not dial %s", host)
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: &fakeHostKeyStore{},
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "2001:0db8:0000:0000:0000:0000:0000:0001", 2),
				membershipAt("device-target", "network-virtual", "虚拟局域网", models.NetworkVirtual, "2001:db8::1", 1),
			},
			DialerKnown: true,
			DialerMemberships: []*models.NetworkMembership{
				membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "2001:db8::2", 3),
				membershipAt("device-dialer", "network-virtual", "虚拟局域网", models.NetworkVirtual, "2001:db8::3", 4),
			},
		},
	})
	if err == nil {
		t.Fatal("an equivalent physical and virtual address must refuse first trust")
	}
	if !errors.Is(err, ErrFirstTrustVirtualLanAddressAlsoPhysical) {
		t.Fatalf("error must match the dedicated sentinel: %v", err)
	}
	if errors.Is(err, ErrFirstTrustNoCommonVirtualLan) {
		t.Fatalf("the common virtual LAN exists, so the generic error is wrong: %v", err)
	}
	for _, want := range []string{
		"2001:db8::1", "家A局域网", "虚拟局域网",
		"同时登记在物理局域网", "无法证明这次连接走的是虚拟局域网",
		"servers pin-hostkey", "--fingerprint",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error text must contain %q:\n%s", want, err.Error())
		}
	}
	if attempts != 0 {
		t.Fatalf("no address may be attempted, got %d attempts", attempts)
	}
}

// TestConnectServerEntryFirstTrustWithoutTopologyStillHonorsVirtualLan：即使
// 拨号请求没有提供拓扑视图，也不能把尚无锚的多地址条目退回兜底地址。
func TestConnectServerEntryFirstTrustWithoutTopologyStillHonorsVirtualLan(t *testing.T) {
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	var attempted []string
	original := lowLevelDial
	lowLevelDial = func(_ context.Context, host string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec, _ []string) (*Client, error) {
		attempted = append(attempted, host)
		return nil, errors.New("must not dial")
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"),
		HostKeys: &fakeHostKeyStore{virtualLan: []string{"100.64.10.5:2202"}},
	})
	if err == nil {
		t.Fatal("first trust without a reachable virtual LAN address must fail")
	}
	want := "首次锚定只能经虚拟局域网；当前拨号设备与目标没有共同虚拟局域网。请用带外锚定，或先从虚拟局域网内的机器连接一次。"
	if err.Error() != want {
		t.Fatalf("error text:\n got %q\nwant %q", err.Error(), want)
	}
	if len(attempted) != 0 {
		t.Fatalf("no address may be attempted, got %v", attempted)
	}
}

// TestConnectServerEntryFailureListsCandidatesWithoutAddresses: 全部候选失败时
// 错误逐行列出网络名加序号或兜底地址；底层错误先经过既有地址打码，拓扑缺
// 失时附补登记提示。
func TestConnectServerEntryFailureListsCandidatesWithoutAddresses(t *testing.T) {
	srv := entryAt("srv-a", "192.0.2.10", 2202)
	original := lowLevelDial
	lowLevelDial = func(context.Context, string, int, string, ssh.AuthMethod, ssh.HostKeyCallback, keepAliveSpec, []string) (*Client, error) {
		return nil, errors.New("dial 192.0.2.10:2202: connection refused")
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: &fakeHostKeyStore{},
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 1),
			},
		},
	})
	if err == nil {
		t.Fatal("all candidates failed: want an error")
	}
	want := "全部候选地址连接失败：\n兜底地址：ssh dial: dial [REDACTED]: connection refused\n" +
		"请补登记本机设备与目标设备的共同网络；单机部署与权威端也可先设定本机设备。"
	if err.Error() != want {
		t.Fatalf("error text:\n got %q\nwant %q", err.Error(), want)
	}
}

// TestConnectServerEntryFailureLabelsEveryPlannedCandidate: 多候选全部失败时，
// 每一行都用「网络名 第N个地址」或「兜底地址」指称本次尝试，底层错误经
// 地址打码后不泄露任何 IP 或域名。
func TestConnectServerEntryFailureLabelsEveryPlannedCandidate(t *testing.T) {
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	errorsByAddress := map[string]error{
		"192.0.2.10":    errors.New("dial 192.0.2.10:2202: refused"),
		"100.64.10.5":   context.DeadlineExceeded,
		"198.51.100.10": errors.New("ssh: handshake failed: EOF"),
	}
	original := lowLevelDial
	lowLevelDial = func(_ context.Context, host string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec, _ []string) (*Client, error) {
		return nil, errorsByAddress[host]
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: &fakeHostKeyStore{pins: map[string]*store.Pin{
			srv.ID: {Blob: []byte("existing anchor"), Format: store.PinFormatBlob},
		}},
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("device-target", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.5", 1),
				membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 2),
			},
			DialerKnown: true,
			DialerMemberships: []*models.NetworkMembership{
				membershipAt("device-dialer", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.2", 3),
				membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 4),
			},
		},
	})
	want := "全部候选地址连接失败：\n" +
		"家A局域网 第1个地址：ssh dial: dial [REDACTED]: refused\n" +
		"虚拟局域网 第1个地址：连接超时\n" +
		"兜底地址：ssh dial: ssh: handshake failed: EOF"
	if err.Error() != want {
		t.Fatalf("error text:\n got %q\nwant %q", err.Error(), want)
	}
	for _, address := range []string{"192.0.2.10", "100.64.10.5", "198.51.100.10", "example.internal"} {
		if strings.Contains(err.Error(), address) {
			t.Fatalf("error text leaked address %q: %q", address, err.Error())
		}
	}
}

// TestConnectServerEntryFailureLabelsSinglePlannedCandidate: 首次锚定只保留
// 一个虚拟局域网候选时，失败说明仍要指称这个地址，而不是退回裸底层错误。
func TestConnectServerEntryFailureLabelsSinglePlannedCandidate(t *testing.T) {
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	original := lowLevelDial
	lowLevelDial = func(_ context.Context, _ string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec, _ []string) (*Client, error) {
		return nil, errors.New("dial 100.64.10.5:2202: refused")
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: &fakeHostKeyStore{},
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("target", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.5", 1),
			},
			DialerKnown: true,
			DialerMemberships: []*models.NetworkMembership{
				membershipAt("dialer", "network-virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.2", 2),
			},
		},
	})
	if err == nil {
		t.Fatal("single candidate failed: want an error")
	}
	var dialErr *ServerDialError
	if !errors.As(err, &dialErr) || len(dialErr.Failures) != 1 {
		t.Fatalf("单候选失败也应返回逐地址汇总：%v", err)
	}
	if !strings.Contains(err.Error(), "全部候选地址连接失败：\n虚拟局域网 第1个地址：") {
		t.Fatalf("单候选失败说明应包含地址指称：%q", err.Error())
	}
}

// TestConnectServerEntryDeletedLocalDeviceHint: 本机设备设定指向的设备已被
// 删除时，兜底地址失败要明确提示重新设定本机设备，而不是只提示补登记共同
// 网络。
func TestConnectServerEntryDeletedLocalDeviceHint(t *testing.T) {
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	original := lowLevelDial
	lowLevelDial = func(_ context.Context, _ string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec, _ []string) (*Client, error) {
		return nil, errors.New("dial 198.51.100.10:2202: refused")
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"),
		HostKeys: &fakeHostKeyStore{pins: map[string]*store.Pin{
			srv.ID: {Blob: []byte("anchor"), Format: store.PinFormatBlob},
		}},
		Topology: StaticDialTopology{DialerUnknownReason: FallbackOnlyLocalDeviceDeleted},
	})
	if err == nil {
		t.Fatal("fallback dial failed: want an error")
	}
	if !strings.Contains(err.Error(), "本机设备设定指向的设备已不存在，请重新设定本机设备") {
		t.Fatalf("error must explain the deleted local device setting: %q", err.Error())
	}
}

// TestConnectServerEntryFailureLabelsSkippedHostKeyMismatch: 全部候选的主机
// 密钥都与锚不一致时，逐地址错误与连接说明都用统一的短说明，不输出指纹，
// 也不泄露地址。
func TestConnectServerEntryFailureLabelsSkippedHostKeyMismatch(t *testing.T) {
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	original := lowLevelDial
	lowLevelDial = func(_ context.Context, host string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec, _ []string) (*Client, error) {
		return nil, fmt.Errorf("%w: presented SHA256:test", ErrHostKeyMismatch)
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, notes, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: &fakeHostKeyStore{pins: map[string]*store.Pin{
			"srv-a": {Blob: []byte("pinned"), Format: store.PinFormatBlob},
		}},
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("target", "home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 1),
			},
			DialerKnown: true,
			DialerMemberships: []*models.NetworkMembership{
				membershipAt("dialer", "home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 2),
			},
		},
	})
	if err == nil {
		t.Fatal("all candidates failed: want an error")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("error must remain identifiable as a host key mismatch, got %v", err)
	}
	want := "全部候选地址连接失败：\n" +
		"家A局域网 第1个地址：主机密钥不同，已跳过\n" +
		"兜底地址：主机密钥不同，已跳过"
	if err.Error() != want {
		t.Fatalf("error text:\n got %q\nwant %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("aggregate failure text must not carry fingerprints: %q", err.Error())
	}
	wantNotes := []string{
		"家A局域网 第1个地址呈现了不同的主机密钥，已跳过",
		"兜底地址呈现了不同的主机密钥，已跳过",
	}
	if strings.Join(notes, "\n") != strings.Join(wantNotes, "\n") {
		t.Fatalf("failure notes = %q, want %q", notes, wantNotes)
	}
}
