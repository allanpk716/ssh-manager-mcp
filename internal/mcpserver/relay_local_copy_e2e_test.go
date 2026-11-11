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

	"ssh-manager-mcp/internal/models"
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

// TestRelayFileWindowsPowerShellLocalCopySuccess 钉住 Windows 本地拷贝的公
// 共行为：选址探测、盘符映射与 robocopy 在同一条命令封装内，密码只经标准
// 输入送达，任务输出不携带注入状态行。
func TestRelayFileWindowsPowerShellLocalCopySuccess(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, false)
	session := environment.openSession(t)
	sourceRemote := `Z:\Reports\source.bin`
	targetRemote := "C:/Users/Public/target.bin"
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("windows payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_share_address": `\\192.0.2.10\datasets\Reports\source.bin`,
			"to_server_id":       environment.windowsID,
			"to_path":            targetRemote,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if result.IsError {
		t.Fatalf("relay_file 失败：%s", toolResultText(result))
	}
	if strings.Contains(toolResultText(result), "mount-password") ||
		strings.Contains(toolResultText(result), networkDriveStatusPrefix) {
		t.Fatalf("启动结果泄漏密码或注入状态标记：%s", toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if output.TaskID == "" || output.Mode != relayLocalCopyMode || !output.RestartAfterInterruption {
		t.Fatalf("启动输出 = %+v，期待 Windows 本地拷贝任务", output)
	}
	stdout, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
	if status != bgStatusDone || errorText != "" {
		t.Fatalf("Windows 本地拷贝终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
	}
	for _, forbidden := range []string{"mount-password", networkDriveStatusPrefix} {
		if strings.Contains(stdout, forbidden) {
			t.Fatalf("任务输出泄漏 %q：\n%s", forbidden, stdout)
		}
	}
	if !strings.Contains(stdout, "本地拷贝模式") || !strings.Contains(stdout, "robocopy ") {
		t.Fatalf("任务输出缺少本地拷贝说明或 robocopy 安全输出：\n%s", stdout)
	}

	commands := environment.fake.recordedCommands()
	if len(commands) != 2 {
		t.Fatalf("远端命令数 = %d，期待一次选址探测和一条同会话封装命令：%q", len(commands), commands)
	}
	wrapped := commands[1]
	scripts := encodedCommandScripts(wrapped)
	if len(scripts) != 1 {
		t.Fatalf("封装内脚本数 = %d，期待一个盘符映射脚本和一个拷贝脚本：%q", len(scripts), wrapped)
	}
	if !strings.Contains(wrapped, "New-SmbMapping") ||
		!strings.Contains(wrapped, `AddArgument('\\192.0.2.10\datasets')`) ||
		!strings.Contains(wrapped, "-Persistent $false") ||
		!strings.Contains(scripts[0], "robocopy") {
		t.Fatalf("盘符映射与 robocopy 不在同一条命令封装内：%s", wrapped)
	}
	inputs := environment.fake.recordedInputs()
	if len(inputs) != 2 || string(inputs[0]) != "" || string(inputs[1]) != "mount-password" {
		t.Fatalf("标准输入帧 = %q，期待密码只在封装命令里出现", inputs)
	}
	if strings.Contains(wrapped, "mount-password") {
		t.Fatalf("封装命令文本泄漏密码：%s", wrapped)
	}
	if strings.Contains(strings.ToLower(wrapped), "net use") {
		t.Fatalf("Windows 本地拷贝不应使用 net use：%s", wrapped)
	}
	if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
		string(content) != "windows payload" {
		t.Fatalf("Windows 目标文件内容 = %q err=%v", content, err)
	}
	if _, err := os.Stat(environment.hostPath(targetRemote + relayPartialSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Windows 半成品在成功后应移除：stat err=%v", err)
	}

	auditRows, err := environment.store.QueryAudit(store.AuditFilter{
		Actions: []string{"relay-bg-start", "relay-bg-end"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditRows) != 2 || auditRows[0].Status != "ok" || auditRows[1].Status != "ok" {
		t.Fatalf("Windows 本地拷贝审计行 = %#v", auditRows)
	}
	var endRow *store.AuditRow
	for index := range auditRows {
		if auditRows[index].Action == "relay-bg-end" {
			endRow = &auditRows[index]
			break
		}
	}
	if endRow == nil || len(endRow.InjectedShareNames) != 1 ||
		endRow.InjectedShareNames[0] != "datasets" {
		t.Fatalf("Windows 本地拷贝终态审计应记录已注入共享名：%#v", endRow)
	}
	for _, row := range auditRows {
		if strings.Contains(row.Command, "mount-password") ||
			strings.Contains(row.Command, networkDriveStatusPrefix) {
			t.Fatalf("审计行泄漏密码或注入状态标记：%+v", row)
		}
	}
}

// TestRelayFileWindowsCmdLocalCopyTargetShare 钉住 cmd 默认外壳与共享地址
// 作为目标端时的对称行为；映射与 robocopy 仍保持在同一条 SSH 命令会话内。
func TestRelayFileWindowsCmdLocalCopyTargetShare(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, false)
	windowsServer, err := environment.store.GetServer(environment.windowsID)
	if err != nil || windowsServer == nil {
		t.Fatalf("读取 Windows 服务器：server=%v err=%v", windowsServer, err)
	}
	windowsServer.ShellType = models.ShellWindowsCmd
	if err := environment.store.UpdateServer(windowsServer); err != nil {
		t.Fatal(err)
	}
	session := environment.openSession(t)
	sourceRemote := "C:/Users/Public/source.bin"
	targetRemote := `Z:\Reports\target.bin`
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("cmd payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_server_id":   environment.windowsID,
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
	if output.Mode != relayLocalCopyMode || !output.RestartAfterInterruption {
		t.Fatalf("启动输出 = %+v，期待本地拷贝模式", output)
	}
	stdout, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
	if status != bgStatusDone || errorText != "" {
		t.Fatalf("cmd 本地拷贝终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
	}
	if strings.Contains(stdout, networkDriveStatusPrefix) || strings.Contains(stdout, "mount-password") {
		t.Fatalf("cmd 任务输出泄漏内部标记或密码：\n%s", stdout)
	}
	if !strings.Contains(stdout, "robocopy ") {
		t.Fatalf("cmd 任务输出缺少 robocopy 安全输出：\n%s", stdout)
	}

	commands := environment.fake.recordedCommands()
	if len(commands) != 2 {
		t.Fatalf("cmd 远端命令数 = %d，期待选址探测与一条同会话封装命令：%q", len(commands), commands)
	}
	scripts := encodedCommandScripts(commands[1])
	if len(scripts) != 2 {
		t.Fatalf("cmd 封装内脚本数 = %d，期待盘符映射与拷贝各一个：%q", len(scripts), commands[1])
	}
	if !strings.Contains(scripts[0], "New-SmbMapping") ||
		!strings.Contains(scripts[0], `AddArgument('\\192.0.2.10\datasets')`) ||
		!strings.Contains(scripts[0], "-Persistent $false") ||
		!strings.Contains(scripts[1], "robocopy") {
		t.Fatalf("cmd 封装脚本没有先后包含盘符映射与 robocopy：%q", scripts)
	}
	if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
		string(content) != "cmd payload" {
		t.Fatalf("共享目标文件内容 = %q err=%v", content, err)
	}
}

// TestRelayFileWindowsLocalCopyStopAndRerun 钉住 Windows 本地拷贝复用传输
// 任务面：停止保留半成品，重跑从头覆盖，运行中同目标冲突沿用既有保护。
func TestRelayFileWindowsLocalCopyStopAndRerun(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, false)
	session := environment.openSession(t)
	sourceRemote := `Z:\Reports\first-source.bin`
	targetRemote := "C:/Users/Public/target.bin"
	prepareSource := func(content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(environment.hostPath(sourceRemote), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prepareSource("first run")
	blockCopy := make(chan struct{})
	environment.fake.blockCopy = blockCopy
	environment.fake.keepPartial = true
	arguments := map[string]any{
		"from_share_address": `\\192.0.2.10\datasets\Reports\first-source.bin`,
		"to_server_id":       environment.windowsID,
		"to_path":            targetRemote,
	}
	start := func() RelayOutput {
		t.Helper()
		result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "relay_file", Arguments: arguments,
		})
		if err != nil || result.IsError {
			t.Fatalf("relay_file：err=%v result=%+v", err, result.Content)
		}
		var output RelayOutput
		unmarshalToolJSON(t, result, &output)
		return output
	}

	first := start()
	relayE2EWaitLine(t, session, first.TaskID, "robocopy ", 30*time.Second)
	partialDirectory := environment.hostPath(targetRemote + relayPartialSuffix)
	partialFile := filepath.Join(partialDirectory, "first-source.bin")
	if content, err := os.ReadFile(partialFile); err != nil || string(content) != "first run" {
		t.Fatalf("停止前 Windows 半成品 = %q err=%v", content, err)
	}
	if _, err := os.Stat(environment.hostPath(targetRemote)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Windows 拷贝完成前目标真名不应出现：stat err=%v", err)
	}

	conflictResult, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file", Arguments: arguments,
	})
	if err != nil {
		t.Fatalf("同目标冲突调用：%v", err)
	}
	if !conflictResult.IsError || !strings.Contains(toolResultText(conflictResult), "relay conflict") ||
		!strings.Contains(toolResultText(conflictResult), first.TaskID) {
		t.Fatalf("同目标冲突报错 = %s", toolResultText(conflictResult))
	}
	stopResult, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "exec_stop", Arguments: map[string]any{"task_id": first.TaskID},
	})
	if err != nil || stopResult.IsError {
		t.Fatalf("exec_stop：err=%v result=%+v", err, stopResult.Content)
	}
	close(blockCopy)
	if _, status, _ := relayE2EPoll(t, session, first.TaskID, 30*time.Second); status != bgStatusStopped {
		t.Fatalf("停止后的 Windows 任务终态不是 stopped")
	}
	if _, err := os.Stat(partialDirectory); err != nil {
		t.Fatalf("停止后 Windows 半成品应保留：stat err=%v", err)
	}
	if _, err := os.Stat(environment.hostPath(targetRemote)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("停止后真名不应出现：stat err=%v", err)
	}

	sourceRemote = `Z:\Reports\second-source.bin`
	prepareSource("second complete run")
	arguments["from_share_address"] = `\\192.0.2.10\datasets\Reports\second-source.bin`
	restart := start()
	stdout, status, errorText := relayE2EPoll(t, session, restart.TaskID, 30*time.Second)
	if status != bgStatusDone || errorText != "" {
		t.Fatalf("Windows 重跑终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
	}
	if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
		string(content) != "second complete run" {
		t.Fatalf("Windows 重跑目标 = %q err=%v", content, err)
	}
	if _, err := os.Stat(partialDirectory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Windows 重跑成功后半成品目录应移除：stat err=%v", err)
	}
}

// TestRelayFileWindowsLocalCopyRobocopyExitCodes 钉住 robocopy 的退出码约
// 定：0 到 7 都按成功继续改名，8 及以上才让任务失败。
func TestRelayFileWindowsLocalCopyRobocopyExitCodes(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, false)
	session := environment.openSession(t)
	sourceRemote := "C:/Users/Public/source.bin"
	targetRemote := `Z:\Reports\target.bin`
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	start := func() RelayOutput {
		t.Helper()
		result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "relay_file",
			Arguments: map[string]any{
				"from_server_id":   environment.windowsID,
				"from_path":        sourceRemote,
				"to_share_address": `\\192.0.2.10\datasets\Reports\target.bin`,
			},
		})
		if err != nil || result.IsError {
			t.Fatalf("relay_file：err=%v result=%+v", err, result.Content)
		}
		var output RelayOutput
		unmarshalToolJSON(t, result, &output)
		return output
	}

	for exitCode := 0; exitCode <= 7; exitCode++ {
		t.Run(fmt.Sprintf("成功退出码%d", exitCode), func(t *testing.T) {
			environment.fake.mu.Lock()
			environment.fake.robocopyExit = exitCode
			environment.fake.mu.Unlock()
			output := start()
			_, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
			if status != bgStatusDone || errorText != "" {
				t.Fatalf("退出码 %d 的终态 = %q 错误 = %q", exitCode, status, errorText)
			}
			if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
				string(content) != "payload" {
				t.Fatalf("退出码 %d 后目标文件 = %q err=%v", exitCode, content, err)
			}
		})
	}
	t.Run("失败退出码8", func(t *testing.T) {
		if err := os.Remove(environment.hostPath(targetRemote)); err != nil {
			t.Fatal(err)
		}
		environment.fake.mu.Lock()
		environment.fake.robocopyExit = 8
		environment.fake.mu.Unlock()
		output := start()
		_, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
		if status != bgStatusFailed || !strings.Contains(errorText, "退出码 8") {
			t.Fatalf("失败退出码终态 = %q 错误 = %q", status, errorText)
		}
		if _, err := os.Stat(environment.hostPath(targetRemote)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("失败退出码不应产生真名目标：stat err=%v", err)
		}
	})
}

