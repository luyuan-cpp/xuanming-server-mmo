# go/db 重试只交给分区属主（④）与 Kafka 源入队的落盘确认（WAITAOF）

> **已过期(2026-10-30)**:玩家存盘改成 scene 直接提交 TiDB 的 `player_store`([player-save-direct-commit.md](./player-save-direct-commit.md)),go/db 的 Kafka 消费、重试队列和 WAITAOF 都已删除。本文只作历史保留,不再描述现行行为。

> **状态**：已落码，未编译、未测试，待 Codex 验证（截至 2026-10-17 工作区，含 R7 语义复核 godb 区域的 9 条修复）。告警表达式没有经过 `promtool` 校验，也没有在真实 Prometheus 上跑过。
> **范围**：存盘管线 S1。包括 ④ 的 (a)「重试只在原始分区的属主进程执行」、(c)「go/db 副本数 ≤ PartitionCnt 的部署门禁」，以及三类 Kafka 源入队点的 WAITAOF 组提交，外加回滚工具 `go/db/cmd/retry_requeue`。B0（调 SubShardCount）和 ④B2（双消费在线换代）不在本批。TiDB 决策 D5 的修订块已于 2026-10-17 写进 `global-data-layer-tidb-decision.md`。
> **执行清单**：Codex 与负责人逐项打勾用 [上线待做](../ops/godb-pipeline-launch-todo.md) §9；回滚到 S1 之前的镜像用 [发布清单](../ops/release-checklist.md) §4「go/db 回滚到 S1(重试只交给分区属主)之前的版本」。两者的设计依据都是本文。
> **依据**：`docs/handoff/godb-pipeline-impl-20261015/` 下的 S1 规格（`S1-retry-owner-waitaof-spec.md`）、复核（`…-review.md`）、修订稿（`…-addendum.md`，优先级高于前两者），以及 R7 语义复核的修复。**代码与规格冲突时以代码为准**。冲突处在正文里标「与规格的差异」，汇总见 §15；修订稿文末另有「2026-10-16 更正」一节。
> **证据口径**：全部来自静态阅读代码。没有编译，没有运行，也没有连过集群。文中的量级都是按设计参数推算的，没有实测。
> **相关文档**：[瓶颈文档](db-task-pipeline-bottlenecks.md) §4.4（G6 停机顺序）与 §5（④ 要点）；[消费组按 zone 拆](db-consumer-group-per-zone.md)（S2，交接闸）；[MySQL 条件写](db-write-fencing-mysql.md)（围栏）；[L-04 延后](db-task-store-unavailable-deferral.md)；[分区契约](db-task-kafka-partition-contract.md)；[合服手册](../ops/merge-zone-runbook.md)；[上线待做](../ops/godb-pipeline-launch-todo.md)；[发布清单](../ops/release-checklist.md)。

## 0. 结论与范围

**结论**

- **改动前**：重试 ready 是全 zone 共用的一个列表，任何 go/db 实例都能认领并执行，不检查自己是否持有该任务的原始分区。所以同一玩家的重试会和新属主手里该分区的新消息跨进程并发，只靠排序锁、applied 游标和 owner epoch 兜底。
- **改动后**：开关 `ServerConfig.Kafka.RetryRouting`（`shared` | `owner`，缺省 `shared`，环境变量 `DB_RETRY_ROUTING` 优先）。
  - `owner` 模式：重试只在持有原始分区 sarama claim 的进程里执行，并且和该分区的新消息落在同一个子 shard 上串行。
  - `shared` 模式：保持原来的语义，另外每拍兜底排空按分区拆开的 ready（回滚路径）。
- **属主判据**只有一个：本进程此刻是否持有这个分区的 claim（ConsumeClaim 正在跑，租约没有关闭，claimCtx 没有取消）。不用 Setup / Cleanup 判属主。
- **ready 拆成两层**：
  - 共享「未分拣收件箱」`kafka:retry:queue:{topic}`；
  - 按原始分区的 `kafka:retry:queue:{topic}:p{N}`。
  回收、停机归还、旧版实例都只写收件箱，由 owner 实例分拣进 p{N}，回收与路由解耦。
- **WAITAOF 的覆盖面**：只覆盖「Kafka 位点提交之前、任务唯一副本首次写进 Redis」的三类入队点。做法是专用小连接池上的组提交，确认异步回调，确认之前不 ack。确认不足时 fail-closed：位点不提交，消息重投。
- **WAITAOF 不保证的东西**：游标、owner epoch、收据 ack 和重试源搬移仍是普通写。**本文不把任何部分表述为「Redis 崩溃安全」**（§7.7）。
- **回滚**：回滚到 S1 之前的镜像前，必须先切 shared，并用 `retry_requeue -check` 确认 p* 为 0（§10）。工具的退出码 0/1/3/4/5 都是门禁的一部分。

**R7 语义复核在本批代码上补的内容**（详见 §6.3、§7.8–§7.10、§10.4、§11）

- 双属主窗口写进了 `retry_routing.go` 文件头。新增 sarama 错误通道的观测：`db_consumer_group_errors_total{source}`，配合限速 ERROR。
- 新增待重搬收据表（`retry_stranded.go`）和 gauge `db_retry_stranded_receipts`。
- 认领与收据 ack 改为按 nonce 防重（`kafka:retry:guard:*`，TTL 5 分钟）。
- 入队未确认时，重新入组前做指数退避（`rejoin_backoff.go`）。
- 实例模式登记进 HASH `kafka:retry:instance_mode:{topic}`。`retry_requeue` 的 `-zone` 改为必填，新增 `-topic-generation`、`-allow-empty-target`，新增退出码 4 / 5。
- 位点语义维持代码现状（§7.4）：只有停机排空和「消息通道关闭」的排空会等确认再提交。这一点偏离修订稿裁决第 1 条与 §1.6。

**拍板**

- 负责人 2026-10-13 定：④ 先做，并修订 D5；WAITAOF 归存盘管线交接（全局单点交接 P0 里 go/db 的那一部分）。owner_epoch 铸造（scene_manager）与账号目录（login）的 WAITAOF 不在本文范围。
- (c) 门禁只放在 `tools/scripts/k8s_deploy.ps1` 的写路径入口，go/db 启动期不拒启（§8）。
- owner 模式**不设 shared 兜底**（§3、§6.2）。

**残余风险**（修订稿裁决第 4、16 条要求的风险段；原规格「风险」与修订稿 §11 中仍成立的各条。详细说明在所指各节，本段只做索引）

1. **证据**：全部来自静态阅读，未编译、未测试；告警表达式未经 `promtool` 校验（文首「状态」）。
2. **MySQL 恢复期的重试排空并发下降**（原规格风险第 6 条）：owner 模式下 p{N} 只由属主执行，非属主与热备不再额外执行，p* 回落比 shared 慢。staging 演练对比两种模式的回落时间，必要时按 B0 调 `SubShardCount`（本批不做）。见 §6.1、§10.2 第 1 步。
3. **owner 模式没有 claim 就停滞**：Kafka 不可用、长时间再平衡、S2 交接闸等待期间 p* 只进不出，数据不丢。见 §6.2，告警 DbZoneNoPartitionClaims。
4. **双属主窗口与排空超时后的重叠**：只靠排序锁与 applied 游标兜底，彻底封死要靠 MySQL 条件写。见 §6.3、§12.3。
5. **WAITAOF 不覆盖游标、收据 ack 与重试源搬移**：AOF 尾巴丢失时游标可能回退，旧 seq 收据重放会把 MySQL 覆盖回旧档，只能由 MySQL 条件写封死。见 §7.7。
6. **位点语义**：再平衡、交接让出与失败收尾走 `drainAfterStop`，之后等到的确认不再标记位点，这些 offset 由新属主重投；MySQL 故障期间每次再平衡每分区多重投约 100～200 条（推算）。见 §7.4。
7. **WAITAOF fail-closed 的可用性代价**：确认不足时 claim 失败并触发再平衡；入队层吞吐上限约每次 fsync 一批，超出时背压成 Kafka lag；K8s 共享 Redis 切到 redis-ha 后，`k8s_deploy.ps1` 把 db ConfigMap 的 `WaitAOFNumReplicas` 渲染成 1（2026-10-19 部署侧落码，未运行，待 Codex 验证），副本维护期间失败路径上的分区都会停滞。见 §7.5、§7.10、§10.2「redis-ha 接入时」。
8. **重复副本**：来源有五类，死信里的副本没有硬上界，需人工按 taskID 或 (topic, partition, offset) 去重。见 §7.6、§7.10。
9. **防重记录与待重搬表的边界**：Redis 卡顿超过 5 分钟（记录 TTL）时可能重复 POP / LREM；待重搬表随进程崩溃丢失，靠租约过期后别的实例回收。见 §7.8、§7.9。
10. **回滚到 S1 之前的镜像**：DbRetryPartitionedStranded 只在 6h 窗口内有效，过后只能靠 `retry_requeue -check`；退出码 5 看不见不登记模式的早期 S1 镜像，三项人工核对不能省。见 §10.3–§10.5、§11。
11. **副本数门禁可被绕过**：`kubectl scale`、`kubectl edit` 与带外 apply 的 HPA 都不经过门禁。见 §8。
12. **观测缺口**：专用入队池没有 `db_redis_pool_*` 指标；`stress_summarize.ps1` 2f 段缺 owned / aof / stranded / cg_err 列；告警按 namespace 聚合，看不到单个分区卡住。见 §7.11、§13.4、§11。

## 1. 现状

### 1.1 改动前的问题

- **重试不绑属主**：
  - `consumeRetryQueue` 从共享 ready 认领，再按载荷 v2 头里的原始分区派给本地 worker，不看本进程有没有这个分区的 claim；重试的 workerTask 也不带 claimCtx。
  - 后果一：同 key 的重试与新属主的新消息跨进程并发，表现为 `ordering_lock_busy` 延后，以及每次再平衡时的重复执行。
  - 后果二：handleTask 入口的 claim 检查只调 `failKafkaTask`，而它对重试是空操作。只给重试挂 claimCtx、不改这里，收据会永远卡在本实例 processing。
- **入队没有落盘确认**：三个入队点（Kafka 源失败入重试、Kafka 源隔离进死信、毒消息）原来都经 `pushPayloadDurably` 只做 LPUSH。appendfsync everysec 下 Redis 崩溃最多丢约 1 秒的写，而 Kafka 位点已经提交，这笔存盘就丢了。

### 1.2 改动后的代码位置

| 文件 | 职责（符号） |
|---|---|
| `go/db/internal/kafka/retry_routing.go` | 键名（`retryReadyKey`、`retryPartitionReadyKey`、`retryPartitionReadyKeyPattern`、`escapeRedisGlob`）；模式（`resolveConsumerRetryRouting`、`retryRouteModeOf`、`ownerRouting`、`retryReadyKeyFor`）；租约（`claimLease`、`ownedPartitions`）；owner 分拣（`forwardRetryScript`、`forwardSharedRetries`）；owner 认领与派发（`consumeRetryQueueOwner`、`ownedRetryQuota`、`consumeOneOwnedRetry`、`moveClaimedRetry`、`requeueRevokedRetry`、`discardQueuedRetries`）；回滚工具核心（`requeuePartitionScript`、`RequeuePartitionedRetries`、`RetryQueueDepths`、`RetryQueueDepthReport` 及其 `PartitionedTotal` / `OwnerInstances` / `LooksUnused`、`ValidateRequeueBatch`、`RequeueDefaultBatch` / `RequeueMaxBatch`）；`claimFrom` |
| `go/db/internal/kafka/durable_enqueue.go` | 入队层：`durableEnqueue` 接口、`directEnqueue`、`aofGroupEnqueue`、`probeAOFAck`、`checkWaitAOFReply` / `waitAOFBudgetMs` / `isRedisErrorReply`、错误哨兵、`durableEnqueueResult` |
| `go/db/internal/kafka/retry_ownership.go` | 收据归属（按实例 processing、租约 ZSET）；R7 起加了 `retryClaimScript` 的 nonce 防重、`retryClaimResolveScript` / `resolveUnresolvedClaims`、实例模式 HASH（`retryInstanceModesKey`、`retryOwnership.routingMode`）、`retryGuardKey` / `retryGuardTTL` |
| `go/db/internal/kafka/retry_stranded.go` | R7：待重搬收据表 `strandedRetries`、`retryStrandedReceipts` |
| `go/db/internal/kafka/rejoin_backoff.go` | R7：`durableFailureBackoff`、`waitRejoinBackoff`、`confirmingEnqueue` |
| `go/db/internal/kafka/consumer_group_errors.go` | R7：`watchConsumerGroupErrors`、`consumerGroupErrorSource`、`claimHandlerError` |
| `go/db/internal/kafka/key_ordered_consumer.go` | `NewKeyOrderedKafkaConsumer`（模式、p 键、租约表、待重搬表、入队层）、`Start`（入队层探针、错误通道协程、重新入组退避）、`StartRetryConsumer`（纳入 `c.wg`）、`consumeRetryQueue` / `consumeRetryQueueShared` / `consumeOneRetryFrom` / `parseClaimedRetry`、`handleTask`（两次 claim 复核、`processCtx`）、`ConsumeClaim`（租约登记、`drainAfterStop`）、`drainClaimForShutdown`、`deferFailedTask` / `persistBeforeAck` / `submitBeforeAck` / `quarantineOrderingConflict` / `persistPoisonMessage` / `quarantinePoisonTask`、`moveRetryReceipt`、`ackRetryReceiptScript` / `ackRetryReceipt`、`moveClaimedRetryToDead`、`Stop`（第⑥步关入队层） |
| `go/db/internal/kafka/store_failure.go` | `sampleRetryQueues`（采样 ready / partitioned / processing / dead）、`homeStoreCoolingDown` |
| `go/db/internal/config/config.go` | `KafkaConfig.RetryRouting`、`RetryRoutingShared` / `RetryRoutingOwner` / `RetryRoutingEnvVar`、`ResolveRetryRouting`；`RedisConfig.WaitAOFNumLocal` / `WaitAOFNumReplicas` / `WaitAOFTimeoutMs` 及 `NumLocal` / `NumReplicas` / `WaitsForAOF`、`DefaultRedisWaitAOF*` / `MaxRedisWaitAOF*`；`(*Config).Validate` |
| `go/db/db.go` | 启动时 `ResolveRetryRouting` 合并配置与环境变量并回写，`Validate`，打印日志 `retry routing: mode=%s source=%s`，启动横幅打印 `retry: routing=… waitaof=…` |
| `go/db/internal/metrics/metrics.go` | §11 用到的全部指标与常量 |
| `go/db/cmd/retry_requeue/main.go` | 回滚工具（§10.4） |
| `go/db/etc/db.yaml` | `ServerConfig.Kafka.RetryRouting: shared`；`ServerConfig.RedisClient.WaitAOFNumLocal: 1` / `WaitAOFNumReplicas: 0` / `WaitAOFTimeoutMs: 1500` |
| `tools/scripts/k8s_deploy.ps1` | db ConfigMap 原样镜像其中三个键（`$dbRetryRouting`、`$dbWaitAOFNumLocal`、`$dbWaitAOFTimeoutMs`），`$dbWaitAOFNumReplicas` 按共享 Redis 形态渲染（`Get-MappingRedisTopology` → `Get-RouteStoreWaitAOFNumReplicas`，与 data-service 的 `RouteStore.WaitAOFNumReplicas` 同一个函数：单实例 0、redis-ha 1；2026-10-19 起，未运行，待 Codex 验证）；`Test-DbReplicaPartitionBudget`、`Assert-DbReplicasWithinPartitionContract`；Redis 连接总账 db 行 `Fixed = 2` |
| `deploy/k8s/db-task-retry-alerts.yaml` | 组 `db-task.retry-and-dead-letter` 里的 ④ / WAITAOF 告警（§11） |
| `tools/merge_zone/preflight.go`、`audit_checks.go` | `dbRetryPartitionQueuePrefix`、`dbRetryPartitionQueuePattern`、`escapeRedisGlob`、`dbTaskQueueKeys`（SCAN 发现 p*，出错 fail-closed） |

