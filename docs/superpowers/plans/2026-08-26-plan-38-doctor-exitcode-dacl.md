# Plan 38 实施计划：doctor 硬化接线——exit 2 接线(backlog P2 #7)+ Windows DACL readback(backlog P2 #6)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 doctor 的 Windows 保护层校验从"只有 32 字节长度"补全为 DACL readback(白名单 read-capable + owner 信号),并把 `doctorExitCode` 的 exit 2 从"被测试钉着但零生产调用者"接进 main 的真实退出路径。

**Architecture:** store 包新增 `InspectFileACL` 读 API(`HardenACL` 的读侧双子,acl_windows 真实现 + acl_other stub);doctor 的 `checkVaultKey` 经包级 var seam 调用并按 `Supported` 分流(取代 `runtime.GOOS`);cli 包新增 `ExitCodeError`/`ExitCodeFor`,main.go 换用,删除 `doctorExitCode` 双真相源。

**Tech Stack:** Go + `golang.org/x/sys/windows`(advapi32 SD API,纯 Go 无 icacls 依赖);cobra;表驱动测试 + Windows build-tag 测试文件。

**Spec:** `docs/superpowers/specs/2026-08-26-plan-38-doctor-exitcode-dacl-design.md.rev3.md`(三轮 xcheck 收敛终版——本 plan 的判定语义/文案/测试矩阵全部以 rev3 为准,冲突时 rev3 赢)

## Global Constraints

