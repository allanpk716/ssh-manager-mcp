package cli

import (
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/ops"
)

// uiClientPull 把 Web UI 的立即同步映射到 `cache pull` 使用的同一个拉取函数。
func uiClientPull(request ops.ClientPullRequest) error {
	_, err := clientops.DoPull(request.URL, request.Token, request.Pin, clientops.PullOpts{
		Instance: request.Instance, Timeout: request.Timeout,
	})
	return err
}
