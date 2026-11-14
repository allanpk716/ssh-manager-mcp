package webuishares

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// TestShareScopePreviewListsImpactByShareName 覆盖共享存储详情页进入的可
// 挂载范围预览：保留机密项与服务器清单，同时按共享名逐行列出每处共享
// 各自新增或失去的服务器；确认后仍按共享名展示。
func TestShareScopePreviewListsImpactByShareName(t *testing.T) {
	st := newIntegrationStore(t)
	linuxID := mustServer(t, st, "linux-web", models.ShellPOSIX)
	dbID := mustServer(t, st, "db-1", models.ShellPOSIX)
	if _, err := st.AddDevice("NAS", "", ""); err != nil {
		t.Fatal(err)
	}
	mustSecret(t, st, "DATASET_PASSWORD")
	if err := st.AllowSecrets(
		[]string{"DATASET_PASSWORD"}, []string{"linux-web"}, store.CLISource(),
	); err != nil {
		t.Fatal(err)
	}
	datasets, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "datasets", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASET_PASSWORD", Drive: "Z",
	}, store.CLISource())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddSharedStorage(store.SharedStorageInput{
		Name: "scratch", Device: "NAS", User: "mount-user",
		PasswordSecret: "DATASET_PASSWORD", Drive: "Y",
	}, store.CLISource()); err != nil {
		t.Fatal(err)
	}
	views, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(views) != 1 {
		t.Fatalf("准备挂载密码机密项失败：%v %+v", err, views)
	}
	secretID := views[0].ID
	h := newIntegrationHandler(t, st)

	form := url.Values{
		"csrf_token":         {csrfToken(t, h)},
		"return_to":          {"/ui/shares/" + datasets.ID},
		"authorization_view": {"share-scope"},
		"share_id":           {datasets.ID},
		"secret_id":          {secretID},
		"server_id":          {linuxID, dbID},
	}
	form.Set("cell__"+secretID+"__"+linuxID, "0")
	form.Set("cell__"+secretID+"__"+dbID, "1")
	preview := h.do(http.MethodPost, "/ui/secrets/authorization/preview", form)
	page := body(t, preview)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("可挂载范围预览状态码 = %d：%s", preview.StatusCode, page)
	}
	for _, want := range []string{
		"新增 1 个授权，移除 1 个授权。",
		"按共享名列出的影响面",
		"同一个挂载密码机密项驱动多处共享存储时，下面逐个列出每处共享各自新增或失去的服务器条目。",
		`<tr><td>DATASET_PASSWORD</td><td>db-1 </td><td>linux-web </td></tr>`,
		`<tr><td>datasets</td><td>db-1 </td><td>linux-web </td></tr>`,
		`<tr><td>scratch</td><td>db-1 </td><td>linux-web </td></tr>`,
		`<input type="hidden" name="share_id" value="` + datasets.ID + `">`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("可挂载范围预览缺少 %q：\n%s", want, page)
		}
	}
	if strings.Contains(page, "DATASET_PASSWORD-value-不回显") {
		t.Fatalf("可挂载范围预览不得显示密码：%s", page)
	}

	summary := pageHiddenValue(t, page, "summary")
	confirmForm := url.Values{
		"csrf_token": {csrfToken(t, h)}, "summary": {summary},
		"return_to": {"/ui/shares/" + datasets.ID},
		"target": {
			secretID + " " + linuxID + " 0",
			secretID + " " + dbID + " 1",
		},
	}
	confirmForm.Set("authorization_view", "share-scope")
	confirmForm.Set("share_id", datasets.ID)
	confirmed := h.do(http.MethodPost, "/ui/secrets/authorization/confirm", confirmForm)
	confirmedPage := body(t, confirmed)
	if confirmed.StatusCode != http.StatusOK || !strings.Contains(confirmedPage, "已保存授权变更") {
		t.Fatalf("确认可挂载范围失败，状态码=%d：\n%s", confirmed.StatusCode, confirmedPage)
	}
	for _, want := range []string{
		"按共享名列出的影响面",
		`<tr><td>datasets</td><td>db-1 </td><td>linux-web </td></tr>`,
		`<tr><td>scratch</td><td>db-1 </td><td>linux-web </td></tr>`,
	} {
		if !strings.Contains(confirmedPage, want) {
			t.Fatalf("确认后的预览缺少 %q：\n%s", want, confirmedPage)
		}
	}
	after, err := st.ListSecretsInScope(store.ScopeAllServers())
	if err != nil || len(after) != 1 || strings.Join(after[0].AllowedServers, ",") != "db-1" {
		t.Fatalf("确认后的可挂载范围不正确：%v %+v", err, after)
	}
}
