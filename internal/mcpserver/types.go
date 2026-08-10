package mcpserver

import "time"

// ServerInfo is a Profile-scoped server as seen by the agent (no credentials).
type ServerInfo struct {
	ID      string `json:"id" jsonschema:"stable server id (use this in exec_command)"`
	Name    string `json:"name" jsonschema:"human-friendly server name"`
	Host    string `json:"host" jsonschema:"server host"`
	User    string `json:"user" jsonschema:"ssh user"`
	HasSudo bool   `json:"has_sudo" jsonschema:"true if sudo=true is supported on this server"`
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
// (= the agent's) filesystem; RemotePath is the destination on the server.
type UploadInput struct {
	ServerID   string `json:"server_id" jsonschema:"server id from list_servers"`
	LocalPath  string `json:"local_path" jsonschema:"absolute local path (on your machine) of the file or directory to push to the server; a directory is uploaded recursively, preserving relative paths"`
	RemotePath string `json:"remote_path" jsonschema:"absolute destination path on the server; its parent directory is created if it does not exist"`
}

// UploadOutput is the upload_file tool output.
type UploadOutput struct {
	Files     int   `json:"files" jsonschema:"number of files uploaded (>=1; >1 if local_path was a directory)"`
	Bytes     int64 `json:"bytes" jsonschema:"total bytes uploaded (may be less than the source size when truncated=true)"`
	Truncated bool  `json:"truncated,omitempty" jsonschema:"true if the 1 MiB total cap was hit mid-upload (only a partial tree landed — retry with a smaller payload)"`
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
// service at 127.0.0.1:local_port on ITS OWN machine (the broker host's
// loopback) — e.g. curl http://127.0.0.1:<local_port>. Pass tunnel_id to
// close_port when done (tunnels also auto-close after ~10 min idle).
type ForwardOutput struct {
	TunnelID  string `json:"tunnel_id" jsonschema:"opaque id; pass to close_port when done with the forward"`
	LocalPort int    `json:"local_port" jsonschema:"the local port now forwarding to remote_host:remote_port — reach it via 127.0.0.1:local_port on your machine"`
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
