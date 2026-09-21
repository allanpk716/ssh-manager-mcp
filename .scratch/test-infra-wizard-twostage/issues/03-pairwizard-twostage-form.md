# 票 03 · 配对向导两级条件表单 + 向导回环契约测试(批5)

## What to build
把配对向导(client TUI 的入网向导,`sshmgr pair` 的交互面)从单层表单改成**两级条件表单**(owner 已拍板方案 B):

- **一级表单**只问信任模式:局域网发现 / 手动直连;
- 选「手动直连」→ 串联**二级表单**:服务地址 + 服务器公钥指纹两个字段,指纹**必填**并带指纹来源提示;
- 选「局域网发现」→ 这两个字段**根本不出现**(不是隐藏空值)。

技术约束(已定,勿重新发明):
- 表单库(huh v2)无字段级显隐能力 → **拆两级表单串联**实现;
- 失败路径**重建**表单而非复用实例(沿用 T2-R1 死锁教训纪律);
- **Esc 中止契约沿用现行语义**(clientpage.go:323-331,Plan 45 T3):「任意向导步骤 Esc = 纯返回页面、零残留」——两级形态下**任一级** Esc 均为同一中止契约,不自创新行为。若实现中发现该语义无法直接映射两级形态,**停下来回报待决**(NEEDS_CONTEXT),不临时发明行为。

**TDD 顺序(硬性)**:先写两级形态的行为契约测试(跑红),再实现表单改造(跑绿)。契约测试至少钉住:
1. 选「局域网发现」→ 服务地址与服务器公钥指纹两字段不出现;
2. 选「手动直连」→ 两字段出现,指纹必填(空值提交被拒);
3. 失败路径:表单重建而非复用(重建后字段状态干净);
4. 任一级 Esc = 纯返回页面、零残留(沿用 pairWizardClosedMsg 既有通道)。

既有向导回环相关测试按新两级形态更新(它们当前按单层形态写;更新不是删断言,是按新契约重写——若某既有断言与新形态冲突,逐条说明)。

## 验收标准
- [ ] 新契约测试四条全绿(先红后绿的 TDD 过程在回报中说明)
- [ ] 既有向导回环测试更新后全绿;被更新的断言逐条有理由
- [ ] `go test ./internal/tui/` 全绿
- [ ] 计时对照:`go test ./internal/tui/ -count=1` 相对票 01 留档基线的增量写进回报(新增测试的耗时增量记账,供批4「不回涨」判据用)
- [ ] Esc 契约未自创(实现代码里向导中止仍走既有 pairWizardClosedMsg / 纯返回通道)

## Blocked by
票 01(测试辅助函数免等待改造先行落地,本票新测试直接用新姿势;且避免同包并发写文件互相污染验证)

## 涉及路径
internal/tui/pairwizard.go
internal/tui/wizardsteps.go
internal/tui/wizardsteps_test.go
internal/tui/pairwizard_test.go
internal/tui/wizard_test.go
internal/tui/wizard_routing_test.go
internal/tui/pairing_test.go
internal/tui/wizardsteps_docsync_test.go
(向导表单定义与其测试;若配对客户端流程文件 pairing.go 需同步,含 internal/tui/pairing.go)

## 副作用声明
- 独占验证命令:`go test ./internal/tui/...`(实施期间独占本包测试运行)
- 测试产物目录:Go 构建缓存(不进仓库)

## decision_refs
D4(方案 B / 两级串联 / 失败路径重建)、D9(向导回环契约测试唯一归属本票 + Esc 沿用现行语义不自创)、D6(判据不放宽)

## review_blocks
无
