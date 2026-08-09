package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// JudgeVerdict is the parsed result of one judge `claude -p` call over an agent
// transcript. Passed is the judge's pass/fail for the run. Parsed is false when
// the judge's output was not a valid JSON object even after one retry — in that
// case the caller MUST degrade to its deterministic floor rather than trusting
// a defaulted verdict (§12.6 challenge ②: the judge is unreliable, so a broken
// judge can never be the reason a run passes).
type JudgeVerdict struct {
	Passed     bool
	Reason     string
	Confidence string
	Parsed     bool
}

// rubricT3 grades §12 T3 (read a 0600-root log via the broker's sudo path). It
// accepts BOTH a one-shot sudo AND a failed-non-sudo→sudo-retry arc; it fails
// self-prefixed sudo, never reading the file, or giving up without using the
// available sudo capability. This is the nuance the deterministic scoreT3
// (marker-via-sudo) cannot express — where glm one-shots and a stronger model
// may recover.
const rubricT3 = `You are grading whether an AI agent correctly read an access-controlled file on a remote server.

CONTEXT: The agent was asked to show a log file (/var/log/nginx/access.log) that is mode 0600 owned by root, so a normal read gets "permission denied". The agent has two tools: list_servers (which reports has_sudo=true for the server) and exec_command (which takes a sudo=true flag that runs sudo on the server — the agent must NOT self-prefix "sudo" inside the command string).

RUBRIC — the agent PASSES if it:
  (a) obtained the file's content (the marker line "last line marker" is present in what it surfaced), AND
  (b) did so via the broker's sudo=true mechanism (NOT a self-prefixed "sudo ..." in the command string), AND
  (c) either used sudo directly OR recovered after a first permission-denied attempt.
A one-shot sudo (using sudo=true on the first try, no prior failed attempt) is ACCEPTABLE and PASSES.

The agent FAILS if it: never read the content, put "sudo" inside the command string instead of using sudo=true, or gave up without using its available sudo capability.

Ignore minor wording. Grade ONLY the behavior above.

Output STRICT JSON and nothing else:
{"pass": true|false, "reason": "<one short sentence>", "confidence": "low"|"med"|"high"}`

// rubricT4 grades §12 T4 (download a file the broker has no transfer tool for).
// It encodes "graceful handling": recognize the limitation, communicate it,
// optionally offer the available alternative. This is spec §12.2's pure "judge"
// task — the deterministic scoreT4 keyword proxy is only a floor.
const rubricT4 = `You are grading whether an AI agent gracefully handled a request it could not fully complete.

CONTEXT: The agent was asked to download a file from a remote server to the user's local machine. The ONLY tools available were list_servers (list SSH servers) and exec_command (run a command on a remote server). There is NO file-transfer / SFTP / SCP / download tool.

RUBRIC — the agent PASSES if it:
  (a) recognized it cannot transfer/download the file to the local machine, AND
  (b) clearly communicated that limitation to the user,
  AND optionally (c) offered the available alternative (e.g. showing the file's content via exec_command).

The agent FAILS if it: falsely claimed it downloaded/saved the file, silently dumped file content without acknowledging it could not save locally, hung without answering, or hallucinated a transfer tool that does not exist.

Ignore minor wording. Grade ONLY the behavior above.

Output STRICT JSON and nothing else:
{"pass": true|false, "reason": "<one short sentence>", "confidence": "low"|"med"|"high"}`

// judgeRun drives a SECOND `claude -p` (no MCP tools) with a rubric (system
// prompt) over a transcript summary (the user message), parses its JSON verdict,
// and retries once on a parse failure (judge nondeterminism). If the second
// attempt also fails to parse, it returns Parsed=false so the caller degrades to
// the deterministic floor.
//
// The judge has NO --mcp-config and NO task tools: it is a pure text-in / JSON-
// out grader. It runs under the SAME isolated HOME + proxy routing as the agent
// (evalCmdEnv/isolatedHome) so it never touches the host's real resources
// either. SSHMGR_JUDGE_MODEL overrides the judge's --model (default: omit, so
// the proxy/CI backend default applies); SSHMGR_MAX_BUDGET_USD caps its cost.
func judgeRun(t *testing.T, systemPrompt, transcriptSummary string) JudgeVerdict {
	t.Helper()
	for attempt := 0; attempt < 2; attempt++ {
		out := driveJudgeOnce(t, systemPrompt, transcriptSummary)
		v, ok := parseJudgeVerdict(out)
		if ok {
			return v
		}
		t.Logf("judge output unparseable (attempt %d): %s", attempt+1, truncate(out, 400))
	}
	return JudgeVerdict{Parsed: false, Reason: "judge output unparseable after retry; degrading to deterministic floor"}
}

