# Plan 33 设计：upload_content 跨机小文件上传

> backlog #14 · P0。2026-08-22 grilling 已拍板的决策不在本文重议：**encoding 参数**（text 默认 + base64 可选，与 Plan 32 exec_output 同构）、**覆盖写**（目标已存在即截断重写，sftp.Create / upload_file 同语义）、**失败留半写**（不做 temp+rename 原子替换，清理归调用方——upload_file 现有语义）、**env seam**（`SSHMGR_UPLOAD_CONTENT_MAX`，缺省 8 MiB，fail-closed；upload_file 的 §6 1 MiB/文件维持不动）、**serve 请求体上限收口**（http.MaxBytesReader）、**审计 action="upload-content" 且 Command 含字节数、内容零入审计**。本文为实现设计。
> 本版为第四版（2026-08-24 三轮收敛修订，owner 突破评审硬上限定稿）。二版吸收首轮 7 项（粗筛扣 padding、WriteFile Close 语义、remote_path 校验、两处已知边界登记、==cap 边界用例、两读点表述）；三版吸收二轮 6 项（描述动态 cap、WriteFile 吸收父目录创建、text 契约 U+FFFD 口径、理由句精确化、413 阈值一般式、padCount 计数对象）；本版吸收三轮 8 项：**MkdirAll 纯 POSIX path.Dir**（不经宿主 filepath.ToSlash——反斜杠是远端合法文件名字符，Windows broker 上会误转）、**base64 钉死单行 padded StdEncoding**（含 \r/\n 即参数层拒绝——换行开销归零，body 上限推导严格成立，owner 拍板）、审计 %d 失败分支值表、**env cap 上限 1 GiB**（超限拒绝启动 + body limit 检查算术，owner 拍板）、「up to」措辞残留、U+FFFD 全链用例、精判分支不可达口径、并发写留痕。初版遗留项：upload_file 的 Close 不查错、MkdirAll 无 ctx、MkdirAll 内 ToSlash 三条为登记债务不回补（见 §8）。

## 0. 目标与缺口

`upload_file` 的 `local_path` 是 **broker 本机路径**。笔记本 agent → NUC10 serve 拓扑下，agent 无法把**自己持有的内容**（配置/脚本/小产物）推到目标机——`download_file` 是内容回传、天然跨机可用，上行却断了：上下行不对称，S1（配置下发）场景残缺。

`upload_content`：内容内联（JSON 入参）写入远程路径。定位与边界（backlog 已裁决）：覆盖配置/脚本/小产物（≤8 MiB）；大文件分块续传 API 明确不做——更大的先落到 broker 可达位置再 `upload_file`，或服务器侧拉取。`download_file` 维持 1 MiB 前缀截断（大文件全文进 agent 上下文是反模式），切片读法（exec head/tail/grep）由 agent-tools.md 承担。

## 1. 工具契约

`upload_content` = **BrokerTools[9]**（第 10 个工具；`internal/mcpserver/server.go` 单源切片追加 + NewServer 对应 `mcp.AddTool`——eval scorer 读同一切片，集合断言自动联动为 10）。

### 1.1 入参 / 出参

```go
// types.go
type UploadContentInput struct {
    ServerID   string `json:"server_id" jsonschema:"server id from list_servers"`
    Content    string `json:"content" jsonschema:"the file content to write (valid UTF-8 text; invalid UTF-8 bytes are replaced with U+FFFD — pass base64 here with encoding=base64 for exact bytes)"`
    RemotePath string `json:"remote_path" jsonschema:"absolute destination path on the server (must start with /); its parent directory is created if missing; an existing file is overwritten"`
    Encoding   string `json:"encoding,omitempty" jsonschema:"how content is encoded: 'text' (default — the JSON-decoded string, written as UTF-8; NOT byte-exact: invalid sequences are already replaced with U+FFFD by JSON decoding) or 'base64' (decode first — exact bytes; SINGLE-LINE standard base64 with padding — CR/LF inside content is rejected). The cap applies to the DECODED byte count"`
}

type UploadContentOutput struct {
    Bytes int64 `json:"bytes" jsonschema:"bytes written to the remote file (the decoded byte count)"`
}
```

