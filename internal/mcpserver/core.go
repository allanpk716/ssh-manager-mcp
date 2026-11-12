package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"ssh-manager-mcp/internal/dialplan"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/secretfill"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// clampExecTimeout applies the default (when t <= 0) and the MaxExecTimeout
// ceiling (when t exceeds it). Pure — unit-tested directly with no server.
func clampExecTimeout(t time.Duration) time.Duration {
	if t <= 0 {
		t = defaultTimeout
	}
	if t > MaxExecTimeout {
		t = MaxExecTimeout
	}
	return t
}

// ListServersForProfile returns the servers the agent may use (Profile-scoped, no credentials).
func ListServersForProfile(st *store.Store, profileID string) ([]ServerInfo, error) {
	ids, err := st.ServersForProfile(profileID)
	if err != nil {
		return nil, err
	}
	devices, err := st.ListDevices(store.DeviceFilter{})
	if err != nil {
		return nil, err
	}
	devicesByID := make(map[string]*models.Device, len(devices))
	for _, device := range devices {
		devicesByID[device.ID] = device
	}
	out := make([]ServerInfo, 0, len(ids))
	for _, id := range ids {
		srv, err := st.GetServer(id)
		if err != nil || srv == nil {
			continue
		}
		// Coalesce a nil Tags slice to an empty array so the agent sees a consistent
		// schema (the brief's "no omitempty" invariant: empty means "explicitly none",
		// expressed as [] for arrays just as "" for strings). Required for the MCP SDK:
		// the MCP SDK validates the marshaled output JSON against the generated jsonschema,
		// and a nil slice marshals to `null`, which fails the `"type":"array"` constraint
		// and causes CallTool to return (nil, err) — breaking list_servers end-to-end.
		tags := srv.Tags
		if tags == nil {
			tags = []string{}
		}
		// Plan 31 host masking: default "hidden"; plaintext only when the
		// owner opted in per-server (spec §3). Port is never exposed
		// (ServerInfo has no Port field by design).
		host := "hidden"
		if srv.ExposeHost {
			host = srv.Host
		}
		deviceName, site, networkNames := topologyFieldsForServer(devicesByID[srv.DeviceID])
		out = append(out, ServerInfo{
			ID:          srv.ID,
			Name:        srv.Name,
			Host:        host,
			User:        srv.User,
			HasSudo:     srv.SudoCredentialID != "",
			Role:        srv.Role,
			Services:    srv.Services,
			Caveats:     srv.Caveats,
			Location:    srv.Location,
			Hardware:    srv.Hardware,
			Tags:        tags,
			Description: srv.Description,
			Device:      deviceName,
			Site:        site,
			Networks:    networkNames,
			Revision:    srv.Revision,
		})
	}
	return out, nil
}

// topologyFieldsForServer 从设备与网络成员算出 list_servers 的三个拓扑名
// 字字段。单机部署的本机 vault 与多机客户端的缓存 Store 都先经 ListDevices
// 读出同一形态，再进入这里，保证两个角色的输出一致。
func topologyFieldsForServer(device *models.Device) (deviceName, site string, networks []string) {
	networks = []string{}
	if device == nil {
		return "", "", networks
	}
	physicalNetworks := make([]string, 0, len(device.Memberships))
	virtualNetworks := make([]string, 0, len(device.Memberships))
	siteSet := make(map[string]bool)
	for _, membership := range device.Memberships {
		switch membership.NetworkType {
		case models.NetworkPhysical:
			physicalNetworks = append(physicalNetworks, membership.NetworkName)
			if membership.SiteName != "" {
				siteSet[membership.SiteName] = true
			}
		case models.NetworkVirtual:
			virtualNetworks = append(virtualNetworks, membership.NetworkName)
		}
	}
	sort.Strings(physicalNetworks)
	sort.Strings(virtualNetworks)
	networks = append(networks, physicalNetworks...)
	networks = append(networks, virtualNetworks...)
	sites := make([]string, 0, len(siteSet))
	for name := range siteSet {
		sites = append(sites, name)
	}
	sort.Strings(sites)
	return device.Name, strings.Join(sites, "、"), networks
}

