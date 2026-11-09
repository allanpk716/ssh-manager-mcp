package store

// WriteSource 描述一次存储层写操作来自哪里：命令行，或 Web UI（附来源
// 地址）。需要把数据行与审计行放在同一个事务里的存储方法用它填充审计
// 来源列；字段保持私有，调用方只能通过下面的构造函数生成合法来源。
type WriteSource struct {
	kind    string
	address string
	// deviceCode 是受审计转发写的设备码编号；只进入审计摘要，让 owner 能
	// 追查是哪台客户端实例提交的草稿。
	deviceCode string
}

// CLISource 返回命令行来源。命令行来源没有来源地址。
func CLISource() WriteSource {
	return WriteSource{kind: AuditSourceCLI}
}

// WebUISource 返回 Web UI 来源，address 是这次请求的来源地址。
func WebUISource(address string) WriteSource {
	return WriteSource{kind: AuditSourceWebUI, address: address}
}

// AgentSource 返回 MCP 工具的来源。权威端与单机上的工具直接写 vault 时
// 使用；客户端经转发写提交时改用 ForwardedWriteSource。
func AgentSource() WriteSource {
	return WriteSource{kind: AuditSourceAgent}
}

// ForwardedWriteSource 返回受审计转发写的来源。deviceCode 是发起这次转
// 发写的设备码编号，会写进审计摘要。
func ForwardedWriteSource(deviceCode string) WriteSource {
	return WriteSource{kind: AuditSourceForward, deviceCode: deviceCode}
}

// Kind 返回来源类别；零值来源返回空字符串。
func (s WriteSource) Kind() string { return s.kind }
