# 存盘管线（go/db）上线待做

> **已过期(2026-10-30)**:玩家存盘改成 scene 直接提交 TiDB 的 `player_store`([player-save-direct-commit.md](../design/player-save-direct-commit.md)),go/db 的 Kafka 消费链已删除,本清单作废。现行的验证清单见 `docs/handoff/player-save-direct-commit-20261030/progress.md`。本文只作历史保留。

> **用途**：存盘管线已经落码、但还没有验证或还没有做完的上线前事项。负责人 2026-10-13 的要求是「先实现代码，先不管测试，测试记录到上线待做里面」，所以代码先落，验证全部记在这里。
> **执行者**：编译、测试、故障注入由 Codex 执行（AGENTS.md §10.1）；部署与集群操作由负责人执行（AGENTS.md §9）。
> **维护**：每做完一项，在该项后面写日期、执行者和结果（通过 / 失败 + 证据位置），不要删行。新增事项追加在对应小节末尾。
> **相关**：[交接](../handoff/godb-pipeline-handoff-20261013.md)、[Codex 任务单](../handoff/godb-pipeline-codex-tasksheet-20261013.md)、[L-04 设计](../design/db-task-store-unavailable-deferral.md)、[条件写设计](../design/db-write-fencing-mysql.md)、[瓶颈文档](../design/db-task-pipeline-bottlenecks.md)。

---

## 1. 第 0 批与 proto2mysql 条件写原语

| # | 事项 | 怎么做 | 状态 |
|---|---|---|---|
| 1.1 | 第 0 批（子 shard 打散、login 自锁、位点重置判定、汇总脚本 DB 段）的编译与单测 | [Codex 任务单](../handoff/godb-pipeline-codex-tasksheet-20261013.md) 任务 A | 待 Codex。负责人已自行安装 Go 1.27.2；2026-10-15 同意 Codex 跑测试时联网下载 Go 模块（任务单第 2.1 版 §0.1「同意 2」）|
| 1.2 | proto2mysql `SaveConditional` 的离线、真库（MySQL 8.4 / TiDB 8.5）、竞态测试 | 同上，任务 B | 待 Codex。负责人已同意拉取两个镜像 |
| 1.3 | 1.2 全绿后合入 proto2mysql main，不打 tag；go/db 用伪版本钉 main 上那个提交 | 负责人 2026-10-13 的决定 | 等 1.2 |

---

## 2. L-04 第 1 批（存储不可用时延后、不进死信；业务池读写超时）

代码已落：第 1 批随 2026-10-14 的提交 `ebccaf04d`（已推送）入库；之后的复核修订随 2026-10-15 的工作区快照 `54c9d775d`（已推送）入库。`2f897bd07..54c9d775d` 之间 go/db 的 15 个文件改动全部属于本批，文件清单见 [L-04 设计](../design/db-task-store-unavailable-deferral.md) §7；同一区间别的会话改了 go/shared（go/db 经 `replace shared => ../shared` 依赖它）。

**怎么跑**：不在主工作区跑（别的会话在写）。用两个临时 worktree：被测 `$env:TEMP\l04_head` 建在 `54c9d775d`，基线 `$env:TEMP\l04_base` 建在 `2f897bd07`（`ebccaf04d` 的父提交）；做法与收尾照 [Codex 任务单](../handoff/godb-pipeline-codex-tasksheet-20261013.md) §2.1 / §2.6，环境变量照任务单 §1（`GOTOOLCHAIN=local`、`GOFLAGS=-mod=readonly`、`GOPROXY=https://goproxy.cn,direct`，「同意 2」2026-10-15 已获）。下面命令的工作目录都是 worktree 里的 `go\db`。

### 2.1 编译与静态检查

工作目录 `go/db`：

```sh
gofmt -l internal/storeerr internal/kafka internal/logic/pkg/proto_sql internal/config internal/metrics
go vet ./...
go build ./...
```

通过标准：`gofmt -l` 无输出；vet 与 build 退出码 0。失败时保留第一条 error 及其前后 20 行。

状态：2026-10-14 提交 `ebccaf04d` 的说明记录「已检查范围的 Go 格式检查通过」（范围未写明；vet 与 build 未记录）。复核修订（`54c9d775d`，提交说明没有记录任何验证）需要在 `l04_head` 上重跑。

### 2.2 已随代码写的单测

工作目录 `go/db`：

```sh
go test ./internal/storeerr/... ./internal/logic/pkg/proto_sql/... ./internal/kafka/... ./internal/config/... ./internal/metrics/... -count=1 -v
```

- 新增：`internal/storeerr/storeerr_test.go`（4 个用例）、`internal/logic/pkg/proto_sql/store_health_test.go`（7 个用例）。
- 改写：`internal/kafka/placement_route_test.go` 的 `TestStoreUnavailable_WriteKeepsBudgetReadFailsFast`（读任务改为立即回失败回执）；`key_ordered_consumer_test.go` 的 `recordingHarness`（handler 签名改为返回 error）。
- 基线：在 `l04_base` 上跑同一条命令（基线没有 `internal/storeerr`，从命令里去掉它），对比失败集合。`internal/kafka` 的既有用例依赖 miniredis 监听 127.0.0.1，沙箱拦回环时先申请放行。
- 通过标准：新增用例全部 PASS；既有用例的失败集合是基线失败集合的子集。出现新增失败时，先看失败栈是否落在本批 15 个文件之外（例如 go/shared），并如实写明，不要直接算到本批头上。

状态：2026-10-14 提交 `ebccaf04d` 的说明记录「storeerr、proto_sql、config、metrics 定向单测通过」；`internal/kafka` 包未跑（本机缺 `miniredis/v2 v2.36.1` 的源码包，离线模式下载不了）。这是复核修订之前的代码；修订改了 `proto_sql`（探活、冷却计数，新增 2 条用例）、`metrics`、`internal/kafka/placement_route.go`，需要重跑。2026-10-15 负责人同意 Codex 联网下载 Go 模块，`internal/kafka` 已不再被模块挡住（Claude 只读核对：go/db 在 `2f897bd07` 与 `54c9d775d` 上缺的源码包只有 miniredis v2.36.1）。

### 2.3 还没写的单测（上线前补）

| 用例 | 断言 |
|---|---|
| `retryBudgetExempt` 新分支 | 四类存储侧标记下写豁免、读不豁免；原有四种豁免不变；未打标记的 `net.Error`（来自 Redis）不豁免 |
| `deferReason` 全表 | 每个原因各一例，含「Redis 侧超时归 redis_unavailable」 |
| 重试循环每拍上限 | ready 里 3 条必失败的任务一拍只认领 3 次；两个消费者共用一个 miniredis 时每个各自至多 3 次 |
| 主库冷却时整拍跳过 | 注入实现了 `storeHealth` 的替身，冷却时一拍认领 0 次，三个队列长度照常采样 |
| 读任务预算用完 | 不进死信，收据被摘掉，`db_task_read_abandoned_total{reason="budget_exhausted"}` 加一 |
| 写任务存储类延后 | 重试来源的写因 `conn_lost` 延后时重试次数不变，连续超过 3 轮仍在 ready；载荷逐字节不变 |
| 计数 | `db_task_deferred_total`、`db_task_dead_letter_total`（含毒消息经 `persistPoisonMessage` 计数）按原因递增 |
| `runDBOp` 上报 | handler 返回 `conn_lost` 时调用 `ReportConnLost`，成功时调用 `ReportHealthy`，复核或缓存回写失败时都不调用 |
| 业务池配置 | `newServiceMysqlConfig` 设了三个超时与两个会话变量；负数不设；`NewMysqlConfig`（迁移用）不带它们 |
| 探活不经过连接池 | `pingStore` 有连接器时新建连接 Ping 后关闭；池里只有死连接时不误判；池满时不误判 |
| 冷却指标收集器 | `NewStoreCooldownCollector` 按 source 输出三类 pool 的序列 |

状态：已写（2026-10-15，随 `de4960bee` 入库；之后 R1 / R2 对其中几个文件的小改随 `e76f6f747` 入库），**未编译、未运行，待 Codex**。命令见 §4.1。文件与用例（按上表顺序）：

| # | 文件 | 用例 |
|---|---|---|
| 1 | `go/db/internal/kafka/l04_deferral_test.go` | `TestRetryBudgetExempt_StoreTaggedErrors` |
| 2 | 同上 | `TestDeferReason_EveryReason` |
| 3 | 同上 | `TestConsumeRetryQueue_ClaimsAtMostReadyLengthPerTick`、`TestConsumeRetryQueue_TwoConsumersEachCapped` |
| 4 | 同上 | `TestConsumeRetryQueue_SkipsTickWhileHomeStoreCoolingDown` |
| 5 | 同上 | `TestConsumeOneRetryTask_ExhaustedReadIsDroppedNotDeadLettered` |
| 6 | 同上 | `TestStoreDeferral_RetryWriteKeepsBudgetAndPayload` |
| 7 | 同上 | `TestDeferAndDeadLetterCounters` |
| 8 | `go/db/internal/kafka/run_db_op_test.go` | `TestRunDBOp_ReportsOnlySQLOutcome`、`TestRunDBOp_CanceledSQLIsNotReported`（SQL 被本进程取消时什么都不报） |
| 9 | `go/db/internal/logic/pkg/proto_sql/service_pool_test.go` | `TestNewServiceMysqlConfig_SetsTimeoutsAndSessionWaits`、`TestNewServiceMysqlConfig_NegativeDisables`、`TestNewMysqlConfig_HasNoServiceTimeouts` |
| 10 | 同上 | `TestPingStore_UsesConnectorNotPool`、`TestPingStore_FullPoolDoesNotBlock`、`TestPingStore_DeadPoolConnsDoNotMatter`、`TestPingStore_ConnectErrorIsConnLost` |
| 11 | `go/db/internal/metrics/metrics_test.go` | `TestStoreCooldownCollector_EmitsAllPools` |

第 8 条的口径以 `key_ordered_consumer.go` 的 `runDBOp` 注释「只把 SQL 这一步的结果报给注册表（L-04 §3.4）」为准：复核、核锁、缓存回写失败都不调 `ReportConnLost`。改 `config.AppConfig` 的用例在 `t.Cleanup` 里还原、不并行。

### 2.4 故障注入

只在本地 compose 或 kind 做，一个 zone，用 robot 或 `go/db/cmd/data_stress` 产生写。保留：go/db 日志里含 cooldown / deferred 的行、故障前中后三个时刻的 `/metrics`、三个列表的 LLEN。

| # | 注入 | 通过标准 |
|---|---|---|
| 2.4.1 | `docker pause` MySQL 容器 30 秒再 `unpause` | 期间 `db_task_deferred_total{reason=~"conn_lost\|store_cooldown"}` 增长，`db_store_cooldown_stores{pool="home"}` 变成 1；死信长度不变；go/db 进程没有挂死（`Stop()` 能在宽限期内退出）；恢复后 ready 排空，抽样玩家在库里的行等于最后一次写入 |
| 2.4.2 | `SET GLOBAL max_connections` 设为当前连接数，再 KILL go/db 的空闲连接逼它重连撞 1040 | 期间 `reason="conn_refused"` 增长；**不进冷却**；池内已有连接的任务照常落库，ready 不按全量入流增长；死信长度不变 |
| 2.4.3 | 在 dev 库上用一个未提交事务读 `player_database` 持有元数据锁，另一会话发起 `ALTER TABLE` 排队，让后续写排在它后面 | 写在约 10 秒后以 `reason="store_busy"`（1205）延后，没有挂到 30 秒的读超时；释放后写入恢复，库内行等于最后一次写入；做完撤掉临时 DDL |
| 2.4.4 | 1226：`ALTER USER … WITH MAX_USER_CONNECTIONS` 压低应用账号的连接上限 | 错误号确实是 1226，归 `conn_refused` |

状态：未做。

### 2.5 上线前置

