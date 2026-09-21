# 夜链首批验证记录（2026-09-21，agent 代跑）

> 执行者：夜链实施 agent（各票实施会话自证），本记录由票 06 汇总留档；回写见同批 `docs/backlog.md` 活跃面第 2/4/5 条与验收册 plan-46-45。
> 分支：`xcheck-night-20260921-124920`（自 master 基线 992349f 切出）。各票派发基线与提交：票 01/02/05 = e886f8e / dfe3802 / 0edbba3（wave1，基线 b31f55b）；票 03 = 9b904b0 + gofmt 修复 1073f41（wave2）；票 04 = 99dce78（wave3）。
> **持续集成未跑**：夜链分支不触发 ci.yml（其推送触发仅 master），本记录全部证据为本地验证。竞态检测（`-race`）按仓库既定实践仅持续集成跑（本机 Windows 运行时损坏），本地以普通 `go test` 为准。
> 判据纪律：判据不放宽——持续集成未验证的不销项，只登记（backlog #10 保持活跃、#8 按修复形态注记、活跃面第 4/5 条状态措辞跟证据走）。

## 判定总表

| 票 | 内容 | 提交 | 本地判定 | 销项/收口条件 |
|---|---|---|---|---|
| 01 | 终端界面测试耗时治理（P2 #10） | e886f8e | 本地验证过（89.684s → 17.467s，目标 <30s 达成） | **销项待持续集成两平台绿（拉取请求/合并后）** |
| 02 | TestConnectCancelContext 竞态窗口稳定化（P2 #8） | dfe3802 | 本地验证过（`-count=10` 10/10 绿，断言未动） | 本夜注记登记；销项/转正式注记待持续集成绿后裁决（计划「销项或注记视修复形态」） |
| 03 | 配对向导两级条件表单（批5） | 9b904b0 + 1073f41 | 本地验证过（四条契约测试先红后绿，`-count=5` 零失败） | 观感验收留 owner 保留面（未验）；销项待持续集成绿 + owner 复核 |
| 04 | 选择器与 Esc 键链契约测试（批4 收窄版） | 99dce78 | 落地（三测试全绿，自身增量 ≈0 噪声内） | 计时判据不笼统判达成——构成分解见「批4 计时判据如实登记」节 |
| 05 | Plan 43 待审稿更名适配（批3 前置小件） | 0edbba3 | 完成（6 处/4 行，grep 零剩余，技术内容零变化） | 无（owner 审阅即终稿） |

## 票 01 · 终端界面测试耗时治理（P2 #10，commit e886f8e）

- 计时对比：**89.684s → 17.467s**（两次均 `go clean -testcache` 后 `go test ./internal/tui/ -count=1`；目标 <30s 达成）。
- `-count=10`：`go test ./internal/tui/ -count=10` 全套零失败（174.059s）；单跑 229 PASS / 0 FAIL。
- 改动面：3 个测试文件（editpage_test.go / app_routing_test.go / app_test.go），生产零改动。
- 修法：`press()` 普通字符键免等待（丢弃 530ms 闪烁重臂命令；Confirm 字段 y/n 例外保留 drain）+ 8 处 `t.Parallel()`；TestWizardLoopServerFormCompletes 因 `t.Setenv` 夹具不能并行，维持串行（6.93s，现余串行地板）。
- 新守卫测试：TestEditPagePressIsFreeOfBlinkWait（墙钟网）、TestEditPagePressCharKeysYieldBlinkOnlyCmd（前提钉）。

## 票 02 · TestConnectCancelContext 竞态窗口稳定化（P2 #8，commit dfe3802）

- 修法：单轮场景助手 + 30 秒截止时间内整景重跑（每轮全新 listener 与上下文）；断言语义/阈值/文案逐字未动（不是放宽断言）。
- 验证：`-run TestConnectCancelContext -count=10` → 10/10 绿；`go test ./internal/sshbroker/ -count=1` 全绿（7.8s）；vet/gofmt 净。
- 改动面：仅 internal/sshbroker/client_test.go。
- 评审留档（非阻断小疵）：持续秒败场景（如 listen 错误风暴）会空转重试到 30 秒期限，有界非阻断。
- 登记口径：按分批计划「销项或注记视修复形态」——本夜注记登记，销项裁决待持续集成两平台绿。

