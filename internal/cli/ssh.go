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

func newSSHCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "ssh <host-name> [command...]",
		Short: "Owner full-access SSH exec (no profile limit). Runs the command on the named server.",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
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
			cli, err := sshbroker.Connect(srv.Host, srv.Port, srv.User, auth, hkCb)
			if err != nil {
				status = "error"
				_ = st.WriteAudit(store.AuditRow{TS: start, ServerID: srv.ID, Action: "exec", Command: commandStr, Status: status, DurationMS: time.Since(start).Milliseconds()})
				return err
			}
			defer cli.Close()
			res, err = cli.Exec(context.Background(), commandStr, 120*time.Second, 0) // owner path: unlimited output
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
			}
			return nil
		},
	}
	return c
}
