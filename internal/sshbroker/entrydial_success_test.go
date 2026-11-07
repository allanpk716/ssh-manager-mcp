package sshbroker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// fileDialSource 把固定拓扑与真实成功地址缓存文件接到统一拨号入口。
type fileDialSource struct {
	StaticDialTopology
	cache *FileDialSuccessCache
}

func (s fileDialSource) ServerDialSuccess(server *models.Server) (DialSuccessRecord, bool, error) {
	return s.cache.Success(server)
}

func (s fileDialSource) RecordServerDialSuccess(server *models.Server, record DialSuccessRecord) error {
	return s.cache.Record(server, record)
}

func (s fileDialSource) DeleteServerDialSuccess(server *models.Server, address string) error {
	return s.cache.Delete(server, address)
}

// TestConnectServerEntrySuccessCacheOrdersSameLevelByDuration: 仍然有效的成功
// 地址在同一级里先按记录耗时排序，未命中的地址继续按登记序号排，级别顺序
// 与兜底地址位置不变。
func TestConnectServerEntrySuccessCacheOrdersSameLevelByDuration(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "ssh-dial-successes.json")
	cache := NewFileDialSuccessCache(cachePath)
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	if err := cache.Record(srv, DialSuccessRecord{
		Address: "192.0.2.20", SucceededAt: time.Now(), Duration: 20 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}

	var attemptedMu sync.Mutex
	var attempted []string
	original := lowLevelDial
	lowLevelDial = func(_ context.Context, host string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec) (*Client, error) {
		attemptedMu.Lock()
		attempted = append(attempted, host)
		attemptedMu.Unlock()
		return nil, errors.New("unavailable")
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv,
		Auth:   PasswordAuth("pw"),
		HostKeys: &fakeHostKeyStore{pins: map[string]*store.Pin{
			srv.ID: {Blob: []byte("anchor"), Format: store.PinFormatBlob},
		}},
		Topology: fileDialSource{
			StaticDialTopology: StaticDialTopology{
				TargetMemberships: []*models.NetworkMembership{
					membershipAt("target", "home-1", "家A", models.NetworkPhysical, "192.0.2.10", 1),
					membershipAt("target", "home-2", "家B", models.NetworkPhysical, "192.0.2.20", 2),
				},
				DialerKnown: true,
				DialerMemberships: []*models.NetworkMembership{
					membershipAt("dialer", "home-1", "家A", models.NetworkPhysical, "192.0.2.1", 3),
					membershipAt("dialer", "home-2", "家B", models.NetworkPhysical, "192.0.2.21", 4),
				},
			},
			cache: cache,
		},
	})
	if err == nil {
		t.Fatal("all candidates failed: want an error")
	}

	want := []string{"192.0.2.20", "192.0.2.10", "198.51.100.10"}
	if len(attempted) != len(want) {
		t.Fatalf("attempted addresses = %v, want %v", attempted, want)
	}
	for i := range want {
		if attempted[i] != want[i] {
			t.Fatalf("attempted addresses = %v, want %v", attempted, want)
		}
	}
}

