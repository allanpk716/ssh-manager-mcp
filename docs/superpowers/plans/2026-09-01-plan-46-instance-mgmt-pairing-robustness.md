# Plan 46 实施计划:实例管理与配对向导健壮性(force 原子化/实例 rm/picker 重做)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 消灭 force 重配的"清理先行"半态事故形态;实例生命周期补齐删除一等公民(CLI+TUI);picker 按产物集合重做三分行;配对失败的恢复指引如实化(可能性分档+419 兜底,不做确定性承诺)。

**Architecture:** ① clientops:`ForceCleanup` 不再于 Enroll 前调用——新凭据经 WriteAndPull 原子覆盖旧材料(既有写序本就是临时文件+rename),quarantine/ 清理移至 WriteAndPull 成功尾部;失败指引统一为双路径文案(先重跑 force;撞 419 则 owner 吊销后重试——client 无法可靠分辨 serve 端状态,确定性承诺不可实现)。② CLI 新增 `cache instances ls/rm`(双根清理:槽目录+ProgramData DEK)。③ TUI picker 重做(产物集合三分/★当前槽/CJK 对齐/[d] 删除)。协议/serve 零改动。

**Tech Stack:** 既有栈;唯一新直接依赖 `mattn/go-runewidth`(已在依赖树 indirect,提升)。

**Spec:** 无独立 spec(grilling Q1-Q11 拍板 2026-09-01 + proposal 三轮异构盲评收敛:rev2 定稿基线 `.xcheck/20260901-174316/proposal.rev2.md` + 复审轮 3 的 8 条豁免修正项已全部织入本 plan;处置记录见该 proposal 末两节)。

## Global Constraints

- **协议/serve 零改动**:/pair 面、wire shape、419/replaceInactive 语义、TouchCacheToken 时机全冻结
- **指引原则(三轮盲评收敛的核心定案)**:client 端**永远无法可靠分辨** serve 是否已标记拉取(请求发出后的传输失败=两态不可分辨;config 写失败是 warning+继续,可能已进首拉)——因此 **finish 后一切失败的恢复指引统一为双路径**:"直接重跑 `sshmgr pair --force`(或 TUI 重配);若重跑报设备名占用(419),请 owner 在 broker 侧执行 `sshmgr cache-tokens revoke <实例名>` 后再重跑"。**禁止任何"必定自愈"式确定性表述**
- **CLI frozen wordings**:改动波及的错误文案(pairing enroll 419 等)须同步更新既有断言并在 commit message 列明;`pair` 交互次序零变化
- **force 语义**:ForceCleanup 方法保留(既有测试零改动),但 TUI(pwEnrolling)/CLI(RunPair)均不再于 Enroll 前调用;quarantine/ 删除移入 WriteAndPull 尾部(覆盖全部成功后),**清理失败仅警告不判失败,下次成功时重清**;DEK 复用不删
- **并发边界(如实)**:进程内 rm/force 进行中拒绝并发 pull **与 pair 写盘**(互斥);跨进程并发不由文件锁拦截,由原子 rename+幂等 rm/可重跑兜底,文档写明
- **路径安全**:rm/ls 的实例名复用 `instname.Valid`;槽目录与 DEK 路径 resolve 后必须落在各自根内,拒 traversal/分隔符/Windows 保留名;用户参数绝不直拼 RemoveAll
- **默认槽(instance="")**:拒绝 rm(文案指路 `sshmgr clear`);picker 行无 p(提示文案不得出现 `--instance` 建议式矛盾)
- **完整性判定**:`auth+bin+meta+DEK` 四者齐=完整(maxOffline>0 时 meta 为离线加载必需——clientops.go:360-365 拒载分支);任缺=残缺(行内标缺什么)
- 419 advisory 分档:完整槽=确定性提示("该实例已拉取过,重配前需 owner 吊销");半态槽(bin 缺)=可能性提示("无法本地预判,若撞 419 见错误指引")
- 零新 wire;中文 conventional commits;每任务 `go build ./... && go vet ./... && go test ./...` 全绿
- 版本:**v0.13.3**;真机验收合批(见尾部)

