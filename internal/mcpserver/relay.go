package mcpserver

// Plan 47: relay_file 大文件中继 —— 同步 preflight (spec §2 ①–⑨) + 引擎
// (runRelay/RelayTaskSpec, spec §2 引擎段, T5)。
//
// 连接所有权纪律 (spec §2, rev2 kimi#3/codex#1): ③④ 建立的 ConnectKeepAlive 自
// 建立起挂 defer 双 close;ReserveRelay 成功 (insertLocked 转正、引擎接管) 才解除
// defer 并把两条连接移交任务槽 (client=dest, auxClient=source)——此前一切 return
// (①–⑦ 的全部拒绝分支) 零泄漏。形态是 ForwardForProfile 的 err!=nil && cli!=nil
// 先例 (core.go) 的双连接版, 双 close 由布尔守卫在移交后整体解除 (幂等, 与
// runTask 终态段/CloseAll 的双保险叠加以外的第三层不叠加——移交后本层彻底不碰)。
//
// preflight 零远端状态变更 (rev3 kimi#1/codex#1/#5): 一切 mutation (MkdirAll /
// fresh 删除 / 空清单落盘) 都在引擎 stage 0、以 ⑧ 租约为前提。同步段只有
// connect + Stat/StatVFS/读清单。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pkg/sftp"

	"ssh-manager-mcp/internal/sshbroker"
	"ssh-manager-mcp/internal/store"
	"ssh-manager-mcp/internal/vault"
)

// relay 的两个 B 端工件后缀 (spec §3; `<to_path>` + 后缀)。字段名/文件名即协议。
const (
	relayPartialSuffix  = ".sshmgr-partial"
	relayManifestSuffix = ".sshmgr-manifest.json"
)

// manifest 解析上限的两个常数 (spec §2⑥ rev3 推导式): 每块 128B + 8KiB 底,
// 再与块数闸上界取 min——清单无界增长被 ③b 与这里双重钉死。
const (
	relayManifestBytesPerEntry int64 = 128
	relayManifestOverhead      int64 = 8 << 10
)

