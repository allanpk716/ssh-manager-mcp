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
		fmt.Fprintf(os.Stderr, "sshmgr: 读取本地 vault 快照修订号失败（跳过变更提示）: %v\n", err)
		return nil
	}
	if h.baseline != nil && h.baseline.SnapshotRevision == revision {
		return nil
	}

	snapshot, err := h.store.ExportSnapshotForProfile(h.profileID, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sshmgr: 导出本地 vault 快照失败（跳过变更提示）: %v\n", err)
		return nil
	}
	if h.baseline == nil {
		h.baseline = snapshot
		return nil
	}

	hint := h.snapshotComparer().Hint(h.baseline, snapshot)
	h.baseline = snapshot
	if hint == "" {
		return nil
	}
	return []string{hint}
}

func (h *localVaultChangeHints) snapshotComparer() *snapshotdiff.Comparer {
	if h.changes == nil {
		return snapshotdiff.Default()
	}
	return h.changes
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
