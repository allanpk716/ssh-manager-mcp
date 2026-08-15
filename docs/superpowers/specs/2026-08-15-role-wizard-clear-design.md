# 设计 spec — Plan 19：角色向导 + `clear` + 概念图解（v2）

> 日期：2026-08-15。状态：**v2 修订版**（v1 经 xcheck 三家异构评审 kimi/pi/codex 全部 SUGGEST_CHANGES，34 条意见吸收；修订记录见文末）。
> 范围：角色唯一化、首次向导（含 server 全闭环+客户端接入卡）、`clear` 全清命令、遗留定时器清理、概念模型图解文档。前置：Plan 17/18 已合并部署（v0.6.0）。

## 1. 角色模型

### 1.1 role.json

```json
{"role": "standalone" | "server" | "client", "setup_complete": false}
```

- **位置跟随角色数据目录**：standalone/server → vault 目录（Win `C:\ProgramData\ssh-manager\`）；client → 用户目录（Win `%AppData%\ssh-manager\`，与 cache.bin/cache.auth.json 同居——普通用户无需管理员权限）。
- `setup_complete`：向导完成标记。false → 下次 `tui` **重入向导续配**（幂等）。

### 1.2 启动判定链（v2：补齐护栏与异常态）

```
tui 启动：
 1. role.json 存在（先查 vault 目录，再查用户目录）：
      非法/损坏内容 → 报错引导 `ssh-manager clear`
      role ∈ {standalone,server} 且 vault 缺失   → 报错「数据被外部破坏，重跑向导需先 clear」
      role=client 且缓存缺失                      → 正常进 client 面板（未配置态，[c] 可配）
      setup_complete=false                        → 重入向导续配
      setup_complete=true                         → 直接进对应界面
 2. 无 role.json → 探测（与 v0.6.0 完全一致，含全部护栏）：
      vault 存在但锁定/不可读 → 报错提示 unlock（**不降级 client**）
      vault 存在且可读        → broker 模式
      有 cache（cred）        → client 模式
      全空                    → 首次向导
 3. vault 机器上 `tui --mode client` → 报错：
      「本机已有 vault（standalone/server 角色）。ssh-manager clear 将**删除本机全部 vault 数据（N 台服务器的所有凭据）**后才能转为 client。」
```

### 1.3 角色转换规则（v2 修正）

- **standalone → server = 非破坏性升级**（不是角色转换）：主控台在 standalone 角色时提示 `[u] 升级为 server（开启多机共享）` → 执行 serve install + 设备码签发引导，vault 数据原样保留，role.json 更新为 server。
- **vault 角色 → client** = 真正的角色转换，**必经 `clear`**。
- client → vault 角色 = 在 client 机直接跑向导（client 机无 vault，不冲突；clear 可先清 client 残留，向导开头检测到旧 client 数据时询问是否清理）。

## 2. 首次向导

### 2.1 首屏（v2：后果导向 + 人话注释）

```
第一次使用 ssh-manager

这台电脑要保管所有 SSH 凭据吗？
  [1] 是——凭据只存这台机（其他机器不能用了它就都用不了）
        └ 这台机器上的 agent 需要连别的电脑吗？
              [1a] 只有本机用        → 单机
              [1b] 要给其他机器共享   → server
  [2] 否——凭据在另一台机器上，这台只连它
        → client（需要先在 server 机完成设置）