// RelayForProfile 执行 relay_file 的全部廉价校验并在 ⑧ 建后台中继任务, 立即返回
// 计划元数据 (exec_background 同款形态)。chunkBytes 来自构造期 env seam
// (SSHMGR_TRANSFER_CHUNK, T2); 测试直传小值。执行序 ①–⑨ 逐字落 spec §2。
func RelayForProfile(ctx context.Context, st *store.Store, tm *TaskManager, projectID, profileID string, in RelayInput, chunkBytes int64, hk ...sshbroker.HostKeyStore) (out RelayOutput, err error) {
	var status string
	var startOwned bool             // start(ok) 行已由 AuditStart 闭包 (持锁段) 落笔——本层不再落
	var auditServer = in.ToServerID // 审计行归因端点: 随 ②③④ 阶段切到当下在判的一端
	var size, chunks, resumed int64 // 摘要模板的三个活值: 阶段推进即更新
	fromCanon, toCanon := "", ""    // ① canonical 一次贯通 (§1.1): 同径判定/工件/审计同值
	start := time.Now()
	relaySummary := func() string {
		fs, fc := in.FromServerID, fromCanon
		if fs == "" {
			fs = "local"
		}
		if fc == "" {
			fc = in.FromPath
		}
		tc := toCanon
		if tc == "" {
			tc = in.ToPath
		}
		return fmt.Sprintf("relay %s:%s -> %s:%s (%d bytes, %d chunks, resumed %d)",
			fs, fc, in.ToServerID, tc, size, chunks, resumed)
	}
	defer func() {
		if startOwned {
			return // start(ok) 行已落 (Insert 持锁段), end 行归引擎——本层零双写
		}
		if status == "" {
			status = "error"
		}
		_ = st.WriteAudit(store.AuditRow{
			TS: start, ProjectID: projectID, ServerID: auditServer,
			Action: "relay-bg-start", Command: relaySummary(), Status: status,
			DurationMS: time.Since(start).Milliseconds(),
		})
	}()

	// 连接所有权: 移交前一切 return 双 close;移交 (⑧ 成功) 后布尔解除。
	transferred := false
	var srcCli, dstCli *sshbroker.Client
	defer func() {
		if transferred {
			return
		}
		if srcCli != nil {
			_ = srcCli.Close()
		}
		if dstCli != nil {
			_ = dstCli.Close()
		}
	}()

	// ---- ① 参数层校验 (caller's own input——先于 gate, upload_content 先例) ----
	if in.ToServerID == "" {
		return RelayOutput{}, fmt.Errorf("to_server_id is required (the relay destination is always a remote server)")
	}
	localSource := in.FromServerID == ""
	if localSource {
		// 本机源按 broker 宿主 OS 语义判绝对 (spec §1.1): Windows 上 /foo 假。
		// 先 IsAbs 原始输入再 Clean (Clean 不改变绝对性, 但判据钉在原始值上)。
		if !filepath.IsAbs(in.FromPath) {
			return RelayOutput{}, fmt.Errorf("from_path %q must be an absolute path on the broker host", in.FromPath)
		}
		fromCanon = filepath.Clean(in.FromPath)
	} else {
		if !isAbsRemotePath(in.FromPath) {
			return RelayOutput{}, fmt.Errorf("from_path %q must be an absolute path starting with / (or a Windows drive root C:/)", in.FromPath)
		}
		// 远程命名空间纯 POSIX: path.Clean, 禁 filepath/ToSlash (反斜杠是合法
		// POSIX 文件名字符, /tmp/a\b ≠ /tmp/a/b)。
		fromCanon = path.Clean(in.FromPath)
	}
	if !isAbsRemotePath(in.ToPath) {
		return RelayOutput{}, fmt.Errorf("to_path %q must be an absolute path starting with / (or a Windows drive root C:/)", in.ToPath)
	}
	toCanon = path.Clean(in.ToPath)

	if in.FromServerID == in.ToServerID && fromCanon == toCanon {
		return RelayOutput{}, fmt.Errorf("same source and destination path %s on server %s — a relay that overwrites its own source is a no-op self-copy", fromCanon, in.ToServerID)
	}

	// 四工件写集 (spec §1.1 rev4: 键 = to_server_id + NUL + canonical 工件路径);
	// own read ∩ own write = ∅ (fresh 会删自己的源、非 fresh 会 truncate 在读的 partial)。
	writeKeys := []string{
		relayKeyOf(in.ToServerID, toCanon),
		relayKeyOf(in.ToServerID, toCanon+relayPartialSuffix),
		relayKeyOf(in.ToServerID, toCanon+relayManifestSuffix),
		relayKeyOf(in.ToServerID, toCanon+relayManifestSuffix+".tmp"),
	}
	if !localSource {
		readKey := relayKeyOf(in.FromServerID, fromCanon)
		for _, wk := range writeKeys {
			if wk == readKey {
				return RelayOutput{}, fmt.Errorf("source path %s is one of this transfer's own destination artifacts on %s — a relay must not read from its own partial/manifest (fresh would delete the source; resume would truncate it)", fromCanon, in.ToServerID)
			}
		}
	}

	// ---- ② profile gate: 双端独立判, 任一越权即 denied (先于一切内容级错误) ----
	allowed, ferr := st.ServersForProfile(profileID)
	if ferr != nil {
		err = ferr
		return
	}
	if !localSource && !contains(allowed, in.FromServerID) {
		status, auditServer = "denied", in.FromServerID
		err = ErrNotInProfile
		return
	}
	if !contains(allowed, in.ToServerID) {
		status = "denied"
		err = ErrNotInProfile
		return
	}

	// ---- ③ 源端 stat (远程 sftp / 本机 os; 须常规文件; mtime 整秒) ----
	var mtime int64
	if localSource {
		fi, serr := os.Stat(fromCanon)
		if serr != nil {
			status = "error"
			err = fmt.Errorf("source stat %s: %w", fromCanon, serr)
			return
		}
		if !fi.Mode().IsRegular() {
			status = "error"
			err = fmt.Errorf("source %s is not a regular file (directories are not supported — tar on the source first)", fromCanon)
			return
		}
		size, mtime = fi.Size(), fi.ModTime().Unix()
	} else {
		srv, gerr := st.GetServer(in.FromServerID)
		if gerr != nil || srv == nil {
			status = "error"
			err = fmt.Errorf("server %s not found", in.FromServerID)
			return
		}
		auditServer = in.FromServerID
		auth, aerr := vault.AuthForServer(st, srv)
		if aerr != nil {
			if errors.Is(aerr, vault.ErrNoCredential) {
				status = "no_credential"
			} else {
				status = "auth_error"
			}
			err = aerr
			return
		}
		hkCb, herr := sshbroker.HostKeyTOFU(hostKeyStoreFor(st, hk), srv.Host, srv.Port)
		if herr != nil {
			status = "error"
			err = herr
			return
		}
		cli, cerr := sshbroker.ConnectKeepAlive(ctx, srv.Host, srv.Port, srv.User, auth, hkCb)
		if cerr != nil {
			switch {
			case errors.Is(cerr, context.Canceled):
				status = "cancelled"
			case errors.Is(cerr, sshbroker.ErrHostKeyMismatch):
				status = "hostkey_mismatch"
			default:
				status = "connect_error"
			}
			err = cerr
			return
		}
		srcCli = cli
		sc, scerr := cli.RelaySFTP()
		if scerr != nil {
			status = "error"
			err = fmt.Errorf("source sftp: %w", scerr)
			return
		}
		defer sc.Close()
		fi, serr := sc.Stat(fromCanon)
		if serr != nil {
			status = "error"
			err = fmt.Errorf("source stat %s: %w", fromCanon, serr)
			return
		}
		if !fi.Mode().IsRegular() {
			status = "error"
			err = fmt.Errorf("source %s is not a regular file (directories are not supported — tar on the source first)", fromCanon)
			return
		}
		size, mtime = fi.Size(), fi.ModTime().Unix()
	}

	// ---- ③b 块数闸 (溢出安全: n = size/chunk 进位, 禁 (size+chunk-1) 形态) ----
	chunks, gerr := relayChunkGate(size, chunkBytes)
	if gerr != nil {
		status = "error"
		err = gerr
		return
	}

	// ---- ④ 目标端连接 (dest 一侧的完整门链, 词汇表与源端同) ----
	auditServer = in.ToServerID
	dstSrv, derr := st.GetServer(in.ToServerID)
	if derr != nil || dstSrv == nil {
		status = "error"
		err = fmt.Errorf("server %s not found", in.ToServerID)
		return
	}
	dstAuth, aerr := vault.AuthForServer(st, dstSrv)
	if aerr != nil {
		if errors.Is(aerr, vault.ErrNoCredential) {
			status = "no_credential"
		} else {
			status = "auth_error"
		}
		err = aerr
		return
	}
	dstHkCb, herr := sshbroker.HostKeyTOFU(hostKeyStoreFor(st, hk), dstSrv.Host, dstSrv.Port)
	if herr != nil {
		status = "error"
		err = herr
		return
	}
	dstCli, cerr := sshbroker.ConnectKeepAlive(ctx, dstSrv.Host, dstSrv.Port, dstSrv.User, dstAuth, dstHkCb)
	if cerr != nil {
		switch {
		case errors.Is(cerr, context.Canceled):
			status = "cancelled"
		case errors.Is(cerr, sshbroker.ErrHostKeyMismatch):
			status = "hostkey_mismatch"
		default:
			status = "connect_error"
		}
		err = cerr
		return
	}
	dstSC, scerr := dstCli.RelaySFTP()
	if scerr != nil {
		status = "error"
		err = fmt.Errorf("destination sftp: %w", scerr)
		return
	}
	defer dstSC.Close()

	// ---- ⑤ Stat(to_path): 目录拒; 其余形态交 ⑥ 状态表判别 ----
	manifestPath := toCanon + relayManifestSuffix
	partialPath := toCanon + relayPartialSuffix
	realFi, rerr := dstSC.Stat(toCanon)
	hasReal := rerr == nil
	if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		status = "error"
		err = fmt.Errorf("destination stat %s: %w", toCanon, rerr)
		return
	}
	if hasReal && realFi.IsDir() {
		status = "error"
		err = fmt.Errorf("destination path %s is an existing directory — relay needs a file path (tar the directory first)", toCanon)
		return
	}

	// ---- ⑥ Manifest 判定 (fresh=true 整段跳过语义解析——只记标记; rev3 codex#5) ----
	var completedBytes int64
	var manifest6 *sshbroker.RelayManifest // ⑥ 命中的已解析清单 = T5 引擎唯一事实源 (nil = 首跑/fresh)
	if !in.Fresh {
		manifestPresent, m, reject := relayPreflightManifest(dstSC, manifestPath, chunks, chunkBytes, size, mtime)
		if reject != nil {
			status = "error"
			err = reject
			return
		}
		if manifestPresent {
			completedBytes = manifestCompletedBytes(m, chunkBytes, size)
			resumed = int64(len(m.Chunks))
			manifest6 = m
			// partial 结构校验 (manifest 命中且 partial 在): 非常规文件拒;
			// size < 最高完成块末尾拒 (manifest 谎报完成度)。
			pFi, pErr := dstSC.Stat(partialPath)
			hasPartial := pErr == nil
			if pErr != nil && !errors.Is(pErr, os.ErrNotExist) {
				status = "error"
				err = fmt.Errorf("stat partial %s: %w", partialPath, pErr)
				return
			}
			if hasPartial {
				if !pFi.Mode().IsRegular() {
					status = "error"
					err = fmt.Errorf("partial file %s is not a regular file — pass fresh=true to discard and restart", partialPath)
					return
				}
				if hi := relayHighestChunkEnd(m, chunkBytes, size); pFi.Size() < hi {
					status = "error"
					err = fmt.Errorf("manifest claims %d completed bytes but the partial file holds only %d — the manifest lies about progress; pass fresh=true to discard and restart", hi, pFi.Size())
					return
				}
			}
			// §3 状态表派发 (rev4 codex#1: "真名在场"行按 chunk 覆盖度分流)。
			allDone := int64(len(m.Chunks)) == chunks
			empty := len(m.Chunks) == 0
			switch {
			case hasPartial && hasReal:
				if !allDone { // r12
					status = "error"
					err = fmt.Errorf("manifest records incomplete progress (%d/%d chunks) while the destination file and partial both exist — abnormal state; pass fresh=true to discard and restart", len(m.Chunks), chunks)
					return
				}
				// r11 重试提交态: missing=0, 直奔提交 (引擎), 零重传。
			case hasPartial: // r6 中断续传 (任意完成度; r6b 含洞)
				// 已过 partial 结构校验。
			case hasReal:
				switch {
				// r8 零字节传输的 debris 形态必须先于 allDone 判: size==0 的合法
				// 清单恒为空 (0≤i<n=0 拒一切下标), empty && size==0 时 allDone 亦
				// 为真——若 allDone 在前, 本分支不可达。
				case empty && size == 0: // r8
					status = "error"
					err = fmt.Errorf("destination %s already holds completed-transfer debris (zero-byte commit's manifest) — harmless leftover; remove %s manually, or pass fresh=true to overwrite intentionally", toCanon, manifestPath)
					return
				case allDone: // r7 提交成功碎片 (上次 commit 已成功; size>0 时全完成
					// 清单必非空——空清单 size>0 落 r9, 两分支互斥)
					status = "error"
					err = fmt.Errorf("destination %s already holds completed-transfer debris (final file + manifest from a successful commit) — harmless leftover; remove %s manually, or pass fresh=true to overwrite intentionally", toCanon, manifestPath)
					return
				case empty: // r9 覆盖旧真名的首跑中断自愈态
					resumed = 0 // 完成时替换真名
				default: // r10
					status = "error"
					err = fmt.Errorf("manifest records incomplete progress (%d/%d chunks) over an existing destination file without a partial — abnormal state; pass fresh=true to discard and restart", len(m.Chunks), chunks)
					return
				}
			default: // !hasPartial && !hasReal
				switch {
				case allDone: // r5 中断于提交前 → 续传=直奔提交
				case empty: // r3 合法自愈态: 引擎补建 partial 续走
					resumed = 0
				default: // r4 非常态 (手动删 partial?)
					status = "error"
					err = fmt.Errorf("manifest records %d/%d completed chunks but the partial file is gone — its data cannot be trusted; pass fresh=true to discard and restart", len(m.Chunks), chunks)
					return
				}
			}
		} else {
			// 无 manifest: r2/r13 (partial 在) 非常态拒; r1/r14 首跑/覆盖语义。
			_, pErr := dstSC.Stat(partialPath)
			hasPartial := pErr == nil
			if pErr != nil && !errors.Is(pErr, os.ErrNotExist) {
				status = "error"
				err = fmt.Errorf("stat partial %s: %w", partialPath, pErr)
				return
			}
			if hasPartial {
				status = "error"
				err = fmt.Errorf("a partial file exists at %s without any manifest — no trustworthy progress exists (the manifest is the only source of truth); pass fresh=true to discard and restart", partialPath)
				return
			}
			resumed = 0 // completedBytes 恒 0 → missing = 全量
		}
	}

	// ---- ⑦ 空间预检 (Available 单值 + fresh 投影回收; 不足 → refusal) ----
	missing := size - completedBytes
	margin := chunkBytes
	if missing < margin {
		margin = missing
	}
	need := relaySpaceNeed(missing, margin)
	reclaim := int64(0)
	if in.Fresh { // rev4 codex#2: fresh 投影计入 Stat(partial) 可回收字节
		pFi, pErr := dstSC.Stat(partialPath)
		if pErr == nil && pFi.Mode().IsRegular() {
			reclaim = pFi.Size()
		} else if pErr != nil && !errors.Is(pErr, os.ErrNotExist) {
			status = "error"
			err = fmt.Errorf("stat partial %s: %w", partialPath, pErr)
			return
		}
	}
	spaceCheck := "ok"
	// StatVFS 定位 = 目标父目录; 不存在时 RelayAvailable 逐级上溯最近存在祖先
	// (rev4 kimi#7), 根仍失败 → soft unavailable。
	avail, availOK, aerr := dstCli.RelayAvailable(dstSC, path.Dir(toCanon))
	if aerr != nil || !availOK {
		spaceCheck = "unavailable" // fail-open (grilling 拍板): 知情继续
	} else {
		projected := relaySatAdd(avail, reclaim)
		if projected < need {
			status = "error"
			err = fmt.Errorf("destination has %d bytes available but the transfer needs %d (%d missing + %d margin) — free space on the destination or resume more chunks; refusing before any byte moves", projected, need, missing, margin)
			return
		}
	}

	// ---- ⑧⑨ ReserveRelay + 返回 (AuditStart 持锁先落 start(ok) 行, 先于 goroutine) ----
	var readKeys []string
	if !localSource {
		readKeys = []string{relayKeyOf(in.FromServerID, fromCanon)}
	}
	// 引擎闭包上下文 (T5): preflight 全部产物 + ⑧ 移交的双连接, 一个结构体捕获
	// (spec §2 引擎段——闭包零重读盘上状态, Manifest 即唯一事实源)。
	relaySpec := &RelayTaskSpec{
		SrcServerID: in.FromServerID, DstServerID: in.ToServerID,
		FromPath: fromCanon, ToPath: toCanon,
		Size: size, Mtime: mtime, ChunkBytes: chunkBytes, ChunksTotal: chunks,
		Fresh: in.Fresh, Resumed: int(resumed), Manifest: manifest6,
		SrcCli: srcCli, DstCli: dstCli,
	}
	spec := BgTaskSpec{
		ProjectID: projectID,
		ServerID:  in.ToServerID,
		Command:   relaySummary(),
		Timeout:   relayRunCap, // 72h 常量 (spec §4), 不走 clampBgTimeout
		// AuditStart 在 insertLocked 持锁段内、引擎 goroutine 启动前落 start(ok)
		// 行 (rev2 kimi#8/codex#6: 字面序会与秒级失败的 end 行倒挂)。
		AuditStart: func() {
			_ = st.WriteAudit(store.AuditRow{
				TS: start, ProjectID: projectID, ServerID: in.ToServerID,
				Action: "relay-bg-start", Command: relaySummary(), Status: "ok",
				DurationMS: time.Since(start).Milliseconds(),
			})
		},
		AuditAction: "relay-bg-end",
		AuditEnd:    func(row store.AuditRow) error { return st.WriteAudit(row) },
		// T5 引擎: runRelay 挂双连接槽 (client=dest, auxClient=source, T3 槽位语义)
		// 后经 runTask 走完整终态纪律 (终态即关两条连接)。
		Run: func(ectx context.Context, tb *bgTask) {
			tm.runRelay(ectx, tb, relaySpec)
		},
	}
	taskID, _, rerr := tm.ReserveRelay(spec, readKeys, writeKeys)
	if rerr != nil {
		// 冲突/满员/manager 已关 → start 行 status=error (spec §6), 本层 defer 落。
		status = "error"
		err = rerr
		return
	}
	// 移交: 两条连接的所有权随 insertLocked 原子移交引擎 (终态即关/CloseAll 可达
	// 即关, runTask 双槽语义);本层 defer 双 close 整体解除, start 行已由闭包落笔。
	transferred = true
	startOwned = true
	out = RelayOutput{
		TaskID:        taskID,
		BytesTotal:    size,
		ChunksTotal:   int(chunks),
		ResumedChunks: int(resumed),
		ChunkBytes:    chunkBytes,
		SpaceCheck:    spaceCheck,
	}
	return
}

