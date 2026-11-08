package ops

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

type fakeRestartManager struct {
	mu        sync.Mutex
	calls     int
	reason    string
	onRestart func()
}

func (f *fakeRestartManager) Restart() error {
	f.mu.Lock()
	onRestart := f.onRestart
	defer f.mu.Unlock()
	if onRestart != nil {
		onRestart()
	}
	f.calls++
	if f.reason != "" {
		return errors.New(f.reason)
	}
	return nil
}

func (f *fakeRestartManager) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type recordingRestartAudit struct {
	mu       sync.Mutex
	entries  []recordedRestartAudit
	failNext bool
}

type recordedRestartAudit struct {
	source string
	ip     string
	entry  RestartAuditEntry
}

func (r *recordingRestartAudit) record(source Source, entry RestartAuditEntry) error {
	sourceName, sourceIP := source.AuditFields()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failNext {
		r.failNext = false
		return errors.New("audit disk is unavailable")
	}
	r.entries = append(r.entries, recordedRestartAudit{source: sourceName, ip: sourceIP, entry: entry})
	return nil
}

func (r *recordingRestartAudit) snapshot() []recordedRestartAudit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRestartAudit(nil), r.entries...)
}

func TestRestartServiceWritesRequestedAuditBeforeCompleteResponse(t *testing.T) {
	manager := &fakeRestartManager{}
	audit := &recordingRestartAudit{}
	service := NewRestartService(RestartDependencies{
		NewService: func() (ServiceRestarter, error) { return manager, nil },
		Audit:      audit.record,
	})

	responseComplete := false
	scheduled := false
	var restart func()
	err := service.Request(WebUISource("198.51.100.9"), func(action func()) {
		scheduled = true
		restart = action
	})
	if err != nil {
		t.Fatalf("请求重启失败：%v", err)
	}

	entries := audit.snapshot()
	if len(entries) != 1 || entries[0].entry.Action != "service.restart" ||
		entries[0].entry.Result != "requested" || !scheduled || responseComplete || manager.count() != 0 {
		t.Fatalf("响应前应先写已发起审计再登记动作，且不重启：已登记=%v 响应完成=%v 重启次数=%d 审计=%+v",
			scheduled, responseComplete, manager.count(), entries)
	}
	if entries[0].source != "web_ui" || entries[0].ip != "198.51.100.9" {
		t.Fatalf("重启审计来源不符：%+v", entries[0])
	}

	responseComplete = true
	restart()
	if manager.count() != 1 || len(audit.snapshot()) != 1 {
		t.Fatalf("响应后应发起一次重启且不重复写已发起审计：次数=%d 审计=%+v",
			manager.count(), audit.snapshot())
	}
}

func TestRestartServiceDoesNotScheduleRestartWhenRequestedAuditFails(t *testing.T) {
	manager := &fakeRestartManager{}
	audit := &recordingRestartAudit{failNext: true}
	service := NewRestartService(RestartDependencies{
		NewService: func() (ServiceRestarter, error) { return manager, nil },
		Audit:      audit.record,
	})
	scheduled := false

	err := service.Request(CLISource(), func(func()) { scheduled = true })
	if err == nil || !strings.Contains(err.Error(), "audit disk is unavailable") {
		t.Fatalf("审计写失败应返回错误，实际 %v", err)
	}
	if scheduled || manager.count() != 0 || len(audit.snapshot()) != 0 {
		t.Fatalf("审计写失败不应调度或重启：已调度=%v 次数=%d 审计=%+v",
			scheduled, manager.count(), audit.snapshot())
	}
}

func TestRestartServiceWritesFailedAuditWhenManagerRestartFails(t *testing.T) {
	manager := &fakeRestartManager{reason: "service manager rejected restart"}
	audit := &recordingRestartAudit{}
	service := NewRestartService(RestartDependencies{
		NewService: func() (ServiceRestarter, error) { return manager, nil },
		Audit:      audit.record,
	})
	responseComplete := false

	err := service.Request(WebUISource("192.0.2.31"), func(restart func()) {
		responseComplete = true
		restart()
	})
	if err != nil {
		t.Fatal(err)
	}
	entries := audit.snapshot()
	if !responseComplete || manager.count() != 1 || len(entries) != 2 {
		t.Fatalf("应先完成响应再重启并补第二条审计：响应完成=%v 次数=%d 审计=%+v",
			responseComplete, manager.count(), entries)
	}
	if entries[0].entry.Result != "requested" || entries[1].entry.Result != "failed" ||
		!strings.Contains(entries[1].entry.Detail, "service manager rejected restart") ||
		entries[1].source != "web_ui" || entries[1].ip != "192.0.2.31" {
		t.Fatalf("重启失败审计不符：%+v", entries)
	}
}

func TestRestartServiceWritesErrorAuditWhenManagerIsNotWired(t *testing.T) {
	audit := &recordingRestartAudit{}
	service := NewRestartService(RestartDependencies{Audit: audit.record})

	err := service.Request(CLISource(), func(restart func()) { restart() })
	if err != nil {
		t.Fatal(err)
	}
	entries := audit.snapshot()
	if len(entries) != 2 || entries[0].entry.Result != "requested" ||
		entries[1].entry.Result != "failed" || entries[1].entry.Detail == "" {
		t.Fatalf("缺少服务管理器也应先写已发起再写失败审计：%+v", entries)
	}
}
