package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/ops"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/webui"
	"ssh-manager-mcp/internal/webuiops"
)

// uiUpdateService 构造回环 Web UI 进程内共享的升级服务。同一时间只允许一个
// 升级，因此服务在 `sshmgr ui` 启动时创建一次；客户端实例按每次请求传入。
func uiUpdateService(role webui.Role, startupInstance string) *ops.UpdateService {
	return ops.NewUpdateService(ops.UpdateDependencies{
		ClientState: func(requestedInstance string) (ops.UpdateClientState, error) {
			state, readErr := readClientVersionState(requestedInstance, io.Discard)
			if readErr != nil && state.IsClient && errors.Is(readErr, errAmbiguousClientCacheInstance) {
				return ops.UpdateClientState{IsClient: true}, &ops.InstanceAmbiguousError{Err: readErr}
			}
			recommended := ""
			if state.Snapshot != nil {
				recommended = state.Snapshot.RecommendedVersion
			}
			return ops.UpdateClientState{IsClient: state.IsClient, RecommendedVersion: recommended}, readErr
		},
		Audit: uiUpdateAudit(role, ""),
	})
}

// uiUpdateAudit 按角色选择升级审计的落点：单机写本机 vault 审计表；客户端
// 追加到该实例的审计日志文件（JSON 每行一条，与只读旁车文件同一形态）。
func uiUpdateAudit(role webui.Role, instance string) func(source ops.Source, entry ops.UpdateAuditEntry) error {
	if role == webui.RoleClient {
		return func(source ops.Source, entry ops.UpdateAuditEntry) error {
			target := entry.Instance
			if target == "" {
				target = instance
			}
			return writeClientUpdateAudit(target)(source, entry)
		}
	}
	return writeUpdateAudit
}

// writeClientUpdateAudit 把升级动作追加到客户端实例的审计日志文件。
func writeClientUpdateAudit(instance string) func(source ops.Source, entry ops.UpdateAuditEntry) error {
	return func(source ops.Source, entry ops.UpdateAuditEntry) error {
		_, _, _, auditPath, err := clientops.CachePathsFor(instance)
		if err != nil {
			return err
		}
		row := updateAuditRow(source, entry)
		data, err := json.Marshal(row.JSONMap())
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
			return fmt.Errorf("创建实例目录 %s: %w", filepath.Dir(auditPath), err)
		}
		file, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("打开实例审计日志 %s: %w", auditPath, err)
		}
		defer file.Close()
		if _, err := file.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("写入实例审计日志 %s: %w", auditPath, err)
		}
		return nil
	}
}

// updateAuditRow 组装 update.apply 的审计行；摘要只含旧版本、新版本、结果与
// 失败说明，不含任何令牌。
func updateAuditRow(source ops.Source, entry ops.UpdateAuditEntry) store.AuditRow {
	summary := map[string]any{
		"old_version": entry.OldVersion, "new_version": entry.NewVersion, "result": entry.Result,
	}
	if entry.Detail != "" {
		summary["detail"] = entry.Detail
	}
	row := store.OwnerAuditRow(entry.Action, summary)
	row.Source, row.SourceIP = source.AuditFields()
	if entry.Result != "ok" {
		row.Status = "error"
	}
	return row
}

// standaloneUIOpsStoreWithUpdates 给单机回环 Web UI 同时接上运维模块与升级服
// 务；vault 按请求打开，升级服务在进程内共享。
func standaloneUIOpsStoreWithUpdates(updates *ops.UpdateService) webui.OpsStore {
	return webuiops.FromOpenerWithUpdates(func() (*ops.Manager, func(), error) {
		st, err := openUnlockedStore()
		if err != nil {
			return nil, nil, err
		}
		return ops.New(st), func() { _ = st.Close() }, nil
	}, updates)
}