依赖版本（`go/db/go.mod`）：`github.com/IBM/sarama v1.43.1`、`github.com/redis/go-redis/v9 v9.18.0`、`github.com/zeromicro/go-zero v1.10.0`、`github.com/alicebob/miniredis/v2 v2.36.1`。sarama 的分配策略没有显式设置，用的是缺省的 Range + eager。

## 2. 键、载荷与需同步的同类入口

### 2.1 Redis 键

| 键 | 类型 | 谁写 | 谁搬出 / 读 | 说明 |
|---|---|---|---|---|
| `kafka:retry:queue:{topic}` | LIST | shared 模式的失败写回；回收（`retryReclaimScript`）；停机归还（`release`）；旧版实例；`retry_requeue` 搬回；`retryClaimResolveScript` 退回（认领自收件箱时） | shared 认领；owner 分拣（`forwardRetryScript`） | owner 模式下是「未分拣收件箱」 |
| `kafka:retry:queue:{topic}:p{N}` | LIST | owner 分拣；owner 模式的 Kafka 源失败入重试与重试失败写回（`retryReadyKeyFor`）；撤销搬回；misrouted 挪回；`retryClaimResolveScript` 退回（认领自 p{N} 时） | owner 认领（`claimFrom`）；shared 兜底认领；`retry_requeue` 搬回 | **新增**。N ∈ [0, PartitionCnt)，十进制，无前导零（`parseRetryPartitionReadyKey`）。topic 后紧跟 `:p`，所以 `db_task_zone_1` 的前缀不会匹配 `db_task_zone_11` 或 `db_task_zone_1_g2` |
| `kafka:retry:processing:{topic}:{instance}` | LIST | 认领脚本（RPOPLPUSH） | ack / 搬移 / 回收 / 归还 | 每个实例一份，不变 |
| `kafka:retry:processing:{topic}` | LIST | 不再写 | 回收（`drainLegacy`） | 旧版共享 processing |
| `kafka:retry:instances:{topic}` | ZSET | 续租 / 认领脚本 | 回收、`RetryQueueDepths` | 不变 |
| `kafka:retry:instance_mode:{topic}` | HASH | 续租 / 认领脚本里与 ZADD 同一原子操作 HSET（`routingMode` 为空时不写） | 回收脚本摘登记时 HDEL；`RetryQueueDepths` 用 HMGET 读 | **R7 新增**。field = 实例名，value = `owner` 或 `shared`（与 `db_retry_routing_mode` 的 mode 取值相同） |
| `kafka:retry:guard:claim:{processingKey}:{nonce}` | STRING，PX 5 分钟 | `retryClaimScript` | 同 nonce 重发时读；`retryClaimResolveScript` 改写成 `resolved` | **R7 新增**。值为「长度:SHA1」或 `resolved` |
| `kafka:retry:guard:ack:{processingKey}:{nonce}` | STRING，PX 5 分钟 | `ackRetryReceiptScript` | 同 nonce 重发时读 | **R7 新增**。值为删掉的条数 |
| `kafka:dead:queue:{topic}` | LIST | 死信入口全部 | 人工 | 不变 |
| `consumer:applied:{topic}:{key}:{msgType}`、`consumer:applied_epoch:{topic}:{key}:{msgType}` | STRING | `advanceApplied`（围栏 v0.4，§12.3） | 游标守卫 | 不变，**不受 WAITAOF 覆盖** |
| `distributed:lock:kafka:ordering:{topic}:{key}:{msgType}` | STRING | 排序锁 | — | 不变 |

guard 键的前缀 `kafka:retry:guard:` 与队列前缀（`kafka:retry:queue:`、`kafka:retry:processing:`）不相交，按队列前缀 SCAN 的工具（merge_zone、人工搬运）扫不到它。

### 2.2 载荷

- **重试载荷仍是 v2**：`[magic 0x02 (retryPayloadMagicV2), 1B][seq 8B 大端][原始分区 4B 大端][DBTask]`（`wrapRetryPayload` / `unwrapRetryPayload`），seq = Kafka offset + 1。
  - 分拣脚本在 Lua 里取第 10–13 字节作原始分区；值 ≥ 2^31 时按负数处理。
  - 越界或负数的分区计 `invalid`，非 v2 计 `legacy`，两者都原字节进死信。
- **毒消息载荷**是 magic 0x03 的 JSON 记录（`encodePoisonMessage`），只进死信。

### 2.3 Lua 脚本一览

| 脚本 | KEYS / ARGV | 返回 | 防重 |
|---|---|---|---|
| `forwardRetryScript` | KEYS=[收件箱, 死信, p0..p{N-1}]；ARGV=[本次上限] | `{moved, legacy, invalid, moved_p0 … moved_p{N-1}}` | 不防重：重发只会多分拣，不丢 |
| `retryClaimScript` | KEYS=[ready, 本实例 processing, 登记表, 认领记录, 模式 HASH]；ARGV=[实例名, 租约毫秒, 记录毫秒, 模式] | 元素或 nil | 按 nonce 防重（§7.9） |
| `retryClaimResolveScript` | KEYS=[认领记录, processing, 原 ready]；ARGV=[记录毫秒] | 0 / 1 | 可重入 |
| `moveRetryReceiptScript` | KEYS=[processing, 目的]；ARGV=[原收据, 替换载荷] | 0 / 1 | 不防重：重发最多多出一份重复执行 |
| `ackRetryReceiptScript` | KEYS=[processing, ack 记录]；ARGV=[收据, 记录毫秒] | 删掉的条数 | 按 nonce 防重（§7.9） |
| `requeuePartitionScript` | KEYS=[p 键, 收件箱, 死信]；ARGV=[上限] | `{moved, dead}` | 可重入 |
| `retryLeaseScript` / `retryReclaimScript` | 见 `retry_ownership.go` | — | 不防重 |

`forwardRetryScript`、`requeuePartitionScript`、`retryClaimResolveScript` 动手之前先核对涉及的键是 list（或不存在），否则整体报错、一条不动。原因是 Redis 脚本没有回滚，不能让 RPOP 之后的 LPUSH 因 WRONGTYPE 失败把收据弄丢。

### 2.4 需同步的同类入口

修订稿 §8 要求列出已改与未改的同类入口。同一工作区里有别的写手在同批改这些文件，下面是 2026-10-17 写完本文之后复核的结果。

**已改**

- `tools/merge_zone/preflight.go` 的 `dbTaskQueueKeys` 与 `audit_checks.go` 的 `kafka_db_task_queues`：用 SCAN MATCH 发现 p*（topic 先做 glob 转义），任一页出错整体拒绝。不按 p0..p{N-1} 枚举，原因有两条：merge_zone 不知道 PartitionCnt；完整的 SCAN 迭代保证返回期间一直存在的键。
- `docs/ops/merge-zone-runbook.md`：P4、`kafka_db_task_queues` 两行，§8 Step 2 的 SCAN 与 LLEN。
- `docs/design/shared-redis-split-by-purpose.md`：
  - dbctl 键清单 2026-10-16 补了 `kafka:retry:queue:<topic>:p*`，2026-10-17 补了 `kafka:retry:instance_mode:<topic>`，并写明 `kafka:retry:guard:*` 不拷；
  - 键族表 2026-10-17 登记了模式 HASH 与 guard 键。
- `docs/design/no-single-node-horizontal-scaling-20261001.md`：键表下 2026-10-17 追加了更正（「ready,所有实例共享(不变)」已过时）。
- `docs/design/global-data-layer-tidb-decision.md`：D5 修订块 2026-10-17 写入，第 1 条按修订稿 §8 定稿。
- `docs/design/db-task-kafka-partition-contract.md` 的离线扩容流程，以及 `go/db/etc/db.yaml` 的 RetryRouting 注释：`retry_requeue` 用法已带 `-zone`，并写了退出码。
- `docs/ops/release-checklist.md` §4：2026-10-17 补了「go/db 回滚到 S1(重试只交给分区属主)之前的版本」条目，含三项人工核对、退出码与 revision 注意事项，见 §10.3。
- `docs/ops/godb-pipeline-launch-todo.md`：§4.4 补了认领 / LREM 重发已修与 TTL 剩余风险；新增 §9（④ + WAITAOF 的前置、单测、端到端、演练、上线）。
- `deploy/k8s/db-task-retry-alerts.yaml`：
  - DbTaskDeferredByStore、DbTaskRetryQueueGrowing 的描述已改为「`kafka:retry:queue:{topic}` 与按分区的 `kafka:retry:queue:{topic}:p{N}`」；
  - DbRetryPartitionedStranded 的处置已改为 `-zone` 必填与退出码 0/1/3/4/5；
  - 新增 DbRetryStrandedReceipts、DbConsumerGroupHeartbeatErrors；
  - DbDurableEnqueueNotConfirmed 写明了死信副本的去重。
- `tools/scripts/k8s_deploy.ps1`：ConfigMap 镜像四个键、副本门禁、Redis 总账 `Fixed = 2`。
- `tools/scripts/stress_summarize.ps1` 的 2f 段：指标名与 label 取值和 metrics.go 一致，缺口见 §13.4。
- `docs/design/tidb-phase1-and-launch-readiness.md` 的清空核对写的是 `kafka:retry:*`，已经覆盖 p*，不需要改。
- `docs/design/server_merge_design.md`、`docs/design/db-task-pipeline-bottlenecks.md` §5：已提到 p* 与 SCAN。

**未改（交对应负责人；本批写手只改本文与修订稿）**

| 位置 | 过时内容 | 应改为 |
|---|---|---|
| `docs/design/db-task-store-unavailable-deferral.md` §9 待决表 | WAITAOF 与 ④ 两行（「未授权开工」「本批不改重试的归属」） | 已被本文取代（复核时该文件里还没有更正） |
| `docs/PROGRESS.md`、`docs/README.md`、Java 仓库 `PARITY.md` | — | 主会话统一登记 |

## 3. 两种模式的行为

每拍的公共前置（`consumeRetryQueue`，两种模式都做）：

1. `retryStrandedReceipts`：重搬待重搬表里的收据，见 §7.8。
2. `retryOwnership.resolveUnresolvedClaims`：处理结果未知的认领，见 §7.9。返回 false 时（Redis 仍出错）本拍到此为止，不分拣也不认领。
3. `sampleRetryQueues`：采样各列表长度，更新 `db_retry_queue_length`。任一 p 键读失败时，本拍不更新 `partitioned`。

| 行为 | `shared`（含零值） | `owner` |
|---|---|---|
| 生效条件 | 缺省 | `RetryRouting=owner` 且 `PartitionCnt > 0`（`ownerRouting`）；`PartitionCnt ≤ 0` 时构造期拒绝（`resolveConsumerRetryRouting`） |
| 分拣 | 不分拣 | 每拍先 `forwardSharedRetries`：上限 min(200, 拍开始时收件箱长度)，每次脚本最多 100 条（`retryForwardBatch`） |
| 本 zone 主库冷却（`homeStoreCoolingDown`） | 整拍跳过 | 照常分拣，但不认领 |
| 谁认领、从哪认领 | 任一实例；来源依次是收件箱与拍开始时非空的各 p{N} | 只对本进程存活的租约（`ownedPartitions.live`），从 p{P} 认领 |
| 每拍预算 | 总 200（`retryDrainBudgetPerTick`）；每个来源的额度 = 拍开始时它的长度（读失败时取 200） | 总 200；按租约轮询，起点按 `retryTick` 轮转；每个租约的额度 = 拍开始时 p{P} 长度 + 本拍分拣进 p{P} 的条数（`ownedRetryQuota`） |
| 派发给谁 | 原始分区的 worker（`parseClaimedRetry`） | 同左；原始分区 ≠ P 时按载荷挪回 p{origin}（`misrouted`），不执行 |
| 失败写回 | 共享收件箱 | p{origin}（`retryReadyKeyFor`） |
| claim 撤销 | 不感知（shared 重试不带 claimCtx，没有在途回执） | 派发前撤销、handleTask 入口复核、SQL 之前复核，三处任一发现撤销，都把原字节搬回 p{origin}（`requeued_revoked`），不执行，不耗预算 |
| 计数（`db_retry_route_total{mode,action}`） | `executed`（本进程持有该分区 claim）/ `executed_nonowner` / `dead` | `forwarded` / `executed` / `requeued_revoked` / `misrouted` / `dead` |
| 拿不到分区的实例 | 照常认领执行 | 热备：只分拣、回收孤儿；Setup 打 INFO `no partitions assigned (hot standby: …)`（shared 模式打 ERROR `no partitions assigned`） |
| 回滚路径 | 兜底排空 p*，从 owner 回滚就靠它 | — |
| 租约登记 / 模式 HASH | 都登记（`DbZoneNoPartitionClaims` 两种模式都适用） | 同左 |

**owner 模式不设 shared 兜底**（修订稿裁决第 5 条）。理由是进程内分不清两种情况：一是「我是热备、分区都在别人手里」，这是 owner 模式的正常状态；二是「全 zone 都没拿到分区」。如果加兜底，在最需要单一属主的交接窗口和僵尸窗口里，会重新出现非属主执行，还会破坏 S2「同一 topic 至多一个组在消费」的保证。代价与处置见 §6.2。