---

### Task 1: clientops force 时序重构 + 失败指引 + artifact 原子写

**Files:**
- Modify: `internal/clientops/pairsession.go`(WriteAndPull 尾部清 quarantine;finish 后失败的错误文案统一带双路径指引;DoPull 前后分段包装错误来源)、`internal/clientops/pair.go`(RunPair 驱动层移除 Enroll 前的 ForceCleanup 调用;419 错误文案补 revoke 指引;`writePrivateFile` 改临时文件+rename 原子写——pair 产物与 --write-mcp 副本同改)、`internal/tui/pairwizard.go`(beginEnroll 移除 ForceCleanup 调用与 cleaned 消费逻辑;force 确认屏文案改为如实双路径)
- **不动**:ForceCleanup/forceCleanInstance 本体(既有测试零改动);DoPull 签名;协议层

**行为定案:**
- [ ] Enroll 前零清理:任何 enroll/poll 阶段失败,旧槽文件一字不动(事故形态根除的回归钉)
- [ ] WriteAndPull 成功尾部:`quarantine/` 存在则整目录删除;失败→WARNING 行(含"下次成功时重清"),不判失败
- [ ] finish 后失败(写盘段/首拉段/finish 请求自身传输失败)的错误文案统一尾缀双路径指引(措辞见 Global Constraints;**不分 b1/b2——分档不可靠,统一双路径**)
- [ ] 419 错误文案(pairsession.go:268/432 两处)补:"owner 在 broker 侧 `sshmgr cache-tokens revoke <实例名>` 后重试";同步更新既有断言
- [ ] `writePrivateFile` 原子化:临时文件(同目录唯一名)+rename+HardenACL,失败不留半文件
- [ ] TUI force 确认屏(pwEnrollForceConfirm)文案:去掉"将清理本实例材料"类表述,改为"重配成功后新凭据覆盖旧材料;若上次配对中断且重跑报设备名占用,需 owner 吊销后重试"

**测试(失败注入矩阵,全部必写):**
- [ ] enroll 419/网络失败→旧槽文件集逐字节不变(黄金断言)
- [ ] finish 成功后不落盘(模拟写失败)→重跑 force 全链自愈(复用探针 `.xcheck/20260901-170939/exp/exp_probe_close46_test.go` 形态,正式化入 pairsession_test.go)
- [ ] 已拉取后重跑 enroll→419(探针对照腿同构正式化)
- [ ] 首拉 body 已接收后 bin/meta rename 注入失败→错误文案含双路径指引
- [ ] finish 响应截断(已提交未收响应形态)→重跑断言放行
- [ ] config 写失败(既有 warning 语义)后重跑→文案仍双路径(不误导)
- [ ] pair 产物原子写:写中途注入失败→无半文件
- [ ] 既有 pair_test.go/pairsession_test.go 端到端断言:除列明的文案断言更新外零改动全绿

---

### Task 2: CLI `cache instances ls/rm`

**Files:**
- Create: `internal/cli/cache_instances.go`、`internal/cli/cache_instances_test.go`
- Modify: `internal/cli/root.go`(挂子命令)、`internal/clientops/`(新增 `RemoveInstance(instance) error`:`instname.Valid`→canonicalize 双路径入根内校验→删 `instances/<名>/` 整目录→`DekProvider(name).Delete()`→任一步失败返回带残留物清单的错误,幂等可重试;进程内互斥:包级互斥量与 DoPull/WriteAndPull 写盘段共享)

