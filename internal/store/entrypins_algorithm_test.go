package store

import (
	"errors"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestLearnEntryPinAlgorithmRequiresExactFingerprint 说明算法字段只是锚的附
// 加事实：写入前仍在事务里重做同一条指纹等值判定，不同指纹不能补记。
func TestLearnEntryPinAlgorithmRequiresExactFingerprint(t *testing.T) {
	st := newTestStore(t)
	id := addEntryForTest(t, st, "算法学习", "198.51.100.20", 22)
	srv := serverOf(t, st, id)
	key, fingerprint := pinHostKey(t)
	wrongKey, _ := pinHostKey(t)
	if _, err := st.UpsertManualEntryPin(
		srv, "", []byte(fingerprint), PinFormatFingerprint, false,
		func(*PinMeta) AuditRow { return AuditRow{Action: "pin-manual"} },
	); err != nil {
		t.Fatal(err)
	}

	if err := st.LearnEntryPinAlgorithm(srv, wrongKey); !errors.Is(err, ErrEntryPinMismatch) {
		t.Fatalf("不同指纹应返回锚不一致，得到：%v", err)
	}
	pin, err := st.GetEntryPinMeta(id)
	if err != nil {
		t.Fatal(err)
	}
	if pin.KeyAlgorithm != "" {
		t.Fatalf("不同指纹不能写算法，得到 %q", pin.KeyAlgorithm)
	}

	if err := st.LearnEntryPinAlgorithm(srv, key); err != nil {
		t.Fatal(err)
	}
	pin, err = st.GetEntryPinMeta(id)
	if err != nil {
		t.Fatal(err)
	}
	if pin.KeyAlgorithm != ssh.KeyAlgoED25519 {
		t.Fatalf("匹配指纹后应写回算法，得到 %q", pin.KeyAlgorithm)
	}
}
