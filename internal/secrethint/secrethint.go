// Package secrethint detects high-confidence suspected-secret shapes in the
// free-text vault server metadata fields (description, tags, role, services,
// location, hardware, caveats). Those fields are sent verbatim to the LLM
// context on every list_servers call, so a pasted private key or API key in a
// note is actively exfiltrated by design; callers use these findings to print
// a warning before that happens.
//
// Detection is deliberately conservative: only the prefix shapes below and the
// tightened PEM rule. Entropy scoring is deliberately absent — the xcheck
// experiment measured a 2.9% false-positive rate for it on legal metadata.
package secrethint

import "strings"

// Finding is one suspected-secret hit in one metadata field.
type Finding struct {
	Field string // "description" / "tags" / …
	Rule  string // "pem-private-key" / "prefix:sk-" / …
}

// prefixes is the exact, closed prefix set from xcheck experiment 3: plain
// case-sensitive substring match, 0 false positives on the 35-line legal
// corpus (testdata/corpus-legal.txt). Do not add entries without re-running
// that corpus regression.
var prefixes = []string{
	"sk-",         // Anthropic-style API keys (sk-ant-api03-…)
	"ghp_",        // GitHub personal access token (classic)
	"gho_",        // GitHub OAuth token
	"github_pat_", // GitHub fine-grained PAT
	"AKIA",        // AWS access key ID (ASIA session keys not included: unmeasured)
	"xoxb-",       // Slack bot token
	"xoxp-",       // Slack user token
	"eyJ",         // JWT header (base64 {"a…)
}

// pemRuleName is the rule name for the tightened PEM detection.
const pemRuleName = "pem-private-key"

// ScanValue scans one metadata field value for suspected-secret shapes.
// It returns one Finding per matched rule (PEM first, then prefixes in table
// order), or nil when nothing matches.
func ScanValue(field, value string) []Finding {
	var findings []Finding
	// PEM rule (rev2 tightening): `-----BEGIN` alone would flag public
	// certificates too, so require BOTH markers co-occurring in the same
	// value. Case-sensitive — PEM headers are standardized uppercase.
	if strings.Contains(value, "-----BEGIN") && strings.Contains(value, "PRIVATE KEY") {
		findings = append(findings, Finding{Field: field, Rule: pemRuleName})
	}
	for _, p := range prefixes {
		if strings.Contains(value, p) {
			findings = append(findings, Finding{Field: field, Rule: "prefix:" + p})
		}
	}
	return findings
}

// ScanServer scans every free-text metadata field of one server entry and
// concatenates the findings in the fixed field order:
// tags, description, location, hardware, services, role, caveats.
func ScanServer(tags, description, location, hardware, services, role, caveats string) []Finding {
	fields := []struct{ field, value string }{
		{"tags", tags},
		{"description", description},
		{"location", location},
		{"hardware", hardware},
		{"services", services},
		{"role", role},
		{"caveats", caveats},
	}
	var findings []Finding
	for _, f := range fields {
		findings = append(findings, ScanValue(f.field, f.value)...)
	}
	return findings
}

// FormatWarning renders one Finding as a single-line warning for CLI/TUI
// output. It is templated off the Finding only and never includes field
// content. No trailing newline — callers add it.
func FormatWarning(f Finding) string {
	return "warning: server metadata may contain a secret — field '" + f.Field +
		"' matched rule '" + f.Rule +
		"' (content not shown; this text would be sent to LLM providers on every list_servers — edit the server to fix, or ignore if intentional)"
}
