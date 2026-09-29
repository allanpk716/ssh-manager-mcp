# 票 01 — backup create 加密模式全链路(CLI 直连旗标)+ verify .sme

## What to build

owner 用一条命令把整个 vault 备份成口令加密文件并可校验:`sshmgr backup create --dir <目录> --passphrase-file <文件> [--keep N] [--prefix vault]` 产出 `vault-<UTC时间戳>.sme`(口令加密信封封全量快照),内建跑前口令世代校验、写后解密验证、按扩展名轮转;`sshmgr backup verify <file> --passphrase-file <文件>` 对 `.sme` 做解密级校验。不带口令时既有明文模式逐字节不变。

## 验收标准

- [ ] 加密模式产物以 `SSHMGRV1` 开头,文件名 `vault-<UTC>.sme`,同秒连跑沿用既有 `-2/-3` 后缀不覆盖
- [ ] 跑前世代校验:dir 已有 `.sme` 且口令解不开最新一份 → 报错退出,不写新文件;dir 无 `.sme` → 正常跑
- [ ] 写后验证:写完重读+试解密+重解析,篡改一字节或口令错 → 报错(测试模拟)
- [ ] 轮转:`--keep N` 只管 `*.sme`,同目录 `.json`/`.sme` 混放互不误删,无孤儿残留
- [ ] marker/`.git`/并发锁/原子写复用既有路径,加密模式同样生效
- [ ] 往返等价:测试夹具 vault → create 加密产物 → `sshmgr import` 导入临时 vault → 服务器/凭据/profile/项目/grants 条数与字段全等
- [ ] `backup verify`:`.sme`+口令(完好/篡改)与无口令(报"加密文件需要 --passphrase-file")三分支;明文 `.json` 分支行为不变
- [ ] 无口令旗标时:既有明文模式全部测试不改一行照绿(`go test ./internal/cli/`)

## Blocked by

无,可立即开始

## 涉及路径

- internal/cli/backup.go
- internal/cli/backup_encrypted_test.go(新建)
- internal/cli/backup_test.go(如需补夹具)

## 副作用声明

- 独占验证命令:`go test ./internal/cli/ -run 'TestBackup'`(以及往返等价所需的 import 相关既有测试)

## decision_refs: D1、D4、D7、D8
## review_blocks: 无
