package sshbroker

import "sync"

// RollingBuffer is an io.Writer that retains the LAST cap bytes of the stream
// (the rolling tail — the mirror of cappedBuffer's first-N prefix retention)
// while counting every byte in total. Unlike an append-and-drop-front slice,
// the backing array is allocated once at exactly cap bytes and never grows:
// the 64 MiB/project memory ceiling in the spec is a real allocation bound.
//
// Snapshot deep-copies under the buffer lock — it never returns a view of the
// internal array (an escaping view would be corrupted by later rolling writes,
// a corruption the race detector cannot see because it happens under the lock).
type RollingBuffer struct {
	mu    sync.Mutex
	buf   []byte // len ≤ cap(buf); capacity fixed at construction
	total int64
}

func NewRollingBuffer(cap int64) *RollingBuffer {
	if cap <= 0 {
		return &RollingBuffer{} // retains nothing, counts only (cap=0 boundary)
	}
	return &RollingBuffer{buf: make([]byte, 0, cap)}
}

func (b *RollingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	b.total += int64(n)
	c := int64(cap(b.buf))
	if c == 0 {
		return n, nil
	}
	if int64(n) > c { // 一次写超整窗: 只留尾 cap 字节
		p = p[int64(n)-c:]
	}
	if int64(len(b.buf)+len(p)) > c { // 丢头滚动(copy 挪动, 容量不变)
		drop := int64(len(b.buf)+len(p)) - c
		copy(b.buf, b.buf[drop:])
		b.buf = b.buf[:len(b.buf)-int(drop)]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

// Snapshot returns the bytes after stream offset `since`, per the spec's three
// pinned branches. start = the stream offset of the first retained byte.
//
//	since <  start (gap):    whole retained window, caller reports lost=start-since; next = total
//	since >= total (ahead):  empty chunk, cursor pulled back; next = total
//	otherwise (normal):      the window tail; next = since + len(chunk)
func (b *RollingBuffer) Snapshot(since int64) (chunk []byte, next, start int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	start = b.total - int64(len(b.buf))
	if start < 0 {
		start = 0
	}
	switch {
	case since >= b.total:
		return nil, b.total, start
	case since < start:
		out := make([]byte, len(b.buf))
		copy(out, b.buf)
		return out, b.total, start
	default:
		i := since - start
		out := make([]byte, int64(len(b.buf))-i)
		copy(out, b.buf[i:])
		return out, since + int64(len(out)), start
	}
}

func (b *RollingBuffer) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}
