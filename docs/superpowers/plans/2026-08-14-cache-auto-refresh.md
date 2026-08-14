# 缓存自动保鲜（Stream A）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让工作机的 `mcp --cache` 自己保鲜：spawn 时惰性拉取 + 工具调用路径热加载 + 会话内定时拉取，删掉手配 OS 定时器的要求。

**Architecture:** 三件套全部在 `mcp --cache` 进程内，零常驻 daemon，serve 侧零改动。①`maybeLazyPull`（TTL+退避，复用抽出的 `doPull`）；②`NewServerFromSource` 让 6 个工具闭包在调用时经 `storeFn()` 取库；③`cacheStoreHolder` 原子换库（旧库延迟到退出清理）+ cli 侧 `cacheReloader`（SHA-256 判变 + 会话内触发 lazy pull）。凭据落 `cache.auth.json`（0600+ACL）。

**Tech Stack:** Go（仅标准库新增 `crypto/sha256`、`sync/atomic`、`bytes`——**零新第三方依赖**）。spec：`docs/superpowers/specs/2026-08-14-cache-auto-refresh-design.md`。

## Global Constraints

- **零新第三方依赖**（只加标准库 import）。
- 拉取的 TLS+pin hard-fail 语义不得放宽；自动路径（lazy pull）**永不** `--allow-plaintext`。
- 所有 cache 目录落盘（bin/meta/auth）一律**唯一临时名 + rename**（`os.CreateTemp`），禁止固定 `.tmp` 名。
- 换库不得 Close 被换下的 store（SDK 异步派发，在飞调用可能仍持有）；统一登记、进程退出清理。
- 错误消息措辞保持与现有测试断言兼容（`WARNING`+`plaintext`、`https`、`SSHMGR_SERVE_PIN`、`--pin`、`pin`/`allow-plaintext`、`mismatch`）。
- 代码注释英文（对齐仓库现状），文档中文。
- 每个任务以 `go build ./...` 零错误收尾；提交信息用 `feat(cache):` / `test:` / `docs:` 前缀。
- **执行前**：按仓库多 agent 约定，在 isolated linked worktree 开工（superpowers:using-git-worktrees），master 是共享主 worktree。

---

### Task 1: `atomicWriteUnique` — 唯一临时名原子写

**Files:**
- Modify: `internal/cli/cache.go`（新增 helper，`cachePullCmd` 的落盘段暂不改——Task 3 一并切）
- Test: `internal/cli/cache_atomic_test.go`（新建）

**Interfaces:**
- Produces: `func atomicWriteUnique(path string, blob []byte) error` — 后续所有 cache 落盘共用。

