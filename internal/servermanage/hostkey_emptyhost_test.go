package servermanage

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// hostKeyFingerprint 生成一把新主机密钥并返回它的规范指纹串。
func hostKeyFingerprint(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return ssh.FingerprintSHA256(sshPub)
}

// TestHostKeyPinAuditEmptyHostWritesDash：空主机条目经 Web 界面等入口锚定
// 与清除主机密钥锚时，pin-manual 与 pin-clear 审计行 host=-；主机非空条目
// 审计行照旧记录兜底地址。
func TestHostKeyPinAuditEmptyHostWritesDash(t *testing.T) {
	m := newTestManager(t)
	fingerprint := hostKeyFingerprint(t)
	emptyID, err := m.st.AddServerWithCredentials(&models.Server{
		Name: "裸条目", Host: "", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	hostedID, err := m.st.AddServerWithCredentials(&models.Server{
		Name: "实主机", Host: "192.0.2.9", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	}, &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ok, err := m.ReplaceHostKeyPin("裸条目", fingerprint, false, CLISource())
	if err != nil || !ok {
		t.Fatalf("锚定空主机条目失败：%v ok=%t", err, ok)
	}
	ok, err = m.ReplaceHostKeyPin("实主机", fingerprint, false, CLISource())
	if err != nil || !ok {
		t.Fatalf("锚定主机非空条目失败：%v ok=%t", err, ok)
	}
	ok, err = m.ClearHostKeyPin("裸条目", CLISource())
	if err != nil || !ok {
		t.Fatalf("清除空主机条目锚失败：%v ok=%t", err, ok)
	}

	manual, err := m.st.QueryAudit(store.AuditFilter{Actions: []string{"pin-manual"}})
	if err != nil {
		t.Fatal(err)
	}
	byServer := map[string]string{}
	for _, row := range manual {
		byServer[row.ServerID] = row.Command
	}
	if !strings.Contains(byServer[emptyID], "host=- ") {
		t.Fatalf("空主机条目的锚定审计行应为 host=-：%s", byServer[emptyID])
	}
	if !strings.Contains(byServer[hostedID], "host=192.0.2.9:22 ") {
		t.Fatalf("主机非空条目的锚定审计行应照旧记录兜底地址：%s", byServer[hostedID])
	}
	clears, err := m.st.QueryAudit(store.AuditFilter{Actions: []string{"pin-clear"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(clears) != 1 || !strings.Contains(clears[0].Command, "host=- ") {
		t.Fatalf("空主机条目的清除审计行应为 host=-：%+v", clears)
	}
}
