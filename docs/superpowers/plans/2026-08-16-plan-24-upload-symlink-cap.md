# Plan 24：upload symlink 跟链过 cap 门（单任务微修）

> 2026-08-16。来源：Plan 23 评审 Low 发现。OWNER 拍板「修」。

## 问题

`uploadDir` 预检读 `filepath.Walk` 的 lstat `FileInfo`——符号链接报**自身**大小（几十字节），指向巨物的 symlink 绕过单文件 cap 门；而实际传输时 `uploadFile` 打开路径**跟链**（目标内容上传）——检查与行为不一致，门被绕。单文件路径（`Upload` 用 `os.Stat`）本就正确。

## 修法（与实际行为对齐：跟链取真实尺寸）

`internal/sshbroker/upload.go` `uploadDir` walk 回调：当条目 `info.Mode()&os.ModeSymlink != 0` 且 `maxBytes > 0` 时，对该路径再显式 `os.Stat`（跟链）取真实尺寸参与 cap 判定；`os.Stat` 失败（断链）→ 作为 walk 错误传播（不静默跳过）。非 symlink 条目维持现状（Walk 的 FileInfo，无额外 syscall）。恰好 == cap 允许、> cap 拒绝的语义不变。

## 测试

- 失败先行（in-process testsshd 模式，同文件既有 helper）：目录内 small.txt + symlink→cap+1 大文件 → 现状（小尺寸过门、目标内容上传）必须先红；修后：错误点名 symlink、目标零字节、small.txt 保留。
- 断链 symlink → 报错（含路径）。
- symlink→小文件（≤cap）→ 正常上传目标内容（跟链语义不被本修破坏）。

## 边界

- 不改「上传跟链」本身（scp -r 对齐语义）；不做跳过 symlink 选项；单文件路径不动。
- Docker 差分不强制加 symlink 用例（Windows symlink 创建需特权，gate 脆化不值）——in-process 足够钉。
