package models

import "testing"

// TestServerNeedsAttentionTruthTable 是「需要注意」规则的真值表：未挂凭据、
// 未设角色、带待补口令标记，三者任一成立就标记；全部齐备则不标记。
func TestServerNeedsAttentionTruthTable(t *testing.T) {
	cases := []struct {
		name         string
		credentialID string
		role         string
		tags         []string
		want         bool
	}{
		{"完整条目不标记", "cred-1", "数据库主库", nil, false},
		{"未挂凭据标记", "", "数据库主库", nil, true},
		{"未设角色标记", "cred-1", "", nil, true},
		{"带待补口令标记", "cred-1", "数据库主库", []string{"gpu", "needs-passphrase"}, true},
		{"其他标签不标记", "cred-1", "数据库主库", []string{"gpu"}, false},
	}
	for _, tc := range cases {
		if got := ServerNeedsAttention(tc.credentialID, tc.role, tc.tags); got != tc.want {
			t.Fatalf("%s：ServerNeedsAttention(%q, %q, %v) = %v，应为 %v", tc.name, tc.credentialID, tc.role, tc.tags, got, tc.want)
		}
	}
}
