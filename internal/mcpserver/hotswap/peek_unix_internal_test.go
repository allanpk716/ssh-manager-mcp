//go:build unix

package hotswap

import (
	"os"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

// TestPeekReadableRetriesAfterInterruptedPoll 防回归:poll 被信号打断是
// 瞬时错误,窥视能力本身仍可用,不能退回无法停读的阻塞读。
func TestPeekReadableRetriesAfterInterruptedPoll(t *testing.T) {
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inR.Close()
	defer inW.Close()

	original := pollPipeReadableFunc
	var calls atomic.Int32
	injected := func(fds []unix.PollFd) (int, error) {
		callNumber := calls.Add(1)
		if callNumber == 1 {
			return 0, unix.EINTR
		}
		return 1, nil
	}
	setPollPipeReadable(injected)
	t.Cleanup(func() { setPollPipeReadable(original) })

	readable, ok := peekReadable(inR)
	if calls.Load() != 2 {
		t.Fatalf("poll calls = %d, want 2 (interrupted once then retry)", calls.Load())
	}
	if !ok || !readable {
		t.Fatalf("peekReadable after interrupted poll = (%v, %v), want (true, true)", readable, ok)
	}
}
