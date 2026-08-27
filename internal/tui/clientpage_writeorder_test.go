package tui

// Plan 40 批2 T9 — spec rev5 §5/§7/§11.12/§11.15。
//
// 与骨架两处有意偏离（均由真实定义强制，沿 clientpage_routing_test.go 先例在此登记）：
//
//  1. “目录占位 → 本次新建空目录被清理”两条腿在真实时序下互斥：占位必须先于提交
//     落盘，于是提交闭包内的 os.Stat(tdir) 命中既存 ⇒ created=false ⇒ 决不进入清理
//     分支（且此刻槽内已有占位 = 非空，os.Remove 的“仅空才删”守卫同样不该删）。
//     清理分支不可从表单端到端观测；其门控变量 created 的两个前置（Stat-IsNotExist /
//     MkdirAll 成功）由 TestPanelSubmit_NewInstanceMkdirAllAndAuth（真空新建成功路）
//     与本文件的既存槽失败用例两点夹逼钉住。
//  2. wizard 后移写（syncCmdMode 内 WriteCacheCredFor(res.Instance)）依赖真 serve +
//     DEK + pull 全链 — 归 T11 e2e；此处只钉表单层的零写入与消息层的自动选中。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/clientops"
)

// TestWizardSubmit_DoesNotWriteAuthBeforePull (rev5 §5 wizard row): the wizard
// form save persists NOTHING — the pull reveals the effective slot (auto-
// relocation), auth lands only after success. The submit must yield
// connSavedMsg carrying draft+cred AND the form-routed slot (canonical target,
// review F2#2), with zero cache.auth.json anywhere.
func TestWizardSubmit_DoesNotWriteAuthBeforePull(t *testing.T) {
	base := isolatedConfigDir(t) // 真空 seed：默认槽四文件全无
	m := newClientModelForGate(t)
	m.wizard = true
	m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "wizard-tok", Pin: formGoodPin}

	fo := openEditConnForm(t, m)
	sendKey(fo, keyEnter) // serve 地址 prefill 合法 → 实例名
	clearFocused(fo)
	typeInto(fo, "agentW") // 跨槽新实例 → rule1 要求设备码
	sendKey(fo, keyEnter)  // → 设备码
	typeInto(fo, "wcode9")
	sendKey(fo, keyEnter) // → pin（prefill 合法）
	res, completed := submitForm(fo)
	if !completed {
		t.Fatal("legal values must complete the form (driver broke?)")
	}
	sm, ok := res.(connSavedMsg)
	if !ok {
		t.Fatalf("wizard save must return connSavedMsg without touching disk, got %T (%v)", res, res)
	}
	if sm.instance != "agentW" || sm.draft == nil || sm.cred == nil {
		t.Fatalf("connSavedMsg must carry the form-routed slot + draft/cred, got %+v", sm)
	}
	// 默认槽与实例槽均无 cache.auth.json（表单保存即写的历史例外就此关闭）。
	for _, p := range []string{
		filepath.Join(base, "cache.auth.json"),
		filepath.Join(base, "instances", "agentW", "cache.auth.json"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("wizard save-time auth must NOT be written at %s (stat err=%v)", p, err)
		}
	}
}

// TestPanelSubmit_NewInstanceMkdirAllAndAuth (rev5 §5 panel row): saving on a
// NEW named instance creates the slot directory and lands cache.auth.json in
// THAT slot; connSavedMsg.instance carries the routed name and the model
// switches its session slot accordingly.
func TestPanelSubmit_NewInstanceMkdirAllAndAuth(t *testing.T) {
	base := isolatedConfigDir(t)
	m := newClientModelForGate(t)
	// 选中默认（真空）+ 字段 agentC + 设备码 —— target 非 "",真空守卫不经停。
	m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "keep-tok", Pin: formGoodPin}

	fo := openEditConnForm(t, m)
	sendKey(fo, keyEnter) // serve 地址 prefill → 实例名
	clearFocused(fo)
	typeInto(fo, "agentC")
	sendKey(fo, keyEnter) // → 设备码（跨槽必填）
	typeInto(fo, "code-C1")
	sendKey(fo, keyEnter) // → pin（prefill）
	res, completed := submitForm(fo)
	if !completed {
		t.Fatal("legal values must complete the form (driver broke?)")
	}
	sm, ok := res.(connSavedMsg)
	if !ok {
		t.Fatalf("panel save must return connSavedMsg (routed slot inside), got %T (%v)", res, res)
	}
	if sm.instance != "agentC" {
		t.Fatalf("connSavedMsg.instance must carry the routed slot, got %q", sm.instance)
	}

	dir := filepath.Join(base, "instances", "agentC")
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("submit must MkdirAll instances/agentC/, stat err=%v", err)
	}
	authBlob, err := os.ReadFile(filepath.Join(dir, "cache.auth.json"))
	if err != nil {
		t.Fatalf("cache.auth.json must land in the new slot: %v", err)
	}
	var got clientops.CacheCred
	if jerr := json.Unmarshal(authBlob, &got); jerr != nil {
		t.Fatalf("landed auth must parse: %v", jerr)
	}
	if got.Token != "code-C1" {
		t.Fatalf("routed slot auth must carry the submitted code, got %q", got.Token)
	}

	// 消息层切槽：connSavedMsg 入模态后面板跟随表单路由的槽。
	nm, _ := m.Update(sm)
	cm, ok := nm.(clientModel)
	if !ok {
		t.Fatalf("want clientModel back, got %T", nm)
	}
	if cm.instance != "agentC" {
		t.Fatalf("model must switch to the form-routed slot, got %q", cm.instance)
	}
}

