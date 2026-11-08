package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/ops"
)

// newBackupConfigCmd 构建 `backup config show/set`，两者都使用标准位置的
// backup.json，与备份页和计划任务保持同一个事实源。
func newBackupConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Show or update backup.json",
	}
	c.AddCommand(newBackupConfigShowCmd(), newBackupConfigSetCmd())
	return c
}

func newBackupConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the three backup config fields",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			cfg, configPath, err := ops.New(st).ReadBackupConfig()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "config: "+configPath)
			fmt.Fprintln(out, "dir: "+cfg.Dir)
			fmt.Fprintf(out, "keep: %d\n", cfg.Keep)
			fmt.Fprintln(out, "passphrase_file: "+cfg.PassphraseFile)
			return nil
		},
	}
}

func newBackupConfigSetCmd() *cobra.Command {
	var dir string
	var keep int
	var passphraseFile string
	c := &cobra.Command{
		Use:   "set [--dir <dir>] [--keep <n>] [--passphrase-file <file>]",
		Short: "Update backup.json; omitted fields keep their current values",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			manager := ops.New(st)
			current, _, currentErr := manager.ReadBackupConfig()
			if currentErr == nil {
				if !cmd.Flags().Changed("dir") {
					dir = current.Dir
				}
				if !cmd.Flags().Changed("keep") {
					keep = current.Keep
				}
				if !cmd.Flags().Changed("passphrase-file") {
					passphraseFile = current.PassphraseFile
				}
			} else if !cmd.Flags().Changed("dir") || !cmd.Flags().Changed("passphrase-file") {
				return fmt.Errorf("backup config is not readable; pass --dir and --passphrase-file to create it (%w)", currentErr)
			}
			if err := manager.SaveBackupConfig(ops.BackupConfig{
				Dir: dir, Keep: keep, PassphraseFile: passphraseFile,
			}, ops.CLISource()); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "saved backup config")
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "backup target directory (absolute path)")
	c.Flags().IntVar(&keep, "keep", 7, "number of most-recent backups to keep (0 = no rotation)")
	c.Flags().StringVar(&passphraseFile, "passphrase-file", "", "passphrase file (absolute path, outside the backup directory)")
	return c
}
