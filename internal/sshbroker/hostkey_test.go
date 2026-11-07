package sshbroker

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// fakeHostKeyStore is the package's HostKeyStore test double. Anchors are held
// as real *store.Pin values so the dual-mode matrix drives the same struct the
// production store returns (Plan 48 §5).
type fakeHostKeyStore struct {
	pins           map[string]*store.Pin // 按条目标识符存放
	saves          int
	lastHandshake  string
	handshakeSaves int
	virtualLan     []string
}

func (f *fakeHostKeyStore) GetEntryPin(srv *models.Server) (*store.Pin, error) {
	return f.pins[srv.ID], nil
}
func (f *fakeHostKeyStore) SaveEntryPin(srv *models.Server, k []byte) error {
	if f.pins == nil {
		f.pins = map[string]*store.Pin{}
	}
	f.saves++
	f.pins[srv.ID] = &store.Pin{Blob: k, Format: store.PinFormatBlob}
	return nil
}

func (f *fakeHostKeyStore) SaveEntryPinAtHandshake(srv *models.Server, handshakeAddr string, k []byte) error {
	if err := f.SaveEntryPin(srv, k); err != nil {
		return err
	}
	f.handshakeSaves++
	f.lastHandshake = handshakeAddr
	return nil
}

func (f *fakeHostKeyStore) EntryVirtualLanAddresses(*models.Server) ([]string, error) {
	return f.virtualLan, nil
}

// entryAt 构造一个指向 host:port 的服务器条目模型。
func entryAt(id, host string, port int) *models.Server {
	return &models.Server{ID: id, Name: id, Host: host, Port: port, User: "u", AuthMethod: models.AuthPassword}
}

func TestHostKeyTOFURecordsThenVerifies(t *testing.T) {
	st := &fakeHostKeyStore{}
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()

	cb, err := HostKeyTOFU(st, entryAt("srv-a", "h", portOf(addr)))
	if err != nil {
		t.Fatal(err)
	}
	// first connect: records host key
	cli, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb)
	if err != nil {
		t.Fatalf("first connect (TOFU): %v", err)
	}
	defer cli.Close()
	if len(st.pins) != 1 {
		t.Fatalf("expected 1 recorded key, got %d", len(st.pins))
	}

	// second connect: verifies, succeeds
	cb2, _ := HostKeyTOFU(st, entryAt("srv-a", "h", portOf(addr)))
	cli2, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb2)
	if err != nil {
		t.Fatalf("second connect (verify): %v", err)
	}
	defer cli2.Close()
}

// TestHostKeyTOFUPassesHandshakeAddressToStore：首次锚定必须把 SSH 回调的
// 实际握手地址交给保存锚的存储，不能改写为条目的兜底地址。
func TestHostKeyTOFUPassesHandshakeAddressToStore(t *testing.T) {
	st := &fakeHostKeyStore{}
	remote := testPublicKey(t)
	cb, err := HostKeyTOFU(st, entryAt("srv-vlan", "192.0.2.10", 22))
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("100.64.10.5", nil, remote); err != nil {
		t.Fatalf("首次锚定失败：%v", err)
	}
	if st.handshakeSaves != 1 || st.lastHandshake != "100.64.10.5:22" {
		t.Fatalf("保存接口收到的握手地址 = %q（调用 %d 次），应为 100.64.10.5:22", st.lastHandshake, st.handshakeSaves)
	}
}

// TestHostKeyTOFU normalizes IPv6 handshake addresses with ports. The SSH
// callback receives the bare IPv6 host before the address is joined, while
// candidate addresses are complete host-and-port values.
func TestHostKeyTOFUNormalizesIPv6HandshakeAddress(t *testing.T) {
	st := &fakeHostKeyStore{virtualLan: []string{"[2001:db8::5]:2202"}}
	cb, err := HostKeyTOFU(st, entryAt("srv-vlan", "192.0.2.10", 2202))
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("2001:db8::5", nil, testPublicKey(t)); err != nil {
		t.Fatalf("IPv6 虚拟局域网地址应能完成首次锚定：%v", err)
	}
	if st.lastHandshake != "[2001:db8::5]:2202" {
		t.Fatalf("握手地址 = %q, want [2001:db8::5]:2202", st.lastHandshake)
	}
}