// ExecCommandForProfile runs command on serverID iff serverID is in profileID (iron rule).
// sudo=true uses sudo -S with the server's stored sudo password.
// Plan 32 T7 (spec §6): the clamped timeout is echoed as
// ExecOutput.EffectiveTimeoutSeconds — behavior otherwise unchanged (120s
// default, 5min hard cap).
//
// Every branch is audited via the deferred WriteAudit: denial (out-of-profile),
// auth failure, missing credential (no_credential, Plan 20 C0), host-key
// mismatch, connect failure, missing sudo, timeout, exec error, and success.
// projectID attributes the call to the agent's project (empty for any future
// owner-facing caller — currently the owner path is internal/cli/ssh.go).
func ExecCommandForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, command string, sudo bool, timeout time.Duration, confirm ExecConfirmation, hk ...sshbroker.HostKeyStore) (out ExecOutput, err error) {
	var status string
	var action = "exec" // 命令策略换用新动作:拦截 exec-blocked / 需确认退回 exec-confirm-required (Status 存命中规则编号)
	var exitCode int
	var injectedEnvironmentNames []string
	var injectedShareNames []string
	start := time.Now()
	defer func() {
		if status == "" {
			status = "error"
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: serverID, Action: action,
			Command: command, Sudo: sudo, Status: status, ExitCode: exitCode, DurationMS: time.Since(start).Milliseconds(),
			InjectedEnvironmentNames: injectedEnvironmentNames,
			InjectedShareNames:       injectedShareNames,
		})
	}()

	// Iron rule: server must be in profile. Gate BEFORE any connect or cred lookup.
	if gerr := gateServer(st, profileID, serverID); gerr != nil {
		if errors.Is(gerr, ErrNotInProfile) {
			status = "denied"
		}
		err = gerr
		return
	}

	srv, serr := st.GetServer(serverID)
	if serr != nil || srv == nil {
		status = "error"
		err = fmt.Errorf("server %s not found", serverID)
		return
	}
	injectionPlan := buildEnvironmentInjectionPlan(st, srv, sudo)

	// 命令策略 (ticket #10/#11/#15 / ADR 0007):拦截档命令在取凭据、建连接之前
	// 拒绝——不建立 SSH 连接,命令一个字节都不会送到服务器;需确认档同样
	// 在连接之前退回:一行说明 + 一次性确认码,agent 征得用户同意后带码
	// 与理由重调才继续走下面的正常执行链。两档的门与审计动作名都在
	// applyPolicyGate(policygate.go)里,与后台镜像;退回/拦截行的
	// Command 保持命令原文,「带确认码执行」行由门内自落。
	if d := policyJudge(st, srv.ShellType, command); d.Disposition != policy.DispositionAllow {
		if granted, gAction, gStatus, gErr := applyPolicyGate(st, projectID, serverID, command, sudo, confirm, d, execPolicyActions); !granted {
			action, status, err = gAction, gStatus, gErr
			return
		}
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

	dialSource := dialplan.NewStoreSource(st, "")
	cli, connectionNotes, cerr := sshbroker.ConnectServerEntry(ctx, sshbroker.ServerDialRequest{
		Server: srv, Auth: auth, HostKeys: hostKeyStoreFor(st, hk),
		SuccessCache: dialSource,
		Topology:     dialSource,
	})
	if cerr != nil {
		switch {
		case errors.Is(cerr, context.Canceled):
			status = "cancelled"
		case errors.Is(cerr, sshbroker.ErrHostKeyMismatch):
			status = "hostkey_mismatch"
		default:
			status = "connect_error"
		}
		err = cerr
		return
	}
	defer cli.Close()

	timeout = clampExecTimeout(timeout) // <=0 → defaultTimeout; cap at MaxExecTimeout
	// 网络盘符注入计划需要先在目标服务器上探测 445，因此在连接建立后求
	// 出；探测与注入步骤都是固定生成的命令，不经命令策略。
	driveExecution := prepareNetworkDriveExecution(ctx, st, profileID, srv, sudo, cli, injectionPlan)
	drivePlan := driveExecution.plan
	preCommandInjection := driveExecution.execOption

	var res sshbroker.ExecResult
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
		readLimit := readLimitForMasking(ctx, MaxOutputBytes)
		injectedEnvironmentNames = injectionPlan.injectedNames()
		res, err = cli.ExecSudo(ctx, command, sudoCred.Secret, timeout, readLimit, injectionPlan.execOption())
	} else {
		readLimit := readLimitForMasking(ctx, MaxOutputBytes)
		injectedEnvironmentNames = injectionPlan.injectedNames()
		// 选址与映射都在同一条 SSH 会话里完成；它们的耗时属于用户命令
		// 超时的一部分，SSH 会话超时不再额外追加。
		res, err = cli.Exec(ctx, command, timeout, readLimit, preCommandInjection)
	}
	// 状态行在输出遮蔽前剔除；映射失败只转成警告，不改变命令结论。
	res.Stdout, res.Stderr = drivePlan.consumeStatusLines(res.Stdout, res.Stderr)
	injectedShareNames = drivePlan.injectedShareNames()
	// sshbroker 按多读后的上限标记 Truncated；对 agent 仍按原 1 MiB 上限判定。
	res.Truncated = res.StdoutBytes > MaxOutputBytes || res.StderrBytes > MaxOutputBytes
	maskedStdout, stdoutNames := maskCappedContent(ctx, res.Stdout, MaxOutputBytes)
	maskedStderr, stderrNames := maskCappedContent(ctx, res.Stderr, MaxOutputBytes)
	res.Stdout, res.Stderr = maskedStdout, maskedStderr
	exitCode = res.ExitCode
	// sshbroker returns nil err for non-zero exits (*ssh.ExitError) and for timeouts;
	// both are results, not errors. A non-nil err here is a genuine exec failure.
	// Plan 41 §2.3 fail-loud: the three known "command did NOT run" sudo outcomes
	// are errors, not normal exit-1 results — a caller told sudo=true must never
	// have to guess whether the elevation happened.
	switch {
	case res.TimedOut:
		status = "timeout"
	case errors.Is(err, context.Canceled):
		status = "cancelled"
	case err != nil:
		status = "error"
	case res.Sudo != nil && sudoFailedOutcome(res.Sudo.Outcome):
		status = "error"
		err = fmt.Errorf("sudo %s — the command did NOT run: %s", res.Sudo.Outcome, res.Sudo.Diagnostic)
	default:
		status = "ok"
	}
	out = ExecOutput{
		Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, TimedOut: res.TimedOut,
		Truncated: res.Truncated, StdoutBytes: res.StdoutBytes, StderrBytes: res.StderrBytes,
		MaskedOutput:          MaskedOutput{Masked: mergeMaskedNames(stdoutNames, stderrNames)},
		ConnectionNotesOutput: ConnectionNotesOutput{ConnectionNotes: connectionNotes},
		// 钳制改响 (Plan 32 T7 / spec §6): 回显 clamp 后实际生效秒数——
		// timeout 已在上方钳定 (<=0 → defaultTimeout; cap MaxExecTimeout)。
		EffectiveTimeoutSeconds: int(timeout.Seconds()),
	}
	if res.Sudo != nil {
		out.Sudo = &SudoInfo{Outcome: res.Sudo.Outcome, UID: res.Sudo.UID}
	}
	out.InjectedNetworkDrives = drivePlan.injectedDrives()
	out.Stdout = injectionPlan.appendWarnings(out.Stdout)
	out.Stdout = drivePlan.appendWarnings(out.Stdout)
	return
}