// TestRelayFileWindowsLocalCopyRejectsDirectory 钉住共享地址无论作为源端
// 还是目标端，目录都只得到失败任务，不会被 robocopy 当目录同步任务执行。
func TestRelayFileWindowsLocalCopyRejectsDirectory(t *testing.T) {
	for name, arguments := range map[string]map[string]any{
		"源端目录": {
			"from_share_address": `\\192.0.2.10\datasets\Reports`,
			"to_server_id":       "__windows__",
			"to_path":            "C:/Users/Public/target.bin",
		},
		"目标端目录": {
			"from_server_id":   "__windows__",
			"from_path":        "C:/Users/Public/source.bin",
			"to_share_address": `\\192.0.2.10\datasets\Reports`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			environment := newRelayLocalCopyEnv(t, false)
			session := environment.openSession(t)
			if arguments["to_server_id"] == "__windows__" {
				arguments["to_server_id"] = environment.windowsID
			}
			if arguments["from_server_id"] == "__windows__" {
				arguments["from_server_id"] = environment.windowsID
			}
			if err := os.MkdirAll(environment.hostPath(`Z:\Reports`), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(environment.hostPath("C:/Users/Public/source.bin")), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(environment.hostPath("C:/Users/Public/source.bin"), []byte("payload"), 0o644); err != nil {
				t.Fatal(err)
			}

			result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "relay_file", Arguments: arguments,
			})
			if err != nil || result.IsError {
				t.Fatalf("relay_file：err=%v result=%+v", err, result.Content)
			}
			var output RelayOutput
			unmarshalToolJSON(t, result, &output)
			stdout, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
			if status != bgStatusFailed {
				t.Fatalf("目录端点终态 = %q 错误 = %q 输出 =\n%s", status, errorText, stdout)
			}
			if !strings.Contains(stdout, "source-not-regular") &&
				!strings.Contains(stdout, "target-directory") {
				t.Fatalf("目录端点输出 = %q", stdout)
			}
			if _, err := os.Stat(environment.hostPath("C:/Users/Public/target.bin")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("目录源端不应产生目标文件：stat err=%v", err)
			}
		})
	}
}

