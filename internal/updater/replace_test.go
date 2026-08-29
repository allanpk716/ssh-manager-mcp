package updater

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 事务性替换/自愈/staged 自检的临时目录副本测试(不限平台):
// GOOS 分支经 currentGOOS seam 注入,rename/remove/executable/fsync/exec 经
// 包级 seam 注入,双平台分支都能在本机跑。

const (
	oldSelfBytes = "OLD-BINARY-IMAGE-v0.12.0\n"
	newSelfBytes = "NEW-BINARY-IMAGE-v0.13.0\n"
)

func writeBin(t *testing.T, path string, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readBin(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists (err=%v), want missing", path, err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s missing: %v", path, err)
	}
}

// renameCall 记录一次 osRename seam 调用。
type renameCall struct{ from, to string }

// recordingRename 包装基础 rename:记录每次调用,并按 fail 谓词注入失败。
func recordingRename(base func(string, string) error, calls *[]renameCall, fail func(from, to string) bool) func(string, string) error {
	return func(from, to string) error {
		*calls = append(*calls, renameCall{from, to})
		if fail != nil && fail(from, to) {
			return fmt.Errorf("injected rename failure: %s -> %s", from, to)
		}
		return base(from, to)
	}
}

func seamExecutable(t *testing.T, path string) {
	t.Helper()
	orig := osExecutable
	osExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { osExecutable = orig })
}

func TestReplaceBinaryWindowsHappyPath(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "sshmgr")
	staged := filepath.Join(dir, "staged-bin")
	writeBin(t, self, oldSelfBytes)
	writeBin(t, staged, newSelfBytes)
	// 预置残留代际:成功路径应先被尽力清理
	writeBin(t, self+".old.111", "gen-111")
	writeBin(t, self+".old.222", "gen-222")

	origGOOS, origFS := currentGOOS, fileSync
	currentGOOS = "windows"
	fileSync = func(f *os.File) error { return nil } // seam 化,避免平台噪音
	defer func() { currentGOOS, fileSync = origGOOS, origFS }()

	var calls []renameCall
	origRename := osRename
	osRename = recordingRename(origRename, &calls, nil)
	defer func() { osRename = origRename }()

	if err := ReplaceBinary(staged, self); err != nil {
		t.Fatalf("ReplaceBinary: %v", err)
	}
	if got := readBin(t, self); got != newSelfBytes {
		t.Fatalf("self content = %q, want new image", got)
	}
	mustNotExist(t, staged)
	// 恰剩一代 = 本代 backup(持有旧字节);预置残留已被尽力清理
	gens, err := oldGenerations(self)
	if err != nil {
		t.Fatal(err)
	}
	if len(gens) != 1 {
		t.Fatalf("generations after replace = %d (%v), want exactly 1", len(gens), gens)
	}
	if got := readBin(t, gens[0].path); got != oldSelfBytes {
		t.Fatalf("backup content = %q, want old image", got)
	}
	if len(calls) != 2 || calls[0].from != self || calls[1].from != staged || calls[1].to != self {
		t.Fatalf("rename calls = %v, want [self→backup, staged→self]", calls)
	}
}

func TestStagedFSyncFailureBlocksRename(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "sshmgr")
	staged := filepath.Join(dir, "staged-bin")
	writeBin(t, self, oldSelfBytes)
	writeBin(t, staged, newSelfBytes)

	origFS := fileSync
	fileSync = func(f *os.File) error { return errors.New("injected fsync failure") }
	defer func() { fileSync = origFS }()

	var calls []renameCall
	origRename := osRename
	osRename = recordingRename(origRename, &calls, nil)
	defer func() { osRename = origRename }()

	err := ReplaceBinary(staged, self)
	if err == nil || !strings.Contains(err.Error(), "injected fsync failure") {
		t.Fatalf("err = %v, want injected fsync failure", err)
	}
	if len(calls) != 0 {
		t.Fatalf("rename entered despite staged fsync failure: %v", calls)
	}
	if got := readBin(t, self); got != oldSelfBytes {
		t.Fatalf("self content = %q, want unchanged", got)
	}
	if got := readBin(t, staged); got != newSelfBytes {
		t.Fatalf("staged content = %q, want unchanged", got)
	}
}

