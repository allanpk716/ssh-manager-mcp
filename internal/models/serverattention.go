package models

// TagNeedsPassphrase 是导入加密私钥时写在服务器条目标签里的「待补口令」
// 标记：导入完成后连接仍会失败，必须先补上口令；补口令的路径会移除这个
// 标记。
const TagNeedsPassphrase = "needs-passphrase"

// ServerNeedsAttention 是服务器条目「需要注意」规则的唯一实现：未挂凭据、
// 未设角色、带待补口令标记，三者任一成立即需要注意。终端图形界面与 Web UI
// 的服务器列表都按这条规则标记并置顶排序，不要在任何一边复制判断。
func ServerNeedsAttention(credentialID, role string, tags []string) bool {
	return credentialID == "" || role == "" || ServerHasTag(tags, TagNeedsPassphrase)
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
