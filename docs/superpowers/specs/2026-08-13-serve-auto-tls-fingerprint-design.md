# serve 同步链路自动加密:自签 TLS + 指纹 TOFU

> **日期**:2026-08-13 · **状态**:设计稿(待 review)
> **范围**:`cache pull` ↔ `serve /snapshot` 同步链路的传输层自动加密。
> **一句话**:`serve` 首次启动自动生成自签证书;`cache-tokens add` 签发设备码时把证书公钥指纹一起交给工作机;`cache pull` 钉死该指纹。零证书分发、零 openssl、首次连接即校验(零 MITM 窗口)。

---

## 1. 背景与动机

### 1.1 现状(已落地的生产)

路线乙(2026-08-13 端到端验过):NUC10 跑 `ssh-manager serve` 作权威 broker,笔记本 client 走 **本地缓存** —— agent 平时连本地 `ssh-manager mcp --cache`(stdio,**零跨网络**),只有 `cache pull` 同步时才跨网络碰 serve。

当前同步链路的传输事实(`internal/mcpserver/serve.go`、`internal/cli/cache.go`):

- `serve` = `net/http` server,默认 `127.0.0.1:7878`,可选 TLS(`--tls-cert/--tls-key` 文件路径,默认**关**)。
- `cache pull`(`cache.go:104-149`)用 `http.DefaultClient` GET `/snapshot`,**Bearer 设备码鉴权**。
- `/snapshot`(`serve.go:183-209`)把**整个 vault 解密后**塞进 JSON body 明文返回。
- 文档既有立场(plan-12 spec §171/317):*"TLS is the transport crypto, DEK is the at-rest crypto."* —— 但 TLS 默认关,且"HTTPS 麻烦"导致实际很多部署不挂。

### 1.2 "HTTPS 麻烦"拆解

1. 生成证书麻烦(openssl 易错)。
2. **CA 分发麻烦** —— 每台客户端要装根证书才认自签。
3. **MCP host 不认自签** —— 但**路线乙下 agent 永不直连 serve**(走本地 `mcp --cache`),此条**不触发**。
4. 续期/轮换麻烦。

痛点 3 在本设计范围外(见 §2.3 砍掉项)。痛点 1/2/4 由"自动生成 + 指纹钉死"一次消解。

### 1.3 设计目标

- **自动**:首次启动自生证书、客户端自钉指纹,全程不碰 openssl。
- **零证书分发**:指纹随设备码交付,无 CA 部署到各客户端。
- **VLAN 够轻、未来公网扛得住**:TLS 1.3 前向保密 + 指纹钉死抗 MITM。
- **平滑迁移**:已部署的 NUC10+笔记本不中断切换。
- **代码改动最小**:不抛弃标准 `net/http`/TLS 栈。

---

## 2. 范围与威胁模型

### 2.1 做什么(in-scope)

1. `serve` 首次启动自动生成自签证书(ed25519),持久化 + ACL。
2. `cache-tokens add` 签发设备码时把服务器证书 **SPKI 指纹**一并交付给工作机。
3. `cache pull` 客户端用 pinning `tls.Config` 钉死该指纹;失配即拒。
4. `serve` 的 MCP 端点(host 在线 live 模式若用)也走同一张证书,顺带受益。

### 2.2 威胁模型增量

| 威胁 | 现状(明文 HTTP) | 本设计后 |
|---|---|---|
| 同 VLAN 嗅探 token | 裸奔 | ✅ TLS 加密全包 |
| 同步时 `/snapshot` 整库凭据明文 | **裸奔(最严重)** | ✅ 加密 + 前向保密 |
| MITM / 伪装 serve | 无防御 | ✅ 指纹钉死,首次连接即校验 |
| 设备码泄露 | 能拉整库(既有风险) | 不变 —— 鉴权范畴,沿用现有 `cache-tokens revoke` |
| host compromise | out of scope(既有模型) | 不变 |

