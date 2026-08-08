package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/store"
)

func newUnlockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlock",
		Short: "Resolve the master key and print SSHMGR_MASTERKEY_HEX for the current shell",
		RunE: func(cmd *cobra.Command, args []string) error {
			kp := store.KeyringKeyProvider{}
			mk, err := kp.Get()
			if err == store.ErrNotFound {
				// first run: generate + store in keychain
				mk, err = store.GenerateMasterKey()
				if err != nil {
					return err
				}
				if err := kp.Set(mk); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "export SSHMGR_MASTERKEY_HEX=%s\n", hexEncode(mk))
			return nil
		},
	}
}

func newLockCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lock",
		Short: "Clear the master key from this shell",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), "unset SSHMGR_MASTERKEY_HEX")
			os.Unsetenv("SSHMGR_MASTERKEY_HEX")
			return nil
		},
	}
}
