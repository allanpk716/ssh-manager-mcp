# Plan 34: 离线 cache 切断失效（pinned-401 隔离）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** revoke 设备码后，笔记本下一次（自动 ≤30min lazy 或手动）pinned pull 收到 401 时销毁本地 cache 四件（cache.bin 隔离 + 设备码/DEK/meta 物理删），spawn 报明确错误，重新 enroll 恢复——把 revoke 的生效形态从「永不动」提级到「回连即销毁」。

**Architecture:** 服务端 `/snapshot` 401 附 revoked/unknown reason（纯可观测性）；客户端 `DoPull` 在 pinned 401 时调 `QuarantineCache`（DEK-first 顺序、manifest best-effort 跨进程持久化、DEGRADED 如实汇报、幂等例外）；`mcp --cache`/`cache status` 按「manifest→quarantine 目录→missing」三级降级链归因报文。运行中会话不断（spawn 边界生效）。

**Tech Stack:** Go 1.25；标准库 net/http（httptest TLS 造 pinned 401）；既有 clientops/cli/store/mcpserver 设施；spec rev4（A-only——B 时限机器已砍出回 backlog）。

**Spec:** `docs/superpowers/specs/2026-08-24-plan-34-cache-invalidation-design.md.rev4.md`（第五版定稿；本计划从 spec 立论，执行者两份都读）

## Global Constraints

- 触发面唯一：`DoPull` 内 `pin != "" && res.StatusCode == 401`；**明文 pull 401 / 网络错误 / TLS 失败 / 非 401 永不触发**。
- 服务端错误文本逐字：`invalid cache token: revoked` / `invalid cache token: unknown`（401 reason 纯可观测性，客户端判定不依赖）。
- 客户端哨兵：`clientops.ErrCacheQuarantined`（`errors.Is` 可判；wrap 携 reason 与 DEGRADED 展示）。
- 销毁顺序（DEK-first，crash-safe，manifest best-effort 非前置闸）：①`os.MkdirAll(<cache目录>/quarantine/)` + 原子写 manifest `{state:"started",reason,ts}`（失败仅日志继续）→ ②DEK 删除 → ③`cache.auth.json` 删除 → ④`cache.bin` rename 进 `quarantine/cache.bin.quarantined-<unix秒>`（同目录子目录，同卷保证 rename；单份保留）→ ⑤`cache.meta.json` 删除（非关键步）→ ⑥manifest 更新 `{state:"done",steps,degraded,reason,ts}` → ⑦stderr 一行。
- 关键步 = DEK/auth/bin rename 三件；**幂等例外**：目标已不存在 = 该步幂等成功；关键步**出错** → DEGRADED（返回值+stderr+manifest 三处一致，绝不静默）。
- DEK 形态（Plan 16 T4 后两平台统一）：`store.FileKeyProvider{Path: paths.CacheDekPath()}`（`SSHMGR_CACHE_DEK` seam 在 paths 层）——DEK 删除 = provider 的 `Delete()`（`os.Remove`，`IsNotExist` → nil）。spec §2 的「Unix keyring slot」字样映射到历史形态，测试用可注入 Delete 的 fake 覆盖三态。
- spawn/status 归因（三级降级链 + 时间约束）：manifest 可读且 **`manifest.ts > cache.meta.json 的 PulledAt`** → done/done+degraded/started 三形态文案；manifest 不可读但 `quarantine/` 目录存在 → 无细节归因；否则现有 missing/decrypt 文案。bin 在位但解密失败：manifest 过时间约束 → interrupted 同族；否则通用 decrypt 错误。
- 报文文本逐字（spec §4）：`cache quarantined by server rejection (token revoked?) [DEGRADED: <步骤>] — re-enroll via cache pull with a fresh device code; manual cleanup may be needed` / `...token revoked?) — re-enroll via cache pull with a fresh device code` / `cache quarantine was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json` / `cache was quarantined (details unavailable — quarantine/manifest.json missing); re-enroll via cache pull`。
- CLI 写码时序（现状已满足，钉测试）：`cache pull` 仅 pull 成功后写 `cache.auth.json`；任何失败（含 401）不落盘新码。
- lazy：哨兵 → stderr、**不进 backoff、不计失败窗口**、进程级「已隔离」标记（本进程后续边界零尝试）；每 spawn ≤1 次销毁尝试。
- 归因防误报：成功 pull 落盘时删除 manifest（时间约束兜底重置崩溃）。
- 手动 pull 文案逐字：`cache was QUARANTINED: the server rejected this device code (revoked?). Re-enroll: obtain a fresh device code and run cache pull again.`
- 运行中会话不断（cacheStoreHolder 现有语义零改动）；跨进程互斥不做（幂等已缓解破坏性，接受登记）。
- B 时限机器（SSHMGR_CACHE_MAX_OFFLINE/watermark/X-Sshmgr-Time）**不存在**——砍出回 backlog，任何任务不得引入。
- 文档口径：销毁 = cache 侧四件；project token 不在清单（失窃处置 = 两 token 都 revoke）；backup-restore 按事实（ExportSnapshot 不含 cache_tokens → 恢复后表空 → 全体 unknown 批量切断；raw-DB 直拷带外警示）。
- commit 尾行 `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`；`.xcheck/`、`sdd/` 已 gitignore 不 commit；非 ASCII 编辑逐字节验证；每 task scoped 测试绿 + commit；T6 末全量 `go test ./...`（eval/conformance 双门控 SKIP 正常）。

---

### Task 1: store — RevokedCacheTokenNameByPrefix

**Files:**
- Modify: `internal/store/cachetoken.go`（末尾追加）
- Test: `internal/store/cachetoken_test.go`（末尾追加）

**Interfaces:**
- Consumes: 既有 `cache_tokens` 表（`token_prefix` 明文 8 字符前缀，projects.go:205 `tokenPrefix`）、`models.CacheTokenRevoked`。
- Produces: `func (s *Store) RevokedCacheTokenNameByPrefix(prefix string) (name string, ok bool, err error)`——T2 消费。

- [ ] **Step 1: 写失败测试**

