//go:build windows

package clientops

import (
	"errors"
	"os"
	"syscall"
)

const (
	softwareInventoryLockExclusive       = 0x00000002
	softwareInventoryLockFailImmediately = 0x00000001
)

func lockSoftwareInventoryFile(file *os.File) error {
	var overlapped syscall.Overlapped
	err := syscall.LockFileEx(
		syscall.Handle(file.Fd()),
		softwareInventoryLockExclusive|softwareInventoryLockFailImmediately,
		0, 1, 0, &overlapped,
	)
	if err != nil {
		if errors.Is(err, syscall.ERROR_LOCK_VIOLATION) {
			return ErrSoftwareInventoryCollectionBusy
		}
		return err
	}
	return nil
}

func unlockSoftwareInventoryFile(file *os.File) error {
	var overlapped syscall.Overlapped
	return syscall.UnlockFileEx(syscall.Handle(file.Fd()), 0, 1, 0, &overlapped)
}
