Output "/tmp/sshmgr-tutorial-frame.mp4"
Set Shell "bash"
Set FontSize 16
Set Width 1200
Set Height 640
Hide
Type "source ./.demo-env" Sleep 200ms Enter
Type "./prepare-claude.sh" Sleep 200ms Enter
Type "source ./.claude-env" Sleep 200ms Enter
Sleep 1s
Type "clear" Sleep 200ms Enter
Sleep 500ms
Show
Type "claude -p --output-format text --mcp-config .mcp.json --strict-mcp-config --allowedTools='mcp__ssh__*' '只使用名为 ssh 的 MCP 服务器：列出可用服务器，然后在 demo-gpu 上运行 uname -a，简要说明结果。'" Sleep 1s Enter
Wait+Screen@180s /GNU\/Linux/
Sleep 3s
Screenshot "../14-claude-code.png"
Sleep 500ms
