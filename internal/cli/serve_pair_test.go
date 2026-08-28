package cli

// Plan 42 批1 T8: `serve pair` CLI——跨进程直连 store 的配对裁决面(TUI 批1 的
// 兜底面)。SAS 裁决(控制器):批准进程拿不到 serve 内存里的 ECDH 私钥,输出行
// = `<name> @ <target_url> (对照 client 屏 SAS 后批准)`——不伪造第三件。
//
// foreign(机械地址校验)无 --allow-foreign-url → 拒绝并打 ⚠ 文案;有 flag → 放行。

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/store"
)

// seedPendingPairing opens the pinned store directly and enrolls one pending
// row (the /pair/enroll handler's shape: 32B id + 10min enroll window).
func seedPendingPairing(t *testing.T, path string, mk []byte, name, targetURL string) store.PendingPairing {
	t.Helper()
	st, err := store.Open(path, mk)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	p := store.PendingPairing{
		ID: id, Name: name, TargetURL: targetURL, ProfileHint: "team-a",
		ClientPub: make([]byte, 32), Cnonce: make([]byte, 16), // schema NOT NULL;审批面不读密钥料
		ServerPub: make([]byte, 32), Snonce: make([]byte, 16), Sig: make([]byte, 64),
		State: "pending", SourceIP: "192.0.2.9",
		EnrollDeadline: time.Now().Add(10 * time.Minute).Unix(),
	}
	if err := st.AddPendingPairing(&p, 0, 0); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServePairApprove_ThreePieceOutputAndOverride(t *testing.T) {
	path, mk := withCliStoreEnv(t)
	runCli(t, "profiles", "add", "team-a")
	seedPendingPairing(t, path, mk, "laptop", "https://127.0.0.1:7878") // 127.0.0.1 恒 foreign

	// ls:清单列全(NAME/TARGET_URL/SOURCE_IP/HINT/FLAGS/PROFILE_HINT)。
	ls := runCli(t, "serve", "pair", "ls")
	for _, want := range []string{"laptop", "https://127.0.0.1:7878", "192.0.2.9", "team-a", "≠"} {
		if !strings.Contains(ls, want) {
			t.Fatalf("serve pair ls missing %q:\n%s", want, ls)
		}
	}
	if !strings.Contains(ls, "client 屏幕") {
		t.Fatalf("ls must hint that the SAS shows on the client screen:\n%s", ls)
	}

	// foreign 无 flag → 拒绝并打 ⚠ 文案,行保持 pending。
	if got := runCliErr(t, "serve", "pair", "approve", "laptop", "--profile", "team-a"); !strings.Contains(got, "⚠") {
		t.Fatalf("foreign approve without --allow-foreign-url must fail with the ⚠ copy, got %q", got)
	}

	// --allow-foreign-url → 批准;输出行 = 两件套 + 对照 client 屏 SAS 的措辞。
	out := runCli(t, "serve", "pair", "approve", "laptop", "--profile", "team-a", "--allow-foreign-url")
	for _, want := range []string{"laptop", "https://127.0.0.1:7878", "SAS", "对照"} {
		if !strings.Contains(out, want) {
			t.Fatalf("approve output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "SAS 0") || strings.Contains(out, "SAS 1") {
		t.Fatalf("approve output must NOT fabricate a SAS code:\n%s", out)
	}

	// 批准后行仍可见但标记为 approved(finish 窗口内 store 契约:approved 行
	// 仍在 actionable 队列);重复批准 → CAS 报错。
	ls2 := runCli(t, "serve", "pair", "ls")
	if !strings.Contains(ls2, "laptop") || !strings.Contains(ls2, "approved") {
		t.Fatalf("approved row must show with the approved flag during its finish window, got:\n%s", ls2)
	}
	if got := runCliErr(t, "serve", "pair", "approve", "laptop", "--profile", "team-a", "--allow-foreign-url"); got == "" {
		t.Fatal("double approve must error (row no longer pending)")
	}
}

func TestServePair_ResolveAndReject(t *testing.T) {
	path, mk := withCliStoreEnv(t)
	runCli(t, "profiles", "add", "team-a")
	row := seedPendingPairing(t, path, mk, "tablet", "https://127.0.0.1:7878")
	idHex := hex.EncodeToString(row.ID)

	// 未知 profile 名 / 未知设备名都报错。
	for _, args := range [][]string{
		{"serve", "pair", "approve", "tablet", "--profile", "ghost"},
		{"serve", "pair", "approve", "ghost", "--profile", "team-a", "--allow-foreign-url"},
		{"serve", "pair", "reject", "ghost"},
	} {
		if got := runCliErr(t, args...); got == "" {
			t.Fatalf("serve pair %v must error", args)
		}
	}

	// by-idHex 解析同样可用;reject 生效。
	out := runCli(t, "serve", "pair", "reject", idHex)
	if !strings.Contains(out, "tablet") {
		t.Fatalf("reject output must name the device:\n%s", out)
	}
	if ls := runCli(t, "serve", "pair", "ls"); strings.Contains(ls, "tablet") {
		t.Fatalf("rejected row must leave ls:\n%s", ls)
	}
}
