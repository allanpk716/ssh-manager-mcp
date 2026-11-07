//go:build windows

package sshbroker

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type dialSuccessFileLock struct {
	file *os.File
}

func (l *dialSuccessFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	var overlapped windows.Overlapped
	err := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &overlapped)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

func lockDialSuccessCache(path string) (*dialSuccessFileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	err = windows.LockFileEx(
		windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &overlapped,
	)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &dialSuccessFileLock{file: file}, nil
}
