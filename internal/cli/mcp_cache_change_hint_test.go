package cli

// 多机客户端配置变更提示的端到端测试:桥换上新快照(共享缓存文件哈希热加载或
// 到期保鲜)时比较新旧快照,把变更提示附在 agent 下一次工具结果开头。
// 这里只用既有的换快照途径触发(改写共享缓存 / 到期保鲜),不依赖后台轮询
// 循环。

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"ssh-manager-mcp/internal/buildinfo"
	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/models"
	"ssh-manager-mcp/internal/policy"
	"ssh-manager-mcp/internal/store"
)

// changeHintEnv 在真实加密缓存目录上组装完整路径:初次拉取 →
// CacheReloader 哈希热加载回调 → MCP 工具面。maxAge 传 0 关闭到期自动
// 拉取,让测试完全掌握换快照时机。
type changeHintEnv struct {
	fx      *cacheFwdFixture
	dir     string
	sess    *mcp.ClientSession
	pullNow func()
}

func newChangeHintEnv(t *testing.T, maxAge time.Duration) *changeHintEnv {
	t.Helper()
	fx := newCacheFwdFixture(t)
	dir := t.TempDir()
	t.Setenv("SSHMGR_CACHE_DIR", dir)
	mem := &store.MemKeyProvider{}
	dek, err := store.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Set(dek); err != nil {
		t.Fatal(err)
	}
	prevDek := clientops.DekProvider
	clientops.DekProvider = func(string) store.KeyProvider { return mem }
	t.Cleanup(func() { clientops.DekProvider = prevDek })

	if err := clientops.WriteCacheCred(&fx.cred); err != nil {
		t.Fatalf("写入设备码凭据:%v", err)
	}
	if _, err := clientops.DoPull(fx.cred.URL, fx.cred.Token, fx.cred.Pin, clientops.PullOpts{}); err != nil {
		t.Fatalf("初次拉取:%v", err)
	}
	rel := clientops.NewCacheReloaderFor("", maxAge)
	snap, err := clientops.LoadCacheSnapshot()
	if err != nil {
		t.Fatalf("读取缓存快照:%v", err)
	}
	sess := fx.newBrokerSnap(t, snap, rel.Check, fx.cred)
	env := &changeHintEnv{fx: fx, dir: dir, sess: sess}
	env.pullNow = func() {
		if _, err := clientops.DoPull(fx.cred.URL, fx.cred.Token, fx.cred.Pin, clientops.PullOpts{}); err != nil {
			t.Fatalf("再次拉取:%v", err)
		}
	}
	return env
}

