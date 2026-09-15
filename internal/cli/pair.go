package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
)

// newPairCmd —— `sshmgr pair` 一条龙(T7):URL 解析(--url 或 LAN 发现
// 选择)后交给 clientops.RunPair(流程冻结点都在那边)。本层只做:instance 必填、
// pin 格式前置校验、discovery 结果挑选、SSHMGR_PAIR_ASSUME_SAS env 透传。
func newPairCmd() *cobra.Command {
	var (
		url, pin, profileHint, writeMCP, instance string
		allowTOFU, force                          bool
	)
	c := &cobra.Command{
		Use:   "pair",
		Short: "Pair this machine with a serve broker (SAS) and mint the agent .mcp.json",
		Long: "一条龙配对:LAN 发现(或 --url)→ enroll → SAS 三件套(与 broker 审批面逐位比对)→ 轮询批准 → finish 解密凭据 → 先落盘(cache.auth.json / cache.config.json / pair.<name>.mcp.json)→ 首拉。\n" +
			"pin 分级:--pin 或 discovery 自带的 SPKI → TLS 层硬校验;--url 无 --pin 默认拒绝 TOFU(--allow-tofu 显式接受无锚通道,信任由 SAS 人工比对 + 密封信封 SPKI 补上)。\n" +
			"警示:SSHMGR_PAIR_ASSUME_SAS=1 跳过 SAS 终端比对(!! STUB 警示),仅用于无人值守自动化;pair.<name>.mcp.json 含真值 project token(0600),勿提交/外发。",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkInstanceFlag(instance); err != nil {
				return err
			}
			if instance == "" {
				return errors.New("--instance is required: the device name to enroll (becomes the local instances/<name> slot)")
			}
			// F7 同款前置:pin 形似而不合法是硬错误,绝不静默落进 TOFU 分支。
			if raw := strings.TrimSpace(pin); raw != "" {
				if _, ok := mcpserver.ParsePin(raw); !ok {
					return fmt.Errorf("--pin is not a valid sha256:<64hex> fingerprint: %q", raw)
				}
			}
			pairURL := strings.TrimSpace(url)
			pairPin := strings.TrimSpace(pin)
			if pairURL == "" {
				d, err := pickDiscovered(cmd)
				if err != nil {
					return err
				}
				pairURL = fmt.Sprintf("https://%s:%d", d.Addr, d.TCPPort)
				if pairPin == "" {
					pairPin = d.SPKI // discovery 的 SPKI 升格为 pin(TLS 硬校验)
				}
			}
			return clientops.RunPair(clientops.PairOpts{
				URL:          pairURL,
				Pin:          pairPin,
				AllowTOFU:    allowTOFU,
				AssumeSAS:    os.Getenv("SSHMGR_PAIR_ASSUME_SAS") == "1",
				ProfileHint:  strings.TrimSpace(profileHint),
				WriteMCPPath: writeMCP,
				Instance:     instance,
				Force:        force,
				Stdin:        cmd.InOrStdin(),
				Stdout:       cmd.OutOrStdout(),
				Stderr:       cmd.ErrOrStderr(),
			})
		},
	}
	f := c.Flags()
	f.StringVar(&url, "url", "", "serve broker URL (https://host:7878); omit to discover on the LAN (udp/7878 broadcast)")
	f.StringVar(&pin, "pin", "", "server SPKI fingerprint sha256:... (hard TLS check; discovery supplies one automatically)")
	f.BoolVar(&allowTOFU, "allow-tofu", false, "accept an unanchored channel when no --pin (insecure; default is to refuse — trust falls back to the SAS comparison + sealed envelope pin)")
	f.StringVar(&profileHint, "profile-hint", "", "optional hint shown on the broker's approval surface")
	f.StringVar(&writeMCP, "write-mcp", "", "also copy the pair.<name>.mcp.json artifact to this path (0600)")
	f.StringVar(&instance, "instance", "", "device name to enroll (= local instances/<name> slot) (required)")
	f.BoolVar(&force, "force", false, "re-enroll over an existing instance credential (nothing is deleted up front — any failure leaves the old slot untouched; on success the fresh material atomically replaces it)")
	return c
}

// pickDiscovered runs the LAN discovery sweep (production target set =
// per-interface directed broadcasts) and resolves the target: one result →
// straight in; several → a numbered picker (name @ addr, spki 前 16 字符);
// none → guidance instead of a bare failure.
func pickDiscovered(cmd *cobra.Command) (clientops.Discovered, error) {
	targets, err := clientops.NonLoopbackIPv4Broadcasts()
	if err != nil {
		return clientops.Discovered{}, fmt.Errorf("discovery: %w", err)
	}
	if len(targets) == 0 {
		return clientops.Discovered{}, errors.New("discovery: no broadcast-capable IPv4 interface — pass --url explicitly")
	}
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "discovering brokers on the LAN (udp/7878)...")
	found, err := clientops.Discover(targets, 0)
	if err != nil {
		return clientops.Discovered{}, fmt.Errorf("discovery: %w", err)
	}
	if len(found) == 0 {
		return clientops.Discovered{}, errors.New("no broker answered the discovery sweep — is `sshmgr serve` running with discovery on? pass --url <https://host:7878> to pair directly")
	}
	if len(found) == 1 {
		d := found[0]
		fmt.Fprintf(out, "found %s @ %s:%d\n", clientops.StripC0C1(d.Name), d.Addr, d.TCPPort)
		return d, nil
	}
	fmt.Fprintf(out, "found %d brokers:\n", len(found))
	for i, d := range found {
		fmt.Fprintf(out, "  %d. %s @ %s:%d  spki %s…\n", i+1, clientops.StripC0C1(d.Name), d.Addr, d.TCPPort, clipSPKI(d.SPKI))
	}
	fmt.Fprintf(out, "select [1-%d]: ", len(found))
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	n, aerr := strconv.Atoi(strings.TrimSpace(line))
	if aerr != nil || n < 1 || n > len(found) {
		return clientops.Discovered{}, fmt.Errorf("invalid selection %q", strings.TrimSpace(line))
	}
	return found[n-1], nil
}

// clipSPKI renders the first 16 characters of a "sha256:<64hex>" pin (the
// frozen picker display shape — enough to tell two brokers apart).
func clipSPKI(s string) string {
	if len(s) > 16 {
		return s[:16]
	}
	return s
}
