package snapshotdiff

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/store"
)

const (
	sharedStorageMountUser    = "nas-mount-user"
	sharedStorageAddress      = "192.0.2.10"
	sharedStoragePassword     = "shared-storage-hint-password"
	sharedStorageSecretID     = "secret-share"
	sharedStoragePasswordName = "NAS_MOUNT_PASSWORD"
)

// sharedStorageHintSnapshot 构造一份带两个可见服务器、提供方设备、网络
// 成员与挂载密码机密项可写入关联的快照，供共享存储比较项测试直接改造。
func sharedStorageHintSnapshot(revision int64, allowedServers ...string) *store.Snapshot {
	snap := &store.Snapshot{
		SnapshotRevision: revision,
		Servers: []store.SnapshotServer{
			{ID: "server-gpu", Name: "gpu"},
			{ID: "server-web", Name: "web"},
		},
		Devices: []store.SnapshotDevice{{
			ID: "device-nas", Name: "NAS", CreatedAt: 1000, UpdatedAt: 1001,
		}},
		NetworkMemberships: []store.SnapshotNetworkMembership{{
			DeviceID: "device-nas", NetworkID: "network-office", Address: sharedStorageAddress,
			CreatedAt: 1000, UpdatedAt: 1001,
		}},
		Secrets: []store.SnapshotSecret{{
			ID: sharedStorageSecretID, Name: sharedStoragePasswordName,
			Value: []byte(sharedStoragePassword), ValueVersion: 3,
		}},
	}
	for _, serverID := range allowedServers {
		snap.SecretAllows = append(snap.SecretAllows, store.SnapshotSecretAllow{
			SecretID: sharedStorageSecretID, ServerID: serverID,
		})
	}
	return snap
}

// sharedStorageHintShare 只写比较需要的字段；挂载账号用户名与机密项值留在
// 测试快照里，用来断言它们绝不进入提示。
func sharedStorageHintShare(version int64, changeNote string) store.SnapshotSharedStorage {
	return store.SnapshotSharedStorage{
		ID: "share-datasets", Name: "datasets", DeviceID: "device-nas",
		MountUser: sharedStorageMountUser, PasswordSecretID: sharedStorageSecretID,
		Drive: "Z", Version: version, ChangeNote: changeNote,
	}
}

func assertNoSharedStorageLeak(t *testing.T, hint string) {
	t.Helper()
	for _, leak := range []string{sharedStorageMountUser, sharedStorageAddress, sharedStoragePassword, sharedStorageSecretID} {
		if strings.Contains(hint, leak) {
			t.Fatalf("共享存储变更提示不应包含挂载账号用户名、地址或密码：%q 含 %q", hint, leak)
		}
	}
}

func TestDefaultHintSharedStorageAdded(t *testing.T) {
	old := sharedStorageHintSnapshot(10, "server-gpu")
	fresh := sharedStorageHintSnapshot(11, "server-gpu")
	fresh.SharedStorages = []store.SnapshotSharedStorage{sharedStorageHintShare(3, "")}

	got := Default().Hint(old, fresh)
	want := "权威端配置已更新（修订号 10 → 11）：新增共享存储 datasets（版本 3）"
	if got != want {
		t.Fatalf("变更提示 = %q，应为 %q", got, want)
	}
	assertNoSharedStorageLeak(t, got)
}

func TestDefaultHintSharedStorageComesAfterExistingItems(t *testing.T) {
	old := sharedStorageHintSnapshot(10, "server-gpu")
	fresh := sharedStorageHintSnapshot(11, "server-gpu")
	fresh.Servers = append(fresh.Servers, store.SnapshotServer{ID: "server-build", Name: "build"})
	fresh.SharedStorages = []store.SnapshotSharedStorage{sharedStorageHintShare(1, "")}

	got := Default().Hint(old, fresh)
	want := "权威端配置已更新（修订号 10 → 11）：新增服务器 build；新增共享存储 datasets（版本 1）"
	if got != want {
		t.Fatalf("共享存储比较项应排在已有比较项之后，实际 = %q，应为 %q", got, want)
	}
}

