# Plan 13 — NAS 定时备份（明文快照）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 ssh-manager-mcp 加 NAS 定时明文备份功能：`backup create` 把整个 vault 以明文 JSON 快照定时写到挂载的群晖目录，无变化不备份（诚实降级语义），按份数轮转，防挂载掉了静默写本地，带 `.sha256` 边车抓 bit-rot；`backup verify` 按需校验；`import` 加格式嗅探支持恢复明文备份。

**Architecture:** 复用 Plan 11 的 `ExportSnapshot`/`ImportSnapshot`（零新加密逻辑）。新增 `backup` 子命令（create + verify）、纯超时回收的 `O_EXCL` 锁（无 build-tag 分流）、`.sha256` 边车。前置修 `ExportSnapshot` 的确定性 bug（5 条查询统一 `ORDER BY id`）。部署目标 Windows，`schtasks` + UNC 路径。

**Tech Stack:** Go 1.x，cobra CLI，SQLite（`internal/store`），`encoding/json`，`crypto/sha256`，`os` 文件原子写。

**Spec:** `docs/superpowers/specs/2026-08-12-plan-13-nas-backup-design.md`（v3，三轮 xcheck 收敛）。本 plan 的每条规格都映射回 spec 章节。

## Global Constraints

（来自 spec，逐条 verbatim；每个 task 隐含遵守）

- **明文 JSON**：备份文件 = `store.Snapshot` 的明文 `json.MarshalIndent(snap, "", "  ")`（与 `export.go:46` 同缩进，保证确定性）。不加密（spec §3.1）。
- **复用 Plan 11**：`ExportSnapshot()` / `ImportSnapshot()` 不改语义，只修确定性（spec §3.3）。
- **ORDER BY 统一 `id`**：`export.go` 五条查询（servers:118, credentials:138, profiles:171, grants:189, projects:207）全部用主键 `id` 排序；grants 是 `ORDER BY profile_id, server_id`；现有 servers/profiles 的 `ORDER BY name` 改 `ORDER BY id`（spec §5.5）。
- **锁文件**：`--dir/.ssh-manager-backup.lock`，`O_EXCL` 建立，内容只有 `<start-ts>`（Unix epoch 秒，十进制 ASCII）。撞锁 + `start-ts` 距今 > 5 min → 窃取重建；否则 exit 0 skip。**无 pidAlive，无 build-tag，无 `golang.org/x/sys/windows`**（spec §3.8）。
- **超时窗口 = 5 min**（`const staleLockSeconds = 300`）。**不**是 30 min。
- **边车**：`<file>.sha256`，内容**只有一行** `file_sha256=<hex>\n`（无 size 字段）。0600（Linux）/ ACL（Windows）。
- **marker**：`--dir/.ssh-manager-backup-marker` 必须存在（只查存在性），否则 fail-closed。
- **`.git` 护栏**：只查 `--dir` 自身是否含 `.git`（`filepath.Join(dir, ".git")` 存在性），**不向上遍历**。命中 fail-closed。
- **文件名**：`<prefix>-<UTC>.json`，UTC 时间戳格式 `YYYYMMDD-HHMMSS`（`time.Now().UTC().Format("20060102-150405")`）。同秒撞名加 `-2`/`-3`。轮转/skip 按文件名字典序（glob 严格 `<prefix>-*.json`，不匹配 `.sha256`）。
- **skip 语义（诚实）**：含 audit_log → 活跃服务器上 skip 几乎不触发，主要服务空闲/静态 vault。测试构造"未变"须清空 audit 或关闭其写入。这是已知降级，不是 bug（spec §3.5）。
- **import 嗅探**：读文件 → `vaultio.IsEncrypted(data)`（sniff 前 lstrip 空白 + 去 UTF-8 BOM）→ 仅加密分支才 `passphrasePrompt()`。明文分支直接 `json.Unmarshal`，不弹口令。
- **写后校验**：`json.Unmarshal` 重读落盘 `.json` 抓结构损坏（主要价值）+ SHA256 重算断言 == 边车（防御纵深）。
- **temp 清理**：原子写失败路径 `defer os.Remove(tempFile)`。
- **轮转孤儿边车**：轮转末尾 best-effort 扫无对应 `.json` 的 `.sha256` 删除。
- **Windows**：dir fsync 跳过（`runtime.GOOS == "windows"`）；文件 fsync 全平台；权限位 0600 写但不程序校验目录。
- **cache_tokens 不进 Snapshot**（spec §11，已是非目标，本 plan 不动）。
- **铁律**：仓库 PUBLIC，zero-tol 凭据泄露；每个 task 结束 `go test ./...` green + `gofmt -l .` 干净 + `go vet ./...` clean。

---

## File Structure

| 文件 | 责任 | 新/改 |
|---|---|---|
| `internal/store/export.go` | ExportSnapshot 五查询 ORDER BY 确定性 | 改 |
| `internal/store/export_test.go` | 加确定性测试（含同名 server/profile） | 改 |
| `internal/vaultio/vaultio.go` | `IsEncrypted(data []byte) bool` helper（lstrip+BOM） | 改 |
| `internal/vaultio/vaultio_test.go` | IsEncrypted 测试（含 BOM/空白） | 改 |
| `internal/cli/import.go` | sniff + prompt 重排（读→sniff→仅加密 prompt） | 改 |
| `internal/cli/import_test.go` 或 `export_import_smoke_test.go` | 明文 .json import 不弹口令；BOM 识别 | 改 |
| `internal/cli/lock.go`（新） | O_EXCL 锁 + start-ts + 5min 超时回收（纯超时，无 pidAlive） | 新 |
| `internal/cli/lock_test.go`（新） | 锁 acquire/steal/concurrent/超时 | 新 |
| `internal/cli/backup.go`（新） | newBackupCmd (create + verify)；marker/.git/锁/skip/原子写/轮转/边车/写后校验 | 新 |
| `internal/cli/backup_test.go`（新） | create 全流程、skip、轮转、孤儿边车、verify bit-rot | 新 |
| `internal/cli/root.go` | 注册 newBackupCmd | 改 |
| `docs/backup-restore.md` | Plan 13 章节（部署硬约束、marker、schtasks UNC+超时、恢复、skip 语义、审计风险） | 改 |

任务顺序：T1（确定性地基）→ T2（IsEncrypted helper）→ T3（import sniff）→ T4（锁）→ T5（backup create）→ T6（backup verify）→ T7（root 注册 + 烟雾测试）→ T8（文档）。T1-T3 独立可并行审，但 T5 依赖 T1+T4，按序执行。

---

### Task 1: ExportSnapshot ORDER BY 确定性修复

**Files:**
- Modify: `internal/store/export.go`（5 处查询）
- Test: `internal/store/export_test.go`（加 2 个确定性测试）

**Interfaces:**
- Consumes: 无（store 内部）
- Produces: `ExportSnapshot()` 的确定性保证（后续 backup skip 依赖）

**背景**：`ExportSnapshot()` 现有 5 条查询中 3 条无 ORDER BY（credentials:138, grants:189, projects:207），2 条用 `ORDER BY name`（servers:118, profiles:171）。SQLite 不保证无 ORDER BY 时行序稳定 → 相同 vault 多次导出 JSON 字节序可能不同 → backup 的 skip 永远失效 → NAS 堆满相同备份。grants 的 `name` 不唯一也会留隐患。**统一用主键 `id`（必唯一）根治。**

- [ ] **Step 1: 写确定性失败测试**

在 `internal/store/export_test.go` 末尾追加：

```go
// TestExportSnapshot_Deterministic asserts the SAME vault exports byte-identical
// JSON across repeated calls (the foundation backup's skip-if-unchanged relies on).
// Guards against missing ORDER BY in any ExportSnapshot query.
func TestExportSnapshot_Deterministic(t *testing.T) {
	s := newTestStore(t)
	cid, _ := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	srv1, _ := s.AddServer(&models.Server{Name: "zeta", Host: "192.0.2.1", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	srv2, _ := s.AddServer(&models.Server{Name: "alpha", Host: "192.0.2.2", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	prof1, _ := s.AddProfile("z-team")
	prof2, _ := s.AddProfile("a-team")
	s.GrantServers(prof1, []string{srv1, srv2})
	s.GrantServers(prof2, []string{srv1})
	s.AddProject("p2", prof2)
	s.AddProject("p1", prof1)
	s.SaveHostKey("192.0.2.2", 22, []byte("hk"))
	s.WriteAudit(AuditRow{Action: "exec", ProjectID: "p1", ServerID: srv1, Status: "ok"})
	s.WriteAudit(AuditRow{Action: "exec", ProjectID: "p2", ServerID: srv2, Status: "ok"})

	first, err := s.ExportSnapshot()
	if err != nil {
		t.Fatalf("first export: %v", err)
	}
	b1, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		snap, err := s.ExportSnapshot()
		if err != nil {
			t.Fatalf("export %d: %v", i, err)
		}
		b, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b1, b) {
			t.Fatalf("export not deterministic on run %d:\n%s\n%s", i, b1, b)
		}
	}
}

// TestExportSnapshot_Deterministic_SameName covers the ORDER BY id (not name) case:
// two servers and two profiles with IDENTICAL names must still produce stable order
// (by primary key), else skip breaks when name collisions exist.
func TestExportSnapshot_Deterministic_SameName(t *testing.T) {
	s := newTestStore(t)
	cid, _ := s.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	// two servers with SAME name, different ids
	s1, _ := s.AddServer(&models.Server{Name: "dup", Host: "10.0.0.1", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	s2, _ := s.AddServer(&models.Server{Name: "dup", Host: "10.0.0.2", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	// two profiles with SAME name
	p1, _ := s.AddProfile("dup")
	p2, _ := s.AddProfile("dup")
	s.GrantServers(p1, []string{s1})
	s.GrantServers(p2, []string{s2})

	first, _ := json.Marshal(mustExport(t, s))
	for i := 0; i < 5; i++ {
		b, _ := json.Marshal(mustExport(t, s))
		if !bytes.Equal(first, b) {
			t.Fatalf("non-deterministic with same-name rows on run %d", i)
		}
	}
}

func mustExport(t *testing.T, s *Store) *Snapshot {
	t.Helper()
	snap, err := s.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}
```