// TestConnectServerEntryWritesSuccessRecordAndRewritesCorruptFile: 成功连接
// 后按服务器条目记录真实握手地址、成功时间与耗时；原文件损坏时当作空缓存
// 继续拨号，并在这次写入时重写成可解析文件。
func TestConnectServerEntryWritesSuccessRecordAndRewritesCorruptFile(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "ssh-dial-successes.json")
	if err := os.WriteFile(cachePath, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := NewFileDialSuccessCache(cachePath)
	srv := entryAt("srv-a", "198.51.100.10", 2202)

	original := lowLevelDial
	lowLevelDial = func(_ context.Context, host string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec) (*Client, error) {
		time.Sleep(2 * time.Millisecond)
		if host != "192.0.2.10" {
			return nil, errors.New("only the physical address is available")
		}
		return &Client{}, nil
	}
	t.Cleanup(func() { lowLevelDial = original })

	before := time.Now()
	cli, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv,
		Auth:   PasswordAuth("pw"),
		HostKeys: &fakeHostKeyStore{pins: map[string]*store.Pin{
			srv.ID: {Blob: []byte("anchor"), Format: store.PinFormatBlob},
		}},
		Topology: fileDialSource{
			StaticDialTopology: StaticDialTopology{
				TargetMemberships: []*models.NetworkMembership{
					membershipAt("target", "home", "家A", models.NetworkPhysical, "192.0.2.10", 1),
				},
				DialerKnown: true,
				DialerMemberships: []*models.NetworkMembership{
					membershipAt("dialer", "home", "家A", models.NetworkPhysical, "192.0.2.1", 2),
				},
			},
			cache: cache,
		},
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	_ = cli

	record, present, err := cache.Success(srv)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("successful connect must record a dial success")
	}
	if record.Address != "192.0.2.10" {
		t.Fatalf("recorded address = %q, want the successful candidate address", record.Address)
	}
	if record.SucceededAt.Before(before) || time.Since(record.SucceededAt) > time.Second {
		t.Fatalf("recorded success time %v is not close to the connect time", record.SucceededAt)
	}
	if record.Duration < 2*time.Millisecond {
		t.Fatalf("recorded duration = %v, want at least the dialer's elapsed time", record.Duration)
	}
}

