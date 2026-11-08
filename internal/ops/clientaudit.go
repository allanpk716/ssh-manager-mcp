package ops

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"ssh-manager-mcp/internal/clientcache"
	"ssh-manager-mcp/internal/store"
)

// clientAuditRecord 是客户端本机审计日志文件一行的磁盘格式。字段名与
// store.AuditRow.JSONMap 写入旁车日志时的字段保持一致；解析与写入分属两个
// 结构，避免以后无意中改坏其中一侧。
type clientAuditRecord struct {
	ID         int64  `json:"id"`
	TS         int64  `json:"ts"`
	ProjectID  string `json:"project_id"`
	ServerID   string `json:"server_id"`
	Action     string `json:"action"`
	Command    string `json:"command"`
	Sudo       bool   `json:"sudo"`
	Status     string `json:"status"`
	ExitCode   int    `json:"exit_code"`
	DurationMS int64  `json:"duration_ms"`
	Source     string `json:"source"`
	SourceIP   string `json:"source_ip"`
}

// ReadClientAudit 读取一个客户端实例旁边的本机审计日志文件，按记录时间顺序
// 倒序返回（最新在前）。instance 空串表示默认实例；limit 为 0 表示不限条
// 数，负数报错。文件不存在表示还没有本机审计记录，返回空结果；单行损坏只
// 跳过该行，不影响其余记录。
func ReadClientAudit(instance string, limit int) ([]store.AuditRow, error) {
	if limit < 0 {
		return nil, fmt.Errorf("limit must be >= 0")
	}
	_, _, _, auditPath, err := clientcache.PathsFor(instance)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(auditPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	rows := make([]store.AuditRow, 0)
	scanner := bufio.NewScanner(file)
	// 单行是一条完整审计记录；摘要可能包含较长命令文本，缓冲上限放宽到
	// 1MB，超过该长度的行按损坏处理。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record clientAuditRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		rows = append(rows, store.AuditRow{
			ID: record.ID, TS: time.Unix(record.TS, 0),
			ProjectID: record.ProjectID, ServerID: record.ServerID,
			Action: record.Action, Command: record.Command, Sudo: record.Sudo,
			Status: record.Status, ExitCode: record.ExitCode, DurationMS: record.DurationMS,
			Source: record.Source, SourceIP: record.SourceIP,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("读取客户端本机审计日志文件失败：%w", err)
	}
	// 日志按追加顺序写入（旧在前）；调用方需要最新记录在前。
	for left, right := 0, len(rows)-1; left < right; left, right = left+1, right-1 {
		rows[left], rows[right] = rows[right], rows[left]
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}
