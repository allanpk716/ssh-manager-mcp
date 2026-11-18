// seedpairing 给已经启动的权威端浏览器测试库补一条待批准配对请求。
// 权威端启动时会清空启动前遗留的待批准行，因此这个工具在 serve 就绪后运行。
package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ssh-manager-mcp/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "用法：seedpairing <临时库目录>")
		os.Exit(2)
	}
	if err := seed(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func seed(dir string) error {
	mode := strings.TrimSpace(os.Getenv("SSHMGR_E2E_PAIRING"))
	if mode != "normal" && mode != "foreign" {
		return fmt.Errorf("SSHMGR_E2E_PAIRING 只支持 normal 或 foreign，当前输入 %q", mode)
	}
	key, err := os.ReadFile(filepath.Join(dir, "master.key.plain"))
	if err != nil {
		return err
	}
	st, err := store.Open(filepath.Join(dir, "store.db"), key)
	if err != nil {
		return err
	}
	defer st.Close()

	name := "端口映射客户端"
	targetURL := "https://172.18.200.47:18178"
	shortCode := "246810"
	if mode == "foreign" {
		name = "陌生地址客户端"
		targetURL = "https://203.0.113.9:7878"
		shortCode = "135790"
	} else if err := st.SetSetting("serve.public_addresses", "172.18.200.47:18178"); err != nil {
		return err
	}
	id := sha256.Sum256([]byte("browser-pairing-" + mode))
	row := &store.PendingPairing{
		ID: id[:], Name: name, TargetURL: targetURL, SAS: shortCode,
		ProfileHint: "运维", SourceIP: "192.0.2.40",
		ClientPub: make([]byte, 32), Cnonce: make([]byte, 16),
		ServerPub: make([]byte, 32), Snonce: make([]byte, 16), Sig: make([]byte, 64),
		State:          "pending",
		EnrollDeadline: time.Now().Add(9 * time.Minute).Unix(),
	}
	return st.AddPendingPairing(row, 0, 0)
}
