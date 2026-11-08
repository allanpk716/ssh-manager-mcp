package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/access"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// newProjectsCmd 生成 projects 命令族：访问管理模块（internal/access）项
// 目令牌部分的薄命令行面，解析参数、打印结果；生命周期规则与审计写入
// 由模块统一处理。show 是只读的项目详情视图，仍直接读存储。
func newProjectsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "projects", Short: "Manage agent projects (each gets a token)"}
	cmd.AddCommand(
		projectsAddCmd(),
		projectsLsCmd(),
		projectsShowCmd(),
		projectsRotateCmd(),
		projectsStatusCmd("disable", "Disable a project's token (reversible via enable; Lazy — next mcp spawn rejects it)", models.ProjectDisabled, func(m *access.Manager, project string) error {
			return m.DisableProject(project, access.CLISource())
		}),
		projectsStatusCmd("enable", "Re-enable a disabled project", models.ProjectActive, func(m *access.Manager, project string) error {
			return m.EnableProject(project, access.CLISource())
		}),
		projectsStatusCmd("revoke", "Permanently revoke a project (token rejected; hidden from default ls)", models.ProjectRevoked, func(m *access.Manager, project string) error {
			return m.RevokeProject(project, access.CLISource())
		}),
		projectsRemoveCmd(),
	)
	return cmd
}

// projectsRemoveCmd 删除已吊销的项目行：访问管理模块负责「先吊销再删
// 除」的规则与 project.delete 审计；审计仍按项目标识符保留历史。
func projectsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Hard-delete a REVOKED project row (audit history stays)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := access.New(s).DeleteProject(args[0], access.CLISource()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted project %s\n", args[0])
			return nil
		},
	}
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
			result, err := access.New(s).AddProject(args[0], profileName, access.CLISource())
			if err != nil {
				return err
			}
			printToken(cmd.OutOrStdout(), result.Token)
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
			projs, err := access.New(s).ListProjects(all)
			if err != nil {
				return err
			}
			for _, p := range projs {
				pn := p.ProfileName
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

// projectsShowCmd 打印项目→profile→服务器的只读视图，不读任何密钥字节。
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

// projectsRotateCmd 原地轮换项目令牌并打印新的一次性令牌与
// `.mcp.json` 片段；旧令牌立刻失效。
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
			result, err := access.New(s).RotateProject(args[0], access.CLISource())
			if err != nil {
				return err
			}
			printToken(cmd.OutOrStdout(), result.Token)
			return nil
		},
	}
}

// projectsStatusCmd 生成 disable/enable/revoke 三个子命令：apply 把命
// 令分发到访问管理模块的对应方法，模块负责状态规则与审计写入。
func projectsStatusCmd(use, short string, status models.ProjectStatus, apply func(m *access.Manager, project string) error) *cobra.Command {
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
			if err := apply(access.New(s), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s project %s (status=%s)\n", use, args[0], status)
			return nil
		},
	}
}

// profileIDByName maps a profile NAME to its id — the owner-facing flag is a
// name (profiles ls shows names); the store binds by id. Empty result errors
// with the list of known names so a typo is immediately self-correcting.
func profileIDByName(s *store.Store, name string) (string, error) {
	profiles, err := s.ListProfiles()
	if err != nil {
		return "", err
	}
	for _, p := range profiles {
		if p.Name == name {
			return p.ID, nil
		}
	}
	known := make([]string, 0, len(profiles))
	for _, p := range profiles {
		known = append(known, p.Name)
	}
	return "", fmt.Errorf("profile %q not found (known: %v)", name, known)
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

// profileNameFromMap renders a row's profile binding from a single-query
// profileNameMap: "-" = unbound legacy device code, "?" = bound to a profile
// deleted since the map was built (display-only race).
func profileNameFromMap(pname map[string]string, id string) string {
	if id == "" {
		return "-"
	}
	if n, ok := pname[id]; ok {
		return n
	}
	return "?"
}

// printToken 打印一次性令牌与 `.mcp.json` 片段（add 与 rotate 共用）。
// 片段由访问管理模块的共用函数生成，Web UI 的签发与轮换页面用同一份。
func printToken(out interface{ Write([]byte) (int, error) }, token string) {
	fmt.Fprintf(out, "Token (shown once): %s\n\n", token)
	fmt.Fprintln(out, ".mcp.json snippet:")
	fmt.Fprintln(out, access.ProjectTokenSnippet(token))
}
