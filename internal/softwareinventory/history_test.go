package softwareinventory

import (
	"context"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
)

// replayInventoryOutput 生成一份全部节都有结论的固定命令集输出：系统与包
// 信息完整，监听端口属于 openssh-server，其余节声明为整节缺失（不算结
// 果不完整）。
func replayInventoryOutput(packages []string, listenAddress string) string {
	lines := []string{
		"SSHMGR-INVENTORY BEGIN posix-v2",
		"SSHMGR-INVENTORY SECTION os",
		"kernel-name=Linux",
		"file=/etc/os-release",
		"NAME=Ubuntu",
		"VERSION_ID=24.04",
		"ID=ubuntu",
		"kernel=6.8.0-1-generic",
		"machine=x86_64",
		"SSHMGR-INVENTORY STATUS os 0",
		"SSHMGR-INVENTORY SECTION packages",
	}
	lines = append(lines, packages...)
	lines = append(lines,
		"SSHMGR-INVENTORY STATUS packages 0",
		"SSHMGR-INVENTORY SECTION listen",
		"tcp LISTEN 0 5 "+listenAddress+":22 0.0.0.0:* users:((\"sshd\",pid=100,fd=3))",
		"SSHMGR-INVENTORY STATUS listen 0",
		"SSHMGR-INVENTORY ABSENT listen-netstat",
		"SSHMGR-INVENTORY ABSENT listen-lsof",
		"SSHMGR-INVENTORY SECTION exe",
		"100\t/usr/sbin/sshd",
		"SSHMGR-INVENTORY STATUS exe 0",
		"SSHMGR-INVENTORY SECTION exe-owner",
		"/usr/sbin/sshd\topenssh-server",
		"SSHMGR-INVENTORY STATUS exe-owner 0",
		"SSHMGR-INVENTORY ABSENT apps",
		"SSHMGR-INVENTORY ABSENT brew",
		"SSHMGR-INVENTORY ABSENT synology-packages",
		"SSHMGR-INVENTORY ABSENT container-images",
		"SSHMGR-INVENTORY ABSENT containers",
		"SSHMGR-INVENTORY ABSENT firmware",
		"SSHMGR-INVENTORY ABSENT gpu-driver",
		"SSHMGR-INVENTORY END",
	)
	joined := ""
	for _, line := range lines {
		joined += line + "\n"
	}
	return joined
}

// TestCollectWritesChangeHistoryAcrossRuns 走完整的解析与落库路径：两次成
// 功采集之间出现新增、移除、版本变化与暴露级别变化时，历史随第二次采集
// 的同一事务写入。
func TestCollectWritesChangeHistoryAcrossRuns(t *testing.T) {
	service, st := newCollectService(t)
	addCollectDevice(t, st, "工作站", "entry-a")
	firstAt := service.Now()
	secondAt := firstAt.Add(time.Hour)
	now := firstAt
	service.Now = func() time.Time { return now }
	st.NowFn = service.Now

	var recorded recordedExec
	service.RemoteExec = replayCollectExec(replayInventoryOutput([]string{
		"openssh-server\t1:9.6p1-1\tamd64\tii",
		"python3\t3.12.3-1\tamd64\tii",
	}, "0.0.0.0"), &recorded, nil)
	if _, err := service.Collect(context.Background(), CollectRequest{Source: CLISource(), DeviceName: "工作站"}); err != nil {
		t.Fatal(err)
	}

	now = secondAt
	service.RemoteExec = replayCollectExec(replayInventoryOutput([]string{
		"openssh-server\t1:9.9p1-1\tamd64\tii",
		"nginx\t1.24.0\tamd64\tii",
	}, "127.0.0.1"), &recorded, nil)
	if _, err := service.Collect(context.Background(), CollectRequest{Source: CLISource(), DeviceName: "工作站"}); err != nil {
		t.Fatal(err)
	}

	rows, err := st.SoftwareChangeHistoryForDevice("工作站")
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[models.SoftwareChangeKind]models.SoftwareChangeEntry{}
	sawEarlier := false
	for _, row := range rows {
		if !row.OccurredAt.Equal(secondAt) {
			sawEarlier = true
			continue
		}
		if sawEarlier {
			t.Fatalf("变化历史未按时间倒序排列：%+v", rows)
		}
		byKind[row.Kind] = row
	}
	if len(byKind) != 4 {
		t.Fatalf("第二次采集应产生四种变化历史：%+v", rows)
	}
	if row := byKind[models.SoftwareChangeVersionChanged]; row.Name != "openssh-server" ||
		row.OldValue != "1:9.6p1-1" || row.NewValue != "1:9.9p1-1" {
		t.Fatalf("版本变化历史 = %+v", row)
	}
	if row := byKind[models.SoftwareChangeExposureChanged]; row.Name != "openssh-server" ||
		row.OldValue != string(models.SoftwareExposureAllAddresses) ||
		row.NewValue != string(models.SoftwareExposureLocal) {
		t.Fatalf("暴露级别变化历史 = %+v", row)
	}
	if row := byKind[models.SoftwareChangeAdded]; row.Name != "nginx" || row.NewValue != "1.24.0" {
		t.Fatalf("新增历史 = %+v", row)
	}
	if row := byKind[models.SoftwareChangeRemoved]; row.Name != "python3" || row.OldValue != "3.12.3-1" {
		t.Fatalf("移除历史 = %+v", row)
	}
}
