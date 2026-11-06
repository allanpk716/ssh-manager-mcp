package mcpserver

import (
	"testing"

	"ssh-manager-mcp/internal/secretmask"
)

func TestMaskingTraversalMasksBytesAndMergesExistingNames(t *testing.T) {
	const secretValue = "binary-secret-value"
	call := maskingCall{masker: secretmask.New([]secretmask.Secret{{
		Name:    "BINARY_TOKEN",
		Current: []byte(secretValue),
	}})}
	output := struct {
		MaskedOutput
		Raw []byte `json:"raw"`
	}{MaskedOutput: MaskedOutput{Masked: []string{"EARLIER_NAME"}}}
	output.Raw = []byte("prefix " + secretValue + " suffix")

	names, err := maskToolResultAndOutput(nil, &output, call, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "{{sshmgr-secret:BINARY_TOKEN}}"
	if string(output.Raw) != "prefix "+want+" suffix" {
		t.Fatalf("raw bytes = %q", output.Raw)
	}
	if len(names) != 2 || names[0] != "BINARY_TOKEN" || names[1] != "EARLIER_NAME" {
		t.Fatalf("names = %#v", names)
	}
	if len(output.Masked) != 2 || output.Masked[0] != "BINARY_TOKEN" || output.Masked[1] != "EARLIER_NAME" {
		t.Fatalf("masked = %#v", output.Masked)
	}
}
