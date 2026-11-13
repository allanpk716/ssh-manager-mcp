package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/servermanage"
	"ssh-manager-mcp/internal/termdetect"
	"ssh-manager-mcp/internal/webui"
)

// serversStdinIsTTY 判断标准输入是否为交互终端；非交互终端的确认类命令要
// 求显式 --yes。
var serversStdinIsTTY = func() bool {
	return termdetect.IsTerminal(os.Stdin.Fd())
}

// writeServerDeletePreview 输出删除影响清单。清单由服务器管理模块组装，命
// 令行只负责排版。
func writeServerDeletePreview(w io.Writer, preview *servermanage.DeletePreview) {
	fmt.Fprintf(w, "删除影响预览：%s（标识符 %s）\n", preview.Server.Name, preview.Server.ID)
	if preview.HasDevice {
		fmt.Fprintf(w, "设备保留：%s\n", preview.DeviceName)
	} else {
		fmt.Fprintln(w, "所属设备：无")
	}
	fmt.Fprintf(w, "要删除的主机密钥锚：%d 个\n", len(preview.Anchors))
	fmt.Fprintf(w, "会失去这台服务器的 profile：%s\n", joinOrNone(preview.ProfileNames))
	fmt.Fprintf(w, "会从可写入服务器列表移出的机密项：%s\n", joinOrNone(preview.SecretNames))
	var deleted, kept []string
	for _, impact := range preview.Credentials {
		label := credentialImpactLabel(impact.Kind, impact.Type)
		if impact.Deleted {
			deleted = append(deleted, label)
		} else {
			kept = append(kept, fmt.Sprintf("%s（仍被 %d 台服务器引用）", label, impact.References))
		}
	}
	fmt.Fprintf(w, "会随之删除的凭据：%s\n", joinOrNone(deleted))
	fmt.Fprintf(w, "因共用而保留的凭据：%s\n", joinOrNone(kept))
	deliveries := make([]string, 0, len(preview.DeliveryRecords))
	for _, record := range preview.DeliveryRecords {
		deliveries = append(deliveries, fmt.Sprintf(
			"%s：%s（机密项 %s）", record.ServerName, record.Path, joinOrNone(record.SecretNames),
		))
	}
	fmt.Fprintf(w, "会随之删除的下发记录：%s\n", joinOrNone(deliveries))
}

func joinOrNone(items []string) string {
	if len(items) == 0 {
		return "无"
	}
	return strings.Join(items, "、")
}

func credentialImpactLabel(kind, typ string) string {
	kindLabel := "登录凭据"
	if kind == "sudo" {
		kindLabel = "sudo 凭据"
	}
	switch typ {
	case string(models.CredPassword), string(models.CredPrivateKey):
		return kindLabel + "（" + webui.CredentialTypeLabel(typ) + "）"
	default:
		return kindLabel
	}
}

// serversShowCmd 输出一台服务器的详情。单机与权威端读 vault；客户端读本实例
// 的本地加密快照。任何角色都不输出凭据内容。
func serversShowCmd() *cobra.Command {
	var instance string
	c := &cobra.Command{
		Use:   "show [name-or-id]",
		Short: "Show one server's details (never prints credential contents)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			launch, err := resolveMachineRole(instance)
			if err != nil {
				return err
			}
			if launch.Kind == roles.LaunchClient {
				return showSnapshotServer(cmd, args[0], instance)
			}
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			detail, err := servermanage.New(s).Detail(args[0])
			if err != nil {
				return err
			}
			if detail == nil {
				return fmt.Errorf("server %q not found", args[0])
			}
			writeServerDetail(cmd.OutOrStdout(), serverDetailText{
				ID: detail.Server.ID, Name: detail.Server.Name, User: detail.Server.User,
				Host: detail.Server.Host, Port: detail.Server.Port,
				Shell: string(detail.Server.ShellType), CredentialType: string(detail.CredentialType),
				HasKeyPassphrase: detail.HasKeyPassphrase, HasSudoPassword: detail.HasSudoPassword,
				Description: detail.Server.Description, Location: detail.Server.Location,
				Hardware: detail.Server.Hardware, Services: detail.Server.Services,
				Role: detail.Server.Role, Caveats: detail.Server.Caveats, Tags: detail.Server.Tags,
				ExposeHost: detail.Server.ExposeHost, DeviceName: detail.DeviceName,
				HasDevice: detail.HasDevice, AttentionReasons: detail.AttentionReasons,
				Reverse: &serverReverseDetailText{
					Secrets:               detail.Secrets,
					SharedStorages:        detail.SharedStorages,
					Profiles:              detail.Profiles,
					SiblingSharedStorages: detail.SiblingSharedStorages,
				},
			})
			return nil
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "client cache instance name (client role only)")
	return c
}

