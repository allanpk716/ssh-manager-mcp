// Package probe 提供与界面无关的连通探测服务：从本机或某台受管服务器出
// 发，按拓扑登记的共同网络逐个探测目标设备地址的传输控制协议端口连通与
// 建连耗时，并把观察写进本地探测结果文件。MCP 工具、命令行子命令与将来
// 的 Web UI 按钮都只调用这里的入口。
package probe

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"ssh-manager-mcp/internal/dialdevice"
	"ssh-manager-mcp/internal/dialplan"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/proberesults"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// 固定限制来自规格：目标最多 10 个、最多 5 个目标并发、整次调用总时限
// 30 秒。
const (
	MaxTargets           = 10
	MaxConcurrency       = 5
	DefaultTotalDeadline = 30 * time.Second
	remoteProbeMaxOutput = int64(64 * 1024)
)

// Request 是一次连通探测的输入。From 是 self 或服务器编号；Targets 里每
// 项是服务器编号或设备名，可带「:端口」后缀。
type Request struct {
	From    string
	Targets []string
	// Source 写入探测结果文件的来源标记（固定为 probe）。
	Source string
}

// LocalDialFunc 是本机 TCP 拨号接缝：按地址、端口与逐地址超时建连，返回
// 建连耗时或错误。
type LocalDialFunc func(ctx context.Context, address string, port int, timeout time.Duration) (time.Duration, error)

// RemoteExecFunc 是在受管服务器上执行固定命令的接缝：返回标准输出或错误。
type RemoteExecFunc func(ctx context.Context, server *models.Server, shell models.ShellType, command string) (string, error)

// Service 是连通探测服务。测试可替换 LocalDial 与 RemoteExec；生产路径使
// 用带超时的真实 TCP 建连与按服务器条目拨号后的一次会话执行。
type Service struct {
	Store    *store.Store
	Instance string
	// GateServer 是服务器编号授权检查（由调用面注入，空值表示 owner 命令
	// 行路径不限制授权范围）。
	GateServer func(serverID string) error
	// Results 是本地探测结果文件；空值表示当前角色没有可写位置。
	Results *proberesults.FileProbeResults
	// HostKeys 是经服务器探测时使用的主机密钥存储；空值使用存储本身。
	HostKeys sshbroker.HostKeyStore
	// LocalDial 与 RemoteExec 是可注入接缝；空值使用生产实现。
	LocalDial  LocalDialFunc
	RemoteExec RemoteExecFunc
	// TotalDeadline 覆盖整次调用总时限；零值使用规格默认值。
	TotalDeadline time.Duration
	// Now 提供观察时间；零值使用 time.Now。
	Now func() time.Time
}

// AddressReport 是一个目标地址的探测结论。地址本身只在内部使用，呈现与
// 审计都不暴露。
type AddressReport struct {
	NetworkName string
	Sequence    int
	address     string
	physical    bool
	OK          bool
	Category    models.ProbeErrorCategory
	Duration    time.Duration
	ObservedAt  time.Time
}

// TargetReport 是一个目标的分组结论。
type TargetReport struct {
	Label string
	Port  int
	// DeviceID 是目标设备标识，供写探测结果使用。
	DeviceID string
	// NoCommonNetwork 表示起点与目标没有共同网络，未探测。
	NoCommonNetwork bool
	// UnprobedNote 表示起点服务器连接失败等原因导致未探测。
	UnprobedNote string
	Lines        []AddressReport
	Notes        []string
}

// Report 是一次探测的完整结论。
type Report struct {
	From       string
	FromDevice string
	Remote     bool
	// OriginError 非空时说明经服务器起点的固定命令会话没有建立。
	OriginError string
	Targets     []TargetReport

	fromDeviceID string
	source       string
}

