package cli

import (
	"bytes"
	"strings"
	"testing"
)

// runCliCapturingStderr 执行一条预期成功的命令，并分别返回标准输出与标准
// 错误，供“成功但警告”的输出测试使用。
func runCliCapturingStderr(t *testing.T, args ...string) (string, string) {
	t.Helper()
	root := NewRootCmd()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		t.Fatalf("cli %v: %v", args, err)
	}
	return stdout.String(), stderr.String()
}

// TestDevicesNetworkMemberConflictWarnsOnStderr 覆盖命令行新增与修改：等价
// 地址同时登记为物理局域网成员与虚拟局域网成员时登记成功，警告走标准错
// 误；地址不同则没有警告。
func TestDevicesNetworkMemberConflictWarnsOnStderr(t *testing.T) {
	withCliStoreEnv(t)
	runCli(t, "sites", "add", "家A")
	runCli(t, "networks", "add", "家A局域网", "--type", "physical", "--site", "家A")
	runCli(t, "networks", "add", "虚拟网", "--type", "virtual")
	runCli(t, "devices", "add", "工作站")
	runCli(t, "devices", "member", "add", "工作站", "--network", "虚拟网", "--address", "100.64.0.1")

	stdout, stderr := runCliCapturingStderr(t,
		"devices", "member", "add", "工作站", "--network", "家A局域网", "--address", "100.64.0.1",
	)
	if !strings.Contains(stdout, "已把设备 工作站 加入网络 家A局域网：100.64.0.1") {
		t.Fatalf("冲突时新增仍应成功并输出成功提示：\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	for _, want := range []string{
		"警告：地址 100.64.0.1 同时登记在物理局域网 家A局域网 与虚拟局域网 虚拟网",
		"无法自动首次锚定", "servers pin-hostkey", "--fingerprint",
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("标准错误缺少警告片段 %q：\nstdout=%s\nstderr=%s", want, stdout, stderr)
		}
	}

	stdout, stderr = runCliCapturingStderr(t,
		"devices", "member", "edit", "工作站", "--network", "家A局域网", "--address", "192.0.2.1",
	)
	if !strings.Contains(stdout, "已修改设备 工作站 在网络 家A局域网 的地址：192.0.2.1") || stderr != "" {
		t.Fatalf("地址不同时应只有成功提示：\nstdout=%s\nstderr=%s", stdout, stderr)
	}

	stdout, stderr = runCliCapturingStderr(t,
		"devices", "member", "edit", "工作站", "--network", "家A局域网", "--address", "100.64.0.1",
	)
	if !strings.Contains(stdout, "已修改设备 工作站 在网络 家A局域网 的地址：100.64.0.1") {
		t.Fatalf("冲突时修改仍应成功并输出成功提示：\nstdout=%s\nstderr=%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "警告：地址 100.64.0.1") {
		t.Fatalf("修改冲突时标准错误应包含警告：\nstdout=%s\nstderr=%s", stdout, stderr)
	}
}
