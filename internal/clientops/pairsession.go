// Plan 45 T1 —— PairSession:把 RunPair 的内部步骤提升为导出分步状态机,
// CLI(`sshmgr pair` 的 RunPair 驱动层)与 T2 的 TUI 向导共用同一条管线。
//
// 驱动序(两条路径同一状态机,plan 冻结):
//
//	NewPairSession(执行 URL/pin+TOFU 规则;URL 为空=发现流,校验推迟到 Bind)
//	→(发现/多 broker 选择由驱动层完成)→ Bind(等价校验 + (重)建 transport)
//	→ 驱动层调 IsEnrolled 做"已装判定"(--force 只过闸,Plan 46 零清理先行:
//	Enroll 前不删任何旧槽文件) → Enroll(置 ID/SAS/密钥态/绝对
//	approvalDeadline = enroll 时刻 + pairPollMax)→ WaitApproval(2s 轮询)
//	→(驱动层人闸:CLI = 轮询到 approved 后 y/N;TUI = Finish 前按键)
//	→ Finish(ack + 解密封信封)→ WriteAndPull(先落盘四件套 0600 →
//	pairBeforePullTestHook → DoPull → 成功尾部清 quarantine/)。
//
//	Plan 46 force 时序定案:旧槽材料直到 WriteAndPull 全部成功才被新凭据原子
//	覆盖——任何 enroll/poll/写盘/首拉失败,旧槽文件一字不动;finish 后失败的
//	错误文案统一尾缀双路径恢复指引(pairRecoverHint)。
//
// 哨兵只有两个(410 的 wire 语义=合并:rejected/expired/delivered/unknown
// 不可分,协议冻结):ErrPairGone(410)与 ErrPairTimeout(本地 deadline);
// ctx 取消 = context.Canceled —— 三态可区分。
//
// 纪律:AssumeSAS 的 env(SSHMGR_PAIR_ASSUME_SAS)读取与判定永驻 CLI 驱动层
// (internal/cli/pair.go),本类型不读该 env、不消费 o.AssumeSAS;SAS 的终端
// 打印与 y/N 人闸同属驱动层,本类型零人闸输出。密钥材料(kAck/kCreds 等)全部
// 为未导出字段(测试有 reflect 钉),只经只读 accessor 暴露 SAS/profile 等
// 非敏感视图。
package clientops

import (
	"context"
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
	"os"
	"path/filepath"
	"strings"
	"time"

	"ssh-manager-mcp/internal/instname"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/pairing"
)

var (
	// ErrPairGone —— broker 端 410:本次申请已被终止(被拒/过期/已交付/未知,
	// 合并语义,协议冻结)。终局:重试必须重新 enroll(新 id)。
	ErrPairGone = errors.New("pairing request ended by broker (rejected/expired/410 — terminal for this request)")
	// ErrPairTimeout —— 仅本地 approval window(Enroll 时刻 + pairPollMax)耗尽,
	// broker 侧状态未知(可能仍可批准,但客户端不再等待)。
	ErrPairTimeout = errors.New("local approval window expired")
)

// pairNoteThrottle is the frozen transient-note cadence (30s): a 10-minute
// poll loop against a rate-limited surface must not spam the consumer. The
// throttle lives HERE (session side) so CLI and TUI get the same discipline;
// the CLI driver additionally routes the note through noteTransient (its own
// 30s gate, lockstep — output stays byte-identical to pre-Plan-45).
const pairNoteThrottle = 30 * time.Second

// PollNote 是轮询进度事件(WaitApproval 的回调载荷):pending=仍在等待
// (每轮 202 上报);backoff=瞬态受阻(429/5xx/网络错误,30s 节流后上报);
// Detail=人类可读短句(CLI 路径与既有 stderr 输出逐字一致)。
type PollNote struct {
	Pending bool
	Backoff bool
	Detail  string
}

