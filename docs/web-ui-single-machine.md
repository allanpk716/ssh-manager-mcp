# TUI 教程 · 单机版（sshmgr tui）

> **读者**：拿到 sshmgr 单机版 exe、想全程用键盘点选（不想记 CLI 命令）的人。
> 与 [quickstart-single-machine.md](./quickstart-single-machine.md)（CLI 速通）殊途同归——同一套
> vault 操作的两个入口。概念模型图解见 [concepts.md](./concepts.md)。

## 1. 启动

```bash
sshmgr tui
```

- Windows Terminal / cmd 原生可用。
- **mintty**（Git Bash 默认终端）不是 Windows 控制台，需加 winpty：`winpty sshmgr tui`。
- 在非 TTY 环境（重定向、CI）下启动会**直接报错**，不会挂死或乱码：

  ```
  tui requires a terminal (in mintty run via `winpty sshmgr tui`, or use Windows Terminal)
  ```

空机器第一次运行 `tui` 自动进入首跑向导；已完成的机器直接进主控台。同一套操作用 CLI 怎么做，见 [quickstart-single-machine.md](./quickstart-single-machine.md)。

## 2. 首跑向导走查（可中断续配）

向导首屏问的是**后果**，不是术语：

> **这台电脑要保管所有 SSH 凭据吗？**
> - 是——凭据只存这台机（其他机器不能用了它就都用不了）
> - 否——凭据在另一台机器上，这台只连它 → client（需先在 server 机完成设置）

单机场景选**是**。第二问：

> **这台机器上的 agent 需要连别的电脑吗？**
> - 只有本机用 → 单机
> - 要给其他机器共享 → server

选**只有本机用 → 单机**。（选「否」走的是 client 流程，属于多机部署，见 [quickstart-multi-machine.md](./quickstart-multi-machine.md)；client 面板的 `[c]` 配对向导与 Pairing 批准页走查见 [tui-multi-machine.md](./tui-multi-machine.md)。）

**选定的瞬间 role.json 就已落盘**（标记 setup 未完成）——此后**任何时刻** `q` / `Esc` / `Ctrl+C` 退出都是安全暂停，重跑 `tui` 从断点继续，不会重录已提交的数据。

选定单机后的流程：

1. **自动建 vault**（等价于跑一次 `sshmgr unlock`：生成 master key + 初始化加密库）。若本机已有**锁定**的 vault，向导不会覆盖它，而是报错引导：
   ```
   本机 vault 已存在但锁定或不可读：先运行 `sshmgr unlock`（向导不会覆盖既有 vault）
   ```
   此时按提示另开终端跑 `sshmgr unlock`，回来按 `r` 重试即可。
2. **「现在录入第一台服务器？」**——跳过是允许的（提示原文：跳过 = profile 暂无成员，agent 将看不到任何服务器；之后可在主控台随时补录）。
3. **服务器表单**（选「是」后）：
   - 基本信息：`名称（唯一）` / `Host / IP` / `SSH 用户` / `端口`（默认 22）；
   - 凭据：`密码（可选，与密钥二选一）` / `私钥路径（可选，与密码互斥…）` / `密钥口令（可选）` / `sudo 密码（可选）`；
   - 结构化备注：`硬件` / `位置` / `角色` / `服务` / `Caveats（agent 行动前必读）` / `备注`。

   要点：
   - **密码与私钥互斥**——两个都填会在提交时报「密码与私钥互斥：二选一」；也可以**都留空**（先记一台无凭据服务器，列表里标 ⚠，之后在主控台补）。
   - sudo 密码留空 = agent 不能跑 `sudo`；填了 agent 就能提权，按需取舍。
   - **结构化备注别放机密**——这些字段 agent 全程可见；保存时还会跑疑似机密扫描，发现疑似内容在状态行给 ⚠ 提示（只提示，不拦截、不回显内容）。
