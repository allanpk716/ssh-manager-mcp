package sshbroker

import "bytes"

// cappedBuffer is an io.Writer that retains the first cap bytes written to it
// (the prefix) while counting the total bytes seen across all writes. cap == 0
// means unlimited — retain everything, never mark truncated.
//
// It lets Exec/ExecSudo bound memory for huge remote outputs (e.g. an agent
// running `cat huge.log`) while still reporting the output's true size, so the
// caller can tell the agent "you saw the first N bytes of M — refine your
// command if you need more". Bytes beyond cap are counted then discarded.
type cappedBuffer struct {
	buf       bytes.Buffer
	cap       int64 // 0 = unlimited
	total     int64
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	c.total += int64(n)
	if c.cap <= 0 {
		c.buf.Write(p)
		return n, nil
	}
	// Retain only up to cap bytes total (the prefix); discard the rest after counting.
	if int64(c.buf.Len()) < c.cap {
		room := c.cap - int64(c.buf.Len())
		take := int64(n)
		if take > room {
			take = room
		}
		c.buf.Write(p[:take])
	}
	if c.total > c.cap {
		c.truncated = true
	}
	return n, nil
}
