package pairing

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
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
	// 回退分支由 sasFromR 直测覆盖(TestSASFromR_RejectAllBlocks_TakesAgainBranch);
	// 此测试仅验常规路径形状(任意输入不 panic)
	_ = SAS(bytes.Repeat([]byte{0xFF}, 96), [32]byte{})
}

func TestSASFromR_RejectAllBlocks_TakesAgainBranch(t *testing.T) {
	// R 全 0xFF:8 块全为 0xFFFFFFFF ≥ 阈值 4,294,000,000,必然走 "again" 递推分支。
	var r [32]byte
	for i := range r {
		r[i] = 0xFF
	}
	got := sasFromR(r)
	if len(got) != 6 {
		t.Fatalf("len=%d", len(got))
	}
	for _, c := range got {
		if c < '0' || c > '9' {
			t.Fatalf("non-digit %q", got)
		}
	}
	// 交叉断言:测试内独立按递推公式算期望——
	// R1=SHA256(R‖"again") 后取首个 <4,294,000,000 的块模 10⁶ 零填充
	// (阈值用字面量,顺带钉死冻结常量,不依赖被测包的 sasRejectBelow)。
	sum := sha256.Sum256(append(append([]byte{}, r[:]...), []byte("again")...))
	want := ""
	for i := 0; i+4 <= len(sum); i += 4 {
		if v := binary.BigEndian.Uint32(sum[i : i+4]); v < 4_294_000_000 {
			want = fmt.Sprintf("%06d", v%1_000_000)
			break
		}
	}
	if want == "" {
		t.Fatal("cross-check premise failed: R1 also fully rejected")
	}
	if got != want {
		t.Fatalf("again branch mismatch: got %s want %s", got, want)
	}
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

// TestSignAndVerifyTranscript_SupportedKeys pins the pairing signature
// contract across the serve certificate key families: ed25519 signs the raw
// transcript (byte-for-byte identical to the previous protocol), ECDSA P-256
// signs the SHA-256 digest of the transcript, and RSA is accepted as a
// courtesy for operator-supplied certificates.
func TestSignAndVerifyTranscript_SupportedKeys(t *testing.T) {
	transcript := TranscriptParts([]byte("sshmgr-pair-v2"), bytes.Repeat([]byte{9}, 32), []byte("dev"))

	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		signer crypto.Signer
		public crypto.PublicKey
	}{
		{"ed25519", edPriv, edPub},
		{"ecdsa-p256", ecPriv, ecPriv.Public()},
		{"rsa-2048", rsaPriv, rsaPriv.Public()},
	} {
		sig, err := SignTranscript(tc.signer, transcript)
		if err != nil {
			t.Fatalf("%s sign: %v", tc.name, err)
		}
		if !VerifyTranscript(tc.public, transcript, sig) {
			t.Fatalf("%s signature does not verify", tc.name)
		}
		if VerifyTranscript(tc.public, append(append([]byte{}, transcript...), 'x'), sig) {
			t.Fatalf("%s signature verified against a different transcript", tc.name)
		}
		if VerifyTranscript(tc.public, transcript, append(append([]byte{}, sig...), 'x')) {
			t.Fatalf("%s corrupted signature verified", tc.name)
		}

		// Independent verification with a second implementation pins the
		// digest choice: ed25519 signs the raw transcript; ECDSA and RSA sign
		// SHA-256(transcript).
		switch key := tc.public.(type) {
		case ed25519.PublicKey:
			if !ed25519.Verify(key, transcript, sig) {
				t.Fatalf("%s independent raw verification failed", tc.name)
			}
			if len(sig) != ed25519.SignatureSize {
				t.Fatalf("%s signature length = %d, want %d", tc.name, len(sig), ed25519.SignatureSize)
			}
		case *ecdsa.PublicKey:
			digest := sha256.Sum256(transcript)
			if !ecdsa.VerifyASN1(key, digest[:], sig) {
				t.Fatalf("%s independent SHA-256 digest verification failed", tc.name)
			}
		case *rsa.PublicKey:
			digest := sha256.Sum256(transcript)
			if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig); err != nil {
				t.Fatalf("%s independent SHA-256 digest verification failed: %v", tc.name, err)
			}
		}
	}
}

// TestVerifyTranscript_RejectsECDSACurveOutsideContract ensures verification
// accepts only ECDSA P-256 keys, matching signing and certificate loading.
func TestVerifyTranscript_RejectsECDSACurveOutsideContract(t *testing.T) {
	transcript := []byte("sshmgr-pair-transcript")
	privateKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(transcript)
	signature, err := ecdsa.SignASN1(rand.Reader, privateKey, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if !ecdsa.VerifyASN1(&privateKey.PublicKey, digest[:], signature) {
		t.Fatal("test premise failed: P-384 signature must verify with crypto/ecdsa")
	}
	if VerifyTranscript(privateKey.Public(), transcript, signature) {
		t.Fatal("P-384 pairing signature must not verify")
	}
}

// TestSignTranscript_UnsupportedKeyRefusesExplicitly ensures an unsupported
// certificate key family yields a clear error instead of a bogus signature.
func TestSignTranscript_UnsupportedKeyRefusesExplicitly(t *testing.T) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, err = SignTranscript(privateKey, []byte("m"))
	if err == nil {
		t.Fatal("P-224 signing must not silently succeed")
	}
	if !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("error should say the key family is unsupported, got: %v", err)
	}
	if SupportedSignatureKey(privateKey.Public()) {
		t.Fatal("P-224 must not be reported as a supported pairing signature key")
	}
}
