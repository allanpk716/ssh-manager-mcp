package cli

import (
	"fmt"
	"os/user"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// killPollBudget: how long `tunnels kill` waits for a broker tick to apply
// the order (spec §4: 3× the 15s control interval).
const killPollBudget = 45 * time.Second

// staleHeartbeatSec: a registry row whose lease heartbeat is older than this
// is flagged as a probable ghost in `tunnels ls` (spec §6: 15s tick ×3).
const staleHeartbeatSec = 45

// osUser is the owner-action attribution stamped on kill orders (spec §7).
func osUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

func newTunnelsCmd() *cobra.Command {
	c := &cobra.Command{Use: "tunnels", Short: "Owner emergency stop for forward_port tunnels (kill / list live tunnels)"}
	c.AddCommand(tunnelsLsCmd(), tunnelsKillCmd())
	return c
}

func tunnelsLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Args:  cobra.NoArgs,
		Short: "List live tunnels (broker-held forwards, ≤45s mirror freshness)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			rows, err := s.ListTunnelRegistry()
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "(no open tunnels — note: tunnels held by OFFLINE cache clients are not in this domain; handle those on that machine)")
				return nil
			}
			now := time.Now().Unix()
			for _, r := range rows {
				flag := ""
				if now-r.LastRenewed > staleHeartbeatSec {
					flag = "  [stale heartbeat — probable ghost of a dead broker process, auto-cleared ≤30min]"
				}
				// LEFT JOIN projects via store reads (spec §5): the project row
				// may be gone (deleted mid-cascade-window) — project_id is
				// ALWAYS shown; the name is best-effort enrichment.
				proj := r.ProjectID
				if p, err := s.GetProject(r.ProjectID); err == nil && p != nil && p.Name != "" {
					proj = fmt.Sprintf("%s(%s)", p.Name, r.ProjectID)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s  project=%s  server=%s  %s -> %s  bind=%s  opened=%s  renewed=%s%s\n",
					r.TunnelID, proj, r.ServerID, r.LocalAddr, r.Remote, r.ListenHost,
					time.Unix(r.OpenedAt, 0).Format(time.RFC3339),
					time.Unix(r.LastRenewed, 0).Format(time.RFC3339), flag)
			}
			return nil
		},
	}
}

func tunnelsKillCmd() *cobra.Command {
	var project string
	c := &cobra.Command{
		Use:   "kill [<tunnel_id>]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Tear down a tunnel (or all of a project's tunnels via --project) — surgical, does not revoke the token",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Exactly-one-of target form: error iff (one positional) and
			// (--project set) agree — both set (ambiguous) or both unset (no
			// target). (len(args)==1)==(project!="") is precisely that.
			if (len(args) == 1) == (project != "") {
				return fmt.Errorf("pass exactly one of: a tunnel_id argument, or --project <name>")
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			var oid int64
			if len(args) == 1 {
				has, err := s.HasTunnelRegistryRow(args[0])
				if err != nil {
					return err
				}
				if !has {
					return fmt.Errorf("no open tunnel %s (this command covers brokers that write the authoritative vault — serve and online stdio; tunnels held by OFFLINE cache clients must be handled on that machine)", args[0])
				}
				oid, err = s.CreateTunnelOrder(args[0], "", osUser())
			} else {
				p, err := resolveProject(s, project)
				if err != nil {
					return err
				}
				if n, _ := s.CountTunnelRegistryProject(p.ID); n == 0 {
					return fmt.Errorf("no open tunnels for project %s", project)
				}
				oid, err = s.CreateTunnelOrder("", p.ID, osUser())
			}
			if err != nil {
				return err
			}
			return waitForOrder(cmd, s, oid, project != "")
		},
	}
	c.Flags().StringVar(&project, "project", "", "project name or id — kill ALL its tunnels")
	return c
}

// resolveProject resolves a --project value by name first, then by id
// (GetProjectByName / GetProject both return (nil, nil) on a miss).
func resolveProject(s *store.Store, nameOrID string) (*models.Project, error) {
	if p, err := s.GetProjectByName(nameOrID); err == nil && p != nil {
		return p, nil
	}
	if p, err := s.GetProject(nameOrID); err == nil && p != nil {
		return p, nil
	}
	return nil, fmt.Errorf("project %q not found", nameOrID)
}

// waitForOrder polls the order once per second for a broker to apply it
// (short-circuits on 'applied'). After the 45s budget it reports the pending
// state honestly instead of pretending success — with the offline-domain
// caveat, whose project variant also names the disable/revoke escalation for
// re-opening agents (spec §4/§5).
func waitForOrder(cmd *cobra.Command, s *store.Store, oid int64, isProject bool) error {
	deadline := time.Now().Add(killPollBudget)
	for time.Now().Before(deadline) {
		o, err := s.GetTunnelOrder(oid)
		if err != nil {
			return err
		}
		if o != nil && o.Outcome != nil && *o.Outcome == "applied" {
			fmt.Fprintln(cmd.OutOrStdout(), "applied")
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	if isProject {
		fmt.Fprintln(cmd.OutOrStdout(), "order pending — no broker applied it within 45s (a broker may be offline). The order only tears down tunnels that existed while it was pending; an agent that keeps RE-opening tunnels needs `projects disable/revoke` to stop it.")
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), "order pending — no broker applied it within 45s (target may belong to an offline/dead process; it will complete when a writable broker ticks)")
	return nil
}