func (env *changeHintEnv) revision(t *testing.T) int64 {
	t.Helper()
	rev, err := env.fx.serve.SnapshotRevision()
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

// TestCacheChangeHintE2E_ServerAdded: 权威端新增服务器并授权给当前 profile
// 后,共享缓存被换新(等价于同机另一个桥更新缓存),下一次任意工具调用
// 的结果开头出现一条带新旧修订号的变更提示;同一会话第二次调用不再出现。
func TestCacheChangeHintE2E_ServerAdded(t *testing.T) {
	env := newChangeHintEnv(t, 0)

	first := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	if len(first) != 1 || !strings.HasPrefix(first[0], `{"servers":`) {
		t.Fatalf("启动后的第一次工具调用不应有提示,实际为 %#v", first)
	}

	before := env.revision(t)
	newID, err := env.fx.serve.AddServer(&models.Server{
		Name: "new-box", Host: "192.0.2.50", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.GrantServers(env.fx.profID, []string{newID}); err != nil {
		t.Fatal(err)
	}
	after := env.revision(t)
	env.pullNow()

	second := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	want := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增服务器 new-box", before, after)
	if len(second) != 2 || second[0] != want || !strings.HasPrefix(second[1], `{"servers":`) {
		t.Fatalf("新增服务器后的工具结果 = %#v,应为提示 %q + 原结果", second, want)
	}
	for _, leak := range []string{"192.0.2.50", "127.0.0.1", "pw"} {
		if strings.Contains(second[0], leak) {
			t.Fatalf("变更提示不应包含地址或凭据:%q", second[0])
		}
	}

	third := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	if len(third) != 1 || !strings.HasPrefix(third[0], `{"servers":`) {
		t.Fatalf("同一会话同样的提示只应出现一次,实际为 %#v", third)
	}
}

// TestCacheChangeHintE2E_RevokedGrantReportedAsRemoved: 撤销授权在客户端
// 视角就是删除;重新授权则显示为新增。
func TestCacheChangeHintE2E_RevokedGrantReportedAsRemoved(t *testing.T) {
	env := newChangeHintEnv(t, 0)

	before := env.revision(t)
	if _, _, err := env.fx.serve.SyncServers(env.fx.profID, nil); err != nil {
		t.Fatal(err)
	}
	after := env.revision(t)
	env.pullNow()

	first := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	want := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：删除服务器 target", before, after)
	if len(first) != 2 || first[0] != want || !strings.HasPrefix(first[1], `{"servers":`) {
		t.Fatalf("撤销授权后的工具结果 = %#v,应为提示 %q + 原结果", first, want)
	}

	before = env.revision(t)
	if err := env.fx.serve.GrantServers(env.fx.profID, []string{env.fx.targetID}); err != nil {
		t.Fatal(err)
	}
	after = env.revision(t)
	env.pullNow()

	second := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	wantBack := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增服务器 target", before, after)
	if len(second) != 2 || second[0] != wantBack || !strings.HasPrefix(second[1], `{"servers":`) {
		t.Fatalf("重新授权后的工具结果 = %#v,应为提示 %q + 原结果", second, wantBack)
	}
}

// TestCacheChangeHintE2E_ServerDeleted: 权威端删除服务器后,下一次工具调用
// 提示该服务器已删除。
func TestCacheChangeHintE2E_ServerDeleted(t *testing.T) {
	env := newChangeHintEnv(t, 0)

	before := env.revision(t)
	if _, _, err := env.fx.serve.SyncServers(env.fx.profID, nil); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.DeleteServer(env.fx.targetID); err != nil {
		t.Fatal(err)
	}
	after := env.revision(t)
	env.pullNow()

	texts := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	want := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：删除服务器 target", before, after)
	if len(texts) != 2 || texts[0] != want || !strings.HasPrefix(texts[1], `{"servers":`) {
		t.Fatalf("删除服务器后的工具结果 = %#v,应为提示 %q + 原结果", texts, want)
	}
}

// TestCacheChangeHintE2E_PolicyChanged: 命令策略变化(新增自定义规则、停用
// 默认规则)都提示「命令策略已变化」;只改规则时间戳不提示。
func TestCacheChangeHintE2E_PolicyChanged(t *testing.T) {
	env := newChangeHintEnv(t, 0)

	before := env.revision(t)
	if err := env.fx.serve.AddPolicyRule(policy.CustomRule{
		ID:          "no-rm",
		Commands:    []string{"rm"},
		Disposition: policy.DispositionBlock,
		Explanation: "删除文件风险高",
		Alternative: "用 trash-put",
	}); err != nil {
		t.Fatal(err)
	}
	after := env.revision(t)
	env.pullNow()

	first := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	want := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：命令策略已变化，此前被拒绝或允许的命令可能不同", before, after)
	if len(first) != 2 || first[0] != want || !strings.HasPrefix(first[1], `{"servers":`) {
		t.Fatalf("命令策略变化后的工具结果 = %#v,应为提示 %q + 原结果", first, want)
	}

	before = env.revision(t)
	if _, err := env.fx.serve.DisableDefaultRule(policy.DefaultRuleIDs()[0]); err != nil {
		t.Fatal(err)
	}
	after = env.revision(t)
	env.pullNow()

	second := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	wantDisable := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：命令策略已变化，此前被拒绝或允许的命令可能不同", before, after)
	if len(second) != 2 || second[0] != wantDisable || !strings.HasPrefix(second[1], `{"servers":`) {
		t.Fatalf("停用默认规则后的工具结果 = %#v,应为提示 %q + 原结果", second, wantDisable)
	}
}

// TestCacheChangeHintE2E_SecretChanges: 多机客户端在新机密项下发、换值、
// 可写入服务器列表变化、真实删除与可写入服务器移出授权后消失时都收到
// 只含名字的提示;同一会话每条提示只出现一次,范围外的机密项变化不提示,
// 提示文本不含任何值。
func TestCacheChangeHintE2E_SecretChanges(t *testing.T) {
	const (
		valueOne   = "change-hint-secret-value-one"
		valueTwo   = "change-hint-secret-value-two"
		valueThree = "change-hint-secret-value-three"
		outsideVal = "change-hint-secret-value-outside"
	)
	env := newChangeHintEnv(t, 0)
	if texts := callToolTexts(t, env.sess, "list_servers", map[string]any{}); len(texts) != 1 {
		t.Fatalf("启动后的第一次工具调用不应有提示,实际为 %#v", texts)
	}

	step := func(name string) string {
		texts := callToolTexts(t, env.sess, "list_servers", map[string]any{})
		if len(texts) != 2 || !strings.HasPrefix(texts[1], `{"servers":`) {
			t.Fatalf("「%s」后的工具结果 = %#v,应为一条提示 + 原结果", name, texts)
		}
		for _, value := range []string{valueOne, valueTwo, valueThree, outsideVal} {
			if strings.Contains(texts[0], value) {
				t.Fatalf("「%s」的变更提示包含机密项值:%q", name, texts[0])
			}
		}
		again := callToolTexts(t, env.sess, "list_servers", map[string]any{})
		if len(again) != 1 || !strings.HasPrefix(again[0], `{"servers":`) {
			t.Fatalf("「%s」的提示在同一会话只应出现一次,第二次结果为 %#v", name, again)
		}
		return texts[0]
	}

	before := env.revision(t)
	if err := env.fx.serve.AddSecret("DB_PASSWORD", "数据库口令", []byte(valueOne)); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.AllowSecrets([]string{"DB_PASSWORD"}, []string{"target"}); err != nil {
		t.Fatal(err)
	}
	after := env.revision(t)
	env.pullNow()
	want := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增机密项 DB_PASSWORD", before, after)
	if got := step("新增机密项"); got != want {
		t.Fatalf("新增机密项提示 = %q,应为 %q", got, want)
	}

	before = env.revision(t)
	if err := env.fx.serve.SetSecretValue("DB_PASSWORD", []byte(valueTwo)); err != nil {
		t.Fatal(err)
	}
	after = env.revision(t)
	env.pullNow()
	want = fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：机密项 DB_PASSWORD 的值已更改，用到它的文件需要重新部署", before, after)
	if got := step("更换机密项值"); got != want {
		t.Fatalf("换值提示 = %q,应为 %q", got, want)
	}

	before = env.revision(t)
	extraID, err := env.fx.serve.AddServer(&models.Server{
		Name: "extra", Host: "192.0.2.61", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.GrantServers(env.fx.profID, []string{extraID}); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.AllowSecrets([]string{"DB_PASSWORD"}, []string{"extra"}); err != nil {
		t.Fatal(err)
	}
	after = env.revision(t)
	env.pullNow()
	want = fmt.Sprintf(
		"权威端配置已更新（修订号 %d → %d）：新增服务器 extra；机密项 DB_PASSWORD 的可写入服务器列表已变化：新增 extra",
		before, after,
	)
	if got := step("可写入服务器列表变化"); got != want {
		t.Fatalf("可写入服务器列表变化提示 = %q,应为 %q", got, want)
	}

	before = env.revision(t)
	if err := env.fx.serve.DeleteSecret("DB_PASSWORD"); err != nil {
		t.Fatal(err)
	}
	after = env.revision(t)
	env.pullNow()
	want = fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：删除机密项 DB_PASSWORD，引用它的模板将被拒绝", before, after)
	if got := step("删除机密项"); got != want {
		t.Fatalf("删除机密项提示 = %q,应为 %q", got, want)
	}

	if _, err := env.fx.serve.AddServer(&models.Server{
		Name: "outside", Host: "192.0.2.62", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	}); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.AddSecret("OUTSIDE_TOKEN", "范围外机密项", []byte(outsideVal)); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.AllowSecrets([]string{"OUTSIDE_TOKEN"}, []string{"outside"}); err != nil {
		t.Fatal(err)
	}
	env.pullNow()
	if texts := callToolTexts(t, env.sess, "list_servers", map[string]any{}); len(texts) != 1 ||
		!strings.HasPrefix(texts[0], `{"servers":`) {
		t.Fatalf("与本客户端无关的机密项变化不应产生提示,实际为 %#v", texts)
	}

	before = env.revision(t)
	if err := env.fx.serve.AddSecret("DB_PASSWORD", "重新下发", []byte(valueThree)); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.AllowSecrets([]string{"DB_PASSWORD"}, []string{"target"}); err != nil {
		t.Fatal(err)
	}
	after = env.revision(t)
	env.pullNow()
	want = fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增机密项 DB_PASSWORD", before, after)
	if got := step("重新下发机密项"); got != want {
		t.Fatalf("重新下发机密项提示 = %q,应为 %q", got, want)
	}

	before = env.revision(t)
	if err := env.fx.serve.DisallowSecrets([]string{"DB_PASSWORD"}, []string{"target"}); err != nil {
		t.Fatal(err)
	}
	after = env.revision(t)
	env.pullNow()
	want = fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：删除机密项 DB_PASSWORD，引用它的模板将被拒绝", before, after)
	if got := step("可写入服务器移出授权后消失"); got != want {
		t.Fatalf("可写入服务器移出授权后消失提示 = %q,应与删除提示 %q 相同", got, want)
	}
}

// TestCacheChangeHintE2E_SecretDisappearsWhenServerLeavesProfile: 从客户端
// profile 撤掉某台服务器后，只因这台服务器才下发的机密项从下一次快照消失；
// 机密项分句与真实删除相同，同一会话只出现一次，且提示不含值。
func TestCacheChangeHintE2E_SecretDisappearsWhenServerLeavesProfile(t *testing.T) {
	const secretValue = "profile-only-secret-value"
	env := newChangeHintEnv(t, 0)
	if texts := callToolTexts(t, env.sess, "list_servers", map[string]any{}); len(texts) != 1 {
		t.Fatalf("启动后的第一次工具调用不应有提示,实际为 %#v", texts)
	}

	serverID, err := env.fx.serve.AddServer(&models.Server{
		Name: "profile-only", Host: "192.0.2.63", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.GrantServers(env.fx.profID, []string{serverID}); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.AddSecret("PROFILE_ONLY_TOKEN", "只因这台服务器下发", []byte(secretValue)); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.AllowSecrets([]string{"PROFILE_ONLY_TOKEN"}, []string{"profile-only"}); err != nil {
		t.Fatal(err)
	}
	env.pullNow()
	if texts := callToolTexts(t, env.sess, "list_servers", map[string]any{}); len(texts) != 2 ||
		!strings.Contains(texts[0], "新增机密项 PROFILE_ONLY_TOKEN") ||
		!strings.HasPrefix(texts[1], `{"servers":`) {
		t.Fatalf("建立基线后的工具结果 = %#v,应先提示新增机密项再返回服务器列表", texts)
	}

	before := env.revision(t)
	if _, _, err := env.fx.serve.SyncServers(env.fx.profID, []string{env.fx.targetID}); err != nil {
		t.Fatal(err)
	}
	after := env.revision(t)
	env.pullNow()

	want := fmt.Sprintf(
		"权威端配置已更新（修订号 %d → %d）：删除服务器 profile-only；删除机密项 PROFILE_ONLY_TOKEN，引用它的模板将被拒绝",
		before, after,
	)
	texts := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	if len(texts) != 2 || texts[0] != want || !strings.HasPrefix(texts[1], `{"servers":`) {
		t.Fatalf("服务器移出 profile 后的工具结果 = %#v,应为提示 %q + 原结果", texts, want)
	}
	if strings.Contains(texts[0], secretValue) {
		t.Fatalf("机密项消失提示不应包含值:%q", texts[0])
	}

	again := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	if len(again) != 1 || !strings.HasPrefix(again[0], `{"servers":`) {
		t.Fatalf("机密项消失提示在同一会话只应出现一次,第二次结果为 %#v", again)
	}
}

// TestCacheChangeHintE2E_NoVisibleDifferenceNoHint: 服务器改名、改连接信息、
// 改 shell 类型、只改其他 profile、规则只改时间戳,这些都不是本票的比较项;
// 快照换了但可见内容没变时不产生提示。
func TestCacheChangeHintE2E_NoVisibleDifferenceNoHint(t *testing.T) {
	env := newChangeHintEnv(t, 0)

	srv, err := env.fx.serve.GetServer(env.fx.targetID)
	if err != nil {
		t.Fatal(err)
	}
	srv.Name = "renamed"
	srv.Host = "192.0.2.99"
	srv.Port = 2222
	srv.ShellType = models.ShellPowerShell
	if err := env.fx.serve.UpdateServer(srv); err != nil {
		t.Fatal(err)
	}
	env.pullNow()
	if texts := callToolTexts(t, env.sess, "list_servers", map[string]any{}); len(texts) != 1 {
		t.Fatalf("服务器改名、改连接信息、改 shell 类型不应产生提示,实际为 %#v", texts)
	}

	otherProfile, err := env.fx.serve.AddProfile("team-b")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := env.fx.serve.AddServer(&models.Server{
		Name: "other-box", Host: "192.0.2.60", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.GrantServers(otherProfile, []string{otherID}); err != nil {
		t.Fatal(err)
	}
	env.pullNow()
	if texts := callToolTexts(t, env.sess, "list_servers", map[string]any{}); len(texts) != 1 {
		t.Fatalf("只改其他 profile 不应产生提示,实际为 %#v", texts)
	}

	if err := env.fx.serve.AddPolicyRule(policy.CustomRule{
		ID:          "no-rm",
		Commands:    []string{"rm"},
		Disposition: policy.DispositionBlock,
		Explanation: "删除文件风险高",
		Alternative: "用 trash-put",
	}); err != nil {
		t.Fatal(err)
	}
	env.pullNow()
	// 先把「新增规则」的提示消费掉,接下来的比较只看时间戳差异。
	callToolTexts(t, env.sess, "list_servers", map[string]any{})
	if _, err := env.fx.serve.DeletePolicyRule("no-rm"); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.AddPolicyRule(policy.CustomRule{
		ID:          "no-rm",
		Commands:    []string{"rm"},
		Disposition: policy.DispositionBlock,
		Explanation: "删除文件风险高",
		Alternative: "用 trash-put",
	}); err != nil {
		t.Fatal(err)
	}
	env.pullNow()
	if texts := callToolTexts(t, env.sess, "list_servers", map[string]any{}); len(texts) != 1 {
		t.Fatalf("规则语义未变、只有时间戳变化不应产生提示,实际为 %#v", texts)
	}
}

// TestCacheChangeHintE2E_CombinedChangesSingleHint: 一次换快照同时发生新增、
// 删除与命令策略变化时,只产生一条提示,三个分句按固定顺序拼在一起。
func TestCacheChangeHintE2E_CombinedChangesSingleHint(t *testing.T) {
	env := newChangeHintEnv(t, 0)

	before := env.revision(t)
	newID, err := env.fx.serve.AddServer(&models.Server{
		Name: "new-box", Host: "192.0.2.51", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.GrantServers(env.fx.profID, []string{newID}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.fx.serve.SyncServers(env.fx.profID, []string{newID}); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.AddPolicyRule(policy.CustomRule{
		ID:          "no-rm",
		Commands:    []string{"rm"},
		Disposition: policy.DispositionBlock,
		Explanation: "删除文件风险高",
	}); err != nil {
		t.Fatal(err)
	}
	after := env.revision(t)
	env.pullNow()

	texts := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	want := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增服务器 new-box；删除服务器 target；命令策略已变化，此前被拒绝或允许的命令可能不同", before, after)
	if len(texts) != 2 || texts[0] != want || !strings.HasPrefix(texts[1], `{"servers":`) {
		t.Fatalf("一次换快照应只有一条合并提示,实际为 %#v,应为 %q", texts, want)
	}
}

// TestCacheChangeHintE2E_DeleteThenAddSameServerTwoHints: 先删后加同一台
// 服务器是两次换快照、两条提示。提示带新旧修订号,所以两条文本不同,同一
// 会话里各出现一次,不会被去重吞掉。
func TestCacheChangeHintE2E_DeleteThenAddSameServerTwoHints(t *testing.T) {
	env := newChangeHintEnv(t, 0)

	firstBefore := env.revision(t)
	if _, _, err := env.fx.serve.SyncServers(env.fx.profID, nil); err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.DeleteServer(env.fx.targetID); err != nil {
		t.Fatal(err)
	}
	firstAfter := env.revision(t)
	env.pullNow()

	// 第一步:删除后的下一次工具调用看到「删除」提示。
	wantRemove := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：删除服务器 target", firstBefore, firstAfter)
	texts := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	if len(texts) != 2 || texts[0] != wantRemove || !strings.HasPrefix(texts[1], `{"servers":`) {
		t.Fatalf("删除后的第一条提示 = %#v,应为 %q + 原结果", texts, wantRemove)
	}

	// 第二步:加回同名服务器(编号已不同),新的修订号对构成另一条提示。
	secondBefore := env.revision(t)
	sameNameID, err := env.fx.serve.AddServer(&models.Server{
		Name: "target", Host: "192.0.2.52", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.GrantServers(env.fx.profID, []string{sameNameID}); err != nil {
		t.Fatal(err)
	}
	secondAfter := env.revision(t)
	env.pullNow()

	wantAdd := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增服务器 target", secondBefore, secondAfter)
	texts = callToolTexts(t, env.sess, "list_servers", map[string]any{})
	if len(texts) != 2 || texts[0] != wantAdd || !strings.HasPrefix(texts[1], `{"servers":`) {
		t.Fatalf("加回后的第二条提示 = %#v,应为 %q + 原结果", texts, wantAdd)
	}
	again := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	if len(again) != 1 || !strings.HasPrefix(again[0], `{"servers":`) {
		t.Fatalf("两条提示在同一会话里都只应出现一次,实际为 %#v", again)
	}
}

// snapshotVariant 复制基准快照并施加改动,供自定义热加载回调按次返回
// 新快照(与后台轮询循环经由同一换快照入口安装的效果等价)。
func snapshotVariant(t *testing.T, base *store.Snapshot, revision int64, mutate func(*store.Snapshot)) *store.Snapshot {
	t.Helper()
	cp := *base
	cp.Credentials = append([]store.SnapshotCredential{}, base.Credentials...)
	cp.Servers = append([]store.SnapshotServer{}, base.Servers...)
	cp.Profiles = append([]store.SnapshotProfile{}, base.Profiles...)
	cp.Grants = append([]store.SnapshotGrant{}, base.Grants...)
	cp.Projects = append([]store.SnapshotProject{}, base.Projects...)
	cp.SnapshotRevision = revision
	mutate(&cp)
	return &cp
}

// TestCacheChangeHintE2E_MultiplePendingHintsKeptInOrder: 桥在进程内存里保存
// 启动以来产生的每条变更提示。快照在工具调用过程中被换新(等价于
// 后台轮询在任意时刻安装新快照)时,提示不会因为下一条到来而丢失,下一次
// 调用按发生顺序全部出现。
func TestCacheChangeHintE2E_MultiplePendingHintsKeptInOrder(t *testing.T) {
	fx := newCacheFwdFixture(t)
	base, err := fx.serve.ExportSnapshotForProfile(fx.profID, false, "")
	if err != nil {
		t.Fatal(err)
	}
	baseRevision := base.SnapshotRevision
	extra := snapshotVariant(t, base, baseRevision+1, func(s *store.Snapshot) {
		s.Servers = append(s.Servers, store.SnapshotServer{ID: "srv-extra", Name: "extra"})
		s.Grants = append(s.Grants, store.SnapshotGrant{ProfileID: fx.profID, ServerID: "srv-extra"})
	})
	withoutTarget := snapshotVariant(t, extra, baseRevision+2, func(s *store.Snapshot) {
		keptServers := s.Servers[:0]
		for _, sv := range s.Servers {
			if sv.ID != fx.targetID {
				keptServers = append(keptServers, sv)
			}
		}
		s.Servers = keptServers
		keptGrants := s.Grants[:0]
		for _, g := range s.Grants {
			if g.ServerID != fx.targetID {
				keptGrants = append(keptGrants, g)
			}
		}
		s.Grants = keptGrants
	})
	// 第一次 Current 报告未变化,同一次调用稍后的 Current 才交出第一份新
	// 快照:模拟快照在工具调用进行中被后台换新。
	pending := []*store.Snapshot{nil, extra, withoutTarget}
	installed := 0
	reload := func() (*store.Snapshot, bool, error) {
		if installed >= len(pending) {
			return nil, false, nil
		}
		next := pending[installed]
		installed++
		if next == nil {
			return nil, false, nil
		}
		return next, true, nil
	}

	sess := fx.newBroker(t, reload, fx.cred)
	if texts := callToolTexts(t, sess, "list_servers", map[string]any{}); len(texts) != 1 {
		t.Fatalf("这一步还没有可见变更,不应有提示,实际为 %#v", texts)
	}

	wantAdd := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增服务器 extra", baseRevision, baseRevision+1)
	wantRemove := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：删除服务器 target", baseRevision+1, baseRevision+2)
	texts := callToolTexts(t, sess, "list_servers", map[string]any{})
	if len(texts) != 3 || texts[0] != wantAdd || texts[1] != wantRemove || !strings.HasPrefix(texts[2], `{"servers":`) {
		t.Fatalf("两条先后到来的提示都应按顺序出现,实际为 %#v,应为 %q 与 %q", texts, wantAdd, wantRemove)
	}
	again := callToolTexts(t, sess, "list_servers", map[string]any{})
	if len(again) != 1 || !strings.HasPrefix(again[0], `{"servers":`) {
		t.Fatalf("两条提示在同一会话里都只应出现一次,实际为 %#v", again)
	}
}

// TestCacheChangeHintE2E_OrderAndFailure: 变更提示排在版本提示之前;工具
// 执行失败时提示照样附在最前。
func TestCacheChangeHintE2E_OrderAndFailure(t *testing.T) {
	previousVersion := buildinfo.Version
	buildinfo.Version = "1.1.0"
	t.Cleanup(func() { buildinfo.Version = previousVersion })
	t.Setenv("SSHMGR_TEST_VERSION", "1.0.0")
	env := newChangeHintEnv(t, 0)

	before := env.revision(t)
	newID, err := env.fx.serve.AddServer(&models.Server{
		Name: "new-box", Host: "192.0.2.53", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.GrantServers(env.fx.profID, []string{newID}); err != nil {
		t.Fatal(err)
	}
	after := env.revision(t)
	env.pullNow()

	wantChange := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增服务器 new-box", before, after)
	wantVersion := "客户端 1.0.0 落后于权威端 1.1.0，运行 `sshmgr update` 升级"
	texts, isError, _ := callToolResultTexts(t, env.sess, "exec_command", map[string]any{
		"server_id": "missing", "command": "true",
	})
	if !isError {
		t.Fatal("不存在的服务器应保持失败的工具结果")
	}
	if len(texts) != 3 || texts[0] != wantChange || texts[1] != wantVersion || !strings.Contains(texts[2], "server is not in your profile") {
		t.Fatalf("失败的工具结果应先附带变更提示再附带版本提示,实际为 %#v", texts)
	}

	again, isError, _ := callToolResultTexts(t, env.sess, "exec_command", map[string]any{
		"server_id": "missing", "command": "true",
	})
	if !isError || len(again) != 1 || !strings.Contains(again[0], "server is not in your profile") {
		t.Fatalf("同一会话重复调用不应再出现提示,实际为 %#v", again)
	}
}

// TestCacheChangeHintE2E_ExpiryRefreshPath: 到期保鲜也是换快照的途径——
// 缓存到期后先查修订号,修订号变了才全量拉取;文件被换新后,下一次工具
// 调用换上新快照并附带变更提示。
func TestCacheChangeHintE2E_ExpiryRefreshPath(t *testing.T) {
	env := newChangeHintEnv(t, time.Minute)

	before := env.revision(t)
	newID, err := env.fx.serve.AddServer(&models.Server{
		Name: "new-box", Host: "192.0.2.54", Port: 22, User: "u", AuthMethod: models.AuthPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := env.fx.serve.GrantServers(env.fx.profID, []string{newID}); err != nil {
		t.Fatal(err)
	}
	after := env.revision(t)
	ageRevisionCache(t, env.dir, time.Minute)

	// 第一次调用触发到期保鲜:修订号变化,全量拉取改写缓存文件,但这一步
	// 故意仍在旧快照上完成,不产生提示。
	texts := callToolTexts(t, env.sess, "list_servers", map[string]any{})
	if len(texts) != 1 || !strings.HasPrefix(texts[0], `{"servers":`) {
		t.Fatalf("到期保鲜中的这次调用不应有提示,实际为 %#v", texts)
	}
	if got := env.fx.snapshotCount(); got < 2 {
		t.Fatalf("到期保鲜应再次全量拉取,请求次数 = %d", got)
	}

	want := fmt.Sprintf("权威端配置已更新（修订号 %d → %d）：新增服务器 new-box", before, after)
	texts = callToolTexts(t, env.sess, "list_servers", map[string]any{})
	if len(texts) != 2 || texts[0] != want || !strings.HasPrefix(texts[1], `{"servers":`) {
		t.Fatalf("到期保鲜换上新快照后的工具结果 = %#v,应为提示 %q + 原结果", texts, want)
	}
}
