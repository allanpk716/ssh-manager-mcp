package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"ssh-manager-mcp/internal/models"
)

// serverConnectionTestSchema 保存每个服务器条目最近一次连接测试的完成结
// 果。外键跟随条目删除，逐地址结果用结构化 JSON 保存，不保存任何凭据。
const serverConnectionTestSchema = `
CREATE TABLE IF NOT EXISTS server_connection_test_results (
  server_id TEXT PRIMARY KEY REFERENCES servers(id) ON DELETE CASCADE,
  completed_at INTEGER NOT NULL,
  succeeded INTEGER NOT NULL CHECK(succeeded IN (0,1)),
  failure_reason TEXT NOT NULL DEFAULT '',
  success_address TEXT NOT NULL DEFAULT '',
  success_network_name TEXT NOT NULL DEFAULT '',
  success_source TEXT NOT NULL DEFAULT '',
  duration_ns INTEGER NOT NULL DEFAULT 0,
  attempts_json TEXT NOT NULL DEFAULT '[]'
);
`

// SaveServerConnectionTestResult 整体覆盖一个服务器条目最近一次连接测试
// 结果。结果必须已经完成；进行中任务仍只留在 Web UI 进程内。
func (s *Store) SaveServerConnectionTestResult(
	serverID string, result models.ServerConnectionTestResult,
) error {
	if s.readOnly {
		return ErrReadOnly
	}
	attempts, err := json.Marshal(result.Attempts)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO server_connection_test_results(
			server_id,completed_at,succeeded,failure_reason,success_address,
			success_network_name,success_source,duration_ns,attempts_json
		) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(server_id) DO UPDATE SET
			completed_at=excluded.completed_at,
			succeeded=excluded.succeeded,
			failure_reason=excluded.failure_reason,
			success_address=excluded.success_address,
			success_network_name=excluded.success_network_name,
			success_source=excluded.success_source,
			duration_ns=excluded.duration_ns,
			attempts_json=excluded.attempts_json`,
		serverID, result.CompletedAt.Unix(), result.Succeeded, string(result.FailureReason),
		result.SuccessAddress, result.SuccessNetworkName, string(result.SuccessSource),
		int64(result.Duration), string(attempts),
	)
	return err
}

// ServerConnectionTestResult 读回一个服务器条目最近一次连接测试结果；从未
// 测试时返回 nil。
func (s *Store) ServerConnectionTestResult(serverID string) (*models.ServerConnectionTestResult, error) {
	var result models.ServerConnectionTestResult
	var completedAt, duration int64
	var failure, successSource, attemptsJSON string
	err := s.db.QueryRow(`
		SELECT completed_at,succeeded,failure_reason,success_address,
		       success_network_name,success_source,duration_ns,attempts_json
		FROM server_connection_test_results WHERE server_id=?`, serverID,
	).Scan(
		&completedAt, &result.Succeeded, &failure, &result.SuccessAddress,
		&result.SuccessNetworkName, &successSource, &duration, &attemptsJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result.CompletedAt = time.Unix(completedAt, 0)
	result.Duration = time.Duration(duration)
	result.FailureReason = models.ConnectionTestFailureReason(failure)
	result.SuccessSource = models.DialAddressSource(successSource)
	if err := json.Unmarshal([]byte(attemptsJSON), &result.Attempts); err != nil {
		return nil, err
	}
	return &result, nil
}
