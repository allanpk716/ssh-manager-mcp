// Package ops 提供与界面无关的「运维」模块：备份、设置、角色、升级与审计
// 查询的公共业务入口统一收拢在这里，命令行和 Web UI 共用同一套规则与审
// 计写入。本文件只放模块构造与来源；具体操作按领域分文件。
package ops

import (
	"os"
	"time"

	"ssh-manager-mcp/internal/store"
)

// Manager 是运维模块的入口，内部只调用存储层与各领域服务的现有函数，不改
// 变它们的语义。
type Manager struct {
	st *store.Store
	// settingInputs 是 serve 进程显式注入的环境变量与启动旗标，供设置
	// 项判断是否被覆盖。
	settingInputs SettingInputs
	// hostname 供设置页计算发现名称默认值；测试可注入固定值。
	hostname func() string
	// 下面三个接缝供测试替换：备份配置位置、计划任务探测与时钟。
	backupConfigPath    func() (string, error)
	backupScheduleProbe func() (installed, probed bool)
	now                 func() time.Time
}

// New 返回挂在一个已解锁存储上的运维模块。
func New(st *store.Store) *Manager {
	return newManager(st)
}

// newManager 统一填好各领域的默认接缝；不同构造入口共享同一份默认值，
// 新增接缝时不会漏掉某个入口。
func newManager(st *store.Store) *Manager {
	return &Manager{
		st:                  st,
		backupConfigPath:    defaultBackupConfigPath,
		backupScheduleProbe: defaultBackupScheduleProbe,
		now:                 time.Now,
		hostname: func() string {
			host, _ := os.Hostname()
			return host
		},
	}
}

// Source 描述一次运维写操作从哪里来：命令行，或 Web UI（附来源地址）。模
// 块按它给审计行填来源列。
type Source struct {
	kind string
	addr string
}

// CLISource 返回命令行来源。
func CLISource() Source {
	return Source{kind: store.AuditSourceCLI}
}

// WebUISource 返回 Web UI 来源，addr 是这次请求的来源地址。
func WebUISource(addr string) Source {
	return Source{kind: store.AuditSourceWebUI, addr: addr}
}

// ownerAuditRow 构造 owner 审计行；failed 决定状态列取 ok 还是 error。
func ownerAuditRow(action string, summary map[string]any, failed bool, src Source) store.AuditRow {
	row := store.OwnerAuditRow(action, summary)
	row.Source, row.SourceIP = src.AuditFields()
	if failed {
		row.Status = "error"
	}
	return row
}

// writeOwnerAudit 是运维模块写 vault 审计表的唯一私有入口，保证各领域动
// 作的来源字段使用同一份换算。
func writeOwnerAudit(st *store.Store, action string, summary map[string]any, failed bool, src Source) error {
	return st.WriteAudit(ownerAuditRow(action, summary, failed, src))
}
