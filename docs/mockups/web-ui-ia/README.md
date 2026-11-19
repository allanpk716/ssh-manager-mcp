# 权威端 Web UI 信息架构重构 · 定稿 mock

2026-11-18 owner 在 mockit 上逐批审定的静态原型(手机优先,假数据,单文件 HTML,浏览器直接打开)。决定与理由见 `docs/adr/0021-web-ui-mobile-first-client-instance-centric.md`,领域词见根目录 `CONTEXT.md`。实现以本目录页面为界面依据;mock 与 ADR、CONTEXT.md 不一致时以后两者为准。

| 文件 | 页面 | 可点的演示交互 |
|---|---|---|
| `home.html` | 主页(状态总览) | 待处理卡片、计数磁贴 |
| `servers.html` | 服务器列表 | 筛选胶囊、「多选」→ 批量授权抽屉 |
| `server-detail.html` | 服务器详情(连接 / 地址与地点 / 授权 / 软件) | 分段切换、「更改」地点树形选择器与新建地点、授权开关确认 |
| `server-new.html` | 新增服务器三步流程 | 私钥选文件、地址填 192.168.100.235 触发同机识别、模拟连接测试 |
| `clients.html` | 客户端列表与详情 | 更换 profile 预览、所在机器与软件清单、laptop-work 未关联机器演示 |
| `pairing.html` | 配对审批 | 倒计时、比对勾选后才可批准 |
| `authorization.html` | 授权 → 机密项 / 共享存储 | 备注必填的新建表单、批量勾选服务器与确认页、实际能拿到它的客户端 |
| `more.html` | 更多 + 地点与网络 | 地点树、网络列表 |

已知 mock 层面的瑕疵(实现时按规格做,不照抄):授权块中资源名与说明须分两行显示;mock 里的地址、机器名均为示例数据。
