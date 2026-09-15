package tui

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// docNorm canonicalizes a raw .mcp.json snippet block: dynamic values →
// placeholders, then whitespace-compacted — so CLI 单行输出与 TUI 多行渲染
// 两种合法排版在规范化后可比（逐字节口径由 Task 1 的 in-code golden 锁定，
// 本测试锁语义等价 + 占位纪律）。
var (
	reDocToken  = regexp.MustCompile(`("SSHMGR_TOKEN":\s*")[^"]*(")`)
	reDocBearer = regexp.MustCompile(`("Authorization":\s*"Bearer )[^"]*(")`)
	reDocURL    = regexp.MustCompile(`("url":\s*")[^"]*(")`)
	reDocHex    = regexp.MustCompile(`("SSHMGR_MASTERKEY_HEX":\s*")[^"]*(")`)
)

func docNorm(raw string) string {
	s := reDocToken.ReplaceAllString(raw, "${1}<TOKEN>${2}")
	s = reDocBearer.ReplaceAllString(s, "${1}<TOKEN>${2}")
	s = reDocURL.ReplaceAllString(s, "${1}<URL>${2}")
	s = reDocHex.ReplaceAllString(s, "${1}<HEX>${2}")
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		panic("docNorm: " + err.Error())
	}
	return buf.String()
}

// extractMcpBlocks scans a doc for every {"mcpServers"...} object (balanced
// braces, works for single-line and pretty blocks).
func extractMcpBlocks(t *testing.T, doc string) []string {
	t.Helper()
	var blocks []string
	for i := 0; i < len(doc); i++ {
		if !strings.HasPrefix(doc[i:], `"mcpServers"`) {
			continue
		}
		start := strings.LastIndexByte(doc[:i], '{')
		if start < 0 {
			continue
		}
		depth := 0
		for j := start; j < len(doc); j++ {
			switch doc[j] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					blocks = append(blocks, doc[start:j+1])
					i = j
					j = len(doc)
				}
			}
		}
	}
	return blocks
}

// TestDocsMcpSnippetsMatchGolden: 每篇文档里的每个 mcpServers 块，规范化后
// 必须命中 canonical 之一（stdio / cache / stdio+masterkey-hex）。http 形态
// 的 canonical 腿已随 ②a 移除退役（Plan 42 批1）——文档里再出现 http 块即红。
// Plan 44 T1→T2 过渡期遗留的 pre-rename（"ssh-manager"）canonical 腿已随
// T2 文档 sweep 落地拆除——文档里再出现 "command": "ssh-manager" 即红。
func TestDocsMcpSnippetsMatchGolden(t *testing.T) {
	canonical := map[string]bool{
		docNorm(jsonBlockOf(mcpConfigLines([]string{`"args": ["mcp"]`, stdioEnvLine("<TOKEN>")}, nil))):                                                 true,
		docNorm(jsonBlockOf(mcpConfigLines([]string{`"args": ["mcp", "--cache"]`, stdioEnvLine("<TOKEN>")}, nil))):                                      true,
		docNorm(`{"mcpServers": {"ssh": {"command": "sshmgr", "args": ["mcp"], "env": {"SSHMGR_TOKEN": "<TOKEN>", "SSHMGR_MASTERKEY_HEX": "<HEX>"}}}}`): true,
	}
	docs := []string{
		"../../README.md",
		"../../docs/quickstart-single-machine.md",
		"../../docs/quickstart-multi-machine.md",
		"../../docs/getting-started.md",
		"../../docs/agent-access.md",
		"../../docs/multi-machine.md",
		"../../docs/tui-single-machine.md",
		"../../docs/tui-multi-machine.md",
		"../../docs/agent-tools.md",
	}
	for _, p := range docs {
		b, err := os.ReadFile(filepath.FromSlash(p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, blk := range extractMcpBlocks(t, string(b)) {
			if !canonical[docNorm(blk)] {
				t.Errorf("%s: snippet not in canonical forms:\n%s", p, blk)
			}
		}
	}
}