type serverDetailText struct {
	ID, Name, User, Host                                     string
	Port                                                     int
	Shell                                                    string
	CredentialType                                           string
	HasKeyPassphrase                                         bool
	HasSudoPassword                                          bool
	Description, Location, Hardware, Services, Role, Caveats string
	Tags                                                     []string
	ExposeHost                                               bool
	DeviceName                                               string
	HasDevice                                                bool
	AttentionReasons                                         []string
	// Reverse 是单机与权威端的反向授权分组；客户端本地快照后续只填裁剪
	// 后的机密项与共享存储两组，未装配时保持空指针。
	Reverse *serverReverseDetailText
}

type serverReverseDetailText struct {
	Secrets               []servermanage.DetailSecret
	SharedStorages        []servermanage.DetailSharedStorage
	Profiles              []servermanage.DetailProfile
	SiblingSharedStorages []servermanage.DetailSiblingSharedStorage
}

// writeServerDetail 是单机与客户端共用的详情排版；字段来源不同，内容一致。
func writeServerDetail(w io.Writer, d serverDetailText) {
	fmt.Fprintf(w, "服务器：%s（标识符 %s）\n", d.Name, d.ID)
	fmt.Fprintf(w, "连接：%s@%s:%d\n", d.User, d.Host, d.Port)
	fmt.Fprintf(w, "shell 类型：%s\n", models.NormalizeShellType(d.Shell))
	credential := "未挂凭据"
	switch d.CredentialType {
	case string(models.CredPassword):
		credential = "密码"
	case string(models.CredPrivateKey):
		credential = "私钥"
	}
	fmt.Fprintf(w, "登录凭据：%s\n", credential)
	if d.CredentialType == string(models.CredPrivateKey) {
		state := "未设置"
		if d.HasKeyPassphrase {
			state = "已设置"
		}
		fmt.Fprintf(w, "私钥口令：%s\n", state)
	}
	sudo := "未设置"
	if d.HasSudoPassword {
		sudo = "已设 sudo 密码"
	}
	fmt.Fprintf(w, "sudo 密码：%s\n", sudo)
	for _, item := range []struct{ label, value string }{
		{"说明", d.Description}, {"位置", d.Location}, {"硬件", d.Hardware},
		{"服务", d.Services}, {"角色", d.Role}, {"注意事项", d.Caveats},
	} {
		value := item.value
		if value == "" {
			value = "-"
		}
		fmt.Fprintf(w, "%s：%s\n", item.label, value)
	}
	tags := "-"
	if len(d.Tags) > 0 {
		tags = strings.Join(d.Tags, ",")
	}
	fmt.Fprintf(w, "标签：%s\n", tags)
	visibility := "对 agent 隐藏主机地址"
	if d.ExposeHost {
		visibility = "向 agent 展示主机地址"
	}
	fmt.Fprintf(w, "主机可见性：%s\n", visibility)
	device := "无"
	if d.HasDevice {
		device = d.DeviceName
	}
	fmt.Fprintf(w, "所属设备：%s\n", device)
	attention := "无"
	if labels := servermanage.ReasonLabels(d.AttentionReasons); len(labels) > 0 {
		attention = strings.Join(labels, "、")
	}
	fmt.Fprintf(w, "注意原因：%s\n", attention)
	if d.Reverse == nil {
		return
	}
	fmt.Fprintln(w, "机密项：")
	if len(d.Reverse.Secrets) == 0 {
		fmt.Fprintln(w, "  无")
	}
	for _, secret := range d.Reverse.Secrets {
		fmt.Fprintf(w, "  %s：备注 %s；环境变量名 %s\n", secret.Name,
			textOrDefault(secret.Note), textOrDefault(secret.EnvironmentName))
	}
	fmt.Fprintln(w, "共享存储：")
	if len(d.Reverse.SharedStorages) == 0 {
		fmt.Fprintln(w, "  无")
	}
	for _, share := range d.Reverse.SharedStorages {
		fmt.Fprintf(
			w, "  %s：提供方设备 %s；挂载用户 %s；挂载密码机密项 %s；挂载点 %s\n",
			share.Name, textOrDefault(share.ProviderDeviceName), textOrDefault(share.MountUser),
			textOrDefault(share.PasswordSecretName), share.MountPoint,
		)
	}
	fmt.Fprintln(w, "profile 授权：")
	if len(d.Reverse.Profiles) == 0 {
		fmt.Fprintln(w, "  无")
	}
	for _, profile := range d.Reverse.Profiles {
		fmt.Fprintf(w, "  %s\n", profile.Name)
	}
	fmt.Fprintln(w, "同设备其他服务器条目带来的共享存储：")
	if len(d.Reverse.SiblingSharedStorages) == 0 {
		fmt.Fprintln(w, "  无")
	}
	for _, share := range d.Reverse.SiblingSharedStorages {
		fmt.Fprintf(w, "  %s：来源服务器条目 %s；这台机器上的挂载点 %s\n",
			share.Name, share.SourceServerName, share.MountPoint)
	}
}

