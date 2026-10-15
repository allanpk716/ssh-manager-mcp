package policy

import (
	"fmt"
	"regexp"
	"strings"
)

// PathCategory 是自定义规则「目标路径类别」字段的可选值(领域定义见
// CONTEXT.md「命令策略」):空串表示不限目标路径。
const (
	// PathCategoryCritical 表示目标参数里含系统关键路径(Linux/macOS 两套
	// 内置集合,与默认删除规则用同一张表)。
	PathCategoryCritical = "critical"
	// PathCategoryDisk 表示目标参数指向 /dev/ 下的磁盘设备(与「直接写
	// 磁盘设备」默认规则用同一张表)。
	PathCategoryDisk = "disk"
)

// knownPathCategories 是目标路径类别的全部已知值。
var knownPathCategories = map[string]bool{
	"":                   true,
	PathCategoryCritical: true,
	PathCategoryDisk:     true,
}

// CustomRule 是 owner 在权威端增加的自定义命令策略规则(领域定义见
// CONTEXT.md「默认规则 / 自定义规则」):结构化字段描述——编号、适用
// shell 类型集合、命令名及别名、参数条件、目标路径类别、处置、风险
// 说明、更安全的替代做法;正则是高级选项,处理结构化字段表达不了的
// 情况。非空的匹配条件之间是「同时满足」(AND)。
type CustomRule struct {
	ID           string      // 规则编号,owner 自定,唯一
	Shells       []ShellType // 适用 shell 类型集合;空 = 全部三类
	Commands     []string    // 命令名及别名(剥掉 sudo 等前缀后匹配);空 = 不按命令名限定
	Args         []string    // 参数条件:每个都必须逐字出现在参数里;空 = 无参数条件
	PathCategory string      // 目标路径类别:""=不限;critical=系统关键路径;disk=磁盘设备
	Regex        string      // 可选正则,对整条命令原文匹配;空 = 不用正则
	Disposition  Disposition // 命中后的处置:拦截或需确认
	Explanation  string      // 风险说明(为什么危险),命中时返回给 agent
	Alternative  string      // 更安全的替代做法,命中时返回给 agent
}

// Validate 校验一条自定义规则的自描述完整性:处置只能是拦截或需确认
// (放行无需规则)、shell 类型与目标路径类别必须是已知值、正则必须能
// 编译、至少要有一个匹配条件(否则规则会拦下一切命令)、风险说明必填
// (命中时的一行反馈里必须有「为什么危险」)。入库前由管理层调用。
func (r CustomRule) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("custom rule id is required")
	}
	if r.Disposition != DispositionBlock && r.Disposition != DispositionConfirmRequired {
		return fmt.Errorf("custom rule disposition must be block or confirm (allowing everything needs no rule)")
	}
	for _, sh := range r.Shells {
		if _, err := ParseShellType(string(sh)); err != nil {
			return fmt.Errorf("custom rule shell type %q is not one of posix, powershell, cmd", string(sh))
		}
	}
	if !knownPathCategories[r.PathCategory] {
		return fmt.Errorf("custom rule path category %q is not one of critical, disk (empty means any path)", r.PathCategory)
	}
	if r.Regex != "" {
		if _, err := regexp.Compile(r.Regex); err != nil {
			return fmt.Errorf("custom rule regex does not compile: %w", err)
		}
	}
	if len(r.Commands) == 0 && len(r.Args) == 0 && r.PathCategory == "" && r.Regex == "" {
		return fmt.Errorf("custom rule needs at least one match condition (command name, argument, path category or regex)")
	}
	if strings.TrimSpace(r.Explanation) == "" {
		return fmt.Errorf("custom rule explanation (why it is dangerous) is required")
	}
	return nil
}

// appliesToShell 报告规则是否适用于给定 shell 类型(空集合 = 全部适用)。
func (r CustomRule) appliesToShell(shell ShellType) bool {
	if len(r.Shells) == 0 {
		return true
	}
	for _, s := range r.Shells {
		if s == shell {
			return true
		}
	}
	return false
}

// matchCustomPOSIX 报告一条自定义规则是否命中当前这条 POSIX 简单命令。
// command 是整条命令原文(正则条件对它匹配);call 是剥掉前缀后的简单
// 命令事实。所有非空条件必须同时满足。
func matchCustomPOSIX(r CustomRule, command string, call *posixCall) bool {
	if len(r.Commands) > 0 && !containsExact(r.Commands, call.name) {
		return false
	}
	for _, want := range r.Args {
		if !callHasArgText(call, want) {
			return false
		}
	}
	switch r.PathCategory {
	case PathCategoryCritical:
		if !callTargetsCriticalPath(call) {
			return false
		}
	case PathCategoryDisk:
		if !callTargetsDiskDevice(call) {
			return false
		}
	}
	if r.Regex != "" {
		// 正则在入库时已校验可编译;此处再失败(理论上不可达)时跳过
		// 该规则,绝不让判定本身失败。
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return false
		}
		if !re.MatchString(command) {
			return false
		}
	}
	return true
}