// relayKeyOf 构造 endpoint 限定键 (rev4 kimi#1/codex#4): server + NUL + canonical
// 路径——四个判定矩阵全部在此同构键空间求交, 跨服务器同路径零互斥。
func relayKeyOf(server, p string) string { return server + "\x00" + p }

// relayCeilDiv 是溢出安全的 ⌈size/chunk⌉ (rev4 codex#7): n = size/chunk 进位——
// 绝不 (size+chunk-1)/chunk (size 近 MaxInt64 时先溢为负绕闸)。
func relayCeilDiv(size, chunk int64) int64 {
	n := size / chunk
	if size%chunk != 0 {
		n++
	}
	return n
}

// relayChunkGate 是 ③b 块数闸 (preflight fail-closed): size<0 拒;
// ⌈size/chunk⌉ > relayMaxChunks → refusal (文本带两值 + SSHMGR_TRANSFER_CHUNK 指引),
// 挡 manifest O(n²) 写放大与清单无界增长 (spec §2③b)。
func relayChunkGate(size, chunkBytes int64) (int64, error) {
	if size < 0 {
		return 0, fmt.Errorf("relay: negative source size %d", size)
	}
	n := relayCeilDiv(size, chunkBytes)
	if n > relayMaxChunks {
		return n, fmt.Errorf("source size %d needs %d chunks of %d bytes but the maximum is %d — raise SSHMGR_TRANSFER_CHUNK and retry (the chunk grid must stay stable across resume attempts)", size, n, chunkBytes, relayMaxChunks)
	}
	return n, nil
}

