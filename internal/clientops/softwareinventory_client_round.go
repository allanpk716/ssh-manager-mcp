package clientops

// 客户端实例的软件清单采集一轮。它只把本地缓存快照当作 SSH 凭据来源，不
// 把软件清单或采集状态写到客户端本地；每一台设备采集完立即在线上交。

import (
	"context"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/store"
)

// SoftwareInventoryClientCollector 把在线分派、客户端 SSH 采集与结果上交
// 组合成一个可注入的公开入口。命令行与 MCP 桥的新鲜度轮询共用这里。
type SoftwareInventoryClientCollector struct {
	Forwarder *SoftwareInventoryForwarder
	// OpenStore 导入客户端快照并返回可读取服务器条目与凭据的存储。
	OpenStore func() (*store.Store, func(), error)
	// NewService 为空时构造生产采集服务；测试注入回放执行函数。
	NewService func(*store.Store) *softwareinventory.Service
	// Now 为空时使用真实时钟，仅供输出报告。
	Now func() time.Time
}

// CollectAssigned 在线取本实例的分派，筛出到期设备，逐台采集并逐台上交。
// 取分派或任一次上交失败时立即返回错误；已成功上交的结果只在权威端，客
// 户端不排队、不留状态。
func (c *SoftwareInventoryClientCollector) CollectAssigned(ctx context.Context) (*softwareinventory.SchedulerRoundReport, error) {
	if c == nil || c.Forwarder == nil || c.OpenStore == nil {
		return nil, fmt.Errorf("客户端软件清单采集服务没有完整配置")
	}
	startedAt := c.now()
	assignments, err := c.Forwarder.SoftwareCollectionAssignments(ctx)
	if err != nil {
		return nil, err
	}
	due := make([]models.SoftwareCollectionAssignment, 0, len(assignments))
	for _, assignment := range assignments {
		if assignment.Due {
			due = append(due, assignment)
		}
	}
	report := &softwareinventory.SchedulerRoundReport{
		StartedAt: startedAt, Due: len(due),
		SkippedNotDue: len(assignments) - len(due),
		Devices:       make([]softwareinventory.SchedulerDeviceReport, 0, len(due)),
	}
	if len(due) == 0 {
		report.EndedAt = c.now()
		return report, nil
	}
	st, closeStore, err := c.OpenStore()
	if err != nil {
		return nil, err
	}
	defer closeStore()
	service := c.service(st)
	for _, assignment := range due {
		deviceReport := softwareinventory.SchedulerDeviceReport{
			DeviceName: assignment.DeviceName, PendingImmediate: assignment.PendingImmediate,
		}
		submission, collection, collectErr := service.CollectForSubmission(ctx, assignment)
		if collectErr != nil {
			submission = failedClientSubmission(assignment, collectErr.Error())
			deviceReport.Err = collectErr.Error()
		} else if submission.Failed {
			deviceReport.Err = submission.Error
		}
		if collection != nil {
			deviceReport.Report = collection
		}
		if submitErr := c.Forwarder.SubmitSoftwareCollectionResult(ctx, *submission); submitErr != nil {
			return nil, submitErr
		}
		if deviceReport.Err == "" {
			report.Succeeded++
		} else {
			report.Failed++
		}
		report.Devices = append(report.Devices, deviceReport)
	}
	report.EndedAt = c.now()
	return report, nil
}

func (c *SoftwareInventoryClientCollector) service(st *store.Store) *softwareinventory.Service {
	if c.NewService != nil {
		return c.NewService(st)
	}
	return &softwareinventory.Service{Store: st}
}

func (c *SoftwareInventoryClientCollector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func failedClientSubmission(
	assignment models.SoftwareCollectionAssignment, reason string,
) *softwareinventory.CollectionSubmission {
	return &softwareinventory.CollectionSubmission{
		DeviceID: assignment.DeviceID, DeviceName: assignment.DeviceName,
		ServerID: assignment.ServerID, Sudo: assignment.Sudo, Failed: true,
		Error: reason, Items: []models.SoftwareObservedItem{},
	}
}

// CollectSoftwareInventoryForInstance 是命令行计划任务与 MCP 桥顺手补采共
// 用的入口：先拿实例锁文件，再在线取分派、导入本地缓存快照、逐台采集并
// 上交。权威端不可达或锁忙时直接返回错误。
func CollectSoftwareInventoryForInstance(
	ctx context.Context, instance string,
) (*softwareinventory.SchedulerRoundReport, error) {
	lockPath, err := SoftwareInventoryCollectionLockPath(instance)
	if err != nil {
		return nil, err
	}
	lock, err := AcquireSoftwareInventoryCollectionLock(lockPath)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	credential, err := ReadCacheCredFor(instance)
	if err != nil {
		return nil, err
	}
	if credential == nil {
		credential = &CacheCred{}
	}
	forwarder, err := NewSoftwareInventoryForwarder(*credential)
	if err != nil {
		return nil, err
	}
	defer forwarder.Close()
	snapshot, err := LoadCacheSnapshotFor(instance)
	if err != nil {
		return nil, err
	}
	temporary, err := store.OpenTemporaryClientCacheStore(snapshot, "sshmgr-inventory-collect-*.db")
	if err != nil {
		return nil, err
	}
	defer func() { _ = temporary.Close() }()
	collector := &SoftwareInventoryClientCollector{
		Forwarder: forwarder,
		OpenStore: func() (*store.Store, func(), error) {
			return temporary.Store, func() {}, nil
		},
	}
	return collector.CollectAssigned(ctx)
}
