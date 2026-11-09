package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretEnvironmentNameValidationAndUniqueness 覆盖环境变量名的格式、
// 拒绝清单与全库大小写不敏感唯一约束。所有报错都要说明命中的规则。
func TestSecretEnvironmentNameValidationAndUniqueness(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddSecretWithEnvironmentName("FIRST_TOKEN", "", []byte("first-secret-value"), "HF_TOKEN"); err != nil {
		t.Fatalf("add secret with environment name: %v", err)
	}
	if err := s.AddSecret("SECOND_TOKEN", "", []byte("second-secret-value")); err != nil {
		t.Fatalf("add secret: %v", err)
	}

	for _, name := range []string{"", "9TOKEN", "TOKEN-NAME", "TOKEN NAME", "口令"} {
		err := s.SetSecretEnvironmentName("SECOND_TOKEN", name)
		if name == "" {
			if err != nil {
				t.Fatalf("clearing environment name must be accepted, got %v", err)
			}
			continue
		}
		if !errors.Is(err, ErrSecretEnvironmentNameInvalid) || !strings.Contains(err.Error(), "格式") {
			t.Fatalf("invalid environment name %q must report the format rule, got %v", name, err)
		}
	}

	reservedExact := []string{
		"PATH", "HOME", "USER", "LOGNAME", "SHELL", "PWD", "OLDPWD", "IFS", "ENV", "BASH_ENV",
		"CDPATH", "PS1", "PS2", "PS3", "PS4", "PROMPT_COMMAND", "TERM", "LANG", "TZ", "TMPDIR",
		"MAIL", "HOSTNAME", "PATHEXT", "COMSPEC", "SYSTEMROOT", "SYSTEMDRIVE", "WINDIR",
		"USERPROFILE", "USERNAME", "USERDOMAIN", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
		"PROGRAMFILES", "TEMP", "TMP", "HOMEDRIVE", "HOMEPATH", "PSMODULEPATH", "PROMPT",
		"PROCESSOR_ARCHITECTURE",
	}
	for _, name := range reservedExact {
		err := s.SetSecretEnvironmentName("SECOND_TOKEN", strings.ToLower(name))
		if !errors.Is(err, ErrSecretEnvironmentNameReserved) || !strings.Contains(err.Error(), "拒绝清单") {
			t.Fatalf("exact reserved environment name %q must report the deny list, got %v", name, err)
		}
	}
	for _, prefix := range []string{"LD_", "DYLD_", "SSH_", "SSHMGR_", "SUDO_", "BASH_FUNC_", "LC_"} {
		name := strings.ToLower(prefix) + "TOKEN"
		err := s.SetSecretEnvironmentName("SECOND_TOKEN", name)
		if !errors.Is(err, ErrSecretEnvironmentNameReserved) || !strings.Contains(err.Error(), "拒绝前缀") {
			t.Fatalf("environment name %q must report its reserved prefix, got %v", name, err)
		}
	}

	err := s.SetSecretEnvironmentName("SECOND_TOKEN", "hf_token")
	if !errors.Is(err, ErrSecretEnvironmentNameExists) ||
		!strings.Contains(err.Error(), "FIRST_TOKEN") ||
		!strings.Contains(err.Error(), "大小写") {
		t.Fatalf("case-insensitive duplicate environment name must identify the existing secret, got %v", err)
	}
	err = s.AddSecretWithEnvironmentName("THIRD_TOKEN", "", []byte("third-secret-value"), "Hf_Token")
	if !errors.Is(err, ErrSecretEnvironmentNameExists) || !strings.Contains(err.Error(), "FIRST_TOKEN") {
		t.Fatalf("duplicate environment name must also reject add, got %v", err)
	}
}