func TestHostKeyMismatchRejected(t *testing.T) {
	// Pre-seed the store with a key that differs from the test server's real key,
	// so the callback must reject the connection as a MITM.
	addr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := &fakeHostKeyStore{pins: map[string]*store.Pin{
		"srv-a": {Blob: []byte("stale-different-key"), Format: store.PinFormatBlob},
	}}
	cb, _ := HostKeyTOFU(st, entryAt("srv-a", "h", portOf(addr)))
	_, err := Connect(context.Background(), hostOf(addr), portOf(addr), "u", PasswordAuth("pw"), cb)
	if err == nil {
		t.Fatal("mismatched host key must be rejected")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("error must wrap ErrHostKeyMismatch, got %v", err)
	}
}

// --- Plan 48: 双模 TOFU + 双指纹 mismatch(spec §2.2、§5、rider 2) ------------

// anchorWith is fakeHostKeyStore seeding sugar: one pin under entry "srv-a".
func anchorWith(p *store.Pin) *fakeHostKeyStore {
	return &fakeHostKeyStore{pins: map[string]*store.Pin{"srv-a": p}}
}

// TestHostKeyTOFU_BlobAnchorEqualPassesWithoutWrite: blob 锚旧语义零回归 —
// a presented key equal to the anchored bytes passes, writing NOTHING (the
// TOFU save path must not fire for an already-anchored host).
func TestHostKeyTOFU_BlobAnchorEqualPassesWithoutWrite(t *testing.T) {
	remote := testPublicKey(t)
	st := anchorWith(&store.Pin{Blob: remote.Marshal(), Format: store.PinFormatBlob})
	cb, err := HostKeyTOFU(st, entryAt("srv-a", "h", 22))
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("h", nil, remote); err != nil {
		t.Fatalf("presented key equal to a blob anchor must pass: %v", err)
	}
	if st.saves != 0 {
		t.Fatalf("equal-presented path must write nothing, got %d saves", st.saves)
	}
}

// TestHostKeyTOFU_FingerprintAnchorEqualPassesWithoutWrite: 指纹锚 + 呈现密钥
// 指纹等值 → pass with zero writes (Plan 48 §5 dual-mode in the callback).
func TestHostKeyTOFU_FingerprintAnchorEqualPassesWithoutWrite(t *testing.T) {
	remote := testPublicKey(t)
	st := anchorWith(&store.Pin{Blob: []byte(ssh.FingerprintSHA256(remote)), Format: store.PinFormatFingerprint})
	cb, err := HostKeyTOFU(st, entryAt("srv-a", "h", 22))
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("h", nil, remote); err != nil {
		t.Fatalf("presented key matching a fingerprint anchor must pass: %v", err)
	}
	if st.saves != 0 {
		t.Fatalf("equal-presented path must write nothing, got %d saves", st.saves)
	}
}

// TestHostKeyTOFU_MismatchCarriesBothFingerprints: rider 2 — a blob-anchor
// rejection wraps ErrHostKeyMismatch, names BOTH fingerprints (presented and
// pinned — the pinned one computed FROM the stored blob), keeps the frozen
// prefix verbatim, single line, and adds NO other key material.
func TestHostKeyTOFU_MismatchCarriesBothFingerprints(t *testing.T) {
	remote := testPublicKey(t)
	pinned := testPublicKey(t)
	st := anchorWith(&store.Pin{Blob: pinned.Marshal(), Format: store.PinFormatBlob})
	cb, err := HostKeyTOFU(st, entryAt("srv-a", "h", 22))
	if err != nil {
		t.Fatal(err)
	}
	err = cb("h", nil, remote)
	if err == nil {
		t.Fatal("mismatching host key must be rejected")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("error must wrap ErrHostKeyMismatch, got %v", err)
	}
	const prefix = "host key mismatch: possible MITM, connection rejected"
	if !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("error text must keep the frozen prefix verbatim, got: %v", err)
	}
	if !strings.Contains(err.Error(), ssh.FingerprintSHA256(remote)) {
		t.Fatalf("error text must carry the PRESENTED fingerprint: %v", err)
	}
	if !strings.Contains(err.Error(), ssh.FingerprintSHA256(pinned)) {
		t.Fatalf("error text must carry the PINNED fingerprint: %v", err)
	}
	if strings.ContainsAny(err.Error(), "\n\r") {
		t.Fatalf("error text must stay single-line: %q", err.Error())
	}
	// rider 2: 只增指纹值 — the presented key's own bytes must not leak.
	if strings.Contains(err.Error(), base64.StdEncoding.EncodeToString(remote.Marshal())) {
		t.Fatalf("error text must not carry key material: %v", err)
	}
}

