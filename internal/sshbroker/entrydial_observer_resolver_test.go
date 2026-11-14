package sshbroker

import (
	"context"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// resolverTopology 在固定拓扑之外还实现观察者解析接口，用来证明统一拨号
// 入口会自动向拓扑来源要观察者，不需要每个调用方各自接线。
type resolverTopology struct {
	StaticDialTopology
	observations atomic.Int64
}

func (t *resolverTopology) ResolveDialObserver() (DialAttemptObserver, error) {
	return t, nil
}

func (t *resolverTopology) ObserveDialAttempt(*models.Server, DialAddress, DialAttemptOutcome) {
	t.observations.Add(1)
}

func TestConnectServerEntryResolvesObserverFromTopologySource(t *testing.T) {
	fixtureAddr, fixtureKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer cleanup()
	st := &fakeHostKeyStore{pins: map[string]*store.Pin{
		"srv-a": {Blob: fixtureKey.Marshal(), Format: store.PinFormatBlob},
	}}
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	srv.DeviceID = "device-target"
	topology := &resolverTopology{StaticDialTopology: StaticDialTopology{
		TargetMemberships: []*models.NetworkMembership{
			membershipAt("device-target", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 1),
		},
		DialerKnown: true,
		DialerMemberships: []*models.NetworkMembership{
			membershipAt("device-dialer", "network-home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 2),
		},
	}}
	original := lowLevelDial
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec, _ []string) (*Client, error) {
		return connectWith(ctx, hostOf(fixtureAddr), portOf(fixtureAddr), user, auth, hostKeyCb, ka, nil)
	}
	t.Cleanup(func() { lowLevelDial = original })

	cli, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv, Auth: PasswordAuth("pw"), HostKeys: st, Topology: topology,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if got := topology.observations.Load(); got != 1 {
		t.Fatalf("observations = %d, want one for the tried topology candidate", got)
	}
}