- **text 契约（rev2，对齐 Plan 32 exec_output 口径）**：text 模式写入的是 **JSON 解码后的字符串**——客户端送来的非法 UTF-8 字节在 JSON 解码层已被替换为 U+FFFD（Go encoding/json 公开行为，非本工具引入），**text 模式不承诺字节精确**；字节精确（二进制/GBK/任意字节串）必须走 base64。此口径与 exec_output 的 encoding 描述逐句同构，agent 零新概念。
- **base64 格式钉死（rev3，owner 2026-08-24 拍板）**：**单行、standard（StdEncoding 字母表）、带 padding**；content 含 `\r` 或 `\n` → 参数层直接拒绝（错误文本 `base64 content must be single-line standard base64 with padding — join lines and resend`，不含原文片段）。多行（MIME 折行）base64 不容忍：换行开销归零使 §3.2 body 上限对 base64 的覆盖**严格成立**，且 §2.1 粗筛无需换行特判。
- **encoding 枚举在 `UploadContentForProfile` 内校验**（Plan 32 先例：SDK 反射 jsonschema 表达不了 enum；非法值 → 显式错误拒绝，非 text/base64 一律拒）。
- **remote_path 参数层校验**（rev1）：非空且以 `/` 开头，否则拒绝——与 schema 描述对齐；相对路径现状会相对 SFTP cwd（登录 home）落盘（upload_file 同现状，core.go:397 守卫跳过 `.`），本工具显式拒绝而非沿用未定义行为。
- **空内容合法**：`content=""` 写出 0 字节文件（truncate 到 0）；base64 空串解码同为空。
- **超限 = refusal 错误，不是 truncated**（与 upload_file 预检拒绝同构，见 §2）：错误文本自带 size/cap 证据，零字节移动、零远程文件创建。
- 超限不设 `truncated` 回显路径——本工具没有「部分成功」形态。

### 1.2 Agent 描述文本（模板钉死，cap 动态嵌入）

描述在 `NewServerFromSource` 构造时生成（该点已解析 cap，零额外成本），`%d` 填**解析后的实际 cap**——env seam 调整后描述如实反映，不给 agent 错误契约：

