package mcpserver

import (
	"context"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SessionHintSource 返回一条会话提示；空字符串表示本次没有提示。来源按注册
// 顺序消费，以后变更提示可以注册在版本提示之前。
type SessionHintSource func() string

// SessionHints 是桥进程里的一次性工具提示通道。记录只放在内存里，热升级后
// 从空记录重新开始是有意行为。
type SessionHints struct {
	mu       sync.Mutex
	sources  []SessionHintSource
	seenText map[string]map[string]struct{}
}

// Add 登记一个提示来源。
func (h *SessionHints) Add(source SessionHintSource) {
	if h == nil || source == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sources = append(h.sources, source)
}

// Take 按来源顺序返回某个 MCP 会话尚未展示过的提示，并登记为已展示。提示
// 文本本身就是去重键。
func (h *SessionHints) Take(sessionID string) []string {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var hints []string
	for _, source := range h.sources {
		text := source()
		if text == "" {
			continue
		}
		if h.seenText == nil {
			h.seenText = make(map[string]map[string]struct{})
		}
		if h.seenText[sessionID] == nil {
			h.seenText[sessionID] = make(map[string]struct{})
		}
		if _, shown := h.seenText[sessionID][text]; shown {
			continue
		}
		h.seenText[sessionID][text] = struct{}{}
		hints = append(hints, text)
	}
	return hints
}

// attachSessionHints 安装通用工具结果通道。它运行在类型化工具包装层生成
// 最终结果之后，把提示作为独立文本插在最前，不改原文本、不改结构化输出，
// 也不吞掉失败的工具结果。
func attachSessionHints(srv *mcp.Server, hints *SessionHints) {
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			if method != "tools/call" || err != nil {
				return res, err
			}
			toolResult, ok := res.(*mcp.CallToolResult)
			if !ok || toolResult == nil {
				return res, err
			}
			sessionID := ""
			if session := req.GetSession(); session != nil {
				sessionID = session.ID()
			}
			notices := hints.Take(sessionID)
			if len(notices) == 0 {
				return res, err
			}
			toolResult.Content = hintContent(notices, toolResult.Content)
			return toolResult, err
		}
	})
}

func hintContent(notices []string, original []mcp.Content) []mcp.Content {
	content := make([]mcp.Content, 0, len(notices)+len(original))
	for _, notice := range notices {
		content = append(content, &mcp.TextContent{Text: notice})
	}
	return append(content, original...)
}
