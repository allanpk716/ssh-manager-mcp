package updater

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 代际信号文件(桥热升级规格实施决策第 1 条)的写读测试。写入函数以 exe
// 路径为参数,t.TempDir() 模拟二进制同目录,不触碰真实安装位置。

// fakeExePath returns a nonexistent "binary" path inside a fresh temp
// directory — the signal functions only derive the directory from it.
func fakeExePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "sshmgr.exe")
}

// pinSignalNow pins the generation timestamp seam for the duration of the
// test (restored automatically).
func pinSignalNow(t *testing.T, at time.Time) {
	t.Helper()
	prev := signalNow
	signalNow = func() time.Time { return at }
	t.Cleanup(func() { signalNow = prev })
}

func TestSignalPathDerivesBinaryDirectory(t *testing.T) {
	dir := t.TempDir()
	got := SignalPath(filepath.Join(dir, "sshmgr.exe"))
	if want := filepath.Join(dir, "sshmgr.update-gen"); got != want {
		t.Fatalf("SignalPath = %q, want %q", got, want)
	}
}

func TestWriteGenerationSignalCreatesValidFileWithThreeFields(t *testing.T) {
	exe := fakeExePath(t)
	sig, err := WriteGenerationSignal(exe, "0.20.0")
	if err != nil {
		t.Fatalf("WriteGenerationSignal: %v", err)
	}
	if sig.Gen <= 0 {
		t.Fatalf("returned gen = %d, want positive", sig.Gen)
	}
	if sig.Version != "0.20.0" {
		t.Fatalf("returned version = %q, want %q", sig.Version, "0.20.0")
	}
	if sig.Time.IsZero() {
		t.Fatal("returned time is zero, want the write moment")
	}

	raw, rerr := os.ReadFile(SignalPath(exe))
	if rerr != nil {
		t.Fatalf("signal file missing after write: %v", rerr)
	}
	if !json.Valid(raw) {
		t.Fatalf("signal file is not valid JSON: %q", raw)
	}
	var decoded GenerationSignal
	if jerr := json.Unmarshal(raw, &decoded); jerr != nil {
		t.Fatalf("signal file does not decode into GenerationSignal: %v (%q)", jerr, raw)
	}
	if decoded.Gen != sig.Gen || decoded.Version != sig.Version || !decoded.Time.Equal(sig.Time) {
		t.Fatalf("file fields (%+v) do not match returned signal (%+v)", decoded, sig)
	}

	// 读库往返:同一份文件经 ReadGenerationSignal 应原样读回。
	got, ok := ReadGenerationSignal(exe)
	if !ok {
		t.Fatal("ReadGenerationSignal reports no signal right after a write")
	}
	if got.Gen != sig.Gen || got.Version != sig.Version || !got.Time.Equal(sig.Time) {
		t.Fatalf("read back %+v, want %+v", got, sig)
	}
}

func TestWriteGenerationSignalGenStrictlyIncreasing(t *testing.T) {
	exe := fakeExePath(t)
	first, err := WriteGenerationSignal(exe, "0.20.0")
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	second, err := WriteGenerationSignal(exe, "0.20.1")
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if second.Gen <= first.Gen {
		t.Fatalf("gen not strictly increasing: first=%d second=%d", first.Gen, second.Gen)
	}
}

func TestWriteGenerationSignalGenBumpsPastExistingOnClockRegression(t *testing.T) {
	// 时钟回拨(或粗粒度时钟同刻)时,gen 仍必须严格大于既有值:
	// 先以未来时刻写入,再把时钟拨回过去写入,gen 只许 +1 地往前走。
	exe := fakeExePath(t)
	future := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	pinSignalNow(t, future)
	first, err := WriteGenerationSignal(exe, "0.20.0")
	if err != nil {
		t.Fatalf("write at future time: %v", err)
	}
	pinSignalNow(t, future.Add(-time.Hour))
	second, err := WriteGenerationSignal(exe, "0.19.0")
	if err != nil {
		t.Fatalf("write after clock regression: %v", err)
	}
	if second.Gen != first.Gen+1 {
		t.Fatalf("gen after regression = %d, want exactly prev+1 = %d", second.Gen, first.Gen+1)
	}
	// 降级写入(0.19.0 < 0.20.0)同样产生更晚的代际——比较只看 gen。
}

func TestReadGenerationSignalMissingFileReturnsNone(t *testing.T) {
	exe := fakeExePath(t)
	sig, ok := ReadGenerationSignal(exe)
	if ok {
		t.Fatalf("missing file reported a signal: %+v", sig)
	}
	if sig != (GenerationSignal{}) {
		t.Fatalf("missing file returned non-zero signal value: %+v", sig)
	}
}

func TestReadGenerationSignalCorruptReturnsNone(t *testing.T) {
	corrupt := []string{
		"",                     // empty file
		"not json at all",      // prose
		`{"gen": 123, "ver`,    // truncated
		`{}`,                   // valid JSON, all fields missing
		`{"version":"0.20.0"}`, // gen missing
		`{"gen":0,"version":"0.20.0","time":"2030-01-01T00:00:00Z"}`,     // gen zero
		`{"gen":-5,"version":"0.20.0","time":"2030-01-01T00:00:00Z"}`,    // gen negative
		`{"gen":"abc","version":"0.20.0","time":"2030-01-01T00:00:00Z"}`, // wrong type
		`{"gen":123,"version":"","time":"2030-01-01T00:00:00Z"}`,         // version missing
		`{"gen":123,"version":"0.20.0"}`,                                 // time missing
		`{"gen":123,"version":"0.20.0","time":"garbage"}`,                // unparseable time
	}
	for _, body := range corrupt {
		exe := fakeExePath(t)
		if werr := os.WriteFile(SignalPath(exe), []byte(body), 0o644); werr != nil {
			t.Fatalf("seeding corrupt file: %v", werr)
		}
		sig, ok := ReadGenerationSignal(exe)
		if ok {
			t.Fatalf("corrupt file %q reported a signal: %+v", body, sig)
		}
		if sig != (GenerationSignal{}) {
			t.Fatalf("corrupt file %q returned non-zero signal value: %+v", body, sig)
		}
	}
}

func TestBirthGenerationZeroWithoutSignal(t *testing.T) {
	exe := fakeExePath(t)
	if got := BirthGeneration(exe); got != 0 {
		t.Fatalf("BirthGeneration without signal = %d, want 0", got)
	}
	// 损坏文件同样视作无信号 → 零值代际。
	exe2 := fakeExePath(t)
	if werr := os.WriteFile(SignalPath(exe2), []byte("truncated{"), 0o644); werr != nil {
		t.Fatalf("seeding corrupt file: %v", werr)
	}
	if got := BirthGeneration(exe2); got != 0 {
		t.Fatalf("BirthGeneration with corrupt signal = %d, want 0", got)
	}
}

func TestBirthGenerationReturnsCurrentDiskGeneration(t *testing.T) {
	exe := fakeExePath(t)
	first, err := WriteGenerationSignal(exe, "0.20.0")
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	if got := BirthGeneration(exe); got != first.Gen {
		t.Fatalf("BirthGeneration = %d, want %d", got, first.Gen)
	}
	second, err := WriteGenerationSignal(exe, "0.20.1")
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if got := BirthGeneration(exe); got != second.Gen {
		t.Fatalf("BirthGeneration after second write = %d, want %d", got, second.Gen)
	}
}
