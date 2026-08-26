package sshbroker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// shellQuote wraps s in POSIX single quotes for embedding in a remote shell
// command line. Everything between single quotes is literal (no
// metacharacters), and each embedded quote is re-spliced with '\” — the
// close-quote/escaped-quote/reopen sequence — so any POSIX shell parses the
// result back to exactly s. That completeness is the security boundary of the
// batch-1 wrapper (Plan 41 rev3 §1.1/§4.1); keep it a single point of
// implementation — never hand-splice quoted command lines elsewhere.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildSudoWrapper renders the batch-1 elevated wrapper for cmd (Plan 41 rev3
// §1.1): the WHOLE command — every `;` / `&&` / `|` segment — must execute
// inside the sudo domain. `--` alone cannot span shell separators, so cmd
// travels as one bash -c argument instead. BASH_ENV= (empty) stops
// non-interactive bash from sourcing a startup script (marker-forgery
// injection surface); LC_ALL=C pins sudo's own diagnostics to English — the
// failure-signature classifier below depends on it. Whether LC_ALL reaches
// the inner command is sudoers-dependent (env_keep; compat-matrix row).
// Env-passing discipline: assignments live in the login-shell prefix —
// `sudo --` rejects VAR=val and `exec VAR=val` is a syntax error (both
// verified against a live target).
func buildSudoWrapper(cmd string) string {
	return "BASH_ENV= LC_ALL=C sudo -S -p '' -- bash -c " + shellQuote(cmd)
}

// markerPrefix is the stderr line the batch-2a wrapper injects as the FIRST
// executable output of the inner bash (Plan 41 rev3 §2.2): <nonce> makes it
// unforgeable by command output (a command can echo the literal text but
// cannot guess this call's nonce), $EUID is a bash builtin (immune to NOEXEC's
// execve interception and to PATH problems), and the leading empty echo (added
// by the caller, see buildSudoWrapperWithMarker) terminates sudo's prompt —
// which is written to stderr WITHOUT a trailing newline when sudo -S reads the
// password from a pipe, so the marker would otherwise fuse onto the prompt
// line and defeat line-anchored matching (observed live, cat -A).
const markerPrefix = "__SSHMGR_SUDO_"

// buildSudoWrapperWithMarker is the batch-2a wrapper (Plan 41 rev3 §2.2):
// batch-1's wrapper with the marker prologue prepended to the inner script.
// The two echos are the first executable output of the inner bash; any
// EXECUTABLE output of the user's command necessarily follows the marker
// (bash parses the whole script first — a parse error emits diagnostics with
// zero command output, which is exactly the wrap-failed classification).
func buildSudoWrapperWithMarker(cmd, nonce string) string {
	inner := "echo >&2; echo " + markerPrefix + nonce + ":uid=$EUID >&2; " + cmd
	return "BASH_ENV= LC_ALL=C sudo -S -p '' -- bash -c " + shellQuote(inner)
}

// newSudoNonce returns a fresh 8-byte hex nonce for one sudo call.
func newSudoNonce() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is not survivable for the marker protocol's
		// unforgeability — but it has never been observed and must not brick
		// exec: fall back to a time-derived nonce. Forgery resistance degrades
		// to "guess the nanosecond", still not guessable by command output.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// Sudo outcome vocabulary (Plan 41 rev3 §2.1/§2.3).
const (
	SudoElevated    = "elevated"          // marker attests this uid; command ran privileged
	SudoAuthFailed  = "auth-failed"       // sudo rejected the credential; command did NOT run
	SudoStartFailed = "sudo-start-failed" // sudo could not start the elevated command (policy/binary); did NOT run
	SudoWrapFailed  = "wrap-failed"       // the wrapped shell rejected the command (e.g. bash syntax error); did NOT run
	SudoUnverified  = "unverified"        // no marker — identity/execution unproven (incl. timing anomalies)
)

// SudoMeta is the parsed privilege-elevation metadata of one sudo exec (Plan
// 41 rev3 §2). UID is the marker-attested uid: nil = unproven, 0 = root as a
// REAL attestation (pointer semantics dodge the omitempty-on-zero trap that
// would silently drop uid:0). Diagnostic carries the pre-strip diagnostic
// excerpt for error-text passthrough (empty when elevated).
type SudoMeta struct {
	Outcome    string
	UID        *int
	Diagnostic string
}

