# 概念模型图解（一页看懂多机架构）

> **这是什么**：`ssh-manager` 多机部署的概念参考页——数据怎么流、每样东西是什么角色。不是教程（装好跑通的步骤见 [getting-started.md](./getting-started.md) / [quickstart-multi-machine.md](./quickstart-multi-machine.md)），是"记不清谁是谁"时回来翻的那一页。首次向导首屏也指向这里。

---

## 数据流图

```
┌──服务器机（server · 唯一的仓库，vault 只在这一台）──────────────────┐
│  ssh-manager tui（broker 主控台 · 四个页签）                        │
│                                                                    │
│  ┌──────────┐   ┌───────────┐   ┌──────────┐   ┌──────────┐      │
│  │ 服务器页   │   │ Profiles  │   │ Projects │   │ 设备码页  │      │
│  │ = 货架     │   │ 页=装箱单  │   │ 页=钥匙   │   │ = 水管钥匙 │      │
│  └────┬─────┘   └─────┬─────┘   └────┬─────┘   └────┬─────┘      │
│       │ grant 进箱子        │ 绑定一个箱子   │            │              │
│       └──────────→ profile ─┴──→ project token │      │ 一台 client   │
│                                                │      │ 一枚、可吊销   │
└────────────────────┬────────────────┬──────────┼──────┼──────────────┘
                     │                │          │      │
   agent 的 MCP 调用    │                │          │      │  cache pull 的取货凭证
   （project token 作   │                │          │      │  （设备码作 Authorization，
    阀门，逐命令把关）    │                │          ▼      ▼   指纹钉死 TLS）
   ┌.mcp.json 指→──────┐│                │  ┌──────────────────────────┐
   │ serve URL（在线）   ││                │  │ GET /snapshot（整仓快照）  │
   └────────┬─────────┘│                │  └────────────┬─────────────┘
            │          │（离线模式：token 不出本机，              │
            │          │  在 cache.bin 里本地验证）              │
            ▼          ▼          ▼                             ▼
┌──client 机（工作机，零 vault）─────────────────────────────────────┐
│  cache pull → cache.bin（加密的整仓只读快照）                      │
│  .mcp.json: mcp --cache（token 走 env SSHMGR_TOKEN，离线兜底，     │
│        token 只在本机对着 cache.bin 验证，不打网络）                │
│        或  指向 serve URL（在线，走 MCP 端点——不是 /snapshot）      │
│                                                                    │
│  agent 只看得见 / 碰得到：自己 project 绑定的 profile 里的服务器    │
│  （= 一把钥匙只开一个箱子，箱子外的货架对它不存在）                 │
│  ⚠ 设备码只打 /snapshot；project token 只打 MCP 端点——两道闸      │
│    永不互认（project token 拿不到整仓快照，设备码当不了 agent 钥匙） │
└────────────────────────────────────────────────────────────────────┘
```

## 类比表（仓库隐喻）

| 概念 | 类比 | 说明 |
|---|---|---|
| 服务器机（server） | **仓库** | vault 只在这一台机；所有凭据、所有页签操作都在这 |
| TUI「服务器」页 | **货架** | 所有 SSH 目标机（连凭据）摆在架子上 |
| TUI「Profiles」页 | **装箱单** | 把若干服务器打包进一个 profile（箱子） |
| TUI「Projects」页 | **钥匙** | 每把钥匙（token）只开一个箱子（绑定的 profile） |
| **设备码** | **水管钥匙** | 允许一台 client 机通过"水管"拉走整仓**加密**快照（cache.bin）；一台 client 一枚，吊销粒度=单机 |
| **project token** | **阀门** | 决定 agent 实际能用什么——只放行自己 profile 箱内的服务器，铁律逐条命令检查 |
| **服务器指纹（pin）** | **防伪封条** | server 自签证书的 SPKI 指纹；client 首次连接必须核对（钉死），不符即拒——防冒充的 server / 中间人 |
| cache.bin | 水管放出来的**加密整箱货物** | 只读快照，断网兜底；解它需要本机 cache-dek.key |

## 设备码的两种输入形态（等价，仅 `cache pull` 命令行）

`cache pull` 的命令行侧两种填法完全等价，任选其一：

1. **分开给**：`--token <裸码>` + `--pin sha256:...`（或环境变量 `SSHMGR_SERVE_PIN`）。
2. **合并串**：只填一个 `<码>:<指纹>` 字符串（如 `cache pull --token 'abc123:sha256:abcd...'`），程序自动拆开。

> 注意：**向导 / client 面板的表单是分开的三个必填栏**（地址 / 设备码 / 指纹），不接受合并串——指纹栏必须单独填 `sha256:...`。

⚠️ 指纹三处（合并串 / `--pin` / 环境变量）一处都没有 → **默认拒连**（hard-fail），不会静默明文。

## 第二台 client 机的完整接入链

1. **server 机 TUI「设备码」页按 `a`** 签发设备码（一台 client 一枚；**不推荐复用**——复用则失窃时只能全吊，一枚一机吊销才精准）。
2. **server 机 TUI「Projects」页按 `a`** 新建 project（绑定想给它看的 profile）→ **token 一次性展示**（丢失可在此页重发）。
3. **client 机**跑 `ssh-manager tui` 进向导（表单分三栏填：server 地址 / 设备码 / 指纹）或直接：
   ```bash
   ssh-manager cache pull --url https://<server>:7878 --token '<码>:sha256:<指纹>'
   ```
4. client 机 `.mcp.json` 配 `mcp --cache` + env `SSHMGR_TOKEN=<project token>`（离线兜底）或指向 serve URL（在线）。

project/token 按 agent 项目粒度自由建（每台机每项目一个都行）；设备码按**机器**粒度发。

## 三种角色与首次向导（一段话版）

本机角色由 `role.json` 唯一确定：**standalone**（单机，凭据只在本机）、**server**（仓库机）、**client**（只连仓库的工作机）。空机器第一次跑 `ssh-manager tui` 进**首次向导**：首屏两问（这台机保管凭据吗？agent 要连别的机吗？）后果导向地选角色，**选定的瞬间即落盘 role.json**——中途 Esc / 崩溃都是安全暂停，重开 `tui` 自动续配。standalone 之后可无损升级为 server（主控台按 `[u]`，vault 数据原样保留）；**vault 角色（standalone/server）转 client 必须先 `clear`**（真删数据，见下）。

## `ssh-manager clear`（角色清理，一段话版）

把本机**按实际存在枚举**的 vault / serve 证书服务 / client 缓存残留 / 遗留定时器（Windows 计划任务 `ssh-manager-cache-refresh`）/ role.json 全部删除，回到首次向导状态。流程：列出清单 → 输入 `DELETE` 确认（输错即取消，零改动）；vault 角色先自动 export 一份口令加密备份（回读校验 + 抄录口令确认）才开始删——vault 锁定时拒绝无安全绳删除。全程**幂等**：中断后重跑，已完成的步骤跳过。exe 永远保留。