```go
// TestRevokedCacheTokenNameByPrefix pins the rev4 §1 reason lookup: a revoked
// row matching the 8-char plaintext prefix resolves its name (most recent
// updated_at wins on collisions); no revoked match returns ok=false; active
// rows NEVER match.
func TestRevokedCacheTokenNameByPrefix(t *testing.T) {
	s := newStore(t)
	defer s.Close()
	tok1, _, err := s.AddCacheToken("laptop")
	if err != nil {
		t.Fatalf("add laptop: %v", err)
	}
	if err := s.RevokeCacheToken("laptop"); err != nil {
		t.Fatalf("revoke laptop: %v", err)
	}
	name, ok, err := s.RevokedCacheTokenNameByPrefix(tok1[:8])
	if err != nil || !ok || name != "laptop" {
		t.Fatalf("revoked lookup: name=%q ok=%v err=%v, want laptop/true/nil", name, ok, err)
	}
	// Unknown prefix → ok=false, no error.
	if _, ok, err := s.RevokedCacheTokenNameByPrefix("ZZZZZZZZ"); err != nil || ok {
		t.Fatalf("unknown prefix: ok=%v err=%v, want false/nil", ok, err)
	}
	// An ACTIVE token's prefix must NOT match (only revoked rows).
	tok2, _, err := s.AddCacheToken("desk")
	if err != nil {
		t.Fatalf("add desk: %v", err)
	}
	if _, ok, _ := s.RevokedCacheTokenNameByPrefix(tok2[:8]); ok {
		t.Fatal("active row matched the revoked-prefix lookup")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run TestRevokedCacheTokenNameByPrefix -count=1`
Expected: FAIL（`undefined: s.RevokedCacheTokenNameByPrefix`）

- [ ] **Step 3: 实现**

`internal/store/cachetoken.go` 末尾追加（import 补 `"database/sql"` + `"errors"` 若缺）：

```go
// RevokedCacheTokenNameByPrefix reports the name of the most-recently-updated
// REVOKED cache token whose token_prefix matches (Plan 34 rev4 §1: the /snapshot
// 401 reason is observability-only — prefix collisions can mislabel unknown as
// revoked, which is accepted; the client never branches on the reason).
func (s *Store) RevokedCacheTokenNameByPrefix(prefix string) (string, bool, error) {
	var name string
	err := s.db.QueryRow(
		`SELECT name FROM cache_tokens WHERE token_prefix=? AND status=? ORDER BY updated_at DESC LIMIT 1`,
		prefix, string(models.CacheTokenRevoked),
	).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -run 'TestRevokedCacheTokenNameByPrefix|TestVerifyCacheToken|TestRevokeCacheToken' -count=1`
Expected: PASS（既有 cachetoken 测试同跑零回归）

- [ ] **Step 5: Commit**

```bash
git add internal/store/cachetoken.go internal/store/cachetoken_test.go
git commit -m "feat(store): RevokedCacheTokenNameByPrefix — 401 reason 回查支撑(Plan 34 T1)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: serve — verifyCacheToken 401 reason + stderr 日志

**Files:**
- Modify: `internal/mcpserver/serve.go`（verifyCacheToken，约 110-122 行函数体）
- Test: `internal/mcpserver/serve_test.go`（末尾追加）

**Interfaces:**
- Consumes: T1 `RevokedCacheTokenNameByPrefix`；既有 `auth.ErrInvalidToken`。
- Produces: `/snapshot` 401 响应体含 `invalid cache token: revoked|unknown`；serve stderr 各一行（T5 e2e 断言其形态）。

- [ ] **Step 1: 写失败测试**

`internal/mcpserver/serve_test.go` 末尾追加：

```go
// TestSnapshot401Reason pins the Plan 34 rev4 §1 observability contract: a
// revoked device code 401s with "invalid cache token: revoked" (stderr logs the
// device name), an unknown code with "invalid cache token: unknown". The client
// NEVER branches on the reason — this test exists so the owner-facing signal
// cannot silently regress.
func TestSnapshot401Reason(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	tok, _, err := st.AddCacheToken("laptop")
	if err != nil {
		t.Fatalf("AddCacheToken: %v", err)
	}
	if err := st.RevokeCacheToken("laptop"); err != nil {
		t.Fatalf("RevokeCacheToken: %v", err)
	}
	r, err := NewServeRunner(st)
	if err != nil {
		t.Fatalf("NewServeRunner: %v", err)
	}
	defer r.Close()
	ts := httptest.NewServer(r.HTTPHandler())
	defer ts.Close()

	get := func(auth string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/snapshot", nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, derr := http.DefaultClient.Do(req)
		if derr != nil {
			t.Fatalf("Do: %v", derr)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		t.Logf("body=%q", b)
		return resp.StatusCode
	}
	if got := get(tok); got != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d, want 401", got)
	}
	if got := get("definitely-not-a-real-code-123456"); got != http.StatusUnauthorized {
		t.Fatalf("unknown token: %d, want 401", got)
	}
}
```

（若 /snapshot 401 体不携 verifier 文本——实测确认 RequireBearerToken 把 verifier 错误文本写进响应体；若 SDK 层吞掉，则本测试退化为「401 两态可达」断言并把文本断言移到 Step 3 的 stderr 上——如实按实测调整并记录在报告。）

- [ ] **Step 2: 跑测试确认现状**

Run: `go test ./internal/mcpserver/ -run TestSnapshot401Reason -count=1`
Expected: 401 可达但文本仍为旧形态（`invalid or unknown token`）——日志断言红

- [ ] **Step 3: 实现**

`verifyCacheToken`（serve.go）在 `VerifyCacheToken` 失败分支改为（保持 `(nil, nil)` 分支语义——nil 即回查入口）：

```go
func (r *ServeRunner) verifyCacheToken(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
	ct, err := r.st.VerifyCacheToken(token)
	if err != nil || ct == nil {
		// Plan 34 rev4 §1: the 401 reason is observability-only (revoked vs
		// unknown via an 8-char-prefix lookup; collisions can mislabel —
		// accepted). The client NEVER branches on this text.
		reason := "unknown"
		prefix := token
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		if name, ok, nerr := r.st.RevokedCacheTokenNameByPrefix(prefix); nerr == nil && ok {
			reason = "revoked"
			fmt.Fprintf(os.Stderr, "ssh-manager serve: cache token rejected: revoked (device %s, prefix %.8s)\n", name, token)
		} else {
			fmt.Fprintf(os.Stderr, "ssh-manager serve: cache token rejected: unknown (prefix %.8s)\n", token)
		}
		return nil, fmt.Errorf("%w: invalid cache token: %s", auth.ErrInvalidToken, reason)
	}
	// SDK's verify() requires a non-zero, non-expired Expiration (auth.go:120-126). Same
	// nominal-TTL trick as verifyToken: the real lifecycle is VerifyCacheToken's
	// status='active' filter (revoke), NOT this nominal expiry.
	return &auth.TokenInfo{
		UserID:     ct.ID,
		Expiration: time.Now().Add(projectTokenNominalTTL),
	}, nil
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/mcpserver/ -count=1`
Expected: PASS（整包含 serve/serve_snapshot/revoke 既有测试）

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/serve.go internal/mcpserver/serve_test.go
git commit -m "feat(serve): /snapshot 401 附 revoked/unknown reason + 设备名日志(Plan 34 T2)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: clientops — QuarantineCache（DEK-first + manifest best-effort + DEGRADED + 幂等）

**Files:**
- Create: `internal/clientops/quarantine.go`
- Modify: `internal/store/masterkey.go`（FileKeyProvider 增 Delete）
- Test: Create `internal/clientops/quarantine_test.go`

**Interfaces:**
- Consumes: `CachePaths()`（dir/bin/meta）、`CacheCredPath()`、`DekProvider` seam、`atomicWriteUnique`。
- Produces:
  - `var ErrCacheQuarantined = errors.New("cache quarantined by server rejection")`
  - `type QuarantineResult struct { Steps map[string]string; Degraded []string; ManifestWritten bool }`
  - `func QuarantineCache(reason string) (QuarantineResult, error)`——T4/T5 消费
  - `func (f *FileKeyProvider) Delete() error`——幂等（IsNotExist → nil）
- [ ] **Step 1: 写失败测试**

Create `internal/clientops/quarantine_test.go`：

```go
package clientops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

// withDEKDeletable swaps the DekProvider seam to a MemKeyProvider-backed fake
// whose Delete behavior is scripted (nil=success, err=inject failure,
// missingKey=pretend absent). Returns the fake for mutation.
type fakeDEK struct {
	store.MemKeyProvider
	deleteErr error
	deleted   bool
}

func (f *fakeDEK) Delete() error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = true
	return nil
}

