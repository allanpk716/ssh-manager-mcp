# Plan 23 设计+实施计划：超限上传预检拒绝（单任务，spec/plan 合一）

> 日期：2026-08-16。OWNER 拍板：选项 B（预检拒绝）。来源：Plan 21 T4 + Plan 22 终审两轮升级的设计题。

## 背景

现契约（三处测试+五处文档锁死）：单文件原子上传跑完不中断；`Truncated` 累计超 cap 翻真；超限后不再开始下一文件——**孤立超限文件完整落盘**。cap（§6 资源预算）对单文件无约束力。

## 新契约（唯一语义变更）

- **上传前 `os.Stat` 本地文件：单文件 size > cap → 拒绝该文件，零字节传输**，返回错误（含文件名、实际大小、cap 值）。多文件场景：走到超限文件时拒绝并停止（此前已完成的文件保留——与现 walk-halt 语义一致）。
- 恰好 == cap 的文件：**允许**（拒绝条件是严格大于）。
- 单文件原子性不变（没开始就没半截）；owner 全权路径（`ssh-manager ssh`/TUI）不受影响。
- 恰好超限后 Truncated 语义保留用于「累计跨过 cap 的多文件场景」？——**不保留歧义**：简化为——预检拒的是「单文件本身超 cap」；累计超 cap 但每个文件都 ≤ cap 时，现 walk-halt+Truncated 语义**原样保留**（多小文件凑超总量仍是完整文件+Truncated 报告）。两层正交。

## 任务（单任务）

**Files:**
- Modify: `internal/sshbroker/upload.go`（Upload 入口单文件 + uploadDir 每文件 io.Copy 前预检）
- Test: `internal/sshbroker/upload_test.go`（翻转 T4 锁定的两文件用例之一 + 新拒绝对策）
- Test/Modify: `internal/mcpserver/core_test.go`（Plan 6 时代的 per-file-atomic 测试 :577-613 须改为拒绝语义）
- Modify: `internal/conformance/upload_forward_test.go`（boundary-cap 子测试：孤立 cap+1 → 拒绝+远端零字节+scp 对照）
- Modify 文档五处：`internal/mcpserver/server.go` 工具 Description、`internal/mcpserver/types.go` jsonschema、`internal/eval/README.md`、`docs/eval/phase3.md`、`docs/scenarios.md`（Plan 22 刚改的措辞再改为「超限单文件传输前拒绝」）

**步骤：**
1. 失败测试先行：单文件 cap+1 → error 含 "exceeds upload cap" 且远端零字节；== cap → 成功；dir 场景小+超限 → 小文件落盘、错误点名超限文件。
2. 实现：uploadDir/uploadFile 在 Copy 前 `os.Stat` + `size > cap` → `fmt.Errorf("file %s (%d bytes) exceeds upload cap %d — refused before transfer (already-completed files remain)", ...)`（exported 常量引用 cap；错误文案走 `mcpserver.MaxOutputBytes` 对齐处已有先例——注意 sshbroker 不能 import mcpserver（反向），cap 由调用方传入的现有参数路径不变）。
3. 翻转既有锁：sshbroker 两文件用例、mcpserver per-file-atomic 用例、conformance boundary 子测试——全部改为断言新语义（错误+零字节/保留已传）；genuinely 每条在报告列明改写理由。
4. 文档五处措辞更新（与错误文案一致：「单个文件超过上限会在传输前被拒绝；多个文件累计超限时已完成的保留并如实标记 Truncated」）。
5. 验证：全量 + `SSHMGR_CONFORMANCE=1 go test ./internal/conformance/ -run TestUploadDifferential -count=1`。

## 边界

- 不做 per-call 提额旋钮（D 留作日后）；不动 download 的 cappedBuffer（下载侧无原子性问题）；不改 cap 常量值。
