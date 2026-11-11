package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/store"
)

// TestRelayFileSharedEndpointInputRules 在 relay_file 工具面钉住共享端点
// 的输入契约：共享地址端点与既有路径端点互斥，且一次传输最多一个共享
// 地址端点。
func TestRelayFileSharedEndpointInputRules(t *testing.T) {
	env := newRelayE2EEnv(t)
	session := env.openSession(t)

	cases := []struct {
		name      string
		arguments map[string]any
		wantError string
	}{
		{
			name: "源端共享地址不能同时给源服务器路径",
			arguments: map[string]any{
				"from_share_address": `\\192.0.2.10\datasets\Reports\2026.bin`,
				"from_path":          "/tmp/source.bin",
				"to_server_id":       env.dstID,
				"to_path":            "/tmp/target.bin",
			},
			wantError: "from_share_address、from_server_id 与 from_path 不能同时作为源端使用",
		},
		{
			name: "目标端共享地址不能同时给目标服务器路径",
			arguments: map[string]any{
				"from_server_id":   env.srcID,
				"from_path":        "/tmp/source.bin",
				"to_share_address": `\\192.0.2.10\datasets\Reports\2026.bin`,
				"to_server_id":     env.dstID,
				"to_path":          "/tmp/target.bin",
			},
			wantError: "to_share_address、to_server_id 与 to_path 不能同时作为目标端使用",
		},
		{
			name: "两端都是共享地址时直接拒绝",
			arguments: map[string]any{
				"from_share_address": `\\192.0.2.10\datasets\Reports\2026.bin`,
				"to_share_address":   `\\192.0.2.10\datasets\Archive\2026.bin`,
			},
			wantError: "源端和目标端不能都是共享地址；请先用 query_topology 查询实际路径，再在可挂载共享的服务器上用 exec_command 拷贝",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "relay_file", Arguments: testCase.arguments,
			})
			if err != nil {
				t.Fatalf("调用 relay_file：%v", err)
			}
			if !result.IsError {
				t.Fatalf("relay_file 应拒绝该输入：%+v", result.Content)
			}
			text := toolResultText(result)
			if !strings.Contains(text, testCase.wantError) {
				t.Fatalf("报错 = %q，期待包含 %q", text, testCase.wantError)
			}
		})
	}
}