// Probe 解析起点与目标、规划共同网络地址、执行探测并写本地探测结果文件。
// 参数或授权错误整次返回；单个目标不通不算失败。
func (s *Service) Probe(parent context.Context, request Request) (*Report, error) {
	if s == nil || s.Store == nil {
		return nil, fmt.Errorf("连通探测服务没有绑定存储")
	}
	if len(request.Targets) == 0 {
		return nil, fmt.Errorf("targets 不能为空")
	}
	addressTimeout, err := sshbroker.ResolveAddressTimeout()
	if err != nil {
		return nil, err
	}
	totalDeadline := s.TotalDeadline
	if totalDeadline <= 0 {
		totalDeadline = DefaultTotalDeadline
	}
	ctx, cancel := context.WithTimeout(parent, totalDeadline)
	defer cancel()

	origin, err := s.resolveOrigin(request.From)
	if err != nil {
		return nil, err
	}
	targets, err := s.resolveTargets(request.Targets)
	if err != nil {
		return nil, err
	}
	source := request.Source
	if source == "" {
		source = proberesults.SourceProbe
	}
	report := &Report{From: request.From, FromDevice: origin.device.Name, Remote: origin.remote, fromDeviceID: origin.device.ID, source: source}
	originMemberships, err := s.Store.ListNetworkMembershipsByRegistration(origin.device.Name)
	if err != nil {
		return nil, err
	}
	for index := range targets {
		target := &targets[index]
		report.Targets = append(report.Targets, TargetReport{
			Label: target.label, Port: target.port, DeviceID: target.device.ID,
		})
		memberships, err := s.Store.ListNetworkMembershipsByRegistration(target.device.Name)
		if err != nil {
			return nil, err
		}
		plan := sshbroker.PlanCommonNetworkAddresses(memberships, originMemberships)
		if len(plan.Candidates) == 0 {
			report.Targets[index].NoCommonNetwork = true
			continue
		}
		for _, candidate := range plan.Candidates {
			report.Targets[index].Lines = append(report.Targets[index].Lines, AddressReport{
				NetworkName: candidate.NetworkName, Sequence: candidate.NetworkSequence,
				address: candidate.Address, physical: candidate.Source == sshbroker.DialAddressPhysical,
			})
		}
	}

	if origin.remote {
		s.probeRemote(ctx, origin.server, report, addressTimeout)
	} else {
		s.probeLocal(ctx, report, addressTimeout)
	}
	for index := range report.Targets {
		annotateTarget(&report.Targets[index])
	}
	return report, nil
}

// origin 是解析后的探测起点。
type origin struct {
	device *models.Device
	server *models.Server
	remote bool
}

func (s *Service) resolveOrigin(from string) (*origin, error) {
	if from == "self" {
		current, err := dialdevice.Current(s.Store, s.Instance)
		if err != nil {
			return nil, err
		}
		if !current.Known || current.Device == nil {
			return nil, fmt.Errorf("本机未登记设备，请 owner 补登记（sshmgr devices local set）")
		}
		return &origin{device: current.Device}, nil
	}
	if s.GateServer != nil {
		if err := s.GateServer(from); err != nil {
			return nil, err
		}
	}
	server, err := s.Store.GetServer(from)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("服务器 %s 不存在", from)
	}
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, fmt.Errorf("服务器 %s 不存在", from)
	}
	if server.DeviceID == "" {
		return nil, fmt.Errorf("起点服务器未关联拓扑设备，请 owner 补登记")
	}
	device, err := deviceByID(s.Store, server.DeviceID)
	if err != nil {
		return nil, err
	}
	if device == nil {
		return nil, fmt.Errorf("起点服务器 %s 关联的拓扑设备已不存在，请 owner 补登记", from)
	}
	return &origin{device: device, server: server, remote: true}, nil
}

// resolvedTarget 是解析后的探测目标。
type resolvedTarget struct {
	label  string
	port   int
	device *models.Device
}

func (s *Service) resolveTargets(raw []string) ([]resolvedTarget, error) {
	seen := make(map[string]bool, len(raw))
	out := make([]resolvedTarget, 0, len(raw))
	for _, item := range raw {
		base := item
		port := 0
		explicit := false
		if position := strings.LastIndex(item, ":"); position > 0 {
			parsed, err := strconv.Atoi(item[position+1:])
			if err == nil {
				base, port, explicit = item[:position], parsed, true
			}
		}
		if explicit && (port < 1 || port > 65535) {
			return nil, fmt.Errorf("目标 %s 的端口超出范围（1-65535）", item)
		}
		var target resolvedTarget
		server, serverErr := s.Store.GetServer(base)
		if serverErr != nil && !errors.Is(serverErr, sql.ErrNoRows) {
			return nil, serverErr
		}
		if server != nil {
			if s.GateServer != nil {
				if err := s.GateServer(base); err != nil {
					return nil, err
				}
			}
			if server.DeviceID == "" {
				return nil, fmt.Errorf("目标服务器 %s 未关联拓扑设备，请 owner 补登记", base)
			}
			device, err := deviceByID(s.Store, server.DeviceID)
			if err != nil {
				return nil, err
			}
			if device == nil {
				return nil, fmt.Errorf("目标服务器 %s 关联的拓扑设备已不存在，请 owner 补登记", base)
			}
			target = resolvedTarget{label: base, device: device}
			if !explicit {
				target.port = server.Port
			}
		} else {
			device, deviceErr := s.Store.GetDevice(base)
			if deviceErr != nil && !errors.Is(deviceErr, sql.ErrNoRows) && !errors.Is(deviceErr, store.ErrDeviceNotFound) {
				return nil, deviceErr
			}
			if device == nil {
				return nil, fmt.Errorf("拓扑中没有这台设备：%s", base)
			}
			target = resolvedTarget{label: base, device: device}
			if !explicit {
				target.port = 22
			}
		}
		if explicit {
			target.port = port
		}
		key := target.label + ":" + strconv.Itoa(target.port)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, target)
	}
	if len(out) > MaxTargets {
		return nil, fmt.Errorf("目标最多 %d 个（去重后），请分批调用", MaxTargets)
	}
	return out, nil
}

