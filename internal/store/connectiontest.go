package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
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
  hint TEXT NOT NULL DEFAULT '',
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
			success_network_name,success_source,duration_ns,attempts_json,hint
		) VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(server_id) DO UPDATE SET
			completed_at=excluded.completed_at,
			succeeded=excluded.succeeded,
			failure_reason=excluded.failure_reason,
			hint=excluded.hint,
			success_address=excluded.success_address,
			success_network_name=excluded.success_network_name,
			success_source=excluded.success_source,
			duration_ns=excluded.duration_ns,
			attempts_json=excluded.attempts_json`,
		serverID, result.CompletedAt.Unix(), result.Succeeded, string(result.FailureReason),
		result.SuccessAddress, result.SuccessNetworkName, string(result.SuccessSource),
		int64(result.Duration), string(attempts), result.Hint,
	)
	return err
}

// ServerConnectionTestResult 读回一个服务器条目最近一次连接测试结果；从未
// 测试时返回 nil。
func (s *Store) ServerConnectionTestResult(serverID string) (*models.ServerConnectionTestResult, error) {
	var row serverConnectionTestResultRow
	err := s.db.QueryRow(`
		SELECT completed_at,succeeded,failure_reason,success_address,
		       success_network_name,success_source,duration_ns,attempts_json,hint
		FROM server_connection_test_results WHERE server_id=?`, serverID,
	).Scan(row.scanTargets()...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return row.result()
}

// ServerConnectionTestResults 一次读取多个服务器条目的最近一次连接测试
// 结果；从未测试的条目不出现在返回值里。服务器列表用这个批量入口组装
// 卡片，不逐条查询。
func (s *Store) ServerConnectionTestResults(
	serverIDs []string,
) (map[string]*models.ServerConnectionTestResult, error) {
	results := make(map[string]*models.ServerConnectionTestResult, len(serverIDs))
	if len(serverIDs) == 0 {
		return results, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(serverIDs)), ",")
	args := make([]any, 0, len(serverIDs))
	for _, serverID := range serverIDs {
		args = append(args, serverID)
	}
	rows, err := s.db.Query(`
		SELECT server_id,completed_at,succeeded,failure_reason,success_address,
		       success_network_name,success_source,duration_ns,attempts_json,hint
		FROM server_connection_test_results WHERE server_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var serverID string
		var row serverConnectionTestResultRow
		if err := rows.Scan(append([]any{&serverID}, row.scanTargets()...)...); err != nil {
			return nil, err
		}
		result, err := row.result()
		if err != nil {
			return nil, err
		}
		results[serverID] = result
	}
	return results, rows.Err()
}

// serverConnectionTestResultRow 是连接测试结果表一行的原始扫描值；单条
// 与批量读取共用它，再统一换成结构化结果。
type serverConnectionTestResultRow struct {
	completedAt    int64
	succeeded      bool
	failureReason  string
	hint           string
	successAddress string
	successNetwork string
	successSource  string
	duration       int64
	attemptsJSON   string
}

func (r *serverConnectionTestResultRow) scanTargets() []any {
	return []any{
		&r.completedAt, &r.succeeded, &r.failureReason, &r.successAddress,
		&r.successNetwork, &r.successSource, &r.duration, &r.attemptsJSON, &r.hint,
	}
}

func (r serverConnectionTestResultRow) result() (*models.ServerConnectionTestResult, error) {
	result := &models.ServerConnectionTestResult{
		CompletedAt: time.Unix(r.completedAt, 0), Succeeded: r.succeeded,
		FailureReason:  models.ConnectionTestFailureReason(r.failureReason),
		Hint:           r.hint,
		SuccessAddress: r.successAddress, SuccessNetworkName: r.successNetwork,
		SuccessSource: models.DialAddressSource(r.successSource),
		Duration:      time.Duration(r.duration),
	}
	if err := json.Unmarshal([]byte(r.attemptsJSON), &result.Attempts); err != nil {
		return nil, err
	}
	return result, nil
}
