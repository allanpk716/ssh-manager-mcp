package tui

import (
	"strconv"
	"strings"

	"charm.land/huh/v2"
)

// editField 描述一个可编辑字段：如何读写 draft、如何显示。Key 是稳定标识，
// 同时充当脏快照键（snapshotDraft 的 map 键）。Secret 字段的 Get 返回状态串
// （"已设…"/"（未设）"类）而非内容——值预览链路永不携带明文。
type editField struct {
	Key     string                         // 稳定标识（测试/脏快照键）
	Label   string                         // 列表显示名
	Secret  bool                           // true=值预览掩码
	Confirm bool                           // true=表单是 huh Confirm（y/Y/n/N 单键设值并前进——泵门依赖）
	Get     func(d *serverDraft) string    // 当前值（Secret 字段返回状态而非内容）
	Set     func(d *serverDraft, v string) // 写回（端口在此做 Atoi+范围验证）
	Build   func(d *serverDraft) *huh.Form // 单字段编辑表单（复用 forms.go 构造器）
}

// editFields 按固定顺序返回字段表（设计决策）：名称/Host/端口/SSH 用户/密码/
// 私钥路径/密钥口令/sudo 密码/清除凭据(编辑态)/硬件/位置/角色/服务/Caveats/
// 备注——编辑态 15 项，新增态 14 项（没有可清除的凭据）。保存项不在表内：
// 它是编辑页（T2）在列表末尾追加的哨兵项。editing 同时被各闭包捕获，
// 决定秘密字段的状态文案与表单标题（编辑态的"留空=不变"语义）。
func editFields(editing bool) []editField {
	fields := make([]editField, 0, 15)
	fields = append(fields,
		strField("name", "名称", func(d *serverDraft) *string { return &d.Name }, "名称（唯一）", nonEmpty),
		strField("host", "Host", func(d *serverDraft) *string { return &d.Host }, "Host / IP", nonEmpty),
		portEditField(),
		strField("user", "SSH 用户", func(d *serverDraft) *string { return &d.User }, "SSH 用户", nonEmpty),
		secretField("password", "密码", editing, "（留空=保持现有）",
			func(d *serverDraft) *string { return &d.Password },
			func(d *serverDraft) huh.Field { return passwordField(d, editing) }),
		editField{ // keypath 是路径不是秘密：预览显示原值，表单复用 keyPathField
			Key:   "keypath",
			Label: "私钥路径",
			Get:   func(d *serverDraft) string { return d.KeyPath },
			Set:   func(d *serverDraft, v string) { d.KeyPath = v },
			Build: func(d *serverDraft) *huh.Form {
				return huh.NewForm(huh.NewGroup(keyPathField(d)))
			},
		},
		secretField("keypass", "密钥口令", editing, "（未设）",
			func(d *serverDraft) *string { return &d.KeyPass },
			func(d *serverDraft) huh.Field {
				return huh.NewInput().Title("密钥口令（可选）").Value(&d.KeyPass).EchoMode(huh.EchoModePassword)
			}),
		secretField("sudopassword", "sudo 密码", editing, "（留空=保持现有）",
			func(d *serverDraft) *string { return &d.SudoPassword },
			func(d *serverDraft) huh.Field { return sudoPasswordField(d) }),
	)
	if editing {
		fields = append(fields, clearCredentialEditField())
	}
	fields = append(fields,
		strField("hardware", "硬件", func(d *serverDraft) *string { return &d.Hardware }, "硬件", nil),
		strField("location", "位置", func(d *serverDraft) *string { return &d.Location }, "位置", nil),
		strField("role", "角色", func(d *serverDraft) *string { return &d.Role }, "角色", nil),
		strField("services", "服务", func(d *serverDraft) *string { return &d.Services }, "服务", nil),
		strField("caveats", "Caveats", func(d *serverDraft) *string { return &d.Caveats }, "Caveats（agent 行动前必读）", nil),
		strField("description", "备注", func(d *serverDraft) *string { return &d.Description }, "备注", nil),
	)
	return fields
}

// strField 构造普通字符串字段：member 选定 draft 成员（Get/Set/Build 三处共用
// 同一选择器，绑定不会走错字段）；title 与长表单逐字一致；validate 可空
// （名称/Host/SSH 用户与 newServerForm 一样 nonEmpty——提交路径不再复检）。
func strField(key, label string, member func(*serverDraft) *string, title string, validate func(string) error) editField {
	return editField{
		Key:   key,
		Label: label,
		Get:   func(d *serverDraft) string { return *member(d) },
		Set:   func(d *serverDraft, v string) { *member(d) = v },
		Build: func(d *serverDraft) *huh.Form {
			input := huh.NewInput().Title(title).Value(member(d))
			if validate != nil {
				input = input.Validate(validate)
			}
			return huh.NewForm(huh.NewGroup(input))
		},
	}
}