| # | 事项 | 状态 |
|---|---|---|
| 2.5.1 | 目标环境的 go/db 指标端口能抓到（db ConfigMap 的 `MetricsListenAddr: ":9160"` 已在 HEAD 的 `k8s_deploy.ps1` 里），有 ServiceMonitor / PodMonitor | 待确认 |
| 2.5.2 | `kubectl apply -n observability -f deploy/k8s/db-task-retry-alerts.yaml`（仓库不部署 Prometheus，手工 apply）| 待做 |
| 2.5.3 | 上线前记录各 zone 现有死信长度（`LLEN kafka:dead:queue:db_task_zone_<zone>`，换代后带 `_g<N>`），否则 `DbTaskDeadLetterNonEmpty` 部署后常亮；存量死信由负责人决定怎么处理 | 待做 |
| 2.5.4 | 与 G2b 那个会话对齐：`deploy/k8s/mysql-connection-alerts.yaml` 描述里「go/db 把 1040 当普通失败 → 进死信」在本批部署后过时 | 待对齐 |
| 2.5.5 | `DbTaskRetryQueueGrowing` 的阈值（现为 1000）按压测数据定 | 待 R20 |
| 2.5.6 | 本批提交后在 Java 仓库 `PARITY.md` 登记 | 2026-10-15 Claude 在 Java 仓库 `PARITY.md` 文件尾追加两行（未提交）：「存储不可用时存盘任务延后」为不适用（Java 不经 Kafka 落库，与「db 多实例」行同理）；「业务库连接超时与会话锁等待」为 Java 待做（待核对）——Java 的 game-scene / game-login 设了 `socketTimeout=10000` 但没设会话锁等待，MySQL 缺省值都比 10 s 长 |
| 2.5.7 | TiDB 阶段：确认 TiDB 接受会话变量 `lock_wait_timeout`，不接受时 `SessionLockWaitSeconds` 写 -1 | 待 TiDB 阶段 |
| 2.5.8 | `kubectl apply -n observability -f deploy/k8s/db-task-partition-alerts.yaml`（仓库不部署 Prometheus，手工 apply）：写规则 `DbTaskWritePartitionContractBroken` 在 go/db 观测上线后即可 apply；读规则 `DbTaskReadPartitionMismatch` 先注释掉，等 login 全部滚到按契约分区的版本后再放开、重 apply 同一文件（滚动期间旧版 login 按 FNV 分区，必然误报）；回滚 `kubectl delete -n observability -f` 同一文件。登记见 `deploy/k8s/README.md`「go/db 存盘管线告警」 | 待做 |

### 2.6 部署顺序与红线

- 逐 zone 滚动，一次一个 Pod，低峰，照瓶颈文档 §4.5 每换一个 Pod 核对 lag、重试队列、processing、死信。
- **G2b 的 `mysql.yaml`（apply 会重建唯一的 MySQL）**：在本批于该环境所有 zone 部署完之前，有真实存档的环境不要 apply。部署完之后 apply 仍是一次存储中断：期间的写只在 Redis 的 ready 里有一份，**同一窗口不得重启或清空 Redis**，先确认 Redis 内存余量。负责人 2026-10-13 对「何时 apply」的答复是先实现代码、测试记入本清单，具体窗口未定。
- 条件写落地之前：同一窗口不跑会阻塞 DML 的 DDL，不暂停 MySQL（L-04 设计 §8 的残余窗口）。

---

## 3. 后续批次（已授权或已分工，尚未落码）

| 项 | 状态 |
|---|---|
| MySQL 行内围栏（条件写）| 负责人在另一会话确认由「玩家消息投递与位置管理」会话落码；该会话先出设计 v0.3，等 L-04 落地与 1.3 之后再动 go/db |
| ① 读写分区对齐 | 负责人 2026-10-13 授权，并接受「db 积压超过 8 秒时进场失败」的行为变化；排在 L-04 之后 |
| ③ 消费组按 zone 拆 | 负责人 2026-10-13 授权，并定为**开服门槛**（开服要支持单 zone 回档或合服）；前置：Kafka 镜像钉版本 |
| ④ 重试只交给分区属主 | 负责人 2026-10-13 定为先做并修订 TiDB 决策 D5；未授权开工 |
| ④B2 换代 topic（在线加分区）| 负责人 2026-10-13 定为要做，排在条件写之后 |
| WAITAOF（go/db 的重试与死信入队）、scene 发 DBTask 失败后的脏快照、Lua `tonumber`（go/db 一处）、db_task 的 retention.bytes 与 PVC | 负责人 2026-10-13 定为归本交接；未授权开工 |
| L-04 第 2 批：死信查看与重新入队工具 | 未授权 |

> 2026-10-16 现状（上表是 2026-10-13 的记录，按本文件「不删行」的约定不改）：①、③、Lua `tonumber`、脏快照、retention.bytes 与 PVC 都已落码（2026-10-15 / 16，**未编译、未测试**），验证与部署项分别见下面 §5、§6、§4、§7、§8；④ 与 WAITAOF 的 go/db 部分也已落码（R2），其验证项随 ④ 的设计文档另批登记，本文件本批不写。条件写仍未开工（等 1.3）。

> 2026-10-17 补：④ 与 WAITAOF 的验证与部署项（含 R7 语义复核的修复）已登记在下面 §9。

---

## 4. G1 治标、停机顺序与测量打点（S4）

代码已落：go/db 部分随 `de4960bee` 入库（之后 R1 语义复核的补修与 R2 对同批文件的改动随 `e76f6f747` 入库）；脚本随 `a7c510d65` / `0f6690604` 入库；`deploy/k8s/manifests/go-svc/db.yaml` 的 `minReadySeconds: 20` 与 `deploy/k8s/db-task-retry-alerts.yaml` 的 `db-task.ordering-lease` 组随 `e76f6f747` 入库。全部**未编译、未测试，待 Codex 验证**。
规格：[S4 规格](../handoff/godb-pipeline-impl-20261015/S4-g1-and-small-items-spec.md)；设计与三句语义（不是栅栏 / 只有失租才取消写 SQL / 在途取消后锁留给 TTL）、A3 相对规格的三处偏离见[瓶颈文档](../design/db-task-pipeline-bottlenecks.md) §7「S4 落码」，停机顺序见 §4.4「G6 停机顺序」。§3 表里 Lua `tonumber` 一项即本节的 `luaMarkAppliedEpoch` 十进制串比较。

### 4.1 编译与单测

做法同 §2（临时 worktree：被测建在含 S4 与 R1 补修的提交上，基线建在 `de4960bee` 的父提交上；环境变量照任务单 §1）。工作目录 worktree 里的 `go/db`：

```sh
gofmt -l internal/kafka internal/metrics internal/logic/pkg/proto_sql internal/migrate .
go vet ./...
go build ./...
go test ./internal/kafka/... ./internal/metrics/... ./internal/logic/pkg/proto_sql/... ./internal/migrate/... -count=1 -v
# race 需要 CGO 与 gcc;Windows 上没有就到 Linux / WSL 跑,并写明平台
go test -race ./internal/kafka/ -run 'TestOrderingLease|TestConsumeClaim_Shutdown|TestStop_|TestRunDBOp|TestRetryConsumer_StopsClaiming|TestClaimAcker_Untrack|RouteReadyGate|TestWaitRejoinBackoff_|TestConsumeClaim_DurabilityFailureArmsRejoinBackoff' -count=1
# 就绪闸时序用例另跑 20 遍排除调度抖动(2026-10-20 补)
go test ./internal/kafka/ -run 'RouteReadyGate' -count=20
```

重点用例（都必须出现在 `-v` 输出里并 PASS）：
- 租约：`ordering_lease_test.go` 的 `TestOrderingLease_*`（续租成功 / 退避恢复 / 用尽失租并取消 SQL / 截止时间 / 易主立即失租 / 停 worker 不取消 SQL / 过截止不发 SQL / abandoned 不删锁 / 释放三种结果 / 释放取消 SQL ctx / 已取消 ctx 的核锁不算失租）、`TestLeaseMarginCoversLockWaits`、`TestOrderingLeaseTiming_Backoff`。
- runDBOp：`run_db_op_test.go` 的 `TestRunDBOp_*` 与 `TestHandleTask_LeaseLostAfterSQLDoesNotPublishCache`（在改动前的代码上应为红）。
- 停机：`consumer_shutdown_test.go` 的 `TestConsumeClaim_Shutdown*`、`TestConsumeClaim_RebalanceStillCancelsClaim`、`TestClaimAcker_Untrack*`、`TestStop_*`、`TestRetryConsumer_StopsClaimingAfterPullStop`。
- 停机 × 读方就绪闸（D1 第 3 批，2026-10-20 补）：`consumer_shutdown_test.go` 的闸全是 nil，覆盖不到 `Placement.RequireRouteReady=true`。开关打开时的停机与退避看 `route_ready_gate_shutdown_test.go` 全部 9 条：`TestStop_RouteReadyGate*` 3 条、`TestConsumeClaim_RouteReadyGate*` 5 条、`TestConsumeRetryQueue_RouteReadyGateClosedOwnerModeForwardsNothing`。另有 `route_ready_gate_test.go` 的 `TestConsumeClaim_RouteReadyGate*` 2 条、`TestConsumeRetryQueue_RouteReadyGateClosedClaimsNothing`，以及 `rejoin_backoff_test.go` 全部。`-v` 输出里都必须出现并 PASS；出现「Stop did not return within 2s」即判失败。
- 打点：`instrumentation_test.go`（`TestRedisSteps_*`、`TestOrderingGuardCounter_Outcomes`、`TestSubShardBatchSizeObserved`、`TestStages_QueueWaitTaskTotalKafkaE2E`）；`metrics_test.go` 的 `TestRedisStepNamesPinned`、`TestRedisStepNamesMatchStressSummarize`、`TestRedisPoolCollector_*`、`TestPipelineCountersPrecreated`。
- Lua：`applied_epoch_lua_test.go` 的 `TestMarkAppliedEpoch_*`（存量 2^53 写入 2^53+1 必须推进，旧脚本为红）。
- §2.3 的 11 条见 §2.3 表。

仓库根目录（PowerShell 7）：

```powershell
pwsh -NoProfile -File tools/scripts/tests/go_services_stop.tests.ps1
pwsh -NoProfile -File tools/scripts/tests/go_services_ports.tests.ps1
pwsh -NoProfile -File tools/scripts/test_stress_summarize.ps1
pwsh -NoProfile -NonInteractive -File tools/scripts/tests/k8s_deploy_contract.tests.ps1
```

通过标准：gofmt 无输出，vet 与 build 退出码 0；新用例全部 PASS、SKIP 为 0；既有用例的失败集合是基线失败集合的子集。`go_services_stop.tests.ps1` 退出码 0、没有失败行（B4 报告共 13 例）；`test_stress_summarize.ps1` 末行 PASS，并有 'DB write path'、'DB Redis steps'、'DB retry routing' 三段的 t20m_db 与 t18m_db 行；`k8s_deploy_contract.tests.ps1` 含 `minReadySeconds: 20` 的断言（「db 是多副本形态」用例）。失败时保留第一条错误前后 20 行与失败用例的 `-v` 全文；红了就停下归因，不连续重试。

### 4.2 故障注入

只在本地 compose 或 kind 做，由 Codex 执行。保留：go/db 日志、注入前中后三个时刻的 `/metrics`（`:9160`）、重试三个列表的 LLEN。

| # | 注入 | 通过标准 |
|---|---|---|
| 4.2.1 | 优雅停：`go/db/cmd/data_stress` 持续写入时执行 `pwsh -File tools/scripts/go_services.ps1 -Command stop -Services db` | 输出 `[stop]  db (PID n, exited in Xs)`，没有 `[kill]`；`run\logs\go_services\db.stdout.log` 依次出现 "Shutting down gracefully..."、"claim drained for shutdown"、"db consumer stopped: … took=…"（took < 15s）；本实例的 `kafka:retry:processing:{topic}:{实例名}` 为空；重启后 lag 正常排空、死信不增长。再验一次 `-ForceStop`：输出 `[kill]  db (PID n): -ForceStop; force-killed process tree`，go.exe 及其子进程都已退出。若输出 "console interrupt not deliverable"，记下辅助进程退出码（2 = AttachConsole 失败，3 = GenerateConsoleCtrlEvent 失败）回报，不要重试 |
| 4.2.2 | 续租失败：在 compose 的 Redis 上执行 `CLIENT PAUSE 70000 WRITE`（写流量在跑时） | `db_ordering_lease_lost_total{reason=~"renew_deadline\|renew_exhausted"}` 增长；在途写的排序锁键 `distributed:lock:kafka:ordering:{topic}:{key}:{msgType}` 留到 TTL（SQL 在途时计 `db_ordering_lock_release_total{outcome="skipped_sql_abandoned"}`）；恢复后抽样玩家的 MySQL 行等于最后一次写入 |
| 4.2.3 | 锁被接管：另一会话 `LOCK TABLES player_database WRITE` 让 SQL 挂起，同时手工把测试玩家的排序锁键改写成别的 token | `db_ordering_lease_lost_total{reason="owner_changed"}` +1、`db_ordering_lock_release_total{outcome="skipped_sql_abandoned"}` +1；`UNLOCK TABLES` 后该行最终等于最新版本；做完撤掉临时锁 |
| 4.2.4 | 回滚兼容：新旧二进制混跑 | 旧实例遇到新实例留到 TTL 的锁只报 `errOrderingLockBusy`（不耗重试预算），不进死信 |
| 4.2.5 | （补充，R1 修法的端到端核对，规格外）MySQL 卡住时停机：`docker pause` MySQL、让 worker 通道积压，再执行 4.2.1 的 stop | 进程在 `StopTimeoutSeconds`（db 35s）内退出或按超时强杀，但不挂到更久；② 超时时日志有 "consumer loop did not exit within … canceling workers before leaving the group"；`unpause` 并重启后 lag 排空、死信不增长，抽样玩家的 MySQL 行等于最后一次写入 |