**关键安全属性:** 指纹在 enroll 时随设备码人工/流程交接到达工作机 → 工作机**第一次连就有 pin** → **无传统 SSH TOFU 那"首次盲连可被劫持"的窗口**。比 `StrictHostKeyChecking=accept-new` 更强。

### 2.3 明确不做(YAGNI)

- ❌ **段 A 的 stdio bridge**:路线乙下 agent 永远走本地 `mcp --cache`,MCP host 不碰 serve,host 信任自签问题**不触发**。在线 live 模式作为可选保留,但本设计不为其加 bridge。
- ❌ 客户端证书 / mTLS / CA 体系 / 吊销列表 —— 方案 3 的复杂度,单 owner overkill。
- ❌ 段 B 之外的传输层改动 —— agent 命令路径零改动。

---

## 3. 组件与数据流

### 3.1 新增 / 改动组件

| 组件 | 文件 | 职责 |
|---|---|---|
| 证书生成器 + 指纹助手 | `internal/mcpserver/cert.go`(新) | 首次生成 ed25519 自签证书;持久化到固定路径;已存在则加载;`SPKIFingerprint(*x509.Certificate) string` |
| serve 监听改造 | `internal/mcpserver/serve.go:246-254` | `tlsCert==""` 分支改用自生证书;显式 `--tls-cert` 非空时尊重操作者证书(向后兼容) |
| 指纹客户端 transport | `internal/cli/cache.go`(改 pull) | 新 `pinningTransport(fp)`:`tls.Config{VerifyConnection: ...}` 校验对端叶子证书 SPKI == fp |
| 设备码签发嵌指纹 | `internal/cli/cache_tokens.go:96`(`printCacheToken`)+ `internal/store/cachetoken.go` | 签发时把当前 serve 指纹并入输出 |
| flag / env | `internal/cli/cache.go` | `--pin` / `SSHMGR_SERVE_PIN` |

### 3.2 首次自动生成(serve 侧)

```
serve 启动
  └─ tlsCert == "" 且 tlsKey == "" ?
       YES → 加载 <固定路径>/serve-cert.pem + serve-key.pem
              ├─ 存在且可解析 → 用之
              └─ 不存在 → 生成 ed25519 key + 自签 x509
                          (SAN: 本机所有网卡 IP + localhost + 主机名)
                          写 pem,私钥文件 ACL 与 master.key.plain 同级
                          audit: "self-signed cert generated, fp=sha256:..."
       NO  → 尊重操作者显式 --tls-cert/--tls-key(向后兼容,不覆盖)
```

**性质:** 自动、幂等、向后兼容。SAN 覆盖本机所有网卡 IP,避免不同 `--url` 写法触发 name-check;但**核心信任靠 SPKI 指纹,不靠主机名**。

### 3.3 信任交付(enroll 时一次)—— 形态 A 为主

指纹随设备码到达工作机。优先级 **env > flag > token 内嵌**(高 → 低):

1. `SSHMGR_SERVE_PIN` env —— 运维在 unit/任务计划里统一注入,优先级最高。
2. `--pin sha256:...` flag —— 显式覆盖。
3. **token 内嵌**(`cache-tokens add` 默认输出 `<设备码>:<指纹>`,client 自动拆)—— 懒得分开传的默认路径。
4. 都没有 → 明文回退(兼容,见 §4)。

默认零新增步骤(本来就要抄设备码),但 `--pin`/env 留口子供手动核验/换证书。

### 3.4 日常同步数据流(改造后)

```
工作机 cache pull:
  transport = pinningTransport(fp)   ← fp 来自 env > --pin > token 拆分
  GET https://<serve>/snapshot (Authorization: Bearer <设备码>)
    └─ TLS 握手:对端叶子证书 ed25519 SPKI → SHA256 → 与 fp 比对(subtle.ConstantTimeCompare)
         ├─ 等 → 接受,后续正常 TLS(全包加密 + 前向保密)
         └─ 不等 → 拒绝,报 "server fingerprint mismatch (expected sha256:.., got sha256:..)"

serve:
  listener = TLS(自签证书)           ← 永远加密
  /snapshot: 设备码鉴权(不变) → 返回 Snapshot JSON(现在全程密文)
```

