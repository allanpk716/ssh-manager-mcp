# Plan 48 验收册:锚定转发 + 带外锚定(v0.15.0 起)

> 来源:spec §12(`docs/superpowers/specs/2026-09-09-plan-48-pin-forwarding-design.md.rev3.md`);术语见 CONTEXT.md「锚定/锚定转发/带外锚定」。
> 环境:NUC10(serve,经 SSH 工具面可达)+ 笔记本(本机)+ 生化高速工控板 192.168.1.108(仅笔记本直连网卡可达)。

## 前置

- [ ] 双端 ≥ v0.15.0 且 compat-matrix 已登该组合行
- [ ] NUC10 `sshmgr doctor` 0 WARN 0 FAIL
- [ ] 工控板条目已在库且已锚(首次信任应已随 v0.15.0 部署完成;未锚则 A1 即首次锚定)

## 状态(2026-09-21 首轮 agent 代跑)

> 运行记录与完整证据:[runs/2026-09-21-plan48-pilot.md](./runs/2026-09-21-plan48-pilot.md)。

| 项 | 判定 | 说明 |
|---|---|---|
| A1 | 部分过 | 判据②③过(2026-09-10 首锚历史行 + 显示三向一致);①延后——工控板断电,待 owner 上电补跑 |
| A2 | 延后 | 无现成「仅笔记本可达的新目标」一次性靶子 |
| A3 | 延后 | 需工控板可达(快照侧已核锚在:source=forward, device=laptop-v040) |
| A4 | 延后 | 建议 owner 裁决降级「单测代证」——构建 v0.14.0 旧客户端成本高、混布窗口生产已不存在 |
| A5 | 过 | copy-probe 12=servers ls 12;凭据 14=快照引用 13+孤儿 1(gc 干跑解释);`ssh -- echo` 抽查通 |
| A6 | 部分过 | 锚生命周期六步过(含⑤受影响清单一致);「revoke 有痕」子判据不满足(`cache-tokens revoke` 不写审计行,登记 backlog);临时实例删除需交互终端(owner 待办) |

## A1 反馈场景三步复跑【agent 可跑:本机工具面 + NUC10 远程】

- **步骤**:
  1. 笔记本经 MCP 工具 `exec_command` 对工控板执行一次命令(如 `echo ok`);
  2. NUC10 上 `sshmgr audit --action pin-forward`(或 audit 过滤等价形态)查转发行;
  3. NUC10 上 `sshmgr servers pin-hostkey 生化高速工控板`(显示形态,无参数)。
- **取证**:三步命令原文+完整输出;audit 行完整原文;工具调用记录。
- **判据**:① exit 0;② audit 行字段齐(设备名 + 指纹 + affects 清单);③ 显示的已锚指纹与转发时一致、`pin_source=forward`、`pin_device=笔记本设备名`。

## A2 真离线带外锚定闭环【agent 可跑(软隔离形态);拔线形态属人工保留面】

spec 原文为「拔 VLAN 真离线」。等价软隔离:**目标机防火墙拦截 NUC10 网段**(agent 经 SSH 工具面在目标机上加一条临时 iptables/防火墙规则,broker 即不可达而笔记本可达);物理拔线形态留给 owner 时,其余步骤相同。

- **步骤**(对**另一**新目标——一次性靶子条目,跑完清理):
  1. 目标机加临时防火墙规则挡 NUC10;
  2. 笔记本对目标首连 → 应得 §4 新文案(含呈现指纹,fail-closed);
  3. 目标机上 `ssh-keyscan` 取主机公钥(或读 `/etc/ssh/ssh_host_ed25519_key.pub` 算指纹);
  4. NUC10 `sshmgr servers pin-hostkey <靶子名> --fingerprint SHA256:...`(带外锚定);
  5. 目标机撤临时规则;
  6. 笔记本 `sshmgr cache pull` 后连接成功。
- **取证**:每步命令原文+输出;②的完整错误文案;④的指纹与③来源对照。
- **判据**:②文案为 §4 契约形态(含 presented fingerprint);⑥连接成功且 NUC10 侧锚来源=manual。
- **清理**:删除靶子条目与其锚(`servers rm` + `servers pin-hostkey --clear --hostport <靶>:22`)。

