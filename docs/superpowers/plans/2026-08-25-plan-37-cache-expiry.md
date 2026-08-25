# Plan 37：B 时限快照（SSHMGR_CACHE_MAX_OFFLINE）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** B 时限快照——离线缓存到龄自废（`SSHMGR_CACHE_MAX_OFFLINE` 开启时），服务器 `Date` 锚 + provenance + 销毁前复查 + 全局禁重定向，纯 client 侧。

**Architecture:** 信任 pull 单写者把服务器钟写进 `cache.meta.json`（`ServerAnchored` 标记来源）；load 侧在解密前三闸（meta 有锚 / 超龄销毁（复用 `QuarantineCache`）/ 回拨拒载）。无独立 watermark 文件、无锁；锚与快照以 bin→meta 提交顺序绑定。到龄错误恒包裹 `ErrCacheQuarantined`，报文经 `QuarantineReport` 按 manifest reason 精确分派。

**Tech Stack:** Go 标准库（`net/http`、`time`、`encoding/json`）；零新依赖；零 server 端改动。

**Spec:** `docs/superpowers/specs/2026-08-25-plan-37-cache-expiry-design.md.rev3.md`（定稿 rev3，本计划逐节对其；冻结文案以 spec §1-§4 为准）

## Global Constraints

以下各条对全部任务生效（出自 spec rev3，冲突时 spec 为权威）：

1. **冻结常数 K = 1h**：skew 闸与回拨容差同一常数 `cacheSkewTolerance = time.Hour`。
2. **env 下限 = 1h**：`SSHMGR_CACHE_MAX_OFFLINE` unset/空/零时长 = 关；不可解析或 `< 1h`（含负）= fail-closed 错误。
3. **冻结文案逐字**（实现与测试断言均不得改写；`%q`/`%s` 为格式位）：
   - env 非法：`invalid SSHMGR_CACHE_MAX_OFFLINE %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)`
   - B 开明文拒：`SSHMGR_CACHE_MAX_OFFLINE is set: refusing plaintext pull — the time anchor requires a pinned TLS server (unset the cap or remove --allow-plaintext)`
   - Date 缺失/坏：`pull succeeded but the response has no valid Date header — refusing to anchor cache time (SSHMGR_CACHE_MAX_OFFLINE requires a trusted server clock)`
   - skew 超限：`server clock skew too large (server %s vs local %s, cap 1h) — refusing pull: SSHMGR_CACHE_MAX_OFFLINE depends on an accurate clock; fix system time sync`（两个 `%s` = RFC3339，先 server 后 local）
   - 重定向：`pull: server returned %d (redirects are not followed)`
   - meta 缺失/坏：`SSHMGR_CACHE_MAX_OFFLINE is set but cache.meta.json is missing or corrupt (or this machine never pulled) — refusing cache (no time anchor); run cache pull`
   - 无服务器锚：`cache.meta.json has no server-anchored time (pulled while SSHMGR_CACHE_MAX_OFFLINE was unset or by an older client) — refusing cache; run cache pull to establish a server time anchor`
   - 复查失败：`cache expiry re-check failed (%v) — refusing cache; run cache pull`
   - 到龄（哨兵前缀后接此文本）：`cache snapshot expired (offline %s > cap %s) — snapshot destroyed; run cache pull to re-enroll`（`%s` = 时长，`offline` 取 `time.Since(锚).Round(time.Second)`）
   - 回拨：`system clock is behind the snapshot's server time anchor (local %s, anchor %s, tolerance 1h) — refusing cache (clock fault or tampering); fix system time, then run cache pull`
   - 报文三变体（reason 精确等值命中 `expiryReason`）：
     - done：`cache expired: offline beyond SSHMGR_CACHE_MAX_OFFLINE — snapshot destroyed; run cache pull (the device code is still valid unless revoked)`
     - done+DEGRADED：`cache expired: offline beyond SSHMGR_CACHE_MAX_OFFLINE [DEGRADED: %v] — snapshot destroyed; run cache pull (the device code is still valid unless revoked)`
     - started：`cache expiry destruction was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json`
   - 非 expiry reason 的现行报文**逐字不动**（`cache quarantined by server rejection (token revoked?) ...` 三变体）。
4. **meta 字段**：`ServerAnchored bool \`json:"server_anchored"\``——**无 omitempty，恒序列化**（B 关 pull 显式写 false；旧 meta 无字段 → 零值 false）。
5. **提交顺序**：bin 原子写成功 → meta 原子写（现行顺序，禁止倒置）；「新锚 + 旧超龄 bin」fail-open 组合按顺序不可达；meta 写失败 = 现行 WARNING（pull 仍成功）。
6. **禁重定向全局生效**（B 开与 B 关；pinned 与明文 client 都设 `CheckRedirect` 返回 `http.ErrUseLastResponse`，3xx 在非 200 分支出冻结文案）。
7. **销毁仅一条路**：`QuarantineCache(expiryReason)` 只在「正向到龄证据 + 复查确认」后调用；回拨/provenance 缺失/复查读失败**只拒载**。到龄错误**恒包裹** `ErrCacheQuarantined`（干净销毁时以哨兵替代 nil 再包裹，DoPull 401 分支同款惯例）。
8. **`QuarantineCache` 销毁例程本体零改动**（复用 A 机器；401 调用点只把字面量换成 `serverRejectedReason` 常量）。
9. **MCP 面与 serve 端零改动**：不碰 `internal/mcpserver`、`internal/cli/serve*.go`、`BrokerTools`。
10. **测试 seam 惯例**：包级 hook + `...ForTest` 命名（对齐 `ResetLazyPullBackoffForTest`/`ResetCacheQuarantineForTest` 先例）。
11. **过程纪律**：每 commit 尾行 `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`；gofmt 净；冻结文案出现在代码里时逐字节校验（em-dash `—` 是 U+2014，非连字符）。

---

### Task 1: pull 侧——env helper + 五闸 + 服务器锚

**Files:**
- Create: `internal/clientops/expiry.go`
- Create: `internal/clientops/expiry_pull_test.go`
- Modify: `internal/clientops/clientops.go`（`cacheMeta` 结构 ~:182、`DoPull` ~:289-390）

**Interfaces:**
- Consumes: `pinningTransport(pin)`（pin.go:57）、`mcpserver.SPKIFingerprint`、`mcpserver.ParsePin`、`atomicWriteUnique`、`DekProvider` seam。
- Produces（T2 依赖，逐字）:
  - `func cacheMaxOffline() (time.Duration, error)`
  - `const cacheSkewTolerance = time.Hour`
  - `const expiryReason = "snapshot expired (offline beyond SSHMGR_CACHE_MAX_OFFLINE)"`
  - `const serverRejectedReason = "server rejected device code"`
  - `cacheMeta.ServerAnchored bool \`json:"server_anchored"\`` 字段
  - `var failNextMetaWriteForTest bool` + `func FailNextMetaWriteForTest()`

