package mcpserver

import (
	"context"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/testsshd"
)

// TestOutputDoesNotExposeTerminalBeforeAuditEnd 确定性钉住发布顺序：结束审计
// 回调未返回时，长轮询不得返回终态；回调返回并广播后，终态与审计行同时可见。
func TestOutputDoesNotExposeTerminalBeforeAuditEnd(t *testing.T) {
	m := newTestTM(t, 1)
	defer m.CloseAll()

	server, hostKey, closeServer := testsshd.Start(t, testsshd.Options{Password: "pw"})
	defer closeServer()
	host, portText, err := net.SplitHostPort(server)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	client, _, err := sshbroker.ConnectServerEntry(context.Background(), sshbroker.ServerDialRequest{
		Server:   &models.Server{Host: host, Port: port, User: "u"},
		Auth:     sshbroker.PasswordAuth("pw"),
		HostKeys: fixedHostKeys(hostKey),
	})
	if err != nil {
		t.Fatal(err)
	}

	auditEntered := make(chan struct{})
	releaseAudit := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseAudit) }) })
	auditRows := make(chan store.AuditRow, 1)
	runStarted := make(chan struct{})

	id, err := m.Insert(&BgTaskSpec{
		ProjectID: "proj",
		ServerID:  "srv",
		Command:   "audit-order",
		Timeout:   time.Minute,
		AuditEnd: func(row store.AuditRow) error {
			close(auditEntered)
			<-releaseAudit
			auditRows <- row
			return nil
		},
		Run: func(ctx context.Context, task *bgTask) {
			<-runStarted
			m.runTask(ctx, task, client, func(context.Context, io.Writer, io.Writer) (int, bool, error) {
				return 0, false, nil
			}, nil)
		},
	})
	if err != nil {
		_ = client.Close()
		t.Fatal(err)
	}
	task, _ := m.lookup(id)

	type outputResult struct {
		view BgView
		ok   bool
		err  error
	}
	outputDone := make(chan outputResult, 1)
	go func() {
		view, ok, err := m.Output(id, 0, 0, 5*time.Second, context.Background())
		outputDone <- outputResult{view: view, ok: ok, err: err}
	}()
	waitParked(t, task, 1)
	close(runStarted)

	select {
	case <-auditEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("结束审计回调没有被调用")
	}

	// 再发起一次零等待读取。若任务互斥锁已在结束审计回调返回前释放，
	// 这一次读取会立即返回终态；正确顺序必须让读取等回调返回。
	probeDone := make(chan outputResult, 1)
	go func() {
		view, ok, err := m.Output(id, 0, 0, 0, context.Background())
		probeDone <- outputResult{view: view, ok: ok, err: err}
	}()
	var early *outputResult
	select {
	case result := <-probeDone:
		early = &result
	case <-time.After(500 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(releaseAudit) })

	var final outputResult
	if early != nil {
		final = *early
	} else {
		final = <-outputDone
	}
	if early != nil {
		t.Fatalf("审计回调阻塞期间长轮询提前返回：%+v", *early)
	}
	if final.err != nil || !final.ok {
		t.Fatalf("长轮询返回错误或未知任务：结果=%+v", final)
	}
	if final.view.Status != bgStatusDone {
		t.Fatalf("任务终态=%q，期望 done", final.view.Status)
	}
	select {
	case row := <-auditRows:
		if row.Action != "exec-bg-end" || row.Command != id || row.Status != "ok" {
			t.Fatalf("结束审计行=%+v，期望动作 exec-bg-end、任务标识=%q、状态 ok", row, id)
		}
	default:
		t.Fatal("长轮询返回终态时结束审计行不存在")
	}
}
