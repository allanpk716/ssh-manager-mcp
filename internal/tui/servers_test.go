package tui

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

func TestServersPage_RowsAndDetail(t *testing.T) {
	sp := &serversPage{items: []*models.Server{{
		Name: "gpu", Host: "192.0.2.10", User: "u", Port: 22,
		Hardware: "2x3090", Tags: []string{"gpu"},
	}}}
	if rows := sp.Rows(); len(rows) != 1 || rows[0] != "gpu" {
		t.Fatalf("rows: %v", rows)
	}
	d := sp.Detail()
	for _, want := range []string{"gpu", "192.0.2.10", "2x3090", "gpu"} {
		if !strings.Contains(d, want) {
			t.Fatalf("detail missing %q:\n%s", want, d)
		}
	}
}
