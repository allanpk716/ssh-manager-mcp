# Plan 36 · audit CLI 读路径 设计 spec(定稿)

- 日期:2026-08-25 · 状态:**定稿**(brainstorm 设计经 3 轮异构评审收敛,rev3 免复审——owner 拍板)
- 修订链:设计 v1 → rev1(第 1 轮 8 条:过滤器语义钉死/--owner 补口/--since 三字面量/时区带偏移/控制字符转义/JSON 空值契约/碰撞取舍/limit 0 警告)→ rev2(第 2 轮 9 条:spec 自包含/统一转义全部动态字段/转义集加反斜杠+C1+bidi/负 limit 报错/人读行加 server 列/limit 0 运行时警告/§4③ 承诺限定人读/exit_code 歧义注记)→ **rev3**(第 3 轮 9 条:截断可见性/json.Marshal 契约事实修正/golden 编码对比/store 层负 limit 二道闸/零宽字符转义/NULL server 渲染 (none)/相对时长形态钉死/未知枚举零校验/name UNIQUE 引用[证伪条零成本补引])。第 3 轮 owner 突破 C5 硬上限;R3 零高项、唯一真 bug 为 rev2 自引入措辞,终态收敛。
- backlog:P1 #16(owner-only 审计读路径;「明确不做 MCP 工具」为其原文裁决)
- Owner 拍板记录(2026-08-25 brainstorm):①--project 过滤器增补(取证第一轴);②--since 相对+绝对双语法;③输出 = 人读默认 + --json。xcheck 决策链:R2 后突破 C5 跑第 3 轮;R3 后 rev3 定稿免复审。

---

## §0 背景与现状

vault 主库 `audit_log` 表只写不读:agent 四工具(exec/download/upload/forward)、owner CLI 动作(project.rotate/disable/enable/revoke/delete)、交互式 `ssh` 都落行,但唯一读路径 `Store.AuditRows(limit)` 是测试构件(14 处调用方全是测试)——owner 无法回答「agent 昨晚在这台机器干了什么」,手开加密库不现实。

**代码现状事实**(实现与评审对照用,均可在仓库验证):

1. `audit_log`:`id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, project_id TEXT, server_id TEXT, action TEXT NOT NULL, command TEXT, sudo INTEGER NOT NULL DEFAULT 0, status TEXT, exit_code INTEGER, duration_ms INTEGER`。**主键自增整数 → `ORDER BY id DESC` = 插入序 = newest-first,确定性成立**。无 retention/GC(登记债),无二级索引(id PK 序即可)。
2. 扫描用 `sql.NullString`/`NullInt64` → Go 零值归一化(SQL NULL → ""/0)。
3. **project_id 为空 = owner(非 agent)动作;server_id 空 = 无 server 语境(如 project 级操作)**。status 取值随版本演进(bind_denied 为 Plan 35 新增先例)。
4. 离线只读 store 的审计走 sidecar JSONL:九字段 `map[string]any` 恒写、零值照写、`json.Marshal` 编码,单向零合并。
5. vault CLI 统一走 `openUnlockedStore()`(master key 门槛 = owner 门槛);`tunnels kill --project` 已有 name|id 解析先例。
6. 实体 id = `newID()`(11 字符随机 base64url);**`servers.name`/`projects.name` 均 `TEXT NOT NULL UNIQUE`(store.go:423/454)——name 精确命中恒 ≤1,解析无歧义**。

**目标**:owner-only 审计读路径;**明确不做 MCP 工具**(审计行含其他 agent 完整命令原文、可能带 secret,给 agent 读 = 把同伙作案记录递给被劫持 agent——backlog 原文裁决)。

**验收(backlog 原文)**:过滤组合断言 + 无 MCP 暴露面。

## §1 命令面

`ssh-manager audit`(root 直挂单命令,同既有 `gc` 形态;走 `openUnlockedStore()`):

| flag | 语义 |
|---|---|
| `--since <值>` | 相对时长 = 单一「整数或小数 + 恰一个单位」(`30m`/`1.5h`/`7d`/`2w`;复合链 `1h30m` 拒);或绝对时间——固定顺序尝试:①RFC3339 全形(带 `Z`/±hh:mm)②本地无偏移 datetime `2026-08-20T09:00:00`(本地时区)③纯日期 `2026-08-20`(本地 00:00)。皆不中 = 坏值,报错文案逐一列出全部合法形态。`ts >= since`;缺省 = 全历史 |
| `--server <name\|id>` | name 精确命中 → 用其 id(schema UNIQUE 保证恒 ≤1 命中);未命中 → 原串**直接作为过滤值**。**零存在性校验**:查无匹配 = 空结果(exit 0)不报错——已删 server 历史 audit 行天然可按旧 id 过滤。可重复/逗号(StringSlice → SQL IN)。name 优先(tunnels kill 先例) |
| `--project <name\|id>` | 同 --server 款解析 |
| `--owner` | 布尔 flag:只选 project_id 为空的行;与 `--project` 同给 = 报错(组合语义恒空,提前拒绝) |
| `--action <v>` / `--status <v>` | 精确匹配,可重复/逗号 → IN。未知值**零校验静默空**(与过滤器族一致;取值随版本演进,前向兼容);README 附已知取值参考表 |
| `--limit N` | 默认 100,`0` = 不限,负值 CLI 层报错(`--limit must be >= 0`)。help 注明「0 = 全量输出;audit_log 无自动清理,大库慎用」 |
| `--json` | JSONL 输出(§3) |