4. **「继续添加下一台服务器？」**——选是回到表单再录一台，选否进入下一步。
5. **Profile 名称 + 授权多选**：名称默认 = 本机主机名（重名自动加 `-2` 后缀）；服务器多选**空格勾选、回车提交**，提示原文：未选 = agent 暂时看不到任何服务器（一台没录时表单退化为只填名称）。
6. **项目名称**（默认 = 主机名；即发给 agent 的「通行证」身份，自动绑定刚才的 profile）。
7. **token 屏（一次性）**：明文 token + 用途行「贴到本机 .mcp.json 的 SSHMGR_TOKEN 字段」+「⚠ 仅此一次。丢失 → 主控台 Projects 页 [a] 重发」。**当场抄下来**——关闭后不可再看。
8. **`.mcp.json` 配置屏**：给出完整片段（token 位置是占位说明「上方已展示的 project token」——把第 7 步抄下的真 token 填进去）：

   ```json
   {
     "mcpServers": {
       "ssh": {
         "command": "sshmgr",
         "args": ["mcp"],
         "env": { "SSHMGR_TOKEN": "<TOKEN>" }
       }
     }
   }
   ```

   三条说明照做：单机角色用普通 `mcp` 启动（**不要**用 `--cache`，那是 client 角色的离线模式）；Windows 建议把 `command` 写成 exe 绝对路径；`.mcp.json` 含 token，**不要提交进 git**。
9. 按任意键**直接进入主控台**（无需重启）。

中断续配的恢复规则：重跑 `tui` 时向导会盘点已有数据——已有 profile + project 就直接跳到收尾屏（token 已在此前展示过，丢失走 Projects 页 [a] 重发）；只有 profile 就跳过服务器录入、从项目名继续；什么都没有才从头问。

## 3. 主控台四页签

`Tab` / `Shift+Tab` 循环切页；`↑` / `↓` 或 `j` / `k` 移动光标；`q` / `Ctrl+C` 退出；表单内 `Esc` 取消。单机角色的页脚另有 `[u]升级为 server`（多机化入口，见 [multi-machine.md](./multi-machine.md)）。

| 页签 | 键 | 动作 |
|---|---|---|
| 服务器 | `a` / `e` / `d` / `i` / `!` | 新增（凭据可选，都不填 = 无凭据 ⚠）/ 编辑（逐字段挑选编辑，「清除凭据」可退回无凭据态）/ 删除（确认；profile 授权一并失效）/ 导入 ssh config（见下）/ 只看 ⚠ 待处理机器（无凭据 / 未填角色 / 缺私钥口令；⚠ 机器始终排在列表最前，切换刷新后过滤保留） |
| Profiles | `a` / `g` / `d` | 新增 / 授权（多选=授权集，取消勾选并提交即移除）/ 删除（被项目引用时拒绝） |
| Projects | `a` / `e` / `d` / `x` | 新建（token 一次性显示，见下）/ 轮换 token / 吊销（永久生效）/ 彻底删除**已吊销**的记录 |
| 设备码（Cache Tokens） | `a` / `d` | 签发 / 吊销。**仅 serve 联机部署时有用，单机忽略本页签**——页签无条件存在，空着即可 |

## 4. 典型任务

### 给第二个 agent 发 token（Projects 页 [a]）

切到 Projects 页按 `a` → 填项目名称、选要绑定的 Profile → token **一次性全屏显示**。

屏上是完整可抄的 stdio 片段（旧的 http 双形态块已随 ②a 移除退役——serve 不再提供远程 MCP 面，多机 agent 走 `sshmgr pair` 配对）：

```
—— 本机/单机 agent（stdio）——
{
  "mcpServers": {
    "ssh": {
      "command": "sshmgr",
      "args": ["mcp"],
      "env": { "SSHMGR_TOKEN": "<TOKEN>" }
    }
  }
}
```

- **片段是真片段**：屏上显示的 JSON 里 token 已经代入（此处用 `<TOKEN>` 示意），抄完即用；
- 屏底固定提示：⚠ 仅此一次显示（关闭后不可再看）；丢失 → Projects 页 `[e]` 轮换换发（旧 token 立即失效）。

每个项目一把独立 token，给第二个 agent 就是再按一次 `a`（可绑同一 profile——授权范围相同，身份/吊销独立）。

