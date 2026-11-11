package cli

import (
	"errors"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// readSharedStorageNewPassword 只接受文件与标准输入两种来源，读到的内容
// 不进入命令参数，也不在任何错误里回显。
func readSharedStorageNewPassword(
	cmd *cobra.Command, fromFile string, fromStdin bool,
) ([]byte, error) {
	if fromFile != "" && fromStdin {
		return nil, errors.New(
			"--new-password-from-file 与 --new-password-stdin 不能同时使用")
	}
	switch {
	case fromFile != "":
		return os.ReadFile(fromFile)
	case fromStdin:
		return io.ReadAll(cmd.InOrStdin())
	default:
		return nil, errors.New(
			"更换共享存储挂载密码必须使用 --new-password-from-file 或 --new-password-stdin")
	}
}
