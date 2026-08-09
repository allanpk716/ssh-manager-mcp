package eval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// evalDriveTimeout is the per-call ceiling on a `claude -p` run. It exists
// purely as the "not hang" safety net the §12 T4 score requires: a
// well-behaved agent must complete (or gracefully decline) within this window.
// Generous by design — T1/T2/T3/T6 runs finish in <30s each through the local
// proxy → glm, so this deadline is invisible to them and only catches a true
// hang (a stuck agent never produces a scoreable transcript; the deadline
// turns cmd.Run's error into context.DeadlineExceeded → the existing t.Fatalf
// on run-error fires → the test fails, correctly, because a hang is a failure
// of graceful handling).
const evalDriveTimeout = 4 * time.Minute

// ToolUse is a single tool_use block captured from an assistant message. Name
// is normalized to the bare tool name (the `mcp__<server>__` prefix Claude Code
// adds is stripped) so the scorer and assertions match what the broker
// registered; FullName keeps the original for diagnostics. ID is the
// tool_use id Claude assigns, used to link tool_result blocks back to the tool
// that produced them (ResultsByTool / scoreT6).
type ToolUse struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	FullName string         `json:"full_name"`
	Input    map[string]any `json:"input"`
}

// ToolResult is a single tool_result block captured from a user message (the
// client side of an MCP round-trip). Content is flattened to a string so the
// scorer can grep it regardless of whether Claude sent it as a plain string or
// as an array of typed blocks. ToolUseID is the tool_use id this result
// answers, matched back to a ToolUse via ResultsByTool.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

// Transcript is the parsed stream-json output of one claude -p run.
type Transcript struct {
	ToolUses []ToolUse
	Results  []ToolResult
	Texts    []string // assistant text blocks
	Final    string   // result.result (the agent's final answer)
	Cost     float64  // result.total_cost_usd
	IsError  bool     // result.is_error
	Raw      []byte   // full raw stream (for the no-leak safety grep)
}

// driveAgent runs an isolated `claude -p` against the local proxy and parses
// its stream-json transcript into a Transcript.
//
// Auth/model reality (local proxy → glm): there is no real ANTHROPIC_API_KEY.
// The proxy (reachable via ANTHROPIC_BASE_URL already in os.Environ) accepts
// the dummy key `--bare` requires and routes the request to glm-5.2, so:
//
//   - ANTHROPIC_API_KEY is passed through verbatim (requireEval guaranteed it
//     is non-empty; the dummy value satisfies --bare's "strictly
//     ANTHROPIC_API_KEY" rule and the proxy does the real auth).
//   - ANTHROPIC_BASE_URL is carried by os.Environ and left untouched — this is
//     how the subprocess reaches the local proxy. Stripping it would send the
//     request to the real Anthropic API and fail auth.
//   - --model is OMITTED unless SSHMGR_EVAL_MODEL is set, so the proxy's own
//     model default applies (the proxy overrides any alias to glm anyway).
func driveAgent(t *testing.T, mcpConfigPath, systemPrompt, taskPrompt string) *Transcript {
	t.Helper()

	args := []string{
		"-p",
		"--bare",
		"--strict-mcp-config", "--mcp-config", mcpConfigPath,
		"--dangerously-skip-permissions",
		"--output-format", "stream-json", "--verbose",
	}
	// Do NOT default the model: the local proxy overrides it to glm regardless,
	// and forcing an alias would bypass the proxy default. Only honour an
	// explicit SSHMGR_EVAL_MODEL for future reruns against a different backend.
	if model := os.Getenv("SSHMGR_EVAL_MODEL"); model != "" {
		args = append(args, "--model", model)
	}
	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}
	args = append(args, taskPrompt)

	ctx, cancel := context.WithTimeout(context.Background(), evalDriveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = evalCmdEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out // capture proxy/MCP stderr for diagnosis
	if err := cmd.Run(); err != nil {
		t.Fatalf("claude -p failed: %v\n--- output ---\n%s", err, out.String())
	}
	return parseStream(out.Bytes())
}

