# Plan 27 Task 5 Report — 文档 + backlog 补项 + 全量验证

Status: DONE (commit below)

## 1. 文档三处落点

### 1.1 README.md:92 — "Other commands" 行加 `doctor`

README 没有独立命令表；owner CLI 命令集中列举在 Quickstart 末尾的 **Other commands** 行（grep `clear` / `servers ls` 定位）。`doctor` 作为同级只读命令加在同一行，`version` 之前：

> `doctor` (side-effect-free local self-check — prints a PASS/WARN/FAIL report; exit `0` = no FAIL findings, `1` = at least one FAIL)

一行用法 + 退出码 0/1（不提 2 —— 控制器裁决：用户面文案只承诺 0/1）。

### 1.2 docs/getting-started.md:373-377 — 新增 `## 自检：doctor（无副作用）` 节

落点选择：grep 全 docs/ 的「排错/排障/故障/诊断」——getting-started.md 的 `## 常见坑（首跑高频）`(原 :373) 是唯一的首跑排错表，README 文档导航表也把「排错」指给 getting-started。doctor 节插在常见坑表**之前**（运维动线：先跑 doctor 定位，再对表修），含 4 个要点（简报要求的 3-5 行）：

1. 8 项只读检查清单（env 名字-only / role / store / masterkey 长度+权限位 / vault-open 拷贝解密探针 / serve-cert 只读孪生 / serve-svc / client-cache）
2. 退出码 `0` = 无 FAIL（允许 WARN），`1` = 至少一个 FAIL
3. 无副作用承诺（临时目录拷贝解密，原件零写入；零网络；不打印秘密值）
4. serve HTTP 存活探测（绿/黄/红）是二期 backlog

### 1.3 docs/backlog.md:10 — 追加第 6 项

> **Windows DACL readback 检查**——现状：doctor v1 在 Windows 上对 master.key 的硬化校验只有 32 字节长度（Unix 上是权限位），没有真正的 DACL 读回校验；`internal/store/acl_windows.go` 的 `getDACLForTest` 是 test 名构件，产品化需要生产命名的包装 API 才能进 doctor。

（`getDACLForTest` 位置核实：internal/store/acl_windows.go:139，仅 acl_windows_test.go 调用。）

## 2. doctor.go 文案修订（控制器三项裁决，随本 commit 搭车）

1. **退出码契约 = 0/1 only**（doctor.go Long 文案，原 :635-636）：
   - 旧: `Exit codes (stable, for scripts): 0 = no FAIL (WARN does not change it), 1 = at least one FAIL, 2 = doctor internal error.`
   - 新: `Exit codes (stable, for scripts): 0 = no FAIL findings (warnings allowed), 1 = at least one FAIL finding.`
   - `doctorExitCode` 纯函数保留 2 分支（reserved, tested）；doctor.go:57 / doctor_test.go:63 的函数注释描述纯函数行为，未动。
2. **去掉内部 spec 引用**：`"check the vault directory (platform root could not be resolved — see spec §3.1)"` → `"check the vault directory (platform vault root could not be resolved)"`。replace_all 命中 **5 处**（评审时 2 处；T3/T4 的 serve-cert ×2、client-cache DEK ×1 复用了同一句式，同为用户面字符串，一并不留 spec 引用）。无测试断言旧串（grep 核实）。
3. **WAL 注释软化**（probeVaultDecrypt 注释，原 :317-321）：`worst case an undercounted PASS, never a false FAIL` → `in realistic write patterns an undercount, not a false verdict; a full re-seal through a long-lived un-checkpointed broker connection could transiently mis-verdict`（仅注释，一句）。

## 3. 验证结果

| 命令 | 结果 |
|---|---|
| `go build ./...` | 绿（零输出） |
| `go vet ./...` | 绿（零输出） |
| `gofmt -l .` | 空（零输出） |
| `go test ./... -count=1` | **首轮 1 个 flake**：`internal/sshbroker` TestConnectCancelContext — `wsarecv: An existing connection was forcibly closed`（Windows 下 ctx-cancel 与 socket reset 谁先到的竞态，want context.Canceled）。与本任务无关（本任务只动文档 + 注释 + 用户面字符串，零行为变更）。复跑：包级绿 1 次 + 该测试 `-count=5` 5/5 绿 + **全量二轮全绿**（15 包 ok）。 |

## 4. 手工冒烟（真机状态，未 export 任何 SSHMGR_*）

```
$ go run ./cmd/ssh-manager doctor
ssh-manager doctor (dev)
env:  PASS  no SSHMGR_* environment overrides in effect
role:  INFO  no role.json — fresh machine, run the wizard
store:  INFO  store.db absent — no usable role on this machine (see the role check)
masterkey:  INFO  master.key absent — no usable role on this machine (see the role check)
vault-open:  INFO  skipped — store.db/master.key not both present
serve-cert:  PASS  serve cert present (fingerprint sha256:0fecc71a885fead4cb7b185db68566a14aef2fca3f32aec918560e9c4250e4d3)
serve-svc:  INFO  serve service not installed (serve not in use)
client-cache:  PASS  cache.bin present (age 1h11m0s)
overall: 0 WARN, 0 FAIL
EXIT=0
```

输出形态符合设计：9 行（env 单行 + 8 检查）、PASS/WARN/FAIL/INFO 四态、overall 汇总、退出码 0（无 FAIL）。

**真机状态观察（按指示 as-is 报告，未修机器状态）**：本机是 client 用机（cache.bin 1h11m 新鲜），但 **role.json 缺失** → role 行 INFO "fresh machine"。同时 serve-cert PASS（dev 期遗留证书在 vault 目录）。机器状态本身不影响 doctor 行为正确性；若 owner 关心，可跑 `ssh-manager tui` 重建 role.json（属机器侧操作，不属本任务）。

## 5. 字节复核（中文文档一行一字不差）

repo 有过一字节 Unicode 回归，对全部 4 个改动文件做了三重校验：

1. **严格 UTF-8 decode**（`bytes.decode('utf-8')` 不带 errors='replace'）4 文件全过——磁盘字节是合法 UTF-8，无截断/坏字节。
2. **U+FFFD（EF BF BD）扫描** 4 文件零命中——无替换字符。
3. **逐行 codepoint 比对**：新写中文行 ASCII-escape dump 逐字符核对（GS:373 `## 自检：\`doctor\`（无副作用）` = `## 自检：...`；GS:375 与 BL:10 全文核对），与意图文本逐字一致。控制台显示乱码是 Windows 终端 cp936 渲染，磁盘字节正确（首非 ASCII 字节 e887aa=自、e69cac=本、e6a380=检 均为正确 UTF-8 编码）。

## 6. Commit

单 commit（文档 + doctor.go 文案修订同车）：

```
docs: doctor usage, exit codes 0/1, backlog DACL item (Plan 27 T5)
```

body 说明 doctor.go 三处文案修订（退出码 0/1 契约、去 spec 引用 ×5、WAL 注释软化）。Trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`。