// sudoFailedOutcome reports whether a sudo outcome means "the command did NOT
// run" (Plan 41 §2.3) — the fail-loud set. `unverified` deliberately stays a
// normal (though flagged) result: an unknown form must not be mistaken for a
// known failure on signature evidence alone.
func sudoFailedOutcome(outcome string) bool {
	switch outcome {
	case sshbroker.SudoAuthFailed, sshbroker.SudoStartFailed, sshbroker.SudoWrapFailed:
		return true
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// DownloadForProfile downloads path from serverID iff serverID is in profileID
// (iron rule — same gate as ExecCommandForProfile). The download is §6-capped
// (MaxOutputBytes): a larger file yields Truncated=true with Content as the
// prefix and Bytes as the true total. Every branch is audited with
// Action="download" (the path is recorded in the audit Command field).
//
// Statuses: denied (out-of-profile), auth_error, no_credential (credential-
// less server, Plan 20 C0 — Plan 21 A1 unified with exec), hostkey_mismatch,
// connect_error, ok, error. There is no no_sudo / timeout branch — SFTP
// download has neither sudo nor a command deadline.
func DownloadForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, path string, hk ...sshbroker.HostKeyStore) (out DownloadOutput, err error) {
	path = normalizeRemotePath(path)
	var status string
	start := time.Now()
	defer func() {
		if status == "" {
			status = "error"
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: serverID, Action: "download",
			Command: path, Status: status, DurationMS: time.Since(start).Milliseconds(),
		})
	}()

	// Iron rule: server must be in profile. Gate BEFORE any connect or cred lookup.
	if gerr := gateServer(st, profileID, serverID); gerr != nil {
		if errors.Is(gerr, ErrNotInProfile) {
			status = "denied"
		}
		err = gerr
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

	dialSource := dialplan.NewStoreSource(st, "")
	cli, connectionNotes, cerr := sshbroker.ConnectServerEntry(ctx, sshbroker.ServerDialRequest{
		Server: srv, Auth: auth, HostKeys: hostKeyStoreFor(st, hk),
		SuccessCache: dialSource,
		Topology:     dialSource,
	})
	if cerr != nil {
		switch {
		case errors.Is(cerr, context.Canceled):
			status = "cancelled"
		case errors.Is(cerr, sshbroker.ErrHostKeyMismatch):
			status = "hostkey_mismatch"
		default:
			status = "connect_error"
		}
		err = cerr
		return
	}
	defer cli.Close()

	res, derr := cli.Download(ctx, path, readLimitForMasking(ctx, MaxOutputBytes))
	if derr != nil {
		if errors.Is(derr, context.Canceled) {
			status = "cancelled"
		} else {
			status = "error"
		}
		err = derr
		return
	}
	status = "ok"
	// 下载与前台命令相同：按原 1 MiB 上限判定截断，先遮蔽多读到的内容再截回。
	res.Truncated = res.Bytes > MaxOutputBytes
	content, maskedNames := maskCappedContent(ctx, res.Content, MaxOutputBytes)
	out = DownloadOutput{
		Content:      content,
		Bytes:        res.Bytes,
		Truncated:    res.Truncated,
		MaskedOutput: MaskedOutput{Masked: maskedNames},
		ConnectionNotesOutput: ConnectionNotesOutput{
			ConnectionNotes: connectionNotes,
		},
	}
	return
}

// UploadForProfile uploads localPath to remotePath on serverID iff serverID is in
// profileID (iron rule — same gate as ExecCommandForProfile / DownloadForProfile).
// localPath is read from the broker's filesystem; remotePath is
// the destination on the server (a file or a directory, uploaded recursively,
// mirroring `scp -r localPath server:remotePath`). The §6 cap (MaxOutputBytes)
// is a hard PER-FILE bound: a single file strictly larger than the cap is
// refused BEFORE transfer (zero bytes move, no remote file is created — the
// error names file/size/cap; files completed before the refusal remain). When
// every file is within the cap but the cumulative total crosses it,
// Truncated=true with Bytes as the true total and the walk halts between files
// (the in-flight file lands complete — files after it are not uploaded). Every branch is
// audited with Action="upload"; the audit Command field records "localPath -> remotePath".
//
// T1 carry — remote parent creation: sshbroker.Client.Upload puts files via
// sftp.Create and dirs via sftp.Mkdir, both of which require the destination's
// PARENT to pre-exist. Before the transfer this function MkdirAll's the parent
// of remotePath (cli.MkdirAll — broker primitive over SFTP), matching the
// `scp --parents` UX so an agent can target a freshly-named destination without
// a preparatory exec_command. remotePath is normalized before use: POSIX paths
// stay unchanged, while Windows drive paths use forward slashes. path.Dir (not
// filepath.Dir) computes the parent from that slash form so the behavior stays
// correct on a Windows broker host too.
//
// Statuses: denied (out-of-profile), auth_error, no_credential (credential-
// less server, Plan 20 C0 — Plan 21 A1 unified with exec), hostkey_mismatch,
// connect_error, ok, error. There is no no_sudo / timeout branch — SFTP
// upload has neither sudo nor a command deadline.
func UploadForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, localPath, remotePath string, hk ...sshbroker.HostKeyStore) (out UploadOutput, err error) {
	remotePath = normalizeRemotePath(remotePath)
	var status string
	start := time.Now()
	defer func() {
		if status == "" {
			status = "error"
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: serverID, Action: "upload",
			Command: localPath + " -> " + remotePath, Status: status, DurationMS: time.Since(start).Milliseconds(),
		})
	}()

	// Iron rule: server must be in profile. Gate BEFORE any connect or cred lookup.
	if gerr := gateServer(st, profileID, serverID); gerr != nil {
		if errors.Is(gerr, ErrNotInProfile) {
			status = "denied"
		}
		err = gerr
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

	dialSource := dialplan.NewStoreSource(st, "")
	cli, connectionNotes, cerr := sshbroker.ConnectServerEntry(ctx, sshbroker.ServerDialRequest{
		Server: srv, Auth: auth, HostKeys: hostKeyStoreFor(st, hk),
		SuccessCache: dialSource,
		Topology:     dialSource,
	})
	if cerr != nil {
		switch {
		case errors.Is(cerr, context.Canceled):
			status = "cancelled"
		case errors.Is(cerr, sshbroker.ErrHostKeyMismatch):
			status = "hostkey_mismatch"
		default:
			status = "connect_error"
		}
		err = cerr
		return
	}
	defer cli.Close()

	// Ensure remotePath's parent exists before the transfer — T1 carry (see
	// doc comment). Skipped for root/relative paths (no parent to create).
	if parent := path.Dir(remotePath); parent != "" && parent != "." && parent != "/" {
		if merr := cli.MkdirAll(parent); merr != nil {
			status = "error"
			err = fmt.Errorf("remote mkdir %s: %w", parent, merr)
			return
		}
	}

	res, uerr := cli.Upload(ctx, localPath, remotePath, MaxOutputBytes)
	if uerr != nil {
		if errors.Is(uerr, context.Canceled) {
			status = "cancelled"
		} else {
			status = "error"
		}
		err = uerr
		return
	}
	status = "ok"
	out = UploadOutput{
		Files:                 res.Files,
		Bytes:                 res.Bytes,
		Truncated:             res.Truncated,
		ConnectionNotesOutput: ConnectionNotesOutput{ConnectionNotes: connectionNotes},
	}
	return
}