```

选定角色的**瞬间**写 role.json（`setup_complete:false`）——此后任何中断都是安全暂停。

### 2.2 通用规则

- **可重入**：`setup_complete:false` 时重开 `tui` 自动回到向导，从对应角色步骤继续（步骤幂等：已存在的 vault/服务器跳过重建）。
- Esc = 暂停退出（状态保留），下次继续；无死态。
- 术语统一：UI 一律叫「**服务器指纹（pin）**」；client 表单的设备码栏**同时接受**裸码、`<码>:<指纹>` 合并串（自动解析）。
- 单机与 server 共享步骤 ②③④⑤；server 额外 ⑥⑦。

### 2.3 单机向导

② unlock 建 vault → ③ 录服务器（循环「继续添加？」，可全跳过）→ ④ profile（默认名=本机 hostname，重名自动 `-2` 后缀）+ 多选授权（跳过=空 profile，允许；UI 明示「agent 将看不到任何服务器」）→ ⑤ 建 project + **token 一次性展示**（带用途标签「贴到本机 .mcp.json 的 --token」+ 丢失重签指引「主控台 Projects 页 [a] 重发」）→ **.mcp.json 收尾屏**（完整片段，与 server 路径对称）→ setup_complete → 主控台。

### 2.4 server 向导

②③④ 同上（④ 的 profile 默认名=客户端名）→ ⑤ 客户端名（默认对方 hostname）+ 双密钥生成，**每屏独立展示、各带用途标签与去向**：

```
┌─ 密钥 1/2：project token ────────────────────┐
│ 用途：贴到 client 机 .mcp.json 的 --token 参数 │
│ ⚠ 仅此一次。丢失 → 主控台 Projects 页 [a] 重发  │
└──────────────────────────────────────────────┘
```

→ ⑥ serve 安装（v2）：
- **地址捕获**：列出本机全部非环网 IPv4 让用户选（多网卡场景），默认绑 `0.0.0.0:7878`；接入卡地址用**选定的 LAN IP 实值**。
- install 需管理员权限，**前置提示**；失败给出**可执行的原文命令**（含提权方式）。
- 安装后**自动探活**（本机 TLS GET /snapshot 期待 401/400）：通过 → 绿色「serve 已就绪」；失败 → 黄色警示「serve 未验证，client 可能连不上」+ 排查提示（端口/防火墙/服务状态），**不阻断**完成。

→ ⑦ **客户端接入卡**：地址实值（`https://<选定IP>:7878`）+ 服务器指纹 + **两密钥去向表**（哪个贴 .mcp.json、哪个填 client 向导）+ 命令式备选（`cache pull --token '<码>:<指纹>'`）+ 密钥不重显（各自⑤已展示，卡上只有去向说明）。→ ⑧ setup_complete → 主控台。

### 2.5 client 向导

- 表单**顶部来源提示**：「设备码与服务器指纹在 server 机 TUI 的『设备码』页签发」。
- 提交即首拉；**失败 → 回表单（保留已输入）**，分类文案：地址不通/401 设备码无效/指纹失配（证书可能已轮换）/超时。
- 成功 → **.mcp.json 收尾屏**（贴 server 机给的 project token，完整片段）→ setup_complete → client 面板。

## 3. `ssh-manager clear`（v2：时序重排 + 清单实枚举）

```
$ ssh-manager clear
本机角色：server
以下文件/组件将被永久删除（按实际存在枚举）：
  ▸ vault：store.db, store.db-wal, store.db-shm, master.key.plain（9 台服务器的全部凭据）
  ▸ serve：证书+私钥+init marker, serve.log, Windows 服务「ssh-manager-serve」（停止+卸载）
  ▸ 同机 client 残留：cache.bin, cache.auth.json, cache-dek.key, cache.meta.json, cache-audit.log
  ▸ 计划任务 ssh-manager-cache-refresh（若存在）
  ▸ role.json
输入 DELETE 确认：
```

**时序（v2 钉死）**：

1. 枚举实际存在文件 → 展示清单 → 输入 `DELETE`（输错/Ctrl+C = 取消，零改动）。
2. **vault 角色**：生成 export → **回读校验**（unmarshal 通过）→ 展示口令 + 「按 y 确认已抄录口令」→ **此时才开始执行删除**。export 生成/校验失败 = 中止零改动；vault 锁定 = 提示先 unlock（不提供无安全绳删除）；export 文件固定写到用户主目录 `ssh-manager-backup-<UTC时间戳>.sme`。
3. 执行删除：停服+卸载（失败=中止+提权重跑指引）→ 删清单文件 → 删 role.json → （client 角色）删定时任务。
4. **全程幂等**：任何一步失败后重跑 clear，已完成步骤跳过、未完成继续。

