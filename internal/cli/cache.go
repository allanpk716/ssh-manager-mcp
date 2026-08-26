package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
)

// The pull/pin/cred/lazy/reload implementation moved to internal/clientops
// (zero-behavior extraction, Stream B Task 1) so the upcoming TUI can reuse it
// without importing internal/cli. This file keeps only the cobra wrappers.

// cachePresentFor reports whether the given instance's cache.bin currently
// exists (used by mcp --cache spawn logging to say "serving stale cache" only
// when there IS a cache). "" = the default instance.
func cachePresentFor(instance string) bool {
	_, bin, _, _, err := clientops.CachePathsFor(instance)
	if err != nil {
		return false
	}
	_, err = os.Stat(bin)
	return err == nil
}

func newCacheCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "Offline read-only cache (pull from a serve broker)"}
	cmd.AddCommand(cachePullCmd(), cacheStatusCmd())
	return cmd
}

func cachePullCmd() *cobra.Command {
	var url, token, instance string
	c := &cobra.Command{
		Use:   "pull",
		Short: "Pull the whole vault from a serve broker into the local encrypted cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkInstanceFlag(instance); err != nil {
				return err
			}
			if url == "" {
				url = os.Getenv("SSHMGR_CACHE_URL")
			}
			if token == "" {
				token = os.Getenv("SSHMGR_CACHE_TOKEN")
			}
			if url == "" || token == "" {
				return fmt.Errorf("--url and --token are required (or SSHMGR_CACHE_URL / SSHMGR_CACHE_TOKEN)")
			}
			// Pin resolution priority: env SSHMGR_SERVE_PIN > --pin flag > token-embedded "<code>:<pin>".
			// plain=true means no pin anywhere. Per xcheck F4 the no-pin path hard-fails by
			// default (no silent plaintext); --allow-plaintext opts back into the legacy path.
			pinFlag, _ := cmd.Flags().GetString("pin")
			// F7: a pin-shaped but INVALID env/flag value is a hard error. We must NOT let it
			// silently fall through to plaintext (a typo in SSHMGR_SERVE_PIN must not remove
			// TLS protection). Only a fully-ABSENT pin is allowed to enter the plain branch.
			if raw := strings.TrimSpace(os.Getenv("SSHMGR_SERVE_PIN")); raw != "" {
				if _, ok := mcpserver.ParsePin(raw); !ok {
					return fmt.Errorf("SSHMGR_SERVE_PIN is set but not a valid sha256:<64hex> fingerprint: %q", raw)
				}
			}
			if raw := strings.TrimSpace(pinFlag); raw != "" {
				if _, ok := mcpserver.ParsePin(raw); !ok {
					return fmt.Errorf("--pin is not a valid sha256:<64hex> fingerprint: %q", raw)
				}
			}
			fp, plain := clientops.ResolvePin(os.Getenv("SSHMGR_SERVE_PIN"), pinFlag, token)
			if plain {
				allowPlain, _ := cmd.Flags().GetBool("allow-plaintext")
				if !allowPlain {
					return fmt.Errorf("no server pin provided: refusing to pull without TLS pin. " +
						"Set --pin/SSHMGR_SERVE_PIN (from `serve cert-info`), or pass --allow-plaintext for an insecure plaintext pull")
				}
				if err := clientops.DoPull(url, token, "", clientops.PullOpts{AllowPlain: true, StatusOut: cmd.ErrOrStderr(), Instance: instance}); err != nil {
					return err
				}
				return nil // plaintext pulls NEVER persist a credential (no auto-plaintext path)
			}
			if err := clientops.DoPull(url, token, fp, clientops.PullOpts{StatusOut: cmd.ErrOrStderr(), Instance: instance}); err != nil {
				if errors.Is(err, clientops.ErrCacheQuarantined) {
					// Plan 34 rev4 §3 — pinned 401: the local cache was destroyed.
					// SilenceUsage: this is a server-side rejection, not a flag typo.
					cmd.SilenceUsage = true
					return fmt.Errorf("cache was QUARANTINED: the server rejected this device code (revoked?).\nRe-enroll: obtain a fresh device code and run cache pull again.\n(detail: %v)", err)
				}
				return err
			}
			// Persist the credential for the lazy pull. Write failure is a WARNING,
			// not an error: the pull itself succeeded, but auto-refresh won't work
			// until a later successful pull — the user must hear about that.
			code := token
			if c, _, ok := clientops.SplitTokenPin(token); ok {
				code = c
			}
			if err := clientops.WriteCacheCredFor(instance, &clientops.CacheCred{URL: url, Token: code, Pin: fp}); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: could not persist cache.auth.json (automatic refresh disabled until a successful pull): %v\n", err)
			}
			return nil
		},
	}
	c.Flags().StringVar(&url, "url", "", "serve broker URL (https://host:7878)")
	c.Flags().StringVar(&token, "token", "", "device authorization code (from `cache-tokens add`)")
	c.Flags().StringVar(&instance, "instance", "", "route this pull to instances/<name> (default slot when omitted; mutually exclusive with SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK)")
	c.Flags().String("pin", "", "server SPKI fingerprint sha256:... (or set SSHMGR_SERVE_PIN); hard-fails without it unless --allow-plaintext")
	c.Flags().Bool("allow-plaintext", false, "opt into plaintext HTTP pull when no server pin is set (insecure; default is to refuse)")
	return c
}