// TestRelayFileTargetShareLocalCopySuccess 覆盖目标端共享地址：挂载现场
// 检查通过后，同一台服务器上的一条 SSH 会话完成半成品拷贝与改名。
func TestRelayFileTargetShareLocalCopySuccess(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, true)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/ordinary/source file.bin"
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("local copy payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(environment.hostPath(sourceRemote)); err != nil {
		t.Fatalf("测试源文件不存在：%v（%s）", err, environment.hostPath(sourceRemote))
	}

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_server_id":   environment.serverID,
			"from_path":        sourceRemote,
			"to_share_address": `\\192.0.2.10\datasets\Reports\2026.bin`,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if result.IsError {
		t.Fatalf("relay_file 失败：%s", toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if output.TaskID == "" || output.Mode != "local_copy" || !output.RestartAfterInterruption {
		t.Fatalf("启动结果 = %+v，期待本地拷贝任务", output)
	}
	if output.BytesTotal != int64(len("local copy payload")) || output.ChunksTotal != 0 {
		t.Fatalf("启动结果字节与块数 = %+v", output)
	}

	stdout, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
	if status != bgStatusDone || errorText != "" {
		t.Fatalf("终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
	}
	for _, fragment := range []string{
		"本地拷贝模式：中断、停止或失败后不能续传，重跑会从文件开头覆盖旧半成品",
		"cp -- '/mnt/sshmgr/datasets/ordinary/source file.bin' '/mnt/sshmgr/datasets/Reports/2026.bin.sshmgr-partial'",
		"mv -- '/mnt/sshmgr/datasets/Reports/2026.bin.sshmgr-partial' '/mnt/sshmgr/datasets/Reports/2026.bin'",
	} {
		if !strings.Contains(stdout, fragment) {
			t.Fatalf("任务输出缺少 %q：\n%s", fragment, stdout)
		}
	}
	if strings.Contains(stdout, "chunk ") {
		t.Fatalf("本地拷贝不应伪造块进度：\n%s", stdout)
	}
	if strings.Contains(stdout, "mount-password") || strings.Contains(stdout, "mount-user") {
		t.Fatalf("任务输出泄漏挂载凭据：\n%s", stdout)
	}
	target := filepath.Join(environment.root, filepath.FromSlash("Reports/2026.bin"))
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "local copy payload" {
		t.Fatalf("目标文件内容 = %q err=%v", content, err)
	}
	if _, err := os.Stat(target + ".sshmgr-partial"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("半成品应已改名：stat err=%v", err)
	}
	commands := environment.fake.recordedCommands()
	if len(commands) != 4 {
		t.Fatalf("假服务器收到命令 = %#v，期待系统识别、挂载检查、文件检查、本地拷贝四条", commands)
	}
	if connectionCount := environment.connections.Load(); connectionCount != 1 {
		t.Fatalf("SSH 连接数 = %d，期待挂载检查、文件检查与本地拷贝共用一条连接", connectionCount)
	}
	for _, command := range commands {
		if strings.Contains(command, "mount-password") || strings.Contains(command, "mount-user") {
			t.Fatalf("固定命令泄漏挂载凭据：%s", command)
		}
	}
	rows, err := environment.store.QueryAudit(store.AuditFilter{
		Actions: []string{"relay-bg-start", "relay-bg-end"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var auditText strings.Builder
	for _, row := range rows {
		fmt.Fprintf(&auditText, "%s %s %s\n", row.Action, row.Status, row.Command)
	}
	startRows, endRows := 0, 0
	for _, row := range rows {
		switch row.Action {
		case "relay-bg-start":
			startRows++
		case "relay-bg-end":
			endRows++
		}
	}
	if startRows != 1 || endRows != 1 {
		t.Fatalf("本地拷贝审计行 start=%d end=%d，都应为 1：\n%s", startRows, endRows, auditText.String())
	}
	if !strings.Contains(auditText.String(), "relay local-copy share=datasets") {
		t.Fatalf("审计应记录本地拷贝模式：\n%s", auditText.String())
	}
	if strings.Contains(auditText.String(), "mount-password") ||
		strings.Contains(auditText.String(), "mount-user") {
		t.Fatalf("审计泄漏挂载凭据：\n%s", auditText.String())
	}
}

// TestRelayFileTargetShareNotMountedGivesMountGuidance 钉住未实际挂载时不
// 自动挂载、不启动拷贝，并提示先调用幂等挂载工具。
func TestRelayFileTargetShareNotMountedGivesMountGuidance(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, false)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/ordinary/source.bin"
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_server_id":   environment.serverID,
			"from_path":        sourceRemote,
			"to_share_address": `\\192.0.2.10\datasets\Reports\2026.bin`,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if !result.IsError {
		t.Fatalf("relay_file 应在未挂载时拒绝：%+v", result.Content)
	}
	text := toolResultText(result)
	want := "共享存储 datasets 在服务器 本地拷贝服务器 上尚未实际挂载；请先调用幂等的 mount_share 工具，再重试 relay_file"
	if !strings.Contains(text, want) {
		t.Fatalf("报错 = %q，期待包含 %q", text, want)
	}
	commands := environment.fake.recordedCommands()
	if len(commands) != 2 || strings.Contains(commands[len(commands)-1], "cp -- ") {
		t.Fatalf("未挂载时只应识别系统并现场检查，不应拷贝：%#v", commands)
	}
}

// TestRelayFileSourceShareLocalCopySuccess 覆盖源端共享地址的方向对称性。
func TestRelayFileSourceShareLocalCopySuccess(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, true)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/Reports/2026.bin"
	targetRemote := "/mnt/sshmgr/datasets/ordinary/target.bin"
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("source share payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_share_address": `smb://192.0.2.10/datasets/Reports/2026.bin`,
			"to_server_id":       environment.serverID,
			"to_path":            targetRemote,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if result.IsError {
		t.Fatalf("relay_file 失败：%s", toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if output.Mode != relayLocalCopyMode || !output.RestartAfterInterruption || output.BytesTotal != int64(len("source share payload")) {
		t.Fatalf("启动结果 = %+v，期待源端共享地址走本地拷贝", output)
	}
	stdout, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
	if status != bgStatusDone || errorText != "" {
		t.Fatalf("终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
	}
	content, err := os.ReadFile(environment.hostPath(targetRemote))
	if err != nil || string(content) != "source share payload" {
		t.Fatalf("目标文件内容 = %q err=%v", content, err)
	}
	if _, err := os.Stat(environment.hostPath(targetRemote) + ".sshmgr-partial"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("半成品应已改名：stat err=%v", err)
	}
}

// TestRelayFileLocalCopyStopLeavesPartialAndRerunOverwrites 覆盖现有停止
// 工具、半成品保留以及本地拷贝从头重跑的语义。
func TestRelayFileLocalCopyStopLeavesPartialAndRerunOverwrites(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, true)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/ordinary/source.bin"
	targetRemote := "/mnt/sshmgr/datasets/Reports/target.bin"
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("first run"), 0o644); err != nil {
		t.Fatal(err)
	}
	blockCopy := make(chan struct{})
	environment.fake.blockCopy = blockCopy
	environment.fake.keepPartial = true

	start := func() RelayOutput {
		result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "relay_file",
			Arguments: map[string]any{
				"from_server_id":   environment.serverID,
				"from_path":        sourceRemote,
				"to_share_address": `\\192.0.2.10\datasets\Reports\target.bin`,
			},
		})
		if err != nil {
			t.Fatalf("调用 relay_file：%v", err)
		}
		if result.IsError {
			t.Fatalf("relay_file 失败：%s", toolResultText(result))
		}
		var output RelayOutput
		unmarshalToolJSON(t, result, &output)
		return output
	}

	first := start()
	relayE2EWaitLine(t, session, first.TaskID, "cp -- ", 30*time.Second)
	partialPath := environment.hostPath(targetRemote) + relayPartialSuffix
	if content, err := os.ReadFile(partialPath); err != nil || string(content) != "first run" {
		t.Fatalf("停止前半成品 = %q err=%v", content, err)
	}
	stopResult, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_stop", Arguments: map[string]any{"task_id": first.TaskID},
	})
	if err != nil || stopResult.IsError {
		t.Fatalf("exec_stop：err=%v result=%+v", err, stopResult.Content)
	}
	close(blockCopy)
	_, status, _ := relayE2EPoll(t, session, first.TaskID, 30*time.Second)
	if status != bgStatusStopped {
		t.Fatalf("第一次任务终态 = %q，期待 stopped", status)
	}
	if content, err := os.ReadFile(partialPath); err != nil || string(content) != "first run" {
		t.Fatalf("停止后半成品 = %q err=%v", content, err)
	}
	if _, err := os.Stat(environment.hostPath(targetRemote)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("停止后真名不应出现：stat err=%v", err)
	}
	if _, err := os.Stat(environment.hostPath(targetRemote) + relayManifestSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("本地拷贝不应生成块清单：stat err=%v", err)
	}

	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("second complete run"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := start()
	stdout, status, errorText := relayE2EPoll(t, session, second.TaskID, 30*time.Second)
	if status != bgStatusDone || errorText != "" {
		t.Fatalf("重跑终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
	}
	if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
		string(content) != "second complete run" {
		t.Fatalf("重跑目标 = %q err=%v", content, err)
	}
	if _, err := os.Stat(partialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("重跑成功后半成品应改名：stat err=%v", err)
	}
}

// TestRelayFileLocalCopyRunningTargetConflict 钉住本地拷贝与既有传输任务
// 共用运行中目标工件冲突保护。
func TestRelayFileLocalCopyRunningTargetConflict(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, true)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/ordinary/source.bin"
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("running"), 0o644); err != nil {
		t.Fatal(err)
	}
	blockCopy := make(chan struct{})
	environment.fake.blockCopy = blockCopy
	environment.fake.keepPartial = true

	arguments := map[string]any{
		"from_server_id":   environment.serverID,
		"from_path":        sourceRemote,
		"to_share_address": `\\192.0.2.10\datasets\Reports\target.bin`,
	}
	firstResult, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file", Arguments: arguments,
	})
	if err != nil || firstResult.IsError {
		t.Fatalf("第一次 relay_file：err=%v result=%+v", err, firstResult.Content)
	}
	var first RelayOutput
	unmarshalToolJSON(t, firstResult, &first)
	relayE2EWaitLine(t, session, first.TaskID, "cp -- ", 30*time.Second)

	secondResult, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file", Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("第二次 relay_file：%v", err)
	}
	if !secondResult.IsError {
		t.Fatalf("同目标运行中冲突应被拒绝：%+v", secondResult.Content)
	}
	text := toolResultText(secondResult)
	if !strings.Contains(text, "relay conflict") || !strings.Contains(text, "exec_stop") {
		t.Fatalf("冲突报错 = %q，期待提示等待或 exec_stop", text)
	}
	if !strings.Contains(text, first.TaskID) {
		t.Fatalf("冲突报错应点名运行中任务 %s：%s", first.TaskID, text)
	}

	stopResult, stopErr := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_stop", Arguments: map[string]any{"task_id": first.TaskID},
	})
	if stopErr != nil || stopResult.IsError {
		t.Fatalf("exec_stop：err=%v result=%+v", stopErr, stopResult.Content)
	}
	close(blockCopy)
	if _, status, _ := relayE2EPoll(t, session, first.TaskID, 30*time.Second); status != bgStatusStopped {
		t.Fatalf("第一次任务终态 = %q，期待 stopped", status)
	}
}

