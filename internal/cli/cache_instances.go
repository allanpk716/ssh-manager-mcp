// Package cli: Plan 46 T2 — `sshmgr cache instances ls/rm`. ls is pure
// discovery (file-existence stat only — NEVER a decrypt); rm is the
// double-root destruction (slot dir + per-instance DEK) behind a typed-name
// confirmation. The default slot is NOT removable here (`sshmgr clear` is the
// whole-machine teardown); the broker-side device code is NOT revocable from
// the client — a successful rm prints the two companion hints (revoke on the
// broker; --write-mcp out-of-slot copies are not cleaned, with the reason).
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/instname"
	"ssh-manager-mcp/internal/paths"
	"ssh-manager-mcp/internal/tui"
)

// cacheInstancesStdinIsTTY gates rm on an interactive stdin — the same
// console check clear's gate uses (GetConsoleMode on Windows, char-device
// stat elsewhere; closes the `rm < NUL` hole a naive stat would have). The
// typed-name confirmation remains the real guard; this is script-proofing.
var cacheInstancesStdinIsTTY = func() bool {
	return tui.IsTerminal(os.Stdin.Fd())
}

func newCacheInstancesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "instances",
		Short: "List or remove named cache instances (slot dir + DEK double-root cleanup)",
	}
	cmd.AddCommand(cacheInstancesLsCmd(), cacheInstancesRmCmd())
	return cmd
}

func cacheInstancesLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List every instance slot: artifact existence (auth/bin/meta/config), DEK presence, cache age",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if clientops.SingleSlotOverrideEnvSet() {
				return errors.New("instance listing needs per-instance path resolution — SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK is set and fully overrides it; unset the env and re-run")
			}
			out := cmd.OutOrStdout()
			names, err := clientops.ListInstances()
			if err != nil {
				return err
			}
			printSlotRow(out, "") // 默认槽一行(无 rm 提示)
			for _, n := range names {
				printSlotRow(out, n)
			}
			orphans, oerr := dekOrphanNames(names)
			if oerr != nil {
				return oerr
			}
			for _, n := range orphans {
				fmt.Fprintf(out, "instance: %s  (槽目录不存在) dek=有  ⚠ DEK 孤儿(槽已不在)  rm: sshmgr cache instances rm %s\n", n, n)
			}
			return nil
		},
	}
}

// slotView is one ls row's data: the five existence bits (stat only — ls
// never decrypts; dek reads "?" when its path cannot be resolved), whether
// the slot DIRECTORY itself exists (real stat — the vacuum/half-state
// discriminator, same judgment as the T3 picker's slotStat.dir), and the
// cache age text.
type slotView struct {
	auth, bin, meta, config, dek bool
	dekUnknown                   bool // DEK 路径解析失败:存在性不可判(渲染 "?",不冒充"缺")
	dirExists                    bool
	age                          string
	pathErr                      string
}

// statSlotView stats one instance's artifacts. DEK presence is a plain stat
// of paths.CacheDekPathFor(instance) — no key material is ever read.
func statSlotView(instance string) slotView {
	v := slotView{age: "-"}
	dir, bin, meta, _, err := clientops.CachePathsFor(instance)
	if err != nil {
		v.pathErr = err.Error()
		return v
	}
	if _, serr := os.Stat(dir); serr == nil {
		v.dirExists = true
	}
	exists := func(p string) bool { _, e := os.Stat(p); return e == nil }
	v.auth = exists(filepath.Join(dir, "cache.auth.json"))
	v.bin = exists(bin)
	v.meta = exists(meta)
	v.config = exists(filepath.Join(dir, "cache.config.json"))
	if dp, derr := paths.CacheDekPathFor(instance); derr == nil {
		v.dek = exists(dp)
	} else {
		v.dekUnknown = true
	}
	if v.bin {
		if fi, serr := os.Stat(bin); serr == nil {
			v.age = time.Since(fi.ModTime()).Round(time.Second).String()
		}
	}
	return v
}

