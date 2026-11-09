//go:build !windows

package clientops

import (
	"errors"
	"os"
	"syscall"
)

func lockSoftwareInventoryFile(file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return ErrSoftwareInventoryCollectionBusy
		}
		return err
	}
	return nil
}

func unlockSoftwareInventoryFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