func textOrDefault(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// showSnapshotServer 在客户端角色上按本地快照输出详情。
func showSnapshotServer(cmd *cobra.Command, identifier, instance string) error {
	snap, err := clientops.LoadCacheSnapshotFor(instance)
	if err != nil {
		return err
	}
	credType := map[string]string{}
	hasPass := map[string]bool{}
	for _, cred := range snap.Credentials {
		credType[cred.ID] = cred.Type
		hasPass[cred.ID] = len(cred.Passphrase) > 0
	}
	deviceNames := map[string]string{}
	for _, device := range snap.Devices {
		deviceNames[device.ID] = device.Name
	}
	for i := range snap.Servers {
		srv := &snap.Servers[i]
		if srv.ID != identifier && srv.Name != identifier {
			continue
		}
		var tags []string
		if srv.TagsRaw != "" {
			_ = jsonUnmarshalStrings(srv.TagsRaw, &tags)
		}
		writeServerDetail(cmd.OutOrStdout(), serverDetailText{
			ID: srv.ID, Name: srv.Name, User: srv.User, Host: srv.Host, Port: srv.Port,
			Shell: srv.ShellType, CredentialType: credType[srv.CredentialID],
			HasKeyPassphrase: hasPass[srv.CredentialID], HasSudoPassword: srv.SudoCredentialID != "",
			Description: srv.Description, Location: srv.Location, Hardware: srv.Hardware,
			Services: srv.Services, Role: srv.Role, Caveats: srv.Caveats, Tags: tags,
			ExposeHost: srv.ExposeHost, DeviceName: deviceNames[srv.DeviceID],
			HasDevice: deviceNames[srv.DeviceID] != "",
			AttentionReasons: models.ServerAttentionReasons(
				srv.CredentialID, srv.Role, tags),
		})
		return nil
	}
	return fmt.Errorf("server %q not found in the local snapshot", identifier)
}

func jsonUnmarshalStrings(raw string, out *[]string) error {
	return json.Unmarshal([]byte(raw), out)
}

// serversCompleteCmd 是导入后的逐台补全命令行等价面：不给参数时补全全部需
// 要注意的服务器；每一步可以保存、跳过这一台或结束整个补全。
func serversCompleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "complete [name-or-id...]",
		Short: "Interactively complete server metadata, sudo password and conditional credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer s.Close()
			manager := servermanage.New(s)
			var queue []string
			if len(args) == 0 {
				items, err := manager.List(true)
				if err != nil {
					return err
				}
				for _, item := range items {
					queue = append(queue, item.Server.ID)
				}
			} else {
				for _, name := range args {
					detail, err := manager.Detail(name)
					if err != nil {
						return err
					}
					if detail == nil {
						return fmt.Errorf("server %q not found", name)
					}
					queue = append(queue, detail.Server.ID)
				}
			}
			return runServersComplete(cmd, queue)
		},
	}
}

