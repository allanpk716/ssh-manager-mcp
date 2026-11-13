package sharemanage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sharemount"
	"ssh-manager-mcp/internal/store"
)

// CheckStatus 是一台服务器上一个共享存储的现场状态代号。
const (
	CheckStatusMounted    = "mounted"
	CheckStatusUnmounted  = "unmounted"
	CheckStatusUnchecked  = "unchecked"
	CheckStatusSkipped    = "skipped"
	checkConcurrencyLimit = 8
)

// CheckRequest 是一次共享存储现场探测请求。Share 接受共享名或标识符。
type CheckRequest struct {
	Share string
}

// CheckServerResult 是一台服务器的安全展示结论，不包含共享地址、挂载账号
// 或密码。ObservedAt 是本次观察完成的时间。
type CheckServerResult struct {
	ServerID   string
	ServerName string
	System     string
	Status     string
	Reason     string
	Notice     string
	Managed    bool
	Version    int64
	Restricted bool
	ObservedAt time.Time
}

// CheckReport 是一处共享存储在全部已勾选服务器上的现场探测报告。
type CheckReport struct {
	ShareName string
	Servers   []CheckServerResult
}

// Check 并发对已勾选的 Linux 与 macOS 服务器执行只读现场检测。Windows 只
// 返回不检测说明；本操作不写审计，也不改动远端。
func (m *Manager) Check(ctx context.Context, request CheckRequest) (*CheckReport, error) {
	if m == nil || m.store == nil {
		return nil, errManagerWithoutStore
	}
	share, err := m.sharedStorage(request.Share)
	if err != nil {
		return nil, err
	}
	results := make([]CheckServerResult, len(share.MountableServers))
	limits := make(chan struct{}, checkConcurrencyLimit)
	var wait sync.WaitGroup
	for index, view := range share.MountableServers {
		results[index] = CheckServerResult{
			ServerID: view.ID, ServerName: view.Name, ObservedAt: time.Now(),
		}
		if models.NormalizeShellType(string(view.ShellType)).IsWindows() {
			results[index].System = sharemount.CheckSystemWindows
			results[index].Status = CheckStatusSkipped
			results[index].Notice = "Windows 服务器不检测：sshmgr 不在其上留下持久改动，盘符由每条命令执行前注入。"
			continue
		}
		wait.Add(1)
		limits <- struct{}{}
		go func(index int, serverView models.SharedStorageServer) {
			defer func() { <-limits }()
			defer wait.Done()
			results[index] = m.checkServer(ctx, share, serverView)
		}(index, view)
	}
	wait.Wait()
	return &CheckReport{ShareName: share.Name, Servers: results}, nil
}

func (m *Manager) sharedStorage(identifier string) (*models.SharedStorage, error) {
	shares, err := m.store.ListSharedStorages(store.SharedStorageFilter{})
	if err != nil {
		return nil, err
	}
	key := strings.TrimSpace(identifier)
	for _, share := range shares {
		if share.ID == key || strings.EqualFold(share.Name, key) {
			return share, nil
		}
	}
	return nil, fmt.Errorf("%w：%s", store.ErrSharedStorageNotFound, key)
}

func (m *Manager) checkServer(
	ctx context.Context, share *models.SharedStorage, view models.SharedStorageServer,
) CheckServerResult {
	result := CheckServerResult{
		ServerID: view.ID, ServerName: view.Name, ObservedAt: time.Now(),
	}
	server, err := m.store.ResolveServer(view.ID)
	if err != nil || server == nil {
		result.Status = CheckStatusUnchecked
		result.Reason = "ssh-error"
		result.Notice = "读取服务器条目失败，无法发起现场检测。"
		return result
	}
	checked, err := m.mounter.Check(ctx, sharemount.CheckRequest{
		Server: server, Shares: []*models.SharedStorage{share},
	})
	if err != nil {
		result.Status = CheckStatusUnchecked
		result.Reason = checkErrorReason(err)
		result.Notice = checkErrorNotice(err)
		return result
	}
	result.System = checked.System
	result.Restricted = checked.Restricted
	if checked.Restricted {
		result.Notice = "服务器条目没有配置 sudo 凭据，本次为受限检测：只能查看挂载表，不能读取持久配置。"
	}
	if checked.System == sharemount.CheckSystemDarwin && !checked.AutoMasterPresent {
		resetNotice := "macOS 系统主配置已被重置；下次挂载会重建间接映射配置。"
		result.Notice = strings.TrimSpace(result.Notice)
		if result.Notice != "" {
			result.Notice += "；"
		}
		result.Notice += resetNotice
	}
	if len(checked.States) == 0 {
		result.Status = CheckStatusUnchecked
		result.Reason = "output-invalid"
		result.Notice = "现场检测没有返回这处共享存储的结论。"
		return result
	}
	state := checked.States[0]
	result.Managed = state.Managed
	result.Version = state.Version
	if state.Mounted {
		result.Status = CheckStatusMounted
	} else {
		result.Status = CheckStatusUnmounted
	}
	return result
}

func checkErrorReason(err error) string {
	var checkFailure *sharemount.CheckFailure
	switch {
	case errors.As(err, &checkFailure) && checkFailure.Reason == sharemount.CheckFailureUnsupportedSystem:
		return "unsupported-system"
	case errors.As(err, &checkFailure) && checkFailure.Reason == sharemount.CheckFailureSudo:
		return "sudo"
	case errors.Is(err, sharemount.ErrCheckOutputInvalid):
		return "output-invalid"
	default:
		return "ssh-error"
	}
}

func checkErrorNotice(err error) string {
	var checkFailure *sharemount.CheckFailure
	switch {
	case errors.As(err, &checkFailure) && checkFailure.Reason == sharemount.CheckFailureUnsupportedSystem:
		return "目标服务器操作系统不支持共享存储检测。"
	case errors.As(err, &checkFailure) && checkFailure.Reason == sharemount.CheckFailureSudo:
		return "sudo 提权没有完成，无法读取持久配置。"
	case errors.Is(err, sharemount.ErrCheckOutputInvalid):
		return "现场检测命令输出不合法，无法给出可靠结论。"
	default:
		return "连接服务器或执行现场检测命令失败。"
	}
}