## 4. 不变量、收据位置与出口表

### 4.1 不变量

1. **属主资格 = 租约未关闭 且 claimCtx 未取消**（`claimLease.live`）。`closed` 只表示「不再接新认领」；handleTask 的两次复核看的是 `task.claimCtx`，判断属主是否被撤销。
2. **每条收据任一时刻恰在一处**：共享收件箱、某个 p{N}、某实例 processing、旧共享 processing、死信，五者之一。派发给 worker 的那份是在途任务，收据仍留在认领者的 processing 里。所有搬移都是单脚本原子操作。
3. **一个实例的 processing 只在它持有未过期租约的那一刻才会增长**：`retryClaimScript` 在同一个原子操作里先续租、再认领。
4. **owner 重试与同分区新消息同 worker、同子 shard 串行**：两者走同一个 `subShardIndex(spreadKey(Key))`。
5. **已经开始的重试 SQL，以及其后的 Redis 步骤，不随 claim 撤销取消**：`handleTask` 里重试的 `processCtx` 固定为 `w.ctx`，只有 Kafka 任务从 claimCtx 派生。代码注释写明今后改动也不得让重试改用 claimCtx。
6. **在途回执恰好交还一次**：`workerTask.finishRetry` 先取出、置 nil，再调用。
7. **先 enter 再认领；排空先 close 再等 idle**：保证排空开始之后不会再有新认领，排空开始之前的认领一定被等到。

### 4.2 owner 重试「认领之后」的每个出口

| 阶段 | 出口 | 收据去向 | 在途回执 |
|---|---|---|---|
| `consumeOneOwnedRetry` | `enter` 失败（租约已关或已撤销） | 没认领 | 不涉及 |
| | 认领为空，或认领出错 | 出错时记进 unresolved（§7.9） | `exit` |
| | `parseClaimedRetry` 判终态：畸形载荷、写任务次数用完、没有原始分区、分区为负或越界 | 原字节进死信（`moveClaimedRetryToDead`，失败登记待重搬）；读任务次数用完则 ack 丢弃（`abandonClaimedReadRetry`，读不进死信） | `exit` |
| | 原始分区 ≠ P | 原字节挪回 p{origin}（`misrouted`） | `exit` |
| | 派发时租约被撤销 | 原字节挪回 p{P}（`requeued_revoked`），RetryCount 不写回 | `finishRetry` |
| | 派发时 `c.ctx` 取消或撤拉取（`pullStop`） | 留在本实例 processing，由 Stop 第⑤步归还收件箱 | `finishRetry` |
| | 派发成功 | 随任务交给 worker，计 `executed` | 由下面接手 |
| `handleTask`（顶部 `defer task.finishRetry()`，最先注册、最后执行） | 入口复核发现撤销 | `requeueRevokedRetry` → p{origin} | defer |
| | 选库之后、SQL 之前复核发现撤销 | `requeueRevokedRetry` → p{origin}，排序锁由既有 defer 释放 | defer |
| | 游标判 stale、older_seq | `ackRetryReceipt` | defer |
| | 成功 | `advanceApplied` 之后 `ackRetryReceipt` | defer |
| | 失败，或 panic 恢复 | `deferFailedTask` 把 RetryCount 已更新的载荷写回 p{origin}；免预算原因先把 RetryCount 减回 | defer |
| | 隔离 | `deadLetterRetry` → 死信 | defer |
| `processTaskBatch` | 被合并（superseded） | `ackRetryReceipt` | `finishRetry` |
| `routeToSubShard` | 停机时子 shard 已关 | 留在 processing | `finishRetry` |
| `worker.start` 退出 | 通道里没做的 | 留在 processing（`discardQueuedRetries`） | `finishRetry` |

所有「搬走」出口遇到 Redis 出错、且本消费者 / worker 的 ctx 没取消时，登记进待重搬表（§7.8）。

## 5. claim 租约与在途计数

- **登记位置**：`ConsumeClaim` 里 claimCtx **最终派生完成之后**、主 for 循环之前调 `owned.acquire(partition, claimCtx)`，`defer owned.release(lease)`。此时 S2 的 `sessionScope`（交接闸的 yieldCtx）、S4 的 `WithCancel(WithoutCancel(parentCtx))` 和观察 goroutine 都已就位。观察 goroutine 只在没有撤拉取时随父 ctx 取消 claimCtx。
- **为什么不用 Setup / Cleanup 判属主**：Setup 早于 ConsumeClaim 开始，Cleanup 晚于排空结束。sarama eager 再平衡下，新属主要等全组退出上一代 claim 才拿到分区，所以「本进程此刻拥有哪些分区」只能看每次 ConsumeClaim 调用自身的生命周期。
- **为什么不用 `sync.WaitGroup`**：enter（相当于 Add）会与排空的等待并发，而 WaitGroup 不允许计数为 0 时 Add 与 Wait 并发。这里用的是互斥锁 + 计数 + 归零 channel（`claimLease.idleCh`）：inflight 从 0 变 1 时换新 channel，降到 0 时关闭；close 之后 enter 一律失败。
- **release 只摘自己这一代**：表里登记的已经是同分区的新一代时不误删。每次 acquire / release 都更新 `db_owned_partitions`（存活租约数）。acquire（非 nil 接收者）每次计一次 `db_claim_acquired_total`。
- **nil 接收者安全**：`ownedPartitions` 与 `claimLease` 的全部方法对 nil 接收者都安全，KOC 的调用点一律不判空。`acquire` 返回一个未登记的租约；`enter` 返回 false；`idle` 返回一个永远关闭的 channel（`closedSignal`）；`revoked` 返回 nil channel；其余为空操作。
- **排空等的是什么**：`claimDrained(acker, lease)` 先等 acker 的在途 Kafka 任务（`acker.pending`，含异步入队还没回调的那些），再等租约 inflight 归零。各路径的时限与是否提交位点见 §7.4。

## 6. 混跑、对 claim 的依赖与双属主窗口

### 6.1 混跑矩阵

| 组合 | 结论 | 说明 |
|---|---|---|
| S1 之前的镜像 + S1 shared | 安全 | 新版只写共享收件箱，兜底排空的 p* 为空 |
| S1 shared + S1 owner | 安全 | shared 实例兜底排空 p*，执行仍在排序锁、游标、epoch 守卫之下；shared 实例计 `executed_nonowner`，DbRetryNonOwnerExecutionInOwnerZone 在切换滚动中途会响，属预期 |
| S1 之前的镜像 + S1 owner | **禁止** | 旧版看不到 p*；owner 实例全部退出后 p* 滞留（DbRetryPartitionedStranded） |
| owner → S1 之前的镜像 | **禁止直接做** | 先切 shared 并等 p* = 0（§10.3）；紧急时回滚后立刻跑 `retry_requeue`（§10.5） |
| 切 owner 的前置 | — | 该 zone 每个 Pod 都暴露 `db_retry_routing_mode`（即都是 S1 版）；建议排在该 zone 完成 S2 切组之后，否则任一 zone 的 Pod 进出都会触发全组 eager 再平衡，owner 模式会出现 `requeued_revoked` 尖峰，重试排空暂停 |
| **owner 模式：重试排空依赖 claim 存活** | 已知、可接受的停滞 | 见 §6.2 |
| **owner 模式：MySQL 恢复期 p* 只由属主排空** | 已知残余（原规格风险第 6 条） | 每个实例都为全部分区建好 worker（`NewKeyOrderedKafkaConsumer` 按 `PartitionCnt` 逐个 `buildWorker`）。shared 模式下任一实例都能认领并执行任一分区的重试；owner 模式下 p{N} 只由持有该分区 claim 的属主执行，并且与该分区的新消息在同一子 shard 上串行（§4.1 第 4 条），热备不参与执行（§8）。所以本 zone 主库冷却解除、MySQL 恢复之后，积压重试的排空并发低于 shared，p* 回落更慢。验收见 §10.2 第 1 步；必要时按 B0 调 `ServerConfig.Kafka.SubShardCount`（本批不做，见文首「范围」） |

**取值只在启动时读**。改了 ConfigMap 的 RetryRouting 却没有 `rollout restart`，只有重启过的 Pod 读到新值，同样会混跑。

### 6.2 owner 模式对 claim 的依赖与停滞处置

**停滞条件**：owner 模式下没有 claim 就没有租约，`consumeRetryQueueOwner` 只分拣、不认领。下列情形里 p* 只进不出，停滞时长等于 Kafka 消费的空窗：

- Kafka 不可用；
- 长时间再平衡；
- S2 交接闸等待期间（被闸住的进程根本不入组）；
- 本 zone 全部成员被逐出。

数据都在 Redis 里，不丢。

**主库冷却不算这一类**：本 zone 主库冷却（`homeStoreCoolingDown`）时两种模式都整拍不认领，那是 L-04 的既有语义。

**处置**（同 DbZoneNoPartitionClaims 的描述）：

1. 先修 Kafka 或交接闸：查 broker 是否可达、本 zone 消费组的 describe、go/db 日志里的 `group handoff waiting`，同时看 DbConsumerGroupHandoffWaiting / DbConsumerGroupHandoffStuck。
2. 交接闸卡住、broker 可达、MySQL 健康时，可以切到 shared，由 shared 实例排空 p*：
   ```
   kubectl -n mmorpg-zone-<z> set env deploy/db DB_RETRY_ROUTING=shared
   kubectl -n mmorpg-zone-<z> rollout status deploy/db
   ```
   `set env` 会改 Pod 模板，天然触发滚动。恢复后去掉覆盖：`kubectl -n mmorpg-zone-<z> set env deploy/db DB_RETRY_ROUTING-`（同样触发滚动）。`kubectl set env` 加的变量不在 last-applied 里，`k8s_deploy.ps1` 的 apply 不会删它，而且环境变量优先于配置键，所以必须显式去掉。
3. **broker 不可达时不要这么做**：新 Pod 在 `NewKeyOrderedKafkaConsumer` 的 `sarama.NewConsumerGroup` 处起不来，滚动会卡住。
4. owner 模式下做 S2 反向交接时允许这段停滞，不要求先切 shared。

### 6.3 双属主窗口（R7）

「claim 活着 ⇒ 本进程是该分区此刻唯一的执行者」只在 coordinator 没有逐出本成员时成立（`retry_routing.go` 文件头）。

- **成因**：本成员到 coordinator 的连接半开，或 coordinator 卡顿。coordinator 在 `Session.Timeout`（`SessionTimeoutMs`，缺省 10000，即 `DefaultKafkaSessionTimeoutMs`）后逐出本成员、完成再平衡，新属主拿到分区并开始执行。本进程这边的心跳要等满 sarama 的 `Net.ReadTimeout` 才失败，之后每次重试同样受它约束、最多 `Metadata.Retry.Max + 1` 次，然后才取消会话。`NewKeyOrderedKafkaConsumer` 没有覆盖这两项，取 sarama v1.43.1 的缺省值（`Net.ReadTimeout` 30s；`Metadata.Retry.Max` 按 sarama 缺省 3 估算）。
- **窗口长度**：从被逐出到本进程察觉，约 `Net.ReadTimeout`（30s，旧连接半开、重连正常时）到 `(Metadata.Retry.Max + 1) × Net.ReadTimeout`（约 120s，到 coordinator 持续分区时）。窗口内本进程的 claimCtx 与租约仍然存活，继续从 p{P} 认领执行，与新属主并发。
- **同类重叠**：再平衡排空超时（`drainAfterStop` 等满 `claimDrainTimeout` 10s）之后仍在跑的重试，也会与新属主重叠。
- **正确性**：只靠排序锁与 applied 游标兜底，与 shared 模式相同。每条收据经 RPOPLPUSH 只会被一个实例认领，所以重叠的是「同分区不同任务」与新属主的新消息，同 key 由排序锁串行。
- **可见性**：两边的执行都计 `executed`，DbRetryNonOwnerExecutionInOwnerZone 看不见这种重叠。窗口靠下面三样定位（`consumer_group_errors.go`）：
  - `db_consumer_group_errors_total{source="heartbeat"}`，以及按它设的 info 告警 DbConsumerGroupHeartbeatErrors（§11）；
  - 限速 ERROR `kafka consumer group error: source=… groupID=… topic=… suppressed_since_last=… err=…`，每个来源每 10s 至多一行（`storeLogEvery`）。
- **错误来源判定**（`consumerGroupErrorSource`）：
  - 不是 `*sarama.ConsumerError` → `heartbeat`（组级错误：心跳、找 coordinator）；
  - 链上有 `claimHandlerError` → `claim`（本进程 ConsumeClaim 返回的错误，`ConsumeClaim` 在 defer 里包一层）；
  - 其余 → `partition`（位点提交与拉取，两者在 sarama 的错误通道上分不开，所以不叫 `offset_commit`）。
- **为什么不调小 `Net.ReadTimeout`**：sarama 读 JoinGroup 响应也用它，长再平衡时会超时。
- **彻底封死**：要靠 MySQL 条件写（§12.3）。

## 7. WAITAOF：组提交 + 异步确认

### 7.1 适用范围

只有三类入队点：此后 Kafka 位点就会提交，Redis 里这一份是任务唯一的副本。

1. `deferFailedTask` 的 Kafka 分支：Kafka 源任务失败，副本写进重试 ready（owner 模式写 p{origin}）。
2. `quarantineOrderingConflict` 的 Kafka 分支：Kafka 源任务隔离进死信，覆盖 orderingIncomparable、stale_topic、missing_required 写，以及围栏的 stale_owner / owner_epoch_regressed / legacy_after_seq。
3. 毒消息 `persistPoisonMessage`：ConsumeClaim 分发前解码失败，以及 handleTask / processTaskBatch 解码失败（`quarantinePoisonTask`）。

**重试源的搬移不走入队层**：`moveRetryReceipt`、`deadLetterRetry`、`ackRetryReceipt`、分拣、搬回、回收都不走。它们是单脚本原子搬移，前一状态本身就在 Redis 里；丢了只回到前一状态，不是「唯一副本首次落地」。

### 7.2 接口

```go
type durableEnqueue interface {
	submit(ctx context.Context, target, queueKey string, payload []byte, done func(error))
	start(ctx context.Context) error
	close()
}
```

**submit 的契约**：

