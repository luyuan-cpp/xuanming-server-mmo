# 登录回归套件与超时检测

> 更新：2026-10-23。本文说明 robot 的 `login-test` 现有机制与验收边界，不代表本轮已运行套件。
> 服务端登录流程见[登录事件流](./player_login_flow.md)，异步会话隔离见[客户端请求顺序 §6.2](./client-request-ordering.md)。

## 1. 套件范围

入口为 [`robot/main.go`](../../robot/main.go) 的 `mode: "login-test"` 分支，场景表在
[`robot/login_test_scenarios.go`](../../robot/login_test_scenarios.go) 的 `runLoginTests`。
当前共 23 项，后续以源码表为准，不再维护另一份逐项清单。

场景覆盖正常登录、重复请求、顶号、同账号并发、断线重连、Access Token 重连、退出再进入、异常消息、
并发批量登录，以及技能、换场景、多机器人行为和货币存盘冒烟。
部分场景会创建角色、顶掉旧连接或修改玩家状态，运行时使用隔离测试账号与对应本地测试栈。

`EnterGameResponse` 无错表示请求已受理，不等于已经进入场景。`NormalLogin`、`BatchConcurrentLogin`
等通过 `loginAndEnter` 完成握手的结果，不能作为场景就绪证据；行为场景的 `prepareBehaviorClient`
另行等待 `NotifyEnterScene`，并在 `WaitSceneReady` 设置等待上限。
完整异步失败与进场语义见[登录事件流](./player_login_flow.md)和 [Gate ↔ Login 边界](./gate-login-rpc-boundary.md)。

## 2. 超时与失败分类

- [`robot/login.go`](../../robot/login.go) 的 `sendAndRecv` 缺省 15 秒，限制的是一次同步请求等待应答，不是整个登录与进场流程的总预算。
- `sendAndRecvTimeout` 超时会增加 `login_stuck` 计数，返回包裹 `errLoginStuck` 的错误。调用方必须关闭这条连接，不能继续握手或再启动 `RecvLoop`，因为原读者可能仍在阻塞。
- 等待过程中遇到非目标消息会暂存，供后续消息循环补投递；应答的信封错误先返回为 `envelopeError`，不把空 body 当成业务成功。
- `LoginStuckDetection` 还对从建连到登录握手返回的总耗时检查 10 秒阈值；超过阈值即使握手成功也判为过慢。它与单次等待的 15 秒上限是不同判据。
- `prepareBehaviorClient` 的场景就绪等待另有 15 秒上限。各阶段预算分别判断，不能据此承诺“所有异常之后整条登录都在 15 秒内成功”。

`login_ok`、`login_fail`、`login_stuck` 是诊断计数；最终仍要按每个场景的预期结果和断言判断，
不能用计数或握手成功替代实际场景、会话与存盘验收。
`BatchConcurrentLogin` 当前在没有 STUCK、成功数达到 `n*8/10` 时即可 PASS；套件全 PASS 不等于每次登录尝试都成功。

## 3. 输出与自动化边界

套件打印每个场景的 `PASS` / `FAIL` 和末尾 `Total / Passed / Failed` 汇总，并在工作目录导出：

| 文件 | 内容 |
|---|---|
| `login_test_results.csv` | 时间、账号、场景、延迟、成功、stuck、错误、attempt、concurrent |
| `behavior_test_results.csv` | 行为与场景结果记录 |
| `behavior_test_results.jsonl` | 同类行为记录的 JSONL 格式 |

记录与导出实现在 [`robot/metrics/stats.go`](../../robot/metrics/stats.go)。部分调用路径未填
`Attempt` / `Concurrent`，会写出默认 0；不能把它们一概解释为有效的重试序号或并发观测值。

**当前场景失败不会自动使进程非零退出。** `runLoginTests` 打印失败后返回，main 随后正常结束；
CSV 导出错误也只记日志。因此不能仅凭 `go run` / EXE 的退出码 0 宣称套件通过，必须核对逐场景结果、
汇总和导出日志。该限制尚未在本次文档整理中修复。

现有 CSV 是分析输入。机器学习训练、`stuck` 概率预测、0.1 告警阈值和按这些结果拒绝发布的 CI 门禁
均没有实现；旧稿中的这些步骤是设想。现有 `login-path-tests.yml` 运行 Java / Go / robot 单元测试，
不等于实栈 `login-test` 场景验收。

## 4. 运行入口

先准备本地服务与可用认证源，将 `robot/etc/robot.yaml` 复制到专用配置位置，核对 gate / Gateway、
认证方式与测试账号，再将下列片段合入该配置；仅有这个片段不足以启动完整测试。
仓库默认配置目前是 `mode: "stress"`，不会自动运行登录回归。

```yaml
mode: "login-test"
robot_count: 10
```

例如将专用配置放在系统临时目录后，从仓库的 `robot` 目录运行：

```powershell
go run . -c "$env:TEMP\robot-login-test.yaml"
```

保留当前运行的日志、汇总和三份结果文件，报告实际执行的场景、失败、未执行项及环境身份。
涉及批量登录压测时仍按 `AGENTS.md` §6 执行前置清理、基线与汇总；不能以本套件替代压测证据。