// TestRelayFileWindowsLocalCopyMappingFailures 钉住盘符注入失败的既有分
// 类呈现；认证失败会进入进程内暂停，后续启动不再携带旧密码重试。
func TestRelayFileWindowsLocalCopyMappingFailures(t *testing.T) {
	resetAuthenticationPause := func() {
		networkDriveAuthenticationPauseState.Lock()
		networkDriveAuthenticationPauseState.versions = map[string]int64{}
		networkDriveAuthenticationPauseState.Unlock()
	}
	resetAuthenticationPause()
	t.Cleanup(resetAuthenticationPause)

	t.Run("盘符被占用", func(t *testing.T) {
		environment := newRelayLocalCopyEnv(t, false)
		session := environment.openSession(t)
		environment.fake.mu.Lock()
		environment.fake.mapStatus = "error-85"
		environment.fake.mu.Unlock()
		if err := os.MkdirAll(filepath.Dir(environment.hostPath(`Z:\Reports\source.bin`)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(environment.hostPath(`Z:\Reports\source.bin`), []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}
		result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "relay_file",
			Arguments: map[string]any{
				"from_share_address": `\\192.0.2.10\datasets\Reports\source.bin`,
				"to_server_id":       environment.windowsID,
				"to_path":            "C:/Users/Public/target.bin",
			},
		})
		if err != nil || result.IsError {
			t.Fatalf("relay_file：err=%v result=%+v", err, result.Content)
		}
		var output RelayOutput
		unmarshalToolJSON(t, result, &output)
		stdout, status, _ := relayE2EPoll(t, session, output.TaskID, 30*time.Second)
		if status != bgStatusFailed {
			t.Fatalf("盘符占用后的任务终态 = %q 输出 =\n%s", status, stdout)
		}
		for _, want := range []string{"网络盘符 Z", "在这台服务器上已被占用"} {
			if !strings.Contains(stdout, want) {
				t.Fatalf("盘符占用警告 = %q，期待包含 %q", stdout, want)
			}
		}
		if strings.Contains(stdout, networkDriveStatusPrefix) || strings.Contains(stdout, "mount-password") {
			t.Fatalf("盘符占用任务输出泄漏内部标记或密码：\n%s", stdout)
		}
	})

	t.Run("认证失败后暂停", func(t *testing.T) {
		environment := newRelayLocalCopyEnv(t, false)
		session := environment.openSession(t)
		environment.fake.mu.Lock()
		environment.fake.mapStatus = "error-86"
		environment.fake.mu.Unlock()
		sourceHost := environment.hostPath(`Z:\Reports\source.bin`)
		if err := os.MkdirAll(filepath.Dir(sourceHost), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sourceHost, []byte("payload"), 0o644); err != nil {
			t.Fatal(err)
		}
		start := func() RelayOutput {
			t.Helper()
			result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "relay_file",
				Arguments: map[string]any{
					"from_share_address": `\\192.0.2.10\datasets\Reports\source.bin`,
					"to_server_id":       environment.windowsID,
					"to_path":            "C:/Users/Public/target.bin",
				},
			})
			if err != nil || result.IsError {
				t.Fatalf("relay_file：err=%v result=%+v", err, result.Content)
			}
			var output RelayOutput
			unmarshalToolJSON(t, result, &output)
			return output
		}
		first := start()
		firstOutput, status, _ := relayE2EPoll(t, session, first.TaskID, 30*time.Second)
		if status != bgStatusFailed || !strings.Contains(firstOutput, "共享密码认证失败，已暂停注入") {
			t.Fatalf("认证失败任务 = %q 输出 =\n%s", status, firstOutput)
		}
		if err := os.Remove(sourceHost); err != nil {
			t.Fatal(err)
		}
		second := start()
		secondOutput, status, _ := relayE2EPoll(t, session, second.TaskID, 30*time.Second)
		if status != bgStatusFailed || !strings.Contains(secondOutput, "因认证失败已暂停注入") {
			t.Fatalf("暂停后的任务 = %q 输出 =\n%s", status, secondOutput)
		}
		commands := environment.fake.recordedCommands()
		inputs := environment.fake.recordedInputs()
		if len(commands) != 3 || strings.Contains(commands[2], "New-SmbMapping") {
			t.Fatalf("认证失败暂停后仍尝试旧密码或选址：%q", commands)
		}
		if len(inputs) != 3 || string(inputs[2]) != "" {
			t.Fatalf("认证失败暂停后的标准输入 = %q，期待没有旧密码", inputs)
		}
		for _, command := range commands {
			if strings.Contains(command, "mount-password") {
				t.Fatalf("远端命令文本泄漏密码：%s", command)
			}
		}
	})
}

