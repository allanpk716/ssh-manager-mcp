# Plan 33 设计：upload_content 跨机小文件上传

> backlog #14 · P0。2026-08-22 grilling 已拍板的决策不在本文重议：**encoding 参数**（text 默认 + base64 可选，与 Plan 32 exec_output 同构）、**覆盖写**（目标已存在即截断重写，sftp.Create / upload_file 同语义）、**失败留半写**（不做 temp+rename 原子替换，清理归调用方——upload_file 现有语义）、**env seam**（`SSHMGR_UPLOAD_CONTENT_MAX`，缺省 8 MiB，fail-closed；upload_file 的 §6 1 MiB/文件维持不动）、**serve 请求体上限收口**（http.MaxBytesReader）、**审计 action="upload-content" 且 Command 含字节数、内容零入审计**。本文为实现设计。
> 本版为第二版（2026-08-24 吸收首轮外部评审 7 项证实条目）：粗筛公式扣 padding（消除 ==cap 误拒）、WriteFile 钉死 Close 错误语义、remote_path 参数层校验、text 转义 × body 上限与并发聚合内存两处登记为 owner 拍板接受的已知边界/残余风险、§7 补 decoded==cap 边界用例、两读点表述修正。初版遗留项：upload_file 的 Close 不查错为登记债务不回补（见 §8）。

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
    Content    string `json:"content" jsonschema:"the file content to write (UTF-8 text); for binary or non-UTF-8 content pass base64 in this field and encoding=base64"`
    RemotePath string `json:"remote_path" jsonschema:"absolute destination path on the server (must start with /); its parent directory is created if missing; an existing file is overwritten"`
    Encoding   string `json:"encoding,omitempty" jsonschema:"how content is encoded: 'text' (default — UTF-8 bytes written as-is) or 'base64' (decode first — exact bytes). The cap applies to the DECODED byte count"`
}

type UploadContentOutput struct {
    Bytes int64 `json:"bytes" jsonschema:"bytes written to the remote file (the decoded byte count)"`
}
```

- **encoding 枚举在 `UploadContentForProfile` 内校验**（Plan 32 先例：SDK 反射 jsonschema 表达不了 enum；非法值 → 显式错误拒绝，非 text/base64 一律拒）。
- **remote_path 参数层校验**（rev1）：非空且以 `/` 开头，否则拒绝——与 schema 描述对齐；相对路径现状会相对 SFTP cwd（登录 home）落盘（upload_file 同现状，core.go:397 守卫跳过 `.`），本工具显式拒绝而非沿用未定义行为。
- **空内容合法**：`content=""` 写出 0 字节文件（truncate 到 0）；base64 空串解码同为空。
- **超限 = refusal 错误，不是 truncated**（与 upload_file 预检拒绝同构，见 §2）：错误文本自带 size/cap 证据，零字节移动、零远程文件创建。
- 超限不设 `truncated` 回显路径——本工具没有「部分成功」形态。

### 1.2 Agent 描述文本（钉死）

> Upload inline content as a file on a server — the cross-machine path (upload_file reads from the broker's own filesystem; use upload_content to push content YOU hold). Pass the server's id (from list_servers) + the content + the absolute destination path (must start with /; parent directories are created; an existing file is overwritten). encoding: 'text' (default, UTF-8) or 'base64' (exact bytes — use it for binary or non-UTF-8 content). Capped at 8 MiB decoded — larger payloads are refused before transfer; for bigger files place them where the broker can reach and use upload_file. No sudo: root-owned paths are not writable. On failure the remote file may be left partially written — verify and clean up yourself.

## 2. 执行序（钉死）

```
UploadContentForProfile(ctx, st, projectID, profileID, serverID, content, remotePath, encoding string, cap int64)
    (out UploadContentOutput, err error)        —— 放 core.go（UploadForProfile 同文件）；encoding 缺省空串 = text

