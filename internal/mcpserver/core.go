package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net"
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
	switch {
	case res.TimedOut:
		status = "timeout"
	case errors.Is(err, context.Canceled):
		status = "cancelled"
	case err != nil:
		status = "error"
	default:
		status = "ok"
	}
	out = ExecOutput{
		Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, TimedOut: res.TimedOut,
		Truncated: res.Truncated, StdoutBytes: res.StdoutBytes, StderrBytes: res.StderrBytes,
	}
	return
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

// ForwardForProfile opens a `ssh -L` tunnel through serverID iff serverID is in
// profileID (iron rule — same gate as ExecCommandForProfile / Download /
// Upload). This is the first STATEFUL broker operation: the SSH connection
// opened here is NOT defer-closed — on success the TunnelManager owns it for the
// tunnel's life (the connection stays open across this call's return so the
// local listener can keep piping bytes through it). The tunnel + client are
// reclaimed by close_port (CloseForwardForProfile), the tunnel sweeper
// (forwardIdleTimeout), or MCP shutdown (TunnelManager.CloseAll).
//
// Resource-cleanup discipline (the load-bearing concern): every error branch
// that connected a client WITHOUT handing it to the TunnelManager closes it in
// the deferred cleanup (err != nil && cli != nil) so no ssh.Client leaks on a
// ForwardLocal failure or any pre-Open error. On success (status="ok", err=nil)
// the manager owns the client and the deferred cleanup skips the close.
//
// Every branch is audited with Action="forward"; the audit Command field
// records the forward target as "remoteHost:remotePort". Statuses: denied
// (out-of-profile), auth_error, no_credential (credential-less server, Plan
// 20 C0 — Plan 21 A1 unified with exec), hostkey_mismatch, connect_error,
// ok, error. There is no no_sudo / timeout branch — a forward is a listener +
// pipe with no command deadline.
func ForwardForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID, remoteHost string, remotePort, localPort int, mgr *TunnelManager) (out ForwardOutput, err error) {
	var status string
	var cli *sshbroker.Client
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
			Command:    net.JoinHostPort(remoteHost, strconv.Itoa(remotePort)),
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

	tun, ferr2 := cli.ForwardLocal(localPort, remoteHost, remotePort)
	if ferr2 != nil {
		status = "error"
		err = ferr2
		return // deferred cleanup sees err != nil && cli != nil → closes cli (no leak)
	}

	id := mgr.Open(tun, cli) // manager owns both tunnel + client from here
	status = "ok"
	out = ForwardOutput{TunnelID: id, LocalPort: localPortOfAddr(tun.LocalAddr())}
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
