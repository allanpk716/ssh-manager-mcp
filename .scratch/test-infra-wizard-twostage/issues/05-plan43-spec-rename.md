# 票 05 · Plan 43 待审规格的更名适配(批3 前置小件)

## What to build
doctor 探活二期的待审规格 `docs/superpowers/specs/2026-08-28-plan-43-doctor-serve-probe-design.md.rev2.1.md` 定稿于二进制改名(ssh-manager → sshmgr)之前,冻结文案里的命令引用还是旧名。做一轮**纯更名适配**:把该文件里旧二进制名形态的命令引用(`ssh-manager xxx`、`ssh-manager(.exe)` 等)换成现行名(`sshmgr`),owner 审阅的即是终稿。

**边界(硬性)**:
- 只改这一个文件(rev2.1);更早的修订链历史文件(rev1/rev2/基稿)不动;
- 不改任何技术内容、决策、结构、编号——diff 里除命令名替换外不应有任何其他变化;
- 若文件里存在「ssh-manager」作为**历史事实陈述**(如「改名前的旧名是 ssh-manager」这类语义),保留不改——只改「当作现行命令在用」的引用。逐处判断,拿不准的列出来回报。

## 验收标准
- [ ] `git diff` 审看:改动全部是命令名替换(或其直接伴生形态如 `ssh-manager-serve` 服务名——该服务名在 v0.13.0 已一并改为 `sshmgr-serve`,同样替换)
- [ ] 替换后 `grep -n "ssh-manager" <文件>` 结果逐条复核:剩余处均为历史事实陈述或语义需要,逐条列理由
- [ ] 技术内容零变化(无段落增删、无措辞改写)

## Blocked by
无,可立即开始。

## 涉及路径
docs/superpowers/specs/2026-08-28-plan-43-doctor-serve-probe-design.md.rev2.1.md

## 副作用声明
无(纯文档编辑;默认只跑类型检查/单文件测试的缺省不适用,无验证命令)

## decision_refs
D1(批3 前置小件在夜链范围)、D6(写作规范——替换不引入缩写)

## review_blocks
无