## A3 跨会话 409 equal 自动放行【agent 可跑:本机】

- **步骤**:笔记本起新 `sshmgr mcp --cache` 进程(initialize + tools/list 冒烟同 v0.16.0 部署验证姿势),再对已锚目标(工控板)发起 `exec_command`。
- **取证**:MCP 冒烟输出 + 连接结果;若文案可见,留 409 equal 提示原文。
- **判据**:零用户动作连接成功(等值锚自动放行,无假 MITM 警报)。

## A4 混布假警报抽查【agent 可跑(需备旧版);成本高,owner 可裁决降级】

spec 原文:「一台仍跑 v0.14 的客户端连接指纹锚目标 → 观察 possible MITM 假警报」。现双端均 ≥ v0.15,无存量旧机。等价法:**从 git tag v0.14.0 源码构建旧 client,以 `SSHMGR_CACHE_DIR` 环境变量指向临时缓存目录对拷贝的指纹锚快照跑一次连接**。
- **判据**:旧 client 将指纹锚按密钥字节解读,报「possible MITM」假警报(留档即证明混布窗口真实形态)。
- **降级选项**:该行为已由单元矩阵钉住契约时,owner 可裁决免真机重演,登记「单测代证」。

## A5 doctor 计数一致【agent 可跑:NUC10】

- **步骤**:NUC10 `sshmgr doctor`(看 copy-probe 的 servers/credentials 计数)与 `sshmgr servers ls` / `sshmgr ssh <名> -- echo` 口径对照。
- **判据**:doctor copy-probe 计数与库实况一致(WAL checkpoint 修复后不应再有「差一」)。

## A6 清毒演练【agent 可跑;一次性靶子护栏——不碰笔记本生产设备码】

- **步骤**(全部用一次性材料:临时设备码 + 临时实例 + 靶子条目):
  1. NUC10 `sshmgr cache-tokens add --name <临时名> --profile <测试轮廓>` 签发临时码;
  2. 笔记本 `sshmgr pair --instance <临时名> --url <serve地址> --pin <SPKI指纹>`(带 `SSHMGR_PAIR_ASSUME_SAS=1`,证据注明)发起入网,随后轮询等审批;
  2b. NUC10 `sshmgr serve pair ls` 核对待审行(含 SAS)→ `sshmgr serve pair approve <临时名> --profile <测试轮廓>`——**profile 绑定发生在审批这一步**;客户端在 120 秒窗口内自动 finish + 首拉;
  3. 该实例对靶子目标首连 → 转发锚落地(靶子只需完成 SSH 握手,认证失败不影响锚落地);
  4. NUC10 `sshmgr cache-tokens revoke <临时名>` 吊销;
  5. NUC10 `sshmgr servers pin-hostkey --list` 找到该设备转发锚 → `--clear`(核对受影响条目清单输出);
  6. 合法重锚(带外 `--fingerprint`)。此后两分支:「临时实例 pull 收敛」**仅在步骤 4 尚未吊销时可达**(吊销后再 pull 会 401→按设计隔离销毁本地缓存)——按册子顺序跑则走「直接进入清理」。
- **判据**:⑤`--clear` 输出的受影响清单与实际一致;全程 audit 有痕(revoke / pin-forward / pin-clear)。〔2026-09-21 首轮注:pin-forward/pin-clear 均有行;`cache-tokens revoke` 现不写审计行——判据维持不放宽,缺口登记 backlog〕
- **清理**:删除临时实例(`cache instances rm`——**需交互终端确认,agent 代跑会被非 TTY 护栏拦下**,无无人值守等价面,登记 backlog;被拦时留 owner 待办)、靶子条目与锚(条目 `servers rm` 后其锚变 `[orphan]`,再 `--clear --hostport <靶>:<端口>` 清)、pair 生成的临时项目(`projects revoke` + `projects remove`,否则 `profiles remove` 拒删)、临时 profile、临时码已 revoke 即终态;清理输出留档。
