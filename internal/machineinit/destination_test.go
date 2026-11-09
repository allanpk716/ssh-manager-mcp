package machineinit_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/machineinit"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

func roleState(role roles.Role, setupComplete bool) *roles.State {
	return &roles.State{Role: role, SetupComplete: setupComplete}
}

func TestDecideUIDestination(t *testing.T) {
	tests := []struct {
		name string
		snap machineinit.StateSnapshot
		want machineinit.UIDestination
	}{
		{
			name: "空机器进入向导选角色",
			snap: machineinit.StateSnapshot{},
			want: machineinit.UIDestination{
				Kind: machineinit.UIDestinationInitializationWizard,
				Step: machineinit.WizardStepChooseRole,
			},
		},
		{
			name: "没有角色文件但有 vault 的旧机器按推断结果进入单机页面",
			snap: machineinit.StateSnapshot{
				DetectedRole:  roles.RoleStandalone,
				VaultExists:   true,
				VaultOpenable: true,
				VaultOpenErr:  nil,
			},
			want: machineinit.UIDestination{Kind: machineinit.UIDestinationStandaloneWebUI},
		},
		{
			name: "没有角色文件但有客户端缓存的旧机器进入客户端页面",
			snap: machineinit.StateSnapshot{
				DetectedRole:       roles.RoleClient,
				ClientCachePresent: true,
			},
			want: machineinit.UIDestination{Kind: machineinit.UIDestinationClientWebUI},
		},
		{
			name: "单机 setup_complete 为假且无 vault 时续做建库",
			snap: machineinit.StateSnapshot{
				Role: roleState(roles.RoleStandalone, false),
			},
			want: machineinit.UIDestination{
				Kind: machineinit.UIDestinationInitializationWizard,
				Role: roles.RoleStandalone,
				Step: machineinit.WizardStepCreateVault,
			},
		},
		{
			name: "单机 vault 可打开时进入正式页面",
			snap: machineinit.StateSnapshot{
				Role:          roleState(roles.RoleStandalone, false),
				VaultExists:   true,
				VaultOpenable: true,
			},
			want: machineinit.UIDestination{Kind: machineinit.UIDestinationStandaloneWebUI},
		},
		{
			name: "权威端缺 vault 时从建 vault 续做",
			snap: machineinit.StateSnapshot{
				Role: roleState(roles.RoleServer, false),
			},
			want: machineinit.UIDestination{
				Kind: machineinit.UIDestinationServerWizard,
				Role: roles.RoleServer,
				Step: machineinit.WizardStepCreateVault,
			},
		},
		{
			name: "权威端缺管理员口令时从设口令续做",
			snap: machineinit.StateSnapshot{
				Role:          roleState(roles.RoleServer, false),
				VaultExists:   true,
				VaultOpenable: true,
			},
			want: machineinit.UIDestination{
				Kind: machineinit.UIDestinationServerWizard,
				Role: roles.RoleServer,
				Step: machineinit.WizardStepSetAdminPassword,
			},
		},
		{
			name: "权威端口令已设但 serve 未运行时直接安装 serve",
			snap: machineinit.StateSnapshot{
				Role:             roleState(roles.RoleServer, true),
				VaultExists:      true,
				VaultOpenable:    true,
				AdminPasswordSet: true,
			},
			want: machineinit.UIDestination{
				Kind: machineinit.UIDestinationServerWizard,
				Role: roles.RoleServer,
				Step: machineinit.WizardStepInstallServe,
			},
		},
		{
			name: "权威端 serve 已运行时只打印状态",
			snap: machineinit.StateSnapshot{
				Role:             roleState(roles.RoleServer, true),
				VaultExists:      true,
				VaultOpenable:    true,
				AdminPasswordSet: true,
				ServeWebUI: machineinit.ServeWebUIState{
					Known:     true,
					Port:      7879,
					Addresses: []string{"127.0.0.1"},
					Running:   true,
				},
			},
			want: machineinit.UIDestination{Kind: machineinit.UIDestinationAuthorityStatus},
		},
		{
			name: "权威端 serve 已运行且口令未设时仍只打印状态",
			snap: machineinit.StateSnapshot{
				Role:          roleState(roles.RoleServer, true),
				VaultExists:   true,
				VaultOpenable: true,
				ServeWebUI: machineinit.ServeWebUIState{
					Known:     true,
					Port:      7879,
					Addresses: []string{"127.0.0.1"},
					Running:   true,
				},
			},
			want: machineinit.UIDestination{Kind: machineinit.UIDestinationAuthorityStatus},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := machineinit.DecideUIDestination(test.snap)
			if err != nil {
				t.Fatalf("不应失败：%v", err)
			}
			if got != test.want {
				t.Fatalf("去向 = %+v，应为 %+v", got, test.want)
			}
		})
	}
}

func TestDecideUIDestinationKeepsVaultOpenError(t *testing.T) {
	openErr := errors.New("vault locked: run `sshmgr unlock`")
	_, err := machineinit.DecideUIDestination(machineinit.StateSnapshot{
		VaultExists:  true,
		VaultOpenErr: openErr,
	})
	if err == nil || !strings.Contains(err.Error(), openErr.Error()) {
		t.Fatalf("vault 打不开时应沿用原错误：%v", err)
	}
}

func TestReadSnapshotInfersOldMachineRole(t *testing.T) {
	t.Run("没有角色文件但有 vault 时推断单机", func(t *testing.T) {
		withMachineEnv(t)
		masterKey, err := store.GenerateMasterKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(os.Getenv("SSHMGR_FILEKEY_PATH")), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv("SSHMGR_FILEKEY_PATH"), masterKey, 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(os.Getenv("SSHMGR_STORE"), masterKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}

		snapshot, err := machineinit.ReadSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.DetectedRole != roles.RoleStandalone {
			t.Fatalf("旧 vault 机器应按现有规则推断单机，实际：%q", snapshot.DetectedRole)
		}
	})

	t.Run("没有角色文件但有客户端缓存时推断客户端", func(t *testing.T) {
		withMachineEnv(t)
		cacheDir := filepath.Join(os.Getenv("SSHMGR_CONFIG_ROOT"), "ssh-manager")
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			t.Fatal(err)
		}
		credential := []byte(`{"url":"https://192.0.2.9:7878","token":"device-code"}`)
		if err := os.WriteFile(filepath.Join(cacheDir, "cache.auth.json"), credential, 0o600); err != nil {
			t.Fatal(err)
		}

		snapshot, err := machineinit.ReadSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.DetectedRole != roles.RoleClient {
			t.Fatalf("客户端缓存机器应推断客户端，实际：%q", snapshot.DetectedRole)
		}
	})
}
