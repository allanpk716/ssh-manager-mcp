//go:build windows

package proberesults

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

type probeResultsFileLock struct {
	file *os.File
}

func (l *probeResultsFileLock) Close() error {
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

func lockProbeResults(path string) (*probeResultsFileLock, error) {
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
	return &probeResultsFileLock{file: file}, nil
}
