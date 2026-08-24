package mcpserver

import "time"

// ServerInfo is a Profile-scoped server as seen by the agent (no credentials).
// Structured metadata fields (Role/Services/Caveats/Location/Hardware) plus Tags and
// Description are surfaced so the agent grasps each server's full picture. Caveats is
// placed before Location/Hardware so it reads prominently; empty strings mean "none".
// Plan 31: Host="hidden" is the ONE deliberate exception to the
// "empty string means explicitly none" invariant — host is never empty, and
// the literal lets the agent distinguish "owner withheld it" from "absent".
// Pathological collision is acknowledged: a real host literally named
// "hidden" with expose_host=true is indistinguishable from masked, and the
// forward_port guard rejects it — owners should avoid the name
// (managing-servers.md); structural elimination is out of scope (spec §3).
type ServerInfo struct {
	ID          string   `json:"id" jsonschema:"stable server id (use this in exec_command)"`
	Name        string   `json:"name" jsonschema:"human-friendly server name"`
	Host        string   `json:"host" jsonschema:"server host; 'hidden' = owner has not exposed it (default) — address the server via its id"`
	User        string   `json:"user" jsonschema:"ssh user"`
	HasSudo     bool     `json:"has_sudo" jsonschema:"true if sudo=true is supported on this server"`
	Role        string   `json:"role" jsonschema:"this server's purpose/role (e.g. 'prod pg primary')"`
	Services    string   `json:"services" jsonschema:"what is deployed/running on this server"`
	Caveats     string   `json:"caveats" jsonschema:"operational gotchas & special handling rules — READ BEFORE acting on this server; empty means none"`
	Location    string   `json:"location" jsonschema:"where this server is deployed (datacenter/region/rack/tenant)"`
	Hardware    string   `json:"hardware" jsonschema:"hardware configuration (CPU/RAM/disk/GPU)"`
	Tags        []string `json:"tags" jsonschema:"free-form labels"`
	Description string   `json:"description" jsonschema:"owner's free-text notes (supplementary; prefer structured fields above)"`
}

// ExecOutput is the result of exec_command. EffectiveTimeoutSeconds (Plan 32
// T7, spec §6) is ALWAYS present — the timeout actually in effect after the
// clamp (input <= 0 → 120 default, anything over 300 → 300 cap): silent
// clamping became an echo.
type ExecOutput struct {
	Stdout      string `json:"stdout" jsonschema:"combined/normal command stdout"`
	Stderr      string `json:"stderr,omitempty" jsonschema:"command stderr"`
	ExitCode    int    `json:"exit_code" jsonschema:"process exit code (0 = success)"`
	TimedOut    bool   `json:"timed_out,omitempty" jsonschema:"true if the command exceeded the timeout"`
	Truncated   bool   `json:"truncated,omitempty" jsonschema:"true if stdout or stderr exceeded the 1 MiB cap — you only received the PREFIX. Check stdout_bytes/stderr_bytes for the true size; if you need more, refine and re-run (e.g. tail -n, head -n, grep) instead of asking for the whole thing"`
	StdoutBytes int64  `json:"stdout_bytes" jsonschema:"total stdout bytes produced by the command (may exceed len(stdout) when truncated)"`
	StderrBytes int64  `json:"stderr_bytes" jsonschema:"total stderr bytes produced by the command (may exceed len(stderr) when truncated)"`
	// EffectiveTimeoutSeconds 恒存在 (no omitempty): clamp 后实际生效秒数。
	EffectiveTimeoutSeconds int `json:"effective_timeout_seconds" jsonschema:"the timeout actually in effect, in seconds: timeout_seconds <= 0 means the 120s default and anything over 300 is capped at 300 — echoed instead of silently clamped; anything longer belongs in exec_background"`
}

// DownloadInput is the download_file tool input.
type DownloadInput struct {
	ServerID string `json:"server_id" jsonschema:"server id from list_servers"`
	Path     string `json:"path" jsonschema:"absolute path of the remote file to download"`
}