- done 恰好调用一次。nil 表示副本已按配置确认（关闭 WAITAOF 时就是 LPUSH 成功）；非 nil 表示未确认，调用方不得 ack；`errors.Is(err, errDurableEnqueueCanceled)` 表示停机或 ctx 取消。
- done 可能在 submit 返回前同步执行，也可能在写协程里执行。它必须快、不得阻塞、不得再调 submit。
- ctx 只约束「交给入队层」这一步（含背压等待），不约束确认过程。

**三种实现**：

- `directEnqueue`：两项都写 0 时用；`c.durable == nil`（直接拼结构体的单测）时也用它。同步 LPUSH，退避 0 / 100ms / 500ms / 1s，共 4 次，done 在 submit 返回前调用。
- `aofGroupEnqueue`：开启 WAITAOF 时用，组提交。
- `confirmingEnqueue`：R7 加在上面两者外面的一层，任一请求确认成功时清零重新入组退避的连续次数（§7.10）。

**调用方**：

- `persistBeforeAck` / `submitBeforeAck`：task 有 claimAcker 时把 onDone 直接交给 submit，即异步确认。`acker == nil` 时（只有直接拼结构体的单测）用容量为 1 的 channel 同步等结果。原因是 sarama 的 MarkMessage 是高水位语义，异步乱序 ack 会越过未确认的 offset。
- onDone 收到 canceled 时打 INFO `shutdown: … offset left uncommitted for replay`，其它错误打 `DATA-LOSS PREVENTED`；两种情况都 `failKafkaTask`（或 `acker.fail`），位点不提交。
- 死信计数（`db_task_dead_letter_total`）放在确认之后。

**错误哨兵**：`errEnqueueNotDurable`、`errWaitAOFRejected`、`errUnexpectedWaitAOFReply`、`errDurableEnqueueCanceled`（及其子类 `errDurableEnqueueClosed`）、`errDurableEnqueueNotStarted`。

### 7.3 组提交（`aofGroupEnqueue`）

- **攒批**：写协程 `run` 取到第一条请求后非阻塞地继续取，满足任一条件就停：条数达到 256（`durableEnqueueMaxBatch`）；累计字节 ≥ 8MiB（`durableEnqueueMaxBatchBytes`）；通道暂空。取下一条之前先判字节，所以一批最多超出上限一条；单条超过上限的载荷单独成一批。
- **粘连连接**：`flush` 在写协程长期持有的一条 `rc.Conn()` 上发。WAITAOF 等的是「本连接」的写偏移，换了连接就覆盖不到。
- **flush 规则**：单批预算 5s（`durableEnqueueBudget`），在截止前按 0 / 100ms / 500ms / 1s 退避循环：
  - 每轮 WAITAOF 的 timeout = min(`WaitAOFTimeoutMs`, 截止前剩余 − 100ms)（`waitAOFBudgetMs`，先截断成整毫秒再判断）。不足 1ms 不发（0 表示无限阻塞），剩余请求以 `errEnqueueNotDurable` 完成。**预算耗尽一律是 not_durable，不会被判成 canceled**。
  - needPush 为真时，同一个 pipeline 里先逐条 LPUSH，最后跟 `Do("WAITAOF", numLocal, numReplicas, waitMs)`；为假时只发 WAITAOF。
  - LPUSH 收到 Redis 错误回复时只让那一条失败；出现网络错误时整条 pipeline 结果未知，作废连接、重建，未完成的请求全部重推（可能重复）。
  - WAITAOF 错误回复 → `errWaitAOFRejected`，不重试；回复形状不对 → `errUnexpectedWaitAOFReply`，不重试；网络错误 → 重建连接并重推（新连接的写偏移覆盖不到旧写）；**份数不足 → 同一连接只重发 WAITAOF**，写偏移不变，不产生重复 LPUSH；份数满足 → 全部成功。
  - 内部 ctx 取消（close）时，剩余请求以 canceled 完成。在途 pipeline 不随 ctx 中断，最多等专用客户端的 ReadTimeout。
- **生命周期**：
  - 入队层的内部 ctx **不从 `c.ctx` 派生**，只由 close 取消。
  - close 的顺序是：置 stopped → cancel 内部 ctx → `submitters.Wait()` → `close(reqs)` → 等写协程退出 → 关粘连连接与客户端。这个顺序保证 done 恰好一次：请求要么从未进入 reqs，由 submit 自己的失败分支完成；要么被写协程读到，由写协程完成。
  - `Stop` 的**第⑥步（最后一步）**才 `c.enqueuer().close()`。
- **启动探针**：`Start` 第一件事是 `c.enqueuer().start(c.ctx)`。`probeAOFAck` 用专用客户端的池连接（不是粘连连接）发一次 WAITAOF，超时为 `WaitAOFTimeoutMs + 2s`。报错或份数不足即返回 `retry enqueue durability: …`，进程拒启。appendonly no 回 ERR，Redis < 7.2 回 unknown command，NumReplicas 配多了则份数不足。

### 7.4 位点语义（以代码为准）

**基本规则**（与修订稿一致）：

- claimAcker 只对连续完成的前缀调 MarkMessage。未确认的 offset X 把本分区已提交位点停在 X 之前；X 之后的消息照常执行，完成状态存进 `acker.completed`；X 确认后前缀一次推进。
- X 未确认时调 `failKafkaTask`：`acker.fail` 置 failed、清空 completed、取消 claimCtx。本 claim 不再标记任何位点，ConsumeClaim 收到 `failureCh` 后走 `drainAfterStop("offset durability failure")` 并返回错误；再平衡后由新属主从 X 起重投。
- 确认到达的等待上界 = 排队时间 + 单批预算 5s。

**claim 收尾时，等确认的 offset 是否提交，按路径分**：

| 收尾路径（`ConsumeClaim`） | 触发 | 顺序 | 等待期间到达的确认 | 位点 |
|---|---|---|---|---|
| `drainClaimForShutdown` | 停机撤拉取（`pullStop`），含撤拉取后会话被兄弟 claim 取消 | `lease.close()` → 等 `claimDrained`（上限 `shutdownDrainTimeout`，缺省 `claimDrainTimeout` 10s），同时看 `consumerDone`；超时或 `consumerDone` 时才 `acker.stop()` | **标记** | 会话释放时提交 |
| 消息通道关闭、会话仍在 | sarama 分区消费者自行关闭（例如 `ErrOffsetOutOfRange`） | `lease.close()` → 等 `claimDrained`（上限 `claimDrainLimit`）→ 返回 nil；超时转 `drainAfterStop("messages closed with pending work")` 并返回错误 | **标记** | 会话释放时提交 |
| `drainAfterStop` | 失败（`offset durability failure`、`failure before dispatch`）；再平衡（`session canceled`、`claim canceled before dispatch`）；交接让出（`handoff peer reclaimed partition`）；`worker canceled before dispatch`；以及上一行等待期间的同类事件（`… while draining`） | `acker.stop()` → `lease.close()` → `waitClaimDrained`（上限 `claimDrainLimit`） | **不标记**：`acker.stop` 已置 failed、清空 completed，之后 `ack` 只摘 tracked | 不提交；等确认的 offset 与其后已完成的 offset 一律由新属主重投 |

**与修订稿的差异**：修订稿裁决第 1 条与 §1.6 写的是「正常再平衡和停机排空都等得到确认再提交」。代码只在停机排空与「消息通道关闭」两条路径上等确认再提交；再平衡、交接让出、失败收尾不提交，等到的确认也不标记。R7 复核维持这一实现（`drainAfterStop` 注释、`durable_enqueue.go` 文件头「claim 收尾时」一节）。单测 `TestClaimAcker_StopDropsLateConfirmations` 在 claimAcker 层面钉住了这条语义。要让再平衡也「等确认再提交」，得在 session canceled 与交接让出这两个非失败原因下改成先 `waitClaimDrained`、再 `acker.stop`。这会改变「再平衡立即撤 claimCtx」的既有语义，须另立设计。

**作废范围是本实例的全部 claim**：sarama v1.43.1 任一 ConsumeClaim 返回都会取消整个会话，所以一个分区确认失败让 claim 返回 error 后，本实例其余 claim 都会走「session canceled → drainAfterStop」。

**重放放大与重复副本的量级**（按设计参数推算，未实测）：

- 与同步 LPUSH 相比，每个等确认的 offset 的未标记窗口从毫秒级拉长到约一次 fsync（everysec 平均约 0.5s，最坏约 1s，上限是单批预算 5s）。
- 稳态下入队很少，再平衡时通常没有等确认的 offset，基本没有额外重投。
- MySQL 故障期间每条写都走入队，每个分区几乎随时都有等确认的 offset。按设计峰值 2000 task/s/zone、10 个分区估算，每次再平衡每分区多重投约 100～200 条，全 zone 约 1000～2000 条；等满单批预算时每分区最多约 1000 条。这些被重投的写在 MySQL 恢复之前会再失败、再入队一份，所以重复的重试副本也是同一量级。
- 恢复后重复副本由游标与 seq 吸收（判 stale 丢弃），每份多花一次认领、一次游标判定和排序锁往返。数据不丢。

**内存上界**：`acker.completed` 约为「吞吐 ×（5s + 排队）」；入队层见 §7.5。

### 7.5 吞吐与内存上限

- **吞吐**：每次 fsync 一批，每批 ≤ 256 条且 ≤ 8MiB，单实例最坏约 256 条/s。超出时 submit 背压，阻塞 worker：待确认总字节 ≤ 32MiB（`durableEnqueueMaxPendingBytes`），请求通道 ≤ 1024 条（`durableEnqueueQueueSize`）；`pendingBytes == 0` 时任何单条都放行。效果等同同步写法，表现为 Kafka lag，不丢数据。
- **为什么不同步等**：设计峰值 80 个子 shard、每条约 35ms、利用率约 87%。同步 WAITAOF 会让失败任务占住子 shard 约 0.5～1s（正常任务的 14～28 倍），峰值时 1% 失败就吃掉剩余余量（修订稿 §1.1）。

### 7.6 重复副本的全部来源

修订稿 §8 写的是「只剩网络错误与 Kafka 重投」，代码的实际来源更多：

1. 组提交遇到网络错误时，在新连接上重推 LPUSH（§7.3）。份数不足只重发 WAITAOF，不在此列。
2. Kafka 重投：确认失败或进程崩溃后，从最早未确认的 offset 起重投，失败的任务再入队一份。
3. 再平衡 / 交接让出时等到的确认不标记（§7.4）：副本已经落地，位点却没提交，新属主重投时若再失败还会再入队一份。
4. 落盘确认持续失败时的重新入组循环：队头是一条确定性进死信的消息时，每个周期往死信多推一份字节相同的副本（§7.10）。R7 加了退避，但没有硬上界。
5. 主池 go-redis 重发 `moveRetryReceiptScript`：重发可能搬走 processing 里另一份字节相同的副本，目的地多一份相同载荷，只会多出重复执行（`moveRetryReceipt` 注释）。

执行侧收敛靠三样：seq 游标（判 stale 丢弃并 ack 收据）、批内同 seq 合并（superseded）、owner epoch 只升不降。死信里的重复要人工按 taskID 或 (topic, partition, offset) 去重。

### 7.7 WAITAOF 不覆盖什么

- WAITAOF 只保证：Kafka 位点提交之前，唯一副本已按配置落盘。
- 它**不覆盖**下面这些写。它们都是主池上的普通写，everysec 下 Redis 崩溃最多丢约 1 秒：
  - 游标 `consumer:applied:*` 与 owner epoch `consumer:applied_epoch:*`（`advanceApplied`）；
  - 收据 ack（`ackRetryReceiptScript` 的 LREM）；
  - 重试源搬移（`moveRetryReceipt`）、分拣、回收、搬回。
- **AOF 尾巴丢失的后果**：游标可能回退，已 ack 的收据可能重新出现在 processing。而 MySQL 与 Kafka 位点都已前进，旧 seq 收据的重放会把 MySQL 覆盖回旧档。组提交的网络重推与 Kafka 重投会制造更多同 seq 副本，让这个窗口更容易撞上。
- **这一残余只能由 MySQL 条件写封死**（`db-write-fencing-mysql.md`，§12.3）。
- **幂等的前提**：「重放由游标判 stale 吸收」只在**游标没有随 Redis 一起回退**时成立（`durable_enqueue.go` 文件头、`handleTask` 成功分支之后 ack 收据处的注释）。
- **措辞纪律**：本文与代码注释都不得表述为「Redis 崩溃安全」。

### 7.8 待重搬收据表（R7，`retry_stranded.go`）

- **问题**：收据被认领进本实例 processing 后，出口都是单脚本原子搬移。搬移因 Redis 出错没做成时，收据留在本实例 processing。本实例一直续租，`reclaimOnce` 又跳过自己，以前要等重启或停机 release 才回到收件箱。db 的存活探针只看 gRPC 端口，Redis 故障不会让 Pod 重启；而 Redis 故障恰好会让 claim 失败、触发再平衡，队列里的 owner 重试成批走撤销搬回。owner 重试的 Kafka 位点早已提交，收据就是那笔存盘唯一的副本。
- **登记点**：`requeueRevokedRetry`、`moveClaimedRetry`（misrouted / 派发前撤销）、`moveClaimedRetryToDead`、`deferFailedTask` 的重试分支，以及同类的 `deadLetterRetry`。
- **登记条件**：只有「搬移返回 error 且本消费者 / worker 的 ctx 没取消」才登记。`moved = false` 且 `err == nil` 不登记，那多半是 go-redis 重发 EVAL 时首发其实已经执行了。
- **重搬**：重试协程每拍第一步调 `retryStrandedReceipts`，每拍最多 100 条（`strandedRetryPerTick`）：
  - 搬成：摘掉，并补记原出口成功时的计数（`onMoved`）。例如进死信的那次计数改在真正搬成时记，`moveClaimedRetryToDead` 因此多了一个 onMoved 参数。
  - `moved = false`：摘掉。
  - 出错：留到下一拍，本拍不再继续。
- **停机与崩溃**：Stop 第⑤步 release 把整张 processing 还回收件箱，失败写回的 RetryCount 更新随之丢失，这一次尝试不计。进程崩溃时表随之丢失，靠租约过期后别的实例回收兜底。
- **观测**：`db_retry_stranded_receipts`（无 label 的 gauge，每个 Pod 一份）是表长。持续 > 0 说明 Redis 一直拒绝这次搬移，例如目的键类型不对。告警 DbRetryStrandedReceipts（§11）持续 10 分钟才报。
- **处置**：
  1. 先按 go/db 日志 `stranded retry receipt still cannot be moved` 里的 dest / err 查原因并修好；
  2. 仍不归零时重启该 Pod（`kubectl -n mmorpg-zone-<z> delete pod <pod>`，或 `rollout restart deploy/db` 整体滚动），由停机 release 把本实例 processing 整表还回收件箱（日志 `returned … unfinished retry receipts to the ready queue on shutdown`）；
  3. release 失败或进程被强杀时，等租约过期由别的实例回收。
  - 不要清空 Redis，不要手工删 processing：owner 重试的位点早已提交，收据是唯一副本。

