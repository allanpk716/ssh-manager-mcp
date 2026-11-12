package sshbroker

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestPowerShellSingleQuoteEscapesQuoteDelimiters 钉住 PowerShell 单引号字符
// 串的完整转义范围：ASCII 单引号与四个弯单引号都被 PowerShell 当作字符串
// 定界符，任何一个出现两次都必须写成两次，才能保持字面量语义。
func TestPowerShellSingleQuoteEscapesQuoteDelimiters(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  string
	}{
		{name: "ASCII 单引号", value: "a'b", want: `'a''b'`},
		{name: "左单引号 U+2018", value: "a‘b", want: `'a‘‘b'`},
		{name: "右单引号 U+2019", value: "a’b", want: `'a’’b'`},
		{name: "低位单引号 U+201A", value: "a‚b", want: `'a‚‚b'`},
		{name: "高位反单引号 U+201B", value: "a‛b", want: `'a‛‛b'`},
		{
			name:  "混合引号与注入语句",
			value: `x’; Remove-Item -Recurse C:\‘`,
			want:  `'x’’; Remove-Item -Recurse C:\‘‘'`,
		},
		{name: "普通字符", value: `C:\data\file.bin`, want: `'C:\data\file.bin'`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := PowerShellSingleQuote(testCase.value); got != testCase.want {
				t.Fatalf("PowerShellSingleQuote(%q) = %q, 期待 %q", testCase.value, got, testCase.want)
			}
		})
	}
}

// TestPowerShellEncodedCommandUsesUTF16LE 钉住跨 PowerShell 与 cmd 使用的封
// 装命令：脚本按 UTF-16LE 编码后整体 base64，不改动脚本内容本身。
func TestPowerShellEncodedCommandUsesUTF16LE(t *testing.T) {
	script := "Write-Output '中文’测试'"
	command := PowerShellEncodedCommand(script)
	prefix := "powershell -NoProfile -NonInteractive -EncodedCommand "
	if !strings.HasPrefix(command, prefix) {
		t.Fatalf("封装命令 = %q，期待前缀 %q", command, prefix)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(command, prefix))
	if err != nil {
		t.Fatalf("base64 解码：%v", err)
	}
	units := make([]uint16, 0, len(decoded)/2)
	for index := 0; index+1 < len(decoded); index += 2 {
		units = append(units, uint16(decoded[index])|uint16(decoded[index+1])<<8)
	}
	if got := string(utf16.Decode(units)); got != script {
		t.Fatalf("UTF-16LE 解码 = %q，期待 %q", got, script)
	}
}