// TestSecretEnvironmentNameAudit 覆盖设置、修改与清除环境变量名的审计行。
// 审计只含机密项名与变量名前后值，绝不含机密值。
func TestSecretEnvironmentNameAudit(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddSecretWithEnvironmentName("DEPLOY_TOKEN", "", []byte("deploy-secret-value"), "HF_TOKEN"); err != nil {
		t.Fatalf("set environment name while adding: %v", err)
	}
	if err := s.SetSecretEnvironmentName("DEPLOY_TOKEN", "MODEL_TOKEN"); err != nil {
		t.Fatalf("change environment name: %v", err)
	}
	if err := s.SetSecretEnvironmentName("DEPLOY_TOKEN", ""); err != nil {
		t.Fatalf("clear environment name: %v", err)
	}

	views, err := s.ListSecretsInScope(ScopeAllServers())
	if err != nil {
		t.Fatalf("list secrets: %v", err)
	}
	if len(views) != 1 || views[0].EnvironmentName != "" {
		t.Fatalf("cleared environment name must disappear from the public view, got %+v", views)
	}

	rows, err := s.QueryAudit(AuditFilter{OwnerOnly: true, Actions: []string{"secret-env"}})
	if err != nil {
		t.Fatalf("query environment-name audit: %v", err)
	}
	want := []map[string]any{
		{"name": "DEPLOY_TOKEN", "before": "MODEL_TOKEN", "after": ""},
		{"name": "DEPLOY_TOKEN", "before": "HF_TOKEN", "after": "MODEL_TOKEN"},
		{"name": "DEPLOY_TOKEN", "before": "", "after": "HF_TOKEN"},
	}
	if len(rows) != len(want) {
		t.Fatalf("environment-name audit rows = %d, want %d: %+v", len(rows), len(want), rows)
	}
	for i, expected := range want {
		if rows[i].Action != "secret-env" || rows[i].Status != "ok" {
			t.Fatalf("audit row %d = %+v, want a successful secret-env row", i, rows[i])
		}
		var summary map[string]any
		if err := json.Unmarshal([]byte(rows[i].Command), &summary); err != nil {
			t.Fatalf("audit row %d is not valid JSON: %v", i, err)
		}
		for key, value := range expected {
			if summary[key] != value {
				t.Fatalf("audit row %d summary = %v, want %s=%v", i, summary, key, value)
			}
		}
		if strings.Contains(rows[i].Command, "deploy-secret-value") {
			t.Fatalf("environment-name audit leaked the secret value: %+v", rows[i])
		}
	}
}

// TestSecretEnvironmentNameSurvivesSnapshots 覆盖全量导出、按 profile 裁剪、
// 快照读取与导入：环境变量名随机密项完整流转，未声明版本的旧客户端仍收
// 不到机密项段。
func TestSecretEnvironmentNameSurvivesSnapshots(t *testing.T) {
	s := newTestStore(t)
	profileA := seedScopedVault(t, s)
	if err := s.AddSecretWithEnvironmentName("API_TOKEN", "接口令牌", []byte("snapshot-secret-value"), "HF_TOKEN"); err != nil {
		t.Fatalf("add secret with environment name: %v", err)
	}
	if err := s.AllowSecrets([]string{"API_TOKEN"}, []string{"srvA"}); err != nil {
		t.Fatalf("allow secret: %v", err)
	}

	declared, err := s.ExportSnapshotForProfile(profileA, true, "")
	if err != nil {
		t.Fatalf("export profile snapshot: %v", err)
	}
	if declared.Version != 1 {
		t.Fatalf("snapshot envelope version = %d, want 1", declared.Version)
	}
	if len(declared.Secrets) != 1 || declared.Secrets[0].EnvironmentName != "HF_TOKEN" {
		t.Fatalf("profile snapshot must carry the environment name, got %+v", declared.Secrets)
	}
	views := SecretViewsFromSnapshot(declared)
	if len(views) != 1 || views[0].EnvironmentName != "HF_TOKEN" {
		t.Fatalf("snapshot view must carry the environment name, got %+v", views)
	}
	undeclared, err := s.ExportSnapshotForProfile(profileA, false, "")
	if err != nil {
		t.Fatalf("export undeclared-version snapshot: %v", err)
	}
	if len(undeclared.Secrets) != 0 || len(undeclared.SecretAllows) != 0 {
		t.Fatalf("undeclared-version client must not receive secrets: %+v %+v",
			undeclared.Secrets, undeclared.SecretAllows)
	}

	full, err := s.ExportSnapshot()
	if err != nil {
		t.Fatalf("export full snapshot: %v", err)
	}
	if len(full.Secrets) != 1 || full.Secrets[0].EnvironmentName != "HF_TOKEN" {
		t.Fatalf("full snapshot must carry the environment name, got %+v", full.Secrets)
	}

	otherKey := make([]byte, 32)
	randRead(t, otherKey)
	target, err := Open(filepath.Join(t.TempDir(), "restored.db"), otherKey)
	if err != nil {
		t.Fatalf("open target store: %v", err)
	}
	defer target.Close()
	if err := target.ImportSnapshot(full); err != nil {
		t.Fatalf("import full snapshot: %v", err)
	}
	restored, err := target.ExportSnapshot()
	if err != nil {
		t.Fatalf("export restored snapshot: %v", err)
	}
	if len(restored.Secrets) != 1 || restored.Secrets[0].EnvironmentName != "HF_TOKEN" {
		t.Fatalf("imported snapshot must keep the environment name, got %+v", restored.Secrets)
	}
}
