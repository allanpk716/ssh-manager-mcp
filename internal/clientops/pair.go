// Plan 42 批1 T7 —— `sshmgr pair` 一条龙的客户端实现(client 侧全流程,
// spec §3.3):enroll → SAS 三件套 → poll → finish → 先落盘后首拉 → 产物。
//
// 流程冻结点(brief/task):①pin 分级——Pin 非空走 pinningTransport(TLS 层硬
// 校验),无 Pin 必须显式 --allow-tofu(否则冻结文案拒绝);②enroll 前 target_url
// 严格 parse(https + host 规范化,transcript/enroll/首拉共用同一规范串);
// ③enroll 响应即本地算 SAS 并打印三件套;④poll 2s 循环 ≤10min;⑤AssumeSAS 打
// 印 STUB 警示自动过,否则终端 y/N;⑥finish→OpenCreds 解信封;⑦先落盘
// (cache.auth.json + cache.config.json + pair.<name>.mcp.json 产物)后首拉;
// ⑧打印 <project-token> 占位符片段 + 产物路径指引;⑨同名已 enroll 默认拒,
// --force 清 enroll 态文件但保留 cache.config.json(Plan 40 换码 runbook)。
package clientops

import (
	"bufio"
	"bytes"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ssh-manager-mcp/internal/instname"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/pairing"
	"ssh-manager-mcp/internal/store"
)

const (
	// pairDomainPrefix is the LOCAL FROZEN COPY of the transcript domain
	// separator — the composition lives identically on both ends
	// (mcpserver/pairserve.go buildPairTranscript; domain documented in the
	// pairing package). Same mirroring discipline as the discovery wire
	// constants in discover.go: drift breaks every pairing, tests pin both.
	pairDomainPrefix = "sshmgr-pair-v2"
	// pairPollInterval / pairPollMax are the frozen poll cadence (④ 2s 循环,
	// ≤10min). The serve's own poll rate limit (30/min per IP) deliberately
	// sits at the same order — 429s are transient backpressure, not errors.
	pairPollInterval = 2 * time.Second
	pairPollMax      = 10 * time.Minute
	// pairHTTPTimeout caps each pairing HTTP request (the poll LOOP is bounded
	// by pairPollMax, not by the per-request timeout).
	pairHTTPTimeout = 15 * time.Second
)

// PairOpts is one `pair` invocation. Stdin/Stdout/Stderr are injected so the
// CLI and tests drive the same flow.
type PairOpts struct {
	// URL is the broker base URL (https://host:port). Required.
	URL string
	// Pin is the serve cert SPKI fingerprint ("sha256:<hex>"). Non-empty → the
	// pairing HTTP client TLS-pins to it (hard check). Empty is only allowed
	// with AllowTOFU.
	Pin string
	// AllowTOFU admits an unanchored channel (no pin): TLS is still encrypted
	// but unauthenticated until the SAS comparison + the sealed envelope's
	// SPKI anchor it. Default (false) refuses with the frozen TOFU wording.
	AllowTOFU bool
	// AssumeSAS skips the terminal y/N confirmation (the CLI passes
	// SSHMGR_PAIR_ASSUME_SAS=1 in) and prints the frozen STUB warning.
	AssumeSAS bool
	// ProfileHint is the optional display hint the broker's approval surface
	// shows the owner.
	ProfileHint string
	// WriteMCPPath, when set, receives a copy of the pair.<name>.mcp.json
	// artifact (0600).
	WriteMCPPath string
	// Instance is the device name to enroll — required; it is the server-side
	// cache-token name AND the local instances/<name> slot.
	Instance string
	// Force re-enrolls over an existing instance credential: removes
	// cache.auth.json/cache.bin/cache.meta.json/quarantine/ (KEEPs
	// cache.config.json) before the flow. Default refuses with the frozen
	// "instance already enrolled; pass --force" wording.
	Force bool

	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// pairBeforePullTestHook runs between the on-disk writes and the first pull.
// Tests use it to kill the pairing server deterministically (先落盘后首拉 的
// 失败半边);production stays nil.
var pairBeforePullTestHook func()

// wire shapes (frozen Task-5 endpoint contract).
type pairEnrollRequest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	TargetURL   string `json:"target_url"`
	ClientPub   string `json:"client_pub"`
	Cnonce      string `json:"cnonce"`
	ProfileHint string `json:"profile_hint,omitempty"`
}

type pairEnrollResponse struct {
	ServerPub string `json:"server_pub"`
	Snonce    string `json:"snonce"`
	Sig       string `json:"sig"`
}