// relayManifestSizeCap 是 ⑥ 的解析上限 (rev3 推导式叠加块数闸上界, 取 min):
// ⌈size/chunk⌉×128B+8KiB 与 relayMaxChunks×128B+8KiB 的较小者。调用点在 ③b 之后,
// chunks ≤ relayMaxChunks, 乘法无溢出。
func relayManifestSizeCap(chunks int64) int64 {
	a := chunks*relayManifestBytesPerEntry + relayManifestOverhead
	b := relayMaxChunks*relayManifestBytesPerEntry + relayManifestOverhead
	if a < b {
		return a
	}
	return b
}

// relayChunkLen 是第 i 块的字节数 (末块可短)。
func relayChunkLen(i, chunkBytes, size int64) int64 {
	rem := size - i*chunkBytes
	if rem < chunkBytes {
		return rem
	}
	return chunkBytes
}

// relayPreflightManifest 是 ⑥ 的解析+结构防御段 (spec §2⑥/§3): Stat 定大小 →
// 推导上限拒 → 解析 → 结构校验 (version==1 / chunk_bytes 匹配 / index 唯一且
// 0≤i<n / sha256 64-hex / 完成字节 ≤ size) → source_size/mtime 对 ③。
// manifest 不在 → (false, nil, nil) (状态表按"无 manifest"派发)。
func relayPreflightManifest(sc *sftp.Client, manifestPath string, chunks, chunkBytes, size, mtime int64) (bool, *sshbroker.RelayManifest, error) {
	mfi, merr := sc.Stat(manifestPath)
	if errors.Is(merr, os.ErrNotExist) {
		return false, nil, nil
	}
	if merr != nil {
		return false, nil, fmt.Errorf("stat manifest %s: %w", manifestPath, merr)
	}
	if !mfi.Mode().IsRegular() {
		return false, nil, fmt.Errorf("manifest %s is not a regular file — pass fresh=true to discard and restart", manifestPath)
	}
	if mfi.Size() > relayManifestSizeCap(chunks) {
		return false, nil, fmt.Errorf("manifest %s is %d bytes, over the derived limit of %d for this transfer — it cannot be trusted; pass fresh=true to discard and restart", manifestPath, mfi.Size(), relayManifestSizeCap(chunks))
	}
	m, perr := sshbroker.RelayReadManifest(sc, manifestPath)
	if perr != nil {
		return false, nil, fmt.Errorf("parse manifest %s: %w (pass fresh=true to discard and restart)", manifestPath, perr)
	}
	if _, verr := relayValidateManifest(m, chunkBytes, size, mtime, chunks); verr != nil {
		return false, nil, verr
	}
	return true, m, nil
}