// runServersComplete 逐台补全给定队列；`servers complete` 与导入结束后的询
// 问共用这一段。
func runServersComplete(cmd *cobra.Command, queue []string) error {
	s, err := openUnlockedStore()
	if err != nil {
		return err
	}
	defer s.Close()
	manager := servermanage.New(s)
	out := cmd.OutOrStdout()
	if len(queue) == 0 {
		fmt.Fprintln(out, "没有需要注意的服务器。")
		return nil
	}
	reader := bufio.NewReader(cmd.InOrStdin())
	for i, id := range queue {
		detail, err := manager.Detail(id)
		if err != nil {
			return err
		}
		if detail == nil {
			continue
		}
		fmt.Fprintf(out, "== %s（第 %d/%d 台）==\n", detail.Server.Name, i+1, len(queue))
		attention := strings.Join(servermanage.ReasonLabels(detail.AttentionReasons), "、")
		if attention != "" {
			fmt.Fprintf(out, "注意原因：%s\n", attention)
		}
		req := servermanage.CompleteRequest{Identifier: detail.Server.ID}
		req.Description = readCompletionField(reader, out, "说明", detail.Server.Description)
		req.Location = readCompletionField(reader, out, "位置", detail.Server.Location)
		req.Hardware = readCompletionField(reader, out, "硬件", detail.Server.Hardware)
		req.Services = readCompletionField(reader, out, "服务", detail.Server.Services)
		req.Role = readCompletionField(reader, out, "角色", detail.Server.Role)
		req.Caveats = readCompletionField(reader, out, "注意事项", detail.Server.Caveats)
		if detail.Server.CredentialID == "" {
			if value := readCompletionSecret(reader, out, "密码（现在设置，回车跳过）"); value != "" {
				req.Password = value
			}
		}
		if models.ServerHasTag(detail.Server.Tags, models.TagNeedsPassphrase) {
			if peers, err := manager.SharedCredentialServers(detail.Server.ID); err == nil && len(peers) > 0 {
				fmt.Fprintf(out, "补私钥口令会影响共用这把私钥的全部服务器：%s\n", strings.Join(peers, ","))
			} else {
				fmt.Fprintln(out, "补私钥口令会影响共用这把私钥的全部服务器。")
			}
			if value := readCompletionSecret(reader, out, "私钥口令（回车跳过）"); value != "" {
				req.KeyPassphrase = value
			}
		}
		if value := readCompletionSecret(reader, out, "sudo 密码（回车保持不变）"); value != "" {
			req.SudoPassword = value
		}
		fmt.Fprint(out, "操作：s=保存并继续，k=跳过这一台，e=结束补全 [s]：")
		action := strings.TrimSpace(readLine(reader))
		switch action {
		case "", "s":
			result, err := manager.Complete(req, servermanage.CLISource())
			if err != nil {
				return err
			}
			if result.NoOp {
				fmt.Fprintf(out, "%s：没有变化\n", detail.Server.Name)
			} else {
				fmt.Fprintf(out, "%s：已保存（%s）\n", detail.Server.Name,
					strings.Join(result.ChangedFields, ","))
			}
			if len(result.AffectedServers) > 1 {
				fmt.Fprintf(out, "补口令影响服务器：%s\n", strings.Join(result.AffectedServers, ","))
			}
		case "k":
			fmt.Fprintf(out, "%s：已跳过，保留注意原因\n", detail.Server.Name)
		case "e":
			writeCompleteSummary(out, manager, len(queue))
			fmt.Fprintln(out, "已结束补全。")
			return nil
		default:
			return fmt.Errorf("未知操作 %q（可用：s 保存并继续，k 跳过，e 结束）", action)
		}
	}
	writeCompleteSummary(out, manager, len(queue))
	fmt.Fprintln(out, "补全队列已完成。")
	return nil
}

func writeCompleteSummary(out io.Writer, manager *servermanage.Manager, total int) {
	items, err := manager.List(true)
	if err != nil {
		return
	}
	fmt.Fprintf(out, "结果：队列 %d 台，仍需注意 %d 台\n", total, len(items))
}

func readCompletionField(reader *bufio.Reader, out io.Writer, label, current string) *string {
	display := current
	if display == "" {
		display = "-"
	}
	fmt.Fprintf(out, "%s（当前：%s，回车保持不变）：", label, display)
	value := strings.TrimSpace(readLine(reader))
	if value == "" {
		return nil // 空输入表示保持当前值，不提交这个字段
	}
	return &value
}

func readCompletionSecret(reader *bufio.Reader, out io.Writer, label string) string {
	fmt.Fprintf(out, "%s：", label)
	if serversStdinIsTTY() {
		fmt.Fprint(out, "\n")
		value, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			return ""
		}
		return string(value)
	}
	return strings.TrimSpace(readLine(reader))
}

func readLine(reader *bufio.Reader) string {
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return ""
	}
	return strings.TrimRight(line, "\r\n")
}
