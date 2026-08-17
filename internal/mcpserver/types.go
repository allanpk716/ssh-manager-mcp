package mcpserver

import "time"

// ServerInfo is a Profile-scoped server as seen by the agent (no credentials).
// Structured metadata fields (Role/Services/Caveats/Location/Hardware) plus Tags and
// Description are surfaced so the agent grasps each server's full picture. Caveats is
// placed before Location/Hardware so it reads prominently; empty strings mean "none".
type ServerInfo struct {
	ID          string   `json:"id" jsonschema:"stable server id (use this in exec_command)"`
	Name        string   `json:"name" jsonschema:"human-friendly server name"`
	Host        string   `json:"host" jsonschema:"server host"`
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

// ExecOutput is the result of exec_command.
type ExecOutput struct {
	Stdout      string `json:"stdout" jsonschema:"combined/normal command stdout"`
	Stderr      string `json:"stderr,omitempty" jsonschema:"command stderr"`
	ExitCode    int    `json:"exit_code" jsonschema:"process exit code (0 = success)"`
	TimedOut    bool   `json:"timed_out,omitempty" jsonschema:"true if the command exceeded the timeout"`
	Truncated   bool   `json:"truncated,omitempty" jsonschema:"true if stdout or stderr exceeded the 1 MiB cap — you only received the PREFIX. Check stdout_bytes/stderr_bytes for the true size; if you need more, refine and re-run (e.g. tail -n, head -n, grep) instead of asking for the whole thing"`
	StdoutBytes int64  `json:"stdout_bytes" jsonschema:"total stdout bytes produced by the command (may exceed len(stdout) when truncated)"`
	StderrBytes int64  `json:"stderr_bytes" jsonschema:"total stderr bytes produced by the command (may exceed len(stderr) when truncated)"`
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