需要在 export_test.go 的 import 块加 `"encoding/json"`（已有 `"bytes"`、`"testing"`、`models`）。

- [ ] **Step 2: 跑测试验证失败**

```
go test ./internal/store/ -run TestExportSnapshot_Deterministic -v
```
Expected: FAIL（当前无 ORDER BY 的查询返回顺序可能不稳；注意 SQLite 在小数据集上可能"碰巧"稳定，若此测试在修复前偶然 PASS，仍执行 Step 3 修复——确定性 bug 是逻辑事实，不靠测试碰巧红）。

- [ ] **Step 3: 改 5 条查询加/改 ORDER BY**

`internal/store/export.go`：

1. **line 118** servers：`ORDER BY name` → `ORDER BY id`
```go
rs, err := s.db.Query(`SELECT id,name,host,port,user,auth_method,credential_id,COALESCE(sudo_credential_id,''),COALESCE(tags,''),description,location,hardware,services,role,caveats,created_at,updated_at FROM servers ORDER BY id`)
```
2. **line 138** credentials：末尾加 ` ORDER BY id`
```go
rc, err := s.db.Query(`SELECT id,type,secret_blob,COALESCE(passphrase_blob,''),created_at,updated_at FROM credentials ORDER BY id`)
```
3. **line 171** profiles：`ORDER BY name` → `ORDER BY id`
```go
rp, err := s.db.Query(`SELECT id,name,created_at,updated_at FROM profiles ORDER BY id`)
```
4. **line 189** grants：末尾加 ` ORDER BY profile_id, server_id`
```go
rg, err := s.db.Query(`SELECT profile_id, server_id FROM profile_servers ORDER BY profile_id, server_id`)
```
5. **line 207** projects：末尾加 ` ORDER BY id`
```go
rj, err := s.db.Query(`SELECT id,name,token_hash,token_salt,token_prefix,profile_id,status,created_at,updated_at FROM projects ORDER BY id`)
```

（audit:231 已是 `ORDER BY id`、host_keys:96 已是 `ORDER BY host_port`，不动。）

- [ ] **Step 4: 跑测试验证通过**

```
go test ./internal/store/ -run TestExportSnapshot -v
go test ./internal/store/ -run TestImportSnapshot -v
```
Expected: PASS（含原有 CapturesAllTables / RoundTrip_CrossMasterKey / RefusesNonEmpty + 2 个新确定性测试）。

- [ ] **Step 5: no-regression + 提交**

```
go test ./... 
gofmt -l .
go vet ./...
```
Expected: 全 green，gofmt 无输出，vet clean。

```bash
git add internal/store/export.go internal/store/export_test.go
git commit -m "fix(store): ExportSnapshot determinism (ORDER BY id on all 5 queries)

credentials/grants/projects had no ORDER BY; servers/profiles used
ORDER BY name (non-unique). SQLite row order is unstable without a
unique key, so identical vaults could serialize to different JSON
bytes across calls — breaking backup's skip-if-unchanged (Plan 13)
and export/import/cache determinism. Unified to primary-key id;
grants use (profile_id, server_id)."
```

---

### Task 2: vaultio.IsEncrypted helper（lstrip + BOM）

**Files:**
- Modify: `internal/vaultio/vaultio.go`（加导出 helper）
- Test: `internal/vaultio/vaultio_test.go`（加测试，若不存在则建）

**Interfaces:**
- Consumes: `magic`（包内未导出 var，line 23）
- Produces: `vaultio.IsEncrypted(data []byte) bool` —— 供 Task 3 的 import 嗅探用

**背景**：import 要支持明文 .json，须区分 `SSHMGRV1` 加密文件 vs 明文 JSON。magic 首字节 `S`(0x53)，JSON 首字节 `{`(0x7B)。但用户手改的 JSON 可能带 UTF-8 BOM（`EF BB BF`）或前导空白 → 首字节≠`{` → 误判加密。helper 内部 lstrip 空白 + 去 BOM 后再比 magic。

- [ ] **Step 1: 写失败测试**

先确认 `internal/vaultio/vaultio_test.go` 是否存在：

```
ls internal/vaultio/
```

在 `internal/vaultio/vaultio_test.go` 追加（若文件不存在则新建，package vaultio）：

```go
package vaultio

import "testing"

func TestIsEncrypted(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want bool
	}{
		{"magic prefix", append([]byte("SSHMGRV1"), []byte("rest...")...), true},
		{"plain json object", []byte(`{"version":1}`), false},
		{"plain json with leading space", []byte("   {\"version\":1}"), false},
		{"plain json with tab/newline", []byte("\n\t{\"v\":1}"), false},
		{"plain json with UTF-8 BOM", []byte{0xEF, 0xBB, 0xBF, '{', '}'}, false},
		{"empty", []byte{}, false},
		{"only whitespace", []byte("   \n\t "), false},
		{"magic only exact 8 bytes", []byte("SSHMGRV1"), true},
		{"short non-magic non-json", []byte("hi"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsEncrypted(c.data); got != c.want {
				t.Fatalf("IsEncrypted(%q) = %v, want %v", c.data, got, c.want)
			}
		})
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

```
go test ./internal/vaultio/ -run TestIsEncrypted -v
```
Expected: FAIL（`IsEncrypted` 未定义，编译错误）。

- [ ] **Step 3: 实现 IsEncrypted**

在 `internal/vaultio/vaultio.go` 末尾追加：

```go
// IsEncrypted reports whether data begins (after any leading whitespace and an
// optional UTF-8 BOM) with the vaultio magic header — i.e. data is a passphrase-
// encrypted envelope produced by Encrypt/EncryptWithKey, not plaintext.
//
// Used by `import` to sniff encrypted exports vs plaintext JSON backups without
// prompting for a passphrase on the plaintext path. The BOM/whitespace skip
// tolerates hand-edited JSON that a user may have saved with a BOM or leading
// newlines; without it, such a file's first byte would not be '{' and would be
// misclassified as encrypted, causing a pointless GCM auth failure.
func IsEncrypted(data []byte) bool {
	// strip UTF-8 BOM
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}
	// lstrip ASCII whitespace
	i := 0
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	data = data[i:]
	return len(data) >= len(magic) && bytes.Equal(data[:len(magic)], magic)
}
```

（`bytes` 已在 import 块，line 11。）

- [ ] **Step 4: 跑测试验证通过**

```
go test ./internal/vaultio/ -run TestIsEncrypted -v
go test ./internal/vaultio/ -v
```
Expected: PASS（含原有 Encrypt/Decrypt/EncryptWithKey/DecryptWithKey 测试 + 新 IsEncrypted）。

- [ ] **Step 5: no-regression + 提交**

```
go test ./...
gofmt -l .
go vet ./...
```

```bash
git add internal/vaultio/vaultio.go internal/vaultio/vaultio_test.go
git commit -m "feat(vaultio): IsEncrypted helper (BOM/whitespace-tolerant magic sniff)