### 3.5 指纹校验算法(客户端)

钉 **SPKI 指纹**(非整证 DER、非主机名):

- 公钥 `SPKI = SubjectPublicKeyInfo`(ASN.1 DER),`fp = sha256:hex(sha256(SPKI))`。
- 校验用 `tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {...}}`。**必须同时设 `InsecureSkipVerify: true`** —— serve 证书是自签(不在系统根池),默认 PKIX 链验证必然失败,Go `crypto/tls` 会在 `VerifyConnection` 回调**跑之前**就中止握手 → pin 校验成死代码。`InsecureSkipVerify:true` 跳过不可能成功的链验证,信任锚完全转移到 `VerifyConnection` 里的 SPKI pin(Go 官方 `VerifyConnection` 示例即此组合)。这是 HPKP / Tailscale 模式,pin 是唯一信任锚。
- 回调取对端叶子证书,算 SPKI sha256,`subtle.ConstantTimeCompare` 与钉死 fp 比对;不等返回 error 让握手失败。`len(PeerCertificates)==0`(对端匿名)也返回 error。

**为何钉 SPKI 而非整证 DER:** 自生证书长生(不靠过期驱动轮换),真正要防的是"公钥被换"(= MITM)。钉 SPKI 在安全等价(换密钥即失配)的同时,避免"同密钥重签"这种无害操作触发误报。SPKI 指纹是 HPKP / Tailscale / `step` 等事实标准。

---

## 4. 迁移(已部署生产不中断)+ 错误处理

### 4.1 NUC10 serve + 笔记本 client 平滑切换序列

> ⚠️ **迁移顺序铁律(修订,xcheck 共识 B)**:**先升全部工作机二进制并配 pin → 后升 serve**。升 serve 瞬间其变 TLS-only,旧明文 client(`http://`)直连 TLS 端口会失败(`malformed HTTP response`/`unknown authority`)。"不中断"**仅在此协调前提下**成立 —— 是计划内操作,不是自动的。

```
① 升级【所有工作机】二进制到含本设计的版本,并先把 pin 备好(从 serve cert-info 拿)
   ⚠️ 必须先做这步 —— 升 serve 前客户端就要准备好 pin
② 在 NUC10 跑一次性命令生成/确认证书 + 打印指纹:
     ssh-manager serve cert-info   → fp=sha256:abcd...
   (幂等:证书已存在则只读不写;明文部署此刻生成自签证书文件)
③ 各工作机注入 SSHMGR_SERVE_PIN=sha256:abcd...(从②拿到)
   或重新发一个带指纹的设备码(形态 A: <设备码>:<指纹>)
④ 【最后】重启 NUC10 serve → 监听强制 TLS(用②的证书)
⑤ 工作机下次 30min 定时 cache pull → 走 TLS+pinning 成功 → 迁移完成
```

**新策略(xcheck 共识 C,用户拍板):** 新 client **无 pin 默认 hard-fail**(拒连),不再明文回退。明文拉取需显式 `--allow-plaintext` opt-in(仅用于旧明文 serve 的过渡/调试)。这消解了"无 pin 静默明文"的 fail-open 隐患。

### 4.2 错误处理矩阵