handler(ForProfile 内):
  ① 参数层校验：encoding 枚举（text|base64，缺省空串 = text）+ remote_path 非空且以 `/` 开头
     （纯解析层、不触任何 server 派生信息，先于 gate 无泄露面）
  ② profile gate（denied 优先于 cap/base64 等**内容级**错误——越权探测不得因内容参数错误得到差异化回显）
  ③ cap 预检（两级，见下；拒 → 审计 error 行 + 错误回 agent，未 connect）
  ④ GetServer → AuthForServer（no_credential）→ HostKeyTOFU → Connect（connect_error/hostkey_mismatch/cancelled，Plan 31 源头清洗继承）
  ⑤ cli.MkdirAll(path.Dir(remotePath))（upload_file 同款 scp --parents UX；path.Dir 非 filepath.Dir——远端恒 POSIX；①已保证绝对路径，parent 恒非 "."）
  ⑥ cli.WriteFile(ctx, remotePath, reader)（SFTP 覆盖写，Close 语义见 §2.2）
  ⑦ 审计 + 返回 {bytes}
```

### 2.1 cap 两级预检（connect 前，零字节移动零远程文件）

- **encoding=text**：`len(content) > cap` → 拒绝。
- **encoding=base64**：
  - **粗筛（解码前，零大分配）**：`stripped` = content 剔除 `\r`/`\n` 后的长度；`est = stripped/4*3 − padCount`（**padCount = 尾部 `=` 个数**，规范 base64 尾 padding 为 1–2 个；Go base64 解码忽略换行、尾部 padding 参与量化——扣掉后 est 即解码字节数的**精确值**而非上界，`==cap` 边界零误拒）。`est > cap` → 拒绝。粗筛的存在理由：**stdio 无 §3.2 body cap**——一条 1 GB 的 base64 入参若不粗筛要先解码出 ~750 MiB；粗筛在解码前挡掉。serve 侧请求体已被 §3.2 先兜，粗筛在其上再省一次注定拒绝的解码。非规范 padding（如中间 `=`）由后续解码报错兜底，粗筛无需处理。
  - **精判（解码后）**：`len(decoded) > cap` → 拒绝。
- 拒绝错误文本（两级同构，自含证据、不含原文片段）：
  - text：`content (%d bytes) exceeds upload-content cap %d — refused before transfer`
  - base64 粗筛：`content (up to %d bytes decoded) exceeds upload-content cap %d — refused before transfer`
  - base64 精判：同 text 形态（%d = 解码后字节数）。
- **base64 解码失败**（非法字符/padding）→ 拒绝，错误文本 `invalid base64 content: %v`（只带 decoder 错误，**绝不带原文片段**）。

### 2.2 写入与失败语义

`sshbroker.Client.WriteFile(ctx context.Context, remotePath string, r io.Reader) error`（**新增**，upload.go）：`sftp.NewClient` → watchdog（ctx.Done 关 sftp client 解卡 in-flight 写，Upload 同款）→ `sc.Create(remotePath)`（已存在即截断）→ `io.Copy` → **Close 语义钉死（rev1）**：`out.Close()` 必须**显式调用且检查**——成功路径 = io.Copy 无错**且** Close 无错才返回 nil；Close 错误按写入失败处理（返回 `fmt.Errorf("close: %w", err)` 形态）。SFTP 写入的最终失败可能到 Close 才暴露（flush/整包落定），不检查 Close 可能把未完整落盘的写返回 ok。（upload_file 既有路径 `defer out.Close()` 不查错为登记债务，见 §8——本工具不沿用该模式。）**失败（含取消）留半写文件，清理归调用方**——与 upload_file / scp 语义完全一致，两工具不分岔。text 模式 `strings.NewReader(content)` 直灌（零额外拷贝）；base64 模式灌解码结果。连接一次 `Connect` + `defer cli.Close()`（upload/download 同款，即用即关，不占 TunnelManager/TaskManager）。

## 3. env seam 与 serve 请求体上限

### 3.1 SSHMGR_UPLOAD_CONTENT_MAX（fail-closed）

- 同一解析函数 `resolveUploadContentCap() (int64, error)`（mcpserver 包内，env 名 `SSHMGR_UPLOAD_CONTENT_MAX`，字节数，缺省 `8 << 20`）：**不可解析 / 非正 → 错误**。
- 接线点（两处，各 fail-closed 先于对外服务）：
  - `NewServerFromSource` 调用并随构造失败拒绝启动（TaskManager 的 SSHMGR_BG_* 同款单点——stdio / --cache 两模式全经此构造）；cap 值存入 server 闭包传给 `UploadContentForProfile`。
  - **serve 模式**：`NewServeRunner` 构造时调同一函数读一次存字段（签名改 `NewServeRunner(st) (*ServeRunner, error)`），`RunServe` 在 **bind 之前**失败退出——不能出现「serve 已监听但首个 MCP 请求才 503」的半死态。
- **两读点口径（rev1 修正）**：serve 进程内该 env 经同一解析函数读**两次**（NewServeRunner 存 body-limit 用字段 + NewServerFromSource 存工具闭包用值）——进程运行期 env 变更**不热生效**，两值均取启动时快照，重启后自然一致；「零漂移」仅指各自内部不再重复读 env。

### 3.2 MaxBytesReader（serve HTTP 链收口）

实测事实（2026-08-22，SDK v1.2.0）：streamable handler 对请求体是无上限 `io.ReadAll`（streamable.go:359/1010）。引入 8 MiB 级请求体后这是显式 DoS 面（持有效 token 者 POST 巨 body 占内存）。

- `HTTPHandler` 的 **mcpChain 外层**包 body-limit 中间件（`/snapshot` 是 GET，不裹）。
- 上限 = serve 存值（§3.1）`× 4 / 3 + 64 KiB`（覆盖 base64 展开 + JSON 包装 + 头部余量），与内容上限**同源联动**（改 env 两个上限一起动，无独立旋钮）。
- **已知边界（rev1 登记，owner 2026-08-24 拍板接受）**：该上限按 base64 最坏展开推导；**text 模式**下 JSON 字符串转义（控制字符 `\uXXXX` 最高 6×/字节、`"`/`\` 2×）可使解码后 ≤ cap 的合法重转义内容在 HTTP 层被 413 早拒（极端形态：8 MiB 控制字符密集 text → 线上体最高 ~48 MiB）。真实场景（配置/脚本）几乎不命中，拒绝是干净的 413 无副作用，**base64 路径不受影响**（base64 字符集无需 JSON 转义）；agent-tools.md 留痕指引（极端二进制/控制字符内容走 base64）。不为该边界放大上限（6× = 把 DoS 面放大到 ~48 MiB/请求，得不偿失）。
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

