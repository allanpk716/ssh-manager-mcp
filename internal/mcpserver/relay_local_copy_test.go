package mcpserver

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
)

// TestRelayShareTargetConflictKeys 钉住共享级冲突键的构造：同一处共享存
// 储的子路径不区分大小写，本地拷贝登记目标与半成品两把键，流式回退额外
// 登记块清单与临时块清单。
func TestRelayShareTargetConflictKeys(t *testing.T) {
	share := &models.SharedStorage{ID: "share-1", Name: "datasets"}
	localKeys := relayShareTargetConflictKeys(share, "Reports/Shared-Target.bin", false)
	if len(localKeys) != 2 {
		t.Fatalf("本地拷贝冲突键 = %#v，期待目标与半成品两把", localKeys)
	}
	wantBase := "shared-storage\x00share-1\x00reports/shared-target.bin"
	for index, suffix := range []string{"", relayPartialSuffix} {
		if localKeys[index] != wantBase+suffix {
			t.Fatalf("本地拷贝冲突键 %d = %q，期待 %q", index, localKeys[index], wantBase+suffix)
		}
	}

	streamingKeys := relayShareTargetConflictKeys(share, "reports/shared-target.BIN", true)
	if len(streamingKeys) != 4 {
		t.Fatalf("流式回退冲突键 = %#v，期待四把", streamingKeys)
	}
	for index, suffix := range []string{
		"", relayPartialSuffix, relayManifestSuffix, relayManifestSuffix + ".tmp",
	} {
		if streamingKeys[index] != wantBase+suffix {
			t.Fatalf("流式回退冲突键 %d = %q，期待 %q", index, streamingKeys[index], wantBase+suffix)
		}
	}
	if strings.Contains(strings.Join(streamingKeys, "\n"), "share-1\x00Reports") {
		t.Fatalf("冲突键的子路径应统一小写：%#v", streamingKeys)
	}
}