### 7.9 认领与 ack 的 nonce 防重（R7）

- **问题**：主池客户端没设 `MaxRetries`，取 go-redis 缺省 3；读超时（缺省 3s）或 EOF 时会把 EVAL 原样重发，而首发可能已经在服务端执行完（只是回包丢了）。
  - 认领脚本不幂等时，两次 RPOPLPUSH 会把第一次认领到的收据留在本实例 processing 里无人派发。
  - LREM 重发会把另一份字节相同的副本也删掉；如果保留的那份随后失败，`deferFailedTask` 找不到任何一份收据，这笔存盘就丢了。
- **认领**（`claimFrom` / `retryClaimScript`）：
  - 每次调用带一个新 nonce（`newRetryGuardNonce`，16 个十六进制字符，取自 crypto/rand；不用进程内计数器，因为同名实例重启后计数器会撞上未过期的记录）。
  - 首次执行认领到元素时，把「长度:SHA1」写进 `kafka:retry:guard:claim:{processingKey}:{nonce}`，PX = `retryGuardTTL`（5 分钟）。
  - 同一 nonce 再执行时不再 POP，只在 processing 里按记录找回那个元素返回；记录为 `resolved` 时返回 nil。
- **结果未知的认领**（EVAL 返回 `redis.Nil` 以外的错误）：
  - 调用方把它记进 `unresolved`。下一拍认领之前，`resolveUnresolvedClaims` 用 `retryClaimResolveScript` 把可能已认领的那一条原字节 RPUSH 回原 ready 的队尾（认领从队尾取，下一次就轮到它），然后 LREM，再把记录改写成 `resolved`，迟到执行的旧副本也不会再 POP。
  - resolve 遇到网络错误：本拍停止分拣与认领，出错的那条与其后的留到下一拍。
  - resolve 遇到 Redis 错误回复（确定性拒绝）：只打 ERROR，放弃这一条，不挡后面的认领；那条收据（如果那次认领真的执行了）留到停机 release。
- **ack**（`ackRetryReceiptScript`）：
  - 记录键 `kafka:retry:guard:ack:{processingKey}:{nonce}` 不存在时，执行 LREM 1 并把删掉的条数记进去；已存在时直接返回记下的条数，不再 LREM。
  - 删掉的条数不是 1 时，`ackRetryReceipt` 返回错误 `retry receipt not found in processing queue`。
- **不加防重的脚本**：搬移、分拣、回收脚本重发最多多出重复执行，不会丢收据。
- **记录量**：单条约 150 字节（连键名）。最坏每实例约「每拍 200 次认领 + 同量 ack」× 300s，满速排空 5 分钟约 12 万条、十几 MB；只有重试链路有流量时才产生。
- **剩余风险**：Redis 卡顿或不可达超过 5 分钟（记录 TTL）时，记录过期，旧连接里迟到执行的副本可能再 POP 一次（第一次认领到的那条照旧滞留到重启），或再删一次 LREM。

### 7.10 重新入组退避（R7，`rejoin_backoff.go`）

- **问题**：WAITAOF 持续无法确认时，LPUSH 早已生效，未确认只让本 claim 失败。典型情形有三：redis-ha 下 `WaitAOFNumReplicas=1` 而副本在维护；AOF 盘 fsync 超过单批预算；运行期被 `CONFIG SET appendonly no`。
  - 如果分区队头是一条确定性进死信的消息（毒消息、orderingIncomparable、stale_topic、missing_required），每个周期就是：LPUSH 进死信 → 预算耗尽或被拒 → `acker.fail` → ConsumeClaim 返回 error → sarama 取消整个会话 → Consume 返回 nil、立即重新入组 → 从同一 offset 重投。
  - 每周期多推一份字节相同的副本，约 5～10s 一份，死信没有消费者，副本数随事故时长线性增长。本实例其余 claim 每周期也一起作废、一起重投。
- **做法**：
  - `ConsumeClaim` 的 defer 判 `isDurabilityFailure(retErr)`（`errEnqueueNotDurable` 或 `errWaitAOFRejected`；canceled 不算），是则 `durableBackoff.claimFailed()`。
  - 消费循环每次 Consume 返回后调 `waitRejoinBackoff`：本轮有这样的退出就在重新入组之前等 `rejoinBackoffFor(连续次数)`，1s 起、翻倍、上限 30s，打 ERROR `consumer group rejoin delayed: a claim failed because an enqueue was not confirmed durable …`。
  - 任一入队请求确认成功（`confirmingEnqueue`）就把连续次数清零；距上一次计入超过 5 分钟（`rejoinBackoffResetAfter`）也从头算。
  - 撤拉取或 `c.ctx` 取消立即打断等待。
- **效果与未做的部分**：副本增速从约 5～10s 一份降到最多约 35s 一份，但**没有硬上界**。R7 的第 2) 步没做：在死信入队处按 (topic, partition, offset) 去重。它要改入队层接口和组提交 pipeline，还要在「LPOS 复核」与「L-04 第 2 批死信工具同步删标记」之间二选一，须负责人与死信工具线一起定。
- **退避期间的分区移交**：本成员不在会话里，超过 `Session.Timeout` 时 coordinator 会把分区交给别的成员；别的成员遇到同一条队头会同样退避。

### 7.11 专用入队客户端与连接账

- **参数**（`durableClientOptions`）：
  - Addr / Password / DB 同 `ServerConfig.RedisClient`；
  - `DisableIndentity: true`；
  - `PoolSize: 2`（`durableClientPoolSize`）：一条给写协程的粘连连接，一条给启动探针；
  - `MaxRetries: -1`：关掉 go-redis 对整条 pipeline 的自动重发，重试由入队层在预算内自己做；
  - `ReadTimeout = WaitAOFTimeoutMs + 1s`，保证 WAITAOF 的服务端超时先于客户端读超时到期；
  - `WriteTimeout = 3s`。
- **为什么不进主池**：WAITAOF 在 everysec 下平均占连接约 0.5s。主池（`db.yaml` 的 `PoolSize: 20`）还要承载排序锁、游标、落点 MGET，放进主池会在 MySQL 故障（每条写都走入队）时饿死热路径。`durable_enqueue.go` 文件头仍写「主池只有 10 条连接(GOMAXPROCS=1 × 10)」，那是显式写出 `PoolSize` 之前的数字，理由本身不变，见 §15。
- **连接总账**：`k8s_deploy.ps1` 的 Redis 连接总账里 db 行 `Fixed = 2`，按开启 WAITAOF 计，偏保守；两项都写 0 时不建专用客户端。
- **已知观测缺口**：`db_redis_pool_*{redis_client}` 只导出 `main` 与 `placement`（`RegisterRedisPools`），不含专用池。专用池的取连接等待与超时看不见，只能间接看 `db_durable_enqueue_seconds` 与 `db_durable_enqueue_total{result="not_durable"}`。

### 7.12 配置与默认值

| 键 | 类型 | 缺省 | 合法范围 | 说明 |
|---|---|---|---|---|
| `ServerConfig.Kafka.RetryRouting` | string | 空串 = `shared` | `shared` \| `owner`（大小写敏感） | 环境变量 `DB_RETRY_ROUTING` 去掉首尾空白后非空时优先，只用于单 zone 金丝雀；非法值拒启（`ResolveRetryRouting`，日志 `retry routing rejected: …`）；空串不在 Normalize 里回填，好让启动日志区分 `default` 与 `config` |
| `ServerConfig.RedisClient.WaitAOFNumLocal` | *int | nil = 1 | 0 \| 1 | |
| `ServerConfig.RedisClient.WaitAOFNumReplicas` | *int | nil = 0 | [0, 16] | db.yaml 保持 0，给本机 compose 单实例用（写 1 本机 go/db 的启动探针等不到副本而拒启）；K8s 上 db ConfigMap 的这一项不镜像 db.yaml，由 `k8s_deploy.ps1` 按共享 Redis 形态渲染：单实例 0、redis-ha 1（见本节末条） |
| `ServerConfig.RedisClient.WaitAOFTimeoutMs` | int | 0 / 不写 = 1500 | [1, 2000]，负数拒启 | 默认不取 1000：everysec 下本地确认最长约 1s，再加事件循环延迟，1000 正压在边界上 |

- **份数用 *int 的原因**：默认值 1 不是零值，而「显式写 0」又有合法语义。两项都写 0 = 关闭确认：退回 `directEnqueue`，启动打一行 ERROR `retry enqueue WAITAOF disabled …`。只给确实没开 AOF 的环境用。
- **校验**：`(*Config).Validate` 在 `db.go` 的 Normalize 与 ResolveRetryRouting 之后调用，错误文本英文并点名键。
- **启动日志**：`retry routing: mode=<shared|owner> source=<default|config|env DB_RETRY_ROUTING>`；横幅一行 `retry: routing=… (…) waitaof=<local>/<replicas>/<ms>ms`；开启时另有 `retry enqueue WAITAOF enabled: numlocal=… numreplicas=… timeout=… batchBudget=…`。
- **Redis 要求**：≥ 7.2 且 `appendonly yes`。K8s `manifests/infra/redis.yaml` 以 `--appendonly yes` 启动（高可用形态 `redis-ha.yaml` 的每个 redis 节点同样开 AOF，镜像 `redis:7.2`）；`deploy/docker-compose.yml` 的 redis 服务也开了 AOF。本机没开 AOF 的开发环境默认会拒启，需要显式写 0/0。appendfsync 不是 everysec 的部署（例如 `no`），WAITAOF 的本地确认行为没有核实，探针会拦。
- **K8s 上改值必须 rollout restart**：ConfigMap 由 `k8s_deploy.ps1` 原样镜像 `go/db/etc/db.yaml` 的 `RetryRouting`、`WaitAOFNumLocal`、`WaitAOFTimeoutMs` 三个键；`WaitAOFNumReplicas` 例外，按共享 Redis 形态渲染（`Get-RouteStoreWaitAOFNumReplicas`，与 data-service 的 `RouteStore.WaitAOFNumReplicas` 同一个函数：单实例 0、redis-ha 1；形态取 `-RedisReplicas` 与档位，zone 写路径没给参数时跟随线上 `statefulset/redis` 副本数）。db.yaml 的那一项保持 0 给本机 compose 单实例。四个键在 db.yaml 里仍必须显式写出，`Get-AuthoritativeScalar` 查不到即 throw。只改 ConfigMap 不会滚动 db。（渲染规则 2026-10-19 落码，未运行，待 Codex 验证。）

### 7.13 计数口径

- **result 判定顺序**（`durableEnqueueResult`）：先判 canceled，其次 not_durable，其余非 nil 为 error。canceled 不进 `db_durable_enqueue_seconds`。
- **同一种网络故障在两条路径上计成不同的 result**：组提交路径上网络错误重试到预算耗尽计 `not_durable`；关闭 WAITAOF 时 `directEnqueue` 的 4 次 LPUSH 全部失败计 `error`。
- **滚动重启时 canceled 会增长**，告警只看 `not_durable|error`。

## 8. 副本数门禁与热备

- **门禁**：`Assert-DbReplicasWithinPartitionContract`（`tools/scripts/k8s_deploy.ps1`）在写路径入口（zone-up / infra-up / all-up）调用，先于任何集群写操作，不看本次是否部署 db。它静态比较 `deploy/k8s/manifests/go-svc/db.yaml` 的 Deployment `replicas` 与 `go/db/etc/db.yaml` 的 `ServerConfig.Kafka.PartitionCnt`，判定交给纯函数 `Test-DbReplicaPartitionBudget`：
  - `replicas > PartitionCnt` 即 throw，文本含「replicas ≤ PartitionCnt」；
  - `PartitionCnt ≤ 0` 或不是整数同样 throw（fail-closed）；
  - 同一份 manifest 里若有 HorizontalPodAutoscaler，它的 `maxReplicas` 同样要 ≤ PartitionCnt，行缺失或不止一处即 throw；
  - 滚动发布的 maxSurge 临时副本不计入。
- **为什么上界是分区数**：db_task 的分区在消费组成员之间独占分配，有效并行上限就是分区数。仓库现值：`replicas: 2`、`maxSurge: 1`、`PartitionCnt: 10`。
- **为什么 go/db 启动期不拒启**：maxSurge 本来就会短暂多出一个副本；共享消费组下进程启动时无从判断分配；热备能在属主崩溃后、在 session 超时内接手。
- **热备语义**：
  - owner 模式下拿不到分区的副本是热备：只分拣共享收件箱、回收孤儿收据，不执行重试；`db_owned_partitions = 0`，Setup 打 INFO 而不是 ERROR。
  - 热备不是故障，但多开的副本白占连接：MySQL / Redis 连接总账都按 manifest 的副本数记。
  - 热备连接的实情：本 zone 主库池在 `proto_sql/db.go` 的 `openDB` 里设了 `SetConnMaxLifetime(5 * time.Minute)`，没设 `ConnMaxIdleTime`。从未执行过 SQL 的热备只剩启动期打开的少量连接，被降为热备的实例最多 5 分钟内还带着 ≤ MaxIdleConn 条。(c) 不减少连接总账。
- **绕过**：门禁只看经本脚本 apply 的 manifest。`kubectl scale deploy/db --replicas=N`、`kubectl edit` 与带外 apply 的 HPA 都会绕过它。要扩副本就改 manifest 再走 zone-up，不要 `kubectl scale`。manifest 注释要求不给 db 配 HPA（sarama eager 再平衡下每次成员变化全员暂停）。门禁不替这条约定把关，只保证同一份 manifest 里万一出现的 HPA 上界不越过分区数。
- **manifest 未改**：修订稿 §8 把 `deploy/k8s/manifests/go-svc/db.yaml` 移出了 S1 清单，热备语义与「kubectl scale 会绕过门禁」写在本节和 `Assert-DbReplicasWithinPartitionContract` 的函数注释里。

## 9. 与 ④B2（双消费在线换代）的交互约束

B2 本轮不做（负责人拍板）。日后要做时，以下四条是硬约束（规格 A4-F10）：