输出 newest-first(`ORDER BY id DESC`)。空结果:人读打一行 `no matching audit rows`(exit 0);`--json` 静默零行(不污染管道)。

**运行时提示(stderr,人读模式专属)**:
- `--limit 0` 全量查询前一行警告(JSON 模式 stdout 纯净不警);
- **截断可见性**:内部取 `limit+1` 行,命中截断(返回 > limit)→ stderr 一行 `showing first N rows (more exist) — use --limit 0 for full output`;恰好 limit 条不提示;`--json` 不警(程序化消费自设限额)。

## §2 store 层

`AuditFilter{Since int64 /*unix 秒,0=不限*/, ServerIDs []string, ProjectIDs []string, OwnerOnly bool, Actions []string, Statuses []string, Limit int}` → 新方法 `QueryAudit(f AuditFilter) ([]AuditRow, error)`:

- 动态 WHERE + 全占位符(零字符串拼接 SQL);
- 行扫描从 `AuditRows` 抽共享 helper,`AuditRows` 签名行为不动(14 个既有调用方零涟漪);
- **`Limit < 0` 在 store 层直接报错**(`limit must be >= 0`)——SQLite `LIMIT -1` = 不限,CLI 拒绝之外的第二道闸(导出方法可能被未来调用方/测试直调)。

## §3 解析与输出

### 人读行

```
2026-08-25 09:15:32+08:00  server-name  proj-name  exec  ok  exit=0  123ms  [sudo]  systemctl status nginx
```

- 时间戳 = 本地时区**且行内带偏移**(`2006-01-02 15:04:05-07:00`,自描述,跨机对照无歧义)。
- **渲染四态**:project_id 空 → `(owner)`;project/server 有 id 但名解析不到(已删)→ `id 前 8 字符…(deleted)`;server_id 空(无 server 语境)→ server 列 `(none)`;正常 → 名字。
- command **不截断但控制字符转义**(下)。

### 转义函数(统一应用于人读行全部动态文本字段:command、project/server 显示名、action、status)

