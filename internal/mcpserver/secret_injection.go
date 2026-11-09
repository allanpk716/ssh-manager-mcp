package mcpserver

import (
	"bytes"
	"sort"
	"strings"
	"unicode/utf8"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

const (
	injectionReasonUnknownShell = "服务器 shell 类型未知"
	injectionReasonPrivileged   = "Windows 服务器的提权层暂不支持环境变量注入"
	injectionReasonZeroByte     = "值含空字节"
	injectionReasonReadFailed   = "读取机密值失败"
	injectionReasonInvalidUTF8  = "Windows 上值不是合法 UTF-8"
	injectionReasonCmdNewline   = "cmd 下值含换行"
)

type environmentInjectionWarning struct {
	EnvironmentName string
	Reason          string
}

type environmentInjectionPlan struct {
	shellType  models.ShellType
	privileged bool
	items      []sshbroker.EnvironmentInjection
	warnings   []environmentInjectionWarning
}

func (p environmentInjectionPlan) injectedNames() []string {
	names := make([]string, 0, len(p.items))
	for _, item := range p.items {
		names = append(names, item.EnvironmentName)
	}
	return names
}

func (p environmentInjectionPlan) uninjectedVariables() []UninjectedEnvironmentVariable {
	variables := make([]UninjectedEnvironmentVariable, 0, len(p.warnings))
	for _, warning := range p.warnings {
		variables = append(variables, UninjectedEnvironmentVariable{
			EnvironmentName: warning.EnvironmentName,
			Reason:          warning.Reason,
		})
	}
	return variables
}

func (p environmentInjectionPlan) execOption() sshbroker.ExecOption {
	layer := sshbroker.ExecLayerNormal
	if p.privileged {
		layer = sshbroker.ExecLayerPrivileged
	}
	items := make([]sshbroker.ExecInjection, 0, len(p.items))
	for _, item := range p.items {
		items = append(items, item)
	}
	return sshbroker.WithPreCommandInjection(sshbroker.ExecInjectionOptions{
		ShellType:   string(models.NormalizeShellType(string(p.shellType))),
		TargetLayer: layer,
		Items:       items,
	})
}

func (p environmentInjectionPlan) appendWarnings(stdout string) string {
	warnings := p.warningLines()
	if len(warnings) == 0 {
		return stdout
	}
	if stdout != "" && !strings.HasSuffix(stdout, "\n") {
		stdout += "\n"
	}
	for _, warning := range warnings {
		stdout += warning + "\n"
	}
	return stdout
}

func (p environmentInjectionPlan) warningLines() []string {
	if len(p.warnings) == 0 {
		return nil
	}
	lines := make([]string, 0, len(p.warnings))
	for _, warning := range p.warnings {
		// Windows 提权层本来就不在注入范围内；这个原因只供 exec_context
		// 解释结构化名单，不追加到命令输出，避免改变既有命令行为。
		if warning.Reason == injectionReasonPrivileged {
			continue
		}
		lines = append(lines, "环境变量 "+warning.EnvironmentName+" 未注入："+warning.Reason)
	}
	return lines
}

func buildEnvironmentInjectionPlan(st *store.Store, server *models.Server, privileged bool) environmentInjectionPlan {
	plan := environmentInjectionPlan{shellType: server.ShellType, privileged: privileged}
	values, failedValues, err := st.SecretEnvironmentValuesForInjection()
	if err != nil {
		plan.addReadFailures(st, server)
		return plan
	}
	for _, failed := range failedValues {
		if serverWritable(failed.WritableServerIDs, server.ID) {
			plan.warn(failed.EnvironmentName, injectionReasonReadFailed)
		}
	}

	for _, value := range values {
		if value.EnvironmentName == "" || !serverWritable(value.WritableServerIDs, server.ID) {
			continue
		}
		switch {
		case unknownShellType(server.ShellType):
			plan.warn(value.EnvironmentName, injectionReasonUnknownShell)
			continue
		case privileged && models.NormalizeShellType(string(server.ShellType)) != models.ShellPOSIX:
			plan.warn(value.EnvironmentName, injectionReasonPrivileged)
			continue
		case bytes.IndexByte(value.Value, 0) >= 0:
			plan.warn(value.EnvironmentName, injectionReasonZeroByte)
			continue
		}
		normalizedShell := models.NormalizeShellType(string(server.ShellType))
		if normalizedShell == models.ShellWindowsCmd && windowsCmdValueHasLineBreak(value.Value) {
			plan.warn(value.EnvironmentName, injectionReasonCmdNewline)
			continue
		}
		if normalizedShell != models.ShellPOSIX && !utf8.Valid(value.Value) {
			plan.warn(value.EnvironmentName, injectionReasonInvalidUTF8)
			continue
		}
		plan.items = append(plan.items, sshbroker.EnvironmentInjection{
			EnvironmentName: value.EnvironmentName,
			Value:           append([]byte(nil), value.Value...),
		})
	}
	sort.Slice(plan.items, func(i, j int) bool {
		return plan.items[i].EnvironmentName < plan.items[j].EnvironmentName
	})
	sort.Slice(plan.warnings, func(i, j int) bool {
		return plan.warnings[i].EnvironmentName < plan.warnings[j].EnvironmentName
	})
	return plan
}

func windowsCmdValueHasLineBreak(value []byte) bool {
	return bytes.IndexByte(value, '\r') >= 0 || bytes.IndexByte(value, '\n') >= 0
}

func (p *environmentInjectionPlan) warn(name, reason string) {
	p.warnings = append(p.warnings, environmentInjectionWarning{EnvironmentName: name, Reason: reason})
}

func (p *environmentInjectionPlan) addReadFailures(st *store.Store, server *models.Server) {
	views, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil {
		return
	}
	for _, view := range views {
		if view.EnvironmentName == "" || !containsString(view.AllowedServers, server.Name) {
			continue
		}
		p.warn(view.EnvironmentName, injectionReasonReadFailed)
	}
	sort.Slice(p.warnings, func(i, j int) bool {
		return p.warnings[i].EnvironmentName < p.warnings[j].EnvironmentName
	})
}

func serverWritable(serverIDs []string, serverID string) bool {
	return containsString(serverIDs, serverID)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func unknownShellType(shell models.ShellType) bool {
	normalized := models.NormalizeShellType(string(shell))
	return normalized != models.ShellPOSIX && normalized != models.ShellPowerShell && normalized != models.ShellWindowsCmd
}