// Failure-signature families (Plan 41 rev3 §2.3). Matched only against the
// pre-marker diagnostic region (or, when no marker, the stderr head), so a
// command's own output can never be mistaken for a sudo diagnostic; sudo's
// diagnostics are pinned to English by the wrapper's LC_ALL=C. Priority
// order: auth > start > wrap.
var (
	sudoAuthFailSignatures = []string{
		"incorrect password attempt", // sudo: 1/2/3 incorrect password attempt(s)
		"no password was provided",
		"Account or password is expired",
	}
	sudoStartFailSignatures = []string{
		"is not in the sudoers file",
		"is not allowed to execute", // Sorry, user X is not allowed to execute ... as root on ...
		"unable to execute",
		"sudo: command not found", // sudo: bash: command not found (policy/binary-level)
	}
	sudoWrapFailSignatures = []string{
		"syntax error", // bash: -c: line N: syntax error near unexpected token ...
	}
)

// sudoPromptOpen is the fused-prompt prefix sudo -S writes to stderr without
// a trailing newline (pipe mode); matching tolerates it before the marker.
const sudoPromptOpen = "[sudo] password for "

// matchMarker reports whether line is this call's marker (uid extraction),
// tolerating a fused sudo-prompt prefix. Strictness: after the prefix+nonce
// the rest must be all digits and nothing else — `id -u`/$EUID never emit
// signs, so the digit-only rule closes the match surface.
func matchMarker(line, nonce string) (int, bool) {
	i := strings.Index(line, markerPrefix)
	if i < 0 {
		return 0, false
	}
	s := line[i:]
	want := markerPrefix + nonce + ":uid="
	if !strings.HasPrefix(s, want) {
		return 0, false
	}
	rest := s[len(want):]
	if rest == "" {
		return 0, false
	}
	for _, ch := range rest {
		if ch < '0' || ch > '9' {
			return 0, false
		}
	}
	uid, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return uid, true
}

// stripPrompt removes an in-line `[sudo] password for <user>: ` substring
// (trailing space optional across sudo versions). ok reports whether a prompt
// was present. Only ever applied to the pre-marker diagnostic region — sudo
// writes the prompt before the inner bash starts, so it can never legitimately
// appear after the marker (where a grep over sudo logs would live).
func stripPrompt(line string) (string, bool) {
	i := strings.Index(line, sudoPromptOpen)
	if i < 0 {
		return line, false
	}
	rest := line[i+len(sudoPromptOpen):]
	j := strings.Index(rest, ":")
	if j < 0 {
		return line, false // "[sudo] password for " without the colon — not a complete prompt
	}
	after := rest[j+1:]
	after = strings.TrimPrefix(after, " ")
	return line[:i] + after, true
}

// sudoStderrProcessor is the stream-side stderr filter + metadata collector
// wrapped around the caller's stderr writer by runSudoSession (Plan 41 rev3
// §2.2-§2.4). Both the foreground ExecSudo and the background engine ride it,
// so the calling side only ever sees the CLEANED stream:
//
//   - pre-marker region: the marker line is consumed (its uid recorded —
//     FIRST match only; later same-nonce lines are stripped as forgeries, a
//     command can read the nonce via /proc/$PPID/cmdline and replay it);
//     prompt substrings are stripped; lines that become empty BY stripping
//     are dropped; one held-back empty line directly before the marker is
//     dropped (the injected blank echo's only possible position — other
//     pre-marker blank lines, e.g. sudo's lecture, pass through);
//   - post-marker region: verbatim, never touched (grep-over-sudo-logs and
//     any other legitimate command output containing prompt-like literals);
//   - the pre-marker raw head (≤8 lines) is kept for failure classification.
//
// Line assembly is streaming-safe: a line split across Write chunks stays in
// the partial buffer until its newline arrives; flush() emits any unterminated
// tail at stream end (pipe-mode prompts have no newline of their own).
type sudoStderrProcessor struct {
	dst          io.Writer
	nonce        string
	uid          *int
	seenMarker   bool
	diag         []string // raw (unstripped) pre-marker head, ≤ diagHeadLines
	pendingEmpty bool     // a pre-marker blank line held back (injected-blank rule)
	partial      []byte
}