- [ ] **Step 1: 写失败测试**（新文件 `internal/clientops/expiry_pull_test.go`）

```go
package clientops

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
)

// newPinnedTLSServer spins a TLS httptest server with a fresh self-signed
// ed25519 cert and returns its URL plus the SPKI pin a client must use
// (same construction as internal/cli's pinned-pull tests).
func newPinnedTLSServer(t *testing.T, h http.HandlerFunc) (string, string) {
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
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
	srv := httptest.NewUnstartedServer(h)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{CertPEM: certPEM, KeyPEM: keyPEM}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv.URL, mcpserver.SPKIFingerprint(cert)
}

// snapshotHandler serves a valid /snapshot body with a controllable Date.
// date==nil → suppress the Date header entirely; date!="" → emit verbatim.
func snapshotHandler(date *string, hits *atomic.Int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		switch {
		case date == nil:
			w.Header()["Date"] = nil // suppress auto-Date
		case *date != "":
			w.Header().Set("Date", *date)
		}
		fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
	}
}

func TestCacheMaxOffline_Parsing(t *testing.T) {
	cases := []struct {
		env     string
		set     bool
		want    time.Duration
		wantErr bool
	}{
		{env: "", set: false, want: 0},
		{env: "0", set: true, want: 0},
		{env: "0h", set: true, want: 0},
		{env: "168h", set: true, want: 168 * time.Hour},
		{env: "1h", set: true, want: time.Hour},
		{env: "abc", set: true, wantErr: true},
		{env: "-1h", set: true, wantErr: true},
		{env: "30m", set: true, wantErr: true},
	}
	for _, tc := range cases {
		if tc.set {
			t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", tc.env)
		} else {
			t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
		}
		got, err := cacheMaxOffline()
		if tc.wantErr {
			if err == nil {
				t.Fatalf("env %q: want error, got %v", tc.env, got)
			}
			want := fmt.Sprintf("invalid SSHMGR_CACHE_MAX_OFFLINE %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)", tc.env)
			if err.Error() != want {
				t.Fatalf("env %q: error text mismatch:\n got %q\nwant %q", tc.env, err.Error(), want)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("env %q: got (%v, %v), want (%v, nil)", tc.env, got, err, tc.want)
		}
	}
}

func TestDoPull_InvalidEnv_RefusedBeforeHTTP(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "30m")
	hits := &atomic.Int32{}
	url, pin := newPinnedTLSServer(t, snapshotHandler(nil, hits))
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": t.TempDir()})
	err := DoPull(url, "code", pin, PullOpts{})
	if err == nil || !strings.Contains(err.Error(), "invalid SSHMGR_CACHE_MAX_OFFLINE") {
		t.Fatalf("want invalid-env refusal, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("request must not be sent, got %d hits", hits.Load())
	}
}

func TestDoPull_PlaintextRefusedWhenMaxOffline(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	hits := &atomic.Int32{}
	url, _ := newPinnedTLSServer(t, snapshotHandler(nil, hits))
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": t.TempDir()})
	err := DoPull(url, "code", "", PullOpts{AllowPlain: true})
	if err == nil || !strings.Contains(err.Error(), "refusing plaintext pull") {
		t.Fatalf("want plaintext refusal, got %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("no HTTP expected, got %d hits", hits.Load())
	}
}

func TestDoPull_NoRedirect_Global(t *testing.T) {
	for _, b := range []string{"", "24h"} { // B off and B on — the ban is global
		t.Run("B="+b, func(t *testing.T) {
			t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", b)
			var plainHits atomic.Int32
			plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				plainHits.Add(1)
				fmt.Fprint(w, `{"servers":[],"credentials":[]}`)
			}))
			defer plain.Close()
			url, pin := newPinnedTLSServer(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, plain.URL, http.StatusFound)
			})
			dir := t.TempDir()
			withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
			err := DoPull(url, "code", pin, PullOpts{})
			if err == nil || !strings.Contains(err.Error(), "redirects are not followed") {
				t.Fatalf("want redirect refusal, got %v", err)
			}
			if plainHits.Load() != 0 {
				t.Fatalf("plaintext hop must not be reached, got %d hits", plainHits.Load())
			}
			if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
				t.Fatal("cache.bin must not be written from a redirect")
			}
		})
	}
}

func TestDoPull_DateTwoBadShapes_Refused(t *testing.T) {
	now := time.Now().Format(http.TimeFormat)
	cases := []struct {
		name string
		date *string
	}{
		{name: "missing", date: nil},
		{name: "malformed", date: ptr("not-a-date")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
			url, pin := newPinnedTLSServer(t, snapshotHandler(tc.date, nil))
			dir := t.TempDir()
			withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
			err := DoPull(url, "code", pin, PullOpts{})
			if err == nil || !strings.Contains(err.Error(), "no valid Date header") {
				t.Fatalf("want Date refusal, got %v", err)
			}
			if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
				t.Fatal("cache.bin must not be written without an anchor")
			}
		})
	}
	// B off: both shapes still pull fine (no anchor machinery).
	t.Run("B-off-tolerates", func(t *testing.T) {
		t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
		url, pin := newPinnedTLSServer(t, snapshotHandler(nil, nil))
		withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": t.TempDir()})
		if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
			t.Fatalf("B-off pull must ignore Date: %v", err)
		}
	})
}

func TestDoPull_Skew_Refused(t *testing.T) {
	skewed := time.Now().Add(2 * time.Hour).Format(http.TimeFormat)
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(skewed), nil))
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	err := DoPull(url, "code", pin, PullOpts{})
	if err == nil || !strings.Contains(err.Error(), "server clock skew too large") {
		t.Fatalf("want skew refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), time.Now().Format(time.RFC3339)[:11]) {
		t.Fatalf("error must carry RFC3339 stamps: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); !os.IsNotExist(serr) {
		t.Fatal("cache.bin must not be written on skew")
	}
}

func TestDoPull_AnchorWritten_BothStates(t *testing.T) {
	serverNow := time.Now().Add(5 * time.Minute).UTC() // within 1h skew
	date := serverNow.Format(http.TimeFormat)
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})

	// B on: PulledAt = server Date, ServerAnchored=true.
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("anchored pull: %v", err)
	}
	m := readMetaForTest(t, dir)
	if m.PulledAt != serverNow.Unix() {
		t.Fatalf("B-on PulledAt = %d, want server Date %d", m.PulledAt, serverNow.Unix())
	}
	if !m.ServerAnchored {
		t.Fatal("B-on must set ServerAnchored")
	}

	// B off: PulledAt = client clock, ServerAnchored explicitly serialized false.
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	before := time.Now().Unix()
	url2, pin2 := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	if err := DoPull(url2, "code", pin2, PullOpts{}); err != nil {
		t.Fatalf("B-off pull: %v", err)
	}
	after := time.Now().Unix()
	m = readMetaForTest(t, dir)
	if m.PulledAt < before || m.PulledAt > after {
		t.Fatalf("B-off PulledAt %d outside [%d,%d]", m.PulledAt, before, after)
	}
	if m.ServerAnchored {
		t.Fatal("B-off must not set ServerAnchored")
	}
	blob, _ := os.ReadFile(filepath.Join(dir, "cache.meta.json"))
	if !strings.Contains(string(blob), `"server_anchored":false`) {
		t.Fatalf("B-off meta must serialize explicit false, got %s", blob)
	}
}

func ptr(s string) *string { return &s }

// readMetaForTest reads+decodes cache.meta.json from a cache dir.
func readMetaForTest(t *testing.T, dir string) cacheMeta {
	t.Helper()
	m, err := readCacheMeta(filepath.Join(dir, "cache.meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}
```