// TestPanelSubmit_FoldRoutedWritesCanonicalSlot (review F2#1, 行为级)：输入
// 小写变体 "agenta"、选中既有规范名目录 AGENTA —— 同槽折叠放行后，auth 必须
// 落进【规范名】目录 instances/AGENTA/，不得新建小写变体目录。
func TestPanelSubmit_FoldRoutedWritesCanonicalSlot(t *testing.T) {
	base := isolatedConfigDir(t)
	canonical := filepath.Join(base, "instances", "AGENTA")
	if err := os.MkdirAll(canonical, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"url":"https://s","pulled_at":1,"device_name":"X"}`
	if err := os.WriteFile(filepath.Join(canonical, "cache.meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	m := newClientModelForGate(t)
	m.instance = "AGENTA" // 选中既有规范名槽（[i] 语义等价物）
	m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "keep-tok", Pin: formGoodPin}

	fo := openEditConnForm(t, m)
	sendKey(fo, keyEnter) // serve 地址 prefill → 实例名（prefill "AGENTA"）
	clearFocused(fo)
	typeInto(fo, "agenta") // 同槽他种大小写：fold 放行（rule3），路由值归一
	sendKey(fo, keyEnter)  // → 设备码（同槽有 token 可保持，留空）
	sendKey(fo, keyEnter)  // → pin（prefill）
	res, completed := submitForm(fo)
	if !completed {
		t.Fatal("same-slot casefold must complete the form (driver broke?)")
	}
	sm, ok := res.(connSavedMsg)
	if !ok {
		t.Fatalf("fold-routed save must return connSavedMsg, got %T (%v)", res, res)
	}
	if sm.instance != "AGENTA" {
		t.Fatalf("routing value must be the CANONICAL dir name AGENTA, got %q", sm.instance)
	}
	if _, err := os.Stat(filepath.Join(canonical, "cache.auth.json")); err != nil {
		t.Fatalf("auth must land in the canonical slot instances/AGENTA/: %v", err)
	}
	// 变体目录检查不能用 os.Stat——NTFS/APFS 大小写不敏感会命中既存 AGENTA；
	// 用大小写保真的 ReadDir 枚举：物理上必须只有一个目录，且名即规范名。
	root := filepath.Join(base, "instances")
	entries, rerr := os.ReadDir(root)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 1 || entries[0].Name() != "AGENTA" {
		t.Fatalf("instances root must hold exactly the canonical dir AGENTA, got %+v", entries)
	}
}

// TestPanelSubmit_AuthWriteFailureCleansEmptyDir (rev5 §11.15) — 与骨架的偏离
// 见文件头注释 ①：占位先于提交 ⇒ created 恒 false ⇒ 不存在“本次新建被清理”的
// 可达形态。以下子测试钉住真实可达边界：写失败必达 errMsg 且对磁盘零进一步动作；
// 既存槽（含材料）完好保留。“本次确实新建”的成功路径已在 MkdirAll 用例中验证
// 无残留；“仅空才删”是 os.Remove 的 OS 语义，非本包实现。
func TestPanelSubmit_AuthWriteFailureCleansEmptyDir(t *testing.T) {
	t.Run("auth path occupied by a pre-placed DIRECTORY: errMsg, existing slot + material retained", func(t *testing.T) {
		base := isolatedConfigDir(t)
		slot := filepath.Join(base, "instances", "agentP")
		if err := os.MkdirAll(slot, 0o700); err != nil {
			t.Fatal(err)
		}
		meta := `{"url":"https://s","pulled_at":1,"device_name":"P"}`
		if err := os.WriteFile(filepath.Join(slot, "cache.meta.json"), []byte(meta), 0o600); err != nil {
			t.Fatal(err)
		}
		// 目录占位：atomicWriteUnique 的 rename 在该路径上必败（不能把文件换成目录）。
		if err := os.MkdirAll(filepath.Join(slot, "cache.auth.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		m := newClientModelForGate(t)
		m.instance = "agentP" // [i] 选中语义 → 同槽允许，直达写入点
		m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "keep-tok", Pin: formGoodPin}

		fo := openEditConnForm(t, m)
		// 全部走 prefill/留空：同槽 + 有 token 保持 ⇒ 直接命中 WriteCacheCredFor 失败。
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("prefilled values must complete the form (driver broke?)")
		}
		em, ok := res.(errMsg)
		if !ok {
			t.Fatalf("occupied auth path must surface errMsg, got %T (%v)", res, res)
		}
		if em.err == nil || em.err.Error() == "" {
			t.Fatal("errMsg must carry the underlying write failure")
		}
		// 既存槽零损伤：目录仍在列、meta 原样（写失败路径不得顺手破坏任何东西）。
		names, lerr := clientops.ListInstances()
		if lerr != nil {
			t.Fatal(lerr)
		}
		kept := false
		for _, n := range names {
			if n == "agentP" {
				kept = true
			}
		}
		if !kept {
			t.Fatalf("failed write must NOT remove a slot that predates the submit, ListInstances=%v", names)
		}
		b, rerr := os.ReadFile(filepath.Join(slot, "cache.meta.json"))
		if rerr != nil || string(b) != meta {
			t.Fatalf("preset meta must survive untouched (%v, %q)", rerr, string(b))
		}
	})

	t.Run("instances entry is a regular FILE: write fails fast (ENOTDIR), nothing created", func(t *testing.T) {
		base := isolatedConfigDir(t)
		if err := os.MkdirAll(filepath.Join(base, "instances"), 0o700); err != nil {
			t.Fatal(err)
		}
		slotFile := filepath.Join(base, "instances", "agentQ")
		if err := os.WriteFile(slotFile, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		m := newClientModelForGate(t)
		m.instance = "agentQ" // 经 CachePathsFor 直达：Stat 命中的是个普通文件
		m.cred = &clientops.CacheCred{URL: "https://s.example", Token: "keep-tok", Pin: formGoodPin}

		fo := openEditConnForm(t, m)
		res, completed := submitForm(fo)
		if !completed {
			t.Fatal("prefilled values must complete the form (driver broke?)")
		}
		if _, ok := res.(errMsg); !ok {
			t.Fatalf("non-directory instance entry must fail the write with errMsg, got %T (%v)", res, res)
		}
		// 文件条目本来就不进 ListInstances；失败路径也不得把它变成任何目录。
		fi, serr := os.Stat(slotFile)
		if serr != nil || !fi.Mode().IsRegular() {
			t.Fatalf("failed write must leave the odd entry alone: fi=%v err=%v", fi, serr)
		}
		names, lerr := clientops.ListInstances()
		if lerr != nil {
			t.Fatal(lerr)
		}
		for _, n := range names {
			if n == "agentQ" {
				t.Fatalf("a file entry is not an instance slot, got %v", names)
			}
		}
	})
}

// TestFinishScreen_InstanceForm (rev5 §7): instance != "" swaps the offline
// snippet's args for the --instance form and prepends the slot note; "" keeps
// the legacy dual forms byte-for-byte (zero regression).
func TestFinishScreen_InstanceForm(t *testing.T) {
	v := clientFinishScreen("https://x", "agentA").View().Content
	if !strings.Contains(v, `"args": ["mcp", "--cache", "--instance", "agentA"],`) {
		t.Fatalf("named-slot finish screen must carry the --instance args:\n%s", v)
	}
	if !strings.Contains(v, "本机 cache 位于实例槽 instances/agentA/") ||
		!strings.Contains(v, "--instance agentA。") {
		t.Fatalf("named-slot finish screen must carry the slot note lines:\n%s", v)
	}
	if strings.Contains(v, `"args": ["mcp", "--cache"],`) {
		t.Fatalf("the legacy args line must be fully replaced:\n%s", v)
	}

	dual := clientFinishScreen("", "").View().Content
	for _, want := range []string{
		`"args": ["mcp", "--cache"],`,
		`"type": "http",`,
		"<serve URL>",
	} {
		if !strings.Contains(dual, want) {
			t.Fatalf("empty instance must keep the legacy dual forms, missing %q:\n%s", want, dual)
		}
	}
	if strings.Contains(dual, "--instance") {
		t.Fatalf("default-slot finish screen must not mention --instance:\n%s", dual)
	}
}

// TestPullSucceeded_AutoSelectsEffectiveInstance (rev5 §7 R2-Q2a + review
// F2#2): the wizard-first-pull success lands the session on the EFFECTIVE
// slot (PullResult.Instance) and hands THAT name to the finish screen.
func TestPullSucceeded_AutoSelectsEffectiveInstance(t *testing.T) {
	m := newClientModel()
	m.wizard = true
	m.cred = &clientops.CacheCred{URL: "https://192.0.2.5:7878", Token: "t", Pin: formGoodPin}

	nm, _ := m.Update(pullSucceededMsg{instance: "agentA"})
	cm, ok := nm.(clientModel)
	if !ok {
		t.Fatalf("want clientModel, got %T", nm)
	}
	if cm.instance != "agentA" {
		t.Fatalf("first pull must auto-select the effective slot, got %q", cm.instance)
	}
	if !cm.finish || cm.overlay == nil {
		t.Fatalf("finish screen must come up, finish=%v overlay=%v", cm.finish, cm.overlay)
	}
	ov := cm.overlay.View().Content
	if !strings.Contains(ov, `"--instance", "agentA"`) || !strings.Contains(ov, "instances/agentA/") {
		t.Fatalf("finish screen must route the agent to the effective slot:\n%s", ov)
	}
}
