package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ssh-manager-mcp/internal/clientops"
	"ssh-manager-mcp/internal/mcpserver"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// resolveToken: flag wins, SSHMGR_TOKEN env is the fallback (identical
// semantics & downstream parsing — same name, same meaning; Plan 20 B2).
// env keeps the token OUT of the process argv (ps visibility); the flag
// path still shows it in argv.
func resolveToken(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv("SSHMGR_TOKEN")
}

// cacheForwarderFor resolves the instance's pull credential into the pin
// forwarder (Plan 48 §2.1). A missing or unreadable cache.auth.json constructs
// the no-capability flavor — its Forward fails closed with the §4 main
// message, exactly like an unreachable broker ("a present cache with a deleted
// credential cannot authenticate any forward" — never a silent local
// fallback). A hard misconfiguration (pinned non-https URL) is a spawn-time
// error.
func cacheForwarderFor(instance string) (*clientops.PinForwarder, error) {
	cred, err := clientops.ReadCacheCredFor(instance)
	if err != nil || cred == nil {
		return clientops.NewPinForwarder(clientops.CacheCred{})
	}
	return clientops.NewPinForwarder(*cred)
}

// cacheForwardDeviceFor resolves the device-code name stamped onto locally
// applied forwarded pins (entry_host_keys.pin_device — diagnostic
// metadata: the owner's cross-profile detection surface, never load-bearing).
// A named instance IS the device identity (Plan 40's pull gate enforces
// identity == directory name); the default slot reads cache.meta's
// device_name as asserted by the pinned serve, falling back to the instance
// name and then "" (legacy meta predating Plan 40, or unreadable).
func cacheForwardDeviceFor(instance string) string {
	if name := clientops.CacheDeviceNameFor(instance); name != "" {
		return name
	}
	return instance
}

// cacheMetaForwarderFor resolves the instance's pull credential into the
// metadata forwarder (Plan 51 §2) — cacheForwarderFor's twin: a missing or
// unreadable cache.auth.json constructs the no-capability flavor whose Edit
// fails closed with the main message (never a silent local write); a hard
// misconfiguration (pinned non-https URL) is a spawn-time error.
func cacheMetaForwarderFor(instance string) (*clientops.MetadataForwarder, error) {
	cred, err := clientops.ReadCacheCredFor(instance)
	if err != nil || cred == nil {
		return clientops.NewMetadataForwarder(clientops.CacheCred{})
	}
	return clientops.NewMetadataForwarder(*cred)
}

// cacheSoftwareInventoryForwarderFor 解析实例的拉取凭据并构造软件清单在
// 线查询转发器（票 #185）——cacheForwarderFor 的又一个同构：缺凭据得到
// 固定报错的形态，硬性配置错误（固定证书配明文地址）在启动时报错。
func cacheSoftwareInventoryForwarderFor(instance string) (*clientops.SoftwareInventoryForwarder, error) {
	cred, err := clientops.ReadCacheCredFor(instance)
	if err != nil || cred == nil {
		return clientops.NewSoftwareInventoryForwarder(clientops.CacheCred{})
	}
	return clientops.NewSoftwareInventoryForwarder(*cred)
}