// TestHostKeyTOFU_FingerprintAnchorMismatchCarriesStoredString: 指纹锚不等值 —
// the PINNED side of the text is the stored fingerprint STRING as-is (a
// fingerprint anchor holds no key bytes to fingerprint).
func TestHostKeyTOFU_FingerprintAnchorMismatchCarriesStoredString(t *testing.T) {
	remote := testPublicKey(t)
	other := testPublicKey(t) // must differ from remote
	storedFP := ssh.FingerprintSHA256(other)
	st := anchorWith(&store.Pin{Blob: []byte(storedFP), Format: store.PinFormatFingerprint})
	cb, err := HostKeyTOFU(st, entryAt("srv-a", "h", 22))
	if err != nil {
		t.Fatal(err)
	}
	err = cb("h", nil, remote)
	if err == nil {
		t.Fatal("mismatching host key must be rejected")
	}
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("error must wrap ErrHostKeyMismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), ssh.FingerprintSHA256(remote)) {
		t.Fatalf("error text must carry the PRESENTED fingerprint: %v", err)
	}
	if !strings.Contains(err.Error(), storedFP) {
		t.Fatalf("error text must carry the pinned fingerprint STRING as-is (%s): %v", storedFP, err)
	}
}

// TestHostKeyTOFU_EntriesAtSameAddressIndependent 同一兜底地址上的两个条目
// 经回调各自锚定、各自校验：一个条目的锚不影响另一个（票 #58）。
func TestHostKeyTOFU_EntriesAtSameAddressIndependent(t *testing.T) {
	st := &fakeHostKeyStore{}
	keyA, keyB := testPublicKey(t), testPublicKey(t)
	a := entryAt("srv-a", "192.168.1.10", 22)
	b := entryAt("srv-b", "192.168.1.10", 22)

	cbA, err := HostKeyTOFU(st, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := cbA("192.168.1.10", nil, keyA); err != nil {
		t.Fatalf("条目 A 首次锚定失败：%v", err)
	}
	cbB, err := HostKeyTOFU(st, b)
	if err != nil {
		t.Fatal(err)
	}
	if err := cbB("192.168.1.10", nil, keyB); err != nil {
		t.Fatalf("同地址条目 B 应能独立首次锚定：%v", err)
	}

	// 换机后的语义：条目 A 换成另一台机器（不同密钥）应报不一致；
	// 条目 B 的锚不受影响。
	if err := cbA("192.168.1.10", nil, keyB); !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("条目 A 呈现不同密钥应被拒绝：%v", err)
	}
	if err := cbB("192.168.1.10", nil, keyB); err != nil {
		t.Fatalf("条目 B 的锚不应受条目 A 影响：%v", err)
	}
}

// TestHostKeyTOFU_FirstTrustUsesDialedHandshakeAddress：首次信任的虚拟局域网
// 检查必须用回调参数里的实际握手地址，而不是条目的兜底地址。
func TestHostKeyTOFU_FirstTrustUsesDialedHandshakeAddress(t *testing.T) {
	st := &fakeHostKeyStore{virtualLan: []string{"100.64.10.5:22"}}
	remote := testPublicKey(t)
	cb, err := HostKeyTOFU(st, entryAt("srv-vlan", "192.0.2.10", 22))
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("100.64.10.5", nil, remote); err != nil {
		t.Fatalf("实际握手地址在候选列表中，首次信任应成功：%v", err)
	}
	if st.saves != 1 {
		t.Fatalf("首次信任应保存一次主机密钥，实际 %d 次", st.saves)
	}

	other := &fakeHostKeyStore{virtualLan: []string{"100.64.10.5:22"}}
	cb, err = HostKeyTOFU(other, entryAt("srv-vlan", "192.0.2.10", 22))
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("192.0.2.10:22", nil, testPublicKey(t)); err == nil ||
		!strings.Contains(err.Error(), "首次锚定只能使用虚拟局域网地址") {
		t.Fatalf("兜底地址不在虚拟局域网候选列表中，首次信任应拒绝并说明原因：%v", err)
	}
	if other.saves != 0 {
		t.Fatal("被拒绝的首次信任不应保存主机密钥")
	}
}