// relayValidateManifest 是 ⑥ 的结构校验 (顺序钉 spec §2⑥: version → chunk_bytes
// → index 唯一/界/hex/完成字节 → size/mtime 对 ③)。返回已完成字节数 (洞感知)。
func relayValidateManifest(m *sshbroker.RelayManifest, wantChunk, size, mtime, chunks int64) (int64, error) {
	if m.Version != 1 {
		return 0, fmt.Errorf("unsupported manifest version %d (want 1) — pass fresh=true to discard and restart", m.Version)
	}
	if m.ChunkBytes != wantChunk {
		return 0, fmt.Errorf("manifest chunk_bytes %d does not match the current %d — restore SSHMGR_TRANSFER_CHUNK to %d or pass fresh=true to discard and restart", m.ChunkBytes, wantChunk, m.ChunkBytes)
	}
	seen := make(map[int]struct{}, len(m.Chunks))
	var completed int64
	for _, c := range m.Chunks {
		if c.I < 0 {
			return 0, fmt.Errorf("invalid manifest: negative chunk index %d — pass fresh=true to discard and restart", c.I)
		}
		if int64(c.I) >= chunks {
			return 0, fmt.Errorf("invalid manifest: chunk index %d out of range (0..%d) — pass fresh=true to discard and restart", c.I, chunks-1)
		}
		if _, dup := seen[c.I]; dup {
			return 0, fmt.Errorf("invalid manifest: duplicate chunk index %d — pass fresh=true to discard and restart", c.I)
		}
		seen[c.I] = struct{}{}
		if !relayIsHex64(c.SHA256) {
			return 0, fmt.Errorf("invalid manifest: chunk %d sha256 is not 64 hex chars — pass fresh=true to discard and restart", c.I)
		}
		completed += relayChunkLen(int64(c.I), wantChunk, size)
	}
	if completed > size {
		return 0, fmt.Errorf("invalid manifest: %d completed bytes exceed the %d-byte source — pass fresh=true to discard and restart", completed, size)
	}
	if m.SourceSize != size || m.SourceMtimeUnix != mtime {
		return 0, fmt.Errorf("source file changed since the interrupted transfer (manifest size %d mtime %d vs current %d/%d) — pass fresh=true to discard and restart", m.SourceSize, m.SourceMtimeUnix, size, mtime)
	}
	return completed, nil
}

// relayIsHex64 报告 s 是否恰 64 个十六进制字符 (大小写均可——校验的是垃圾,
// 解码比较在引擎抽读复核, hex.DecodeString 对两态皆真)。
func relayIsHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// manifestCompletedBytes 汇总已完成块字节 (洞感知; 供 ⑦ missing 口径)。
func manifestCompletedBytes(m *sshbroker.RelayManifest, chunkBytes, size int64) int64 {
	var n int64
	for _, c := range m.Chunks {
		n += relayChunkLen(int64(c.I), chunkBytes, size)
	}
	return n
}

// relayHighestChunkEnd 返回最高已完成块的末尾偏移 (partial 结构校验的锚)。
func relayHighestChunkEnd(m *sshbroker.RelayManifest, chunkBytes, size int64) int64 {
	var hi int64
	for _, c := range m.Chunks {
		if end := int64(c.I)*chunkBytes + relayChunkLen(int64(c.I), chunkBytes, size); end > hi {
			hi = end
		}
	}
	return hi
}

// relaySpaceNeed 是 ⑦ 的需求式 (rev3 kimi#6): missing + min(chunk, missing) 余量,
// 饱和加法——missing 近 MaxInt64 时余量不再进位为负 (溢出安全)。
func relaySpaceNeed(missing, margin int64) int64 {
	if missing <= 0 {
		return 0 // missing=0 免余量 (只差提交的续传/zero-byte 不被过度拒)
	}
	return relaySatAdd(missing, margin)
}

