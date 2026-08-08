package mcpserver

import (
	"context"
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
func ExecCommandForProfile(ctx context.Context, st *store.Store, profileID, serverID, command string, sudo bool, timeout time.Duration) (ExecOutput, error) {
	allowed, err := st.ServersForProfile(profileID)
	if err != nil {
		return ExecOutput{}, err
	}
	if !contains(allowed, serverID) {
		return ExecOutput{}, ErrNotInProfile
	}
	srv, err := st.GetServer(serverID)
	if err != nil || srv == nil {
		return ExecOutput{}, fmt.Errorf("server %s not found", serverID)
	}
	auth, err := vault.AuthForServer(st, srv)
	if err != nil {
		return ExecOutput{}, err
	}
	hkCb, _ := sshbroker.HostKeyTOFU(st, srv.Host)
	cli, err := sshbroker.Connect(srv.Host, srv.Port, srv.User, auth, hkCb)
	if err != nil {
		return ExecOutput{}, err
	}
	defer cli.Close()

	if timeout <= 0 {
		timeout = defaultTimeout
	}
	start := time.Now()
	var res sshbroker.ExecResult
	status := "ok"
	if sudo {
		if srv.SudoCredentialID == "" {
			return ExecOutput{}, fmt.Errorf("sudo not configured for server %s (call list_servers: has_sudo tells you)", srv.Name)
		}
		sudoCred, err := st.GetCredential(srv.SudoCredentialID)
		if err != nil || sudoCred == nil {
			return ExecOutput{}, fmt.Errorf("sudo credential for %s not found", srv.Name)
		}
		res, err = cli.ExecSudo(command, sudoCred.Secret, timeout)
	} else {
		res, err = cli.Exec(command, timeout)
	}
	if res.TimedOut {
		status = "timeout"
	} else if err != nil {
		status = "error"
	}
	_ = st.WriteAudit(store.AuditRow{
		TS: start, ServerID: serverID, Action: "exec", Command: command,
		Sudo: sudo, Status: status, ExitCode: res.ExitCode, DurationMS: time.Since(start).Milliseconds(),
	})
	// Connect/exec errors that weren't exit codes surface here; non-zero exit is a result, not an error.
	return ExecOutput{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, TimedOut: res.TimedOut}, err
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