状态：未做。

### 4.3 上线前置

| # | 事项 | 状态 |
|---|---|---|
| 4.3.1 | `kubectl apply -n observability -f deploy/k8s/db-task-retry-alerts.yaml`（与 2.5.2 是同一个文件，现含 `db-task.ordering-lease` 组的 `DbOrderingLockReleasedNotOwned`（critical）与 `DbOrderingLeaseLost`（warning，reason 取 `owner_changed` / `renew_exhausted` / `renew_deadline` / `verify_error`），以及 ③ 的 `db-task.consumer-group-handoff` 组）；负责人执行 | 待做 |
| 4.3.2 | `minReadySeconds: 20` 属于 Deployment spec，改它不触发 Pod 重建，在下一次 db 滚动时生效；部署后核对 `kubectl -n <zone ns> get deploy db -o jsonpath={.spec.minReadySeconds}` 为 20 | 待做 |
| 4.3.3 | 线上发布按瓶颈文档 §4.5：低峰，一次一个 zone、每个 zone 一次一个 Pod；每换一个 Pod 核对 lag、重试队列、processing、死信，以及 `db_ordering_lock_release_total{outcome="not_owner"}` 与 `db_ordering_lease_lost_total` 不增长；看 Stop 日志的 took；没有 "shutdown drain timed out"（有积压时允许出现）。键名与值格式都没变（applied_epoch 仍是规范十进制），新旧混跑安全，回滚直接换回旧镜像 | 待做 |
| 4.3.4 | 对比口径：写任务的 `op_handler` 多了一次核锁往返；核锁失败由原来记 ok 改记 `db_task_result_total{result="error"}`；R20 作为 DB 段的新基线（含第 0 批的子 shard 打散），与 R19 只比 task_total、排空时长、Redis commandstats | 待 R20 |
| 4.3.5 | 把下面三条发给条件写会话的接手方（交接文档 §4 的约定）：核锁已挪进 `runDBOp`，Redis 顺序变为 SQL → 复核 MGET → 核锁 → 缓存 SET → 游标 SET → epoch → LREM → ack → 释放；Stop 为六步；比较 uint64 时复用 `luaDecimalGreaterFn` | 待做 |

### 4.4 状态

- 代码已落，未编译、未测试；4.1–4.3 全部待做。
- 剩余风险（不在本批修）：② 超时若源于 sarama 自身慢（提交、心跳、LeaveGroup 的网络调用），只受它的 Net 超时与 45s 宽限兜底；`routeToSubShard` 在子 shard 数 ≤1 时阻塞发送没有 `w.ctx` 出口（生产 `SubShardCount: 8` 碰不到，建议单独立项）；本地截止时间依赖进程单调时钟，虚拟机暂停恢复后 SQL 可能抢在看门狗之前发出；margin 30s 要求两个会话锁等待之和 ≤ 25s，TiDB 阶段若按 2.5.7 把 `SessionLockWaitSeconds` 写成 -1，启动只打 ERROR；go-redis 默认 `MaxRetries=3` 时单条 SETNX 重发可能把自己刚设上的锁判成被占（既有问题）；Ctrl+C 辅助进程方案未在本机实测（失败退回强杀）；`Invoke-Stop` 强杀前不核对进程身份，PID 被复用时 `taskkill /T` 的波及面比旧的单 PID 强杀大。
- 2026-10-17 补（R7 语义复核）：上一条里「go-redis 默认 `MaxRetries=3` 时原样重发」的同类问题，在重试链路上的两处已修：重试认领脚本被重发时会多 POP 一条、把第一次认领到的收据留在本实例 processing 里无人派发；收据 ack 的 `LREM` 被重发时会把另一份字节相同的副本也删掉。现在每次认领 / ack 带随机 nonce，第一次执行写防重记录 `kafka:retry:guard:{claim|ack}:<本实例 processing 键>:<nonce>`（`go/db/internal/kafka/retry_ownership.go` 的 `retryGuardKey`、`retryGuardTTL` = 5 分钟），重发时不再 POP、不再多删；认领结果未知时，下一拍认领之前先用 `retryClaimResolveScript` 把可能已认领的那条原样搬回原 ready（`resolveUnresolvedClaims`），resolve 遇到网络错误时本拍暂停分拣与认领。**剩余风险是防重记录的 TTL**：Redis 卡顿或不可达超过 5 分钟、记录过期之后，旧连接里迟到执行的副本仍可能再 POP 一次（第一次认领到的那条照旧滞留到重启或停机 release）或再删一份。排序锁 `SETNX` 的重发问题不在此列，仍是既有问题。验证见 §9.2。已落码，未编译、未测试，待 Codex 验证。

---

## 5. ① 读写分区对齐（S3）

代码已落：Go 与 C++ 随 `a7c510d65` / `592d7e718` / `0f6690604` 等快照提交入库；告警文件 `deploy/k8s/db-task-partition-alerts.yaml` 随 `e76f6f747` 入库。**未编译、未测试，待 Codex 验证。**
契约、两侧实现、抽样命令、冷读冒烟与行为变化以[分区契约文档](../design/db-task-kafka-partition-contract.md)「分区函数契约」「golden 向量」「滚动对齐步骤与观测」「屏障覆盖范围与剩余缺口」为准。

### 5.1 编译与单测（Codex，Go 1.27.2，环境变量照任务单 §1）

| 工作目录 | 命令 | 通过标准 |
|---|---|---|
| `go/shared` | `go vet ./kafkautil/`；`go test ./kafkautil/ -count=1 -v -run "DBTask\|ParseConsoleConsumerLine\|IsDBTaskTopic"` | `TestDBTaskPartitionCheckValue`、`TestDBTaskPartitionGoldenVectors`、`TestNewDBTaskPartitionerMatchesDBTaskPartition`、`TestDBTaskKeyEncoding`、`TestDBTaskPartitionRejectsEmptyKeyAndBadCount`、`TestDBTaskPartitionDistribution`（打印分布表）、`TestIsDBTaskTopic`、`TestParseConsoleConsumerLine` PASS，`TestDBTaskPartitionAuditDump` SKIP。golden 不一致时保留 `-v` 全文回报，不改 JSON |
| `go/shared` | `go test ./kafkautil/... -count=1` | ok。`topic_init_test`、`group_lag_test` 第一次进 L1，且别的会话在改 `topic_init.go`（S5b）与新增 `topic_durability*`，红了单独归因 |
| `go/login` | `go build ./...`；`go vet ./internal/kafka/... ./internal/logic/pkg/dataloader/... ./internal/svc/...`；`go test ./internal/kafka/... ./internal/logic/pkg/dataloader/... -count=1 -v` | 全绿；`TestDBTaskReadAndWriteShareKafkaPartitionIntegration` SKIP；`TestSendTaskLeaderNotAvailableDoesNotDeadlock`、`TestDBTaskProducerConfig*`、`TestSend*RoutesByDBTaskContract`、`TestSendTaskRejectsMismatchedKey`、`TestCommandAwarePartitionerScope`、`TestDBTaskTopicNamingMatchesContractPrefix`、`TestSendToTopic*RefusesDBTaskTopics`、`TestPartitionDriftIrreversiblyFencesDBTaskSends`、`TestPartitionsMatchContractRequiresExactContiguousSet` PASS |
| 仓库根 | `git grep -n "pkg/consistent" -- "go/*.go"` | 没有输出 |
| `go/db` | `go build ./...`；`go vet ./internal/kafka/... ./internal/metrics/... ./cmd/data_stress/...`；`go test ./internal/kafka/... ./internal/metrics/... -count=1`；`go test ./internal/kafka/ -count=1 -v -run 'ProducerPartition'` | 全绿（build 覆盖 `cmd/data_stress`、`cmd/verifier`）；`TestObserveProducerPartitionVerdicts`（15 个子用例）、`TestObserveProducerPartitionPreRegistersAllSeries`、`TestObserveProducerPartitionNilInputs`、`TestAllowProducerPartitionLogBudget` PASS。`data_stress/main.go` 文件头注释改动前就不是 gofmt 格式，不计入 |
| 仓库根 | `pwsh -File go/test.ps1` | 退出码 0、末行 `L1_TESTS_OK`；日志里能看到 db、shared（`./placement/... ./kafkautil/...`）、login（`./internal/kafka/...`）三组都执行了 |
| `go/login`（可选，本机 docker kafka 已起） | `$env:KAFKA_INTEGRATION_BROKER='127.0.0.1:9092'; go test ./internal/kafka/ -run Integration -count=1 -v; Remove-Item Env:KAFKA_INTEGRATION_BROKER` | `TestDBTaskReadAndWriteShareKafkaPartitionIntegration` PASS（pW == pR == 契约分区，且 oR > oW）；用例自己删掉 `mmorpg_dbtask_partition_it_*` topic |

C++（仓库根，VS 开发者环境，MSBuild 严格串行 `/m:1`；不以 `game.sln` 全量为通过标准，主干整体编不过）：
1. `git -C <仓库根> submodule status third_party/librdkafka` 行首不是 `-`（需要 init 属于联网下载，先征得负责人同意）。
2. 与 §7.1 同一组工程（`infra.vcxproj` → `scene.vcxproj` → `kafka_command_test.vcxproj` → `cpp\nodes\scene\scene.vcxproj`），0 error，`kafka_producer.cpp`、`player_lifecycle.cpp`、`kafka_partition_contract_test.cpp` 无新增警告。
3. `pwsh -File tools/scripts/run_cpp_tests.ps1 -Filter kafka_command`：`DbTaskPartitionContract.VendoredDefaultPartitionerIsConsistentRandom`、`ExplicitPartitionerOnGlobalConfReachesDefaultTopicConf`、`LibrdkafkaConsistentRandomMatchesSharedVectors`、`KafkaKeyMatchesGoFormatUint` 四例 PASS，原有用例不新增失败；main 找不到 `base_deploy_config.yaml` 时只告警，属正常。
4. 可选：`.\build\cpp\tests\kafka_command_test.exe --gtest_filter=DbTaskPartitionContract.* --gtest_output=xml:<路径>`，XML 里 `librdkafka_runtime_version` 应为 2.14.0。版本不一致时回报 DLL 的版本串，由负责人决定统一 DLL 还是调整断言，不要删断言。

golden 表由 Go `crc32`、sarama、librdkafka 三方都通过才算成立；任何一方不一致都以机器结果回报。

### 5.2 本机一区联调（Codex）

前置按 AGENTS.md §6.2 第 6 条清空本机 redis、mysql、etcd，删掉 `tools/scripts/.run` 下的 pid 与 log。
1. 用 `tools/scripts/start_game.ps1` 起服，顺序 db → login → scene；robot 登录测试通过率与基线一致。
2. `curl.exe -s http://127.0.0.1:9160/metrics | Select-String db_task_producer_partition_total`：12 条序列都在；op="write" 与 op="read" 都只有 `verdict="ok"` 在增长；`key_mismatch`、`invalid` 为 0。
3. 冷读冒烟（契约文档「滚动对齐步骤与观测」，含「在 Kafka 侧确认 W 已到 broker」）：至少 5 次有结论且全部通过。
4. 对本机 kafka 容器演练一遍第 1 步的抽样与 `TestDBTaskPartitionAuditDump`，确认 runbook 可用。
- 失败时保留：`go test -v` 全文、编译错误前 20 行、`kafka_command_test` 输出、`bin/logs/cpp_nodes/`、go/db 与 login 日志、`:9160` 与 `:9101` 的 `/metrics` 快照。不得据此宣称性能结论。

### 5.3 上线

