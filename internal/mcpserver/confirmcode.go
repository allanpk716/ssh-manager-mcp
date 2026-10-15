package mcpserver

// 命令策略·需确认档与一次性确认码 (ticket #11 / ADR 0007)。判定内核
// (internal/policy) 给出需确认结论时,执行工具在取凭据、建 SSH 连接
// 之前把命令退回给 agent:一行说明 (规则编号 + 为什么危险 + 更安全的
// 替代做法) 加一枚一次性确认码;agent 征得用户同意后带确认码与理由
// 重调,码核销通过才继续走正常执行链。
//
// 确认码只活在执行所在进程的内存里 (不持久化,进程重启即全部失效):
//   - 与「服务器编号 + 命令完整文本」的哈希绑定——命令改一个字符或换
//     一台服务器即失效;
//   - 5 分钟内有效,用一次即作废;
//   - 约 6 位字母数字 (字母表去掉易混淆的 0/O、1/I/L)。
//
// 它与命令策略同一定位:防 agent 误操作的护栏,不防有意绕过,不构成
// 安全边界。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
)

const (
	// confirmationTTL 是确认码的有效期 (规格:5 分钟)。
	confirmationTTL = 5 * time.Minute
	// confirmationCodeLength 是确认码长度 (规格:约 6 位字母数字)。
	confirmationCodeLength = 6
)

// confirmationCodeAlphabet 是确认码字母表:大写字母与数字,去掉肉眼
// 易混淆的 0/O、1/I/L,用户口头核对时不易错。
const confirmationCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// confirmCodeEntry 是一枚已签发、尚未使用的确认码:码文与到期时刻。
// 用一次即从表里
// 删除 (删除即作废,不靠标记位)。
type confirmCodeEntry struct {
	code      string
	expiresAt time.Time
}

// confirmationRegistry 保管进程内全部已签发的确认码。now 是可注入时钟
// (过期行为测试用;生产恒为 time.Now,测试结束即还原——本包测试不并
// 行,无竞态)。
type confirmationRegistry struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]confirmCodeEntry
}

// newConfirmationRegistry 建一张空登记表。
func newConfirmationRegistry() *confirmationRegistry {
	return &confirmationRegistry{now: time.Now, entries: make(map[string]confirmCodeEntry)}
}

// execConfirmations 是两个执行工具共用的进程级登记表——桥与单机
// broker 都在同一进程内签发与核销 (规格:由执行所在进程签发并保存)。
var execConfirmations = newConfirmationRegistry()

// confirmBindingKey 把确认码绑到「服务器编号 + 命令完整文本」的哈希
// 上:命令或服务器任一不同,键就不同,码就核销不上。
func confirmBindingKey(serverID, command string) string {
	sum := sha256.Sum256([]byte(serverID + "\x00" + command))
	return hex.EncodeToString(sum[:])
}

// issue 为「某台服务器上的某条命令」签发一枚新确认码,覆盖同键旧码
// (同一绑定只认最新一枚)。随机源失败时返回错误——这时宁可这次退回
// 失败,也不发一枚可预测的码。
func (r *confirmationRegistry) issue(serverID, command string) (string, error) {
	buf := make([]byte, confirmationCodeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("cannot generate confirmation code: %w", err)
	}
	code := make([]byte, confirmationCodeLength)
	for i, b := range buf {
		code[i] = confirmationCodeAlphabet[int(b)%len(confirmationCodeAlphabet)]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	// 顺手清掉过期项:键空间是 (服务器,命令) 全集,不清会随时间堆积。
	for k, e := range r.entries {
		if now.After(e.expiresAt) {
			delete(r.entries, k)
		}
	}
	r.entries[confirmBindingKey(serverID, command)] = confirmCodeEntry{
		code:      string(code),
		expiresAt: now.Add(confirmationTTL),
	}
	return string(code), nil
}

// consume 核销一枚确认码:键 (服务器 + 命令全文)、码文、有效期三项全
// 对上才返回 true,并立即删除——用一次即作废。任何一项不对都返回
// false,已签发的那枚码保持原样 (agent 可能只是笔误,有效期内还能重试)。
func (r *confirmationRegistry) consume(serverID, command, code string) bool {
	key := confirmBindingKey(serverID, command)
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[key]
	if !ok {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(code), e.code) {
		return false
	}
	if r.now().After(e.expiresAt) {
		delete(r.entries, key)
		return false
	}
	delete(r.entries, key)
	return true
}

// confirmReasonAuditBytes 是确认理由进审计行的长度上限。
const confirmReasonAuditBytes = 4096

// sanitizeConfirmReason 把 agent 给的确认理由整理成能安全进审计行的
// 形态:控制字符换空格 (防在离线侧文件里伪造行),超长在 UTF-8 边界
// 截断。理由是取证片段,不回读。
func sanitizeConfirmReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) <= confirmReasonAuditBytes {
		return s
	}
	cut := s[:confirmReasonAuditBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1] // 退到最后一个完整字符
	}
	return cut
}

