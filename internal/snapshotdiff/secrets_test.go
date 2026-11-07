package snapshotdiff

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

func TestDefaultHintSecretChanges(t *testing.T) {
	const (
		secretID  = "secret-1"
		serverID1 = "server-1"
		serverID2 = "server-2"
		oldValue  = "snapshot-diff-old-value"
		newValue  = "snapshot-diff-new-value"
	)

	baseSnapshot := func() *store.Snapshot {
		return &store.Snapshot{
			SnapshotRevision: 10,
			Servers: []store.SnapshotServer{
				{ID: serverID1, Name: "gpu"},
				{ID: serverID2, Name: "web"},
			},
		}
	}
	secret := func(version int64) store.SnapshotSecret {
		value := []byte(oldValue)
		if version > 1 {
			value = []byte(newValue)
		}
		return store.SnapshotSecret{
			ID: secretID, Name: "DB_PASSWORD", Value: value,
			PreviousValue: []byte(oldValue), ValueVersion: version,
		}
	}
	allows := func(serverIDs ...string) []store.SnapshotSecretAllow {
		out := make([]store.SnapshotSecretAllow, 0, len(serverIDs))
		for _, id := range serverIDs {
			out = append(out, store.SnapshotSecretAllow{SecretID: secretID, ServerID: id})
		}
		return out
	}

	tests := []struct {
		name  string
		old   func(*store.Snapshot)
		fresh func(*store.Snapshot)
		want  string
	}{
		{
			name: "新增机密项",
			old:  func(snap *store.Snapshot) {},
			fresh: func(snap *store.Snapshot) {
				snap.Secrets = []store.SnapshotSecret{secret(1)}
				snap.SecretAllows = allows(serverID1)
			},
			want: "权威端配置已更新（修订号 10 → 11）：新增机密项 DB_PASSWORD",
		},
		{
			name: "删除机密项",
			old: func(snap *store.Snapshot) {
				snap.Secrets = []store.SnapshotSecret{secret(1)}
				snap.SecretAllows = allows(serverID1)
			},
			fresh: func(snap *store.Snapshot) {},
			want:  "权威端配置已更新（修订号 10 → 11）：删除机密项 DB_PASSWORD，引用它的模板将被拒绝",
		},
		{
			name: "更换机密项值",
			old: func(snap *store.Snapshot) {
				snap.Secrets = []store.SnapshotSecret{secret(1)}
				snap.SecretAllows = allows(serverID1)
			},
			fresh: func(snap *store.Snapshot) {
				snap.Secrets = []store.SnapshotSecret{secret(2)}
				snap.SecretAllows = allows(serverID1)
			},
			want: "权威端配置已更新（修订号 10 → 11）：机密项 DB_PASSWORD 的值已更改，用到它的文件需要重新部署",
		},
		{
			name: "可写入服务器列表变化",
			old: func(snap *store.Snapshot) {
				snap.Secrets = []store.SnapshotSecret{secret(1)}
				snap.SecretAllows = allows(serverID2)
			},
			fresh: func(snap *store.Snapshot) {
				snap.Secrets = []store.SnapshotSecret{secret(1)}
				snap.SecretAllows = allows(serverID1)
			},
			want: "权威端配置已更新（修订号 10 → 11）：机密项 DB_PASSWORD 的可写入服务器列表已变化：新增 gpu、删除 web",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			old := baseSnapshot()
			test.old(old)
			fresh := baseSnapshot()
			fresh.SnapshotRevision = 11
			test.fresh(fresh)

			got := Default().Hint(old, fresh)
			if got != test.want {
				t.Fatalf("变更提示 = %q，应为 %q", got, test.want)
			}
			if strings.Contains(got, oldValue) || strings.Contains(got, newValue) {
				t.Fatalf("变更提示绝不应包含机密项值：%q", got)
			}
		})
	}
}

func TestDefaultHintUnrelatedSecretChangeProducesNoHint(t *testing.T) {
	old := &store.Snapshot{SnapshotRevision: 10}
	fresh := &store.Snapshot{SnapshotRevision: 11}
	if got := Default().Hint(old, fresh); got != "" {
		t.Fatalf("与本客户端无关的机密项变化不应产生提示，实际为 %q", got)
	}
}
