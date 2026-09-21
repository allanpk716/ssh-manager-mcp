# Plan 34/37 验收册:吊销销毁 + 到龄自废(v0.10.0 起)

> 来源:backlog #3/#3b 销项条内的「owner 真机手工复验待发版前做」欠账。
> 两条都是**安全失效路径**,生产裸奔最久的验收,优先跑。
> 环境:NUC10(serve)+ 笔记本(本机)。**全部走一次性靶子,不碰生产设备码与生产缓存。**

## 前置

- [ ] 双端 ≥ v0.10.1(P0 锚修复后)
- [ ] 笔记本生产实例无恙(全程用临时实例 + `SSHMGR_CACHE_DIR` 指向临时目录)

## C1 吊销 → 销毁 → 重新 enroll(Plan 34 切断失效)

- **步骤**:
  1. NUC10 `sshmgr cache-tokens add --name <临时名> --profile <测试轮廓>` 签发临时码;
  2. 笔记本 `sshmgr pair --instance <临时名>`(带 `SSHMGR_PAIR_ASSUME_SAS=1`,证据注明)入网 + 首 pull 成功;
  3. NUC10 `sshmgr cache-tokens revoke <临时名>`;
  4. 笔记本临时实例再 pull → 应被 pinned 401 拒 → **本地缓存四件销毁 / manifest 标记 / DEGRADED 报文链**按契约出现;
  5. `sshmgr cache status --instance <临时名>` 确认销毁形态;
  6. NUC10 重签新码 → 笔记本 `pair --force --instance <临时名>` 重新 enroll → pull 恢复。
- **取证**:每步命令原文+输出;④的完整降级报文。
- **判据**:④销毁链完整(不只是「拉不动」——盘上的快照必须失效);⑥恢复成功。
- **清理**:`cache instances rm`、靶子码 revoke 终态。

## C2 到龄自废 → 重拉恢复(Plan 37 B 时限快照)

- **步骤**:
  1. 临时实例入网后,以 `SSHMGR_CACHE_MAX_OFFLINE=1h` 拉一次(拉取路径带 env);
  2. `sshmgr cache status --instance <临时名>` 确认 cap 已记录(cache.config.json / 显示三源);
  3. 等待到龄(1h;或以更短 cap 如 2m 重做本步骤——**cap 语义与时长无关,短 cap 等价且省时**,登记用短 cap 的实际值);
  4. 到龄后以**带 env 的加载路径**起 `sshmgr mcp --cache --instance <临时名>` → 应拒载(到龄自废,fail-closed 文案);
  5. 再 pull 一次(服务器 Date 锚重写)→ 加载恢复。
- **判据**:④拒载且文案为到龄契约形态;⑤恢复;全程服务器 Date 锚逻辑未被无 env 拉取破坏(P0 锚语义)。
- **注意**:第 3 步「到龄」判定依赖服务器时间锚;用短 cap 时如实登记实际值,不虚构 1h 等待。
  - **2026-09-21 批2 实测订正**:过,四处据实修正——① 本节设想的「2m 短 cap 等价」**不可用**(上限校验硬地板 ≥1h,`--max-offline`/env 均拒 <1h);② env 形式的 cap 只作用当次拉取**不写文件**,持久化走 `--max-offline` 旗标;③ 无 env 起 `mcp --cache` 默认 `--cache-max-age 30m` 会**先自动拉新**,在线机器永续鲜、拒载不触发——验拒载须 `--cache-max-age 0`;④ 到龄行为为**销毁式拒载**(同吊销隔离链,快照隔离+码+解密钥删除),恢复 = 重新入网(第 5 步「同码再拉恢复」不成立——码随隔离已删);报文第二段 `(token revoked?)` 提示对到龄场景误导,已登记 backlog。详见[运行记录](./runs/2026-09-21-batch2-plan34-37.md)。
- **清理**:同 C1。

## C3 交叉回归(顺带)

- 临时实例销毁/自废过程中,**生产实例**(默认槽)pull 与 `mcp --cache` 冒烟不受影响——隔离性证据。
