package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/models"
)

func newServersCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "servers", Short: "Manage SSH target servers"}
	cmd.AddCommand(serversAddCmd(), serversListCmd(), serversRmCmd())
	return cmd
}

func serversAddCmd() *cobra.Command {
	var (
		name, host, user, password, keyPath, keyPass, sudoPassword string
		port                                                       int
		tags                                                       []string
	)
	c := &cobra.Command{
		Use:   "add",
		Short: "Add a server (with its credential)",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()
			if password != "" && keyPath != "" {
				return fmt.Errorf("--password and --key are mutually exclusive; provide exactly one")
			}
			if password == "" && keyPath == "" {
				return fmt.Errorf("required: exactly one of --password or --key")
			}
			var cred models.Credential
			if password != "" {
				cred = models.Credential{Type: models.CredPassword, Secret: []byte(password)}
			} else {
				keyBytes, err := readKeyFile(keyPath)
				if err != nil {
					return err
				}
				cred = models.Credential{Type: models.CredPrivateKey, Secret: keyBytes, Passphrase: []byte(keyPass)}
			}
			cid, err := s.SetCredential(&cred)
			if err != nil {
				return err
			}
			srv := &models.Server{
				Name: name, Host: host, Port: port, User: user,
				AuthMethod: cred.Type.AuthMethodForServer(), CredentialID: cid, Tags: tags,
			}
			if sudoPassword != "" {
				sid, err := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte(sudoPassword)})
				if err != nil {
					return err
				}
				srv.SudoCredentialID = sid
			}
			id, err := s.AddServer(srv)
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
	c.Flags().StringVar(&password, "password", "", "password auth (mutually exclusive with --key)")
	c.Flags().StringVar(&keyPath, "key", "", "path to private key file")
	c.Flags().StringVar(&keyPass, "key-passphrase", "", "passphrase for encrypted private key")
	c.Flags().StringVar(&sudoPassword, "sudo-password", "", "sudo password (enables sudo -S)")
	c.Flags().StringSliceVar(&tags, "tags", nil, "tags")
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
			s, err := openUnlockedStore(cmd)
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
				fmt.Fprintf(cmd.OutOrStdout(), "%-16s %-20s %s@%s:%d [%s]\n", srv.Name, srv.ID, srv.User, srv.Host, srv.Port, sudo)
			}
			return nil
		},
	}
}

func serversRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm [name-or-id]",
		Short: "Remove a server",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore(cmd)
			if err != nil {
				return err
			}
			defer s.Close()
			srv, _ := s.GetServerByName(args[0])
			id := args[0]
			if srv != nil {
				id = srv.ID
			}
			return s.DeleteServer(id)
		},
	}
}

// readKeyFile reads a private key from disk.
func readKeyFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