// PairSession 是一次配对申请的分步状态机。所有字段未导出(密钥材料不出
// 结构体);消费面 = 构造 + 驱动方法 + 只读 accessor。
type PairSession struct {
	// opts 是发起参数(URL 为空 = 发现流,等 Bind 补齐)。Force/AssumeSAS/
	// Stdin/Stdout 由驱动层消费,本类型不读。
	opts PairOpts
	// validated 表示连接参数已完成等价校验(①pin 分级 → ②URL 严格 parse →
	// ①transport 分级三步全过):New 带 URL 直接置位;发现流由 Bind 置位。
	// ForceCleanup 的调用前提、Enroll 的前置都在这一位上。
	validated bool
	// targetURL 是规范化的 https://host:port(transcript/enroll/首拉共用)。
	targetURL string
	// brokerPin 是本次连接的生效 pin(opts.Pin 优先,缺省升格 discovery 的
	// SPKI);TOFU 路径为空。Finish 用它与信封 SPKI 交叉核对。
	brokerPin string
	// brokerName 是 discovery offer 的显示名(URL 直连路径为空)。
	brokerName string
	// client 是校验完成后构建的 HTTP 客户端(pinning 或 TOFU transport,
	// 3xx 永不跟随,单请求 15s 上限)。
	client *http.Client

	// enroll 态(②③)。
	pairID   [32]byte
	kAck     [32]byte
	kCreds   [32]byte
	sas      string
	deadline time.Time // 绝对 approvalDeadline = enroll 时刻 + pairPollMax
	enrolled bool

	// finish 态(⑥):解密后的信封与信任锚 fp。
	env         pairCredsEnvelope
	envelopePin string
	finished    bool

	// write 态(⑦):pair.<name>.mcp.json 实际落点(WriteAndPull 成功后有效)。
	artifactPath string
}

// NewPairSession 构造会话并执行连接参数校验(URL 非空路径):instance 必填
// 且合法 → ①pin 分级(无 pin 且未显式 TOFU → 冻结文案拒绝,先于任何 IO)→
// ②target_url 严格 parse → ①transport 分级。URL 为空 = 发现流:全部校验推迟
// 到 Bind(校验先于清理的不变量由 validated 位承载,坏 URL/坏 pin 绝不消耗
// 一次 force 销毁)。
func NewPairSession(o PairOpts) (*PairSession, error) {
	if o.Instance == "" {
		return nil, errors.New("pair requires --instance (the device name to enroll)")
	}
	if err := instname.Valid(o.Instance); err != nil {
		return nil, err
	}
	s := &PairSession{opts: o}
	if o.URL == "" {
		return s, nil
	}
	if err := s.applyTarget(o.URL, o.Pin, ""); err != nil {
		return nil, err
	}
	return s, nil
}

// applyTarget 执行与旧 RunPair 逐字等价的三步校验 + transport 构建(①pin
// 分级 → ②URL 严格 parse → ①transport 分级),三步全过才置 validated。
// New(URL 非空)与 Bind 共用同一实现 —— 单一来源,两条路径不可能漂移。
func (s *PairSession) applyTarget(rawURL, pin, brokerName string) error {
	// ① pin 分级:无 pin 且未显式 TOFU → 冻结文案拒绝(先于任何 IO)。
	if pin == "" && !s.opts.AllowTOFU {
		return errors.New("refusing TOFU pairing without --pin; pass --allow-tofu to accept an unanchored channel")
	}
	// ② target_url 严格 parse + 规范化(transcript/enroll/首拉共用同一串)。
	// 必须先于 --force 清理(fix round 1 I1):一次 typo 的 URL 绝不能销毁在用
	// 凭据——纯字符串校验零 IO,失败即刻退出,盘上文件分毫不动。
	targetURL, err := canonicalPairTarget(rawURL)
	if err != nil {
		return err
	}
	// ① transport 分级:pin → pinningTransport(TLS 层硬校验,信任锚=pin);
	// TOFU → 自签 cert 过不了系统校验,显式跳过系统验证(加密仍成立),信任由
	// SAS 人工比对 + 密封信封里的 SPKI 补上。与 URL 同理先于 --force 清理:
	// pinningTransport 也是纯校验,坏 pin 不许消耗一次 force 销毁。
	var transport *http.Transport
	if pin != "" {
		transport, err = pinningTransport(pin)
		if err != nil {
			return err
		}
	} else {
		if s.opts.Stderr != nil {
			fmt.Fprintf(s.opts.Stderr, "WARNING: pairing over an UNVERIFIED TLS channel (--allow-tofu): trust will be anchored by the SAS comparison and the sealed envelope's pin\n")
		}
		transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}}
	}
	s.targetURL = targetURL
	s.brokerPin = pin
	s.brokerName = brokerName
	s.client = &http.Client{
		Transport:     transport,
		Timeout:       pairHTTPTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	s.validated = true
	return nil
}

