package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/tui"
)

// secretsStdinIsTTY 判断标准输入是不是交互终端，与 cache instances 的终端
// 判定走同一个跨平台入口；测试通过替换这个函数构造交互/非交互两种形态。
var secretsStdinIsTTY = func() bool {
	return tui.IsTerminal(os.Stdin.Fd())
}

// secretsReadHiddenLine 在交互终端里读一行且不回显。单独成函数是为了让测试
// 在不依赖真实终端的情况下驱动两次输入的路径。
var secretsReadHiddenLine = func() ([]byte, error) {
	return term.ReadPassword(int(os.Stdin.Fd()))
}

// requireSecretsVault 断言本机持有 vault（权威端或单机部署）并返回已解锁的
// 存储。角色判定与 policy 命令组共用同一套判定（vault 优先，其次看客户端缓
// 存登记）。客户端上机密项是只读快照，写操作仍到权威端执行。
func requireSecretsVault() (*store.Store, error) {
	role, err := resolvePolicyRole()
	if err != nil {
		return nil, err
	}
	switch role {
	case policyRoleClient:
		return nil, errors.New("本机是缓存客户端：机密项在权威端维护，客户端只能执行只读的 secrets list——请在权威端执行这条命令")
	case policyRoleUnenrolled:
		return nil, errors.New("本机还没有 vault：机密项随 vault 一起存放，请先运行初始化向导（直接执行 sshmgr）")
	}
	return openUnlockedStore()
}

func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage write-only secrets (values can be overwritten, never read back)",
	}
	cmd.AddCommand(
		secretsAddCmd(),
		secretsSetCmd(),
		secretsEditCmd(),
		secretsRmCmd(),
		secretsListCmd(),
		secretsAllowCmd(),
		secretsDisallowCmd(),
	)
	return cmd
}

const secretsValueInputHelp = `值没有任何命令参数入口，以免进入 shell 历史与进程列表。取值优先级：
  1. --from-file PATH：按字节原样读取，末尾换行会保留；
  2. --stdin：读到输入结束，按字节原样读取，末尾换行会保留；
  3. 交互终端：关闭回显输入两次，两次不一致时报错；只取一行，不含换行符。
  4. 非交互终端且未给 --from-file / --stdin：报错，提示改用这两个参数。`

func secretsAddCmd() *cobra.Command {
	var note, fromFile string
	var fromStdin bool
	c := &cobra.Command{
		Use:   "add NAME [--note 文本] [--from-file PATH | --stdin]",
		Args:  cobra.ExactArgs(1),
		Short: "Register a secret (value is write-only)",
		Long: `Register a secret in the vault.

` + secretsValueInputHelp + `

--note 备注：备注对 agent 可见，不要写敏感内容。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSecretsVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := store.ValidateSecretName(args[0]); err != nil {
				return err
			}
			value, err := readSecretValueInput(cmd, fromFile, fromStdin)
			if err != nil {
				return err
			}
			if err := s.AddSecret(args[0], note, value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已登记机密项 %s\n", args[0])
			return nil
		},
	}
	c.Flags().StringVar(&note, "note", "", "备注（对 agent 可见，不要写敏感内容）")
	registerSecretValueInputFlags(c, &fromFile, &fromStdin)
	return c
}

// registerSecretValueInputFlags 注册新增与换值共用的两个值来源参数。
func registerSecretValueInputFlags(c *cobra.Command, fromFile *string, fromStdin *bool) {
	c.Flags().StringVar(fromFile, "from-file", "", "从文件按字节原样读取值（末尾换行保留）")
	c.Flags().BoolVar(fromStdin, "stdin", false, "从标准输入读到结束并按字节原样保存（末尾换行保留）")
}

func secretsSetCmd() *cobra.Command {
	var fromFile string
	var fromStdin bool
	c := &cobra.Command{
		Use:   "set NAME [--from-file PATH | --stdin]",
		Args:  cobra.ExactArgs(1),
		Short: "Overwrite a secret value (the only update path)",
		Long: `Overwrite a secret value.

` + secretsValueInputHelp + `

换值只改值、上一个值与值版本号；备注和可写入服务器列表保持不变。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSecretsVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := store.ValidateSecretName(args[0]); err != nil {
				return err
			}
			value, err := readSecretValueInput(cmd, fromFile, fromStdin)
			if err != nil {
				return err
			}
			if err := s.SetSecretValue(args[0], value); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已更换机密项 %s 的值\n", args[0])
			return nil
		},
	}
	registerSecretValueInputFlags(c, &fromFile, &fromStdin)
	return c
}

