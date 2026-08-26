package sshbroker

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"ssh-manager-mcp/internal/testsshd"
)

// TestRunSessionWritersReceiveDirect proves the runSession writer seam: the
// caller-supplied writers receive stdout/stderr directly (no cappedBuffer
// intermediary) with byte-pristine content, and the remote exit code propagates
// through the (exitCode, timedOut, err) triple. Task 4's background engine
// consumes this seam with its own writers.
func TestRunSessionWritersReceiveDirect(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password: "pw",
		Exec:     func(string, io.Reader) (string, string, int) { return "OUT", "ERR", 3 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	var mu sync.Mutex
	var out, errb strings.Builder
	code, timedOut, err := c.runSession(context.Background(), "x", 0, writerFunc(func(p []byte) { mu.Lock(); out.Write(p); mu.Unlock() }), writerFunc(func(p []byte) { mu.Lock(); errb.Write(p); mu.Unlock() }))
	if err != nil || timedOut || code != 3 || out.String() != "OUT" || errb.String() != "ERR" {
		t.Fatalf("code=%d timedOut=%v err=%v out=%q err=%q", code, timedOut, err, out.String(), errb.String())
	}
}

// TestRunSudoSessionWritersReceiveDirect is the same seam proof for the sudo
// kernel: the password dance still runs (testsshd consumes the pw line before
// executing the inner command) while output flows to external writers and the
// exit code propagates.
func TestRunSudoSessionWritersReceiveDirect(t *testing.T) {
	addr, hk, cleanup := testsshd.Start(t, testsshd.Options{
		Password:     "pw",
		SudoPassword: "sudopw",
		Exec:         func(string, io.Reader) (string, string, int) { return "OUT", "ERR", 3 },
	})
	defer cleanup()
	c := connectTest(t, addr, hk)

	var mu sync.Mutex
	var out, errb strings.Builder
	code, timedOut, err, _ := c.runSudoSession(context.Background(), "x", "sudopw", 0, writerFunc(func(p []byte) { mu.Lock(); out.Write(p); mu.Unlock() }), writerFunc(func(p []byte) { mu.Lock(); errb.Write(p); mu.Unlock() }))
	if err != nil || timedOut || code != 3 || out.String() != "OUT" || errb.String() != "ERR" {
		t.Fatalf("code=%d timedOut=%v err=%v out=%q err=%q", code, timedOut, err, out.String(), errb.String())
	}
}

type writerFunc func([]byte)

func (f writerFunc) Write(p []byte) (int, error) { f(p); return len(p), nil }
