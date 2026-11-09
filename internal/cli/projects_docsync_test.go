package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// docMcpNorm 把 `.mcp.json` 片段里的动态值统一成占位符再压缩空白，让
// 命令行单行输出与文档里的多行排版可以逐语义比较。
var (
	reDocMcpToken = regexp.MustCompile(`("SSHMGR_TOKEN":\s*")[^"]*(")`)
	reDocMcpHex   = regexp.MustCompile(`("SSHMGR_MASTERKEY_HEX":\s*")[^"]*(")`)
)

func docMcpNorm(raw string) string {
	s := reDocMcpToken.ReplaceAllString(raw, "${1}<TOKEN>${2}")
	s = reDocMcpHex.ReplaceAllString(s, "${1}<HEX>${2}")
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(s)); err != nil {
		panic("docMcpNorm: " + err.Error())
	}
	return buf.String()
}

// extractDocMcpBlocks 扫描文档里的每个 mcpServers JSON 对象（按花括号配
// 平，兼容单行与多行排版）。
func extractDocMcpBlocks(t *testing.T, doc string) []string {
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

// TestDocsMcpSnippetsMatchProjectsAddOutput 守护「文档里的 `.mcp.json` 片段
// 与代码生成结果一致」：基准片段来自真实运行 `sshmgr projects add` 打印的
// 输出；README 与在用文档里的每个 mcpServers 块规范化后必须命中它，或者
// 命中文档明确补充的 SSHMGR_MASTERKEY_HEX 变体（无密钥环机器手动传主密
// 钥的写法，见 docs/agent-access.md）。已退役的 HTTP 形态不在集合里，文
// 档中再出现即失败。
func TestDocsMcpSnippetsMatchProjectsAddOutput(t *testing.T) {
	dbPath, mk := withCliStoreEnv(t)
	st, err := store.Open(dbPath, mk)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddProfile("docs-sync"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	out := runCli(t, "projects", "add", "docs-sync-agent", "--profile", "docs-sync")
	marker := ".mcp.json snippet:\n"
	idx := strings.Index(out, marker)
	if idx < 0 {
		t.Fatalf("`projects add` 输出缺少片段标题：\n%s", out)
	}
	snippet := strings.SplitN(out[idx+len(marker):], "\n", 2)[0]
	var parsed struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(snippet), &parsed); err != nil || len(parsed.MCPServers) == 0 {
		t.Fatalf("`projects add` 输出的片段应是含 mcpServers 的 JSON：%q（%v）", snippet, err)
	}

	canonical := map[string]bool{
		docMcpNorm(snippet): true,
		// 文档补充变体：无密钥环机器把主密钥十六进制值放进同一个 env
		// 字段（命令输出不含它，写法见 docs/agent-access.md）。
		docMcpNorm(`{"mcpServers": {"ssh": {"command": "sshmgr", "args": ["mcp"], "env": {"SSHMGR_TOKEN": "<TOKEN>", "SSHMGR_MASTERKEY_HEX": "<HEX>"}}}}`): true,
	}

	docs := []string{
		"../../README.md",
		"../../docs/agent-access.md",
		"../../docs/getting-started.md",
		"../../docs/quickstart-single-machine.md",
	}
	for _, p := range docs {
		b, err := os.ReadFile(filepath.FromSlash(p))
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, blk := range extractDocMcpBlocks(t, string(b)) {
			if !canonical[docMcpNorm(blk)] {
				t.Errorf("%s: 片段与 `sshmgr projects add` 的输出不一致:\n%s", p, blk)
			}
		}
	}
}