| 场景 | 现状 | 新行为 |
|---|---|---|
| client 有 pin,指纹匹配 | n/a | ✅ 正常 |
| client 有 pin,指纹**不匹配** | n/a | ❌ 拒握手,清晰报错 `server fingerprint mismatch (expected sha256:.., got sha256:..)` + 退出非 0 |
| client 有 pin 但 URL 非 https | n/a | ❌ hard-fail(xcheck F8:pin 已设却走 http 会静默明文) |
| client **无 pin** | 明文 + STDERR 警告 | ❌ **hard-fail**(拒连);明文需显式 `--allow-plaintext`(xcheck F4) |
| client pin 格式非法(env/flag 给了但非 sha256:<64hex>) | n/a | ❌ hard-fail(xcheck F7:打错别字不能静默降级) |
| serve 证书文件损坏 / 读不了 | n/a | ❌ serve 拒启动(不降级明文) |
| serve 证书文件被误删(marker 仍在) | n/a | ❌ serve 拒启动(xcheck F10:不静默重生新 key 致全客户端硬失败) |
| serve 证书文件存在但私钥与 cert 不配 | n/a | ❌ serve 拒启动 |

**不可降级硬失败:** (a) 有 pin 但失配 → 拒;(b) serve 证书损坏/误删 → 拒启动(**绝不**静默降级明文/重生 —— 防"证书坏了就裸奔"或"误删致全员 bricked")。

---

## 5. 测试策略

### 5.1 Layer-1 单元测试(默认 `go test ./...`)

- `cert.go`:生成 → SPKI 指纹稳定(同密钥重签证书,指纹不变);私钥文件权限/ACL 正确(平台断言)。
- pinning `tls.Config`:对端证书 SPKI 匹配 → 通过;换不同公钥证书 → 失配 error。
- token 拆分:`<token>:<pin>` 拆两半;优先级 env > `--pin` > token 内嵌。
- `cache-tokens add` 输出包含当前 serve 指纹。

### 5.2 Layer-2 集成测试(gated `SSHMGR_*_INTEGRATION=1` 惯例)

- 内存 serve + 自签证书 → client 正确指纹拉 `/snapshot` 成功。
- 同 client 错误指纹 → 被拒(握手失败)。
- 无 pin client → 明文回退 + 警告(兼容路径)。
- **迁移回归**:旧明文 serve + 新 client、新 TLS serve + 无 pin client 两个过渡窗口,都不断。

### 5.3 不在范围

- 性能(30min 一次的 KB-MB 级快照,非热路径)。
- 段 A(host 直连,本设计不动)。

---

## 6. 配置接口总览

```
serve 侧(自动,无需新 flag):
  首次启动 → 生成 <固定路径>/serve-cert.pem + serve-key.pem (ACL=master.key.plain 同级)
  --tls-cert/--tls-key 非空 → 尊重操作者证书(向后兼容)
  新诊断命令:ssh-manager serve cert-info → 打印当前 fp

client 侧(cache pull):
  --token <token:pin>        形态 A,指纹随交接到达(默认路径)
  --pin sha256:...           显式覆盖
  SSHMGR_SERVE_PIN           env 覆盖(优先级:env > --pin > token 内嵌)
```

---

## 7. 需在实现时敲定的细节(留给 plan)

- `serve cert-info` 子命令的确切输出格式(是否含 cert 路径 / SAN / 有效期)。
- 证书 SAN 采集:是否纳入 IPv6 / 临时地址(可能需 `--san` flag 让操作者显式追加公网域名)。
- `cache-tokens add` 输出 `token:pin` 的分隔符选择(`:` 是否与 token 字符集冲突 —— 现有设备码是 base64url,无 `:`,安全)。
- serve 证书私钥 ACL 的平台实现(复用 master.key.plain 的 ACL helper)。

---

## 8. 相关文档(实现时同步更新)

- `docs/multi-machine.md` —— 加密一节改写;`.mcp.json` 在线模式说明保留但标注"路线乙下非主路径"。
- `docs/threat-model.md` —— 同步链路威胁从"TLS 可选"升级为"强制 TLS + 指纹钉死"。
- plan-12 spec §171/317 的 *"TLS is the transport crypto"* 立场 —— 更新为"自动 TLS + 指纹钉死,默认开"。
