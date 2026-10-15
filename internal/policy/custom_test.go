package policy

import (
	"strings"
	"testing"
)

// 自定义规则(ticket #15 / 规格 #8 第 27/28/29 条)的判定测试:结构化字段
// (命令名与别名、参数条件、目标路径类别)与正则高级选项各一例,以及
// 条件合并、shell 适用范围、与停用默认规则的并存。只断言外部行为:
// 给定命令文本与生效规则集,得到什么处置、命中哪条规则。

// csRule 构造一条结构化自定义规则的捷径。
func csRule(id string, disposition Disposition, cmds, args []string, pathCategory string) CustomRule {
	return CustomRule{
		ID:           id,
		Shells:       []ShellType{ShellPOSIX},
		Commands:     cmds,
		Args:         args,
		PathCategory: pathCategory,
		Disposition:  disposition,
		Explanation:  "custom rule test explanation",
		Alternative:  "custom rule test alternative",
	}
}

// TestCustomRule_StructuredCommands:命令名条件——默认放行的命令(reboot)
// 被自定义规则改为拦截,不在命令名表里的命令不受影响。
func TestCustomRule_StructuredCommands(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{
		csRule("NO-REBOOT", DispositionBlock, []string{"reboot", "shutdown"}, nil, ""),
	}}
	d := Evaluate(ShellPOSIX, "reboot", rules)
	if d.Disposition != DispositionBlock || d.RuleID != "NO-REBOOT" {
		t.Fatalf("reboot = %+v, want block/NO-REBOOT", d)
	}
	if d.Explanation == "" || d.Alternative == "" {
		t.Fatalf("custom rule decision must carry explanation and alternative: %+v", d)
	}
	// 别名同样命中。
	if d := Evaluate(ShellPOSIX, "shutdown -h now", rules); d.Disposition != DispositionBlock || d.RuleID != "NO-REBOOT" {
		t.Fatalf("shutdown = %+v, want block/NO-REBOOT", d)
	}
	if d := Evaluate(ShellPOSIX, "uptime", rules); d.Disposition != DispositionAllow {
		t.Fatalf("uptime = %+v, want allow", d)
	}
}

// TestCustomRule_NeedsConfirmDisposition:自定义规则也可以是需确认档。
func TestCustomRule_NeedsConfirmDisposition(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{
		csRule("ASK-BEFORE-KILL", DispositionConfirmRequired, []string{"pkill"}, nil, ""),
	}}
	d := Evaluate(ShellPOSIX, "pkill sshd", rules)
	if d.Disposition != DispositionConfirmRequired || d.RuleID != "ASK-BEFORE-KILL" {
		t.Fatalf("pkill = %+v, want confirm/ASK-BEFORE-KILL", d)
	}
}

// TestCustomRule_ArgsCondition:参数条件——每个条件都必须逐字出现在参数里,
// 少一个就不命中。
func TestCustomRule_ArgsCondition(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{
		csRule("NO-FORCE-PUSH", DispositionBlock, []string{"git"}, []string{"push", "--force"}, ""),
	}}
	if d := Evaluate(ShellPOSIX, "git push --force origin", rules); d.Disposition != DispositionBlock || d.RuleID != "NO-FORCE-PUSH" {
		t.Fatalf("git push --force = %+v, want block/NO-FORCE-PUSH", d)
	}
	if d := Evaluate(ShellPOSIX, "git push origin", rules); d.Disposition != DispositionAllow {
		t.Fatalf("git push (no --force) = %+v, want allow", d)
	}
}

// TestCustomRule_PathCategoryCritical:目标路径类别——目标含系统关键路径才命中。
func TestCustomRule_PathCategoryCritical(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{
		csRule("NO-MOVE-INTO-CRITICAL", DispositionBlock, []string{"mv"}, nil, "critical"),
	}}
	if d := Evaluate(ShellPOSIX, "mv /tmp/x /etc", rules); d.Disposition != DispositionBlock || d.RuleID != "NO-MOVE-INTO-CRITICAL" {
		t.Fatalf("mv into /etc = %+v, want block/NO-MOVE-INTO-CRITICAL", d)
	}
	if d := Evaluate(ShellPOSIX, "mv /tmp/a /tmp/b", rules); d.Disposition != DispositionAllow {
		t.Fatalf("mv inside /tmp = %+v, want allow", d)
	}
}