For import's plaintext-JSON sniff (Plan 13): distinguish SSHMGRV1
envelopes from plaintext backups without prompting for a passphrase.
Strips a leading UTF-8 BOM and ASCII whitespace before comparing the
magic prefix, so hand-edited JSON saved with a BOM/leading newlines is
not misclassified as encrypted (which would cause a GCM auth failure)."
```

---

### Task 3: import 格式嗅探 + passphrase prompt 重排

**Files:**
- Modify: `internal/cli/import.go`
- Test: `internal/cli/export_import_smoke_test.go`（加明文 import 测试）

**Interfaces:**
- Consumes: `vaultio.IsEncrypted`（Task 2）、`passphrasePrompt`（cli 包 seam）、`openUnlockedStore`、`store.ImportSnapshot`
- Produces: `import <file>` 同时接受 `SSHMGRV1` 加密文件和明文 .json

**背景**：现状 `import.go:30` 无条件先 `passphrasePrompt()` 再解密。支持明文备份后若不改顺序，明文路径会无谓弹口令（UX 倒退）。流程改为：读文件 → `IsEncrypted` → 仅加密分支 prompt。

- [ ] **Step 1: 写失败测试**

在 `internal/cli/export_import_smoke_test.go` 末尾追加：

```go
// TestImport_PlaintextJSON_NoPassphrase writes a PLAINTEXT snapshot JSON (no
// SSHMGRV1 envelope), imports it into a fresh store, and asserts the import
// succeeds WITHOUT ever prompting for a passphrase (the prompt seam fails the
// test if called).
func TestImport_PlaintextJSON_NoPassphrase(t *testing.T) {
	dir := t.TempDir()
	dbB := filepath.Join(dir, "b.db")
	inFile := filepath.Join(dir, "vault.json")

	// seed store A, export to PLAINTEXT json (no encryption) by calling ExportSnapshot directly
	dbA := filepath.Join(dir, "a.db")
	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{"SSHMGR_STORE": dbA, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	stA, err := store.Open(dbA, mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	stA.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", User: "deploy", AuthMethod: models.AuthPassword, CredentialID: cid})
	snap, err := stA.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	stA.Close()
	plaintext, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inFile, plaintext, 0o600); err != nil {
		t.Fatal(err)
	}

	// point at empty B with a DIFFERENT master key
	mk2, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{"SSHMGR_STORE": dbB, "SSHMGR_MASTERKEY_HEX": hexEncode(mk2)})

	// FAIL the test if import prompts for a passphrase on the plaintext path
	orig := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) {
		t.Fatal("passphrasePrompt must NOT be called for plaintext import")
		return nil, nil
	}
	t.Cleanup(func() { passphrasePrompt = orig })

	root := NewRootCmd()
	root.SetArgs([]string{"import", inFile})
	if err := root.Execute(); err != nil {
		t.Fatalf("plaintext import: %v", err)
	}

	// verify server landed in B
	stB, err := store.Open(dbB, mk2)
	if err != nil {
		t.Fatal(err)
	}
	defer stB.Close()
	got, err := stB.GetServerByName("gpu")
	if got == nil || err != nil {
		t.Fatalf("server not imported from plaintext: %v %v", got, err)
	}
}

// TestImport_EncryptedFile_StillPrompts guards that the encrypted path is
// unchanged: a real SSHMGRV1 file still prompts and decrypts.
func TestImport_EncryptedFile_StillPrompts(t *testing.T) {
	dir := t.TempDir()
	dbA := filepath.Join(dir, "a.db")
	dbB := filepath.Join(dir, "b.db")
	outFile := filepath.Join(dir, "vault.export")

	mk, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{"SSHMGR_STORE": dbA, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	stA, _ := store.Open(dbA, mk)
	cid, _ := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw-A")})
	stA.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", User: "deploy", AuthMethod: models.AuthPassword, CredentialID: cid})
	stA.Close()

	// export encrypted (uses passphrasePrompt seam)
	orig := passphrasePrompt
	passphrasePrompt = func() ([]byte, error) { return []byte("strong-passphrase-123"), nil }
	origConfirm := passphraseConfirmPrompt
	passphraseConfirmPrompt = func() ([]byte, error) { return []byte("strong-passphrase-123"), nil }
	t.Cleanup(func() { passphrasePrompt = orig; passphraseConfirmPrompt = origConfirm })

	root := NewRootCmd()
	root.SetArgs([]string{"export", "--out", outFile})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}

	// import into fresh B — should prompt (seam already swapped) and succeed
	mk2, _ := store.GenerateMasterKey()
	withEnv(t, map[string]string{"SSHMGR_STORE": dbB, "SSHMGR_MASTERKEY_HEX": hexEncode(mk2)})
	root2 := NewRootCmd()
	root2.SetArgs([]string{"import", outFile})
	if err := root2.Execute(); err != nil {
		t.Fatalf("encrypted import: %v", err)
	}
}
```

需在 export_import_smoke_test.go 的 import 块确认有 `"encoding/json"`（若无则加）。现有 import 块：`bytes`、`os`、`path/filepath`、`testing`、`models`、`store` —— 加 `"encoding/json"`。

- [ ] **Step 2: 跑测试验证失败**

```
go test ./internal/cli/ -run TestImport_PlaintextJSON_NoPassphrase -v
```
Expected: FAIL（现状 import 无条件先 prompt → `passphrasePrompt` 被调用 → `t.Fatal`）。

- [ ] **Step 3: 重排 import.go 的流程**

把 `internal/cli/import.go` 的 `RunE` 改成 sniff-first（读文件 → IsEncrypted → 仅加密分支 prompt）。完整新 `RunE`：

```go
		RunE: func(cmd *cobra.Command, args []string) error {
			blob, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			// Sniff BEFORE prompting: a plaintext JSON backup (Plan 13) must not
			// trigger a passphrase prompt. Only the SSHMGRV1 envelope path prompts.
			// NOTE: vaultio.EncryptWithKey (Plan 12 cache) ALSO uses SSHMGRV1 magic.
			// Feeding a cache file here would classify as encrypted, prompt, then
			// fail GCM auth (safe — just not the user's intent). Import is for
			// export/backup files, not cache files.
			var plaintext []byte
			if vaultio.IsEncrypted(blob) {
				pw, err := passphrasePrompt()
				if err != nil {
					return err
				}
				plaintext, err = vaultio.Decrypt(pw, blob)
				if err != nil {
					return err
				}
			} else {
				plaintext = blob
			}
			var snap store.Snapshot
			if err := json.Unmarshal(plaintext, &snap); err != nil {
				return err
			}
			st, err := openUnlockedStore()
			if err != nil {
				return err
			}
			defer st.Close()
			if err := st.ImportSnapshot(&snap); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "imported %d servers / %d credentials\n", len(snap.Servers), len(snap.Credentials))
			return nil
		},
```

（保留 `newImportCmd` 的其余部分：Use/Short/Long/Args 不变。`fmt`/`os`/`cobra`/`store`/`vaultio` import 块不变。）

- [ ] **Step 4: 跑测试验证通过**

```
go test ./internal/cli/ -run TestImport -v
go test ./internal/cli/ -run TestExportImport -v
```
Expected: PASS（含原有 CLIRoundTrip + 新 PlaintextJSON_NoPassphrase + EncryptedFile_StillPrompts）。

- [ ] **Step 5: no-regression + 提交**

```
go test ./...
gofmt -l .
go vet ./...
```

```bash
git add internal/cli/import.go internal/cli/export_import_smoke_test.go
git commit -m "feat(cli): import sniffs plaintext JSON vs SSHMGRV1 envelope