// TestRelayFileWindowsLocalCopyEscapesQuoteCharacters 钉住生成脚本按
// PowerShell 单引号规则转义路径，特殊字符不改变参数边界。
func TestRelayFileWindowsLocalCopyEscapesQuoteCharacters(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, false)
	session := environment.openSession(t)
	sourceRemote := `Z:\Reports\source 'quoted'.bin`
	targetRemote := "C:/Users/Public/target 'quoted'.bin"
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("quoted"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_share_address": `\\192.0.2.10\datasets\Reports\source 'quoted'.bin`,
			"to_server_id":       environment.windowsID,
			"to_path":            targetRemote,
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("relay_file：err=%v result=%+v", err, result.Content)
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if _, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second); status != bgStatusDone ||
		errorText != "" {
		t.Fatalf("带单引号路径的 Windows 本地拷贝失败：%s", errorText)
	}
	commands := environment.fake.recordedCommands()
	scripts := encodedCommandScripts(commands[len(commands)-1])
	copyScript := scripts[len(scripts)-1]
	if !strings.Contains(copyScript, `$__sshmgr_source = 'Z:\Reports\source ''quoted''.bin'`) ||
		!strings.Contains(copyScript, `$__sshmgr_target = 'C:\Users\Public\target ''quoted''.bin'`) {
		t.Fatalf("PowerShell 路径没有安全转义单引号：%s", copyScript)
	}
	if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
		string(content) != "quoted" {
		t.Fatalf("带单引号路径的目标内容 = %q err=%v", content, err)
	}
}

