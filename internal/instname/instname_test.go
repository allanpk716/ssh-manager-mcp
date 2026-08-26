package instname

import "testing"

func TestValid(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"a", true}, {"agentA", true}, {"laptop-agentA", true},
		{"con.foo", false}, {"COM1.x", false}, {"nul.tar.gz", false}, // 首段保留名（实测 MkdirAll 必败）
		{"CON", false}, {"aux", false}, {"lpt9", false},
		{"foo.bar", true}, {"foo", true}, {"A1-b_2.c", true},
		{"foo.", false}, {".foo", false}, {"foo-", false}, {"-foo", false}, // 首尾必须字母数字
		{"a b", false}, {"a/b", false}, {"../x", false}, {"a\\b", false}, // 路径穿越/非法字符
		{"", false},
		{string(make([]byte, 0)) + "0123456789012345678901234567890123456789012345678901234567890123", true}, // 64
		{"01234567890123456789012345678901234567890123456789012345678901234", false},                         // 65
	}
	for _, tc := range cases {
		err := Valid(tc.name)
		if tc.ok && err != nil {
			t.Errorf("Valid(%q) = %v, want nil", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("Valid(%q) = nil, want error", tc.name)
		}
	}
}

func TestFoldASCIIONLY(t *testing.T) {
	if Fold("AgentA") != "agenta" {
		t.Fatalf("Fold = %q", Fold("AgentA"))
	}
	// 非 ASCII 不折叠（与 SQLite lower() 同语义；Kelvin sign 不得折叠成 k）
	if Fold("K\xE2\x84\xAA") != "k\xE2\x84\xAA" && Fold("K") != "k" {
		t.Fatal("Fold must be ASCII-only")
	}
}