type pairIDRequest struct {
	ID string `json:"id"`
}

type pairFinishRequest struct {
	ID  string `json:"id"`
	Ack string `json:"ack"`
}

type pairFinishResponse struct {
	Sealed string `json:"sealed"`
}

// pairCredsEnvelope is the finish plaintext ({spki, profile, device_code,
// project_token, max_offline} — frozen shape served by mcpserver).
type pairCredsEnvelope struct {
	SPKI         string `json:"spki"`
	Profile      string `json:"profile"`
	DeviceCode   string `json:"device_code"`
	ProjectToken string `json:"project_token"`
	MaxOffline   string `json:"max_offline"`
}

// pairMCPArtifact is the full .mcp.json handed to the agent host: command
// sshmgr, args [mcp --cache --instance <name>], env.SSHMGR_TOKEN = the
// REAL project token (the printed snippet carries only the placeholder).
type pairMCPArtifact struct {
	MCPServers map[string]pairMCPServer `json:"mcpServers"`
}

type pairMCPServer struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// buildPairTranscript mirrors the frozen composition (see pairDomainPrefix).
func buildPairTranscript(id []byte, name, targetURL string, clientPub, cnonce, serverPub, snonce []byte) []byte {
	return pairing.TranscriptParts(
		[]byte(pairDomainPrefix), id, []byte(name), []byte(targetURL), clientPub, cnonce, serverPub, snonce)
}