func (s *Service) probeLocal(ctx context.Context, report *Report, addressTimeout time.Duration) {
	limiter := make(chan struct{}, MaxConcurrency)
	var group sync.WaitGroup
	for index := range report.Targets {
		target := &report.Targets[index]
		if target.NoCommonNetwork {
			continue
		}
		group.Add(1)
		go func() {
			defer group.Done()
			limiter <- struct{}{}
			defer func() { <-limiter }()
			s.probeLocalTarget(ctx, report, target, addressTimeout)
		}()
	}
	group.Wait()
}

func (s *Service) probeLocalTarget(ctx context.Context, report *Report, target *TargetReport, addressTimeout time.Duration) {
	for index := range target.Lines {
		if ctx.Err() != nil {
			target.Lines[index].Category = models.ProbeErrorIncomplete
			continue
		}
		line := &target.Lines[index]
		duration, err := s.localDial()(ctx, line.address, target.Port, addressTimeout)
		line.OK = err == nil
		line.Category = sshbroker.ClassifyProbeDialError(err)
		// 总时限取消打断了这次拨号：没有真实结论，不能照搬拨号错误当成
		// 连接超时，也不写探测结果文件。
		if err != nil && ctx.Err() != nil {
			line.OK = false
			line.Category = models.ProbeErrorIncomplete
		}
		line.Duration = duration
		line.ObservedAt = s.now()
		s.record(report, target, *line)
	}
}

func (s *Service) probeRemote(ctx context.Context, server *models.Server, report *Report, addressTimeout time.Duration) {
	type remoteItem struct {
		targetIndex int
		lineIndex   int
	}
	var items []ShellCommandItem
	var mapping []remoteItem
	for targetIndex := range report.Targets {
		target := &report.Targets[targetIndex]
		if target.NoCommonNetwork {
			continue
		}
		for lineIndex := range target.Lines {
			line := &target.Lines[lineIndex]
			if err := validateProbeAddress(line.address); err != nil {
				line.OK = false
				line.Category = models.ProbeErrorOther
				line.ObservedAt = s.now()
				s.record(report, target, *line)
				continue
			}
			items = append(items, ShellCommandItem{Address: line.address, Port: target.Port, Timeout: addressTimeout})
			mapping = append(mapping, remoteItem{targetIndex: targetIndex, lineIndex: lineIndex})
		}
	}
	if len(items) == 0 {
		return
	}
	shell := models.NormalizeShellType(string(server.ShellType))
	command, err := BuildShellProbeCommand(shell, items)
	if err != nil {
		report.OriginError = err.Error()
		markUnprobed(report, "未探测（固定命令生成失败）")
		return
	}
	output, err := s.remoteExec()(ctx, server, shell, command)
	if err != nil {
		report.OriginError = err.Error()
		markUnprobed(report, "未探测（无法连接起点服务器）")
		return
	}
	results := ParseShellProbeOutput(output, len(items))
	for index, result := range results {
		item := mapping[index]
		line := &report.Targets[item.targetIndex].Lines[item.lineIndex]
		line.OK = result.OK
		line.Category = result.Category
		line.Duration = result.Duration
		line.ObservedAt = s.now()
		s.record(report, &report.Targets[item.targetIndex], *line)
	}
}

func markUnprobed(report *Report, note string) {
	for index := range report.Targets {
		target := &report.Targets[index]
		if !target.NoCommonNetwork {
			target.UnprobedNote = note
		}
	}
}