client 角色无 export（无 secret 可救），DELETE 后直接第 3 步。exe 永远保留；clear 后机器回到首次向导状态。

## 4. 遗留定时器清理

1. 笔记本本机已删（2026-08-15 执行完毕）。
2. clear(client) 删 `ssh-manager-cache-refresh` 计划任务（Windows，存在才删；Unix no-op——用户自建 unit 不碰）。
3. 文档：multi-machine.md「可选定时器」模板段标注 legacy。

## 5. 概念模型图解（文档）

内容（v2 补全）：
- 数据流图（server 机四页签 ↔ 设备码(水管)/token(阀门) ↔ client 机 cache.bin → agent）。
- **类比表补行**：服务器指纹（pin）= server 的防伪封条，client 首次连接必须核对；设备码 = 水管钥匙。**两种输入形态说明**（分开填 vs `<码>:<指纹>` 合并串等价）。
- 「第二台客户端完整操作链」：server 机 TUI 设备码页 [a] 签发（每台 client 一枚，吊销粒度=单机）→ Projects 页 [a] 新建 project 绑 profile → 发 token → client 机向导/`cache pull` → .mcp.json。设备码**不推荐复用**（失窃只能全吊）；project/token 按 agent 项目粒度自由建。
- 三份上手文档 + 向导首屏**前置引用**此节。

## 6. 测试要点（v2 扩充）

- role.json 全异常态矩阵（非法值/缺 vault/缺 cache/两处并存）。
- 向导重入：Esc 中断后重开 → 回向导续配；步骤幂等（重复执行不重建已有实体）。
- 首拉失败四类注入（地址/401/指纹/超时）→ 断言回表单+保留输入+文案正确。
- serve 探活失败注入 → 断言警示不阻断；LAN IP 多网卡枚举。
- clear：export 失败注入→零改动；service 卸载失败→中止+幂等重跑；清单与实存文件逐一对照；DELETE 前无任何文件改动。
- standalone→server 升级：vault 内容不变断言。

## 7. 边界与不做

- 不做 `~/.ssh/config` 批量导入；不改 mcp/cache/serve 运行时；不加网络端点。
- clear 不删 exe；Unix timer 不由程序删。
- 向导不覆盖 v0.6.0 已有 TUI 功能，只增量。

---

## 修订记录

- **v2（2026-08-15，吸收 xcheck 34 条）**：① 向导改「选角色即写 role.json + setup_complete + 可重入续配」，消灭 Esc 死态（kimi#1/pi#1/codex#1）；② standalone→server 改为非破坏升级 `[u]`，仅 vault→client 必经 clear（codex#2）；③ serve 安装补 LAN IP 捕获/0.0.0.0 绑定/admin 前置提示/装后探活/失败可执行指引，接入卡地址用实值（pi#2/codex#7/kimi#4）；④ 每个密钥屏带用途标签+重签指引，三路径统一 .mcp.json 收尾屏（kimi#2/#3、pi#7、codex#6）；⑤ client 向导失败路径+来源提示（kimi#5/pi#4）；⑥ 首屏后果导向+人话注释（kimi#8/pi#5/codex#8）；⑦ clear 时序重排（DELETE→export 校验→口令抄录确认→才删）、清单实枚举补 meta/wal/shm/同机 client 残留、幂等（codex#3/#4、pi#3/#6、kimi#11）；⑧ 判定链补 locked-vault 护栏与 role.json 异常态矩阵（codex#5/#11、pi#8）；⑨ role.json 跟随角色数据目录（kimi#6）；⑩ --mode client 报错明示删除后果（kimi#7）；⑪ 术语统一「服务器指纹（pin）」+ 合并串解析（kimi#10/pi#9/codex#9）；⑫ profile 命名规则（kimi#9/pi#7）；⑬ 跳过语义与空 profile 明示（codex#10）；⑭ 第二台客户端完整操作链+设备码复用建议（codex#12）。
- v1（2026-08-15）：grilling 三轮定稿初版。
