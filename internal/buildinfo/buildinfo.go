// Package buildinfo is the single source of the release version, injectable
// via -ldflags -X. Both the CLI version command and the MCP serverInfo read
// it (cli→mcpserver import direction forbids mcpserver reading cli).
package buildinfo

// Version is the build version. Defaults to "dev" for local `go build` /
// `go install`; overridden at release time via ldflags:
//
//	go build -ldflags "-X ssh-manager-mcp/internal/buildinfo.Version=<version>"
//
// GoReleaser sets this to the tag-derived semver (tag v1.0.0 -> "1.0.0").
var Version = "dev"

// Owner is the GitHub owner of the release repository — used by the
// self-update flow to build the releases API URL (spec §4.1:
// repos/allanpk716/ssh-manager-mcp/releases/...).
const Owner = "allanpk716"

// Repo is the GitHub repository name, mirroring the repo (which is also the
// Go module path — the module path itself is NOT renamed by Plan 44).
const Repo = "ssh-manager-mcp"
