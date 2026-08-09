package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

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
		out = append(out, ServerInfo{
			ID: srv.ID, Name: srv.Name, Host: srv.Host, User: srv.User,
			HasSudo: srv.SudoCredentialID != "",
		})
	}
	return out, nil
}

// ExecCommandForProfile runs command on serverID iff serverID is in profileID (iron rule).
// sudo=true uses sudo -S with the server's stored sudo password.
//
// Every branch is audited via the deferred WriteAudit: denial (out-of-profile), auth
// failure, host-key mismatch, connect failure, missing sudo, timeout, exec error, and
// success. projectID attributes the call to the agent's project (empty for any future
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

	cli, cerr := sshbroker.Connect(srv.Host, srv.Port, srv.User, auth, hkCb)
	if cerr != nil {
		if errors.Is(cerr, sshbroker.ErrHostKeyMismatch) {
			status = "hostkey_mismatch"
		} else {
			status = "connect_error"
		}
		err = cerr
		return
	}
	defer cli.Close()

	if timeout <= 0 {
		timeout = defaultTimeout
	}

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
		res, err = cli.ExecSudo(command, sudoCred.Secret, timeout)
	} else {
		res, err = cli.Exec(command, timeout)
	}
	exitCode = res.ExitCode
	// sshbroker returns nil err for non-zero exits (*ssh.ExitError) and for timeouts;
	// both are results, not errors. A non-nil err here is a genuine exec failure.
	if res.TimedOut {
		status = "timeout"
	} else if err != nil {
		status = "error"
	} else {
		status = "ok"
	}
	out = ExecOutput{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, TimedOut: res.TimedOut}
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
