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
// --force 只过闸不预清理(Plan 46 零清理先行:enroll/轮询任何失败,旧槽文件
// 一字不动;新凭据经 WriteAndPull 原子覆盖,quarantine/ 于成功尾部清理,
// cache.config.json 本就保留——Plan 40 换码 runbook)。
package clientops

import (
	"bufio"
	"bytes"
	"context"
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
	// pairHTTPTimeout caps each pairing HTTP request (the poll LOOP is bounded
	// by pairPollMax, not by the per-request timeout).
	pairHTTPTimeout = 15 * time.Second
)

// pairPollInterval / pairPollMax are the frozen poll cadence (④ 2s 循环,
// ≤10min). The serve's own poll rate limit (30/min per IP) deliberately
// sits at the same order — 429s are transient backpressure, not errors.
// Plan 45 T1: 转 var 作为测试缝(timeout 用例缩窗),生产值不变。
var (
	pairPollInterval = 2 * time.Second
	pairPollMax      = 10 * time.Minute
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
	// Force re-enrolls over an existing instance credential. Plan 46 零清理
	// 先行: NOTHING is deleted before Enroll — any enroll/poll failure leaves
	// the old slot byte-identical; on WriteAndPull success the fresh material
	// atomically replaces the old files and the quarantine/ subtree is cleaned
	// at the success tail. Default refuses with the frozen
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

// RunPair runs the whole pairing flow as the CLI DRIVER over PairSession
// (Plan 45 T1):步骤已提升为导出状态机(pairsession.go,CLI 与 TUI 共用单一
// 管线),本函数只保留 CLI 独占面 —— 冻结次序 ④⑤(enroll → 轮询 → SAS y/N
// 确认 → finish)与全部 frozen wordings 逐字不变;AssumeSAS 的 env 判定与终端
// y/N 永驻本层,discovery/多 broker 选择在 internal/cli/pair.go。轮询瞬态提示
// 经 WaitApproval 的 note 回调回流到既有 stderr 输出点(410/超时改为
// ErrPairGone/ErrPairTimeout 哨兵,plan 裁决)。Every frozen step is commented ①–⑨.
func RunPair(o PairOpts) error {
	out := o.Stdout
	if out == nil {
		out = io.Discard
	}
	errw := o.Stderr
	if errw == nil {
		errw = io.Discard
	}

	// instance 必填 + 合法;①pin 分级;②target_url 严格 parse;①transport
	// 分级(URL 非空路径在 NewPairSession 内一次完成,次序与旧实现一致)。
	s, err := NewPairSession(o)
	if err != nil {
		return err
	}

	// ② 驱动层兜底 parse:CLI 路径 URL 必非空(cli/pair.go 的 discovery 会补
	// 齐);为空时与旧实现同文案拒绝,同时为 ③ 的 SAS 行取与 session 同一规范串。
	targetURL, err := canonicalPairTarget(o.URL)
	if err != nil {
		return err
	}

	// ⑨ 同名已 enroll(驱动层 IsEnrolled 判定):默认拒。--force 只过闸——
	// Plan 46 零清理先行:Enroll 前不删任何旧槽文件(真实事故形态「先删后
	// enroll 撞 419 → 半配对死槽」根除);旧材料直到 WriteAndPull 全部成功才
	// 被新凭据原子覆盖,quarantine/ 于成功尾部清理。
	enrolled, err := IsEnrolled(o.Instance)
	if err != nil {
		return err
	}
	if enrolled && !o.Force {
		return errors.New("instance already enrolled; pass --force")
	}

	ctx := context.Background()

	// ②③ enroll:严格 parse 过的 target_url 进 transcript;响应当场算 SAS。
	if err := s.Enroll(ctx); err != nil {
		return err
	}
	// ③ SAS 三件套(逐字冻结)。
	fmt.Fprintf(out, "%s @ %s SAS %s\n", o.Instance, targetURL, s.SAS())
	fmt.Fprintf(out, "与 broker 审批面上显示的 SAS 逐位比对,一致才继续。\n")

	// ④ poll 2s 循环 ≤10min(ApprovalDeadline 锚);410 → ErrPairGone(合并
	// 语义),deadline 尽 → ErrPairTimeout,ctx 取消三者可区分;瞬态(429/5xx/
	// 网络)backoff note 回流到既有 stderr 输出点(30s 节流,session 与
	// noteTransient 同步推进,输出逐字节不变)。
	var lastNote time.Time
	if err := s.WaitApproval(ctx, func(n PollNote) {
		if n.Backoff {
			noteTransient(errw, &lastNote, "%s", n.Detail)
		}
	}); err != nil {
		return err
	}

	// ⑤ 确认:AssumeSAS 打印 STUB 警示自动过;否则终端 y/N(CLI 独占人闸;
	// session 不读 SSHMGR_PAIR_ASSUME_SAS)。
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

	// ⑥ finish → 解密封信封(ack 校验事务语义原样)。
	if err := s.Finish(ctx); err != nil {
		return err
	}
	fmt.Fprintf(out, "已授权 profile: %s\n", s.AuthorizedProfile())

	// ⑦ 先落盘四件套(0600)→ pairBeforePullTestHook → 首拉。
	if _, err := s.WriteAndPull(ctx); err != nil {
		return err
	}

	// ⑧ 占位符片段 + 产物指引(逐字冻结)。
	snippet, err := pairArtifactJSON(o.Instance, "<project-token>")
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "配对完成。产物(0600,含真值 token,勿提交/外发):%s\n", s.ArtifactPath())
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

// forceCleanInstance removes the instance's enroll-state files: cache.auth.json,
// cache.bin, cache.meta.json and the quarantine/ subtree. cache.config.json is
// deliberately PRESERVED — the offline cap is machine-level policy (Plan 40),
// not a credential; the 换码 runbook must not silently drop it. Plan 46: only
// reached via the retained ForceCleanup entry (drivers no longer pre-clean
// before Enroll — zero-cleanup-first force semantics).
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
// Plan 46: the write is ATOMIC — unique temp file in the target directory +
// rename (shared atomicWriteUnique, same posture as cache.auth.json/bin/meta)
// so a mid-write failure never leaves a torn artifact; both the pair.<name>.mcp.json
// artifact and the --write-mcp copy go through here.
func writePrivateFile(path string, blob []byte) error {
	if d := filepath.Dir(path); d != "" {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	if err := atomicWriteUnique(path, blob); err != nil {
		return err
	}
	return store.HardenACL(path)
}

// pairPost POSTs one JSON body to the /pair surface; 3xx are never followed.
// Plan 45 T1 ctx 管线:请求建在 ctx 之上(NewRequestWithContext),enroll/poll/
// finish 的取消随调用方的 ctx 传播。
func pairPost(ctx context.Context, client *http.Client, base, path string, payload any) (*http.Response, error) {
	blob, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(blob))
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
