package sharemount

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"ssh-manager-mcp/internal/sshbroker"
)

// candidateAddressVariablesScript 生成候选地址的 shell 变量块。Linux 与
// macOS 的固定挂载命令共用同一份拼装方式，地址经单引号包装后写入命令
// 文本；候选顺序由调用方按共同网络规则排好。
func candidateAddressVariablesScript(candidates []*Candidate) string {
	var builder strings.Builder
	for index, candidate := range candidates {
		fmt.Fprintf(&builder, "candidate_%d_address=%s\n",
			index+1, sshbroker.POSIXShellQuote(candidate.Address))
	}
	return builder.String()
}

// candidateAddressSelectionScript 生成「优先保留现有地址」的公共片段：
// 现有地址仍出现在候选里就沿用它的序号，否则退回第一个候选。远端固定
// 命令在锁内执行这段逻辑，避免并发改写时换错地址。
// mountCommandOptions 区分 agent 的普通挂载与 owner 的重选地址。普通挂
// 载优先保留仍可连通的现有地址；重选地址总是采用候选列表中的第一个可达
// 地址，并在改写配置前处理占用。
type mountCommandOptions struct {
	PreferFirstCandidate bool
	Reselect             bool
	Force                bool
	LoginUser            string
}

func candidateAddressSelectionScript(
	candidates []*Candidate, preserveExisting bool,
) string {
	if !preserveExisting {
		firstCase := ""
		if len(candidates) > 0 {
			firstCase = "  1) sshmgr_selected_address=$candidate_1_address ;;\n"
		}
		return "sshmgr_selected_index=1\ncase \"$sshmgr_selected_index\" in\n" +
			firstCase + "esac\n"
	}
	selections := strings.Builder{}
	cases := strings.Builder{}
	for index := range candidates {
		number := index + 1
		fmt.Fprintf(&selections,
			"if [ \"$candidate_%d_address\" = \"$sshmgr_existing_address\" ]; then sshmgr_selected_index=%d; fi\n",
			number, number)
		fmt.Fprintf(&cases, "%d) sshmgr_selected_address=$candidate_%d_address ;;\n", number, number)
	}
	return "sshmgr_selected_index=0\n" + selections.String() + `if [ "$sshmgr_selected_index" -eq 0 ]; then
  sshmgr_selected_index=1
fi
case "$sshmgr_selected_index" in
` + cases.String() + "esac\n"
}

// remoteVersionGuardScript 生成远端版本比较片段。远端共享存储版本号比本
// 进程快照新时立即拒绝，且发生在写入任何文件之前。
func remoteVersionGuardScript(outputMarker string, exitCode int) string {
	return fmt.Sprintf(`if [ -n "$sshmgr_remote_version" ] && [ "$sshmgr_remote_version" -gt "$sshmgr_expected_version" ]; then
  echo %s remote-version-newer >&2
  exit %d
fi
`, sshbroker.POSIXShellQuote(outputMarker), exitCode)
}

// parseMountCommandOutput 解析远端固定挂载命令的安全输出行。输出只包含
// 动作、候选序号、macOS 主配置重建标记与 Linux 属主映射补充标记，不携
// 带地址、账号或机密。
func parseMountCommandOutput(
	stdout, outputMarker, systemName string,
) (action string, selectedIndex int, reset bool, ownershipMappingAdded bool, err error) {
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(strings.TrimRight(line, "\r"))
		if len(fields) == 0 || fields[0] != outputMarker {
			continue
		}
		for _, field := range fields[1:] {
			switch {
			case strings.HasPrefix(field, "result="):
				action = strings.TrimPrefix(field, "result=")
			case strings.HasPrefix(field, "selected="):
				value := strings.TrimPrefix(field, "selected=")
				parsed, parseErr := strconv.Atoi(value)
				if parseErr != nil || parsed < 1 {
					return "", 0, false, false, fmt.Errorf(
						"%s 固定挂载命令返回了非法候选序号", systemName)
				}
				selectedIndex = parsed
			case field == "reset=yes":
				reset = true
			case field == "ownership-mapping=added":
				ownershipMappingAdded = true
			}
		}
	}
	if action == "" || selectedIndex == 0 {
		return "", 0, false, false, fmt.Errorf("%s 固定挂载命令没有返回成功标记", systemName)
	}
	return action, selectedIndex, reset, ownershipMappingAdded, nil
}

