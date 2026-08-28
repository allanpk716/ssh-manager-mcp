package tui

// Plan 42 批1 T8: broker TUI 的 Pairing 批准页。
//
// 驱动形态(对 brief 的一处登记偏离):huh 的键击回环已由 TestAppLoopProfileFormCompletes
// / wizard_loop 系测试钉住,本文件钉的是页面语义——列表渲染(name/@/⚠ 标记)、
// 批准闸(foreign 必键 OVERRIDE)、以及批准/拒绝提交路径的真实 store 效果(CAS)。
// 因此提交直接驱动 page 的 submit 函数,不经 huh 键击模拟。
//
// SAS 裁决(控制器):批准面只显示 name+target_url 两件 + 「SAS 码见 client 屏幕」
// 提示行——SAS 需要 serve 进程内存里的 ECDH 私钥,批准进程(TUI/CLI)算不出,
// 不伪造第三件。

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/store"
)

// seedPairingStore opens an isolated vault store and seeds two profiles; the
// caller enrolls its own pending rows (enroll window 10min like the handler).
func seedPairingStore(t *testing.T) (*store.Store, []*models.Profile) {
	t.Helper()
	dir := t.TempDir()
	mk, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "t.db"), mk)
	if err != nil {
		t.Fatalf("open temp store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	pa, err := st.AddProfile("home")
	if err != nil {
		t.Fatal(err)
	}
	pb, err := st.AddProfile("work")
	if err != nil {
		t.Fatal(err)
	}
	return st, []*models.Profile{{ID: pa, Name: "home"}, {ID: pb, Name: "work"}}
}

// enroll adds one pending pairing row with a fresh 32B id.
func enroll(t *testing.T, st *store.Store, name, targetURL, hint string, replaceInactive bool) store.PendingPairing {
	t.Helper()
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	p := store.PendingPairing{
		ID: id, Name: name, TargetURL: targetURL, ProfileHint: hint,
		ClientPub: make([]byte, 32), Cnonce: make([]byte, 16), // schema NOT NULL;审批面不读密钥料
		ServerPub: make([]byte, 32), Snonce: make([]byte, 16), Sig: make([]byte, 64),
		ReplaceInactive: replaceInactive, State: "pending", SourceIP: "192.0.2.9",
		EnrollDeadline: time.Now().Add(10 * time.Minute).Unix(),
	}
	if err := st.AddPendingPairing(&p, 0, 0); err != nil {
		t.Fatal(err)
	}
	return p
}

// localTargetURL builds a target that ForeignTarget must accept (this host's
// first non-loopback IPv4); skipped upstream when the host has none.
func localTargetURL(t *testing.T) string {
	t.Helper()
	ips := mcpserver.LocalNonLoopbackIPs()
	if len(ips) == 0 {
		t.Skip("host has no non-loopback IPv4 — the local-target leg is unbuildable")
	}
	u := fmt.Sprintf("https://%s:7878", ips[0])
	if mcpserver.ForeignTarget(u) {
		t.Fatalf("premise: %q must be non-foreign", u)
	}
	return u
}

func TestPairingPage_ListAndApprove(t *testing.T) {
	st, profiles := seedPairingStore(t)
	local := localTargetURL(t)
	a := enroll(t, st, "laptop", local, "home", true)                // 本机地址 + ⚠未激活码替换
	b := enroll(t, st, "phone", "https://127.0.0.1:7878", "", false) // 外部地址

	rows, err := st.ListPendingPairing()
	if err != nil || len(rows) != 2 {
		t.Fatalf("premise: 2 pending rows, got %d (%v)", len(rows), err)
	}
	page := newPairingPage(rows, profiles, "")
	if page.Title() == "" {
		t.Fatal("page must carry a title")
	}
	if got := page.Rows(); len(got) != 2 || got[0] != "laptop" || got[1] != "phone" {
		t.Fatalf("rows must list both names in enroll order, got %v", got)
	}

	// 渲染件:每行 Title=name,desc 含 @target/来源 IP;flags 按行标注。
	desc0 := pairingItem{p: rows[0]}.Description()
	if !strings.Contains(desc0, local) || !strings.Contains(desc0, "192.0.2.9") {
		t.Fatalf("local row desc missing @target/IP:\n%s", desc0)
	}
	if d := pairingRowDesc(rows[0], time.Now().Unix()); !strings.Contains(d, "替换") {
		t.Fatalf("replace_inactive row must carry the ⚠替换 marker:\n%s", d)
	}
	if d := pairingRowDesc(rows[1], time.Now().Unix()); !strings.Contains(d, "≠") {
		t.Fatalf("foreign row must carry the 目标≠本机 marker:\n%s", d)
	}
	if d := pairingRowDesc(rows[0], time.Now().Unix()); strings.Contains(d, "≠") {
		t.Fatalf("local row must NOT carry the foreign marker:\n%s", d)
	}
	// 批准面提示行:两件套 + SAS 在 client 屏。
	detail := page.Detail()
	if !strings.Contains(detail, "laptop") || !strings.Contains(detail, "SAS") {
		t.Fatalf("detail must show the two-piece line + the SAS-on-client-screen hint:\n%s", detail)
	}

	// 批准(home):提交 → CAS 生效 → 行 state=approved 且 profile 落库。
	cmd := submitPairingApproval(st, &rows[0], &pairingApproval{ProfileID: profiles[0].ID})
	if cmd == nil {
		t.Fatal("approval submit must return a cmd")
	}
	msg := cmd()
	if _, ok := msg.(actionDoneMsg); !ok {
		t.Fatalf("approval success must ride actionDoneMsg, got %T (%v)", msg, msg)
	}
	after, err := st.ListPendingPairing()
	if err != nil {
		t.Fatal(err)
	}
	var approved *store.PendingPairing
	for i := range after {
		if bytes.Equal(after[i].ID, a.ID) {
			approved = &after[i]
		}
	}
	if approved == nil || approved.State != "approved" || approved.Profile != profiles[0].ID {
		t.Fatalf("approve must flip the row to approved with the chosen profile, got %+v", approved)
	}
	// 另一行不受影响。
	for _, r := range after {
		if bytes.Equal(r.ID, b.ID) && r.State != "pending" {
			t.Fatalf("unrelated row must stay pending, got %s", r.State)
		}
	}
}

func TestPairingPage_ForeignRequiresOverride(t *testing.T) {
	st, profiles := seedPairingStore(t)
	row := enroll(t, st, "phone", "https://127.0.0.1:7878", "", false)
	if !mcpserver.ForeignTarget(row.TargetURL) {
		t.Fatalf("premise: 127.0.0.1 target must be foreign")
	}

	// 闸:foreign 且未精确键入 OVERRIDE → 拒;键入 → 放行;非 foreign 无需键入。
	for _, c := range []struct {
		override string
		foreign  bool
		wantErr  bool
	}{
		{"", true, true},
		{"override", true, true},   // 小写不算
		{"OVERRIDE ", true, false}, // 首尾空白容忍(TrimSpace)
		{"OVERRIDE", true, false},
		{"", false, false},
	} {
		err := validatePairingOverride(c.override, c.foreign)
		if (err != nil) != c.wantErr {
			t.Fatalf("validatePairingOverride(%q,%v) err=%v wantErr=%v", c.override, c.foreign, err, c.wantErr)
		}
	}

	// 提交层复核(纵深防御):foreign 未键 OVERRIDE → 报错且 store 零变化。
	msg := submitPairingApproval(st, &row, &pairingApproval{ProfileID: profiles[0].ID})()
	if _, ok := msg.(errMsg); !ok {
		t.Fatalf("override-less submit on a foreign row must be refused, got %T (%v)", msg, msg)
	}
	live, err := st.ListPendingPairing()
	if err != nil || len(live) != 1 || live[0].State != "pending" {
		t.Fatalf("refused submit must leave the row pending, got %+v (%v)", live, err)
	}

	// 键入 OVERRIDE → 通过并 approved。
	msg2 := submitPairingApproval(st, &row, &pairingApproval{ProfileID: profiles[1].ID, Override: "OVERRIDE"})()
	if _, ok := msg2.(actionDoneMsg); !ok {
		t.Fatalf("override submit must pass, got %T (%v)", msg2, msg2)
	}
	live2, _ := st.ListPendingPairing()
	if len(live2) != 1 || live2[0].State != "approved" || live2[0].Profile != profiles[1].ID {
		t.Fatalf("override submit must approve into profile work, got %+v", live2)
	}
}

func TestPairingPage_RejectAndCASMiss(t *testing.T) {
	st, _ := seedPairingStore(t)
	row := enroll(t, st, "tab", "https://127.0.0.1:7878", "", false)

	if msg := submitPairingReject(st, &row)(); func() bool { _, ok := msg.(actionDoneMsg); return !ok }() {
		t.Fatalf("reject must ride actionDoneMsg, got %T (%v)", msg, msg)
	}
	live, _ := st.ListPendingPairing()
	if len(live) != 0 {
		t.Fatalf("rejected row must leave the actionable queue, got %+v", live)
	}
	// CAS 未命中(行已终态)→ 明确报错,不静默。
	if msg := submitPairingReject(st, &row)(); func() bool { _, ok := msg.(errMsg); return !ok }() {
		t.Fatalf("second reject must surface the CAS miss, got %T (%v)", msg, msg)
	}
}
