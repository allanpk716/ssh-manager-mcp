package cli

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/roles"
	"ssh-manager-mcp/internal/store"
)

// seedKeyscanCandidatesVault 准备一个单机 vault：本机设备「笔记本」与目标设
// 备「工作站」同属物理局域网，工作站在两个网络里各挂一个成员地址（先
// 192.0.2.5 后 192.0.2.6，即候选顺序），上面挂四台空主机条目与一台主机非空
// 条目；另有一台不挂设备的空主机条目。返回 master key 供落库断言。
func seedKeyscanCandidatesVault(t *testing.T) []byte {
	t.Helper()
	configRoot := t.TempDir()
	t.Setenv("APPDATA", configRoot)
	t.Setenv("XDG_CONFIG_HOME", configRoot)
	t.Setenv("SSHMGR_CONFIG_ROOT", configRoot)
	_, mk := withCliStoreEnv(t)
	if err := roles.Save(roles.State{Role: roles.RoleStandalone, SetupComplete: true}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(os.Getenv("SSHMGR_STORE"), mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.AddSite("家A", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家A局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetwork(models.Network{Name: "家B局域网", Type: models.NetworkPhysical, SiteName: "家A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDevice("工作站", "", ""); err != nil {
		t.Fatal(err)
	}
	local, err := st.AddDevice("笔记本", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := roles.SetLocalDeviceID(local.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "家A局域网", "192.0.2.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("工作站", "家B局域网", "192.0.2.6"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddNetworkMembership("笔记本", "家A局域网", "192.0.2.99"); err != nil {
		t.Fatal(err)
	}
	credential := &models.Credential{Type: models.CredPassword, Secret: []byte("pw")}
	for _, name := range []string{"裸条目", "裸条目二", "裸条目三", "裸条目四"} {
		if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
			Name: name, Host: "", Port: 22, User: "ops", AuthMethod: models.AuthPassword,
		}, credential, nil, "工作站"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.AddServerWithCredentialsToDevice(&models.Server{
		Name: "实主机", Host: "198.51.100.7", Port: 2222, User: "ops", AuthMethod: models.AuthPassword,
	}, credential, nil, "工作站"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddServerWithCredentials(&models.Server{
		Name: "孤立条目", Host: "", Port: 22, User: "ops", AuthMethod: models.AuthPassword,
	}, credential, nil); err != nil {
		t.Fatal(err)
	}
	return mk
}

// TestPinHostkey_KeyscanMatchesCandidateAddresses：--from-keyscan 对条目的
// 全部候选地址（设备网络成员地址加兜底主机）逐一推导 known_hosts 形态去匹
// 配，握手地址记录匹配上的那个候选地址；仍要求唯一密钥。
func TestPinHostkey_KeyscanMatchesCandidateAddresses(t *testing.T) {
	mk := seedKeyscanCandidatesVault(t)
	keyA, fpA := genHostKey(t)
	keyB, _ := genHostKey(t)
	keyG, fpG := genHostKey(t)
	keyH, fpH := genHostKey(t)
	line := func(pattern string, key ssh.PublicKey) string {
		return pattern + " " + key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())
	}

	cases := []struct {
		name          string
		entry         string
		lines         []string
		wantHandshake string   // 非空：期望成功，锚的握手地址为此值
		wantFp        string   // 成功时锚的指纹
		wantOut       []string // 成功时输出必须包含的片段
		wantErr       []string // 非空：期望拒绝，报错必须包含这些片段
	}{
		{
			name:          "空主机条目经成员地址匹配成功",
			entry:         "裸条目",
			lines:         []string{line("192.0.2.6", keyG)},
			wantHandshake: "192.0.2.6:22",
			wantFp:        fpG,
			wantOut:       []string{"pinned 裸条目 host=192.0.2.5:22", "handshake=192.0.2.6:22"},
		},
		{
			name:          "多个候选匹配同一把密钥按候选顺序取第一个",
			entry:         "裸条目二",
			lines:         []string{line("192.0.2.6", keyA), line("192.0.2.5", keyA)},
			wantHandshake: "192.0.2.5:22",
			wantFp:        fpA,
			wantOut:       []string{"handshake=192.0.2.5:22"},
		},
		{
			name:    "多个候选对应不同密钥时拒绝",
			entry:   "裸条目三",
			lines:   []string{line("192.0.2.5", keyA), line("192.0.2.6", keyB)},
			wantErr: []string{"2 different keys match", "forwarded automatically", "--fingerprint", "--force"},
		},
		{
			name:  "无匹配时报错列出全部尝试过的形态",
			entry: "裸条目四",
			lines: []string{line("203.0.113.9", keyA)},
			wantErr: []string{
				"forms found: [203.0.113.9]",
				"expected forms: [192.0.2.5, 192.0.2.6]",
			},
		},
		{
			name:          "非空主机条目经成员地址匹配成功",
			entry:         "实主机",
			lines:         []string{line("[192.0.2.5]:2222", keyH)},
			wantHandshake: "192.0.2.5:2222",
			wantFp:        fpH,
			wantOut:       []string{"pinned 实主机 host=198.51.100.7:2222", "handshake=192.0.2.5:2222"},
		},
		{
			name:    "没有任何候选地址时明确报错",
			entry:   "孤立条目",
			lines:   []string{line("203.0.113.9", keyA)},
			wantErr: []string{"no candidate addresses"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := keyscanFile(t, tc.lines...)
			if len(tc.wantErr) > 0 {
				errText := runCliErr(t, "servers", "pin-hostkey", tc.entry, "--from-keyscan", path)
				for _, want := range tc.wantErr {
					if !strings.Contains(errText, want) {
						t.Fatalf("报错缺少 %q：\n%s", want, errText)
					}
				}
				if strings.Contains(errText, "expected form: ") {
					t.Fatalf("报错不得再出现旧的单数形态提示 expected form: ：\n%s", errText)
				}
				if pin := entryPinAt(t, mk, tc.entry); pin != nil {
					t.Fatal("拒绝不得写锚")
				}
				return
			}
			out := runCli(t, "servers", "pin-hostkey", tc.entry, "--from-keyscan", path)
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Fatalf("输出缺少 %q：\n%s", want, out)
				}
			}
			meta, err := openPinVault(t, mk).GetEntryPinMeta(pinServerOf(t, mk, tc.entry).ID)
			if err != nil || meta == nil {
				t.Fatalf("锚定后应已有锚：%v %v", meta, err)
			}
			if meta.HandshakeAddr != tc.wantHandshake {
				t.Fatalf("锚的握手地址应为 %s，实际 %s", tc.wantHandshake, meta.HandshakeAddr)
			}
			if got := store.PinFingerprint(meta.Blob, meta.Format); got != tc.wantFp {
				t.Fatalf("锚的指纹应为 %s，实际 %s", tc.wantFp, got)
			}
		})
	}

	// 空主机条目的 pin-manual 审计行 host=-（#367 规则），指纹与握手取值
	// 来自匹配结果；三张成功用例各写一条审计，拒绝用例不写。
	for _, row := range pinAuditRows(t, mk, "pin-manual") {
		if strings.HasPrefix(row.Command, "entry=裸条目 ") {
			want := "entry=裸条目 host=- fingerprint=" + fpG + " via=keyscan forced=false"
			if row.Command != want {
				t.Fatalf("pin-manual 审计行不符：\nwant %q\n got %q", want, row.Command)
			}
			if n := len(pinAuditRows(t, mk, "pin-manual")); n != 3 {
				t.Fatalf("应恰好三条 pin-manual 审计（三张成功用例），实际 %d", n)
			}
			return
		}
	}
	t.Fatal("缺少裸条目的 pin-manual 审计行")
}