（`ptr`/`readMetaForTest` 若与本包既有 helper 重名则改名；import 块已列全。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ -run 'TestCacheMaxOffline|TestDoPull_' -v`
Expected: FAIL（`cacheMaxOffline`/`readCacheMeta` undefined、编译错误）

- [ ] **Step 3: 实现**（新文件 `internal/clientops/expiry.go`）

```go
// Package clientops: Plan 37 B 时限快照 — the offline cache expiry machinery:
// the SSHMGR_CACHE_MAX_OFFLINE seam, the shared clock-tolerance constant, the
// quarantine reason constants, and the test seams for the destructive-race
// matrix. The pull-side gates live in DoPull; the load-side gates in
// LoadCacheSnapshot (both in clientops.go).
package clientops

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// cacheSkewTolerance is the design's frozen K (§5): ONE constant serving both
// the pull-side skew gate (|server Date − local now| ≤ K) and the load-side
// rollback tolerance (now ≥ PulledAt − K). Same constant on both sides is what
// makes "a clock that passed the pull gate can never trip the load gate" hold.
const cacheSkewTolerance = time.Hour

// The only two quarantine manifest reasons (§4). QuarantineReport dispatches
// its message on exact equality against expiryReason; every other reason —
// i.e. the Plan 34 server-rejection one — keeps the legacy texts verbatim.
const (
	expiryReason         = "snapshot expired (offline beyond SSHMGR_CACHE_MAX_OFFLINE)"
	serverRejectedReason = "server rejected device code"
)

// cacheMaxOffline parses SSHMGR_CACHE_MAX_OFFLINE (§1): unset/empty/zero = off
// (0, nil); a valid duration >= 1h = on; anything else — unparseable, negative,
// or a positive sub-hour value — is a fail-closed error carrying the raw value
// (both LoadCacheSnapshot and DoPull refuse on it; there is no half-open state).
func cacheMaxOffline() (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv("SSHMGR_CACHE_MAX_OFFLINE"))
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid SSHMGR_CACHE_MAX_OFFLINE %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)", v)
	}
	if d == 0 {
		return 0, nil
	}
	if d < cacheSkewTolerance {
		return 0, fmt.Errorf("invalid SSHMGR_CACHE_MAX_OFFLINE %q: must be a Go duration >= 1h (e.g. 168h; unset/0 disables expiry)", v)
	}
	return d, nil
}

// readCacheMeta loads+decodes cache.meta.json. Any read or parse failure is an
// error — the load-side gates treat "no usable anchor" as refuse-not-destroy.
func readCacheMeta(path string) (cacheMeta, error) {
	var m cacheMeta
	blob, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(blob, &m); err != nil {
		return m, fmt.Errorf("corrupt cache.meta.json: %w", err)
	}
	return m, nil
}

// failNextMetaWriteForTest, when armed, makes the NEXT DoPull's meta write
// fail (the pull itself still succeeds — the §2.4 WARNING path). Test-only
// seam for the commit-order matrix (§8.18); nil-effect in production.
var failNextMetaWriteForTest bool

