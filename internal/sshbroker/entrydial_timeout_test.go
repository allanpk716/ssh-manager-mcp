package sshbroker

import (
	"context"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
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

// TestHostKeyVerificationContextDeadlineContract covers the context contract:
// before host-key verification the visible deadline is the earlier of the short
// address timeout and the parent deadline; after verification the parent
// deadline is reported unchanged.
func TestHostKeyVerificationContextDeadlineContract(t *testing.T) {
	now := time.Now()
	parentLater, cancelLater := context.WithDeadline(context.Background(), now.Add(2*time.Second))
	defer cancelLater()
	later, _, stop := newHostKeyVerificationContext(parentLater, 50*time.Millisecond)
	defer stop()
	parentLaterDeadline, _ := parentLater.Deadline()
	deadline, present := later.Deadline()
	if !present || deadline.After(parentLaterDeadline) {
		t.Fatalf("before release deadline = %v present=%v, want the earlier of address and parent deadlines", deadline, present)
	}
	later.releaseHostKeyVerification()
	deadline, present = later.Deadline()
	parentDeadline, parentPresent := parentLater.Deadline()
	if !present || !deadline.Equal(parentDeadline) {
		t.Fatalf("after release deadline = %v present=%v, want parent deadline %v present=%v",
			deadline, present, parentDeadline, parentPresent)
	}

	parentEarlier, cancelEarlier := context.WithDeadline(context.Background(), now.Add(20*time.Millisecond))
	defer cancelEarlier()
	earlier, _, stopEarlier := newHostKeyVerificationContext(parentEarlier, 50*time.Millisecond)
	defer stopEarlier()
	parentEarlierDeadline, _ := parentEarlier.Deadline()
	deadline, present = earlier.Deadline()
	if !present || !deadline.Equal(parentEarlierDeadline) {
		t.Fatalf("parent-first deadline = %v present=%v, want %v", deadline, present, parentEarlierDeadline)
	}

	noParentDeadline, release, stopNoParent := newHostKeyVerificationContext(context.Background(), 50*time.Millisecond)
	defer stopNoParent()
	release()
	if deadline, present := noParentDeadline.Deadline(); present {
		t.Fatalf("released context without a parent deadline reported %v, want no deadline", deadline)
	}
}

type hostKeyStoreObservingVerificationContext struct {
	fakeHostKeyStore
	dialContext     context.Context
	deadline        time.Time
	deadlinePresent bool
	entered         chan struct{}
	release         chan struct{}
}

func (s *hostKeyStoreObservingVerificationContext) EntryVirtualLanAddresses(*models.Server) ([]string, error) {
	close(s.entered)
	<-s.release
	deadline, present := s.dialContext.Deadline()
	s.deadline, s.deadlinePresent = deadline, present
	return nil, nil
}

// TestConnectServerEntryReleasesAddressTimeoutWhenHostKeyArrives: the short
// timeout ends as soon as the server presents its host key and the verification
// callback starts. Authority-side verification during first trust therefore runs
// under the parent deadline rather than the short address timeout.
func TestConnectServerEntryReleasesAddressTimeoutWhenHostKeyArrives(t *testing.T) {
	parentDeadline := time.Now().Add(2 * time.Second)
	parent, cancel := context.WithDeadline(context.Background(), parentDeadline)
	defer cancel()

	hostKeys := &hostKeyStoreObservingVerificationContext{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	original := lowLevelDial
	lowLevelDial = func(ctx context.Context, _ string, _ int, _ string, _ ssh.AuthMethod, hostKeyCallback ssh.HostKeyCallback, _ keepAliveSpec, _ []string) (*Client, error) {
		hostKeys.dialContext = ctx
		return nil, hostKeyCallback("192.0.2.10:2202", nil, testPublicKey(t))
	}
	t.Cleanup(func() { lowLevelDial = original })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = ConnectServerEntry(parent, ServerDialRequest{
			Server: entryAt("srv-a", "198.51.100.10", 2202), Auth: PasswordAuth("pw"),
			HostKeys: hostKeys, AddressTimeout: 20 * time.Millisecond,
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
	}()

	<-hostKeys.entered
	close(hostKeys.release)
	<-done
	if !hostKeys.deadline.Equal(parentDeadline) {
		t.Fatalf("verification deadline = %v, want parent deadline %v", hostKeys.deadline, parentDeadline)
	}
}

// TestConnectServerEntryAddressTimeoutAndSingleCandidate: 多候选时每个地址
// 在主机密钥校验完成前都有独立短超时，超时后继续下一个地址；唯一候选不套
// 用短超时，仍只受调用方上下文约束。
func TestConnectServerEntryAddressTimeoutAndSingleCandidate(t *testing.T) {
	t.Run("multiple candidates", func(t *testing.T) {
		fixtureAddr, fixtureKey, cleanup := testsshd.Start(t, testsshd.Options{Password: "pw"})
		defer cleanup()
		reclaimed := make(chan struct{})
		var attempted []string
		original := lowLevelDial
		lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec, _ []string) (*Client, error) {
			attempted = append(attempted, host)
			if host != "192.0.2.10" {
				return connectWith(ctx, hostOf(fixtureAddr), portOf(fixtureAddr), user, auth, hostKeyCb, ka, nil)
			}
			select {
			case <-ctx.Done():
				close(reclaimed)
				return nil, ctx.Err()
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
				HostKeys: &fakeHostKeyStore{pins: map[string]*store.Pin{
					"srv-a": {Blob: fixtureKey.Marshal(), Format: store.PinFormatBlob},
				}}, AddressTimeout: 20 * time.Millisecond,
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
		lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec, _ []string) (*Client, error) {
			if _, hasDeadline := ctx.Deadline(); hasDeadline {
				t.Error("the only candidate must inherit the caller context without an address deadline")
			}
			close(entered)
			<-release
			return connectWith(ctx, hostOf(fixtureAddr), portOf(fixtureAddr), user, auth, hostKeyCb, ka, nil)
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

// TestConnectServerEntryAddressTimeoutEndsAtHostKeyVerification: 多地址短
// 超时只覆盖建立连接到主机密钥校验完成。密钥校验通过后，登录认证可以慢于
// 短超时；这条地址仍应成功，而不是被误判为超时后继续尝试兜底地址。
func TestConnectServerEntryAddressTimeoutEndsAtHostKeyVerification(t *testing.T) {
	signer := fixedSigner(t)
	fixtureAddr, hostKey, cleanup := testsshd.Start(t, testsshd.Options{
		HostKey: signer, Password: "pw", AuthDelay: 120 * time.Millisecond,
	})
	defer cleanup()

	st := &fakeHostKeyStore{pins: map[string]*store.Pin{
		"srv-a": {Blob: hostKey.Marshal(), Format: store.PinFormatBlob},
	}}
	original := lowLevelDial
	lowLevelDial = func(ctx context.Context, host string, port int, user string, auth ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, ka keepAliveSpec, _ []string) (*Client, error) {
		if host == "192.0.2.10" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return connectWith(ctx, hostOf(fixtureAddr), portOf(fixtureAddr), user, auth, hostKeyCb, ka, nil)
	}
	t.Cleanup(func() { lowLevelDial = original })

	cli, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: entryAt("srv-a", "198.51.100.10", 2202), Auth: PasswordAuth("pw"),
		HostKeys: st, AddressTimeout: 20 * time.Millisecond,
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
		t.Fatalf("authentication slower than the address timeout must still connect: %v", err)
	}
	defer cli.Close()
	if st.saves != 0 {
		t.Fatalf("the matching anchor must not be rewritten, got %d saves", st.saves)
	}
}