- 文案逐字冻结:WARN Detail/Fix 的分段子句表见 spec rev3 §2.1,逐字符实现(spec §0 尾部「错误文案有冻结传统」)。
- 退出码范围:`NewExitCodeError`/`ExitCodeFor` 上下界 **[1,125]**(spec §3.1,防 os.Exit 低 8 位截断)。
- 危险位 = `mask & (FILE_READ_DATA | WRITE_DAC | WRITE_OWNER) != 0`(spec §1.2 信号 3;GENERIC 位已实证恒展开,不并入)。
- 白名单主体 = SYSTEM(S-1-5-18)/ BUILTIN\Administrators(S-1-5-32-544)/ 当前用户(进程令牌)(spec §1.2)。
- 只查过松不查过严;deny ACE、INHERIT_ONLY_ACE、白名单主体超额权限一律不告警(spec §1.2 不对称原则)。
- doctor 帮助文本本 plan **不动**(exit 2 行随 #5 上,spec §3.4);帮助文本里仍只写 0/1 是**正确状态**。
- 不动 `HardenACL` 写侧;不检测 master.key 之外的文件(spec §6)。
- 所有 Windows-only 测试进 `//go:build windows` 文件(runtime guard 不解决编译隔离,spec §4.2)。
- Go 无分离声明:acl.go 只放类型+方法,`InspectFileACL` 函数体在 acl_windows.go / acl_other.go 各一份(spec §1.1)。

---

### Task 1: `InspectFileACL` 读 API(store 包,含正名与测试矩阵)

**Files:**
- Create: `internal/store/acl.go`(跨平台类型 + 方法)
- Modify: `internal/store/acl_windows.go`(getDACLForTest 正名 readDACL + 新增 InspectFileACL)
- Modify: `internal/store/acl_other.go`(stub)
- Modify: `internal/store/acl_windows_test.go`(调用点正名 + 三个新测试)
- Create: `internal/store/acl_other_test.go`(stub 断言)
- Modify: `docs/superpowers/specs/2026-08-26-plan-38-doctor-exitcode-dacl-design.md`(§4.1① 探针结论回填)

**Interfaces:**
- Consumes: 现有 `getDACLForTest` / `isDaclProtected` / `trusteeInACL` / `buildExplicitAccess` / `currentUserSID`(acl_windows.go)。
- Produces: `type FileACLReport struct{ Supported, DaclNull, Protected bool; UnexpectedReadGrantors []string; OwnerSID string; OwnerUnexpected bool }`;`func (FileACLReport) TooLoose() bool`;`func InspectFileACL(path string) (FileACLReport, error)`——Task 3 的 doctor 接线消费这三个名字。

- [ ] **Step 1: 探针先行——钉死两个实现期未知数**

spec 留了两个「实现期核实」点,先写临时探针测试跑出结论再写正式代码。新建 `internal/store/acl_probe_test.go`:

```go
//go:build windows

package store

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestACLProbe_NullDACLAndDACLBit is a THROWAWAY probe (deleted in Step 2):
// ① can SetNamedSecurityInfo with a nil DACL plant a no-DACL file (spec §4.1①)?
// ② what does SECURITY_DESCRIPTOR.DACL()'s second return mean when the DACL
// is valid (spec fact #14 note: measured false-but-valid)?
func TestACLProbe_NullDACLAndDACLBit(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "probe.key")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// ① plant null DACL
	si := windows.DACL_SECURITY_INFORMATION
	if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, si, nil, nil, nil, nil); err != nil {
		t.Logf("PROBE1 null-DACL plant FAILED: %v", err)
	} else {
		sd, err := windows.GetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		dacl, second, derr := sd.DACL()
		ctrl, _, cerr := sd.Control()
		t.Logf("PROBE1 null-DACL plant OK: DACL()=(dacl=%v second=%v err=%v) Control()=(ctrl=0x%04x SE_DACL_PRESENT=%v err=%v)",
			dacl != nil, second, derr, ctrl, ctrl&windows.SE_DACL_PRESENT != 0, cerr)
	}
	// ② reference: a normal protected DACL — what does the second return say?
	if err := HardenACL(f); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	dacl, second, derr := sd.DACL()
	ctrl, _, cerr := sd.Control()
	t.Logf("PROBE2 hardened file: DACL()=(dacl=%v second=%v err=%v, AceCount=%d) Control()=(ctrl=0x%04x SE_DACL_PRESENT=%v err=%v)",
		dacl != nil, second, derr, daclCount(dacl), ctrl, ctrl&windows.SE_DACL_PRESENT != 0, cerr)
	_ = unsafe.Pointer(nil)
}

func daclCount(a *windows.ACL) uint16 {
	if a == nil {
		return 0
	}
	return a.AceCount
}
```

Run(Windows 本机): `go test ./internal/store/ -run TestACLProbe_NullDACLAndDACLBit -v`
Expected: PASS,日志给出两个结论。**判定规则**:
- PROBE1 plant OK 且读回 `SE_DACL_PRESENT=0`(或 dacl=nil)= null DACL **可种**→ 矩阵①走强制腿;plant FAILED = 不可种 → 矩阵①走降级腿(见 Step 6)。
- PROBE2 揭示 second 位的真实语义(预期:它是 defaulted 位而非 present 位,present 须从 `Control()&SE_DACL_PRESENT` 读)——`DaclNull` 的判定一律以 `SE_DACL_PRESENT` 位 + dacl 指针为准,**不依赖 DACL() 第二返回值**。

把两个结论回填 spec:`docs/superpowers/specs/2026-08-26-plan-38-doctor-exitcode-dacl-design.md` §4.1①(可行/不可行 + 判定口径一句)与 §7「DACL() 返回位语义」残差条目(改为已核实结论)。commit spec 回填。

- [ ] **Step 2: 删除探针,写跨平台类型(acl.go)**

删除 `internal/store/acl_probe_test.go`。创建 `internal/store/acl.go`:

```go
package store

// FileACLReport is the read-side verdict of InspectFileACL — the read-only
// twin of HardenACL (same LoadOrCreateServeCert/ReadServeCertFingerprint
// pairing precedent). The "who may read" semantics live in this package, next
// to the writer, so the whitelist cannot drift from what HardenACL grants.
type FileACLReport struct {
	// Supported is false on non-Windows platforms (file mode bits are the
	// protection layer there; the stub reports this).
	Supported bool
	// DaclNull: no DACL present — every principal is allowed (signal 1).
	DaclNull bool
	// Protected: SE_DACL_PROTECTED set (inheritance cut). ADVISORY ONLY —
	// it never triggers TooLoose by itself (live exposure via inherited ACEs
	// is already caught by the grantor walk; owner-approved downgrade).
	Protected bool
	// UnexpectedReadGrantors: SIDs of non-whitelisted principals holding an
	// allow ACE with a dangerous mask (signal 3), deduped and ascending.
	UnexpectedReadGrantors []string
	// OwnerSID: the file owner's SID (rendering only).
	OwnerSID string
	// OwnerUnexpected: owner outside the whitelist (signal 4, conservative
	// warning — OWNER_RIGHTS ACEs can limit the owner's implicit rights, so
	// this is "owner anomaly" advice, not an absolute privilege claim).
	OwnerUnexpected bool
}

// TooLoose reports whether the protection is looser than the hardened shape.
// A non-supported (stub) report can never be loose.
func (r FileACLReport) TooLoose() bool {
	return r.Supported && (r.DaclNull || r.OwnerUnexpected || len(r.UnexpectedReadGrantors) > 0)
}
```

- [ ] **Step 3: Windows 实现(acl_windows.go)——先正名,再新增**

3a. 正名:把 `getDACLForTest` 重命名为 `readDACL`(函数体不动,注释改为生产口吻——"readDACL reads the security descriptor for path and returns the DACL plus the SECURITY_DESCRIPTOR. Promoted from test helper for InspectFileACL (Plan 38); behavior unchanged."),`acl_windows_test.go` 里全部 `getDACLForTest(` 调用点改为 `readDACL(`(约 8 处,含 `getDACLForTestOrFatal` 内部)。删除原 "Test helpers" 分隔注释块头(131-135 行),`isDaclProtected`/`trusteeInACL` 上移的注释说明它们现在是生产函数。

3b. 新增 `InspectFileACL`(放在 readDACL 之后):

```go
// aclDangerousBits is the signal-3 dangerous mask (spec §1.2): read data, or
// the ability to rewrite the DACL / take ownership (self-elevation then read).
// Read-back masks always carry expanded specific rights (measured 2026-08-26:
// a stored GENERIC_ALL comes back as 0x00120089), so GENERIC bits never appear.
const aclDangerousBits = windows.FILE_READ_DATA | windows.WRITE_DAC | windows.WRITE_OWNER

// InspectFileACL reads the file's security descriptor back and reports whether
// its protection matches the hardened shape: whitelist = SYSTEM,
// BUILTIN\Administrators, and the current process user (exactly the three
// HardenACL grants). Only "looser than hardened" is reported — deny ACEs,
// INHERIT_ONLY ACEs, absent whitelist principals (over-tight) and whitelist
// principals holding extra rights are deliberately NOT flagged (spec §1.2
// asymmetry). Read-only: no SD is written.
func InspectFileACL(path string) (FileACLReport, error) {
	rep := FileACLReport{Supported: true}

	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return FileACLReport{}, fmt.Errorf("inspect ACL: %w", err)
	}

	whitelist, err := aclWhitelist()
	if err != nil {
		return FileACLReport{}, fmt.Errorf("inspect ACL: %w", err)
	}

	// Signal 4 — owner (conservative: OWNER_RIGHTS ACEs could limit the
	// owner's implicit WRITE_DAC, but we do not model that; anomaly advice).
	owner, _, err := sd.Owner()
	if err != nil {
		return FileACLReport{}, fmt.Errorf("inspect ACL: owner: %w", err)
	}
	rep.OwnerSID = owner.String()
	for _, w := range whitelist {
		if w.Equals(owner) {
			rep.OwnerUnexpected = false
			break
		}
		rep.OwnerUnexpected = true
	}

	// Signal 1 — DACL presence. The present bit is read from Control()
	// (SECURITY_DESCRIPTOR.DACL()'s second return measured ambiguous on
	// 2026-08-26; Control is authoritative).
	ctrl, _, err := sd.Control()
	if err != nil {
		return FileACLReport{}, fmt.Errorf("inspect ACL: control: %w", err)
	}
	if ctrl&windows.SE_DACL_PRESENT == 0 {
		rep.DaclNull = true
		return rep, nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return FileACLReport{}, fmt.Errorf("inspect ACL: dacl: %w", err)
	}
	if dacl == nil {
		rep.DaclNull = true
		return rep, nil
	}
	rep.Protected = ctrl&windows.SE_DACL_PROTECTED != 0

	// Signal 3 — grantor walk.
	seen := map[string]bool{}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			continue // unreadable ACE: skip, the structural signals above stand
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue // deny/audit ACEs are tightening, not exposure (spec fact #9)
		}
		if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
			continue // applies to children only, not to this file (spec §1.2)
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		whitelisted := false
		for _, w := range whitelist {
			if w.Equals(aceSID) {
				whitelisted = true
				break
			}
		}
		if whitelisted || ace.Mask&aclDangerousBits == 0 {
			continue
		}
		s := aceSID.String()
		if !seen[s] {
			seen[s] = true
			rep.UnexpectedReadGrantors = append(rep.UnexpectedReadGrantors, s)
		}
	}
	sort.Strings(rep.UnexpectedReadGrantors) // deterministic rendering
	return rep, nil
}

// aclWhitelist builds the trusted-principal set: the exact three HardenACL
// grants (well-known SIDs + the current process token user).
func aclWhitelist() ([]*windows.SID, error) {
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, fmt.Errorf("build SYSTEM sid: %w", err)
	}
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, fmt.Errorf("build Administrators sid: %w", err)
	}
	userSID, err := currentUserSID()
	if err != nil {
		return nil, fmt.Errorf("resolve current user sid: %w", err)
	}
	return []*windows.SID{systemSID, adminsSID, userSID}, nil
}
```

imports 增补:`sort`、`unsafe`(已有)。注意 owner 判定循环的写法:`for ... { if w.Equals(owner) { break } rep.OwnerUnexpected = true }` 在命中时 break 前未置 false、未命中时逐位置 true——语义正确但绕,直白写法:

```go
	rep.OwnerUnexpected = true
	for _, w := range whitelist {
		if w.Equals(owner) {
			rep.OwnerUnexpected = false
			break
		}
	}
```

3c. stub(acl_other.go 追加):

```go
// InspectFileACL reports Supported=false on non-Windows: mode bits are that
// platform's protection layer (see HardenACL's note). Callers branch on
// Supported instead of runtime.GOOS (spec §1.5).
func InspectFileACL(path string) (FileACLReport, error) {
	return FileACLReport{Supported: false}, nil
}
```

- [ ] **Step 4: 跑编译确认两平台符号齐**

Run: `go build ./... && go vet ./internal/store/`
Expected: 编译过(windows 本机)。跨平台 stub 正确性由 CI unix lane 兜底(acl_other.go 无新 import)。

- [ ] **Step 5: 写 store 侧测试(acl_windows_test.go 追加 + acl_other_test.go 新建)**

`internal/store/acl_windows_test.go` 追加(种法 helper + 三个测试):

```go
// seedLooseACE plants a single explicit allow ACE for sid with the given mask
// (PROTECTED — no inheritance), replacing the file's DACL. Test-only seeding,
// SetNamedSecurityInfo direct (TestOpen_DoesNotRewriteExistingStoreDBACL
// precedent).
func seedLooseACE(t *testing.T, path string, sid *windows.SID, mask uint32) {
	t.Helper()
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.ACCESS_MASK(mask),
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		t.Fatalf("build seed DACL: %v", err)
	}
	si := windows.SECURITY_INFORMATION(windows.PROTECTED_DACL_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, si, nil, nil, dacl, nil); err != nil {
		t.Fatalf("seed DACL: %v", err)
	}
}

// seedLooseOwner plants a non-whitelisted owner (Everyone).
func seedLooseOwner(t *testing.T, path string) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	si := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, si, everyone, nil, nil, nil); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
}

func mustEveryoneSID(t *testing.T) *windows.SID {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func TestInspectFileACL_Hardened(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "master.key.plain")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := HardenACL(f); err != nil {
		t.Fatal(err)
	}
	rep, err := InspectFileACL(f)
	if err != nil {
		t.Fatalf("InspectFileACL: %v", err)
	}
	if !rep.Supported || rep.TooLoose() {
		t.Fatalf("hardened file must not be loose: %+v", rep)
	}
	if !rep.Protected {
		t.Error("hardened file must report Protected")
	}
	if len(rep.UnexpectedReadGrantors) != 0 {
		t.Errorf("hardened file must have no unexpected grantors: %v", rep.UnexpectedReadGrantors)
	}
	if rep.OwnerUnexpected {
		t.Error("hardened file owner (creator) must be whitelisted")
	}
}

func TestInspectFileACL_TooLoose_Matrix(t *testing.T) {
	dir := t.TempDir()
	newFile := func() string {
		f := filepath.Join(dir, fmt.Sprintf("m%d.key", len(dirEntries(t, dir))))
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return f
	}
	everyone := mustEveryoneSID(t)
	users, err := windows.CreateWellKnownSid(windows.WinBuiltinUsersSid)
	if err != nil {
		t.Fatal(err)
	}

	// ① null DACL — per the Step-1 probe outcome, ONE of the two legs:
	//   (probe said plantable) assert DaclNull && TooLoose
	//   (probe said NOT plantable) assert the fallback: explicit Everyone-allow
	//   + UNPROTECTED covers signal 3 only — and DELETE the DaclNull assertion,
	//   leaving the §7 residual as registered.
	f := newFile()
	si := windows.DACL_SECURITY_INFORMATION
	if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, si, nil, nil, nil, nil); err != nil {
		t.Skipf("null-DACL not plantable via x/sys (probe branch): %v", err)
	}
	rep, err := InspectFileACL(f)
	if err != nil {
		t.Fatalf("①: %v", err)
	}
	if !rep.DaclNull || !rep.TooLoose() {
		t.Errorf("① null DACL must be DaclNull && TooLoose: %+v", rep)
	}

	// ② broad inheritable parent → child inherits the Everyone ACE.
	pdir := filepath.Join(dir, "broad-parent")
	if err := os.Mkdir(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	pdacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	psi := windows.SECURITY_INFORMATION(windows.UNPROTECTED_DACL_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(pdir, windows.SE_FILE_OBJECT, psi, nil, nil, pdacl, nil); err != nil {
		t.Fatalf("seed broad parent: %v", err)
	}
	child := filepath.Join(pdir, "child.key")
	if err := os.WriteFile(child, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = InspectFileACL(child)
	if err != nil {
		t.Fatalf("②: %v", err)
	}
	if rep.TooLoose() != true || !containsSID(rep.UnexpectedReadGrantors, everyone.String()) {
		t.Errorf("② inherited broad ACE must be caught: %+v", rep)
	}

	// ③ explicit Everyone-allow read ACE.
	f = newFile()
	seedLooseACE(t, f, everyone, uint32(windows.FILE_GENERIC_READ))
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("③: %v", err)
	}
	if !rep.TooLoose() || !containsSID(rep.UnexpectedReadGrantors, everyone.String()) {
		t.Errorf("③ explicit Everyone read must be caught: %+v", rep)
	}

	// ④ BUILTIN\Users read ACE (any non-whitelisted SID representative).
	f = newFile()
	seedLooseACE(t, f, users, uint32(windows.FILE_GENERIC_READ))
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("④: %v", err)
	}
	if !rep.TooLoose() || !containsSID(rep.UnexpectedReadGrantors, users.String()) {
		t.Errorf("④ BUILTIN\\Users read must be caught: %+v", rep)
	}

	// ⑤ write-only mask → NOT dangerous (mask filter pin).
	f = newFile()
	seedLooseACE(t, f, everyone, uint32(windows.FILE_WRITE_DATA))
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑤: %v", err)
	}
	if rep.TooLoose() {
		t.Errorf("⑤ write-only Everyone must NOT be flagged: %+v", rep)
	}

	// ⑥ deny ACE → NOT flagged (allow-only pin).
	f = newFile()
	denyDACL, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.DENY_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dsi := windows.SECURITY_INFORMATION(windows.PROTECTED_DACL_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, dsi, nil, nil, denyDACL, nil); err != nil {
		t.Fatalf("seed deny: %v", err)
	}
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑥: %v", err)
	}
	if rep.TooLoose() {
		t.Errorf("⑥ Everyone-deny must NOT be flagged: %+v", rep)
	}

	// ⑦ WRITE_DAC-only → caught (elevation-bit positive).
	f = newFile()
	seedLooseACE(t, f, everyone, uint32(windows.WRITE_DAC))
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑦: %v", err)
	}
	if !rep.TooLoose() {
		t.Errorf("⑦ WRITE_DAC-only must be caught: %+v", rep)
	}

	// ⑧ WRITE_OWNER-only → caught.
	f = newFile()
	seedLooseACE(t, f, everyone, uint32(windows.WRITE_OWNER))
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑧: %v", err)
	}
	if !rep.TooLoose() {
		t.Errorf("⑧ WRITE_OWNER-only must be caught: %+v", rep)
	}

	// ⑨ raw GENERIC_ALL stored → read back expanded (fact #15 regression pin).
	f = newFile()
	seedLooseACE(t, f, everyone, 0x80000000)
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑨: %v", err)
	}
	if !rep.ToooseOrExpanded() {
		t.Errorf("⑨ GENERIC_ALL must expand to dangerous specific rights: %+v", rep)
	}

	// ⑩ INHERIT_ONLY allow ACE → NOT flagged (no effect on the file itself).
	f = newFile()
	ioDACL, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.INHERIT_ONLY,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, dsi, nil, nil, ioDACL, nil); err != nil {
		t.Fatalf("seed inherit-only: %v", err)
	}
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatalf("⑩: %v", err)
	}
	if rep.TooLoose() {
		t.Errorf("⑩ INHERIT_ONLY ACE must NOT be flagged: %+v", rep)
	}
}

func TestInspectFileACL_Owner(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "owner.key")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := HardenACL(f); err != nil {
		t.Fatal(err)
	}
	seedLooseOwner(t, f)
	rep, err := InspectFileACL(f)
	if err != nil {
		t.Fatalf("InspectFileACL: %v", err)
	}
	if !rep.OwnerUnexpected || !rep.TooLoose() {
		t.Fatalf("non-whitelisted owner must trigger: %+v", rep)
	}
	if rep.OwnerSID != mustEveryoneSID(t).String() {
		t.Errorf("OwnerSID must render the planted SID: %s", rep.OwnerSID)
	}
	// restore owner to Administrators → not triggered
	admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		t.Fatal(err)
	}
	si := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(f, windows.SE_FILE_OBJECT, si, admins, nil, nil, nil); err != nil {
		t.Fatalf("restore owner: %v", err)
	}
	rep, err = InspectFileACL(f)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OwnerUnexpected {
		t.Errorf("Administrators owner must be whitelisted: %+v", rep)
	}
}

func TestInspectFileACL_MissingPath(t *testing.T) {
	if _, err := InspectFileACL(filepath.Join(t.TempDir(), "nope.key")); err == nil {
		t.Fatal("missing path must return an error")
	}
}

func containsSID(sids []string, sid string) bool {
	for _, s := range sids {
		if s == sid {
			return true
		}
	}
	return false
}

func dirEntries(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ents
}
```

**修正注**:⑨ 里的 `rep.ToooseOrExpanded()` 是笔误占位——直接用 `!rep.TooLoose()` 取反映为 `if rep.TooLoose() == false { t.Errorf(...) }`,即 GENERIC_ALL 种入后必须被抓(展开后含 FILE_READ_DATA)。写测试时用:

```go
	if !rep.TooLoose() {
		t.Errorf("⑨ GENERIC_ALL must expand to dangerous specific rights and be caught: %+v", rep)
	}
```

`internal/store/acl_other_test.go` 新建:

```go
//go:build !windows

package store

import "testing"

func TestInspectFileACL_StubUnsupported(t *testing.T) {
	rep, err := InspectFileACL("/dev/null/whatever")
	if err != nil {
		t.Fatalf("stub must not error: %v", err)
	}
	if rep.Supported {
		t.Fatal("stub must report Supported=false")
	}
	if rep.TooLoose() {
		t.Fatal("non-supported report must never be loose (Supported guard pin)")
	}
}
```

- [ ] **Step 6: 跑 store 测试**

Run: `go test ./internal/store/ -run 'TestInspectFileACL' -v`
Expected: 全 PASS(① 腿若 Skip,确认 spec 回填的降级结论与 Skip 理由一致)。

Run: `go test ./internal/store/ -v`(全量)
Expected: 既有 ACL 测试(getDACLForTest 正名后)全绿。

- [ ] **Step 7: Commit**

```bash
git add internal/store/acl.go internal/store/acl_windows.go internal/store/acl_other.go internal/store/acl_windows_test.go internal/store/acl_other_test.go docs/superpowers/specs/2026-08-26-plan-38-doctor-exitcode-dacl-design.md
git commit -m "feat(store): InspectFileACL 读侧 API——白名单 read-capable+owner 信号(test-helper 正名+⑩腿矩阵),Plan 38 T1"
```

---

### Task 2: ExitCodeError 接线(cli 包 + main,删 doctorExitCode 双真相源)

**Files:**
- Create: `internal/cli/exit.go`
- Create: `internal/cli/exit_test.go`
- Modify: `internal/cli/doctor.go`(删 doctorExitCode;runDoctor 尾部换 NewExitCodeError)
- Modify: `internal/cli/doctor_test.go`(TestDoctorExitCodes state 3 重写)
- Modify: `cmd/ssh-manager/main.go`

**Interfaces:**
- Produces: `type ExitCodeError struct{ Code int; Err error }`;`func NewExitCodeError(code int, err error) *ExitCodeError`(违约 panic);`func ExitCodeFor(err error) int`——Task 3 不消费但 main.go 与 #5 二期消费;`errDoctorFindings` 不变(doctor.go 既有)。

- [ ] **Step 1: 写失败测试(exit_test.go)**

```go
package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
)

func TestExitCodeFor(t *testing.T) {
	if got := ExitCodeFor(nil); got != 0 {
		t.Errorf("nil → 0, got %d", got)
	}
	if got := ExitCodeFor(errors.New("boom")); got != 1 {
		t.Errorf("plain error → 1, got %d", got)
	}
	if got := ExitCodeFor(NewExitCodeError(2, errors.New("x"))); got != 2 {
		t.Errorf("pinned code 2 → 2, got %d", got)
	}
	// Hand-rolled literals bypassing the constructor must degrade to 1 —
	// never "error but exit 0", never OS-truncated garbage.
	if got := ExitCodeFor(&ExitCodeError{Code: 0, Err: errors.New("x")}); got != 1 {
		t.Errorf("literal code 0 → 1, got %d", got)
	}
	if got := ExitCodeFor(&ExitCodeError{Code: 999, Err: errors.New("x")}); got != 1 {
		t.Errorf("literal code 999 → 1, got %d", got)
	}
}

func TestNewExitCodeErrorInvariants(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		err  error
	}{
		{"code 0", 0, errors.New("x")},
		{"code 999", 999, errors.New("x")},
		{"nil err", 1, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("%s must panic", tc.name)
				}
			}()
			NewExitCodeError(tc.code, tc.err)
		})
	}
}

func TestExitCodeErrorNilErrRendering(t *testing.T) {
	e := &ExitCodeError{Code: 1} // hand-rolled, Err nil — must not panic at print time
	if e.Error() == "" {
		t.Error("nil-Err literal must still render a non-empty message")
	}
}

// TestExitCodeForCrossesCobra proves the code survives a cobra Execute round
// trip — any command can pin its exit code.
func TestExitCodeForCrossesCobra(t *testing.T) {
	root := &cobra.Command{Use: "root"}
	root.AddCommand(&cobra.Command{
		Use: "boom",
		RunE: func(cmd *cobra.Command, args []string) error {
			return NewExitCodeError(2, fmt.Errorf("internal"))
		},
	})
	root.SetArgs([]string{"boom"})
	err := root.Execute()
	if err == nil {
		t.Fatal("RunE error must surface")
	}
	if got := ExitCodeFor(err); got != 2 {
		t.Fatalf("code 2 must cross cobra, got %d", got)
	}
}
```

Run: `go test ./internal/cli/ -run 'TestExitCodeFor|TestNewExitCodeErrorInvariants|TestExitCodeErrorNilErrRendering' -v`
Expected: FAIL(ExitCodeFor/NewExitCodeError undefined)。

- [ ] **Step 2: 实现 exit.go**

```go
package cli

import (
	"errors"
	"fmt"
)

// ExitCodeError lets a RunE pin the process exit code that main will honor;
// every other error keeps the generic 1. The stable convention (scripts rely
// on it): 0 = success, 1 = command error / doctor FAIL findings, 2 = doctor
// internal error (first real producer: #5 serve liveness probe).
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string {
	if e.Err == nil {
		// hand-rolled literal that bypassed NewExitCodeError — never
		// nil-deref at print time; the code alone is still meaningful.
		return fmt.Sprintf("ssh-manager: exit code %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitCodeError) Unwrap() error { return e.Err }

// NewExitCodeError is the sanctioned constructor: code in [1,125] and err != nil
// are internal invariants — violations panic loudly (pinned by test) instead of
// silently producing a zero-code success or a nil-deref at print time. (125:
// exit codes are truncated to the low 8 bits by the OS; >125 risks colliding
// with shell-reserved codes like 126/127.)
func NewExitCodeError(code int, err error) *ExitCodeError {
	if code < 1 || code > 125 || err == nil {
		panic(fmt.Sprintf("NewExitCodeError: invalid code=%d err=%v", code, err))
	}
	return &ExitCodeError{Code: code, Err: err}
}

// ExitCodeFor maps a root-command error to the process exit code: an
// ExitCodeError pins its code, anything else is 1. A hand-rolled literal that
// bypassed NewExitCodeError with a nonsense code (<1 or >125) falls back to 1.
func ExitCodeFor(err error) int {
	if err == nil {
		return 0
	}
	var ec *ExitCodeError
	if errors.As(err, &ec) {
		if ec.Code < 1 || ec.Code > 125 {
			return 1
		}
		return ec.Code
	}
	return 1
}
```

Run: `go test ./internal/cli/ -run 'TestExitCodeFor|TestNewExitCodeErrorInvariants|TestExitCodeErrorNilErrRendering|TestExitCodeForCrossesCobra' -v`
Expected: PASS。

- [ ] **Step 3: 删 doctorExitCode,runDoctor 换带码错误**

`internal/cli/doctor.go`:删除 `doctorExitCode` 函数与 `errDoctorFindings` 上方关于三态映射的旧注释中"2 = doctor internal error"句(doctorExitCode 的 doc comment 整块删,55-67 行);`errDoctorFindings` 变量与注释保留。`runDoctor` 尾部:

```go
	if fail > 0 {
		return NewExitCodeError(1, fmt.Errorf("%w (%d) — see the report above", errDoctorFindings, fail))
	}
	return nil
```

`internal/cli/doctor_test.go` 的 `TestDoctorExitCodes` state 3 段(110-121 行)替换为:

```go
	// State 3 — the wiring: findings (wrapped included) keep errors.Is AND
	// pin exit 1 via ExitCodeFor; a plain error maps to the generic 1.
	out, err = driveDoctor(t)
	_ = out
	if !errors.Is(err, errDoctorFindings) {
		t.Fatalf("corrupt role leg must still return findings, got: %v", err)
	}
	if got := ExitCodeFor(err); got != 1 {
		t.Fatalf("findings must map to exit 1, got %d", got)
	}
	if got := ExitCodeFor(errors.New("boom")); got != 1 {
		t.Fatalf("plain error must map to generic 1, got %d", got)
	}
```

(注意 state 3 紧跟 state 2 的 driveDoctor——直接复用其 err,去掉重复 drive:实际写时把 `out, err = driveDoctor(t); _ = out` 两行删掉,直接用 state 2 已有的 `err`。)

Run: `go test ./internal/cli/ -run 'TestDoctorExitCodes' -v`
Expected: PASS。

- [ ] **Step 4: main.go 换 ExitCodeFor**

`cmd/ssh-manager/main.go`:

```go
package main

import (
	"fmt"
	"os"

	"ssh-manager-mcp/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(cli.ExitCodeFor(err))
	}
}
```

Run: `go build ./...`
Expected: 编译过。

- [ ] **Step 5: 全量 cli 测试**

Run: `go test ./internal/cli/ -v 2>&1 | tail -20`
Expected: 全 PASS(无 doctorExitCode 残留引用——`grep -rn doctorExitCode internal/` 应为空)。

- [ ] **Step 6: Commit**

```bash
git add internal/cli/exit.go internal/cli/exit_test.go internal/cli/doctor.go internal/cli/doctor_test.go cmd/ssh-manager/main.go
git commit -m "feat(cli): ExitCodeError 接线——main 走 ExitCodeFor,删 doctorExitCode 双真相源(exit 2 管道通,产源留 #5),Plan 38 T2"
```

---

### Task 3: doctor 接线(checkVaultKey Windows 分支 + 测试 + 文档销项)

**Files:**
- Modify: `internal/cli/doctor.go`(checkVaultKey default 分支重构 + seam + 分段渲染 helper + import 清理)
- Modify: `internal/cli/doctor_test.go`(seedDoctorVault 改 Set + seam err→FAIL 腿)
- Create: `internal/cli/doctor_windows_test.go`(WARN 腿)
- Modify: `docs/backlog.md`(#6/#7 销项)
- Modify: `docs/compat-matrix.md`(v0.10 系占位注释追加)

**Interfaces:**
- Consumes: `store.InspectFileACL` / `store.FileACLReport`(Task 1);`ExitCodeFor`(Task 2,本任务不动它)。
- Produces: `var inspectFileACL = store.InspectFileACL`(seam,doctor_windows_test.go / doctor_test.go 消费);`aclLooseDetail` / `aclLooseFix`(私有 helper)。

- [ ] **Step 1: 写失败的 seam 测试(doctor_test.go 追加,跨平台)**

```go
// stubInspectFileACL replaces the store seam (serveServiceState precedent):
// drives the err→FAIL branch, which cannot be seeded for real (a hardened
// user mask carries READ_CONTROL, so SD reads succeed).
func stubInspectFileACL(t *testing.T, rep store.FileACLReport, err error) {
	t.Helper()
	prev := inspectFileACL
	inspectFileACL = func(p string) (store.FileACLReport, error) { return rep, err }
	t.Cleanup(func() { inspectFileACL = prev })
}

// TestDoctorVaultKeyACLBranches drives checkVaultKey's Windows-side branches
// through the seam (cross-platform — the seam, not the OS, decides).
func TestDoctorVaultKeyACLBranches(t *testing.T) {
	stubServeServiceState(t, "Running")
	vd, _ := withDoctorDirs(t)
	seedDoctorVault(t, vd)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	// err → FAIL
	stubInspectFileACL(t, store.FileACLReport{}, errors.New("sd read denied"))
	out, err := driveDoctor(t)
	if !errors.Is(err, errDoctorFindings) {
		t.Fatalf("ACL unreadable must FAIL, got: %v\n%s", err, out)
	}
	for _, want := range []string{
		"masterkey:  FAIL",
		"master.key ACL unreadable",
		"overall: 0 WARN, 1 FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// Supported=false → the Unix mode-bit path (file is 0600 → stays PASS).
	stubInspectFileACL(t, store.FileACLReport{Supported: false}, nil)
	out, err = driveDoctor(t)
	if err != nil {
		t.Fatalf("unsupported stub must not FAIL on a 0600 key: %v\n%s", err, out)
	}
	if !strings.Contains(out, "masterkey:  PASS") {
		t.Fatalf("0600 key under stub must PASS:\n%s", out)
	}

	// TooLoose → WARN with the frozen §2.1 signal-3 clause.
	stubInspectFileACL(t, store.FileACLReport{
		Supported:              true,
		Protected:              true,
		UnexpectedReadGrantors: []string{"S-1-1-0"},
	}, nil)
	out, err = driveDoctor(t)
	if err != nil {
		t.Fatalf("a loose-ACL WARN must not change the exit code: %v\n%s", err, out)
	}
	for _, want := range []string{
		"masterkey:  WARN",
		"grants access to unexpected principals: S-1-1-0",
		"— the plaintext key is protected by this ACL alone",
		"/inheritance:r /remove:g",
		"*S-1-5-18:(F)",
		"overall: 1 WARN, 0 FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// DaclNull-only → the §2.1 signal-1 clause (no empty-SIDs rendering).
	stubInspectFileACL(t, store.FileACLReport{Supported: true, DaclNull: true}, nil)
	out, err = driveDoctor(t)
	if err != nil {
		t.Fatalf("WARN must not FAIL: %v\n%s", err, out)
	}
	if !strings.Contains(out, "it has no DACL — every principal is allowed") {
		t.Fatalf("signal-1 clause must render:\n%s", out)
	}
	if strings.Contains(out, "unexpected principals: ") {
		t.Fatalf("signal-1 must not render an empty principals list:\n%s", out)
	}

	// Owner-only → the §2.1 signal-4 clause + /setowner fix.
	stubInspectFileACL(t, store.FileACLReport{
		Supported:       true,
		Protected:       true,
		OwnerSID:        "S-1-1-0",
		OwnerUnexpected: true,
	}, nil)
	out, err = driveDoctor(t)
	if err != nil {
		t.Fatalf("WARN must not FAIL: %v\n%s", err, out)
	}
	for _, want := range []string{
		"the file owner is S-1-1-0 — the owner can typically rewrite the DACL",
		"/setowner *S-1-5-32-544",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}

	// Loose + inheritance alive → the advisory parenthetical.
	stubInspectFileACL(t, store.FileACLReport{
		Supported:              true,
		UnexpectedReadGrantors: []string{"S-1-1-0"},
	}, nil)
	out, err = driveDoctor(t)
	if err != nil {
		t.Fatalf("WARN must not FAIL: %v\n%s", err, out)
	}
	if !strings.Contains(out, "(inheritance also enabled)") {
		t.Fatalf("advisory parenthetical must render when !Protected:\n%s", out)
	}
}
```

(seedDoctorVault 的改造在 Step 3——本测试先用现状 seed 也行:本测试走 seam,真 seed 的 ACL 不影响。但 `overall: 0 WARN, 1 FAIL` / `overall: 1 WARN, 0 FAIL` 计数要求其他行安静,Step 3 的 Set-seed 改造是 Windows lane 现存断言不破的前提,与本测试互补。)

Run: `go test ./internal/cli/ -run TestDoctorVaultKeyACLBranches -v`
Expected: FAIL(inspectFileACL undefined / 现行为不产出这些行)。

- [ ] **Step 2: 实现 doctor.go 接线**

2a. seam + import(doctor.go 顶部已有 store import):

```go
// inspectFileACL is the seam over store.InspectFileACL (serveServiceState
// precedent): tests stub it to drive the error branch, which cannot be seeded
// for real — a hardened user mask carries READ_CONTROL, so SD reads succeed.
var inspectFileACL = store.InspectFileACL
```

2b. `checkVaultKey` 的 `default:` 分支(现 289-299 行)替换为:

```go
	default:
		c.Status = statusPass
		c.Detail = fmt.Sprintf("master.key present (%d bytes)", len(b))
		rep, aerr := inspectFileACL(p)
		switch {
		case aerr != nil:
			// Deep anomaly: hardened users hold READ_CONTROL, so an SD read
			// failure means the ACL was rewritten past legibility.
			c.Status = statusFail
			c.Detail = fmt.Sprintf("master.key ACL unreadable: %v", aerr)
			c.Fix = "inspect the file's security descriptor as admin (icacls <master.key>); restore the key from backup if the SD is corrupt"
		case !rep.Supported:
			// Non-Windows: mode bits are the layer — existing check, moved in.
			if info, serr := os.Stat(p); serr == nil && info.Mode().Perm()&0o077 != 0 {
				c.Status = statusWarn
				c.Detail = fmt.Sprintf("master.key present (%d bytes) but group/world readable (mode %o) — the plaintext key is protected by mode bits alone", len(b), info.Mode().Perm())
				c.Fix = "chmod 600 the master.key file (and 0700 its parent directory)"
			}
		case rep.TooLoose():
			c.Status = statusWarn
			c.Detail = aclLooseDetail(len(b), rep)
			c.Fix = aclLooseFix(rep)
		}
	}
```

并从 `checkVaultKey` 及其注释中移除 `runtime` 语义(§247 注释里 "on Windows protection is ACLs ... skipped" 段改写为分流说明;若 doctor.go 无其他 `runtime` 引用则删 import——先 `grep -n runtime doctor.go` 确认)。

2c. 分段渲染 helper(doctor.go 尾部,`newDoctorCmd` 之前):

```go
// aclLooseDetail renders the WARN Detail per the frozen clause table (spec
// rev3 §2.1): one clause per triggered signal, semicolon-joined, common tail,
// advisory parenthetical when inheritance is live.
func aclLooseDetail(keyBytes int, rep store.FileACLReport) string {
	var parts []string
	if rep.DaclNull {
		parts = append(parts, fmt.Sprintf(
			"master.key present (%d bytes) but it has no DACL — every principal is allowed", keyBytes))
	}
	if len(rep.UnexpectedReadGrantors) > 0 {
		parts = append(parts, fmt.Sprintf(
			"master.key present (%d bytes) but its DACL grants access to unexpected principals: %s",
			keyBytes, strings.Join(rep.UnexpectedReadGrantors, ", ")))
	}
	if rep.OwnerUnexpected {
		parts = append(parts, fmt.Sprintf(
			"master.key present (%d bytes) but the file owner is %s — the owner can typically rewrite the DACL",
			keyBytes, rep.OwnerSID))
	}
	detail := strings.Join(parts, "; ") + " — the plaintext key is protected by this ACL alone"
	if !rep.Protected {
		detail += " (inheritance also enabled)"
	}
	return detail
}

// aclLooseFix renders the WARN Fix per the frozen clause table (spec rev3
// §2.1): icacls segments joined in owner→inheritance/grants order, all
// asterisk-SID form (account names localize on non-English Windows).
func aclLooseFix(rep store.FileACLReport) string {
	var segs []string
	if rep.OwnerUnexpected {
		segs = append(segs, "/setowner *S-1-5-32-544")
	}
	if len(rep.UnexpectedReadGrantors) > 0 {
		segs = append(segs, "/inheritance:r", "/remove:g <SIDs...>")
	}
	if rep.DaclNull || len(rep.UnexpectedReadGrantors) > 0 {
		segs = append(segs, "/grant:r *S-1-5-18:(F) *S-1-5-32-544:(F) *<you-SID>:(RC,R,W,D)")
	}
	return "icacls <master.key> " + strings.Join(segs, " ") +
		" — replace <SIDs...> with the principals listed above (asterisk-prefixed SID form, e.g. *S-1-1-0) and <you-SID> with your own SID (`whoami /user`)"
}
```

(`strings` 已在 doctor.go import。)

Run: `go test ./internal/cli/ -run TestDoctorVaultKeyACLBranches -v`
Expected: PASS。

- [ ] **Step 3: seedDoctorVault 改走 FileKeyProvider.Set**

`doctor_test.go` 的 `seedDoctorVault`(223-237 行)替换为:

```go
// seedDoctorVault builds a REAL vault in the test's temp dir — seedClearVault
// precedent (clear_test.go): store.Open to create store.db (side effects are
// legal in TESTS; doctor itself only Stats/ReadFiles) + the 32-byte
// master.key.plain next to it, written via FileKeyProvider.Set so Windows
// ACL hardening actually runs (a raw os.WriteFile leaves the inherited broad
// DACL and would trip the new loose-ACL WARN on the windows lane; Unix stays
// 0600 — Set is CreateTemp+rename+MkdirAll 0700).
func seedDoctorVault(t *testing.T, vaultDir string) {
	t.Helper()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(vaultDir, "store.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	fp := store.FileKeyProvider{Path: filepath.Join(vaultDir, "master.key.plain")}
	if err := fp.Set(mk); err != nil {
		t.Fatal(err)
	}
}
```

Run: `go test ./internal/cli/ -run 'TestDoctor' -v 2>&1 | tail -15`
Expected: 全 PASS(windows 本机:healthy-vault 腿 masterkey PASS——Set 产出硬化 ACL;unix lane 由 CI 兜底:0600 → mode-bit 不触发)。

- [ ] **Step 4: Windows-only 真路径腿(doctor_windows_test.go 新建)**

```go
//go:build windows

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"ssh-manager-mcp/internal/roles"
)

// seedBroadReadACE plants an explicit Everyone-allow read ACE on path
// (replacing its DACL, PROTECTED) — the signal-3 WARN trigger, through the
// REAL InspectFileACL (no seam).
func seedBroadReadACE(t *testing.T, path string) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	dacl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.FILE_GENERIC_READ,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}, nil)
	if err != nil {
		t.Fatalf("build DACL: %v", err)
	}
	si := windows.SECURITY_INFORMATION(windows.PROTECTED_DACL_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION)
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, si, nil, nil, dacl, nil); err != nil {
		t.Fatalf("seed DACL: %v", err)
	}
}

// TestDoctorVaultKeyWindowsRealPath drives checkVaultKey against the REAL
// on-disk ACL (no seam): hardened seed → PASS; a planted broad read ACE →
// WARN with the frozen clause and unchanged exit code.
func TestDoctorVaultKeyWindowsRealPath(t *testing.T) {
	stubServeServiceState(t, "Running")
	vd, _ := withDoctorDirs(t)
	seedDoctorVault(t, vd)
	if err := roles.Save(roles.State{Role: roles.RoleServer, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}

	// Hardened seed → PASS (proves seedDoctorVault's Set-based seeding).
	out, err := driveDoctor(t)
	if err != nil {
		t.Fatalf("hardened seed must not FAIL: %v\n%s", err, out)
	}
	if !strings.Contains(out, "masterkey:  PASS") || !strings.Contains(out, "overall: 0 WARN, 0 FAIL") {
		t.Fatalf("hardened seed must be fully quiet:\n%s", out)
	}

	// Planted broad ACE → WARN through the real InspectFileACL.
	seedBroadReadACE(t, filepath.Join(vd, "master.key.plain"))
	out, err = driveDoctor(t)
	if err != nil {
		t.Fatalf("loose-ACL WARN must not change the exit code: %v\n%s", err, out)
	}
	for _, want := range []string{
		"masterkey:  WARN",
		"grants access to unexpected principals: S-1-1-0",
		"overall: 1 WARN, 0 FAIL",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	_ = os.Getenv // keep os imported if unused after trimming
}
```

(若末行 `_ = os.Getenv` 不需要则去掉并删 os import——以最终编译器提示为准,plan 里保留是为了写测试时警觉 unused import。)

Run: `go test ./internal/cli/ -run TestDoctorVaultKeyWindowsRealPath -v`
Expected: PASS(真路径:seed→Set 硬化→PASS;种宽→WARN)。

- [ ] **Step 5: 全量回归**

Run: `go test ./... 2>&1 | tail -25`
Expected: 全 PASS(store/cli 两包新增测试 + 既有全绿)。
Run: `go vet ./...`
Expected: 无输出。

- [ ] **Step 6: 文档销项**

6a. `docs/backlog.md`:P2 节 #6、#7 条目套用本仓销项格式(P0/P1 条目的画线样式):原文保留,前缀加 `~~...~~` 并追加 **已落地(Plan 38, 2026-08-26 并 master;spec 三轮 xcheck 收敛 rev3[owner 特批第三轮人工终审] + 3 任务 SDD;……)** 摘要——#6 注明白名单 read-capable + owner 信号 + 分段文案;#7 注明 ExitCodeError 管道 + doctorExitCode 双真相源根除 + 2 的产源/帮助文本留 #5(两项预埋验收)。

6b. `docs/compat-matrix.md`:在 7-11 行的 v0.10 系占位注释块内追加一条:

```markdown
<!-- v0.10.0/0.11.0（Plan 38 doctor 硬化接线）：CLI-only 增量——doctor exit 2 管道已接线、契约已在代码/测试层定义（0/1 不变，无破坏），生产 2 源与帮助文本行随 #5 二期同步；Windows master.key 新增 DACL-loose WARN 行（owner 异常 / 非白名单读授权 / null DACL，WARN 不改退出码）。与 Plan 32-37 同批发版回写，占位注释届时删除。 -->
```

- [ ] **Step 7: Commit**

```bash
git add internal/cli/doctor.go internal/cli/doctor_test.go internal/cli/doctor_windows_test.go docs/backlog.md docs/compat-matrix.md
git commit -m "feat(cli): doctor master.key DACL readback 接线——分段冻结文案/seam err→FAIL/Set-seed/Windows 真路径腿+backlog #6 #7 销项,Plan 38 T3"
```

---

## Self-Review 记录(plan 作者已跑)

1. **Spec 覆盖**:rev3 §1(API+语义)→ Task 1;§2/§2.1(接线+文案)→ Task 3;§3/§3.1-3.4(exit 链)→ Task 2(§3.4 帮助文本不动 = Global Constraint);§4.1 → Task 1 Step 5;§4.2 → Task 3 Step 1/3/4(seed 修正 + seam 腿跨平台 + Windows 真路径腿拆 build-tag 文件);§4.3 → Task 2 Step 1/3;§5 → Task 3 Step 6 + Task 1 Step 1(探针回填);§6/§7 为约束与登记,无实施动作。无缺口。
2. **Placeholder 扫描**:Task 1 Step 5 矩阵⑨ 的 `ToooseOrExpanded` 笔误已附修正注(写码时用 `!rep.TooLoose()`);其余步骤均含全码。
3. **类型一致性**:`FileACLReport` 六字段/`TooLoose`/`InspectFileACL(path)(FileACLReport, error)` 在 Task 1 Produces、Task 3 消费处一致;`inspectFileACL` seam 在 Task 3 Step 1(stub)与 Step 2(定义)一致;`NewExitCodeError`/`ExitCodeFor` 签名 Task 2 内自洽。
