package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/ops"
)

func newSettingsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "settings",
		Short: "Show and change known vault settings",
	}
	c.AddCommand(
		newSettingsListCmd(), newSettingsGetCmd(),
		newSettingsSetCmd(), newSettingsUnsetCmd(),
	)
	return c
}

func newSettingsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List known settings, their current values, sources and effects",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			rows, err := ops.NewWithSettingInputs(st, ops.SettingInputsFromEnv()).ListSettings()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "键  当前值  默认值  取值来源  是否被覆盖  生效时机")
			for _, row := range rows {
				fmt.Fprintf(out, "%s  %s  %s  %s  %s  %s\n",
					row.Key, settingDisplay(row.CurrentValue), settingDisplay(row.DefaultValue),
					settingSourceText(row.Source), settingBooleanText(row.Overridden), row.Effect)
			}
			return nil
		},
	}
}

func newSettingsGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <键>",
		Short: "Show one known setting",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			row, err := findSetting(cmd, args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "键=%s\n当前值=%s\n默认值=%s\n取值来源=%s\n是否被覆盖=%s\n生效时机=%s\n",
				row.Key, settingDisplay(row.CurrentValue), settingDisplay(row.DefaultValue),
				settingSourceText(row.Source), settingBooleanText(row.Overridden), row.Effect)
			return nil
		},
	}
}

func newSettingsSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set <键> <值>",
		Short: "Validate and save one known setting",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			manager := ops.NewWithSettingInputs(st, ops.SettingInputsFromEnv())
			if err := manager.SetSetting(args[0], args[1], ops.CLISource()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已保存 %s=%s。\n", args[0], args[1])
			return nil
		},
	}
}

func newSettingsUnsetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unset <键>",
		Short: "Remove one known setting and restore its default value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			manager := ops.NewWithSettingInputs(st, ops.SettingInputsFromEnv())
			if err := manager.UnsetSetting(args[0], ops.CLISource()); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已恢复默认值：%s。\n", args[0])
			return nil
		},
	}
}

func findSetting(cmd *cobra.Command, key string) (ops.Setting, error) {
	st, err := openUnlockedStore()
	if err != nil {
		return ops.Setting{}, err
	}
	defer st.Close()
	rows, err := ops.NewWithSettingInputs(st, ops.SettingInputsFromEnv()).ListSettings()
	if err != nil {
		return ops.Setting{}, err
	}
	for _, row := range rows {
		if row.Key == key {
			return row, nil
		}
	}
	return ops.Setting{}, fmt.Errorf("未知设置项 %q", key)
}

func settingDisplay(value string) string {
	if value == "" {
		return "自动推导"
	}
	return value
}

func settingSourceText(source ops.SettingSource) string {
	switch source {
	case ops.SettingSourceStore:
		return "设置存储"
	case ops.SettingSourceFlag:
		return "命令行旗标"
	case ops.SettingSourceEnvironment:
		return "环境变量"
	case ops.SettingSourceDefault:
		return "默认值"
	default:
		return string(source)
	}
}

func settingBooleanText(value bool) string {
	if value {
		return "是"
	}
	return "否"
}
