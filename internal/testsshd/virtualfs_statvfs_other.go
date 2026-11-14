//go:build !darwin && !linux

package testsshd

import (
	"os"

	"github.com/pkg/sftp"
)

// statVirtualFilesystemHostPath 在不支持空间统计的平台上保持不可用，与
// SFTP 服务端库的原生行为一致。
func statVirtualFilesystemHostPath(string) (*sftp.StatVFS, error) {
	return nil, os.ErrInvalid
}
