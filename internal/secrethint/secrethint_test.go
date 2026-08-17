package secrethint

import (
	"os"
	"strings"
	"testing"
)

// TestScanValueTruePositives pins at least one true positive per rule,
// including the multi-line PEM shapes and prefixes embedded in ordinary prose.
func TestScanValueTruePositives(t *testing.T) {
	cases := []struct {
		name      string
		value     string
		wantRule  string
		wantField string
	}{
		{
			name: "openssh pem multi-line",
			value: "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
				"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAABFwAAAAdzc2gtcn\n" +
				"NhAAAAAwEAAQAAAQEAtc0vLB3n6e5h8pxfGLDTYlkTQv8SRwTcWbwRIqEF9oIJiKcFxFg3\n" +
				"-----END OPENSSH PRIVATE KEY-----",
			wantRule:  "pem-private-key",
			wantField: "description",
		},
		{
			name:      "rsa pem",
			value:     "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA0wqdLZXa\n-----END RSA PRIVATE KEY-----",
			wantRule:  "pem-private-key",
			wantField: "caveats",
		},
		{
			name:      "anthropic api key prefix",
			value:     "sk-ant-api03-9Z4tG7qQ2mXbK8vN1sJ5rTyU0hP6wE3dFgL2xC7zAaB5cD6eF4gH8iJ0k",
			wantRule:  "prefix:sk-",
			wantField: "description",
		},
		{
			name:      "github classic token 40 chars",
			value:     "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ABCD",
			wantRule:  "prefix:ghp_",
			wantField: "tags",
		},
		{
			name:      "github oauth token",
			value:     "gho_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ABCD",
			wantRule:  "prefix:gho_",
			wantField: "tags",
		},
		{
			name:      "github fine-grained token",
			value:     "github_pat_11A2B3C4D5E6F7G8H9I0JKLMNOPQRSTU",
			wantRule:  "prefix:github_pat_",
			wantField: "description",
		},
		{
			name:      "aws access key id",
			value:     "AKIAIOSFODNN7EXAMPLE",
			wantRule:  "prefix:AKIA",
			wantField: "location",
		},
		{
			// Tails deliberately pattern-breaking (period segments, like the
			// jwt fixture below): GitHub push protection flags Slack-shaped
			// xox[bap]-… literals; the periods keep OUR prefix rule hit while
			// breaking the token pattern.
			name:      "slack bot token",
			value:     "xoxb-not.a.real.token.test.fixture.only.0000000000",
			wantRule:  "prefix:xoxb-",
			wantField: "services",
		},
		{
			name:      "slack user token",
			value:     "xoxp-not.a.real.token.test.fixture.only.0000000000",
			wantRule:  "prefix:xoxp-",
			wantField: "services",
		},
		{
			name:      "jwt header",
			value:     "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.payload.signature",
			wantRule:  "prefix:eyJ",
			wantField: "role",
		},
		{
			name:      "prefix embedded in ordinary text",
			value:     "deploy note: CI bot uses ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ABCD for pulls",
			wantRule:  "prefix:ghp_",
			wantField: "description",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScanValue(tc.wantField, tc.value)
			found := false
			for _, f := range got {
				if f.Rule == tc.wantRule && f.Field == tc.wantField {
					found = true
				}
			}
			if !found {
				t.Errorf("ScanValue(%q, …) = %v, want a Finding with rule %q and field %q",
					tc.wantField, got, tc.wantRule, tc.wantField)
			}
		})
	}
}

// TestScanValueLegalCorpus is the 0-FP regression: every non-empty line of the
// legal corpus must produce zero findings. The corpus must retain its sha256
// pin line (the one category that came closest to a false positive in the
// xcheck experiment).
func TestScanValueLegalCorpus(t *testing.T) {
	data, err := os.ReadFile("testdata/corpus-legal.txt")
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	lines := 0
	sawSha256Pin := false
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines++
		if strings.Contains(line, "sha256:") {
			sawSha256Pin = true
		}
		if got := ScanValue("description", line); len(got) != 0 {
			t.Errorf("false positive on corpus line %d (%q): %v", i+1, line, got)
		}
	}
	if lines < 30 {
		t.Errorf("corpus suspiciously small: %d non-empty lines, want ~35", lines)
	}
	if !sawSha256Pin {
		t.Error("corpus is missing its sha256 pin line")
	}
}

// TestScanValuePublicCertNotFlagged pins the tightened PEM rule: a public
// certificate (or public key, or prose that merely mentions "private key")
// must NOT be flagged — the rule requires the -----BEGIN marker AND the
// PRIVATE KEY label co-occurring in the same value.
func TestScanValuePublicCertNotFlagged(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{
			name:  "public certificate",
			value: "-----BEGIN CERTIFICATE-----\nMIIDdzCCAl+gAwIBAgIEbGz\n-----END CERTIFICATE-----",
		},
		{
			name:  "public key",
			value: "-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEF\n-----END PUBLIC KEY-----",
		},
		{
			name:  "prose mentioning private key without pem marker",
			value: "the private key file lives in the usual place, do not commit it",
		},
		{
			name:  "pem marker without private key label",
			value: "-----BEGIN TRUSTED CERTIFICATE-----\nMIIDdzCCAl+gAwIBAgIE\n-----END TRUSTED CERTIFICATE-----",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScanValue("caveats", tc.value); len(got) != 0 {
				t.Errorf("ScanValue(%q, %q) = %v, want no findings", "caveats", tc.value, got)
			}
		})
	}
}

// TestFormatWarningNoContentEcho pins that the warning line is templated off
// the Finding alone and never echoes scanned field content; it also pins the
// exact one-line shape (no trailing newline — callers add it).
func TestFormatWarningNoContentEcho(t *testing.T) {
	sentinel := "sk-ant-api03-SENTINELdoNotEcho9Z4tG7qQ2mXbK8vN"
	f := Finding{Field: "description", Rule: "pem-private-key"}
	out := FormatWarning(f)
	if strings.Contains(out, sentinel) {
		t.Errorf("FormatWarning echoed scanned content: %q", out)
	}
	want := "warning: server metadata may contain a secret — field 'description' matched rule 'pem-private-key' (content not shown; this text would be sent to LLM providers on every list_servers — edit the server to fix, or ignore if intentional)"
	if out != want {
		t.Errorf("FormatWarning() =\n%q\nwant\n%q", out, want)
	}
	if strings.HasSuffix(out, "\n") {
		t.Error("FormatWarning() must not end with a newline; callers add it")
	}
}

// TestScanServerAggregates pins the fixed scan order
// (tags, description, location, hardware, services, role, caveats) and that
// each hit lands as its own Finding with the right field name.
func TestScanServerAggregates(t *testing.T) {
	tags := "ci ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ABCD"
	caveats := "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA0wqdLZXa\n-----END RSA PRIVATE KEY-----"
	got := ScanServer(tags, "", "", "", "", "", caveats)
	want := []Finding{
		{Field: "tags", Rule: "prefix:ghp_"},
		{Field: "caveats", Rule: "pem-private-key"},
	}
	if len(got) != len(want) {
		t.Fatalf("ScanServer() = %v, want %d findings", got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ScanServer()[%d] = %+v, want %+v (fixed order tags→…→caveats)", i, got[i], want[i])
		}
	}
	if all := ScanServer("", "", "", "", "", "", ""); len(all) != 0 {
		t.Errorf("ScanServer(all empty) = %v, want no findings", all)
	}
}
