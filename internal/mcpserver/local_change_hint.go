package mcpserver

import (
	"fmt"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/snapshotdiff"
	"ssh-manager-mcp/internal/store"
)

// localVaultChangeHints 在单机部署的每次工具调用后读取本地 vault 的快照
// 修订号。第一次调用只保存当前 profile 的快照基线；之后修订号变化时重新
// 导出同一 profile 的快照，并复用多机路径的同一个比较环节生成变更提示。
type localVaultChangeHints struct {
	store     *store.Store
	profileID string
	changes   *snapshotdiff.Comparer

	mu       sync.Mutex
	baseline *store.Snapshot
}

// notices 返回本次需要交给会话提示通道的变更提示。读库或导出失败时保留
// 旧基线并返回空提示，下一次工具调用会重新尝试，不影响原工具结果。
func (h *localVaultChangeHints) notices() []string {
	if h == nil || h.store == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	revision, err := h.store.SnapshotRevision()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr: read local vault snapshot revision (skipping change hints): %v\n", err)
		return nil
	}
	if h.baseline != nil && h.baseline.SnapshotRevision == revision {
		return nil
	}

	// 单机部署的当前程序认识机密项段,与声明了客户端版本的多机客户端
	// 一样导出;这样才能对机密项变化给出同样的变更提示。
	snapshot, err := h.store.ExportSnapshotForProfile(h.profileID, true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr: export local vault snapshot (skipping change hints): %v\n", err)
		return nil
	}
	if h.baseline == nil {
		h.baseline = snapshot
		return nil
	}

	hint := effectiveSnapshotComparer(h.changes).Hint(h.baseline, snapshot)
	h.baseline = snapshot
	if hint == "" {
		return nil
	}
	return []string{hint}
}

// effectiveSnapshotComparer 返回要使用的快照比较器;调用方没有注入比较器时
// 退回默认比较器,让测试路径与生产路径比较同一组配置差异。
func effectiveSnapshotComparer(changes *snapshotdiff.Comparer) *snapshotdiff.Comparer {
	if changes == nil {
		return snapshotdiff.Default()
	}
	return changes
}

// newLocalVaultServer 构造直接读取本地 vault 的单机工具面，并只挂本地
// 变更提示这一个提示来源。这里不启动快照轮询定时器。
func newLocalVaultServer(st *store.Store, profileID, projectID string) (*mcp.Server, *TunnelManager, *TaskManager, error) {
	hints := &localVaultChangeHints{store: st, profileID: profileID, changes: snapshotdiff.Default()}
	return NewServerFromSourceWithHints(
		func() *store.Store { return st },
		profileID,
		projectID,
		nil,
		nil,
		hints.notices,
	)
}
