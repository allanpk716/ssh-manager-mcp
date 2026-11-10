package clientops

// 票 #203：客户端挂载共享存储前的快照保鲜。挂载会把登记信息与挂载密码
// 写进目标服务器，用旧快照操作可能写出旧密码；因此 mount_share 在真正
// 挂载前先向权威端查询快照修订号，本地落后就沿用现有的全量拉取函数同步。
// 查询或同步失败只返回错误：工具层继续用本地快照挂载，失败结果里附「快
// 照可能过期」的排查提示，网络抖动不挡住挂载。

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// MountShareSnapshotSyncer 按一台客户端实例的拉取凭据执行挂载前快照保鲜。
// 它实现 mcpserver.MountShareSnapshotSyncer：返回权威端当前快照修订号，
// 并保证返回 nil 错误时本地加密缓存已经达到该修订号。
type MountShareSnapshotSyncer struct {
	instance string
	cred     *CacheCred
}

// NewMountShareSnapshotSyncerFor 读取实例的拉取凭据并构造同步器。缺少凭据
// 或凭据不可读时同样构造成功（与元数据转发器一致，不因此拒绝桥启动），
// 挂载前保鲜在调用时报错并按「快照可能过期」处理。
func NewMountShareSnapshotSyncerFor(instance string) (*MountShareSnapshotSyncer, error) {
	cred, err := ReadCacheCredFor(instance)
	if err != nil {
		return &MountShareSnapshotSyncer{instance: instance}, nil
	}
	return &MountShareSnapshotSyncer{instance: instance, cred: cred}, nil
}

// SyncBeforeMount 确认本地缓存快照没有落后于权威端。修订号一致时只做轻量
// 查询；落后时先看共享缓存（同一台机器上别的桥可能已更新），仍不够新才
// 全量拉取。返回错误表示无法确认，不表示挂载必须停止。
func (s *MountShareSnapshotSyncer) SyncBeforeMount(ctx context.Context) (int64, error) {
	if s.cred == nil {
		return 0, fmt.Errorf("没有读取到本实例的拉取凭据，无法确认快照修订号")
	}
	code, pin := deviceCodeAndPin(*s.cred)
	if pin == "" {
		return 0, fmt.Errorf("拉取凭据没有固定证书，拒绝明文查询快照修订号")
	}
	remoteRevision, _, err := querySnapshotVersion(ctx, s.cred.URL, code, pin, LazyPullTimeout)
	if err != nil {
		return 0, err
	}
	localRevision, present, err := cachedSnapshotRevision(s.instance)
	if err != nil {
		return 0, err
	}
	if present && localRevision >= remoteRevision {
		return remoteRevision, nil
	}
	if _, err := DoPull(s.cred.URL, code, pin, PullOpts{
		Timeout: LazyPullTimeout, StatusOut: os.Stderr, Instance: s.instance, Context: ctx,
	}); err != nil {
		if errors.Is(err, ErrCacheQuarantined) {
			// 与到期保鲜同一语义：权威端销毁缓存材料后，本进程不再自动拉取。
			cacheQuarantinedFlag = true
		}
		return 0, err
	}
	syncedRevision, present, err := cachedSnapshotRevision(s.instance)
	if err != nil {
		return 0, err
	}
	if !present || syncedRevision < remoteRevision {
		return 0, fmt.Errorf("挂载前同步后缓存修订号仍落后（权威端 %d，本地 %d）", remoteRevision, syncedRevision)
	}
	return remoteRevision, nil
}
