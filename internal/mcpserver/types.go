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
	Stdout   string `json:"stdout" jsonschema:"combined/normal command stdout"`
	Stderr   string `json:"stderr,omitempty" jsonschema:"command stderr"`
	ExitCode int    `json:"exit_code" jsonschema:"process exit code (0 = success)"`
	TimedOut bool   `json:"timed_out,omitempty" jsonschema:"true if the command exceeded the timeout"`
}

// ErrNotInProfile is returned when an agent requests a server outside its Profile (iron rule).
var ErrNotInProfile = errWithString("server is not in your profile — call list_servers to see the servers you may use")

// errWithString is a sentinel error that also satisfies the string the agent should see.
type errWithString string

func (e errWithString) Error() string { return string(e) }

// defaultTimeout caps a single exec_command.
const defaultTimeout = 120 * time.Second