// containsExact 报告列表里是否有逐字相等的项。
func containsExact(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// callHasArgText 报告简单命令的参数里是否逐字出现给定文字。
// 含变量等动态成分的参数在执行前不知道会是什么,不参与逐字匹配。
func callHasArgText(call *posixCall, want string) bool {
	for _, a := range call.args {
		if !a.dynamic && a.text == want {
			return true
		}
	}
	return false
}

// callTargetsCriticalPath 报告参数里是否有逐字可确定、且规范化后属于
// 系统关键路径的目标。与默认删除规则用同一张关键路径表与同一个
// 规范化函数,类别语义只有一套。
func callTargetsCriticalPath(call *posixCall) bool {
	for _, a := range call.args {
		if !a.dynamic && criticalPOSIXPaths[normalizePOSIXPath(a.text)] {
			return true
		}
	}
	return false
}

// callTargetsDiskDevice 报告参数或写入类重定向里是否有指向 /dev/ 下
// 磁盘设备的目标。与「直接写磁盘设备」默认规则用同一张设备表。
func callTargetsDiskDevice(call *posixCall) bool {
	for _, a := range call.args {
		if !a.dynamic && isDiskDevicePath(a.text) {
			return true
		}
	}
	for _, red := range call.redirects {
		if red.write && !red.targetDyn && isDiskDevicePath(red.target) {
			return true
		}
	}
	return false
}

// RuleInfo 描述一条内置默认规则的展示面:编号、处置、风险说明、替代
// 做法。列出命令与停用编号校验用它,不需要规则内部的判定闭包。
type RuleInfo struct {
	ID          string
	Disposition Disposition
	Explanation string
	Alternative string
}

// DefaultRuleInfos 返回全部内置默认规则的描述(按声明顺序,编号一经
// 发布不可更改):POSIX 一套 + PowerShell 与 cmd 共用的一套(WIN- 前缀)。
// 列出命令与停用编号校验用它。
func DefaultRuleInfos() []RuleInfo {
	out := make([]RuleInfo, 0, len(defaultPOSIXRules)+len(defaultWindowsRules))
	for _, r := range defaultPOSIXRules {
		out = append(out, RuleInfo{
			ID:          r.id,
			Disposition: r.disposition,
			Explanation: r.explanation,
			Alternative: r.alternative,
		})
	}
	for _, r := range defaultWindowsRules {
		out = append(out, RuleInfo{
			ID:          r.id,
			Disposition: r.disposition,
			Explanation: r.explanation,
			Alternative: r.alternative,
		})
	}
	return out
}

// DefaultRuleIDs 返回全部内置默认规则的编号清单(默认规则的唯一合法
// 停用对象;自定义规则编号不得与之冲突)。
func DefaultRuleIDs() []string {
	infos := DefaultRuleInfos()
	out := make([]string, 0, len(infos))
	for _, i := range infos {
		out = append(out, i.ID)
	}
	return out
}

// matchCustomWindows 报告一条自定义规则是否命中当前这段 Windows 简单
// 命令。语义与 POSIX 侧对齐,并遵循 Windows 的匹配惯例:命令名与参数
// 比较不区分大小写;目标路径类别 critical 用 Windows 关键路径表;disk
// 类别是 POSIX 的 /dev/ 磁盘设备概念,Windows 判定不命中(命令行帮助
// 文案已写明)。正则对整条命令原文匹配。
func matchCustomWindows(r CustomRule, rawCommand string, seg *winSegment) bool {
	if len(r.Commands) > 0 {
		name := seg.name // 已小写规范化
		hit := false
		for _, c := range r.Commands {
			if normalizeWinCommandName(c) == name {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for _, want := range r.Args {
		w := strings.ToLower(want)
		found := false
		for _, a := range seg.args {
			if !a.dynamic && strings.ToLower(a.text) == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if r.PathCategory == PathCategoryCritical {
		critical := false
		for _, a := range seg.args {
			if !a.dynamic && criticalWindowsPaths[normalizeWindowsPath(a.text)] {
				critical = true
				break
			}
		}
		if !critical {
			return false
		}
	}
	if r.Regex != "" {
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			return false
		}
		if !re.MatchString(rawCommand) {
			return false
		}
	}
	return true
}
