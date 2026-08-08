package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newProjectsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "projects", Short: "Manage agent projects (each gets a token)"}
	cmd.AddCommand(projectsAddCmd(), projectsLsCmd())
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
			profs, err := s.ListProfiles()
			if err != nil {
				return err
			}
			var profileID string
			for _, p := range profs {
				if p.Name == profileName {
					profileID = p.ID
				}
			}
			if profileID == "" {
				return fmt.Errorf("profile %q not found", profileName)
			}
			_, token, err := s.AddProject(args[0], profileID)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Token (shown once): %s\n\n", token)
			fmt.Fprintln(out, ".mcp.json snippet:")
			fmt.Fprintf(out, `{"mcpServers":{"ssh":{"command":"ssh-manager","args":["mcp","--token","%s"]}}}`+"\n", token)
			return nil
		},
	}
	c.Flags().String("profile", "", "profile name to bind")
	_ = c.MarkFlagRequired("profile")
	return c
}

func projectsLsCmd() *cobra.Command {
	return &cobra.Command{
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
			for _, p := range projs {
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s token=%s… profile=%s\n", p.Name, p.ID, p.TokenPrefix, p.ProfileID)
			}
			return nil
		},
	}
}