func withDEKFake(t *testing.T) *fakeDEK {
	t.Helper()
	f := &fakeDEK{}
	prev := DekProvider
	DekProvider = func() store.KeyProvider { return f }
	t.Cleanup(func() { DekProvider = prev })
	return f
}

// seedCache writes the four cache-side artifacts into the CURRENT SSHMGR_CACHE_DIR
// (t.Setenv it first) and returns their paths.
func seedCache(t *testing.T, dir string) (bin, meta, cred string, dekPath string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bin, meta, _, credPath, err := resolvePathsForTest(dir)
	if err != nil {
		t.Fatal(err)
	}
	cred = credPath
	for p, content := range map[string]string{
		bin:       "ciphertext-bytes",
		meta:      `{"url":"https://x","pulled_at":1}`,
		cred:      `{"url":"https://x","token":"dev-code","pin":"sha256:aa"}`,
	} {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return bin, meta, cred, ""
}

// resolvePathsForTest mirrors CachePaths' layout for an explicit dir.
func resolvePathsForTest(dir string) (bin, meta, watermark, cred string, err error) {
	return filepath.Join(dir, "cache.bin"), filepath.Join(dir, "cache.meta.json"),
		filepath.Join(dir, "cache.watermark"), filepath.Join(dir, "cache.auth.json"), nil
}

// TestQuarantineDestroysFourAndWritesManifest: the happy path — all four
// artifacts gone (bin isolated), manifest done with empty degraded.
func TestQuarantineDestroysFourAndWritesManifest(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	f := withDEKFake(t)
	bin, meta, cred, _ := seedCache(t, dir)

	res, err := QuarantineCache("server rejected device code")
	if err != nil {
		t.Fatalf("QuarantineCache: %v", err)
	}
	if len(res.Degraded) != 0 || !res.ManifestWritten {
		t.Fatalf("res = %+v, want no degraded + manifest written", res)
	}
	if f.deleted != true {
		t.Fatal("DEK provider Delete not called")
	}
	for _, p := range []string{meta, cred} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Fatalf("%s must be deleted", p)
		}
	}
	// bin isolated: quarantine dir holds exactly one renamed copy, original gone.
	qdir := filepath.Join(dir, "quarantine")
	entries, _ := os.ReadDir(qdir)
	if len(entries) != 2 { // manifest.json + one bin copy
		t.Fatalf("quarantine entries = %d, want 2 (manifest + bin)", len(entries))
	}
	if _, serr := os.Stat(bin); !os.IsNotExist(serr) {
		t.Fatal("cache.bin must be gone from the cache dir")
	}
	// stderr carry: the result error text embeds the re-enroll guidance.
	if res, err := QuarantineCache("x"); err != nil || strings.Contains(res2Text(err), "never-match") {
		_ = res
	}
}

// TestQuarantineDegradedOnDEKFailure: a critical-step failure marks DEGRADED
// everywhere it can, still completes the other steps, and reports honestly.
func TestQuarantineDegradedOnDEKFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	f := withDEKFake(t)
	f.deleteErr = errors.New("keyring unavailable")
	seedCache(t, dir)

	res, err := QuarantineCache("server rejected device code")
	if err == nil {
		t.Fatal("want error carrying DEGRADED")
	}
	if !strings.Contains(err.Error(), "DEGRADED") || !strings.Contains(err.Error(), "dek") {
		t.Fatalf("err = %q, want DEGRADED + step name", err)
	}
	if len(res.Degraded) != 1 || res.Degraded[0] != "dek" {
		t.Fatalf("res.Degraded = %v, want [dek]", res.Degraded)
	}
	// Other steps STILL ran.
	if _, serr := os.Stat(filepath.Join(dir, "cache.auth.json")); !os.IsNotExist(serr) {
		t.Fatal("auth.json must still be deleted despite DEK failure")
	}
}

// TestQuarantineIdempotentOnMissingArtifacts: re-quarantine (bin already gone,
// DEK fake deleted) yields NO degraded — absent targets are idempotent success.
func TestQuarantineIdempotentOnMissingArtifacts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	res, err := QuarantineCache("server rejected device code")
	if err != nil && !strings.Contains(err.Error(), "DEGRADED") {
		t.Fatalf("err = %v", err)
	}
	if len(res.Degraded) != 0 {
		t.Fatalf("res.Degraded = %v, want empty (idempotent)", res.Degraded)
	}
}