| # | 事项 | 状态 |
|---|---|---|
| 5.3.1 | 只读抽样门槛：生产与预发每个 zone 都做，在 login 切换之前；**负责人执行**（Codex 只在本机或指定的 dev 环境）。写的非 ok 为 0、解析失败为 0、抽样里有写；读不符比例记作基线 | 待做 |
| 5.3.2 | staging 按生产顺序走一遍并做冷读冒烟（只在 staging 删 Redis 键）；可选用 `KAFKA_INTEGRATION_BROKER` 指向 staging broker 跑一次集成用例 | 待做 |
| 5.3.3 | 生产 login 逐副本滚动。**前置**：回滚目标提交必须包含 `d5cbb8a8e`（`git merge-base --is-ancestor d5cbb8a8e <commit>`），否则会把 G0 的自锁带回来 | 待做 |
| 5.3.4 | go/db 观测随下一次例行 go/db 发版上（不单独滚 go/db），上线后至少看 2 个存盘周期 | 待做 |
| 5.3.5 | 告警 apply：见 2.5.8（写规则在 5.3.4 之后，读规则在 5.3.3 全部滚完之后） | 见 2.5.8 |
| 5.3.6 | C++ 改动随下次 scene / battle 例行发版；scene 启动日志没有 "failed to pin partitioner" | 待做 |
| 5.3.7 | 发布说明与 runbook 写明契约文档列出的五条行为变化（db 积压超过约 8 秒时冷读进场失败、客户端收到 tip 3023 等），排障看 `entergame_total{result="preload_failed"}` 与按分区的 lag | 待做 |
| 5.3.8 | 负责人确认偏离：`command_partitioner_test.go` 里名为 `"db_task"` 的 topic 仍断言走默认 FNV | 待负责人 |
| 5.3.9 | 负责人定：op="other" 是否设告警（现在没设） | 待负责人 |

---

## 6. ③ 消费组按 zone 拆（S2）

代码已落：go/db、`go/shared/kafkautil`、`tools/merge_zone`、`lib/kafka_consumer_groups.ps1`、`kafka_offset_reset.ps1`、`k8s_zone_rollback.ps1` 等随 `de4960bee` 入库；`k8s_deploy.ps1`（R4）、`dev_tools.ps1`、契约测试、`deploy/k8s/README.md`、`db.yaml` 注解占位与告警组随 `e76f6f747` 入库。Kafka 镜像已四处钉到 `apache/kafka:4.3.1@sha256:77e3df9054047a88b520d0cc46e16696d3b22022e1d580aeccd2632df6532837`。**未编译、未测试，待 Codex 验证。**
设计、四种 ConfigMap 形状、决策表与 runbook 以 `docs/design/db-consumer-group-per-zone.md`（本批另行撰写）为准；规格以 [S2 修订稿](../handoff/godb-pipeline-impl-20261015/S2-consumer-group-per-zone-addendum.md) 为准。

### 6.1 编译与单测（Codex）

| 工作目录 | 命令 | 通过标准 |
|---|---|---|
| `go/shared` | `go vet ./kafkautil/`；`go test ./kafkautil/ -count=1 -run 'CommittedOffsets\|ComputeGroupLag' -v` | `TestCommittedOffsets`、`TestComputeGroupLag` PASS |
| `go/data_service` | `go build ./...` | 退出码 0（它用 `kafkautil.GroupLagProber`） |
| `go/db` | `go vet ./internal/config/ ./internal/kafka/ ./internal/metrics/`；`go build ./...`；`go test ./internal/config/ -count=1 -v`；`go test ./internal/metrics/ -count=1 -v` | `TestKafkaTimeoutDefaults`、`TestValidateConsumerGroup`、`TestEffectiveGroupID`、`TestPreHandoffBinaryRefusesZoneShapedConfig`、`TestPreHandoffBinaryLoadsFinalZoneShape`、`TestPreHandoffBinaryIgnoresUnknownKeys`、`TestEtcYamlConsumerGroupIsFinalShape`、`TestConsumerGroupHandoffSeriesPrecreated` PASS；`PreHandoff*` 失败时保留 go-zero 的原始错误文本（F9 的核实证据） |
| `go/db` | `go test ./internal/kafka/ -count=1 -v -run 'GroupHandoff\|HeldPartitions\|PeerView\|RebalanceGap\|ConsumerClientID\|ConsumerSetup\|ConsumeClaim\|NewKeyOrderedKafkaConsumer\|Inspector\|Settled\|StateUnknown\|ReplayAfterSeed'`（有 cgo 时再加 `-race`），再 `go test ./internal/kafka/ -count=1` 与 `go test ./... -count=1` | 全部 PASS，单个用例不超过几秒（等待全部走假时钟） |
| `go/login` | `go build ./...`；`go test ./internal/config/ -count=1` | 带 `GroupID` 键的 login.yaml 仍能加载（login 的 `GroupID` 已改 optional、注明废弃） |
| `tools/merge_zone` | `gofmt -l .`；`go vet ./...`；`go vet -tags merge_integration ./...`；`go test ./... -count=1 -v -run 'Lag\|Authoritative\|KafkaGroup\|CommittedOffset\|ConsumerGroupMissing\|DbTaskQueueKeys\|DbRetryPartition\|DbTaskTopic'`；`go test ./... -count=1` | 全部 PASS（`TestResolveAuthoritativeLag_*`、`TestCommittedOffsetPresent_ColumnOrderFromHeader`、`TestConsumerGroupMissing_Whitelist`、`TestKafkaGroupCandidates` 等） |

PowerShell（仓库根，PowerShell 7）：逐个执行 `pwsh -NoProfile -NonInteractive -File tools/scripts/tests/<名>.tests.ps1`，名为 `k8s_deploy_contract`、`k8s_client_entry_contract`、`k8s_migrate_gate`、`k8s_gate_drain`、`dev_tools_merge_zone_contract`、`dev_tools_kafka_offset_reset`、`k8s_zone_rollback_gate_router_mode`、`k8s_zone_rollback_flush_guard`、`kafka_offset_reset_output_guard`，再跑 `pwsh -NoProfile -File tools/scripts/test_stress_summarize.ps1`；每个都以 0 退出、没有 FAIL 行。`k8s_client_entry_contract` 的 Forwarded 清单已含 `DbConsumerGroup`。
手工 dry-run 冒烟（不连集群）：`pwsh -NoProfile -File tools/scripts/k8s_zone_rollback.ps1 -ZoneName t1 -ZoneId 101 -TargetTime 2026-05-15T14:23:00Z -NodeImage ghcr.io/luyuancpp/mmorpg-node:0123456789ab -SkipMySqlPause`：退出码 0；摘要含 `group=db_rpc_consumer_group_z101 retire=db_rpc_consumer_group partitions=10`；Step 6 行含 `-DbConsumerGroup zone`；没有 `Step 2: drain`。
（2026-10-18 更正，回档语义裁决：`k8s_zone_rollback_flush_guard` 已删除，由 `k8s_zone_rollback_discard` 取代；回档相关的测试清单与冒烟期望以 6.5 为准。）

### 6.2 Kafka 钉版与 CLI 行为核对（V0 / V3）

| # | 内容 | 通过标准 / 去向 |
|---|---|---|
| V0 | 上线前（负责人或 Codex，只读）：`kubectl get pod -n mmorpg-infra -l app=kafka -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.containerStatuses[0].imageID}{"\n"}{end}'`（`-n` 换成实际的 `-InfraNamespace`）；本机 `docker image inspect apache/kafka:latest --format '{{json .RepoDigests}}'` | 每个 broker 的 imageID、本机 RepoDigests 都含 `sha256:77e3df9054047a88b520d0cc46e16696d3b22022e1d580aeccd2632df6532837`；任一不一致按一次 broker 版本升级评估（metadata.version、日志目录兼容、能否回退），由负责人决定是否另开窗口 |
| V3 只读 | `docker buildx imagetools inspect apache/kafka:4.3.1` 与 `… apache/kafka:latest` | 两条的 Digest 行都等于上面的 digest |
| V3 拉镜像（**下载，执行前请负责人确认**） | `docker pull` 钉版引用；`docker image inspect <钉版引用> --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}'`；`docker compose -f deploy/docker-compose.yml up -d kafka kafka-topic-init`；`docker exec kafka /opt/kafka/bin/kafka-topics.sh --version` | 入口为 `["/__cacert_entrypoint.sh"]` 与 `["/etc/kafka/docker/run"]`；kafka-topic-init 以 0 退出；版本 4.3.1。本机命名卷可能是更旧版本写的，按 AGENTS.md §6.2 先清再起 |
| V3 (a)–(j) | 在 4.3.1 上逐条记录原始输出与退出码：(a) Stable 组 `--reset-offsets --execute`；(b) describe 不存在的组；(c) 对不存在的组 `--reset-offsets --to-datetime --execute` 能否建出位点；(d) 组 Stable 但没有成员订阅该 topic 时 `--delete-offsets`；(e) 有成员订阅时的输出（预期含 `GROUP_SUBSCRIBED_TO_TOPIC`）；(f) `--execute` 是否打印 NEW-OFFSET 表；(g) `kafka-get-offsets.sh --time -2 / -1` 的格式；(h) Empty 组无位点时 describe 的输出；(i) 不存在组 describe 的原文；(j) kind 里 `kubectl exec -n mmorpg-infra kafka-0 -c kafka -- /bin/sh -c "KAFKA_HEAP_OPTS='-Xms16m -Xmx64m' /opt/kafka/bin/kafka-consumer-groups.sh --bootstrap-server localhost:9092 --describe --group <g>"` 能否成功（`k8s_deploy.ps1` 的 `Read-DbGroupTopicFacts` 就用这个形状：经 `/bin/sh -c` 覆盖小堆，防 CLI 继承 broker 大堆把 kafka-0 撑到 OOMKilled） | 回填 `docs/design/db-consumer-group-per-zone.md` §11；与代码假设不符时两边同步补白名单正则（`tools/scripts/lib/kafka_consumer_groups.ps1` 的 `Get-KafkaGroupMissingPatterns` 与 `tools/merge_zone/preflight.go` 的 `consumerGroupMissingPatterns`），原文追加为 `tools/merge_zone/merge_unit_test.go` 与 `tools/scripts/tests/kafka_offset_reset_output_guard.tests.ps1` 的 fixture（lib 是纯函数库，只放正则，不放 fixture）。**若 (c) 证实建不出位点，`k8s_zone_rollback.ps1` 的改动不得发布，交负责人决策**。（2026-10-18 更正：回档 Step 5 与第 0 步 d 改为 `-ToLatest`，(c) 改核对象为对不存在的组 `--reset-offsets --to-latest --execute`） |

### 6.3 本地交接集成（V5，功能验证，不出性能结论）与 kind 演练（V6，可选）

按 S2 修订稿 §13 V5：compose 起 kafka / redis / mysql / etcd，从 `go/db/etc/db.yaml` 生成 legacy 形状 L1 / L2 与交接形状 Z1 / Z2 四份配置，`data_stress` 持续写入期间正向切组、收尾（`--delete-offsets` 后 describe 复核）、再起最终形状 Z3 续消费、反向切组与收尾、切组后起 legacy 实例触发守望。通过标准：verifier 退出码 0；Z1 / Z2 的 seeded 与 no_offsets 覆盖的不同分区合计 10，每个 seeded 值等于 L 停掉后该分区的 CURRENT-OFFSET；`db_consumer_group_handoff_conflict_total` 在切组期间为 0、起 legacy 实例后增加；zone 组 LAG 为 0；死信不增长；反向时最长消费空窗 < 15s；旧二进制加交接形状必须拒启，旧二进制加 final(Z) 形状能起并从 zone 组位点续消费。V6（只在 dev 集群）：kind 上两个 zone，robot 负载期间对 z1 切组与删 Pod，z2 的 db 不应出现 `db_consumer_group_rebalance_gap_seconds` 样本。

### 6.4 上线（负责人执行，不停服、逐 zone）