// RunPair runs the whole pairing flow. Every frozen step is commented ①–⑨.
func RunPair(o PairOpts) error {
	out := o.Stdout
	if out == nil {
		out = io.Discard
	}
	errw := o.Stderr
	if errw == nil {
		errw = io.Discard
	}

	if o.Instance == "" {
		return errors.New("pair requires --instance (the device name to enroll)")
	}
	if err := instname.Valid(o.Instance); err != nil {
		return err
	}

	// ① pin 分级:无 pin 且未显式 TOFU → 冻结文案拒绝(先于任何 IO)。
	if o.Pin == "" && !o.AllowTOFU {
		return errors.New("refusing TOFU pairing without --pin; pass --allow-tofu to accept an unanchored channel")
	}

	// ② target_url 严格 parse + 规范化(transcript/enroll/首拉共用同一串)。
	// 必须先于 ⑨ 的 --force 清理(fix round 1 I1):一次 typo 的 URL 绝不能
	// 销毁在用凭据——纯字符串校验零 IO,失败即刻退出,盘上文件分毫不动。
	targetURL, err := canonicalPairTarget(o.URL)
	if err != nil {
		return err
	}

	// ① transport 分级:pin → pinningTransport(TLS 层硬校验,信任锚=pin);
	// TOFU → 自签 cert 过不了系统校验,显式跳过系统验证(加密仍成立),信任由
	// SAS 人工比对 + 密封信封里的 SPKI 补上。与 URL 同理先于 --force 清理:
	// pinningTransport 也是纯校验,坏 pin 不许消耗一次 force 销毁。
	var transport *http.Transport
	if o.Pin != "" {
		transport, err = pinningTransport(o.Pin)
		if err != nil {
			return err
		}
	} else {
		fmt.Fprintf(errw, "WARNING: pairing over an UNVERIFIED TLS channel (--allow-tofu): trust will be anchored by the SAS comparison and the sealed envelope's pin\n")
		transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}}
	}

	// ⑨ 同名已 enroll:默认拒;--force 清 enroll 态(保留 cache.config.json)。
	authPath, err := CacheCredPathFor(o.Instance)
	if err != nil {
		return err
	}
	if _, serr := os.Stat(authPath); serr == nil {
		if !o.Force {
			return errors.New("instance already enrolled; pass --force")
		}
		if err := forceCleanInstance(o.Instance); err != nil {
			return fmt.Errorf("--force cleanup: %w", err)
		}
	}

	client := &http.Client{
		Transport:     transport,
		Timeout:       pairHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	// ② 一次性临时身份:X25519 密钥对 + id32B + cnonce16B。
	priv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("keygen: %w", err)
	}
	var pairID [32]byte
	var cnonce [16]byte
	if _, err := rand.Read(pairID[:]); err != nil {
		return err
	}
	if _, err := rand.Read(cnonce[:]); err != nil {
		return err
	}

	// ②③ enroll:严格 parse 过的 target_url 进 transcript;响应当场算 SAS。
	res, err := pairPost(client, targetURL, "/pair/enroll", pairEnrollRequest{
		ID:          hex.EncodeToString(pairID[:]),
		Name:        o.Instance,
		TargetURL:   targetURL,
		ClientPub:   base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		Cnonce:      base64.RawURLEncoding.EncodeToString(cnonce[:]),
		ProfileHint: o.ProfileHint,
	})
	if err != nil {
		return fmt.Errorf("pairing enroll: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		msg := pairErrBody(res)
		switch res.StatusCode {
		case http.StatusTooManyRequests:
			return errors.New("pairing enroll: rate limited or queue full (429) — retry shortly")
		case 419:
			return fmt.Errorf("pairing enroll: device name %q is in use on the broker (419) — pick another --instance", o.Instance)
		case http.StatusConflict:
			return errors.New("pairing enroll: id already enrolled (409) — rerun (a fresh id is generated each run)")
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
			return fmt.Errorf("pairing enroll refused: HTTP %d %s", res.StatusCode, clipStr(msg, 200))
		default:
			return fmt.Errorf("pairing enroll: HTTP %d %s", res.StatusCode, clipStr(msg, 200))
		}
	}
	var er pairEnrollResponse
	if err := json.NewDecoder(res.Body).Decode(&er); err != nil {
		res.Body.Close()
		return fmt.Errorf("pairing enroll: response not JSON: %w", err)
	}
	res.Body.Close()
	serverPub, err := base64.RawURLEncoding.DecodeString(er.ServerPub)
	if err != nil || len(serverPub) != 32 {
		return fmt.Errorf("pairing enroll: bad server_pub (err=%v len=%d)", err, len(serverPub))
	}
	snonce, err := base64.RawURLEncoding.DecodeString(er.Snonce)
	if err != nil || len(snonce) != 16 {
		return fmt.Errorf("pairing enroll: bad snonce (err=%v len=%d)", err, len(snonce))
	}
	sig, err := base64.RawURLEncoding.DecodeString(er.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("pairing enroll: bad sig (err=%v len=%d)", err, len(sig))
	}

	transcript := buildPairTranscript(pairID[:], o.Instance, targetURL,
		priv.PublicKey().Bytes(), cnonce[:], serverPub, snonce)
	// 响应签名绑定 TLS 端点:enroll 响应必须由(被 pin 的)TLS 证书的 ed25519
	// 私钥签署 — 一个 HTTP 层注入者无法伪造(F5 的客户端半边)。
	if res.TLS == nil || len(res.TLS.PeerCertificates) == 0 {
		return errors.New("pairing enroll: no TLS peer certificate to anchor the response signature")
	}
	certPub, ok := res.TLS.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("pairing enroll: serve cert key is %T, want ed25519 — cannot verify the response signature",
			res.TLS.PeerCertificates[0].PublicKey)
	}
	if !ed25519.Verify(certPub, transcript, sig) {
		return errors.New("pairing enroll: response signature does not verify against the TLS certificate — aborting")
	}

	remote, err := ecdh.X25519().NewPublicKey(serverPub)
	if err != nil {
		return err
	}
	ikm, err := priv.ECDH(remote)
	if err != nil {
		return err
	}
	kAck, kCreds := pairing.DeriveKeys(ikm, transcript)
	// ③ SAS 三件套。
	fmt.Fprintf(out, "%s @ %s SAS %s\n", o.Instance, targetURL, pairing.SAS(transcript, kCreds))
	fmt.Fprintf(out, "与 broker 审批面上显示的 SAS 逐位比对,一致才继续。\n")

	// ④ poll 2s 循环 ≤10min;410 终局,其余(429/5xx/网络)瞬态重试。
	approved := false
	deadline := time.Now().Add(pairPollMax)
	var lastNote time.Time
	for !approved {
		res, perr := pairPost(client, targetURL, "/pair/poll", pairIDRequest{ID: hex.EncodeToString(pairID[:])})
		if perr == nil {
			switch res.StatusCode {
			case http.StatusOK:
				res.Body.Close()
				approved = true
			case http.StatusAccepted:
				res.Body.Close()
			case http.StatusGone:
				res.Body.Close()
				return errors.New("pairing poll: request expired or rejected (410) — start over with `sshmgr pair`")
			default:
				res.Body.Close()
				noteTransient(errw, &lastNote, "pairing poll: HTTP %d (retrying until the 10m deadline)", res.StatusCode)
			}
		} else {
			noteTransient(errw, &lastNote, "pairing poll: %v (retrying)", perr)
		}
		if !approved {
			if !time.Now().Add(pairPollInterval).Before(deadline) {
				return errors.New("pairing approval timed out after 10m — start over with `sshmgr pair`")
			}
			time.Sleep(pairPollInterval)
		}
	}

	// ⑤ 确认:AssumeSAS 打印 STUB 警示自动过;否则终端 y/N。
	if o.AssumeSAS {
		fmt.Fprintf(errw, "!! STUB: SAS comparison SKIPPED (SSHMGR_PAIR_ASSUME_SAS)\n")
	} else {
		if o.Stdin == nil {
			return errors.New("no stdin available for the SAS confirmation; set SSHMGR_PAIR_ASSUME_SAS=1 for unattended pairing")
		}
		fmt.Fprintf(out, "确认两侧 SAS 一致? (y/N): ")
		line, _ := bufio.NewReader(o.Stdin).ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
		default:
			return errors.New("SAS comparison not confirmed — pairing aborted (no credentials were issued)")
		}
	}

	// ⑥ finish → OpenCreds 解信封。
	res, err = pairPost(client, targetURL, "/pair/finish", pairFinishRequest{
		ID:  hex.EncodeToString(pairID[:]),
		Ack: hex.EncodeToString(pairing.FinishAck(kAck, pairID[:])),
	})
	if err != nil {
		return fmt.Errorf("pairing finish: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		msg := pairErrBody(res)
		switch res.StatusCode {
		case http.StatusForbidden:
			return errors.New("pairing finish: ack mismatch (403) — the SAS differed, the two sides are NOT the same pair; start over and compare the digits")
		case http.StatusConflict:
			return errors.New("pairing finish: request not approved yet (409)")
		case http.StatusGone:
			return errors.New("pairing finish: approval window over (410) — start over with `sshmgr pair`")
		case 419:
			return fmt.Errorf("pairing finish: device name %q is in use (419)", o.Instance)
		default:
			return fmt.Errorf("pairing finish: HTTP %d %s", res.StatusCode, clipStr(msg, 200))
		}
	}
	var fr pairFinishResponse
	if err := json.NewDecoder(res.Body).Decode(&fr); err != nil {
		res.Body.Close()
		return fmt.Errorf("pairing finish: response not JSON: %w", err)
	}
	res.Body.Close()
	sealed, err := base64.RawURLEncoding.DecodeString(fr.Sealed)
	if err != nil {
		return fmt.Errorf("pairing finish: sealed is not base64url: %w", err)
	}
	pt, err := pairing.OpenCreds(kCreds, sealed)
	if err != nil {
		return fmt.Errorf("pairing finish: sealed envelope failed to open: %w", err)
	}
	var env pairCredsEnvelope
	if err := json.Unmarshal(pt, &env); err != nil {
		return fmt.Errorf("pairing finish: envelope not JSON: %w", err)
	}

	// 首拉的信任锚 = 信封里的 SPKI;与连接 pin 不一致 = 服务端证书轮换或异常,宁拒不吞。
	fp := strings.TrimSpace(env.SPKI)
	if _, ok := mcpserver.ParsePin(fp); !ok {
		return fmt.Errorf("sealed envelope carries no usable SPKI pin (%q) — refusing to anchor the first pull", clipStr(fp, 80))
	}
	if o.Pin != "" && fp != o.Pin {
		return fmt.Errorf("sealed envelope pins %s but the connection was pinned to %s — refusing (serve cert rotated? re-pair)", clipStr(fp, 24), clipStr(o.Pin, 24))
	}
	fmt.Fprintf(out, "已授权 profile: %s\n", env.Profile)

	// ⑦ 先落盘(凭据 + max_offline 策略 + .mcp.json 产物)后首拉。
	dir, _, _, _, err := CachePathsFor(o.Instance)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := WriteCacheCredFor(o.Instance, &CacheCred{URL: targetURL, Token: env.DeviceCode, Pin: fp}); err != nil {
		return fmt.Errorf("persist cache.auth.json: %w", err)
	}
	if werr := WriteCacheConfig(dir, env.MaxOffline); werr != nil {
		// 策略写失败与 cache pull 同姿势:WARNING(拉取照跑,只是 cap 未持久化)。
		fmt.Fprintf(errw, "WARNING: could not persist cache.config.json (the max_offline cap is not persisted): %v\n", werr)
	}
	artifact, err := pairArtifactJSON(o.Instance, env.ProjectToken)
	if err != nil {
		return err
	}
	artifactPath := filepath.Join(dir, "pair."+o.Instance+".mcp.json")
	if err := writePrivateFile(artifactPath, artifact); err != nil {
		return fmt.Errorf("write %s: %w (credentials are already on disk; finish the first pull with `sshmgr cache pull --instance %s`)", artifactPath, err, o.Instance)
	}
	if o.WriteMCPPath != "" {
		if err := writePrivateFile(o.WriteMCPPath, artifact); err != nil {
			return fmt.Errorf("write --write-mcp %s: %w", o.WriteMCPPath, err)
		}
	}

	if pairBeforePullTestHook != nil {
		pairBeforePullTestHook()
	}
	// 首拉(Instance 非空 → gateNamedInstance 锁定设备身份,不存在默认槽重定位,
	// 故 ⑦ 的落盘槽与 res.Instance 恒一致)。
	res2, err := DoPull(targetURL, env.DeviceCode, fp, PullOpts{StatusOut: errw, Instance: o.Instance})
	if err != nil {
		return fmt.Errorf("first pull failed: %w — the credentials and artifact ARE already on disk; finish with `sshmgr cache pull --instance %s` (device code in cache.auth.json)", err, o.Instance)
	}
	if res2.Instance != o.Instance {
		fmt.Fprintf(errw, "WARNING: first pull landed in instance %q (asked for %q) — the pre-pull files were written to %q\n", res2.Instance, o.Instance, o.Instance)
	}

	// ⑧ 占位符片段 + 产物指引。
	snippet, err := pairArtifactJSON(o.Instance, "<project-token>")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "配对完成。产物(0600,含真值 token,勿提交/外发):%s\n", artifactPath)
	if o.WriteMCPPath != "" {
		fmt.Fprintf(out, "副本已写至:%s\n", o.WriteMCPPath)
	}
	fmt.Fprintf(out, "agent 的 .mcp.json 片段(<project-token> 为占位符,真值只在产物内):\n%s\n", snippet)
	return nil
}

