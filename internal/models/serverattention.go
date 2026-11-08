package models

// TagNeedsPassphrase 是导入加密私钥时写在服务器条目标签里的「待补口令」
// 标记：导入完成后连接仍会失败，必须先补上口令；补口令的路径会移除这个
// 标记。
const TagNeedsPassphrase = "needs-passphrase"

// 注意原因代号。列表、详情与过滤视图共用这组代号；界面层负责换成中文文案。
const (
	// ReasonNoCredential 表示服务器条目还没有挂任何登录凭据。
	ReasonNoCredential = "no-credential"
	// ReasonNoRole 表示服务器条目还没有填写角色（服务器元数据六字段之一）。
	ReasonNoRole = "no-role"
	// ReasonNeedsPassphrase 表示登录私钥还缺私钥口令，连接会失败。
	ReasonNeedsPassphrase = "needs-passphrase"
)

// ServerAttentionReasons 返回服务器条目当前全部注意原因，顺序固定：未挂凭
// 据、未设角色、待补私钥口令。缺 sudo 密码或缺其他服务器元数据字段不报警。
func ServerAttentionReasons(credentialID, role string, tags []string) []string {
	var reasons []string
	if credentialID == "" {
		reasons = append(reasons, ReasonNoCredential)
	}
	if role == "" {
		reasons = append(reasons, ReasonNoRole)
	}
	if ServerHasTag(tags, TagNeedsPassphrase) {
		reasons = append(reasons, ReasonNeedsPassphrase)
	}
	return reasons
}

// ServerNeedsAttention 是服务器条目「需要注意」规则：未挂凭据、未设角色、
// 带待补口令标记，三者任一成立即需要注意。它基于注意原因列表实现；终端图
// 形界面、命令行与 Web UI 都共用这一条规则，不要在任何一边复制判断。
func ServerNeedsAttention(credentialID, role string, tags []string) bool {
	return len(ServerAttentionReasons(credentialID, role, tags)) > 0
}

// ServerHasTag 判断服务器条目是否带有某个标签。
func ServerHasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