| 步 | 事项 | 状态 |
|---|---|---|
| 0 | 先做 V0，再合入钉版；下一次 infra-up 会因镜像字符串变化滚动重启 kafka StatefulSet：单 broker 档等同一次 broker 重启，在无玩家窗口执行，之后跑 `k8s_deploy.ps1 -Command infra-kafka-topics -WaitReady`；多 broker 档按 PDB 逐个滚 | 待做 |
| 1 | Phase A：发布带交接闸的 go/db 镜像，生成器不变（线上仍是 `GroupID` 形状）；记录 `db_consumer_group_rebalance_gap_seconds` 基线，describe 的 CLIENT-ID 出现 `db-z<zone>-*`。除新增指标外没有任何差异 | 待做 |
| 2 | 合入脚本（`k8s_deploy.ps1` 等）。线上 `GroupID` 形状的 zone 在 keep 下保持不变，但第一次部署会因新增的 Pod 注解（`mmorpg.io/db-consumer-group` / `mmorpg.io/db-group-handoff-from`）滚一次 db。全新 namespace 与回档只渲染最终形状；全新集群首次 all-up（不带 `-SkipInfra`、非 DryRun）先只读探测 infra namespace，不存在即视为空 Kafka、两组都无位点，不 exec | 待做 |
| 3 | 金丝雀：选人数最少的 zone，冻结其它 zone 的发布，执行 `pwsh -File tools/scripts/k8s_deploy.ps1 -Command zone-up -ZoneName <z> -ZoneId <id> -GoSvcRegistry <registry> -GoSvcTag <与线上相同> -DbConsumerGroup zone`（其余参数与该 zone 的例行发布相同）。**必须带 `-GoSvcRegistry` 且不带 `-SkipGoSvc`**：本次不部署 db 时显式 `-DbConsumerGroup zone\|legacy` 直接拒绝（不拒绝的话 C++ 照常重部署、退出码 0，运维会以为切过了）。脚本先做 F2 预检，输出 `DB consumer group: zone=… mode=zone shape=handoff group=db_rpc_consumer_group_z<id> handoff_from=db_rpc_consumer_group source=explicit-handoff …`。观察 handoff waiting → released → seeded / no_offsets 覆盖 10 个分区、conflict 为 0、gap < 15s。然后收尾：`kafka_offset_reset.ps1 -BootstrapServer <b> -Topic db_task_zone_<id> -Group db_rpc_consumer_group_z<id> -RetireGroup db_rpc_consumer_group -Partitions 10 -FinalizeHandoff`，先 dry-run 再加 `-Apply`；紧接着以 keep 再跑一次同样的 zone-up，确认输出 `source=handoff-finalized`、ConfigMap 变为 `GroupID: "db_rpc_consumer_group_z<id>"`、db 滚动完成 | 待做 |
| 4 | 金丝雀浸泡 24h；生产只观察指标、经历一次例行发布，delete pod 演练放到 staging 或 kind | 待做 |
| 5 | 其余 zone 每天切 1–2 个，每个 zone 重复第 3 步（含收尾与 keep 再跑一次）。全部切完后 legacy 组变为 Empty | 待做 |
| 6 | （以后另立项）按 AGENTS.md §6 出对比表之后再调 `HeartbeatIntervalMs` / `RebalanceTimeoutMs`，没有对比表不调 | 待定 |
| 7 | （下一轮）全部 login Pod 跑上 optional 版本 ≥ 7 天后，删除 `go/login/etc/login.yaml`、`k8s_deploy.ps1` 的 login 模板、`deploy/login-stack.linux/login.yaml` 三处 login 的 `GroupID`；`tools/merge_zone` 的 `-kafka-legacy-group` 缺省改为空 | 待定 |

- 回滚（单 zone 反向）：先确认正向已收尾（脚本也会拦：两组都有位点时显式模式 throw 并打印收尾命令）→ `zone-up -DbConsumerGroup legacy` → `FinalizeHandoff`（`-Group db_rpc_consumer_group -RetireGroup db_rpc_consumer_group_z<id>`）→ keep 再跑一次 zone-up 落到 final(L)。最终形状下可以直接用旧镜像部署；交接形状下必须先收尾。
- 其它入口：`dev_tools.ps1` 的 `k8s-image-*` / `k8s-release-*` 拒绝 `-DbConsumerGroup`（`k8s_image.ps1` 不部署 db）；`k8s-zone-rollback` 也拒绝它（回档 Step 6 固定传 zone）。`ConvertFrom-LiveDbConsumerGroup`（2026-10-18 起在 `lib/kafka_consumer_groups.ps1`，k8s_deploy 与回档第 0 步 f 共用）只认 `ServerConfig.Kafka.{GroupID,ConsumerGroup,GroupHandoffFrom}` 路径上的键，别的段的同名键忽略；`Apply-OneGoSvc` 遇到 db manifest 缺注解占位直接 throw。
- 告警：`db-task.consumer-group-handoff` 组 5 条随 4.3.1 一起 apply——`DbConsumerGroupHandoffWaiting`（> 45s，warning）、`DbConsumerGroupHandoffStuck`（`min by (namespace)` > 300，critical：只有本 zone 没有任何 Pod 越过闸才呼人）、`DbConsumerGroupHandoffOwnBehindPeer`（critical，处置分本组陈旧 / 对方陈旧两种）、`DbConsumerGroupHandoffConflict`（critical）、`DbConsumerGroupHandoffSeedAborted`（warning，按分区计数）。
- 状态：代码已落，未编译、未测试；6.1–6.4 全部待做。

### 6.5 回档：丢弃 TargetTime 之后的写（负责人 2026-10-17 裁决，2026-10-18 落码）

裁决：回档 = 回到 TargetTime 时刻 MySQL 里已经落库的状态；Step 5 改 `-ToLatest`，Step 4 / 5 之间盘点并拦下本 topic 的重试收据，不清游标。代价（已接受）：T 之后的合法存盘一并丢弃。
流程、各状态族的处理、残余风险与本轮回档复核 [0]–[11] 的处置见 `docs/design/zone_data_rollback.md`「2026-10-18 修订」。改动：`tools/scripts/k8s_zone_rollback.ps1`（含文件头的回档工具契约）、`lib/kafka_consumer_groups.ps1`
（新收 `ConvertFrom-LiveDbConsumerGroup`）、`k8s_deploy.ps1`（只移走该函数）、`kafka_offset_reset.ps1`（注释）、`dev_tools.ps1`（新增 `-RollbackAllowEmptyTarget` / `-RollbackAcknowledgeNoPostTWrites` 透传）、
`tests/k8s_zone_rollback_discard.tests.ps1`、`tests/k8s_zone_rollback_gate_router_mode.tests.ps1`、`tests/lib/k8s_zone_rollback_sandbox.ps1`、`tests/k8s_deploy_contract.tests.ps1`（注释）。**未编译、未测试，待 Codex 验证。**

| # | 事项 | 状态 |
|---|---|---|
| 6.5.1 | Codex（仓库根，PowerShell 7）：逐个 `pwsh -NoProfile -NonInteractive -File tools/scripts/tests/<名>.tests.ps1`，名为 `k8s_zone_rollback_discard`、`k8s_zone_rollback_gate_router_mode`（PATH 上要有 git）、`kafka_offset_reset_output_guard`、`dev_tools_kafka_offset_reset`、`k8s_deploy_contract`、`k8s_client_entry_contract`、`k8s_migrate_gate`、`dev_tools_merge_zone_contract`；通过标准：每个以 0 退出、`fail=0`，失败时保留全部 `[FAIL]` 行。dry-run 冒烟同 6.1 那条命令，期望再加：Step 5 行含 `-ToLatest True`、不含 `ToDatetime`；有 `Redis identity (read-only): kubectl exec -n mmorpg-infra pod/redis-0` 与 `Discard tool build:` 两行；五个模式的工具命令都带 `-expect-run-id`；没有 `FLUSHDB` | 待做 |
| 6.5.2 | 落地回档工具 `go/db/cmd/zone_rollback_discard`，以脚本文件头「zone_rollback_discard 契约」为准（`-version` 与 ldflags 嵌入提交、`-expect-run-id`、判定参数、续跑规则与带轮次的隔离键、SCAN 边界、分族计数、verify 复核已处理玩家）；工具自己的测试覆盖契约末尾点名的用例。落地之前 `-Apply` 停在入口校验 | 待排期（负责人定批次） |
| 6.5.3 | V3(c) 改为实测 `--reset-offsets --to-latest --execute` 对不存在的组能否建出位点（6.2） | 待做 |
| 6.5.4 | 负责人另行拍板：#1 zone 维护围栏（落地之前工具 preflight 一律以 3 拒绝）、#3 严格屏障协议下的处置、#4 blocked 玩家与 `-AcceptBlockedPlayers`、#6 T 之后合入本 zone 的玩家 | 待负责人 |
| 6.5.5 | 运维禁令（写进 runbook 之前先记在这里）：回档之后对该 zone 的 db_task topic 做 `-ToDatetime` / `-ToEarliest` 不得越过 `run/logs/zone-rollback/<rid>/kafka-cut.json` 的切点；隔离区 `rollback:quarantine:<zone>:<rid>:*` 永不回注线上队列 | 生效中（只靠文档） |
| 6.5.6 | Java 版：不适用（Java 版没有 Kafka 存盘链，同设计文档 §12） | — |

---

## 7. scene 发 DBTask 失败后的脏快照（S5a，C++）

代码已落：随 `0f6690604` / `de4960bee` 等快照提交入库；`kafka_producer.cpp`、`redis.cpp`、`db_task_dispatch_test.cpp`、`cross_zone_test.vcxproj` 之后的改动（含别的会话的）随 `e76f6f747` 入库。**未编译、未测试，待 Codex 验证。**
要点：`cpp/libs/services/scene/player/system/db_task_dispatch_{ledger,system}.{h,cpp}` 按玩家记 DBTask 投递账本；某次存盘的 DBTask 同步被拒或投递回执失败时，该玩家强制重存（快路径 `db_task_dispatch::AllowsDirtySaveSkip` 不再放行），一律按当前内存重新 marshal、绝不重放旧字节；下线收尾经 `PlayerLifecycleSystem::FinishExitOrAwaitDbTaskAck` 等本玩家所有在途 DBTask 的回执（`player_exit::DecideDbTaskGate`，重存上限 `kMaxExitDbTaskResaveRounds` = 3）；生产者经 `KafkaProducer::sendTracked` / `DeliveryTracker` 逐条回执，50ms 的 Pump 定时器在 loop 顶层处理回执，票据超时兜底 `kTicketTimeout` = 330s。设计文档 `docs/design/db-task-delivery-and-retention.md` §A 本批另行撰写。

### 7.1 编译与单测（Codex；仓库根，PowerShell 7 + VS 开发者环境，MSBuild 严格串行 `/m:1`，前一步红了就停下归因）

```powershell
msbuild cpp\libs\engine\infra\infra.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
msbuild cpp\libs\services\scene\scene.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
msbuild cpp\tests\kafka_command_test\kafka_command_test.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
msbuild cpp\tests\cross_zone_test\cross_zone_test.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
msbuild cpp\nodes\scene\scene.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
pwsh tools/scripts/run_cpp_tests.ps1 -Filter kafka_command
pwsh tools/scripts/run_cpp_tests.ps1 -Filter cross_zone
.\build\cpp\tests\kafka_command_test.exe --gtest_filter=DeliveryTracker.*
.\build\cpp\tests\cross_zone_test.exe --gtest_filter=DbTaskDispatch*:ExitDbTaskGate*:ExitPersist*
# 回归(同样链接 scene.lib,走退出 / 存盘路径)
msbuild cpp\tests\bag_test\bag_test.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
msbuild cpp\tests\currency_test\currency_test.vcxproj /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64
pwsh tools/scripts/run_cpp_tests.ps1 -Filter bag
pwsh tools/scripts/run_cpp_tests.ps1 -Filter currency
```

通过标准：各工程 0 error，scene 节点链接无 unresolved external；`DeliveryTracker.*`、`DbTaskDispatchLedger.*`、`DbTaskDispatchStats.*`、`ExitDbTaskGate.*`、`DbTaskDispatchEcs.*` 全绿；既有 `ExitPersist*` 与 bag / currency 用例没有新增失败。不以 `game.sln` 为通过标准（主干整体编不过）。失败时保留 msbuild 第一条 error 前后 20 行、gtest 失败用例全文；报错落在别的会话的未提交文件里时单独记录。

### 7.2 故障注入（只在本地 compose 或 kind，由 Codex 执行）