// FailNextMetaWriteForTest arms the one-shot meta write failure.
func FailNextMetaWriteForTest() { failNextMetaWriteForTest = true }
```

**`clientops.go` 修改**（三处）：

(a) `cacheMeta` 结构加字段（~:182）：

```go
type cacheMeta struct {
	URL      string `json:"url"`
	PulledAt int64  `json:"pulled_at"` // pull time: server-clock anchored (ServerAnchored=true) or local clock (false)
	// ServerAnchored records whether PulledAt came from a pinned server Date
	// that passed the skew gate (Plan 37 §2.4). No omitempty: B-off pulls
	// serialize an explicit false; a legacy meta without the field reads as
	// the zero value false — which is exactly the provenance semantics.
	ServerAnchored bool `json:"server_anchored"`
}
```

(b) `DoPull` 开头（`code := token` 之后、client 构造之前）加 env 预检；两个 client 构造点都加 `CheckRedirect`：

```go
func DoPull(url, token, pin string, o PullOpts) error {
	code := token
	// Plan 37 §1/§2: fail-closed env precheck — an invalid cap refuses the
	// pull before any HTTP (no half-open "pulls but won't load" state).
	maxOffline, err := cacheMaxOffline()
	if err != nil {
		return err
	}
	var client *http.Client
	if pin == "" {
		if maxOffline > 0 {
			// Plan 37 §2.1: the time anchor requires a pinned TLS server;
			// a plaintext response's Date is injectable and cannot anchor.
			return fmt.Errorf("SSHMGR_CACHE_MAX_OFFLINE is set: refusing plaintext pull — the time anchor requires a pinned TLS server (unset the cap or remove --allow-plaintext)")
		}
		if !o.AllowPlain {
			return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
				"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
		}
		if o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "WARNING: --allow-plaintext set: pulling over unverified HTTP (no TLS pin). /snapshot credentials travel in cleartext.\n")
		}
		// Plan 37 §2.0: redirects are never followed, on the plain client too —
		// a followed 30x would take the response (body AND headers) off the
		// transport the caller chose (xcheck experiment, 2026-08-25).
		client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	} else {
		// ...（原 https 校验 + SplitTokenPin + pinningTransport 不动）...
		client = &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	// ...（Timeout 设置、loadOrCreateDEK、请求构造不动）...
```

(c) 非 200 分支加 3xx 冻结文案（401 分支保持原样、仅字面量换常量）；200 分支写盘前加锚闸；meta 写入带 provenance + 测试 seam：

```go
	if res.StatusCode != 200 {
		io.Copy(io.Discard, res.Body) // keep the keep-alive socket reusable
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			// Plan 37 §2.0: CheckRedirect returned ErrUseLastResponse, so a 3xx
			// surfaces here — never followed, on any client.
			return fmt.Errorf("pull: server returned %d (redirects are not followed)", res.StatusCode)
		}
		if pin != "" && res.StatusCode == 401 {
			// ...（原 Plan 34 rev4 §3 注释不动）...
			_, qerr := QuarantineCache(serverRejectedReason)
			if qerr == nil {
				qerr = ErrCacheQuarantined
			}
			return fmt.Errorf("pull: %w — re-enroll with a fresh device code", qerr)
		}
		return fmt.Errorf("pull: server returned %d (is the authorization code valid/active?)", res.StatusCode)
	}
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	// Plan 37 §2.2-2.4: B on → the Date header of THIS pinned 200 response is
	// the time anchor. Missing/malformed Date or |server − local| > 1h refuses
	// the pull (fail-closed); a passing Date is written into meta.PulledAt with
	// ServerAnchored=true. B off keeps the legacy local clock + explicit false.
	pulledAt := time.Now().Unix()
	anchored := false
	if maxOffline > 0 {
		serverTime, perr := http.ParseTime(res.Header.Get("Date"))
		if perr != nil {
			return fmt.Errorf("pull succeeded but the response has no valid Date header — refusing to anchor cache time (SSHMGR_CACHE_MAX_OFFLINE requires a trusted server clock)")
		}
		if skew := time.Since(serverTime); skew > cacheSkewTolerance || skew < -cacheSkewTolerance {
			return fmt.Errorf("server clock skew too large (server %s vs local %s, cap 1h) — refusing pull: SSHMGR_CACHE_MAX_OFFLINE depends on an accurate clock; fix system time sync",
				serverTime.Format(time.RFC3339), time.Now().Format(time.RFC3339))
		}
		pulledAt = serverTime.Unix()
		anchored = true
	}
	blob, err := vaultio.EncryptWithKey(dek, body)
	if err != nil {
		return err
	}
	_, bin, metaPath, _, err := CachePaths()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		return err
	}
	// Plan 37 §2.4 commit order, pinned: bin first, meta LAST — a crash can
	// leave (new bin + old anchor) whose age error is bounded <= 2K, never
	// (new anchor + old bin). Meta-write failure stays the legacy WARNING.
	if err := atomicWriteUnique(bin, blob); err != nil {
		return err
	}
	if failNextMetaWriteForTest {
		failNextMetaWriteForTest = false
		if o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "WARNING: cache.meta.json write failed (source URL will show as unknown): test-injected failure\n")
		}
	} else {
		mb, _ := json.Marshal(cacheMeta{URL: url, PulledAt: pulledAt, ServerAnchored: anchored})
		if werr := atomicWriteUnique(metaPath, mb); werr != nil && o.StatusOut != nil {
			fmt.Fprintf(o.StatusOut, "WARNING: cache.meta.json write failed (source URL will show as unknown): %v\n", werr)
		}
	}
	// ...（manifest 清理 + status 行 + return nil 原样不动）...
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/clientops/ -run 'TestCacheMaxOffline|TestDoPull_' -v && go test ./internal/clientops/ ./internal/cli/ -count=1`
Expected: 新测试全 PASS；既有包测试零回归（重定向改动可能影响依赖跟随行为的旧测试——若有，属本 plan 预期内行为变更，按 §1「唯二例外」处置并在 commit message 说明）

- [ ] **Step 5: Commit**

```bash
git add internal/clientops/expiry.go internal/clientops/expiry_pull_test.go internal/clientops/clientops.go
git commit -m "feat(cache): Plan 37 T1 pull 侧五闸——env 下限/明文拒/全局禁重定向/Date+skew/服务器锚 provenance

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 2: load 侧——三闸 + 销毁前复查 + reason 分派

**Files:**
- Modify: `internal/clientops/clientops.go`（`LoadCacheSnapshot` ~:253）
- Modify: `internal/clientops/quarantine_report.go`（reason 分派 ~:52-60）
- Create: `internal/clientops/expiry_load_test.go`

**Interfaces:**
- Consumes: T1 全部 Produces + `QuarantineCache`、`QuarantineReport`、`ErrCacheQuarantined`、`store.MemKeyProvider`（withDEK）、`vaultio.EncryptWithKey`。
- Produces:
  - `var expiryTestHooks struct { afterAgeCheck, afterRecheck func() }` + `func ResetExpiryHooksForTest()`
  - `LoadCacheSnapshot` 新错误族（§3.2/3.3/3.4/3.5 冻结文案 + `ErrCacheQuarantined` 包裹）

- [ ] **Step 1: 写失败测试**（新文件 `internal/clientops/expiry_load_test.go`）

```go
package clientops

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ssh-manager-mcp/internal/vaultio"
)

// writeCacheFixture seeds a cache dir with a decryptable cache.bin (encrypted
// under the in-memory DEK) plus a meta carrying the given anchor state.
func writeCacheFixture(t *testing.T, dir string, pulledAt int64, anchored bool) {
	t.Helper()
	mem := withDEK(t)
	dek, err := mem.Get()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := vaultio.EncryptWithKey(dek, []byte(`{"servers":[],"credentials":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeMetaFile(t, dir, pulledAt, anchored); err != nil {
		t.Fatal(err)
	}
}

func writeMetaFile(t *testing.T, dir string, pulledAt int64, anchored bool) error {
	t.Helper()
	blob := fmt.Sprintf(`{"url":"u","pulled_at":%d,"server_anchored":%t}`, pulledAt, anchored)
	return os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(blob), 0o600)
}

func intactFor(t *testing.T, dir string) bool {
	t.Helper()
	for _, f := range []string{"cache.bin", "cache.meta.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return false
		}
	}
	return true
}

func TestLoad_BOff_Unchanged(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(-365*24*time.Hour).Unix(), false) // absurd age, no anchor
	if _, err := LoadCacheSnapshot(); err != nil {
		t.Fatalf("B off must load regardless of age/anchor: %v", err)
	}
}

func TestLoad_InvalidEnv_Refused(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "30m")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Unix(), true)
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "invalid SSHMGR_CACHE_MAX_OFFLINE") {
		t.Fatalf("want invalid-env refusal, got %v", err)
	}
	if !intactFor(t, dir) {
		t.Fatal("invalid env must not destroy anything")
	}
}