// TestConnectServerEntrySuccessCacheIgnoresStaleRecords: 过期记录、已删除
// 服务器条目的记录、以及不在当前候选里的地址都不改变登记顺序。
func TestConnectServerEntrySuccessCacheIgnoresStaleRecords(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, cache *FileDialSuccessCache, server *models.Server)
	}{
		{
			name: "expired",
			seed: func(t *testing.T, cache *FileDialSuccessCache, server *models.Server) {
				if err := cache.Record(server, DialSuccessRecord{
					Address:     "192.0.2.20",
					SucceededAt: time.Now().Add(-DialSuccessCacheLifetime - time.Second),
					Duration:    time.Millisecond,
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "address no longer candidate",
			seed: func(t *testing.T, cache *FileDialSuccessCache, server *models.Server) {
				if err := cache.Record(server, DialSuccessRecord{
					Address: "203.0.113.20", SucceededAt: time.Now(), Duration: time.Millisecond,
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "server entry deleted",
			seed: func(t *testing.T, cache *FileDialSuccessCache, _ *models.Server) {
				deleted := entryAt("srv-deleted", "198.51.100.20", 2202)
				if err := cache.Record(deleted, DialSuccessRecord{
					Address: "192.0.2.20", SucceededAt: time.Now(), Duration: time.Millisecond,
				}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cache := NewFileDialSuccessCache(filepath.Join(t.TempDir(), "ssh-dial-successes.json"))
			srv := entryAt("srv-a", "198.51.100.10", 2202)
			testCase.seed(t, cache, srv)

			var attemptedMu sync.Mutex
			var attempted []string
			original := lowLevelDial
			lowLevelDial = func(_ context.Context, host string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec) (*Client, error) {
				attemptedMu.Lock()
				attempted = append(attempted, host)
				attemptedMu.Unlock()
				return nil, errors.New("unavailable")
			}
			t.Cleanup(func() { lowLevelDial = original })

			_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
				Server: srv,
				Auth:   PasswordAuth("pw"),
				HostKeys: &fakeHostKeyStore{pins: map[string]*store.Pin{
					srv.ID: {Blob: []byte("anchor"), Format: store.PinFormatBlob},
				}},
				Topology: fileDialSource{
					StaticDialTopology: StaticDialTopology{
						TargetMemberships: []*models.NetworkMembership{
							membershipAt("target", "home-1", "家A", models.NetworkPhysical, "192.0.2.10", 1),
							membershipAt("target", "home-2", "家B", models.NetworkPhysical, "192.0.2.20", 2),
						},
						DialerKnown: true,
						DialerMemberships: []*models.NetworkMembership{
							membershipAt("dialer", "home-1", "家A", models.NetworkPhysical, "192.0.2.1", 3),
							membershipAt("dialer", "home-2", "家B", models.NetworkPhysical, "192.0.2.21", 4),
						},
					},
					cache: cache,
				},
			})
			if err == nil {
				t.Fatal("all candidates failed: want an error")
			}
			want := []string{"192.0.2.10", "192.0.2.20", "198.51.100.10"}
			if len(attempted) != len(want) || attempted[0] != want[0] || attempted[1] != want[1] || attempted[2] != want[2] {
				t.Fatalf("attempted addresses = %v, want %v", attempted, want)
			}
		})
	}
}

// TestConnectServerEntryMismatchDeletesSuccessRecord: 主机密钥不一致被跳过的
// 地址不能保留旧的成功记录；后续候选连接成功时也不会把它写回。
func TestConnectServerEntryMismatchDeletesSuccessRecord(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "ssh-dial-successes.json")
	cache := NewFileDialSuccessCache(cachePath)
	srv := entryAt("srv-a", "198.51.100.10", 2202)
	if err := cache.Record(srv, DialSuccessRecord{
		Address: "192.0.2.10", SucceededAt: time.Now(), Duration: 10 * time.Millisecond,
	}); err != nil {
		t.Fatal(err)
	}

	original := lowLevelDial
	lowLevelDial = func(_ context.Context, host string, _ int, _ string, _ ssh.AuthMethod, hostKeyCb ssh.HostKeyCallback, _ keepAliveSpec) (*Client, error) {
		if host == "192.0.2.10" {
			return nil, fmt.Errorf("%w: presented SHA256:wrong", ErrHostKeyMismatch)
		}
		return &Client{}, nil
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: srv,
		Auth:   PasswordAuth("pw"),
		HostKeys: &fakeHostKeyStore{pins: map[string]*store.Pin{
			srv.ID: {Blob: []byte("anchor"), Format: store.PinFormatBlob},
		}},
		Topology: fileDialSource{
			StaticDialTopology: StaticDialTopology{
				TargetMemberships: []*models.NetworkMembership{
					membershipAt("target", "home", "家A", models.NetworkPhysical, "192.0.2.10", 1),
				},
				DialerKnown: true,
				DialerMemberships: []*models.NetworkMembership{
					membershipAt("dialer", "home", "家A", models.NetworkPhysical, "192.0.2.1", 2),
				},
			},
			cache: cache,
		},
	})
	if err != nil {
		t.Fatalf("connect through the next candidate: %v", err)
	}

	record, present, err := cache.Success(srv)
	if err != nil {
		t.Fatal(err)
	}
	if !present {
		t.Fatal("the successful fallback address must be recorded")
	}
	if record.Address != "198.51.100.10" {
		t.Fatalf("mismatched address must be removed from the success cache; got record %+v", record)
	}
}

// TestConnectServerEntrySuccessCacheKeepsSiblingEntry: 同一设备上的兄弟服务
// 器条目各自记录成功地址；一个条目遇到主机密钥不一致时只清自己的记录。
func TestConnectServerEntrySuccessCacheKeepsSiblingEntry(t *testing.T) {
	cache := NewFileDialSuccessCache(filepath.Join(t.TempDir(), "ssh-dial-successes.json"))
	first := entryAt("srv-first", "198.51.100.10", 2202)
	second := entryAt("srv-second", "198.51.100.10", 2202)
	for _, server := range []*models.Server{first, second} {
		if err := cache.Record(server, DialSuccessRecord{
			Address: "192.0.2.10", SucceededAt: time.Now(), Duration: 10 * time.Millisecond,
		}); err != nil {
			t.Fatal(err)
		}
	}

	original := lowLevelDial
	lowLevelDial = func(_ context.Context, _ string, _ int, _ string, _ ssh.AuthMethod, _ ssh.HostKeyCallback, _ keepAliveSpec) (*Client, error) {
		return nil, fmt.Errorf("%w: presented SHA256:wrong", ErrHostKeyMismatch)
	}
	t.Cleanup(func() { lowLevelDial = original })

	_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
		Server: first,
		Auth:   PasswordAuth("pw"),
		HostKeys: &fakeHostKeyStore{pins: map[string]*store.Pin{
			first.ID: {Blob: []byte("anchor"), Format: store.PinFormatBlob},
		}},
		Topology: fileDialSource{
			StaticDialTopology: StaticDialTopology{
				TargetMemberships: []*models.NetworkMembership{
					membershipAt("target", "home", "家A", models.NetworkPhysical, "192.0.2.10", 1),
				},
				DialerKnown: true,
				DialerMemberships: []*models.NetworkMembership{
					membershipAt("dialer", "home", "家A", models.NetworkPhysical, "192.0.2.1", 2),
				},
			},
			cache: cache,
		},
	})
	if !errors.Is(err, ErrHostKeyMismatch) {
		t.Fatalf("all candidates mismatched: want host key mismatch, got %v", err)
	}

	if record, present, _ := cache.Success(first); present {
		t.Fatalf("first sibling record must be removed, got %+v", record)
	}
	record, present, err := cache.Success(second)
	if err != nil {
		t.Fatal(err)
	}
	if !present || record.Address != "192.0.2.10" {
		t.Fatalf("second sibling record = %+v present=%v, want the original record", record, present)
	}
}

