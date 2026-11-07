package roles

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"ssh-manager-mcp/internal/store"
)

// localDeviceSetting is the machine-local file form of the owner's「本机设备」
// setting. It deliberately lives beside role.json rather than inside the vault
// database, so backups and exports can never carry one machine's identity to
// another machine.
type localDeviceSetting struct {
	DeviceID string `json:"device_id"`
}

// LocalDevicePath returns the standalone/server state file that records which
// topology device is this machine.
func LocalDevicePath() (string, error) {
	p := vaultRolePath()
	if p == "" {
		return "", errors.New("vault dir unresolvable (cannot locate local-device.json)")
	}
	return filepath.Join(filepath.Dir(p), "local-device.json"), nil
}

// SetLocalDeviceID atomically records a topology device identifier as this
// machine's local device.
func SetLocalDeviceID(deviceID string) error {
	if deviceID == "" {
		return errors.New("local device identifier is empty")
	}
	path, err := LocalDevicePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	blob, err := json.Marshal(localDeviceSetting{DeviceID: deviceID})
	if err != nil {
		return err
	}
	if err := atomicWriteUnique(path, blob); err != nil {
		return err
	}
	return store.HardenACL(path)
}

// ReadLocalDeviceID returns the configured local topology device identifier.
// An absent file is the empty value, not an error; a corrupt file is an error.
func ReadLocalDeviceID() (string, error) {
	path, err := LocalDevicePath()
	if err != nil {
		return "", err
	}
	blob, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var setting localDeviceSetting
	if err := json.Unmarshal(blob, &setting); err != nil {
		return "", fmt.Errorf("local-device.json (%s) 内容非法: %w", path, err)
	}
	if setting.DeviceID == "" {
		return "", fmt.Errorf("local-device.json (%s) 缺少 device_id", path)
	}
	return setting.DeviceID, nil
}

// ClearLocalDevice removes the machine-local setting and reports whether a file
// was removed.
func ClearLocalDevice() (bool, error) {
	path, err := LocalDevicePath()
	if err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
