package sshbroker

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// dialObserverRecorder 把统一拨号入口交出的观察写进临时探测结果文件，并
// 记录调用顺序，供断言「第一个候选失败、第二个成功时两条都写」。
type dialObserverRecorder struct {
	results *proberesults.FileProbeResults
	seen    []string
}

func (r *dialObserverRecorder) ObserveDialAttempt(server *models.Server, candidate DialAddress, outcome DialAttemptOutcome) {
	r.seen = append(r.seen, candidate.NetworkName+"="+string(outcome.Category))
	category := outcome.Category
	if outcome.OK {
		category = models.ProbeErrorNone
	}
	_ = r.results.Record(proberesults.Observation{
		OriginDeviceID: "device-dialer", TargetDeviceID: server.DeviceID,
		Network: candidate.NetworkName, Address: candidate.Address, Port: server.Port,
		OK: outcome.OK, Category: category, Detail: outcome.Detail,
		Duration: outcome.Duration, ObservedAt: time.Now(), Source: proberesults.SourceDial,
	})
}

// TestConnectServerEntryObservesEveryTriedTopologyCandidate: 统一拨号入口每
// 试完一个拓扑候选地址就交一条观察；第一个候选失败、第二个成功时两条都
// 写进探测结果文件，兜底主机不属于登记网络、不写。
func TestConnectServerEntryObservesEveryTriedTopologyCandidate(t *testing.T) {
	fixtureAddr, fixtureKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := &fakeHostKeyStore{pins: map[string]*store.Pin{
		"srv-a": {Blob: fixtureKey.Marshal(), Format: store.PinFormatBlob},
	}}
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	srv.DeviceID = "device-target"
	results := proberesults.NewFileProbeResults(filepath.Join(t.TempDir(), "probe-results.json"))
	recorder := &dialObserverRecorder{results: results}

	original := lowLevelDial
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec) (*Client, error) {
		switch host {
		case "192.0.2.10":
			return nil, errors.New("connect failed")
		case "100.64.10.5", "198.51.100.10", "203.0.113.99":
			return connectWith(ctx, hostOf(fixtureAddr), portOf(fixtureAddr), user, auth, hostKeyCb, ka)
		}
		return nil, errors.New("unexpected candidate address")
	}
	t.Cleanup(func() { lowLevelDial = original })

	cli, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st,
		Observer: recorder,
		Topology: StaticDialTopology{
			TargetMemberships: []*models.NetworkMembership{
				membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 2),
				membershipAt("device-target", "network-virtual", "NetBird", models.NetworkVirtual, "100.64.10.5", 1),
			},
			DialerKnown: true,
			DialerMemberships: []*models.NetworkMembership{
				membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 3),
				membershipAt("device-dialer", "network-virtual", "NetBird", models.NetworkVirtual, "100.64.10.2", 4),
			},
		},
	})
	if err != nil {
		t.Fatalf("connect via the virtual local area network address: %v", err)
	}
	defer cli.Close()

	if got := strings.Join(recorder.seen, ";"); got != "家A局域网=other;NetBird=" {
		t.Fatalf("observed attempts = %q, want the physical failure then the virtual success", got)
	}
	observations, err := results.List("device-dialer", "device-target")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations = %+v, want one per tried topology address", observations)
	}
	// 成功连接后再触发一次兜底主机拨号，证明兜底主机不写观察。
	fallbackServer := entryAt("srv-a", "203.0.113.99", 2202)
	if _, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: fallbackServer, Auth: PasswordAuth("pw"), HostKeys: st, Observer: recorder,
	}); err != nil {
		t.Fatalf("dial the fallback entry: %v", err)
	}
	afterFallback, err := results.List("device-dialer", "device-target")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterFallback) != 2 {
		t.Fatalf("observations after fallback = %+v, want the fallback host unrecorded", afterFallback)
	}
}

// TestConnectServerEntryObservesHostKeyMismatchAsFailure: 主机密钥不一致导致
// 跳过的地址也交一条失败观察，错误分类为「其他」并附说明。
func TestConnectServerEntryObservesHostKeyMismatchAsFailure(t *testing.T) {
	wrongSigner, correctSigner := fixedSigner(t), fixedSigner(t)
	wrongAddr, _, wrongCleanup := testsshd.Start(t, testsshd.Options{HostKey: wrongSigner, Password: "pw"})
	defer wrongCleanup()
	correctAddr, correctHostKey, correctCleanup := testsshd.Start(t, testsshd.Options{
		HostKey: correctSigner, Password: "pw",
	})
	defer correctCleanup()

	st := &fakeHostKeyStore{pins: map[string]*store.Pin{
		"srv-a": {Blob: correctHostKey.Marshal(), Format: store.PinFormatBlob},
	}}
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	srv.DeviceID = "device-target"
	results := proberesults.NewFileProbeResults(filepath.Join(t.TempDir(), "probe-results.json"))
	recorder := &dialObserverRecorder{results: results}
	fixtureByAddress := map[string]string{"192.0.2.10": wrongAddr, "198.51.100.10": correctAddr}
	original := lowLevelDial
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec) (*Client, error) {
		return connectWith(ctx, hostOf(fixtureByAddress[host]), portOf(fixtureByAddress[host]), user, auth, hostKeyCb, ka)
	}
	t.Cleanup(func() { lowLevelDial = original })

	cli, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st, Observer: recorder,
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
		t.Fatalf("connect through the fallback address: %v", err)
	}
	defer cli.Close()

	observations, err := results.List("device-dialer", "device-target")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].OK ||
		observations[0].Category != models.ProbeErrorOther ||
		!strings.Contains(observations[0].Detail, "主机密钥不同") {
		t.Fatalf("observations = %+v, want the mismatched address as a failure with detail", observations)
	}
}