// printSlotRow renders one row: 实例名 / auth·bin·meta·config / dek / age.
// Half-state slots (directory present, any artifact missing) are annotated
// inline; named slots carry the rm hint, the default slot never does.
func printSlotRow(out io.Writer, instance string) {
	label := instance
	if instance == "" {
		label = "(默认实例)"
	}
	v := statSlotView(instance)
	if v.pathErr != "" {
		// 异形目录名等:如实成行,不炸整个列表(与 cache status list 同纪律)
		fmt.Fprintf(out, "instance: %s  (路径解析失败: %s)\n", label, v.pathErr)
		return
	}
	mark := map[bool]string{true: "有", false: "缺"}
	pairs := []struct {
		name string
		have bool
	}{
		{"auth", v.auth}, {"bin", v.bin}, {"meta", v.meta}, {"config", v.config},
		{"dek", v.dek || v.dekUnknown}, // 不可判不收进 missing(列值由 dekMark 渲染 "?")
	}
	var missing []string
	for _, p := range pairs {
		if !p.have {
			missing = append(missing, p.name)
		}
	}
	dekMark := mark[v.dek]
	if v.dekUnknown {
		dekMark = "?" // 解析不出 ≠ 缺失——不可判的列不冒充"缺"
	}
	ann := ""
	// 半态槽 = 槽目录在而材料有缺。空机器的默认槽(连目录都无)是合法的
	// vacuum 态,不是半态 —— 标注只在目录真实存在时给。
	if v.dirExists && len(missing) > 0 {
		ann = fmt.Sprintf("  ⚠ 半态槽(缺 %s)", strings.Join(missing, "·"))
	}
	rmHint := ""
	if instance != "" {
		rmHint = fmt.Sprintf("  rm: sshmgr cache instances rm %s", instance)
	}
	fmt.Fprintf(out, "instance: %s  auth=%s bin=%s meta=%s config=%s dek=%s  age=%s%s%s\n",
		label, mark[v.auth], mark[v.bin], mark[v.meta], mark[v.config], dekMark, v.age, ann, rmHint)
}

// dekOrphanNames returns names that HAVE a per-instance DEK file
// (cache-dek-<name>.key) but NO slot directory — the crash residue shape
// (DEK written, first pull never completed, or the dir deleted out-of-band).
// The default instance's unsuffixed cache-dek.key is not scanned here (the
// default slot row already shows its DEK); non-name-shaped files are skipped.
func dekOrphanNames(named []string) ([]string, error) {
	root := os.Getenv(paths.CacheDekDirEnv)
	if root == "" {
		vd, err := paths.VaultDir()
		if err != nil {
			// 无 vault 根 → 无孤儿可扫,但静默跳过会埋掉排查线索——如实说一声。
			fmt.Fprintf(os.Stderr, "WARNING: DEK orphan scan skipped — vault dir resolution failed (%v)\n", err)
			return nil, nil
		}
		root = vd
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil // 根不存在 = 无孤儿
	}
	inSlots := map[string]bool{"": true}
	for _, n := range named {
		inSlots[n] = true
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "cache-dek-") || !strings.HasSuffix(e.Name(), ".key") {
			continue
		}
		n := strings.TrimSuffix(strings.TrimPrefix(e.Name(), "cache-dek-"), ".key")
		if n == "" || inSlots[n] {
			continue
		}
		if verr := instname.Valid(n); verr != nil {
			continue // 异形文件不冒充实例
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

func cacheInstancesRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <实例名>",
		Short: "Remove one named instance's slot directory and its DEK (typed-name confirm; idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if name == "" {
				return errors.New("默认槽不可 rm(它没有名字)——如需清空本机全部 sshmgr 数据请用 `sshmgr clear`")
			}
			if clientops.SingleSlotOverrideEnvSet() {
				return errors.New("instance removal and SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK are mutually exclusive — the env fully overrides the cache path/DEK resolution and would target the wrong slot; unset the env and re-run")
			}
			// 先解析两根的真实落点——instname 白名单在这一步把 traversal/
			// 分隔符/Windows 保留名/绝对路径全拒(不进确认屏、不触 TTY 门)。
			dir, _, _, _, err := clientops.CachePathsFor(name)
			if err != nil {
				return err
			}
			dekPath, err := paths.CacheDekPathFor(name)
			if err != nil {
				return err
			}
			if !cacheInstancesStdinIsTTY() {
				return errors.New("cache instances rm 需要交互式终端(stdin 不是 TTY)——为防止脚本误删,拒绝执行")
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "将永久删除实例 %q 的以下本地材料(不可恢复):\n", name)
			fmt.Fprintf(out, "  ▸ 槽目录(整目录:cache.bin/auth/meta/config/配对产物等):%s\n", dir)
			fmt.Fprintf(out, "  ▸ 离线缓存 DEK:%s\n", dekPath)
			fmt.Fprintln(out, "(broker 侧设备码不受影响)")
			fmt.Fprint(out, "输入实例名确认:")
			line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
			if strings.TrimSpace(line) != name {
				fmt.Fprintln(out, "\n已取消,未做任何改动。")
				return nil
			}
			if err := clientops.RemoveInstance(name); err != nil {
				return err
			}
			fmt.Fprintf(out, "已删除实例 %q(槽目录与 DEK 均已清理)。\n", name)
			fmt.Fprintln(out, "配套两件事本机 rm 无法代劳:")
			fmt.Fprintf(out, "  1. broker 侧吊销该设备码(client 无权远程吊销):sshmgr cache-tokens revoke %s\n", name)
			fmt.Fprintln(out, "  2. --write-mcp 写在槽外的 .mcp.json 副本不随 rm 清理——该目标路径不持久化(cache.config.json 仅存 max_offline),rm 无从得知其位置,请自行删除")
			fmt.Fprintln(out, "提醒:若某 TUI/MCP 进程正使用该实例,重启对应进程后生效。")
			return nil
		},
	}
}
