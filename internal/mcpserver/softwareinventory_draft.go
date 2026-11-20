package mcpserver

// 声明草稿提交不再注册 MCP 工具。这里只保留受审计 HTTP 转发使用的请求
// 与响应形态；owner 的命令行和 Web UI 仍负责确认、修改或驳回草稿。

import "ssh-manager-mcp/internal/softwareinventory"

// DraftSoftwareDeclarationsInput 是提交声明草稿 HTTP 请求的输入。
// declarations 里的每一条都是一份声明草稿：可以新增声明，也可以通过
// claimed_observed_key 认领实测条目或未识别进程并补充声明才有的暴露级
// 别。草稿确认前不影响查询结果与待办。
type DraftSoftwareDeclarationsInput struct {
	Declarations []softwareinventory.DeclarationDraft `json:"declarations" jsonschema:"declaration drafts to submit; each becomes a draft the owner must review — none of them takes effect until the owner confirms"`
}

// DraftSoftwareDeclarationsOutput 是提交声明草稿 HTTP 请求的输出：按输
// 入顺序返回每条草稿编号。
type DraftSoftwareDeclarationsOutput struct {
	MaskedOutput
	DraftIDs []string `json:"draft_ids" jsonschema:"draft ids in the same order as the input declarations"`
}

// SoftwareDeclarationDrafter 是客户端经受审计 HTTP 转发提交声明草稿的
// 能力。
type SoftwareDeclarationDrafter interface {
	DraftSoftwareDeclarations(in DraftSoftwareDeclarationsInput) (DraftSoftwareDeclarationsOutput, error)
}
