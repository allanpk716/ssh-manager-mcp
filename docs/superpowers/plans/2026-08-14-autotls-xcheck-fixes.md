# 自动 TLS xcheck 评审修复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 xcheck 异构评审(4 家一致 SUGGEST_CHANGES)暴露的问题:1 个真 bug(http://+pin 静默明文)+ 1 个安全策略改动(明文回退改 hard-fail + `--allow-plaintext`) + 2 个加固(F10 误删 cert 防护、KeyEncipherment 清理)+ 文档对齐(spec §3.5/§4.1)+ 2 个 dead-feature/测试补全。

**Architecture:** 全部是现有 `cache pull` / `cert.go` / docs 的定向修复,不动 TLS pinning 核心机制(`InsecureSkipVerify:true + VerifyConnection` SPKI pin 本身正确,4 家共识)。策略改动(明文回退→hard-fail)由用户拍板。

**Tech Stack:** Go stdlib(`net/url`、`crypto/x509`、`crypto/tls`、`os`);cobra CLI;现有 `internal/paths`、`internal/store.HardenACL`;现有测试 helper `withDEK`。

## Global Constraints

- **核心 pinning 不动**:`pinningTransport` 的 `InsecureSkipVerify:true + VerifyConnection` 是正确实现(4 家共识 + Task 5 review 已证)。本计划只修 bug、策略、加固、文档,不重写 pinning。
- **新策略(用户拍板)**:无 pin 时 **hard-fail**(拒连);明文连接需显式 `--allow-plaintext` opt-in。这同时消解 F7(malformed pin fail-open)和 F4(明文回退 fail-open)。
- **F10(用户拍板纳入)**:加"已初始化"标记文件;cert 缺失但标记存在 → serve 拒启动(不静默重生)。防误删 → 全客户端硬失败。
- **scheme 校验(F8,kimi bug)**:pin 非空时强制 `https://` URL,否则 hard-fail。
- **每任务 TDD**:先写失败测试 → 跑红 → 最小实现 → 跑绿 → commit。
- **`gofmt -w` 只动本任务改的文件**;每任务结束前 `gofmt -l <files>` 必须空 + `go build ./...` + 相关 `go test` 通过。
- **向后兼容**:显式 `--tls-cert` 路径不动;`--allow-plaintext` 是 opt-in,不强制改已有自动化(除非它们无 pin —— 那本来就该配 pin)。

---

## File Structure

| 文件 | 改动 | 任务 |
|---|---|---|
| `internal/cli/cache.go` | scheme 校验 + hard-fail 策略 + `--allow-plaintext` flag + token 内嵌单串 | T1, T2 |
| `internal/cli/cache_test.go` | scheme 校验测试 + 改写 PlaintextFallback 为 hard-fail 测试 | T1, T2 |
| `internal/cli/cache_pull_integration_test.go` | 内嵌 pin e2e 改用单串 token(随 T2) | T2 |
| `internal/mcpserver/cert.go` | KeyEncipherment 去掉 + init-marker 防误删 | T3, T4 |
| `internal/mcpserver/cert_test.go` | KeyUsage 断言 + init-marker 测试 | T3, T4 |
| `internal/mcpserver/serve.go` | (T4 若需在 RunServe 报错信息) | T4 |
| `internal/paths/paths.go` | `ServeCertMarkerPath()` + 常量 | T4 |
| `internal/cli/cache_tokens.go` | printCacheToken 输出内嵌单串(随 T2) | T2 |
| `internal/mcpserver/serve_test.go` | pinningTransport peer-cert 匿名测试(补缺) | T5 |
| spec + plan + docs | 文档对齐(§3.5 tls.Config、§4.1 迁移、轮换 runbook) | T6 |

依赖顺序:T3(KeyEncipherment)与 T4(init-marker)独立;T1(scheme)+T2(hard-fail/单串)是 cache.go 串行;T5(测试补全)独立;T6(文档)最后。

---

### Task 1: F8 — pin 非空时强制 https:// URL(kimi 抓到的真 bug)