// TestCustomRule_PathCategoryDisk:目标路径类别——目标是 /dev/ 下的磁盘设备才命中。
func TestCustomRule_PathCategoryDisk(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{
		csRule("NO-RAW-COPY", DispositionBlock, []string{"cp"}, nil, "disk"),
	}}
	if d := Evaluate(ShellPOSIX, "cp img.bin /dev/sdb", rules); d.Disposition != DispositionBlock || d.RuleID != "NO-RAW-COPY" {
		t.Fatalf("cp onto /dev/sdb = %+v, want block/NO-RAW-COPY", d)
	}
	if d := Evaluate(ShellPOSIX, "cp img.bin /tmp/img.bin", rules); d.Disposition != DispositionAllow {
		t.Fatalf("cp into /tmp = %+v, want allow", d)
	}
}

// TestCustomRule_Regex:正则高级选项——对整条命令文本匹配,结构化字段表达
// 不了的写法(如任意命令里出现某串文字)靠它兜住。
func TestCustomRule_Regex(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{{
		ID:          "NO-DESTRUCTIVE-PIPES",
		Regex:       `\brm\s+-rf\s+/data/warehouse`,
		Disposition: DispositionBlock,
		Explanation: "the warehouse is replicated, never deleted in place",
		Alternative: "ask the owner to archive and drop partitions instead",
	}}}
	if d := Evaluate(ShellPOSIX, "sudo rm -rf /data/warehouse/2024", rules); d.Disposition != DispositionBlock || d.RuleID != "NO-DESTRUCTIVE-PIPES" {
		t.Fatalf("rm -rf /data/warehouse = %+v, want block/NO-DESTRUCTIVE-PIPES", d)
	}
	if d := Evaluate(ShellPOSIX, "rm -rf /data/scratch", rules); d.Disposition != DispositionAllow {
		t.Fatalf("rm -rf /data/scratch = %+v, want allow", d)
	}
}

// TestCustomRule_ConditionsCombineWithAnd:非空条件之间是「同时满足」。
func TestCustomRule_ConditionsCombineWithAnd(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{{
		ID:           "COMBINED",
		Shells:       []ShellType{ShellPOSIX},
		Commands:     []string{"find"},
		Args:         []string{"-delete"},
		PathCategory: "critical",
		Disposition:  DispositionBlock,
	}}}
	if d := Evaluate(ShellPOSIX, "find /etc -name x -delete", rules); d.Disposition != DispositionBlock || d.RuleID != "COMBINED" {
		t.Fatalf("find /etc -delete = %+v, want block/COMBINED", d)
	}
	// 命令名与参数命中但目标不在关键路径:不命中。
	if d := Evaluate(ShellPOSIX, "find /tmp -name x -delete", rules); d.Disposition != DispositionAllow {
		t.Fatalf("find /tmp -delete = %+v, want allow", d)
	}
	// 命令名不对:参数与路径命中也不算。
	if d := Evaluate(ShellPOSIX, "ls /etc -delete", rules); d.Disposition != DispositionAllow {
		t.Fatalf("ls /etc -delete = %+v, want allow", d)
	}
}

// TestCustomRule_ShellFilter:只适用于其他 shell 类型的自定义规则不影响
// POSIX 判定。
func TestCustomRule_ShellFilter(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{{
		ID:          "WIN-ONLY",
		Shells:      []ShellType{ShellPowerShell, ShellWindowsCmd},
		Commands:    []string{"format"},
		Disposition: DispositionBlock,
	}}}
	if d := Evaluate(ShellPOSIX, "format c:", rules); d.Disposition != DispositionAllow {
		t.Fatalf("posix judging must skip a windows-only rule, got %+v", d)
	}
}

// TestCustomRule_AppliesInsideCompoundCommand:自定义规则与默认规则一样,
// 对组合命令里的每条简单命令生效(前缀剥掉、藏在 && 后面也命中)。
func TestCustomRule_AppliesInsideCompoundCommand(t *testing.T) {
	rules := &RuleSet{CustomRules: []CustomRule{
		csRule("NO-REBOOT", DispositionBlock, []string{"reboot"}, nil, ""),
	}}
	d := Evaluate(ShellPOSIX, "cd /x && sudo reboot", rules)
	if d.Disposition != DispositionBlock || d.RuleID != "NO-REBOOT" {
		t.Fatalf("compound reboot = %+v, want block/NO-REBOOT", d)
	}
}