const diagHeadLines = 8

func newSudoStderrProcessor(dst io.Writer, nonce string) *sudoStderrProcessor {
	return &sudoStderrProcessor{dst: dst, nonce: nonce}
}

func (p *sudoStderrProcessor) Write(b []byte) (int, error) {
	p.partial = append(p.partial, b...)
	for {
		i := bytes.IndexByte(p.partial, '\n')
		if i < 0 {
			break
		}
		line := string(p.partial[:i])
		p.partial = p.partial[i+1:]
		p.line(line)
	}
	return len(b), nil
}

// line processes one complete newline-terminated line.
func (p *sudoStderrProcessor) line(line string) {
	if p.seenMarker {
		// Post-marker: the command's own output — never touched, EXCEPT a
		// same-nonce marker replay (the command can read the nonce via
		// /proc/$PPID/cmdline and echo it back; the first marker already won,
		// and the nonce is unguessable so any occurrence here is a forgery).
		if strings.Contains(line, markerPrefix+p.nonce) {
			return
		}
		io.WriteString(p.dst, line+"\n")
		return
	}
	p.recordDiag(line)

	if uid, ok := matchMarker(line, p.nonce); ok {
		p.uid = &uid // first (and decisive) marker
		p.seenMarker = true
		p.pendingEmpty = false // the held-back blank was the injected echo
		return                 // marker line consumed
	}
	stripped, hadPrompt := stripPrompt(line)
	switch {
	case hadPrompt && strings.TrimSpace(stripped) == "":
		// Prompt-only line (or prompt + nothing else): drop.
		return
	case stripped == "":
		// Blank line pre-marker: hold back — it is the injected blank echo iff
		// the marker follows; otherwise (sudo lecture etc.) flush it first.
		if p.pendingEmpty {
			io.WriteString(p.dst, "\n")
		}
		p.pendingEmpty = true
		return
	default:
		if p.pendingEmpty {
			io.WriteString(p.dst, "\n")
			p.pendingEmpty = false
		}
		io.WriteString(p.dst, stripped+"\n")
	}
}

// recordDiag keeps the raw pre-marker head for classification.
func (p *sudoStderrProcessor) recordDiag(line string) {
	if len(p.diag) < diagHeadLines {
		p.diag = append(p.diag, line)
	}
}

// flush emits the unterminated tail at stream end (prompts have no newline).
func (p *sudoStderrProcessor) flush() {
	if len(p.partial) == 0 {
		return
	}
	line := string(p.partial)
	p.partial = nil
	if p.seenMarker {
		io.WriteString(p.dst, line)
		return
	}
	p.recordDiag(line)
	if uid, ok := matchMarker(line, p.nonce); ok {
		if p.uid == nil {
			p.uid = &uid
		}
		p.seenMarker = true
		return
	}
	stripped, hadPrompt := stripPrompt(line)
	if hadPrompt && strings.TrimSpace(stripped) == "" {
		return
	}
	io.WriteString(p.dst, stripped)
	// Leave pendingEmpty held: at stream end with no marker following it is
	// indistinguishable from trailing sudo noise — dropping one blank is the
	// conservative choice (the injected echo never ran in no-marker paths).
}