### 加服务器 / 批量导入 ssh config

**单台**：服务器页 `a`，表单同向导第 3 步（密码/私钥互斥、都留空 = 无凭据 ⚠）。

**批量**：服务器页 `i` 导入 ssh config，四段流程：

1. **路径表单**：预填 `~/.ssh/config`，回车确认；
2. **候选多选**：全部预先勾选，空格增减；与 vault 冲突的（同名 / 同 host:port:user）已自动排除并计数；config 含 `Match` 块时会提示继承值可能与真实 ssh 不一致；
3. **批量导入**：同名密钥只建一份凭据、加密私钥带「缺密钥口令」⚠ 原样入库；
4. **逐台补全循环**（「补全 n/N」）：结构化字段 + sudo 密码，外加至多一个条件凭据——无凭据机器问「密码（现在设置）」、缺口令机器问「密钥口令（补全加密私钥）」；**`Esc` 跳过保留 ⚠**（之后处理）、`q` 结束整个补全、回车提交进下一台。

结果屏给「导入 / 跳过 / 失败 / 待补」四个数。有待补机器时：回主界面按 **`!`** 只看 ⚠ 列表，逐台 `e` 编辑补齐。

### 轮换 token（Projects 页 [e]）

光标停在目标项目上按 `e` → 确认「轮换 "xxx" 的 token？（旧 token 立即失效）」→ 新 token 以同款一次性片段屏展示。把新片段更新进 agent 的 `.mcp.json` 即完成换发；怀疑 token 泄露时这是标准处置。吊销不再用的项目按 `d`（永久生效，不可恢复）。

## 5. 排错

| 症状 | 处置 |
|---|---|
| mintty 下启动即退出/乱码 | mintty 不是 Windows 控制台：`winpty sshmgr tui`，或改用 Windows Terminal / cmd |
| 非 TTY 下启动直接报错 | 这是预期行为（防挂死）；换真终端再跑 |
| 向导报「本机 vault 已存在但锁定或不可读」 | 先跑 `sshmgr unlock`，回到向导按 `r` 重试 |
| 向导中途退出了 | 什么都不用做：重跑 `sshmgr tui` 自动从断点续配 |
| 向导/补全的输入框打不进字母 `q` | `q` 被全局拦截为退出键（既有取舍）；需要输入含 q 的内容（如密码），先在别处写好再粘贴 |
| 误按 `q` 退出了主控台 | 无任何丢失，重新 `sshmgr tui` |
| 表单填错想放弃 | 表单内 `Esc` 取消（不提交）；`q` / `Ctrl+C` 退出整个程序 |

## 6. 安全面

- **凭据输入全程掩码**：密码 / 私钥口令 / sudo 密码的输入框一律密文显示。
- **已设凭据只显示「已设置（输入新值以更换）」**，从不回显旧值；未设则显示「未设置」。
- **token / 设备码一次性全屏显示**：关闭后不可再查（库里只存哈希，明文无法恢复），丢失的唯一出路是轮换/重发。
- `.mcp.json` 含 token，**不要提交进 git**（发 token 的每一屏都带这条提示）。
- 疑似机密扫描只给 ⚠ 提示，不回显、不拦截——但根子上的纪律是：结构化备注里**本来就不该放机密**。

## 相关文档

- CLI 视角的完整流程与排错：[getting-started.md](./getting-started.md)
- token 生命周期（轮换 / 吊销 / 设备码）：[agent-access.md](./agent-access.md)
- 服务器增删改查与批量导入：[managing-servers.md](./managing-servers.md)
- 概念模型图解（vault / profile / project / 设备码谁是谁的）：[concepts.md](./concepts.md)
- 联机版 TUI 教程（server 侧 + 工作机 client 面板）：[tui-multi-machine.md](./tui-multi-machine.md)
- 给 AI agent 的工具手册（可贴进 CLAUDE.md 的规则模板）：[agent-tools.md](./agent-tools.md)
- 多机部署（serve + 设备码的用武之地）：[quickstart-multi-machine.md](./quickstart-multi-machine.md)、[multi-machine.md](./multi-machine.md)
