package mcpserver

import (
	"bytes"
	"sort"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

const (
	injectionReasonUnknownShell = "服务器 shell 类型未知"
	injectionReasonNotPOSIX     = "服务器 shell 类型不是 POSIX"
	injectionReasonPrivileged   = "提权层暂不支持环境变量注入"
	injectionReasonZeroByte     = "值含空字节"
	injectionReasonReadFailed   = "读取机密值失败"
)

type environmentInjectionWarning struct {
	EnvironmentName string
	Reason          string
}

type environmentInjectionPlan struct {
	shellType models.ShellType
	items     []sshbroker.EnvironmentInjection
	warnings  []environmentInjectionWarning
}

func (p environmentInjectionPlan) injectedNames() []string {
	names := make([]string, 0, len(p.items))
	for _, item := range p.items {
		names = append(names, item.EnvironmentName)
	}
	return names
}

func (p environmentInjectionPlan) execOption() sshbroker.ExecOption {
	layer := sshbroker.ExecLayerNormal
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
		lines = append(lines, "环境变量 "+warning.EnvironmentName+" 未注入："+warning.Reason)
	}
	return lines
}

func buildEnvironmentInjectionPlan(st *store.Store, server *models.Server, privileged bool) environmentInjectionPlan {
	plan := environmentInjectionPlan{shellType: server.ShellType}
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
		case privileged:
			plan.warn(value.EnvironmentName, injectionReasonPrivileged)
			continue
		case unknownShellType(server.ShellType):
			plan.warn(value.EnvironmentName, injectionReasonUnknownShell)
			continue
		case models.NormalizeShellType(string(server.ShellType)) != models.ShellPOSIX:
			plan.warn(value.EnvironmentName, injectionReasonNotPOSIX)
			continue
		case bytes.IndexByte(value.Value, 0) >= 0:
			plan.warn(value.EnvironmentName, injectionReasonZeroByte)
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
