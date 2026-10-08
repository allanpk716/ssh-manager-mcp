package hotswap_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"ssh-manager-mcp/internal/mcpserver/hotswap"
)

// TestBuildEnvParseEnvRoundTrip:领养会话参数经 BuildEnv 序列化、ParseEnv
// 解析后无损往返;上一轮领养遗留的 SSHMGR_HOTSWAP_* 必须被剥干净,
// 就绪文件路径恰一条且为新值。
func TestBuildEnvParseEnvRoundTrip(t *testing.T) {
	base := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/u",
		"SSHMGR_HOTSWAP_STALE=previous-generation",
		"SSHMGR_HOTSWAP_READY=/stale/ready.json",
	}
	sess := hotswap.Session{
		"mcp_protocol": "2025-06-18",
		"host_caps":    "tools,list_changed",
		"note":         "值可含空格与 ünïcode",
		"empty":        "",
	}
	const readyPath = `/tmp/ready 1.json`
	env, err := hotswap.BuildEnv(base, sess, readyPath)
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/u"} {
		if !slices.Contains(env, want) {
			t.Fatalf("base entry %q must survive BuildEnv; got %v", want, env)
		}
	}
	prefixed, readyCount := 0, 0
	for _, kv := range env {
		if !strings.HasPrefix(kv, hotswap.EnvPrefix) {
			continue
		}
		prefixed++
		if strings.HasPrefix(kv, hotswap.EnvReadyPath+"=") {
			readyCount++
			if kv != hotswap.EnvReadyPath+"="+readyPath {
				t.Fatalf("ready var = %q, want path %q", kv, readyPath)
			}
			continue
		}
		if strings.HasPrefix(kv, "SSHMGR_HOTSWAP_STALE=") {
			t.Fatalf("stale adoption var leaked to next generation: %q", kv)
		}
	}
	if prefixed != len(sess)+1 {
		t.Fatalf("prefixed vars = %d, want %d (session + READY)", prefixed, len(sess)+1)
	}
	if readyCount != 1 {
		t.Fatalf("READY entries = %d, want exactly 1", readyCount)
	}

	got, gotReady, adopted := hotswap.ParseEnv(env)
	if !adopted {
		t.Fatal("ParseEnv must report adopted=true for a handover environment")
	}
	if gotReady != readyPath {
		t.Fatalf("ParseEnv readyPath = %q, want %q", gotReady, readyPath)
	}
	if len(got) != len(sess) {
		t.Fatalf("session round trip changed size: got %v, want %v", got, sess)
	}
	for k, v := range sess {
		if got[k] != v {
			t.Fatalf("session[%q] = %q after round trip, want %q", k, got[k], v)
		}
	}
}

// TestBuildEnvDedupsLastWins:同键后值覆盖前值(测试仪表用 Env 注入
// 覆盖父环境同名键时依赖此语义,且不赌 exec 内部去重行为)。
func TestBuildEnvDedupsLastWins(t *testing.T) {
	env, err := hotswap.BuildEnv([]string{"X=1", "X=2", "SSHMGR_HOTSWAP_OLD=1"}, nil, "/r")
	if err != nil {
		t.Fatalf("BuildEnv: %v", err)
	}
	xCount := 0
	for _, kv := range env {
		if kv == "X=1" {
			t.Fatal("X=1 must be overridden by the later X=2")
		}
		if kv == "X=2" {
			xCount++
		}
		if strings.HasPrefix(kv, "SSHMGR_HOTSWAP_OLD=") {
			t.Fatalf("stale adoption var leaked: %q", kv)
		}
	}
	if xCount != 1 {
		t.Fatalf("X entries = %d, want exactly 1 (last value wins)", xCount)
	}
}

// TestBuildEnvRejectsBadKeys:保留键(READY)、非法字符与大小写归一后
// 重复的键必须被拒——它们会破坏「解析侧对称」。
func TestBuildEnvRejectsBadKeys(t *testing.T) {
	cases := []struct {
		name string
		sess hotswap.Session
	}{
		{"reserved key ready", hotswap.Session{"ready": "/x"}},
		{"reserved key READY (case-folded)", hotswap.Session{"READY": "/x"}},
		{"reserved key Ready (case-folded)", hotswap.Session{"Ready": "/x"}},
		{"dash in key", hotswap.Session{"bad-key": "v"}},
		{"dot in key", hotswap.Session{"a.b": "v"}},
		{"space in key", hotswap.Session{"sp ace": "v"}},
		{"empty key", hotswap.Session{"": "v"}},
		{"case-folded duplicate keys", hotswap.Session{"A_b": "1", "a_B": "2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := hotswap.BuildEnv([]string{"PATH=/bin"}, tc.sess, "/ready.json")
			if err == nil {
				t.Fatalf("session %v must be rejected", tc.sess)
			}
			if !errors.Is(err, hotswap.ErrInvalidSessionKey) {
				t.Fatalf("err = %v, want ErrInvalidSessionKey", err)
			}
		})
	}
}

// TestBuildEnvRequiresReadyPath:就绪文件路径为空即协议错误。
func TestBuildEnvRequiresReadyPath(t *testing.T) {
	if _, err := hotswap.BuildEnv([]string{"PATH=/bin"}, nil, ""); err == nil {
		t.Fatal("empty ready path must be rejected")
	}
}

// TestParseEnvNotAdopted:无任何 SSHMGR_HOTSWAP_* 的环境不是领养启动。
func TestParseEnvNotAdopted(t *testing.T) {
	sess, readyPath, adopted := hotswap.ParseEnv([]string{"PATH=/bin", "HOME=/u"})
	if adopted {
		t.Fatal("adopted must be false without SSHMGR_HOTSWAP_* vars")
	}
	if readyPath != "" || len(sess) != 0 {
		t.Fatalf("non-adopted parse must return zero values, got sess=%v ready=%q", sess, readyPath)
	}
}

// TestParseEnvReadyKeyCaseInsensitive:READY 键按大小写不敏感识别
// (Windows 环境变量本就大小写不敏感,序列化侧只发大写)。
func TestParseEnvReadyKeyCaseInsensitive(t *testing.T) {
	sess, readyPath, adopted := hotswap.ParseEnv([]string{
		"PATH=/bin",
		"SSHMGR_HOTSWAP_ready=/lowercase/path",
	})
	if !adopted {
		t.Fatal("adopted must be true when any SSHMGR_HOTSWAP_* var is present")
	}
	if readyPath != "/lowercase/path" {
		t.Fatalf("readyPath = %q, want /lowercase/path", readyPath)
	}
	if len(sess) != 0 {
		t.Fatalf("READY must not leak into session map, got %v", sess)
	}
}