Read file -> vaultio.IsEncrypted -> prompt passphrase ONLY on the
encrypted branch. Plaintext backups (Plan 13) import without a prompt
(else UX regression). Adds a comment that Plan 12 cache files share the
SSHMGRV1 magic, so a cache file fed to import would classify as
encrypted and fail GCM auth (safe, just not intended)."
```

---

### Task 4: O_EXCL 锁 + 纯超时陈旧锁回收

**Files:**
- Create: `internal/cli/lock.go`
- Test: Create `internal/cli/lock_test.go`

**Interfaces:**
- Consumes: `os`、`time`、`fmt`
- Produces:
  - `const staleLockSeconds = 300`
  - `type backupLock struct { ... }`
  - `func acquireBackupLock(dir string) (*backupLock, error)` —— 成功返回需 `Release()` 的锁；撞锁且未超时返回特殊可识别 error（见下）
  - 锁文件路径 = `filepath.Join(dir, ".ssh-manager-backup.lock")`
  - 内容 = `<start-ts>`（Unix 秒，十进制 ASCII + `\n`）

**背景**：`O_EXCL` 锁不会进程退出时自释放，任何崩溃留孤儿锁 → 静默停摆。纯超时回收（不用 pidAlive，不用 build-tag）：撞锁时读锁文件 `start-ts`，距今 > 5 min 判孤儿窃取重建，否则真并发 exit 0 skip。不用 flock（SMB 不可靠）。

**设计约定**：
- `acquireBackupLock` 先尝试 `O_CREATE|O_EXCL|O_WRONLY` 建锁写 `<start-ts>`。成功 → 返回 `*backupLock`。
- 失败（`os.ErrExist`）→ 读锁文件 `start-ts`：
  - 解析失败 / 文件没了（race，别人刚释放）→ **重试一次 O_EXCL**；再失败 → 当真并发。
  - `time.Since(startTs) > staleLockSeconds` → 窃取：`os.Remove` 锁文件 + 重建 O_EXCL 写新 `start-ts`。窃取也失败（race）→ 当真并发。
  - 未超时 → 返回 `ErrConcurrentBackup`（sentinel）让 caller exit 0 skip。
- `backupLock.Release()` = `os.Remove(锁文件)`（忽略 not-found）。
- start-ts 用调用方的时钟（`time.Now().Unix()`）；注意脚本里不能用 `Date.now()`（那是 JS 限制），这里是 Go，`time.Now()` 合法。

- [ ] **Step 1: 写失败测试**

新建 `internal/cli/lock_test.go`：

```go
package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeStaleLock writes a lock file with a start-ts far in the past,
// simulating a crashed previous run's orphan lock.
func writeStaleLock(t *testing.T, dir string, age time.Duration) {
	t.Helper()
	ts := time.Now().Add(-age).Unix()
	if err := os.WriteFile(filepath.Join(dir, ".ssh-manager-backup.lock"),
		[]byte(strconv.FormatInt(ts, 10)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireBackupLock_Fresh(t *testing.T) {
	dir := t.TempDir()
	lk, err := acquireBackupLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lk.Release()
	// lock file exists with a numeric start-ts
	b, err := os.ReadFile(filepath.Join(dir, ".ssh-manager-backup.lock"))
	if err != nil {
		t.Fatal(err)
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		t.Fatalf("lock content not a unix ts: %q", b)
	}
	if time.Unix(ts, 0).After(time.Now().Add(time.Second)) {
		t.Fatalf("start-ts in the future: %d", ts)
	}
}

func TestAcquireBackupLock_ConcurrentSkip(t *testing.T) {
	dir := t.TempDir()
	// hold a fresh lock (start-ts = now)
	lk1, err := acquireBackupLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lk1.Release()
	// second acquire should hit ErrConcurrentBackup (not stale — just created)
	_, err = acquireBackupLock(dir)
	if err != ErrConcurrentBackup {
		t.Fatalf("second acquire err = %v, want ErrConcurrentBackup", err)
	}
}

func TestAcquireBackupLock_StaleReclaim(t *testing.T) {
	dir := t.TempDir()
	// orphan lock from 10 min ago (> 5 min threshold)
	writeStaleLock(t, dir, 10*time.Minute)
	lk, err := acquireBackupLock(dir)
	if err != nil {
		t.Fatalf("acquire should reclaim stale lock: %v", err)
	}
	defer lk.Release()
}

func TestAcquireBackupLock_NotYetStale(t *testing.T) {
	dir := t.TempDir()
	// lock from 1 min ago (< 5 min threshold) — still "running", must NOT reclaim
	writeStaleLock(t, dir, time.Minute)
	_, err := acquireBackupLock(dir)
	if err != ErrConcurrentBackup {
		t.Fatalf("err = %v, want ErrConcurrentBackup (lock not yet stale)", err)
	}
}

func TestBackupLock_ReleaseRemovesFile(t *testing.T) {
	dir := t.TempDir()
	lk, err := acquireBackupLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	lk.Release()
	if _, err := os.Stat(filepath.Join(dir, ".ssh-manager-backup.lock")); !os.IsNotExist(err) {
		t.Fatalf("lock file still exists after Release: %v", err)
	}
	// double Release is a no-op (must not panic)
	lk.Release()
}
```

- [ ] **Step 2: 跑测试验证失败**

```
go test ./internal/cli/ -run TestAcquireBackupLock -v
go test ./internal/cli/ -run TestBackupLock -v
```
Expected: FAIL（`acquireBackupLock`/`ErrConcurrentBackup`/`backupLock` 未定义，编译错误）。

- [ ] **Step 3: 实现 lock.go**

新建 `internal/cli/lock.go`：

```go
package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// staleLockSeconds is how old a backup lock file must be before a later run
// reclaims it. KB-scale vault + LAN NAS backup completes in seconds; 5 min is
// generous (a backup still running after 5 min means the NAS is hung — not worth
// waiting longer). Spec §3.8.
const staleLockSeconds = 300

const backupLockName = ".ssh-manager-backup.lock"

// ErrConcurrentBackup is returned by acquireBackupLock when the lock is held by
// a still-running (non-stale) backup. Callers exit 0 with a "skipping" message
// rather than waiting or erroring.
var ErrConcurrentBackup = errors.New("another backup is in progress")

// backupLock is an O_EXCL advisory lock guarding `backup create` against
// concurrent runs. O_EXCL does NOT auto-release on process exit, so any crash
// (SIGKILL/OOM/panic) leaves an orphan; reclaimLogic is pure-timestamp (not
// pidAlive) — single-host single-mount deployment has no cross-machine contention.
type backupLock struct {
	path string
}

// acquireBackupLock creates an O_EXCL lock file containing the current unix ts.
// If the lock exists, it inspects the stored start-ts: older than staleLockSeconds
// => reclaim (steal); otherwise => ErrConcurrentBackup.
func acquireBackupLock(dir string) (*backupLock, error) {
	path := filepath.Join(dir, backupLockName)
	if lk, err := tryCreateLock(path); err == nil {
		return lk, nil
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}

	// Lock exists — read its start-ts.
	stale, err := lockIsStale(path)
	if err != nil {
		// unreadable / unparseable / raced away — one retry, then treat as concurrent.
		if lk, e2 := tryCreateLock(path); e2 == nil {
			return lk, nil
		}
		return nil, ErrConcurrentBackup
	}
	if !stale {
		return nil, ErrConcurrentBackup
	}
	// Stale: steal by removing then recreating. If the remove/recreate races
	// (another run beat us), fall back to ErrConcurrentBackup.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reclaim stale lock %s: %w", path, err)
	}
	if lk, err := tryCreateLock(path); err == nil {
		return lk, nil
	}
	return nil, ErrConcurrentBackup
}

// tryCreateLock does the O_EXCL create + writes "<unix-ts>\n".
func tryCreateLock(path string) (*backupLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.WriteString(strconv.FormatInt(time.Now().Unix(), 10) + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return &backupLock{path: path}, nil
}

// lockIsStale reads the lock's start-ts and reports whether it exceeds
// staleLockSeconds. An unparseable/missing file returns an error so the caller
// can retry-create.
func lockIsStale(path string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return false, err
	}
	return time.Since(time.Unix(ts, 0)) > staleLockSeconds, nil
}

// Release removes the lock file. Idempotent (ignores not-found).
func (l *backupLock) Release() {
	if l == nil || l.path == "" {
		return
	}
	os.Remove(l.path) // best-effort; ignore error
}
```

- [ ] **Step 4: 跑测试验证通过**

```
go test ./internal/cli/ -run "TestAcquireBackupLock|TestBackupLock" -v
```
Expected: 全 PASS。

- [ ] **Step 5: no-regression + 提交**

```
go test ./...
gofmt -l .
go vet ./...
```

```bash
git add internal/cli/lock.go internal/cli/lock_test.go
git commit -m "feat(cli): backup O_EXCL lock with pure-timestamp stale reclaim

O_EXCL does not auto-release on crash, so orphan locks would silently
stall later runs. Reclaim is pure-timestamp (start-ts older than 5 min
=> steal) — NO pidAlive, NO lock_unix.go/lock_windows.go build-tag
split. Single-host single-mount deployment has no cross-machine
contention and daily-trigger scheduling makes 'steal now' vs 'wait for
next trigger' equivalent (Plan 13 spec §3.8, v3 slimming). Lock file
holds only <start-ts> (pid/host fields dropped). 5 min not 30 min
(KB-scale backup; 5 min with no completion = NAS hung)."
```

---

### Task 5: backup create 命令（marker / .git / 锁 / skip / 原子写 / 轮转 / 边车 / 写后校验）

**Files:**
- Create: `internal/cli/backup.go`（先只做 `create`，verify 留 Task 6）
- Test: Create `internal/cli/backup_test.go`

**Interfaces:**
- Consumes: `store.ExportSnapshot`、`openUnlockedStore`、`acquireBackupLock`/`ErrConcurrentBackup`/`backupLock`（Task 4）、`encoding/json`、`crypto/sha256`、`os`、`path/filepath`、`time`、`sort`、`cobra`
- Produces: `func newBackupCmd() *cobra.Command`（create 子命令；verify 在 Task 6 加到同一 cmd tree）

**关键决策（spec §5.2）**：
- `create --dir <p> [--keep 7] [--prefix vault]`
- 流程：marker 检测 → .git 自身护栏 → 锁 → ExportSnapshot+MarshalIndent+SHA256 → skip 判定 → 原子写（temp+rename+defer remove）→ fsync → 边车（`file_sha256=<hex>` 单行）→ 写后校验（Unmarshal + hash 断言）→ 轮转（含孤儿边车扫）→ 删锁。
- skip：glob `<prefix>-*.json` 取字典序最大，读其 `.sha256` 的 `file_sha256`，相等则 skip 删锁 exit 0。边车缺失 → fail-open 出新备份。
- 原子写：`tempFile := filepath.Join(dir, ".<name>.tmp.<rand>")`；`0600`；写 → fsync → close → `os.Rename(temp, final)`。失败 `defer os.Remove(temp)`。同秒撞名 → final 已存在则加 `-2`/`-3`。
- 轮转：glob `<prefix>-*.json`，文件名降序，删第 `--keep` 个之后的；每个删 `.json` 同时 best-effort 删 `.sha256`（忽略 not-found）。再扫一遍所有 `.sha256` 删无对应 `.json` 的孤儿。
- `--keep 0` = 不轮转。
- Windows dir fsync 跳过（`runtime.GOOS == "windows"`）；文件 fsync 全平台。
- SHA256 边车/写后校验用**同一明文 `[]byte`** 算 hash 和写盘（不 re-marshal）。

**helper 设计**（都放 backup.go，便于 Task 6 verify 复用）：
- `func computeSnapshotJSON() ([]byte, error)` —— openUnlockedStore + ExportSnapshot + MarshalIndent（**注意**：算 hash 用这个返回的 `[]byte` 本身，写盘也用它）。
- `func writeSidecar(path, fileSHA string) error`
- `func parseSidecar(path string) (fileSHA string, ok bool)`
- `func atomicWriteFile(dir, name string, data []byte) (finalPath string, err error)` —— temp+fsync+rename+撞名处理+defer remove。
- `func rotateBackups(dir, prefix string, keep int) error` —— 含孤儿边车扫。
- `func dirContainsGit(dir string) bool` —— `filepath.Join(dir, ".git")` 存在性。
- `func markerExists(dir string) bool`

**skip 测试的 audit 陷阱**（spec §3.5）：`ExportSnapshot` 含 audit_log。要构造"vault 未变"让 skip 触发，测试 store 须**不写 audit**（即 seed 后不再调任何会 WriteAudit 的操作）。两个连续 `create` 中间不碰 store → 第二次 hash 相等 → skip。测试里若用了 `openUnlockedStore`，注意它打开的是真实 vault；用 `withEnv` 指向临时 db，seed 一次后不再动。

- [ ] **Step 1: 写失败测试（核心场景）**

新建 `internal/cli/backup_test.go`：

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedVaultForBackup points SSHMGR_STORE at a fresh temp db, seeds one server,
// and returns the dir + master key. Does NOT write audit (so skip can trigger).
func seedVaultForBackup(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "vault.db")
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	withEnv(t, map[string]string{"SSHMGR_STORE": db, "SSHMGR_MASTERKEY_HEX": hexEncode(mk)})
	st, err := store.Open(db, mk)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServer(&models.Server{Name: "gpu", Host: "192.0.2.10", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// runCreate runs `backup create` against bdir (a backup target dir) and returns stdout.
func runCreate(t *testing.T, bdir string, args ...string) (*bytes.Buffer, error) {
	t.Helper()
	full := append([]string{"backup", "create", "--dir", bdir}, args...)
	root := NewRootCmd()
	root.SetArgs(full)
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	err := root.Execute()
	return out, err
}

// touchMarker creates the marker file bdir requires.
func touchMarker(t *testing.T, bdir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bdir, ".ssh-manager-backup-marker"), []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCreate_MissingMarker_FailClosed(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir() // no marker
	_, err := runCreate(t, bdir)
	if err == nil {
		t.Fatal("expected fail-closed on missing marker")
	}
}

func TestBackupCreate_DirContainsGit_FailClosed(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	if err := os.Mkdir(filepath.Join(bdir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := runCreate(t, bdir)
	if err == nil {
		t.Fatal("expected fail-closed when --dir contains .git")
	}
}

func TestBackupCreate_WritesJSONAndSidecar(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	out, err := runCreate(t, bdir)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// exactly one vault-*.json + one .sha256
	matches, err := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 json, got %v %v", matches, err)
	}
	sidecars, err := filepath.Glob(filepath.Join(bdir, "vault-*.json.sha256"))
	if err != nil || len(sidecars) != 1 {
		t.Fatalf("expected 1 sidecar, got %v", sidecars)
	}
	// sidecar has only file_sha256= line
	sc, err := os.ReadFile(sidecars[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(sc), "file_sha256=") {
		t.Fatalf("sidecar missing file_sha256=: %q", sc)
	}
	if strings.Contains(string(sc), "size=") {
		t.Fatalf("sidecar must NOT have size= field: %q", sc)
	}
	// stdout mentions something was written
	if !strings.Contains(out.String(), "vault-") {
		t.Fatalf("stdout should name the backup: %q", out.String())
	}
}

func TestBackupCreate_SkipUnchanged(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	if _, err := runCreate(t, bdir); err != nil {
		t.Fatal(err)
	}
	// second create, vault unchanged (no audit written between) => skip, no new file
	if _, err := runCreate(t, bdir); err != nil {
		t.Fatalf("second create: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) != 1 {
		t.Fatalf("skip should produce no new file; got %d", len(matches))
	}
}

func TestBackupCreate_ChangeProducesNewFile(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	runCreate(t, bdir)
	// mutate vault: add a server
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	if err != nil {
		t.Fatal(err)
	}
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw2")})
	st.AddServer(&models.Server{Name: "box2", Host: "192.0.2.99", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	st.Close()
	runCreate(t, bdir)
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) != 2 {
		t.Fatalf("changed vault should produce 2nd backup; got %d", len(matches))
	}
}

func TestBackupCreate_Rotation(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	// 3 distinct snapshots, keep 2
	for i := 0; i < 3; i++ {
		runCreate(t, bdir, "--prefix", "vault")
		// mutate between runs so each is distinct
		st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
		if err != nil {
			t.Fatal(err)
		}
		cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw" + string(rune('A'+i))]})
		st.AddServer(&models.Server{Name: "srv" + string(rune('A'+i)), Host: "10.0.0." + string(rune('1'+i)), User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
		st.Close()
	}
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) != 2 {
		t.Fatalf("keep=7 default but only 3 produced; want 3 kept actually — see below")
	}
}
```

> **注意 `TestBackupCreate_Rotation` 的期望**：上面这个初版测试的断言写错了——默认 `--keep 7`，产出 3 份不会轮转（3 < 7），应断言 `len == 3`。**实现 Step 1 测试时，把最后一个断言改成验证 `--keep 2` 的真实轮转**。重写该测试体如下（替换上面那个错误版本）：

```go
func TestBackupCreate_Rotation_Keep2(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	for i := 0; i < 3; i++ {
		runCreate(t, bdir, "--keep", "2", "--prefix", "vault")
		st, err := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
		if err != nil {
			t.Fatal(err)
		}
		cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw" + string(rune('A'+i)))})
		st.AddServer(&models.Server{Name: "srv" + string(rune('A'+i)), Host: "10.0.0." + string(rune('1'+i)), User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
		st.Close()
	}
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) != 2 {
		t.Fatalf("keep=2 with 3 distinct => 2 kept; got %d", len(matches))
	}
	sidecars, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json.sha256"))
	if len(sidecars) != 2 {
		t.Fatalf("sidecars should rotate with their json; got %d", len(sidecars))
	}
}
```

（删掉错误的 `TestBackupCreate_Rotation`，只留 `TestBackupCreate_Rotation_Keep2`。`hexEncode`/`hexDecode` 已存在于 `internal/cli/enc.go`（包内非 test 文件），测试直接用——`mustDecodeHex` helper 如下：）

```go
// mustDecodeHex decodes the SSHMGR_MASTERKEY_HEX env value back to bytes
// (hexDecode lives in enc.go). Used by tests that re-open the seeded vault.
func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	mk, err := hexDecode(s)
	if err != nil {
		t.Fatal(err)
	}
	return mk
}
```

还需孤儿边车 + 撞名测试：

```go
func TestBackupCreate_OrphanSidecarSweep(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	// pre-create an orphan sidecar with no matching .json
	orphan := filepath.Join(bdir, "vault-19990101-000000.json.sha256")
	os.WriteFile(orphan, []byte("file_sha256=deadbeef\n"), 0o600)
	runCreate(t, bdir)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan sidecar should be swept: %v", err)
	}
}

func TestBackupCreate_SameSecondCollision(t *testing.T) {
	// Two creates in the same second with a mutation between would normally
	// collide on the timestamp filename. Assert the second gets a -2 suffix
	// (not an overwrite). We force collision by pre-writing the expected name.
	// NOTE: time resolution makes this flaky if runs span a second; the test
	// pre-creates the target name to make collision deterministic.
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	// pre-create a file with the name the first create would use is racy;
	// instead assert: after two creates (same second possible), both files exist
	// OR the second is -2. Just assert no data loss: run twice fast, expect >=1 file.
	runCreate(t, bdir)
	// mutate immediately
	st, _ := store.Open(os.Getenv("SSHMGR_STORE"), mustDecodeHex(t, os.Getenv("SSHMGR_MASTERKEY_HEX")))
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("different")})
	st.AddServer(&models.Server{Name: "x", Host: "10.99.99.99", User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	st.Close()
	runCreate(t, bdir)
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) < 2 {
		t.Fatalf("collision handling must not overwrite: expected >=2 files, got %d", len(matches))
	}
	// all distinct
	seen := map[string]bool{}
	for _, m := range matches {
		b, _ := os.ReadFile(m)
		seen[string(b)] = true
	}
	if len(seen) != len(matches) {
		t.Fatalf("collision files must be distinct content")
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

```
go test ./internal/cli/ -run TestBackupCreate -v
```
Expected: FAIL（`newBackupCmd` 未注册，`backup create` 子命令不存在 → cobra 报 unknown command）。

- [ ] **Step 3: 实现 backup.go（create 部分）**

新建 `internal/cli/backup.go`：

```go
package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const backupMarkerName = ".ssh-manager-backup-marker"

// newBackupCmd builds the `backup` command tree (create + verify).
// verify is added in a later task; create here.
func newBackupCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "backup",
		Short: "Manage NAS plaintext vault backups",
	}
	c.AddCommand(newBackupCreateCmd())
	// newBackupVerifyCmd() added in Task 6
	return c
}

