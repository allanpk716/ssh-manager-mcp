package store

// WriteSource 描述一次存储层写操作来自哪里：命令行，或 Web UI（附来源
// 地址）。需要把数据行与审计行放在同一个事务里的存储方法用它填充审计
// 来源列；字段保持私有，调用方只能通过下面的构造函数生成合法来源。
type WriteSource struct {
	kind    string
	address string
}

// CLISource 返回命令行来源。命令行来源没有来源地址。
func CLISource() WriteSource {
	return WriteSource{kind: AuditSourceCLI}
}

// WebUISource 返回 Web UI 来源，address 是这次请求的来源地址。
func WebUISource(address string) WriteSource {
	return WriteSource{kind: AuditSourceWebUI, address: address}
}
