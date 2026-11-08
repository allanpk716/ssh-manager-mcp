package mcpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/webui"
)

type fakeListener struct {
	address string
	closed  bool
}

func (l *fakeListener) Accept() (net.Conn, error) { return nil, io.EOF }
func (l *fakeListener) Close() error {
	l.closed = true
	return nil
}
func (l *fakeListener) Addr() net.Addr { return fakeAddr(l.address) }

type fakeAddr string

func (a fakeAddr) Network() string { return "tcp" }
func (a fakeAddr) String() string  { return string(a) }

type listenerTest struct {
	mu        sync.Mutex
	plans     []webui.ListenPlan
	planErrs  []error
	bindCalls []string
	bindFail  map[string]int
	listeners []*fakeListener
	allowed   [][]string
	logs      []string
	served    []string
}

func (t *listenerTest) plan() (webui.ListenPlan, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.planErrs) > 0 {
		err := t.planErrs[0]
		t.planErrs = t.planErrs[1:]
		return webui.ListenPlan{}, err
	}
	if len(t.plans) == 0 {
		return webui.ListenPlan{}, errors.New("没有更多监听计划")
	}
	plan := t.plans[0]
	t.plans = t.plans[1:]
	return plan, nil
}

func TestWebUIListenerManagerRunContinuesAfterPlanError(t *testing.T) {
	test := &listenerTest{
		planErrs: []error{errors.New("读取拓扑失败")},
		plans:    []webui.ListenPlan{{Addresses: []string{"127.0.0.1"}}},
	}
	manager := NewWebUIListenerManager(WebUIListenerOptions{
		Port: 7879, Plan: test.plan, Bind: test.bind, Serve: test.serve,
		SetAllowedHosts: test.setAllowedHosts, Log: test.log,
		PollInterval: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		test.mu.Lock()
		bound := len(test.bindCalls)
		test.mu.Unlock()
		if bound == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("监听管理者退出错误 = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("监听管理者关闭超时")
	}
	test.mu.Lock()
	bindCalls := append([]string(nil), test.bindCalls...)
	logs := append([]string(nil), test.logs...)
	test.mu.Unlock()
	if len(bindCalls) != 1 || bindCalls[0] != "127.0.0.1:7879" {
		t.Fatalf("推导恢复后应继续监听回环地址，绑定调用 = %v", bindCalls)
	}
	if len(logs) == 0 {
		t.Fatal("推导失败应写日志")
	}
}

func (t *listenerTest) bind(address string) (net.Listener, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.bindCalls = append(t.bindCalls, address)
	if t.bindFail == nil {
		t.bindFail = map[string]int{}
	}
	if t.bindFail[address] > 0 {
		t.bindFail[address]--
		return nil, errors.New("地址暂时不可用")
	}
	listener := &fakeListener{address: address}
	t.listeners = append(t.listeners, listener)
	return listener, nil
}

func (t *listenerTest) serve(listener net.Listener) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.served = append(t.served, listener.Addr().String())
	return nil
}

func (t *listenerTest) setAllowedHosts(addresses []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.allowed = append(t.allowed, addresses)
}

func (t *listenerTest) log(format string, arguments ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.logs = append(t.logs, strings.TrimSpace(format))
}

func TestWebUIListenerManagerClosesOldAndOpensNewListeners(t *testing.T) {
	test := &listenerTest{plans: []webui.ListenPlan{
		{Addresses: []string{"100.64.10.5", "127.0.0.1"}},
		{Addresses: []string{"100.64.10.5", "100.64.10.6"}},
		{Addresses: []string{"100.64.10.5", "100.64.10.6"}},
	}}
	manager := NewWebUIListenerManager(WebUIListenerOptions{
		Port: 7879, Plan: test.plan, Bind: test.bind, Serve: test.serve,
		SetAllowedHosts: test.setAllowedHosts, Log: test.log,
	})

	status, err := manager.Sync(time.Unix(1800000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(status.ListeningAddresses, ",") != "100.64.10.5,127.0.0.1" {
		t.Fatalf("初始监听地址 = %v", status.ListeningAddresses)
	}
	firstLoop := test.listeners[1]

	status, err = manager.Sync(time.Unix(1800000001, 0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(status.ListeningAddresses, ",") != "100.64.10.5,100.64.10.6" {
		t.Fatalf("变化后的监听地址 = %v", status.ListeningAddresses)
	}
	if !firstLoop.closed {
		t.Fatal("拓扑变化后应关闭旧监听")
	}
	last := len(test.allowed) - 1
	if strings.Join(test.allowed[last], ",") != "100.64.10.5,100.64.10.6" {
		t.Fatalf("同步给 Handler 的 Host 集合 = %v", test.allowed[last])
	}

	if _, err := manager.Sync(time.Unix(1800000002, 0)); err != nil {
		t.Fatal(err)
	}
	if got := len(test.bindCalls); got != 3 {
		t.Fatalf("地址集合未变化时不应重复绑定，调用次数 = %d，应为 3", got)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	for _, listener := range test.listeners {
		if !listener.closed {
			t.Fatalf("监听 %s 关闭管理者时应关闭", listener.address)
		}
	}
}

func TestWebUIListenerManagerRetriesFailedAddressAfterThirtySeconds(t *testing.T) {
	test := &listenerTest{
		plans: []webui.ListenPlan{
			{Addresses: []string{"100.64.10.5"}},
			{Addresses: []string{"100.64.10.5"}},
			{Addresses: []string{"100.64.10.5"}},
		},
		bindFail: map[string]int{"100.64.10.5:7879": 1},
	}
	manager := NewWebUIListenerManager(WebUIListenerOptions{
		Port: 7879, Plan: test.plan, Bind: test.bind, Serve: test.serve,
		SetAllowedHosts: test.setAllowedHosts, Log: test.log,
	})

	start := time.Unix(1800000000, 0)
	status, err := manager.Sync(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.ListeningAddresses) != 0 || len(status.PendingAddresses) != 1 {
		t.Fatalf("首次绑定失败后的状态 = %+v", status)
	}
	if !status.NextRetry.Equal(start.Add(30 * time.Second)) {
		t.Fatalf("下次重试时间 = %v，应为 30 秒后", status.NextRetry)
	}

	if _, err := manager.Sync(start.Add(29 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(test.bindCalls) != 1 {
		t.Fatalf("未到 30 秒不应重试，绑定调用 = %v", test.bindCalls)
	}
	status, err = manager.Sync(start.Add(30 * time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(status.ListeningAddresses) != 1 || len(status.PendingAddresses) != 0 {
		t.Fatalf("30 秒后重试应成功，状态 = %+v", status)
	}
	if len(test.logs) == 0 {
		t.Fatal("绑定失败与重试应写日志")
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
}
