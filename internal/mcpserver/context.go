package mcpserver

// exec_context — Plan 41 §3 (批 2b). One round returns the execution
// channel's TRUE context (uid/uid_map/tty/LSM label/SSH provenance/process
// tree), replacing the hand-assembled `id; cat /proc/self/uid_map; …` probes
// that ride the composite-command semantics and invite the exact
// "mystery EACCES" misdiagnosis this tool exists to end (see the 2026-08-26
// feedback incident: five-plus rounds misattributing a wrapper defect to a
// server-side interception layer).
//
// Two capture shapes (spec rev3 §3):
//   - sudo=false: one plain command run by the login shell — the sshenv
//     section and the context body all execute at login-user level;
//   - sudo=true: the sshenv section runs in the LOGIN-SHELL layer first
//     (sudo's env_reset empties SSH_CLIENT/SSH_CONNECTION in the privileged
//     layer — capturing there yields misleading empty strings), then `exec`
//     hands over to `env LC_ALL=C sudo … bash -c <body>`. `env` is mandatory:
//     `exec LC_ALL=C sudo` is a syntax error (exec takes a command name) and
//     `sudo --` rejects VAR=val — both verified against a live target. This
//     is a deliberate exception to the "builtin-first" marker-protocol
//     discipline: exec_context is a diagnostic path, not the marker path.
//
// Output parsing walks `__SSHMGR_CTX_<nonce>:<field>` section markers on
// stdout; best-effort per field (an unreadable /proc path yields "none", a
// failed parse yields the zero value — the tool reports, it never guesses).

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

const ctxSectionPrefix = "__SSHMGR_CTX_"

// newCtxNonce returns a hex nonce for one exec_context call (section markers
// are unguessable by any concurrent output on the stream).
func newCtxNonce() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// buildCtxBody renders the privileged-layer context body (one command per
// section; $$ keeps referring to the running shell inside $() — the comm probe
// reads /proc/$$/comm, NOT /proc/self/comm, which inside a command
// substitution points at cat itself).
func buildCtxBody(n string) string {
	m := ctxSectionPrefix + n + ":"
	return strings.Join([]string{
		"echo " + m + "id; id",
		"echo " + m + "tty; [ -t 0 ] && echo tty || echo no-tty",
		"echo " + m + "uidmap; cat /proc/self/uid_map",
		"echo " + m + "lsm; cat /proc/self/attr/current 2>/dev/null || echo none",
		`echo ` + m + `proc; echo "pid=$$ ppid=$PPID comm=$(cat /proc/$$/comm)"`,
	}, "\n")
}

// buildCtxCommand assembles the full command for the requested channel:
// plain (login shell) or elevated (sshenv pre-capture + exec into sudo).
func buildCtxCommand(n string, sudo bool) string {
	sshenv := fmt.Sprintf("echo %s%s:sshenv C=[$SSH_CLIENT] S=[$SSH_CONNECTION]; ", ctxSectionPrefix, n)
	if !sudo {
		return sshenv + buildCtxBody(n)
	}
	// env is load-bearing (see file header); shellQuote is the sshbroker
	// single-point discipline for anything entering bash -c.
	return sshenv + "exec env LC_ALL=C sudo -S -p '' -- bash -c " + shellQuoteForCtx(buildCtxBody(n))
}

// shellQuoteForCtx mirrors sshbroker's single-quote quoting. mcpserver cannot
// reach the unexported original; keeping the identical algorithm next to its
// only consumer, with a test pinning byte-equality against a known encoding.
func shellQuoteForCtx(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// parseCtxSections splits raw stdout into section bodies keyed by field name.
func parseCtxSections(raw, nonce string) map[string]string {
	out := map[string]string{}
	prefix := ctxSectionPrefix + nonce + ":"
	var cur string
	var buf []string
	flush := func() {
		if cur != "" {
			out[cur] = strings.Join(buf, "\n")
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, prefix) {
			flush()
			rest := strings.TrimPrefix(line, prefix)
			// Section markers may carry their value inline (the sshenv echo)
			// or stand alone with the body on following lines.
			if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
				cur = rest[:sp]
				buf = []string{rest[sp+1:]}
			} else {
				cur = rest
				buf = nil
			}
			continue
		}
		if cur != "" {
			buf = append(buf, line)
		}
	}
	flush()
	return out
}