**Files:**
- Modify: `internal/cli/cache.go`(`cachePullCmd`,~line 120-152)
- Test: `internal/cli/cache_test.go`

**Interfaces:**
- Consumes: `resolvePin`(已有)、`http.NewRequest`
- Produces:`cachePullCmd` 在 pin 非空时校验 url scheme;非法 → `return error`

- [ ] **Step 1: Write the failing test**

追加到 `cache_test.go`。复用现有 `TestCachePull_PinnedTLS_Succeeds` 的 httptest TLS server 套路(读它 228-306 行拿 `standUpServeTLS` / `withDEK` / `withEnv` helper)。新测试:用同一个 TLS server,但 `SSHMGR_CACHE_URL` 设成 `http://127.0.0.1:<port>`(故意 http),设一个正确 pin,**断言 cache pull 失败**(因 pin 非空 + 非 https → hard-fail),错误信息含 `https`:

```go
func TestCachePull_PinWithHttpURL_HardFails(t *testing.T) {
	// Reuse the TLS server helper from TestCachePull_PinnedTLS_Succeeds.
	srv, fp := standUpServeTLS(t) // returns *httptest.Server + its SPKI pin; see existing test
	defer srv.Close()

	// Rewrite the URL to http:// (drop the TLS) but keep a valid pin.
	httpURL := "http://" + strings.TrimPrefix(srv.URL, "https://")
	withEnv(t, "SSHMGR_CACHE_URL", httpURL)
	withEnv(t, "SSHMGR_CACHE_TOKEN", "code-123")
	withEnv(t, "SSHMGR_SERVE_PIN", fp)
	withDEK(t)

	root := newRootForTest(t) // existing helper
	_, err := execCobra(root, "cache", "pull")
	if err == nil {
		t.Fatal("expected hard-fail when pin set but URL is http://, got nil")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Fatalf("error should mention https, got: %v", err)
	}
}
```
NOTE:若 `standUpServeTLS` 签名不是返回 `(srv, fp)`,读 `TestCachePull_PinnedTLS_Succeeds` 看它怎么取 fp,照样取。`newRootForTest`/`execCobra`/`withEnv` 是 cache_test.go 现有 helper(读 168-210 行确认名字)。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/cli/ -run TestCachePull_PinWithHttpURL_HardFails -v`
Expected: FAIL —— 当前 pin 非空但 http:// 会静默走明文(不报错),测试断言 err != nil 会失败。

- [ ] **Step 3: Write minimal implementation**

在 `cachePullCmd` 的 pin 解析之后(`if plain {...} else {...}` 块之后、`loadOrCreateDEK` 之前)插入 scheme 校验。只对 **pin 非空**(即走 TLS 路径)的情况强制 https:

```go
// pin is set (TLS path): the URL MUST be https://, else the TLSClientConfig
// (with the pin) is silently never used — http:// doesn't negotiate TLS, so
// the pin would be dead and the request would go in cleartext with no warning.
// Hard-fail instead of silently downgrading. (xcheck F8)
if !plain {
	if u, perr := url.Parse(url); perr != nil || (u.Scheme != "https" && u.Scheme != "tls") {
		return fmt.Errorf("--url must be https:// when a server pin is set (got %q); "+
			"use --allow-plaintext for an explicit plaintext pull", url)
	}
}
```
顶部 import 加 `"net/url"`。(`u.Scheme != "tls"` 防御 —— 实际只 https 合法;若嫌多余可只判 `!= "https"`。)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/cli/ -run TestCachePull_PinWithHttpURL_HardFails -v`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cache.go internal/cli/cache_test.go
git commit -m "fix(cli): hard-fail when pin set but URL is not https (xcheck F8)"
```

---

### Task 2: F4/F7 — 无 pin 改 hard-fail + `--allow-plaintext` opt-in + token 内嵌单串

**Files:**
- Modify: `internal/cli/cache.go`(`cachePullCmd` + `resolvePin` 注释 + flag)
- Modify: `internal/cli/cache_test.go`(改写 `TestCachePull_PlaintextFallback_Warns` → hard-fail;加 opt-in 测试)
- Modify: `internal/cli/cache_pull_integration_test.go`(若其测了无 pin 回退路径)
- Modify: `internal/cli/cache_tokens.go`(`printCacheToken` 输出内嵌单串 `<code>:<pin>`)

**Interfaces:**
- Consumes: Task 1 的 scheme 校验、`resolvePin`、`stripEmbeddedPin`
- Produces:`cachePullCmd` 无 pin 默认 hard-fail;`--allow-plaintext` opt-in 才走明文;malformed pin 也 hard-fail

- [ ] **Step 1: Write the failing tests**

追加到 `cache_test.go`(两个):

```go
// F4/F7: no pin at all → hard-fail by default.
func TestCachePull_NoPin_HardFailsByDefault(t *testing.T) {
	srv, _ := standUpServeTLS(t)
	defer srv.Close()
	withEnv(t, "SSHMGR_CACHE_URL", srv.URL) // https
	withEnv(t, "SSHMGR_CACHE_TOKEN", "code-123")
	withEnv(t, "SSHMGR_SERVE_PIN", "") // none
	withDEK(t)
	root := newRootForTest(t)
	_, err := execCobra(root, "cache", "pull")
	if err == nil {
		t.Fatal("expected hard-fail with no pin by default, got nil")
	}
	if !strings.Contains(err.Error(), "pin") && !strings.Contains(err.Error(), "allow-plaintext") {
		t.Fatalf("error should mention pin/allow-plaintext, got: %v", err)
	}
}