func (s *Service) record(report *Report, target *TargetReport, line AddressReport) {
	if line.Category == models.ProbeErrorIncomplete {
		return
	}
	if s.Results == nil || target.DeviceID == "" || line.NetworkName == "" || line.address == "" {
		return
	}
	_ = s.Results.Record(proberesults.Observation{
		OriginDeviceID: report.FromDeviceID(), TargetDeviceID: target.DeviceID,
		Network: line.NetworkName, Address: line.address, Port: target.Port,
		OK: line.OK, Category: line.Category, Duration: line.Duration,
		ObservedAt: line.ObservedAt, Source: report.source,
	})
}

// FromDeviceID 返回起点设备标识，供调用面写审计摘要。
func (r *Report) FromDeviceID() string { return r.fromDeviceID }

// SetFromDeviceID 由服务内部写入起点设备标识。
func (r *Report) SetFromDeviceID(id string) { r.fromDeviceID = id }

func (s *Service) localDial() LocalDialFunc {
	if s.LocalDial != nil {
		return s.LocalDial
	}
	return realLocalDial
}

func (s *Service) remoteExec() RemoteExecFunc {
	if s.RemoteExec != nil {
		return s.RemoteExec
	}
	return s.defaultRemoteExec
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// realLocalDial 是生产环境使用的本机 TCP 拨号：超时内完成建连即算连通。
func realLocalDial(ctx context.Context, address string, port int, timeout time.Duration) (time.Duration, error) {
	started := time.Now()
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(address, strconv.Itoa(port)))
	if err != nil {
		return time.Since(started), err
	}
	defer conn.Close()
	return time.Since(started), nil
}

func (s *Service) defaultRemoteExec(ctx context.Context, server *models.Server, shell models.ShellType, command string) (string, error) {
	auth, err := vault.AuthForServer(s.Store, server)
	if err != nil {
		return "", fmt.Errorf("resolve credential: %w", err)
	}
	dialSource := dialplan.NewStoreSource(s.Store, s.Instance)
	hostKeys := sshbroker.HostKeyStore(s.Store)
	if s.HostKeys != nil {
		hostKeys = s.HostKeys
	}
	cli, _, err := sshbroker.ConnectServerEntry(ctx, sshbroker.ServerDialRequest{
		Server: server, Auth: auth, HostKeys: hostKeys,
		SuccessCache: dialSource, Topology: dialSource,
	})
	if err != nil {
		return "", err
	}
	defer cli.Close()
	result, err := cli.Exec(ctx, command, time.Until(ctxDeadline(ctx)), remoteProbeMaxOutput)
	if err != nil {
		return "", err
	}
	if result.TimedOut {
		return result.Stdout, fmt.Errorf("固定命令执行超时")
	}
	return result.Stdout, nil
}

func ctxDeadline(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Now().Add(DefaultTotalDeadline)
}

func deviceByID(st *store.Store, id string) (*models.Device, error) {
	devices, err := st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	for _, device := range devices {
		if device.ID == id {
			return device, nil
		}
	}
	return nil, nil
}

// annotateTarget 给一个目标补充「登记与实际不一致」与物理局域网绕行说明。
func annotateTarget(target *TargetReport) {
	if target.NoCommonNetwork || target.UnprobedNote != "" {
		return
	}
	type networkSummary struct {
		name     string
		physical bool
		allFail  bool
		anyOK    bool
		reason   models.ProbeErrorCategory
	}
	byNetwork := map[string]*networkSummary{}
	var order []string
	incomplete := false
	for _, line := range target.Lines {
		if line.Category == models.ProbeErrorIncomplete {
			incomplete = true
			continue
		}
		summary, present := byNetwork[line.NetworkName]
		if !present {
			summary = &networkSummary{name: line.NetworkName, physical: line.physical, allFail: true}
			byNetwork[line.NetworkName] = summary
			order = append(order, line.NetworkName)
		}
		if line.OK {
			summary.anyOK = true
			summary.allFail = false
		} else if summary.reason == "" {
			summary.reason = line.Category
		}
	}
	physicalAllFail := false
	for _, name := range order {
		summary := byNetwork[name]
		if summary.physical && summary.allFail {
			physicalAllFail = true
			target.Notes = append(target.Notes, fmt.Sprintf("登记与实际不一致：%s（%s）", name, CategoryLabel(summary.reason)))
		}
	}
	if physicalAllFail {
		for _, name := range order {
			if byNetwork[name].physical || !byNetwork[name].anyOK {
				continue
			}
			target.Notes = append(target.Notes, fmt.Sprintf("物理局域网不通，实际可经 %s 到达", name))
			break
		}
	}
	if incomplete {
		target.Notes = append(target.Notes, "探测未完成")
	}
}