func secretsEditCmd() *cobra.Command {
	var note string
	c := &cobra.Command{
		Use:   "edit NAME --note 文本",
		Args:  cobra.ExactArgs(1),
		Short: "Change only a secret's note",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSecretsVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.EditSecretNote(args[0], note); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已更新机密项 %s 的备注\n", args[0])
			return nil
		},
	}
	c.Flags().StringVar(&note, "note", "", "新的备注（对 agent 可见，不要写敏感内容）")
	_ = c.MarkFlagRequired("note")
	return c
}

func secretsRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm NAME",
		Args:  cobra.ExactArgs(1),
		Short: "Delete a secret",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := requireSecretsVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if err := s.DeleteSecret(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "已删除机密项 %s\n", args[0])
			return nil
		},
	}
}

func secretsListCmd() *cobra.Command {
	var query, server, instance string
	c := &cobra.Command{
		Use:   "list [--query 文本] [--server 条目] [--instance 名称]",
		Args:  cobra.NoArgs,
		Short: "List secrets (never their values)",
		RunE: func(cmd *cobra.Command, args []string) error {
			views, err := loadSecretViews(cmd.ErrOrStderr(), instance, server)
			if err != nil {
				return err
			}
			if query != "" {
				views = filterSecretsByQuery(views, query)
			}
			out := cmd.OutOrStdout()
			for _, v := range views {
				printSecretView(out, v)
			}
			return nil
		},
	}
	c.Flags().StringVar(&query, "query", "", "只显示名字或备注包含该文本的机密项")
	c.Flags().StringVar(&server, "server", "", "只显示可写入列表包含该服务器条目的机密项")
	c.Flags().StringVar(&instance, "instance", "", policyInstanceFlagHelp)
	return c
}

// loadSecretViews 按本机角色读取机密项可见列表：权威端与单机部署从 vault
// 读；客户端从加密缓存快照读。两条路径共用存储层的服务器授权判定。
func loadSecretViews(errOut io.Writer, instance, server string) ([]store.SecretView, error) {
	role, err := resolvePolicyRoleFor(instance)
	if err != nil {
		return nil, err
	}
	switch role {
	case policyRoleAuthority:
		s, err := openUnlockedStore()
		if err != nil {
			return nil, err
		}
		defer s.Close()
		if server != "" {
			srv, err := s.GetServerByName(server)
			if err != nil {
				return nil, err
			}
			if srv == nil {
				return nil, fmt.Errorf("服务器条目 %q 不存在", server)
			}
		}
		views, err := s.ListSecretsForAuthorizedServers(store.AuthorizeAllSecretServers())
		if err != nil {
			return nil, err
		}
		if server == "" {
			return views, nil
		}
		return filterSecretsByServer(views, server), nil
	case policyRoleClient:
		snapshot, err := loadPolicyClientSnapshot(errOut, instance, "listing secrets")
		if err != nil {
			return nil, err
		}
		if snapshot == nil {
			return nil, errors.New("本机客户端缓存不可读：请先执行 cache pull，再查看机密项列表")
		}
		views := store.SecretViewsFromSnapshot(snapshot, store.AuthorizeAllSecretServers())
		if server == "" {
			return views, nil
		}
		return filterSecretsByServer(views, server), nil
	default:
		return nil, errors.New("本机还没有 vault：机密项随 vault 一起存放，请先运行初始化向导（直接执行 sshmgr）")
	}
}

func secretsAllowCmd() *cobra.Command {
	return newSecretAllowChangeCmd(true)
}

func secretsDisallowCmd() *cobra.Command {
	return newSecretAllowChangeCmd(false)
}

// newSecretAllowChangeCmd 构造授权与取消授权共用的命令：两者只有动作方向
// 与成功提示不同，参数解析与调用顺序保持一致。
func newSecretAllowChangeCmd(allow bool) *cobra.Command {
	name := "disallow"
	short := "Remove server entries from secrets' write lists"
	done := "已取消 %d 个机密项写入 %d 个服务器条目\n"
	if allow {
		name = "allow"
		short = "Add server entries to secrets' write lists"
		done = "已授权 %d 个机密项写入 %d 个服务器条目\n"
	}
	c := &cobra.Command{
		Use:   name + " --secrets NAME[,NAME...] --servers 条目[,条目...]",
		Args:  cobra.NoArgs,
		Short: short,
		RunE: func(cmd *cobra.Command, args []string) error {
			secrets, servers, err := secretAllowFlags(cmd)
			if err != nil {
				return err
			}
			s, err := requireSecretsVault()
			if err != nil {
				return err
			}
			defer s.Close()
			if allow {
				if err := s.AllowSecrets(secrets, servers); err != nil {
					return err
				}
			} else if err := s.DisallowSecrets(secrets, servers); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), done, len(secrets), len(servers))
			return nil
		},
	}
	registerSecretAllowFlags(c)
	return c
}

