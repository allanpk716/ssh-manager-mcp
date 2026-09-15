package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// ownerSSHDeadline bounds the WHOLE owner ssh invocation — connect AND exec
// share it (was: connect used context.Background() and could hang for the OS
// TCP timeout on an unreachable host). ssh.Dial cannot be interrupted;
// sshbroker.Connect abandons the dial at ctx expiry (client.go contract).
var ownerSSHDeadline = 120 * time.Second

func newSSHCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ssh <host-name> [command...]",
		Short: "Owner full-access SSH exec (no profile limit). Runs the command on the named server.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The owner ssh path is a SINGLE non-interactive command (no PTY).
			// Empty/whitespace joins would otherwise run an empty command on
			// the remote host — fail fast instead (Plan 25 / xcheck A4).
			if len(args) == 1 || strings.TrimSpace(strings.Join(args[1:], " ")) == "" {
				return fmt.Errorf("no command given: usage sshmgr ssh <server> <command...> (single non-interactive command; for an interactive terminal use your own ssh client)")
			}
			ctx, cancel := context.WithTimeout(context.Background(), ownerSSHDeadline)
			defer cancel()
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			srv, err := st.GetServerByName(args[0])
			if err != nil {
				return err
			}
			if srv == nil {
				return fmt.Errorf("server %q not found", args[0])
			}
			auth, err := vault.AuthForServer(st, srv)
			if err != nil {
				return err
			}
			hkCb, err := sshbroker.HostKeyTOFU(st, srv.Host, srv.Port)
			if err != nil {
				return err
			}
			start := time.Now()
			commandStr := strings.Join(args[1:], " ")
			status := "ok"
			var res sshbroker.ExecResult
			cli, err := sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)
			if err != nil {
				status = "error"
				_ = st.WriteAudit(store.AuditRow{TS: start, ServerID: srv.ID, Action: "exec", Command: commandStr, Status: status, DurationMS: time.Since(start).Milliseconds()})
				return err
			}
			defer cli.Close()
			res, err = cli.Exec(ctx, commandStr, ownerSSHDeadline, 0) // owner path: unlimited output; shared deadline caps total time
			if err != nil {
				status = "error"
			}
			if res.TimedOut {
				status = "timeout"
			}
			_ = st.WriteAudit(store.AuditRow{
				TS: start, ServerID: srv.ID, Action: "exec", Command: commandStr,
				Status: status, ExitCode: res.ExitCode, DurationMS: time.Since(start).Milliseconds(),
			})
			out := cmd.OutOrStdout()
			fmt.Fprint(out, res.Stdout)
			fmt.Fprint(cmd.ErrOrStderr(), res.Stderr)
			if res.ExitCode != 0 {
				cmd.SilenceErrors = true
				cmd.SilenceUsage = true
				// Output above is already printed; surface the remote exit
				// code as the CLI's own non-zero exit (A4 — was swallowed).
				return fmt.Errorf("remote command exited with code %d", res.ExitCode)
			}
			return nil
		},
	}
	return c
}