// UploadContentForProfile 把调用方直接提交的内容写入远端文件；目标服务
// 器必须已在 profile 授权集合内。执行顺序固定：① 参数校验；② profile
// 授权检查；③ 读取文本字节或解码 Base64；④ 读取本地服务器条目、填入
// 机密项占位符并按填值后的字节数检查大小上限；⑤ 取凭据并连接目标服务
// 器；⑥ 写入文件；⑦ 尽力登记机密项下发记录；⑧ 写审计。前四步都
// 不访问目标服务器。
//
// 大小上限由 NewServerFromSource 从环境变量 SSHMGR_UPLOAD_CONTENT_MAX
// 解析；测试直接传入较小的上限。
//
// 审计状态沿用上传工具：profile 或机密项授权拒绝为 denied；缺凭据为
// no_credential；主机密钥不匹配、连接失败和取消各有固定值；参数错误、
// 大小超限、Base64 错误、机密项读取失败和写入失败为 error。
//
// 审计里的字节数在成功时等于填值后的实际写入字节数；已完成文本字节读
// 取或 Base64 解码后的拒绝为填值前模板字节数，避免间接暴露机密值长度；
// 更早失败为 0。
func UploadContentForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, content, remotePath, encoding string, cap int64, hk ...sshbroker.HostKeyStore) (out UploadContentOutput, err error) {
	return uploadContentForProfile(ctx, st, projectID, profileID, serverID, content, remotePath, encoding, cap, nil, hk...)
}

func uploadContentForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, content, remotePath, encoding string, cap int64, deliveryRecorder SecretDeliveryRecorder, hk ...sshbroker.HostKeyStore) (out UploadContentOutput, err error) {
	remotePath = normalizeRemotePath(remotePath)
	if encoding == "" {
		encoding = "text"
	}
	byteCount := int64(0) // 只供审计使用；成功后改为实际写入字节数
	var status string
	var filledSecretNames []string
	var deliveryRecordStatus, deliveryReason string
	start := time.Now()
	defer func() {
		if status == "" {
			status = "error"
		}
		auditCommand := fmt.Sprintf("inline %d bytes -> %s", byteCount, remotePath)
		if len(filledSecretNames) > 0 {
			auditCommand += "; secrets=" + strings.Join(filledSecretNames, ",")
			auditCommand += "; delivery-record=" + deliveryRecordStatus
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: serverID, Action: "upload-content",
			Command: auditCommand,
			Status:  status, DurationMS: time.Since(start).Milliseconds(),
		})
	}()

	// ① 参数校验只依赖调用方自己提交的输入，先于 profile 授权检查执行。
	if encoding != "text" && encoding != "base64" {
		return UploadContentOutput{}, fmt.Errorf("encoding must be \"text\" or \"base64\", got %q", encoding)
	}
	if remotePath == "" || !isAbsRemotePath(remotePath) {
		return UploadContentOutput{}, fmt.Errorf("remote_path must be an absolute path: /dir/file, or a Windows drive path such as C:/dir/file (backslashes are accepted and converted)")
	}
	if encoding == "base64" && strings.ContainsAny(content, "\r\n") {
		return UploadContentOutput{}, fmt.Errorf("base64 content must be single-line standard base64 with padding — join lines and resend")
	}

	// ② 目标服务器必须属于 profile 授权集合；先检查，再读取凭据或连接。
	profileServerIDs, gerr := st.ServersForProfile(profileID)
	if gerr != nil {
		return UploadContentOutput{}, gerr
	}
	if gerr := gateServerIn(profileServerIDs, serverID); gerr != nil {
		if errors.Is(gerr, ErrNotInProfile) {
			status = "denied"
		}
		return UploadContentOutput{}, gerr
	}

	// ③ Base64 解码。大小上限在下一步按占位符填值后的内容检查。
	var contentBytes []byte
	switch encoding {
	case "text":
		contentBytes = []byte(content)
	default: // base64
		decoded, derr := base64.StdEncoding.DecodeString(content)
		if derr != nil {
			byteCount = 0
			return UploadContentOutput{}, fmt.Errorf("invalid base64 content: %v", derr)
		}
		contentBytes = decoded
	}
	byteCount = int64(len(contentBytes))

	// ④ 机密项占位符在取得服务器凭据和发起连接前填值。这里读取的是本地
	// vault 或客户端缓存快照，不访问网络。
	srv, serr := st.GetServer(serverID)
	if serr != nil || srv == nil {
		status = "error"
		err = fmt.Errorf("server %s not found", serverID)
		return
	}
	filled := secretfill.Fill(ctx, contentBytes, secretfill.ServerRef{ID: srv.ID, Name: srv.Name}, secretfill.NewStoreSource(st, profileServerIDs))
	filledSecretNames = filled.Names
	projectedByteCount := int64(len(filled.ProjectedContent))
	fillErr := filled.Err()
	if st.CacheAuthorityTooOld() && secretFillResultUsesPlaceholder(filled) {
		status = "denied"
		err = secretfill.ErrAuthorityTooOld
		return
	}
	if projectedByteCount > cap {
		sizeErr := fmt.Errorf("content (%d bytes) exceeds upload-content cap %d — refused before transfer", projectedByteCount, cap)
		if fillErr == nil {
			err = sizeErr
		} else {
			err = errors.Join(fillErr, sizeErr)
		}
	} else {
		err = fillErr
	}
	if err != nil {
		for _, problem := range filled.Problems {
			if problem.Kind == secretfill.ProblemInvalidFormat ||
				problem.Kind == secretfill.ProblemMissingOrNotAllowed {
				status = "denied"
			}
		}
		return
	}
	templateContent := append([]byte(nil), contentBytes...)
	contentBytes = filled.Content
	r := bytes.NewReader(contentBytes)

	// ⑤ 取凭据、校验主机密钥并连接目标服务器；目标主机明文在连接层生成
	// 错误时统一遮蔽。
	auth, aerr := vault.AuthForServer(st, srv)
	if aerr != nil {
		if errors.Is(aerr, vault.ErrNoCredential) {
			// 没有登记凭据的服务器在任何连接前拒绝；错误文本包含配置凭据
			// 的提示。
			status = "no_credential"
			err = aerr
			return
		}
		status = "auth_error"
		err = aerr
		return
	}

	dialSource := dialplan.NewStoreSource(st, "")
	cli, connectionNotes, cerr := sshbroker.ConnectServerEntry(ctx, sshbroker.ServerDialRequest{
		Server: srv, Auth: auth, HostKeys: hostKeyStoreFor(st, hk),
		SuccessCache: dialSource,
		Topology:     dialSource,
	})
	if cerr != nil {
		switch {
		case errors.Is(cerr, context.Canceled):
			status = "cancelled"
		case errors.Is(cerr, sshbroker.ErrHostKeyMismatch):
			status = "hostkey_mismatch"
		default:
			status = "connect_error"
		}
		err = cerr
		return
	}
	defer cli.Close()

	// ⑥ 写入文件；父目录创建和关闭结果检查都在写入函数内完成。
	var createMode os.FileMode
	if len(filledSecretNames) > 0 && !srv.ShellType.IsWindows() {
		createMode = 0o600
	}
	if werr := cli.WriteFileWithCreateMode(ctx, remotePath, r, createMode); werr != nil {
		if errors.Is(werr, context.Canceled) {
			status = "cancelled"
		} else {
			status = "error"
		}
		err = werr
		return
	}
	status = "ok"
	byteCount = int64(len(filled.Content))
	if filledSecretNames == nil {
		filledSecretNames = []string{}
	}
	if len(filledSecretNames) > 0 {
		deliveryRecordStatus = secretDeliveryStatusNotRecorded
		registration := registerSecretDelivery(ctx, st, deliveryRecorder, cli, secretDeliveryRegistrationRequest{
			ServerID: serverID, ProfileID: profileID, RemotePath: remotePath,
			Filled: filled, Template: templateContent, TemplateLimitBytes: cap,
		})
		deliveryRecordStatus = registration.Status
		deliveryReason = registration.Reason
	}
	out = UploadContentOutput{
		Bytes:                 byteCount,
		FilledSecretNames:     filledSecretNames,
		DeliveryRecordStatus:  deliveryRecordStatus,
		DeliveryRecordReason:  deliveryReason,
		ConnectionNotesOutput: ConnectionNotesOutput{ConnectionNotes: connectionNotes},
	}
	return
}