type concurrentDialSource struct {
	cache *FileDialSuccessCache
}

func (s concurrentDialSource) ServerDialTopology(server *models.Server) (DialTopology, error) {
	return DialTopology{
		TargetMemberships: []*models.NetworkMembership{
			membershipAt(server.DeviceID, "home", "家A", models.NetworkPhysical, server.Host, 1),
		},
		DialerKnown: true,
		DialerMemberships: []*models.NetworkMembership{
			membershipAt("dialer", "home", "家A", models.NetworkPhysical, "192.0.2.1", 2),
		},
	}, nil
}

func (s concurrentDialSource) ServerDialSuccess(server *models.Server) (DialSuccessRecord, bool, error) {
	return s.cache.Success(server)
}

func (s concurrentDialSource) RecordServerDialSuccess(server *models.Server, record DialSuccessRecord) error {
	return s.cache.Record(server, record)
}

func (s concurrentDialSource) DeleteServerDialSuccess(server *models.Server, address string) error {
	return s.cache.Delete(server, address)
}

// TestConnectServerEntryConcurrentWritesKeepCacheUsable: 同一实例的多个连接
// 并发写共享成功地址缓存时，最终文件仍可解析，每个条目的记录都可见。
func TestConnectServerEntryConcurrentWritesKeepCacheUsable(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "ssh-dial-successes.json")
	cache := NewFileDialSuccessCache(cachePath)
	source := concurrentDialSource{cache: cache}

	original := lowLevelDial
	lowLevelDial = func(context.Context, string, int, string, ssh.AuthMethod, ssh.HostKeyCallback, keepAliveSpec) (*Client, error) {
		return &Client{}, nil
	}
	t.Cleanup(func() { lowLevelDial = original })

	const serverCount = 24
	servers := make([]*models.Server, serverCount)
	for i := range servers {
		servers[i] = entryAt(fmt.Sprintf("srv-%02d", i), fmt.Sprintf("192.0.2.%d", 10+i), 2202)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, serverCount)
	for _, server := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := ConnectServerEntry(context.Background(), ServerDialRequest{
				Server: server,
				Auth:   PasswordAuth("pw"),
				HostKeys: &fakeHostKeyStore{pins: map[string]*store.Pin{
					server.ID: {Blob: []byte("anchor"), Format: store.PinFormatBlob},
				}},
				Topology: source,
			})
			errCh <- err
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent connect: %v", err)
		}
	}

	for _, server := range servers {
		record, present, err := cache.Success(server)
		if err != nil {
			t.Fatal(err)
		}
		if !present || record.Address != server.Host {
			t.Fatalf("server %s record = %+v present=%v, want its successful address", server.ID, record, present)
		}
	}
}