> Upload inline content as a file on a server — the cross-machine path (upload_file reads from the broker's own filesystem; use upload_content to push content YOU hold). Pass the server's id (from list_servers) + the content + the absolute destination path (must start with /; parent directories are created; an existing file is overwritten). encoding: 'text' (default, UTF-8 — invalid sequences are replaced with U+FFFD, not byte-exact) or 'base64' (exact bytes — SINGLE-LINE standard base64 with padding; use it for binary, non-UTF-8 or byte-exact content). Capped at %d bytes decoded — larger payloads are refused before transfer; for bigger files place them where the broker can reach and use upload_file. No sudo: root-owned paths are not writable. Concurrent writes to the same path are not atomic — avoid racing another upload. On failure the remote file may be left partially written — verify and clean up yourself.

## 2. 执行序（钉死）

```
UploadContentForProfile(ctx, st, projectID, profileID, serverID, content, remotePath, encoding string, cap int64)
    (out UploadContentOutput, err error)        —— 放 core.go（UploadForProfile 同文件）；encoding 缺省空串 = text

handler(ForProfile 内):
  ① 参数层校验：encoding 枚举（text|base64，缺省空串 = text）+ remote_path 非空且以 `/` 开头
     + base64 单行校验（encoding=base64 且 content 含 \r/\n → 拒绝，见 §1.1）
     （参数层只反映调用方自身输入、不含任何目标 server 派生信息，故先于 gate；②的 denied 优先原则
       约束的是**内容级**错误——越权探测不得因 cap/base64 错误得到差异化回显，参数层不在此列）
  ② profile gate（denied）
  ③ cap 预检（见下；拒 → 审计 error 行 + 错误回 agent，未 connect）
  ④ GetServer → AuthForServer（no_credential）→ HostKeyTOFU → Connect（connect_error/hostkey_mismatch/cancelled，Plan 31 源头清洗继承）
  ⑤ cli.WriteFile(ctx, remotePath, reader)（SFTP 覆盖写；**父目录创建吸收在内**（rev2）——同一条
     sftp client、同一 watchdog ctx 覆盖，见 §2.2；①已保证绝对路径，parent 恒非 "."）
  ⑥ 审计 + 返回 {bytes}
```

### 2.1 cap 预检（connect 前，零字节移动零远程文件）

- **encoding=text**：`len(content) > cap` → 拒绝。
- **encoding=base64**（单行已由 ① 保证，无换行特判）：
  - **粗筛（解码前，零大分配）**：`est = len(content)/4*3 − padCount`（`padCount` = content **尾部** `=` 个数，规范 padding 1–2 个）。est 是解码字节数的**精确值**而非上界（rev3 起 content 恒单行，无 stripped 概念）。`est > cap` → 拒绝。粗筛的存在理由：**stdio 无 §3.2 body cap**——一条 1 GB 的 base64 入参若不粗筛要先解码出 ~750 MiB；粗筛在解码前挡掉。serve 侧请求体已被 §3.2 先兜，粗筛在其上再省一次注定拒绝的解码。非规范 padding（如中间 `=`）由解码报错兜底。
  - **精判（解码后）**：`len(decoded) > cap` → 拒绝。**该分支在公开输入空间不可达（rev3 口径）**：est 对一切解码器会接受的输入恒等于 decoded，粗筛已拒——精判保留为**防御代码**（防未来格式放宽/实现回归），不设公开触发用例（§7）。
- 拒绝错误文本（粗筛/精判同一形态——两值恒等，rev3 删「up to」残留）：
  - text：`content (%d bytes) exceeds upload-content cap %d — refused before transfer`
  - base64：`content (%d bytes decoded) exceeds upload-content cap %d — refused before transfer`
- **base64 解码失败**（非法字符/padding）→ 拒绝，错误文本 `invalid base64 content: %v`（只带 decoder 错误，**绝不带原文片段**）。

### 2.2 写入与失败语义

`sshbroker.Client.WriteFile(ctx context.Context, remotePath string, r io.Reader) error`（**新增**，upload.go）——**父目录创建吸收在内（rev2）**，全程序列同一条 sftp client、同一 watchdog 覆盖：

1. `sftp.NewClient` → watchdog（ctx.Done 关 sftp client 解卡 in-flight 操作，Upload 同款）；
2. `sc.MkdirAll(path.Dir(remotePath))`（**纯 POSIX `path.Dir`，不经宿主 `filepath.ToSlash`（rev3）**——反斜杠在远程 POSIX 路径是合法文件名字符，Windows broker 上 ToSlash 会把 `/tmp/a\b` 误转 `/tmp/a/b` 建错父目录且行为随 broker OS 漂移；upload_file 既有 `Client.MkdirAll` 内的同款 ToSlash 为登记债务，见 §8）；
3. `sc.Create(remotePath)`（已存在即截断）→ `io.Copy`；
4. **Close 语义钉死（rev1）**：`out.Close()` 必须**显式调用且检查**——成功路径 = io.Copy 无错**且** Close 无错才返回 nil；Close 错误按写入失败处理（返回 `fmt.Errorf("close: %w", err)` 形态）。SFTP 写入的最终失败可能到 Close 才暴露（flush/整包落定），不检查 Close 可能把未完整落盘的写返回 ok。（upload_file 既有路径 `defer out.Close()` 不查错为登记债务，见 §8——本工具不沿用该模式。）

**失败（含取消）留半写文件/已建父目录，清理归调用方**——与 upload_file / scp 语义完全一致，两工具不分岔。**同路径并发写不保证原子性/最终一致性（rev3 留痕）**：两个并发 upload_content 写同一 remote_path 会交错截断/写入（SFTP 无锁语义，与 upload_file 同病非新增）——配置下发恰是易并发覆写场景，agent 描述与 agent-tools.md 均留痕「避免对同路径并发上传」。text 模式 `strings.NewReader(content)` 直灌（零额外拷贝）；base64 模式灌解码结果。连接一次 `Connect` + `defer cli.Close()`（upload/download 同款，即用即关，不占 TunnelManager/TaskManager）。

## 3. env seam 与 serve 请求体上限

### 3.1 SSHMGR_UPLOAD_CONTENT_MAX（fail-closed + 上限）

- 同一解析函数 `resolveUploadContentCap() (int64, error)`（mcpserver 包内，env 名 `SSHMGR_UPLOAD_CONTENT_MAX`，字节数，缺省 `8 << 20`）：**不可解析 / 非正 / 大于 `1 << 30`（1 GiB，rev3 owner 拍板）→ 错误**。上限理由：默认的 128× 已极宽（再大改代码）；同时保证 §3.2 的 `cap×4/3+64 KiB` 远离 int64 溢出，且 owner 不会因误设巨值把 serve body limit 联动放大到失控（§6 登记该联动）。
- 接线点（两处，各 fail-closed 先于对外服务）：
  - `NewServerFromSource` 调用并随构造失败拒绝启动（TaskManager 的 SSHMGR_BG_* 同款单点——stdio / --cache 两模式全经此构造）；cap 值存入 server 闭包传给 `UploadContentForProfile`，**并嵌入 Agent 描述模板（§1.2）**。
  - **serve 模式**：`NewServeRunner` 构造时调同一函数读一次存字段（签名改 `NewServeRunner(st) (*ServeRunner, error)`），`RunServe` 在 **bind 之前**失败退出——不能出现「serve 已监听但首个 MCP 请求才 503」的半死态。
- **两读点口径（rev1 修正）**：serve 进程内该 env 经同一解析函数读**两次**（NewServeRunner 存 body-limit 用字段 + NewServerFromSource 存工具闭包用值）——进程运行期 env 变更**不热生效**，两值均取启动时快照，重启后自然一致；「零漂移」仅指各自内部不再重复读 env。

### 3.2 MaxBytesReader（serve HTTP 链收口）

实测事实（2026-08-22，SDK v1.2.0）：streamable handler 对请求体是无上限 `io.ReadAll`（streamable.go:359/1010）。引入 8 MiB 级请求体后这是显式 DoS 面（持有效 token 者 POST 巨 body 占内存）。

- `HTTPHandler` 的 **mcpChain 外层**包 body-limit 中间件（在 token 门之外层、`/snapshot` 是 GET 不裹）。
- 上限 = serve 存值（§3.1）`× 4 / 3 + 64 KiB`（覆盖 base64 展开 + JSON 包装 + 头部余量），与内容上限**同源联动**（改 env 两个上限一起动，无独立旋钮；联动放大已在 §6 threat-model 登记）。**计算用检查算术（rev3）**：以 `cap + cap/3 + 64 KiB` 形态或显式溢出检查实现——§3.1 的 1 GiB 上限下本无溢出可能，双保险防未来上限放宽回归。
- base64 覆盖**严格成立（rev3）**：base64 字母表无需 JSON 转义、单行钉死后无换行开销——贴 cap 的合法 base64 线上体恒 ≤ cap×4/3 + 包装余量。
- **已知边界（rev1 登记、rev2 阈值精确化，owner 2026-08-24 拍板接受）**：**text 模式**下 JSON 字符串转义（`"`/`\` 2×、控制字符 `\uXXXX` 最高 6×/字节）的**线上平均膨胀超过 4/3 即可触发早拒**——不是只有极端形态才中（以 8 MiB cap 为例：全 2× 转义内容 >~5.6 MiB、控制字符 6× 内容 >~1.8 MiB 即 413；~48 MiB 只是 6× 全覆盖的形态上界）。被 413 的内容解码后可能 ≤ cap。真实场景（配置/脚本——平均膨胀通常 <1.1）几乎不命中，拒绝是干净的 413 无副作用；agent-tools.md 留痕指引（极端转义/二进制内容走 base64）。不为该边界放大上限（6× = 把 DoS 面放大到 ~48 MiB/请求，得不偿失）。
- 两级行为（如实钉死）：
  - **Content-Length 诚实超限** → 中间件直接 `413 Request Entity Too Large`（真 MCP 客户端单 JSON POST 通常带 Content-Length，此为实际路径）。
  - **无/谎报 Content-Length（chunked 攻击）** → `http.MaxBytesReader` 兜底，SDK 的 ReadAll 中途报错、返回错误响应（非 413——SDK 占写响应；可接受，攻击者拿不到工具执行）。
- **stdio 不加 cap**（留痕）：stdio 对端是本机 agent 进程，非网络 DoS 面；本地恶意进程的 DoS 面远不止 stdin。

## 4. 资源口径

- **常驻新增：零**（无状态工具，无表、无 goroutine）。
- **瞬时内存**：峰值 ≈ 2× 解码后内容 + SDK 内部请求副本 ≈ 8 MiB cap 下 ~27 MiB/请求。粗筛预检（§2.1）保证 8 MiB cap 下不会为注定拒绝的请求做解码分配。
- **已知接受残余风险（rev1 登记，owner 2026-08-24 拍板）**：MaxBytesReader 是**单请求体**粒度；已认证客户端可并发发送多个近上限请求，**并发聚合内存无全局上界**。接受理由：现状 serve 对所有工具响应本就是同款无界并发（既有环境属性，非本特性新引入）；并发主体（token → project → agent）受 owner token 生命周期管控；加全局 semaphore/内存约束是新机制，超本 plan scope（纯增量口径）。
- **连接**：每次调用一条即用即关的 SSH 连接（upload/download 同款，无复用池——与现有工具一致，不新增长连面）。

## 5. 审计与 no-leak

- `action="upload-content"`（独立于 "upload"，audit CLI #16 按 action 过滤可区分两工具）；`Command = "inline %d bytes -> %s"`（remote_path）。**%d 分支值表（rev3 钉死）**：ok / text 拒 = `len(content)` / base64 粗筛拒 = `est`（恒等 decoded）/ base64 精判拒（不可达防御分支）= `len(decoded)` / base64 解码失败、单行拒绝、encoding/remote_path 参数非法 = `0`。
- **内容零入审计**（可能含 secret——零容忍）：测试反向断言——写入 secret 形态内容后扫 audit 表**无内容片段**。
- statuses 与 upload 全同：`denied / auth_error / no_credential / hostkey_mismatch / connect_error / cancelled / ok / error`。cap 预检拒绝、base64 非法、参数校验失败（encoding/remote_path/单行）、WriteFile（含父目录创建与 Close）失败均归 `error`。每分支一行（deferred WriteAudit，现有同款）。
- **no-leak 继承**：connect 错误经同一 `sshbroker.Connect`（Plan 31 源头 redactAddr 清洗）；SFTP 层错误为路径/原因文本（permission denied / no such file 等），无地址形态预期。no-leak 断言网扩本工具全部错误分支兜底（含 SFTP 中途失败路径）。

## 6. 文档变更

- **agent-tools.md**：upload_content 完整口径——跨机不对称缺口补齐（upload_file 是 broker 本机路径）、base64 承载二进制与字节精确（text 的 U+FFFD 非字节精确口径、base64 单行要求）、8 MiB 解码后上限、失败留半写自查、root 属主路径不可写（SFTP 无 sudo）、**同路径并发写不保证原子性（rev3）**、大文件指引（broker 可达位置 + upload_file / 服务器侧拉取）、**极端转义/控制字符内容走 base64 的留痕**（§3.2 已知边界：text 转义膨胀 >4/3 的贴上限内容可能在 serve 侧被 413 早拒）。
- **threat-model.md**：§6（1 MiB 输出/传输封顶）加注——upload_content 独立 8 MiB 的理由：内容已在 agent 上下文中（不新增读取面/上下文膨胀，与 download 方向相反），上限 env seam 可调且 fail-closed（**含 1 GiB 硬上限与「body limit 随该 env 同源联动放大」的登记**，rev3）；serve 请求体收口（MaxBytesReader）登记为传输层加固，text 转义早拒与并发聚合内存两处已知边界/残余风险一并登记。
- **compat-matrix.md**：新增工具属纯增量；v0.10.0 尚未发版，并入 v0.10.0 行还是开 v0.11.0 行**留 owner 发版时拍板**（先占位注释，Plan 32 同款）。
- **README / agent-access / scenarios（S1 配置下发）/ differences-ledger**（upload_content 无 ssh 二进制直接对应物——`cat > file` stdin 近似但不等同，登记 Broker-specific）同步。

## 7. 测试矩阵

- **单测（mcpserver；UploadContentForProfile 测试随 core_test.go 同层扩展，serve 中间件测试进 serve_test.go）**：
  - 两态编码落盘字节精确：UTF-8 文本；二进制 fixture（含 0x00/0xFF/GBK 字节串）经 base64 往返 `exec cat`/下载比对。
  - **text 模式 U+FFFD 边界（rev2；rev3 补全链）**：①ForProfile 层——传已含 U+FFFD 形态 string，断言落盘即该文本；②**serve 层全链（rev3）**——httptest POST 构造含非法 UTF-8 字节的原始 JSON 入参 → SDK 解码 → U+FFFD → 落盘断言（锚「替换发生在 JSON 解码层」的契约，防 SDK 未来行为变化静默破约）。
  - cap 预检拒绝：text 超 / base64 超（粗筛拒；**精判分支注明公开输入空间不可达、不设公开用例（rev3）**，精判代码保留为防御）——断言**零远程文件**（testsshd 上目标不存在）+ 错误文本含 size/cap 且无「up to」残留形态。
  - **decoded == cap 边界成功用例（rev1）**：text 与 base64 各一个——内容字节数**恰好等于 cap** 必须成功落盘（锚粗筛精确值：base64 用例构造为带尾 padding、`len/4*3` 虚高 > cap 而扣 padding 后 == cap）。
  - base64 非法拒绝（错误文本无原文片段断言）+ **多行 base64 拒绝（rev3）**：content 含 `\n` → 参数层拒绝、错误文本含单行指引。
  - 空内容 → 0 字节文件（两态编码各一）。
  - **父目录创建（rev2 收进 WriteFile）**：深层新路径落盘成功（WriteFile 单测层覆盖；**含反斜杠文件名用例（rev3）**——remote_path 含 `\` 的合法 POSIX 名，断言父目录/文件按原样创建不经 ToSlash 误转）。
  - remote_path 参数层拒绝（空串/相对路径两用例，先于 gate——两错并有时返回参数错误而非 denied）。
  - denied 分支（越权 server_id）+ 审计行格式断言（action/Command 模板 + **%d 分支值表抽查（rev3）**：粗筛拒行含 est、参数非法行 0）+ **内容不落审计反向断言**。
  - 错误分支 no-leak（connect_error 等文本无 host:port）。
  - encoding 非法值拒绝。
  - **Agent 描述动态 cap 断言（rev2）**：以非默认 `SSHMGR_UPLOAD_CONTENT_MAX` 构造 server，断言工具描述含实际 cap 值。
- **sshbroker**：WriteFile 单测（内容/父目录创建/截断已有文件/取消留半写/Close 错误按写入失败处理——testsshd）。
- **e2e（e2e_test.go）**：工具集合等式 9→10 更新 + upload_content 全流程（含 parent 创建）。
- **serve（serve_test.go / 新）**：Content-Length 超限 → 413；贴上限（cap 内 base64 满载）→ 工具调用成功；MaxBytesReader 兜底路径（谎报/无 Content-Length 超限 → 错误响应，不 413，如实断言）。
- **conformance（真 OpenSSH，双重门控同款）**：upload_content base64 二进制 round trip——上传后 `exec sha256sum` 与本地比对；覆盖写（已有文件被截断重写）。
- **eval**：BrokerTools 单源联动使集合断言自动含第 10 工具；新增 upload_content agent 用例 + scorer（用例覆盖：跨机上传小配置→exec 验证内容）。
- **env seam 解析单测**：`SSHMGR_UPLOAD_CONTENT_MAX` 非法/非正/>1 GiB 三态拒绝 + 合法值接受（fail-closed 锚）。

## 8. 明确不做（scope 纪律）

- **大文件分块续传 API**（backlog 已裁决；8 MiB 内联覆盖目标场景，更大的走 broker 可达位置或服务器侧拉取）。
- **append / create-new 语义位**（覆盖写已拍板；防误覆盖 agent 可先 `exec test -e` 自查）。
- **temp+rename 原子替换**（已拍板直接写；与 upload_file 语义不分岔）。
- **upload_file §6 1 MiB 联动调整**（两者理由不同：本地源=传输入保护，内联=agent 已持有内容；互不动）。
- **download 上限变动**（维持 1 MiB 前缀截断）。
- **stdio 请求体 cap**（非网络面，§3.2 留痕）。
- **目录上传**（content 是单文件语义；目录走 upload_file）。
- **text 转义早拒边界的上限放大**（§3.2 已知边界，owner 拍板接受 + 文档留痕；不为极端形态放大 DoS 面）。
- **并发聚合内存全局约束**（§4 已知残余风险，owner 拍板登记接受；超本 plan scope）。
- **多行（MIME 折行）base64 容忍**（rev3 钉死单行，owner 2026-08-24 拍板；换行开销归零换取 body 上限推导严格成立与规格简化）。
- **同路径并发写互斥/加锁**（SFTP 无锁语义，与 upload_file 同病非新增；留痕「避免并发覆写」指引，不加机制）。
- **upload_file 既有债回补**（登记三条：①upload.go:117 `defer out.Close()` 不查错；②core.go:397-398 独立 `Client.MkdirAll` 无 ctx；③upload.go:260 MkdirAll 内 `filepath.ToSlash` 对远端 POSIX 路径的跨平台误转——均为既有 plan 遗留，与本 plan 无关；upload_content 新路径已钉死正确语义（Close 检查 + 父目录同 watchdog + 纯 path.Dir），旧路径留 backlog 债务登记，不在本 plan 顺手改）。

## 9. 验收与发版注记

- **自动化**：§7 全绿（含超限拒绝分支、decoded==cap 边界用例、U+FFFD 全链用例与多行 base64 拒绝——backlog 验收原文）。
- **跨机端到端（owner 手工，backlog 验收原文）**：笔记本 agent → NUC10 serve → 目标机：upload_content 下发配置 → 目标机 `exec cat` 验证；二进制走 base64 同验。
- **发版**：纯增量（新工具 + serve 中间件 + 文档）。v0.10.0 未发版——并入 v0.10.0 或开 v0.11.0 由 owner 发版时拍板（§6）。发版后 compat-matrix 回写删占位注释。
