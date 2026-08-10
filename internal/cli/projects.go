package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

func newProjectsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "projects", Short: "Manage agent projects (each gets a token)"}
	cmd.AddCommand(
		projectsAddCmd(),
		projectsLsCmd(),
		projectsShowCmd(),
		projectsRotateCmd(),
		projectsStatusCmd("disable", "Disable a project's token (reversible via enable; Lazy — next mcp spawn rejects it)", models.ProjectDisabled, "project.disable"),
		projectsStatusCmd("enable", "Re-enable a disabled project", models.ProjectActive, "project.enable"),
		projectsStatusCmd("revoke", "Permanently revoke a project (token rejected; hidden from default ls)", models.ProjectRevoked, "project.revoke"),
	)
	return cmd
}

func projectsAddCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "add [name] --profile [profile]",
		Args:  cobra.ExactArgs(1),
		Short: "Create a project and print its one-time token + .mcp.json snippet",
		RunE: func(cmd *cobra.Command, args []string) error {
			profileName, _ := cmd.Flags().GetString("profile")
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			profileID, err := profileIDByName(s, profileName)
			if err != nil {
				return err
			}
			_, token, err := s.AddProject(args[0], profileID)
			if err != nil {
				return err
			}
			printToken(cmd.OutOrStdout(), token)
			return nil
		},
	}
	c.Flags().String("profile", "", "profile name to bind")
	_ = c.MarkFlagRequired("profile")
	return c
}

func projectsLsCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "ls",
		Short: "List projects (token prefix only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			projs, err := s.ListProjects()
			if err != nil {
				return err
			}
			pname, _ := profileNameMap(s)
			for _, p := range projs {
				if !all && p.Status == models.ProjectRevoked {
					continue // revoked = "deleted" in the owner's model → hidden by default
				}
				pn := pname[p.ProfileID]
				if pn == "" {
					pn = p.ProfileID
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s token=%s… status=%s profile=%s\n",
					p.Name, p.ID, p.TokenPrefix, p.Status, pn)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "include revoked projects")
	return c
}

// projectsShowCmd prints the agent→profile→servers view. It reads NO secret bytes (the iron
// rule holds on the owner side too).
func projectsShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Show a project: status, profile, and granted servers (no secrets)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			proj, err := s.GetProjectByName(args[0])
			if err != nil {
				return err
			}
			if proj == nil {
				return fmt.Errorf("project %q not found", args[0])
			}
			pname, _ := profileNameMap(s)
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "project: %s  (status: %s)\n", proj.Name, proj.Status)
			pn := pname[proj.ProfileID]
			if pn == "" {
				pn = proj.ProfileID
			}
			fmt.Fprintf(out, "profile: %s\n", pn)
			srvIDs, err := s.ServersForProfile(proj.ProfileID)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, "servers:")
			if len(srvIDs) == 0 {
				fmt.Fprintln(out, "  (none)")
				return nil
			}
			for _, sid := range srvIDs {
				srv, _ := s.GetServer(sid)
				if srv == nil {
					continue
				}
				fmt.Fprintf(out, "  - %-16s %s@%s:%d\n", srv.Name, srv.User, srv.Host, srv.Port)
			}
			return nil
		},
	}
}

// projectsRotateCmd re-keys a project's token in place (same id/profile) and prints the new
// token + .mcp.json snippet. The old token stops verifying immediately.
func projectsRotateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Re-key a project's token in place (old token dies; prints new token + .mcp.json)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			proj, err := s.GetProjectByName(args[0])
			if err != nil {
				return err
			}
			if proj == nil {
				return fmt.Errorf("project %q not found", args[0])
			}
			token, err := s.RotateProject(proj.ID)
			if err != nil {
				return err
			}
			if err := s.WriteAudit(store.AuditRow{TS: time.Now(), ProjectID: proj.ID, Action: "project.rotate", Status: "ok"}); err != nil {
				return err
			}
			printToken(cmd.OutOrStdout(), token)
			return nil
		},
	}
}

// projectsStatusCmd builds disable/enable/revoke: set the status, audit it, confirm.
func projectsStatusCmd(use, short string, status models.ProjectStatus, action string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " [name]",
		Args:  cobra.ExactArgs(1),
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			proj, err := s.GetProjectByName(args[0])
			if err != nil {
				return err
			}
			if proj == nil {
				return fmt.Errorf("project %q not found", args[0])
			}
			if err := s.SetProjectStatus(proj.ID, status); err != nil {
				return err
			}
			if err := s.WriteAudit(store.AuditRow{TS: time.Now(), ProjectID: proj.ID, Action: action, Status: "ok"}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s project %s (status=%s)\n", use, proj.Name, status)
			return nil
		},
	}
}

// profileIDByName resolves a profile name to its id.
func profileIDByName(s *store.Store, name string) (string, error) {
	profs, err := s.ListProfiles()
	if err != nil {
		return "", err
	}
	for _, p := range profs {
		if p.Name == name {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("profile %q not found", name)
}

// profileNameMap returns profileID→name for readable ls/show output.
func profileNameMap(s *store.Store) (map[string]string, error) {
	profs, err := s.ListProfiles()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range profs {
		out[p.ID] = p.Name
	}
	return out, nil
}

// printToken emits the one-time token + .mcp.json snippet (shared by add and rotate).
func printToken(out interface{ Write([]byte) (int, error) }, token string) {
	fmt.Fprintf(out, "Token (shown once): %s\n\n", token)
	fmt.Fprintln(out, ".mcp.json snippet:")
	fmt.Fprintf(out, `{"mcpServers":{"ssh":{"command":"ssh-manager","args":["mcp","--token","%s"]}}}`+"\n", token)
}
