package cli

// Plan 51 T8 (spec §9): the mcp --cache wiring — cacheMetaForwarderFor builds
// the metadata forwarder at the same --instance resolver as the pin forwarder
// (one CacheCred read), and a missing credential yields the no-capability
// flavor whose Edit fails closed with the §2.1 main message.

import (
	"context"
	"strings"
	"testing"

	clientops "ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/store"
)

func TestCacheMetaForwarderFor_MissingCredentialFailsClosed(t *testing.T) {
	// No cache.auth.json for a nonexistent instance: the no-capability
	// flavor, NOT an error and NOT a silent local-write capability.
	f, err := cacheMetaForwarderFor("no-such-instance-plan51")
	if err != nil {
		t.Fatalf("missing credential must construct the no-capability flavor, got %v", err)
	}
	if f == nil {
		t.Fatal("nil forwarder — the tool face requires a non-nil editor")
	}
	_, err = f.EditMeta("srv-1", 0, map[string]*string{"role": ptrStr("x")})
	if err == nil || !strings.Contains(err.Error(), "could not reach the broker") {
		t.Fatalf("no-cred edit must fail closed with the main message, got %v", err)
	}
	if !strings.Contains(err.Error(), "NOT applied anywhere") {
		t.Fatalf("the main message must state the nothing-applied guarantee: %v", err)
	}
}

func TestCacheSecretDeliveryForwarderFor_MissingCredentialFailsClosed(t *testing.T) {
	forwarder, err := cacheSecretDeliveryForwarderFor("no-such-instance-delivery-record")
	if err != nil {
		t.Fatalf("缺少凭据必须构造固定报错的转发器，实际错误：%v", err)
	}
	if forwarder == nil {
		t.Fatal("转发器不能为空")
	}
	result, err := forwarder.RecordSecretDelivery(context.Background(), store.SecretDeliveryRecordInput{
		ServerID: "server-1", Path: "/etc/app/config.yaml",
		Template: []byte("token: {{sshmgr-secret:API_TOKEN}}"),
		Secrets:  []store.SecretDeliverySecret{{Name: "API_TOKEN", ValueVersion: 1}},
	})
	if err == nil || result.Status != clientops.SecretDeliveryUnreachable {
		t.Fatalf("缺少凭据的登记结果 = %+v error=%v，应固定按权威端不可达报告", result, err)
	}
	if !strings.Contains(err.Error(), "没有登记，也没有保存本地状态") {
		t.Fatalf("缺少凭据的错误文本应说明未登记且无本地状态：%v", err)
	}
}

func ptrStr(v string) *string { return &v }