// parseCtxOutput assembles the structured output from the section bodies
// (best-effort per field: zero value on parse failure, never an error).
func parseCtxOutput(raw, nonce string, elevated bool) ExecContextOutput {
	sec := parseCtxSections(raw, nonce)
	o := ExecContextOutput{Elevated: elevated}

	if idLine, ok := firstLine(sec["id"]); ok {
		for _, field := range strings.Fields(idLine) {
			end := strings.IndexByte(field, '(')
			if end < 0 {
				end = len(field)
			}
			switch {
			case strings.HasPrefix(field, "uid="):
				o.UID, _ = strconv.Atoi(field[4:end])
			case strings.HasPrefix(field, "gid="):
				o.GID, _ = strconv.Atoi(field[4:end])
			case strings.HasPrefix(field, "groups="):
				o.Groups = strings.TrimPrefix(field, "groups=")
			}
		}
	}
	if ttyLine, ok := firstLine(sec["tty"]); ok {
		if ttyLine == "tty" || ttyLine == "no-tty" {
			o.TTY = ttyLine
		}
	}
	if um, ok := firstLine(sec["uidmap"]); ok {
		o.UIDMap = um
	}
	if lsm, ok := firstLine(sec["lsm"]); ok {
		o.LSMLabel = lsm
	}
	if envLine, ok := firstLine(sec["sshenv"]); ok {
		if v, ok := bracketValue(envLine, 'C'); ok {
			o.SSHClient = v
		}
		if v, ok := bracketValue(envLine, 'S'); ok {
			o.SSHConnection = v
		}
	}
	if procLine, ok := firstLine(sec["proc"]); ok {
		for _, field := range strings.Fields(procLine) {
			switch {
			case strings.HasPrefix(field, "pid="):
				o.PID, _ = strconv.Atoi(strings.TrimPrefix(field, "pid="))
			case strings.HasPrefix(field, "ppid="):
				o.PPID, _ = strconv.Atoi(strings.TrimPrefix(field, "ppid="))
			case strings.HasPrefix(field, "comm="):
				o.Comm = strings.TrimPrefix(field, "comm=")
			}
		}
	}
	return o
}

func firstLine(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i], true
	}
	return s, true
}

// bracketValue extracts the [...] payload following `X=` from a line of the
// form `C=[1.2.3.4 61741 22] S=[...]`.
func bracketValue(line string, key byte) (string, bool) {
	idx := strings.IndexByte(line, key)
	if idx < 0 || idx+2 > len(line) || line[idx+1] != '=' || line[idx+2] != '[' {
		return "", false
	}
	rest := line[idx+3:]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return "", false
	}
	return rest[:end], true
}

// ExecContextForProfile captures the exec channel's true context on serverID
// iff serverID is in profileID (iron rule — same gate as exec_command). Every
// branch is audited with Action="exec-context". Statuses mirror exec_command:
// denied / error / auth_error / no_credential / hostkey_mismatch /
// connect_error / no_sudo (sudo=true but unconfigured) / cancelled / ok.
func ExecContextForProfile(ctx context.Context, st *store.Store, projectID, profileID, serverID string, sudo bool) (out ExecContextOutput, err error) {
	var status string
	start := time.Now()
	defer func() {
		if status == "" {
			status = "error"
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: serverID, Action: "exec-context",
			Command: fmt.Sprintf("exec_context(sudo=%v)", sudo), Sudo: sudo,
			Status: status, DurationMS: time.Since(start).Milliseconds(),
		})
	}()

	allowed, ferr := st.ServersForProfile(profileID)
	if ferr != nil {
		err = ferr
		return
	}
	if !contains(allowed, serverID) {
		status = "denied"
		err = ErrNotInProfile
		return
	}

	srv, serr := st.GetServer(serverID)
	if serr != nil || srv == nil {
		status = "error"
		err = fmt.Errorf("server %s not found", serverID)
		return
	}

	auth, aerr := vault.AuthForServer(st, srv)
	if aerr != nil {
		if errors.Is(aerr, vault.ErrNoCredential) {
			status = "no_credential"
			err = aerr
			return
		}
		status = "auth_error"
		err = aerr
		return
	}

	hkCb, herr := sshbroker.HostKeyTOFU(st, srv.Host, srv.Port)
	if herr != nil {
		status = "error"
		err = herr
		return
	}

	var sudoCred []byte
	if sudo {
		if srv.SudoCredentialID == "" {
			status = "no_sudo"
			err = fmt.Errorf("sudo not configured for server %s (call list_servers: has_sudo tells you)", srv.Name)
			return
		}
		cred, gerr := st.GetCredential(srv.SudoCredentialID)
		if gerr != nil || cred == nil {
			status = "no_sudo"
			err = fmt.Errorf("sudo credential for %s not found", srv.Name)
			return
		}
		sudoCred = cred.Secret
	}

	cli, cerr := sshbroker.Connect(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)
	if cerr != nil {
		switch {
		case errors.Is(cerr, context.Canceled):
			status = "cancelled"
		case errors.Is(cerr, sshbroker.ErrHostKeyMismatch):
			status = "hostkey_mismatch"
		default:
			status = "connect_error"
		}
		err = cerr
		return
	}
	defer cli.Close()

	nonce := newCtxNonce()
	timeout := 30 * time.Second // fixed: a context probe is tiny; runaway probes must not linger

	var res sshbroker.ExecResult
	if sudo {
		res, err = cli.ExecSudoWrapped(ctx, buildCtxCommand(nonce, true), sudoCred, timeout, MaxOutputBytes)
	} else {
		res, err = cli.Exec(ctx, buildCtxCommand(nonce, false), timeout, MaxOutputBytes)
	}
	switch {
	case res.TimedOut:
		status = "timeout"
		err = fmt.Errorf("exec_context timed out after %s", timeout)
		return
	case errors.Is(err, context.Canceled):
		status = "cancelled"
		return
	case err != nil:
		status = "error"
		return
	}
	if res.ExitCode != 0 {
		status = "error"
		err = fmt.Errorf("exec_context probe failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		return
	}
	status = "ok"
	out = parseCtxOutput(res.Stdout, nonce, sudo)
	return
}