// TestRelayFileWindowsLocalCopyEscapesCurlyQuoteCharacters 钉住 PowerShell
// 服务器把弯单引号也当字符串定界符：路径含弯单引号时必须双写转义，不能
// 让路径片段跳出字符串注入语句。
func TestRelayFileWindowsLocalCopyEscapesCurlyQuoteCharacters(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, false)
	session := environment.openSession(t)
	sourceRemote := `Z:\Reports\source ’quoted‘.bin`
	targetRemote := `C:/Users/Public/target ‚quoted‛.bin`
	if err := os.MkdirAll(filepath.Dir(environment.hostPath(sourceRemote)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(environment.hostPath(sourceRemote), []byte("curly"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_share_address": `\\192.0.2.10\datasets\Reports\source ’quoted‘.bin`,
			"to_server_id":       environment.windowsID,
			"to_path":            targetRemote,
		},
	})
	if err != nil || result.IsError {
		t.Fatalf("relay_file：err=%v 失败=%s", err, toolResultText(result))
	}
	var output RelayOutput
	unmarshalToolJSON(t, result, &output)
	if _, status, errorText := relayE2EPoll(t, session, output.TaskID, 30*time.Second); status != bgStatusDone ||
		errorText != "" {
		t.Fatalf("带弯单引号路径的 Windows 本地拷贝失败：%s", errorText)
	}
	commands := environment.fake.recordedCommands()
	scripts := encodedCommandScripts(commands[len(commands)-1])
	copyScript := scripts[len(scripts)-1]
	if !strings.Contains(copyScript, `$__sshmgr_source = 'Z:\Reports\source ’’quoted‘‘.bin'`) ||
		!strings.Contains(copyScript, `$__sshmgr_target = 'C:\Users\Public\target ‚‚quoted‛‛.bin'`) {
		t.Fatalf("PowerShell 路径没有双写弯单引号：%s", copyScript)
	}
	if content, err := os.ReadFile(environment.hostPath(targetRemote)); err != nil ||
		string(content) != "curly" {
		t.Fatalf("带弯单引号路径的目标内容 = %q err=%v", content, err)
	}
}