type secretDeliveryRegistrationRequest struct {
	ServerID           string
	ProfileID          string
	RemotePath         string
	Filled             secretfill.Result
	Template           []byte
	TemplateLimitBytes int64
}

type secretDeliveryRegistration struct {
	Status string
	Reason string
}

// registerSecretDelivery 在文件写入成功后尽力登记下发记录，并把登记状态
// 与未登记原因整理成 upload_content 的输出字段。
func registerSecretDelivery(
	ctx context.Context,
	st *store.Store,
	deliveryRecorder SecretDeliveryRecorder,
	cli *sshbroker.Client,
	request secretDeliveryRegistrationRequest,
) secretDeliveryRegistration {
	result := secretDeliveryRegistration{Status: secretDeliveryStatusNotRecorded}
	finalPath, err := cli.RealRemotePath(ctx, request.RemotePath)
	if err != nil {
		result.Reason = "解析服务器上的最终路径失败，未登记下发记录：" + err.Error()
		return result
	}
	digest := sha256.Sum256(request.Filled.Content)
	deliverySecrets := make([]store.SecretDeliverySecret, 0, len(request.Filled.Names))
	for _, name := range request.Filled.Names {
		version, exists := request.Filled.ValueVersions[name]
		if !exists || version <= 0 {
			result.Reason = "机密项值版本号不可用，未登记下发记录"
			return result
		}
		deliverySecrets = append(deliverySecrets, store.SecretDeliverySecret{
			Name: name, ValueVersion: version,
		})
	}
	recordInput := store.SecretDeliveryRecordInput{
		ServerID: request.ServerID, Path: finalPath, Template: request.Template,
		FileSHA256: hex.EncodeToString(digest[:]), Secrets: deliverySecrets,
		WrittenAt: time.Now(), AuthorizationProfileID: request.ProfileID,
		TemplateLimitBytes: request.TemplateLimitBytes,
	}
	if deliveryRecorder == nil {
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			hostname = "local machine"
		}
		recordInput.InitiatingDeviceName = hostname
		recordInput.InitiatingInstanceName = "single-machine broker"
		if _, err := st.RecordSecretDelivery(recordInput, store.AgentSource()); err != nil {
			result.Reason = "下发记录登记失败：" + err.Error()
			return result
		}
		result.Status = secretDeliveryStatusRecorded
		return result
	}
	outcome, err := deliveryRecorder.RecordSecretDeliveryForBroker(ctx, recordInput)
	switch {
	case err != nil:
		result.Reason = outcome.Reason
		if result.Reason == "" {
			result.Reason = err.Error()
		}
	case outcome.Recorded:
		result.Status = secretDeliveryStatusRecorded
	default:
		result.Reason = outcome.Reason
	}
	return result
}

// secretFillResultUsesPlaceholder 判断内容是否包含合法机密项占位符，或以
// 机密项占位符前缀开头的非法片段。旧权威端判断只对这类内容生效。
func secretFillResultUsesPlaceholder(result secretfill.Result) bool {
	if len(result.Names) > 0 {
		return true
	}
	for _, problem := range result.Problems {
		if problem.Kind == secretfill.ProblemInvalidFormat {
			return true
		}
	}
	return false
}