// TestCustomRule_AloneWithDisabledDefault:停用的默认规则不再命中,自定义
// 规则照常生效——两种调整共存于同一个生效规则集。
func TestCustomRule_AloneWithDisabledDefault(t *testing.T) {
	rules := &RuleSet{
		DisabledDefaultRuleIDs: []string{"POSIX-FORMAT"},
		CustomRules: []CustomRule{
			csRule("NO-REBOOT", DispositionBlock, []string{"reboot"}, nil, ""),
		},
	}
	if d := Evaluate(ShellPOSIX, "mkfs.ext4 /dev/sdb", rules); d.Disposition != DispositionAllow {
		t.Fatalf("disabled default must stop matching, got %+v", d)
	}
	if d := Evaluate(ShellPOSIX, "reboot", rules); d.Disposition != DispositionBlock || d.RuleID != "NO-REBOOT" {
		t.Fatalf("custom rule must still fire, got %+v", d)
	}
}

// TestCustomRule_Validate:自定义规则入库前的自描述校验——处置只能是拦截
// 或需确认(放行无需规则)、shell 类型与目标路径类别必须是已知值、正则
// 必须能编译、至少要有一个匹配条件、风险说明必填。
func TestCustomRule_Validate(t *testing.T) {
	cases := []struct {
		name string
		rule CustomRule
		want string // 期望错误里包含的文字;空串表示应当通过
	}{
		{"ok structured", csRule("OK-1", DispositionBlock, []string{"reboot"}, nil, ""), ""},
		{"ok regex", CustomRule{ID: "OK-2", Regex: "x+", Disposition: DispositionConfirmRequired, Explanation: "why"}, ""},
		{"empty id", csRule("", DispositionBlock, []string{"x"}, nil, ""), "id"},
		{"allow disposition", csRule("A", DispositionAllow, []string{"x"}, nil, ""), "disposition"},
		{"bad shell", CustomRule{ID: "B", Shells: []ShellType{"fish"}, Commands: []string{"x"}, Disposition: DispositionBlock, Explanation: "w"}, "shell"},
		{"bad path category", csRule("C", DispositionBlock, []string{"x"}, nil, "somewhere"), "path category"},
		{"bad regex", CustomRule{ID: "D", Regex: "(", Disposition: DispositionBlock, Explanation: "w"}, "regex"},
		{"no condition", CustomRule{ID: "E", Disposition: DispositionBlock, Explanation: "w"}, "condition"},
		{"no explanation", CustomRule{ID: "F", Commands: []string{"x"}, Disposition: DispositionBlock}, "explanation"},
	}
	for _, tc := range cases {
		err := tc.rule.Validate()
		if tc.want == "" {
			if err != nil {
				t.Fatalf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// TestDefaultRuleIDs:内置默认规则编号清单(停用命令按编号校验、列出
// 命令按它标注)包含已发布的 POSIX 默认规则编号。
func TestDefaultRuleIDs(t *testing.T) {
	ids := DefaultRuleIDs()
	for _, want := range []string{"POSIX-FORMAT", "POSIX-RAW-DISK-WRITE", "POSIX-PARTITION", "POSIX-CRITICAL-DELETE", "POSIX-RECURSIVE-ROOT-PERM", "POSIX-FORK-BOMB"} {
		found := false
		for _, id := range ids {
			if id == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("DefaultRuleIDs missing %q: %v", want, ids)
		}
	}
}

// TestDefaultRuleInfos:默认规则的描述面(编号、处置、风险说明、替代做法)
// 供列出命令使用,不遗漏任何一条已发布的默认规则。
func TestDefaultRuleInfos(t *testing.T) {
	infos := DefaultRuleInfos()
	if len(infos) != len(DefaultRuleIDs()) {
		t.Fatalf("infos = %d entries, ids = %d", len(infos), len(DefaultRuleIDs()))
	}
	for _, info := range infos {
		if info.ID == "" || info.Explanation == "" || info.Alternative == "" {
			t.Fatalf("default rule info incomplete: %+v", info)
		}
	}
}