// Bind 为发现流(URL 为空路径必须如此)补上连接参数并(重)建 transport:
// 等价执行 New 跳过的 URL/pin 校验。pin 取 opts.Pin,缺省升格 d.SPKI(与 CLI
// pickDiscovered 的升格规则一致);URL 由 d.Addr:d.TCPPort 拼出。已 validated
// 的会话重复 Bind = 以新 offer 重建(覆盖)。
func (s *PairSession) Bind(d Discovered) error {
	pin := s.opts.Pin
	if pin == "" {
		pin = d.SPKI
	}
	return s.applyTarget(fmt.Sprintf("https://%s:%d", d.Addr, d.TCPPort), pin, d.Name)
}

// ForceCleanup 删除实例的 enroll 态文件(cache.auth.json/cache.bin/cache.meta.json/
// quarantine/,保留 cache.config.json)。Plan 46 起驱动层(CLI RunPair/TUI 向导)
// **不再**于 Enroll 前调用它(force 零清理先行:旧材料直到 WriteAndPull 全部成功
// 才被原子覆盖)——方法保留作显式清理入口,既有直接测试(TestForceCleanup_*)
// 零改动。调用前提 = 会话已完成连接参数校验(New 带 URL 或 Bind 成功):
// 校验先于清理,坏 URL/坏 pin 绝不许消耗一次 force 销毁(fix round 1 I1);
// 未校验调用返回带前提说明的错误。内部复用既有 forceCleanInstance 原函数
// (TestForceCleanInstance_KeepsConfig 直接调它,零改动)。
func (s *PairSession) ForceCleanup() error {
	if !s.validated {
		return errors.New("ForceCleanup requires a validated session (NewPairSession with a URL, or Bind) — validation must precede any --force destruction")
	}
	if err := forceCleanInstance(s.opts.Instance); err != nil {
		return fmt.Errorf("--force cleanup: %w", err)
	}
	return nil
}

// IsEnrolled 报告该实例的 cache.auth.json 是否已在(已 enroll 判定)。由驱动层
// 在 New 之后调用:CLI 据此打印冻结文案 "instance already enrolled; pass
// --force";TUI 表单侧即此判定。stat 出错(非 not-exist)如实上抛。
func IsEnrolled(instance string) (bool, error) {
	p, err := CacheCredPathFor(instance)
	if err != nil {
		return false, err
	}
	_, serr := os.Stat(p)
	if serr == nil {
		return true, nil
	}
	if errors.Is(serr, fs.ErrNotExist) {
		return false, nil
	}
	return false, serr
}