func TestLoad_MetaMissing_Refused(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Unix(), true)
	os.Remove(filepath.Join(dir, "cache.meta.json"))
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "missing or corrupt (or this machine never pulled)") {
		t.Fatalf("want meta-missing refusal, got %v", err)
	}
	if !intactFor(t, dir) {
		t.Fatal("meta missing must not destroy")
	}
}

func TestLoad_ProvenanceGate(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	// B-off-era meta: explicit false.
	writeCacheFixture(t, dir, time.Now().Unix(), false)
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "no server-anchored time") {
		t.Fatalf("want provenance refusal, got %v", err)
	}
	if !intactFor(t, dir) {
		t.Fatal("provenance must not destroy")
	}
	// Legacy meta shape: no server_anchored field at all → zero value false.
	os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte(`{"url":"u","pulled_at":123}`), 0o600)
	_, err = LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "no server-anchored time") {
		t.Fatalf("want provenance refusal for legacy meta, got %v", err)
	}
}

func TestLoad_WithinWindow_Loads(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(-30*time.Minute).Unix(), true)
	if _, err := LoadCacheSnapshot(); err != nil {
		t.Fatalf("in-window anchored cache must load: %v", err)
	}
}

func TestLoad_Rollback_Refused(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "24h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(2*time.Hour).Unix(), true) // future anchor
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "system clock is behind the snapshot's server time anchor") {
		t.Fatalf("want rollback refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "fix system time, then run cache pull") {
		t.Fatalf("rollback text must direct clock repair first: %v", err)
	}
	if !intactFor(t, dir) {
		t.Fatal("rollback must not destroy")
	}
}

// TestLoad_AgedDestroys is matrix 3: the six destruction assertions plus the
// post-destruction report path (expired message on the NEXT load too).
func TestLoad_AgedDestroys(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	mem := withDEK(t)
	writeCacheFixture(t, dir, time.Now().Add(-2*time.Hour).Unix(), true)

	_, err := LoadCacheSnapshot()
	if err == nil {
		t.Fatal("aged cache must be refused")
	}
	if !errors.Is(err, ErrCacheQuarantined) {
		t.Fatalf("expiry error must wrap ErrCacheQuarantined, got %v", err)
	}
	if !strings.Contains(err.Error(), "cache snapshot expired (offline") {
		t.Fatalf("expiry text missing: %v", err)
	}
	qdir := filepath.Join(dir, "quarantine")
	entries, _ := os.ReadDir(qdir)
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "cache.bin.quarantined-") {
			found = true
		}
	}
	if !found {
		t.Fatal("cache.bin must be isolated into quarantine/")
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.meta.json")); !os.IsNotExist(serr) {
		t.Fatal("meta must be deleted (destruction step 4)")
	}
	if _, derr := mem.Get(); derr == nil {
		t.Fatal("DEK must be deleted")
	}
	// manifest reason + post-destruction report path.
	mblob, rerr := os.ReadFile(filepath.Join(qdir, "manifest.json"))
	if rerr != nil || !strings.Contains(string(mblob), `"reason":"`+expiryReason+`"`) {
		t.Fatalf("manifest must carry expiryReason: %v %s", rerr, mblob)
	}
	_, err2 := LoadCacheSnapshot() // next spawn: meta-missing gate fires…
	if err2 == nil || !strings.Contains(err2.Error(), "missing or corrupt") {
		t.Fatalf("post-destruction load should hit the meta gate: %v", err2)
	}
	if msg, ok := QuarantineReport(err2); !ok || !strings.HasPrefix(msg, "cache expired: offline beyond SSHMGR_CACHE_MAX_OFFLINE — snapshot destroyed") {
		t.Fatalf("post-destruction report must be the expired line, got %q ok=%v", msg, ok)
	}
}

// TestLoad_RecheckAborts is matrix 16 branch A: a trusted pull re-anchors
// between the age verdict and the re-check → destruction aborts.
func TestLoad_RecheckAborts(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(-2*time.Hour).Unix(), true)

	date := time.Now().Format(http.TimeFormat)
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	ResetExpiryHooksForTest()
	expiryTestHooks.afterAgeCheck = func() {
		if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
			t.Errorf("hook pull: %v", err)
		}
	}
	t.Cleanup(ResetExpiryHooksForTest)
	if _, err := LoadCacheSnapshot(); err != nil {
		t.Fatalf("re-anchored load must succeed: %v", err)
	}
	if !intactFor(t, dir) {
		t.Fatal("abort must leave cache files in place")
	}
}

// TestLoad_RecheckFailure_Refused is matrix 16's injection leg: the re-check
// read fails → refuse WITHOUT destroying (independent copy, not §3.2's).
func TestLoad_RecheckFailure_Refused(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(-2*time.Hour).Unix(), true)

	ResetExpiryHooksForTest()
	expiryTestHooks.afterAgeCheck = func() {
		os.WriteFile(filepath.Join(dir, "cache.meta.json"), []byte("{not json"), 0o600)
	}
	t.Cleanup(ResetExpiryHooksForTest)
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "cache expiry re-check failed") {
		t.Fatalf("want re-check-failure refusal, got %v", err)
	}
	if _, serr := os.Stat(filepath.Join(dir, "cache.bin")); serr != nil {
		t.Fatal("re-check failure must not destroy the snapshot")
	}
}

// TestLoad_DestroyRacingPull_Residual is matrix 16 branch B: a pull landing
// AFTER the re-check still gets destroyed — the registered millisecond-window
// residual, nailed behaviorally: outcome = fresh cache quarantined + re-pull
// self-heals.
func TestLoad_DestroyRacingPull_Residual(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Add(-2*time.Hour).Unix(), true)

	date := time.Now().Format(http.TimeFormat)
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	ResetExpiryHooksForTest()
	expiryTestHooks.afterRecheck = func() {
		if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
			t.Errorf("hook pull: %v", err)
		}
	}
	t.Cleanup(ResetExpiryHooksForTest)
	_, err := LoadCacheSnapshot()
	if err == nil || !strings.Contains(err.Error(), "cache snapshot expired") {
		t.Fatalf("residual window must still report expiry: %v", err)
	}
	// Self-heal: one re-pull restores a loadable cache.
	if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("self-heal pull: %v", err)
	}
	if _, err := LoadCacheSnapshot(); err != nil {
		t.Fatalf("post-heal load: %v", err)
	}
}

