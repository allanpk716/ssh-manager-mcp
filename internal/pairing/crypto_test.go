package pairing

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestTranscript_LengthPrefixedLE(t *testing.T) {
	got := TranscriptParts([]byte("a"), []byte("bb"))
	want := []byte{1, 0, 0, 0, 'a', 2, 0, 0, 0, 'b', 'b'}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %x", got)
	}
}

func TestDeriveKeys_DeterministicVector(t *testing.T) {
	tr := TranscriptParts([]byte("sshmgr-pair-v2"), bytes.Repeat([]byte{1}, 32), []byte("laptop"),
		[]byte("https://10.0.0.5:7878"), bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 16),
		bytes.Repeat([]byte{4}, 32), bytes.Repeat([]byte{5}, 16))
	ikm := bytes.Repeat([]byte{0xAB}, 32)
	kAck, kCreds := DeriveKeys(ikm, tr)
	// 黄金向量(钉死推导,两侧实现不得漂移;变更即破坏协议兼容)
	const wantKAckHex = "31fe7dabf2b85e0e545f2d9afd2c65d6a8761edf1d2da82ae0d39e3b0d3f467b"
	const wantKCredsHex = "4c51785879183f9fa229a93ec1abd6396949a8b740ee4d6e56755a3f5c36a0d3"
	if got := hex.EncodeToString(kAck[:]); got != wantKAckHex {
		t.Fatalf("kAck drifted: got %s want %s", got, wantKAckHex)
	}
	if got := hex.EncodeToString(kCreds[:]); got != wantKCredsHex {
		t.Fatalf("kCreds drifted: got %s want %s", got, wantKCredsHex)
	}
	kAck2, kCreds2 := DeriveKeys(ikm, tr)
	if kAck != kAck2 || kCreds != kCreds2 {
		t.Fatal("nondeterministic")
	}
	if kAck == kCreds {
		t.Fatal("keys must differ")
	}
}

func TestSAS_RejectionSamplingAndFallback(t *testing.T) {
	// 常规:6 位零填充十进制
	s := SAS(make([]byte, 96), [32]byte{})
	if len(s) != 6 {
		t.Fatalf("len=%d", len(s))
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			t.Fatalf("non-digit %q", s)
		}
	}
	// 回退:构造前 8 块全 ≥4,294,000,000 的 R(全 0xFF),必须走到 "again" 递推且不 panic
	_ = SAS(bytes.Repeat([]byte{0xFF}, 96), [32]byte{})
}

func TestSealOpen_RoundTrip(t *testing.T) {
	sealed, err := SealCreds([32]byte{7}, []byte(`{"spki":"sha256:ab"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenCreds([32]byte{7}, sealed)
	if err != nil || string(got) != `{"spki":"sha256:ab"}` {
		t.Fatalf("%v %q", err, got)
	}
	if _, err := OpenCreds([32]byte{8}, sealed); err == nil {
		t.Fatal("wrong key must fail")
	}
}
