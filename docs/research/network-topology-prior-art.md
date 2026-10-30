# 网络拓扑先例调研:拓扑建模、多地址 SSH、可视化与给 agent 的呈现

> 调研票:#43(上级地图 #31;为 #35「网络拓扑:数据模型」与 #40「Web UI:网络拓扑图交互原型」提供先例)。
> 调研日期:2026-10-30。只引用一手来源:官方文档、源码仓库、官方发布说明。
> 脱敏:文中示例一律用「家 A」「家 B」「NAS」「工作机 1」以及文档保留网段 192.0.2.x与 198.51.100.x(示例中物理局域网地址用前者,虚拟局域网地址用后者)。

sshmgr 现状(仓库内):服务器条目 `internal/models/models.go` 的 `Server` 只有一个 `Host` 作为 SSH 地址,`Location` 是自由文本;主机密钥的锚归属键是「主机:端口」(见 `CONTEXT.md`「锚定」),条目改地址后旧锚不随之迁移;服务器之间的大文件传输走 Relay(经 broker 内存转发)。

---

## 1. 数据模型:各项目怎样表达站点、网络、地址

### 1.1 各项目怎么做

**NetBox(网络基础设施的事实源工具)**

- 站点(Site):「通常一个站点相当于一栋建筑或一个园区」,可选归属上级区域(Region)。来源:[Site](https://netboxlabs.com/docs/netbox/models/dcim/site/)。
- 位置(Location):站点内的楼层、房间、机笼等,可嵌套,每个位置只属于一个站点。来源:[Location](https://netboxlabs.com/docs/netbox/models/dcim/location/)。
- 设备(Device):属于一个站点,可选属于站点内某个位置;「每台设备可指定一个主 IPv4 地址和/或一个主 IPv6 地址,用于管理」,另可指定一个带外管理地址(通常走物理隔离的管理网)。来源:[Device](https://netboxlabs.com/docs/netbox/models/dcim/device/)。
- 接口(Interface):地址挂在接口上,不直接挂在设备上;接口有速率(单位千比特每秒)、双工、最大传输单元、「仅管理用途」标记;「只有物理接口能用线缆连接」。来源:[Interface](https://netboxlabs.com/docs/netbox/models/dcim/interface/)。
- 网段(Prefix):用无类别域间路由写法表示一个网段;作用范围(scope)可指向区域、站点组、站点或位置(4.2 起替代原来的 site 字段);可选属于一个 VRF(虚拟路由转发表),没有指定 VRF 的网段属于「全局表」。来源:[Prefix](https://netboxlabs.com/docs/netbox/models/ipam/prefix/)。
- IP 地址(IP Address):挂在设备接口、虚拟机接口或网关冗余组上;有角色(环回、次要、任播、虚拟地址等);可记录网络地址转换的内外对应关系(一个内部地址对应一个外部地址,不记录端口级转换)。来源:[IP Address](https://netboxlabs.com/docs/netbox/models/ipam/ipaddress/)。
- VRF(虚拟路由转发表):「每个 VRF 本质上是一张独立的路由表」;**默认允许 VRF 内网段重复**,勾选「强制唯一」才禁止;同一网段要出现在多个 VRF 就各建一份记录。来源:[VRF](https://netboxlabs.com/docs/netbox/models/ipam/vrf/)。
- 线缆(Cable):表示设备部件之间的直接物理连接,两端各一组端点;拓扑靠「线缆追踪」得到:从任一端点出发沿线缆和直通端口一路走到底,界面与 REST 接口都能查。来源:[Cable](https://netboxlabs.com/docs/netbox/models/dcim/cable/)。
- 隧道(Tunnel):「隧道表示两个或多个端点之间建立的私有虚拟连接」;设备或虚拟机接口通过「隧道端点」挂到隧道上;封装方式只列了 GRE、IP-in-IP、IPSec,**没有 WireGuard 类网状虚拟局域网的专门概念**。来源:[Tunnel](https://netboxlabs.com/docs/netbox/models/vpn/tunnel/)。
- 线路(Circuit):运营商提供的点到点物理连接,有承诺速率(千比特每秒)。来源:[Circuit](https://netboxlabs.com/docs/netbox/models/circuits/circuit/)。

**Nautobot(NetBox 的分支项目)**

- 2.0 版删除了 Site 与 Region,统一为「位置(Location)+ 位置类型(LocationType)」,旧数据迁移为对应类型的位置。来源:[Nautobot 2.0 发布说明](https://docs.nautobot.com/projects/core/en/stable/release-notes/version-2.0/)。
- 2.0 引入命名空间(Namespace):「本质上是 IP 地址管理数据唯一性与重复的边界」;同一命名空间里每个网段、每个地址只能有一条记录;官方举例:两家合并公司都用 `10.0.0.0/8`,就分别放进两个并列的命名空间。来源:[Namespace](https://docs.nautobot.com/projects/core/en/stable/user-guide/core-data-model/ipam/namespace/)。

**phpIPAM(IP 地址管理工具)**

- 顶层对象:分区(sections,可嵌套)、子网、地址、VLAN、二层域(l2domains,「管理 VLAN 域」)、VRF、设备;位置与网络地址转换是工具类子对象。按「分区」而不是按地点来隔开重复网段。来源:[phpIPAM 接口文档](https://phpipam.net/api/api_documentation/)。

**Netdisco 与 LibreNMS(自动发现型网络管理工具)**

- Netdisco:选一台支持 CDP、FDP 或 LLDP(链路层邻居发现协议)的设备作为种子,「沿着邻居链发现网络的其余部分」;不支持邻居发现协议的设备,拓扑可以在数据库或网页界面里人工补录(1.x 的拓扑文件可导入)。来源:[App::Netdisco 源码文档](https://github.com/netdisco/netdisco/blob/master/lib/App/Netdisco.pm)。
- LibreNMS:网络图的数据源是邻居发现协议与 MAC/ARP 表匹配,默认两者都用;官方提醒「全局图会变得很大,绘制和响应都慢」,大网络建议看单台设备的邻居图或按设备组看。来源:[LibreNMS Network Map](https://docs.librenms.org/Extensions/Network-Map/)。前端用 vis-network(源码 `resources/views/map/netmap.blade.php` 引入 `vis-network.min.js`,[仓库](https://github.com/librenms/librenms))。
- Zabbix 网络图:元素是主机、主机组、子图、触发器、图片;元素之间的连线由人在图上画,可在连线上显示信息。来源:[Zabbix 网络图配置](https://www.zabbix.com/documentation/current/en/manual/config/visualization/maps/map)。

**Tailscale、Headscale、NetBird(虚拟局域网产品)**

- Tailscale 的节点结构 `tailcfg.Node`:`Addresses`(节点自身的虚拟局域网地址)、`AllowedIPs`(路由到该节点的地址范围)、`PrimaryRoutes`(该节点当前作为主子网路由器承担的网段)、`Endpoints`(「公网地址(经 STUN 探得)以及本地局域网地址」加端口)、`HomeDERP`(默认中继区域)、`Tags`、`Online`。来源:[tailcfg.go](https://github.com/tailscale/tailscale/blob/main/tailcfg/tailcfg.go)。
- Tailscale 子网路由器:节点用 `--advertise-routes` 宣告自己能到达的物理网段,须经管理端批准(或按策略自动批准)才生效;「路由批准决定哪些路由注入客户端路由表」,访问规则另行控制;重叠路由按最长前缀匹配。来源:[Subnet routers](https://tailscale.com/kb/1019/subnets)。
- Headscale(Tailscale 控制服务器的开源实现)沿用同一模型:宣告、批准(`headscale nodes approve-routes`)、`list-routes` 列出「已批准 / 可用 / 正在承担(主)」三列;两个以上节点宣告同一网段时默认启用高可用。来源:[Headscale Routes](https://headscale.net/stable/ref/routes/)。
- NetBird 的「网络(Network)」:「一个映射某个环境(办公室、云上私有网络、机房)的配置容器」;里面有「资源」(单个地址、网段或域名)和「路由节点」(运行客户端、替不能装客户端的设备转发流量的节点);默认拒绝,靠「组 + 策略」放行。来源:[NetBird Networks](https://docs.netbird.io/how-to/networks)。

### 1.2 共同点 / 分歧

共同点:

1. **物理归属与逻辑网络分开建模**。NetBox 的站点/位置管「在哪」,网段/VRF 管「在哪张网里」;NetBird 的「网络」本身就是「某个环境」的容器。
2. **地址挂在「机器在某张网里的成员身份」上,而不是挂在机器上**。NetBox 是「接口上的地址」,Tailscale 是「节点的虚拟地址 + 它能代为到达的网段」。一台机器多张网、多个地址是常态。
3. **网段重复靠「唯一性边界」解决,不靠改地址**:NetBox 的 VRF(默认允许重复)、Nautobot 的命名空间、phpIPAM 的分区。
4. **在多个地址里指定一个「主地址」用于管理**(NetBox 的主 IPv4/IPv6、带外地址)。
5. **虚拟局域网里的「子网路由器」是公认必需的概念**:Tailscale、Headscale、NetBird 都有,且都区分「宣告了」与「已批准、当前在承担」。

分歧:

- 拓扑来源:NetBox、Zabbix 以**人工登记**为主(线缆、连线);Netdisco、LibreNMS 以**自动发现**为主,人工补录为辅;虚拟局域网产品的成员关系由控制服务器**自动掌握**。
- 连通关系的粒度:NetBox 精确到接口与线缆(物理层);虚拟局域网产品只关心「谁能路由到谁」(网络层);Zabbix 只是画图,连线没有语义。
- 地点层级:NetBox 有区域/站点组/站点/位置四层;Nautobot 统一为一种可自定义层级的「位置」。

### 1.3 对 sshmgr 的启示

- sshmgr 的用途是判断「能不能连、走哪条网」,不是管理机房资产,所以只需要网络层粒度:**地点 → 网络 → 网络成员(机器在该网络里的地址)**。接口、线缆、机柜、区域层级都可以省。
- 「家 A 和家 B 都是 192.0.2.x」的问题,按 NetBox VRF / Nautobot 命名空间的思路,**让每个物理局域网自己就是唯一性边界**:判断连通只看「是否同在一个网络」,地址只在网络内部才有意义。这正是 #35 已经提出的方向,有先例支撑。
- 子网路由器值得作为可选概念保留:它决定「只能经虚拟局域网到达的设备」(比如不能装客户端的 NAS)到底能不能到。
- Tailscale 的 `Endpoints` 同时记录公网与本地局域网地址,说明「同一台机器在不同网里有不同地址」由成员身份承载,而不是由机器承载。

---

## 2. 多地址 SSH:同一台主机多个地址,当下用哪个

### 2.1 各项目怎么做

**OpenSSH 客户端配置(`ssh_config`)**

- `Match exec "<命令>"`:在用户的 shell 里执行命令,退出码为 0 即条件成立;配合 `Hostname` 可以「探测到物理局域网可达就用物理局域网地址,否则用虚拟局域网地址」。
- `Match localnetwork <网段列表>`:用本机活动网卡的地址匹配给定网段,「适合在多个网络之间漫游的设备」;官方同时警告「网络地址在很多场景下不是可信的判据(例如由动态主机配置协议自动分配),用它控制安全敏感配置要谨慎」。该关键字在 OpenSSH 9.4(2023-08-10)加入。
- 「每个配置项以最先取得的值为准」,所以更具体的条件要写在前面。
- `HostKeyAlias`:「查找或保存主机密钥时用这个别名代替真实主机名」,用于隧道或一台主机多个入口的场景——**多个地址共用一份主机密钥记录**靠的就是它。
- 来源:[ssh_config(5) 手册](https://man.openbsd.org/ssh_config.5);[OpenSSH 9.4 发布说明](https://www.openssh.com/releasenotes.html)。

示例(脱敏):

```
Match host nas exec "nc -z -w 1 192.0.2.20 22"
    Hostname 192.0.2.20
Host nas
    Hostname 198.51.100.20
    HostKeyAlias nas
```

**Ansible 清单**

- `inventory_hostname` 是主机在 Ansible 里的唯一标识;`ansible_host` 是「实际连接用的可解析名称或地址(当它与别名不同时)」。一台主机只有一个 `ansible_host`,**没有原生的多地址或自动挑选**。
- 一台主机可属于多个组,变量按「全体组 < 父组 < 子组 < 主机」的优先级合并;常见做法是按网络分组、用组变量或不同清单文件切换 `ansible_host`,由使用者决定用哪份。
- 来源:[Ansible 清单指南](https://docs.ansible.com/ansible/latest/inventory_guide/intro_inventory.html)。
- `delegate_to`:把某个任务放到另一台机器上执行,此时连接变量(`ansible_host` 等)取自被委派的机器。这是「从 X 出发去做一件涉及 Y 的事」的先例,但不涉及路径挑选。来源:[Ansible 委派](https://docs.ansible.com/ansible/latest/playbook_guide/playbooks_delegation.html)。

**Teleport(身份感知的访问代理)**

- SSH 服务「主动向 Teleport 代理服务建立反向 SSH 隧道」,客户端流量经这条隧道到达服务器;用户按节点名或标签寻址(`tsh ssh root@env=example`),**根本不选地址**。来源:[Teleport Server Access 入门](https://goteleport.com/docs/enroll-resources/server-access/getting-started/)。

**Termius(图形 SSH 客户端)**

- 一个主机条目由一个地址定义,附带标签;同一主机可配多种协议(SSH、Mosh、Telnet),各有端口与凭据;到达不了的主机用「主机链」(相当于 `ProxyJump`)或 HTTP/SOCKS5 代理。官方文档未见「同一主机多个备选地址」功能。来源:[Termius 术语表](https://termius.com/documentation/glossary);[连接服务器](https://docs.termius.com/organize-and-connect-to-hosts/connecting-to-a-server)。

**Tailscale(在虚拟局域网层自动选路)**

- 节点的 `Endpoints` 同时包含公网与本地局域网地址,客户端在其中挑出直连路径,打不通才经 DERP 中继;`tailscale ping` 默认「建立直连路径后即停止」。来源:[tailcfg.go](https://github.com/tailscale/tailscale/blob/main/tailcfg/tailcfg.go);[How Tailscale works](https://tailscale.com/blog/how-tailscale-works);[Tailscale 命令行](https://tailscale.com/kb/1080/cli)。

### 2.2 共同点 / 分歧

- 决定「用哪个地址」有三种做法:
  1. **由发起方按自己所在网络判断**(OpenSSH 的 `Match localnetwork` / `Match exec` 探测):简单,但依赖发起方本地状态,且网卡地址不可信。
  2. **由使用者手工切换**(Ansible 的组变量 / 多份清单、Termius 一主机一地址):没有自动化。
  3. **把选路下沉到网络层**(Tailscale 自动在局域网直连与中继间挑;Teleport 用反向隧道彻底不暴露地址)。
- 只有 OpenSSH 显式处理了「多地址共用一份主机密钥」(`HostKeyAlias`)。

### 2.3 对 sshmgr 的启示

- **发起方是谁很关键**:sshmgr 的 SSH 连接由 broker(多机形态下也可能是持有网络路径的客户端)发起,所以「用哪个地址」应当以**发起连接的那台机器所在的网络**为准,而不是以 agent 所在机器为准。这和 OpenSSH「按发起方所在网络匹配」同构。
- **主机密钥的锚不能再以「主机:端口」为键**,否则同一台服务器的物理局域网地址与虚拟局域网地址会各需一次锚定,且换地址时旧锚不迁移(`CONTEXT.md` 已记录此现状)。先例是 `HostKeyAlias`:锚应跟随服务器条目(或条目的一个稳定别名),各地址共用。
- 不建议照搬 `Match exec` 式的「每次连接前现探测」作为唯一判据:它慢,而且 OpenSSH 官方自己警告网络地址不可信。推荐「拓扑决定候选顺序 + 连接失败按顺序回退 + 结果记录下来」。

---

## 3. 拓扑可视化与编辑

### 3.1 各项目怎么做

- **NetBox Topology Views 插件**:按 NetBox 里登记的线缆画图,可按名称、站点、标签、设备角色过滤,可导出 draw.io 格式与 PNG;开启 `allow_coordinates_saving` 后可**保存节点坐标**(坐标组,旧的自定义字段 `coordinates` 存为 `X;Y`,已弃用)。前端依赖 `vis-network`、`vis-data`,用 esbuild 打包。来源:[netbox-topology-views 仓库与 README](https://github.com/netbox-community/netbox-topology-views);[package.json](https://github.com/netbox-community/netbox-topology-views/blob/develop/netbox_topology_views/static_dev/package.json)。
- **其他 NetBox 画图插件**:netbox-diagram(把设备分组成图、自动画线缆与线路、用 X/Y 坐标定位,[仓库](https://github.com/netboxdiagram/netbox-diagram/));CESNET/netbox_drawio(在 NetBox 里嵌 draw.io 画图并关联对象,[仓库](https://github.com/CESNET/netbox_drawio));camerongray1515/netbox-diagrams 是早期集成 mxGraph 的半成品,已不活跃([仓库](https://github.com/camerongray1515/netbox-diagrams))。
- **LibreNMS**:网络图与自定义图都用 vis-network,以 `<script>` 直接引入 `vis-network.min.js`([源码](https://github.com/librenms/librenms/blob/master/resources/views/map/netmap.blade.php))。
- **Zabbix**:自绘图,元素与连线都由人在图上摆放。

前端图库对比(都查了官方仓库或文档):

| 图库 | 能否用一个 `<script>` 引入、不要前端构建 | 「网络包含机器」的分组表达 | 图上编辑 | 许可证 |
|---|---|---|---|---|
| vis-network | 能,官方提供 `standalone/umd/vis-network.min.js` | 只有「聚类」(把多个节点折叠成一个),没有真正的容器节点 | 内置编辑模块:加节点、拖线加边、编辑、删除,各步都有回调可接表单 | Apache 2.0 / MIT 双许可 |
| Cytoscape.js | 能,「无外部依赖」,`cytoscape.min.js` 或 ES 模块单文件 | **复合节点**:节点数据里写 `parent` 即成为父节点的子节点,天然对应「地点 ⊃ 网络 ⊃ 机器」 | 核心不带编辑工具栏;官方扩展 edgehandles 拖线加边,其余靠自写 | MIT |
| React Flow(xyflow) | 不能,须在 React 项目里安装 `@xyflow/react`;另有 Svelte Flow,同样需要构建 | 子流程 / 分组节点 | 交互最丰富 | MIT |

来源:[vis-network README](https://github.com/visjs/vis-network)、[vis-network 编辑模块](https://visjs.github.io/vis-network/docs/network/manipulation.html)、[Cytoscape.js 官方文档](https://js.cytoscape.org/)、[React Flow 入门](https://reactflow.dev/learn)、[xyflow 仓库](https://github.com/xyflow/xyflow)。

### 3.2 共同点 / 分歧

- 共同点:成熟项目都把「图」当作**数据的视图**,数据在表单与数据库里,图只多存一份节点坐标(netbox-topology-views 的坐标组)。没有哪个项目把图当唯一事实源。
- 共同点:规模一大全局图就难用(LibreNMS 官方警告),都提供「以某台设备为中心」或「按组」的局部图。
- 分歧:vis-network 生态里的网络管理项目最多(LibreNMS、NetBox Topology Views),上手最快;Cytoscape.js 的复合节点更贴合「网络作为容器」的表达;React Flow 交互最好但必须引入 React 构建链。

### 3.3 对 sshmgr 的启示

- 在 `go:embed` + `html/template` 服务端渲染的前提下(#34 结论),React Flow 出局;vis-network 与 Cytoscape.js 都能以单个静态文件嵌进二进制。
- sshmgr 的拓扑核心是**包含关系**(地点里有物理局域网,虚拟局域网横跨地点,机器同时是多个网络的成员),这与 Cytoscape.js 复合节点一一对应;但「一台机器同属两个网络」在复合节点里只能有一个父节点,需要把虚拟局域网画成跨地点的连线或独立的父节点(这是 #40 原型要验证的点)。
- 编辑走表单、图只负责呈现与少量拖拽(连线 = 新增网络成员),符合所有先例的做法,也让手机上可用。

---

## 4. 给 AI agent 的拓扑呈现

### 4.1 各项目怎么做

- **NetBox 官方 MCP 服务**(netboxlabs/netbox-mcp-server):「刻意保持简单」的只读服务,只有三个通用工具:`get_objects`(按类型和过滤条件取对象)、`get_object_by_id`、`get_changelogs`;支持 `fields` 参数裁剪字段,README 的例子是 50 台设备从约 5000 个词元降到约 500 个。没有专门的拓扑或可达性工具。来源:[netbox-mcp-server README](https://github.com/netboxlabs/netbox-mcp-server)。
- **社区 NetBox MCP**(mhajder/netbox-mcp):在通用增删查改之外提供 `netbox_trace_cable`(「从接口、端口或线路端点出发的完整线缆路径」)、空闲地址分配等专门工具。来源:[mhajder/netbox-mcp README](https://github.com/mhajder/netbox-mcp)。
- **NetBox REST 接口本身**:线缆追踪可从界面或 REST 接口发起,返回完整路径。来源:[Cable](https://netboxlabs.com/docs/netbox/models/dcim/cable/)。
- 虚拟局域网产品的机器可读输出(agent 若在机器上执行命令就能读到):`tailscale status --json`、`netbird status --json` / `--yaml`(见第 5 节)。

### 4.2 共同点 / 分歧

- 官方 MCP 选择「通用对象查询 + 字段裁剪」,把拼拓扑的工作留给模型;社区版本则加了「从某点出发的路径追踪」这类面向任务的工具。
- 都返回结构化 JSON;都把「少给字段」当作节省上下文的主要手段。

### 4.3 对 sshmgr 的启示

- 让 agent 自己从「地点 / 网络 / 成员」原始对象拼出可达性,既费上下文也容易推错(网段重复时尤其容易误判)。应当像 `netbox_trace_cable` 那样提供**面向问题的工具**:以某台机器为中心、直接给出结论(见第 5 节)。
- 照搬官方 MCP 的经验:输出默认精简,详细字段按需取。

---

## 5. 以某台服务器为中心的可达性查询(owner 的核心用法)

核心用法:agent 正通过 sshmgr 操作服务器 X 时,一次查询得到「从 X 出发,哪些设备与 X 同在物理局域网、哪些只能经虚拟局域网到达、哪些不可达」,并知道各路径的瓶颈(虚拟局域网是中继还是直连、物理局域网是千兆还是万兆)。

### 5.1 现有项目是否支持「以某节点为中心」的查询,接口长什么样

| 项目 | 以节点为中心的查询 | 接口形态 | 回答的问题 |
|---|---|---|---|
| NetBox | 线缆追踪:从一个端点出发沿线缆走到对端 | 界面按钮 + REST 接口;社区 MCP 封装成 `netbox_trace_cable` | 「这个口物理上连到哪」,不回答「能不能路由到」 |
| LibreNMS | 单台设备的邻居图 | 网页 | 「这台设备的直接邻居是谁」(来自邻居发现协议) |
| Tailscale | `tailscale status`:列出本机看到的全部对端,每个对端标注连接类型 `direct`、`relay`、`peer-relay`,直连时给出对端地址,中继时给出中继城市代码,附收发字节数;`--json` 输出机器可读结果(`CurAddr` 当前使用的地址、`Relay` 中继区域、`PeerRelay`、`Active`、`Online`、`PrimaryRoutes`) | 命令行 + JSON | 「从本机到每个对端,现在走直连还是中继」 |
| Tailscale | `tailscale ping <对端>`:输出 `pong from <名称> (<地址>) via <路径> in <延迟>`,路径是对端的 `地址:端口`、`DERP(<区域>)` 或 `peer-relay(...)`;默认建立直连后即停止 | 命令行 | 「本机到某一个对端的实测路径与延迟」 |
| Tailscale | `tailscale netcheck`:本机 UDP 是否可用、到各中继的延迟 | 命令行 | 「本机所处网络能否直连」 |
| NetBird | `netbird status -d`:每个对端的连接类型 `P2P` 或 `Relayed`、`Direct` 真假、本地/远端 ICE 候选类型、延迟、收发字节数、经该对端的路由;可按 `P2P`/`Relayed` 过滤;`--json`/`--yaml` | 命令行 + JSON/YAML | 同 Tailscale |
| Ansible | `delegate_to` 只是「在另一台机器上执行」,不做可达性判断 | 剧本关键字 | 不适用 |

来源:[Cable](https://netboxlabs.com/docs/netbox/models/dcim/cable/);[mhajder/netbox-mcp](https://github.com/mhajder/netbox-mcp);[LibreNMS Network Map](https://docs.librenms.org/Extensions/Network-Map/);[Tailscale 命令行](https://tailscale.com/kb/1080/cli);[ipnstate.go](https://github.com/tailscale/tailscale/blob/main/ipn/ipnstate/ipnstate.go);[ping.go](https://github.com/tailscale/tailscale/blob/main/cmd/tailscale/cli/ping.go);[NetBird 命令行](https://docs.netbird.io/get-started/cli);[Ansible 委派](https://docs.ansible.com/ansible/latest/playbook_guide/playbooks_delegation.html)。

要点:**虚拟局域网产品天生就是「以本机为中心」视角**——`status` 给出本机到所有对端的一张表,这正是 owner 要的形状;但它们只覆盖虚拟局域网,不知道「谁和我在同一个物理局域网」。NetBox 一类工具知道物理连接,但不回答路由可达性,也没有中心视角的汇总查询。**没有现成项目把两者合成一张「从 X 出发」的表**,这是 sshmgr 需要自己做的部分。

### 5.2 链路或网络上记录哪些性能属性,人工登记还是自动探测

| 属性 | 谁记录 | 人工 / 自动 | 来源 |
|---|---|---|---|
| 接口速率(千比特每秒)、双工、最大传输单元 | NetBox 接口 | 人工登记(或导入) | [Interface](https://netboxlabs.com/docs/netbox/models/dcim/interface/) |
| 线路承诺速率(千比特每秒) | NetBox 线路 | 人工登记 | [Circuit](https://netboxlabs.com/docs/netbox/models/circuits/circuit/) |
| 「仅管理用途」、带外管理地址 | NetBox 接口与设备 | 人工登记 | [Interface](https://netboxlabs.com/docs/netbox/models/dcim/interface/)、[Device](https://netboxlabs.com/docs/netbox/models/dcim/device/) |
| 直连 / 中继 / 对等中继,中继区域 | Tailscale、NetBird | 自动探测,随时变化 | [Tailscale 命令行](https://tailscale.com/kb/1080/cli)、[NetBird 命令行](https://docs.netbird.io/get-started/cli) |
| 延迟 | Tailscale `ping`、NetBird `status -d`、`netcheck` 到中继的延迟 | 自动探测 | 同上 |
| 收发字节数(可作计费流量依据) | Tailscale `status`、NetBird `status -d` | 自动累计 | 同上 |
| 本机网络能否走 UDP(决定能否直连) | Tailscale `netcheck` | 自动探测 | [Tailscale 命令行](https://tailscale.com/kb/1080/cli) |
| 邻居关系 | Netdisco、LibreNMS | 自动发现(邻居发现协议 + MAC/ARP),Netdisco 允许人工补录 | [Netdisco](https://github.com/netdisco/netdisco/blob/master/lib/App/Netdisco.pm)、[LibreNMS](https://docs.librenms.org/Extensions/Network-Map/) |

规律:**静态、反映硬件规格的属性(带宽、速率)靠人工登记;动态、会随网络环境变化的属性(直连还是中继、延迟、流量)靠自动探测,且只代表「最近一次」**。没有项目把「是否走中继」作为登记字段,因为它随时会变。按流量计费的标记在所调研的一手文档里没有找到专门字段,属于 sshmgr 自己的需求。

### 5.3 对 sshmgr 的启示

- 把「从 X 出发」的结论拆成两层:
  1. **登记层(静态,人工维护)**:X 与 Y 是否同在某个物理局域网、是否同在某个虚拟局域网;物理局域网的标称带宽(例如千兆、万兆);网络是否按流量计费。由此推导出「同物理局域网 / 只能经虚拟局域网 / 不可达」三类。
  2. **探测层(动态,带时间戳)**:虚拟局域网当前是直连还是中继、延迟、上次探测时间。数据来源可以直接借用虚拟局域网客户端的机器可读输出(在 X 上执行 `tailscale status --json` 或 `netbird status --json`),不必自己实现打洞探测。
- 输出要显式区分「登记推导的结论」与「探测得到的事实」,并给出探测时间,避免 agent 把一小时前的「直连」当成现在的事实。

---

## 6. 对数据模型的建议清单

### 6.1 建议采纳的概念

| 概念 | 先例 | 说明 |
|---|---|---|
| 地点 | NetBox 站点、Nautobot 位置 | 只要一层,无需区域/楼层层级;可取代或生成服务器元数据里的自由文本 `Location`(由 #35 定) |
| 网络,分两类:物理局域网(属于一个地点)、虚拟局域网(不属于地点,横跨地点) | NetBox 网段 + VRF;NetBird 网络 | 每个网络就是**地址唯一性边界**(对应 NetBox VRF、Nautobot 命名空间),网段重复不影响判断 |
| 网络成员:机器 × 网络 × 该网络里的地址 | NetBox「接口上的地址」;Tailscale 节点 `Addresses` | 一台机器多个成员身份;SSH 多地址直接从这里来 |
| 设备(不受管机器,例如 NAS、路由器) | NetBox 设备;NetBird「资源」 | 能进拓扑但不是受管服务器;共享存储的提供方可以是它 |
| 子网路由器(可选):虚拟局域网成员替某个物理局域网转发 | Tailscale / Headscale 子网路由、NetBird 路由节点 | 决定「不能装虚拟局域网客户端的设备能否经虚拟局域网到达」 |
| 网络的静态性能属性:标称带宽、是否按流量计费 | NetBox 接口速率、线路承诺速率 | 人工登记,挂在网络上即可(不用细到接口) |
| 探测结果(带时间戳):直连/中继、延迟 | Tailscale `status`/`ping`、NetBird `status -d` | 与登记数据分开存;只作参考,不作判断依据 |
| 节点坐标(仅给图用) | netbox-topology-views 坐标组 | 图是视图,坐标是附属数据 |

### 6.2 可省的概念

- 区域、站点组、多层位置(楼层、房间、机柜)。
- 接口、线缆、线缆追踪、配线架直通端口——sshmgr 关心网络层可达,不关心物理接线。
- VLAN 与二层域(phpIPAM l2domains)——同一物理局域网内的 VLAN 隔离如需表达,登记成两个物理局域网即可。
- IP 地址管理功能:网段利用率、空闲地址分配、地址角色(任播、虚拟地址)。
- 隧道对象与封装类型(GRE、IPSec)——虚拟局域网整体当一个网络。
- 网络地址转换内外地址对应、端口转发——首版可省;若将来要表达「从外网经端口转发进家 A」,再按 NetBox 的「内部地址 ↔ 外部地址」思路加。
- 路由批准状态、访问控制策略——这是虚拟局域网控制端的职责,sshmgr 只记录结果(能否到达)。

### 6.3 SSH 多地址的推荐做法

1. **地址来源**:服务器条目的 SSH 地址不再是单个 `Host`,而是由它的网络成员身份推出的候选列表(每个成员身份一个地址);保留一个可选的「首选地址」覆盖项,对应 NetBox 的「主地址」。
2. **怎样决定用哪个**(以**发起连接的那台机器**为准,不是 agent 所在机器):
   - 发起方与目标同在某个物理局域网 → 先用该物理局域网地址;
   - 否则同在某个虚拟局域网 → 用虚拟局域网地址;
   - 都不在 → 若有子网路由器能到达目标所在物理局域网,用物理局域网地址;否则判定不可达,不盲目尝试。
   - 按上述顺序连接,失败才回退到下一个候选;实际用了哪个地址写进审计与返回结果。
   - 不以「发起方网卡地址落在哪个网段」作为判据(OpenSSH 官方警告网络地址不可信;且家 A 与家 B 网段可能重复),而以「发起方登记在哪个网络」为判据。
3. **主机密钥锚**:锚改为跟随服务器条目(先例:OpenSSH `HostKeyAlias`),同一服务器的所有地址共用一份锚,避免同一台机器因地址不同而要重复锚定、换地址后旧锚失效。这会改动 `CONTEXT.md`「锚定」的定义,需要 owner 拍板。
4. **Relay 的含义**:服务器之间的大文件传输目前经 broker 内存中转,两跳各自按上面的规则选地址;「大文件走物理局域网」只有在 broker 与两端同在一个物理局域网时才真正成立。若要 X 与 Y 之间直接走它们共有的物理局域网,需要「在 X 上直接向 Y 推送」这类新传输方式,超出本票范围,列为待决。

### 6.4 可视化图库推荐及理由

**推荐 Cytoscape.js,备选 vis-network;不用 React Flow。**

- 两者都能以单个静态 JavaScript 文件通过 `go:embed` 嵌进二进制,用 `<script>` 引入,不需要前端构建链,符合 #34 的服务端渲染结论。React Flow 必须在 React 构建链里用,排除。
- Cytoscape.js 的复合节点(`parent`)天然表达「地点 ⊃ 物理局域网 ⊃ 机器」的包含关系,而 sshmgr 拓扑的本质正是包含关系;零外部依赖,MIT 许可。
- vis-network 的优势是自带编辑工具栏(加点、拖线、删除都有回调,可直接接表单),且是网络管理项目里最常用的(LibreNMS、NetBox Topology Views 都用);缺点是没有真正的容器节点,只能用聚类或手画背景框表达「网络」。
- 编辑以表单为主、图上只做「拖线新增成员身份」与「拖动保存坐标」,这样哪个库都够用,也照顾手机。最终用哪个,建议在 #40 原型里两个各做一版给 owner 在手机上比较。

### 6.5 给 agent 的呈现格式推荐

提供一个以服务器为中心的工具(名称待定,例如「从 X 出发的可达性」),一次返回分组结论,而不是原始的地点、网络、成员对象。建议的 JSON 形状(示例已脱敏):

```json
{
  "from": "工作机 1",
  "as_of": "2026-10-30T10:00:00Z",
  "same_physical_lan": [
    {"name": "NAS", "network": "家 A 物理局域网", "address": "192.0.2.20",
     "bandwidth": "10G", "metered": false}
  ],
  "virtual_lan_only": [
    {"name": "工作机 2", "network": "虚拟局域网", "address": "198.51.100.12",
     "probe": {"path": "relay", "relay_region": "示例区域", "latency_ms": 85,
               "probed_at": "2026-10-30T09:40:00Z"}}
  ],
  "unreachable": [
    {"name": "打印机", "reason": "只在家 B 物理局域网,且无子网路由器"}
  ]
}
```

要点:

- **分三组**(同物理局域网 / 只能经虚拟局域网 / 不可达),直接对应 owner 的问法,agent 不用自己推理。
- 每项带**选中的地址与网络名**,以及**瓶颈提示**:物理局域网的标称带宽、是否按流量计费;虚拟局域网的最近探测结果(直连或中继、延迟、探测时间)。登记推导的字段与探测字段分开放,探测字段必须带时间。
- 默认精简,详细字段(全部候选地址、全部网络成员身份)按参数展开——照搬 NetBox 官方 MCP「`fields` 裁剪」的经验。
- 只列出名称与网络,不暴露 agent 不需要的东西;地址是否明文返回沿用现有 `ExposeHost` 的遮蔽规则。

### 6.6 需要 owner 拍板的分歧点

1. 主机密钥锚是否从「主机:端口」改为跟随服务器条目(影响 `CONTEXT.md`「锚定」「锚定转发」定义与存量数据迁移)。
2. 「用哪个地址」以哪台机器所在网络为准:broker / 发起连接的客户端(建议),还是 agent 所在机器。
3. 是否接入虚拟局域网客户端的机器可读输出(`tailscale status --json`、`netbird status --json`)作为探测层数据源,还是首版只做登记层。
4. 「大文件走物理局域网」是否需要新的点对点传输方式,还是接受现有 Relay 经 broker 中转的路径限制。
5. 图库在 Cytoscape.js(包含关系表达好)与 vis-network(自带编辑、生态常见)之间,交给 #40 原型比较后决定。
