//go:build windows

package store

import (
	"fmt"
	"syscall"
	"unsafe"
)

// dataBlob matches Windows CRYPTOAPI_BLOB / DATA_BLOB.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

const flagMachine = 0x1 // CRYPTPROTECT_LOCAL_MACHINE — NOT used (user-scope only)

// dpapiProtect encrypts plain with user-scope DPAPI (binds to current user SID).
// The caller must NOT pass CRYPTPROTECT_LOCAL_MACHINE (spec §3.2).
func dpapiProtect(plain []byte) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("dpapi: empty plain")
	}
	in := dataBlob{cbData: uint32(len(plain)), pbData: &plain[0]}
	var out dataBlob
	r, _, e := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		0, // flags=0 → user-scope
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptProtectData failed: %v", e)
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(out), nil
}

// dpapiUnprotect decrypts a user-scope DPAPI blob. Returns a non-nil error
// (NOT ErrNotFound) if decryption fails — callers must hard-fail, not
// fall through to a plaintext fallback (spec §5.6).
func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("dpapi: empty blob")
	}
	in := dataBlob{cbData: uint32(len(blob)), pbData: &blob[0]}
	var out dataBlob
	r, _, e := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		0,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptUnprotectData failed: %v", e)
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(out), nil
}

// localFree releases a DPAPI-allocated buffer (LocalAlloc). REQUIRED on every
// output blob or every call leaks memory/handles (spec §5.3, review codex#6/pi#5).
func localFree(p uintptr) {
	procLocalFree.Call(p)
}

// blobToBytes copies a DATA_BLOB's content into a Go slice. The caller has
// already deferred localFree on out.pbData.
func blobToBytes(out dataBlob) []byte {
	if out.cbData == 0 || out.pbData == nil {
		return nil
	}
	b := make([]byte, out.cbData)
	copy(b, (*[1 << 30]byte)(unsafe.Pointer(out.pbData))[:out.cbData])
	return b
}
