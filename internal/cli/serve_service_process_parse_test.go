package cli

import "testing"

func TestWindowsTaskListHasServeProcessIgnoresCurrentProcess(t *testing.T) {
	output := []byte("\"sshmgr.exe\",\"100\",\"Console\",\"1\",\"12,345 K\"\r\n" +
		"\"sshmgr.exe\",\"200\",\"Console\",\"1\",\"23,456 K\"\r\n" +
		"\"my-sshmgr.exe\",\"300\",\"Console\",\"1\",\"1,234 K\"\r\n")

	if !windowsTaskListHasServeProcess(output, 100) {
		t.Fatal("another sshmgr.exe process must be detected")
	}
	if windowsTaskListHasServeProcess([]byte("\"sshmgr.exe\",\"100\",\"Console\",\"1\",\"12,345 K\"\r\n"), 100) {
		t.Fatal("the current process number must be ignored")
	}
	if windowsTaskListHasServeProcess([]byte("\"my-sshmgr.exe\",\"200\",\"Console\",\"1\",\"1,234 K\"\r\n"), 100) {
		t.Fatal("a similarly named process must be ignored")
	}
}

func TestWindowsTaskListHasServeProcessMatchesNameCaseInsensitively(t *testing.T) {
	output := []byte("\"SSHmgr.EXE\",\"200\",\"Console\",\"1\",\"12,345 K\"\r\n")
	if !windowsTaskListHasServeProcess(output, 100) {
		t.Fatal("process image names on Windows are case-insensitive")
	}
}