// canonicalPairTarget strictly parses the connect address for target_url:
// https scheme + non-empty host; path/query/userinfo dropped, explicit port
// kept. The canonical string feeds the transcript, the enroll payload AND the
// first pull — one byte-identical value on both ends (the serve signs it
// verbatim).
func canonicalPairTarget(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("pair target URL is required (--url or discovery)")
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("pair target URL %q: %w", raw, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("pair target must be https:// (got %q)", raw)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("pair target %q has no host", raw)
	}
	return (&neturl.URL{Scheme: "https", Host: u.Host}).String(), nil
}

// forceCleanInstance removes the instance's enroll-state files ahead of a
// --force re-pair: cache.auth.json, cache.bin, cache.meta.json and the
// quarantine/ subtree. cache.config.json is deliberately PRESERVED — the
// offline cap is machine-level policy (Plan 40), not a credential; the 换码
// runbook must not silently drop it.
func forceCleanInstance(instance string) error {
	dir, bin, metaPath, _, err := CachePathsFor(instance)
	if err != nil {
		return err
	}
	for _, p := range []string{filepath.Join(dir, "cache.auth.json"), bin, metaPath} {
		if rerr := os.Remove(p); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return rerr
		}
	}
	if rerr := os.RemoveAll(filepath.Join(dir, "quarantine")); rerr != nil {
		return rerr
	}
	return nil
}

