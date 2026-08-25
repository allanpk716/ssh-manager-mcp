package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newServeBindCmd: `ssh-manager serve bind add|rm|ls <ip>` — the owner's
// pre-approved non-loopback bind host whitelist (spec §2). Reads happen
// per-call in ForwardForProfile, so a change takes effect on the next
// forward_port without restarting serve.
func newServeBindCmd() *cobra.Command {
	c := &cobra.Command{Use: "bind", Short: "Manage the owner-approved non-loopback bind host whitelist (forward_port listen_host)"}
	c.AddCommand(serveBindAddCmd(), serveBindRmCmd(), serveBindLsCmd())
	return c
}

func serveBindAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <ip>",
		Args:  cobra.ExactArgs(1),
		Short: "Approve a non-loopback IP for forward_port listen_host",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.AddForwardBindHost(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "approved %s\n", args[0])
			return nil
		},
	}
}

func serveBindRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <ip>",
		Args:  cobra.ExactArgs(1),
		Short: "Revoke an approved bind host (existing tunnels bound to it close within ~15s)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			ok, err := s.RemoveForwardBindHost(args[0])
			if err != nil {
				return err
			}
			if !ok {
				fmt.Fprintf(cmd.OutOrStdout(), "no whitelist entry for %s\n", args[0])
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "revoked %s (existing tunnels on it close within ~15s; loopback is unaffected)\n", args[0])
			return nil
		},
	}
}

func serveBindLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Args:  cobra.NoArgs,
		Short: "List the approved bind hosts",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			hosts, err := s.ListForwardBindHosts()
			if err != nil {
				return err
			}
			if len(hosts) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(empty — only loopback binds allowed)")
				return nil
			}
			for _, h := range hosts {
				fmt.Fprintln(cmd.OutOrStdout(), h)
			}
			return nil
		},
	}
}