// --allow-plaintext opts back into the plaintext path.
func TestCachePull_NoPin_AllowPlaintext_OptsIn(t *testing.T) {
	// A plaintext (non-TLS) httptest server serving /snapshot.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	defer srv.Close()
	withEnv(t, "SSHMGR_CACHE_URL", srv.URL) // http
	withEnv(t, "SSHMGR_CACHE_TOKEN", "code-123")
	withEnv(t, "SSHMGR_SERVE_PIN", "")
	withDEK(t)
	root := newRootForTest(t)
	_, err := execCobra(root, "cache", "pull", "--allow-plaintext")
	if err != nil {
		t.Fatalf("expected --allow-plaintext to permit plaintext pull, got: %v", err)
	}
}
```

**改写现有 `TestCachePull_PlaintextFallback_Warns`**(307 行起):它当前断言"无 pin → 警告 + 成功"。新策略下无 pin 默认 hard-fail,这个测试的语义变了。把它**改名为 `TestCachePull_AllowPlaintext_Warns`** 并加 `--allow-plaintext` 参数,断言仍走明文 + 有警告(警告文案改准,见 Step 3)。**不要删它,改写它**。

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -run 'TestCachePull_NoPin_HardFailsByDefault|TestCachePull_NoPin_AllowPlaintext_OptsIn|TestCachePull_AllowPlaintext_Warns' -v`
Expected: FAIL —— 当前无 pin 是 warn+proceed(不 hard-fail);`--allow-plaintext` flag 不存在。

- [ ] **Step 3: Write minimal implementation**

(a) 加 flag(`cachePullCmd` 的 flag 注册处,Task 1 加 `--pin` 的旁边):
```go
c.Flags().Bool("allow-plaintext", false, "opt into plaintext HTTP pull when no server pin is set (insecure; default is to refuse)")
```

