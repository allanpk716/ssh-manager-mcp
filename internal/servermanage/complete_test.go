package servermanage

import (
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestCompleteIdempotentAndConditionalCredentials 校验逐台补全：重复提交同
// 一张表单不加修订号；元数据相同不写；条件凭据按服务器状态出现；设置密码
// 后注意原因消失；补私钥口令校验并影响共用凭据的多台服务器。
func TestCompleteIdempotentAndConditionalCredentials(t *testing.T) {
	m := newTestManager(t)
	bareID := mustAddServer(t, m, &models.Server{Name: "bare", Host: "192.0.2.1", Port: 22, User: "u"})
	keyPEM := genEncryptedKey(t, "secret-pass")
	credID, err := m.st.SetCredential(&models.Credential{Type: models.CredPrivateKey, Secret: keyPEM})
	if err != nil {
		t.Fatal(err)
	}
	encID := mustAddServer(t, m, &models.Server{
		Name: "enc", Host: "192.0.2.2", Port: 22, User: "u",
		CredentialID: credID, Tags: []string{models.TagNeedsPassphrase},
	})
	mustAddServer(t, m, &models.Server{
		Name: "enc2", Host: "192.0.2.3", Port: 22, User: "u",
		CredentialID: credID, Tags: []string{models.TagNeedsPassphrase},
	})
	before := serverRevision(t, m, bareID)

	// 同值重复提交：不加修订号、不写审计。
	role := ""
	for i := 0; i < 2; i++ {
		result, err := m.Complete(CompleteRequest{Identifier: "bare", Role: &role}, CLISource())
		if err != nil {
			t.Fatal(err)
		}
		if !result.NoOp {
			t.Fatalf("同值提交应为无变化：%+v", result)
		}
	}
	if after := serverRevision(t, m, bareID); after != before {
		t.Fatalf("同值提交不得加修订号：%d -> %d", before, after)
	}

	// 条件凭据：无凭据机器设置密码 + 服务器元数据 + sudo 密码。
	location := "机房 A"
	filledRole := "应用服务器"
	result, err := m.Complete(CompleteRequest{
		Identifier: "bare", Role: &filledRole, Location: &location,
		Password: "login-pass-7f2a", SudoPassword: "sudo-pass-91bc",
	}, WebUISource("192.0.2.9"))
	if err != nil {
		t.Fatal(err)
	}
	if result.NoOp || result.Server.CredentialID == "" || result.Server.SudoCredentialID == "" {
		t.Fatalf("条件凭据应写入：%+v", result)
	}
	if got := models.ServerAttentionReasons(result.Server.CredentialID, result.Server.Role, result.Server.Tags); len(got) != 0 {
		t.Fatalf("设置密码并填角色后不应再有注意原因：%v", got)
	}
	if result.Server.Location != "机房 A" {
		t.Fatalf("服务器元数据应写入：%+v", result.Server)
	}

	// 同一张表单重发：密码与 sudo 留空表示不变；结果幂等。
	repeatRole := filledRole
	repeatLocation := location
	repeat, err := m.Complete(CompleteRequest{
		Identifier: "bare", Role: &repeatRole, Location: &repeatLocation,
	}, WebUISource("192.0.2.9"))
	if err != nil {
		t.Fatal(err)
	}
	if !repeat.NoOp {
		t.Fatalf("重复提交应无变化：%+v", repeat)
	}

	// 条件凭据：带待补口令标签的机器补私钥口令，共用凭据的机器一起去标签。
	encRole := "计算节点"
	passResult, err := m.Complete(CompleteRequest{
		Identifier: "enc", Role: &encRole, KeyPassphrase: "secret-pass",
	}, CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if len(passResult.AffectedServers) != 2 {
		t.Fatalf("共用凭据的服务器应一起去掉标签：%v", passResult.AffectedServers)
	}
	for _, name := range []string{"enc", "enc2"} {
		srv, err := m.st.GetServerByName(name)
		if err != nil || srv == nil {
			t.Fatal(err)
		}
		if models.ServerHasTag(srv.Tags, models.TagNeedsPassphrase) {
			t.Fatalf("%s 的待补标签应移除", name)
		}
	}

	// 错误口令：被拒且不写。
	badRole := "又一次"
	if _, err := m.Complete(CompleteRequest{
		Identifier: "enc", Role: &badRole, KeyPassphrase: "wrong",
	}, CLISource()); err == nil {
		t.Fatal("错误私钥口令应被拒绝")
	}
	enc2, _ := m.st.GetServerByName("enc2")
	if enc2.Role != "" {
		t.Fatalf("被拒绝的提交不得写入：%+v", enc2)
	}

	// 审计：每次有效保存都记录来源与字段名，不含值。
	rows := auditActions(t, m)
	okRows := 0
	for _, row := range rows {
		if row.Action == "server.edit" && row.Status == "ok" {
			okRows++
			if row.Source == "" {
				t.Fatalf("补全审计应带来源：%+v", row)
			}
			if strings.Contains(row.Command, "login-pass-7f2a") || strings.Contains(row.Command, "sudo-pass-91bc") || strings.Contains(row.Command, "secret-pass") {
				t.Fatalf("审计摘要不得包含凭据值：%s", row.Command)
			}
		}
	}
	if okRows < 3 {
		t.Fatalf("有效保存都应写审计，实际 %d 行：%+v", okRows, rows)
	}
	if _, _, found, _ := m.st.CredentialSummary(credID); !found {
		t.Fatal("补口令不得删除凭据")
	}
	var _ = encID
	var _ = store.AuditSourceCLI
}