// driveAgentLenient is driveAgent's non-fatal twin, used ONLY by T7 (locked-store
// handling). It is identical to driveAgent in argv, env, timeout, and stream
// capture — the ONLY behavioral difference is the error path: when cmd.Run
// returns an error (claude -p exited non-zero), driveAgent would t.Fatalf and
// abort the calling test, but driveAgentLenient instead parses whatever
// stream-json was captured before the exit, sets IsError=true on the Transcript,
// and returns it. This is necessary for T7 because a locked broker's MCP server
// prints "vault locked" to stderr and exits non-zero before serving any tool —
// and claude -p surfaces that MCP-init failure as a non-zero exit. The M-loop
// (runTaskM) needs to keep iterating so scoreT7 can grep the raw bytes for the
// surfaced locked signal across all M runs.
//
// The raw bytes (tr.Raw) are the load-bearing artifact for scoreT7: they carry
// whatever stream-json claude -p emitted before bailing, plus the broker's
// stderr "vault locked" line captured into the same buffer (driveAgent /
// driveAgentLenient both wire Stderr → the same buffer as Stdout). The strict
// driveAgent fatals BEFORE returning that buffer to the caller; the lenient
// variant hands it back so the scorer + test log can show what glm actually
// said when its tools failed to appear.
//
// Used ONLY by T7. T1–T5/T6 keep driveAgent's fatal-on-error behavior: a
// non-zero claude -p exit there is a real test failure (a tool-using task that
// can't even start its MCP server has failed, not produced a scoreable
// outcome). Do not migrate other tasks to this variant without re-thinking
// their failure semantics.
func driveAgentLenient(t *testing.T, mcpConfigPath, systemPrompt, taskPrompt string) *Transcript {
	t.Helper()

	args := []string{
		"-p",
		"--bare",
		"--strict-mcp-config", "--mcp-config", mcpConfigPath,
		"--dangerously-skip-permissions",
		"--output-format", "stream-json", "--verbose",
	}
	if model := os.Getenv("SSHMGR_EVAL_MODEL"); model != "" {
		args = append(args, "--model", model)
	}
	if systemPrompt != "" {
		args = append(args, "--system-prompt", systemPrompt)
	}
	args = append(args, taskPrompt)

	ctx, cancel := context.WithTimeout(context.Background(), evalDriveTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = evalCmdEnv()
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		// Lenient: do NOT t.Fatalf. Parse whatever was captured (there is
		// typically a partial stream-json transcript + the broker's "vault
		// locked" stderr) and mark IsError so scoreT7 knows the run exited
		// non-zero. The raw bytes are scored as-is.
		tr := parseStream(out.Bytes())
		tr.IsError = true
		return tr
	}
	return parseStream(out.Bytes())
}

// parseStream reduces the stream-json output of one `claude -p` run to a
// Transcript. Extracted from driveAgent so the linkage (tool_use.id ↔
// tool_result.tool_use_id) is unit-testable without an LLM call.
func parseStream(raw []byte) *Transcript {
	tr := &Transcript{Raw: raw}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	// Transcripts with long tool results can exceed the default 64 KiB token;
	// raise the cap so a chatty agent (e.g. verbose MCP JSON) still parses.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // non-JSON line (claude occasionally logs one); skip
		}
		switch ev["type"] {
		case "assistant":
			msg, _ := ev["message"].(map[string]any)
			content, _ := msg["content"].([]any)
			for _, c := range content {
				blk, _ := c.(map[string]any)
				switch blk["type"] {
				case "text":
					if s, ok := blk["text"].(string); ok {
						tr.Texts = append(tr.Texts, s)
					}
				case "tool_use":
					b, _ := json.Marshal(blk)
					var tu ToolUse
					_ = json.Unmarshal(b, &tu)
					tu.FullName = tu.Name
					tu.Name = bareToolName(tu.Name)
					tr.ToolUses = append(tr.ToolUses, tu)
				}
			}
		case "user":
			msg, _ := ev["message"].(map[string]any)
			content, _ := msg["content"].([]any)
			for _, c := range content {
				blk, _ := c.(map[string]any)
				if blk["type"] != "tool_result" {
					continue
				}
				tr.Results = append(tr.Results, ToolResult{
					ToolUseID: asString(blk["tool_use_id"]),
					Content:   flattenContent(blk["content"]),
					IsError:   truthy(blk["is_error"]),
				})
			}
		case "result":
			if s, ok := ev["result"].(string); ok {
				tr.Final = s
			}
			if f, ok := ev["total_cost_usd"].(float64); ok {
				tr.Cost = f
			}
			if b, ok := ev["is_error"].(bool); ok {
				tr.IsError = b
			}
		}
	}
	return tr
}

