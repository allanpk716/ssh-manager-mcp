# 设计 spec — Plan 19：角色向导 + `clear` + 概念图解

> 日期：2026-08-15。状态：设计定稿（grilling 三轮：Round1 角色模型/clear 护栏、Round2 向导细节/定时器、Round3 server 闭环+概念对齐）。
> 范围：角色唯一化、首次向导（含 server 全闭环+客户端接入卡）、`clear` 全清命令、遗留定时器清理、概念模型图解文档。前置：Plan 17（缓存自动保鲜）、Plan 18（TUI 主控台）已合并部署（v0.6.0）。

## 1. 角色模型

- 新文件 `C:\ProgramData\ssh-manager\role.json`（VaultDir；Unix `/var/lib/ssh-manager/`）：`{"role":"standalone"|"server"|"client"}`。
- `tui` 启动判定链：**role.json 存在 → 遵从**（不再探测）；不存在 → 探测（有 vault → broker 模式；有 cache → client 模式；都无 → 首次向导，向导完成时写 role.json）。
- 存量部署（NUC10/笔记本）无 role.json，走探测回退，行为与 v0.6.0 完全一致——零迁移。
- vault 机器上：向导永不出现；`tui --mode client` 报错并指向 `ssh-manager clear`（**角色转换必经 clear**，保证「有且只有一个方向」）。
- 角色只影响 tui 呈现/向导/清理；`mcp`/`cache`/`serve` 等子命令运行时行为零改动。

## 2. 首次向导（tui 内，全空机器触发）

```
第一次使用 — 这台机器的角色？
  [1] 单机   [2] 联机 → [2a] server  [2b] client
```

- **单机**：unlock 建 vault →（可跳过）录服务器表单 →（可跳过）profile 多选 + project 生成 → 一次性 token 展示 → 写 role.json → 进主控台。
- **server**（一条龙，每步可跳过、Esc 退出不写 role.json）：
  ① unlock 建 vault → ② 录服务器（循环「继续添加？」）→ ③ **「这台客户端的 agent 可用哪些服务器？」多选**（自动建 profile 装勾选集）→ ④ 给客户端起名（默认 hostname）→ 生成 project token + 设备码（**各自一次性展示**，设备码含指纹）→ ⑤ 自动 `serve install`（失败打印手动命令）→ ⑥ **客户端接入卡** → 写 role.json → 进主控台。
- **client**：复用现有连接表单（地址/设备码/pin）→ 提交即首拉 → 成功写 role.json → 进 client 面板。
- **客户端接入卡**（⑥，最终一屏）：client 机安装指引 + 交互式（`tui` 向导填 地址/设备码/指纹）与命令式（`cache pull --url … --token '<码>:<指纹>'`）两条路 + `.mcp.json` 的 `--token <project token>` 配置片段。token/设备码不重显（各自生成时已展示），卡内只含非敏感项（地址/指纹/命令模板）+ 「token 见上一步展示」提示。

## 3. `ssh-manager clear`（CLI，高危仪式）

- 判定本机角色（role.json > 探测），**按角色**列删除清单：
  - **standalone/server**：serve 服务停止+卸载（若装）→ **自动 export 安全绳**（随机口令，口令显示一次，加密文件写在 vault 目录之外如用户主目录）→ 删 vault 全家（store.db / master.key.plain / serve-cert.pem / serve-key.pem / init marker / serve.log）→ role.json。
  - **client**：删 cache.bin / cache.auth.json / cache-dek.key / cache-audit.log → **删除 `ssh-manager-cache-refresh` 计划任务**（Windows `schtasks /Delete`，存在才删；Unix no-op）→ role.json。
- 确认：显示清单+安全绳位置 → **要求输入 `DELETE`**（大小写敏感）；输错/Ctrl+C/Esc = 取消，零改动。
- exe 本体永远保留；clear 后机器回到「第一次」状态（下次 `tui` 出向导）。
- 仅 CLI 入口；TUI 不放按钮（误触风险），文档提及命令名。

## 4. 遗留定时器清理（三处）

1. 笔记本本机立即删 `ssh-manager-cache-refresh`（部署时执行，不等开发）。
2. `clear`(client) 删同名任务——Windows-only 实现，Unix no-op 并注释（用户自建 unit 文件，程序不碰）。
3. 文档：multi-machine.md「可选定时器」模板段标注 legacy（v0.5.0+ 进程内自动保鲜取代）。

## 5. 概念模型图解（文档，本轮新增的核心动机之一）

新文档节（README TUI 章节附近或独立 `docs/concepts.md` 并交叉链接），内容：

```
┌─────────────── server 机（唯一 vault）───────────────┐
│  服务器页：所有 SSH 机器（货架）                        │
│  Profiles 页：服务器集合（装箱单）──┐                  │
│  Projects 页：客户端凭证（钥匙）──绑─┘ 一把钥匙开一个箱  │
└──────────────┬───────────────────────┬───────────────┘
        缓存同步(设备码=水管)      agent 调用(project token=阀门)
               ▼                       ▼
┌────────── client 机 ─────────────────────────────────┐
│ cache.bin(加密全量快照) → mcp --cache --token <钥匙>    │
│ → agent 只看见钥匙对应的箱子里的服务器                   │
└──────────────────────────────────────────────────────┘
```

- 类比表（货架/装箱单/钥匙/水管/阀门）、token vs 设备码职责对比、
  「第二台客户端要不同集合怎么办」（Projects 页新建绑不同 profile）操作路径。
- 三份上手文档与 README 交叉链接到此节。

## 6. 测试要点

- role.json 读写/回退探测/`--mode client` 在 vault 机报错（三态单测，复用 T2 的探测测试基建）。
- 向导状态机：纯函数步骤流转 + 每步跳过/Esc 中断不写 role.json。
- clear：角色判定清单生成、确认词门槛、export 安全绳生成、client 定时器删除（Windows 计划任务 API 的可测封装）、取消路径零改动。
- 概念文档：随功能一并评审措辞准确性。

## 7. 边界与不做

- 不做 `~/.ssh/config` 批量导入（另立的迁移领域）。
- 不改 mcp/cache/serve 运行时行为；不加新网络端点。
- clear 不删 exe、不删 OS 级 ssh-manager 以外的任何东西；Unix 的 systemd timer/launchd plist 不由程序删除（文档指引）。