func TestReplaceWindowsRollbackOnStagedRenameFailure(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "sshmgr")
	staged := filepath.Join(dir, "staged-bin")
	writeBin(t, self, oldSelfBytes)
	writeBin(t, staged, newSelfBytes)
	// 预置两代旧备份:windows 分支起手清残留(尽力)会先清掉它们,
	// 因此回滚时"最新代际"只能是本代 backup——断言正是回滚它,而非捞旧代。
	writeBin(t, self+".old.111", "gen-111")
	writeBin(t, self+".old.222", "gen-222")

	origGOOS := currentGOOS
	currentGOOS = "windows"
	defer func() { currentGOOS = origGOOS }()

	var calls []renameCall
	origRename := osRename
	osRename = recordingRename(origRename, &calls, func(from, to string) bool {
		return from == staged // 仅 staged→self 注入失败
	})
	defer func() { osRename = origRename }()

	err := ReplaceBinary(staged, self)
	if err == nil {
		t.Fatal("want error from injected staged rename failure")
	}
	if errors.Is(err, ErrRollbackFailed) {
		t.Fatalf("err = %v, want plain failure (rollback succeeded)", err)
	}
	if got := readBin(t, self); got != oldSelfBytes {
		t.Fatalf("self content = %q, want old image restored by rollback", got)
	}
	if len(calls) != 3 {
		t.Fatalf("rename calls = %v, want 3 (backup, staged-fail, rollback)", calls)
	}
	if calls[1].from != staged || calls[1].to != self {
		t.Fatalf("second call = %v, want staged→self", calls[1])
	}
	// 回滚取最新代际:第三次的 from 必是本代 backup(第一次的 to)
	if calls[2].from != calls[0].to || calls[2].to != self {
		t.Fatalf("rollback call = %v, want %s→%s (newest generation)", calls[2], calls[0].to, self)
	}
	// 预置残留代际被起手清理,本代 backup 被回滚消费:代际清零
	if gens, _ := oldGenerations(self); len(gens) != 0 {
		t.Fatalf("generations after rollback = %v, want none", gens)
	}
	if got := readBin(t, staged); got != newSelfBytes {
		t.Fatalf("staged content = %q, want untouched", got)
	}
}

func TestReplaceWindowsDoubleFaultReturnsErrRollbackFailed(t *testing.T) {
	dir := t.TempDir()
	self := filepath.Join(dir, "sshmgr")
	staged := filepath.Join(dir, "staged-bin")
	writeBin(t, self, oldSelfBytes)
	writeBin(t, staged, newSelfBytes)

	origGOOS := currentGOOS
	currentGOOS = "windows"
	defer func() { currentGOOS = origGOOS }()

	var calls []renameCall
	origRename := osRename
	osRename = recordingRename(origRename, &calls, func(from, to string) bool {
		return to == self // staged→self 与 backup→self 都注入失败
	})
	defer func() { osRename = origRename }()

	err := ReplaceBinary(staged, self)
	if !errors.Is(err, ErrRollbackFailed) {
		t.Fatalf("err = %v (%T), want ErrRollbackFailed", err, err)
	}
	// Error() 含手工恢复命令 ren <backup> <self>
	backup := calls[0].to
	for _, want := range []string{"ren ", backup, self} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err.Error() = %q, want it to contain %q", err.Error(), want)
		}
	}
	// 可恢复现场:backup 仍在且持旧字节,self 缺失
	if got := readBin(t, backup); got != oldSelfBytes {
		t.Fatalf("backup content = %q, want old image", got)
	}
	mustNotExist(t, self)
}

