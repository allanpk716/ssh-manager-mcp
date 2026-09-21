# Plan 46/45 验收册:实例管理 + 配对向导流程(v0.13.2/v0.13.3 起)

> 来源:backlog「Plan 46 待 GW 真机验收」条 + 「Plan 45 真机验收反馈登记」节。
> **执行策略(ADR 0003)**:本册条目按三层拆——CLI 等价面【agent 可跑】、TUI 行为契约【待进程内测试补齐后由测试代证】、真终端观感【人工保留面】。原 GW 编号保留对照。

## 前置

- [ ] 双端 ≥ v0.13.3(现产线 v0.16.0,直接在其上验)
- [ ] 本机 doctor 0/0

## GW2′ picker `p` 重配全链含 419 撞墙自愈

- **CLI 等价面【agent 可跑,一次性靶子】**:`sshmgr pair --force --instance <临时名>` 全链:
  1. 临时实例正常入网(带 `SSHMGR_PAIR_ASSUME_SAS=1`,证据注明);
  2. NUC10 `cache-tokens revoke` 该临时码;
  3. 再跑 `pair --force` → 撞 419(码已吊销)→ 错误文案给出双路径恢复指引(revoke 后重跑 / 换新码);
  4. NUC10 重签码 → `pair --force` 成功,槽材料原子覆盖。
- **判据**:③的 419 文案为「残缺槽可能性」分档形态;④成功且旧槽材料无残留半态。
- **TUI 行为契约【测试代证(2026-09-21 夜链回写)】**:picker `p` 的确认屏与 419 advisory 分档——`[p]` 重配流转 = TestInstancePicker_PKeyRepairsPairedRow / TestInstancePicker_PKeyDefaultRowHint(既有);确认屏 419 advisory 分档 = TestPairWizard_ForceConfirm_AdvisoryTiers(既有,随批5 两级表单形态更新);选择器行状态四要素磁盘源映射与 scoped 门 = TestInstancePicker_FourElementsDiskToRow / TestInstancePicker_ProfileFromScopedMeta(批4 新增,commit 99dce78)。

## GW3 被拒重试

- **CLI 等价面【agent 可跑】**:入网审批被拒(owner 在 NUC10 拒绝配对请求)后,client 侧重试一次成功。
- **判据**:被拒状态可见、重试全链成功;失败路径材料不动(force 零清理先行语义)。

## GW4 Esc 全链 + CLI 回归

- **TUI 行为契约【测试代证(2026-09-21 夜链回写)】**:配对向导各屏 Esc 的退出/返回语义——**形态已两级化(方案 B 两级条件表单已落地,本条代证以两级表单形态为准)**:任一级 Esc = 纯返回页面零残留 = TestPWTwoStage_EscAnyLevelCloses(批5);确认屏 Esc 零残留 = TestPairWizard_ForceConfirm_EscZeroResidue、等待屏 Esc 取消在飞上下文 = TestPairWizard_WaitingEscCancelsCtx(既有,随两级形态更新)。
- **CLI 回归【agent 可跑】**:向导等价的 CLI 路径(`sshmgr pair` 全参数形态)冒烟。

## GW5 `cache instances rm` 真机删除

- **CLI 等价面【agent 可跑,一次性靶子】**:
  1. 临时实例入网;
  2. `sshmgr cache instances ls` 看行状态(产物/DEK/年龄,半态标注);
  3. `sshmgr cache instances rm <临时名>`(输名确认)→ 输出 broker 侧 revoke 与 `--write-mcp` 槽外副本两件配套提示;
  4. 复核:目录与 DEK 双根消失,`ls` 不再列出。
- **判据**:③提示两件配套事项;④双根干净;幂等可重跑(再 rm 报不存在而非报错)。
- **picker ★ 与中文列对齐观感【人工保留面】**:owner 真终端目验一次。

## Plan 45 GW1–G4 配对向导

- **GW1 表单两态分流**:owner 已定**方案 B 两级条件表单**(LAN 发现/手动直连一级分流,直连才展开 URL+pin)——**独立改进项,登记 backlog 未实施**;实施后本条随批验收。**已实施(2026-09-21 夜链,批5,commit 9b904b0 + gofmt 修复 1073f41)**:一级问信任模式,选直连才串联二级(服务地址 + 服务器公钥指纹,均必填,指纹带来源提示);选发现两字段根本不出现且残值强制清空(非隐藏空值);失败路径按级重建。行为契约测试代证 = TestPWTwoStage_LANFlowHidesDirectFields / TestPWTwoStage_DirectFlowSecondLevel / TestPWTwoStage_FailureRebuildsCurrentLevel(批5,先红后绿);**真终端表单观感留 owner 目验(人工保留面,未验)**。
- **GW2–G4 向导全屏走查**:TUI 行为契约【测试代证(2026-09-21 夜链回写)+ owner 目验一次(人工保留面,未验)】——**形态已两级化(方案 B 落地,代证以批5 落地后的两级表单形态为准)**:回环与 Esc 契约 = 四条 TestPWTwoStage 契约测试(LANFlowHidesDirectFields / DirectFlowSecondLevel / FailureRebuildsCurrentLevel / EscAnyLevelCloses)+ 既有回环测试按两级形态更新(约 12–13 个测试函数触及,多为机械驱动改写);`TestPWTwoStage|TestPairWizard -count=5` 零失败;观感走查仍留 owner。

## 附加登记(原 backlog 随批)

- **默认槽 ⚠ 半态豁免语义**:自动归位机器上 `ls`/picker 给默认行挂 ⚠ 的观感——随本册 GW5 时 owner 顺手目验后定豁免或文档说明。