// classify assembles the final SudoMeta (Plan 41 rev3 §2.3, evaluation order:
// marker → timing anomaly → signature families → unverified).
func (p *sudoStderrProcessor) classify() SudoMeta {
	diag := strings.Join(p.diag, "\n")
	firstNonEmpty := ""
	for _, l := range p.diag {
		if strings.TrimSpace(l) != "" {
			firstNonEmpty = l
			break
		}
	}
	if p.seenMarker {
		// Marker present = the command executed and the uid is attested. A
		// failure signature in the PRE-marker region is a timing anomaly
		// (forgery or invariant break): the command DID run, so the
		// did-NOT-run outcomes contradict it — pin to unverified, no error.
		if containsAny(diag, sudoAuthFailSignatures...) || containsAny(diag, sudoStartFailSignatures...) || containsAny(diag, sudoWrapFailSignatures...) {
			return SudoMeta{Outcome: SudoUnverified, Diagnostic: firstNonEmpty}
		}
		return SudoMeta{Outcome: SudoElevated, UID: p.uid}
	}
	// No marker: the stderr head is the wrapper-layer diagnostic region (the
	// command did not run — auth/start/parse failures all precede execution;
	// an extreme truncation could lose the marker mid-stream, which the
	// head-limited match keeps from mistaking command output for diagnostics).
	switch {
	case containsAny(diag, sudoAuthFailSignatures...):
		return SudoMeta{Outcome: SudoAuthFailed, Diagnostic: firstNonEmpty}
	case containsAny(diag, sudoStartFailSignatures...):
		return SudoMeta{Outcome: SudoStartFailed, Diagnostic: firstNonEmpty}
	case containsAny(diag, sudoWrapFailSignatures...):
		return SudoMeta{Outcome: SudoWrapFailed, Diagnostic: firstNonEmpty}
	}
	return SudoMeta{Outcome: SudoUnverified, Diagnostic: firstNonEmpty}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// ctxErrOr reports a ctx cancellation as itself, so a caller sees context.Canceled
// (or DeadlineExceeded) rather than the lower-level error it surfaced as. Used in
// runSudoSession: a cancel arriving in the narrow sess.Start → stdin.Write window
// closes the session (via the watchdog) before the password write completes,
// surfacing as a generic Start/Write error that would otherwise reach the caller
// as-is instead of as the cancellation it is — and at the MCP layer would map to
// status="error" rather than status="cancelled".
func ctxErrOr(ctx context.Context, err error) error {
	if ce := ctx.Err(); ce != nil {
		return ce
	}
	return err
}

// runSessionRaw is the transport kernel under every elevated exec: starts
// `wrapped` as one SSH exec, feeds pass (a single line, then close) to the
// session's stdin when non-empty — the sudo -S password feed — and honors
// timeout/ctx exactly like runSession. It imposes NO wrapper and NO stderr
// processing: callers compose (runSudoSession adds the marker wrapper and the
// stderr processor; ExecSudoWrapped passes a caller-prebuilt command through).
func (c *Client) runSessionRaw(ctx context.Context, wrapped, pass string, timeout time.Duration, stdout, stderr io.Writer) (exitCode int, timedOut bool, err error) {
	sess, err := c.c.NewSession()
	if err != nil {
		return 0, false, err
	}
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return 0, false, err
	}
	sess.Stdout = stdout
	sess.Stderr = stderr

	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = sess.Signal(ssh.SIGKILL)
			_ = sess.Close() // some servers ignore SIGKILL; closing forces Wait to return
		case <-done:
		}
	}()

	if err := sess.Start(wrapped); err != nil {
		return 0, false, ctxErrOr(ctx, err)
	}
	if pass != "" {
		pw := make([]byte, len(pass)+1)
		copy(pw, pass)
		pw[len(pass)] = '\n'
		if _, err := stdin.Write(pw); err != nil {
			return 0, false, ctxErrOr(ctx, err)
		}
	}
	stdin.Close()

	err = sess.Wait()
	switch ctx.Err() {
	case context.DeadlineExceeded:
		return 0, true, nil // timeout is a result, not an error
	case context.Canceled:
		return 0, false, ctx.Err() // caller cancellation — surface as an error, not flagged as TimedOut
	}
	if exitErr, ok := err.(*ssh.ExitError); ok {
		return exitErr.ExitStatus(), false, nil // non-zero exit is a result, not an error
	}
	return 0, false, err
}

// runSudoSession is the writer-seam kernel behind ExecSudo (and the background
// engine's privileged variant): like runSession but running cmd through sudo -S
// with the marker prologue (Plan 41 §2.2) and feeding pass to sudo's stdin
// (password line, then close) before waiting. The caller's stderr writer is
// wrapped by sudoStderrProcessor, so callers only ever see the cleaned stream
// and receive the parsed SudoMeta alongside the usual triple; ctx, timeout,
// and the (exitCode, timedOut, err) classification are identical to runSession.
func (c *Client) runSudoSession(ctx context.Context, cmd, pass string, timeout time.Duration, stdout, stderr io.Writer) (exitCode int, timedOut bool, err error, sudoMeta SudoMeta) {
	nonce := newSudoNonce()
	proc := newSudoStderrProcessor(stderr, nonce)
	wrapped := buildSudoWrapperWithMarker(cmd, nonce)
	exitCode, timedOut, err = c.runSessionRaw(ctx, wrapped, pass, timeout, stdout, proc)
	proc.flush()
	return exitCode, timedOut, err, proc.classify()
}