// DownloadOutput is the download_file tool output.
type DownloadOutput struct {
	Content   string `json:"content" jsonschema:"the file content (the prefix if truncated=true)"`
	Bytes     int64  `json:"bytes" jsonschema:"total file size in bytes (may exceed len(content) when truncated)"`
	Truncated bool   `json:"truncated,omitempty" jsonschema:"true if the file exceeded the size cap and content is only the prefix"`
}

// UploadInput is the upload_file tool input. LocalPath is read from the broker's
// filesystem; RemotePath is the destination on the server.
type UploadInput struct {
	ServerID   string `json:"server_id" jsonschema:"server id from list_servers"`
	LocalPath  string `json:"local_path" jsonschema:"absolute local path (on the machine the broker runs on) of the file or directory to push to the server; a directory is uploaded recursively, preserving relative paths"`
	RemotePath string `json:"remote_path" jsonschema:"absolute destination path on the server; its parent directory is created if it does not exist"`
}

// UploadOutput is the upload_file tool output.
type UploadOutput struct {
	Files     int   `json:"files" jsonschema:"number of files uploaded (>=1; >1 if local_path was a directory)"`
	Bytes     int64 `json:"bytes" jsonschema:"total bytes uploaded (may be less than the source size when truncated=true)"`
	Truncated bool  `json:"truncated,omitempty" jsonschema:"true if the cumulative 1 MiB cap was crossed mid-upload by files each within the single-file cap — already-completed files are kept, the file in flight lands complete, and later files are not uploaded (a single file larger than the cap is refused before transfer with an error instead, zero bytes sent)"`
}

// UploadContentInput is the upload_content tool input (Plan 33; the cross-
// machine counterpart of UploadInput — content is INLINE, not a broker-local
// path). Encoding is validated in UploadContentForProfile (enum via handler,
// Plan 32 precedent). base64 must be SINGLE-LINE padded standard base64.
type UploadContentInput struct {
	ServerID   string `json:"server_id" jsonschema:"server id from list_servers"`
	Content    string `json:"content" jsonschema:"the file content to write (valid UTF-8 text; invalid UTF-8 bytes are replaced with U+FFFD — pass base64 here with encoding=base64 for exact bytes)"`
	RemotePath string `json:"remote_path" jsonschema:"absolute destination path on the server (must start with / or a Windows drive root X:/); its parent directory is created if missing; an existing file is overwritten"`
	Encoding   string `json:"encoding,omitempty" jsonschema:"how content is encoded: 'text' (default — the JSON-decoded string, written as UTF-8; NOT byte-exact: invalid sequences are already replaced with U+FFFD by JSON decoding) or 'base64' (decode first — exact bytes; SINGLE-LINE standard base64 with padding — CR/LF inside content is rejected). The cap applies to the DECODED byte count"`
}

// UploadContentOutput is the upload_content tool output. No truncated field:
// over-cap is a refusal ERROR before transfer, never a partial success.
type UploadContentOutput struct {
	Bytes int64 `json:"bytes" jsonschema:"bytes written to the remote file (the decoded byte count)"`
}

// ForwardInput is the forward_port tool input. forward_port opens a local TCP
// listener that forwards each connection to remote_host:remote_port over an SSH
// connection to server_id (the `ssh -L` semantic). It is the first STATEFUL
// broker operation — the broker holds the long-lived SSH connection + listener
// in its TunnelManager and returns a tunnel_id the agent passes to close_port.
type ForwardInput struct {
	ServerID   string `json:"server_id" jsonschema:"server id from list_servers (the SSH endpoint to forward through)"`
	RemoteHost string `json:"remote_host" jsonschema:"the host TO forward to, FROM THE SERVER'S PERSPECTIVE (usually '127.0.0.1' to reach a service on the server's own loopback)"`
	RemotePort int    `json:"remote_port" jsonschema:"the port on remote_host to reach"`
	LocalPort  int    `json:"local_port,omitempty" jsonschema:"optional local listen port (omit / 0 = let the broker pick a free port)"`
}