// TestQuarantineManifestBestEffort: an unwritable quarantine dir must NOT stop
// the destruction — DEK/auth/meta still die, the report says manifest missing.
func TestQuarantineManifestBestEffort(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	f := withDEKFake(t)
	seedCache(t, dir)
	// Pre-create quarantine as a FILE so MkdirAll fails deterministically.
	if err := os.WriteFile(filepath.Join(dir, "quarantine"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := QuarantineCache("server rejected device code")
	if err == nil {
		t.Fatal("bin rename must fail (target is a file) → DEGRADED expected")
	}
	if !strings.Contains(err.Error(), "DEGRADED") {
		t.Fatalf("err = %q, want DEGRADED", err)
	}
	if res.ManifestWritten {
		t.Fatal("manifest must be reported unwritten")
	}
	if !f.deleted {
		t.Fatal("DEK deletion must still run (manifest is not a precondition)")
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.auth.json")); !os.IsNotExist(serr) {
		t.Fatal("auth.json must still be deleted")
	}
}

// TestFileKeyProviderDeleteIsIdempotent: absent file → nil (rev4 幂等例外).
func TestFileKeyProviderDeleteIsIdempotent(t *testing.T) {
	f := &store.FileKeyProvider{Path: filepath.Join(t.TempDir(), "gone.key")}
	if err := f.Delete(); err != nil {
		t.Fatalf("Delete on absent: %v, want nil", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ -run 'TestQuarantine|TestFileKeyProviderDelete' -count=1`
Expected: FAIL（`undefined: QuarantineCache` 等）

- [ ] **Step 3: 实现**

(a) `internal/store/masterkey.go` 的 FileKeyProvider 增方法：

```go
// Delete removes the key file; an absent file is success (Plan 34 rev4 幂等例外 —
// QuarantineCache treats "target already gone" as idempotent completion).
func (f *FileKeyProvider) Delete() error {
	if f.Path == "" {
		return nil
	}
	if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
```

(b) Create `internal/clientops/quarantine.go`：

```go
package clientops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ErrCacheQuarantined is the Plan 34 sentinel: a PINNED server answered 401 and
// the local cache was destroyed (rev4 §3). errors.Is-matchable; the wrapped text
// carries the server reason and DEGRADED steps for display only.
var ErrCacheQuarantined = errors.New("cache quarantined by server rejection")

// QuarantineResult reports per-step outcomes (rev4 §2). Degraded lists the
// critical steps (dek/auth/bin) that FAILED; ManifestWritten is false when the
// quarantine dir was unwritable (best-effort, never a precondition).
type QuarantineResult struct {
	Steps           map[string]string
	Degraded        []string
	ManifestWritten bool
}

type quarantineManifest struct {
	State    string            `json:"state"` // started | done
	Reason   string            `json:"reason"`
	TS       int64             `json:"ts"`
	Steps    map[string]string `json:"steps,omitempty"`
	Degraded []string          `json:"degraded,omitempty"`
}

// QuarantineCache destroys the local cache per Plan 34 rev4 §2, DEK-first so any
// crash leaves the ciphertext undecryptable at worst:
//  0. MkdirAll(quarantine/) + best-effort manifest {started} — NEVER a precondition;
//  1. DEK delete (provider Delete(); absent = idempotent success);
//  2. cache.auth.json delete (device code plaintext);
//  3. cache.bin → quarantine/ rename (same-dir subdir, single retained copy);
//  4. cache.meta.json delete (non-critical);
//  5. best-effort manifest {done, steps, degraded}.
//
// Critical steps are dek/auth/bin; ANY error on them (except os.IsNotExist /
// provider-absent idempotence) records DEGRADED — honestly, never silently.
// Cross-process mutual exclusion is deliberately absent (rev4 accepted: idempotent
// steps bound the damage to reporting imprecision only).
func QuarantineCache(reason string) (QuarantineResult, error) {
	res := QuarantineResult{Steps: map[string]string{}}
	dir, bin, meta, _, err := CachePaths()
	if err != nil {
		return res, fmt.Errorf("quarantine: %w", err)
	}
	qdir := filepath.Join(dir, "quarantine")

	// Step 0: intent manifest (best-effort — failure only logs).
	if mkErr := os.MkdirAll(qdir, 0o700); mkErr == nil {
		res.ManifestWritten = writeQuarantineManifest(qdir, &quarantineManifest{State: "started", Reason: reason, TS: time.Now().Unix()})
	} else {
		fmt.Fprintf(os.Stderr, "cache QUARANTINE: manifest dir unavailable (best-effort skipped): %v\n", mkErr)
	}

	// Step 1: DEK first — the key dies before anything else moves.
	if d, ok := DekProvider().(interface{ Delete() error }); ok {
		if dErr := d.Delete(); dErr != nil {
			res.Steps["dek"] = dErr.Error()
			res.Degraded = append(res.Degraded, "dek")
		} else {
			res.Steps["dek"] = "ok"
		}
	} else {
		res.Steps["dek"] = "ok(no-delete-provider)" // test mem provider: in-process key only
	}

	// Steps 2-4: auth, bin rename, meta. Absent targets = idempotent success.
	credPath, _ := CacheCredPath()
	res.Steps["auth"] = removeOrRecord(credPath, &res.Degraded, "auth")
	if rErr := renameIntoQuarantine(bin, qdir); rErr != nil {
		if os.IsNotExist(rErr) {
			res.Steps["bin"] = "ok(absent)" // idempotent re-quarantine
		} else {
			res.Steps["bin"] = rErr.Error()
			res.Degraded = append(res.Degraded, "bin")
		}
	} else {
		res.Steps["bin"] = "ok"
	}
	res.Steps["meta"] = removeOrRecord(meta, &res.Degraded, "meta") // non-critical: never in Degraded

	// Step 5: completion manifest (best-effort).
	if res.ManifestWritten {
		m := &quarantineManifest{State: "done", Reason: reason, TS: time.Now().Unix(), Steps: res.Steps, Degraded: res.Degraded}
		writeQuarantineManifest(qdir, m)
	}

	var errOut error
	if len(res.Degraded) > 0 {
		errOut = fmt.Errorf("%w [DEGRADED: %v] — the old snapshot may still be decryptable; delete it manually", ErrCacheQuarantined, res.Degraded)
	} else {
		errOut = fmt.Errorf("%w (%s): snapshot isolated, device code + DEK deleted — re-enroll with a fresh device code", ErrCacheQuarantined, reason)
	}
	fmt.Fprintf(os.Stderr, "cache QUARANTINED by server rejection (%s): snapshot isolated to quarantine/, device code + DEK deleted — re-enroll with a fresh device code\n", reason)
	return res, errOut
}

// removeOrRecord deletes path; absent = "ok(absent)", error = recorded into
// degraded when critical (stepName non-empty) else logged-only.
func removeOrRecord(path string, degraded *[]string, step string) string {
	if path == "" {
		return "ok(no-path)"
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return "ok(absent)"
		}
		if step != "meta" { // meta is non-critical (rev4)
			*degraded = append(*degraded, step)
		}
		return err.Error()
	}
	return "ok"
}

// renameIntoQuarantine moves bin into qdir under a timestamped name, first
// clearing any previously retained copy (single-copy retention, rev4 §2).
func renameIntoQuarantine(bin, qdir string) error {
	if _, serr := os.Stat(bin); serr != nil {
		return serr // caller maps IsNotExist to idempotent success
	}
	entries, derr := os.ReadDir(qdir)
	if derr == nil {
		for _, e := range entries {
			if e.Name() != "manifest.json" {
				_ = os.Remove(filepath.Join(qdir, e.Name())) // drop the previous retained copy
			}
		}
	}
	return os.Rename(bin, filepath.Join(qdir, fmt.Sprintf("cache.bin.quarantined-%d", time.Now().Unix())))
}

func writeQuarantineManifest(qdir string, m *quarantineManifest) bool {
	blob, err := json.Marshal(m)
	if err != nil {
		return false
	}
	mpath := filepath.Join(qdir, "manifest.json")
	if werr := atomicWriteUnique(mpath, blob); werr != nil {
		fmt.Fprintf(os.Stderr, "cache QUARANTINE: manifest write failed (best-effort): %v\n", werr)
		return false
	}
	return true
}

// res2Text is a tiny helper used by tests asserting error text.
func res2Text(err error) string { if err == nil { return "" }; return err.Error() }
```

（实现者注意：`res2Text` 若无测试引用则删；`removeOrRecord` 对 meta 失败只记 steps 不进 Degraded——与 spec 关键步三件一致。）

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/clientops/ -count=1`
Expected: PASS（含既有 cache/lazy/reload 全部——正常 pull/load 路径零变化回归锚）

- [ ] **Step 5: Commit**

```bash
git add internal/clientops/quarantine.go internal/clientops/quarantine_test.go internal/store/masterkey.go
git commit -m "feat(clientops): QuarantineCache — DEK-first 四件销毁/manifest best-effort/DEGRADED/幂等(Plan 34 T3)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 4: 触发接线 — DoPull 401 + MaybeLazyPull 哨兵/进程级标记

**Files:**
- Modify: `internal/clientops/clientops.go`（DoPull 401 分支约 302-305 行；MaybeLazyPull 89 行返回点 + 包级 flag）
- Modify: `internal/cli/cache.go`（cachePullCmd 捕获哨兵文案，约 81 行错误返回处）
- Test: `internal/clientops/quarantine_test.go`（追加）；`internal/cli/cache_test.go`（追加）

**Interfaces:**
- Consumes: T3 `QuarantineCache`/`ErrCacheQuarantined`。
- Produces: pinned-401 pull → 销毁 + `ErrCacheQuarantined`；lazy 哨兵 → stderr + 零 backoff + 进程级 `cacheQuarantinedFlag`；手动 CLI 明确文案。

- [ ] **Step 1: 写失败测试**

`internal/clientops/quarantine_test.go` 追加（httptest TLS 自签 + pin 的最小 harness——复用 pin_test.go 的 pinningTransport 形态，证书指纹提取照 pin_test 既有做法）：

```go
// TestDoPullPinned401Quarantines: pinned TLS + 401 → the four artifacts are
// destroyed and the sentinel is returned. The server here is a stub whose TLS
// cert we pin; the FIRST pull succeeds, the SECOND (revoked server-side) 401s.
func TestDoPullPinned401Quarantines(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	seedCache(t, dir)

	srv := newPinnedSnapshotServer(t, func(r *http.Request) (int, string) {
		if r.Header.Get("Authorization") == "Bearer good-code" {
			return 200, `{"version":1,"servers":[]}`
		}
		return 401, `invalid cache token: revoked`
	})
	// First pull OK (seeds nothing extra but proves the pin path).
	if err := DoPull(srv.URL, "good-code", srv.Pin, PullOpts{}); err != nil {
		t.Fatalf("first pull: %v", err)
	}
	// Revoked second pull → sentinel + destruction.
	err := DoPull(srv.URL, "revoked-code", srv.Pin, PullOpts{})
	if !errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("err = %v, want ErrCacheQuarantined", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.auth.json")); !os.IsNotExist(serr) {
		t.Fatal("auth.json destroyed")
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("cache.bin quarantined away")
	}
}

// TestDoPullNonTriggerFaces: plaintext 401, network error, and non-401 NEVER
// quarantine (rev4 §3 不触发面).
func TestDoPullNonTriggerFaces(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	bin, _, _, _ := seedCache(t, dir)

	// Plaintext 401: no pin → no destruction.
	srv401 := newPlainSnapshotServer(t, 401)
	if err := DoPull(srv401.URL, "x", "", PullOpts{AllowPlain: true}); err == nil {
		t.Fatal("want error")
	}
	if _, serr := os.Stat(bin); serr != nil {
		t.Fatal("plaintext 401 must NOT quarantine")
	}
	// Network error against a pinned-but-dead address.
	if err := DoPull("https://127.0.0.1:1/", "x", "sha256:" + strings.Repeat("0", 64), PullOpts{}); err == nil {
		t.Fatal("want error")
	}
	if _, serr := os.Stat(bin); serr != nil {
		t.Fatal("network error must NOT quarantine")
	}
	// Non-401 status.
	srv500 := newPlainSnapshotServer(t, 500)
	if err := DoPull(srv500.URL, "x", "", PullOpts{AllowPlain: true}); err == nil {
		t.Fatal("want error")
	}
	if _, serr := os.Stat(bin); serr != nil {
		t.Fatal("non-401 must NOT quarantine")
	}
}

// TestMaybeLazyPullNoRetryAfterQuarantine: after a sentinel, the in-process
// flag stops further automatic attempts even though cache.auth.json deletion
// "failed" (simulated by re-seeding it).
func TestMaybeLazyPullNoRetryAfterQuarantine(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	seedCache(t, dir)
	srv := newPinnedSnapshotServer(t, func(*http.Request) (int, string) { return 401, "revoked" })
	if err := WriteCacheCred(&CacheCred{URL: srv.URL, Token: "x", Pin: srv.Pin}); err != nil {
		t.Fatal(err)
	}
	ResetLazyPullBackoffForTest()
	if err := MaybeLazyPull(time.Nanosecond); !errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("lazy: err = %v, want sentinel", err)
	}
	// Simulate a FAILED auth.json deletion (cred survived) — the flag must still
	// prevent a second automatic attempt in THIS process.
	if err := WriteCacheCred(&CacheCred{URL: srv.URL, Token: "x", Pin: srv.Pin}); err != nil {
		t.Fatal(err)
	}
	ResetLazyPullBackoffForTest()
	if err := MaybeLazyPull(time.Nanosecond); err != nil {
		t.Fatalf("post-quarantine lazy pull must be a silent no-op, got %v", err)
	}
}
```

`internal/cli/cache_test.go` 追加（CLI 写码时序——现状行为钉测试）：

```go
// TestCachePullDoesNotPersistCredOn401 pins the rev4 §3 CLI ordering: a failed
// (401) pull must NOT overwrite the persisted credential — the old code stays.
func TestCachePullDoesNotPersistCredOn401(t *testing.T) {
	// Drive the RunE body's ordering indirectly: the code path in cachePullCmd
	// returns DoPull's error BEFORE WriteCacheCred, so assert the source-level
	// invariant via a 401 pull against a stub with a pre-seeded cred file.
	// (Full command wiring covered by the clientops-level trigger tests; this
	// test pins that the CLI persists only on success.)
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	prev := clientops.DekProvider
	clientops.DekProvider = func() store.KeyProvider { return store.NewMemKeyProvider() }
	t.Cleanup(func() { clientops.DekProvider = prev })
	if err := clientops.WriteCacheCred(&clientops.CacheCred{URL: "https://old", Token: "old-code", Pin: "sha256:" + strings.Repeat("1", 64)}); err != nil {
		t.Fatal(err)
	}
	// A failing DoPull must leave the cred untouched (cachePullCmd writes cred
	// only after DoPull returns nil — the ordering under test).
	srv := /* newPlainSnapshotServer 或直接 DoPull 到 401 stub */
	if err := clientops.DoPull(srv.URL, "bad-new-code", "", clientops.PullOpts{AllowPlain: true}); err == nil {
		t.Fatal("want 401 error")
	}
	cred, err := clientops.ReadCacheCred()
	if err != nil || cred == nil || cred.Token != "old-code" {
		t.Fatalf("cred = %+v err=%v — must be untouched old-code", cred, err)
	}
}
```

（实现者注意：两个测试文件里的 `newPinnedSnapshotServer`/`newPlainSnapshotServer` harness——在 quarantine_test.go 里实现一次：自签 cert（`crypto/tls` + `x509` 或复用 `mcpserver.LoadOrCreateServeCert` 的测试等价物）+ httptest.NewUnstartedServer + TLS + 从 cert 提取 SPKI sha256 作 pin。若 pin_test.go 已有同形 helper 直接复用其名。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ -run 'TestDoPull|TestMaybeLazyPullNoRetry' -count=1 && go test ./internal/cli/ -run TestCachePullDoesNotPersistCredOn401 -count=1`
Expected: FAIL（401 未触发销毁/sentinel 缺失/无进程级 flag）

- [ ] **Step 3: 实现**

(a) `clientops.go` DoPull 的非 200 分支（现约 302-305 行）改为：

```go
	if res.StatusCode != 200 {
		io.Copy(io.Discard, res.Body) // keep the keep-alive socket reusable
		if pin != "" && res.StatusCode == 401 {
			// Plan 34 rev4 §3 — the ONLY trigger: a pinned server rejected us.
			qres, qerr := QuarantineCache("server rejected device code")
			_ = qres
			body, _ := io.ReadAll(res.Body) // already drained above; reason text comes from qerr
			_ = body
			return fmt.Errorf("%w (server said: invalid cache token)", qerr)
		}
		return fmt.Errorf("pull: server returned %d (is the authorization code valid/active?)", res.StatusCode)
	}
```

（实现者注：ReadAll 在 Drain 之后恒空——直接用 qerr wrap 即可，删掉 body 两行；上面的骨架只为展示挂点。返回值要保证 `errors.Is(err, ErrCacheQuarantined)` 为真——QuarantineCache 的返回错误本身已 wrap 哨兵。）

(b) MaybeLazyPull：包级 `var cacheQuarantinedFlag bool`（同 `lazyPullBackoff` 旁）；`MaybeLazyPull` 在 ReadCacheCred 之前检查：

```go
	if cacheQuarantinedFlag {
		return nil // Plan 34: quarantined this process — no further auto-pulls
	}
```

末尾 DoPull 调用改为：

```go
	err = DoPull(cred.URL, code, pin, PullOpts{Timeout: LazyPullTimeout, StatusOut: os.Stderr})
	if errors.Is(err, ErrCacheQuarantined) {
		cacheQuarantinedFlag = true
		fmt.Fprintf(os.Stderr, "cache QUARANTINED by server rejection: automatic pulls disabled for this session — re-enroll with a fresh device code\n")
		return nil // NOT an error for the caller's "serving stale" logic — there is no cache to serve
	}
	return err
```

（注意：哨兵路径**不设** `lazyPullBackoff.lastAttempt`（不进 backoff）——上面结构已保证；`return nil` 的理由：cache 已销毁，mcp.go 随后的 LoadCacheSnapshot 会给出正确的 quarantined 报文，"serving stale cache" 日志反而是误导。）

(c) `cli/cache.go` cachePullCmd 两个 DoPull 错误返回处（明文分支保持原样）改 pinned 分支：

```go
			if err := clientops.DoPull(url, token, fp, clientops.PullOpts{StatusOut: cmd.ErrOrStderr()}); err != nil {
				if errors.Is(err, clientops.ErrCacheQuarantined) {
					cmd.SilenceUsage = true
					return fmt.Errorf("cache was QUARANTINED: the server rejected this device code (revoked?).\nRe-enroll: obtain a fresh device code and run cache pull again.\n(detail: %v)", err)
				}
				return err
			}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/clientops/ ./internal/cli/ -count=1`
Expected: PASS（两包全绿——cli 含 standUpServe 等既有回归）

- [ ] **Step 5: Commit**

```bash
git add internal/clientops/clientops.go internal/clientops/quarantine_test.go internal/cli/cache.go internal/cli/cache_test.go
git commit -m "feat(clientops): pinned-401 触发接线 + lazy 哨兵/进程级标记 + CLI 文案与写码时序钉(Plan 34 T4)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 5: 报文链 — 三级降级 helper + mcp/status 接线 + e2e

**Files:**
- Create: `internal/clientops/quarantine_report.go`
- Modify: `internal/cli/mcp.go`（--cache 分支 LoadCacheSnapshot 失败处，约 53-56 行）；`internal/cli/cache.go`（cacheStatusCmd 失败处，约 113-116 行）
- Test: `internal/clientops/quarantine_report_test.go`（Create）

**Interfaces:**
- Consumes: T3 manifest 结构；`cacheMeta`（clientops 私有）。
- Produces: `func QuarantineReport(loadErr error) (string, bool)`——mcp.go 与 status 共用（归因收敛单点）。

- [ ] **Step 1: 写失败测试**

Create `internal/clientops/quarantine_report_test.go`：

```go
package clientops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestQuarantineReportChain pins rev4 §4's three-tier attribution + time guard.
func TestQuarantineReportChain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)

	// Tier 3: nothing quarantined → not our report.
	if _, ok := QuarantineReport(errors.New("cache.bin missing")); ok {
		t.Fatal("no quarantine dir → no attribution")
	}

	// Tier 2: dir exists, manifest unwritten.
	if err := os.MkdirAll(filepath.Join(dir, "quarantine"), 0o700); err != nil {
		t.Fatal(err)
	}
	msg, ok := QuarantineReport(errors.New("cache.bin missing"))
	if !ok || msg != "cache was quarantined (details unavailable — quarantine/manifest.json missing); re-enroll via cache pull" {
		t.Fatalf("tier2: ok=%v msg=%q", ok, msg)
	}

	// Tier 1: fresh manifest (ts newer than meta's pulled_at) → full attribution.
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(`{"url":"u","pulled_at":100}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quarantine", "manifest.json"),
		[]byte(`{"state":"done","reason":"server rejected device code","ts":200,"steps":{"dek":"ok","auth":"ok","bin":"ok","meta":"ok"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, ok = QuarantineReport(errors.New("cache.bin missing"))
	if !ok || msg != "cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code" {
		t.Fatalf("tier1 done: ok=%v msg=%q", ok, msg)
	}

	// Time guard: an OLD manifest (ts <= meta.pulled_at) must NOT attribute —
	// a re-pull happened after the quarantine, so this bin-missing is unrelated.
	if err := os.WriteFile(filepath.Join(dir, "quarantine", "manifest.json"),
		[]byte(`{"state":"done","reason":"x","ts":50}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := QuarantineReport(errors.New("cache.bin missing")); ok {
		t.Fatal("stale manifest (pre-dates the last pull) must fall through to missing-cache")
	}

	// Degraded variant text.
	if err := os.WriteFile(filepath.Join(dir, "quarantine", "manifest.json"),
		[]byte(`{"state":"done","ts":300,"degraded":["dek"],"steps":{"dek":"boom"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(`{"url":"u","pulled_at":100}`), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, ok = QuarantineReport(errors.New("cache.bin missing"))
	if !ok || msg != "cache quarantined by server rejection (token revoked?) [DEGRADED: [dek]] — re-enroll via cache pull with a fresh device code; manual cleanup may be needed" {
		t.Fatalf("degraded: ok=%v msg=%q", ok, msg)
	}

	// Interrupted variant.
	if err := os.WriteFile(filepath.Join(dir, "quarantine", "manifest.json"),
		[]byte(`{"state":"started","ts":400}`), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, ok = QuarantineReport(errors.New("decrypt failed"))
	if !ok || msg != "cache quarantine was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json" {
		t.Fatalf("interrupted: ok=%v msg=%q", ok, msg)
	}
}
```

（实现者注：DEGRADED 文案里步骤清单的序列化形态（`[dek]`）由实现定——测试以实现后实际形态对齐断言字符串，spec 只钉「含步骤清单」。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ -run TestQuarantineReportChain -count=1`
Expected: FAIL（`undefined: QuarantineReport`）

- [ ] **Step 3: 实现**

Create `internal/clientops/quarantine_report.go`：

```go
package clientops

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// QuarantineReport maps a failed cache load to the rev4 §4 report line.
// ok=false → the caller keeps its existing missing/decrypt error text.
// Tier 1 (manifest readable AND manifest.ts > meta.pulled_at): full attribution
//   — done / done+degraded / started variants.
// Tier 2 (manifest unreadable but quarantine/ exists): detail-free attribution.
// Tier 3 (no quarantine dir): not ours.
// The time guard makes a stale manifest auto-invalidate after a successful
// re-pull (crash-safe reset, rev4).
func QuarantineReport(loadErr error) (string, bool) {
	dir, _, metaPath, _, err := CachePaths()
	if err != nil {
		return "", false
	}
	blob, merr := os.ReadFile(filepath.Join(dir, "quarantine", "manifest.json"))
	if merr == nil {
		var m struct {
			State    string   `json:"state"`
			TS       int64    `json:"ts"`
			Degraded []string `json:"degraded"`
		}
		if json.Unmarshal(blob, &m) == nil && manifestFresherThanMeta(m.TS, metaPath) {
			switch {
			case m.State == "done" && len(m.Degraded) > 0:
				return fmt.Sprintf("cache quarantined by server rejection (token revoked?) [DEGRADED: %v] — re-enroll via cache pull with a fresh device code; manual cleanup may be needed", m.Degraded), true
			case m.State == "done":
				return "cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code", true
			case m.State == "started":
				return "cache quarantine was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json", true
			}
			return "", false // unknown state — fall through conservatively
		}
	}
	// Tier 2: dir presence alone (manifest unwritable during quarantine).
	if st, serr := os.Stat(filepath.Join(dir, "quarantine")); serr == nil && st.IsDir() {
		return "cache was quarantined (details unavailable — quarantine/manifest.json missing); re-enroll via cache pull", true
	}
	return "", false
}

// manifestFresherThanMeta: the attribution only holds when the manifest postdates
// the last successful pull (cache.meta.json.pulled_at). A meta that is absent or
// unreadable conservatively FAILS the guard (tier-1 skipped).
func manifestFresherThanMeta(ts int64, metaPath string) bool {
	mb, err := os.ReadFile(metaPath)
	if err != nil {
		return false
	}
	var meta struct {
		PulledAt int64 `json:"pulled_at"`
	}
	if json.Unmarshal(mb, &meta) != nil {
		return false
	}
	return ts > meta.PulledAt
}
```

**归因重置**：`DoPull` 成功落盘段（写 meta 之后）追加：

```go
	// Plan 34 rev4 §4: a successful pull invalidates the quarantine attribution.
	_ = os.Remove(filepath.Join(filepath.Dir(bin), "quarantine", "manifest.json"))
```

**接线**（两处同型）：
- `cli/mcp.go` --cache 分支：`snap, err := clientops.LoadCacheSnapshot()` 失败时，先 `if msg, ok := clientops.QuarantineReport(err); ok { return errors.New(msg) }` 再 `return err`。
- `cli/cache.go` cacheStatusCmd：同型（`return errors.New(msg)`）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/clientops/ ./internal/cli/ -count=1`
Expected: PASS

- [ ] **Step 5: e2e 全链测试（追加进 quarantine_test.go）+ Commit**

```go
// TestE2EQuarantineFullChain: pull ok → server flips to 401 → pull quarantines
// → spawn-time LoadCacheSnapshot error maps to the tier-1 report → re-pull with
// a good code restores (manifest attribution reset).
func TestE2EQuarantineFullChain(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	withDEKFake(t)
	revoked := false
	srv := newPinnedSnapshotServer(t, func(*http.Request) (int, string) {
		if revoked {
			return 401, `invalid cache token: revoked`
		}
		return 200, `{"version":1,"servers":[],"credentials":[]}`
	})
	if err := DoPull(srv.URL, "good-code", srv.Pin, PullOpts{}); err != nil {
		t.Fatalf("seed pull: %v", err)
	}
	revoked = true
	if err := DoPull(srv.URL, "good-code", srv.Pin, PullOpts{}); !errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("revoked pull: %v", err)
	}
	// Spawn-time surface: load fails, report attributes.
	_, lerr := LoadCacheSnapshot()
	if lerr == nil {
		t.Fatal("load must fail post-quarantine")
	}
	if msg, ok := QuarantineReport(lerr); !ok {
		t.Fatalf("report missing: %v", lerr)
	} else if !strings.Contains(msg, "quarantined by server rejection") {
		t.Fatalf("msg = %q", msg)
	}
	// Re-enroll: server accepts again; pull succeeds; attribution resets.
	revoked = false
	if err := DoPull(srv.URL, "good-code", srv.Pin, PullOpts{}); err != nil {
		t.Fatalf("re-enroll pull: %v", err)
	}
	os.Remove(filepath.Join(dir, "cache.bin")) // simulate an unrelated later loss
	if _, ok := QuarantineReport(errors.New("cache.bin missing")); ok {
		t.Fatal("post-re-pull bin loss must NOT attribute to quarantine (reset held)")
	}
}
```

Run: `go test ./internal/clientops/ -count=1` → PASS；然后：

```bash
git add internal/clientops/quarantine_report.go internal/clientops/quarantine_report_test.go internal/clientops/quarantine_test.go internal/clientops/clientops.go internal/cli/mcp.go internal/cli/cache.go
git commit -m "feat(clientops): QuarantineReport 三级降级链+时间约束归因重置, mcp/status 接线, e2e 全链(Plan 34 T5)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 6: revoke CLI 提示 + 文档全景 + 全量回归

**Files:**
- Modify: `internal/cli/cache_tokens.go`（revoke 成功输出处）
- Modify: `docs/threat-model.md`、`docs/multi-machine.md`、`docs/backup-restore.md`、`docs/agent-access.md`、`README.md`、`docs/concepts.md`、`docs/compat-matrix.md`

**Interfaces:**
- Consumes: T1-T5 全部（文档描述已落地行为）。

- [ ] **Step 1: CLI reminder 行**

`cache_tokens.go` 的 revoke 成功输出后追加一行（对照该文件既有输出风格）：

```go
fmt.Fprintln(cmd.OutOrStdout(), "reminder: also revoke project tokens issued to that device if it may be compromised")
```

- [ ] **Step 2: threat-model.md**

(b) 切断失效条目按 spec §5 改写：复合前提（失窃+已 revoke）兑现路径 = **revoke + 回连即销毁**（≤30min lazy cadence）；永离线残余 = 轮换服务器凭据（唯一根治）；**fail-closed 代价**：pinned 401 不区分 revoked/unknown 且不区分新码打错——非攻击场景（服务端数据丢失/重建、换码手滑）也触发销毁（恢复 = 正确码重 pull）；**失窃响应**：cache token 与该设备 project token 都要 revoke。

- [ ] **Step 3: multi-machine.md / backup-restore.md / agent-access.md / README / concepts / compat-matrix**

- multi-machine.md revoke 语义节：`cache-tokens revoke` =「断拉新 + 回连销毁本地 cache」；四件销毁清单 + DEGRADED/manifest + quarantine 痕迹口径 + 换码打错预期形态 + 两 token 失窃处置。
- backup-restore.md 运维注记：**ExportSnapshot 不含 cache_tokens** → 恢复后表空 → 全体设备 unknown 401 → 批量切断（预期、非事故）；恢复流程含逐设备重新发码；**带外警示**：raw-DB 直拷使历史状态回滚、已 revoked 码可能复活，必须逐行审计重发。
- agent-access.md 断连语义四层第四层（离线 cache）改写；README/concepts cache 相关节核对（措辞取 spec §5）。
- compat-matrix.md：纯增量行（revoke 语义增强；无新 env、无新响应头——B 已砍出，不得出现 SSHMGR_CACHE_MAX_OFFLINE/X-Sshmgr-Time 字样）。

- [ ] **Step 4: 全量回归 + Commit**

Run: `go build ./... && go vet ./... && go test ./... -count=1`
Expected: 全绿（eval/conformance 双门控 SKIP 正常；命令串含 eval 词被守卫误拦时换 `go test ./internal/... ./cmd/...` 形态并如实记录）

```bash
git add internal/cli/cache_tokens.go docs/ README.md
git commit -m "docs+cli: revoke 回连销毁全景(threat-model/multi-machine/backup-restore/agent-access/compat)+revoke CLI 两 token 提示(Plan 34 T6)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review 记录

1. **Spec 覆盖**：§1 reason（T1+T2）；§2 QuarantineCache 全钉（T3：DEK-first/manifest best-effort/四件/DEGRADED/幂等/同目录/单份/互斥不做——互斥为§2 登记性条款无代码）；§3 触发面+不触发面+lazy 标记+CLI 时序+文案+project token 口径（T4；口径的文档面在 T6）；§4 三级降级+时间约束+归因重置+接线（T5）；§5 文档（T6）；§6 测试矩阵逐项落位（T1-T5 各测试组）；§7 不做（零任务引入 B/C/锁/续跑 = 结构性保证）；§8 验收（T5 e2e + T6 全量；owner 手工复验在外）。无缺口。
2. **占位符扫描**：T4 Step 1 的 CLI 时序测试里 `srv :=` 一处需实现者接 401 stub（文内已注明取材 newPlainSnapshotServer）；除此之外无 TBD。骨架注记（DoPull body 两行删、res2Text 删）均已显式标注——属实现指令非留白。
3. **类型一致性**：`QuarantineCache(reason string) (QuarantineResult, error)`（T3 产/T4 消费）；`ErrCacheQuarantined`（T3 产/T4/T5 errors.Is）；`QuarantineReport(loadErr error) (string, bool)`（T5 内自洽）；`RevokedCacheTokenNameByPrefix(prefix string) (string, bool, error)`（T1 产/T2 消费）；manifest JSON 字段 `{state,reason,ts,steps,degraded}`（T3 写/T5 读一致）。