(b) 改 RunE 的 `plain` 分支逻辑。当前(读 Task 1 改后的状态)大致是:
```go
pinFlag, _ := cmd.Flags().GetString("pin")
fp, plain := resolvePin(os.Getenv("SSHMGR_SERVE_PIN"), pinFlag, token)
```
在 `resolvePin` 之后插入 hard-fail 判定 + opt-in。**但先处理 malformed pin(F7)**:`resolvePin` 现在把 malformed pin 当"没有 pin"(fail-open)。改成:`resolvePin` 区分"未提供" vs "提供但非法"。最简方案 —— 在 `resolvePin` 调用前,单独检测 env/flag 是否"非空但非法":
```go
// F7: a pin-like value that fails to parse is a hard error (typo shouldn't
// silently downgrade to plaintext). Only a fully-absent pin allows fallback.
if raw := strings.TrimSpace(os.Getenv("SSHMGR_SERVE_PIN")); raw != "" {
	if _, ok := mcpserver.ParsePin(raw); !ok {
		return fmt.Errorf("SSHMGR_SERVE_PIN is set but not a valid sha256:<64hex> fingerprint: %q", raw)
	}
}
if raw := strings.TrimSpace(pinFlag); raw != "" {
	if _, ok := mcpserver.ParsePin(raw); !ok {
		return fmt.Errorf("--pin is not a valid sha256:<64hex> fingerprint: %q", raw)
	}
}
```
然后 `plain` 分支:
```go
if plain {
	allowPlain, _ := cmd.Flags().GetBool("allow-plaintext")
	if !allowPlain {
		return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
			"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: --allow-plaintext set: pulling over unverified HTTP (no TLS pin). /snapshot credentials travel in cleartext.\n")
	client = http.DefaultClient
} else {
	// ... (Task 1 的 scheme 校验 + pinningTransport,不变)
}
```
(注:`resolvePin` 自身对 token 内嵌的 malformed pin 仍 fail-safe 回退 —— 但 token 是用户手拼的,F7 主要防 env/flag 打错。token 内嵌 malformed 会落到 plain → 被 `--allow-plaintext` gate 挡住,除非 opt-in,语义一致。)

(c) `printCacheToken`(`cache_tokens.go:104`)输出内嵌单串(F11 dead-feature)。把分立 `--token X --pin Y` 改成**默认输出单串** `<code>:<pin>`:
```go
if fingerprint != "" {
	fmt.Fprintf(out, "  ssh-manager cache pull --url https://<serve-host>:7878 --token '%s:%s'\n", code, fingerprint)
	fmt.Fprintf(out, "  # (or) set SSHMGR_SERVE_PIN=%s and pass --token %s\n", fingerprint, code)
} else {
	fmt.Fprintf(out, "  ssh-manager cache pull --url https://<serve-host>:7878 --token %s\n", code)
}
```
这样第三优先级(token 内嵌)就有了生产者,与 `resolvePin`/`stripEmbeddedPin` 一致。

(d) `resolvePin` 的注释把"never hard-fail a configured client"改成新语义("malformed pin is a hard error; absent pin gates on --allow-plaintext")。

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestCachePull_NoPin_HardFailsByDefault|TestCachePull_NoPin_AllowPlaintext_OptsIn|TestCachePull_AllowPlaintext_Warns|TestCachePull_PinnedTLS_Succeeds|TestCachePull_PinMismatch_Fails' -v`
Expected: PASS(确认没破坏已有 pin 测试)。

