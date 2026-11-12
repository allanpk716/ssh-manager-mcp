package clientops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
)

func TestSecretDeliveryForwarderRejectsUnexpectedCertificate(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	other.Close()
	forwarder, err := NewSecretDeliveryForwarder(CacheCred{
		URL: target.URL, Token: "device-code",
		Pin: mcpserver.SPKIFingerprint(other.Certificate()),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()

	result, err := forwarder.RecordSecretDelivery(context.Background(), store.SecretDeliveryRecordInput{})
	if err == nil || result.Status != SecretDeliveryUnreachable || !IsSecretDeliveryUnreachable(err) {
		t.Fatalf("证书不匹配必须按权威端不可达分支失败：result=%+v err=%v", result, err)
	}
}

func TestSecretDeliveryForwarderDoesNotFollowRedirects(t *testing.T) {
	requestCount := 0
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.URL.Path == "/secret-delivery-records" {
			http.Redirect(w, r, "/secret-delivery-records/redirected", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(secretDeliveryResponse{Replaced: false})
	}))
	defer target.Close()
	forwarder, err := NewSecretDeliveryForwarder(CacheCred{
		URL: target.URL, Token: "device-code",
		Pin: mcpserver.SPKIFingerprint(target.Certificate()),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer forwarder.Close()

	result, err := forwarder.RecordSecretDelivery(context.Background(), store.SecretDeliveryRecordInput{})
	if err == nil || result.Status != SecretDeliveryUnreachable {
		t.Fatalf("重定向不得被跟随：result=%+v err=%v", result, err)
	}
	if requestCount != 1 {
		t.Fatalf("重定向后的请求数 = %d，应为 1", requestCount)
	}
}

func TestSecretDeliveryForwarderFailsClosedWithoutFixedCertificate(t *testing.T) {
	forwarder, err := NewSecretDeliveryForwarder(CacheCred{
		URL: "http://127.0.0.1:1", Token: "device-code",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := forwarder.RecordSecretDelivery(context.Background(), store.SecretDeliveryRecordInput{})
	if err == nil || result.Status != SecretDeliveryRefused || result.Message != secretDeliveryPlaintextText {
		t.Fatalf("明文拒绝结果 = %+v，错误 = %v", result, err)
	}

	noCredential, err := NewSecretDeliveryForwarder(CacheCred{})
	if err != nil {
		t.Fatal(err)
	}
	result, err = noCredential.RecordSecretDelivery(context.Background(), store.SecretDeliveryRecordInput{})
	if err == nil || result.Status != SecretDeliveryUnreachable ||
		result.Message != SecretDeliveryUnreachableText {
		t.Fatalf("缺少凭据应按不可达失败：result=%+v err=%v", result, err)
	}
}
