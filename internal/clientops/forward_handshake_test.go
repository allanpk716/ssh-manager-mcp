package clientops

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestPinForwarder_Forward_FailureTextUsesHandshakeAddr：空主机条目锚定转发
// 失败时，提示里写本次真正握手的地址，不出现「:22」这样的空主机假地址；
// 主机非空条目的提示照旧以握手地址为准。
func TestPinForwarder_Forward_FailureTextUsesHandshakeAddr(t *testing.T) {
	blob, _ := newHostKey(t)
	unreachable := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer unreachable.Close()

	emptyHost := &models.Server{ID: "srv-bare", Name: "bare", Host: "", Port: 22, User: "u", AuthMethod: models.AuthPassword}
	hosted := fwdEntry("198.51.100.7", 22)

	err := newTestForwarder(t, unreachable).Forward(emptyHost, "192.0.2.5:22", blob)
	if err == nil {
		t.Fatal("5xx must fail closed")
	}
	if !strings.Contains(err.Error(), "host key for 192.0.2.5:22 is unknown") {
		t.Fatalf("失败提示应写真实握手地址 192.0.2.5:22：\n%v", err)
	}
	if strings.Contains(err.Error(), "for :22") || strings.Contains(err.Error(), "for @:") {
		t.Fatalf("失败提示不得出现空主机假地址：\n%v", err)
	}

	err = newTestForwarder(t, unreachable).Forward(hosted, store.EntryFallbackAddr(hosted), blob)
	if err == nil || !strings.Contains(err.Error(), "host key for 198.51.100.7:22 is unknown") {
		t.Fatalf("主机非空条目的失败提示应照旧含握手地址：%v", err)
	}
}

// TestPinForwarder_Forward_EmptyHandshakeAddrFallsBackToEntry：握手地址缺
// 失时提示退回条目兜底地址，不因空值产生残缺文案。
func TestPinForwarder_Forward_EmptyHandshakeAddrFallsBackToEntry(t *testing.T) {
	blob, _ := newHostKey(t)
	unreachable := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer unreachable.Close()
	hosted := fwdEntry("198.51.100.7", 22)

	err := newTestForwarder(t, unreachable).Forward(hosted, "", blob)
	if err == nil || !strings.Contains(err.Error(), "host key for 198.51.100.7:22 is unknown") {
		t.Fatalf("握手地址缺失时提示应退回条目兜底地址：%v", err)
	}
}
