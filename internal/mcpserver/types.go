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
