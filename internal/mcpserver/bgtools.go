package mcpserver

// exec_background 的 ForProfile 包装 (Plan 32 T6; exec_output/exec_stop 归 T7)。
// 门链与分支状态逐分支镜像 ExecCommandForProfile (core.go): denied / not found /
// no_credential / auth_error / hostkey_mismatch / connect 三态 (connect_error /
// cancelled / hostkey_mismatch) / no_sudo / 超限 / error / ok (spec §4)。
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

import (
	"context"
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
