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

const flagMachine = 0x1 // CRYPTPROTECT_LOCAL_MACHINE — machine-scope (vs user-scope)

// dpapiProtect encrypts plain. machine=true → CRYPTPROTECT_LOCAL_MACHINE
// (binds to machine, not user/logon-session → any session can unprotect;
// Plan 15 spec §3.2). machine=false → user-scope (legacy v0.2.0/Plan-14 path).
// spike 2 实证:flag 不强制 scope 隔离,blob 自描述 scope(见 TestDpapi_CrossScopeInteroperable)。
func dpapiProtect(plain []byte, machine bool) ([]byte, error) {
	if len(plain) == 0 {
		return nil, fmt.Errorf("dpapi: empty plain")
	}
	in := dataBlob{cbData: uint32(len(plain)), pbData: &plain[0]}
	var out dataBlob
	flags := uintptr(0)
	if machine {
		flags = flagMachine // 0x1 = CRYPTPROTECT_LOCAL_MACHINE
	}
	r, _, e := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		flags,
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return nil, fmt.Errorf("dpapi: CryptProtectData failed: %v", e)
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return blobToBytes(out), nil
}

// dpapiUnprotect decrypts. machine=true → try machine-scope; the flag is a hint,
// not a hard gate (spike 2: blob self-describes scope). Callers that must handle
// legacy user-scope blobs try both (see DpapiKeyProvider.Get, T2).
func dpapiUnprotect(blob []byte, machine bool) ([]byte, error) {
	if len(blob) == 0 {
		return nil, fmt.Errorf("dpapi: empty blob")
	}
	in := dataBlob{cbData: uint32(len(blob)), pbData: &blob[0]}
	var out dataBlob
	flags := uintptr(0)
	if machine {
		flags = flagMachine
	}
	r, _, e := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		flags,
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
