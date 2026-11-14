//go:build darwin

package testsshd

import (
	"syscall"

	"github.com/pkg/sftp"
)

// statVirtualFilesystemHostPath 返回虚拟文件系统宿主路径的真实空间统计。
func statVirtualFilesystemHostPath(hostPath string) (*sftp.StatVFS, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(hostPath, &stat); err != nil {
		return nil, err
	}
	return &sftp.StatVFS{
		Bsize:   uint64(stat.Bsize),
		Frsize:  uint64(stat.Bsize),
		Blocks:  stat.Blocks,
		Bfree:   stat.Bfree,
		Bavail:  stat.Bavail,
		Files:   stat.Files,
		Ffree:   stat.Ffree,
		Favail:  stat.Ffree,
		Fsid:    uint64(uint64(stat.Fsid.Val[1])<<32 | uint64(stat.Fsid.Val[0])),
		Flag:    uint64(stat.Flags),
		Namemax: 1024,
	}, nil
}