| # | 注入 | 通过标准 |
|---|---|---|
| 7.2.1 | 一个玩家下线时 `docker pause` kafka 6 分钟（超过 `message.timeout.ms` 缺省 300s 与票据超时 330s），再 `unpause` | 期间该玩家实体被保留（不销毁）；30s 统计行 `[DbTaskDispatch]` 的 `exit_await_ack` +1；约 300s 后首批回执失败，`delivery_failed` 增长、`exit_resave` +1（第 1 轮重存，新 DBTask 全部入队后闸门回到等回执）；`unpause` 后第 1 轮重存的回执成功、实体销毁；`[DbTaskDispatch]` 的 `exit_resave_capped`、`exit_unsynced`、`orphan_unsynced` 为 0；抽样玩家的 MySQL 行与 Redis 一致。依据：`kafka_producer.cpp` 的 `createProducer` 不覆盖 `message.timeout.ms`（librdkafka 缺省 300000），每轮失败回执约 300s 才到，6 分钟里最多走 1 轮重存，`exit_resave_capped` 不可能增长 |
| 7.2.2 | Kafka 不可用期间观察在线玩家的周期存盘 | 周期存盘不再被脏比较快路径跳过，每周期整份写一次 Redis（日志 "forcing a real save … (metric=db_task_forced_resave)"）；这是负责人拍板接受的行为变化，只记录 Redis 写量 |
| 7.2.3 | Kafka 不可用时停 scene 节点 | 停机打一行 ERROR "[DbTaskDispatch] shutting down with N player(s) …" 列出未落定的玩家号（最多 50 个）；这些玩家的 MySQL 落后于 Redis，下次登录由 Redis 自愈 |
| 7.2.4 | （可选，2026-10-16 补，专门验证重存上限）同 7.2.1，但 `docker pause` kafka 不少于约 21 分钟再 `unpause` | 约每 300s 一次回执失败，`exit_resave` 累计 +3（`player_exit::kMaxExitDbTaskResaveRounds` = 3）；约 1200s（4 × `message.timeout.ms`）第 4 次回执失败时闸门判 `kRetainExhausted`，`[DbTaskDispatch]` 的 `exit_resave_capped` +1（每次判定计一次，暂停再长还会再计），日志有 "metric=exit_db_task_resave_capped"；期间实体保留、`IsSaveInFlight` 为真；`unpause` 后实体经 `FinishExitOrAwaitDbTaskAck` 注释所列的出口收尾——周期存盘欠账强制真写（计 `forced_resave`，周期见 `SCENE_PLAYER_SAVE_INTERVAL_SECONDS`，缺省 300s）或再来一次退出请求（计 `exit_rekick`），不应拖到停机看门狗；实体销毁后抽样玩家的 MySQL 行与 Redis 一致。依据：`player_exit::DecideDbTaskGate` 只在轮次达到上限后返回 `kRetainExhausted`，而每轮重存的 DBTask 全部入队即解除 `NeedsResave`，所以第 4 次失败才触顶 |

状态：未做。

### 7.3 上线

| # | 事项 | 状态 |
|---|---|---|
| 7.3.1 | scene 节点滚动升级即可：不改 proto、配置、库表，不改与 go/db 之间的消息格式，新旧版本可以并存 | 待做 |
| 7.3.2 | 观察 30s 一行的 `[DbTaskDispatch]`：`exit_await_ack` 随下线增长属正常；稳态下 `produce_rejected`、`delivery_failed`（扣除 `delivery_failed_superseded`）、`ticket_expired`、`exit_resave_capped`、`exit_unsynced`、`orphan_unsynced` 都应为 0；停机 drain 的耗时基本不变 | 待做 |
| 7.3.3 | 发布说明写明行为变化（负责人已拍板）：下线销毁实体要等本玩家 DBTask 的回执，正常多 ≤ 50ms 的 Pump 间隔；Kafka 长时间不可用时下线实体被保留，直到 Kafka 恢复、再次收到退出请求触发 rekick，或停机看门狗到期；Kafka 长时间不可用时周期存盘不再被快路径跳过，每周期整份写一次 Redis（Redis 写量回到没有脏比较时的水平）；退出实体重存 3 轮后保留 | 待做 |
| 7.3.4 | 回滚：换回旧的 scene 镜像；账本只在内存里，没有持久化状态要处理 | — |

### 7.4 状态

- 代码已落，未编译、未测试；7.1–7.3 全部待做。§3 表里「scene 发 DBTask 失败后的脏快照」即本节。
- 剩余风险：`kTicketTimeout`（330s）依赖 librdkafka 缺省 `message.timeout.ms`（300s），以后改其一不同步另一个会把迟到的成功回执误判为失败（只多一次重存）；停机看门狗到期时这些玩家的 MySQL 落后于 Redis，合服、回档这类以 MySQL 为准的操作会读到旧档；被废黜、交接放行路径产生的孤儿要等新属主的首存来补；scene 先于 db / login 发 DBTask 时 broker 的 auto.create 会把 db_task 建成 1 分区（既有问题，未修）。

---

## 8. db_task 的 retention.bytes 与 Kafka PVC（S5b）

代码已落：Go（R3：`kafkautil.TopicSpec.RetentionBytes`、db / login 的 `KafkaConfig.RetentionBytes`）与部署（R5a：`k8s_deploy.ps1` 盘预算、`kafka.yaml` 的 `__KAFKA_STORAGE_SIZE__` 占位）随 `e76f6f747` 入库。**未编译、未测试，待 Codex 验证。**
公式、缺省值表、账本行、扩盘 runbook 与「已存在的 topic 立即改配置」见 `deploy/k8s/README.md`「Kafka 磁盘预算与 db_task retention.bytes」；设计文档 `docs/design/db-task-delivery-and-retention.md` §B 本批另行撰写。

要点（以代码为准）：
- `TopicSpec.RetentionBytes`：0 = 不管理（沿用 broker 缺省），> 0 = 建 topic 时带上并在每次启动时增量 SET（与 `retention.ms` 合成一次 `IncrementalAlterConfig`），< 0 拒绝。
- 档位缺省（`Apply-KafkaProfileDefaults`）：dev 每 zone 256 KiB/s、缓冲目标 1h、其它 topic 预留 1 GiB；prod-like 1 MiB/s、2h、2 GiB；prod 3 MiB/s、2h、4 GiB。新参数 `-KafkaDbTaskRetentionBytes` / `-KafkaDbTaskDesignBytesPerSec` / `-KafkaDbTaskBufferTargetMs` / `-KafkaStorageGi` / `-KafkaOtherTopicsReserveGi`，0 = 按档位或自动。
- 生效值 = target ≤ 按盘可容纳值时取 target，否则取 floor(可容纳值, 64MiB)（`FitLimited`，只告警）；每个 broker 的份额按「持有每个分区各一份副本」取上界，再乘 1.3 余量（`Get-KafkaDiskPlan`）。
- 线上盘 = min(StatefulSet 模板, 各 PVC `status.capacity` 的最小值)（`Get-LiveKafkaStorageGi` / `Get-LiveKafkaPvcMinGi`）；`Apply-Infra` 按 `StorageTemplateGi` 渲染 PVC 模板；显式 `-KafkaStorageGi` 与线上不符时 `Assert-KafkaStorageChangeIsSafe` 按 `Get-KafkaStorageChangeVerdict` 的 expand-refused / shrink-refused / pvc-not-expanded 拒绝，**zone-up 也做这项检查**。
- zone 数 Z = zones 配置 ∪ `-ZoneId` ∪ 非 DryRun 时线上带 `-NamespacePrefix-` 前缀、不在配置里的 zone namespace。**行为变化**：`-ReleaseProfile` 不是 dev 时 zones 配置缺席直接拒绝部署（Z 不可信）。

### 8.1 编译与单测（Codex）

| 工作目录 | 命令 | 通过标准 |
|---|---|---|
| `go/shared` | `gofmt -l kafkautil`；`go vet ./kafkautil/`；`go test ./kafkautil/ -run 'TestEnsureTopics\|TestPartitionContract\|TestParseReplicationFactor' -count=1 -v` | `TestEnsureTopicsSetsRetentionBytesOnCreateAndAlter`、`TestEnsureTopicsLeavesRetentionBytesUnmanagedWhenZero`、`TestEnsureTopicsRejectsNegativeRetentionBytes`、`TestEnsureTopicsRetentionBytesWithoutRetentionMs` 与既有用例 PASS |
| `go/db` | `gofmt -l internal/config`；`go vet ./...`；`go build ./...` | gofmt 无输出，退出码 0 |
| `go/login` | `gofmt -l internal/config`；`go vet ./...`；`go build ./...` | 同上 |
| 仓库根 | `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1` | 全部 PASS，含末尾「S5b 保留期与 Kafka 盘预算」一节（缺省值与各判定路径、两个 ConfigMap 的 `RetentionMs` / `RetentionBytes` 相等、账本行、kafka 清单的 `storage:`、负向参数、5 broker 取上界、模板与实际 PVC 取小 / live-pvc / pvc-not-expanded、线上 zone namespace 并入 Z、zones 配置缺席的告警与非 dev 档拒绝、FitLimited 的 lag 提醒） |
| 仓库根（DryRun，不连集群） | `pwsh -NoProfile -NonInteractive -File tools/scripts/k8s_deploy.ps1 -Command infra-up -DryRun -SkipPreflight -SkipGoSvc -SkipJavaSvc -KafkaProfile dev -NodeImage registry.invalid/test/mmorpg-node:0123456789ab` | 输出一行 `Kafka disk ledger:`（`pvc_source=computed`），kafka 清单渲染出 `storage: 20Gi`、没有残留的 `__KAFKA_STORAGE_SIZE__` |
| F9（S5 规格附录） | 用最小用例分别核实 go/db（go-zero v1.10.0）与 go/login（go-zero v1.9.2）的 `conf.MustLoad` 遇到未知键 `RetentionBytes` 是忽略还是报错；go/db 现有的 `TestConfigLoaderIgnoresUnknownKeys` / `TestPreHandoffBinaryIgnoresUnknownKeys` 可作 db 侧旁证，login 侧没有对应用例 | 结论写回本行；若会报错，S5b 的回滚必须连同旧的 ConfigMap 生成器一起回退 |

### 8.2 上线（负责人执行）

| # | 事项 | 状态 |
|---|---|---|
| 8.2.1 | 合入 Go 改动，构建 db / login 镜像（Go 侧要与 `k8s_deploy.ps1` 渲染新键同批或先合入） | 待做 |
| 8.2.2 | 逐 zone 跑 zone-up：新 ConfigMap 带 `RetentionMs` / `RetentionBytes`，db / login 重启时经 `kafkautil.EnsureTopics` 生效。不想为改保留期滚动 go/db（瓶颈文档 §4.5 的发版红线）时，用 README 里的 `kafka-configs.sh --alter`（经 `/bin/sh -c` 覆盖 `KAFKA_HEAP_OPTS='-Xms16m -Xmx64m'`）立即生效，ConfigMap 等下次正常发布再收敛 | 待做 |
| 8.2.3 | **账本行出现 `[KafkaDiskLedger]FitLimited:` 时先确认 db 消费 lag ≈ 0 再发布**：生效值低于 topic 现值时，db / login 重启立刻下调 topic 级 retention.bytes，Kafka 在下一次 retention 检查（2 分钟）内删掉超出部分的最旧积压，不管 db 读没读到；lag 不为 0 时先扩盘或等 db 追平 | 每次发布核对 |
| 8.2.4 | infra-up：已有集群沿用线上盘，盘低于预算时按盘收缩并打 WARN（例：prod 档、20Gi、2 个 zone 时从 broker 缺省 512MiB 降到 256MiB，按设计速率约缓冲 10 分钟）；要扩盘按 README 的扩盘 runbook（patch 各 PVC → `delete statefulset kafka --cascade=orphan` → 带 `-KafkaStorageGi N` 重跑 infra-up → 对每个 zone 重跑 zone-up）；新集群直接按公式建盘 | 待做 |
| 8.2.5 | 部署前确认 `-ZonesConfigPath` 指向登记了共用这套 Kafka 的全部 zone 的配置（staging / prod 缺席即拒绝）；加 zone 之后对全部 zone 重跑 zone-up | 待做 |
| 8.2.6 | kind / 本机显式传 `-KafkaProfile dev`（缺省 prod 会申请 80Gi PVC） | 待做 |
| 8.2.7 | 回滚：topic 级配置在回滚后仍留在 topic 上，本身无害；要撤用 `--alter --delete-config retention.bytes`；F9 未核实前回滚要连同旧的 ConfigMap 生成器一起回退 | — |

### 8.3 状态

- 代码已落，未编译、未测试；8.1–8.2 全部待做。§3 表里「db_task 的 retention.bytes 与 PVC」即本节。
- 剩余风险：zone-down 删 namespace 之后，那个 zone 的旧 db_task topic 仍按旧的 retention.bytes 留着数据，直到 retention.ms 到期才释放盘，盘预算数不到它；lag 秒级告警本轮没做（go/db 没有导出消费 lag 指标），积压逼近缓冲时没有主动告警，删除是静默的；prod / prod-like 的 retention.bytes 按 2h 算，先于 24h / 6h 的 retention.ms 生效，账本行打 `[KafkaDiskLedger]BufferBelowRetention:`（属预期）；设计速率是推算值，`implied_db_task_buffer_min` 只是估算，回档的 TargetTime 不能早于这个窗口。（2026-10-18 更正：这一句是「从 TargetTime 起重放」语义下的约束；负责人 2026-10-17 裁决回档为「丢弃 TargetTime 之后的写」，Step 5 拨到日志末端、不再从 Kafka 重放，TargetTime 只受 MySQL binlog 保留期约束，见 6.5。）