func TestReplaceUnix(t *testing.T) {
	setup := func(t *testing.T) (dir, self, staged string) {
		t.Helper()
		dir = t.TempDir()
		self = filepath.Join(dir, "sshmgr")
		staged = filepath.Join(dir, "staged-bin")
		writeBin(t, self, oldSelfBytes)
		writeBin(t, staged, newSelfBytes)
		return dir, self, staged
	}

	t.Run("happy path: no generation left behind", func(t *testing.T) {
		_, self, staged := setup(t)
		origGOOS, origFS := currentGOOS, fileSync
		currentGOOS = "linux"
		fileSync = func(f *os.File) error { return nil } // 目录 fsync seam(Windows 主机真目录句柄必失败)
		defer func() { currentGOOS, fileSync = origGOOS, origFS }()

		if err := ReplaceBinary(staged, self); err != nil {
			t.Fatalf("ReplaceBinary: %v", err)
		}
		if got := readBin(t, self); got != newSelfBytes {
			t.Fatalf("self content = %q, want new image", got)
		}
		mustNotExist(t, staged)
		if gens, _ := oldGenerations(self); len(gens) != 0 {
			t.Fatalf("unix path must not create .old generations, got %v", gens)
		}
	})

	t.Run("dir fsync failure is committed-with-error", func(t *testing.T) {
		dir, self, staged := setup(t)
		origGOOS, origFS := currentGOOS, fileSync
		currentGOOS = "linux"
		fileSync = func(f *os.File) error {
			if f.Name() == dir {
				return errors.New("injected dir fsync failure")
			}
			return nil
		}
		defer func() { currentGOOS, fileSync = origGOOS, origFS }()

		err := ReplaceBinary(staged, self)
		var ce *CommittedWithError
		if !errors.As(err, &ce) {
			t.Fatalf("err = %v (%T), want *CommittedWithError", err, err)
		}
		if ce.Path != self {
			t.Fatalf("CommittedWithError.Path = %q, want %q", ce.Path, self)
		}
		if !strings.Contains(err.Error(), "committed") {
			t.Fatalf("err.Error() = %q, want committed-with-error wording", err.Error())
		}
		// 已提交:目标字节必须已是新镜像,不回滚不装死
		if got := readBin(t, self); got != newSelfBytes {
			t.Fatalf("self content = %q, want new image (committed, not rolled back)", got)
		}
	})

	t.Run("staged rename failure leaves zero change", func(t *testing.T) {
		_, self, staged := setup(t)
		origGOOS, origFS := currentGOOS, fileSync
		currentGOOS = "linux"
		fileSync = func(f *os.File) error { return nil }
		defer func() { currentGOOS, fileSync = origGOOS, origFS }()

		var calls []renameCall
		origRename := osRename
		osRename = recordingRename(origRename, &calls, func(from, to string) bool {
			return to == self
		})
		defer func() { osRename = origRename }()

		err := ReplaceBinary(staged, self)
		if err == nil {
			t.Fatal("want error from injected staged rename failure")
		}
		var ce *CommittedWithError
		if errors.As(err, &ce) {
			t.Fatalf("err = %v, must not be committed-with-error (nothing happened)", err)
		}
		if got := readBin(t, self); got != oldSelfBytes {
			t.Fatalf("self content = %q, want unchanged", got)
		}
		if got := readBin(t, staged); got != newSelfBytes {
			t.Fatalf("staged content = %q, want untouched", got)
		}
	})
}

func TestCleanOldBackups(t *testing.T) {
	t.Run("removes generations only", func(t *testing.T) {
		dir := t.TempDir()
		self := filepath.Join(dir, "sshmgr")
		staged := filepath.Join(dir, "staged-bin")
		writeBin(t, self, oldSelfBytes)
		writeBin(t, staged, newSelfBytes)
		writeBin(t, self+".old.111", "gen-111")
		writeBin(t, self+".old.222", "gen-222")
		unrelated := filepath.Join(dir, "sshmgr.exe.bak")
		writeBin(t, unrelated, "not a generation")

		if err := CleanOldBackups(self); err != nil {
			t.Fatalf("CleanOldBackups: %v", err)
		}
		mustNotExist(t, self+".old.111")
		mustNotExist(t, self+".old.222")
		mustExist(t, self)
		mustExist(t, staged)
		mustExist(t, unrelated)
	})

	t.Run("removal failure tolerated, always nil", func(t *testing.T) {
		dir := t.TempDir()
		self := filepath.Join(dir, "sshmgr")
		writeBin(t, self, oldSelfBytes)
		writeBin(t, self+".old.111", "gen-111")
		writeBin(t, self+".old.222", "gen-222")

		origRemove := osRemove
		osRemove = func(path string) error {
			if strings.HasSuffix(path, ".old.111") {
				return errors.New("injected remove failure (file held by old process)")
			}
			return origRemove(path)
		}
		defer func() { osRemove = origRemove }()

		if err := CleanOldBackups(self); err != nil {
			t.Fatalf("CleanOldBackups = %v, want nil (best-effort semantics)", err)
		}
		mustExist(t, self+".old.111")    // 清不掉的保留,不报错
		mustNotExist(t, self+".old.222") // 清得掉的照清
	})

	t.Run("no generations at all", func(t *testing.T) {
		dir := t.TempDir()
		self := filepath.Join(dir, "sshmgr")
		writeBin(t, self, oldSelfBytes)
		if err := CleanOldBackups(self); err != nil {
			t.Fatalf("CleanOldBackups = %v, want nil", err)
		}
	})
}