- `action="upload-content"`（独立于 "upload"，audit CLI #16 按 action 过滤可区分两工具）；`Command = "inline %d bytes -> %s"`（解码后字节数 + remote_path）。
- **内容零入审计**（可能含 secret——零容忍）：测试反向断言——写入 secret 形态内容后扫 audit 表**无内容片段**。
- statuses 与 upload 全同：`denied / auth_error / no_credential / hostkey_mismatch / connect_error / cancelled / ok / error`。cap 预检拒绝、base64 非法、参数校验失败（encoding/remote_path）、MkdirAll/WriteFile（含 Close）失败均归 `error`。每分支一行（deferred WriteAudit，现有同款）。
- **no-leak 继承**：connect 错误经同一 `sshbroker.Connect`（Plan 31 源头 redactAddr 清洗）；SFTP 层错误为路径/原因文本（permission denied / no such file 等），无地址形态预期。no-leak 断言网扩本工具全部错误分支兜底（含 SFTP 中途失败路径）。

## 6. 文档变更

- **agent-tools.md**：upload_content 完整口径——跨机不对称缺口补齐（upload_file 是 broker 本机路径）、base64 承载二进制、8 MiB 解码后上限、失败留半写自查、root 属主路径不可写（SFTP 无 sudo）、大文件指引（broker 可达位置 + upload_file / 服务器侧拉取）、**极端控制字符/二进制内容走 base64 的留痕**（§3.2 已知边界：极重转义的贴上限 text 可能在 serve 侧被 413 早拒）。
- **threat-model.md**：§6（1 MiB 输出/传输封顶）加注——upload_content 独立 8 MiB 的理由：内容已在 agent 上下文中（不新增读取面/上下文膨胀，与 download 方向相反），上限 env seam 可调且 fail-closed；serve 请求体收口（MaxBytesReader）登记为传输层加固，text 转义早拒与并发聚合内存两处已知边界/残余风险一并登记。
- **compat-matrix.md**：新增工具属纯增量；v0.10.0 尚未发版，并入 v0.10.0 行还是开 v0.11.0 行**留 owner 发版时拍板**（先占位注释，Plan 32 同款）。
- **README / agent-access / scenarios（S1 配置下发）/ differences-ledger**（upload_content 无 ssh 二进制直接对应物——`cat > file` stdin 近似但不等同，登记 Broker-specific）同步。

## 7. 测试矩阵