也跑集成测试:`go test ./internal/cli/ -run TestCachePull_TokenEmbeddedPin -v`(若它依赖旧 plaintext 行为会需同步改 —— 读它确认)。

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cache.go internal/cli/cache_test.go internal/cli/cache_pull_integration_test.go internal/cli/cache_tokens.go
git commit -m "feat(cli): no-pin pull hard-fails unless --allow-plaintext; embed pin as single token (xcheck F4/F7/F11)"
```

---

### Task 3: F9 — cert KeyUsage 去掉 KeyEncipherment(ed25519 无意义)

**Files:**
- Modify: `internal/mcpserver/cert.go`(`generateServeCert`,line 138)
- Modify: `internal/mcpserver/cert_test.go`(加 KeyUsage 断言)
- Modify: `cert.go` 注释(InsecureSkipVerify 原因归正)

**Interfaces:** 无外部接口变化(只改 cert 模板内部字段)。

- [ ] **Step 1: Write the failing test**

追加到 `cert_test.go`。生成一张 cert,断言 KeyUsage **不含** KeyEncipherment、**含** DigitalSignature:

```go
func TestGenerateServeCert_KeyUsage_NoKeyEncipherment(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "serve-cert.pem")
	keyPath := filepath.Join(dir, "serve-key.pem")
	if err := generateServeCert(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	der, _ := os.ReadFile(certPath)
	block, _ := pem.Decode(der)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.KeyUsage&x509.KeyUsageKeyEncipherment != 0 {
		t.Errorf("ed25519 cert should NOT set KeyEncipherment (meaningless for pure-signature alg), got KeyUsage=%v", cert.KeyUsage)
	}
	if cert.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Errorf("cert should set DigitalSignature, got KeyUsage=%v", cert.KeyUsage)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcpserver/ -run TestGenerateServeCert_KeyUsage_NoKeyEncipherment -v`
Expected: FAIL —— 当前 `KeyUsage: DigitalSignature | KeyEncipherment`,KeyEncipherment 位被设。

- [ ] **Step 3: Write minimal implementation**

改 `cert.go:138`:
```go
KeyUsage:     x509.KeyUsageDigitalSignature, // ed25519 is a pure-signature alg; KeyEncipherment is meaningless for it (xcheck F9)
```

同时修注释(cert.go `pinningTransport` 不在这,但 cache.go 的 pinningTransport 注释把 InsecureSkipVerify 部分归因于"Windows ed25519"—— 那条在 cache.go,codex 说跑偏。**本任务只改 cert.go 内的注释**;cache.go 的注释归属 Task 2 的改动区,若 Task 2 没碰它,这里不动它,留 Minor)。cert.go 内若有"Windows ed25519"措辞,补一句真正跨平台原因是"自签证书不在信任池"。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mcpserver/ -run TestGenerateServeCert_KeyUsage_NoKeyEncipherment -v && go test ./internal/mcpserver/ -count=1`
Expected: PASS + 全包绿(确认没破坏 cert 生成/加载/指纹)。

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/cert.go internal/mcpserver/cert_test.go
git commit -m "fix(mcpserver): drop meaningless KeyEncipherment from ed25519 serve cert (xcheck F9)"
```

---

### Task 4: F10 — init-marker 防误删 cert 静默重生(opencode)

**Files:**
- Modify: `internal/paths/paths.go`(`ServeCertMarkerPath()` + 常量)
- Modify: `internal/mcpserver/cert.go`(`LoadOrCreateServeCert` + `generateServeCert`)
- Modify: `internal/mcpserver/serve.go`(报错信息,若需要)
- Test: `internal/mcpserver/cert_test.go`

**Interfaces:**
- Consumes: `paths.ServeCertPath/ServeKeyPath`、`store.HardenACL`
- Produces:`paths.ServeCertMarkerPath()`;`LoadOrCreateServeCert` 第三态:cert 缺失 + marker 存在 → error(拒启动)

- [ ] **Step 1: Write the failing test**

追加到 `cert_test.go`。模拟"曾初始化过(cert 生成过)→ cert 文件被误删 → 再次调用应拒启动":

```go
func TestLoadOrCreateServeCert_DeletedCertRefusesRegen(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SSHMGR_SERVE_CERT", filepath.Join(dir, "serve-cert.pem"))
	t.Setenv("SSHMGR_SERVE_KEY", filepath.Join(dir, "serve-key.pem"))
	t.Setenv("SSHMGR_SERVE_MARKER", filepath.Join(dir, ".initialized"))

	// First call: generates cert + writes marker.
	_, _, fp1, err := LoadOrCreateServeCert()
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Simulate accidental cert deletion (NOT the marker).
	if err := os.Remove(filepath.Join(dir, "serve-cert.pem")); err != nil {
		t.Fatal(err)
	}

	// Second call: cert gone but marker present → MUST refuse to silently regen.
	_, _, fp2, err := LoadOrCreateServeCert()
	if err == nil {
		t.Fatalf("expected refusal when cert deleted but marker present; got new cert fp=%s (fp1=%s) — silent regen invalidates all client pins", fp2, fp1)
	}
	if !strings.Contains(err.Error(), "marker") && !strings.Contains(err.Error(), "delete") && !strings.Contains(err.Error(), "regenerate") {
		t.Logf("error message: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mcpserver/ -run TestLoadOrCreateServeCert_DeletedCertRefusesRegen -v`
Expected: FAIL —— 当前无 marker,删 cert 后第二次调用会静默重生(fp2 != fp1,err==nil),测试断言 err==nil 失败。

- [ ] **Step 3: Write minimal implementation**

(a) `paths.go` 加 marker 路径(沿用 ServeCertPath 模式):
```go
const ServeCertMarkerFilename = ".serve-cert-initialized"
// ServeCertMarkerPath returns the "cert already initialized" sentinel path.
// Env SSHMGR_SERVE_MARKER overrides (test). Presence of this marker with an
// ABSENT cert file means the cert was deleted out-of-band → refuse to silently
// regenerate (which would invalidate every client's pin). (xcheck F10)
func ServeCertMarkerPath() (string, error) {
	if v := os.Getenv("SSHMGR_SERVE_MARKER"); v != "" {
		return v, nil
	}
	dir, err := VaultDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ServeCertMarkerFilename), nil
}
```

(b) `cert.go` `LoadOrCreateServeCert`:在"Absent → generate"分支**之前**,检查 marker。改逻辑:cert 不存在时,若 marker 存在 → return error;若 marker 也不存在 → generate cert **+ 写 marker**。

在 `// Absent → generate` 分支(certificate stat 是 NotExist 那里)之前插入:
```go
// F10: if the marker exists but the cert is gone, the cert was deleted
// out-of-band. Regenerating would silently invalidate every client's pin.
// Refuse; operator must explicitly delete the marker to acknowledge regen.
markerPath, mErr := paths.ServeCertMarkerPath()
if mErr != nil {
	return "", "", "", mErr
}
if _, statErr := os.Stat(markerPath); statErr == nil {
	return "", "", "", fmt.Errorf("serve cert %s is missing but the initialization marker %s exists "+
		"(cert appears deleted out-of-band; refusing to silently regenerate — that would invalidate all client pins). "+
		"To regenerate deliberately, delete BOTH the marker and the cert, then re-enroll all clients", certPath, markerPath)
}
```
然后在 `generateServeCert` 成功后(或在 LoadOrCreateServeCert 的 generate 分支末尾)写 marker:
```go
markerPath, _ := paths.ServeCertMarkerPath()
if mpErr := atomicWriteFile(markerPath, []byte("initialized\n"), 0o600); mpErr != nil {
	return "", "", "", fmt.Errorf("write cert-init marker: %w", mpErr)
}
if err := store.HardenACL(markerPath); err != nil {
	return "", "", "", fmt.Errorf("harden marker ACL: %w", err)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mcpserver/ -run TestLoadOrCreateServeCert_DeletedCertRefusesRegen -v && go test ./internal/mcpserver/ -count=1`
Expected: PASS + 全包绿。确认 `TestLoadOrCreateServeCert_GenerateThenLoad`(幂等)和 `_CorruptReturnsError`(损坏)仍过 —— marker 不影响这两条(幂等:cert 存在走 load 分支,marker 无关;损坏:cert 存在但坏走 error 分支)。

- [ ] **Step 5: Commit**

```bash
git add internal/paths/paths.go internal/mcpserver/cert.go internal/mcpserver/cert_test.go
git commit -m "feat(mcpserver): init-marker refuses silent cert regen after deletion (xcheck F10)"
```

---

### Task 5: F12 — 补 pinningTransport 匿名对端(无证书)测试(pi)

**Files:**
- Test: `internal/mcpserver/serve_test.go`(或 cache_test.go,看 pinningTransport 测在哪)

**Interfaces:** 无代码改动,纯补测试。

- [ ] **Step 1: Write the failing-then-passing test**

`pinningTransport` 的 `len(cs.PeerCertificates)==0` 分支代码正确(返回 error)但无测试。补一个:起一个 **不配置证书** 的 TLS httptest server(client 会看到空 PeerCertificates),用 pinningTransport 连,断言返回 error 且错误含 "no certificate"。

注:`httptest.NewTLSServer` 默认带证书。要测"对端不发证书",需用 `httptest.NewUnstartedServer` + `TLS: &tls.Config{}`(空 Certificates,且不强制 RequireAnyClientCert —— 服务端不发证书的场景需 `GetCertificate` 返回 nil,实际较难构造)。**务实兜底**:若难构造"对端零证书"的 TLS server,改为**单元测试直接调 VerifyConnection 回调**:构造一个 `tls.ConnectionState{PeerCertificates: nil}`,调 `pinningTransport(fp)` 拿到的 `tls.Config.VerifyConnection`,断言它返回 error。读 `cache.go` `pinningTransport` 拿到 `tlsCfg` 后:
```go
func TestPinningTransport_NoPeerCert_HardFails(t *testing.T) {
	fp := "sha256:" + strings.Repeat("a", 64)
	tr, err := pinningTransport(fp)
	if err != nil {
		t.Fatal(err)
	}
	cb := tr.TLSClientConfig.VerifyConnection
	if cb == nil {
		t.Fatal("no VerifyConnection callback")
	}
	// Empty peer certs (anonymous / no-cert server).
	err = cb(tls.ConnectionState{PeerCertificates: nil})
	if err == nil {
		t.Fatal("expected error when server presents no certificate")
	}
	if !strings.Contains(err.Error(), "no certificate") {
		t.Fatalf("error should mention no certificate, got: %v", err)
	}
}
```
放 `cache_test.go`(pinningTransport 在 cli 包)。`tls` 需 import。

- [ ] **Step 2: Run test**

Run: `go test ./internal/cli/ -run TestPinningTransport_NoPeerCert_HardFails -v`
Expected: PASS(代码已正确,纯补覆盖)。

- [ ] **Step 3: Commit**

```bash
git add internal/cli/cache_test.go
git commit -m "test(cli): cover pinningTransport no-peer-cert branch (xcheck F12)"
```

---

### Task 6: 文档对齐(spec §3.5 / §4.1 / plan / 轮换 runbook / threat-model)

**Files:**
- Modify: `docs/superpowers/specs/2026-08-13-serve-auto-tls-fingerprint-design.md`(§3.5 tls.Config、§4.1 迁移前提、§4.2 错误矩阵加 hard-fail)
- Modify: `docs/superpowers/plans/2026-08-13-serve-auto-tls-fingerprint.md`(Task 4 的"不用 InsecureSkipVerify"标注为已推翻)
- Modify: `docs/multi-machine.md`(明文回退改 hard-fail + `--allow-plaintext`;迁移 runbook 顺序;轮换 runbook)
- Modify: `docs/threat-model.md`(§1.1 更新策略)

**Interfaces:** 无。

- [ ] **Step 1: spec §3.5 改 tls.Config 描述**

把"用 `tls.Config{VerifyConnection: ...}`,**不用** `InsecureSkipVerify`"改为(4 家共识):
> "用 `tls.Config{InsecureSkipVerify: true, VerifyConnection: func(cs){...}}`。**必须同时设 `InsecureSkipVerify: true`** —— 自签证书不在系统根池,默认 PKIX 链验证必失败,握手会在 `VerifyConnection` 跑之前中止,使 pin 校验成死代码。`InsecureSkipVerify:true` 跳过不可能成功的链验证,信任锚完全转移到 `VerifyConnection` 里的 SPKI pin(Go 官方 `VerifyConnection` 示例即此组合)。这是 HPKP/Tailscale 模式,pin 是唯一信任锚。"

- [ ] **Step 2: spec §4.1 + 错误矩阵改迁移/hard-fail**

§4.1 加一句:"⚠️ 迁移顺序铁律:先升全部工作机二进制并配 pin,**后**升 serve。升 serve 瞬间其变 TLS-only,旧明文 client 直连会失败 —— '不中断'仅在此协调前提下成立。" 错误矩阵(§4.2)把"client 无 pin → 同现状(明文回退)"改为"client 无 pin → **hard-fail**(除非 `--allow-plaintext`)"。

- [ ] **Step 3: plan Task 4 标注 InsecureSkipVerify 推翻**

在 plan 的 Task 4 描述处加一行:"> ⚠️ 已被 xcheck 评审推翻 + Task 5 F1 落地纠正:自签证书必须 `InsecureSkipVerify:true + VerifyConnection`,见 spec §3.5。" 不要删原文(历史),加标注。

- [ ] **Step 4: multi-machine.md 更新**

(a) Step 2 附近:把"无 pin 旧 client 回退明文"叙述改为"无 pin 默认拒连,需 `--allow-plaintext`"。
(b) 迁移 Runbook:强调"先升客户端后升 serve"顺序。
(c) 新增"密钥轮换 runbook"小节:私钥疑似泄露 → (1) 在 serve 删 cert + marker(`rm serve-cert.pem serve-key.pem .serve-cert-initialized`)→ (2) 重启 serve 生成新 key → (3) `serve cert-info` 取新指纹 → (4) 全客户端更新 `SSHMGR_SERVE_PIN` / 重发设备码。

- [ ] **Step 5: threat-model.md §1.1 更新**

明文回退那条改为 hard-fail 语义;补一句轮换引用 multi-machine 的轮换 runbook。

- [ ] **Step 6: Commit**

```bash
git add docs/superpowers/specs/2026-08-13-serve-auto-tls-fingerprint-design.md docs/superpowers/plans/2026-08-13-serve-auto-tls-fingerprint.md docs/multi-machine.md docs/threat-model.md
git commit -m "docs: align spec/plan/docs to xcheck findings (InsecureSkipVerify, hard-fail, migration order, key rotation)"
```

---

## Self-Review(写 plan 后自查)

**1. Spec/报告覆盖:**
- F8(kimi http:// bug)→ T1 ✅
- F4(明文回退 fail-open)/ F7(malformed pin fail-open)/ F11(token 内嵌 dead-feature)→ T2 ✅(用户拍板 hard-fail + opt-in)
- F9(KeyEncipherment)→ T3 ✅
- F10(误删 cert 静默重生)→ T4 ✅(用户拍板纳入)
- F12(peer-cert 无测试)→ T5 ✅
- 共识 A(spec §3.5)/ B(迁移)/ E(轮换)→ T6 ✅
- 共识 C(明文回退策略)→ T2(用户已拍板 hard-fail)✅
- 共识 D(KeyEncipherment)= F9 → T3 ✅

**未纳入(刻意):**
- Tier-3 策略主张("迁移截止日")—— 不是对错,YAGNI,不动。
- codex 的"env>flag unusual"—— 口味,不动。
- 备注:codex 提的 cache.go 注释"Windows ed25519"归因 —— 归 Task 2 改动区,若 T2 没碰则在 T3 cert.go 注释顺带修(已注明)。

**2. Placeholder scan:** 无 TBD/TODO;每个 NOTE 都是"实现前读现有 helper 确认名字"的可执行指引。

**3. Type consistency:** `resolvePin`/`pinningTransport`/`stripEmbeddedPin`/`LoadOrCreateServeCert`/`ServeCertMarkerPath` 跨任务一致;T2 改 `resolvePin` 注释不改签名,T4 加 marker 不改 cert fingerprint 返回。

**遗留(非阻断):**
- T4 marker 文件加一个新固定路径,与 cache-dek.key 的"路径不一致"已知问题(MEMORY 记录)叠加 —— 可在 T6 docs 记一笔 marker 位置。
- T2 的 malformed-pin 检测重复了 `resolvePin` 内部的 ParsePin(性能微忽,可接受;或重构 `resolvePin` 返回三态 —— 留 reviewer 判断)。

---

## Execution Handoff

Plan saved to `docs/superpowers/plans/2026-08-14-autotls-xcheck-fixes.md`。用户已指示制定修复+验证计划 —— 计划即此。下一步可由用户决定执行(subagent-driven 或 inline)。