1. B2 之前必须已落 MySQL 条件写。
2. 必须另行修订 D5「改 topic 路由必须走停写 drain + 新 TopicGeneration」，不能以 S1 的 D5 修订为依据。
3. 双消费窗口内禁止 merge_zone relocate / 合服，或者把 relocate 改为同时等两代排序锁。
4. `start_game.ps1` 与 `k8s_deploy.ps1` 的三方相等校验要同步放宽为「当前代 / 排空代」。① 读写分区对齐（S3）已落地，B2 设计须让 login 读与 scene 写同步切代，否则会打断读后写屏障。

## 10. 上线、回滚与紧急搬回

### 10.1 前置（Codex，线上不动）

1. 跑通 `docs/handoff/placement-reverify-checklist-20261012.md` §2「第 12 步」第 3 条（本机双 db 实例），以及 §5「已知覆盖缺口」第 1、4、7 条（第 7 条由 `TestStop_RetryLoopJoinsWaitGroup` 覆盖）。结果写回 PROGRESS。
2. 查清并记录线上各 zone 的 go/db 镜像与副本数：
   ```
   kubectl -n mmorpg-zone-<z> get deploy db -o jsonpath='{.spec.replicas} {.spec.template.spec.containers[0].image}'
   ```
   低于 `be6b4ab1d` 的 zone 先按瓶颈文档 §4.5 处理。S1 不与「副本 1→2」同批，也不在 TiDB Phase 1 迁移窗口内。
3. 确认 K8s `redis-0` 的 `redis_version ≥ 7.2`、`appendonly = yes`：按 `redis.yaml` 的口令注入方式在容器内执行 `INFO server` / `CONFIG GET appendonly`，口令不进命令行与日志。
4. 完成 §13 的构建与测试。

### 10.2 发布步骤（逐 zone、低峰、一次一个 Pod）

- **第 1 步：kind / staging 演练**。锁注入、kill -9、MySQL 暂停、回滚与工具演练全部在这里做，**生产不做任何注入**。
  - 生产需要积压时，只允许对合成测试玩家持有带 TTL 的排序锁 `distributed:lock:kafka:ordering:db_task_zone_<z>:<测试pid>:<MsgType>`（`errOrderingLockBusy` 属免预算错误，不进死信）。
  - 修订稿 §10 追加两项验收：
    - **异步不阻塞**：稳态负载下锁住 10% 合成测试玩家的排序锁 60s。健康玩家的 `w_task` / `w_queue`（`stress_summarize.ps1` 2d 段，或 `db_task_stage_seconds{stage="op_total"}`）与不锁时相差 ≤ 20%；Kafka lag 不持续增长；`db_durable_enqueue_seconds` 均值 ≤ 1.5s；`not_durable` / `error` 为 0。
    - **滚动重启**：`db_durable_enqueue_total{result="canceled"}` 可以增长；`not_durable` / `error` 必须为 0；DbDurableEnqueueNotConfirmed 不响。
  - 原规格风险第 6 条要求的演练：
    - **MySQL 恢复期的重试回落时间**：同一负载、同一积压量下，分别在 `shared` 与 `owner` 两种模式下暂停本 zone 主库造出重试积压，恢复后记录 `db_retry_queue_length{queue=~"ready|partitioned"}`（口径同 DbTaskRetryQueueGrowing：先按 pod 把两项相加，再按 namespace 取 max；各 Pod 采的是同一组 Redis 列表，不能跨 Pod 相加）从峰值回落到 0 的时间，并对比两种模式。owner 明显更慢、而 MySQL 仍有余量时，按 B0 调 `ServerConfig.Kafka.SubShardCount`（本批不做）；结果写回 PROGRESS（§6.1「MySQL 恢复期 p* 只由属主排空」）。
- **第 2 步：发布 S1 镜像**，`RetryRouting: shared`，WaitAOF 1/0/1500（db.yaml 的值；K8s 上副本份数按共享 Redis 形态渲染，redis-ha 形态为 1/1/1500，见 §7.12）。`k8s_deploy.ps1 zone-up` 生成 ConfigMap，镜像变化会触发滚动。
  - 每换一个 Pod 核对：`db_retry_routing_mode{mode="shared"} = 1`；`db_retry_queue_length{queue="partitioned"} = 0`；`db_durable_enqueue_total{result=~"not_durable|error"}` 不增（canceled 允许增长）；启动没有被拒（探针通过）；DbZoneNoPartitionClaims 不响。
  - 记录基线：`executed_nonowner` / `executed` 的比例、`db_ordering_lock_busy_total`、死信增速、`db_task_result_total{result="error"}`、Kafka lag。
  - 回滚：回旧镜像，不会残留 p*。
- **第 3 步：单 zone 金丝雀**，建议在该 zone 完成 S2 切组之后。前提是第 2 步已完成，且该 zone 每个 Pod 都暴露 `db_retry_routing_mode`；期间冻结其它 zone 的发布。
  ```
  kubectl -n mmorpg-zone-<z> set env deploy/db DB_RETRY_ROUTING=owner
  kubectl -n mmorpg-zone-<z> rollout status deploy/db
  ```
  - 只被动观察：每个 Pod 启动日志 `retry routing: mode=owner source=env DB_RETRY_ROUTING`；稳定后各 Pod 的 `db_owned_partitions` 之和 = PartitionCnt；`executed_nonowner = 0`；`forwarded` 之后共享收件箱 1–2 拍归零；`requeued_revoked` 只在再平衡时刻出现（同期 `db_claim_acquired_total` 增长）；`db_ordering_lock_busy_total{origin="retry"}` 接近 0；死信速率不高于基线；没有 `processing receipt not found`；`db_retry_stranded_receipts = 0`。
  - 回滚：`kubectl -n mmorpg-zone-<z> set env deploy/db DB_RETRY_ROUTING-`（触发滚动），shared 实例自动排空 p*。
- **第 4 步：全量**。`go/db/etc/db.yaml` 改 `RetryRouting: owner` → `k8s_deploy.ps1 zone-up` → `kubectl -n mmorpg-zone-<z> rollout restart deploy/db` 加 `rollout status`（只改 ConfigMap 不会滚动）→ 去掉金丝雀 env（`set env … DB_RETRY_ROUTING-`）。
  - 验收：每个 Pod 启动日志 `retry routing: mode=owner source=config`；24 小时内 `executed_nonowner = 0`；死信速率不升；p* 无长期积压；lag = 0；DbDurableEnqueueNotConfirmed、DbZoneNoPartitionClaims 不响。
  - 回滚到 shared：db.yaml 改回 `shared` → zone-up → rollout restart 加 status。
- **告警**：`kubectl apply -n observability -f deploy/k8s/db-task-retry-alerts.yaml`（PrometheusRule）。
- **redis-ha 接入时**：不改 db.yaml（`WaitAOFNumReplicas` 保持 0，给本机 compose 单实例；写 1 本机 go/db 的启动探针等不到副本而拒启）。K8s 上 db ConfigMap 的 `WaitAOFNumReplicas` 由 `k8s_deploy.ps1` 按共享 Redis 形态渲染（与 data-service 同一个 `Get-RouteStoreWaitAOFNumReplicas`：单实例 0、高可用 1；2026-10-19 部署侧落码，未运行，待 Codex 验证）。单实例切到高可用后，对每个 zone 重跑 zone-up（副本份数变 1），再 `kubectl -n mmorpg-zone-<z> rollout restart deploy/db` 加 `rollout status`（只改 ConfigMap 不会滚动），并重新评估 fail-closed 的可用性代价：
  - 副本维护期间所有失败路径上的分区都会停滞；
  - 三个 Redis Pod 同时坏两个时写入被拒（`min-replicas-to-write 1`）；切主后几秒内新主还没有从时，WAITAOF 确认失败（消息重投），此期间刚启动的 go/db 会被启动探针拒启。

### 10.3 回滚到 S1 之前的镜像（前置，已写进 release-checklist）

旧版本看不到 p*，直接回滚会让 p* 里的存盘滞留不落库。所以 `kubectl rollout undo` 或部署上一版本之前，必须按顺序做完：

1. **切 shared**：db.yaml 改 `RetryRouting: shared` → `k8s_deploy.ps1 zone-up` → `kubectl -n mmorpg-zone-<z> rollout restart deploy/db` → `kubectl -n mmorpg-zone-<z> rollout status deploy/db`。有金丝雀 env 时一并 `set env … DB_RETRY_ROUTING-`，同样要等 rollout status。
2. **三项人工核对**（工具的退出码 5 只看得见 R7 起登记模式的版本，更早的 S1 镜像不登记模式）：
   1. 各 db Pod 的 `db_retry_routing_mode{mode="owner"}` 都是 0（S1 之前的 Pod 没有这条序列，不算 owner）；
   2. `kubectl -n mmorpg-zone-<z> set env deploy/db --list` 里没有 `DB_RETRY_ROUTING=owner`。`kubectl set env` 加的变量不在 last-applied 里，k8s_deploy 的 apply 不会删它，而且环境变量优先于配置键；
   3. `rollout status` 已完成（只改 ConfigMap 不会滚动 db）。
3. **盘点**：`retry_requeue -check` 以退出码 0 退出（命令见 §10.4），即 p* 全 0、没有 owner 实例登记、目标不是空的。**非 0 一律不许回滚**。
4. **回滚镜像时认准 revision**：第 1 步的 `rollout restart` / `set env` 本身会生成新的 revision，之后不带参数的 `kubectl rollout undo deploy/db` 回到的是切换前那个 S1 revision，不是 S1 之前的镜像。要先用 `kubectl -n mmorpg-zone-<z> rollout history deploy/db` 找到 S1 之前镜像的 revision，再执行 `kubectl -n mmorpg-zone-<z> rollout undo deploy/db --to-revision=<N>`；或者走「部署上一版本」。
5. **回滚之后**：建议再跑一次 `-check` 确认 p* 仍为 0。回到的镜像早于按实例 processing 列表、不往登记表里写时，共享收件箱、死信、旧共享 processing（`kafka:retry:processing:{topic}`，这类镜像的在途收据就落在这里）也都为 0 时以 4 退出，参数核对无误即可；旧共享 processing 非 0、p* 为 0 时以 0 退出（`go/db/internal/kafka/retry_routing.go` 的 `RetryQueueDepthReport.LooksUnused`）。

- **ConfigMap 兼容性已确认**：go-zero v1.10.0 忽略未知键，旧二进制读带新键的 ConfigMap 能正常启动，回滚规程不需要先改 ConfigMap。`go/db/internal/config/config_test.go` 的 `TestConfigLoaderIgnoresUnknownKeys` 钉住这一点：以后升级 go-zero、改成严格模式时，它会先红。
- **release-checklist 条目**：修订稿 §8 原文里的命令缺 `-zone`，也没有退出码 4/5 与三项人工核对（修订稿文末「2026-10-16 更正」第 3 条）。`docs/ops/release-checklist.md` §4 的「go/db 回滚到 S1(重试只交给分区属主)之前的版本」已于 2026-10-17 按代码口径补上，上面第 4、5 条的 revision 与回滚后核对取自该条目，执行时以它为准。

### 10.4 `go/db/cmd/retry_requeue`

**用法**（工作目录 `go/db`；口令只经环境变量传入，不上命令行、不打印）：

```
go run ./cmd/retry_requeue -f etc/db.yaml -zone <z> -check                      # 只读盘点
go run ./cmd/retry_requeue -f etc/db.yaml -zone <z>                             # 搬回
go run ./cmd/retry_requeue -f etc/db.yaml -zone <z> -topic-generation <g> -check  # 已换代到 _g<g> 的 zone
```

**连 K8s 上的共享 Redis**：先在另一个终端执行 `kubectl -n mmorpg-infra port-forward svc/redis 16379:6379`。再在当前 shell 里把口令放进环境变量（例如 `MMORPG_REDIS_PASSWORD`；不要写进命令历史或脚本文件），然后：

```
go run ./cmd/retry_requeue -f etc/db.yaml -zone <z> -redis 127.0.0.1:16379 -redis-password-env MMORPG_REDIS_PASSWORD -check
```

**参数**（`main.go` 的 `flag` 定义）：

| 参数 | 缺省 | 说明 |
|---|---|---|
| `-f` | `etc/db.yaml` | 从中取 `ServerConfig.Kafka.PartitionCnt`、`TopicGeneration` 与 RedisClient |
| `-zone` | 0 = 没给 | **必填**（R7）。没给时以退出码 1 退出：`-zone is required …`。配置文件里的 `ZoneId` 不作缺省。改成必填的理由是 R7 之前的行为：缺省取配置里的 `ZoneId`，对 K8s 上的 zone 101 跑漏了 `-zone`，会盘点仓库配置里 zone 1 的 topic 并以 0 退出（fail-open）；现在漏了 `-zone` 一条不动、以 1 退出 |
| `-topic-generation` | -1 = 用配置的 `TopicGeneration` | 范围 [0, 2^32−1]；topic 由 `DbTaskTopicForGeneration(zone, generation)` 推导，generation ≤ 1 为 `db_task_zone_<z>`，否则 `db_task_zone_<z>_g<g>` |
| `-redis` | 空 = `RedisClient.Hosts` | host:port |
| `-redis-db` | -1 = `RedisClient.DB` | ≥ 0 |
| `-redis-password-env` | 空 = `RedisClient.Password` | 环境变量名；指定了却没设时以 1 退出 |
| `-check` | false | 只读：LLEN / ZRANGE / HMGET / SCAN |
| `-allow-empty-target` | false | 放行「目标看起来没在用」；只在该 zone 全部 go/db 都已正常停机时用 |
| `-batch` | 100（`RequeueDefaultBatch`） | [1, 1000]（`RequeueMaxBatch`），一次 `requeuePartitionScript` 最多搬几条 |

分区数没有覆盖参数，取 `-f` 里的 `PartitionCnt`。盘点与搬回都会另外 SCAN `kafka:retry:queue:{topic}:p*`，所以配置里的分区数偏小时，越界的 p 键照样会被列出并计入 p* 总数（输出里逐个列出）。

**输出与流程**：

1. 先打印前置条件行 `precondition: …`，里面写有三项人工核对。
2. 打印 `target: topic=… partitions=… redis=… db=… mode=… batch=…`，再打印盘点 `[before] shared_inbox=… partitioned_total=… dead=… legacy_processing=… instances=…`，逐行列出每个 p 键，以及每个实例的 processing 长度与登记模式（没登记显示 `unknown`）。
3. 两道门先于 p* 的判定，`-check` 与搬回都要过：
   - 登记表里有 owner 实例 → 5；
   - 目标看起来没在用 → 4。
