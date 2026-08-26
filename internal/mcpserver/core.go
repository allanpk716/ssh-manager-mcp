package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

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
		})
	}
	return out, nil
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
func ExecCommandForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, command string, sudo bool, timeout time.Duration) (out ExecOutput, err error) {
	var status string
	var exitCode int
	start := time.Now()
	defer func() {
		if status == "" {
			status = "error"
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: serverID, Action: "exec",
			Command: command, Sudo: sudo, Status: status, ExitCode: exitCode, DurationMS: time.Since(start).Milliseconds(),
		})
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

	cli, cerr := sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)
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
		res, err = cli.ExecSudo(ctx, command, sudoCred.Secret, timeout, MaxOutputBytes)
	} else {
		res, err = cli.Exec(ctx, command, timeout, MaxOutputBytes)
	}
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
		// 钳制改响 (Plan 32 T7 / spec §6): 回显 clamp 后实际生效秒数——
		// timeout 已在上方钳定 (<=0 → defaultTimeout; cap MaxExecTimeout)。
		EffectiveTimeoutSeconds: int(timeout.Seconds()),
	}
	if res.Sudo != nil {
		out.Sudo = &SudoInfo{Outcome: res.Sudo.Outcome, UID: res.Sudo.UID}
	}
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
func DownloadForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, path string) (out DownloadOutput, err error) {
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

	cli, cerr := sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)
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

	res, derr := cli.Download(ctx, path, MaxOutputBytes)
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
	out = DownloadOutput{Content: res.Content, Bytes: res.Bytes, Truncated: res.Truncated}
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
// a preparatory exec_command. remotePath is a POSIX path; path.Dir (not
// filepath.Dir) computes the parent so the gate stays correct on a Windows broker
// host too (the remote's path convention is always POSIX).
//
// Statuses: denied (out-of-profile), auth_error, no_credential (credential-
// less server, Plan 20 C0 — Plan 21 A1 unified with exec), hostkey_mismatch,
// connect_error, ok, error. There is no no_sudo / timeout branch — SFTP
// upload has neither sudo nor a command deadline.
func UploadForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, localPath, remotePath string) (out UploadOutput, err error) {
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

	cli, cerr := sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)
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
	out = UploadOutput{Files: res.Files, Bytes: res.Bytes, Truncated: res.Truncated}
	return
}