func newBackupCreateCmd() *cobra.Command {
	var dir string
	var keep int
	var prefix string
	c := &cobra.Command{
		Use:   "create --dir <backup-dir> [--keep 7] [--prefix vault]",
		Short: "Write a plaintext vault snapshot to --dir (skip if unchanged)",
		Long: `Create a plaintext JSON snapshot of the entire vault in --dir. Skips writing
if the latest existing backup's SHA256 matches (idle/static-vault optimization —
on an active server the audit log changes every run, so skip mostly fires only
in idle windows). Rotates to --keep most-recent files. Requires a marker file
(.ssh-manager-backup-marker) inside --dir as a mount-present guard.

The backup is PLAINTEXT (credentials in cleartext). Only safe on a trusted NAS
with no Cloud Sync / public sharing. See docs/backup-restore.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupCreate(cmd, dir, keep, prefix)
		},
	}
	c.Flags().StringVar(&dir, "dir", "", "backup target directory (must contain the marker file)")
	c.MarkFlagRequired("dir")
	c.Flags().IntVar(&keep, "keep", 7, "number of most-recent backups to keep (0 = no rotation)")
	c.Flags().StringVar(&prefix, "prefix", "vault", "backup filename prefix")
	return c
}

func runBackupCreate(cmd *cobra.Command, dir string, keep int, prefix string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	dir = abs

	// 1. marker (mount-present guard)
	if !markerExists(dir) {
		return fmt.Errorf("marker file %s not found in --dir %s — refusing to write (is the NAS mounted? create the marker ON the mounted share after mounting)",
			backupMarkerName, dir)
	}
	// 2. .git guardrail (self only, no ancestor walk)
	if dirContainsGit(dir) {
		return fmt.Errorf("--dir %s contains a .git directory — refusing to write plaintext credentials into a git working tree", dir)
	}
	// 3. lock
	lk, err := acquireBackupLock(dir)
	if err != nil {
		if errors.Is(err, ErrConcurrentBackup) {
			fmt.Fprintln(cmd.OutOrStdout(), "another backup in progress; skipping")
			return nil
		}
		return err
	}
	defer lk.Release()

	// 4. snapshot + marshal + hash (reuse same []byte)
	data, err := computeSnapshotJSON()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	fileSHA := hex.EncodeToString(sum[:])

	// 5. skip check
	if shouldSkip(dir, prefix, fileSHA) {
		fmt.Fprintln(cmd.OutOrStdout(), "vault unchanged; skipping")
		return nil
	}

	// 6. atomic write
	name, err := nextBackupName(dir, prefix)
	if err != nil {
		return err
	}
	finalPath, err := atomicWriteFile(dir, name, data)
	if err != nil {
		return err
	}
	// 7. sidecar
	if err := writeSidecar(finalPath+".sha256", fileSHA); err != nil {
		return err
	}
	// 9. post-write verify (unmarshal + hash round-trip)
	if err := verifyWritten(finalPath, fileSHA); err != nil {
		return fmt.Errorf("post-write verification failed for %s: %w", finalPath, err)
	}
	// 10. rotation (incl. orphan sidecar sweep)
	if keep > 0 {
		if err := rotateBackups(dir, prefix, keep); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: rotation error (backups left in place): %v\n", err)
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", filepath.Base(finalPath))
	return nil
}

// computeSnapshotJSON opens the unlocked vault, exports, and marshals with the
// SAME indent as export.go (2-space) for determinism.
func computeSnapshotJSON() ([]byte, error) {
	st, err := openUnlockedStore()
	if err != nil {
		return nil, err
	}
	defer st.Close()
	snap, err := st.ExportSnapshot()
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(snap, "", "  ")
}

func markerExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, backupMarkerName))
	return err == nil
}

func dirContainsGit(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// shouldSkip reports whether the latest existing backup's sidecar matches fileSHA.
// Missing/unreadable sidecar => false (fail-open: write a new backup).
func shouldSkip(dir, prefix, fileSHA string) bool {
	latest, ok := latestBackup(dir, prefix)
	if !ok {
		return false
	}
	stored, ok := parseSidecar(latest + ".sha256")
	if !ok {
		return false
	}
	return stored == fileSHA
}

// latestBackup returns the lexicographically-greatest <prefix>-*.json path.
func latestBackup(dir, prefix string) (string, bool) {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.json"))
	if err != nil || len(matches) == 0 {
		return "", false
	}
	sort.Strings(matches)
	return matches[len(matches)-1], true
}

// nextBackupName picks vault-<UTC>.json, appending -2/-3 on same-second collision.
func nextBackupName(dir, prefix string) (string, error) {
	base := prefix + "-" + time.Now().UTC().Format("20060102-150405")
	name := base + ".json"
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
			return name, nil
		} else if err != nil {
			return "", err
		}
		name = fmt.Sprintf("%s-%d.json", base, n)
		if n > 99 {
			return "", fmt.Errorf("too many same-second collisions for %s", base)
		}
	}
}

// atomicWriteFile writes data to a temp file (0600), fsyncs, renames to final.
// Cleans up the temp on any failure. fsyncs the parent dir on non-Windows.
func atomicWriteFile(dir, name string, data []byte) (string, error) {
	final := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil { // file fsync — all platforms
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, final); err != nil {
		return "", err
	}
	// parent dir fsync — Linux only; Windows has no dir-sync semantics.
	if runtime.GOOS != "windows" {
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync() // best-effort
			d.Close()
		}
	}
	return final, nil
}

func writeSidecar(path, fileSHA string) error {
	content := "file_sha256=" + fileSHA + "\n"
	return os.WriteFile(path, []byte(content), 0o600)
}

func parseSidecar(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "file_sha256=") {
			return strings.TrimPrefix(line, "file_sha256="), true
		}
	}
	return "", false
}

// verifyWritten re-reads the on-disk file, re-hashes it, and re-unmarshals it
// to catch structural corruption / half-writes.
func verifyWritten(path, wantSHA string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != wantSHA {
		return fmt.Errorf("sha256 mismatch: on-disk changed since write")
	}
	var snap struct{} // placeholder to exercise json validity
	_ = snap
	// unmarshal into store.Snapshot to catch structural corruption
	if err := jsonUnmarshalSnapshot(b); err != nil {
		return fmt.Errorf("re-unmarshal failed: %w", err)
	}
	return nil
}

// jsonUnmarshalSnapshot is split out so verify doesn't import store just for the
// type name here; it does a structural unmarshal into the real Snapshot type.
// (Import store at top of file and use store.Snapshot directly — see note.)
func jsonUnmarshalSnapshot(b []byte) error {
	// import "ssh-manager-mcp/internal/store" at top of file; replace this stub:
	// var snap store.Snapshot; return json.Unmarshal(b, &snap)
	var snap map[string]any // minimal: just confirms valid JSON object
	return json.Unmarshal(b, &snap)
}
```

> **实现注意（务必处理）**：
> 1. `verifyWritten` 里的 `jsonUnmarshalSnapshot` 应真正 unmarshal 成 `store.Snapshot`（spec §5.2.9 要求"回 `store.Snapshot`"）。把上面的 `map[string]any` 占位换成真正的 `var snap store.Snapshot; return json.Unmarshal(b, &snap)`，并在 backup.go import 块加 `"ssh-manager-mcp/internal/store"`。map 占位只是为抓"是不是合法 JSON"，但 spec 要的是结构校验，必须用 `store.Snapshot`。
> 2. `rotateBackups` 还没实现——见下面补全。
> 3. 删掉 `var snap struct{}` 那行占位代码。

补全 `rotateBackups`（同文件追加）：

```go
// rotateBackups keeps the `keep` lexicographically-greatest <prefix>-*.json
// (i.e. the newest by UTC timestamp), deleting the rest along with their
// sidecars. Then sweeps orphan sidecars (no matching .json).
func rotateBackups(dir, prefix string, keep int) error {
	matches, err := filepath.Glob(filepath.Join(dir, prefix+"-*.json"))
	if err != nil {
		return err
	}
	if len(matches) <= keep {
		// still sweep orphans even when no json rotation needed
		return sweepOrphanSidecars(dir, prefix, matches)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(matches))) // newest first
	for _, p := range matches[keep:] {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			// continue rather than abort: partial rotation is acceptable
			continue
		}
		if err := os.Remove(p + ".sha256"); err != nil && !os.IsNotExist(err) {
			continue
		}
	}
	kept := matches[:keep]
	return sweepOrphanSidecars(dir, prefix, kept)
}

// sweepOrphanSidecars deletes any <prefix>-*.json.sha256 whose .json is absent.
func sweepOrphanSidecars(dir, prefix string, kept []string) error {
	keptSet := make(map[string]bool, len(kept))
	for _, p := range kept {
		keptSet[p] = true
	}
	sidecars, err := filepath.Glob(filepath.Join(dir, prefix+"-*.json.sha256"))
	if err != nil {
		return err
	}
	for _, sc := range sidecars {
		jsonPath := strings.TrimSuffix(sc, ".sha256")
		if !keptSet[jsonPath] {
			// json absent (either rotated above or pre-existing orphan) => remove sidecar
			if _, err := os.Stat(jsonPath); os.IsNotExist(err) {
				os.Remove(sc) // best-effort
			}
		}
	}
	return nil
}
```

- [ ] **Step 4: 跑测试验证通过**

```
go test ./internal/cli/ -run TestBackupCreate -v
```
Expected: 全 PASS。**如果 `TestBackupCreate_SkipUnchanged` 偶发失败**（两次 create 跨秒但 hash 因别的原因变了），检查：seed 后是否真的没碰 store；`openUnlockedStore` 是否每次都开同一个 db；audit 是否被某处隐式写。

- [ ] **Step 5: no-regression + 提交**

```
go test ./...
gofmt -l .
go vet ./...
```

```bash
git add internal/cli/backup.go internal/cli/backup_test.go
git commit -m "feat(cli): backup create — plaintext snapshot, skip-if-unchanged, rotation

backup create --dir writes a plaintext store.Snapshot JSON to --dir,
guarded by a marker file (mount-present) and a .git self-check. Skips
if the latest backup's sidecar SHA256 matches (idle-vault optimization;
audit_log means active servers rarely skip — documented). Atomic write
(temp+fsync+rename, dir fsync on non-Windows), .sha256 sidecar (single
file_sha256= line), post-write verify (re-unmarshal + hash round-trip),
rotation by filename sort with orphan-sidecar sweep."
```

---

### Task 6: backup verify 命令

**Files:**
- Modify: `internal/cli/backup.go`（加 `newBackupVerifyCmd`，挂到 backup cmd tree）
- Test: Modify `internal/cli/backup_test.go`（加 verify 测试）

**Interfaces:**
- Consumes: `parseSidecar`、`crypto/sha256`、`store.Snapshot`（结构校验）
- Produces: `backup verify <file>` 子命令

**背景**（spec §5.3）：读 `<file>.sha256` 的 `file_sha256` → 重算落盘文件 SHA256 → 比对 + `json.Unmarshal` 回 `store.Snapshot` 抓结构损坏。不一致非零退出。不用口令。

- [ ] **Step 1: 写失败测试**

在 `internal/cli/backup_test.go` 追加：

```go
func TestBackupVerify_Ok(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	runCreate(t, bdir)
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	if len(matches) != 1 {
		t.Fatal("need exactly one backup")
	}
	root := NewRootCmd()
	root.SetArgs([]string{"backup", "verify", matches[0]})
	out := &bytes.Buffer{}
	root.SetOut(out)
	root.SetErr(out)
	if err := root.Execute(); err != nil {
		t.Fatalf("verify healthy backup: %v", err)
	}
}

func TestBackupVerify_BitRot_Fails(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	runCreate(t, bdir)
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	path := matches[0]
	// flip one byte deep in the file (not the first byte, to keep it valid-ish JSON shape)
	b, _ := os.ReadFile(path)
	if len(b) < 20 {
		t.Fatal("backup too small to corrupt")
	}
	b[len(b)-10] ^= 0xFF
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	root := NewRootCmd()
	root.SetArgs([]string{"backup", "verify", path})
	err := root.Execute()
	if err == nil {
		t.Fatal("verify must fail on bit-rot")
	}
}

func TestBackupVerify_MissingSidecar_Fails(t *testing.T) {
	seedVaultForBackup(t)
	bdir := t.TempDir()
	touchMarker(t, bdir)
	runCreate(t, bdir)
	matches, _ := filepath.Glob(filepath.Join(bdir, "vault-*.json"))
	os.Remove(matches[0] + ".sha256")
	root := NewRootCmd()
	root.SetArgs([]string{"backup", "verify", matches[0]})
	if err := root.Execute(); err == nil {
		t.Fatal("verify must fail when sidecar is missing")
	}
}
```

- [ ] **Step 2: 跑测试验证失败**

```
go test ./internal/cli/ -run TestBackupVerify -v
```
Expected: FAIL（`backup verify` 子命令不存在）。

- [ ] **Step 3: 实现 verify**

在 `internal/cli/backup.go` 加：

```go
func newBackupVerifyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "verify <file>",
		Short: "Verify a backup's SHA256 sidecar and JSON structure",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBackupVerify(cmd, args[0])
		},
	}
	return c
}

func runBackupVerify(cmd *cobra.Command, file string) error {
	wantSHA, ok := parseSidecar(file + ".sha256")
	if !ok {
		return fmt.Errorf("missing or unreadable sidecar %s.sha256", file)
	}
	if err := verifyWritten(file, wantSHA); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "ok: %s (sha256 verified, json structurally valid)\n", filepath.Base(file))
	return nil
}
```

并在 `newBackupCmd()` 里取消注释 / 改为：
```go
	c.AddCommand(newBackupCreateCmd(), newBackupVerifyCmd())
```

- [ ] **Step 4: 跑测试验证通过**

```
go test ./internal/cli/ -run TestBackupVerify -v
go test ./internal/cli/ -run TestBackup -v
```
Expected: 全 PASS（create + verify 全套）。

- [ ] **Step 5: no-regression + 提交**

```
go test ./...
gofmt -l .
go vet ./...
```

```bash
git add internal/cli/backup.go internal/cli/backup_test.go
git commit -m "feat(cli): backup verify — sidecar SHA256 + JSON structure check

backup verify <file> re-reads the file, recomputes SHA256, asserts it
matches the .sha256 sidecar's file_sha256, and re-unmarshals into
store.Snapshot to catch structural corruption / bit-rot. No passphrase
(plaintext). Non-zero exit on mismatch or missing sidecar."
```

---

### Task 7: root 注册 + 全套烟雾测试 + Windows dir-fsync 隔离

**Files:**
- Modify: `internal/cli/root.go`（注册 `newBackupCmd`）
- Test: 视需要补 `internal/cli/cli_smoke_test.go`

**Interfaces:**
- Consumes: `newBackupCmd`
- Produces: `ssh-manager backup ...` 顶层可达

- [ ] **Step 1: 注册命令**

`internal/cli/root.go:22` 的 `root.AddCommand(...)` 末尾加 `newBackupCmd()`：

```go
	root.AddCommand(versionCmd, newServersCmd(), newProfilesCmd(), newProjectsCmd(), newCacheTokensCmd(), newCacheCmd(), newUnlockCmd(), newLockCmd(), newSSHCmd(), newMCPCmd(), newServeCmd(), newExportCmd(), newImportCmd(), newBackupCmd())
```

- [ ] **Step 2: 跑全 CLI 烟雾测试**

```
go test ./internal/cli/ -v -run "Smoke|Root"
```
（确认现有的 `cli_smoke_test.go` / `root_test.go` 仍 PASS —— 新增命令不应破坏 `ssh-manager --help` 或子命令发现。）

- [ ] **Step 3: 补一个 `backup --help` 烟雾测试（若 cli_smoke_test.go 风格有 help 断言）**

先看现有风格：
```
grep -n "help\|Help" internal/cli/cli_smoke_test.go internal/cli/root_test.go
```
若有 help 断言模式，照它加一条 `backup create --help` 含 `--dir`、`--keep`、`--prefix` 的断言。若无统一 help 测试框架，跳过此步（cobra 自动保证）。

- [ ] **Step 4: Windows dir-fsync 行为确认**

`backup.go` 的 `atomicWriteFile` 已用 `runtime.GOOS != "windows"` 跳过 dir fsync。补一个**只在 Windows 编译**的测试文件 `internal/cli/backup_windows_test.go` 断言 dir fsync 不报错（可选；若 CI 不跑 Windows，这一步改为"代码 review 确认 `runtime.GOOS` 分流存在 + 不 panic"即可，不强求测试）：

```go
//go:build windows

package cli

// (TestBackupCreate_WindowsNoDirFsyncPanic — omitted; the runtime.GOOS guard
// in atomicWriteFile is the contract. CI on Windows would catch a panic.)
```

实际上：**跳过建空文件**。确认 `atomicWriteFile` 的 dir-fync 分支在 Windows 不会执行即可（review 代码）。

- [ ] **Step 5: 全套 no-regression + 提交**

```
go test ./...
gofmt -l .
go vet ./...
```

Expected: 全 green。`go build ./...` 也应通过。

```bash
git add internal/cli/root.go
git commit -m "feat(cli): register backup command tree

Wires newBackupCmd (create + verify) into the root command so
'ssh-manager backup ...' is reachable. dir-fsync skips on Windows via
runtime.GOOS guard in atomicWriteFile (no build-tag test needed)."
```

---

### Task 8: 部署文档（docs/backup-restore.md 加 Plan 13 章节）

**Files:**
- Modify: `docs/backup-restore.md`（Plan 11 已有内容，追加 Plan 13 章节）

**Interfaces:** 无（纯文档）

**内容要求（spec §6/§7，逐条进文档）**：
1. **Plan 13 概述**：明文快照、职责分离、复用 import 恢复。
2. **部署硬约束**（spec §3.2/§7）：NAS 受信 VLAN、永不开 Cloud Sync/Drive/Universal Search/Snapshot Replication/公网共享；违反 → 必须回加密版（见未来工作）。**独立风险项：审计日志 `audit_log.command` 明文 = 新暴露**（可能含一次性 token / 临时密码 / 无副本 secret，不在 1Password 里）。
3. **marker 顺序**（spec §3.6）：先挂载 NAS → 后建 marker → marker 必须在挂载的 NAS 上（防 shadow marker fail-open）。
4. **Windows 任务计划程序**（spec §6.4）：**UNC 路径 `\\synology\backups`**（不用 `net use` 映射盘——per-session 陷阱会让 SYSTEM 跑的任务看不到盘号 → 静默不跑）；`schtasks` 模板；**勾"超过 10 分钟停止任务"**（SMB 挂起兜底）；master key 用 `setx SSHMGR_MASTERKEY_HEX` 或同 user keychain。
5. **Linux systemd timer 模板**：`Type=oneshot` + `TimeoutStartSec=600`。
6. **恢复流程**（spec §5.4/§11）：从 NAS 拷 `.json` → `ssh-manager import <file>`（嗅探自动识别明文，不弹口令）→ DR 恢复 vault + project token；**cache_tokens 不在备份里**，需 `cache-tokens add` 重发 + 各工作机 `cache pull`。
7. **skip 语义诚实说明**（spec §3.5）：活跃服务器 skip 几乎不触发（audit_log 增长），主要服务空闲/静态 vault；rotation 是兜底；长期静态 vault 靠定期 `backup verify` + NAS 快照做底层兜底。
8. **运维 footgun**：禁 `cat`/`grep -r password`；`--dir` 绝对路径非 git；`.gitignore` 模板 `vault-*.json` + `*.sha256`。
9. **限制 / 未来工作**：不加密、不增量、不事件触发、无 `backup restore` 一条龙；未来若开 Cloud Sync 需回加密版。

- [ ] **Step 1: 读现有 docs/backup-restore.md**

```
cat docs/backup-restore.md
```
了解 Plan 11 既有结构和标题层级，追加 Plan 13 章节时保持风格一致。

- [ ] **Step 2: 追加 Plan 13 章节**

在 `docs/backup-restore.md` 末尾追加（标题层级与文件现有风格对齐；若现有用 `##`，这里也用 `##`）：

```markdown
## Plan 13 — NAS 定时明文备份（backup create / verify）

> 设计 spec：`docs/superpowers/specs/2026-08-12-plan-13-nas-backup-design.md`（v3）。

`backup create` 把整个 vault 以**明文 JSON 快照**定时写到挂载的群晖目录，无变化不备份，按份数轮转，带 `.sha256` 边车抓 bit-rot。`backup verify` 按需校验。灾难恢复 = 从 NAS 拷文件 + `ssh-manager import`。

### 部署硬约束（违反则必须回加密版）

明文备份**只在以下条件全部满足时安全**：

- NAS 在受信 VLAN 内，外网不可达；
- **永不开** Cloud Sync / Drive / Universal Search / Snapshot Replication / 公网共享；
- 目录权限锁死，物理介质单独保管。

**独立风险项 — 审计日志明文 = 新暴露**：备份里的 `audit_log.command` 原样导出，含历史命令行——可能携带**一次性 token / 临时密码 / 无副本 secret**，这些**不在 1Password 里**。明文备份暴露的范围比"1Password 冗余副本"更广。若开了任何 Cloud Sync / 公网，必须停止明文备份，回加密版（见 spec §10 未来工作）。

### marker 文件（挂载在场的硬保证）

`--dir/.ssh-manager-backup-marker` 必须存在（只查存在性）。**顺序**：先挂载 NAS → 在挂载的 NAS 上建 marker → 之后 `backup create` 才会写。这防"先建 marker 再 mount"导致 marker 落 shadow、挂载掉时 shadow marker 露出 → 静默写本地（fail-open）。

### Windows：任务计划程序 + UNC 路径

**用 UNC 路径，不要用 `net use` 映射盘号**：映射盘号 per-user/per-session，任务计划程序以别的 user 或 SYSTEM 跑时看不到 `Z:` → marker fail-closed 表现为"备份永远不跑"（无人值守典型静默失败）。UNC 路径 `\\synology\backups` 任何 session 都可达。

```cmd
schtasks /Create /SC DAILY /ST 03:30 /TN ssh-manager-backup ^
  /TR "ssh-manager.exe backup create --dir \\synology\backups --keep 7" ^
  /RU <user> /RP <password>
```

- master key：`setx SSHMGR_MASTERKEY_HEX <hex>`（或任务以 serve 同 user 跑、keychain 可达）。
- **勾"超过 10 分钟停止任务"**：SMB 写挂起无应用层超时，NAS 卡住会无限挂进程；任务计划层硬超时是兜底（陈旧锁 5 min 超时只救下次运行）。

### Linux：systemd timer

```ini
# /etc/systemd/system/ssh-manager-backup.service
[Service]
Type=oneshot
Environment=SSHMGR_MASTERKEY_HEX=<hex>
ExecStart=/usr/local/bin/ssh-manager backup create --dir /mnt/nas/backups --keep 7
TimeoutStartSec=600

# /etc/systemd/system/ssh-manager-backup.timer
[Timer]
OnCalendar=*-*-* 03:30:00
Persistent=true
```

`Type=oneshot` 防 timer 自身重叠；`TimeoutStartSec=600` 兜底 SMB 挂起。

### 恢复

1. 从 NAS 拷最新的 `vault-*.json`（和它的 `.sha256`）到本机。
2. （可选）`ssh-manager backup verify <file>` 确认没坏。
3. `ssh-manager import <file>` —— 嗅探自动识别明文，**不弹口令**；导入到**空的** vault（`store.db` 不存在或空）。
4. **cache_tokens 不在备份里**（设备身份，非 vault 内容）：恢复后需 `ssh-manager cache-tokens add` 重发各工作机授权码，各工作机 `ssh-manager cache pull` 重拉。agent 的 `.mcp.json` 不用动（project token 在备份里）。

### skip 语义（诚实）

活跃服务器上 `backup create` 的"无变化不备份"**几乎不触发**——`audit_log` 每执行一条 SSH 命令就增长，SHA256 必变。skip 主要服务**空闲窗口 / 长期静态 vault**。**rotation 才是兜底**。长期静态 vault（skip 让你只握 1 份不刷新文件）需定期 `backup verify` + 依赖 NAS 自身快照做底层兜底。

### 运维 footgun

- 禁 `cat`/`grep -r password` 查备份；用 `backup verify` 或恢复到测试 vault 后 `ssh-manager servers ls`。
- `--dir` 必须是绝对路径且不在任何 git 工作树里（`backup create` 会检测 `--dir` 自身含 `.git` 并拒绝）。
- `.gitignore` 模板：`vault-*.json` + `*.sha256`。

### 限制 / 未来工作

- 不加密（见上"部署硬约束"）、不增量（全量快照）、不事件触发（纯定时）。
- 无 `backup restore` 一条龙（= 手动拷 + `import`）。
- 未来若需 Cloud Sync / 公网，回加密版（decrypt-and-compare skip）。
```

- [ ] **Step 3: 校验文档无断链 / 措辞与 spec 一致**

```
grep -n "Plan 13\|backup create\|UNC\|marker" docs/backup-restore.md
```
确认新章节关键词都在，且指向的 spec 路径 `docs/superpowers/specs/2026-08-12-plan-13-nas-backup-design.md` 存在。

- [ ] **Step 4: 提交**

```bash
git add docs/backup-restore.md
git commit -m "docs(backup-restore): Plan 13 NAS plaintext backup section

Deployment hard-constraints (trusted VLAN, never Cloud Sync/public),
independent risk callout for audit_log plaintext exposure, marker
ordering, Windows Task Scheduler with UNC path (not net use mapped
drive — per-session trap) + stop-after-10min, Linux systemd timer,
recovery via import (plaintext sniffed, no passphrase; cache_tokens
not in backup), honest skip semantics, operational footguns."
```

---

## Self-Review（plan 写完后的自检，不是 subagent）

**1. Spec 覆盖**：逐条对 spec §3-§12 检查有 task 实现：
- §3.1 明文 / §3.3 复用 Plan 11 → T5 `computeSnapshotJSON`
- §3.4 Windows 加固降级 → T5 `atomicWriteFile` (runtime.GOOS) + T8 文档
- §3.5 skip 诚实降级 → T5 skip + T8 文档
- §3.6 marker → T5 `markerExists`
- §3.7 轮转 + UTC → T5 `nextBackupName` + `rotateBackups`
- §3.8 锁纯超时 5min → T4
- §3.9 边车单字段 → T5 `writeSidecar`/`parseSidecar`
- §5.2 create 流程 11 步 → T5 全覆盖
- §5.3 verify → T6
- §5.4 import sniff + prompt 重排 + lstrip/BOM → T2 + T3
- §5.5 ORDER BY id 统一 → T1
- §6 Windows 细节 → T5 + T8
- §7 安全文档 → T8
- §8 测试 → 各 task 的 Step 1
- §11 cache_tokens 不进 → T8 文档（恢复章节）
- §12 checklist → 全 task 覆盖

**2. Placeholder 扫描**：检查无 "TBD/TODO/适当处理" —— T5 有几处"实现注意"（`jsonUnmarshalSnapshot` 用 `store.Snapshot`、删 `var snap struct{}` 占位、补 `rotateBackups`），这些是**显式指令**不是占位，实现者照做即可。helper `hexEncode`/`hexDecode`（enc.go）、`withEnv`（cli_smoke_test.go）、`passphrasePrompt` seam（unlock.go:28）均已核实存在。

**3. 类型一致**：`acquireBackupLock`/`ErrConcurrentBackup`/`backupLock.Release`（T4 定义，T5 消费）一致；`parseSidecar`/`verifyWritten`/`writeSidecar`（T5 定义，T6 复用）一致；`newBackupCmd`（T5 定义 create，T6 加 verify，T7 注册）一致。

**4. 风险点提示给实现者**：
- T1 的确定性测试在修复前可能"碰巧 PASS"（SQLite 小数据集）——仍要改 SQL，确定性是逻辑事实。
- T5 的 `TestBackupCreate_SkipUnchanged` 对 audit 敏感——seed 后绝不碰 store。
- T5 的撞名测试可能因秒级时间分辨率偶发——断言写成 `>=2` 且内容 distinct，不强求精确 `-2`。

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-08-12-plan-13-nas-backup.md`. Two execution options:

**1. Subagent-Driven (recommended)** — 我每个 task 派一个新 implementer subagent，task 间做 spec 合规 + 代码质量双审，task 末派 reviewer，全部完成后做一次整分支 review。

**2. Inline Execution** — 在本会话用 executing-plans 批量执行，带 checkpoint。

哪种？