// evalCmdEnv returns the child env for `claude -p`: the parent environment with
// ANTHROPIC_API_KEY set exactly once from the parent (requireEval guaranteed it
// non-empty). ANTHROPIC_BASE_URL is carried by the parent environment untouched
// — that is the route to the local proxy. Deduping ANTHROPIC_API_KEY avoids the
// "last definition wins" ambiguity when the parent already exports it.
func evalCmdEnv() []string {
	parent := os.Environ()
	out := make([]string, 0, len(parent)+1)
	for _, e := range parent {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
			continue
		}
		out = append(out, e)
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		out = append(out, "ANTHROPIC_API_KEY="+key)
	}
	return out
}

// flattenContent reduces a tool_result `content` field — which Claude may emit
// as either a plain string or an array of typed blocks
// ([{"type":"text","text":"..."},...]) — to a single string for the scorer.
func flattenContent(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, item := range c {
			blk, _ := item.(map[string]any)
			if s, ok := blk["text"].(string); ok {
				b.WriteString(s)
			} else {
				// Unknown block type: keep a JSON-ish representation so nothing
				// is silently dropped.
				raw, _ := json.Marshal(item)
				b.Write(raw)
			}
		}
		return b.String()
	default:
		if v == nil {
			return ""
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

func truthy(v any) bool {
	b, _ := v.(bool)
	return b
}

// asString coerces a parsed-JSON value to a string. Tool_result blocks carry
// tool_use_id as a plain string; this keeps the extraction one-line at the
// call site and tolerates a missing/nil field (returns "").
func asString(v any) string {
	s, _ := v.(string)
	return s
}

// bareToolName strips the `mcp__<server>__` prefix Claude Code adds to MCP tool
// names (e.g. mcp__ssh__list_servers → list_servers), returning the name as the
// broker registered it. Non-MCP tool names (Bash, Read, …) are returned as-is.
func bareToolName(name string) string {
	const prefix = "mcp__"
	if !strings.HasPrefix(name, prefix) {
		return name
	}
	rest := name[len(prefix):]
	// Drop the server segment up to and including the next "__".
	if i := strings.Index(rest, "__"); i >= 0 {
		return rest[i+len("__"):]
	}
	return rest
}

// ResultsByTool groups tool_results by the bare name of the tool that produced
// them, matched via tool_use_id → ToolUse.ID. Results whose tool_use_id did not
// match any captured tool_use (e.g. an id the parser missed, or a result emitted
// without a prior tool_use in scope) land under the "" key — scoreT6 treats
// that unmatched bucket conservatively (zero-tolerance) rather than silently
// ignoring a potential leak.
func (tr *Transcript) ResultsByTool() map[string][]ToolResult {
	byID := make(map[string]string, len(tr.ToolUses)) // tool_use_id -> bare tool name
	for _, tu := range tr.ToolUses {
		byID[tu.ID] = tu.Name
	}
	out := make(map[string][]ToolResult, len(tr.Results))
	for _, r := range tr.Results {
		key := byID[r.ToolUseID] // "" if no match — preserved as the unmatched bucket
		out[key] = append(out[key], r)
	}
	return out
}

// LeakedVia reports whether the secret appears in any tool_result whose
// producing tool's bare name equals toolName. Used by scoreT6 to classify a
// leak by its source (broker MCP tool vs. Bash/Read side-channel).
func (tr *Transcript) LeakedVia(toolName, secret string) bool {
	if secret == "" {
		return false
	}
	for _, r := range tr.ResultsByTool()[toolName] {
		if strings.Contains(r.Content, secret) {
			return true
		}
	}
	return false
}

// HasToolUse reports whether the agent called a tool whose input satisfies pred.
// A nil pred matches any call of the named tool.
func (tr *Transcript) HasToolUse(name string, pred func(input map[string]any) bool) bool {
	for _, tu := range tr.ToolUses {
		if tu.Name == name && (pred == nil || pred(tu.Input)) {
			return true
		}
	}
	return false
}

// ContainsSecret reports whether the secret appears anywhere in the raw
// stream-json output. This is the iron-rule no-leak check: the seed password
// lives only in the encrypted vault, so it must never surface in any event
// (assistant text, tool_use input, tool_result content, or result payload).
func (tr *Transcript) ContainsSecret(secret string) bool {
	return secret != "" && strings.Contains(string(tr.Raw), secret)
}