| 输入 | 输出 |
|---|---|
| `\` | `\\`(保可逆) |
| ASCII 控制字符 <0x20 与 0x7f | `\n`/`\t`/`\r` 字面名;其余 `\xNN` |
| C1 控制区 U+0080-U+009F、U+2028/U+2029 | `\uXXXX` |
| bidi 控制(U+200E/U+200F/U+202A-U+202E/U+2066-U+2069) | `\uXXXX` |
| 不可见格式化字符(U+00AD、U+200B-U+200D、U+2060、U+FEFF) | `\uXXXX` |
| 其余非 ASCII(含中文) | 原文 |

性质:**可逆**(转义集封闭:反斜杠自身已转义)、行边界恒单行、终端控制序列注入关闭、不可见字符显示欺骗关闭。威胁主体注记:command 为 agent 可控;name 类为 owner 自设——统一转义消灭对「名称安全」的假设依赖。

### --json 行

- 九字段(`ts/project_id/server_id/action/command/sudo/status/exit_code/duration_ms`)**恒出现、零值照写、无 null 无 omitempty**。
- nullable 归一化:SQL NULL 扫描为 Go 零值(NullString→""/NullInt64→0)后恒写。
- **编码契约 = Go `json.Marshal` 实际行为**:强制转义 <0x20、`<`、`>`、`&`、U+2028/U+2029;无效 UTF-8 替换为 U+FFFD;**0x7f 原样**。不自行定义编码规则。
- 实现注记:--json 行复用 sidecar 同款 `map[string]any` 九字段构造 → 键序两侧一致(json.Marshal map 字典序),「与 sidecar 逐字段逐编码一致」由构造机制保证。
- JSON 是数据路径,终端直灌建议走 jq/文件;exit_code 零值歧义(「无退出码的失败」vs「exit 0 判失败」同显 0)README 留痕。

## §4 安全论证(owner-only 的机制保证)

1. 数据在加密 vault 内,CLI 走 `openUnlockedStore()`——**master key 门槛即 owner 门槛**,无需发明权限位;
2. 不注册任何 MCP 工具——测试**断言 server 工具枚举集合不含 audit**(「无 MCP 暴露面」验收可测化);
3. 命令原文只到 owner 终端 stdout;**人读模式经 §3 转义**(全部动态字段,含注入与显示欺骗防护);--json 为数据路径;`audit` 命令自身不写审计行(只读动作,与「owner 低价值动作不进 agent 审计表」既有取舍同精神);
4. 离线 sidecar(cache-audit.log)不读不聚合(backlog 原文;其本身是 JSONL,cat/grep 可用)。

## §5 不做(scope 纪律)

- MCP 工具 / 跨端聚合(等真实取证需求)。
- `--follow`(tail -f)。
- audit_log retention/GC(**登记为已知债**:无界增长真实存在,属独立裁决)。
- TUI 审计页(CLI only)。
- 输出脱敏/掩码(owner 看全量);audit 自审计。
- `--project-id`/`--server-id` 强制按 id 入口(name 优先 + 零校验下无实际需要)。
- 流式/游标输出(单次 CLI 输出,`[]AuditRow` 物化是既有模式,大库用 --limit 自护)。
- JSON 路径的终端安全承诺(数据路径,jq/文件消费)。
- `--json` 模式截断/全量警告(stdout 纯净)。

## §6 测试矩阵

| # | 对象 | 断言 |
|---|---|---|
| 1 | store 过滤 | 五维(since/server/project/action/status)+ OwnerOnly 各单独 + 任意组合 + 空结果;newest-first(乱序 ts 插入仍按插入序返回);已删实体旧 id 直配命中 |
| 2 | store limit | 默认 100、0=不限、**负值 store 层报错** |
| 3 | CLI --since | 三种绝对字面量正例(RFC3339 带 Z/带偏移/本地无偏移/纯日期)+ 相对正例(含小数 `1.5h`)+ 复合 `1h30m` 拒 + 坏值报错文案含全部合法形态 |
| 4 | CLI 解析 | server/project name 命中 → id;未命中 → 原串直配(不报错、空结果);可重复 flag IN 语义;--owner 选 owner 行;--owner + --project 同给报错;未知 action/status 静默空 |
| 5 | CLI limit | 负值 CLI 报错;`--limit 0` 人读 stderr 警告、--json 无;默认 limit=100 生效;**截断:命中时人读 stderr 提示、恰好 limit 条不提示、--json 不警** |
| 6 | 人读渲染 | 四态((owner)/(deleted)/(none)/名字)+ 时间戳带偏移 + server 列;注入构造行(ESC 序列、真换行、字面 `\n`、U+009B、bidi、零宽/FEFF/soft hyphen)输出单行、字面 `\n` 与真换行显示可区分、对应 `\uXXXX`/`\xNN` 转义;动态字段注入(name 含 ESC 的 server)同被转义 |
| 7 | --json | 可 Unmarshal;字段集合恰九个;owner 行零值非缺失(project_id=="" 而非 absent);**golden 编码对比:构造含控制字符/`<>&`/U+2028/2029/无效 UTF-8 的 audit 行 → `--json` 输出与 sidecar `WriteAudit` JSONL 逐字节一致** |
| 8 | MCP 面 | server 工具枚举断言无 audit 工具 |
| 9 | 回归 | 全量 `go test ./...` |

## §7 文档联动

- `README.md`:命令清单 + 用法例(转义说明、--limit 0 警告、server 列、exit_code 零值歧义、已知 action/status 取值参考表、截断提示说明)。
- `docs/multi-machine.md`:注记「权威 broker 上跑才有全量 agent 历史」(serve 拓扑下 agent 动作审计行落在权威端 vault;各机 CLI 只读本机 vault)。
- `docs/threat-model.md`:审计相关段落补 owner 读路径一句(audit CLI 是 owner 面工具,永不入 MCP 工具面)。
- compat-matrix 不登记(只读新 CLI、零契约变更)。

## §8 任务拆分预览(writing-plans 骨架)

1. **store 层**:`AuditFilter` + `QueryAudit`(动态 WHERE 全占位符、扫描 helper 抽取、负 limit 二道闸)+ 单测(矩阵 1/2)。
2. **CLI**:`audit` 命令(flags/解析:--since 双语法三字面量+时长形态/name 解析/互斥/负值;转义函数;渲染四态;--json map 构造;截断探测 limit+1;两处 stderr 提示)+ 单测(矩阵 3-7)。
3. **面与文档**:MCP 工具枚举断言(矩阵 8)+ README/multi-machine/threat-model 联动 + 全量回归(矩阵 9)。