func newMCPCmd() *cobra.Command {
	var token string
	var useCache bool
	var instance string
	var cacheMaxAge time.Duration
	c := &cobra.Command{
		Use:   "mcp",
		Short: "Run the SSH MCP server (stdio) for an AI agent",
		RunE: func(cmd *cobra.Command, args []string) error {
			token = resolveToken(token)
			if token == "" {
				return fmt.Errorf("--token or SSHMGR_TOKEN is required")
			}
			if useCache {
				if err := checkInstanceFlag(instance); err != nil {
					return err
				}
				if instance == "" {
					// §2.5: reading an instance is ALWAYS explicit — never guess.
					if _, bin, _, _, perr := clientops.CachePaths(); perr == nil {
						if _, berr := os.Stat(bin); os.IsNotExist(berr) {
							if names, lerr := clientops.ListInstances(); lerr == nil && len(names) > 0 {
								return fmt.Errorf("no cache in the default slot, but %d named instance(s) exist: %s — pass --instance <name> (the default slot is never auto-guessed)", len(names), strings.Join(names, ", "))
							}
						}
					}
				}
				// ① spawn-time freshness (failure degrades to the existing cache)
				if err := clientops.MaybeLazyPullFor(instance, cacheMaxAge); err != nil {
					// Plan 34 final review (Minor 3): a quarantine already logged
					// two lines (QuarantineCache's step verdict + MaybeLazyPull's
					// session-disable notice) — skip a redundant third here.
					if !errors.Is(err, clientops.ErrCacheQuarantined) {
						// "serving stale cache" is only true when a cache EXISTS — with no
						// cache.bin the upcoming LoadCacheSnapshot hard-fails instead, so
						// don't promise a degradation that isn't happening.
						if cachePresentFor(instance) {
							fmt.Fprintf(os.Stderr, "lazy cache pull failed (serving stale cache): %v\n", err)
						} else {
							fmt.Fprintf(os.Stderr, "lazy cache pull failed: %v\n", err)
						}
					}
				}
				// ② hot-reload baseline BEFORE the initial load (see clientops.CacheReloader)
				rel := clientops.NewCacheReloaderFor(instance, cacheMaxAge)
				snap, err := clientops.LoadCacheSnapshotFor(instance)
				if err != nil {
					// Plan 34 rev4 §4: attribute a server-rejection quarantine when
					// the on-disk manifest says so; otherwise the original error.
					if msg, ok := clientops.QuarantineReport(err); ok {
						return errors.New(msg)
					}
					return err
				}
				cacheDir, _, _, auditPath, err := clientops.CachePathsFor(instance)
				if err != nil {
					return err
				}
				// 统一拨号入口里的成功地址缓存按实例存放。这里把已解析的
				// 实例目录固定给本次桥进程，避免深处重新解析时落回默认槽。
				previousCacheDir := os.Getenv("SSHMGR_CACHE_DIR")
				if err := os.Setenv("SSHMGR_CACHE_DIR", cacheDir); err != nil {
					return err
				}
				// 同处固定实例名：桥内工具（如连通探测）据此解析 self 设备
				// 与本实例的探测结果文件，避免深处重新解析时落回默认槽。
				previousMCPInstance := os.Getenv("SSHMGR_MCP_INSTANCE")
				if err := os.Setenv("SSHMGR_MCP_INSTANCE", instance); err != nil {
					return err
				}
				defer func() {
					if previousCacheDir == "" {
						_ = os.Unsetenv("SSHMGR_CACHE_DIR")
					} else {
						_ = os.Setenv("SSHMGR_CACHE_DIR", previousCacheDir)
					}
					if previousMCPInstance == "" {
						_ = os.Unsetenv("SSHMGR_MCP_INSTANCE")
					} else {
						_ = os.Setenv("SSHMGR_MCP_INSTANCE", previousMCPInstance)
					}
				}()
				// 多机客户端的桥启动快照修订号后台轮询循环。间隔读实例配置
				// (环境变量可临时覆盖);启动阶段发现非法值时写告警并退回默认
				// 5 分钟,桥继续可用。单机部署不走这个分支,不启动任何定时器。
				pollInterval, _, perr := clientops.EffectiveSnapshotPollInterval(cacheDir)
				if perr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(),
						"sshmgr: %v; using the default 5m snapshot poll interval\n", perr)
					pollInterval = clientops.DefaultSnapshotPollInterval
				}
				// Plan 48 §2.1: the pin forwarder is constructed HERE — the
				// --instance resolver owns the CacheCred read; RunStdioCache never
				// guesses an instance. A missing/unreadable cache.auth.json builds
				// the no-capability forwarder: forwarding fails closed with the §4
				// main message, never a silent local fallback (a deleted credential
				// cannot authenticate any forward).
				fwd, err := cacheForwarderFor(instance)
				if err != nil {
					return err
				}
				// Plan 51 §7: the metadata forwarder is constructed at the
				// SAME --instance resolver (one CacheCred read, both
				// capabilities); a missing credential yields the no-capability
				// forwarder — the update_server_metadata tool exists but its
				// calls fail closed with the §2.1 main message.
				mfwd, err := cacheMetaForwarderFor(instance)
				if err != nil {
					return err
				}
				// 票 #185：软件清单查询转发器在同一处解析实例凭据。客户端
				// 的清单查询永远在线，权威端不可达时按固定文本报错。
				inventoryForwarder, err := cacheSoftwareInventoryForwarderFor(instance)
				if err != nil {
					return err
				}
				err = mcpserver.RunStdioCacheWithRefresher(token, snap, auditPath, rel.Check,
					clientops.ForwardingHostKeys(fwd), cacheForwardDeviceFor(instance), mfwd,
					func(refresher mcpserver.CacheRefresher) func() {
						return clientops.StartSnapshotPoller(clientops.SnapshotPollerConfig{
							Instance:  instance,
							Interval:  pollInterval,
							Refresher: refresher,
						})
					}, inventoryForwarder)
				// Release the pinned transports' idle connections on shutdown
				// (the forwarders have served their last call by now).
				fwd.Close()
				mfwd.Close()
				inventoryForwarder.Close()
				return err
			}
			// Residual-key guardrail: warn to STDERR only (stdout is the MCP channel).
			if st, err := vault.OpenStore(store.FileKeyProvider{}); err == nil {
				if found, _ := store.CheckResidualKeys(); len(found) > 0 {
					fmt.Fprintf(os.Stderr, "WARNING: ssh credential files detected at %v — hard enforcement can be bypassed by an agent that reads them directly. Remove them for full isolation.\n", found)
				}
				st.Close()
			}
			if err := mcpserver.RunStdio(token, store.FileKeyProvider{}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			return nil
		},
	}
	c.Flags().StringVar(&token, "token", "", "project token from `projects add` (or env SSHMGR_TOKEN)")
	c.Flags().BoolVar(&useCache, "cache", false, "serve from the local offline cache (read-only; pulled via `cache pull`)")
	c.Flags().StringVar(&instance, "instance", "", "serve the named cache instance instances/<name> (default slot when omitted; mutually exclusive with SSHMGR_CACHE_DIR/SSHMGR_CACHE_DEK)")
	c.Flags().DurationVar(&cacheMaxAge, "cache-max-age", 30*time.Minute,
		"auto-pull the offline cache when older than this (0 disables automatic pulls entirely)")
	return c
}