---

## 9. ④ 重试只交给分区属主 + 重试 / 死信 / 毒消息入队 WAITAOF（S1）

> 2026-10-17 新增。状态：**已落码，未编译、未测试，待 Codex 验证。**

代码已落：S1 的 go/db 部分（`internal/kafka/retry_routing.go`、`durable_enqueue.go`、`cmd/retry_requeue/main.go` 都首次出现在 `e76f6f747`）、`go/db/etc/db.yaml` 的 `RetryRouting` 与 `WaitAOFNumLocal` / `WaitAOFNumReplicas` / `WaitAOFTimeoutMs`、告警 `deploy/k8s/db-task-retry-alerts.yaml` 的 ④ 段已在 HEAD。R7 语义复核的修复截至 2026-10-17 在工作区、未提交：待重搬收据表（`retry_stranded.go`，指标 `db_retry_stranded_receipts`）、认领与收据 ack 的防重（`kafka:retry:guard:*`，见 §4.4 末条）、实例模式 HASH `kafka:retry:instance_mode:{topic}`、消费组错误观测（`consumer_group_errors.go`，指标 `db_consumer_group_errors_total{source}`）、入队未确认时的重新入组退避（`rejoin_backoff.go`）、`retry_requeue` 的 `-zone` 必填与退出码 4 / 5，以及告警 `DbRetryStrandedReceipts`、`DbConsumerGroupHeartbeatErrors`。
设计与运维以 [db-retry-owner-routing.md](../design/db-retry-owner-routing.md) 为准（本批另行撰写）；规格以 [S1 修订稿](../handoff/godb-pipeline-impl-20261015/S1-retry-owner-waitaof-addendum.md) 为准，修订稿 §10 只列差异，没写到的照 [S1 原规格](../handoff/godb-pipeline-impl-20261015/S1-retry-owner-waitaof-spec.md)「上线与回滚」；回滚到 S1 之前的镜像按 [发布清单](release-checklist.md) §4「go/db 回滚到 S1(重试只交给分区属主)之前的版本」。

### 9.1 前置（线上不动）

| # | 事项 | 执行者 | 状态 |
|---|---|---|---|
| 9.1.1 | 复验清单 [placement-reverify-checklist-20261012.md](../handoff/placement-reverify-checklist-20261012.md) §2「第 12 步」第 3 条「本机双 db 实例」，以及 §5「已知覆盖缺口」第 1、4、7 条（第 7 条由 `TestStop_RetryLoopJoinsWaitGroup` 覆盖）；compose 文件是 `deploy/docker-compose.yml`；结果写回 PROGRESS | Codex | 待做 |
| 9.1.2 | 查清并记录各 zone 线上 go/db 的镜像与副本数；发布窗口不与 TiDB Phase 1 任何迁移批次重叠（[TiDB 决策](../design/global-data-layer-tidb-decision.md) D5 修订第 1 条：S1 整体是一个独立发布单元） | 负责人 | 待做 |
| 9.1.3 | K8s 的 Redis（`redis-0`）`redis_version` ≥ 7.2、`appendonly` 为 `yes`：开启 WAITAOF 时 go/db 启动先探一次，失败拒启。按 `deploy/k8s/manifests/infra/redis.yaml` 的口令注入方式在容器内执行 `INFO server` / `CONFIG GET appendonly`，口令不进命令行与日志 | 负责人 | 待做 |

### 9.2 编译与单测（Codex）

做法同 §2（临时 worktree，环境变量照任务单 §1）。工作目录 worktree 里的 `go/db`（PowerShell 7）：

```powershell
gofmt -l ./internal ./cmd ./db.go
go vet ./...
go build ./...
go test -count=1 ./internal/kafka/... ./internal/config/... ./internal/metrics/... ./cmd/...
go test -count=1 -v -run 'TestClaimLease|TestOwnedPartitions|TestRetryRouting|TestRequeuePartitioned|TestRetryQueueDepths|TestConsumeClaim|TestRetryOwnership|TestAOFGroupEnqueue|TestDirectEnqueue|TestDurableEnqueue_|TestProbeAOFAck|TestCheckWaitAOFReply|TestWaitAOFBudgetMs|TestDeferFailedTask_|TestQuarantine_|TestPoison_|TestStop_|TestConcurrentWorkers|TestRetryConsumer' ./internal/kafka/
go test -count=1 -v -run 'TestResolveRetryRouting|TestRedisWaitAOF|TestConfigValidateRejects|TestEtcYamlRetryKeysValid|TestConfigLoaderIgnoresUnknownKeys' ./internal/config/
# R7 复核的修复(待重搬收据、认领 / ack 防重、重新入组退避、消费组错误、claim 收尾语义、retry_requeue)
go test -count=1 -v -run 'TestRetryStranded_|TestRetryClaim_|TestAckRetryReceipt_|TestRejoinBackoffFor|TestDurableFailureBackoff_|TestWaitRejoinBackoff_|TestConfirmingEnqueue_|TestConsumerGroupError|TestClaimAcker_StopDropsLateConfirmations' ./internal/kafka/
go test -count=1 -v -run 'TestRetryRoutingSeriesPrecreated|TestStrandedAndConsumerGroupErrorSeriesPrecreated' ./internal/metrics/
go test -count=1 -v ./cmd/retry_requeue/
# race:需要 CGO 与 gcc;Windows 上没有就到 Linux / WSL 跑,并写明平台
$env:CGO_ENABLED='1'; go test -count=1 -race -run 'TestOwnedPartitions|TestClaimLease|TestRetryRouting|TestConsumeClaim|TestAOFGroupEnqueue|TestDeferFailedTask_|TestStop_|TestConcurrentWorkers|TestRetryStranded_|TestRetryClaim_|TestDurableFailureBackoff_|TestConsumerGroupError|TestClaimAcker_|TestWaitRejoinBackoff_|TestConfirmingEnqueue_' ./internal/kafka/
# 真 Redis(compose 的 redis,--appendonly yes,redis-server ≥ 7.2):必须 PASS,不得 SKIP
$env:GODB_REDIS_ITEST_ADDR='127.0.0.1:6379'; go test -count=1 -v -run 'TestDurableEnqueue_RealRedis' ./internal/kafka/
```

工作目录 `tools/merge_zone`：`go vet ./...`；`go test -count=1 ./...`（P4 / 审计对 p* 的 SCAN）。仓库根：`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`。可选：本机有 promtool 时，把 `deploy/k8s/db-task-retry-alerts.yaml` 的 `spec` 抽成规则文件跑 `promtool check rules`（含本批新增的两条告警）；没有就报告「告警语法未验证」。

通过标准：
- gofmt 无输出。R7 报告记下 `internal/kafka/applied_epoch_lua_test.go`、`internal/kafka/ordering_lease_test.go` 两个文件此前就不满足 gofmt（不是本批改的），若只列出它们，单独记录、不算本节失败；
- vet 与 build 退出码 0；全部 PASS；新用例名都出现在 `-v` 输出里且不是 SKIP（第二条命令的正则会带上 `TestDurableEnqueue_RealRedis`，没设 `GODB_REDIS_ITEST_ADDR` 时 SKIP 属预期，由最后一条命令补跑）；
- 既有的 retry_routing / retry_ownership / consumer_shutdown / l04 / durable_enqueue 用例没有新增失败；契约测试 0 失败。
- 失败时保留失败用例名、`-v` 输出的最后 200 行、编译错误前 20 行；红了就停下归因（区分 S1 / R7 与他人改动），不连续重试。

### 9.3 本机端到端（Codex；功能验证，不出性能结论）

先按 AGENTS.md §6.2 清环境（`redis-cli FLUSHALL`、清空测试库、清 etcd、删 `tools/scripts/.run` 的 pid / log）。

```powershell
# 仓库根
$env:DB_RETRY_ROUTING='owner'; pwsh -NoProfile -File tools/scripts/go_services.ps1 -Command start-exe -Services db -Counts db=2
# 两个实例日志各有 "retry routing: mode=owner source=env DB_RETRY_ROUTING";/metrics(:9160)有 db_retry_routing_mode{mode="owner"} 1
cd go/db; go run ./cmd/data_stress -zone 1 -players 200 -writes 50 -concurrency 32
# 压测中对 10 个测试玩家持锁 30 秒(MsgType 取 go/db 日志 "task processed" 行或缓存键 {MsgType}:{Key} 的前缀):
docker exec redis redis-cli SET "distributed:lock:kafka:ordering:db_task_zone_1:<pid>:<MsgType>" e2e-hold PX 30000
# 持锁期间 taskkill /F /PID <其中一个 db 的 PID>
go run ./cmd/verifier -config etc/db.yaml -mode strict -wait 180s
# 抓两个实例 /metrics 过滤 db_retry_ / db_owned_partitions / db_durable_enqueue_ / db_consumer_group_errors_;
# 键:docker exec redis redis-cli --scan --pattern 'kafka:retry:queue:db_task_zone_1*' 逐个 LLEN,及 kafka:retry:processing:db_task_zone_1:*、kafka:dead:queue:db_task_zone_1
```

| # | 演练 | 通过标准 |
|---|---|---|
| 9.3.1 | 上面的 owner 双实例 + 持锁 + 强杀 | verifier 0 mismatch；两实例 executed_nonowner = 0；所有 ready / p* / processing / dead 的 LLEN = 0；被杀实例有在途收据时，存活实例打出 "reclaimed abandoned retry receipts"；没有 "processing receipt not found"；`db_durable_enqueue_total{result!="ok"}` = 0；`db_retry_stranded_receipts` = 0 |
| 9.3.2 | shared 回滚演练：owner 下持锁造出 p* 积压 → 停两实例 → `$env:DB_RETRY_ROUTING='shared'` 重启 | p* 排空，verifier 0 mismatch |
| 9.3.3 | retry_requeue 门禁与搬回（工作目录 `go/db`；R7 后 `-zone` 必填）：owner 下持锁造出 p* 积压；owner 实例还在时跑 `go run ./cmd/retry_requeue -f etc/db.yaml -zone 1 -check`；再 `go_services.ps1 -Command stop -Services db` 全停，依次跑 `-zone 1 -check`、`-zone 1`（搬回）、`-zone 1 -check`；另跑一次 `-zone 9 -check` 与不带 `-zone` 的 `-check`；最后用 shared 起实例 | 依次以 5（一条不动，输出逐行列出实例与 mode=owner）、3、0（`done: every partitioned ready list is empty`）、0 退出；`-zone 9` 以 4 退出（target looks unused）；不带 `-zone` 以 1 退出；shared 启动后 verifier 通过 |
| 9.3.4 | WAITAOF 拒启演练（可选）：`docker exec redis redis-cli CONFIG SET appendonly no` → 起一个 db | 必须拒启且日志含 WAITAOF 提示；做完**必须** `docker exec redis redis-cli CONFIG SET appendonly yes` 还原 |
| 9.3.5 | （可选，R7 补，规格外）运行中关 AOF：db 运行、持锁注入让 Kafka 源任务走入队时 `CONFIG SET appendonly no`，约 2 分钟后还原为 yes | `db_durable_enqueue_total{result=~"not_durable\|error"}` 增长、位点不前进；go/db 日志「consumer group rejoin delayed … delay=…」的 delay 按 1s、2s、4s… 增长、封顶 30s；还原后第一次入队确认成功即清零，lag 排空、verifier 0 mismatch。**必须还原 appendonly** |
| 9.3.6 | 用 `RetryRouting=shared` 把 9.3.1 整跑一遍 | 标准同 9.3.1，executed_nonowner 允许 > 0 |

待重搬收据（`db_retry_stranded_receipts`）不做端到端注入：要让 Redis 只拒绝某一次搬移，本机难以确定性构造，以单测 `TestRetryStranded_*` 为准。失败时保留两实例 stdout / stderr、各时刻 metrics 快照、verifier 输出、Redis 键清单。

### 9.4 kind / staging 演练（生产第 1 步；Codex 执行，生产不做任何注入）