// relaySatAdd 是饱和加法: 结果钳在 MaxInt64, 永不为负 (uint64→int64 安全口径的
// int64 侧)。
func relaySatAdd(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

// ---------- Plan 47 T5: runRelay 引擎 (spec §2 引擎段) ----------

// RelayTaskSpec 是引擎闭包的一次性上下文 (preflight ①–⑦ 的产物 + ⑧ 移交的双
// 连接): canonical 路径、③ 源 stat、③b 块网格、⑥ manifest 判定结果、fresh 标记
// 与续传块数。闭包零重读盘上状态——Manifest 非空即 preflight 解析并结构校验过的
// 唯一事实源, nil = 首跑/fresh (stage 0 条件落空清单)。
type RelayTaskSpec struct {
	SrcServerID string // "" = broker 本机源 (os.File 读, 不登记 read 键)
	DstServerID string
	FromPath    string // canonical 源路径 (远程 POSIX / 本机宿主语义)
	ToPath      string // canonical 目标路径 (远程 POSIX)
	Size, Mtime int64  // preflight ③ 的源 stat (完成段 re-stat 的基准)
	ChunkBytes  int64
	ChunksTotal int64
	Fresh       bool
	Resumed     int // 进入循环前已完成的块数 (⑥ 命中; 自愈态已归 0)
	// Manifest 是 ⑥ 解析过的盘上清单 (引擎内取工作副本, 不改此结构); nil = 首跑
	// 或 fresh (stage 0 fresh 删除后条件落空清单)。
	Manifest *sshbroker.RelayManifest
	// SrcCli/DstCli 是 ⑧ 移交的两条连接 (本机源 SrcCli=nil)。引擎入场挂任务槽
	// (client=DstCli, auxClient=SrcCli), 终态即关 / CloseAll 可达即关 (runTask 双槽)。
	SrcCli, DstCli *sshbroker.Client
}

// runRelay 是 relay 任务的 BgTaskSpec.Run 闭包体 (T3 终态纪律零复制): 挂双连接
// 槽后交 runTask——stopReq/timeout/failed/done→ok 锁内映射、closed 抑制、终态
// notify、cancel 释放 WithTimeout、锁外关双连接、auditEnd(relay-bg-end) 全继承。
func (tm *TaskManager) runRelay(ctx context.Context, t *bgTask, spec *RelayTaskSpec) {
	tm.mu.Lock()
	t.client, t.auxClient = spec.DstCli, spec.SrcCli
	tm.mu.Unlock()
	tm.runTask(ctx, t, spec.DstCli, func(ectx context.Context, stdout, _ io.Writer) (int, bool, error) {
		return relayEngineRun(ectx, spec, stdout)
	}, nil)
}

// relayEngineRun 是引擎本体 (spec §2 引擎段: stage 0 钉序 → 逐块 → 完成)。经
// runTask 的 exec 闭包形态接入终态纪律, 返回三元组: (0,false,nil)=done;
// (0,true,deadline)=timeout; (0,false,ctx.Err)=stopped/closed 抑制 (stopReq 置位
// 先于 cancel, 同锁无窗口——stop 恒胜过错误); (0,false,err)=failed。进度行一律
// 经 out (runTask 的 notifyWriter) 落笔——落笔即广播, exec_output 长轮询即时唤醒。
func relayEngineRun(ctx context.Context, spec *RelayTaskSpec, out io.Writer) (int, bool, error) {
	startedAt := time.Now()
	// abort 是唯一失败出口 (Upload 同款取消优先): ctx 已死时以 ctx 自身错误上报,
	// 终态交给 runTask 的映射——stop→stopped、deadline→timeout、CloseAll→closed
	// 抑制; ctx 活着时的 err 才是任务自身的 failed。
	abort := func(e error) (int, bool, error) {
		if cerr := ctx.Err(); cerr != nil {
			if errors.Is(cerr, context.DeadlineExceeded) {
				return 0, true, cerr
			}
			return 0, false, cerr
		}
		return 0, false, e
	}
	progress := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }

	toPath, fromPath := spec.ToPath, spec.FromPath
	partialPath, manifestPath := toPath+relayPartialSuffix, toPath+relayManifestSuffix
	progress("relay plan: %d bytes, %d chunks (resumed %d), chunk=%d", spec.Size, spec.ChunksTotal, spec.Resumed, spec.ChunkBytes)

	// 引擎自建 sftp 客户端 (preflight 的同类句柄已随其 defer 关闭); 连接本体是
	// ⑧ 移交的两条。watchdog (Upload 同款): ctx cancel → 关 sftp → 在途块即断,
	// manifest 保留已完成块。
	dstSC, derr := spec.DstCli.RelaySFTP()
	if derr != nil {
		return abort(fmt.Errorf("destination sftp: %w", derr))
	}
	defer dstSC.Close() // 与 watchdog 关幂等
	var srcSC *sftp.Client
	if spec.SrcCli != nil {
		var serr error
		if srcSC, serr = spec.SrcCli.RelaySFTP(); serr != nil {
			return abort(fmt.Errorf("source sftp: %w", serr))
		}
		defer srcSC.Close()
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = dstSC.Close() // 在途 sftp 操作解除阻塞 → 逐块循环即断
			if srcSC != nil {
				_ = srcSC.Close()
			}
		case <-done:
		}
	}()

	// 源句柄 (远程 sftp / 本机 os.File; 本机源的复制走引擎本地镜像, 见
	// relayCopyLocalChunk 的取舍注释)。
	var srcFile *sftp.File
	var srcLocal *os.File
	if srcSC != nil {
		var oerr error
		if srcFile, oerr = srcSC.Open(fromPath); oerr != nil {
			return abort(fmt.Errorf("open source %s: %w", fromPath, oerr))
		}
		defer srcFile.Close()
	} else {
		var oerr error
		if srcLocal, oerr = os.Open(fromPath); oerr != nil {
			return abort(fmt.Errorf("open source %s: %w", fromPath, oerr))
		}
		defer srcLocal.Close()
	}

	// ---- stage 0 (首块前; 顺序钉死——一切 mutation 以 ⑧ 租约为前提) ----

	// 1. posix-rename 硬依赖探测 (零 IO 内存查找, 续传/自愈路径同样先行; rev4
	//    codex#3: 无防御性探针 rename——首跑空清单真实落盘即活探针)。不支持与
	//    IO 错误分流。
	if !spec.DstCli.RelayPosixRenameOK(dstSC) {
		return abort(errors.New("destination SFTP server lacks posix-rename@openssh.com — relay requires it for atomic manifest updates"))
	}

	// 2. MkdirAll 直调 (WriteFile 同款——不经 Client.MkdirAll 的 ToSlash)。
	parent := path.Dir(toPath)
	if merr := dstSC.MkdirAll(parent); merr != nil {
		return abort(fmt.Errorf("destination mkdir %s: %w", parent, merr))
	}

	// 3. fresh 删除 (malformed/超限清单同删——⑥ 对 fresh 已跳过解析); 删除后、
	//    任何数据移动前终检 StatVFS (rev4 codex#2: ⑦ 是投影值)。StatVFS 不可用
	//    → fail-open 知情继续 (与 ⑦ 同口径, grilling 拍板)。
	if spec.Fresh {
		for _, artifact := range []string{partialPath, manifestPath} {
			if rerr := dstSC.Remove(artifact); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
				return abort(fmt.Errorf("fresh discard %s: %w", artifact, rerr))
			}
		}
		need := relaySpaceNeed(spec.Size, min(spec.Size, spec.ChunkBytes))
		if avail, ok, aerr := spec.DstCli.RelayAvailable(dstSC, parent); aerr == nil && ok && avail < need {
			return abort(fmt.Errorf("destination has %d bytes available after the fresh discard but the transfer needs %d — nothing moved; free space and re-run", avail, need))
		}
	}

	// 4. 条件落空 manifest: 仅当 ⑥ 判定"无 manifest" (fresh 删除后/首跑)。续传
	//    场景绝不写 manifest——原样保留已完成块记录 (rev2 kimi#1/codex#2)。
	manifest := &sshbroker.RelayManifest{
		Version: 1, ChunkBytes: spec.ChunkBytes,
		SourceSize: spec.Size, SourceMtimeUnix: spec.Mtime,
		Chunks: []sshbroker.RelayChunkDone{},
	}
	if spec.Manifest != nil { // ⑥ 命中: 工作副本从已解析清单播种 (原结构不动)
		manifest.Chunks = append(manifest.Chunks, spec.Manifest.Chunks...)
	} else if werr := sshbroker.RelayWriteManifestAtomic(dstSC, manifestPath, manifest); werr != nil {
		return abort(fmt.Errorf("write empty manifest %s: %w", manifestPath, werr))
	}

	// 已完成块集 (循环跳过的依据; index→清单哈希)。
	completed := make(map[int]string, len(manifest.Chunks))
	for _, c := range manifest.Chunks {
		completed[c.I] = c.SHA256
	}

	// 5. 抽读复核 (先于一切既有 manifest 之外的数据移动): 最大 index 已完成块,
	//    源端 Seek+读+sha256 对清单; 不符 → failed, 零块移动、原 manifest 完好。
	if len(completed) > 0 {
		maxI := 0
		for i := range completed {
			if i > maxI {
				maxI = i
			}
		}
		h := sha256.New()
		if rerr := relayReadExact(srcFile, srcLocal, int64(maxI)*spec.ChunkBytes, relayChunkLen(int64(maxI), spec.ChunkBytes, spec.Size), h); rerr != nil {
			return abort(fmt.Errorf("spot re-check chunk %d: %w", maxI, rerr))
		}
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, completed[maxI]) {
			return abort(fmt.Errorf("source file changed since the interrupted transfer: chunk %d hashes to %s but the manifest records %s — nothing moved, manifest intact", maxI, got, completed[maxI]))
		}
	}

	// 6. 入场即以 O_RDWR|O_CREATE 建 partial (零块亦是; fresh 路径=重建; 自愈态=
	//    补建) + 尺寸收敛 (stale 尾巴清到 source_size)。
	pf, perr := dstSC.OpenFile(partialPath, os.O_RDWR|os.O_CREATE)
	if perr != nil {
		return abort(fmt.Errorf("open partial %s: %w", partialPath, perr))
	}
	pfClosed := false
	defer func() {
		if !pfClosed {
			_ = pf.Close() // 失败路径的 best-effort 收口 (半成品留在盘上等续传)
		}
	}()
	if pfi, serr := pf.Stat(); serr != nil {
		return abort(fmt.Errorf("stat partial %s: %w", partialPath, serr))
	} else if pfi.Size() > spec.Size {
		if terr := pf.Truncate(spec.Size); terr != nil {
			return abort(fmt.Errorf("truncate partial %s to %d: %w", partialPath, spec.Size, terr))
		}
	}

	// ---- 逐块 (index 升序, 跳过已完成) ----
	//
	// 双哈希 tee: chunkHash 每块重置; fileHash 全程不重置, 但仅当本任务从 byte 0
	// 连续流到 EOF 才有效——resumed==0 时循环无跳块、stage 0 无抽读 (无已完成块
	// 才会 resumed==0), 全部源读恰为 0→EOF 单程; resumed>0 时有跳块, 只报 merkle
	// 根 (判据 = 续传块数, 非 fresh 参数——自愈态空清单 resumed 恒 0, 同报)。
	fileHash := sha256.New()
	runBytes := int64(0) // 本任务实跑移动的字节 (速率分母)
	baseline := int64(0) // 续传基线 (已完成块字节, 洞感知)
	if spec.Manifest != nil {
		baseline = manifestCompletedBytes(spec.Manifest, spec.ChunkBytes, spec.Size)
	}
	for i := 0; i < int(spec.ChunksTotal); i++ {
		if _, ok := completed[i]; ok {
			continue // 续传: 已完成块跳过
		}
		off := int64(i) * spec.ChunkBytes
		n := relayChunkLen(int64(i), spec.ChunkBytes, spec.Size)
		chunkHash := sha256.New() // 每块重置
		// 复制形态钉死 (rev3 kimi#5): sftp 源走 T1 RelayCopyChunk (Seek+Seek →
		// CopyN over TeeReader(LimitReader), 精确长度纪律内建); 本机源走同形态
		// 的引擎本地镜像 (T1 签名把源钉在 *sftp.File, 见 helper 注释)。
		var written int64
		var cerr error
		if srcFile != nil {
			written, cerr = sshbroker.RelayCopyChunk(ctx, srcFile, pf, off, n, []io.Writer{chunkHash, fileHash})
		} else {
			written, cerr = relayCopyLocalChunk(ctx, srcLocal, pf, off, n, []io.Writer{chunkHash, fileHash})
		}
		if cerr != nil {
			return abort(fmt.Errorf("chunk %d at offset %d: %w", i, off, cerr)) // written≠n 不入清单
		}
		_ = written // 精确长度由复制原语保证 (short → error, 到不了这里)
		runBytes += n
		entry := sshbroker.RelayChunkDone{I: i, SHA256: hex.EncodeToString(chunkHash.Sum(nil))}
		completed[i] = entry.SHA256
		manifest.Chunks = append(manifest.Chunks, entry)
		sort.Slice(manifest.Chunks, func(a, b int) bool { return manifest.Chunks[a].I < manifest.Chunks[b].I })
		if uerr := sshbroker.RelayWriteManifestAtomic(dstSC, manifestPath, manifest); uerr != nil {
			return abort(fmt.Errorf("record chunk %d in manifest: %w", i, uerr))
		}
		elapsed := time.Since(startedAt)
		progress("chunk %d/%d ok bytes=%d/%d rate=%s elapsed=%s", i+1, spec.ChunksTotal, baseline+runBytes, spec.Size, relayRate(runBytes, elapsed), elapsed.Round(time.Millisecond))
	}

	// 末块 EOF 确认 (再读 1 字节须 EOF——源在传输窗口内变长的即时闸; 零块任务在
	// offset 0 处同样确认空源)。
	if rerr := relayReadExact(srcFile, srcLocal, spec.Size, 1, io.Discard); rerr == nil {
		return abort(fmt.Errorf("source did not end at %d bytes — the file grew during the transfer; refusing to continue", spec.Size))
	} else if !errors.Is(rerr, io.EOF) {
		return abort(fmt.Errorf("EOF confirm at %d: %w", spec.Size, rerr))
	}

	// ---- 完成 ----

	// 源 re-stat (72h 窗口内源变更的最终防线); 不符 → failed 不提交。残余 =
	// 同 mtime in-place 重写 (rsync 同病, 登记 §9)。
	var curSize, curMtime int64
	if srcSC != nil {
		fi, serr := srcSC.Stat(fromPath)
		if serr != nil {
			return abort(fmt.Errorf("source re-stat %s: %w", fromPath, serr))
		}
		curSize, curMtime = fi.Size(), fi.ModTime().Unix()
	} else {
		fi, serr := os.Stat(fromPath)
		if serr != nil {
			return abort(fmt.Errorf("source re-stat %s: %w", fromPath, serr))
		}
		curSize, curMtime = fi.Size(), fi.ModTime().Unix()
	}
	if curSize != spec.Size || curMtime != spec.Mtime {
		return abort(fmt.Errorf("source file changed since the transfer began (preflight size=%d mtime=%d, now size=%d mtime=%d) — refusing to commit", spec.Size, spec.Mtime, curSize, curMtime))
	}

	// rootHash = sha256(按 index 升序串接的各块 32B 摘要)——续传任务的校验凭据。
	sort.Slice(manifest.Chunks, func(a, b int) bool { return manifest.Chunks[a].I < manifest.Chunks[b].I })
	rootHash := sha256.New()
	for _, c := range manifest.Chunks {
		d, derr := hex.DecodeString(c.SHA256)
		if derr != nil || len(d) != sha256.Size {
			return abort(fmt.Errorf("corrupt chunk %d digest in manifest: %v", c.I, derr))
		}
		rootHash.Write(d)
	}

	// checked Close → PosixRename = 提交点 (此 rename 成功即传输完成; 尺寸收敛由
	// stage 0 truncate 保证)。
	if cerr := pf.Close(); cerr != nil {
		return abort(fmt.Errorf("close partial %s: %w", partialPath, cerr))
	}
	pfClosed = true
	if rerr := dstSC.PosixRename(partialPath, toPath); rerr != nil {
		return abort(fmt.Errorf("commit rename %s -> %s: %w", partialPath, toPath, rerr))
	}

	// manifest 删除 = best-effort: 失败 → 任务仍 done, 警告行指引手清 (§2)。
	if rerr := dstSC.Remove(manifestPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		progress("stale manifest left behind — harmless, remove at leisure")
	}

	// 终态 done 末行 (root 恒有; file_sha256 仅 byte0→EOF 全程任务, 判据 = resumed==0)。
	elapsed := time.Since(startedAt)
	if spec.Resumed == 0 {
		progress("relay done: root=sha256:%x file_sha256=sha256:%x(total=%d) renamed -> %s rate=%s elapsed=%s", rootHash.Sum(nil), fileHash.Sum(nil), spec.Size, toPath, relayRate(runBytes, elapsed), elapsed.Round(time.Millisecond))
	} else {
		progress("relay done: root=sha256:%x(total=%d) renamed -> %s rate=%s elapsed=%s", rootHash.Sum(nil), spec.Size, toPath, relayRate(runBytes, elapsed), elapsed.Round(time.Millisecond))
	}
	return 0, false, nil
}