// TestLoad_Concurrent_NoAnchorWrites is matrix 13: concurrent loads never
// touch meta (the zero-anchor-write design, behaviorally).
func TestLoad_Concurrent_NoAnchorWrites(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	writeCacheFixture(t, dir, time.Now().Unix(), true)
	before, _ := os.ReadFile(filepath.Join(dir, "cache.meta.json"))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = LoadCacheSnapshot()
		}()
	}
	wg.Wait()
	after, _ := os.ReadFile(filepath.Join(dir, "cache.meta.json"))
	if string(before) != string(after) {
		t.Fatalf("meta mutated by loads:\nbefore %s\nafter  %s", before, after)
	}
}

// TestLoad_AgeFromOldAnchor is matrix 18: with a fresh bin but a retained old
// anchor (meta write failed), age is computed from the OLD anchor — asserted
// without claiming a direction (bounded ≤ 2K both ways, spec §2.4).
func TestLoad_AgeFromOldAnchor(t *testing.T) {
	t.Setenv("SSHMGR_CACHE_MAX_OFFLINE", "1h")
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	oldAnchor := time.Now().Add(-90 * time.Minute).Unix()
	writeCacheFixture(t, dir, oldAnchor, true)

	date := time.Now().Format(http.TimeFormat)
	url, pin := newPinnedTLSServer(t, snapshotHandler(ptr(date), nil))
	FailNextMetaWriteForTest() // pull succeeds, meta write fails, old anchor stays
	if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("pull must survive meta-write failure (WARNING): %v", err)
	}
	m := readMetaForTest(t, dir)
	if m.PulledAt != oldAnchor {
		t.Fatalf("old anchor must be retained, got %d want %d", m.PulledAt, oldAnchor)
	}
	_, err := LoadCacheSnapshot() // 90m > 1h cap → aged per the OLD anchor
	if err == nil || !strings.Contains(err.Error(), "cache snapshot expired") {
		t.Fatalf("age must be judged from the retained old anchor: %v", err)
	}
	// The in-window direction: a 30m-old retained anchor loads fine.
	writeCacheFixture(t, dir, time.Now().Add(-30*time.Minute).Unix(), true)
	FailNextMetaWriteForTest()
	if err := DoPull(url, "code", pin, PullOpts{}); err != nil {
		t.Fatalf("second pull: %v", err)
	}
	if _, err := LoadCacheSnapshot(); err != nil {
		t.Fatalf("in-window old anchor must load: %v", err)
	}
}

// TestQuarantineReport_ReasonDispatch is matrix 12: exact-equality dispatch.
func TestQuarantineReport_ReasonDispatch(t *testing.T) {
	dir := t.TempDir()
	withEnv(t, map[string]string{"SSHMGR_CACHE_DIR": dir})
	qdir := filepath.Join(dir, "quarantine")
	os.MkdirAll(qdir, 0o700)
	cases := []struct {
		name    string
		manifest string
		want    string
	}{
		{"expired-done", `{"state":"done","reason":"` + expiryReason + `","ts":100}`, "cache expired: offline beyond SSHMGR_CACHE_MAX_OFFLINE — snapshot destroyed; run cache pull (the device code is still valid unless revoked)"},
		{"expired-started", `{"state":"started","reason":"` + expiryReason + `","ts":100}`, "cache expiry destruction was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json"},
		{"server-rejected-done", `{"state":"done","reason":"` + serverRejectedReason + `","ts":100}`, "cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.WriteFile(filepath.Join(qdir, "manifest.json"), []byte(tc.manifest), 0o600)
			msg, ok := QuarantineReport(errors.New("load failed"))
			if !ok || !strings.HasPrefix(msg, tc.want) {
				t.Fatalf("dispatch mismatch:\n got %q (ok=%v)\nwant prefix %q", msg, ok, tc.want)
			}
		})
	}
	// degraded variants keep their [DEGRADED: ...] segment.
	os.WriteFile(filepath.Join(qdir, "manifest.json"),
		[]byte(`{"state":"done","reason":"`+expiryReason+`","ts":100,"degraded":["dek"]}`), 0o600)
	msg, _ := QuarantineReport(errors.New("x"))
	if !strings.Contains(msg, "[DEGRADED: [dek]] — snapshot destroyed") {
		t.Fatalf("expired+degraded text wrong: %q", msg)
	}
}
```

（`http`、`sync`、`vaultio` 等按需进 import；`newPinnedTLSServer`/`snapshotHandler`/`ptr`/`readMetaForTest` 来自 T1 测试文件——同包共享。）

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/clientops/ -run 'TestLoad_|TestQuarantineReport_ReasonDispatch' -v`
Expected: FAIL（`expiryTestHooks`/`ResetExpiryHooksForTest` undefined、gate 行为缺失）

- [ ] **Step 3: 实现**（`clientops.go` 的 `LoadCacheSnapshot` 整体改造 + `expiry.go` 加 hooks + `quarantine_report.go` 分派）

**`expiry.go` 追加：**

```go
// expiryTestHooks lets tests orchestrate the destructive-race matrix (spec
// §8.16) at the three load-side points: after the age verdict (before the
// re-read) and after the re-check verdict (before destruction). Nil in
// production; ResetExpiryHooksForTest clears them between tests.
var expiryTestHooks struct {
	afterAgeCheck func()
	afterRecheck  func()
}

// ResetExpiryHooksForTest clears the load-side expiry hooks.
func ResetExpiryHooksForTest() {
	expiryTestHooks.afterAgeCheck = nil
	expiryTestHooks.afterRecheck = nil
}
```

**`clientops.go` 的 `LoadCacheSnapshot`**（整函数替换；解密段不动）：

