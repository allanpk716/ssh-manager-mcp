package mcpserver

// exec_background / exec_output / exec_stop 的 ForProfile 包装 (Plan 32 T6+T7)。
// exec_background 门链与分支状态逐分支镜像 ExecCommandForProfile (core.go):
// denied / not found / no_credential / auth_error / hostkey_mismatch / connect
// 三态 (connect_error / cancelled / hostkey_mismatch) / no_sudo / 超限 / error /
// ok (spec §4)。
//
// 与前台的三处结构性差别:
//   - 连接与入表交给 TaskManager.Start (Reserve→ConnectKeepAlive→Insert, spec §1
//     槽位预约 admission; Connect 在 Reserve 之后), 本层不直接 Connect;
//   - timeout 钳定 (clampBgTimeout: 0→runCap、>runCap→runCap) 在 Start 内做,
//     生效值响式回显 effective_timeout_seconds;
//   - sudo 凭据解析提前到连接前 (BgStartSpec.SudoPass 是 Start 入参; 密码瞬时
//     传递用完即弃, 不入任何 task 记录——spec §1), no_sudo 分支因此先于 connect
//     触发, 词汇表与前台逐字一致。
//
// 审计分工 (spec §5, 防双写): connect 三态与 ok 的 start 行由 Start 自落
// (connect 分支先落笔再归还预约; ok 行在 Insert 持锁段内、goroutine 启动前);
// 其余全分支 (含 T4 handoff 的 超限 与 manager 收口竞态的 error) 由本层 defer
// 落笔——startOwned 标记区分两边。
//
// exec_output / exec_stop (T7, 本文件尾部): 纯进程内操作——不碰服务器/凭据,
// 无 profile 门 (task_id 不可猜 + per-Server TaskManager 跨 project 结构性
// 隔离, 照 close_port 先例), 零审计行 (spec §4/§5: exec_output 不审计;
// exec_stop 的终态行由任务 goroutine 落笔, stop 调用对已终态任务无转换)。

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// ExecBackgroundForProfile starts command on serverID as a background task iff
// serverID is in profileID (iron rule), returning the task_id the agent polls
// with exec_output / stops with exec_stop. Every branch is audited with
// Action="exec-bg-start" (Command=命令原文; status vocabulary per spec §5 —
// 超限 is the all-running 32-cap refusal). projectID attributes the audit row
// to the agent's project. mgr must be the process's TaskManager (per-Server
// instance; the tool closure in server.go binds NewServerFromSource's).
func ExecBackgroundForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, command string, sudo bool, timeoutSec int, mgr *TaskManager) (out BgStartOutput, err error) {
	var status string
	var startOwned bool // Start 已自落 start 行的分支 (connect 三态/ok)——本层不重复写
	start := time.Now()
	defer func() {
		if status == "" {
			status = "error"
		}
		if !startOwned {
			_ = st.WriteAudit(store.AuditRow{
				TS: start, ProjectID: projectID, ServerID: serverID, Action: "exec-bg-start",
				Command: command, Sudo: sudo, Status: status,
				DurationMS: time.Since(start).Milliseconds(),
			})
		}
	}()

	// Iron rule: server must be in profile. Gate BEFORE any connect or cred lookup.
	allowed, ferr := st.ServersForProfile(profileID)
	if ferr != nil {
		err = ferr
		return
	}
	if !contains(allowed, serverID) {
		status = "denied"
		err = ErrNotInProfile
		return
	}

	srv, serr := st.GetServer(serverID)
	if serr != nil || srv == nil {
		status = "error"
		err = fmt.Errorf("server %s not found", serverID)
		return
	}

	auth, aerr := vault.AuthForServer(st, srv)
	if aerr != nil {
		if errors.Is(aerr, vault.ErrNoCredential) {
			// Credential-less server (Plan 20 C0): refused BEFORE any connect —
			// the error carries the configure-a-credential hint for the agent.
			status = "no_credential"
			err = aerr
			return
		}
		status = "auth_error"
		err = aerr
		return
	}

	hkCb, herr := sshbroker.HostKeyTOFU(st, srv.Host, srv.Port)
	if herr != nil {
		status = "error"
		err = herr
		return
	}

	// sudo 密码解析 (先于连接, 见文件头): 瞬时传给 Start, 不入 task 记录。
	sudoPass := ""
	if sudo {
		if srv.SudoCredentialID == "" {
			status = "no_sudo"
			err = fmt.Errorf("sudo not configured for server %s (call list_servers: has_sudo tells you)", srv.Name)
			return
		}
		sudoCred, gerr := st.GetCredential(srv.SudoCredentialID)
		if gerr != nil || sudoCred == nil {
			status = "no_sudo"
			err = fmt.Errorf("sudo credential for %s not found", srv.Name)
			return
		}
		sudoPass = string(sudoCred.Secret)
	}

	// Start 编排门外的全部启动链 (spec §1): Reserve (超限/closed 原样上抛——
	// 超限行归本层) → ConnectKeepAlive (connect 三态行由 Start 落) → Insert
	// (start(ok) 行在持锁段内、goroutine 启动前) → runTask goroutine。
	tid, eff, stErr := mgr.Start(ctx, st, BgStartSpec{
		ProjectID: projectID, ServerID: serverID, Command: command,
		Sudo: sudo, SudoPass: sudoPass, TimeoutSec: timeoutSec,
		Server: srv, Auth: auth, HostKeyCb: hkCb,
	})
	if stErr != nil {
		switch {
		case errors.Is(stErr, ErrBgTaskLimit):
			status = "超限" // T4 handoff: 满员全 running 拒绝的 start 行归本层
		case errors.Is(stErr, ErrBgManagerClosed):
			status = "error" // Reserve/Insert 收口竞态拒绝——行归本层 (Start 未落)
		case errors.Is(stErr, context.Canceled):
			status, startOwned = "cancelled", true
		case errors.Is(stErr, sshbroker.ErrHostKeyMismatch):
			status, startOwned = "hostkey_mismatch", true
		default:
			status, startOwned = "connect_error", true
		}
		err = stErr
		return
	}
	status, startOwned = "ok", true
	out = BgStartOutput{
		TaskID:                  tid,
		EffectiveTimeoutSeconds: int(eff.Seconds()),
		Status:                  bgStatusRunning,
	}
	return
}

