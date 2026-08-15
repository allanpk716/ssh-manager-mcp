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
