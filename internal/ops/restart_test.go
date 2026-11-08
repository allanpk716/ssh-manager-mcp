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
	mu      sync.Mutex
	entries []recordedRestartAudit
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
	r.entries = append(r.entries, recordedRestartAudit{source: sourceName, ip: sourceIP, entry: entry})
	return nil
}

func (r *recordingRestartAudit) snapshot() []recordedRestartAudit {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedRestartAudit(nil), r.entries...)
}

func TestRestartServiceRunsOnlyFromResponseEndScheduler(t *testing.T) {
	manager := &fakeRestartManager{}
	audit := &recordingRestartAudit{}
	service := NewRestartService(RestartDependencies{
		NewService: func() (ServiceRestarter, error) { return manager, nil },
		Audit:      audit.record,
	})

	responseBody := "完整的「正在重启」响应"
	scheduled := false
	err := service.Request(WebUISource("198.51.100.9"), func(restart func()) {
		scheduled = true
		if manager.count() != 0 {
			t.Fatalf("调度前不应发起重启，实际调用次数=%d", manager.count())
		}
		if responseBody == "" {
			t.Fatal("响应体尚未写出时不应调度重启")
		}
		restart()
	})
	if err != nil {
		t.Fatalf("请求重启失败：%v", err)
	}
	if !scheduled || manager.count() != 1 {
		t.Fatalf("响应结束调度器应发起一次重启，已调度=%v 调用次数=%d", scheduled, manager.count())
	}

	entries := audit.snapshot()
	if len(entries) != 1 {
		t.Fatalf("应写一条重启审计，实际 %+v", entries)
	}
	got := entries[0]
	if got.entry.Action != "service.restart" || got.entry.Result != "ok" ||
		got.source != "web_ui" || got.ip != "198.51.100.9" {
		t.Fatalf("重启审计不符：%+v", got)
	}
}

func TestRestartServiceWritesSuccessAuditBeforeCallingManager(t *testing.T) {
	var events []string
	manager := &fakeRestartManager{}
	manager.onRestart = func() { events = append(events, "restart") }
	audit := &recordingRestartAudit{}
	service := NewRestartService(RestartDependencies{
		NewService: func() (ServiceRestarter, error) { return manager, nil },
		Audit: func(source Source, entry RestartAuditEntry) error {
			events = append(events, "audit")
			return audit.record(source, entry)
		},
	})

	err := service.Request(CLISource(), func(restart func()) {
		events = append(events, "response-complete")
		restart()
		events = append(events, "request-returned")
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"response-complete", "audit", "restart", "request-returned"}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("应先确保审计落盘再发起重启，事件=%v", events)
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
	if len(entries) != 1 || entries[0].entry.Result != "error" ||
		entries[0].entry.Action != "service.restart" || entries[0].entry.Detail == "" {
		t.Fatalf("缺少服务管理器也应写入失败审计：%+v", entries)
	}
}