```go
// LoadCacheSnapshot reads + DEK-decrypts + unmarshals the cache. Shared by `cache status` and
// `mcp --cache`. Returns an error if the cache is absent / corrupt / the DEK is missing.
//
// Plan 37 §3: with SSHMGR_CACHE_MAX_OFFLINE set, three gates run BEFORE the
// DEK is even touched (meta is plaintext): usable-anchor, provenance, and
// expiry/rollback. Destruction happens only on the expiry gate, only after a
// re-check confirms the anchor did not just advance (destructive-race guard).
func LoadCacheSnapshot() (*store.Snapshot, error) {
	_, bin, metaPath, _, err := CachePaths()
	if err != nil {
		return nil, err
	}
	maxOffline, err := cacheMaxOffline()
	if err != nil {
		return nil, err
	}
	if maxOffline > 0 {
		meta, merr := readCacheMeta(metaPath)
		if merr != nil {
			// §3.2: no usable anchor — refuse, never destroy (includes the
			// never-pulled first run; a fresh machine has no meta by design).
			return nil, fmt.Errorf("SSHMGR_CACHE_MAX_OFFLINE is set but cache.meta.json is missing or corrupt (or this machine never pulled) — refusing cache (no time anchor); run cache pull: %v", merr)
		}
		if !meta.ServerAnchored {
			// §3.3 provenance gate: a local-clock anchor never passed any gate.
			return nil, fmt.Errorf("cache.meta.json has no server-anchored time (pulled while SSHMGR_CACHE_MAX_OFFLINE was unset or by an older client) — refusing cache; run cache pull to establish a server time anchor")
		}
		anchorT := time.Unix(meta.PulledAt, 0)
		now := time.Now()
		if now.Sub(anchorT) > maxOffline {
			if expiryTestHooks.afterAgeCheck != nil {
				expiryTestHooks.afterAgeCheck()
			}
			// §3.4 re-check: destruction requires positive expiry evidence AND
			// confirmation. Re-read the anchor right before destroying.
			meta2, rerr := readCacheMeta(metaPath)
			if rerr != nil {
				return nil, fmt.Errorf("cache expiry re-check failed (%v) — refusing cache; run cache pull", rerr)
			}
			if expiryTestHooks.afterRecheck != nil {
				expiryTestHooks.afterRecheck()
			}
			if meta2.PulledAt > meta.PulledAt && meta2.ServerAnchored {
				// A concurrent trusted pull just re-anchored: abort destruction
				// and judge again from the fresh anchor.
				return LoadCacheSnapshot()
			}
			_, qerr := QuarantineCache(expiryReason)
			if qerr == nil {
				qerr = ErrCacheQuarantined
			}
			return nil, fmt.Errorf("%w — cache snapshot expired (offline %s > cap %s): snapshot destroyed; run cache pull to re-enroll",
				qerr, now.Sub(anchorT).Round(time.Second), maxOffline)
		}
		if now.Before(anchorT.Add(-cacheSkewTolerance)) {
			// §3.5 rollback gate: refuse (never destroy) — a clock fault and a
			// clock attack look identical; the next trusted pull re-anchors.
			return nil, fmt.Errorf("system clock is behind the snapshot's server time anchor (local %s, anchor %s, tolerance 1h) — refusing cache (clock fault or tampering); fix system time, then run cache pull",
				now.Format(time.RFC3339), anchorT.Format(time.RFC3339))
		}
	}
	dek, err := loadDEK()
	if err != nil {
		return nil, fmt.Errorf("cache DEK not found in keychain (run `cache pull` first): %w", err)
	}
	blob, err := os.ReadFile(bin)
	if err != nil {
		return nil, err
	}
	plaintext, err := vaultio.DecryptWithKey(dek, blob)
	if err != nil {
		return nil, fmt.Errorf("cache decrypt failed (the DEK and cache.bin may be from different installs): %w", err)
	}
	var snap store.Snapshot
	if err := json.Unmarshal(plaintext, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}
```

（注意：到龄错误的冻结文本是「— snapshot destroyed; run cache pull to re-enroll」段，哨兵以前缀 `%w — ` 领头，与 DoPull 401 分支 `pull: %w — ...` 同款。）

**`quarantine_report.go`** switch 前段改 reason 分派（其余逐字不动）：

```go
		if blob, merr := os.ReadFile(filepath.Join(dir, "quarantine", "manifest.json")); merr == nil {
			var m quarantineManifest // T3's writer type is the reader's contract
			if json.Unmarshal(blob, &m) == nil {
				fresh, onRecord := manifestVersusMeta(m.TS, metaPath)
				if onRecord && !fresh {
					//（原注释不动）
					return "", false
				}
				// Plan 37 §4: dispatch on exact reason equality. The expiry
				// reason gets its own texts; every other reason — the Plan 34
				// server-rejection one — keeps the legacy texts verbatim.
				expired := m.Reason == expiryReason
				switch {
				case m.State == "done" && len(m.Degraded) > 0 && expired:
					return fmt.Sprintf("cache expired: offline beyond SSHMGR_CACHE_MAX_OFFLINE [DEGRADED: %v] — snapshot destroyed; run cache pull (the device code is still valid unless revoked)", m.Degraded), true
				case m.State == "done" && expired:
					return "cache expired: offline beyond SSHMGR_CACHE_MAX_OFFLINE — snapshot destroyed; run cache pull (the device code is still valid unless revoked)", true
				case m.State == "started" && expired:
					return "cache expiry destruction was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json", true
				case m.State == "done" && len(m.Degraded) > 0:
					return fmt.Sprintf("cache quarantined by server rejection (token revoked?) [DEGRADED: %v] — re-enroll via cache pull with a fresh device code; manual cleanup may be needed", m.Degraded), true
				case m.State == "done":
					return "cache quarantined by server rejection (token revoked?) — re-enroll via cache pull with a fresh device code", true
				case m.State == "started":
					return "cache quarantine was interrupted — the snapshot may still exist; re-enroll via cache pull, or inspect quarantine/manifest.json", true
				}
				return "", false // unknown state — fall through conservatively (not ours)
			}
			//（后续 tier 2 原样不动）
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/clientops/ -count=1 && go test ./internal/cli/ -count=1`
Expected: 全 PASS（含既有 quarantine_report/quarantine 回归——reason 字面量换常量后语义不变）

- [ ] **Step 5: Commit**

```bash
git add internal/clientops/clientops.go internal/clientops/expiry.go internal/clientops/quarantine_report.go internal/clientops/expiry_load_test.go
git commit -m "feat(cache): Plan 37 T2 load 侧三闸+销毁前复查+reason 分派(到龄销毁恒裹哨兵,回拨/provenance/复查失败只拒载)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

### Task 3: 文档联动 + backlog 销项 + 全量收尾

**Files:**
- Modify: `README.md`（env 表加行）
- Modify: `docs/multi-machine.md`（B 配置节）
- Modify: `docs/threat-model.md`（B 语义 + 残余清单）
- Modify: `docs/compat-matrix.md`（纯增量行）
- Modify: `docs/backlog.md:21`（#3b 销项）

**Interfaces:**
- Consumes: T1/T2 已定稿的行为与冻结文案（文档措辞从 spec §1/§5/§7/§10 取）。
- Produces: 无代码接口；验收 = 文档四处落位 + 全量绿。

- [ ] **Step 1: README env 表加行**

在 README 的 env 变量表（`SSHMGR_CACHE_DEK` 行邻近；表若无固定位置，grep `SSHMGR_CACHE_DEK` 定位）加：

```markdown
| `SSHMGR_CACHE_MAX_OFFLINE` | `168h`（Go duration，≥1h；unset/`0` 关） | 开启离线缓存到龄自废：超龄的下次 load/spawn 销毁本地 cache（服务器 Date 锚 + 1h 时钟容差）。详见 docs/multi-machine.md |
```

- [ ] **Step 2: multi-machine.md B 配置节**

在 revoke 语义节之后加（措辞按 spec §10）：

```markdown
## 离线缓存到龄自废（SSHMGR_CACHE_MAX_OFFLINE）