// relayCopyLocalChunk 是本机源逐块复制的引擎本地镜像, 形态与 T1 RelayCopyChunk
// 逐字同款钉死 (rev3 kimi#5: Seek+Seek → io.CopyN over TeeReader(LimitReader) —
// 裸 io.Copy 读到 EOF 会把源剩余全灌进 partial 覆写后续块区域; written≠n 即
// error 的精确长度纪律同在)。不直接复用 T1 原语的原因: 其签名把源钉在
// *sftp.File, 而本机源是 *os.File——泛化 T1 签名会动到测试锚定的远端原语
// (rev3 kimi#5 锚); 引擎侧镜像让 sftp→sftp 路径留在 T1 的已测代码上, 本机路径
// 获得同一纪律。两路共享 relayChunkLen 网格与引擎的 EOF 确认, 漂移面为零。
func relayCopyLocalChunk(ctx context.Context, src *os.File, dst *sftp.File, offset, n int64, tee []io.Writer) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if _, err := src.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek src to %d: %w", offset, err)
	}
	if _, err := dst.Seek(offset, io.SeekStart); err != nil {
		return 0, fmt.Errorf("seek dst to %d: %w", offset, err)
	}
	written, err := io.CopyN(dst, io.TeeReader(io.LimitReader(src, n), io.MultiWriter(tee...)), n)
	if err != nil {
		return written, fmt.Errorf("chunk copy at offset %d stopped after %d of %d bytes: %w", offset, written, n, err)
	}
	if written != n { // CopyN 短拷贝必错——精确长度纪律的 belt and braces
		return written, fmt.Errorf("chunk copy at offset %d: %d of %d bytes", offset, written, n)
	}
	return written, nil
}

