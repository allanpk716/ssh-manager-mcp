package mcpserver

// 票 #188：draft_software_declarations 工具。权威面与单机用本地适配层按
// profile 可见范围直接写草稿；客户端缓存面把草稿交给注入的受审计转发写
// 能力（clientops 的转发器），未注入时固定报错。这里不提供任何确认草稿
// 或修改生效声明的工具——确认与驳回只属于 owner 的命令行与 Web UI。

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/softwareinventory"
	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
)

// DraftSoftwareDeclarationsInput 是 draft_software_declarations 的输入。
// declarations 里的每一条都是一份声明草稿：可以新增声明，也可以通过
// claimed_observed_key 认领实测条目或未识别进程并补充声明才有的暴露级
// 别。草稿确认前不影响查询结果与待办。
type DraftSoftwareDeclarationsInput struct {
	Declarations []softwareinventory.DeclarationDraft `json:"declarations" jsonschema:"declaration drafts to submit; each becomes a draft the owner must review — none of them takes effect until the owner confirms"`
}

// DraftSoftwareDeclarationsOutput 是工具的输出：按输入顺序返回每条草稿
// 编号。
type DraftSoftwareDeclarationsOutput struct {
	MaskedOutput
	DraftIDs []string `json:"draft_ids" jsonschema:"draft ids in the same order as the input declarations"`
}

// SoftwareDeclarationDrafter 是提交声明草稿的能力。权威面与单机注入本地
// 适配层；客户端缓存面注入 clientops 的受审计转发写转发器。
type SoftwareDeclarationDrafter interface {
	DraftSoftwareDeclarations(in DraftSoftwareDeclarationsInput) (DraftSoftwareDeclarationsOutput, error)
}

// errSoftwareDraftNotWired 是客户端面没有接上受审计转发写能力时的固定
// 错误文本。
var errSoftwareDraftNotWired = errors.New(
	"提交声明草稿需要经权威端落库，但这个客户端桥没有接上转发器；不会写本地数据，请修复部署后重试")

// failClosedSoftwareDeclarationDrafter 只报错，不写任何数据。
type failClosedSoftwareDeclarationDrafter struct{}

func (failClosedSoftwareDeclarationDrafter) DraftSoftwareDeclarations(
	DraftSoftwareDeclarationsInput,
) (DraftSoftwareDeclarationsOutput, error) {
	return DraftSoftwareDeclarationsOutput{DraftIDs: []string{}}, errSoftwareDraftNotWired
}

// localSoftwareDeclarationDrafter 在权威端或单机的 vault 上按 profile 可
// 见范围直接写草稿。
type localSoftwareDeclarationDrafter struct {
	storeFn   func() *store.Store
	profileID string
}

func (d localSoftwareDeclarationDrafter) DraftSoftwareDeclarations(
	in DraftSoftwareDeclarationsInput,
) (DraftSoftwareDeclarationsOutput, error) {
	st := d.storeFn()
	granted, err := st.ServersForProfile(d.profileID)
	if err != nil {
		return DraftSoftwareDeclarationsOutput{}, fmt.Errorf("读取 profile 授权失败: %w", err)
	}
	ids, err := softwareinventory.New(st).SubmitDeclarationDrafts(
		in.Declarations,
		softwareinventory.DevicesVisibleThroughServers(granted...),
		softwareinventory.AgentSource(),
	)
	if ids == nil {
		ids = []string{}
	}
	return DraftSoftwareDeclarationsOutput{DraftIDs: ids}, err
}

// registerSoftwareDeclarationDraftTool 注册 draft_software_declarations。
// inventory 为 nil 时使用本地适配层（权威端与单机）；客户端缓存面由注入
// 的在线能力同时提供查询与草稿提交，未实现草稿提交时保底报错。
func registerSoftwareDeclarationDraftTool(
	srv *mcp.Server,
	busy *BusyTracker,
	storeFn func() *store.Store,
	hkFn func() sshbroker.HostKeyStore,
	profileID string,
	inventory SoftwareInventoryQuerier,
) {
	var drafter SoftwareDeclarationDrafter
	switch typed := inventory.(type) {
	case nil:
		drafter = localSoftwareDeclarationDrafter{storeFn: storeFn, profileID: profileID}
	case SoftwareDeclarationDrafter:
		drafter = typed
	default:
		drafter = failClosedSoftwareDeclarationDrafter{}
	}
	addTrackedTool(srv, busy, storeFn, hkFn,
		&mcp.Tool{
			Name:        BrokerTools[17], // "draft_software_declarations"（票 #188）
			Description: "Submit software declaration drafts (new declarations, claims of observed items or unidentified processes via claimed_observed_key, and declaration-only exposure levels). Multiple drafts can be submitted at once; the reply returns one draft id per input. Drafts do NOT affect query results or follow-ups until the owner confirms them — there is deliberately no tool to confirm, reject, or edit effective declarations.",
		},
		func(_ context.Context, _ *mcp.CallToolRequest, in DraftSoftwareDeclarationsInput) (*mcp.CallToolResult, DraftSoftwareDeclarationsOutput, error) {
			if len(in.Declarations) == 0 {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "declarations must carry at least one declaration draft"}},
				}, DraftSoftwareDeclarationsOutput{DraftIDs: []string{}}, nil
			}
			output, err := drafter.DraftSoftwareDeclarations(in)
			if err != nil {
				if output.DraftIDs == nil {
					output.DraftIDs = []string{}
				}
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, output, nil
			}
			return nil, output, nil
		},
	)
}