func TestDetectHeal(t *testing.T) {
	t.Run("self missing with generation backup", func(t *testing.T) {
		dir := t.TempDir()
		self := filepath.Join(dir, "sshmgr")
		writeBin(t, self+".old.1700000000", oldSelfBytes)
		seamExecutable(t, self)

		hint, ok := DetectHeal()
		if !ok {
			t.Fatal("want heal detection for missing self with backup present")
		}
		for _, want := range []string{self, self + ".old.1700000000"} {
			if !strings.Contains(hint, want) {
				t.Fatalf("hint %q missing %q", hint, want)
			}
		}
	})

	t.Run("running from backup, canonical missing", func(t *testing.T) {
		dir := t.TempDir()
		backup := filepath.Join(dir, "sshmgr.old.1700000001")
		writeBin(t, backup, oldSelfBytes)
		seamExecutable(t, backup)

		hint, ok := DetectHeal()
		if !ok {
			t.Fatal("want heal detection when executing from a generation backup")
		}
		canonical := filepath.Join(dir, "sshmgr")
		for _, want := range []string{backup, canonical} {
			if !strings.Contains(hint, want) {
				t.Fatalf("hint %q missing %q", hint, want)
			}
		}
	})

	t.Run("running from backup, canonical present: no heal", func(t *testing.T) {
		dir := t.TempDir()
		writeBin(t, filepath.Join(dir, "sshmgr"), newSelfBytes)
		backup := filepath.Join(dir, "sshmgr.old.1700000001")
		writeBin(t, backup, oldSelfBytes)
		seamExecutable(t, backup)

		if hint, ok := DetectHeal(); ok {
			t.Fatalf("DetectHeal = %q, want no heal when canonical exists", hint)
		}
	})

	t.Run("healthy install", func(t *testing.T) {
		dir := t.TempDir()
		self := filepath.Join(dir, "sshmgr")
		writeBin(t, self, newSelfBytes)
		seamExecutable(t, self)

		if hint, ok := DetectHeal(); ok || hint != "" {
			t.Fatalf("DetectHeal = (%q, true), want no heal", hint)
		}
	})

	t.Run("self missing without any backup", func(t *testing.T) {
		dir := t.TempDir()
		seamExecutable(t, filepath.Join(dir, "sshmgr"))
		if _, ok := DetectHeal(); ok {
			t.Fatal("want no heal when neither self nor backup exists")
		}
	})

	t.Run("executable seam error", func(t *testing.T) {
		orig := osExecutable
		osExecutable = func() (string, error) { return "", errors.New("boom") }
		defer func() { osExecutable = orig }()
		if _, ok := DetectHeal(); ok {
			t.Fatal("want no heal when executable path cannot be determined")
		}
	})
}

func TestSplitOldGeneration(t *testing.T) {
	cases := []struct {
		in   string
		stem string
		ts   int64
		ok   bool
	}{
		{"sshmgr.old.1700000000", "sshmgr", 1700000000, true},
		{"sshmgr.exe.old.123", "sshmgr.exe", 123, true},
		{"sshmgr", "", 0, false},
		{"sshmgr.old", "", 0, false},
		{"sshmgr.old.", "", 0, false},
		{"sshmgr.old.abc", "", 0, false},
		{"sshmgr.old.-5", "", 0, false},
		{"sshmgr.old.+5", "", 0, false},
		{".old.123", "", 0, false},
		{"sshmgr.old.99999999999999999999", "", 0, false}, // int64 溢出
		{"sshmgr2.old.123", "sshmgr2", 123, true},         // 通用解析;旧代际过滤靠 stem==base
	}
	for _, tc := range cases {
		stem, ts, ok := splitOldGeneration(tc.in)
		if ok != tc.ok || (ok && (stem != tc.stem || ts != tc.ts)) {
			t.Errorf("splitOldGeneration(%q) = (%q, %d, %v), want (%q, %d, %v)",
				tc.in, stem, ts, ok, tc.stem, tc.ts, tc.ok)
		}
	}
}

