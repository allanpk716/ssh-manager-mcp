package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
)

func newServersCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "servers", Short: "Manage SSH target servers"}
	cmd.AddCommand(serversAddCmd(), serversListCmd(), serversRmCmd(), serversEditCmd(), serversImportCmd())
	return cmd
}

func serversAddCmd() *cobra.Command {
	var (
		name, host, user, password, keyPath, keyPass, sudoPassword, description string
		location, hardware, services, role, caveats                             string
		port                                                                    int
		tags                                                                    []string
	)
	c := &cobra.Command{
		Use:   "add",
		Short: "Add a server (credential optional)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			// Credential is OPTIONAL (Plan 20 C0): --password/--key are mutually
			// exclusive, but a server may carry NEITHER (credential-less — e.g.
			// an ssh_config host without IdentityFile). exec on such a server
			// returns "no_credential" until one is attached via `servers edit`.
			if password != "" && keyPath != "" {
				return fmt.Errorf("--password and --key are mutually exclusive; provide one or neither")
			}
			srv := &models.Server{
				Name: name, Host: host, Port: port, User: user, Tags: tags,
				Description: strings.TrimSpace(description),
				Location:    strings.TrimSpace(location),
				Hardware:    strings.TrimSpace(hardware),
				Services:    strings.TrimSpace(services),
				Role:        strings.TrimSpace(role),
				Caveats:     strings.TrimSpace(caveats),
			}
			// Credentials are minted (or reused, when .ID is set) INSIDE the
			// same transaction as the server insert — a mid-way failure leaves
			// zero orphan credential rows (Plan 20 B1/G6).
			var cred, sudoCred *models.Credential
			switch {
			case password != "":
				cred = &models.Credential{Type: models.CredPassword, Secret: []byte(password)}
			case keyPath != "":
				keyBytes, err := readKeyFile(keyPath)
				if err != nil {
					return err
				}
				cred = &models.Credential{Type: models.CredPrivateKey, Secret: keyBytes, Passphrase: []byte(keyPass)}
			}
			if sudoPassword != "" {
				sudoCred = &models.Credential{Type: models.CredPassword, Secret: []byte(sudoPassword)}
			}
			id, err := s.AddServerWithCredentials(srv, cred, sudoCred)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "added server %s id=%s\n", name, id)
			return nil
		},
	}
	c.Flags().StringVar(&name, "name", "", "server name (unique)")
	c.Flags().StringVar(&host, "host", "", "hostname or IP")
	c.Flags().IntVar(&port, "port", 22, "port")
	c.Flags().StringVar(&user, "user", "", "ssh user")
	c.Flags().StringVar(&password, "password", "", "password auth (mutually exclusive with --key; omit both for a credential-less server)")
	c.Flags().StringVar(&keyPath, "key", "", "path to private key file (mutually exclusive with --password; omit both for a credential-less server)")
	c.Flags().StringVar(&keyPass, "key-passphrase", "", "passphrase for encrypted private key")
	c.Flags().StringVar(&sudoPassword, "sudo-password", "", "sudo password (enables sudo -S)")
	c.Flags().StringSliceVar(&tags, "tags", nil, "tags")
	c.Flags().StringVar(&description, "description", "", "owner notes (shown to the agent); prefer structured fields below")
	c.Flags().StringVar(&location, "location", "", "where deployed (datacenter/region/rack/tenant) — shown to the agent")
	c.Flags().StringVar(&hardware, "hardware", "", "hardware config (CPU/RAM/disk/GPU) — shown to the agent")
	c.Flags().StringVar(&services, "services", "", "what is deployed/running here — shown to the agent")
	c.Flags().StringVar(&role, "role", "", "this server's purpose (e.g. 'prod pg primary') — shown to the agent")
	c.Flags().StringVar(&caveats, "special-handling", "", "operational gotchas / special handling rules — the agent reads this BEFORE acting")
	_ = c.MarkFlagRequired("name")
	_ = c.MarkFlagRequired("host")
	_ = c.MarkFlagRequired("user")
	return c
}

func serversListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List servers",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			servers, err := s.ListServers()
			if err != nil {
				return err
			}
			for _, srv := range servers {
				sudo := "-"
				if srv.SudoCredentialID != "" {
					sudo = "sudo"
				}
				role := srv.Role
				if role == "" {
					role = "-"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s %s@%s:%d [%s] (%s) · %s\n",
					srv.Name, srv.ID, srv.User, srv.Host, srv.Port, sudo, role, truncate(srv.Caveats))
			}
			return nil
		},
	}
}

// truncate clips a free-text field for the ls line (rune-safe; "" → "-").
func truncate(s string) string {
	if s == "" {
		return "-"
	}
	if r := []rune(s); len(r) > 40 {
		return string(r[:37]) + "..."
	}
	return s
}

func serversRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm [name-or-id]",
		Short: "Remove a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			srv, _ := s.GetServerByName(args[0])
			id := args[0]
			if srv != nil {
				id = srv.ID
			}
			// Cascading: also drops credentials this server EXCLUSIVELY owned
			// (shared credentials survive — two-column reference check).
			return s.DeleteServerCascading(id)
		},
	}
}

// serversEditCmd edits a server in place: only flags the operator passed are applied
// (cobra Changed()), so the server id + profile bindings are preserved. Re-credential is
// just --password/--key (mutually exclusive): it mints a new credential and repoints the
// server's credential_id + auth_method.
func serversEditCmd() *cobra.Command {
	var (
		newName, host, user, password, keyPath, keyPass, sudoPassword, description string
		location, hardware, services, role, caveats                                string
		port                                                                       int
		tags                                                                       []string
		clearCred                                                                  bool
	)
	c := &cobra.Command{
		Use:   "edit [name]",
		Args:  cobra.ExactArgs(1),
		Short: "Edit a server's fields and/or re-credential (id + profile bindings preserved)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			srv, _ := s.GetServerByName(args[0])
			if srv == nil {
				return fmt.Errorf("server %q not found", args[0])
			}
			if cmd.Flags().Changed("name") {
				srv.Name = newName
			}
			if cmd.Flags().Changed("host") {
				srv.Host = host
			}
			if cmd.Flags().Changed("port") {
				srv.Port = port
			}
			if cmd.Flags().Changed("user") {
				srv.User = user
			}
			if cmd.Flags().Changed("description") {
				srv.Description = strings.TrimSpace(description)
			}
			if cmd.Flags().Changed("tags") {
				srv.Tags = tags
			}
			if cmd.Flags().Changed("location") {
				srv.Location = strings.TrimSpace(location)
			}
			if cmd.Flags().Changed("hardware") {
				srv.Hardware = strings.TrimSpace(hardware)
			}
			if cmd.Flags().Changed("services") {
				srv.Services = strings.TrimSpace(services)
			}
			if cmd.Flags().Changed("role") {
				srv.Role = strings.TrimSpace(role)
			}
			if cmd.Flags().Changed("special-handling") {
				srv.Caveats = strings.TrimSpace(caveats)
			}
			// Re-credential (optional; mutually exclusive). nil cred/sudo keeps
			// the existing rows; a swap runs in ONE transaction with the row
			// update, dropping the replaced row when nothing else references
			// it (two-column check) — no orphan rows, no stranded servers.
			pwSet := cmd.Flags().Changed("password")
			keySet := cmd.Flags().Changed("key")
			if pwSet && keySet {
				return fmt.Errorf("--password and --key are mutually exclusive; provide exactly one")
			}
			// --clear-credential is the reverse operation: reset the server to
			// the credential-less form (Plan 21 A2). It is an EXCLUSIVE action —
			// mutex with re-credential, and the early return means sudo is
			// cleared along with the login credential rather than set, and any
			// field flags passed in the same invocation are not applied.
			if clearCred && (pwSet || keySet) {
				return fmt.Errorf("--clear-credential is mutually exclusive with --password/--key")
			}
			if clearCred {
				if err := s.ClearServerCredential(srv.ID); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "cleared credentials for %s\n", srv.Name)
				return nil
			}
			// An explicitly-passed empty/whitespace value is a quoting or
			// typo artifact, NOT a "clear my credential" request: minting an
			// empty-secret row would brick exec with a confusing auth failure
			// (Plan 22 T2). Clearing is --clear-credential's exclusive job.
			if pwSet && strings.TrimSpace(password) == "" {
				return fmt.Errorf("--password 不能为空（更换凭据请给新值；清除凭据请用 --clear-credential）")
			}
			if keySet && strings.TrimSpace(keyPath) == "" {
				return fmt.Errorf("--key 不能为空（更换凭据请给新值；清除凭据请用 --clear-credential）")
			}
			if cmd.Flags().Changed("sudo-password") && strings.TrimSpace(sudoPassword) == "" {
				return fmt.Errorf("--sudo-password 不能为空（更换凭据请给新值）")
			}
			var cred, sudoCred *models.Credential
			switch {
			case pwSet:
				cred = &models.Credential{Type: models.CredPassword, Secret: []byte(password)}
			case keySet:
				keyBytes, err := readKeyFile(keyPath)
				if err != nil {
					return err
				}
				cred = &models.Credential{Type: models.CredPrivateKey, Secret: keyBytes, Passphrase: []byte(keyPass)}
			}
			if pwSet || keySet {
				// ANY successful re-credential resolves needs-passphrase (the
				// tag the import flows write means "current credential lacks
				// its passphrase"). Applied AFTER the --tags replacement above
				// and strips from whatever srv.Tags holds then — an explicit
				// --tags list passed alongside --key/--password loses the tag
				// too: after re-credential the marker would be false.
				srv.Tags = stripTag(srv.Tags, "needs-passphrase")
			}
			if cmd.Flags().Changed("sudo-password") {
				sudoCred = &models.Credential{Type: models.CredPassword, Secret: []byte(sudoPassword)}
			}
			if err := s.UpdateServerWithCredentials(srv, cred, sudoCred); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "updated server %s\n", srv.Name)
			return nil
		},
	}
	c.Flags().StringVar(&newName, "name", "", "rename the server")
	c.Flags().StringVar(&host, "host", "", "hostname or IP")
	c.Flags().IntVar(&port, "port", 22, "port")
	c.Flags().StringVar(&user, "user", "", "ssh user")
	c.Flags().StringVar(&description, "description", "", "owner notes (shown to the agent); prefer structured fields below")
	c.Flags().StringVar(&location, "location", "", "where deployed (datacenter/region/rack/tenant) — shown to the agent")
	c.Flags().StringVar(&hardware, "hardware", "", "hardware config (CPU/RAM/disk/GPU) — shown to the agent")
	c.Flags().StringVar(&services, "services", "", "what is deployed/running here — shown to the agent")
	c.Flags().StringVar(&role, "role", "", "this server's purpose — shown to the agent")
	c.Flags().StringVar(&caveats, "special-handling", "", "operational gotchas / special handling rules — pass \"\" to clear")
	c.Flags().StringSliceVar(&tags, "tags", nil, "tags (replaces existing)")
	c.Flags().StringVar(&password, "password", "", "switch to / replace password auth")
	c.Flags().StringVar(&keyPath, "key", "", "switch to / replace key auth (path to private key)")
	c.Flags().StringVar(&keyPass, "key-passphrase", "", "passphrase for encrypted private key (use with --key)")
	c.Flags().StringVar(&sudoPassword, "sudo-password", "", "set / replace sudo password")
	c.Flags().BoolVar(&clearCred, "clear-credential", false, "remove the server's credentials (back to credential-less)")
	return c
}

// readKeyFile reads a private key from disk.
func readKeyFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// stripTag returns tags minus every occurrence of tag (the CLI-side twin of
// the TUI's dropTag; an all-matches list yields a valid empty slice).
func stripTag(tags []string, tag string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != tag {
			out = append(out, t)
		}
	}
	return out
}