// registerSecretAllowFlags 注册授权与取消授权共用的两个多值参数。
func registerSecretAllowFlags(c *cobra.Command) {
	c.Flags().StringSlice("secrets", nil, "机密项名字（逗号分隔或重复参数，可给多个）")
	c.Flags().StringSlice("servers", nil, "服务器条目名（逗号分隔或重复参数，可给多个）")
}

// secretAllowFlags 读取 allow / disallow 的两个多值参数，并要求两个都给出。
func secretAllowFlags(cmd *cobra.Command) ([]string, []string, error) {
	secrets, _ := cmd.Flags().GetStringSlice("secrets")
	servers, _ := cmd.Flags().GetStringSlice("servers")
	if len(secrets) == 0 || len(servers) == 0 {
		return nil, nil, errors.New("--secrets 与 --servers 都至少要给一个值")
	}
	return secrets, servers, nil
}

// printSecretView 打印一个机密项的可见字段。绝不打印值、上一个值、值的哈
// 希、长度或值版本号。
func printSecretView(out io.Writer, v store.SecretView) {
	fmt.Fprintf(out, "%s\n", v.Name)
	fmt.Fprintf(out, "  备注: %s\n", v.Note)
	servers := strings.Join(v.AllowedServers, ", ")
	if servers == "" {
		servers = "（无）"
	}
	fmt.Fprintf(out, "  可写入服务器: %s\n", servers)
	fmt.Fprintf(out, "  创建时间: %s\n", formatSecretTime(v.CreatedAt))
	fmt.Fprintf(out, "  值最后更改: %s\n", formatSecretTime(v.ValueUpdatedAt))
}

func formatSecretTime(unix int64) string {
	return time.Unix(unix, 0).Format("2006-01-02 15:04:05 Z07:00")
}

func filterSecretsByServer(views []store.SecretView, server string) []store.SecretView {
	out := make([]store.SecretView, 0, len(views))
	for _, v := range views {
		for _, name := range v.AllowedServers {
			if name == server {
				out = append(out, v)
				break
			}
		}
	}
	return out
}

func filterSecretsByQuery(views []store.SecretView, query string) []store.SecretView {
	needle := strings.ToLower(query)
	out := make([]store.SecretView, 0, len(views))
	for _, v := range views {
		if strings.Contains(strings.ToLower(v.Name), needle) || strings.Contains(strings.ToLower(v.Note), needle) {
			out = append(out, v)
		}
	}
	return out
}

// readSecretValueInput 是新增与换值共用的值读取器。任何报错都不回显读到的
// 内容。
func readSecretValueInput(cmd *cobra.Command, fromFile string, fromStdin bool) ([]byte, error) {
	if fromFile != "" && fromStdin {
		return nil, errors.New("--from-file 与 --stdin 不能同时使用")
	}
	switch {
	case fromFile != "":
		return os.ReadFile(fromFile)
	case fromStdin:
		return io.ReadAll(cmd.InOrStdin())
	case secretsStdinIsTTY():
		errOut := cmd.ErrOrStderr()
		fmt.Fprint(errOut, "输入机密项的值（输入不回显）：")
		first, err := secretsReadHiddenLine()
		if err != nil {
			fmt.Fprintln(errOut)
			return nil, errors.New("读取机密项的值失败")
		}
		fmt.Fprintln(errOut)
		fmt.Fprint(errOut, "再输入一遍确认：")
		second, err := secretsReadHiddenLine()
		if err != nil {
			fmt.Fprintln(errOut)
			return nil, errors.New("读取机密项的确认值失败")
		}
		fmt.Fprintln(errOut)
		if string(first) != string(second) {
			return nil, errors.New("两次输入的机密项值不一致，未做任何改动")
		}
		return first, nil
	default:
		return nil, errors.New("标准输入不是交互终端，且未指定 --from-file 或 --stdin；请改用这两个参数之一录入机密项的值")
	}
}
