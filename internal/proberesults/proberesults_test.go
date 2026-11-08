package proberesults

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

func observationAt(origin, target, network, address string, port int, ok bool, category models.ProbeErrorCategory, observedAt time.Time, source string) Observation {
	return Observation{
		OriginDeviceID: origin, TargetDeviceID: target, Network: network,
		Address: address, Port: port, OK: ok, Category: category,
		Duration: 3 * time.Millisecond, ObservedAt: observedAt, Source: source,
	}
}

// TestFileProbeResultsKeepsLatestPerKey: 同一个「起点设备/目标设备/网络/
// 地址/端口」键只保留最新一条观察；任一键成员不同都是另一条记录。
func TestFileProbeResultsKeepsLatestPerKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe-results.json")
	results := NewFileProbeResults(path)
	first := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	second := time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC)
	if err := results.Record(observationAt("device-self", "device-nas", "家A局域网", "192.0.2.10", 445, false, models.ProbeErrorTimeout, first, "probe")); err != nil {
		t.Fatal(err)
	}
	if err := results.Record(observationAt("device-self", "device-nas", "家A局域网", "192.0.2.10", 445, true, models.ProbeErrorNone, second, "probe")); err != nil {
		t.Fatal(err)
	}
	// 端口不同是另一个键。
	if err := results.Record(observationAt("device-self", "device-nas", "家A局域网", "192.0.2.10", 22, false, models.ProbeErrorConnectionRefused, second, "dial")); err != nil {
		t.Fatal(err)
	}

	got, err := results.List("device-self", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("observations = %+v, want one record per key", got)
	}
	latestFound := false
	for _, item := range got {
		if item.Port != 445 {
			continue
		}
		if !item.OK || !item.ObservedAt.Equal(second) || item.Source != "probe" {
			t.Fatalf("port 445 observation = %+v, want the newest success record", item)
		}
		latestFound = true
	}
	if !latestFound {
		t.Fatalf("port 445 observation missing: %+v", got)
	}
}

// TestFileProbeResultsListFiltersByOriginAndTarget: 读取接口按起点设备过滤，
// 可选再按目标设备过滤，供后续拓扑查询工具使用。
func TestFileProbeResultsListFiltersByOriginAndTarget(t *testing.T) {
	results := NewFileProbeResults(filepath.Join(t.TempDir(), "probe-results.json"))
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	records := []Observation{
		observationAt("device-self", "device-nas", "家A局域网", "192.0.2.10", 445, true, models.ProbeErrorNone, now, "probe"),
		observationAt("device-self", "device-workstation", "NetBird", "100.64.10.5", 22, false, models.ProbeErrorTimeout, now, "probe"),
		observationAt("device-server", "device-nas", "家A局域网", "192.0.2.11", 445, true, models.ProbeErrorNone, now, "dial"),
	}
	for _, record := range records {
		if err := results.Record(record); err != nil {
			t.Fatal(record, err)
		}
	}

	fromSelf, err := results.List("device-self", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(fromSelf) != 2 {
		t.Fatalf("origin filter = %+v, want two observations", fromSelf)
	}
	fromSelfToNAS, err := results.List("device-self", "device-nas")
	if err != nil {
		t.Fatal(err)
	}
	if len(fromSelfToNAS) != 1 || fromSelfToNAS[0].TargetDeviceID != "device-nas" {
		t.Fatalf("origin+target filter = %+v, want only the NAS observation", fromSelfToNAS)
	}
}

// TestFileProbeResultsCleanupAndFileHygiene: 读写都会清理超过 7 天的观察；
// 文件带版本号、只给当前用户读写，并有独立锁文件，写法与成功地址缓存一致。
func TestFileProbeResultsCleanupAndFileHygiene(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "probe-results.json")
	results := NewFileProbeResults(path)
	old := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC)
	if err := results.Record(observationAt("device-self", "device-nas", "家A局域网", "192.0.2.10", 445, true, models.ProbeErrorNone, old, "probe")); err != nil {
		t.Fatal(err)
	}
	if err := results.Record(observationAt("device-self", "device-nas", "NetBird", "100.64.10.5", 445, true, models.ProbeErrorNone, fresh, "probe")); err != nil {
		t.Fatal(err)
	}

	got, err := results.List("device-self", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Network != "NetBird" {
		t.Fatalf("after cleanup = %+v, want only the fresh observation", got)
	}

	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(blob, &envelope); err != nil || envelope.Version != 1 {
		t.Fatalf("file %s must be version 1 JSON: %v %v", path, envelope.Version, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %o, want 0600", info.Mode().Perm())
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
}

// TestFileProbeResultsConcurrentWritesStayParseable: 多个写入者并发记录时，
// 文件锁保证最终文件仍是带版本号的完整 JSON。
func TestFileProbeResultsConcurrentWritesStayParseable(t *testing.T) {
	results := NewFileProbeResults(filepath.Join(t.TempDir(), "probe-results.json"))
	var group sync.WaitGroup
	for writer := 0; writer < 8; writer++ {
		group.Add(1)
		go func(writer int) {
			defer group.Done()
			address := "192.0.2.1" + string(rune('0'+writer))
			if err := results.Record(observationAt("device-self", "device-nas", "家A局域网", address, 445, true, models.ProbeErrorNone, time.Now(), "probe")); err != nil {
				t.Errorf("writer %d: %v", writer, err)
			}
		}(writer)
	}
	group.Wait()

	got, err := results.List("device-self", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8 {
		t.Fatalf("observations = %d, want eight intact records", len(got))
	}
}
