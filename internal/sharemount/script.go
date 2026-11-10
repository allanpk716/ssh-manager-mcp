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
func candidateAddressSelectionScript(candidates []*Candidate) string {
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
// 动作、候选序号与 macOS 主配置重建标记，不携带地址、账号或机密。
func parseMountCommandOutput(stdout, outputMarker, systemName string) (action string, selectedIndex int, reset bool, err error) {
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
					return "", 0, false, fmt.Errorf(
						"%s 固定挂载命令返回了非法候选序号", systemName)
				}
				selectedIndex = parsed
			case field == "reset=yes":
				reset = true
			}
		}
	}
	if action == "" || selectedIndex == 0 {
		return "", 0, false, fmt.Errorf("%s 固定挂载命令没有返回成功标记", systemName)
	}
	return action, selectedIndex, reset, nil
}

// linuxFstabOptionParserScript 生成 fstab 选项识别的 shell 函数，挂载脚本
// 与现场检测脚本共用同一份实现；它只拆出 sshmgr 管理标记与协议版本，不
// 解析 owner 手写行的其他选项。
func linuxFstabOptionParserScript() string {
	return `sshmgr_parse_options() {
  sshmgr_option_share=''
  sshmgr_option_version=''
  sshmgr_option_vers=''
  sshmgr_saved_ifs=$IFS
  IFS=,
  set -f
  set -- $1
  set +f
  IFS=$sshmgr_saved_ifs
  for sshmgr_option in "$@"; do
    case "$sshmgr_option" in
      sshmgr-share=*) sshmgr_option_share=${sshmgr_option#sshmgr-share=} ;;
      sshmgr-version=*) sshmgr_option_version=${sshmgr_option#sshmgr-version=} ;;
      vers=*) sshmgr_option_vers=${sshmgr_option#vers=} ;;
    esac
  done
}
`
}

// mountOutputUnknownAction 是固定挂载命令返回未知动作时的统一错误构造。
func mountOutputUnknownAction(systemName string) error {
	return errors.New(systemName + " 固定挂载命令返回了未知结果")
}