| # | 事项 | 通过标准 | 状态 |
|---|---|---|---|
| 9.4.1 | 9.3 的锁注入、kill -9、MySQL 暂停、shared 回滚与 retry_requeue 工具演练在这里重做；retry_requeue 按发布清单 §4 的 port-forward 形状执行，`-zone` 必填 | 同 9.3 | 待做 |
| 9.4.2 | **异步不阻塞**：稳态负载下锁住 10% 合成测试玩家的排序锁 60s（只用带 TTL 的 `distributed:lock:kafka:ordering:db_task_zone_<z>:<测试pid>:<MsgType>`，`errOrderingLockBusy` 免预算、不进死信） | 健康玩家的 w_task / w_queue（`tools/scripts/stress_summarize.ps1` 的 2d 段，或 `db_task_stage_seconds{stage="op_total"}`）与不锁时相差 ≤ 20%；Kafka lag 不持续增长；`db_durable_enqueue_seconds` 均值 ≤ 1.5s；`db_durable_enqueue_total{result=~"not_durable\|error"}` 为 0 | 待做 |
| 9.4.3 | **滚动重启**：`kubectl -n <zone ns> rollout restart deploy/db` + `rollout status` | `db_durable_enqueue_total{result="canceled"}` 可以增长，`not_durable` / `error` 必须为 0，DbDurableEnqueueNotConfirmed 不响；（R7 补）`db_retry_stranded_receipts` 为 0，DbRetryStrandedReceipts 不响 | 待做 |
| 9.4.4 | 告警载入：kind 的 Prometheus 载入本文件告警后，规则全部 health=ok；记录滚动期间 `db_consumer_group_errors_total` 各 source 的增量（DbConsumerGroupHeartbeatErrors 是 info，broker 滚动时响属预期） | 无载入错误 | 待做 |

### 9.5 生产上线（负责人执行；逐 zone、低峰、一次一个 Pod，遵守[瓶颈文档](../design/db-task-pipeline-bottlenecks.md) §4.5）

| # | 事项 | 状态 |
|---|---|---|
| 9.5.1 | 告警：`kubectl apply -n observability -f deploy/k8s/db-task-retry-alerts.yaml`（与 2.5.2、4.3.1 同一个文件）。④ 所在的 `db-task.retry-and-dead-letter` 组现有 DbRetryPartitionedNoProgress、DbRetryPartitionedStranded、DbZoneNoPartitionClaims、DbRetryNonOwnerExecutionInOwnerZone、DbRetryRevokedOutsideRebalance、DbDurableEnqueueNotConfirmed，R7 新增 DbRetryStrandedReceipts（warning，for 10m）与 DbConsumerGroupHeartbeatErrors（info） | 待做 |
| 9.5.2 | **第 2 步**：发布 S1 镜像，`go/db/etc/db.yaml` 保持 `RetryRouting: shared`、WaitAOF 1 / 0 / 1500（`k8s_deploy.ps1 zone-up` 生成 ConfigMap；镜像变化会触发滚动）。每换一个 Pod 核对：启动未拒（WAITAOF 探针通过）、启动日志 `retry routing: mode=shared source=config`、`db_retry_routing_mode{mode="shared"}` = 1、`db_retry_queue_length{queue="partitioned"}` = 0。**验收**（修订稿 §10）：`db_durable_enqueue_total{result=~"not_durable\|error"}` 不增长（滚动期间 `canceled` 允许增长），DbZoneNoPartitionClaims 不响；（R7 补）`db_retry_stranded_receipts` 为 0。记录基线：executed_nonowner / executed 比例、`db_ordering_lock_busy_total`、死信增速、`db_task_result_total{result="error"}`、Kafka lag、`db_consumer_group_errors_total` 各 source 的增速。回滚：回旧镜像（shared 模式不写 p*），仍按发布清单 §4 先跑 `-check` | 待做 |
| 9.5.3 | **第 3 步 金丝雀**（建议在该 zone 完成 S2 切组之后）：前提是第 2 步完成、该 zone 每个 Pod 都暴露 `db_retry_routing_mode`；冻结其它 zone 发布；`kubectl -n <zone ns> set env deploy/db DB_RETRY_ROUTING=owner` + `rollout status`。只被动观察：各 Pod `db_owned_partitions` 之和 = PartitionCnt；executed_nonowner = 0；forwarded 之后共享收件箱 1–2 拍归零；requeued_revoked 只在再平衡时刻出现；`db_ordering_lock_busy_total{origin="retry"}` 接近 0；死信速率不高于基线；没有 "processing receipt not found"。需要积压时只对合成测试玩家持带 TTL 的排序锁。回滚：`set env deploy/db DB_RETRY_ROUTING-`（触发滚动），shared 实例自动排空 p* | 待做 |
| 9.5.4 | **第 4 步 全量**：`db.yaml` 改 `RetryRouting: owner` → zone-up → `kubectl -n <zone ns> rollout restart deploy/db` + `rollout status`（只改 ConfigMap 不会滚动）→ 去掉金丝雀 env（`set env … DB_RETRY_ROUTING-`）。验收：每个 Pod 启动日志 `retry routing: mode=owner source=config`；24 小时内 executed_nonowner = 0、死信速率不升、p* 无长期积压、lag = 0、DbDurableEnqueueNotConfirmed 不响。回滚：改回 shared → zone-up → rollout restart + status | 待做 |
| 9.5.5 | **回滚到 S1 之前的镜像**：按发布清单 §4 那一条：切 shared 并等滚动完成 → 三项人工核对 → `go run ./cmd/retry_requeue -f etc/db.yaml -zone <z> -check` 退出码 0（3 / 4 / 5 / 1 的处置见该条）→ 认准 revision 回滚；已经回滚的立即搬回（DbRetryPartitionedStranded 约 10 分钟内响） | 每次回滚 |
| 9.5.6 | redis-ha 接入时 go/db 的 `WaitAOFNumReplicas`：**不再手改 db.yaml**（保持 0，给本机 compose 单实例；写 1 本机 go/db 的启动探针等不到副本而拒启）。取值来源：K8s 上 db ConfigMap 的这一项由 `tools/scripts/k8s_deploy.ps1` 按共享 Redis 形态渲染（`Get-MappingRedisTopology` → `Get-RouteStoreWaitAOFNumReplicas`，与 data-service 的 `RouteStore.WaitAOFNumReplicas` 同一个函数：单实例 0、redis-ha 1；形态由 `-RedisReplicas` 与档位决定，zone 写路径没给参数时跟随线上 `statefulset/redis` 副本数），`WaitAOFNumLocal` / `WaitAOFTimeoutMs` 仍镜像 db.yaml。剩下的运维动作：单实例切到 redis-ha 后对每个 zone 重跑 zone-up，再 `rollout restart deploy/db`（data-service 同样要 `rollout restart deploy/data-service`；只改 ConfigMap 不会滚动），并重新评估 WAITAOF 的可用性代价（副本不可用时确认恒不足，见 9.6；切主后几秒内新主没有从时确认失败、刚启动的 go/db 被探针拒启） | 部署侧已落码（2026-10-19，未运行，待 Codex 验证）；切换演练与可用性复评视 redis-ha 进度 |
| 9.5.7 | Java 仓 `PARITY.md` 登记「不适用」（Java 没有 Kafka 存盘链与重试队列），由主会话统一登记 | 待做 |

### 9.6 状态

- 代码已落，未编译、未测试；9.1–9.5 全部待做。
- 剩余风险（R7 复核之后）：
  - 防重记录 TTL 5 分钟：见 §4.4 末条。
  - 死信副本没有硬上界：WAITAOF 持续无法确认（redis-ha 下 `WaitAOFNumReplicas=1` 而副本在维护、AOF 盘 fsync 超过单批预算、运行期被关 AOF）时，队头是确定性进死信的消息就每个周期多推一份字节相同的副本。R7 只做了重新入组退避（副本从约 5–10s 一份降到最多约 35s 一份）；在死信入队处按 (topic, partition, offset) 去重没做 —— 要改 `durableEnqueue` 接口与组提交 pipeline，并需负责人与 L-04 第 2 批死信工具一起定「LPOS 复核」还是「工具同步删标记」。在此之前死信按 (topic, partition, offset) 或 taskID 人工去重。
  - 双属主窗口：coordinator 逐出本成员后，本进程要过约 Net.ReadTimeout 到 (Metadata.Retry.Max+1)×Net.ReadTimeout 才察觉，期间 owner 重试与新属主并发，只靠排序锁与 applied 游标兜底；不调小 Net.ReadTimeout（读 JoinGroup 响应也用它）。只能经 `db_consumer_group_errors_total{source="heartbeat"}` 与日志定位（`retry_routing.go` 文件头）。
  - claim 再平衡 / 让出 / 失败收尾时，等到的异步入队确认不再标记位点（R7 选 (a)，不改行为）：等确认的 offset 及其后已完成的 offset 由新属主重投，存储故障期会多出重复的重试副本，靠游标与 seq 吸收；只有停机排空与消息通道关闭的排空会先等确认再标记（`key_ordered_consumer.go` 里 `drainAfterStop` 的注释）。规格与设计文档按这一实现改写。
  - `retry_requeue` 的退出码 5 看不见不登记模式的更早 S1 镜像，三项人工核对不能省；DbRetryPartitionedStranded 只看 6h 内的最后样本，窗口过后只能靠 `-check`。
  - WAITAOF 不覆盖游标、收据 ack 与搬移：AOF 尾巴丢失时旧写可能重放，残余靠 MySQL 条件写封死；幂等依据的前提是游标没有随 Redis 回退。
  - 待重搬收据表只在进程内存里：进程崩溃时随之丢失，靠租约过期后别的实例回收；Redis 一直拒绝搬移时只能重启该 Pod（DbRetryStrandedReceipts）。
  - owner 模式下没有 claim（Kafka 故障、S2 交接闸卡住）时重试停滞，由 DbZoneNoPartitionClaims 兜底。
  - `stress_summarize.ps1` 的 2f 段没有 `db_owned_partitions` 与 `db_durable_enqueue_*` 列（修订稿登记为下次改脚本时补）。

## 10. task:result 读回执通知按 login 实例分频道（B0-3 第 2 批，2026-10-18 登记）

go/db 按 `DBTask.reply_instance` 只 PUBLISH 一次：合法发发起方 login 的实例频道 `task:result:notify:<id>`，为空或不合法回落全服旧频道；新指标 `db_task_result_notify_total{target,result}`。改动清单、上线顺序与交 Codex 的权威验证命令见[规格](../handoff/launch-readiness-20261013/spec_B0-3_task_result_channel.md)文末「# 第 2 批交付（2026-10-18）」。代码在工作区，**未编译、未测试，待 Codex 验证**。

| # | 事项 | 状态 |
|---|---|---|
| 10.1 | 编译、单测与修复前红证据：按规格「第 2 批交付」的验证命令执行（Codex）；L1 的 db 套件本批新加了 `./internal/metrics/...`，先确认它稳定为绿 | 待做 |
| 10.2 | 上线顺序：先全部 zone 的 go/db（行为不变，读回执全部计 `target="legacy_unset"`，先确认新序列 9 条都已采集到），再按 zone 金丝雀 login。金丝雀观察：`db_task_result_notify_total{target="instance"}` 里 `no_receiver` 占比约为 0；login 的 `login_task_result_delivery_total{outcome=~"salvaged\|timed_out\|result_missing"}` 不上升；entergame 的 preload 失败率不变。三项都正常再全量。回滚：任一侧单独回滚即可，无数据迁移（2026-10-18 更正：「先全部 zone 的 go/db、行为不变」只在该 zone 的 login 还是第 1 批之前的版本时成立。第 1 批 login 已在 `e7bb46070`，可能已部署或与 go/db 同批发版；那样一次升全部 zone 的 go/db 会让这些 zone 同时切到实例频道，绕开 login 金丝雀，且告警尚未 apply。动手前逐 zone 只读核对：login `:9101` 上已有 `login_task_result_delivery_total` 序列，或 task:result 所用 Redis 上 `PUBSUB CHANNELS 'task:result:notify:*'` 非空，即按「已是第 1 批」处理，改为按 zone 金丝雀 go/db：先上一个 zone，看同样三项，正常再逐 zone 推开。同一 zone 的 go/db 与 login 不要在同一次发版里同时升。观察期间 `PUBSUB NUMPAT` 应为 0：PUBLISH 的订阅者数含模式订阅，开着 `PSUBSCRIBE task:result:notify:*` 会把 no_receiver 压成 0。详见规格「第 2 批交付 / 上线顺序」第 0 步。） | 待做 |
| 10.3 | 告警：go/db 与 login **全量升级之后**再 `kubectl apply -n observability -f deploy/k8s/task-result-notify-alerts.yaml`（独立文件，不与 2.5.2 / 4.3.1 / 9.5.1 的 `db-task-retry-alerts.yaml` 同批；全量之前 `DbTaskResultNotifyLegacyFallback` 会在金丝雀 zone 响）。阈值待压测基线。回滚 `kubectl delete -n observability -f` 同一文件；负责人执行 | 待做 |