- **单测（mcpserver；UploadContentForProfile 测试随 core_test.go 同层扩展，serve 中间件测试进 serve_test.go）**：
  - 两态编码落盘字节精确：UTF-8 文本；二进制 fixture（含 0x00/0xFF/GBK 字节串）经 base64 往返 `exec cat`/下载比对。
  - cap 预检拒绝：text 超 / base64 粗筛超 / base64 精判超——三路各自断言**零远程文件**（testsshd 上目标不存在）+ 错误文本含 size/cap。
  - **decoded == cap 边界成功用例（rev1）**：text 与 base64 各一个——解码后/内容字节数**恰好等于 cap** 的内容必须成功落盘（锚粗筛 padding 修复：base64 用例的编码长须使 `stripped/4*3` 虚高 > cap 而扣 padding 后 == cap）。
  - base64 非法拒绝（错误文本无原文片段断言）。
  - 空内容 → 0 字节文件（两态编码各一）。
  - MkdirAll 父目录（深层新路径落盘成功）。
  - remote_path 参数层拒绝（空串/相对路径两用例，先于 gate——越权 server_id + 相对路径得参数错误而非 denied 的断言顺序不适用：参数层在 gate 前，两错并有时返回参数错误）。
  - denied 分支（越权 server_id）+ 审计行格式断言（action/Command 模板）+ **内容不落审计反向断言**。
  - 错误分支 no-leak（connect_error 等文本无 host:port）。
  - encoding 非法值拒绝。
- **sshbroker**：WriteFile 单测（内容/截断已有文件/取消留半写/Close 错误按写入失败处理——testsshd）。
- **e2e（e2e_test.go）**：工具集合等式 9→10 更新 + upload_content 全流程（含 parent 创建）。
- **serve（serve_test.go / 新）**：Content-Length 超限 → 413；贴上限（cap 内 base64 满载）→ 工具调用成功；MaxBytesReader 兜底路径（谎报/无 Content-Length 超限 → 错误响应，不 413，如实断言）。
- **conformance（真 OpenSSH，双重门控同款）**：upload_content base64 二进制 round trip——上传后 `exec sha256sum` 与本地比对；覆盖写（已有文件被截断重写）。
- **eval**：BrokerTools 单源联动使集合断言自动含第 10 工具；新增 upload_content agent 用例 + scorer（用例覆盖：跨机上传小配置→exec 验证内容）。

## 8. 明确不做（scope 纪律）

- **大文件分块续传 API**（backlog 已裁决；8 MiB 内联覆盖目标场景，更大的走 broker 可达位置或服务器侧拉取）。
- **append / create-new 语义位**（覆盖写已拍板；防误覆盖 agent 可先 `exec test -e` 自查）。
- **temp+rename 原子替换**（已拍板直接写；与 upload_file 语义不分岔）。
- **upload_file §6 1 MiB 联动调整**（两者理由不同：本地源=传输入保护，内联=agent 已持有内容；互不动）。
- **download 上限变动**（维持 1 MiB 前缀截断）。
- **stdio 请求体 cap**（非网络面，§3.2 留痕）。
- **目录上传**（content 是单文件语义；目录走 upload_file）。
- **text 转义早拒边界的上限放大**（§3.2 已知边界，owner 拍板接受 + 文档留痕；不为 ~48 MiB 级极端形态放大 DoS 面）。
- **并发聚合内存全局约束**（§4 已知残余风险，owner 拍板登记接受；超本 plan scope）。
- **upload_file Close 既有债回补**（rev1 登记：upload.go:117 `defer out.Close()` 不查错是 Plan 6 遗留，与本 plan 无关；upload_content 新路径已钉死正确语义，旧路径留 backlog 债务登记，不在本 plan 顺手改）。

## 9. 验收与发版注记

- **自动化**：§7 全绿（含超限拒绝分支与 decoded==cap 边界用例——backlog 验收原文）。
- **跨机端到端（owner 手工，backlog 验收原文）**：笔记本 agent → NUC10 serve → 目标机：upload_content 下发配置 → 目标机 `exec cat` 验证；二进制走 base64 同验。
- **发版**：纯增量（新工具 + serve 中间件 + 文档）。v0.10.0 未发版——并入 v0.10.0 或开 v0.11.0 由 owner 发版时拍板（§6）。发版后 compat-matrix 回写删占位注释。