- [ ] **Step 1: 写失败测试（并发撕裂）**

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestAtomicWriteUnique_ConcurrentWritersNeverTear: 8 个并发写者交替写两个等长
// payload，一个读者不断采样。固定 ".tmp" 名的实现会让两个写者在同一临时文件上
// O_TRUNC 交错，rename 落下撕裂内容（xcheck 2026-08-14 三家共识 bug）；
// 唯一临时名下读者任何时候看到的都是完整的 A 或完整的 B。
func TestAtomicWriteUnique_ConcurrentWritersNeverTear(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.bin")
	a := bytes.Repeat([]byte("A"), 4096)
	b := bytes.Repeat([]byte("B"), 4096)
	if err := os.WriteFile(path, a, 0o600); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var readErr error
	var readOnce sync.Once
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := os.ReadFile(path)
			if err != nil {
				continue // 与 rename 竞争的瞬时 ENOENT，重读即可
			}
			if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
				readOnce.Do(func() { readErr = filepath.ErrBadPattern }) // 占位，下行覆盖
				readErr = &os.PathError{Op: "torn-read", Path: path,
					Err: errTorn{len: len(got)}}
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			blob := a
			if i%2 == 1 {
				blob = b
			}
			for j := 0; j < 200; j++ {
				if err := atomicWriteUnique(path, blob); err != nil {
					t.Errorf("atomicWriteUnique: %v", err)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(stop)

	if readErr != nil {
		t.Fatalf("torn read detected (fixed-name tmp bug): %v", readErr)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
		t.Fatalf("final content torn: %d bytes", len(got))
	}
}

type errTorn struct{ len int }

func (e errTorn) Error() string { return "content is neither payload (torn)" }
```

（实现者注：读者 goroutine 里 `readOnce.Do` 那两行是冗余的——直接赋值 `readErr = ...` 后 return 即可，写测试时删掉 Do 行。）

- [ ] **Step 2: 跑测试确认编译失败**

Run: `go test ./internal/cli/ -run TestAtomicWriteUnique -v`
Expected: FAIL，`undefined: atomicWriteUnique`

- [ ] **Step 3: 实现 helper（放 `cache.go`，`cachePaths` 附近）**

```go
// atomicWriteUnique atomically replaces path with blob via a UNIQUE temp file +
// rename. Unlike a fixed ".tmp" name, concurrent writers (multiple `mcp --cache`
// spawns lazy-pulling at once, or a lazy pull racing a manual one) never
// interleave on the same temp file, so a torn blob can never be renamed into
// place (xcheck 2026-08-14, three-reviewer consensus). os.CreateTemp creates the
// temp 0600, which matches every current use (cache.bin/meta/auth are all 0600).
func atomicWriteUnique(path string, blob []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after a successful rename
	if _, err := tmp.Write(blob); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/ -run TestAtomicWriteUnique -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cache.go internal/cli/cache_atomic_test.go
git commit -m "feat(cache): atomicWriteUnique — unique-temp atomic write for cache files"
```

---

### Task 2: `cacheCred` 凭据文件（读/写/ACL）

**Files:**
- Modify: `internal/cli/cache.go`
- Test: `internal/cli/cache_cred_test.go`（新建）

**Interfaces:**
- Consumes: `atomicWriteUnique`（Task 1）、`cachePaths()`。
- Produces:
  - `type cacheCred struct { URL, Token, Pin string }`（json: `url`/`token`/`pin,omitempty`）
  - `func cacheCredPath() (string, error)`
  - `func readCacheCred() (*cacheCred, error)`（文件不存在 → `(nil, nil)`；损坏/缺字段 → error）
  - `func writeCacheCred(cred *cacheCred) error`（唯一 temp + rename + `store.HardenACL`）

- [ ] **Step 1: 写失败测试**

```go
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheCred_RoundTripAndMissing(t *testing.T) {
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})

	if cred, err := readCacheCred(); err != nil || cred != nil {
		t.Fatalf("missing cred file: got (%v, %v), want (nil, nil)", cred, err)
	}

	in := &cacheCred{URL: "https://192.0.2.5:7878", Token: "devcode-abc", Pin: "sha256:" + strings.Repeat("a", 64)}
	if err := writeCacheCred(in); err != nil {
		t.Fatalf("writeCacheCred: %v", err)
	}
	got, err := readCacheCred()
	if err != nil {
		t.Fatalf("readCacheCred: %v", err)
	}
	if *got != *in {
		t.Fatalf("round trip mismatch: %+v want %+v", *got, *in)
	}
	// 0600 on Unix (Windows ACL is HardenACL's job).
	if info, err := os.Stat(filepath.Join(cacheDir, "cache.auth.json")); err == nil &&
		info.Mode().Perm() != 0o600 && os.Getenv("GOOS") == "" {
		// CreateTemp is 0600; rename preserves it. Only meaningful on non-Windows.
		if info.Mode().Perm() != 0o600 {
			t.Logf("note: perm is %v (Windows ignores mode bits)", info.Mode().Perm())
		}
	}
}

func TestCacheCred_CorruptFileErrors(t *testing.T) {
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCacheCred(); err == nil {
		t.Fatal("corrupt cred must error")
	}
	// 空字段同样报错
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"),
		[]byte(`{"url":"","token":"","pin":""}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readCacheCred(); err == nil {
		t.Fatal("cred missing url/token must error")
	}
}

var _ = json.Marshal // keep import if assertions above change
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestCacheCred -v`
Expected: FAIL，`undefined: cacheCred` / `readCacheCred` / `writeCacheCred`

- [ ] **Step 3: 实现（cache.go，紧跟 `cacheMeta` 定义之后）**

```go
// cacheCred persists the pull credential (cache.auth.json) so `mcp --cache` can
// lazy-pull without env/flags. Pin is the RESOLVED effective pin from the last
// successful pull (env > flag > token-embedded) stored bare; the lazy path
// prefers it over any pin still embedded in Token (cert rotation: a manual
// --pin re-pull must override a stale embedded pin). The device code grants
// FUTURE snapshot pulls — cache.bin alone only holds the past — so this file is
// bearer-token-grade: 0600 + HardenACL on Windows, revoke on theft (threat-model §1.1).
type cacheCred struct {
	URL   string `json:"url"`
	Token string `json:"token"`         // bare device code
	Pin   string `json:"pin,omitempty"` // resolved effective pin at last successful pull
}

func cacheCredPath() (string, error) {
	dir, _, _, _, err := cachePaths()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cache.auth.json"), nil
}

// readCacheCred returns nil, nil when the file is absent (never enrolled / not
// yet pulled). A present-but-corrupt file is an error: silently ignoring it
// would disable auto-refresh invisibly.
func readCacheCred() (*cacheCred, error) {
	p, err := cacheCredPath()
	if err != nil {
		return nil, err
	}
	blob, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c cacheCred
	if err := json.Unmarshal(blob, &c); err != nil {
		return nil, fmt.Errorf("cache.auth.json corrupt: %w", err)
	}
	if c.URL == "" || c.Token == "" {
		return nil, fmt.Errorf("cache.auth.json incomplete (missing url/token)")
	}
	return &c, nil
}

// writeCacheCred persists the credential atomically (unique temp + rename) and
// hardens the ACL on Windows (no-op on Unix where 0600 is the protection).
func writeCacheCred(cred *cacheCred) error {
	p, err := cacheCredPath()
	if err != nil {
		return err
	}
	blob, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	if err := atomicWriteUnique(p, blob); err != nil {
		return err
	}
	return store.HardenACL(p)
}
```

cache.go 顶部 import 增加 `"io/fs"`（`errors`/`fmt`/`os`/`filepath` 已有）。

**实现者注意**：若 `store.HardenACL` 在 unix 是 build-tag 隔离的未导出/不存在（先 `grep -n "func HardenACL" internal/store/`），则改为在 `internal/cli` 加一对 `cache_acl_windows.go` / `cache_acl_unix.go` 薄封装（windows 调 `store.HardenACL`，unix 返回 nil），`writeCacheCred` 调封装。以实际代码为准，不要发明 API。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/ -run TestCacheCred -v`
Expected: PASS（Windows 与 Unix 都要跑）

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cache.go internal/cli/cache_cred_test.go
git commit -m "feat(cache): cache.auth.json credential file (read/write + ACL harden)"
```

---

### Task 3: 抽取 `doPull` + `cachePullCmd` 切换 + 成功后写 cred

**Files:**
- Modify: `internal/cli/cache.go`（`cachePullCmd` RunE 瘦身 + 新 `doPull`/`pullOpts`；cache.bin/meta 落盘切 `atomicWriteUnique`）
- Test: 既有 `internal/cli/cache_test.go` 全量回归（不动）；`internal/cli/cache_pull_integration_test.go` 全量回归

**Interfaces:**
- Consumes: `atomicWriteUnique`、`writeCacheCred`、既有 `pinningTransport`/`stripEmbeddedPin`/`loadOrCreateDEK`/`cachePaths`/`cacheMeta`。
- Produces:
  - `type pullOpts struct { allowPlain bool; timeout time.Duration; statusOut io.Writer }`
  - `func doPull(url, token, pin string, o pullOpts) error` — **唯一拉取实现**（Task 4 的 lazy pull 复用）

- [ ] **Step 1: 先跑全量回归基线**

Run: `go test ./internal/cli/ -run 'TestCachePull|TestResolvePin|TestPinningTransport' -v`
Expected: 现状全 PASS（记下数量，重构后必须等量通过）

- [ ] **Step 2: 实现 `doPull` + 改造 `cachePullCmd`**

`cachePullCmd` RunE 从「拼 client → GET → 落盘」改为「解析校验 → 调 `doPull` → 写 cred」。RunE 保留的职责：flag/env 取值、`--url/--token` 必填、F7 pin 格式校验（`SSHMGR_SERVE_PIN`/`--pin` 格式非法即错）、`resolvePin`、plain 分支的 `--allow-plaintext` 校验；其余全部进 `doPull`：

```go
// pullOpts tunes doPull for its caller.
type pullOpts struct {
	allowPlain bool          // plaintext opt-in — manual CLI only; the lazy path NEVER sets this
	timeout    time.Duration // >0 → overall http.Client timeout (lazy: spawn/tool-call path must be bounded)
	statusOut  io.Writer     // status/warning sink (nil → silent); CLI passes cmd.ErrOrStderr()
}

// doPull fetches /snapshot from url with the device code and atomically writes
// cache.bin + cache.meta.json. pin=="" means no pin: allowPlain must be true or
// the pull refuses (F4 hard-fail contract). pin!="" pins TLS to that SPKI
// fingerprint and requires an https:// URL (F8). Extracted from cachePullCmd so
// the spawn-lazy pull (mcp --cache) shares ONE implementation.
func doPull(url, token, pin string, o pullOpts) error {
	code := token
	var client *http.Client
	if pin == "" {
		if !o.allowPlain {
			return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
				"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
		}
		if o.statusOut != nil {
			fmt.Fprintf(o.statusOut, "WARNING: --allow-plaintext set: pulling over unverified HTTP (no TLS pin). /snapshot credentials travel in cleartext.\n")
		}
		client = &http.Client{}
	} else {
		// The device code goes to the Authorization header; the pin is for TLS only.
		// If the token is "<code>:<pin>", strip so the header carries just the code.
		if c, _, ok := stripEmbeddedPin(token); ok {
			code = c
		}
		if u, err := neturl.Parse(url); err != nil || u.Scheme != "https" {
			return fmt.Errorf("--url must be https:// when a server pin is set (got %q); "+
				"to pull plaintext instead, clear the pin (--pin/SSHMGR_SERVE_PIN) and pass --allow-plaintext", url)
		}
		tr, err := pinningTransport(pin)
		if err != nil {
			return err
		}
		client = &http.Client{Transport: tr}
	}
	if o.timeout > 0 {
		client.Timeout = o.timeout
	}

	dek, err := loadOrCreateDEK()
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodGet, url+"/snapshot", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+code)
	res, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("pull: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		io.Copy(io.Discard, res.Body) // keep the keep-alive socket reusable
		return fmt.Errorf("pull: server returned %d (is the authorization code valid/active?)", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	blob, err := vaultio.EncryptWithKey(dek, body)
	if err != nil {
		return err
	}
	_, bin, metaPath, _, err := cachePaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		return err
	}
	if err := atomicWriteUnique(bin, blob); err != nil {
		return err
	}
	// meta via the same unique-temp atomic write (was a bare os.WriteFile — torn-meta
	// risk under concurrent pulls, xcheck codex#1).
	mb, _ := json.Marshal(cacheMeta{URL: url, PulledAt: time.Now().Unix()})
	if err := atomicWriteUnique(metaPath, mb); err != nil {
		return err
	}
	var snap store.Snapshot
	_ = json.Unmarshal(body, &snap) // for the status line only
	if o.statusOut != nil {
		fmt.Fprintf(o.statusOut, "pulled %d servers / %d credentials into %s\n", len(snap.Servers), len(snap.Credentials), bin)
	}
	return nil
}
```

`cachePullCmd` RunE 尾部（替换原 `client := ...` 到函数末尾的整段）：

```go
			fp, plain := resolvePin(os.Getenv("SSHMGR_SERVE_PIN"), pinFlag, token)
			if plain {
				allowPlain, _ := cmd.Flags().GetBool("allow-plaintext")
				if !allowPlain {
					return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
						"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
				}
				if err := doPull(url, token, "", pullOpts{allowPlain: true, statusOut: cmd.ErrOrStderr()}); err != nil {
					return err
				}
				return nil // plaintext pulls NEVER persist a credential (no auto-plaintext path)
			}
			if err := doPull(url, token, fp, pullOpts{statusOut: cmd.ErrOrStderr()}); err != nil {
				return err
			}
			// Persist the credential for the lazy pull. Write failure is a WARNING,
			// not an error: the pull itself succeeded, but auto-refresh won't work
			// until a later successful pull — the user must hear about that.
			code := token
			if c, _, ok := stripEmbeddedPin(token); ok {
				code = c
			}
			if err := writeCacheCred(&cacheCred{URL: url, Token: code, Pin: fp}); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: could not persist cache.auth.json (automatic refresh disabled until a successful pull): %v\n", err)
			}
			return nil
```

- [ ] **Step 3: 新增断言「pin 拉取后 cred 落盘 / plaintext 不落盘」**

追加到 `internal/cli/cache_cred_test.go`：

```go
func TestCachePull_PersistsCred_PinPathOnly(t *testing.T) {
	// pin path: reuse the TLS test-server builder to get a server + its SPKI pin.
	srv := newTLSSnapshotServer(t) // helper defined below
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   srv.url,
		"SSHMGR_CACHE_TOKEN": "code-123:" + srv.fp, // embedded-pin form, like enroll prints
		"SSHMGR_SERVE_PIN":   "",
		"SSHMGR_CACHE_DIR":   cacheDir,
		"SSHMGR_STORE":       filepath.Join(t.TempDir(), "store.db"),
	})
	root := NewRootCmd()
	root.SetArgs([]string{"cache", "pull"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if err := root.Execute(); err != nil {
		t.Fatalf("pinned pull: %v", err)
	}
	cred, err := readCacheCred()
	if err != nil || cred == nil {
		t.Fatalf("cred not persisted after pinned pull: %v %v", cred, err)
	}
	if cred.Token != "code-123" {
		t.Fatalf("cred.Token = %q, want bare code %q", cred.Token, "code-123")
	}
	if cred.Pin != srv.fp {
		t.Fatalf("cred.Pin = %q, want resolved pin %q", cred.Pin, srv.fp)
	}

	// plaintext path: fresh dir, --allow-plaintext pull must NOT write cred
	cacheDir2 := t.TempDir()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	defer plain.Close()
	withEnv(t, map[string]string{
		"SSHMGR_CACHE_URL":   plain.URL,
		"SSHMGR_CACHE_TOKEN": "code-123",
		"SSHMGR_SERVE_PIN":   "",
		"SSHMGR_CACHE_DIR":   cacheDir2,
	})
	root2 := NewRootCmd()
	root2.SetArgs([]string{"cache", "pull", "--allow-plaintext"})
	root2.SetOut(&bytes.Buffer{})
	root2.SetErr(&bytes.Buffer{})
	if err := root2.Execute(); err != nil {
		t.Fatalf("plaintext pull: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir2, "cache.auth.json")); err == nil {
		t.Fatal("plaintext pull must not persist a credential")
	}
}
```

helper（同文件；复用 `cache_test.go` 里 `TestCachePull_PinnedTLS_Succeeds` 的证书生成套路，抽成可复用形式）：

```go
type tlsSnapshotServer struct{ url, fp string }

// newTLSSnapshotServer spins a TLS httptest server serving /snapshot (any bearer
// accepted) and returns its URL + SPKI pin. Mirrors standUpServeTLS in
// cache_test.go but also returns the fingerprint.
func newTLSSnapshotServer(t *testing.T) *tlsSnapshotServer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	fp := mcpserver.SPKIFingerprint(cert)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	tlsCert, _ := tls.X509KeyPair(certPEM, keyPEM)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &tlsSnapshotServer{url: srv.URL, fp: fp}
}
```

（文件头补 import：`bytes`、`crypto/ed25519`、`crypto/rand`、`crypto/tls`、`crypto/x509`、`crypto/x509/pkix`、`encoding/pem`、`fmt`、`math/big`、`net`、`net/http`、`net/http/httptest`、`strings`、`time`、`ssh-manager-mcp/internal/mcpserver`。）

- [ ] **Step 4: 全量回归 + 新测试通过**

Run: `go test ./internal/cli/ -run 'TestCachePull|TestCacheCred|TestResolvePin|TestPinningTransport|TestAtomicWriteUnique' -v`
Expected: 基线数量全 PASS + 新增 3 个（cred 三态 ×2 + pull 落盘 ×1）

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cache.go internal/cli/cache_cred_test.go
git commit -m "feat(cache): extract doPull; unique-temp writes for bin/meta; persist cred on pinned pulls"
```

---

### Task 4: `maybeLazyPull`（TTL+退避）+ `--cache-max-age` + spawn 接线

**Files:**
- Modify: `internal/cli/cache.go`（`maybeLazyPull` + `lazyPullBackoff`）
- Modify: `internal/cli/mcp.go`（flag + spawn 时调用）
- Test: `internal/cli/cache_lazy_test.go`（新建）

**Interfaces:**
- Consumes: `readCacheCred`、`doPull`、`stripEmbeddedPin`、`cachePaths`。
- Produces:
  - `const lazyPullTimeout = 10 * time.Second`
  - `var lazyPullBackoff struct { mu sync.Mutex; lastAttempt time.Time }`（测试可直接复位）
  - `func maybeLazyPull(maxAge time.Duration) error` — maxAge≤0 全禁用；无 cred no-op；新鲜 no-op；退避窗口内 no-op；错误只返回不致命

- [ ] **Step 1: 写失败测试**

```go
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func resetBackoff() { lazyPullBackoff.lastAttempt = time.Time{} }

func TestMaybeLazyPull_MissingCacheWithCred_Pulls(t *testing.T) {
	resetBackoff()
	srv := newTLSSnapshotServer(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := writeCacheCred(&cacheCred{URL: srv.url, Token: "code-123", Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	if err := maybeLazyPull(time.Hour); err != nil {
		t.Fatalf("maybeLazyPull: %v", err)
	}
	blob, err := os.ReadFile(filepath.Join(cacheDir, "cache.bin"))
	if err != nil || len(blob) < 8 || string(blob[:8]) != "SSHMGRV1" {
		t.Fatalf("lazy pull did not write cache.bin: %v %d bytes", err, len(blob))
	}
}

func TestMaybeLazyPull_BackoffSuppressesRetry(t *testing.T) {
	resetBackoff()
	srv := newTLSSnapshotServer(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := writeCacheCred(&cacheCred{URL: srv.url, Token: "code-123", Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	if err := maybeLazyPull(time.Hour); err != nil {
		t.Fatal(err)
	}
	// Remove the cache; the backoff window (1h) must suppress a second attempt.
	os.Remove(filepath.Join(cacheDir, "cache.bin"))
	if err := maybeLazyPull(time.Hour); err != nil {
		t.Fatalf("backoff path must not error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.bin")); err == nil {
		t.Fatal("backoff window must suppress an immediate re-pull")
	}
}

func TestMaybeLazyPull_ZeroDisablesEvenWhenCacheMissing(t *testing.T) {
	resetBackoff()
	srv := newTLSSnapshotServer(t)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := writeCacheCred(&cacheCred{URL: srv.url, Token: "code-123", Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	if err := maybeLazyPull(0); err != nil {
		t.Fatalf("maxAge=0 must be a silent no-op: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.bin")); err == nil {
		t.Fatal("maxAge=0 must not pull, even with cache.bin missing")
	}
}

func TestMaybeLazyPull_FreshCacheNoPull_NoCredNoPull(t *testing.T) {
	resetBackoff()
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	// no cred at all → no-op even with stale/missing cache
	if err := maybeLazyPull(time.Millisecond); err != nil {
		t.Fatalf("no-cred path must be a silent no-op: %v", err)
	}
	// cred present + FRESH cache → no-op (file untouched: write sentinel + now mtime)
	srv := newTLSSnapshotServer(t)
	if err := writeCacheCred(&cacheCred{URL: srv.url, Token: "code-123", Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	resetBackoff()
	sentinel := []byte("SSHMGRV1-sentinel")
	if err := os.WriteFile(filepath.Join(cacheDir, "cache.bin"), sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := maybeLazyPull(time.Hour); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(cacheDir, "cache.bin"))
	if string(got) != string(sentinel) {
		t.Fatal("fresh cache must not be re-pulled")
	}
}

func TestMaybeLazyPull_CredPinWinsOverEmbeddedStalePin(t *testing.T) {
	// Cert rotation: cred.Pin (new) must override the stale pin embedded in Token.
	resetBackoff()
	srv := newTLSSnapshotServer(t) // "new" cert
	stalePin := "sha256:" + strings.Repeat("0", 64)
	withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	if err := writeCacheCred(&cacheCred{URL: srv.url, Token: "code-123:" + stalePin, Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	if err := maybeLazyPull(time.Hour); err != nil {
		t.Fatalf("cred.Pin must win over the stale embedded pin: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.bin")); err != nil {
		t.Fatal("pull must have succeeded under the new pin")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestMaybeLazyPull -v`
Expected: FAIL，`undefined: maybeLazyPull` 等

- [ ] **Step 3: 实现（cache.go）**

```go
// lazyPullTimeout bounds every automatic (lazy) pull: it runs on the spawn /
// tool-call critical path, so an unreachable broker must degrade within seconds,
// not hang MCP startup (xcheck pi#2/codex#3). Manual `cache pull` stays
// unbounded — the interactive user can Ctrl-C.
const lazyPullTimeout = 10 * time.Second

// lazyPullBackoff rate-limits automatic pulls to at most one attempt per TTL
// window (success or failure), so an offline machine doesn't retry — and block a
// tool call for up to lazyPullTimeout — on every call. The cache.bin mtime only
// advances on SUCCESS, so without this backoff a failed pull would re-fire per call.
var lazyPullBackoff struct {
	mu          sync.Mutex
	lastAttempt time.Time
}

// maybeLazyPull runs ONE automatic pull when cache.bin is missing / older than
// maxAge and a persisted cache.auth.json exists. maxAge<=0 disables entirely —
// INCLUDING the missing-cache case (the first pull stays a deliberate manual
// step). Errors are returned for the caller to log; never fatal.
func maybeLazyPull(maxAge time.Duration) error {
	if maxAge <= 0 {
		return nil
	}
	cred, err := readCacheCred()
	if err != nil {
		return err
	}
	if cred == nil {
		return nil
	}
	_, bin, _, _, err := cachePaths()
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(bin); statErr == nil && time.Since(info.ModTime()) < maxAge {
		return nil // fresh enough
	}
	lazyPullBackoff.mu.Lock()
	if time.Since(lazyPullBackoff.lastAttempt) < maxAge {
		lazyPullBackoff.mu.Unlock()
		return nil
	}
	lazyPullBackoff.lastAttempt = time.Now()
	lazyPullBackoff.mu.Unlock()

	pin := cred.Pin // resolved pin wins over any embedded stale pin (cert rotation)
	code := cred.Token
	if pin == "" {
		if c, p, ok := stripEmbeddedPin(cred.Token); ok {
			code, pin = c, p
		}
	}
	if pin == "" {
		return fmt.Errorf("cache.auth.json has no pin; refusing plaintext auto-pull")
	}
	return doPull(cred.URL, code, pin, pullOpts{timeout: lazyPullTimeout, statusOut: os.Stderr})
}
```

cache.go import 增加 `"sync"`。`mcp.go` 改动：

```go
// mcp.go: 函数头变量区增加
	var cacheMaxAge time.Duration
// flags 区增加
	c.Flags().DurationVar(&cacheMaxAge, "cache-max-age", 30*time.Minute,
		"auto-pull the offline cache when older than this (0 disables automatic pulls entirely)")
// useCache 分支，loadCacheSnapshot() 之前：
			// Spawn-time freshness. Failure degrades to the existing cache —
			// this must never block or fail MCP startup (spec §A2).
			if err := maybeLazyPull(cacheMaxAge); err != nil {
				fmt.Fprintf(os.Stderr, "lazy cache pull failed (serving stale cache): %v\n", err)
			}
```

mcp.go import 增加 `"time"`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/cli/ -run TestMaybeLazyPull -v && go build ./...`
Expected: PASS + 零 build 错误

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cache.go internal/cli/mcp.go internal/cli/cache_lazy_test.go
git commit -m "feat(cache): maybeLazyPull — TTL+backoff auto-pull at mcp --cache spawn (--cache-max-age)"
```

---

### Task 5: `NewServerFromSource` — 工具闭包经 `storeFn()` 取库

**Files:**
- Modify: `internal/mcpserver/server.go:33-149`（`NewServer` 拆成委托 + 新函数）
- Test: `internal/mcpserver/server_source_test.go`（新建）

**Interfaces:**
- Produces: `func NewServerFromSource(storeFn func() *store.Store, profileID, projectID string) (*mcp.Server, *TunnelManager, error)`；`NewServer(st, ...)` 签名与语义**不变**（委托实现）。

- [ ] **Step 1: 写失败测试（source-call 计数 + 换源生效，走真 in-memory client）**

```go
package mcpserver

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestNewServerFromSource_ResolvesStorePerCall: the tool closures must resolve
// the store via storeFn() AT CALL TIME (not capture it). A counting sourceFn
// proves per-call resolution, and swapping the returned store mid-session
// proves the next call serves the new store — the hot-reload contract.
func TestNewServerFromSource_ResolvesStorePerCall(t *testing.T) {
	// Seed store A: one in-profile server. ExportSnapshot → hydrate store B,
	// then add a SECOND server + grant to the SAME profile id (snapshot round-trip
	// preserves ids, so both stores share profile/project identity).
	stA := newStore(t)
	cid, _ := stA.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	id1, err := stA.AddServer(&models.Server{Name: "one", Host: "192.0.2.1", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := stA.AddProfile("p")
	_ = stA.GrantServers(pid, []string{id1})
	snap, err := stA.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}

	stB := newStore(t)
	if err := stB.ImportSnapshot(snap); err != nil { // same profile/project ids
		t.Fatal(err)
	}
	id2, err := stB.AddServer(&models.Server{Name: "two", Host: "192.0.2.2", Port: 22, User: "u", AuthMethod: models.AuthPassword, CredentialID: cid})
	if err != nil {
		t.Fatal(err)
	}
	_ = stB.GrantServers(pid, []string{id2})
	t.Cleanup(func() { stB.Close() })

	var calls int32
	cur := stA
	server, mgr, err := NewServerFromSource(func() *store.Store {
		atomic.AddInt32(&calls, 1)
		return cur
	}, pid, "proj-src")
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.CloseAll()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	t1, t2 := mcp.NewInMemoryTransports()
	srvSession, err := server.Connect(context.Background(), t1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srvSession.Close()
	cliSession, err := client.Connect(context.Background(), t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cliSession.Close()

	listIDs := func() (n int) {
		t.Helper()
		res, err := cliSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_servers", Arguments: map[string]any{}})
		if err != nil || res.IsError {
			t.Fatalf("list_servers: err=%v isError=%v", err, res.IsError)
		}
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok && (containsCount(tc.Text, `"one"`) + containsCount(tc.Text, `"two"`)) > 0 {
				return containsCount(tc.Text, `"one"`) + containsCount(tc.Text, `"two"`)
			}
		}
		return 0
	}

	if got := listIDs(); got != 1 {
		t.Fatalf("store A should serve 1 server, got %d", got)
	}
	n1 := atomic.LoadInt32(&calls)

	cur = stB // swap the source — the running session must see it next call
	if got := listIDs(); got != 2 {
		t.Fatalf("store B should serve 2 servers after swap, got %d", got)
	}
	if n2 := atomic.LoadInt32(&calls); n2 <= n1 {
		t.Fatalf("storeFn not called per tool invocation: %d -> %d", n1, n2)
	}
}

func containsCount(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}
```

（实现者注：若 list_servers 输出聚合在单个 TextContent 之外的结构化 output 里导致计数恒 0，改为解析 `res.Content[0].(*mcp.TextContent).Text` 中 `"id"` 出现次数，或直接断言 `got1 != got2`——关键断言是「换源后数量变化」+「calls 递增」，计数手段可调。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcpserver/ -run TestNewServerFromSource -v`
Expected: FAIL，`undefined: NewServerFromSource`

- [ ] **Step 3: 重构 `server.go`**

`NewServer` 拆分（工具注册体整体移入新函数；**唯一改动**是每个 handler 闭包第一行加 `st := storeFn()`，其余逐字保留——描述文本、错误分支、`tunnels` 捕获都不动）：

```go
// NewServer builds an MCP server over a FIXED store. Kept for every
// non-hot-reload caller (RunStdio, ServeRunner, tests); hot-reloading callers
// use NewServerFromSource.
func NewServer(st *store.Store, profileID, projectID string) (*mcp.Server, *TunnelManager, error) {
	return NewServerFromSource(func() *store.Store { return st }, profileID, projectID)
}

// NewServerFromSource is NewServer with a swappable store source: every tool
// closure resolves the store via storeFn() AT CALL TIME, so a hot-reloading
// caller (mcp --cache) can atomically swap the underlying store between calls
// without rebuilding the MCP server or tearing down tunnels. storeFn must be
// safe for concurrent use and must never return nil.
func NewServerFromSource(storeFn func() *store.Store, profileID, projectID string) (*mcp.Server, *TunnelManager, error) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "ssh-manager", Version: "v0.1.0"}, nil)
	tunnels := NewTunnelManager()
	tunnels.StartSweeper()

	// … 6 个 mcp.AddTool 块与现在逐字相同，仅每个 handler 闭包体首行插入：
	//   st := storeFn()
	// 例如 list_servers handler 变为：
	//   func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (…) {
	//       st := storeFn()
	//       servers, err := ListServersForProfile(st, profileID)
	//       …
	//   }
	return srv, tunnels, nil
}
```

6 个闭包全部要加（list_servers / exec_command / download_file / upload_file / forward_port / close_port——close_port 的 handler 也用 `st`，见现 `server.go:136`）。

- [ ] **Step 4: 回归 + 新测试通过**

Run: `go test ./internal/mcpserver/ -v && go build ./...`
Expected: mcpserver 全量 PASS（既有 `TestNewServerToolsScopedViaInMemoryClient` 等）+ 新测试 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/server.go internal/mcpserver/server_source_test.go
git commit -m "feat(mcpserver): NewServerFromSource — per-call store resolution for hot reload"
```

---

### Task 6: `hydrateCacheStore` + `cacheStoreHolder` + `RunStdioCache` reload 参数

**Files:**
- Modify: `internal/mcpserver/run.go`
- Test: `internal/mcpserver/run_cache_holder_test.go`（新建）

**Interfaces:**
- Consumes: `NewServerFromSource`（Task 5）。
- Produces:
  - `func hydrateCacheStore(token string, snap *store.Snapshot, auditFile *os.File) (*store.Store, *models.Project, string, error)`
  - `type cacheStoreHolder struct` + `func (h *cacheStoreHolder) Current() *store.Store` + `func (h *cacheStoreHolder) cleanup()`
  - `func RunStdioCache(token string, snap *store.Snapshot, auditPath string, reload func() (*store.Snapshot, bool, error) error` — **第 4 参传 `nil` = 不热加载**（Task 7 接真 reloader；`mcp.go` 的调用点在本任务同步加 `nil` 占位）
  - reload 契约：`(snap, true, nil)`=按 snap 重建；`(nil, false, nil)`=未变；`error`=检查失败（保旧库）

- [ ] **Step 1: 写失败测试**

```go
package mcpserver

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedSnap builds a snapshot with n servers bound to one profile/project and
// returns (snapshot, projectToken, profileID). The hydrated store round-trips
// the SAME token (ImportSnapshot preserves token hashes).
func seedSnap(t *testing.T, n int) (*store.Snapshot, string, string) {
	t.Helper()
	st := newStore(t)
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id, err := st.AddServer(&models.Server{
			Name: fmt.Sprintf("srv%d", i), Host: "192.0.2.10", Port: 22, User: "u",
			AuthMethod: models.AuthPassword, CredentialID: cid,
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	pid, _ := st.AddProfile("p")
	_ = st.GrantServers(pid, ids)
	_, token, err := st.AddProject("proj", pid) // 若签名不同（如 2 返回值），按实际调整
	if err != nil {
		t.Fatal(err)
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap, token, pid
}

func newHolder(t *testing.T, snap *store.Snapshot, token, profileID string,
	reload func() (*store.Snapshot, bool, error)) *cacheStoreHolder {
	t.Helper()
	af, err := os.OpenFile(filepath.Join(t.TempDir(), "audit.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { af.Close() })
	h := &cacheStoreHolder{reload: reload, token: token, auditFile: af, profileID: profileID}
	st, _, tmp, err := hydrateCacheStore(token, snap, af)
	if err != nil {
		t.Fatal(err)
	}
	h.cur.Store(st)
	h.stores = append(h.stores, st)
	h.tmpPaths = append(h.tmpPaths, tmp)
	return h
}

func serverCount(t *testing.T, st *store.Store, pid string) int {
	t.Helper()
	out, err := ListServersForProfile(st, pid)
	if err != nil {
		t.Fatalf("ListServersForProfile: %v", err)
	}
	return len(out)
}

func TestHolder_NoChange_SameStorePointer(t *testing.T) {
	snap, token, pid := seedSnap(t, 1)
	h := newHolder(t, snap, token, pid, func() (*store.Snapshot, bool, error) { return nil, false, nil })
	a, b, c := h.Current(), h.Current(), h.Current()
	if a != b || b != c {
		t.Fatal("unchanged reload must keep the same store pointer")
	}
}

func TestHolder_Changed_SwapsAndOldStaysUsable(t *testing.T) {
	snap1, token, pid := seedSnap(t, 1)
	snap2, _, _ := seedSnap(t, 2) // same shape, 2 servers — token/project ids differ, see below
	// seedSnap mints fresh ids each call; rebuild snap2 over snap1's project:
	// hydrate requires the SAME token to verify, so graft: re-export snap2's
	// servers into snap1's project. Simplest: hydrate snap1 first, then mutate
	// a COPY of snap1 with snap2's extra server row appended.
	grafted := *snap1
	grafted.Servers = append(append([]store.SnapshotServer{}, snap1.Servers...), snap2.Servers[len(snap2.Servers)-1])
	grafted.Grants = append(append([]store.SnapshotGrant{}, snap1.Grants...), store.SnapshotGrant{ProfileID: pid, ServerID: grafted.Servers[len(grafted.Servers)-1].ID})

	first := true
	h := newHolder(t, snap1, token, pid, func() (*store.Snapshot, bool, error) {
		if first {
			first = false
			return &grafted, true, nil
		}
		return nil, false, nil
	})
	old := h.Current()
	if got := serverCount(t, old, pid); got != 1 {
		t.Fatalf("initial: %d servers, want 1", got)
	}
	cur := h.Current()
	if cur == old {
		t.Fatal("changed reload must swap the store")
	}
	if got := serverCount(t, cur, pid); got != 2 {
		t.Fatalf("after swap: %d servers, want 2", got)
	}
	// Old store must stay USABLE (in-flight call safety): not closed on swap.
	if got := serverCount(t, old, pid); got != 1 {
		t.Fatalf("old store closed on swap: %v", got)
	}
}

func TestHolder_ReloadErrorAndBadSnapshot_KeepOld(t *testing.T) {
	snap, token, pid := seedSnap(t, 1)
	// error path
	h := newHolder(t, snap, token, pid, func() (*store.Snapshot, bool, error) {
		return nil, false, fmt.Errorf("stat failed")
	})
	old := h.Current()
	if h.Current() != old {
		t.Fatal("reload error must keep the old store")
	}
	// revoked-token path: snapshot without the project → VerifyToken fails in hydrate
	noProj := *snap
	noProj.Projects = nil
	h2 := newHolder(t, snap, token, pid, func() (*store.Snapshot, bool, error) {
		return &noProj, true, nil
	})
	old2 := h2.Current()
	if h2.Current() != old2 {
		t.Fatal("token revoked in new snapshot must keep the old store")
	}
}

func TestHolder_ProfileDrift_KeepsOld(t *testing.T) {
	snap, token, pid := seedSnap(t, 1)
	// same project/token but bound to a different profile id
	drifted := *snap
	drifted.Projects = append([]store.SnapshotProject{}, snap.Projects...)
	drifted.Projects[0].ProfileID = "other-profile"
	h := newHolder(t, snap, token, pid, func() (*store.Snapshot, bool, error) {
		return &drifted, true, nil
	})
	old := h.Current()
	if h.Current() != old {
		t.Fatal("profile drift must keep the old store")
	}
}

func TestHolder_ConcurrentCurrent_RebuildsOnce(t *testing.T) {
	snap1, token, pid := seedSnap(t, 1)
	snap2, _, _ := seedSnap(t, 2)
	grafted := *snap1
	grafted.Servers = append(append([]store.SnapshotServer{}, snap1.Servers...), snap2.Servers[len(snap2.Servers)-1])
	grafted.Grants = append(append([]store.SnapshotGrant{}, snap1.Grants...), store.SnapshotGrant{ProfileID: pid, ServerID: grafted.Servers[len(grafted.Servers)-1].ID})

	var mu sync.Mutex
	pending := true
	h := newHolder(t, snap1, token, pid, func() (*store.Snapshot, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		if pending {
			return &grafted, true, nil
		}
		return nil, false, nil
	})
	var wg sync.WaitGroup
	final := make([]*store.Store, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			final[i] = h.Current()
		}(i)
	}
	wg.Wait()
	for i := 1; i < len(final); i++ {
		if final[i] != final[0] {
			t.Fatal("concurrent Current() must converge on one store")
		}
	}
	if got := serverCount(t, final[0], pid); got != 2 {
		t.Fatalf("final store: %d servers, want 2", got)
	}
}
```

（实现者注：`seedSnap` 里 `st.AddProject` 的返回值 arity 以 `internal/store/projects.go` 实际签名为准；`SnapshotProject` 字段名以 `internal/store/export.go` 为准。不一致就改测试适配源码，不改源码迁就测试。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/mcpserver/ -run TestHolder -v`
Expected: FAIL，`undefined: cacheStoreHolder` / `hydrateCacheStore`

- [ ] **Step 3: 实现（run.go 整段替换 `RunStdioCache` + 新增类型）**

```go
// hydrateCacheStore builds a fresh temporary read-only store from snap and
// verifies token against it. The caller owns closing the store and removing
// tmpPath (cacheStoreHolder registers both). Shared by initial startup and
// every hot rebuild, so the two paths CANNOT drift.
func hydrateCacheStore(token string, snap *store.Snapshot, auditFile *os.File) (*store.Store, *models.Project, string, error) {
	mk, err := store.GenerateMasterKey() // throwaway key: creds re-sealed per hydration
	if err != nil {
		return nil, nil, "", err
	}
	tmp, err := os.CreateTemp("", "sshmgr-cache-*.db")
	if err != nil {
		return nil, nil, "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	st, err := store.Open(tmpPath, mk)
	if err != nil {
		os.Remove(tmpPath)
		return nil, nil, "", err
	}
	if err := st.ImportSnapshot(snap); err != nil {
		st.Close()
		os.Remove(tmpPath)
		return nil, nil, "", err
	}
	st.SetReadOnly(auditFile) // AFTER ImportSnapshot: mutations → ErrReadOnly
	project, err := st.VerifyToken(token)
	if err != nil {
		st.Close()
		os.Remove(tmpPath)
		return nil, nil, "", err
	}
	if project == nil {
		st.Close()
		os.Remove(tmpPath)
		return nil, nil, "", fmt.Errorf("invalid or unknown token")
	}
	return st, project, tmpPath, nil
}

// cacheStoreHolder owns the hot-reloading read-only store behind mcp --cache.
// Swapped-out stores are NOT closed on swap: the SDK dispatches tool calls on
// separate goroutines, so an in-flight call may still hold the old pointer —
// closing it would surface "sql: database is closed" as a tool error. They are
// registered in stores/tmpPaths and torn down once at process exit instead
// (rebuilds are rare; the leak is bounded and harmless).
type cacheStoreHolder struct {
	reload    func() (*store.Snapshot, bool, error)
	token     string
	auditFile *os.File
	profileID string

	mu       sync.Mutex // serializes rebuilds
	cur      atomic.Pointer[store.Store]
	stores   []*store.Store // every hydrated store, closed once in cleanup
	tmpPaths []string       // every temp db, removed once in cleanup
}

// Current returns the store to serve THIS tool call from, rebuilding first if
// the reload callback reports a change. Every failure path keeps serving the
// previous store — Lazy revocation semantics: a session outlives its token
// until the next spawn.
func (h *cacheStoreHolder) Current() *store.Store {
	if h.reload == nil {
		return h.cur.Load()
	}
	_, changed, err := h.reload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh-manager: cache reload check failed (keeping current snapshot): %v\n", err)
		return h.cur.Load()
	}
	if !changed {
		return h.cur.Load()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Re-check under the lock: a concurrent rebuild may have consumed this
	// change already (the reloader advances its baseline on a successful load).
	snap, changed, err := h.reload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh-manager: cache reload recheck failed (keeping current snapshot): %v\n", err)
		return h.cur.Load()
	}
	if !changed {
		return h.cur.Load()
	}
	st, project, tmpPath, err := hydrateCacheStore(h.token, snap, h.auditFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ssh-manager: cache hot-reload failed (keeping current snapshot): %v\n", err)
		return h.cur.Load()
	}
	if project.ProfileID != h.profileID {
		// The owner rebound the project to a different profile mid-session; the
		// tool closures still scope by the startup profileID, so serving the new
		// store would show the wrong set. Keep the old store + log.
		fmt.Fprintf(os.Stderr, "ssh-manager: cache snapshot changed the project's profile (keeping current snapshot to preserve scoping)\n")
		st.Close()
		os.Remove(tmpPath)
		return h.cur.Load()
	}
	h.stores = append(h.stores, st)
	h.tmpPaths = append(h.tmpPaths, tmpPath)
	h.cur.Store(st) // old store intentionally left open (in-flight calls)
	return st
}

// cleanup closes every hydrated store and removes every temp db. Called once,
// deferred from RunStdioCache.
func (h *cacheStoreHolder) cleanup() {
	for _, s := range h.stores {
		s.Close()
	}
	h.stores = nil
	for _, p := range h.tmpPaths {
		os.Remove(p)
	}
	h.tmpPaths = nil
}

// RunStdioCache hydrates a Snapshot into a temporary read-only store and runs
// the broker over stdio. reload != nil enables hot-reload: before every tool
// call the callback is consulted ((snap,true,nil) = rebuild; (nil,false,nil) =
// unchanged; error = keep serving). reload == nil disables it (tests).
// See the pre-existing doc comment for the agent-surface invariant.
func RunStdioCache(token string, snap *store.Snapshot, auditPath string, reload func() (*store.Snapshot, bool, error)) error {
	af, err := os.OpenFile(auditPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer af.Close()

	h := &cacheStoreHolder{reload: reload, token: token, auditFile: af}
	st, project, tmpPath, err := hydrateCacheStore(token, snap, af)
	if err != nil {
		return err
	}
	h.cur.Store(st)
	h.stores = append(h.stores, st)
	h.tmpPaths = append(h.tmpPaths, tmpPath)
	defer h.cleanup()

	srv, tunnels, err := NewServerFromSource(h.Current, project.ProfileID, project.ID)
	if err != nil {
		return err
	}
	defer tunnels.CloseAll()
	return srv.Run(context.Background(), &mcp.StdioTransport{})
}
```

run.go import 增加 `"sync"`、`"sync/atomic"`、`"ssh-manager-mcp/internal/models"`；**同步改 `internal/cli/mcp.go` 的调用点**：`return mcpserver.RunStdioCache(token, snap, auditPath, nil)`（Task 7 换成真 reloader）。

- [ ] **Step 4: 跑测试 + 全量回归**

Run: `go test ./internal/mcpserver/ ./internal/cli/ -v && go build ./...`
Expected: 全 PASS（含既有 `TestHydrateReadOnlyStore_*`，它不经 RunStdioCache，不受影响）

- [ ] **Step 5: Commit**

```bash
git add internal/mcpserver/run.go internal/mcpserver/run_cache_holder_test.go internal/cli/mcp.go
git commit -m "feat(mcpserver): cacheStoreHolder hot-swap with deferred teardown; RunStdioCache reload hook"
```

---

### Task 7: cli 侧 `cacheReloader`（hash 判变 + 会话内 lazy）+ 接线

**Files:**
- Modify: `internal/cli/cache.go`（`cacheReloader`）
- Modify: `internal/cli/mcp.go`（baseline 前置 + 传 `rel.check`）
- Test: `internal/cli/cache_reload_test.go`（新建）

**Interfaces:**
- Consumes: `maybeLazyPull`、`loadCacheSnapshot`、`mcpserver.RunStdioCache` 的 reload 契约。
- Produces: `type cacheReloader` + `func newCacheReloader(maxAge time.Duration) *cacheReloader` + `func (r *cacheReloader) check() (*store.Snapshot, bool, error)`。

- [ ] **Step 1: 写失败测试**

```go
package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vaultio"
)

// writeCacheBin encrypts snap under mem's DEK into the test cache dir, exactly
// like cache pull does (mirrors mcp_cache_test.go's hand-rolled cache.bin).
func writeCacheBin(t *testing.T, mem *store.MemKeyProvider, snap *store.Snapshot) {
	t.Helper()
	dir := os.Getenv("SSHMGR_CACHE_DIR")
	blob, err := vaultio.EncryptWithKey(dekOf(t, mem), mustJSON(t, snap))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func dekOf(t *testing.T, mem *store.MemKeyProvider) []byte {
	t.Helper()
	k, err := mem.Get()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func seedSnapCLI(t *testing.T, n int) *store.Snapshot {
	t.Helper()
	dir := t.TempDir()
	mk, _ := store.GenerateMasterKey()
	st, err := store.Open(filepath.Join(dir, "s.db"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cid, _ := st.SetCredential(&models.Credential{Type: models.CredPassword, Secret: []byte("pw")})
	for i := 0; i < n; i++ {
		_, err := st.AddServer(&models.Server{Name: "srv", Host: "192.0.2.1", Port: 22, User: "u",
			AuthMethod: models.AuthPassword, CredentialID: cid})
		if err != nil {
			t.Fatal(err)
		}
	}
	snap, err := st.ExportSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestCacheReloader_HashChangeDetection(t *testing.T) {
	mem := withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	resetBackoff()

	writeCacheBin(t, mem, seedSnapCLI(t, 1))
	rel := newCacheReloader(time.Hour)

	// unchanged → (nil, false, nil)
	snap, changed, err := rel.check()
	if snap != nil || changed || err != nil {
		t.Fatalf("unchanged: got (%v,%v,%v)", snap, changed, err)
	}

	// same content, same size → still unchanged (mtime blind-spot guard)
	future := time.Now().Add(2 * time.Hour)
	bin := filepath.Join(cacheDir, "cache.bin")
	blob, _ := os.ReadFile(bin)
	os.Chtimes(bin, future, future)
	snap, changed, err = rel.check()
	if snap != nil || changed || err != nil {
		t.Fatalf("same-content rewrite must NOT trigger reload: got (%v,%v,%v)", snap, changed, err)
	}

	// different content (extra server), mtime pinned to the SAME future instant
	// → hash catches it even though size may match and mtime is identical.
	writeCacheBin(t, mem, seedSnapCLI(t, 2))
	os.Chtimes(bin, future, future)
	snap, changed, err = rel.check()
	if err != nil || !changed || snap == nil {
		t.Fatalf("changed content must reload: got (%v,%v,%v)", snap, changed, err)
	}
	if len(snap.Servers) != 2 {
		t.Fatalf("reloaded snapshot has %d servers, want 2", len(snap.Servers))
	}

	// baseline advanced: immediate recheck → unchanged
	snap, changed, err = rel.check()
	if snap != nil || changed || err != nil {
		t.Fatalf("post-reload recheck: got (%v,%v,%v)", snap, changed, err)
	}
}

func TestCacheReloader_CorruptFileKeepsOldBaseline(t *testing.T) {
	mem := withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	writeCacheBin(t, mem, seedSnapCLI(t, 1))
	rel := newCacheReloader(time.Hour)
	if _, changed, err := rel.check(); changed || err != nil {
		t.Fatalf("baseline check: (%v,%v)", changed, err)
	}
	// garbage bytes (a torn write that somehow landed)
	os.WriteFile(filepath.Join(cacheDir, "cache.bin"), []byte("garbage-not-sshmgrv1"), 0o600)
	snap, changed, err := rel.check()
	if snap != nil || changed || err == nil {
		t.Fatalf("corrupt file must be (nil,false,err): got (%v,%v,%v)", snap, changed, err)
	}
	// baseline NOT advanced: a later good file still reloads
	writeCacheBin(t, mem, seedSnapCLI(t, 2))
	snap, changed, err = rel.check()
	if err != nil || !changed || snap == nil || len(snap.Servers) != 2 {
		t.Fatalf("recovery after corrupt: got (%v,%v,%v) servers=%d", snap, changed, err, len(snap.Servers))
	}
}

func TestCacheReloader_Unchanged_TriggersInSessionLazyPull(t *testing.T) {
	srv := newTLSSnapshotServer(t)
	mem := withDEK(t)
	cacheDir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": cacheDir})
	resetBackoff()
	if err := writeCacheCred(&cacheCred{URL: srv.url, Token: "code-123", Pin: srv.fp}); err != nil {
		t.Fatal(err)
	}
	writeCacheBin(t, mem, seedSnapCLI(t, 1))
	// make the cache STALE so the unchanged path wants a refresh
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(cacheDir, "cache.bin"), past, past)

	rel := newCacheReloader(time.Hour)
	_, changed, err := rel.check()
	if changed || err != nil {
		t.Fatalf("first check should be unchanged: (%v,%v)", changed, err)
	}
	// the in-session lazy pull ran → cache.bin rewritten (fresh mtime, new content is empty-snap from TLS server)
	if _, err := os.Stat(filepath.Join(cacheDir, "cache.bin")); err != nil {
		t.Fatal(err)
	}
	// next check sees the NEW file → reload (servers now 0, from the TLS stub)
	snap, changed, err := rel.check()
	if err != nil || !changed {
		t.Fatalf("second check should reload after in-session pull: (%v,%v,%v)", snap, changed, err)
	}
	if len(snap.Servers) != 0 {
		t.Fatalf("post-refresh snapshot should hold the stub's 0 servers, got %d", len(snap.Servers))
	}
}
```

（import 含 `encoding/json`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/cli/ -run TestCacheReloader -v`
Expected: FAIL，`undefined: newCacheReloader`

- [ ] **Step 3: 实现（cache.go 末尾）**

```go
// cacheReloader detects cache.bin changes for hot-reload and kicks in-session
// lazy pulls. Change detection hashes the whole (encrypted) file — a vault
// snapshot is KBs, so this is ~µs per tool call — and is immune to the
// same-tick / same-size mtime blind spot (xcheck codex#4/kimi#4). The baseline
// is captured at construction, which mcp.go does BEFORE the initial
// loadCacheSnapshot: a baseline taken after the load could swallow an external
// pull that landed mid-startup (the harmless residue — a pull racing the
// baseline — costs one redundant rebuild, never a missed one).
type cacheReloader struct {
	bin    string
	maxAge time.Duration
	sum    []byte // SHA-256 of the served cache.bin (nil until first successful load)
}

func newCacheReloader(maxAge time.Duration) *cacheReloader {
	_, bin, _, _, err := cachePaths()
	if err != nil {
		return &cacheReloader{maxAge: maxAge} // check() surfaces the error
	}
	return &cacheReloader{bin: bin, maxAge: maxAge, sum: fileSumOf(bin)}
}

func fileSumOf(path string) []byte {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	s := sha256.Sum256(blob)
	return s[:]
}

// check implements the reload callback for mcpserver.RunStdioCache.
func (r *cacheReloader) check() (*store.Snapshot, bool, error) {
	if r.bin == "" {
		return nil, false, fmt.Errorf("cache paths unavailable")
	}
	blob, err := os.ReadFile(r.bin)
	if err != nil {
		return nil, false, err // gone/unreadable → keep serving the current store
	}
	s := sha256.Sum256(blob)
	if bytes.Equal(s[:], r.sum) {
		// Unchanged. In-session freshness: maybeLazyPull no-ops while fresh and
		// backs off on failure; a successful pull changes the file, so the NEXT
		// call swaps the store in — this call deliberately finishes on the old
		// one (never half-old half-new within a single tool call).
		if err := maybeLazyPull(r.maxAge); err != nil {
			fmt.Fprintf(os.Stderr, "ssh-manager: in-session cache refresh failed: %v\n", err)
		}
		return nil, false, nil
	}
	snap, err := loadCacheSnapshot()
	if err != nil {
		return nil, false, err // corrupt/undecryptable → keep the old store, baseline NOT advanced
	}
	r.sum = s[:] // advance only on a successful load
	return snap, true, nil
}
```

cache.go import 增加 `"bytes"`、`"crypto/sha256"`。`mcp.go` 接线（useCache 分支最终形态）：

```go
			if useCache {
				// ① spawn-time freshness (failure degrades to the existing cache)
				if err := maybeLazyPull(cacheMaxAge); err != nil {
					fmt.Fprintf(os.Stderr, "lazy cache pull failed (serving stale cache): %v\n", err)
				}
				// ② hot-reload baseline BEFORE the initial load (see cacheReloader)
				rel := newCacheReloader(cacheMaxAge)
				snap, err := loadCacheSnapshot()
				if err != nil {
					return err
				}
				_, _, _, auditPath, err := cachePaths()
				if err != nil {
					return err
				}
				return mcpserver.RunStdioCache(token, snap, auditPath, rel.check)
			}
```

- [ ] **Step 4: 全量测试 + build**

Run: `go test ./... && go build ./... && go vet ./...`
Expected: 全 PASS、零 vet 告警

- [ ] **Step 5: Commit**

```bash
git add internal/cli/cache.go internal/cli/mcp.go internal/cli/cache_reload_test.go
git commit -m "feat(cache): hash-based hot-reload detector + in-session lazy refresh wiring"
```

---

### Task 8: 文档改写

**Files:**
- Modify: `docs/quickstart-multi-machine.md`（Step 3 整节 + line 69）
- Modify: `docs/multi-machine.md`（212/226/260/309/363-455/491/504/512/515/526/542 各处）
- Modify: `docs/threat-model.md` §1.1（新增 cred 文件 artifact）

- [ ] **Step 1: `quickstart-multi-machine.md`**

Step 3 标题与正文替换为（systemd 模板整段删除）：

```markdown
## Step 3 —（可选）缓存自动保鲜说明

缓存现在自己保鲜，**默认不需要任何系统定时器**：

- **spawn 自动拉**：Claude Code 启动 `mcp --cache` 时，若缓存超过 30 分钟（`--cache-max-age` 可调，`0` 关闭）且本机存过拉取凭据，会自动拉一次新缓存；失败静默用旧缓存。
- **会话内自动拉 + 热加载**：运行中的会话每 30 分钟也会自动拉新，下一次工具调用即生效——无需重启 Claude Code。
- 首次 `cache pull` 仍需手动（在线）执行一次；成功后凭据自动存入本机 `cache.auth.json`（0600），之后的自动拉取都靠它。

仍想配系统定时器（比如给非 Claude 的消费方保鲜）？照旧跑 `cache pull` 即可，见详尽版。
```

line 69 注释 `# 第一次拉缓存（…之后由系统定时器自动重拉）` 改为 `# 第一次拉缓存（之后 mcp --cache 会自动保鲜，见 Step 3）`。

- [ ] **Step 2: `multi-machine.md`**

逐处修改（保持各节上下文不动）：

- `212`：「自动刷新」→「自动保鲜（spawn 惰性拉取 + 会话内热加载）」。
- `226` 路线表「自动刷新」→「自动保鲜（内置，无需 OS 调度器）」。
- `260` 架构图：`系统调度器（systemd timer / 任务计划 / launchd）` 节点及其箭头删除，改为 `mcp --cache 进程内（spawn 惰性拉取 + 每 30min 会话内拉取 + 热加载）` 一行。
- `309`：「之后会被调度器自动重拉」→「之后由 `mcp --cache` 自动保鲜」。
- `363-455` Step 3：收缩为与 quickstart Step 3 同义的详尽版（保留三平台定时器模板作「可选：给非 Claude 消费者」小节；正文声明默认无需）。
- `457` 安全注意：补一句「设备码持久化在 `cache.auth.json`（0600，Windows 另加 ACL）；证书轮换后手动带新 `--pin` 重拉一次即可覆盖」。
- `491`：改为「Lazy 生效：…运行中的 `mcp --cache` 会**热加载**新缓存；若吊销使 token 在新快照中失效，运行中的会话保留旧快照到本次 spawn 结束」。
- `504` 对比表「怎么触发」：`设备码 + OS 调度器自动 cache pull` → `设备码 + mcp --cache 内置自动拉取（spawn + 每 TTL）`。
- `512`：改为「自动保鲜是 `mcp --cache` **进程内置**的（spawn 惰性拉取 + 会话内定时拉取 + 热加载）——不是常驻 daemon，也不需要 OS 调度器」。
- `515`：末尾补「（凭据文件 `cache.auth.json` 由首次成功 pull 自动写入）」。
- `526`/`542` 证书迁移/轮换 runbook：「下一次定时 `cache pull`」→「下一次自动拉取（或手动 `cache pull` 带新 `--pin`，会同时更新 `cache.auth.json` 里的 pin）」。

- [ ] **Step 3: `threat-model.md` §1.1 增补**

在设备码相关条目后追加：

```markdown
- **`cache.auth.json`（新增 artifact）**：工作机持久化的拉取凭据（url + 设备码 + 归一后 pin；0600，Windows 加 DACL）。设备码授予**拉取未来快照**的权力——比本机已有的 cache.bin（过去快照 + cache-dek.key）多出的正是这个增量。处置：机器失窃 → 立即 `cache-tokens revoke`（断"拉新"）；serve 证书轮换 → 手动带新 `--pin` 重拉一次覆盖旧 pin。自动路径永不 `--allow-plaintext`。
```

- [ ] **Step 4: 校验文档无残留旧表述**

Run: `grep -rn "系统定时器\|OS 调度器\|不会自己刷新\|不会热加载" docs/quickstart-multi-machine.md docs/multi-machine.md | grep -v "可选\|不再\|无需\|旧版"`
Expected: 无输出（历史归档 `docs/superpowers/plans/` 不改）

- [ ] **Step 5: Commit**

```bash
git add docs/quickstart-multi-machine.md docs/multi-machine.md docs/threat-model.md
git commit -m "docs: cache auto-refresh — timers demoted to optional, hot-reload + lazy-pull documented"
```

---

### Task 9: 全量验证 + 端到端手工验收

**Files:** 无新文件。

- [ ] **Step 1: 全量机器验证**

Run: `go build ./... && go vet ./... && go test ./...`
Expected: 全部零错误零失败。

- [ ] **Step 2: 端到端手工验收（NUC10 真 broker，可选但推荐）**

1. 笔记本删 `cache.bin`（保留或重建 cred）→ 起 `mcp --cache` → stderr 无 fatal，缓存自动拉取（`cache status` 验证）。
2. **核心验收点**：开一个 Claude Code 会话（--cache 模式）→ broker 侧 `servers add` 一台新机器 → 等 TTL（或临时把 `--cache-max-age` 设 1m）→ 会话内下一次 `list_servers` 出现新机器，全程无人手动 pull、未重启会话。
3. 断网（或停 serve）→ 会话继续可用旧缓存；stderr 出现一次降级日志且**同一 TTL 窗口内不重复刷屏**（退避生效）。
4. `cache pull --allow-plaintext`（连明文 serve）→ 确认 `cache.auth.json` 未被写入。

- [ ] **Step 3: 收尾 Commit（若有零星修正）+ 汇报**

按 superpowers:finishing-a-development-branch 走合并/PR 流程；push 前按仓库铁律做 secret scan（覆盖本会话接触的所有 secret，含设备码/pin）。

---

## Self-Review 记录

- **Spec 覆盖**：spec §2 决策表 13 行 → Task 1（原子写）、Task 2-3（cred/pin 归一/警告/plaintext 不落盘）、Task 4（TTL/退避/超时/0 语义）、Task 5（闭包 per-call）、Task 6（延迟清理/守卫/Lazy）、Task 7（hash 判变/baseline/会话内拉取）、Task 8（文档/threat-model）——全覆盖；spec §6 测试计划 11 条 → Task 1(3)、2(2)、3(1)、4(5)、5(1)、6(5)、7(3) 全部落位。
- **占位符扫描**：无 TBD/TODO；三处「实现者注」给出的是适配指令而非空缺。
- **类型一致性**：`doPull(url, token, pin string, pullOpts)` 在 Task 3 定义、Task 4 消费一致；`atomicWriteUnique(path string, blob []byte)` Task 1/2/3 一致；`RunStdioCache` 4 参签名 Task 6 定义、Task 7 消费一致；`reload` 契约三元组语义在 Task 6/7 一致。