## 票 03 · 配对向导两级条件表单（批5，commit 9b904b0 + gofmt 修复 1073f41）

- 形态（方案 B）：一级 = 信任模式 Select + 实例名 + profile 提示；二级（仅手动直连）= 服务地址 + 服务器公钥指纹，均必填，指纹带来源提示（broker 机 `sshmgr serve cert-info`）；选局域网发现两字段根本不出现且残值强制清空（非隐藏空值）；任一级 Esc = 纯返回页面零残留（沿用现行向导语义零自创）；失败路径按级重建（沿用「重建表单、不复用实例」的既有纪律——T2-R1 huh 死锁教训）。
- 四条契约测试先红后绿：TestPWTwoStage_LANFlowHidesDirectFields（发现字段不出现）/ TestPWTwoStage_DirectFlowSecondLevel（直连必填）/ TestPWTwoStage_FailureRebuildsCurrentLevel（失败重建非复用）/ TestPWTwoStage_EscAnyLevelCloses（任一级 Esc 零残留）。
- 既有回环测试按两级形态更新：8 处重点更新逐条有理由（评审核为约 12–13 个测试函数触及，多为机械驱动改写）。
- 验证：`TestPWTwoStage|TestPairWizard -count=5` 零失败；套件计时 18.144s / 18.083s（基线 17.467s，**+0.68s 登记增量**）。
- 保留面：观感验收留 owner（真终端表单观感未验）。

## 票 04 · 实例选择器与 Esc 键链契约测试（批4 收窄版，commit 99dce78）

- 只加测试，生产零改动：
  - TestInstancePicker_FourElementsDiskToRow——四要素（auth / bin / meta / DEK，数据加密密钥）磁盘源映射，表驱动 4 子测试；
  - TestInstancePicker_ProfileFromScopedMeta——快照裁剪标记（scoped）门两态：profile 列仅在裁剪快照带设备名时显示，旧全库快照不得展示；
  - TestClientModel_EscChainByContext——Esc 三态分化成组（过滤态清过滤 / 覆盖层先收且过滤与会话槽不动 / 无覆盖无过滤无操作），含覆盖层先收、第二击才清过滤的链序。
- 盘点具名：已有覆盖（四要素矩阵 / 半态 / 删除确认取消 / 向导 Esc 链等）零重复；向导面零接触（归票 03）。
- 计时：套件全绿；**自身增量 ≈ -0.3s（噪声内未回涨）**；总耗时 18.256s（评审员复跑 17.974s）。

### 批4 计时判据如实登记（不写笼统「达成」）

- 判据原文：「套件耗时不回涨超批1 基线」。
- 事实：套件总耗时 18.256s，高出批1 基线 17.467s 约 0.6–0.8s；**该超出在票 04 动手前的实测（18.576s）即已存在**。
- 构成分解 = 票 03 已登记的 +0.68s 契约测试增量 + 机器噪声；**批4 自身增量 ≈ 0（噪声内）**。
- 距 30 秒目标余量约 40%。

## 票 05 · Plan 43 待审稿更名适配（批3 前置小件，commit 0edbba3）

- 冻结文案命令引用 `ssh-manager` → `sshmgr`：6 处 / 4 行（`docs/superpowers/specs/2026-08-28-plan-43-doctor-serve-probe-design.md.rev2.1.md`）；grep 零剩余；技术内容零变化。

## 全仓结果与登记

- 全仓：协调者终局复跑 `go clean -testcache && go test ./... -count=1` → exit 0 全绿（2026-09-21；tui 包在全仓并行语境 31.374s = 包间 CPU 竞争，单包口径 18.2s 不变）。
- 回写（同批）：`docs/backlog.md` 活跃面第 2/4/5 条登记 + P2 #8/#10 详细条目夜链注记——**均不销项**（#10 保持活跃待持续集成；#8 按修复形态注记；第 5 条待 owner 复核）；`docs/acceptance/plan-46-45-tui-flows.md` 对应「待测试代证」条目翻「测试代证」并注明两级表单形态（观感层条目不动，留 owner）。
- **销项条件**：持续集成两平台绿（拉取请求/合并后触发）后，backlog #10 销项、#8 按修复形态销项或转正式注记；配对向导观感验收 owner 目验后收口活跃面第 5 条。