// UploadContentForProfile (Plan 33, spec rev3 §2) writes INLINE content to
// remotePath on serverID iff serverID is in profileID (iron rule). The
// execution order is pinned: ① param-level validation (encoding enum,
// remote_path absolute, base64 single-line — these reflect only the CALLER'S
// OWN input, leak nothing about any server, and hence run BEFORE the gate;
// the denied-first principle constrains CONTENT-level errors: cap/base64) →
// ② profile gate → ③ cap pre-check (two-stage for base64; connect-free, zero
// bytes move, no remote file) → ④ GetServer → AuthForServer (no_credential)
// → HostKeyTOFU → Connect (Plan 31 redaction at the source) → ⑤ WriteFile
// (parent creation INSIDE, one watchdog, checked Close) → ⑥ audit + {bytes}.
//
// cap arrives from the NewServerFromSource-resolved env seam
// (SSHMGR_UPLOAD_CONTENT_MAX); tests pass small caps directly.
//
// Statuses mirror upload: denied / auth_error / no_credential /
// hostkey_mismatch / connect_error / cancelled / ok / error (param
// validation, cap refusal, base64 errors, WriteFile failures → error).
//
// Audit %d branch value table (spec rev3 §5): ok + text refusal =
// len(content); base64 coarse refusal = est (exact for every
// decoder-accepted input); unreachable defensive fine-check = len(decoded);
// decode failure + single-line rejection + param errors = 0.
func UploadContentForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, content, remotePath, encoding string, cap int64) (out UploadContentOutput, err error) {
	if encoding == "" {
		encoding = "text"
	}
	byteCount := int64(0) // feeds the refusal error AND the audit Command (see table above)
	var status string
	start := time.Now()
	defer func() {
		if status == "" {
			status = "error"
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: serverID, Action: "upload-content",
			Command: fmt.Sprintf("inline %d bytes -> %s", byteCount, remotePath),
			Status:  status, DurationMS: time.Since(start).Milliseconds(),
		})
	}()

	// ① param-level validation (before the gate — caller's own input only).
	if encoding != "text" && encoding != "base64" {
		return UploadContentOutput{}, fmt.Errorf("encoding must be \"text\" or \"base64\", got %q", encoding)
	}
	if remotePath == "" || !isAbsRemotePath(remotePath) {
		return UploadContentOutput{}, fmt.Errorf("remote_path must be an absolute path starting with /")
	}
	if encoding == "base64" && strings.ContainsAny(content, "\r\n") {
		return UploadContentOutput{}, fmt.Errorf("base64 content must be single-line standard base64 with padding — join lines and resend")
	}

	// ② iron rule: server must be in profile. Gate BEFORE any connect or cred lookup.
	allowed, ferr := st.ServersForProfile(profileID)
	if ferr != nil {
		return UploadContentOutput{}, ferr
	}
	if !contains(allowed, serverID) {
		status = "denied"
		return UploadContentOutput{}, ErrNotInProfile
	}

	// ③ cap pre-check + decode (connect-free). base64's est is EXACT for every
	// decoder-accepted input (single line is pinned by ①), so the fine check
	// below is defensive-only — unreachable from public input, kept per spec
	// rev3 §2.1.
	var r io.Reader
	switch encoding {
	case "text":
		byteCount = int64(len(content))
		if byteCount > cap {
			status = "error"
			return UploadContentOutput{}, fmt.Errorf("content (%d bytes) exceeds upload-content cap %d — refused before transfer", byteCount, cap)
		}
		r = strings.NewReader(content)
	default: // base64
		padCount := int64(0)
		for i := len(content) - 1; i >= 0 && content[i] == '=' && padCount < 2; i-- {
			padCount++
		}
		est := int64(len(content))/4*3 - padCount
		if est > cap {
			byteCount = est
			status = "error"
			return UploadContentOutput{}, fmt.Errorf("content (%d bytes decoded) exceeds upload-content cap %d — refused before transfer", byteCount, cap)
		}
		decoded, derr := base64.StdEncoding.DecodeString(content)
		if derr != nil {
			byteCount = 0
			return UploadContentOutput{}, fmt.Errorf("invalid base64 content: %v", derr)
		}
		byteCount = int64(len(decoded))
		if byteCount > cap { // defensive fine check — see comment above
			status = "error"
			return UploadContentOutput{}, fmt.Errorf("content (%d bytes decoded) exceeds upload-content cap %d — refused before transfer", byteCount, cap)
		}
		r = bytes.NewReader(decoded)
	}

	// ④ server + credential + host key + connect — the same chain every broker
	// tool walks (DownloadForProfile verbatim; Plan 31 redaction lives in
	// sshbroker.Connect at the source).
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

	cli, cerr := sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)
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

	// ⑤ write (parent creation + checked Close live inside WriteFile).
	if werr := cli.WriteFile(ctx, remotePath, r); werr != nil {
		if errors.Is(werr, context.Canceled) {
			status = "cancelled"
		} else {
			status = "error"
		}
		err = werr
		return
	}
	status = "ok"
	out = UploadContentOutput{Bytes: byteCount}
	return
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
func ForwardForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, remoteHost string, remotePort, localPort int, listenHost string, mgr *TunnelManager) (out ForwardOutput, err error) {
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

	hkCb, herr := sshbroker.HostKeyTOFU(st, srv.Host, srv.Port)
	if herr != nil {
		status = "error"
		err = herr
		return
	}

	cli, cerr := sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)
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
	out = ForwardOutput{TunnelID: id, LocalPort: localPortOfAddr(tun.LocalAddr()), ListenHost: hostOfAddr(tun.LocalAddr())}
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
	uploadContentCapDefault int64 = 8 << 20 // 8 MiB
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
