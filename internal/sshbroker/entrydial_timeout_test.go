package sshbroker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/testsshd"
)

// TestResolveAddressTimeout covers the environment variable seam: unset means
// the specified 1.5-second default; any non-positive or unparsable value is an
// immediate startup error rather than a silent fallback.
func TestResolveAddressTimeout(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    time.Duration
		wantErr bool
	}{
		{name: "default", value: "", want: 1500 * time.Millisecond},
		{name: "override", value: "2s", want: 2 * time.Second},
		{name: "zero", value: "0", wantErr: true},
		{name: "negative", value: "-1s", wantErr: true},
		{name: "garbage", value: "fast", wantErr: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("SSHMGR_SSH_ADDRESS_TIMEOUT", testCase.value)
			got, err := ResolveAddressTimeout()
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("value %q must be rejected, got %s", testCase.value, got)
				}
				return
			}
			if err != nil || got != testCase.want {
				t.Fatalf("value %q: got %s, %v; want %s, nil", testCase.value, got, err, testCase.want)
			}
		})
	}
}

// TestConnectServerEntryAddressTimeoutAndSingleCandidate: 多候选时每个地址
// 得到独立短超时，超时后迟到的底层连接被关闭并继续下一个地址；唯一候选
// 不套用短超时，仍只受调用方上下文约束。
func TestConnectServerEntryAddressTimeoutAndSingleCandidate(t *testing.T) {
	t.Run("multiple candidates", func(t *testing.T) {
		fixtureAddr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
		defer cleanup()
		reclaimed := make(chan struct{})
		var attempted []string
		original := lowLevelDial
		lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec) (*Client, error) {
			attempted = append(attempted, host)
			if host != "192.0.2.10" {
				return connectWith(ctx, hostOf(fixtureAddr), portOf(fixtureAddr), user, auth, hostKeyCb, ka)
			}
			cli, err := connectWith(context.Background(), hostOf(fixtureAddr), portOf(fixtureAddr), user, auth, hostKeyCb, ka)
			if err != nil {
				return nil, err
			}
			select {
			case <-ctx.Done():
				cli.Close()
				close(reclaimed)
				return nil, ctx.Err()
			case <-time.After(2 * time.Second):
				cli.Close()
				return nil, errors.New("test dial release timed out")
			}
		}
		t.Cleanup(func() { lowLevelDial = original })

		done := make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			defer close(done)
			cli, _, err := ConnectServerEntry(ctx, ServerDialRequest{
				Server: entryAt("srv-a", "198.51.100.10", 2202), Auth: PasswordAuth("pw"),
				HostKeys: &fakeHostKeyStore{}, AddressTimeout: 20 * time.Millisecond,
				Topology: StaticDialTopology{
					TargetMemberships: []*models.NetworkMembership{
						membershipAt("target", "home", "家A局域网", models.NetworkPhysical, "192.0.2.10", 1),
						membershipAt("target", "virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.5", 2),
					},
					DialerKnown: true,
					DialerMemberships: []*models.NetworkMembership{
						membershipAt("dialer", "home", "家A局域网", models.NetworkPhysical, "192.0.2.2", 3),
						membershipAt("dialer", "virtual", "虚拟局域网", models.NetworkVirtual, "100.64.10.2", 4),
					},
				},
			})
			if err != nil {
				t.Errorf("second address must succeed: %v", err)
				return
			}
			cli.Close()
		}()
		select {
		case <-reclaimed:
		case <-time.After(300 * time.Millisecond):
			cancel()
			<-done
			t.Fatal("the first address was not bounded by its short timeout")
		}
		<-done
		if strings.Join(attempted, ",") != "192.0.2.10,100.64.10.5" {
			t.Fatalf("attempted addresses = %v", attempted)
		}
	})

	t.Run("single candidate", func(t *testing.T) {
		fixtureAddr, _, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
		defer cleanup()
		entered, release := make(chan struct{}), make(chan struct{})
		original := lowLevelDial
		lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec) (*Client, error) {
			if _, hasDeadline := ctx.Deadline(); hasDeadline {
				t.Error("the only candidate must inherit the caller context without an address deadline")
			}
			close(entered)
			<-release
			return connectWith(ctx, hostOf(fixtureAddr), portOf(fixtureAddr), user, auth, hostKeyCb, ka)
		}
		t.Cleanup(func() { lowLevelDial = original })

		done := make(chan struct{})
		go func() {
			defer close(done)
			cli, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
				Server: entryAt("srv-a", "192.0.2.10", 2202), Auth: PasswordAuth("pw"),
				HostKeys: &fakeHostKeyStore{}, AddressTimeout: 10 * time.Millisecond,
			})
			if err != nil {
				t.Errorf("single candidate must succeed after release: %v", err)
				return
			}
			cli.Close()
		}()
		<-entered
		select {
		case <-done:
			t.Fatal("the single candidate must not be cut off by the address timeout")
		case <-time.After(80 * time.Millisecond):
		}
		close(release)
		<-done
	})
}