// TestRelayFileLocalCopyRejectsInvalidVia 钉住另一端可直接挂载时的 via 约
// 束：这时在另一端本地拷贝，不能再指定共享地址端点。
func TestRelayFileLocalCopyRejectsInvalidVia(t *testing.T) {
	environment := newRelayLocalCopyEnv(t, true)
	session := environment.openSession(t)
	cases := []struct {
		name      string
		arguments map[string]any
		wantError string
	}{
		{
			name: "本地拷贝不能使用 via",
			arguments: map[string]any{
				"from_server_id":   environment.serverID,
				"from_path":        "/mnt/sshmgr/datasets/ordinary/source.bin",
				"to_share_address": `\\192.0.2.10\datasets\Reports\target.bin`,
				"via":              environment.serverID,
			},
			wantError: "另一端可挂载共享时直接在另一端执行本地拷贝，不能再用 via 指定共享地址端点",
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
				t.Fatalf("relay_file 应拒绝无效 via：%+v", result.Content)
			}
			text := toolResultText(result)
			if !strings.Contains(text, testCase.wantError) {
				t.Fatalf("报错 = %q，期待包含 %q", text, testCase.wantError)
			}
		})
	}
	if commands := environment.fake.recordedCommands(); len(commands) != 0 {
		t.Fatalf("本地拷贝拒绝路径不应连接服务器：%#v", commands)
	}
	if environment.connections.Load() != 0 {
		t.Fatalf("本地拷贝拒绝路径不应建立 SSH 连接：%d", environment.connections.Load())
	}

	environment.fake.mounted = false
	result, err := session.cli.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "relay_file",
		Arguments: map[string]any{
			"from_server_id":   environment.notMountableID,
			"from_path":        "/tmp/source.bin",
			"to_share_address": `\\192.0.2.10\datasets\Reports\target.bin`,
		},
	})
	if err != nil {
		t.Fatalf("调用 relay_file：%v", err)
	}
	if !result.IsError {
		t.Fatalf("无已挂载端点应拒绝：%+v", result.Content)
	}
	text := toolResultText(result)
	for _, want := range []string{
		"没有可作为共享地址端点的已实际挂载 Linux 或 macOS 服务器",
		"请先在当前授权范围内可挂载", "调整授权",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("无候选错误 = %q，期待包含 %q", text, want)
		}
	}
}