// ForwardOutput is the forward_port tool output. The agent reaches the remote
// service at 127.0.0.1:local_port on the machine the broker runs on (stdio:
// the agent's own machine; remote serve: the serve host) — e.g. curl
// http://127.0.0.1:<local_port> or point your client at it, from that host.
// Pass tunnel_id to close_port when done (tunnels auto-close ~10 min after
// creation — creation-based, not activity-based).
type ForwardOutput struct {
	TunnelID  string `json:"tunnel_id" jsonschema:"opaque id; pass to close_port when done with the forward"`
	LocalPort int    `json:"local_port" jsonschema:"the local port now forwarding to remote_host:remote_port — reach it via 127.0.0.1:local_port on the machine the broker runs on"`
}

// CloseForwardInput is the close_port tool input. The tunnel_id is the opaque
// value forward_port returned; close_port tears the tunnel down (listener + SSH
// connection). No profile re-gate: the id is unguessable + close_port only
// closes tunnels this TunnelManager owns.
type CloseForwardInput struct {
	TunnelID string `json:"tunnel_id" jsonschema:"the tunnel_id forward_port returned"`
}

// ExecBackgroundInput is the exec_background tool input (Plan 32 T6; the
// foreground analog is ExecCommandInput). No env/workdir/stdin parameters —
// the agent composes the command line itself (`cd /dir && VAR=x cmd`).
type ExecBackgroundInput struct {
	ServerID       string `json:"server_id" jsonschema:"server id from list_servers"`
	Command        string `json:"command" jsonschema:"shell command to run in the background on the server"`
	Sudo           bool   `json:"sudo,omitempty" jsonschema:"true to run with sudo (broker handles sudo -S; do not prepend sudo). Requires has_sudo=true."`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"optional max seconds; defaults to 86400 (24h) and is capped there; the effective value is echoed back as effective_timeout_seconds"`
}

// BgStartOutput is the exec_background tool output (Plan 32 T6; T7's
// BgReadOutput/BgStopOutput are the same naming family).
type BgStartOutput struct {
	TaskID                  string `json:"task_id" jsonschema:"opaque task id; poll its output with exec_output, stop it with exec_stop"`
	EffectiveTimeoutSeconds int    `json:"effective_timeout_seconds" jsonschema:"the clamped timeout actually in effect, in seconds (default and cap: 86400 = 24h)"`
	Status                  string `json:"status" jsonschema:"task status right after start; running"`
}

// ExecOutputInput is the exec_output tool input (Plan 32 T7). The numeric
// params (wait_seconds / stdout_offset / stderr_offset) cannot express
// minimum:0 via the SDK's reflection-generated jsonschema and encoding cannot
// express an enum — ExecOutputForProfile validates them at handler level and
// rejects negatives / unknown encodings with explicit errors (T6 handoff).
type ExecOutputInput struct {
	TaskID       string `json:"task_id" jsonschema:"the task_id exec_background returned"`
	WaitSeconds  int    `json:"wait_seconds,omitempty" jsonschema:"optional long-poll budget in seconds: the call returns once either stream has new bytes past your offsets, the task leaves running, or the budget expires (0/omit = return the current snapshot immediately; capped at 60; keep it under ~30 to stay below your own client timeout)"`
	StdoutOffset int64  `json:"stdout_offset,omitempty" jsonschema:"absolute byte offset within the task's stdout stream to read from (0/omit = stream start); pass back the next_stdout_offset you received to continue"`
	StderrOffset int64  `json:"stderr_offset,omitempty" jsonschema:"absolute byte offset within the task's stderr stream to read from (0/omit = stream start); pass back the next_stderr_offset you received to continue"`
	Encoding     string `json:"encoding,omitempty" jsonschema:"how the stdout/stderr chunks are encoded: 'text' (default — raw bytes as UTF-8, invalid sequences become U+FFFD, a multi-byte character may be split at a read boundary) or 'base64' (exact bytes — decode it yourself; use it for binary or non-UTF-8 e.g. GBK output). Offsets are byte-based in both modes"`
}

// BgReadOutput is the exec_output tool output (Plan 32 T7): the increment
// after each offset plus running/terminal status and honest-degradation
// bookkeeping. House style: constant fields, empty values explicit.
type BgReadOutput struct {
	Status           string `json:"status" jsonschema:"task status: running|done|stopped|timeout|failed"`
	ExitCode         int    `json:"exit_code" jsonschema:"process exit code (meaningful when status=done; 0 otherwise)"`
	Error            string `json:"error" jsonschema:"error text when status=failed (address shapes cleaned); empty otherwise"`
	Stdout           string `json:"stdout" jsonschema:"stdout bytes after stdout_offset, encoded per encoding (empty = nothing new)"`
	Stderr           string `json:"stderr" jsonschema:"stderr bytes after stderr_offset, encoded per encoding (empty = nothing new)"`
	NextStdoutOffset int64  `json:"next_stdout_offset" jsonschema:"the stdout byte offset right after this chunk — pass it as stdout_offset on your next call"`
	NextStderrOffset int64  `json:"next_stderr_offset" jsonschema:"the stderr byte offset right after this chunk — pass it as stderr_offset on your next call"`
	StdoutBytesTotal int64  `json:"stdout_bytes_total" jsonschema:"total stdout bytes the task has produced so far (the whole stream, not just the retained window)"`
	StderrBytesTotal int64  `json:"stderr_bytes_total" jsonschema:"total stderr bytes the task has produced so far (the whole stream, not just the retained window)"`
	Truncated        bool   `json:"truncated" jsonschema:"true if an offset fell behind the retained 1 MiB tail window — the skipped bytes are gone; read lost_stdout_bytes/lost_stderr_bytes and continue from next_*_offset instead of retrying the old offset"`
	LostStdoutBytes  int64  `json:"lost_stdout_bytes" jsonschema:"stdout bytes skipped because stdout_offset fell behind the retained window (0 unless truncated=true)"`
	LostStderrBytes  int64  `json:"lost_stderr_bytes" jsonschema:"stderr bytes skipped because stderr_offset fell behind the retained window (0 unless truncated=true)"`
}

// ExecStopInput is the exec_stop tool input (Plan 32 T7).
type ExecStopInput struct {
	TaskID string `json:"task_id" jsonschema:"the task_id exec_background returned"`
}

// BgStopOutput is the exec_stop tool output (Plan 32 T7).
type BgStopOutput struct {
	Status string `json:"status" jsonschema:"the task's status at trigger time: 'running' for a running task (the stop was set in motion; watch for the terminal 'stopped' state via exec_output — this call never blocks), or the task's terminal status if it had already finished (idempotent)"`
}

// ErrNotInProfile is returned when an agent requests a server outside its Profile (iron rule).
var ErrNotInProfile = errWithString("server is not in your profile — call list_servers to see the servers you may use")

// errWithString is a sentinel error that also satisfies the string the agent should see.
type errWithString string

func (e errWithString) Error() string { return string(e) }

// defaultTimeout caps a single exec_command.
const defaultTimeout = 120 * time.Second

// MaxOutputBytes caps how much of each output channel exec_command retains and
// returns to the agent (the prefix). Bytes beyond this are counted (reported as
// stdout_bytes/stderr_bytes with truncated=true) then discarded, so a huge remote
// output cannot blow up broker memory or flood the agent's context — the agent
// learns the true size and can refine its command. The owner `ssh-manager ssh`
// path is unaffected (it requests unlimited output). 1 MiB per spec §6.
const MaxOutputBytes int64 = 1 << 20

// MaxExecTimeout is the server-side ceiling on a single exec_command's run time
// (the time analog of MaxOutputBytes). An agent-supplied timeout over this cap is
// silently clamped down to it — defense-in-depth against a runaway / instructed
// agent tying up the broker with a very long command. defaultTimeout (120s) sits
// below this cap, so normal commands are unaffected.
const MaxExecTimeout = 5 * time.Minute
