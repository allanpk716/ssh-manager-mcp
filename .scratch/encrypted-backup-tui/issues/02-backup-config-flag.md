# 票 02 — --config 旗标 + backup.json 备份配置加载与校验

## What to build

owner 把备份目录/保留份数/口令文件路径写进 vault 目录的 `backup.json`,一条 `sshmgr backup create --config <路径>` 即可跑加密备份,计划任务/TUI/手工三入口共用同一事实源。配置读取做全部校验并 fail-closed,任一不过即报错退出不落半套行为。

## 验收标准

- [ ] `--config` 读入三字段(dir/keep/passphrase_file)作为对应旗标缺省值;旗标 > 配置 > 内置默认优先级有测试钉死
- [ ] 三字段全必填:文件不存在/缺任一字段/坏 JSON → 明确报错退出
- [ ] 两路径字段均须绝对路径:任一相对路径 → 报错
- [ ] 口令文件位置不变式:passphrase_file 位于 dir 子树内(规范化后前缀判定)→ 报错拒绝,报文说明口令文件不能进备份目录
- [ ] 经 `--config` 进入加密模式与直连旗标行为一致(复用票 01 全链路,含跑前世代校验)
- [ ] 无 `--config` 且无口令时:既有明文用法照绿(回归)

## Blocked by

票 01

## 涉及路径

- internal/cli/backupconfig.go(新建)
- internal/cli/backup.go(--config 挂接)
- internal/cli/backupconfig_test.go(新建)

## 副作用声明

- 独占验证命令:`go test ./internal/cli/ -run 'TestBackup'`

## decision_refs: D7(含 F3 子树禁入不变式落地)
## review_blocks: 无