func TestStagedFSync(t *testing.T) {
	t.Run("existing file", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "staged-bin")
		writeBin(t, p, newSelfBytes)
		if err := StagedFSync(p); err != nil {
			t.Fatalf("StagedFSync: %v", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		err := StagedFSync(filepath.Join(t.TempDir(), "nope"))
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("err = %v, want missing-file failure naming the path", err)
		}
	})
}

// TestStagedVersionCheckHelperProcess 不是真测试——它是 StagedVersionCheck 的
// 子进程入口(execStaged seam 以 -test.run=TestStagedVersionCheckHelperProcess$
// 重跑本测试二进制)。SSHMGR_STAGED_HELPER 选择行为;无该 env 时跳过。
func TestStagedVersionCheckHelperProcess(t *testing.T) {
	switch os.Getenv("SSHMGR_STAGED_HELPER") {
	case "":
		t.Skip("helper process only")
	case "version":
		fmt.Println("v1.2.3")
	case "mismatch":
		fmt.Println("v9.9.9")
	case "flood":
		fmt.Println(strings.Repeat("x", 8<<10))
	case "hang":
		time.Sleep(60 * time.Second)
	}
	os.Exit(0)
}

func helperCmd(ctx context.Context, behavior string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestStagedVersionCheckHelperProcess$")
	cmd.Env = append(os.Environ(), "SSHMGR_STAGED_HELPER="+behavior)
	return cmd
}

func seamStagedHelper(t *testing.T, behavior string) {
	t.Helper()
	orig := execStaged
	execStaged = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return helperCmd(ctx, behavior)
	}
	t.Cleanup(func() { execStaged = orig })
}

func TestStagedVersionCheck(t *testing.T) {
	t.Run("normalized match (v prefix + newline)", func(t *testing.T) {
		seamStagedHelper(t, "version")
		got, err := StagedVersionCheck("staged-bin", "1.2.3")
		if err != nil || got != "1.2.3" {
			t.Fatalf("StagedVersionCheck = (%q, %v), want (1.2.3, nil)", got, err)
		}
		// want 带 v 前缀同样命中(双侧规范化)
		got, err = StagedVersionCheck("staged-bin", "v1.2.3")
		if err != nil || got != "1.2.3" {
			t.Fatalf("StagedVersionCheck(v-prefixed want) = (%q, %v), want (1.2.3, nil)", got, err)
		}
	})

	t.Run("mismatch rejected, got returned for evidence", func(t *testing.T) {
		seamStagedHelper(t, "mismatch")
		got, err := StagedVersionCheck("staged-bin", "1.2.3")
		if err == nil || !strings.Contains(err.Error(), "mismatch") {
			t.Fatalf("err = %v, want mismatch failure", err)
		}
		if got != "9.9.9" {
			t.Fatalf("got = %q, want staged-reported 9.9.9 for evidence", got)
		}
	})

	t.Run("output flood rejected at 4KiB limit", func(t *testing.T) {
		seamStagedHelper(t, "flood")
		if _, err := StagedVersionCheck("staged-bin", "1.2.3"); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("err = %v, want output-limit failure", err)
		}
	})

	t.Run("hang killed by timeout", func(t *testing.T) {
		origTimeout := stagedCheckTimeout
		stagedCheckTimeout = 300 * time.Millisecond
		defer func() { stagedCheckTimeout = origTimeout }()
		seamStagedHelper(t, "hang")

		start := time.Now()
		_, err := StagedVersionCheck("staged-bin", "1.2.3")
		if err == nil || !strings.Contains(err.Error(), "timed out") {
			t.Fatalf("err = %v, want timeout failure", err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("took %s, want killed near the shrunken deadline", d)
		}
	})

	t.Run("unstartable binary", func(t *testing.T) {
		orig := execStaged
		execStaged = func(ctx context.Context, name string, args ...string) *exec.Cmd {
			return exec.Command("sshmgr-no-such-binary-for-tests")
		}
		defer func() { execStaged = orig }()
		if _, err := StagedVersionCheck("staged-bin", "1.2.3"); err == nil || !strings.Contains(err.Error(), "cannot run") {
			t.Fatalf("err = %v, want cannot-run failure", err)
		}
	})
}
