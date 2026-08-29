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

// ServeServiceName is the registered name of the serve service on every
// platform (Windows service / systemd unit / launchd plist). Plan 44 hoisted
// it here from internal/cli so the self-update probe (updater.ProbeService)
// checks the same name the cli installs under — buildinfo is a leaf package,
// so updater → buildinfo introduces no import cycle. The pre-rename name
// ("ssh-manager-serve") is deliberately NOT a constant here: it exists only
// as the updater's legacy probe target (internal/updater legacyServiceName).
const ServeServiceName = "sshmgr-serve"
