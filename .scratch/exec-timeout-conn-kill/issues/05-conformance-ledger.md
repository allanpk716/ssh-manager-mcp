# 票 05 · conformance 真线用例 + 差异台账更新

## What to build

在门控 conformance 套件（SSHMGR_CONFORMANCE=1 + docker 真 OpenSSH）新增有界超时用例，对真实 sshd 钉住"超时必然有界返回 + 分类正确 + 服务器仍健康"契约；同步更新 ssh 差异台账两行。**夜链环境不要求 docker 真跑**（泳道纪律禁联网；本地若已有镜像可跑则跑，否则以"编译绿 + 无门控时自跳过 + 全量测试绿"验收，docker 真跑如实标注留给 CI/晨报）。

## 验收标准

- [ ] 新文件 internal/conformance/exec_timeout_test.go：TestExecTimeoutConnKillRealSSH——requireConformance 门控（照既有用例先例，如 cancel_test.go 的 TestCancellationAbortsRealExec 形态：generateKey + startOpenSSH + sshbroker.Connect(FixedHostKey)）；t.Setenv 宽限 300ms；Exec("sleep 60", 3s 超时)；断言：err==nil（超时是结果不是错误）、TimedOut=true、elapsed ≤ 6s（3s+0.3s+docker 余量）；随后新连接 round-trip（printf alive）证明容器仍健康
- [ ] 用例注释写明：断言钉契约（有界返回+分类）不钉走了哪一段——配合型服务器（阶段 1/2 解锁）与不配合服务器（阶段 3 断连解锁）都应绿；真机证据 2026-10-08（4090x2 OpenSSH 9.6p1）
- [ ] docs/ssh-conformance/differences-ledger.md：测试清单表登记新用例行；"逐命令超时杀除"行改写为三段式语义（SIGKILL+关通道 → 宽限 → 关整条连接；有界返回；残留远端进程在部分服务器上是服务器侧行为）；"exec_stop 杀除语义"行补"部分服务器完全无视通道级拆除，broker 仍及时终结任务与连接"
- [ ] go build ./... 与 go test ./internal/conformance/ -count=1 绿（无门控时用例自跳过）
- [ ] （可选，环境允许时）SSHMGR_CONFORMANCE=1 跑通本用例并记录输出；环境不允许则在本票回报中如实说明未跑

## Blocked by

票 01（用例测的是三段式行为；编译与自跳过不依赖，但语义完成以 01 为准）

## 涉及路径

- internal/conformance/exec_timeout_test.go（新）
- docs/ssh-conformance/differences-ledger.md

## 副作用声明

默认验证：go build ./... + go test ./internal/conformance/ -count=1（自跳过路径，秒级）；docker 真跑需 SSHMGR_CONFORMANCE=1 且本地已有 OpenSSH 容器镜像——夜链禁联网，缺镜像时跳过并如实标注

## decision_refs

D3（第三层 conformance）、D6（台账与登记）

## review_blocks

无