// TestRelayFileLocalCopyRejectsDirectory 保证共享地址指到目录时沿用常规文
// 件限制与目录指引。
func TestRelayFileLocalCopyRejectsDirectory(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, true)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/ordinary/source.bin"
	directoryRemote := "/mnt/sshmgr/datasets/Reports"
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(environment.hostPath(directoryRemote), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_server_id":   environment.serverID,
			"from_path":        sourceRemote,
			"to_share_address": `\\192.0.2.10\datasets\Reports`,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if !result.IsError {
		t.Fatalf("目录共享地址应被拒绝：%+v", result.Content)
	}
	text := toolResultText(result)
	if !strings.Contains(text, "目标路径 /mnt/sshmgr/datasets/Reports 是目录") ||
		!strings.Contains(text, "请先打包目录再传输") {
		t.Fatalf("目录报错 = %q", text)
	}
	for _, command := range environment.fake.recordedCommands() {
		if strings.Contains(command, "cp -- ") {
			t.Fatalf("目录目标不应启动拷贝：%s", command)
		}
	}
}

// TestRelayFileLocalCopyEscapesQuoteCharacters 钉住固定 cp 命令对路径中的
// 单引号做 POSIX shell 安全转义。
func TestRelayFileLocalCopyEscapesQuoteCharacters(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, true)
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/ordinary/source 'quoted'.bin"
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("quoted"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_server_id":   environment.serverID,
			"from_path":        sourceRemote,
			"to_share_address": `\\192.0.2.10\datasets\Reports\target 'quoted'.bin`,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if result.IsError {
		t.Fatalf("relay_file 失败：%s", toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if _, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second); status != bgStatusDone ||
		errorText != "" {
		t.Fatalf("带单引号路径的拷贝终态 = %q 错误 = %q", status, errorText)
	}
	commands := environment.fake.recordedCommands()
	copyCommand := commands[len(commands)-1]
	if !strings.Contains(copyCommand, `source '\''quoted'\''.bin`) ||
		!strings.Contains(copyCommand, `target '\''quoted'\''.bin.sshmgr-partial`) {
		t.Fatalf("固定命令没有安全转义单引号：%s", copyCommand)
	}
	content, err := os.ReadFile(environment.hostPath(`/mnt/sshmgr/datasets/Reports/target 'quoted'.bin`))
	if err != nil || string(content) != "quoted" {
		t.Fatalf("目标文件内容 = %q err=%v", content, err)
	}
}

// TestRelayFileDarwinLocalCopySuccess 钉住 macOS 现场检查与同一固定本地拷
// 贝命令同样可用。
func TestRelayFileDarwinLocalCopySuccess(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, true)
	environment.fake.systemName = "Darwin"
	session := environment.openSession(t)
	sourceRemote := "/mnt/sshmgr/datasets/ordinary/source.bin"
	targetRemote := "/mnt/sshmgr/datasets/Reports/target.bin"
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("darwin payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_server_id":   environment.serverID,
			"from_path":        sourceRemote,
			"to_share_address": `\\192.0.2.10\datasets\Reports\target.bin`,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if result.IsError {
		t.Fatalf("relay_file 失败：%s", toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	stdout, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
	if status != bgStatusDone || errorText != "" {
		t.Fatalf("macOS 本地拷贝终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
	}
	if !strings.Contains(environment.fake.recordedCommands()[1], "SSHMGR-DARWIN-CHECK") {
		t.Fatalf("macOS 应使用自己的只读挂载现场检查：%#v", environment.fake.recordedCommands())
	}
	content, err := os.ReadFile(environment.hostPath(targetRemote))
	if err != nil || string(content) != "darwin payload" {
		t.Fatalf("macOS 目标文件内容 = %q err=%v", content, err)
	}
}

// TestRelayFileLocalCopyUnsupportedEndpoints 钉住本票未实现的选择与 Windows
// 路径必须明确报错，不能悄悄回退或自动挂载。
func TestRelayFileLocalCopyUnsupportedEndpoints(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, true)
	session := environment.openSession(t)
	cases := []struct {
		name      string
		arguments map[string]any
		wantError string
	}{
		{
			name: "via 尚未支持",
			arguments: map[string]any{
				"from_server_id":   environment.serverID,
				"from_path":        "/mnt/sshmgr/datasets/ordinary/source.bin",
				"to_share_address": `\\192.0.2.10\datasets\Reports\target.bin`,
				"via":              environment.serverID,
			},
			wantError: "via 指定共享地址端点服务器的流式回退尚未支持",
		},
		{
			name: "Windows 本地拷贝尚未支持",
			arguments: map[string]any{
				"from_share_address": `\\192.0.2.10\datasets\Reports\source.bin`,
				"to_server_id":       environment.windowsID,
				"to_path":            "C:/Users/Public/target.bin",
			},
			wantError: "Windows 服务器的共享地址本地拷贝尚未支持",
		},
		{
			name: "另一端不可挂载时回退尚未支持",
			arguments: map[string]any{
				"from_server_id":   environment.notMountableID,
				"from_path":        "/tmp/source.bin",
				"to_share_address": `\\192.0.2.10\datasets\Reports\target.bin`,
			},
			wantError: "服务器 不可挂载服务器 不在共享存储 datasets 的可挂载范围内；另一端不可挂载时的流式回退尚未支持",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "relay_file", Arguments: testCase.arguments,
			})
			if err != nil {
				t.Fatalf("调用 relay_file：%v", err)
			}
			if !result.IsError {
				t.Fatalf("relay_file 应明确拒绝尚未支持的情形：%+v", result.Content)
			}
			text := toolResultText(result)
			if !strings.Contains(text, testCase.wantError) {
				t.Fatalf("报错 = %q，期待包含 %q", text, testCase.wantError)
			}
		})
	}
	if commands := environment.fake.recordedCommands(); len(commands) != 0 {
		t.Fatalf("尚未支持的情形不应连接服务器：%#v", commands)
	}
	if environment.connections.Load() != 0 {
		t.Fatalf("尚未支持的情形不应建立 SSH 连接：%d", environment.connections.Load())
	}
}