// isAbsRemotePath reports whether remotePath is absolute in the TARGET's path
// namespace: POSIX-rooted ("/...") is the documented contract (schema text),
// plus the Windows drive form ("X:/..." — slash after the colon only; a
// backslash stays a legal POSIX filename char). The invariant this check
// protects is "not relative": a relative path would resolve against the sftp
// server's start dir, surprising the caller. The drive form exists so the
// tool (and its tests, plus the Plan 33 e2e lane) work against a
// Windows-host-FS sftp — testsshd serves the host FS and the dev lane is a
// Windows broker host; the linux CI lane only ever sees "/...".
func isAbsRemotePath(p string) bool {
	if strings.HasPrefix(p, "/") {
		return true
	}
	return len(p) >= 3 &&
		((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) &&
		p[1] == ':' && p[2] == '/'
}

// normalizeRemotePath 统一文件工具收到的远端路径写法。GitHub issue 47 的背
// 景是 Windows 盘符路径里的反斜杠曾被原样传给 SFTP，导致目标路径被误处
// 理；因此盘符加冒号后跟斜杠或反斜杠的路径会把全部反斜杠换成正斜杠。
// 以正斜杠开头的路径可能是 Linux 或 macOS 路径，其中反斜杠是合法文件名
// 字符，必须原样保留。其他输入不在这里改写；是否拒绝相对路径由调用方
// 决定。该规则不依赖服务器条目的 shell 类型。
func normalizeRemotePath(remotePath string) string {
	if strings.HasPrefix(remotePath, "/") {
		return remotePath
	}
	if len(remotePath) >= 3 &&
		((remotePath[0] >= 'A' && remotePath[0] <= 'Z') ||
			(remotePath[0] >= 'a' && remotePath[0] <= 'z')) &&
		remotePath[1] == ':' &&
		(remotePath[2] == '/' || remotePath[2] == '\\') {
		return strings.ReplaceAll(remotePath, `\`, `/`)
	}
	return remotePath
}

// ForwardForProfile opens a `ssh -L` tunnel through serverID iff serverID is in
// profileID (iron rule — same gate as ExecCommandForProfile / Download /
// Upload). This is the first STATEFUL broker operation: the SSH connection
// opened here is NOT defer-closed — on success the TunnelManager owns it for the
// tunnel's life (the connection stays open across this call's return so the
// local listener can keep piping bytes through it). The tunnel + client are
// reclaimed by close_port (CloseForwardForProfile), the tunnel sweeper
// (forwardIdleTimeout), or MCP shutdown (TunnelManager.CloseAll).
//
// listenHost (Plan 35, spec §2) picks the local bind address: empty →
// "127.0.0.1"; loopback always allowed; any other IP literal must be on the
// owner-approved bind-host whitelist (canonical compare, per-call read — a
// whitelist edit takes effect on the NEXT call). The gate runs after the
// masked-host guard and before GetServer (fail fast before any credential
// lookup or connect). After Open, the tunnel's onActivity hook is wired to
// mgr.Touch so carrying traffic keeps the tunnel alive (spec §3).
//
// Resource-cleanup discipline (the load-bearing concern): every error branch
// that connected a client WITHOUT handing it to the TunnelManager closes it in
// the deferred cleanup (err != nil && cli != nil) so no ssh.Client leaks on a
// ForwardLocal failure or any pre-Open error. On success (status="ok", err=nil)
// the manager owns the client and the deferred cleanup skips the close.
//
// Every branch is audited with Action="forward"; the audit Command field
// records the forward target as "remoteHost:remotePort" until the tunnel id is
// known, "remoteHost:remotePort id=<tunnelID>" once Open returned one (the
// correlation key tying forward rows to later close/sweep events — spec §7).
// Statuses: denied (out-of-profile), bind_denied (listen_host rejected —
// non-IP-literal, wildcard, or non-loopback off the whitelist), auth_error,
// no_credential (credential-less server, Plan 20 C0 — Plan 21 A1 unified with
// exec), hostkey_mismatch, connect_error, ok, error (incl. the fail-closed
// whitelist read failure). There is no no_sudo / timeout branch — a forward is
// a listener + pipe with no command deadline.
func ForwardForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, remoteHost string, remotePort, localPort int, listenHost string, mgr *TunnelManager, hk ...sshbroker.HostKeyStore) (out ForwardOutput, err error) {
	var status string
	var cli *sshbroker.Client
	var tunID string // "" until Open succeeded — drives the audit Command form (spec §7)
	start := time.Now()
	defer func() {
		if status == "" {
			status = "error"
		}
		// On any error path where we connected a client but did NOT register it
		// with the manager, close it now so it cannot leak. On success the manager
		// owns cli (Close/CloseAll/SweepIdle close it) — do NOT close here.
		if err != nil && cli != nil {
			_ = cli.Close()
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: serverID, Action: "forward",
			Command:    joinAuditCommand(remoteHost, remotePort, tunID),
			Status:     status,
			DurationMS: time.Since(start).Milliseconds(),
		})
	}()

	// Iron rule: server must be in profile. Gate BEFORE any connect or cred lookup.
	if gerr := gateServer(st, profileID, serverID); gerr != nil {
		if errors.Is(gerr, ErrNotInProfile) {
			status = "denied"
		}
		err = gerr
		return
	}

	// Plan 31: the masked-host literal must never be dialed. It is the one
	// channel where the agent could "use" the list_servers mask value — a
	// malicious resolver record for "hidden" server-side would capture the
	// mistyped traffic. DNS is case-insensitive, and "hidden." (FQDN trailing
	// dot) resolves to the same name — so the comparison is case-insensitive
	// AND strips one trailing dot plus surrounding whitespace.
	if strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(remoteHost, ".")), "hidden") {
		status = "error"
		err = errors.New("remote_host \"hidden\" is the list_servers masked-host literal, not a real host — pass the actual host:port to forward to")
		return
	}

	// listen_host gate (spec §2): default loopback; loopback always allowed;
	// non-loopback must be owner-whitelisted (canonical compare, per-call read
	// so a whitelist edit bites on the NEXT call). Read failure on a
	// non-loopback request fails CLOSED. Errors quote the agent's own input
	// only — whitelist contents are never echoed.
	bindHost := "127.0.0.1"
	if strings.TrimSpace(listenHost) != "" {
		canonical, cerr := store.CanonicalBindIP(listenHost)
		if cerr != nil {
			status = "bind_denied"
			err = fmt.Errorf("listen_host %q must be a specific IP literal (not a hostname, wildcard 0.0.0.0/::, or zoned address)", listenHost)
			return
		}
		if ip := net.ParseIP(canonical); ip.IsUnspecified() {
			status = "bind_denied"
			err = fmt.Errorf("listen_host %q is a wildcard address — binding 0.0.0.0/:: is forbidden", listenHost)
			return
		} else if !ip.IsLoopback() {
			hosts, lerr := st.ListForwardBindHosts()
			if lerr != nil {
				status = "error" // fail-closed (spec §2): DB read failure must not open a non-loopback bind
				err = fmt.Errorf("cannot read bind host whitelist, refusing non-loopback listen_host: %w", lerr)
				return
			}
			if !contains(hosts, canonical) {
				status = "bind_denied"
				err = fmt.Errorf("listen_host %q is not in the owner-approved bind host whitelist", listenHost)
				return
			}
		}
		bindHost = canonical
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

	dialSource := dialplan.NewStoreSource(st, "")
	cli, connectionNotes, cerr := sshbroker.ConnectServerEntry(ctx, sshbroker.ServerDialRequest{
		Server: srv, Auth: auth, HostKeys: hostKeyStoreFor(st, hk),
		SuccessCache: dialSource,
		Topology:     dialSource,
	})
	if cerr != nil {
		switch {
		case errors.Is(cerr, context.Canceled):
			status = "cancelled"
		case errors.Is(cerr, sshbroker.ErrHostKeyMismatch):
			status = "hostkey_mismatch"
		default:
			status = "connect_error"
		}
		err = cerr
		return
	}
	// NO defer cli.Close() here — on success the TunnelManager owns cli (stateful).

	tun, ferr2 := cli.ForwardLocal(localPort, bindHost, remoteHost, remotePort, nil)
	if ferr2 != nil {
		status = "error"
		err = ferr2
		return // deferred cleanup sees err != nil && cli != nil → closes cli (no leak)
	}

	// Plan 35 T3: Open mirrors the tunnel into tunnel_registry (fail-the-Open).
	// ListenHost carries the gate-produced canonical form.
	id, oerr := mgr.Open(tun, cli, TunnelMeta{
		ProjectID:  projectID,
		ServerID:   serverID,
		Remote:     net.JoinHostPort(remoteHost, strconv.Itoa(remotePort)),
		ListenHost: bindHost,
	})
	if oerr != nil {
		status = "error"
		err = oerr
		// fail-the-Open already closed tunnel + client; the deferred cleanup
		// re-closes cli (idempotent) — nothing registered, nothing leaked.
		return
	}
	tunID = id // audit rows from here on correlate via " id=<tunnelID>" (spec §7)
	// Activity-aware reclaim wiring (spec §3): every throttled activity ping
	// refreshes the manager's lastActivity for the REGISTERED id, so a tunnel
	// carrying traffic survives the idle sweeper. Attached AFTER Open returns
	// the id (the closure must capture the registered id, not a stale one).
	tun.SetOnActivity(func() { mgr.Touch(id) })
	status = "ok"
	out = ForwardOutput{
		TunnelID:              id,
		LocalPort:             localPortOfAddr(tun.LocalAddr()),
		ListenHost:            hostOfAddr(tun.LocalAddr()),
		ConnectionNotesOutput: ConnectionNotesOutput{ConnectionNotes: connectionNotes},
	}
	return
}

// CloseForwardForProfile tears down a tunnel by its opaque id. The id is
// unguessable (a UUID) and CloseForwardForProfile only closes tunnels THIS
// TunnelManager owns, so there is no profile re-gate (forward_port already
// gated the targeted server; close_port operates on the broker-local handle).
// Not-found is surfaced as a tool error (the manager's Close returns false).
// Audited with Action="close-forward"; the audit Command field records the
// tunnel_id (the correlation key) and Status is ok (the tunnel existed + was
// torn down) or error (not found).
func CloseForwardForProfile(ctx context.Context, st *store.Store, projectID, tunnelID string, mgr *TunnelManager) (err error) {
	var status string
	start := time.Now()
	defer func() {
		if status == "" {
			status = "error"
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, Action: "close-forward",
			Command: tunnelID, Status: status, DurationMS: time.Since(start).Milliseconds(),
		})
	}()
	if !mgr.Close(tunnelID) {
		status = "error"
		err = fmt.Errorf("no open tunnel with id %s", tunnelID)
		return
	}
	status = "ok"
	return
}

// localPortOfAddr extracts the port from a "host:port" listen address (the
// tunnel's LocalAddr). Returns 0 on any parse failure (the caller treats <=0 as
// an error in tests; production just reports it back to the agent).
func localPortOfAddr(addr string) int {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(portStr)
	return p
}

// hostOfAddr extracts the host from a "host:port" listen address (the tunnel's
// LocalAddr — e.g. "[2001:db8::1]:54321" → "2001:db8::1"). Returns the input
// unchanged on any parse failure. ForwardOutput.ListenHost reports this so the
// agent learns the ACTUAL bound address (the canonical form the kernel bound).
func hostOfAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

// joinAuditCommand renders the forward audit correlation key (spec §7):
// "host:port" until the tunnel id is known (every pre-Open failure), and
// "host:port id=<tunnelID>" once Open returned one — tying the forward row to
// the later close-forward row (whose Command is the bare tunnel id).
func joinAuditCommand(remoteHost string, remotePort int, tunID string) string {
	base := net.JoinHostPort(remoteHost, strconv.Itoa(remotePort))
	if tunID == "" {
		return base
	}
	return base + " id=" + tunID
}

// ---- Plan 33: upload_content env seam (spec rev3 §3.1) ----

// uploadContentCapDefault / uploadContentCapMax bound SSHMGR_UPLOAD_CONTENT_MAX:
// the seam is fail-closed — unset → 8 MiB; unparsable / non-positive / over
// 1 GiB → error (the process refuses to start; never a silent clamp). The 1
// GiB ceiling keeps §3.2's cap+cap/3+64KiB body limit far from int64 overflow
// and stops an accidental huge value from ballooning the serve body limit
// (that scaling is registered in threat-model per spec §6).
const (
	uploadContentCapDefault int64 = store.UploadContentDefaultLimitBytes
	uploadContentCapMax     int64 = 1 << 30 // 1 GiB
)

func resolveUploadContentCap() (int64, error) {
	v := os.Getenv("SSHMGR_UPLOAD_CONTENT_MAX")
	if v == "" {
		return uploadContentCapDefault, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("SSHMGR_UPLOAD_CONTENT_MAX: invalid value %q (want positive integer)", v)
	}
	if n > uploadContentCapMax {
		return 0, fmt.Errorf("SSHMGR_UPLOAD_CONTENT_MAX: %d exceeds the 1 GiB ceiling (1073741824)", n)
	}
	return n, nil
}
