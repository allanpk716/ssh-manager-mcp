package sshbroker

import "testing"

func TestCappedBufferRetainsPrefixAndCounts(t *testing.T) {
	c := &cappedBuffer{cap: 4}
	for _, s := range []string{"ab", "cde", "f"} {
		if _, err := c.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if c.buf.String() != "abcd" {
		t.Fatalf("retained=%q want abcd", c.buf.String())
	}
	if c.total != 6 {
		t.Fatalf("total=%d want 6", c.total)
	}
	if !c.truncated {
		t.Fatal("want truncated=true")
	}
}

func TestCappedBufferAtCapNotTruncated(t *testing.T) {
	c := &cappedBuffer{cap: 4}
	c.Write([]byte("abcd"))
	if c.buf.String() != "abcd" {
		t.Fatalf("retained=%q want abcd", c.buf.String())
	}
	if c.total != 4 {
		t.Fatalf("total=%d want 4", c.total)
	}
	if c.truncated {
		t.Fatal("exactly at cap must NOT be truncated")
	}
}

func TestCappedBufferUnlimited(t *testing.T) {
	c := &cappedBuffer{} // cap 0 = unlimited
	c.Write([]byte("abcdef"))
	if c.buf.String() != "abcdef" {
		t.Fatalf("retained=%q want abcdef", c.buf.String())
	}
	if c.total != 6 {
		t.Fatalf("total=%d want 6", c.total)
	}
	if c.truncated {
		t.Fatal("unlimited must never be truncated")
	}
}