// secretField 构造掩码字段：Get 渲染状态串（绝不返回内容），Set 写回明文，
// Build 用给定构造器包成单组单字段表单。blankEdit 是编辑态空值时的状态文案
// （prefill 留空的语义是"保持现有"）。
func secretField(key, label string, editing bool, blankEdit string, member func(*serverDraft) *string, build func(*serverDraft) huh.Field) editField {
	return editField{
		Key:    key,
		Label:  label,
		Secret: true,
		Get:    func(d *serverDraft) string { return secretStatus(*member(d) != "", editing, blankEdit) },
		Set:    func(d *serverDraft, v string) { *member(d) = v },
		Build:  func(d *serverDraft) *huh.Form { return huh.NewForm(huh.NewGroup(build(d))) },
	}
}

// secretStatus 渲染秘密字段的状态串。编辑态：已填 = 已设（新值）——与被保持的
// 现有凭据区分；空 = blankEdit（密码/sudo 是"保持现有"，keypass 没有可保持的
// 现有值——它只随新填的私钥路径一起生效，所以是（未设））。新增态：已设 /
// （未设）。任何分支都不含字段内容。
func secretStatus(set, editing bool, blankEdit string) string {
	if set {
		if editing {
			return "已设（新值）"
		}
		return "已设"
	}
	if editing {
		return blankEdit
	}
	return "（未设）"
}

// portEditField：端口是唯一的整数字段。Set 做 Atoi+1-65535 夹验证——单字段
// 表单（portField）本身已校验，Set 正常只见到合法输入；非法输入是安全 no-op
// （保持原值，不写脏数据）。
func portEditField() editField {
	return editField{
		Key:   "port",
		Label: "端口",
		Get:   func(d *serverDraft) string { return strconv.Itoa(d.Port) },
		Set: func(d *serverDraft, v string) {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 1 || n > 65535 {
				return
			}
			d.Port = n
		},
		Build: func(d *serverDraft) *huh.Form {
			return huh.NewForm(huh.NewGroup(portField(&d.Port)))
		},
	}
}

// clearCredentialEditField：仅编辑态存在（新增态没有可清除的凭据）。bool 的
// 字符串形态是状态对"未勾选/已勾选"；Set 额外接受规范形"true"（"false"及
// 其他一律 false），让 snapshotDraft 的原值能经 Set 往返。表单复用
// newServerForm 编辑分支的同一个 Confirm 构造。
func clearCredentialEditField() editField {
	return editField{
		Key:     "clearcredential",
		Label:   "清除凭据",
		Confirm: true,
		Get: func(d *serverDraft) string {
			if d.ClearCredential {
				return "已勾选"
			}
			return "未勾选"
		},
		Set: func(d *serverDraft, v string) { d.ClearCredential = v == "true" || v == "已勾选" },
		Build: func(d *serverDraft) *huh.Form {
			return huh.NewForm(huh.NewGroup(huh.NewConfirm().
				Title("清除凭据（回到无凭据态）").Value(&d.ClearCredential).
				Affirmative("清除").Negative("保留")))
		},
	}
}

// snapshotDraft 捕获全部 15 个可编辑字段的原值（含秘密明文）——仅用于脏比较，
// 永不渲染。键即 editField.Key（TestEditFieldsKeysMatchSnapshot 锁定对应关系）。
func snapshotDraft(d *serverDraft) map[string]string {
	return map[string]string{
		"name":            d.Name,
		"host":            d.Host,
		"user":            d.User,
		"port":            strconv.Itoa(d.Port),
		"password":        d.Password,
		"keypath":         d.KeyPath,
		"keypass":         d.KeyPass,
		"sudopassword":    d.SudoPassword,
		"clearcredential": strconv.FormatBool(d.ClearCredential),
		"hardware":        d.Hardware,
		"location":        d.Location,
		"role":            d.Role,
		"services":        d.Services,
		"caveats":         d.Caveats,
		"description":     d.Description,
	}
}

// dirtyAgainst 报告哪些字段相对快照发生了变化。改了又改回原值 = 净（比较的是
// 原值而非编辑轨迹）。
func dirtyAgainst(d *serverDraft, snap map[string]string) map[string]bool {
	cur := snapshotDraft(d)
	dirty := make(map[string]bool, len(snap))
	for k, v := range snap {
		dirty[k] = cur[k] != v
	}
	return dirty
}

// fieldPreview 格式化一个列表行：title 脏时带 "● " 前缀（着色是 T2 的事），
// desc 是值预览——秘密字段经 Get 已是状态串（永不含明文）、端口是数字、空值
// 显示（空）——脏时再追加"（已改）"后缀。
func fieldPreview(f editField, d *serverDraft, dirty bool) (title, desc string) {
	title = f.Label
	if dirty {
		title = "● " + f.Label
	}
	desc = f.Get(d)
	if desc == "" {
		desc = "（空）"
	}
	if dirty {
		desc += "（已改）"
	}
	return title, desc
}