4. `-check` 按 p* 退出。搬回时：p* 为空直接 0；否则调 `RequeuePartitionedRetries`——先 p0..p{N-1}，再 SCAN 处理越界键；只搬 v2 载荷，非 v2 进死信；每个键的批数上限 = 开始时长度 / batch + 2，免得与 owner 实例来回搬个没完。搬完再盘点一次。
5. Ctrl+C / SIGTERM 取消 ctx。工具可重入：中途失败或被打断，修好原因后重跑即可，已搬的不会重复。

**退出码**：

| 退出码 | 含义 | 处置 |
|---|---|---|
| 0 | `-check`：p* 全 0 且两道门都过；搬回：p* 本来就空，或搬完为 0 | 可以继续 |
| 1（`exitError`） | 参数 / 配置 / Redis 连通 / 盘点 / 搬回出错（缺 `-zone`、`-batch` 越界、`-topic-generation` 越界、`-redis-db` 为负、口令变量未设、Ping 失败、键类型不对等） | 修好原因后重跑；搬回中途出错时已搬的部分会打印出来 |
| 2 | 命令行参数解析失败（Go `flag` 包的缺省行为，`main.go` 未单独定义） | 修正参数 |
| 3（`exitResidual`） | `-check` 盘点到 p* 非 0；或搬回完成但 p* 仍非 0（多半还有 owner 实例在把收件箱分拣回 p*） | **不许回滚**；先切 shared、等 rollout，再重跑 |
| 4（`exitTargetUnused`，R7） | 目标看起来没在用：登记表为空，且共享收件箱、死信、旧共享 processing、全部 p* 都是 0（`LooksUnused`）。topic / `-redis` / `-redis-db` / `-zone` / 代次指错时所有键都不存在，盘点照样全 0、不报错，不拦就是 fail-open | 核对 `-f` / `-zone` / `-topic-generation` / `-redis` / `-redis-db`；只有该 zone 全部 go/db 都已正常停机时，才加 `-allow-empty-target` |
| 5（`exitOwnerInstances`，R7） | 模式 HASH `kafka:retry:instance_mode:{topic}` 里有实例登记为 `owner`（`OwnerInstances`；租约已过期、还没被回收的也算）；搬回一条不动 | 把该 zone 每个 go/db 切到 shared（或 S1 之前的镜像），等 rollout 完成后重跑 |

**实例模式 HASH**：续租脚本（`retryLeaseScript`）与认领脚本（`retryClaimScript`）在 ZADD 的同一原子操作里 HSET 本实例的生效模式；回收脚本（`retryReclaimScript`）把实例摘出登记表时一并 HDEL。正常停机的 `release` 也走回收脚本。更早的 S1 镜像（R7 之前）不写这个 HASH，工具里显示 `unknown`，退出码 5 拦不住它们，所以 §10.3 的三项人工核对不能省。

### 10.5 紧急回滚（来不及切 shared）

- 适用情形：新镜像的 shared 模式本身有缺陷。
- 回滚镜像后立刻执行 `retry_requeue`：先 `-check`，预期是 3；再去掉 `-check` 搬回；最后 `-check` 确认为 0。
- **搬回完成之前不清理任何 Redis 键**。
- 告警 DbRetryPartitionedStranded 会在约 10 分钟内响起（序列过期约 5 分钟加 `for: 5m`）。它只看 Prometheus 里 6h 内的最后样本：搬回之后窗口内仍会响，以 `-check` 退出码 0 为准并静默；窗口过后会自行消失，但这不代表已经处理，只能以 `retry_requeue -check` 的结果为准。

## 11. 告警

以下以 `deploy/k8s/db-task-retry-alerts.yaml` 在 2026-10-17 工作区的实际内容为准，均在组 `db-task.retry-and-dead-letter`（interval 30s）。表达式里的取值都与 `metrics.go` 的常量逐字一致：mode `shared|owner`；action `executed|executed_nonowner|forwarded|requeued_revoked|misrouted|dead`；queue `ready|partitioned|processing|dead`；target `retry|dead`；result `ok|not_durable|error|canceled`；pool `home`。写错取值的表达式不报错，只会永远不响（写在 unless 里的则是永远不抑制）。

| 告警 | 级别 / for | 条件（要点） | 窗口 | 局限与处置 |
|---|---|---|---|---|
| DbTaskRetryQueueGrowing（改 expr） | warning / 10m | `max by (namespace) (sum by (namespace, pod) (db_retry_queue_length{queue=~"ready\|partitioned"})) > 1000` | — | 先按 pod 相加、再按 namespace 取 max；S1 之前的 Pod 不报 partitioned，照样按 ready 计入。阈值待压测确认 |
| DbRetryPartitionedNoProgress | warning / 0m | p* 的 15 分钟最小值 > 0，**unless** 本 namespace 15 分钟内 `executed` 或 `executed_nonowner` 有增长，**unless** `db_store_cooldown_stores{pool="home"} > 0` | 15m（子查询步长 1m） | 按 namespace 聚合，看不到单个分区卡住；单分区的属主卡住时，该分区的 Kafka 消费也一起停（重试与新消息走同一个 worker），同时看 Kafka lag 与 DbZoneNoPartitionClaims。按分区的深度 gauge 不做（修订稿裁决第 9 条）。冷却抑制见下 |
| DbRetryPartitionedStranded | critical / 5m | 「最后一次上报时间最晚」的那个 Pod（`timestamp()` 子查询 + `topk by (namespace)`）的 partitioned 最后值 > 0，**unless** 本 namespace 有任何 `db_retry_routing_mode` 序列 | `last_over_time` 6h | 不依赖新版二进制在线；不能简化成 max（早先退出的 Pod 留下的旧非零值会误报）；6h 之后只能靠 `retry_requeue -check`；处置见 §10.5 |
| DbZoneNoPartitionClaims | critical / 5m | `sum by (namespace) (db_owned_partitions) == 0`，**unless** ready 序列条数 > `mode="owner"` 序列条数（说明还有 S1 之前的 Pod 在混跑） | — | 两种模式都适用（租约总是登记）；db 全停时没有序列、不响（由 Stranded 报）；再平衡与切组中途的短暂为 0 由 for 吸收；处置见 §6.2 |
| DbRetryNonOwnerExecutionInOwnerZone | warning / 0m | 10 分钟内 `executed_nonowner` 增长 **and** 本 namespace `max(db_retry_routing_mode{mode="owner"}) == 1` | 10m | 切换滚动中途会响，属预期；**看不见双属主窗口**（两边都计 `executed`，§6.3） |
| DbRetryRevokedOutsideRebalance | info / 0m | 10 分钟内 `requeued_revoked` 增长，**unless** `db_claim_acquired_total` 10 分钟内增长 | 10m | 用计数器而不用 `changes(db_owned_partitions)`：抓取间隔内完成的同数量再平衡，gauge 看不出变化 |
| DbDurableEnqueueNotConfirmed | critical / 0m | `sum by (namespace, target, result) (increase(db_durable_enqueue_total{result=~"not_durable\|error"}[5m])) > 0` | 5m | canceled 不计入；not_durable 也可能是 Redis 不可达或网络分区；事故期间死信会积累字节相同的副本（§7.10），按 (topic, partition, offset) 去重；不要靠关掉 WAITAOF 来止血，除非负责人同意 |
| DbRetryStrandedReceipts（R7） | warning / 10m | `max by (namespace, pod) (db_retry_stranded_receipts) > 0` | — | gauge 每个 Pod 一份，所以按 pod 报；重试协程约每秒一拍重搬，Redis 恢复后很快归零，for 10m 只报持续拒绝；处置见 §7.8 |
| DbConsumerGroupHeartbeatErrors（R7） | info / 0m | `sum by (namespace, pod) (increase(db_consumer_group_errors_total{source="heartbeat"}[10m])) > 0` | 10m | 对应 §6.3 的双属主窗口；偶发（broker 滚动、coordinator 迁移）可忽略，持续出现时查本 Pod 到 coordinator 所在 broker 的网络，并看同期 DbOrderingLockReleasedNotOwned / DbOrderingLeaseLost；`partition` / `claim` 两个 source 只进看板 |

**DbRetryPartitionedNoProgress 的冷却抑制带 `pool="home"`（与修订稿 §6 的差异）**

- 修订稿原文是 `sum by (namespace) (db_store_cooldown_stores) > 0`，不带 pool。代码按 `homeStoreCoolingDown` 的口径收窄到 `pool="home"`。
- 理由：只有本 zone 主库冷却时，shared 与 owner 两种模式才整拍不认领重试（owner 仍分拣、记 `forwarded`，不算进展），属于预期，冷却本身由 DbStoreCoolingDown 报。额外落点库（`extra_zone` / `extra_non_zone`）冷却时重试照常认领，派发即记 executed，不会让本条误报。按任意库抑制的话，合服源区库或非 zone 落点库长时间不可达时，会把「owner 失踪 / 误回滚旧镜像」一直压住。
- `"home"` 必须与 `metrics.go` 的 `PoolHome` 逐字一致。

**只进看板、不设告警的指标**：

- `db_ordering_lock_busy_total{origin=kafka|retry}`：owner 稳态下 retry 应接近 0；
- `db_durable_enqueue_seconds{target}`；
- `db_owned_partitions` 本身（被 DbZoneNoPartitionClaims 引用）；
- `db_consumer_group_errors_total` 的 `partition` / `claim` 两个 source。

**R7 新增告警的来历**：R7 修复报告要求新增「`db_retry_stranded_receipts > 0` 持续 10m」的 warning 告警，并把给 `heartbeat` 加告警列为可选。两条已由修改告警文件的写手于 2026-10-17 同批加上（上表最后两行）。本文完稿后对该文件做过一次复核；之后若再改，以该文件为准。

全部计数器在进程启动时以 0 预建：

- retry_route 2×6；
- ordering_lock_busy 2；
- durable_enqueue 2×4；
- retry_routing_mode 2，每个 S1 版 Pod 恰好两条，生效的为 1；`SetRetryRoutingMode` 遇到未知模式时两条都置 0 并记 ERROR；
- claim_acquired 1；
- consumer_group_errors 3；
- `db_retry_stranded_receipts` 置 0。

这些由 `TestRetryRoutingSeriesPrecreated`、`TestStrandedAndConsumerGroupErrorSeriesPrecreated` 钉住。`db_retry_queue_length` 例外：重试循环每拍采样之后才有。

## 12. 与 S2 / S4 / 围栏的接缝（按代码现状）

### 12.1 S2（消费组按 zone 拆、交接闸）

- 租约在 `sessionScope` 返回的父 ctx（交接形状下是 yieldCtx）派生完成之后登记，见 §5。
- 交接闸等待期间没有 claim，owner 模式下重试停滞（§6.2），属已知、可接受。
- 守望让出时走 `drainAfterStop("handoff peer reclaimed partition")`，等到的确认不标记（§7.4）。
- S1 在 Setup / Cleanup 里没有挂点，只改了 Setup 无分区分支的日志（owner 模式 INFO）。
- 切换顺序保持「该 zone 先完成 S2 切组，再切 owner」。

### 12.2 S4（G1 治标、停机六步、打点）

- **Stop 六步**：① 撤拉取 → ② 等消费循环（每个 claim 走 `drainClaimForShutdown`，上限 `shutdownDrainTimeout` 加 `consumerShutdownGrace` 5s）→ ③ 离组 → ④ 取消 worker 并等 `c.wg`（重试协程在其中，`StartRetryConsumer` 在 go 之前 `c.wg.Add(1)`）→ ⑤ `releaseRetryOwnership`（先等续租 goroutine 退出）与 `closeHandoff` → ⑥ `c.enqueuer().close()`，**必须是最后一行**。
- **claimCtx**：由 `WithCancel(WithoutCancel(parentCtx))` 派生，观察 goroutine 只在没有撤拉取时随父 ctx 取消它。所以停机排空期间 claimCtx 不取消，已派发的 owner 重试照常跑完，`lease.idle()` 等得到它们。
- **`drainClaimForShutdown` 的两路兜底**：`lease.idle()` 与入队层的异步确认都不随 `c.ctx` 取消，所以 `consumerDone`（② 超时后 Stop 先取消 c.ctx）与计时器两路必须都在，少任一路 ③④ 会重新互等。
- **派发 select 的撤拉取分支**：重试协程的两个派发 select（`consumeOneRetryFrom`、`consumeOneOwnedRetry`）都带 `pullStop` 分支，撤拉取即不再派发、收据留在 processing；`forwardSharedRetries`、`consumeRetryQueueOwner`、`consumeRetryQueueShared`、`retryStrandedReceipts` 的循环也都看 `pullStopped()`。
- **改停机顺序时必须保留**：worker 退出时的 `discardQueuedRetries`，以及 `routeToSubShard` 丢弃分支的 `finishRetry`。否则每次停机都要等满排空时限。
- **重试的 `processCtx` 保持 `w.ctx`**：S4 让 handler 接了 ctx（写的 SQL 用排序锁租约派生的 ctx，只随失租取消），这一点不变，失租取消对重试与 Kafka 一视同仁。

### 12.3 围栏（MySQL 条件写，`db-write-fencing-mysql.md` v0.4）

- **成功分支已改**：`handleTask` 的成功分支已从规格时的「`markAppliedSeq` → `markAppliedOwnerEpoch`」改为 `advanceApplied`。它是一个带排序锁令牌校验的 Lua（`appliedAdvanceScript`），同时推进游标与 epoch，上下文是 `WithTimeout(WithoutCancel(w.ctx), orderingLockReleaseTimeout)`；返回 0 时按失租处理，出错走 `deferFailedTask`（重试会写回 p{origin}）。
- **RedisSteps 扩到 12**：`metrics.RedisSteps` 为 `lock_acquire`、`cursor_get`、`epoch_get`、`owner_epoch_probe`、`placement_lookup`、`expand_get`、`placement_recheck`、`lease_verify`、`cache_publish`、`cache_set`、`applied_advance`、`lock_release`，与 `stress_summarize.ps1` 2e 段逐字对应。
- **S1 规格的「不动 markApplied* 的调用顺序与位置」已被围栏取代**。S1 没有改这一段，S1 改的只是这一段之前的两次 claim 复核与 `processCtx` 条件。
- **围栏隔离走既有入口**：
  - Kafka 源的 `writeStaleOwner` / `writeLegacyAfterSeq` 经 `quarantineOrderingConflict` 自动走 WAITAOF 入队；
  - 重试源走 `deadLetterRetry`（不走 WAITAOF；出错时登记待重搬表）；
  - 三类围栏死信由 `owner-epoch-alerts.yaml` 报，`DbTaskDeadLetterAdded` 已把它们排除。