// pairArtifactJSON renders the .mcp.json artifact (or its placeholder snippet).
func pairArtifactJSON(instance, token string) ([]byte, error) {
	blob, err := json.MarshalIndent(pairMCPArtifact{
		MCPServers: map[string]pairMCPServer{
			"ssh-manager": {
				Command: "sshmgr",
				Args:    []string{"mcp", "--cache", "--instance", instance},
				Env:     map[string]string{"SSHMGR_TOKEN": token},
			},
		},
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(blob, '\n'), nil
}

// writePrivateFile writes blob to path at 0600 (the frozen artifact posture)
// and hardens the Windows ACL like every other client-side credential file.
func writePrivateFile(path string, blob []byte) error {
	if d := filepath.Dir(path); d != "" {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(blob); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return store.HardenACL(path)
}

// pairPost POSTs one JSON body to the /pair surface; 3xx are never followed.
func pairPost(client *http.Client, base, path string, payload any) (*http.Response, error) {
	blob, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

// pairErrBody drains a bounded error body for the message (and closes it).
func pairErrBody(res *http.Response) string {
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
	return strings.TrimSpace(string(b))
}

// noteTransient prints a poll hiccup at most every 30s (a 10-minute poll loop
// against a rate-limited surface must not spam the terminal).
func noteTransient(errw io.Writer, last *time.Time, format string, args ...any) {
	if errw == nil || time.Since(*last) < 30*time.Second {
		return
	}
	*last = time.Now()
	fmt.Fprintf(errw, format+"\n", args...)
}

// clipStr shortens s for an error message.
func clipStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