// ---------- Task 7: exec_output / exec_stop ForProfile (spec §4) ----------

// bgWaitCap 是 exec_output 长轮询预算上限 (spec §3): >60 → 60。
const bgWaitCap = 60

// clampWaitSeconds 钳定 exec_output 的 wait 预算 (spec §3 纯函数, 单测覆盖):
// 0/缺省 → 0 (不等待立即返回); >60 → 60; 负值本应 handler 层拒 (本函数的
// 调用方已拒)——此处防御性按 0 处理。
func clampWaitSeconds(sec int) int {
	if sec > bgWaitCap {
		return bgWaitCap
	}
	if sec < 0 {
		return 0
	}
	return sec
}

// ErrBgUnknownTask 是 unknown task_id 的三因文案 (spec §4 原文 verbatim——
// 过期/驱逐/重启三因不可区分: task 记录纯进程内, 无持久化无恢复; 泛化文案
// 防误导排障)。exec_output 与 exec_stop 共用; 表项已失与 manager 关闭同因。
var ErrBgUnknownTask = errors.New("unknown task_id — it may never have existed, expired after the retention window (1h), been evicted for capacity (32-task limit), or the broker restarted; task records are in-process only")

// ExecOutputForProfile 读取 taskID 的增量输出 (exec_output, spec §4)。
//
// 输入校验在本层 (handler 级): 负 wait/offset 与非 text/base64 的 encoding
// 拒绝 (SDK 反射式 jsonschema 表达不了 minimum/enum——T6 handoff, 见
// ExecOutputInput); encoding 空串缺省 "text"。wait 经 clampWaitSeconds 钳定
// (0→0、>60→60) 后透传 mgr.Output (T5 长轮询回路); ctx 原样透传——tool-call
// 取消 → Output 立即快照返回, 不报错。
//
// BgView → BgReadOutput 按 encoding 装配: text = string(chunk) (原始字节直入
// JSON 字符串, 非法 UTF-8 在 JSON 序列化时被替换 U+FFFD——与前台 exec_command
// 同语义, 有损; 多字节字符可能被读取窗边界切断, 各损半属固有语义);
// base64 = base64.StdEncoding.EncodeToString(chunk) (字节精确, agent 侧解码,
// 跨窗口重组无损, 二进制安全)。两模式 offset 恒为字节口径 (同一游标)。
//
// 零审计行 (spec §4/§5: 纯进程内读不碰服务器/凭据, 与 list_servers 同级)。
// st/projectID 仅为三件套签名对称, 本函数不触 store。
func ExecOutputForProfile(ctx context.Context, st *store.Store, projectID, taskID string, waitSec int, stdoutOff, stderrOff int64, encoding string, mgr *TaskManager) (out BgReadOutput, err error) {
	if waitSec < 0 {
		return BgReadOutput{}, fmt.Errorf("wait_seconds must be >= 0 (got %d)", waitSec)
	}
	if stdoutOff < 0 {
		return BgReadOutput{}, fmt.Errorf("stdout_offset must be >= 0 (got %d)", stdoutOff)
	}
	if stderrOff < 0 {
		return BgReadOutput{}, fmt.Errorf("stderr_offset must be >= 0 (got %d)", stderrOff)
	}
	switch encoding {
	case "", "text", "base64": // "" 缺省 text; 无归一化 (spec §4 同口径)
	default:
		return BgReadOutput{}, fmt.Errorf("encoding must be \"text\" or \"base64\" (got %q)", encoding)
	}
	if encoding == "" {
		encoding = "text"
	}

	v, ok, oerr := mgr.Output(taskID, stdoutOff, stderrOff,
		time.Duration(clampWaitSeconds(waitSec))*time.Second, ctx)
	if oerr != nil {
		return BgReadOutput{}, oerr
	}
	if !ok {
		return BgReadOutput{}, ErrBgUnknownTask // 表项已失/manager 关闭 → 三因文案 (本层拼装)
	}

	var so, se string
	if encoding == "base64" {
		so = base64.StdEncoding.EncodeToString(v.Stdout)
		se = base64.StdEncoding.EncodeToString(v.Stderr)
	} else {
		so, se = string(v.Stdout), string(v.Stderr)
	}
	out = BgReadOutput{
		Status: v.Status, ExitCode: v.ExitCode, Error: v.ErrText,
		Stdout: so, Stderr: se,
		NextStdoutOffset: v.NextStdout, NextStderrOffset: v.NextStderr,
		StdoutBytesTotal: v.StdoutTotal, StderrBytesTotal: v.StderrTotal,
		Truncated: v.Truncated, LostStdoutBytes: v.LostStdout, LostStderrBytes: v.LostStderr,
	}
	if v.Sudo != nil { // Plan 41 §2: 后台 sudo 任务的提权元数据 (终态后非 nil)
		out.Sudo = &SudoInfo{Outcome: v.Sudo.Outcome, UID: v.Sudo.UID}
	}
	return out, nil
}

// ExecStopForProfile 停止 taskID (exec_stop, spec §4): mgr.Stop 持锁置
// stopReq + cancel 后立即返回触发时刻的 status——运行中任务即 running
// (status 枚举无 stopping, 终态经 exec_output 观察, 本调用不阻塞等终态);
// 已终态幂等回其终态; unknown → 三因文案 (ErrBgUnknownTask)。
// 零审计行: 终态行 (exec-bg-end) 由任务 goroutine 落笔 (spec §5 只记状态
// 转换; stop 调用对已终态任务无转换, 对运行中任务的转换在引擎侧落笔)。
// st/projectID/ctx 仅为三件套签名对称, 本函数不触 store。
func ExecStopForProfile(ctx context.Context, st *store.Store, projectID, taskID string, mgr *TaskManager) (out BgStopOutput, err error) {
	status, ok := mgr.Stop(taskID)
	if !ok {
		return BgStopOutput{}, ErrBgUnknownTask
	}
	return BgStopOutput{Status: status}, nil
}