// confirmRequiredError 把需确认结论拼成 agent 能直接向用户转述的一行:
// 规则编号 + 为什么危险 + 更安全的替代做法 + 确认码与重调指引。只此
// 一行——不返回规则全文,也不列出其他规则 (ADR 0007:agent 看不到
// 规则清单)。note 非空时插在确认码之前,说明这次为什么重新退回
// (码没对上/缺理由)。
func confirmRequiredError(d policy.Decision, code, note string) error {
	var b strings.Builder
	b.WriteString("confirmation required by command policy")
	if d.RuleID != "" {
		fmt.Fprintf(&b, " [%s]", d.RuleID)
	}
	if d.Explanation != "" {
		fmt.Fprintf(&b, ": %s", d.Explanation)
	}
	if d.Alternative != "" {
		fmt.Fprintf(&b, " — instead: %s", d.Alternative)
	}
	fmt.Fprintf(&b, " | %sconfirmation code %s: ask the user for consent to run this exact command on this server, then call the same tool again with confirmation_code and confirmation_reason; the code is single-use, valid for 5 minutes, and bound to this exact command and server", note, code)
	return errors.New(b.String())
}

// gateExecConfirmation 是两个执行工具共用的需确认门,在拦截档之后、
// 取凭据之前:
//   - 未带确认码:签发新码,返回一行说明 + 码 (granted=false);
//   - 带了码缺理由:退回并说明差什么,已签发的码不作废 (granted=false);
//   - 带了码和理由:核销通过则落「带确认码执行」审计行 (含理由,
//     Status 存命中规则编号) 并放行 (granted=true);核销不过则签发
//     新码重新退回 (granted=false)。
//
// granted=false 时调用方照拦截档的写法换审计动作 (exec-confirm-required
// 家族) 并把 err 返回给 agent;「需确认退回」行由调用方的 defer 审计
// 统一落笔,这里只落「带确认码执行」这一行。confirmedAction 是该行的
// 动作名 (前台 exec-confirmed / 后台 exec-bg-confirmed)。
func gateExecConfirmation(st *store.Store, projectID, serverID, command string, sudo bool, confirmCode, confirmReason string, d policy.Decision, confirmedAction string) (granted bool, err error) {
	if confirmCode != "" && confirmReason == "" {
		return false, errors.New("confirmation_reason is required together with confirmation_code — describe what the user consented to, then call again with the same command, the same confirmation_code and confirmation_reason")
	}
	if confirmCode == "" {
		code, ierr := execConfirmations.issue(serverID, command)
		if ierr != nil {
			return false, ierr
		}
		return false, confirmRequiredError(d, code, "")
	}
	if !execConfirmations.consume(serverID, command, confirmCode) {
		code, ierr := execConfirmations.issue(serverID, command)
		if ierr != nil {
			return false, ierr
		}
		return false, confirmRequiredError(d, code, "the confirmation code was not accepted (wrong code, expired, already used, or issued for a different command or server); a new one follows — ")
	}
	_ = st.WriteAudit(store.AuditRow{
		TS: time.Now(), ProjectID: projectID, ServerID: serverID,
		Action: confirmedAction,
		// Command 列存 JSON 摘要 (照 owner 审计的先例):命令原文 + 确认
		// 理由,事后追查「谁为什么放行了这条命令」两样都在。
		Command: store.MarshalAuditSummary(map[string]any{
			"command": command,
			"reason":  sanitizeConfirmReason(confirmReason),
		}),
		Sudo:   sudo,
		Status: d.RuleID,
	})
	return true, nil
}
