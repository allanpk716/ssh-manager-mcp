package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newProfilesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "profiles", Short: "Manage server profiles (groups)"}
	cmd.AddCommand(profilesAddCmd(), profilesLsCmd(), profilesGrantCmd(), profilesRemoveCmd())
	return cmd
}

// profilesRemoveCmd deletes a profile by name. The store REFUSES while any
// project still references it (error names the projects) — silently unbinding
// would brick those projects' agents. Grant rows are removed with it.
func profilesRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Delete a profile (refuses while projects reference it)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			profs, err := s.ListProfiles()
			if err != nil {
				return err
			}
			var profileID string
			for _, p := range profs {
				if p.Name == args[0] {
					profileID = p.ID
				}
			}
			if profileID == "" {
				return fmt.Errorf("profile %q not found", args[0])
			}
			if err := s.DeleteProfile(profileID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted profile %s\n", args[0])
			return nil
		},
	}
}

func profilesAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Create a profile",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			id, err := s.AddProfile(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "created profile %s id=%s\n", args[0], id)
			return nil
		},
	}
}

func profilesLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List profiles",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			profs, err := s.ListProfiles()
			if err != nil {
				return err
			}
			for _, p := range profs {
				srvs, _ := s.ServersForProfile(p.ID)
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s servers=%d\n", p.Name, p.ID, len(srvs))
			}
			return nil
		},
	}
}

func profilesGrantCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grant [profile] [server1 server2 ...]",
		Args:  cobra.MinimumNArgs(2),
		Short: "Grant servers to a profile (by name)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			profs, err := s.ListProfiles()
			if err != nil {
				return err
			}
			var profileID string
			for _, p := range profs {
				if p.Name == args[0] {
					profileID = p.ID
				}
			}
			if profileID == "" {
				return fmt.Errorf("profile %q not found", args[0])
			}
			var serverIDs []string
			for _, name := range args[1:] {
				srv, _ := s.GetServerByName(name)
				if srv == nil {
					return fmt.Errorf("server %q not found", name)
				}
				serverIDs = append(serverIDs, srv.ID)
			}
			if err := s.GrantServers(profileID, serverIDs); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "granted %d server(s) to %s\n", len(serverIDs), args[0])
			return nil
		},
	}
}