// Enroll 执行 ②③:一次性临时身份(X25519 密钥对 + id32B + cnonce16B)→
// /pair/enroll → 响应验签(绑定 TLS 端点证书)→ ECDH/DeriveKeys → SAS 就位,
// 并置绝对 approvalDeadline(= enroll 时刻 + pairPollMax)。零输出:SAS 三件套
// 的打印留给驱动层(SAS() 取值)。ctx 进 HTTP 管线(NewRequestWithContext)。
func (s *PairSession) Enroll(ctx context.Context) error {
	if !s.validated {
		return errors.New("pair session not validated: call NewPairSession with a URL, or Bind a discovered broker, before Enroll")
	}
	if s.enrolled {
		return errors.New("pair session already enrolled — a retry drives a NEW PairSession (a fresh id is generated each run)")
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
	res, err := pairPost(ctx, s.client, s.targetURL, "/pair/enroll", pairEnrollRequest{
		ID:          hex.EncodeToString(pairID[:]),
		Name:        s.opts.Instance,
		TargetURL:   s.targetURL,
		ClientPub:   base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
		Cnonce:      base64.RawURLEncoding.EncodeToString(cnonce[:]),
		ProfileHint: s.opts.ProfileHint,
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
			return fmt.Errorf("pairing enroll: device name %q is in use on the broker (419) — pick another --instance;或 owner 在 broker 侧执行 `sshmgr cache-tokens revoke %s` 后重试", s.opts.Instance, s.opts.Instance)
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

	transcript := buildPairTranscript(pairID[:], s.opts.Instance, s.targetURL,
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
	s.pairID = pairID
	s.kAck = kAck
	s.kCreds = kCreds
	s.sas = pairing.SAS(transcript, kCreds)
	// 绝对 approvalDeadline = enroll 时刻 + 窗口上限(TUI 倒计时与轮询共用
	// 同一锚,不复制常量)。
	s.deadline = time.Now().Add(pairPollMax)
	s.enrolled = true
	return nil
}

// SAS 返回 ③ 的 6 位短认证串(Enroll 成功后有效)。
func (s *PairSession) SAS() string { return s.sas }

// BrokerName 返回 discovery offer 的显示名;URL 直连路径为空串。
func (s *PairSession) BrokerName() string { return s.brokerName }

// ApprovalDeadline 返回绝对 approvalDeadline(= enroll 时刻 + pairPollMax):
// TUI 倒计时与 WaitApproval 共用同一锚,不复制常量。Enroll 前为零值。
func (s *PairSession) ApprovalDeadline() time.Time { return s.deadline }

// WaitApproval 执行 ④:2s 轮询 ≤ approvalDeadline;200 → 批准到达(nil);
// 410 → ErrPairGone;deadline 尽 → ErrPairTimeout;ctx 取消 → context.Canceled
// —— 三态可区分。每个瞬态受阻(429/5xx/网络错误)经 note 上报 Backoff note
// (30s 节流语义保留,节流在 session 侧);每轮 202 上报 Pending note。
// note 可为 nil。
func (s *PairSession) WaitApproval(ctx context.Context, note func(PollNote)) error {
	if !s.enrolled {
		return errors.New("pair session has no enroll state: run Enroll before WaitApproval")
	}
	var lastNote time.Time
	for {
		res, perr := pairPost(ctx, s.client, s.targetURL, "/pair/poll", pairIDRequest{ID: hex.EncodeToString(s.pairID[:])})
		if perr == nil {
			switch res.StatusCode {
			case http.StatusOK:
				res.Body.Close()
				return nil
			case http.StatusAccepted:
				res.Body.Close()
				s.emit(note, PollNote{Pending: true, Detail: "waiting for the owner's approval"})
			case http.StatusGone:
				res.Body.Close()
				return ErrPairGone
			default:
				res.Body.Close()
				s.emitThrottled(note, &lastNote, fmt.Sprintf("pairing poll: HTTP %d (retrying until the 10m deadline)", res.StatusCode))
			}
		} else {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.emitThrottled(note, &lastNote, fmt.Sprintf("pairing poll: %v (retrying)", perr))
		}
		if !time.Now().Add(pairPollInterval).Before(s.deadline) {
			return ErrPairTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pairPollInterval):
		}
	}
}

// emit 上报一个进度事件(note 为 nil 时静默)。
func (s *PairSession) emit(note func(PollNote), n PollNote) {
	if note != nil {
		note(n)
	}
}

// emitThrottled 以 30s 节流上报一个瞬态受阻事件(与既有 noteTransient 同一
// 门:节流窗口推进先于回调,消费方缺席同样推进)。
func (s *PairSession) emitThrottled(note func(PollNote), last *time.Time, detail string) {
	if time.Since(*last) < pairNoteThrottle {
		return
	}
	*last = time.Now()
	if note != nil {
		note(PollNote{Backoff: true, Detail: detail})
	}
}

// Finish 执行 ⑥:ack → /pair/finish → 解密封信封(事务语义原样),并做信封
// SPKI 信任锚校验(可用性 + 与连接 pin 的一致性,不一致宁拒不吞)。零输出:
// "已授权 profile" 的打印留给驱动层(AuthorizedProfile() 取值)。
func (s *PairSession) Finish(ctx context.Context) error {
	if !s.enrolled {
		return errors.New("pair session has no enroll state: run Enroll + WaitApproval before Finish")
	}
	if s.finished {
		return errors.New("pair session already finished")
	}
	res, err := pairPost(ctx, s.client, s.targetURL, "/pair/finish", pairFinishRequest{
		ID:  hex.EncodeToString(s.pairID[:]),
		Ack: hex.EncodeToString(pairing.FinishAck(s.kAck, s.pairID[:])),
	})
	if err != nil {
		// finish 请求自身传输失败 = 「已提交未收响应」形态可能(serve 可能已
		// 铸码)→ 双路径恢复指引。ctx 取消是用户主动中止,不是待恢复失败。
		if ctx.Err() != nil {
			return fmt.Errorf("pairing finish: %w", err)
		}
		return fmt.Errorf("pairing finish: %w\n%s", err, pairRecoverHint(s.opts.Instance))
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
			return fmt.Errorf("pairing finish: device name %q is in use (419) — owner 在 broker 侧执行 `sshmgr cache-tokens revoke %s` 后重试", s.opts.Instance, s.opts.Instance)
		default:
			return fmt.Errorf("pairing finish: HTTP %d %s", res.StatusCode, clipStr(msg, 200))
		}
	}
	var fr pairFinishResponse
	if err := json.NewDecoder(res.Body).Decode(&fr); err != nil {
		res.Body.Close()
		// 200 已收但信封不完整(响应截断/非 JSON)= serve 已提交、客户端丢了
		// 凭据 → 双路径恢复指引。
		return fmt.Errorf("pairing finish: response not JSON: %w\n%s", err, pairRecoverHint(s.opts.Instance))
	}
	res.Body.Close()
	sealed, err := base64.RawURLEncoding.DecodeString(fr.Sealed)
	if err != nil {
		return fmt.Errorf("pairing finish: sealed is not base64url: %w", err)
	}
	pt, err := pairing.OpenCreds(s.kCreds, sealed)
	if err != nil {
		return fmt.Errorf("pairing finish: sealed envelope failed to open: %w", err)
	}
	var env pairCredsEnvelope
	if err := json.Unmarshal(pt, &env); err != nil {
		return fmt.Errorf("pairing finish: envelope not JSON: %w", err)
	}

	// 首拉的信任锚 = 信封里的 SPKI;与连接 pin 不一致 = 服务端证书轮换或异常,
	// 宁拒不吞。(比对对象 = 本次连接的生效 pin:URL 直连 = opts.Pin,发现流 =
	// 升格后的 SPKI。)
	fp := strings.TrimSpace(env.SPKI)
	if _, ok := mcpserver.ParsePin(fp); !ok {
		return fmt.Errorf("sealed envelope carries no usable SPKI pin (%q) — refusing to anchor the first pull", clipStr(fp, 80))
	}
	if s.brokerPin != "" && fp != s.brokerPin {
		return fmt.Errorf("sealed envelope pins %s but the connection was pinned to %s — refusing (serve cert rotated? re-pair)", clipStr(fp, 24), clipStr(s.brokerPin, 24))
	}
	s.env = env
	s.envelopePin = fp
	s.finished = true
	return nil
}

// AuthorizedProfile 返回信封授权的 profile(Finish 成功后有效)。
func (s *PairSession) AuthorizedProfile() string { return s.env.Profile }

// WriteAndPull 执行 ⑦:先落盘四件套(cache.auth.json / cache.config.json /
// pair.<name>.mcp.json 产物 0600 / --write-mcp 副本 0600)→
// pairBeforePullTestHook → DoPull(PullOpts{Context: ctx})→ 成功尾部清
// quarantine/ 整目录(Plan 46 force 时序:清理在全部成功之后,清理失败仅
// 警告不判失败,下次成功时重清)。成功后 AuthorizedProfile()/ArtifactPath()
// 可读;首拉失配 WARNING 沿既有文案。finish 之后的一切失败(写盘段/首拉段)
// 的错误文案统一尾缀双路径恢复指引(pairRecoverHint)。
func (s *PairSession) WriteAndPull(ctx context.Context) (PullResult, error) {
	if !s.finished {
		return PullResult{}, errors.New("pair session has no sealed credentials: run Finish before WriteAndPull")
	}
	o := s.opts
	env := s.env
	errw := o.Stderr
	if errw == nil {
		errw = io.Discard
	}

	// ⑦ 先落盘(凭据 + max_offline 策略 + .mcp.json 产物)后首拉。写盘段的每
	// 一处失败都尾缀双路径指引(finish 已发生,client 无法可靠分辨 serve 端状态)。
	dir, _, _, _, err := CachePathsFor(o.Instance)
	if err != nil {
		return PullResult{}, fmt.Errorf("%w\n%s", err, pairRecoverHint(o.Instance))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return PullResult{}, fmt.Errorf("%w\n%s", err, pairRecoverHint(o.Instance))
	}
	if err := WriteCacheCredFor(o.Instance, &CacheCred{URL: s.targetURL, Token: env.DeviceCode, Pin: s.envelopePin}); err != nil {
		return PullResult{}, fmt.Errorf("persist cache.auth.json: %w\n%s", err, pairRecoverHint(o.Instance))
	}
	if werr := WriteCacheConfig(dir, env.MaxOffline); werr != nil {
		// 策略写失败与 cache pull 同姿势:WARNING(拉取照跑,只是 cap 未持久化)。
		fmt.Fprintf(errw, "WARNING: could not persist cache.config.json (the max_offline cap is not persisted): %v\n", werr)
	}
	artifact, err := pairArtifactJSON(o.Instance, env.ProjectToken)
	if err != nil {
		return PullResult{}, fmt.Errorf("%w\n%s", err, pairRecoverHint(o.Instance))
	}
	s.artifactPath = filepath.Join(dir, "pair."+o.Instance+".mcp.json")
	if err := writePrivateFile(s.artifactPath, artifact); err != nil {
		return PullResult{}, fmt.Errorf("write %s: %w (credentials are already on disk; finish the first pull with `sshmgr cache pull --instance %s`)\n%s", s.artifactPath, err, o.Instance, pairRecoverHint(o.Instance))
	}
	if o.WriteMCPPath != "" {
		if err := writePrivateFile(o.WriteMCPPath, artifact); err != nil {
			return PullResult{}, fmt.Errorf("write --write-mcp %s: %w\n%s", o.WriteMCPPath, err, pairRecoverHint(o.Instance))
		}
	}

	if pairBeforePullTestHook != nil {
		pairBeforePullTestHook()
	}
	// 首拉(Instance 非空 → gateNamedInstance 锁定设备身份,不存在默认槽重定位,
	// 故 ⑦ 的落盘槽与 res.Instance 恒一致)。ctx 进 PullOpts(DoPull 把 req 建
	// 在它之上,nil = context.Background 旧行为)。首拉段失败同样尾缀双路径指引
	// (ctx 取消是用户主动中止,不算待恢复失败)。
	res2, err := DoPull(s.targetURL, env.DeviceCode, s.envelopePin, PullOpts{StatusOut: errw, Instance: o.Instance, Context: ctx})
	if err != nil {
		if ctx.Err() != nil {
			return PullResult{}, fmt.Errorf("first pull failed: %w — the credentials and artifact ARE already on disk; finish with `sshmgr cache pull --instance %s` (device code in cache.auth.json)", err, o.Instance)
		}
		return PullResult{}, fmt.Errorf("first pull failed: %w — the credentials and artifact ARE already on disk; finish with `sshmgr cache pull --instance %s` (device code in cache.auth.json)\n%s", err, o.Instance, pairRecoverHint(o.Instance))
	}
	if res2.Instance != o.Instance {
		fmt.Fprintf(errw, "WARNING: first pull landed in instance %q (asked for %q) — the pre-pull files were written to %q\n", res2.Instance, o.Instance, o.Instance)
	}
	// Plan 46:quarantine/ 的清理移到全部成功之后(Enroll 前零清理——事故形态
	// 「先删后 enroll 撞 419」根除)。RemoveAll 对不存在的路径返回 nil(存在才
	// 删);失败仅警告,不判本次配对失败——下次成功时重清。
	if rerr := os.RemoveAll(filepath.Join(dir, "quarantine")); rerr != nil {
		fmt.Fprintf(errw, "WARNING: quarantine/ cleanup failed (pairing is fine; 下次成功时重清): %v\n", rerr)
	}
	return res2, nil
}

// pairRecoverHint 是 finish 后一切失败的统一恢复指引(plan 冻结双路径措辞,
// <实例名> 以实际实例名代入)。client 永远无法可靠分辨 serve 是否已标记拉取
// (finish/首拉请求发出后的传输失败两态不可分辨;config 写失败是 WARNING+继续,
// 可能已进首拉)——因此禁止任何"必定自愈"式确定性表述,统一双路径。
func pairRecoverHint(instance string) string {
	return fmt.Sprintf("恢复:直接重跑 `sshmgr pair --force`(或 TUI 重配);若重跑报设备名占用(419),请 owner 在 broker 侧执行 `sshmgr cache-tokens revoke %s` 后再重跑", instance)
}

// ArtifactPath 返回 pair.<name>.mcp.json 的实际落点(WriteAndPull 成功后有效;
// 之前为零值)。
func (s *PairSession) ArtifactPath() string { return s.artifactPath }