// relayReadExact 从源句柄 offset 处精确读 n 字节进 w (抽读复核与 EOF 确认共用;
// 短读/传输错误原样上抛——EOF 确认靠 errors.Is(err, io.EOF) 识别)。
func relayReadExact(srcFile *sftp.File, srcLocal *os.File, off, n int64, w io.Writer) error {
	var err error
	if srcFile != nil {
		if _, err = srcFile.Seek(off, io.SeekStart); err != nil {
			return fmt.Errorf("seek source to %d: %w", off, err)
		}
		_, err = io.CopyN(w, srcFile, n)
	} else {
		if _, err = srcLocal.Seek(off, io.SeekStart); err != nil {
			return fmt.Errorf("seek source to %d: %w", off, err)
		}
		_, err = io.CopyN(w, srcLocal, n)
	}
	if err != nil {
		return fmt.Errorf("read %d bytes at offset %d: %w", n, off, err)
	}
	return nil
}

// relayRate 渲染二进制单位速率 (进度行装饰, 非协议)。
func relayRate(bytes int64, d time.Duration) string {
	if bytes <= 0 || d <= 0 {
		return "0 B/s"
	}
	bps := float64(bytes) / d.Seconds()
	units := []string{"B/s", "KiB/s", "MiB/s", "GiB/s", "TiB/s"}
	u := 0
	for bps >= 1024 && u < len(units)-1 {
		bps /= 1024
		u++
	}
	return fmt.Sprintf("%.1f %s", bps, units[u])
}