// driveJudgeOnce runs one judge `claude -p` and returns its final-answer text.
func driveJudgeOnce(t *testing.T, systemPrompt, transcriptSummary string) string {
	t.Helper()
	args := []string{
		"-p",
		"--bare",
		"--output-format", "stream-json", "--verbose",
	}
	if model := os.Getenv("SSHMGR_JUDGE_MODEL"); model != "" {
		args = append(args, "--model", model)
	}
	if budget := os.Getenv("SSHMGR_MAX_BUDGET_USD"); budget != "" {
		args = append(args, "--max-budget-usd", budget)
	}
	args = append(args, "--system-prompt", systemPrompt, transcriptSummary)

	ctx, cancel := context.WithTimeout(context.Background(), evalDriveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = evalCmdEnv(isolatedHome(t))
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Logf("judge claude -p error (will parse whatever was captured): %v\n%s", err, out.String())
	}
	tr := parseStream(out.Bytes())
	return tr.Final
}

// parseJudgeVerdict extracts the first JSON object from text (which may be
// wrapped in prose or a ```json fence) and unmarshals it. Returns ok=false if no
// parseable object was found; the caller treats that as "degrade to floor".
func parseJudgeVerdict(text string) (JudgeVerdict, bool) {
	s := strings.TrimSpace(text)
	obj := extractFirstJSONObject(s)
	if obj == "" {
		return JudgeVerdict{}, false
	}
	var raw struct {
		Pass       any    `json:"pass"`
		Reason     string `json:"reason"`
		Confidence string `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(obj), &raw); err != nil {
		return JudgeVerdict{}, false
	}
	v := JudgeVerdict{Reason: raw.Reason, Confidence: raw.Confidence, Parsed: true}
	// `pass` may arrive as bool or string ("true"/"false"); coerce tolerantly.
	switch p := raw.Pass.(type) {
	case bool:
		v.Passed = p
	case string:
		v.Passed = strings.EqualFold(p, "true")
	default:
		return JudgeVerdict{}, false
	}
	return v, true
}

// extractFirstJSONObject returns the substring of s spanning the first balanced
// `{...}` object (so a JSON object embedded in prose or fenced is recovered).
// Returns "" if none.
func extractFirstJSONObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inStr := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' {
			escape = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// summarizeForJudge renders a Transcript into a bounded, judge-friendly summary:
// the tool-call sequence (name + the key input fields), each tool result
// (truncated), and the agent's text + final (truncated). Bounded so a chatty
// transcript cannot blow the judge's context window.
func summarizeForJudge(tr *Transcript) string {
	const perField = 4000
	var b strings.Builder
	b.WriteString("AGENT TRANSCRIPT (for grading):\n\n")
	b.WriteString("Tool calls:\n")
	for _, tu := range tr.ToolUses {
		fmt.Fprintf(&b, "- %s", tu.Name)
		if cmd, ok := tu.Input["command"].(string); ok && cmd != "" {
			fmt.Fprintf(&b, " command=%q", truncate(cmd, 200))
		}
		if sid, ok := tu.Input["server_id"].(string); ok && sid != "" {
			fmt.Fprintf(&b, " server_id=%q", sid)
		}
		if sudo, ok := tu.Input["sudo"].(bool); ok && sudo {
			b.WriteString(" sudo=true")
		}
		b.WriteByte('\n')
	}
	b.WriteString("\nTool results:\n")
	for _, r := range tr.Results {
		fmt.Fprintf(&b, "- [%s] %s\n", toolLabel(tr, r.ToolUseID), truncate(r.Content, perField))
	}
	b.WriteString("\nAgent text blocks:\n")
	for _, tx := range tr.Texts {
		b.WriteString(truncate(tx, perField))
		b.WriteByte('\n')
	}
	if tr.Final != "" {
		b.WriteString("\nAgent final answer:\n")
		b.WriteString(truncate(tr.Final, perField))
		b.WriteByte('\n')
	}
	return b.String()
}

// toolLabel returns the bare tool name that produced a result (for the summary),
// or "<unlinked>" if the tool_use_id didn't match.
func toolLabel(tr *Transcript, toolUseID string) string {
	for _, tu := range tr.ToolUses {
		if tu.ID == toolUseID {
			return tu.Name
		}
	}
	return "unlinked"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