// linuxFstabOptionParserScript 生成 fstab 选项识别的 shell 函数，挂载脚本
// 与现场检测脚本共用同一份实现；它只拆出 sshmgr 管理标记与协议版本，不
// 解析 owner 手写行的其他选项。
func linuxFstabOptionParserScript() string {
	return `sshmgr_parse_options() {
  sshmgr_option_share=''
  sshmgr_option_version=''
  sshmgr_option_vers=''
  sshmgr_option_uid=''
  sshmgr_option_gid=''
  sshmgr_saved_ifs=$IFS
  IFS=,
  set -f
  set -- $1
  set +f
  IFS=$sshmgr_saved_ifs
  for sshmgr_option in "$@"; do
    case "$sshmgr_option" in
      x-sshmgr-share=*|sshmgr-share=*)
        sshmgr_option_share=${sshmgr_option#x-sshmgr-share=}
        sshmgr_option_share=${sshmgr_option_share#sshmgr-share=}
      ;;
      x-sshmgr-version=*|sshmgr-version=*)
        sshmgr_option_version=${sshmgr_option#x-sshmgr-version=}
        sshmgr_option_version=${sshmgr_option_version#sshmgr-version=}
      ;;
      vers=*) sshmgr_option_vers=${sshmgr_option#vers=} ;;
      uid=*) sshmgr_option_uid=present ;;
      gid=*) sshmgr_option_gid=present ;;
    esac
  done
}

`
}

// mountTableCheckScript 生成「已挂载」核验片段：先访问挂载点触发自动挂
// 载，再查挂载表确认挂载点上确有对应文件系统，结论写入
// sshmgr_mounted（yes/no）。Linux 读 /proc/mounts，确认挂载点那一列等于
// 挂载点且类型为 cifs（/proc/mounts 把路径里的空格写成 \040；共享名已禁
// 止空白，无需解码），读不到时退回 mount 输出；macOS 用 mount 输出确认
// 存在「on <挂载点> (smbfs」的行；macOS 的 /mnt 位于数据卷，mount 输出
// 可能把同一挂载点写成 /System/Volumes/Data 前缀，因此两种路径都接受。
// 挂载工具的最终验证与现场检测共用这一判断，空目录不再被误判成已挂载。
func mountTableCheckScript(system string) string {
	switch system {
	case CheckSystemLinux:
		return `ls -A "$mount_point" >/dev/null 2>&1 || true
sshmgr_mounted=no
if [ -r /proc/mounts ]; then
  while IFS= read -r sshmgr_mount_entry || [ -n "$sshmgr_mount_entry" ]; do
    set -f
    set -- $sshmgr_mount_entry
    set +f
    if [ "$#" -ge 3 ] && [ "$2" = "$mount_point" ] && [ "$3" = cifs ]; then
      sshmgr_mounted=yes
      break
    fi
  done < /proc/mounts
else
  if mount | grep -qF " on $mount_point type cifs "; then
    sshmgr_mounted=yes
  fi
fi
`
	case CheckSystemDarwin:
		return `ls -A "$mount_point" >/dev/null 2>&1 || true
sshmgr_mounted=no
sshmgr_data_mount_point="/System/Volumes/Data$mount_point"
if mount | grep -qF " on $mount_point (smbfs" ||
   mount | grep -qF " on $sshmgr_data_mount_point (smbfs"; then
  sshmgr_mounted=yes
fi
`
	}
	return ""
}

// mountOutputUnknownAction 是固定挂载命令返回未知动作时的统一错误构造。
func mountOutputUnknownAction(systemName string) error {
	return errors.New(systemName + " 固定挂载命令返回了未知结果")
}