func TestDefaultHintSharedStorageRemoved(t *testing.T) {
	old := sharedStorageHintSnapshot(10, "server-gpu")
	old.SharedStorages = []store.SnapshotSharedStorage{sharedStorageHintShare(3, "旧说明")}
	fresh := sharedStorageHintSnapshot(11, "server-gpu")

	got := Default().Hint(old, fresh)
	want := "权威端配置已更新（修订号 10 → 11）：共享存储 datasets 已删除或不再可见"
	if got != want {
		t.Fatalf("变更提示 = %q，应为 %q", got, want)
	}
	assertNoSharedStorageLeak(t, got)
}

func TestDefaultHintSharedStorageVersionChanged(t *testing.T) {
	tests := []struct {
		name         string
		oldServers   []string
		freshServers []string
		freshVersion int64
		changeNote   string
		wantSentence string
	}{
		{
			name:         "版本变化附说明与服务器并集",
			oldServers:   []string{"server-gpu"},
			freshServers: []string{"server-gpu", "server-web"},
			freshVersion: 5,
			changeNote:   "更换挂载密码",
			wantSentence: "机密项 NAS_MOUNT_PASSWORD 的可写入服务器列表已变化：新增 web；共享存储 datasets 版本 2 → 5，说明：更换挂载密码；以下服务器上的挂载可能需要更新：gpu、web",
		},
		{
			name:         "可挂载范围缩小也提醒旧服务器",
			oldServers:   []string{"server-gpu", "server-web"},
			freshServers: []string{"server-gpu"},
			freshVersion: 3,
			changeNote:   "缩小可挂载范围",
			wantSentence: "机密项 NAS_MOUNT_PASSWORD 的可写入服务器列表已变化：删除 web；共享存储 datasets 版本 2 → 3，说明：缩小可挂载范围；以下服务器上的挂载可能需要更新：gpu、web",
		},
		{
			name:         "可写入关联指向不可见服务器时不列出",
			oldServers:   []string{"server-gpu", "server-hidden"},
			freshServers: []string{"server-gpu", "server-hidden"},
			freshVersion: 3,
			changeNote:   "只改登记",
			wantSentence: "共享存储 datasets 版本 2 → 3，说明：只改登记；以下服务器上的挂载可能需要更新：gpu",
		},
		{
			name:         "没有变更说明时省略说明子句",
			oldServers:   []string{"server-gpu"},
			freshServers: []string{"server-gpu"},
			freshVersion: 3,
			changeNote:   "",
			wantSentence: "共享存储 datasets 版本 2 → 3；以下服务器上的挂载可能需要更新：gpu",
		},
		{
			name:         "两份快照都没有可确定的服务器时省略服务器子句",
			oldServers:   nil,
			freshServers: nil,
			freshVersion: 3,
			changeNote:   "联动更新",
			wantSentence: "共享存储 datasets 版本 2 → 3，说明：联动更新",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			old := sharedStorageHintSnapshot(10, test.oldServers...)
			old.SharedStorages = []store.SnapshotSharedStorage{sharedStorageHintShare(2, "旧说明")}
			fresh := sharedStorageHintSnapshot(11, test.freshServers...)
			fresh.SharedStorages = []store.SnapshotSharedStorage{sharedStorageHintShare(test.freshVersion, test.changeNote)}

			got := Default().Hint(old, fresh)
			want := "权威端配置已更新（修订号 10 → 11）：" + test.wantSentence
			if got != want {
				t.Fatalf("变更提示 = %q，应为 %q", got, want)
			}
			assertNoSharedStorageLeak(t, got)
		})
	}
}

func TestDefaultHintSharedStorageUnrelatedChangeProducesNoHint(t *testing.T) {
	old := sharedStorageHintSnapshot(10, "server-gpu")
	oldShare := sharedStorageHintShare(3, "旧说明")
	oldShare.Note = "原备注"
	oldShare.UpdatedAt = 1001
	old.SharedStorages = []store.SnapshotSharedStorage{oldShare}

	fresh := sharedStorageHintSnapshot(11, "server-gpu")
	freshShare := sharedStorageHintShare(3, "旧说明")
	freshShare.Note = "新备注"
	freshShare.UpdatedAt = 2001
	fresh.SharedStorages = []store.SnapshotSharedStorage{freshShare}

	if got := Default().Hint(old, fresh); got != "" {
		t.Fatalf("共享存储版本号未变时不应产生提示，实际为 %q", got)
	}
}
