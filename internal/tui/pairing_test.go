package tui

// Plan 42 批1 T8: broker TUI 的 Pairing 批准页。
//
// 驱动形态(对 brief 的一处登记偏离):huh 的键击回环已由 TestAppLoopProfileFormCompletes
// / wizard_loop 系测试钉住,本文件钉的是页面语义——列表渲染(name/@/⚠ 标记)、
// 批准闸(foreign 必键 OVERRIDE)、以及批准/拒绝提交路径的真实 store 效果(CAS)。
// 因此提交直接驱动 page 的 submit 函数,不经 huh 键击模拟。
//
// SAS 双屏比对(2026-09-01 裁决,恢复 rev4:68 冻结原文):serve 在 enroll 时算好
// SAS 落行,本页直读真值——Detail/表单/提交消息都是三件套 `<name> @ <url> SAS <6位>`;
// 行缺 SAS(版本错配)→ ⚠ 警示并建议拒绝,不静默回退两件套。

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

// enroll adds one pending pairing row with a fresh 32B id. sas seeds the
// row's landed SAS — "" models a pre-2026-09-01 serve's row.
func enroll(t *testing.T, st *store.Store, name, targetURL, hint string, replaceInactive bool, sas string) store.PendingPairing {
	t.Helper()
	id := make([]byte, 32)
	if _, err := rand.Read(id); err != nil {
		t.Fatal(err)
	}
	p := store.PendingPairing{
		ID: id, Name: name, TargetURL: targetURL, ProfileHint: hint, SAS: sas,
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
	a := enroll(t, st, "laptop", local, "home", true, "314159")                // 本机地址 + ⚠未激活码替换
	b := enroll(t, st, "phone", "https://127.0.0.1:7878", "", false, "271828") // 外部地址

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
	// 批准面三件套:Detail 显示行内真 SAS(serve enroll 时落库),owner 与
	// client 屏逐位比对。
	detail := page.Detail()
	if !strings.Contains(detail, "laptop") || !strings.Contains(detail, "SAS     314159") {
		t.Fatalf("detail must show the three-piece line with the row's real SAS:\n%s", detail)
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

// TestPairingPage_MissingSASWarns:行缺 SAS(旧版 serve 写的/版本错配)→ Detail
// 与批准表单都打 ⚠ 警示并建议拒绝——绝不静默回退到抓不住 MITM 的两件套对照,
// 也绝不伪造一个码。
func TestPairingPage_MissingSASWarns(t *testing.T) {
	st, profiles := seedPairingStore(t)
	local := localTargetURL(t)
	enroll(t, st, "laptop", local, "", false, "")

	rows, err := st.ListPendingPairing()
	if err != nil || len(rows) != 1 {
		t.Fatalf("premise: 1 pending row, got %d (%v)", len(rows), err)
	}
	page := newPairingPage(rows, profiles, "")
	detail := page.Detail()
	for _, want := range []string{"⚠ 行缺 SAS", "建议拒绝"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail must warn on the missing SAS (%q):\n%s", want, detail)
		}
	}
	if got := sasValue(rows[0]); !strings.Contains(got, "⚠ 行缺 SAS") {
		t.Fatalf("sasValue must surface the warning, got %q", got)
	}
	if got := sasValue(store.PendingPairing{SAS: "314159"}); got != "314159" {
		t.Fatalf("sasValue must pass the real code through, got %q", got)
	}
}

func TestPairingPage_ForeignRequiresOverride(t *testing.T) {
	st, profiles := seedPairingStore(t)
	row := enroll(t, st, "phone", "https://127.0.0.1:7878", "", false, "")
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
	row := enroll(t, st, "tab", "https://127.0.0.1:7878", "", false, "")

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

// TestPairingRender_StripsControlChars (终审修复 Important-2): pending 行的
// name/target_url/profile_hint 是未认证输入,列表行渲染前必须剥净 C0/C1 ——
// ESC 清屏序列、CR、LF 不得出现在任何渲染面上(剥离不吞可印正文)。
func TestPairingRender_StripsControlChars(t *testing.T) {
	row := store.PendingPairing{
		ID:             bytes.Repeat([]byte{9}, 32),
		Name:           "evil\x1b[2Jdev",
		TargetURL:      "https://10.0.0.5:7878/\rX",
		ProfileHint:    "hint]\x1b",
		State:          "pending",
		SourceIP:       "192.0.2.9",
		EnrollDeadline: time.Now().Add(10 * time.Minute).Unix(),
	}
	for label, got := range map[string]string{
		"title": pairingItem{p: row}.Title(),
		"desc":  pairingRowDesc(row, time.Now().Unix()),
	} {
		for _, bad := range []string{"\x1b", "\r", "\n", "\t"} {
			if strings.Contains(got, bad) {
				t.Fatalf("%s rendered a raw control byte %q:\n%q", label, bad, got)
			}
		}
	}
	// 剥离不吞正文:desc 行可见剥净后的 target 与 hint;name 走 Title。
	d := pairingRowDesc(row, time.Now().Unix())
	if !strings.Contains(d, "https://10.0.0.5:7878/X") || !strings.Contains(d, "hint:hint]") {
		t.Fatalf("stripped target/hint must stay visible in the row:\n%q", d)
	}
	title := pairingItem{p: row}.Title()
	if title != "evil[2Jdev" {
		t.Fatalf("title = %q, want the stripped name", title)
	}
}
