# 重新生成单机教程截图

这份说明只负责重新生成 [`../../../tutorial-single-machine.md`](../../../tutorial-single-machine.md) 使用的终端截图。截图终端固定为 120 列 × 32 行：每个脚本使用默认深色主题、字号 16，并把画布设为 1200 × 640 像素。脚本只用 `Screenshot` 输出静态 PNG；`/tmp/sshmgr-tutorial-frame.mp4` 是 vhs 的临时视频轨，会被后一个脚本不断覆盖。

## 准备

需要 `go`、`docker`、`mktemp`、`python3`、`vhs`、`ttyd`、`ffmpeg`，并确保 `lsof` 可用。不要改动生产环境里的 sshmgr 数据。

在 `docs/images/single-machine/tapes` 目录执行：

```bash
./setup-demo.sh
```

脚本会：

1. 用 `mktemp -d` 创建一个全新的演示目录；
2. 把 `SSHMGR_VAULT_ROOT`、`SSHMGR_STORE`、`SSHMGR_FILEKEY_PATH`、`SSHMGR_CONFIG_ROOT`、`SSHMGR_CACHE_DEK_DIR` 全部指向该目录；
3. 编译 `./cmd/sshmgr` 到演示目录；
4. 启动名为 `sshmgr-tutorial-demo` 的 OpenSSH 服务器容器，监听 `127.0.0.1:2222`，用户名 `deploy`，密码 `deploy-password`。

生成的 `.demo-env` 会把演示终端的 `HOME` 也切到演示目录，避免截图出现真实主机名、用户名或真实路径。

## 录制顺序

这些脚本共享同一个演示 vault，必须按顺序执行；中途失败时先执行 `./teardown-demo.sh`，再执行 `./setup-demo.sh` 从头开始。

```bash
vhs 01-binary-version.tape
vhs 02-wizard-first-question.tape
vhs 03-wizard-standalone.tape
vhs 04-server-basics.tape
vhs 05-server-password.tape
vhs 06-wizard-continuation.tape
vhs 11-main-servers.tape
vhs 12-main-profiles.tape
vhs 13-main-projects.tape
vhs 14-claude-code.tape
```

`06-wizard-continuation.tape` 是连续流程，会依次输出 `06-profile-name.png` 到 `10-mcp-config.png`。它还会把一张只用于驱动状态机的中间帧写到 `/tmp/sshmgr-tutorial-discard.png`，那张图不属于教程素材。

`06-wizard-continuation.tape` 按一次空格后截图，所以 `07-profile-grant.png` 必须显示 `✓ 1. demo-gpu`；提交后的状态必须显示授权 1 台服务器，否则重录。

`14-claude-code.tape` 会先轮换 `my-agent` 的 project token，生成只在演示目录中使用的 `.mcp.json`，再启动 Claude Code 的非交互模式。`.mcp.json` 的 `command` 使用临时编译出的 sshmgr 绝对路径，`env` 同时带上 token 和上面五个隔离环境变量；这些路径不会出现在截图里。命令使用 `--output-format text` 和 `--allowedTools='mcp__ssh__*'`，画面只保留人能读懂的请求、服务器列表和 `uname -a` 输出。

## 清理

```bash
./teardown-demo.sh
```

脚本会删除 `sshmgr-tutorial-demo` 容器、演示目录里的文件，以及 `tapes` 目录下的 `.demo-root` 和 `.demo-env`。为了保留审计线索，演示目录本身不会被递归删除；脚本会打印它的路径。
