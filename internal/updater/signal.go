package updater

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// This file implements the update generation signal (bridge hot-upgrade
// spec, implementation decision 1): after `sshmgr update` replaces the
// binary, it writes `sshmgr.update-gen` next to the replaced binary. The
// file records THAT THE ON-DISK BINARY CHANGED — consumers (the running
// bridge / doctor) compare generations by the gen timestamp only, never by
// the version string, so a downgrade is newer the same way an upgrade is.
// One installed binary copy carries one signal file; multiple instances
// sharing a binary directory share the same signal.
//
// Failure philosophy: a missing or corrupt file is "no signal", never an
// error and never a crash — consumers stay on their current generation.

// GenerationSignalFileName is the fixed name of the generation signal file,
// written next to the sshmgr binary.
const GenerationSignalFileName = "sshmgr.update-gen"

// GenerationSignal is the decoded content of one signal file. JSON shape:
// gen = monotonically increasing write timestamp (unix nanoseconds),
// version = the version of the newly installed binary (informational only —
// never compared), time = the write moment.
type GenerationSignal struct {
	Gen     int64     `json:"gen"`
	Version string    `json:"version"`
	Time    time.Time `json:"time"`
}

// signalNow is the generation timestamp source. Package var solely so tests
// can pin it (clock-regression behavior); production code must never mutate
// it.
var signalNow = time.Now

// SignalPath returns the signal file path for a binary at exePath: the
// fixed file name inside the binary's directory.
func SignalPath(exePath string) string {
	return filepath.Join(filepath.Dir(exePath), GenerationSignalFileName)
}

// WriteGenerationSignal records a successful binary replacement: it writes
// the signal file next to the binary at exePath, carrying version as the
// newly installed version. The write is flushed to stable storage before
// returning ("写入前刷盘"), matching the staged-binary fsync contract.
//
// gen is time.Now().UnixNano() at write time, forced to strictly increase
// past the previously recorded generation: on a coarse clock or a clock
// regression the new gen becomes prev+1, so consumers comparing generations
// always see later installs as newer.
//
// The file is written in place (create/truncate + write + fsync). A crash
// mid-write leaves a corrupt file, which every reader treats as "no signal"
// — the safe direction, so no temp-file dance is spent on it.
func WriteGenerationSignal(exePath, version string) (GenerationSignal, error) {
	path := SignalPath(exePath)
	prev, _ := ReadGenerationSignal(exePath) // missing/corrupt previous = no floor
	now := signalNow()
	gen := now.UnixNano()
	if gen <= prev.Gen {
		gen = prev.Gen + 1
	}
	sig := GenerationSignal{Gen: gen, Version: version, Time: now}
	raw, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return GenerationSignal{}, fmt.Errorf("generation signal encode: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return GenerationSignal{}, fmt.Errorf("generation signal write %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		return GenerationSignal{}, fmt.Errorf("generation signal write %s: %w", path, err)
	}
	// The handle is opened with write access, so FlushFileBuffers accepts it
	// on Windows too (same contract as StagedFSync).
	if err := fileSync(f); err != nil {
		return GenerationSignal{}, fmt.Errorf("generation signal fsync %s: %w", path, err)
	}
	return sig, nil
}

// ReadGenerationSignal reads the signal file for a binary at exePath. A
// missing OR corrupt file (unparsable JSON, missing fields, non-positive
// gen, unparseable time) is reported as "no signal" (ok=false, zero value)
// — never an error, never a panic. All three fields must be present and
// well-formed for the signal to count.
func ReadGenerationSignal(exePath string) (GenerationSignal, bool) {
	raw, err := os.ReadFile(SignalPath(exePath))
	if err != nil {
		return GenerationSignal{}, false
	}
	var sig GenerationSignal
	if err := json.Unmarshal(raw, &sig); err != nil {
		return GenerationSignal{}, false
	}
	if sig.Gen <= 0 || sig.Version == "" || sig.Time.IsZero() {
		return GenerationSignal{}, false
	}
	return sig, true
}

// BirthGeneration snapshots the on-disk generation for a binary at exePath
// at the caller's birth (bridge startup records it, then compares later
// against the disk to notice new generations). A disk without a usable
// signal — none ever written, or corrupt — yields the zero generation (0),
// meaning "no generation change can be proven".
func BirthGeneration(exePath string) int64 {
	if sig, ok := ReadGenerationSignal(exePath); ok {
		return sig.Gen
	}
	return 0
}