**行为定案:**
- [ ] `ls`:每行=实例名/槽产物存在性(auth·bin·meta·config)/DEK 存在性(stat 不解密)/cache 年龄;DEK 孤儿(DEK 在槽无)与半态槽显式标注;默认槽一行(`(默认实例)`,无 rm 提示)
- [ ] `rm <名>`:默认槽拒绝(文案指路 clear);确认屏;成功输出提示两件配套:①broker 侧吊销该设备码(client 无权远程吊销)②`--write-mcp` 槽外副本不随 rm 清理——**泛化提示+原因明说**(该目标路径不持久化,cache.config.json 仅存 max_offline,无从得知具体路径)
- [ ] 删当前正在使用的槽:完整删除成功后才回落默认槽语义仅 TUI 有(CLI 无会话态);CLI 输出提醒"若某 TUI/MCP 正使用该实例,重启后生效"
- [ ] 路径安全测试:traversal(`../x`)、分隔符、Windows 保留名(CON/NUL…)、绝对路径全拒
- [ ] 双根 partial 注入:目录删成 DEK 删失败(反向同)→错误含残留物;重跑 rm 幂等清干净
- [ ] 进程内互斥断言:rm 进行中触发 pull/pair 写盘→被拒(而非交错)

---

### Task 3: TUI picker 重做

**Files:**
- Modify: `internal/tui/instancepicker.go`(行模型/渲染/键位)、`internal/tui/clientpage.go`([d] 删除流/当前槽回落)、`internal/tui/pairwizard.go`(p 入口对接,如涉及)
- Modify: `go.mod`(runewidth 提升直接依赖;`go mod tidy` 后 indirect 注释消失)

**行为定案:**
- [ ] 行状态:完整=auth+bin+meta+DEK 四者齐;残缺=目录在任缺(行内标"缺 bin/缺 meta/缺 DEK");★前缀标当前选中槽
- [ ] 渲染:go-runewidth 列宽对齐(中文实例名/混排行列边界一致);clip 尾注"本地视角——远端吊销状态不可见"
- [ ] 键位:Enter=切换;`p`=完整/残缺行开 force 重配向导,默认槽行按 p 显示提示("默认槽为本机原始身份,不支持 picker 重配;如需重配请先了解默认槽语义后在 CLI 操作"——**不得出现 --instance 建议式矛盾文案**);`d`=删除(确认屏,同 T2 语义+吊销/副本双提示;删除目标为当前槽且完整成功→自动回落默认槽并刷新列表,失败不回落);Esc/q=关闭
- [ ] 419 advisory 分档入 p 确认屏:完整槽="该实例已拉取过,重配前需 owner 在 broker 吊销其设备码";残缺槽="该实例材料不完整,无法本地预判远端状态;若重跑撞 419 见错误指引"
- [ ] 半态行(用户事故形态)优先视觉提示(⚠ 前缀)

**测试:**
- [ ] 三态判定单测:auth/bin/meta/DEK 的 16 组合矩阵→行状态断言
- [ ] CJK 列宽对齐断言:中文名/混排行渲染后列边界(等宽列起点)一致
- [ ] 默认槽行 p 提示文案钉;★当前槽钉;[d] 确认/回落流;删当前槽后 [s]/[i] 全路由默认槽(无悬空引用)
- [ ] 既有 instancepicker/clientpage 测试零回归

---

### Task 4: 文档 + 发布注记

**Files:**
- Modify: `docs/tui-multi-machine.md`(键位表/重配 runbook/并发边界/失败指引双路径)、`docs/multi-machine.md`(instances ls/rm 用法+rm×吊销两件事+--write-mcp 原因)、`docs/backlog.md`(本 plan 销项)、`docs/tui-multi-machine.md` L138-139 "批准面两件套"过时描述修正(上版 merge 残留,应为三件套同屏)、README/quickstart 如涉及
- [ ] 发布注记草稿(v0.13.3:force 原子化/实例管理/picker 重做/失败指引如实化;真机验收 GW 清单见下)
- [ ] backlog 销项提交(2026-09-01 GW1 反馈登记节的表单改进条目=方案 B 两级条件表单——**注:该项属 Plan 46 范围外的独立改进,保持登记不动,本 plan 不含**)

---

## 真机验收(GW 批,随 v0.13.3)

- GW2' 重测:picker p 重配全链(含 419 撞墙→owner revoke→自愈);enroll 阶段失败→旧槽完好验证(如断网中按 p)
- GW3 被拒重试 r;GW4 Esc 全链 + CLI 回归(`sshmgr pair` 直跑)
- GW5 新增:`cache instances rm` 真机删除一个测试实例(双根清干净+吊销提示);picker ★/CJK 对齐目验
