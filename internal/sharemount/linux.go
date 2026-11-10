package sharemount

import (
	"fmt"
	"strings"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/sshbroker"
)

const (
	linuxMountMarker        = "SSHMGR-LINUX-MOUNT"
	linuxExitMissingCIFS    = 83
	linuxExitProtocolFailed = 84
	linuxCredentialRoot     = "/etc/sshmgr/shares"
	linuxMountRoot          = "/mnt/sshmgr"
)

func linuxMountPoint(shareName string) string {
	return linuxMountRoot + "/" + shareName
}

func linuxCredentialPath(shareID string) string {
	return linuxCredentialRoot + "/" + shareID + ".credentials"
}

// buildLinuxMountCommand 生成 Linux 首次挂载固定命令。地址、共享名与挂载
// 账号全部经单引号包装；挂载密码只由调用方经标准输入传入，绝不出现在返
// 回的命令文本里。
func buildLinuxMountCommand(share *models.SharedStorage, address string) string {
	shareName := sshbroker.POSIXShellQuote(share.Name)
	mountUser := sshbroker.POSIXShellQuote(share.MountUser)
	source := sshbroker.POSIXShellQuote("//" + address + "/" + share.Name)
	mountPoint := sshbroker.POSIXShellQuote(linuxMountPoint(share.Name))
	credential := sshbroker.POSIXShellQuote(linuxCredentialPath(share.ID))

	return fmt.Sprintf(`set -eu
if ! command -v mount.cifs >/dev/null 2>&1; then
  echo %[1]q missing-mount-cifs >&2
  exit %[2]d
fi
install -d -m 700 %[3]s
install -d -m 755 %[4]s
credential_file=%[5]s
mount_point=%[6]s
install -d -m 755 "$mount_point"
{
  printf 'username=%%s\n' %[7]s
  printf 'password='
  cat
  printf '\n'
} > "$credential_file"
chown root:root "$credential_file"
chmod 600 "$credential_file"
selected_version=''
for sshmgr_version in 3.1.1 3.0 2.1 2.0; do
  if mount -t cifs %[8]s "$mount_point" -o "vers=$sshmgr_version,credentials=$credential_file"; then
    selected_version="$sshmgr_version"
    break
  fi
done
if [ -z "$selected_version" ]; then
  rm -f "$credential_file"
  echo %[1]q protocol-failed >&2
  exit %[9]d
fi
if ! umount "$mount_point"; then
  echo %[1]q trial-unmount-failed >&2
  exit 85
fi
fstab_options="credentials=$credential_file,vers=$selected_version,_netdev,nofail,x-systemd.automount,x-systemd.mount-timeout=10"
fstab_line=$(printf '//%%s/%%s %%s cifs %%s 0 0\n' %[10]s %[11]s "$mount_point" "$fstab_options")
if ! grep -Fqx "$fstab_line" /etc/fstab; then
  printf '%%s\n' "$fstab_line" >> /etc/fstab
fi
systemctl daemon-reload
automount_unit=$(systemd-escape -p --suffix=automount "$mount_point")
systemctl start "$automount_unit"
if ! ls -A "$mount_point" >/dev/null 2>&1; then
  echo %[1]q verify-failed >&2
  exit 86
fi
echo %[1]q ok`,
		linuxMountMarker, linuxExitMissingCIFS,
		sshbroker.POSIXShellQuote(linuxCredentialRoot), sshbroker.POSIXShellQuote(linuxMountRoot),
		credential, mountPoint, mountUser, source, linuxExitProtocolFailed,
		sshbroker.POSIXShellQuote(address), shareName,
	)
}

func safeLinuxShareName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}