笔记本侧设 `SSHMGR_CACHE_MAX_OFFLINE=168h`（Go duration 文法，**下限 1h**；unset/`0` = 关）即启用：
超龄缓存在**下次 load/spawn 边界**销毁（DEK/设备码删除、密文进 `quarantine/`），重新 `cache pull` 即恢复（设备码未被 revoke 就仍有效）。

运维前提与语义边界：

- **首次启用需联网**：provenance 闸会拒绝一切非服务器锚的旧缓存（含本特性之前拉的）——开 B 后第一次使用前先 `cache pull` 建立服务器锚。
- **两端时钟需基本同步（NTP）**：pull 时 `|server Date − 本地钟| > 1h` 拒拉（skew 闸）；错钟**前跳**超过上限则触发销毁，恢复 = 联网 re-pull。
- **生产建议 ≥24h**：1h 是测试下限；server 钟落后接近 1h 时小上限的缓存可用期趋零（fail-closed 方向，宁可早废重拉）。
- **销毁只在下次运行本客户端时发生**：关机失窃的机器不会自动擦盘——盘上材料保留至下次运行（threat-model 残余清单）。
```

- [ ] **Step 3: threat-model.md 残余登记**

在离线 cache / 切断失效相关小节（grep `revoke` 或 `quarantine` 定位）追加：

```markdown
- **B 时限快照（SSHMGR_CACHE_MAX_OFFLINE，默认关）**：到龄缓存在下次 load/spawn 边界自废（DEK/设备码物理删除）。语义边界如实登记：①无定时执行器——关机失窃的机器不运行客户端，盘上材料保留至下次运行；②运行中会话服务至进程退出；③回拨闸只捕获 >1h 的回拨，容差内单次回拨可延长 ≤1h，反复回拨/冻结时钟可任意续命——属控钟对手，与 FS 控制同级，出范围；④FS 对手可还原旧 bin+DEK+meta 组合绕过（含 backup-restore 恢复路径）。根治仍只有轮换服务器凭据。另：pinned pull 自 Plan 37 起不再跟随重定向（修复响应体可被导出 pin 信任域的传输级缺口）。
```

- [ ] **Step 4: compat-matrix.md 纯增量行**

在表末尾加（版本行归属 owner 发版拍板——**占位注释回写时删**，同 Plan 32-36 惯例）：

```markdown
<!-- v0.10 系（Plan 32-37）同批发版：以下行并入 v0.10.0 还是开 v0.11.0 由 owner 发版拍板，回写时删本注释 -->
| Plan 37 | `SSHMGR_CACHE_MAX_OFFLINE`（默认关）：离线缓存到龄自废——服务器 Date 锚 + provenance + 销毁前复查；meta 新增 `server_anchored` 字段（恒序列化，旧客户端忽略未知字段/新客户端读旧 meta 视为 false，双向兼容）；pinned pull 不再跟随重定向（行为修复） |
```

- [ ] **Step 5: backlog.md #3b 销项**

`docs/backlog.md:21` 的 3b 条目整段改为（保留原文于删除线后，同 #16 惯例）：

```markdown
3b. ~~**B 时限快照（Plan 34 砍出项）**——离线到龄自废（SSHMGR_CACHE_MAX_OFFLINE 形态）。~~ **已落地（Plan 37, 2026-08-25 并 master; spec 三轮 xcheck 收敛 rev3 定稿[owner 突破 C5 ×1，书面级修订免复审] + 3 任务 SDD + 整分支终审; 三件配套落法——服务器时间锚 = 标准 Date 头仅 pinned 采信 + skew 闸 + 缺头 fail-closed、跨进程写串行化 = 设计性消解（锚并入 meta.PulledAt 单写者，无锁）、前向毒化自愈 = 采信口闸门 + 可重写锚; 复审轮实验实证 pinned 302→明文重定向洞 → 全局禁重定向顺带修复; spec/plan 见 docs/superpowers/{specs,plans}/2026-08-25-plan-37-cache-expiry*；owner 真机手工复验（1h 到龄销毁→重拉恢复）待发版前做）**。原文：离线到龄自废（SSHMGR_CACHE_MAX_OFFLINE 形态）。Plan 34 四轮评审实证完整正确性需三件配套：服务器时间锚信任边界（仅 pinned 采信 + skew 校验 + 缺头 fail-closed）+ 跨进程写串行化 + 前向毒化自愈；修复面连续膨胀且为默认关可选件，owner 2026-08-24 拍板砍出。未来要做按 Plan 34 二/三轮评审结论起手（.xcheck/ 评审留底 2026-08-24）。
```

- [ ] **Step 6: 全量回归**

Run: `go build ./... && gofmt -l . && go test ./... -count=1`
Expected: 构建零错、gofmt 零输出、全包绿（Windows 本地不带 `-race`，CI Linux 覆盖）

- [ ] **Step 7: Commit**

```bash
git add README.md docs/multi-machine.md docs/threat-model.md docs/compat-matrix.md docs/backlog.md
git commit -m "docs: Plan 37 B 时限快照文档联动+backlog #3b 销项(threat-model 残余清单/multi-machine 运维前提/compat 增量行)

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Self-Review 记录

1. **Spec coverage**：§1（T1 helper+测试 2）、§2.0-2.4（T1 全闸+测试 6/7/8/9/17）、§3.2-3.6（T2 三闸+测试 1/11/3/4/5/15）、§3.4 复查（T2 测试 16 三腿）、§4 分派（T2 测试 12 + report 改造）、§5 K 双职（T1 const + T2 回拨闸同源）、§6（无锁——代码零锁原语 ✓）、§7（T3 threat-model）、§8 矩阵 1-18（T1: 2/6/7/8/9/17；T2: 1/3/4/5/10/11/12/13/14/15/16/18；14 mid-session = 既有 cache_reload 回归 + B-off 测试；10 毒化自愈 = RecheckAborts+Rollback 组合覆盖）、§9-§11（scope 与骨架对齐）、§12 验收（owner 手工项列 backlog 销项文本）。
2. **Placeholder scan**：无 TBD/「适当处理」；全部测试与实现给全码。
3. **Type consistency**：`cacheMaxOffline`/`readCacheMeta`/`expiryReason`/`serverRejectedReason`/`cacheSkewTolerance`/`ServerAnchored`/`expiryTestHooks`/`ResetExpiryHooksForTest`/`FailNextMetaWriteForTest` 各任务引用一致；测试 helper（`newPinnedTLSServer`/`snapshotHandler`/`ptr`/`readMetaForTest`）T1 定义 T2 同包复用。