func cacheStatusCmd() *cobra.Command {
	var instance string
	c := &cobra.Command{
		Use:   "status",
		Short: "Show cache presence, freshness, and counts",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkInstanceFlag(instance); err != nil {
				return err
			}
			if instance != "" {
				return cacheStatusSingle(cmd, instance)
			}
			return cacheStatusList(cmd)
		},
	}
	c.Flags().StringVar(&instance, "instance", "", "show this named instance's detail; without it, list every instance slot (default + named; mutually exclusive with SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK)")
	return c
}

// cacheStatusSingle renders one instance's detail view — the pre-Plan-40
// `cache status` format, For-ified, plus the Plan-40 device identity line.
func cacheStatusSingle(cmd *cobra.Command, instance string) error {
	_, bin, metaPath, _, err := clientops.CachePathsFor(instance)
	if err != nil {
		return err
	}
	snap, err := clientops.LoadCacheSnapshotFor(instance)
	if err != nil {
		// Plan 34 rev4 §4: attribute a server-rejection quarantine when
		// the on-disk manifest says so; otherwise the original error.
		if msg, ok := clientops.QuarantineReport(err); ok {
			return errors.New(msg)
		}
		return err
	}
	info, _ := os.Stat(bin)
	var age string
	if info != nil {
		age = time.Since(info.ModTime()).Round(time.Second).String()
	}
	url := "(unknown)"
	scoped := false
	device := "(unknown)"
	if mb, err := os.ReadFile(metaPath); err == nil {
		// anonymous twin of clientops' private cacheMeta: status reads
		// url + the Plan-39 scope provenance + the Plan-40 device identity
		var m struct {
			URL        string `json:"url"`
			Scoped     bool   `json:"scoped"`
			DeviceName string `json:"device_name"`
		}
		if json.Unmarshal(mb, &m) == nil && m.URL != "" {
			url = m.URL
		}
		scoped = m.Scoped
		if m.DeviceName != "" {
			device = m.DeviceName
		}
	}
	// Plan 39 provenance display (code-review #3): a single-profile
	// WHOLE-VAULT cache is shape-identical to a cropped one, so the
	// profile line is only honest when the pull itself recorded the
	// serve's scope header. Otherwise say "unverified" — never let a
	// legacy cache pass as cropped.
	profile := "unverified — re-pull to crop (pre-Plan-39 snapshot or old serve)"
	if scoped {
		switch len(snap.Profiles) {
		case 1:
			profile = snap.Profiles[0].Name
		case 0:
			profile = "(none)"
		default:
			profile = "(multiple — pre-Plan-39 whole-vault snapshot)"
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "cache:    %s\nage:      %s\nservers:  %d\ncreds:    %d\nscope:    %s\ndevice:    %s\nsource:   %s\n",
		bin, age, len(snap.Servers), len(snap.Credentials), profile, device, url)
	return nil
}

// cacheStatusList renders one line-group per instance slot: the default slot
// first (even when empty — said honestly), then every named instance. A slot
// that cannot LOAD (missing DEK, undecryptable bin) renders its error inline —
// listing is discovery, one broken slot must not hide the others (spec §2.6).
func cacheStatusList(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	dir, bin, metaPath, _, err := clientops.CachePaths()
	if err != nil {
		return err
	}
	printSlot := func(instance, d, binPath, mp string) {
		fmt.Fprintf(out, "instance: %s (%s)\n", map[bool]string{true: instance, false: "default"}[instance != ""], d)
		if _, serr := os.Stat(binPath); serr != nil {
			fmt.Fprintf(out, "  cache:   (no cache.bin)\n")
			// spec §2.6's field list includes a quarantine summary: post-quarantine
			// the manifest is the only trace, and a bare "(no cache.bin)" would hide
			// WHY. QuarantineReport consults the DEFAULT slot only, so this line is
			// default-slot-only for now (per-instance report = registered residual).
			if instance == "" {
				if msg, ok := clientops.QuarantineReport(serr); ok {
					fmt.Fprintf(out, "  quarantine: %s\n", msg)
				}
			}
			fmt.Fprintf(out, "\n")
			return
		}
		var (
			device, url, profileLine = "(unknown)", "(unknown)", ""
			anchored                 = "-"
			scoped                   bool
		)
		if mb, merr := os.ReadFile(mp); merr == nil {
			var m struct {
				DeviceName     string `json:"device_name"`
				URL            string `json:"url"`
				ServerAnchored bool   `json:"server_anchored"`
				Scoped         bool   `json:"scoped"`
			}
			if json.Unmarshal(mb, &m) == nil {
				device, anchored = m.DeviceName, map[bool]string{true: "server", false: "local"}[m.ServerAnchored]
				url, scoped = m.URL, m.Scoped
			}
		}
		servers, creds := "-", "-"
		if snap, lerr := clientops.LoadCacheSnapshotFor(instance); lerr == nil && snap != nil {
			servers, creds = fmt.Sprint(len(snap.Servers)), fmt.Sprint(len(snap.Credentials))
			// profile 行仅 scoped 时显示（Plan 39 溯源纪律——与 single 视图同规则）
			if scoped {
				switch len(snap.Profiles) {
				case 1:
					profileLine = "  profile: " + snap.Profiles[0].Name + "\n"
				case 0:
					profileLine = "  profile: (none)\n"
				default:
					profileLine = "  profile: (multiple — pre-Plan-39 whole-vault snapshot)\n"
				}
			}
		} else {
			fmt.Fprintf(out, "  load:    ERROR %v\n", lerr) // 行级错误,不中断列表
		}
		fmt.Fprintf(out, "  device:  %s\n%s  anchor:  %s\n  servers: %s\n  creds:   %s\n  source:  %s\n\n",
			device, profileLine, anchored, servers, creds, url)
	}
	printSlot("", dir, bin, metaPath) // "" = 默认槽
	names, err := clientops.ListInstances()
	if err != nil {
		return err
	}
	for _, n := range names {
		id, ib, im, _, ierr := clientops.CachePathsFor(n)
		if ierr != nil {
			fmt.Fprintf(out, "instance: %s (path error: %v)\n\n", n, ierr)
			continue
		}
		printSlot(n, id, ib, im)
	}
	return nil
}