// ExecSudoWrapped runs a caller-prebuilt wrapped sudo command (Plan 41 §3:
// exec_context composes its own shape — the sshenv section must run in the
// login-shell layer BEFORE the exec into sudo, which the generic marker
// wrapper cannot express). The password is fed to sudo's stdin exactly as in
// ExecSudo; NO marker prologue is injected and NO stderr processing happens —
// the caller owns the whole stream. maxBytes/timeout semantics as in Exec.
func (c *Client) ExecSudoWrapped(ctx context.Context, wrapped string, sudoPassword []byte, timeout time.Duration, maxBytes int64) (ExecResult, error) {
	stdout := &cappedBuffer{cap: maxBytes}
	stderr := &cappedBuffer{cap: maxBytes}
	exitCode, timedOut, err := c.runSessionRaw(ctx, wrapped, string(sudoPassword), timeout, stdout, stderr)
	res := ExecResult{
		Stdout:      stdout.buf.String(),
		Stderr:      stderr.buf.String(),
		StdoutBytes: stdout.total,
		StderrBytes: stderr.total,
		Truncated:   stdout.truncated || stderr.truncated,
		ExitCode:    exitCode,
		TimedOut:    timedOut,
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

// ExecSudoWriters 是 runSudoSession 内核的导出 writer-seam (Plan 32 T4: 后台
// 引擎的特权变体——调用方自带 io.Writer, pass 喂入后即弃, 不落任何记录)。
// 分类三元组与 runSudoSession 恒同; timeout 语义照旧 (引擎传 0)。批 2a
// (Plan 41 §2) 起附带 SudoMeta——调用方(后台引擎)可据此记录提权结果。
func (c *Client) ExecSudoWriters(ctx context.Context, cmd, pass string, timeout time.Duration, stdout, stderr io.Writer) (exitCode int, timedOut bool, err error, sudoMeta SudoMeta) {
	return c.runSudoSession(ctx, cmd, pass, timeout, stdout, stderr)
}

// ExecSudo runs cmd with privilege escalation via `sudo -S`, feeding sudoPassword
// to sudo's stdin. ctx is honored exactly as in Exec (cancel → ctx.Err(),
// TimedOut stays false). Use this when the remote user needs a password for sudo;
// for NOPASSWD sudo, plain Exec(ctx, "sudo "+cmd, …) suffices. maxBytes has the
// same meaning as in Exec (0 = unlimited).
//
// ExecSudo is a thin shell over runSudoSession: it supplies cappedBuffer writers
// and folds the kernel's (exitCode, timedOut, err, SudoMeta) into ExecResult —
// Stderr is the CLEANED stream (marker/prompt stripped), Sudo carries the
// five-state outcome with the marker-attested uid.
func (c *Client) ExecSudo(ctx context.Context, cmd string, sudoPassword []byte, timeout time.Duration, maxBytes int64) (ExecResult, error) {
	stdout := &cappedBuffer{cap: maxBytes}
	stderr := &cappedBuffer{cap: maxBytes}
	exitCode, timedOut, err, meta := c.runSudoSession(ctx, cmd, string(sudoPassword), timeout, stdout, stderr)
	res := ExecResult{
		Stdout:      stdout.buf.String(),
		Stderr:      stderr.buf.String(),
		StdoutBytes: stdout.total,
		StderrBytes: stderr.total,
		Truncated:   stdout.truncated || stderr.truncated,
		ExitCode:    exitCode,
		TimedOut:    timedOut,
		Sudo:        &meta,
	}
	// ExitError is already folded into exitCode by the kernel; the shell does no
	// further classification — a non-nil err here is cancel or a genuine failure.
	if err != nil {
		return res, err
	}
	return res, nil
}