- **`advanceApplied` 不受 WAITAOF 覆盖**：它与收据 ack 一样是主池上的普通写，§7.7 的残余（AOF 尾巴丢失 → 旧写重放覆盖 MySQL）照旧成立。围栏开关切 `dual` 后，受围栏的玩家表由行内 (owner_epoch, save_seq) 条件写挡住旧写；`legacy` 模式下以及不受围栏的表，这一残余仍在。

## 13. Codex 验证命令

**口径**：与 [上线待做](../ops/godb-pipeline-launch-todo.md) §9.2–§9.4 一致（做法同该文件 §2：临时 worktree）。本节的正则在其基础上并入了 R7 的用例，打勾以该文件为准。

**环境**：

- Go 1.27.2（`go version` 应为 `go1.27.2 windows/amd64`）；环境变量照任务单 §1：`GOTOOLCHAIN=local`、`GOFLAGS=-mod=readonly`、`GOPROXY=https://goproxy.cn,direct`；
- PowerShell 7；
- docker compose（`deploy/docker-compose.yml`）的 redis，带 `--appendonly yes`，`docker exec redis redis-server --version` ≥ 7.2。

**纪律**：按序执行，失败即停，不连续重试。失败时保留失败用例的 `go test -v` 全文（至少最后 200 行）、编译错误前 20 行、契约测试完整输出；归因时区分 S1 / R7 与他人改动。

### 13.1 go/db（工作目录 `go/db`）

```
gofmt -l ./internal ./cmd ./db.go
go vet ./...
go build ./...
go test -count=1 ./internal/kafka/... ./internal/config/... ./internal/metrics/... ./cmd/...
go test -count=1 -v -run 'TestClaimLease|TestOwnedPartitions|TestRetryPartitionReadyKeys|TestResolveConsumerRetryRouting|TestRetryOwnership|TestRetryRouting|TestRequeuePartitioned|TestRetryQueueDepths|TestConsumeClaim|TestStop_|TestAOFGroupEnqueue|TestDirectEnqueue|TestDurableEnqueue_|TestNewDurableEnqueue|TestProbeAOFAck|TestCheckWaitAOFReply|TestWaitAOFBudgetMs|TestDeferFailedTask_|TestQuarantine_|TestPoison_|TestRetryStranded|TestRetryClaim|TestAckRetryReceipt|TestRejoinBackoff|TestDurableFailureBackoff|TestWaitRejoinBackoff|TestConfirmingEnqueue|TestConsumerGroupError|TestClaimAcker_|TestConcurrentWorkers|TestRetryConsumer' ./internal/kafka/
go test -count=1 -v -run 'TestResolveRetryRouting|TestRedisWaitAOF|TestConfigValidateRejects|TestEtcYamlRetryKeysValid|TestConfigLoaderIgnoresUnknownKeys' ./internal/config/
go test -count=1 -v -run 'TestRetryRoutingSeriesPrecreated|TestRetryRoutingNamesMatchStressSummarize|TestStrandedAndConsumerGroupErrorSeriesPrecreated' ./internal/metrics/
go test -count=1 -v ./cmd/retry_requeue/
$env:CGO_ENABLED='1'; go test -count=1 -race -run 'TestOwnedPartitions|TestClaimLease|TestRetryRouting|TestConsumeClaim|TestAOFGroupEnqueue|TestDeferFailedTask_|TestStop_|TestConcurrentWorkers|TestRetryStranded|TestRetryClaim|TestDurableFailureBackoff|TestConsumerGroupError|TestClaimAcker_|TestWaitRejoinBackoff|TestConfirmingEnqueue' ./internal/kafka/
$env:GODB_REDIS_ITEST_ADDR='127.0.0.1:6379'; go test -count=1 -v -run 'TestDurableEnqueue_RealRedis' ./internal/kafka/
```

**通过标准**：

- `gofmt -l` 只允许列出 R7 修复报告记录的两个存量文件 `internal/kafka/applied_epoch_lua_test.go`、`internal/kafka/ordering_lease_test.go`（非本批引入），出现其它文件即失败；
- 其余全部 PASS，新增用例名都出现在 `-v` 输出里且不是 SKIP。例外：kafka 包那条 `-v` 命令的正则会带上 `TestDurableEnqueue_RealRedis`，没设 `GODB_REDIS_ITEST_ADDR` 时 SKIP 属预期，由最后一条命令补跑；
- 最后一条命令里 `TestDurableEnqueue_RealRedis` 必须 PASS 不得 SKIP（appendonly 为 no 时它按设计 Fatal 提示）；
- 既有的 `retry_routing` / `retry_ownership` / `consumer_shutdown` / `l04` / `durable_enqueue` 用例无新增失败；
- 没有 gcc 时 `-race` 在 Linux / WSL 跑，并注明平台。

### 13.2 其它目录

- `tools/merge_zone`：`go vet ./...`；`go test -count=1 ./...`。
- 仓库根：`pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`（db ConfigMap 的 RetryRouting、WaitAOFNumLocal、WaitAOFTimeoutMs == go/db/etc/db.yaml，WaitAOFNumReplicas 按共享 Redis 形态：dev 档缺省单实例 0、prod 档缺省高可用 1，db.yaml 那一项为 0；`Test-DbReplicaPartitionBudget` 11 > 10 throw、10 == 10 放行、PartitionCnt = 0 throw；仓库 manifest 副本数 ≤ PartitionCnt）；`pwsh -NoProfile -File tools/scripts/test_stress_summarize.ps1`。0 失败。
- 可选：本机有 `promtool` 时，把 `deploy/k8s/db-task-retry-alerts.yaml` 的 `spec` 抽成规则文件跑 `promtool check rules`；没有就报告「告警语法未验证」。

### 13.3 本机端到端（Codex，非文件）

**准备**：按 AGENTS §6.2 清理：`redis-cli FLUSHALL`、清空测试库、清 etcd、删 `tools/scripts/.run` 的 pid / log。

**owner 模式双实例**：

```
$env:DB_RETRY_ROUTING='owner'; pwsh -NoProfile -File tools/scripts/go_services.ps1 -Command start-exe -Services db -Counts db=2
# 两个实例日志各有 "retry routing: mode=owner source=env DB_RETRY_ROUTING";/metrics 有 db_retry_routing_mode{mode="owner"} 1
cd go/db; go run ./cmd/data_stress -zone 1 -players 200 -writes 50 -concurrency 32
# 压测中对 10 个测试玩家持锁 30 秒(MsgType 取 go/db 日志 task processed 行或缓存键前缀):
docker exec redis redis-cli SET "distributed:lock:kafka:ordering:db_task_zone_1:<pid>:<MsgType>" e2e-hold PX 30000
# 持锁期间 taskkill /F /PID <其中一个 db 的 PID>
go run ./cmd/verifier -config etc/db.yaml -mode strict -wait 180s
```

抓两个实例的 `/metrics`，过滤 `db_retry_`、`db_owned_partitions`、`db_durable_enqueue_`、`db_consumer_group_errors_total`。列出键：`docker exec redis redis-cli --scan --pattern 'kafka:retry:queue:db_task_zone_1*'` 后逐个 LLEN，以及 `kafka:retry:processing:db_task_zone_1:*`、`kafka:dead:queue:db_task_zone_1`。

**通过标准**：

- verifier 0 mismatch；
- 两实例 `executed_nonowner = 0`；
- 所有 ready / p* / processing / dead 的 LLEN = 0；
- 被杀实例有在途收据时，存活实例打出 `reclaimed abandoned retry receipts`；
- 没有 `processing receipt not found`；
- `db_durable_enqueue_total{result=~"not_durable|error"} = 0`；
- `db_retry_stranded_receipts = 0`。

**各项演练**：

- **shared 回滚演练**：owner 下持锁造出 p* 积压 → 停两实例 → `$env:DB_RETRY_ROUTING='shared'` 重启 → p* 排空，verifier 0 mismatch。
- **工具演练**：
  1. owner 造积压。在实例仍运行时 `go run ./cmd/retry_requeue -f etc/db.yaml -zone 1 -check` 应以 5 退出。
  2. 全停之后再 `-check`：两实例正常停机时会从登记表里摘掉自己，p* 非 0 时应以 3 退出。
  3. 去掉 `-check` 执行搬回 → `-check` 以 0 退出 → shared 启动 → verifier 通过。
  4. 另对一个不存在的 zone（例如 `-zone 999`）执行 `-check`，应以 4 退出；加 `-allow-empty-target` 后为 0。
  5. 不带 `-zone` 时应以 1 退出。
- **WAITAOF 拒启演练（可选）**：`docker exec redis redis-cli CONFIG SET appendonly no` → 起一个 db，必须拒启，且日志含 `WAITAOF … failed (needs appendonly yes and Redis >= 7.2 …)` → **必须**执行 `docker exec redis redis-cli CONFIG SET appendonly yes` 还原。
- **R7 的替代验证**：Redis 卡顿时认领与 ack 的重发（`retry_claim_guard_test.go`）、待重搬表（`retry_stranded_test.go`）已经由单测覆盖，端到端只看上面的 stranded 与 `processing receipt not found` 两项。
- **shared 整跑**：用 `RetryRouting=shared` 把第 13.3 节整跑一遍，标准相同（`executed_nonowner` 允许 > 0）。

失败时保留两实例的 stdout / stderr、各时刻的 metrics 快照、verifier 输出、Redis 键清单。

### 13.4 stress_summarize 2f 的缺口

`tools/scripts/stress_summarize.ps1` 的「2f. DB retry routing」段已核对：读取的指标名与 label 取值和 `metrics.go` 一致，由 `TestRetryRoutingNamesMatchStressSummarize` 钉住。列为 `mode`、`q_ready` / `q_part` / `q_proc` / `q_dead`、`exec` / `nonowner` / `fwd` / `revoked` / `misrouted` / `rt_dead`。

**已知缺口**：

- 没有 `db_owned_partitions`（owned）；
- 没有 `db_durable_enqueue_*`（aof_bad、aof_ms）；
- 也没有 R7 新增的 `db_retry_stranded_receipts` 与 `db_consumer_group_errors_total`。

压测通常不触发入队失败，owner 的校验在端到端步骤里直接抓 `/metrics`，所以本批不补；登记为「下次改 stress_summarize 时补 owned、aof_bad、aof_ms 三列（以及 stranded、cg_err）」。`stress_snap.ps1` 整页抓取 :9160，不需要改。

## 14. Java 版

**不适用**。Java 版没有 Kafka 存盘链和重试队列，这些都是服务端内部的重试管线，不是客户端可见的契约（AGENTS §12）。`PARITY.md` 的登记由主会话统一做。

## 15. 与规格 / 修订稿的差异汇总（以代码为准）

1. **位点语义**：只有停机排空与「消息通道关闭」的排空会等确认再提交；再平衡、交接让出、失败收尾（`drainAfterStop`）不提交，等到的确认也不标记（§7.4）。偏离修订稿裁决第 1 条与 §1.6。
2. **`drainAfterStop` 的顺序**是 `acker.stop()` → `lease.close()` → `waitClaimDrained`。原规格「逐文件改动 / key_ordered_consumer.go」第 15 条的顺序与代码一致；修订稿裁决第 5 条 ②、§3 第 2 条、「给实现者的说明」第 4 条写的「每条排空路径的第一步都是 lease.close()」，对 `drainAfterStop` 不成立（对 `drainClaimForShutdown` 与消息通道关闭分支成立）。
3. **重复副本来源**不止「网络错误与 Kafka 重投」（§7.6）。
4. **DbRetryPartitionedNoProgress 的冷却抑制带 `pool="home"`**（§11）。
5. **R7 新增、规格里没有的**：待重搬收据表与 `db_retry_stranded_receipts`；认领 / ack 的 nonce 防重与 `kafka:retry:guard:*`；实例模式 HASH 与退出码 4 / 5；`-zone` 必填、`-topic-generation`、`-allow-empty-target`；重新入组退避；sarama 错误通道观测与 `db_consumer_group_errors_total`；`moveClaimedRetryToDead` 的 `onMoved`。
6. **`retry_requeue`**：规格写的是「`-zone` 覆盖 ZoneId」「出错非零退出」，代码是 `-zone` 必填，退出码 0 / 1 / 3 / 4 / 5（参数解析失败为 Go `flag` 包缺省的 2）。
7. **分拣脚本**多返回各分区本拍的分拣数，owner 认领额度把它计入（`ownedRetryQuota`）；分拣、搬回、resolve 脚本动手前核对键类型。规格里的返回值只有 `{moved, legacy, invalid}`。
8. **`RetryQueueDepthReport`** 多了 `InstanceModes` 与 `PartitionedTotal` / `OwnerInstances` / `LooksUnused`。
9. **`durable_enqueue.go` 文件头的「主池只有 10 条连接」已过时**：`db.yaml` 现在显式写了 `PoolSize: 20`，理由不变。这是代码注释，本批不改。
10. **成功分支已由围栏 v0.4 改为 `advanceApplied`**，`RedisSteps` 扩到 12（§12.3）；规格里「不动 markApplied*」的不变量已被取代。
11. **handler 已经接 ctx**（S4 G1）：规格时「handler 丢掉 ctx」的现状描述已过时；重试不用 claimCtx 这一条照旧成立。
12. **release-checklist 条目比修订稿 §8 的原文多了两点**：revision 注意事项（`rollout restart` / `set env` 会生成新 revision，要 `--to-revision`），以及回滚后再 `-check`（§10.3）。
13. **告警比修订稿 §6 的 7 条多了 R7 的两条**：DbRetryStrandedReceipts、DbConsumerGroupHeartbeatErrors（§11）。

## 16. 修订历史

- 2026-10-17：新建（R6 文档批）。按 S1 规格、复核、修订稿与 R7 修复后的代码写成。状态：已落码，未编译、未测试，待 Codex 验证。
- 2026-10-18：文档复核更正。修订稿裁决条号三处（§3、§11、§15 第 2 条：「不设 shared 兜底」与挂点不变量 ② 是第 5 条，「按分区的深度 gauge 不做」是第 9 条）；§10.3 第 5 条补上退出码 4 还要求旧共享 processing 为 0（`LooksUnused`）；§10.4 `-zone` 行改写为 R7 之前的行为；§0 新增「残余风险」索引，§6.1 补「MySQL 恢复期 p* 只由属主排空」，§10.2 第 1 步补回落时间对比演练；§13.1 的 `-race` 正则补 `TestWaitRejoinBackoff`、`TestConfirmingEnqueue`。代码状态不变：已落码，未编译、未测试，待 Codex 验证。
